# Dev builds keep their data in .dev/ so they never touch the data the
# installed app uses. Override with HUDDLE_DATA_DIR=... make run.
HUDDLE_DATA_DIR ?= $(CURDIR)/.dev/data
export HUDDLE_DATA_DIR

# Machine-specific settings (SIGN, NOTARY_PROFILE); see local.mk.example.
-include local.mk
SIGN ?= -

# The whiteboard needs GEMINI_API_KEY. When it isn't set in the
# environment, the targets below take that one line (never the rest of the
# file) from HUDDLE_ENV: .env here by default, which git ignores (see
# .env.example). The key is never echoed.
HUDDLE_ENV ?= $(CURDIR)/.env
KEY = GEMINI_API_KEY="$${GEMINI_API_KEY:-$$(sed -n 's/^GEMINI_API_KEY=//p' "$(HUDDLE_ENV)" 2>/dev/null | tail -1 | tr -d "\"'")}"

# This Mac's address on the LAN, which LiveKit advertises and the other Mac
# dials. Override with LAN_IP=... when it guesses wrong.
LAN_IP ?= $(shell ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || echo 127.0.0.1)

# The dev stack (docker-compose.yml, make livekit). Dev credentials only:
# livekit-server --dev uses devkey/secret.
DATABASE_URL ?= postgres://huddle:huddle@127.0.0.1:5442/huddle?sslmode=disable
TEST_DATABASE_URL ?= postgres://huddle:huddle@127.0.0.1:5442/huddle_test?sslmode=disable
LIVEKIT_URL ?= ws://$(LAN_IP):7880
SERVER ?= http://localhost:8080
ROOM ?= test

GO = CGO_ENABLED=0 go

.PHONY: build app run render test test-db vet shaders shaders-check nocgo check bot voice-check \
	dev-up dev-down livekit server notarize lan-ip live-probe fixtures agent-check meet

build:
	$(GO) build -o build/huddle ./cmd/huddle
	$(GO) build -o build/huddled ./cmd/huddled

# build/Huddle.app, signed with SIGN (ad hoc without it).
app: build
	@SIGN="$(SIGN)" ./scripts/make-app.sh build/huddle build/Huddle.app

# The meeting window in ROOM on SERVER (user only: it opens the mic; -no-voice
# keeps it shut). Flags via ARGS, e.g. make run ARGS="-theme dark".
run: app
	open -n --env HUDDLE_DATA_DIR="$(HUDDLE_DATA_DIR)" build/Huddle.app --args app -server "$(SERVER)" -room "$(ROOM)" $(ARGS)

# Draws the meeting window to a PNG, e.g.
# make render ARGS="-fixture testdata/workflows/orders.jsonl -out .dev/out/x.png"
render: build
	./build/huddle render $(ARGS)

test:
	go test ./...

# The store's tests against the dev Postgres's test database (make dev-up).
test-db:
	HUDDLE_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test ./internal/server/store -count=1 -v

vet:
	go vet ./...

# Rebuild shaders after editing shaders/src (needs: brew install glslang spirv-cross).
shaders:
	./scripts/build-shaders.sh

shaders-check:
	./scripts/check-shaders.sh

nocgo:
	./scripts/check-nocgo.sh

# Everything CI runs.
check: vet test shaders-check nocgo build

# Postgres in Docker.
dev-up:
	docker compose up -d --wait

dev-down:
	docker compose down

# LiveKit on this Mac, reachable from the LAN (brew install livekit).
livekit:
	livekit-server --dev --bind 0.0.0.0 --node-ip "$(LAN_IP)"

# The server on this Mac, reachable from the LAN, in dev mode. Its media
# goes through LiveKit (make livekit), or HUDDLE_MEDIA=builtin make server
# for its own SFU, which offers LAN_IP.
server: build
	@HUDDLE_DEV=1 DATABASE_URL="$(DATABASE_URL)" HUDDLE_NODE_IP="$(LAN_IP)" LIVEKIT_URL="$(LIVEKIT_URL)" \
		LIVEKIT_API_KEY=devkey LIVEKIT_API_SECRET=secret $(KEY) ./build/huddled serve $(ARGS)

# Notarizes build/Huddle.app (signed with a Developer ID certificate) and
# staples the ticket, so it opens on another Mac without a warning.
notarize: app
	ditto -c -k --keepParent build/Huddle.app build/Huddle.zip
	@if [ -n "$(NOTARY_PROFILE)" ]; then \
		xcrun notarytool submit build/Huddle.zip --keychain-profile "$(NOTARY_PROFILE)" --wait; \
	elif [ -n "$(NOTARY_KEY_FILE)" ]; then \
		xcrun notarytool submit build/Huddle.zip --key "$(NOTARY_KEY_FILE)" --key-id "$(NOTARY_KEY_ID)" --issuer "$(NOTARY_ISSUER)" --wait; \
	else \
		echo "set NOTARY_PROFILE, or NOTARY_KEY_FILE/NOTARY_KEY_ID/NOTARY_ISSUER, in local.mk"; rm build/Huddle.zip; exit 1; \
	fi
	xcrun stapler staple build/Huddle.app
	rm build/Huddle.zip
	spctl -a -t exec -vv build/Huddle.app

lan-ip:
	@echo $(LAN_IP)

# Dev participants in ROOM, let in by SERVER's dev join (make server running):
# make bot ARGS="echo"  |  make bot ARGS="play -ogg .dev/fixtures/samantha.ogg"
bot: build
	./build/huddled bot $(word 1,$(ARGS)) -server "$(SERVER)" -room "$(ROOM)" $(wordlist 2,99,$(ARGS))

# The voice gate's file-to-file runs through LiveKit (make server running):
# click latency, and speech at 0, 5 and 10% loss with 40 ms jitter, WAVs in .dev/out.
voice-check: build
	./build/huddle voice-check -server "$(SERVER)" $(ARGS)

# Asks the realtime and transcription models what the whiteboard relies on
# (a few cents). ARGS="-only brain" or "-only transcribe".
live-probe: build
	@$(KEY) ./build/huddled live-probe $(ARGS)

# Renders the fixture meetings (testdata/meetings/*.txt) with macOS voices
# into per-speaker Ogg Opus (git ignores the audio).
fixtures: build
	./build/huddled fixtures

# Scores the whiteboard on a fixture meeting against the local stack, e.g.
# make agent-check MEETING=review [ARGS="-voice answers"]
MEETING ?= review
agent-check: build
	@DATABASE_URL="$(DATABASE_URL)" HUDDLE_NODE_IP="$(LAN_IP)" LIVEKIT_URL="$(LIVEKIT_URL)" LIVEKIT_API_KEY=devkey LIVEKIT_API_SECRET=secret \
		$(KEY) ./build/huddled agent-check -meeting testdata/meetings/$(MEETING).txt $(ARGS)

# Dev: fixture bots join ROOM on SERVER and play MEETING (make fixtures
# first), for watching the whiteboard in the app.
meet: build
	./build/huddled meet -server "$(SERVER)" -room "$(ROOM)" -meeting testdata/meetings/$(MEETING).txt
