# huddle plan

huddle is meeting software for macOS: voice, camera and screen share, and an
AI whiteboard that draws on a shared board only when someone addresses it,
can diagram and take notes as the meeting goes on request, and answers aloud
when asked a direct question. Every board change is a version on the
meeting's timeline; replay plays the board, the transcript and the audio
together.

## Decisions

| Area | Decision |
|---|---|
| Platform | macOS only for now (Apple Silicon, macOS 14+). Windows and Linux are deferred; the core stays portable |
| Media | Voice, then camera and screen share. Pure Go clients over our own SFU inside the server (on pion, adapted from LiveKit's), no cgo. Stages 1 and 2 ran on a self-hosted LiveKit, removed in the media server's step 5 |
| AI | Gemini realtime. Per meeting, the whiteboard's voice is `answers` (default: silent while drawing, speaks only when asked a question), `muted` or `narrate` |
| Board | Only the AI writes. People direct it by voice, a typed command or a push-to-address key |
| Replay | Board versions, transcript and audio |
| Accounts | Phone number, with a Twilio Verify code typed into the app. Invites by phone number, SMS invites, join codes, scheduled meetings |
| Server | Go over Postgres and an S3-compatible store |

## Stage 1: voice and the whiteboard, on a local network

**Step 0, the base.** The app window and offscreen `render`; the server
with its store (append-only migrations), the meeting WebSocket and a dev-only
join; the shared wire protocol and the media interface the tracks build on; a
signed app bundle with SDL in `Contents/Frameworks` under the hardened
runtime, notarizable; CI.

**Track V, voice through LiveKit (pure Go).** LiveKit's Go SDK in the app;
Opus (a pure-Go library, picked on measurements); an adaptive jitter buffer
with FEC and concealment; playout mixed on the output device's clock; macOS
voice processing for echo cancellation at 48 kHz; a headless file-to-file
mode with simulated loss and jitter for tests; an echo bot and fixture
participants. Gates: latency and loss measured file to file, then two Macs
talking over speakers with no echo.

**Track A, the AI whiteboard (server first).** A probe of the realtime
model's input paths and tool timing; the board on the server, with layout
and text measurement there (embedded fonts only) so every app draws the same
picture, plus sorting, a notes panel, undo and revert; per-speaker
transcription; a deterministic gate that opens only when someone addresses
the whiteboard (by name, typed command or key); one realtime session per
meeting as the brain, one command in flight, with session resumption; voice
out only when the brain decides to answer. Fixture meetings rendered with
macOS voices score it: no board writes without the gate, wake precision and
recall, latency, silence on draw commands, ordering, 60 minutes of
continuity.

**Integration (the user's gate passed on two Macs, 2026-10-29, on huddled's
own SFU; `docs/gates.md`, "Stage 1").** Two Macs, two people: talk, ask for
a diagram, both see the same board; ask a question and hear the answer; auto
mode on and off; undo.

## Stage 2: camera and screen share

Built the way stage 1 was: a step on main, then two tracks in parallel
worktrees, then integration. Agents never open a camera or capture the
screen; the test pattern (`internal/video`: a moving picture with its frame
number as a barcode and a flash patch) stands in, through the same encoder,
network and decoder.

**Step 0 (done).**
- The connection screen says why the server or voice is down, with a
  countdown, and voice retries on its own.
- The whiteboard's track keeps sending, so LiveKit doesn't pad it.
- `huddle video-probe`: VideoToolbox through purego, end to end.
- The seams: the video types in `internal/rtc`, and `cmd/huddle/video.go`.

**Track P, the pipeline (merged 2026-10-28).**
- **Capture:**
  - The camera through AVCaptureSession, asking for NV12. SDL3's camera
    bindings aren't usable yet, and AVCaptureSession hands VideoToolbox its
    buffers without a copy.
  - The screen through ScreenCaptureKit, chosen in the system picker
    (`SCContentSharingPicker`): up to 15 fps and 1920 wide.
- **Encode and publish:** one VideoToolbox encoder per simulcast layer.
  - Camera: 720p, 360p and 180p.
  - Screen: full size and half.
  - Published with lksdk's `PublishSimulcastTrack`. A receiver's PLI or FIR
    forces a keyframe on that layer.
- **Congestion control:** pion's send-side estimator (GCC, with TWCC)
  shares the estimate among the layers, lowest first, and pauses a layer
  that doesn't fit.
- **Receive:** H.264 is depacketized and decoded, with a PLI when a frame is
  lost. Lip sync comes from sender reports, against the same person's
  audio. The whiteboard subscribes to audio only.
- **Tools:**
  - `huddle video` (file mode: the pattern in, frames and a latency CSV
    out, with network simulation and an uplink limit).
  - `huddled bot cam` (a pattern camera or screen in a room).
- **Gate:**
  - capture to decoded under 120 ms on loopback
  - recovery from 5% loss within 1 s
  - at a 1 Mbps uplink: the top layer pauses and the voice holds
  - layer switches within 1 s
  - lip sync within ±60 ms median

**Track U, tiles and the stage (merged 2026-10-28).**
- **Drawing:** NV12 planes go up as two textures, and a pass converts them
  to RGBA for each tile.
- **Layout:**
  - The stage holds the board or a screen share, with tabs. A share takes
    the stage, and the board's tab shows a badge when the whiteboard draws.
  - Camera tiles run in a strip, and each asks for the layer that fits it.
- **Controls:** Camera and Share buttons, and a camera section in the device
  menu.
- **Gate:** renders with 1 to 7 people, a share, the minimum window, both
  themes, and upload costs measured.

**Integration (agent checks passed 2026-10-28; the user's gate passed 2026-10-29, on huddled's own SFU; `docs/gates.md`, "Stage 2").**
- **Alone:** pattern bots for cameras and a share, plus the echo bot.
- **Two Macs:** cameras both ways, a display and a window shared, the board
  drawing during a share, and a 1 Mbps link that drops the video a layer
  while the voice stays clean.

## The media server (between stages 2 and 3)

LiveKit runs as a service of its own, and its SDK stands between us and
fixes: it pads a track nobody has sent on (new subscribers' jitter buffers
sat at 300 ms), doesn't expose Dynacast (every layer is sent, watched or
not), and picks the layer below a tile slightly larger than it. (The 1.1 s
to a first video frame it was blamed for turned out to be our own
assembler: step 1.) So the SFU moves into `huddled serve`, written
here on pion and adapted from LiveKit's server and SDK (Apache 2.0: the
license and NOTICE ship with the code and the app), and our own client
replaces lksdk behind `rtc.Connector`. LiveKit stayed beside it
(`HUDDLE_MEDIA=livekit|builtin`) until every gate matched its numbers, and
went in step 5.

- **The shape:**
  - Media on one UDP port, the HTTP port's number, advertising the LAN
    address; full ICE; TURN waits for the release stage.
  - Signalling on its own WebSocket, `/rtc`, in JSON; HMAC tokens from the
    dev join.
  - Two connections per person, as LiveKit has: the client offers on the
    one it publishes on, the server on the one it sends on.
  - pion's NACK, reports and TWCC on both sides; no RTX (pion's responder
    never marks a simulcast layer's retransmissions) and no padding.
  - Sender reports on each down track translated from the publisher's, so
    lip sync holds.
  - The smallest layer at least the size asked for, switched at keyframes,
    a keyframe asked for at once on a new subscription.
  - The whiteboard joins in-process: the voices come to it as RTP from the
    SFU, its voice goes straight in.
- **Step 0 (done 2026-10-28):**
  - LiveKit's server source for reference, and baselines through LiveKit:
    voice-check, the `huddle video` set, agent-check, and an app run with
    bot cams.
  - Spikes as tests in CI: pion simulcast pion to pion, the forwarder's
    splice on synthetic and encoded layers, pion's GCC on the server's side.
  - **Found:**
    - A switch waits for the old layer to finish its frame. A keyframe
      older than what was sent is used anyway, the client showing only
      newer frames: waiting for a newer one never ends if the new layer
      lags.
    - pion's GCC on the sending side holds a narrow link when what's
      sent follows it, and climbs while less is sent, so the allocation
      caps it.
- **Step 1, the seams (done 2026-10-28):**
  - The /rtc protocol and its tokens on main first, then three worktrees,
    merged:
    - lkrtc became `internal/rtc/room`: lksdk only in `lk.go`, behind a
      transport interface.
    - The dev join says which media server, the bots join through it,
      and the whiteboard reaches media through an interface.
    - The SFU forwards voices with translated sender reports, and
      `sfuclient.Session` joins it.
  - `HUDDLE_MEDIA=builtin` runs huddled's SFU, without the whiteboard
    until step 2.
  - **Found:** the 1.1 s to a first video frame was the app's assembler.
    It waited for the first marker, which threw away the keyframe every
    subscription starts with. Starting at a keyframe's first packet
    brings it to about 70 ms, through LiveKit too.
- **Step 2, voice on our SFU (done 2026-10-28):**
  - Two worktrees, merged:
    - The room's builtin transport over `sfuclient.Session`: the app,
      the tools and `bot cam` join huddled's SFU with no change of
      their own.
    - The whiteboard in-process (`sfu.LocalJoin`: in the room with
      neither socket nor connections, handed people's voices only), and
      the voice bots on sfuclient.
  - **Gate, measured:** voice-check and the speech scores within run-to-run
    noise of LiveKit's; a new subscriber at 40 ms from its first packet,
    which came 36–44 ms after joining (LiveKit's 133–136 ms); agent-check
    `review`, `overlap` and `planning` with the same scores, the ears
    finding speech ends at the same 460 ms; `commands.wav` through the
    app's mic path at 480–520 ms, as on LiveKit.
  - **Found:** pion announces a track before its first packet, and its
    codec stays empty until one is read, so the room takes the codec
    from what was negotiated.
- **Step 3, video (done 2026-10-28):**
  - Two worktrees, merged:
    - The SFU forwards video: each layer read by its RID, a forwarder per
      viewer, the smallest layer at least the size asked for (the nearest
      running one while its publisher pauses it), a keyframe asked for at
      once and at most every 200 ms a layer, sender reports translated
      from the layer being sent.
    - The app's receive side: video under heavy loss, and lip sync from
      the first frame, timed by when the voice's and video's packets come.
  - **Gate, measured:** run for run, the video set matched or beat
    LiveKit:
    - a viewer's first frame 100–117 ms after the track arrived
    - a 342×192 tile at 360p
    - 720p 63–67 ms after asking (LiveKit's 1.1 s)
    - lip sync worst within 19 ms, also at 1 Mbps
    - at 20% loss with jitter, no freeze over 60 s (LiveKit 13–15)
    - five cameras, a share and three viewers at 11% of a core
  - **Found:**
    - The voice carried transport-wide numbers, and the pacer sends it
      ahead of queued video, so GCC saw departures run backwards and took
      that for congestion. At 1 Mbps up, the 360p layer paused for most
      of a run. The voice now goes unnumbered, as on LiveKit.
    - With no RTX, a resend has its packet's own sequence number, so
      SRTP's replay window threw away every resend of a packet the
      receive-side simulation had dropped after decrypting it. Replay
      protection is off under simulation only.
    - Video fell behind at heavy loss because each hole was waited for
      250 ms from when the assembler reached it, not from when it
      appeared, so the waits queued. LiveKit made it worse: a PLI stops
      its retransmissions until the next keyframe.
- **Step 4, Dynacast and the downlink (done 2026-10-29):**
  - Two worktrees, merged:
    - Dynacast: the SFU tells each publisher which layers anyone is sent,
      at once when that grows and 3 s after a layer leaves it, every layer
      kept for the first 10 s; the publisher stops encoding the rest, and
      brings one back at its floor at once when it's wanted again.
    - The downlink: pion's GCC on every down connection; a viewer is sent
      what its settings want until a queue stands in front of its link,
      then each video gets the largest layer that fits, a screen first,
      down at once and up only through a probe of padding between frames.
  - **Gate, measured:** against LiveKit, run for run:
    - a layer nobody watches costs nothing: encoder services at
      0.0–0.1% with no viewer (LiveKit 1.7–2.0% throughout)
    - 720p paused by Dynacast is shown 56–74 ms after asking (LiveKit,
      never paused, up to 1.17 s)
    - a first frame on a camera with every layer paused in 77–121 ms
    - behind a 1 Mbps downlink, no freeze and nothing lost (LiveKit's
      link dropped 4,400–5,100 packets and the voice scored 0.41–0.44),
      and a screen keeps its layer while the camera beside it drops
    - huddled at 9.1–9.4% of a core and 84 MB with five cameras, a share
      and three viewers (LiveKit 10.1%, 119 MB)
    - missed: the voice behind 1 Mbps scored 0.80–0.87 against 0.88. No
      packet was lost; the jitter buffer steps up for the first queue and
      back down, a second of the score each. Clean 720p latency is 24–25 ms
      against LiveKit's 21–22.
  - **Found:**
    - VideoToolbox's rate control counts a gap in the timestamps as bits
      not spent: a 720p layer resumed at its floor sent 1.7–1.9 Mbps for
      4 s. A paused encoder now has the gap taken out.
    - LiveKit's allocator goes by REMB, which pion never sends, so
      through LiveKit no viewer's downlink is followed at all.
    - pion's GCC sees a queue only while it grows, and took a lossy
      network's jitter for overuse. A downlink is limited by a queue
      standing, measured from the feedback's one-way delays, with GCC's
      estimate under what's sent.
    - pion's NACK responder answers every NACK, and the viewer asks every
      20 ms: behind 1 Mbps a viewer was sent 13 Mbps of resends. Resends
      are gated while the link is saturated.
    - pion's GCC counts padding in the header as the header alone, so the
      SFU writes its padding in the payload.
    - Where the two met: a narrow viewer's smaller layer, paused by
      Dynacast, was never chosen; it's now chosen and wanted. And in the
      publisher, the pacer's rate fell with the paused layers, and a
      smaller layer wanted again could pause a running larger one.
- **Step 5, builtin the default, then LiveKit gone (done 2026-10-29):**
  - builtin became the default and every agent gate was rerun on it
    (`docs/history.md`, step 5a). Found on the way: the whiteboard could lose a
    command behind a question whose last turn-complete never came, and the
    jitter buffer told lip sync a stale position after a silence (a frame
    held 350 ms behind a narrow uplink). Both fixed.
  - The user's two-Mac gates passed on it.
  - Then LiveKit, lksdk, the protocol module and `lktoken` went (step 5b):
    the module graph from 231 modules to 114, the app from 57 MB to 39 MB,
    the server from 63 MB to 45 MB. The dev join names huddled's own SFU,
    and an app refuses a server that names another.

## Later stages

3. Stage 3, accounts and meetings: Verify sign-in, meetings, scheduling, invites, join
   codes and a lobby, SMS invites (needs A2P 10DLC registration).
4. Stage 4, recording and replay: per-speaker audio to object storage, mixdown, one
   meeting clock, a replay screen, consent at join.
5. Stage 5, release: a notarized DMG on GitHub Releases, a version check, hardening.

## Facts the design rests on (checked 2026-10)

- Twilio Verify sends codes only (no links in SMS) and is exempt from A2P
  10DLC on Twilio's pooled senders; texts with links need Programmable
  Messaging and 10DLC or toll-free verification.
- Gemini's realtime model answers in audio only and may choose not to
  speak; sessions last about 10 minutes per connection and continue through
  resumption, with context compression for unlimited length. The streaming
  transcription model takes 16 kHz PCM, returns text, and runs 10-minute
  sessions without speaker labels (each participant's own track gives those).
- The app's first video frame waits on its own assembler, not the SFU: a
  subscription starts with a keyframe, and starting a unit at the
  keyframe's first packet shows it at once.
- pion's NACK responder makes RTX by copying the original header and never
  writes repaired-rid (interceptor v0.1.47), so a pion SFU can't tell which
  simulcast layer an RTX stream repairs; without RTX it resends on the
  stream itself.
- pion's Opus decoder is pure Go and complete; its encoder is CELT-only.
- The hardened runtime needs the mic and camera entitlements, and every
  loaded library signed by the same team.
