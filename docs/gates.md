# huddle: the user's gates

These are runs only the user makes: real microphones, cameras and screens, on two Macs. All of them passed on huddled's own SFU on 2026-10-29 (the media server gate, which reran the stage 1 and stage 2 gates). Rerun them after changing the media path, the devices or capture. An agent never runs them (AGENTS.md, Rules).

## Voice (Track V)
**The user's gate.** Postgres running (`make dev-up`); `scripts/voice-fixtures.sh` once for the fixtures. Logs: `.dev/data/logs/app.log` for `make run`; `~/Library/Logs/Huddle/app.log` for the app opened on its own.
1. **Alone, with the echo bot.** You hear yourself once, about 1.5 s later; never a second, quieter echo 3 s later (that would be voice processing failing to cancel the echo it played).
   ```
   make server                    # terminal 1
   make bot ARGS="echo"           # terminal 2 (ROOM defaults to test)
   make run                       # terminal 3: talk, then quit the app
   grep -E "audio: voice processing (on|mic)|voice: (joined|hearing|track)" .dev/data/logs/app.log | tail
   ```
   Optional company while alone: `make bot ARGS="play -ogg .dev/fixtures/samantha.ogg"` in a fourth terminal (you hear her; her dot lights in the bar). `m` or a click on the mic pill mutes.
2. **Two Macs in the same room, speakers on, no headphones.** On Mac A (runs the stack): `make dev-up`, `make server`, `make lan-ip` (note the address), then `make run`. On Mac B, the app copied from A (`scp -r build/Huddle.app <B>:`; no notarizing needed, see the stage 1 gate): `open -n Huddle.app --args app -server http://<A's address>:8080 -room test`. Talk: both hear each other, neither hears their own voice come back, latency feels like a call. Then on each: `grep "voice: track" <log> | tail -2` (A: `.dev/data/logs/app.log`, B: `~/Library/Logs/Huddle/app.log`) for the other side's `jitterDelay`, `lost`, `plc`, `underrunMs`.
3. **Two Macs in separate rooms.** The same commands, the Macs in different rooms; the logs show each side's jitter-buffer delay (`jitterDelay`) every 10 s.

## Stage 1
Both tracks together on two Macs. Mac A runs the stack: `make dev-up`, `make server` (the whiteboard needs `GEMINI_API_KEY` in `.env`), `make lan-ip` (note the address). Mac B runs the app copied from A (`make app`, then `scp -r build/Huddle.app <B>:` or rsync: copied over ssh it isn't marked as downloaded, so it opens without notarizing). Both join one room: A `make run ROOM=stage1`; B `open -n Huddle.app --args app -server http://<A's address>:8080 -room stage1`.
1. Two people talk: each hears the other, neither hears themselves come back.
2. "Whiteboard, draw what we just discussed, alphabetically": the same board appears on both Macs.
3. Ask it a question ("whiteboard, which step comes first?"): both hear it answer.
4. "Whiteboard, keep diagramming and take notes as we go", talk for a minute, then "whiteboard, stop".
5. "Whiteboard, undo".

Also worth a try: `/`, a typed command, Enter; and holding right ⌥ while saying a command without the name. Logs: the server's terminal (`agent: command`, `agent: turn done`); each app's log (A `.dev/data/logs/app.log`, B `~/Library/Logs/Huddle/app.log`) for `conn: version` and `voice: track` (the whiteboard's `jitterDelay` should sit near 40 ms).

## Stage 2
The real camera, the system picker and two Macs. Restart `make server` first so it runs this build. The camera works only in the signed bundle (`make run`, `make app`). Mac B doesn't need a notarized app: copied over ssh (`scp -r build/Huddle.app <B>:` or rsync), it isn't marked as downloaded and opens as it is.
1. **Alone, on Mac A.**
   ```
   make server                            # terminal 1
   make bot ARGS="cam -name Ann"          # terminal 2: a camera of the test pattern
   make bot ARGS="cam -name Bob -share"   # terminal 3: a shared screen of it
   make bot ARGS="echo"                   # terminal 4
   make run                               # terminal 5
   grep -E "video: (camera|share|layer|sending)" .dev/data/logs/app.log | tail
   ```
   - Bob's screen takes the stage under "Whiteboard | Bob's screen"; Ann's tile shows the pattern.
   - Camera: macOS asks for the camera once; your tile shows you, mirrored; the button says "Camera on".
   - The device menu (⌃ by the mic, or `d`) → Camera: pick another (a phone nearby shows up through Continuity Camera); your tile switches to it.
   - Share screen: the system picker; pick a window. The button says "Sharing · stop" (nothing here shows your share back; step 2 does). Stop it from the menu bar's icon: the button goes back to "Share screen".
   - With Bob's screen on stage, type "draw a three-step plan" in the field: a dot appears on the Whiteboard tab.
2. **Two Macs.** On A: the stack as in the stage 1 gate, `make app`, copy the app to B, `make run ROOM=stage2`. On B: `open -n Huddle.app --args app -server http://<A's address>:8080 -room stage2`.
   - Cameras both ways.
   - Share a display, then a window: its text reads on the other Mac.
   - Switch between the tabs; ask the whiteboard to draw while a share is on stage, and see the dot.
   - Camera off and on.
3. **A slow link.** Network Link Conditioner (in Xcode's Additional Tools) on B, a profile limited to 1 Mbps up: A's tile of B drops to a lower layer (on A, `grep "video: track" .dev/data/logs/app.log | tail -3`: `w=640` or `w=320`), and B's voice stays clean on A (`voice: track`: `underrunMs`, `plc`).
4. **Twenty minutes** with cameras on: CPU (Activity Monitor) and heat feel normal; `grep "video: sending"` on each for the estimate.

Logs: A `.dev/data/logs/app.log`, B `~/Library/Logs/Huddle/app.log`.

## A narrow downlink (optional)
Network Link Conditioner on B limited to 1 Mbps *down*. Within a couple of seconds, A's camera on B drops to a lower layer (B's `video: track`: `w=640` or `w=320`). A's voice on B may step its delay up once and back (`voice: track`: `jitterDelay`, with `lost` and `plc` near 0). Measured on one Mac, this costs the voice a second or two of quality (see "Open issues" in `design.md`); a real link shows whether it matters.

## What to send back
From each app's log: `grep -E "voice: track|video: track|video: sending"`. From A: `grep -E "serve: listening|api: dev join|sfu: |agent: turn done" .dev/data/logs/serve.log`.
