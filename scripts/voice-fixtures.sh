#!/bin/sh
# Speech fixtures for the voice tests and the dev bots, made on this Mac
# with macOS voices and ffmpeg (brew install ffmpeg). Generated audio is
# git-ignored: only this script and the lines below are committed.
#
#   scripts/voice-fixtures.sh [dir]     # default .dev/fixtures
#
# Writes, at 48 kHz mono:
#   speech.wav    two voices taking turns, with pauses (~25 s)
#   samantha.ogg  one voice as Ogg/Opus, 20 ms frames, for huddled bot play
#   daniel.ogg    another voice, for a second fixture participant
set -eu
cd "$(dirname "$0")/.."
dir=${1:-.dev/fixtures}
mkdir -p "$dir"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

line() { # voice, text, out.wav
	say -v "$1" -o "$tmp/l.aiff" "$2"
	ffmpeg -loglevel error -y -i "$tmp/l.aiff" -ar 48000 -ac 1 -c:a pcm_s16le "$3"
}
pause() { # seconds, out.wav
	ffmpeg -loglevel error -y -f lavfi -i "anullsrc=r=48000:cl=mono" -t "$1" -c:a pcm_s16le "$2"
}

line Samantha "Okay, let's get started. The first thing on the list is the orders service, which has been timing out since Tuesday." "$tmp/1.wav"
pause 0.8 "$tmp/p1.wav"
line Daniel "Right. I looked at it this morning. The database pool is too small, so requests queue up behind the slow report." "$tmp/2.wav"
pause 0.6 "$tmp/p2.wav"
line Samantha "Can we move the report to the replica, and then raise the pool to twenty connections?" "$tmp/3.wav"
pause 1.0 "$tmp/p3.wav"
line Daniel "Yes. I'll do the replica today. Whiteboard, draw the orders service, the pool, the database and the replica." "$tmp/4.wav"
printf "file '%s'\n" "$tmp/1.wav" "$tmp/p1.wav" "$tmp/2.wav" "$tmp/p2.wav" "$tmp/3.wav" "$tmp/p3.wav" "$tmp/4.wav" >"$tmp/list.txt"
ffmpeg -loglevel error -y -f concat -safe 0 -i "$tmp/list.txt" -c:a pcm_s16le "$dir/speech.wav"

line Samantha "Hi, this is Samantha, a fixture participant. I keep talking so you can check the talking indicator, the mix and the jitter buffer. Here is a short pause." "$tmp/s.wav"
pause 1.5 "$tmp/sp.wav"
printf "file '%s'\n" "$tmp/s.wav" "$tmp/sp.wav" >"$tmp/s.txt"
ffmpeg -loglevel error -y -f concat -safe 0 -i "$tmp/s.txt" -c:a libopus -b:a 32k -frame_duration 20 -application voip "$dir/samantha.ogg"

line Daniel "Hello, Daniel here, the second fixture participant. When we both talk at once, the mix should stay clean and not clip." "$tmp/d.wav"
pause 2 "$tmp/dp.wav"
printf "file '%s'\n" "$tmp/d.wav" "$tmp/dp.wav" >"$tmp/d.txt"
ffmpeg -loglevel error -y -f concat -safe 0 -i "$tmp/d.txt" -c:a libopus -b:a 32k -frame_duration 20 -application voip "$dir/daniel.ogg"

# Commands to the whiteboard with pauses between, over a little room noise,
# for the ears through the app's own mic path (huddle voice -in): AGENTS.md,
# "The ears".
line Daniel "Whiteboard, add a box called build." "$tmp/c1.wav"
line Daniel "I think we should ship on Friday." "$tmp/c2.wav"
line Daniel "Whiteboard, add a box called test after build." "$tmp/c3.wav"
line Daniel "Whiteboard, what is on the board right now?" "$tmp/c4.wav"
pause 2 "$tmp/c0.wav"
pause 4 "$tmp/cp4.wav"
pause 3 "$tmp/cp3.wav"
pause 6 "$tmp/cp6.wav"
printf "file '%s'\n" "$tmp/c0.wav" "$tmp/c1.wav" "$tmp/cp4.wav" "$tmp/c2.wav" "$tmp/cp3.wav" "$tmp/c3.wav" "$tmp/cp4.wav" "$tmp/c4.wav" "$tmp/cp6.wav" >"$tmp/c.txt"
ffmpeg -loglevel error -y -f concat -safe 0 -i "$tmp/c.txt" -c:a pcm_s16le "$tmp/clean.wav"
ffmpeg -loglevel error -y -i "$tmp/clean.wav" -f lavfi -i "anoisesrc=color=pink:amplitude=0.003:r=48000" \
	-filter_complex "[0][1]amix=inputs=2:duration=first:normalize=0" -ar 48000 -ac 1 -c:a pcm_s16le "$dir/commands.wav"

ls -l "$dir"
