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
| Media | Voice, then camera and screen share. Pure Go clients over a self-hosted LiveKit SFU, no cgo |
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

**Integration.** Two Macs, two people: talk, ask for a diagram, both see the
same board; ask a question and hear the answer; auto mode on and off; undo.

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

**Integration (agent checks passed 2026-10-28; the user's gate is in AGENTS.md, "The stage 2 gate").**
- **Alone:** pattern bots for cameras and a share, plus the echo bot.
- **Two Macs:** cameras both ways, a display and a window shared, the board
  drawing during a share, and a 1 Mbps link that drops the video a layer
  while the voice stays clean.

## Later stages

1. Accounts and meetings: Verify sign-in, meetings, scheduling, invites, join
   codes and a lobby, SMS invites (needs A2P 10DLC registration).
2. Recording and replay: per-speaker audio to object storage, mixdown, one
   meeting clock, a replay screen, consent at join.
3. Release: a notarized DMG on GitHub Releases, a version check, hardening.

## Facts the design rests on (checked 2026-10)

- Twilio Verify sends codes only (no links in SMS) and is exempt from A2P
  10DLC on Twilio's pooled senders; texts with links need Programmable
  Messaging and 10DLC or toll-free verification.
- Gemini's realtime model answers in audio only and may choose not to
  speak; sessions last about 10 minutes per connection and continue through
  resumption, with context compression for unlimited length. The streaming
  transcription model takes 16 kHz PCM, returns text, and runs 10-minute
  sessions without speaker labels (each participant's own track gives those).
- LiveKit's Go SDK takes already-encoded media; its root package needs no cgo.
- pion's Opus decoder is pure Go and complete; its encoder is CELT-only.
- The hardened runtime needs the mic and camera entitlements, and every
  loaded library signed by the same team.
