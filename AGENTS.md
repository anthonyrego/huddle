# huddle: notes for coding agents

huddle is meeting software for macOS: people meet with voice (camera and screen share come later), and an AI whiteboard draws on a board everyone shares, but only when someone asks it to ("whiteboard, diagram what we just discussed, alphabetically"). It can also diagram and take notes as the meeting goes, on request, and it answers aloud when someone asks it a direct question. Only the AI writes to the board; every change is a version on the meeting's timeline.

Two binaries: `huddle`, the app (Go on SDL3 through `github.com/Zyko0/go-sdl3`, purego, no cgo, with our own GPU renderer), and `huddled`, the server (HTTP and a meeting WebSocket over Postgres; LiveKit carries the media). The engine is copied from an earlier project of the author's; per-file provenance lines say so.

- **Plan:** `docs/plan.md`. Stage 1 is Step 0 (this base) → Track V (voice through LiveKit, pure Go) and Track A (the AI whiteboard, server first), built in parallel worktrees → integration on two Macs. Each part ends with an agent gate and a gate the user runs.
- **Status:** Step 0 built (2026-10-27): the app window and `render`, the server (`serve`, `migrate`, `put-board`), the store with migrations, the WebSocket hub, the dev join, the signed app bundle with SDL in `Contents/Frameworks` under the hardened runtime, CI. Checked: `make check`; `make test-db`; a dev join and an app run against local LiveKit and the server (the app got the board version `put-board` stored); the Developer ID–signed bundle runs and loads its own SDL. Waiting on the user's gate: `make run`, and `make notarize` then the app opened on the second Mac.

## Packages
| Package | What it does |
|---|---|
| `internal/wire` | What the app and the server say to each other: WebSocket envelopes (`hello`, `welcome`, `board.version`, `board.ops`, `agent.status`, `command.typed`, `command.address`, `transcript.segment`, `error`) and the dev join. `Protocol` is checked on hello. Pure. |
| `internal/rtc` | The app's side of the media, as an interface: `Connector`, `Room`, `Mic`, `Speaker`, events. Audio crosses it as 48 kHz mono float32 in 10 ms frames. Track V implements it with LiveKit's Go SDK. Pure. |
| `internal/board` | The board: nodes (with kinds), links, groups, notes. Ops are the tool calls; `Apply` checks an op and applies it only if valid, with errors written for the model; `Replay` rebuilds a board from ops; `Snapshot` (the ops that rebuild a version) is a version's `Doc` on the wire and in the store; `ReadOps` reads fixtures; `Digest` is what `get_board` returns; `Tools` declares the tools (their descriptions are the model's manual). Pure. |
| `internal/board/layout` | Left-to-right layout with several roots, seeded from the previous layout so an add moves as little as possible. Pure. |
| `internal/cue` | Holds ops until they should be seen, one per gap. Pure. |
| `internal/live` | One Gemini Live session (`google.golang.org/genai`): a writer goroutine owns every send, a reader turns server messages into ordered `Event`s. Pure. |
| `internal/pcm` | Mono s16 helpers: conversions, dBFS levels, the speech `Detector`. Pure. |
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
| `internal/platform/audio` | Mic and speakers: SDL streams, and macOS voice processing (`vp_darwin.go`, AVAudioEngine through purego/objc). Track V reworks it to 48 kHz. |
| `internal/config`, `internal/logx` | Data and log paths (`HUDDLE_DATA_DIR` overrides the data dir); a log file per subcommand plus crash output. |
| `shaders/` | HLSL sources in `src/`, compiled SPIR-V + MSL in `compiled/`, and `manifest.json`. |
| `cmd/huddle` | The app. One file per subcommand, registered in `init()`: `app` (the default when opened from Finder), `render`, `audio-devices`. `meeting.go` is what the app knows of the meeting (written by the connection, taken by the scene on the main thread); `connect.go` the dev join and the WebSocket; `scene.go` the board stage and status pill (Track A's); `bar.go` the bottom bar (Track V's). |
| `cmd/huddled` | The server. `serve`, `migrate`, `put-board` (dev: store a fixture as the next version). |

## Rules
- **This repo is public (MIT).** No personal details in files or commit messages: names, emails, machines, hardware names, certificate hashes, team IDs, private repository names. Code copied from the author's private projects says "Copied from an earlier project of the author's"; code from reel (public) may name it. `SIGN`, `NOTARY_*` live in `local.mk`, which git ignores. Scan the diff and the history before every push. Push only when the user says.
- **The API key is never printed, logged or committed.** It comes from `GEMINI_API_KEY`; the Makefile takes only that line from `HUDDLE_ENV` (default `.env`, which git ignores; `.env.example` shows the line).
- **Don't open the user's microphone or capture their screen from an agent.** `make run` and the app joined to a room (once voice lands) are the user's to run. Agents use fixtures, file-to-file runs, `render`, and `audio-devices` (lists devices, opens only the speakers).
- **Never touch the user's real data.** Dev runs set `HUDDLE_DATA_DIR` to `.dev/data` (the Makefile does).
- **No cgo.** Native code is loaded with purego only; builds use `CGO_ENABLED=0` and `make nocgo` checks the binaries build that way. Never import `github.com/livekit/server-sdk-go/v2/pkg/media` (it pulls libopus through cgo).
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
```
Logs: `.dev/data/logs/<subcommand>.log` for the app (`conn: joined`, `conn: welcome`, `conn: version`, `conn: lost`); the server logs to stderr (`make server`): `api: dev join`, `ws: joined`, `ws: left`.

## What the models do (live-probe, 2026-10-27)
`make live-probe` (`huddled live-probe [-only brain|transcribe] [-auto-schedule]`) asks the real models; about $0.10 a run. Findings:
- **`gemini-3.8-live` takes commands either way, mid-session.** Ten commands alternating client content (`Session.SendTurn`) and realtime text (`SendText`) in one session all drew: first tool call 0.35–1.0 s after the send. Neither path broke tool calls after the other was used.
- **A whole turn's tool calls come first, in one message, then audio.** On a question the `speak` call arrives at 0.36–0.42 s and the first audio at 0.86–1.3 s, after `speak` is answered. So the agent knows a turn will be spoken before any audio arrives.
- **Answer `speak` `WHEN_IDLE`, never `SILENT`.** A `SILENT` answer to `speak` left the question unanswered, and the model said that answer aloud during a later, unrelated command.
- **Answer successful drawing calls `SILENT`.** Answered `WHEN_IDLE`, a drawing turn starts another generation per answer: one command repeated the same three `add_node` calls six times, and another spoke filler ("<no speech>{pause}"). Answered `SILENT`, every drawing command came as one batch with `turn-complete` at the same moment, and no audio. A rejected call, `get_board` and `speak` are answered `WHEN_IDLE`, so the model can fix the call, use the read or speak (a `get_board` then a `connect` came 0.37 s apart).
- **A turn is over at a `turn-complete` with no `WHEN_IDLE` answer outstanding.**
- **`gemini-3.5-transcribe-live` takes 16 kHz s16 PCM in 100 ms chunks and returns text** (setup: TEXT modality, input transcription on). Interim hypotheses (`interimInputTranscription`) come about every 0.5 s while someone talks; the final (`inputTranscription`, then `generation-complete`) comes 0.35–0.58 s after the speech ends when the client sends `audioStreamEnd` at its own VAD's end of speech, and 1.4 s after with the server's VAD alone. The final is punctuated ("Whiteboard, draw our release process: build, then test, then deploy."); "White board" came back as "Whiteboard".

## Conventions
- **Commits:** conventional style (`feat(server): …`), one per logical step, authored as the repo's local git config says. Push only when the user says.
