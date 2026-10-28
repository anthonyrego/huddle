# huddle: notes for coding agents

huddle is meeting software for macOS: people meet with voice (camera and screen share come later), and an AI whiteboard draws on a board everyone shares, but only when someone asks it to ("whiteboard, diagram what we just discussed, alphabetically"). It can also diagram and take notes as the meeting goes, on request, and it answers aloud when someone asks it a direct question. Only the AI writes to the board; every change is a version on the meeting's timeline.

Two binaries: `huddle`, the app (Go on SDL3 through `github.com/Zyko0/go-sdl3`, purego, no cgo, with our own GPU renderer), and `huddled`, the server (HTTP and a meeting WebSocket over Postgres; LiveKit carries the media). The engine is copied from an earlier project of the author's; per-file provenance lines say so.

- **Plan:** `docs/plan.md`. Stage 1 is Step 0 (this base) → Track V (voice through LiveKit, pure Go) and Track A (the AI whiteboard, server first), built in parallel worktrees → integration on two Macs. Each part ends with an agent gate and a gate the user runs.
- **Status:** Step 0 built (2026-10-27): the app window and `render`, the server (`serve`, `migrate`, `put-board`), the store with migrations, the WebSocket hub, the dev join, the signed app bundle with SDL in `Contents/Frameworks` under the hardened runtime, CI. Checked: `make check`; `make test-db`; a dev join and an app run against local LiveKit and the server (the app got the board version `put-board` stored); the Developer ID–signed bundle runs and loads its own SDL. Waiting on the user's gate: `make run`, and `make notarize` then the app opened on the second Mac.
- **Status, Track V (voice), 2026-10-27:** built on branch `track/v-voice`: pure-Go Opus (`internal/opus`), the jitter buffer, the playout mix, LiveKit rooms (`internal/rtc/lkrtc`), the 48 kHz voice-processing path, the file mode (`huddle voice`, `huddle voice-check`), the bots (`huddled bot echo|play`), and voice in the window (the bar's people, talking dots and mute). Checked: `make check`; `make test-db`; `voice-check` through local LiveKit: click latency median 70 ms, p95 70 ms (file to file: packetizing, Opus, transit, jitter buffer, mix; no devices); speech correlation with the source, a second at a time, 0.914 clean (the codec's own ceiling at 32 kbps), 0.882 at 5% loss and 0.891 at 10% loss with 40 ms jitter (FEC recovered 96% and 91% of the losses, none late), WAVs in `.dev/out/`; `audio-devices` showed the voice path and played through the speakers alone with the mic never running; a `-no-voice -exit-after` window run among the bots heard both fixtures at a 40 ms jitter delay with no loss. Waiting on the user's gate (below, "Voice"): device latency through voice processing is his to measure.

## Packages
| Package | What it does |
|---|---|
| `internal/wire` | What the app and the server say to each other: WebSocket envelopes (`hello`, `welcome`, `board.version`, `board.ops`, `agent.status`, `command.typed`, `command.address`, `transcript.segment`, `error`) and the dev join. `Protocol` is checked on hello. Pure. |
| `internal/rtc` | The app's side of the media, as an interface: `Connector`, `Room`, `Mic`, `Speaker`, events. Audio crosses it as 48 kHz mono float32 in 10 ms frames. Pure. |
| `internal/rtc/lkrtc` | `rtc.Connector` on LiveKit's Go SDK (lksdk's root package only). Publishes the mic as Opus through an lksdk sample track with the RFC 6464 level on every packet (the SFU's speaker detection reads it; LiveKit doesn't forward it to subscribers) and the RTP timestamp following the mic (DTX and mute read as time going by; the first DTX frame of a silence is sent, then one every 400 ms); reads each remote audio track as raw RTP into its own jitter buffer; mixes them with `playout`. Logs each track's numbers every 10 s (`voice: track`). `Connector.Sim` makes the receive side worse through a pion interceptor. Pure. |
| `internal/rtc/fileio` | A WAV mic and a recording speaker, paced on the wall clock 10 ms a frame, for runs with no devices. Pure. |
| `internal/opus` | Opus at 48 kHz mono, 20 ms frames, in-band FEC, DTX: one `Encoder`/`Decoder` interface over libopus 1.6 transpiled to Go (`kazzmir/opus-go`, the one used) and `pion/opus`'s decoder. Every call into the transpiled code runs on one pinned worker goroutine (`pin.go`; see Rules). `PacketSamples` reads a packet's length. Pure. |
| `internal/jitter` | One remote track's jitter buffer: reorders by sequence number; target delay = 95th percentile of arrival delay over 5 s plus a frame, 40–300 ms; FEC from the next packet, else libopus concealment; late packets dropped; DTX silence played and the part past the target skipped; bursts past the target trimmed; `Stats`. Pure. |
| `internal/netsim` | Loss, jitter (uniform 0–2J, in order) and reordering, seedable (`Sim.Fate`) or in real time (`Pipe`). Pure. |
| `internal/playout` | Mixes every track into 10 ms frames, paced by the speaker's blocking `Write` (the device's clock), soft-limited; per-participant levels for talking dots; follows each buffer's target by skipping a quiet frame or playing silence in place of one (never during speech, at most one frame in five). Pure. |
| `internal/board` | The board: nodes (with kinds), links, groups, notes. Ops are the tool calls; `Apply` checks an op and applies it only if valid, with errors written for the model; `Replay` rebuilds a board from ops; `Snapshot` (the ops that rebuild a version) is a version's `Doc` on the wire and in the store; `ReadOps` reads fixtures; `Digest` is what `get_board` returns; `Tools` declares the tools (their descriptions are the model's manual). Pure. |
| `internal/board/layout` | Left-to-right layout with several roots, seeded from the previous layout so an add moves as little as possible. Pure. |
| `internal/cue` | Holds ops until they should be seen, one per gap. Pure. |
| `internal/live` | One Gemini Live session (`google.golang.org/genai`): a writer goroutine owns every send, a reader turns server messages into ordered `Event`s. Pure. |
| `internal/pcm` | Mono s16 helpers: conversions, dBFS levels, the speech `Detector`; float32 conversions and levels, `Correlation`, `BestLag`, `Segmental` (scores speech whose delay moves), click trains and `Onsets`. Pure. |
| `internal/server/store` | Postgres through pgx: embedded, append-only migrations (`migrations/NNNN_name.sql`, run under an advisory lock); meetings; immutable board versions with `Commit(expectHead)` (`ErrHeadMoved` when another commit got there first); the ops of each version; transcript segments; the whiteboard's resumption state. Pure. |
| `internal/server/wshub` | The meeting WebSocket: hello (checked against `wire.Protocol`), then the welcome and the head version, then broadcasts; what apps send goes to `OnMessage`. A client that can't keep up is dropped. Pure. |
| `internal/server/httpapi` | `GET /healthz`, `POST /dev/join` (dev mode only: a LiveKit token and the WebSocket URL for any name in any room), `GET /ws`. Pure. |
| `internal/server/lktoken` | LiveKit access tokens: one room, one identity, short-lived. Pure. |
| `internal/ui/boardview` | The board widget: cards, frames, arrows, glides and fades, `point_at` glow, a camera that fits the board until the user pans or zooms. Pure. |
| `internal/ui/transcript` | A transcript panel. Pure. |
| `internal/ui` | Input `Event`s, the `Widget` contract, the `Host`, focus layers, the `TextField`. Pure. |
| `internal/draw`, `internal/geom`, `internal/text`, `internal/theme` | The display list, path geometry, text (go-text shaping, embedded Inter and JetBrains Mono, system fallback), color tokens. Pure. |
| `internal/gfx` | GPU renderer: device and window, the uber-shader pipeline, glyph atlases, `Batch`, `RenderImage` (offscreen). |
| `internal/platform` | SDL loading (the bundle's own `Contents/Frameworks` first, then the system, then the copy in the binary) and init, `QuitOnSignal`, `Wake`, macOS Reduce Motion and the Dock badge. |
| `internal/platform/audio` | Mic and speakers. `OpenVoice`: macOS voice processing on AVAudioEngine through purego/objc (`vp_darwin.go`), float32 mono at rates given as parameters (48 kHz), as `rtc.Mic`/`rtc.Speaker`. The speakers: each 10 ms frame is a buffer on an AVAudioPlayerNode, and Write blocks while 50 ms waits to be rendered (paced by the player's completions). The mic: the tap delivers 100 ms at a time, released evenly 10 ms a frame (`paced.go`). The restart watcher keeps the engine on the default devices. `SpeakerOnly` never touches the input node: no mic, no voice processing. `Defaults`/`InputRunning` read CoreAudio without opening anything. Also SDL capture/playback streams. |
| `internal/config`, `internal/logx` | Data and log paths (`HUDDLE_DATA_DIR` overrides the data dir); a log file per subcommand plus crash output. |
| `shaders/` | HLSL sources in `src/`, compiled SPIR-V + MSL in `compiled/`, and `manifest.json`. |
| `cmd/huddle` | The app. One file per subcommand, registered in `init()`: `app` (the default when opened from Finder), `render`, `audio-devices`, `voice` (file mode), `voice-check` (the voice gate's file-to-file runs). `call.go` is the voice session (joins LiveKit with the connection's dev-join token, opens the voice path once in, publishes and plays; its own lock, wakes the loop when what the bar shows changes); `meeting.go` is what the app knows of the meeting (written by the connection, taken by the scene on the main thread); `connect.go` the dev join and the WebSocket; `scene.go` the board stage and status pill (Track A's); `bar.go` the bottom bar (Track V's). |
| `cmd/huddled` | The server. `serve`, `migrate`, `put-board` (dev: store a fixture as the next version), `bot echo` (repeats each person 1.5 s later as its own track; never bots) and `bot play -ogg f.ogg` (a looping fixture participant), which join LiveKit with the key `make bot` passes. |

## Rules
- **This repo is public (MIT).** No personal details in files or commit messages: names, emails, machines, hardware names, certificate hashes, team IDs, private repository names. Code copied from the author's private projects says "Copied from an earlier project of the author's"; code from reel (public) may name it. `SIGN`, `NOTARY_*` live in `local.mk`, which git ignores. Scan the diff and the history before every push. Push only when the user says.
- **The API key is never printed, logged or committed.** It comes from `GEMINI_API_KEY`; the Makefile takes only that line from `HUDDLE_ENV` (default `.env`, which git ignores; `.env.example` shows the line).
- **Don't open the user's microphone or capture their screen from an agent.** `make run` and the app joined to a room (once voice lands) are the user's to run. Agents use fixtures, file-to-file runs, `render`, and `audio-devices` (lists devices, opens only the speakers).
- **Never touch the user's real data.** Dev runs set `HUDDLE_DATA_DIR` to `.dev/data` (the Makefile does).
- **No cgo.** Native code is loaded with purego only; builds use `CGO_ENABLED=0` and `make nocgo` checks the binaries build that way. Never import `github.com/livekit/server-sdk-go/v2/pkg/media` (it pulls libopus through cgo).
- **Agents' window runs use `-no-voice`.** It joins the room's audio listen-only: the speakers alone, the input node never touched. `audio-devices` opens only the speakers too, and says whether any process had the mic running. Voice on (the default) is the user's run.
- **Every call into the transpiled libopus goes through `opus.onPinned`.** The transpiled code passes Go stack addresses as uintptrs; a call that grows a moved stack reads garbage (an abort in SILK after concealment, only with the GC running). The pinned worker's stack is grown once and never shrinks. `TestConcealUnderGC` is the regression test. Under `-race`, Go's checkptr trips inside the transpiled code: race-test with `go test -race -gcflags=all=-d=checkptr=0`.
- **Go stays off the real-time audio thread.** AVAudioEngine taps and player completions run on the engine's own threads and only copy and signal. No AVAudioSinkNode or render callbacks from Go.
- **The core stays pure.** Everything under `internal/` except `gfx` and `platform` never imports SDL; `internal/deps/purity_test.go` enforces this; add new pure packages to its list.
- **Only the main thread touches the GPU.** Other goroutines write shared state under a lock and call `platform.Wake()` (see `meeting.update`).
- **Only the server changes the board.** Apps send commands and render versions; a version's layout comes from the server once Track A lands, so every app and every replay draws the same picture.
- **Migrations are append-only.** Never edit a migration that has run anywhere; add the next number.
- **The dev server is for a private network.** `huddled serve` refuses to run without `HUDDLE_DEV=1` until signing in exists (stage 3): anyone who reaches it can join any room.
- **Graphics shaders use no storage buffers.** After editing `shaders/src`, run `make shaders` and commit the outputs and `manifest.json`.
- **Colors come from `theme.Palette`.** Sizes are logical units.
- **Exit checks are run, not asserted.** Look at what you built: render it and read the PNG, run it against the local stack and read the log.
- **Copied code** keeps its one-line provenance comment.

## Commands
```
make check          # vet + test + shader manifest + no-cgo + build
make dev-up         # Postgres in Docker (dev-down stops it)
make test-db        # the store's tests against the dev Postgres's test database
make livekit        # LiveKit on this Mac, reachable from the LAN (brew install livekit); LAN_IP=... to override the address
make server         # huddled on :8080 in dev mode, against the dev Postgres and LiveKit
make app            # build/Huddle.app, signed with SIGN from local.mk (ad hoc without)
make run ROOM=test  # the signed app joined to ROOM on SERVER (user only once voice lands)
make notarize       # notarize and staple build/Huddle.app (NOTARY_PROFILE or NOTARY_KEY_* in local.mk)
make render ARGS="-fixture testdata/workflows/orders.jsonl -out .dev/out/x.png [-dark -agent working -agent-detail '…' -queued 1 -conn Connected]"
./build/huddle app -room test -exit-after 6s </dev/null >/dev/null 2>&1   # agents: a short window run against make server; read .dev/data/logs/app.log
DATABASE_URL=... ./build/huddled put-board -room test -fixture testdata/workflows/trip.jsonl   # dev: the next version, seen on join
scripts/voice-fixtures.sh   # speech fixtures (macOS voices + ffmpeg) into .dev/fixtures: speech.wav, samantha.ogg, daniel.ogg (git-ignored)
make bot ARGS="echo" ROOM=test                                  # the echo bot (Ctrl-C to stop)
make bot ARGS="play -ogg .dev/fixtures/samantha.ogg" ROOM=test  # a fixture participant
make voice-check SERVER=http://localhost:8080   # the voice gate: click latency, speech at 0/5/10% loss + 40 ms jitter; WAVs in .dev/out
./build/huddle voice -in x.wav -out y.wav -room r -server URL [-sim-loss 0.05 -sim-jitter 40ms -sim-reorder 0.01] [-for 20s]   # file mode, no devices
./build/huddle audio-devices                     # devices, the voice-processing path, the speakers opened alone
./build/huddle app -room test -no-voice -exit-after 8s </dev/null >/dev/null 2>&1   # agents: a window run in a room's audio, listen-only; grep "voice:|audio:" .dev/data/logs/app.log
HUDDLE_AUDIO_TEST=1 go test ./internal/platform/audio -run SpeakerOnly -v   # the player path on the real speakers (silence)
```
Logs: `.dev/data/logs/<subcommand>.log` for the app (`conn: joined`, `conn: welcome`, `conn: version`, `conn: lost`) and voice (`voice: joined`, `voice: hearing`, `voice: track` every 10 s with `jitterDelay`, `lost`, `fec`, `plc`, `late`, `underrunMs`; `voice: devices`; `audio: voice processing on`, `audio: voice processing mic frames=… every=…`, `audio: the engine restarted after a device change`); the server logs to stderr (`make server`): `api: dev join`, `ws: joined`, `ws: left`.

## Voice (Track V)
**The path.** Mic → voice processing (AVAudioEngine; the tap delivers 100 ms at a time, released 10 ms a frame) → Opus (20 ms, FEC, DTX) → lksdk sample track with the RFC 6464 level → LiveKit → each listener: raw RTP → (optional `Sim` interceptor) → jitter buffer → playout mix (10 ms, paced by the speaker) → AVAudioPlayerNode (50 ms queued) → speakers, where voice processing hears it and cancels it from the mic.

**Decisions, and why.**
- **Opus: libopus transpiled to Go (`kazzmir/opus-go`).** pion/opus has no public encoder (its CELT encoder is internal), so encoding had to be libopus either way; for decoding, the measurements below favour libopus too: real concealment and in-band FEC, where pion has neither (ours repeats the last frame, fading). Speech fixture, on the development Mac, `go test ./internal/opus -bench . -run Concealment -v`:

  | | libopus | pion |
  |---|---|---|
  | encode, 20 ms frame (complexity 9) | 0.48 ms (2.4% of a core; 1.3% at complexity 5) | no encoder |
  | decode, 20 ms frame | 44 µs (0.22%) | 55 µs (0.28%) |
  | conceal, 20 ms | 41 µs | 1 µs (repeat-fade) |
  | correlation with the source, 5% loss | 0.913 with FEC, 0.881 PLC only | 0.843 |
  | same, 10% loss | 0.909 with FEC, 0.848 PLC only | 0.762 |
  | lost frames vs the clean decode, 10% | 0.937 with FEC, 0.647 PLC only | -0.10 |

  The transpiled code's moving-stack bug (Rules) is contained by the pinned worker; `opus.Chosen` switches the decoder to pion in one line if it ever bites again.
- **lksdk takes our own pion interceptors**: `lksdk.WithInterceptors(...)`, and `WithIncludeDefaultInterceptors(true)` keeps its own (NACK, RTCP reports, TWCC, the RTT ones), which giving any custom list otherwise drops. Ours go in first, nearest the network, so everything after (RTCP receiver reports, the jitter buffer) sees what they do. The receive-side network simulation is one. lksdk also registers the RFC 6464 audio-level extension for audio itself; we write the level on every packet through `SampleWriteOptions.AudioLevel` (lksdk's `LocalTrack` can; pion's `TrackLocalStaticSample` can't), and the SFU's active-speaker levels show it arrives (`sfuSpeakerLevel` in `voice: track`).
- **Voice processing stays on AVAudioEngine with Go off the real-time thread**, so the mic comes from a tap, and taps come 100 ms at a time on this hardware whatever is asked (4800 frames every 100 ms in the earlier project's logs). Sent as bursts, those look like 80 ms of jitter to every receiver; `paced` releases them evenly instead, which costs nothing the chunking hasn't already (~90 ms on the mic side). That is the largest single latency in the path; lower would need native code on the render thread (an AVAudioSinkNode block or a render callback), which purego can only give us as Go.
- **The speakers are paced by the player's completions** (`DataRendered`), 10 ms per buffer, 40–50 ms queued; Write gives up waiting after 200 ms and drops past 500 ms queued, so a stopped engine never stalls the mix (the watchdog restarts it).

**The user's gate.** Postgres and LiveKit running (`make dev-up`, `make livekit`); `scripts/voice-fixtures.sh` once for the fixtures. Logs: `.dev/data/logs/app.log` for `make run`; `~/Library/Logs/Huddle/app.log` for the app opened on its own.
1. **Alone, with the echo bot.** You hear yourself once, about 1.5 s later; never a second, quieter echo 3 s later (that would be voice processing failing to cancel the echo it played).
   ```
   make server                    # terminal 1
   make bot ARGS="echo"           # terminal 2 (ROOM defaults to test)
   make run                       # terminal 3: talk, then quit the app
   grep -E "audio: voice processing (on|mic)|voice: (joined|hearing|track)" .dev/data/logs/app.log | tail
   ```
   Optional company while alone: `make bot ARGS="play -ogg .dev/fixtures/samantha.ogg"` in a fourth terminal (you hear her; her dot lights in the bar). `m` or a click on the mic pill mutes.
2. **Two Macs in the same room, speakers on, no headphones.** On Mac A (runs the stack): `make dev-up`, `make livekit`, `make server`, `make lan-ip` (note the address), then `make run`. On Mac B, the notarized app (`make notarize` on A, copy `build/Huddle.app`): `open -n Huddle.app --args app -server http://<A's address>:8080 -room test`. Talk: both hear each other, neither hears their own voice come back, latency feels like a call. Then on each: `grep "voice: track" <log> | tail -2` (A: `.dev/data/logs/app.log`, B: `~/Library/Logs/Huddle/app.log`) for the other side's `jitterDelay`, `lost`, `plc`, `underrunMs`.
3. **Two Macs in separate rooms.** The same commands, the Macs in different rooms; the logs show each side's jitter-buffer delay (`jitterDelay`) every 10 s.

## Conventions
- **Commits:** conventional style (`feat(server): …`), one per logical step, authored as the repo's local git config says. Push only when the user says.
