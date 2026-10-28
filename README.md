# huddle

Meetings with an AI whiteboard. People talk; when someone says
"whiteboard, draw what we just discussed" (or types it), the AI draws a
diagram on a board everyone in the meeting shares. It draws only when asked,
can keep diagramming and taking notes as the meeting goes, and answers aloud
when asked a direct question. Every change to the board is a version on the
meeting's timeline.

A native macOS app in Go, drawn with our own renderer on SDL3's GPU API
(Metal), with no cgo: native libraries load through
[purego](https://github.com/ebitengine/purego). Media goes through a
self-hosted [LiveKit](https://livekit.io) server; the whiteboard runs on
the server with Gemini's realtime models.

**Status:** early. Stage 1 (voice and the whiteboard, on a local network) is
being built; see [docs/plan.md](docs/plan.md).

## Building

Needs Go (the toolchain in `go.mod` is fetched automatically), Docker for
Postgres, and `brew install livekit`.

```
make check      # vet, tests, shader manifest, no-cgo build
make dev-up     # Postgres
make livekit    # LiveKit, in another terminal
make server     # the server, in another terminal (dev mode, private networks only)
make run ROOM=test
```

The whiteboard needs a Gemini API key in `.env` (see `.env.example`).
Signing settings go in `local.mk` (see `local.mk.example`).

## License

MIT
