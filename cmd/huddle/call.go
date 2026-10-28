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
	callWaiting      = "Voice waits for the server"
	callJoining      = "Joining voice…"
	callOn           = "Mic on"
	callMuted        = "Muted"
	callListening    = "Listening only"
	callLost         = "Voice lost"
	callRetry        = "Voice can't connect" // with retryIn: the countdown
	callReconnecting = "Reconnecting voice…"
	callSwitching    = "Switching…"
)

// tokenFresh is how long a join's token is retried with. The server's
// tokens can join for 10 minutes; past this, voice asks the connection to
// join again for a new one.
var tokenFresh = 5 * time.Minute

type call struct {
	noVoice bool
	wake    func()
	data    string // where devices.json is kept ("" keeps nothing)
	// rejoin asks the connection for a fresh join (conn.rejoin).
	rejoin func()

	mu       sync.Mutex
	st       callState
	joined   chan wire.Joined // the latest dev join, for (re)connecting
	room     rtc.Room
	dev      *audio.Voice // nil between devices
	prefs    devicePrefs
	vid      videoState // video.go
	switchMu sync.Mutex // one opening of the devices at a time (devices.go)
}

// callState is what the bar shows.
type callState struct {
	status  string
	detail  string // why the mic is off
	self    string // our identity
	people  []rtc.Participant
	talking map[string]bool
	muted   bool
	canMute bool
	noEcho  bool   // the mic is on without echo cancelling because it wouldn't start
	echoErr string // why
	retryIn int    // with callRetry: seconds to the next try
	lostErr string // why voice isn't connected
	before  string // the status a reconnect interrupted
}

func newCall(noVoice bool, data string, wake func()) *call {
	c := &call{noVoice: noVoice, data: data, wake: wake, joined: make(chan wire.Joined, 1), st: callState{status: callWaiting, talking: map[string]bool{}}}
	if data != "" {
		c.prefs = readDevicePrefs(data)
	}
	return c
}

// devicePicks is what the device menu has picked.
func (c *call) devicePicks() devicePrefs {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prefs
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

// run joins voice with the connection's token and keeps it joined until
// ctx ends. When joining fails or the room is lost, it tries again with
// backoff on its own (the meeting connection may be fine while LiveKit is
// down); a token too old to join with is traded for a new join. A new join
// from the connection (it reconnected, as someone new) moves voice to it.
func (c *call) run(ctx context.Context) {
	defer c.hangUp()
	var j wire.Joined
	var got time.Time
	backoff := time.Second
	for {
		if j.Token == "" {
			c.update(func(s *callState) bool { s.status, s.retryIn = callWaiting, 0; return true })
			select {
			case <-ctx.Done():
				return
			case j = <-c.joined:
				got = time.Now()
			}
		}
		select { // the newest join wins
		case j = <-c.joined:
			got = time.Now()
		default:
		}
		start := time.Now()
		lost := make(chan error, 1)
		err := c.connect(ctx, j, lost)
		if err == nil {
			select {
			case <-ctx.Done():
				return
			case err = <-lost:
			case nj := <-c.joined:
				slog.Info("voice: moving to the new join", "identity", nj.Identity)
				c.hangUp()
				j, got, backoff = nj, time.Now(), time.Second
				continue
			}
		}
		if ctx.Err() != nil {
			return
		}
		c.hangUp()
		if time.Since(start) > 30*time.Second {
			backoff = time.Second // it was up a while: the loss is news
		}
		slog.Warn("voice: not connected", "err", err, "retry", backoff)
		msg := err.Error()
		c.update(func(s *callState) bool { s.people, s.lostErr = nil, msg; return true })
		end := time.Now().Add(backoff)
	wait:
		for left := time.Until(end); left > 0; left = time.Until(end) {
			n := int((left + time.Second - 1) / time.Second)
			c.update(func(s *callState) bool { s.status, s.retryIn, s.canMute = callRetry, n, false; return true })
			select {
			case <-ctx.Done():
				return
			case j = <-c.joined: // a new token: try it now
				got = time.Now()
				break wait
			case <-time.After(min(left, time.Second)):
			}
		}
		backoff = min(backoff*2, 30*time.Second)
		if time.Since(got) > tokenFresh && c.rejoin != nil {
			slog.Info("voice: the token is old; asking for a new join")
			c.rejoin()
			j = wire.Joined{}
		}
	}
}

func (c *call) connect(ctx context.Context, j wire.Joined, lost chan error) error {
	c.update(func(s *callState) bool { s.status, s.retryIn, s.lostErr = callJoining, 0, ""; return true })
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
		Reconnecting: func() {
			c.update(func(s *callState) bool {
				if s.status != callReconnecting {
					s.before, s.status = s.status, callReconnecting
				}
				return true
			})
		},
		Reconnected: func() {
			c.update(func(s *callState) bool {
				if s.status == callReconnecting {
					s.status = s.before
				}
				return true
			})
		},
	}
	room, err := (&lkrtc.Connector{}).Connect(ctx, rtc.Join{URL: j.LiveKitURL, Token: j.Token, Identity: j.Identity}, ev)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.room = room
	c.mu.Unlock()
	// The devices open only once the room is joined. The room publishes the
	// mic for the whole call (silence while there's none) unless -no-voice.
	c.switchMu.Lock()
	dev, ds, err := c.openDevices(c.devicePicks())
	if err == nil {
		c.setDevice(room, dev, ds)
	}
	c.switchMu.Unlock()
	if err != nil {
		return fmt.Errorf("speakers: %w", err)
	}
	if !c.noVoice {
		if err := room.PublishMic(&liveMic{c: c}); err != nil {
			return err
		}
	}
	if err := room.Play(&liveSpeaker{c: c}); err != nil {
		return err
	}
	go c.logDevices(ctx, room)
	return nil
}

// logDevices puts the voice path's numbers in the log every 10 s while the
// call is in room.
func (c *call) logDevices(ctx context.Context, room rtc.Room) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		dev, same := c.dev, c.room == room
		c.mu.Unlock()
		if !same {
			return
		}
		if dev == nil {
			continue
		}
		i := dev.Info()
		slog.Info("voice: devices", "mic", i.MicName, "speakers", i.SpeakerName, "echoCancel", i.EchoCancel, "micChannel", i.MicChannel, "micChannels", i.MicChannels,
			"deviceMicRate", i.DeviceMic, "outputLatency", i.Latency.Round(100*time.Microsecond),
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
	c := newCall(false, "", nil)
	st := &c.st
	st.status, st.canMute = callOn, true
	switch mic {
	case "muted":
		st.status, st.muted = callMuted, true
	case "listening":
		st.status, st.canMute = callListening, false
	case "lost":
		st.status, st.canMute = callLost, false
	case "waiting":
		st.status, st.canMute = callWaiting, false
	case "retry":
		st.status, st.canMute, st.retryIn = callRetry, false, 4
		st.lostErr = "join the voice room: could not establish signal connection: dial tcp 127.0.0.1:7880: connect: connection refused"
	case "reconnecting":
		st.status, st.before = callReconnecting, callOn
	case "noecho":
		st.noEcho, st.echoErr = true, "start the audio engine: (1650879860)"
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
