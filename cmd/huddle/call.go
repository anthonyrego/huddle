package main

// The call: the meeting's voice, for the window. It joins the LiveKit room
// with the token the dev join gave the connection, opens the voice path
// (macOS voice processing, so the mic is echo-cancelled against what the
// speakers play), publishes the mic and plays everyone else. With -no-voice
// it never opens the mic: it listens through the speakers alone.
//
// Its state (who's here, who's talking, muted) is written by the room's
// goroutines under the lock and read by the bar on the main thread; a
// change that shows wakes the loop.

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/platform/audio"
	"github.com/anthonyrego/huddle/internal/playout"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/lkrtc"
	"github.com/anthonyrego/huddle/internal/wire"
)

// Call states for the bar.
const (
	callJoining   = "Joining voice…"
	callOn        = "Mic on"
	callMuted     = "Muted"
	callListening = "Listening only"
	callLost      = "Voice lost"
)

type call struct {
	noVoice bool
	wake    func()

	mu     sync.Mutex
	st     callState
	joined chan wire.Joined // the latest dev join, for (re)connecting
	room   rtc.Room
	dev    *audio.Voice
}

// callState is what the bar shows.
type callState struct {
	status  string
	detail  string // why voice is off
	self    string // our identity
	people  []rtc.Participant
	talking map[string]bool
	muted   bool
	canMute bool
}

func newCall(noVoice bool, wake func()) *call {
	return &call{noVoice: noVoice, wake: wake, joined: make(chan wire.Joined, 1), st: callState{status: callJoining, talking: map[string]bool{}}}
}

// snapshot is the state for one frame.
func (c *call) snapshot() callState {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.st
	s.people = slices.Clone(s.people)
	return s
}

func (c *call) update(f func(*callState) bool) {
	c.mu.Lock()
	changed := f(&c.st)
	c.mu.Unlock()
	if changed && c.wake != nil {
		c.wake()
	}
}

// onJoin is the connection's hook: each dev join's token, the latest kept.
func (c *call) onJoin(j wire.Joined) {
	select {
	case <-c.joined:
	default:
	}
	c.joined <- j
}

// run joins voice with the first token and again with the next one after
// voice is lost, until ctx ends.
func (c *call) run(ctx context.Context) {
	defer c.hangUp()
	for {
		var j wire.Joined
		select {
		case <-ctx.Done():
			return
		case j = <-c.joined:
		}
		lost := make(chan error, 1)
		if err := c.connect(ctx, j, lost); err != nil {
			slog.Error("voice: couldn't join", "err", err)
			c.update(func(s *callState) bool { s.status, s.detail = callLost, err.Error(); return true })
			c.hangUp()
			continue // wait for the connection's next join
		}
		select {
		case <-ctx.Done():
			return
		case err := <-lost:
			c.update(func(s *callState) bool { s.status, s.detail, s.people = callLost, err.Error(), nil; return true })
			c.hangUp()
		}
	}
}

func (c *call) connect(ctx context.Context, j wire.Joined, lost chan error) error {
	c.update(func(s *callState) bool { s.status, s.detail = callJoining, ""; return true })
	ev := rtc.Events{
		Joined: func(self rtc.Participant, others []rtc.Participant) {
			c.update(func(s *callState) bool { s.self, s.people = self.Identity, others; return true })
		},
		Arrived: func(p rtc.Participant) {
			c.update(func(s *callState) bool {
				if !slices.ContainsFunc(s.people, func(q rtc.Participant) bool { return q.Identity == p.Identity }) {
					s.people = append(s.people, p)
				}
				return true
			})
		},
		Left: func(id string) {
			c.update(func(s *callState) bool {
				s.people = slices.DeleteFunc(s.people, func(q rtc.Participant) bool { return q.Identity == id })
				delete(s.talking, id)
				return true
			})
		},
		Levels: func(ls []rtc.Level) {
			c.update(func(s *callState) bool {
				changed := false
				now := map[string]bool{}
				for _, l := range ls {
					if l.DBFS > playout.TalkingDBFS {
						now[l.Identity] = true
					}
				}
				if s.muted {
					delete(now, s.self)
				}
				if len(now) != len(s.talking) {
					changed = true
				}
				for id := range now {
					changed = changed || !s.talking[id]
				}
				s.talking = now
				return changed
			})
		},
		Disconnected: func(err error) {
			select {
			case lost <- err:
			default:
			}
		},
	}
	room, err := (&lkrtc.Connector{}).Connect(ctx, rtc.Join{URL: j.LiveKitURL, Token: j.Token, Identity: j.Identity}, ev)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.room = room
	c.mu.Unlock()
	// The devices open only once the room is joined.
	dev, err := audio.OpenVoice(audio.VoiceConfig{SpeakerOnly: c.noVoice})
	if err != nil && !c.noVoice {
		slog.Error("voice: no voice processing; listening only", "err", err)
		c.update(func(s *callState) bool { s.detail = "no mic: " + err.Error(); return true })
		dev, err = audio.OpenVoice(audio.VoiceConfig{SpeakerOnly: true})
	}
	if err != nil {
		return fmt.Errorf("speakers: %w", err)
	}
	c.mu.Lock()
	c.dev = dev
	c.mu.Unlock()
	info := dev.Info()
	if info.VoiceProcess {
		if err := room.PublishMic(dev.Mic()); err != nil {
			return err
		}
	}
	if err := room.Play(dev.Speaker()); err != nil {
		return err
	}
	c.update(func(s *callState) bool {
		s.canMute = info.VoiceProcess
		s.status = callListening
		if s.canMute {
			s.status = callOn
		}
		return true
	})
	go c.logDevices(ctx, dev)
	return nil
}

// logDevices puts the voice path's numbers in the log every 10 s.
func (c *call) logDevices(ctx context.Context, dev *audio.Voice) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		same := c.dev == dev
		c.mu.Unlock()
		if !same {
			return
		}
		i := dev.Info()
		slog.Info("voice: devices", "voiceProcessing", i.VoiceProcess, "deviceMicRate", i.DeviceMic, "outputLatency", i.Latency.Round(100*time.Microsecond),
			"queued", i.Queued, "micHeld", i.MicHeld, "played", i.Written.Round(time.Second), "dropped", i.Dropped, "engineRunning", i.EngineAlive)
	}
}

// toggleMute mutes or unmutes the mic; it reports whether there is a mic.
func (c *call) toggleMute() bool {
	c.mu.Lock()
	room, can := c.room, c.st.canMute
	muted := !c.st.muted
	if can {
		c.st.muted = muted
		c.st.status = callOn
		if muted {
			c.st.status = callMuted
			delete(c.st.talking, c.st.self)
		}
	}
	c.mu.Unlock()
	if !can || room == nil {
		return false
	}
	room.Mute(muted)
	if c.wake != nil {
		c.wake()
	}
	return true
}

// hangUp leaves the room and closes the devices.
func (c *call) hangUp() {
	c.mu.Lock()
	room, dev := c.room, c.dev
	c.room, c.dev = nil, nil
	c.mu.Unlock()
	if room != nil {
		room.Leave()
	}
	if dev != nil {
		dev.Close()
	}
}

// renderCall is a call for render: people as "Name*" (talking) or "Name!"
// (the whiteboard), you first; mic is on, muted, listening or lost.
func renderCall(people, mic string) *call {
	c := newCall(false, nil)
	st := &c.st
	st.status, st.canMute = callOn, true
	switch mic {
	case "muted":
		st.status, st.muted = callMuted, true
	case "listening":
		st.status, st.canMute = callListening, false
	case "lost":
		st.status, st.canMute = callLost, false
	}
	for i, p := range strings.Split(people, ",") {
		p = strings.TrimSpace(p)
		talking, agent := strings.HasSuffix(p, "*"), strings.HasSuffix(p, "!")
		name := strings.TrimRight(p, "*!")
		id := strings.ToLower(name) + "-" + itoa(i)
		if i == 0 {
			st.self = id
		} else {
			st.people = append(st.people, rtc.Participant{Identity: id, Name: name, Agent: agent})
		}
		if talking && !(i == 0 && st.muted) {
			st.talking[id] = true
		}
	}
	return c
}
