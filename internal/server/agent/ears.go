package agent

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/pion/opus"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/gate"
	"github.com/anthonyrego/huddle/internal/live"
	"github.com/anthonyrego/huddle/internal/pcm"
)

// The ears: each person's track decoded to 16 kHz (pion's pure-Go Opus
// decoder outputs that rate directly), a voice detector, and only their
// speech sent to their own streaming transcription session. At the end of
// each utterance the session is told the audio stream ended, so the final
// transcript comes about half a second later instead of waiting on the
// server's silence detection (AGENTS.md). Sessions last ten minutes, so a
// session older than rotateAfter is replaced at the next utterance.

const (
	earRate     = 16000
	chunkBytes  = earRate * 2 / 10 // 100 ms
	prerollMs   = 300
	vadLevel    = -45.0 // dBFS
	vadHangover = 450 * time.Millisecond
	rotateAfter = 8 * time.Minute
	hardRotate  = 9*time.Minute + 30*time.Second
	idleClose   = 3 * time.Minute
	finalWait   = 2500 * time.Millisecond
)

// audioMsg is a stretch of one person's speech for their transcriber.
type audioMsg struct {
	pcm   []byte
	start bool      // the first of an utterance
	end   bool      // the last: the utterance ended at at
	at    time.Time // when it was heard
}

// listen hears one person's track until it ends.
func (a *Agent) listen(ctx context.Context, track *webrtc.TrackRemote, identity, name string) {
	a.log.Info("agent: listening", "identity", identity, "codec", track.Codec().MimeType)
	dec, err := opus.NewDecoderWithOutput(earRate, 1)
	if err != nil {
		a.log.Error("agent: decoder", "err", err)
		return
	}
	audio := make(chan audioMsg, 600) // a minute of 100 ms chunks while a session connects
	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go a.transcribe(tctx, identity, name, audio)
	defer close(audio)

	vad := pcm.Detector{Threshold: vadLevel, Hangover: vadHangover}
	samples := make([]int16, 2880)
	var pre, chunk []byte
	frame := make([]byte, 0, 1920)
	send := func(m audioMsg) {
		select {
		case audio <- m:
		default:
			a.log.Warn("agent: transcriber behind; dropping audio", "identity", identity)
		}
	}
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			a.log.Info("agent: track ended", "identity", identity, "err", err)
			return
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		n, err := dec.DecodeToInt16(pkt.Payload, samples)
		if err != nil || n == 0 {
			continue
		}
		now := time.Now()
		frame = frame[:0]
		for _, s := range samples[:n] {
			frame = binary.LittleEndian.AppendUint16(frame, uint16(s))
		}
		edge, at := vad.Feed(pcm.Level(frame), now)
		switch {
		case edge == pcm.Start:
			msg := audioMsg{pcm: append(append([]byte(nil), pre...), frame...), start: true, at: now}
			pre, chunk = pre[:0], chunk[:0]
			send(msg)
		case vad.Speaking():
			chunk = append(chunk, frame...)
			if len(chunk) >= chunkBytes {
				send(audioMsg{pcm: append([]byte(nil), chunk...), at: now})
				chunk = chunk[:0]
			}
		case edge == pcm.End:
			send(audioMsg{pcm: append([]byte(nil), chunk...), end: true, at: at})
			chunk = chunk[:0]
		default:
			pre = append(pre, frame...)
			if keep := prerollMs * earRate * 2 / 1000; len(pre) > keep {
				pre = append(pre[:0], pre[len(pre)-keep:]...)
			}
		}
	}
}

// lastFinal waits for a session's final transcript.
func lastFinal(s *live.Session, d time.Duration) string {
	t := time.After(d)
	for {
		select {
		case <-t:
			return ""
		case ev, ok := <-s.Events():
			if !ok {
				return ""
			}
			if ev.Kind == live.InputText {
				return ev.Text
			}
		}
	}
}

// transcribe runs one person's transcription sessions.
func (a *Agent) transcribe(ctx context.Context, identity, name string, audio <-chan audioMsg) {
	var sess *live.Session
	var opened, lastAudio time.Time
	var uttStart, segStart, uttEnd int64 // meeting ms
	var awaiting bool                    // the utterance ended; its final hasn't come
	var deadline time.Time
	speaking := false
	var interimAt time.Time
	defer func() {
		if sess != nil {
			sess.Close()
		}
	}()
	open := func() {
		if sess != nil {
			sess.Close()
			a.trace(Event{Kind: EvRotate, Speaker: name})
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		s, err := live.Connect(cctx, a.cfg.APIKey, live.Config{Model: a.cfg.EarModel, Text: true})
		cancel()
		if err != nil {
			a.log.Error("agent: transcription session", "identity", identity, "err", err)
			a.trace(Event{Kind: EvError, Detail: "transcribe connect: " + err.Error()})
			sess = nil
			return
		}
		sess, opened = s, time.Now()
	}
	final := func(text string) {
		end := uttEnd
		if speaking || end < segStart {
			end = a.now()
		}
		g := gate.Segment{Identity: identity, Speaker: name, Text: text, StartMs: segStart, EndMs: end, Final: true}
		segStart = end
		a.post(func() { a.onSegment(g) })
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var events <-chan live.Event
	for {
		if sess != nil {
			events = sess.Events()
		} else {
			events = nil
		}
		select {
		case <-ctx.Done():
			return
		case m, ok := <-audio:
			if !ok {
				return
			}
			lastAudio = time.Now()
			if m.start {
				if awaiting {
					final("") // the last utterance's final never came
					awaiting = false
				}
				if sess == nil || time.Since(opened) > rotateAfter {
					open()
				}
				speaking = true
				uttStart = a.ms(m.at) - prerollMs
				segStart = uttStart
				// The gate hears speech start before any words.
				g := gate.Segment{Identity: identity, Speaker: name, StartMs: uttStart, EndMs: uttStart}
				a.post(func() { a.onSegment(g) })
			}
			if sess == nil {
				continue
			}
			if len(m.pcm) > 0 {
				sess.SendAudio(m.pcm, earRate)
			}
			if m.end {
				speaking = false
				uttEnd = a.ms(m.at)
				sess.SendAudioEnd()
				awaiting, deadline = true, time.Now().Add(finalWait)
			} else if time.Since(opened) > hardRotate {
				// Ten minutes is the session's limit: end the utterance here,
				// take its final from the old session, go on in a new one.
				uttEnd = a.now()
				sess.SendAudioEnd()
				text := lastFinal(sess, finalWait)
				speaking = false
				final(text)
				speaking = true
				open()
			}
		case ev, ok := <-events:
			if !ok || ev.Kind == live.Closed {
				if ok && ev.Err != nil {
					a.log.Warn("agent: transcription closed", "identity", identity, "err", ev.Err)
				}
				sess = nil
				continue
			}
			switch ev.Kind {
			case live.InterimText:
				if time.Since(interimAt) < 250*time.Millisecond {
					continue
				}
				interimAt = time.Now()
				g := gate.Segment{Identity: identity, Speaker: name, Text: ev.Text, StartMs: segStart, EndMs: a.now()}
				a.post(func() { a.onSegment(g) })
			case live.InputText:
				final(ev.Text)
				awaiting = false
			case live.Usage:
				if u := ev.Usage; u != nil {
					a.trace(Event{Kind: EvUsage, N: int(u.PromptTokenCount), Detail: "ear prompt"})
				}
			}
		case <-tick.C:
			if awaiting && time.Now().After(deadline) {
				final("")
				awaiting = false
			}
			if sess != nil && !speaking && !awaiting && time.Since(lastAudio) > idleClose {
				sess.Close()
				sess = nil
			}
		}
	}
}
