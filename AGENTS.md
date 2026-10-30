# huddle: notes for coding agents

huddle is meeting software for macOS. People meet with voice, camera and screen share, and an AI whiteboard draws on a board everyone shares, but only when someone asks it to ("whiteboard, diagram what we just discussed, alphabetically"). It can also diagram and take notes as the meeting goes, on request, and it answers aloud when someone asks it a direct question. Only the AI writes to the board; every change is a version on the meeting's timeline.

Two binaries: `huddle`, the app (Go on SDL3 through `github.com/Zyko0/go-sdl3`, purego, no cgo, with our own GPU renderer), and `huddled`, the server (HTTP and a meeting WebSocket over Postgres, the whiteboard on Gemini Live, and the media server: our own SFU on pion, adapted from LiveKit's). The engine is copied from an earlier project of the author's; per-file provenance lines say so.

## Docs
- `docs/plan.md`: the plan, its decisions and the stages ahead.
- `docs/design.md`: how each part works and why, in depth: every package, the voice and video paths, the ears, what the models do, every log line. Read the section for what you're changing first.
- `docs/history.md`: what each step built, checked and measured, oldest first. Compare a change against its numbers.
- `docs/gates.md`: the runs only the user makes (real devices, two Macs).

## Status (2026-10-29)
Stages 1 and 2 are built and passed the user's gates on two Macs: voice, the whiteboard, camera and screen share, all on huddled's own SFU. LiveKit is gone (the media server's step 5b). Next: stage 3 in `docs/plan.md`, accounts and meetings.

Open issues (detail in `docs/design.md`, "Video"):
- A viewer's downlink starts unlimited, and its first overload costs the voice a second or two (speech 0.80–0.87 behind 1 Mbps down, against 0.916 unlimited).
- A publisher's probe for a paused layer costs the voice 10–30 ms of underrun, and one drop in GCC's estimate ends it (once, a camera's 720p never started).
- Lip sync assumes a frame's encoding takes about as long as Opus's delay; a slow encoder puts frames late.
- The camera's and the share's failure messages are generic; the details are in the log.

## Packages
Everything under `internal/` is pure (never imports SDL) except `gfx` and `platform`. Each package in full: `docs/design.md`, "Packages".

| Package | What it does |
|---|---|
| `internal/wire` | What the app and the server say: WebSocket envelopes, the dev join (`DevJoin`, `Joined`). `wire/signal` is the SFU's /rtc protocol (join, offers, tracks, a viewer's `settings`, `wanted` for Dynacast). |
| `internal/rtc` | The app's side of the media as interfaces: `Connector`, `Room`, `Mic`, `Speaker`, events. Audio is 48 kHz mono float32 in 10 ms frames. |
| `internal/rtc/room` | `rtc.Connector` over huddled's SFU (`sfu.go`). Publishes the mic (Opus) and simulcast video (H.264) under GCC and an allocator (`alloc.go`); plays voices through jitter buffers and the mix; puts video back together, syncs it to the voice and decodes it (`assemble.go`, `videorecv.go`). |
| `internal/rtc/sfuclient` | Joins the SFU (adapted from LiveKit's Go SDK): the /rtc socket, the publish and subscribe connections, events in order, `Publish`, `Settings`, `PLI`. |
| `internal/rtc/codecs` | What every connection speaks: Opus (PT 111) and H.264 Constrained Baseline (PT 102), their RTCP feedback, the header extensions. |
| `internal/rtc/icept` | pion interceptors: our pacer with pion's GCC (`NewCC`), and links for tests (`Uplink`, `Downlink`, `Sim`). |
| `internal/rtc/fileio` | A WAV mic and a recording speaker, for runs with no devices. |
| `internal/opus` | Opus (48 kHz mono, 20 ms, FEC, DTX) over libopus transpiled to Go. |
| `internal/jitter` | One remote voice's jitter buffer: target 40–300 ms from recent arrivals, FEC, concealment, DTX. |
| `internal/playout` | Mixes every voice into 10 ms frames paced by the speaker; levels for the talking dots. |
| `internal/netsim` | Loss, jitter and reordering, for tests. |
| `internal/pcm` | PCM helpers: levels, the speech `Detector`, the scores the checks use (correlation, click onsets). |
| `internal/board` | The board and its ops (the model's tool calls): `Apply`, `Replay`, `Digest`, `Tools` (the descriptions are the model's manual). |
| `internal/board/layout` | Lays a whole board out as a `Placement`; the server runs it for every version. |
| `internal/board/measure` | Sizes the board with the embedded fonts only, so the server and every app agree. |
| `internal/gate` | Whether someone is talking to the whiteboard (its name, a typed command, the push-to-address key) and where the command ends. |
| `internal/server/agent` | The whiteboard, one per room: joins the SFU in-process (`sfuroom.go`), the ears (`ears.go`), the gate, one Gemini Live brain with reconnects, the command queue, versions, its voice (`voice.go`). |
| `internal/live` | One Gemini Live session: ordered events, every send from one goroutine. |
| `internal/cue` | Holds ops until they should be seen (nothing uses it yet). |
| `internal/server/sfu` | The media server inside `huddled serve` (adapted from LiveKit's): rooms, two connections per person, voices forwarded, a video layer per viewer (`forwarder.go`), Dynacast (`dynacast.go`), each viewer's downlink (`downlink.go`, `resend.go`), speakers, the whiteboard's in-process participant (`local.go`). |
| `internal/server/store` | Postgres through pgx: append-only migrations, meetings, immutable board versions (`Commit(expectHead)`), transcripts, the whiteboard's state. |
| `internal/server/wshub` | The meeting WebSocket. |
| `internal/server/httpapi` | `/healthz`, `/dev/join` (dev mode), `/ws`, `/rtc`. |
| `internal/devjoin` | The dev join's client side, for the app, the tools and the bots. |
| `internal/fixturemeet` | Fixture meetings: scripts, rendering with `say` and ffmpeg, bots that play them into a room. |
| `internal/ui`, `ui/boardview`, `ui/transcript` | Input, widgets and focus; the board widget, drawing a version at the server's placement; the transcript panel (not shown in the app yet). |
| `internal/draw`, `geom`, `text`, `theme` | The display list, path geometry, text (embedded Inter and JetBrains Mono, system fallback), colors. |
| `internal/gfx` | The GPU renderer: the uber-shader, glyph atlases, offscreen renders, video frames (NV12 to RGBA in `nv12.frag`). |
| `internal/platform` | SDL loading and init, signals, `Wake`, Reduce Motion, the Dock badge. |
| `internal/platform/audio` | Mic and speakers on AVAudioEngine through purego: voice processing (echo cancelling), devices by UID, the fallbacks. |
| `internal/platform/macvideo` | VideoToolbox (encode, decode, scale) and capture (the camera, the screen through the system picker, the test pattern) through purego; `Sender` encodes the simulcast layers. Darwin only. |
| `internal/video` | Frames in Go memory (NV12), `Latest`, the test pattern and its barcodes, H.264's byte formats. |
| `internal/config`, `internal/logx` | Data and log paths (`HUDDLE_DATA_DIR`), a log file per subcommand. |
| `shaders/` | HLSL in `src/`, compiled SPIR-V and MSL in `compiled/`, `manifest.json`. |
| `cmd/huddle` | The app, one file per subcommand (`app`, `render`, `voice`, `voice-check`, `video`, `video-probe`, `audio-devices`, `tiles-bench`). `call.go` is the voice session, `devices.go` and `devmenu.go` the devices, `scene.go`, `stage.go`, `tiles.go` and `bar.go` the window. Keys: `/` the command field, `f` fit the board, `m` mute, `d` the device menu, right ⌥ held to address the whiteboard. |
| `cmd/huddled` | The server: `serve` (`media.go` sets up the SFU), `migrate`, `put-board`, `agent-check`, `live-probe`, `fixtures`, `meet`, `type`, `bot echo`, `bot play`, `bot cam`. |

## Rules
- **This repo is public (MIT).** No personal details in files or commit messages: names, emails, machines, hardware names, certificate hashes, team IDs, private repository names. Code copied from the author's private projects says "Copied from an earlier project of the author's"; code from reel (public) may name it. `SIGN`, `NOTARY_*` live in `local.mk`, which git ignores. Scan the diff and the history before every push. Push only when the user says.
- **The API key is never printed, logged or committed.** It comes from `GEMINI_API_KEY`; the Makefile takes only that line from `HUDDLE_ENV` (default `.env`, which git ignores; `.env.example` shows the line).
- **Don't open the user's microphone or capture their screen from an agent.** `make run` and the app joined to a room with voice on are the user's to run. Agents use fixtures, file-to-file runs, `render`, and `audio-devices` (lists devices, opens only the speakers).
- **Never touch the user's real data.** Dev runs set `HUDDLE_DATA_DIR` to `.dev/data` (the Makefile does).
- **No cgo.** Native code is loaded with purego only; builds use `CGO_ENABLED=0` and `make nocgo` checks the binaries build that way.
- **Agents' window runs use `-no-voice -no-video`.** `-no-voice` joins the room's audio listen-only: the speakers alone, the input node never touched. `-no-video` never opens the camera or the share (others' videos still come in and decode); `-fake-camera` makes the Camera and Share buttons send the test pattern instead. To check device choice, point the speakers at a virtual output (`audio-devices -speakers`, `TestSwitchSpeakers`, or a `devices.json` in a data dir of your own: never the user's `.dev/data`, which `make run` reads).
- **Every call into the transpiled libopus goes through `opus.onPinned`.** The transpiled code passes Go stack addresses as uintptrs; a call that grows a moved stack reads garbage (an abort in SILK after concealment, only with the GC running). The pinned worker's stack is grown once and never shrinks, and each codec's shim stack (the C stack the transpiled code puts its locals on: `newTLS` in `libopus.go`) is allocated at 1 MB up front so it never moves either. `TestConcealUnderGC` is the regression test. Under `-race`, Go's checkptr trips inside the transpiled code: race-test with `go test -race -gcflags=all=-d=checkptr=0`.
- **Go stays off the real-time audio thread.** AVAudioEngine taps and player completions run on the engine's own threads and do little: the tap picks the channel, converts it to 48 kHz through an SDL stream and queues it; a completion counts what was rendered and signals. No AVAudioSinkNode or render callbacks from Go.
- **The core stays pure.** Everything under `internal/` except `gfx` and `platform` never imports SDL; `internal/deps/purity_test.go` enforces this; add new pure packages to its list.
- **Only the main thread touches the GPU.** Other goroutines write shared state under a lock and call `platform.Wake()` (see `meeting.update`).
- **Only the server changes the board.** Apps send commands and render versions; a version carries its layout (`wire.BoardVersion.Layout`, a `layout.Placement`), so every app and every replay draws the same picture.
- **The board is measured with the embedded fonts only** (`internal/board/measure`). A character Inter lacks draws as its missing-glyph box rather than in a system face, which would size differently on another machine. Text outside the board (the status pill, the field, the transcript) keeps the system fallback. A change to `measure` (or a card's geometry) changes every size the server places, so the server and the apps ship it together.
- **Nothing reaches the brain without the gate.** Every turn starts from a gate command (wake, typed, key) or auto mode, which a command turned on; `agent-check` counts versions written without one (must be 0).
- **Answer drawing SILENT, speak and reads WHEN_IDLE** (`docs/design.md`, "What the models do"). Changing the scheduling, the tool descriptions or the system prompt (`internal/server/agent/prompt.go`) changes how the model behaves: run `agent-check` after.
- **Gemini spend is logged.** `agent-check` prints an estimate per run and appends it to `.dev/check/spend.log`. Fixture runs are short on purpose (about $0.03 each); the hour-long soak costs well under a dollar; run it when the reconnect or rotation code changes.
- **Migrations are append-only.** Never edit a migration that has run anywhere; add the next number.
- **The dev server is for a private network.** `huddled serve` refuses to run without `HUDDLE_DEV=1` until signing in exists (stage 3): anyone who reaches it can join any room.
- **Graphics shaders use no storage buffers.** After editing `shaders/src`, run `make shaders` and commit the outputs and `manifest.json`.
- **Colors come from `theme.Palette`.** Sizes are logical units.
- **Exit checks are run, not asserted.** Look at what you built: render it and read the PNG, run it against the local stack and read the log.
- **Copied code** keeps its one-line provenance comment. Code adapted from LiveKit (Apache 2.0: `internal/server/sfu` and `internal/rtc/sfuclient`) keeps LiveKit's copyright and license header, then one line naming the file and version it comes from and what changed; `NOTICE` and `LICENSES/Apache-2.0.txt` cover it, and the app carries both.

## Decisions not to undo
Each looked simpler the other way and failed when measured; the reasons are in `docs/design.md`.
- **Opus is libopus transpiled to Go**, not pion/opus, which has no encoder and neither FEC nor real concealment.
- **Every voice's RTP timestamp follows the clock**, through DTX and mute, and the whiteboard sends a silent frame every 400 ms between answers: a timestamp that stands still holds a listener's jitter buffer at 300 ms.
- **The mic falls back to running without echo cancelling** when voice processing won't start on a device, rather than leaving the person listening only.
- **Our pacer, not pion's:** the voice goes out the moment it's written, video at 1.2× the estimate the layers were given from (the cap, never less than what they were given). The layers get at most 1.5× what was sent plus 100 kbps, since pion's GCC climbs while less is sent than it allows.
- **No RTX, and transport-wide numbers on video only:** no RTX because pion's NACK responder never writes repaired-rid, so the SFU couldn't tell which layer an RTX stream repairs; no transport-wide numbers on the voice because the pacer sends it ahead of queued video, and GCC took the departures running backwards for congestion.
- **A paused layer comes back through a probe** (SEI filler padding the highest running layer), not by starting the paused layer, which switched viewers up and back.
- **Video is put back together by `room`'s assembler**, not pion's samplebuilder, and a hole is waited for 250 ms from when it showed, not from when the assembler reached it (that fell 23 s behind at 20% loss).
- **Lip sync compares arrival clocks**, not sender reports, which come late and wander with the encoder.
- **The ears send a person's next utterance only after the last one's final** (4 s at most); otherwise the transcriber folds both into one.

## Baselines
On huddled's SFU on a quiet Mac (the step 4 and 5b runs; every run is in `docs/history.md`). Single runs spread by about 0.03 in speech scores and a few ms in latency.
- `make voice-check`: click latency median 62 ms, p95 72 ms; speech 0.917 clean, 0.877 at 5% loss and 0.854 at 10% (with 40 ms jitter).
- `huddle video`: 720p30 capture → shown median 24–25 ms, p95 30–37 ms; 720p 25–27 ms after asking; 5%, 10% and 20% loss with 40 ms jitter median about 110, 130 and 160 ms, no freeze; lip sync median within 8 ms, worst within 18 ms; at 1 Mbps up 720p paused and the voice 0.90–0.92.
- Load, five `bot cam`s, a share and three viewers: huddled at about 9% of a core and 84 MB.
- `make agent-check MEETING=review`: 6 of 6 heard, 0 false, 0 writes without the gate, silent on 4 of 4 drawing commands, spoke on 2 of 2 questions, first op median 1.3–1.5 s.

## Commands
```
make check          # vet + test + shader manifest + no-cgo + build
make dev-up         # Postgres in Docker (dev-down stops it)
make test-db        # the store's tests against the dev Postgres's test database
make server         # huddled on :8080 in dev mode, against the dev Postgres, its media through its own SFU (LAN_IP on UDP 8080)
make app            # build/Huddle.app, signed with SIGN from local.mk (ad hoc without)
make run ROOM=test  # the signed app joined to ROOM on SERVER (user only: it opens the mic)
make notarize       # notarize and staple build/Huddle.app (NOTARY_PROFILE, or NOTARY_KEY_FILE, NOTARY_KEY_ID and NOTARY_ISSUER, in local.mk)
make render ARGS="-fixture testdata/workflows/orders.jsonl -out .dev/out/x.png [-dark -agent working -agent-detail '…' -queued 1 -conn Connected]"
export HUDDLE_DATA_DIR=$PWD/.dev/data   # first, when running ./build/huddle or ./build/huddled directly (make sets it): without it the app's and the tools' logs and caches go to the installed app's folders, and `huddled serve` logs to stderr alone
DATABASE_URL=... ./build/huddled put-board -room test -fixture testdata/workflows/trip.jsonl   # dev: the next version, seen on join
scripts/voice-fixtures.sh   # speech fixtures (macOS voices + ffmpeg) into .dev/fixtures: speech.wav, samantha.ogg, daniel.ogg, commands.wav (git-ignored)
./build/huddle voice -in .dev/fixtures/commands.wav -room ears -name Dan -server http://localhost:8080   # the ears through the app's own mic path (DTX, pauses, room noise); then grep "agent: " in serve.log
make bot ARGS="echo" ROOM=test                                  # the echo bot, let in by SERVER's dev join (make server running; Ctrl-C to stop)
make bot ARGS="play -ogg .dev/fixtures/samantha.ogg" ROOM=test  # a fixture participant
make bot ARGS="cam -name Ann" ROOM=test     # a camera of the test pattern (Ctrl-C to stop); ARGS="cam -name Bob -share" a screen
make voice-check SERVER=http://localhost:8080   # the voice gate: click latency, speech at 0/5/10% loss + 40 ms jitter; WAVs in .dev/out
./build/huddle video -in pattern -out .dev/out/v -room r -server URL [-for 20s] [-kind screen] [-screen-too] [-sim-loss 0.05 -sim-jitter 40ms] [-uplink 1M] [-voice speech.wav] [-flash-every 1s] [-want 320x180|off -switch 1280x720@8s] [-downlink 1M [-downlink-for 20s]] [-loop=false] [-trace]   # the video path with no camera (a receive-only viewer is -out with no -in): frames.csv (capture → last packet → put together → shown, the lip-sync hold), estimate.csv, PNGs; -trace adds packets.csv, units.csv, plis.csv
./build/huddle video-probe [-out .dev/out/video-probe]   # VideoToolbox end to end on the test pattern; no camera, no screen
./build/huddle voice -in x.wav -out y.wav -room r -server URL [-sim-loss 0.05 -sim-jitter 40ms -sim-reorder 0.01] [-for 20s] [-arrivals x.csv]   # file mode, no devices; -arrivals logs every received packet, for tracing a jitter buffer
./build/huddle audio-devices [-speakers NAME]    # devices (with UIDs), the voice-processing path, the speakers opened alone (on NAME: a virtual output keeps the room quiet)
HUDDLE_AUDIO_TEST=1 go test ./cmd/huddle -run SwitchSpeakers -v   # switch the call's speakers between two virtual outputs mid-stream
make render ARGS="-people 'Alex,Whiteboard!,Sam' -mic noecho -devices -out .dev/out/menu.png"   # the device menu on made-up devices
make render ARGS="-cams 'You,Ann,Bob*' -cam-off Cy -share Dee [-stage board -badge -muted Ann -camera on|off|busy|error -cam-err '…' -sharing on|off|busy|error -devices -no-cameras] -out .dev/out/x.png"   # video on made-up tracks; prints the size each track was told
./build/huddle tiles-bench [-tiles 6 -w 640 -h 360 -frames 300 -check -out dir]   # main-thread cost of each frame's tiles; -check compares the GPU's conversion with the CPU's
./build/huddle app -room test -no-voice -no-video -exit-after 8s </dev/null >/dev/null 2>&1   # agents: a short window run against make server, the room's audio listen-only (never without -no-voice -no-video; add -server for a worktree's port); grep -E "conn:|voice:|audio:|video:" .dev/data/logs/app.log
HUDDLE_AUDIO_TEST=1 go test ./internal/platform/audio -run SpeakerOnly -v   # the player path on the real speakers (silence)
make live-probe [ARGS="-only brain"]          # ask the models what the whiteboard relies on (~$0.10)
make fixtures                                 # render testdata/meetings/*.txt to per-speaker Ogg Opus (macOS say + ffmpeg; git ignores the audio)
make agent-check MEETING=review [ARGS="-voice muted"]   # score the whiteboard on a fixture: a fresh room, bots, the whole server in-process; report in .dev/check/
make meet ROOM=demo MEETING=review            # dev: bots play a fixture into ROOM on a running make server
./build/huddled type -server http://localhost:8080 -room demo "sort the steps alphabetically"   # dev: a typed command, prints the versions it made
./build/huddle render -version .dev/check/<run>/final.json -out .dev/out/x.png   # draw a stored version as an app would
```
Logs: the app, `voice`, `voice-check` and `video` write `.dev/data/logs/<subcommand>.log` (`conn:`, `command:`, `voice:`, `audio:`, `devices:`, `video:`), and the other tools print to the terminal; the server writes stderr, and under `make server` `.dev/data/logs/serve.log` too (`serve:`, `api:`, `ws:`, `agent:`, `sfu:`). What each line means: `docs/design.md`, "Logs". `agent-check` writes `server.log`, `events.json`, `report.txt`, `final.json` and two renders to `.dev/check/<meeting>-<time>/`.

Worktrees: a `local.mk` can point a worktree at its own databases and port (`DATABASE_URL`, `TEST_DATABASE_URL`, `SERVER`, `export HUDDLE_ADDR = :8082`).

## Conventions
- **Commits:** conventional style (`feat(server): …`), one per logical step, authored as the repo's local git config says. Push only when the user says.
- **Docs:** when a step is done, change Status here by a line or two. Its full record (what was built, checked and measured) goes at the end of `docs/history.md`, and how things now work goes in `docs/design.md`. Keep this file short.
