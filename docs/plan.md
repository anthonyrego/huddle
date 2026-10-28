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

## Later stages

1. Camera and screen share: VideoToolbox H.264 through purego, ScreenCaptureKit,
   congestion control, video tiles and a stage that switches between the
   screen and the board.
2. Accounts and meetings: Verify sign-in, meetings, scheduling, invites, join
   codes and a lobby, SMS invites (needs A2P 10DLC registration).
3. Recording and replay: per-speaker audio to object storage, mixdown, one
   meeting clock, a replay screen, consent at join.
4. Release: a notarized DMG on GitHub Releases, a version check, hardening.

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
