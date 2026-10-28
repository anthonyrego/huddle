// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Adapted from livekit/livekit v1.13.11 pkg/sfu/audio/audiolevel.go;
// changed: its default settings fixed as constants, durations always from
// RTP timestamps, the list of speakers made here (room.go's
// audioUpdateWorker's idea, sent whole and loudest first).

package sfu

import (
	"math"
	"slices"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// Who's talking, from the RFC 6464 levels (0 loudest, 127 silent) on each
// voice's packets: over each window of levelWindow, a voice louder than
// activeLevel for at least activeShare of it is talking, as loud as its
// loudest moment, less for being loud for less of the window, smoothed
// over about two windows. LiveKit's defaults.
const (
	activeLevel   = 35 // −35 dBov
	activeShare   = 0.4
	levelWindow   = 400 // ms of RTP time
	levelSmoothed = 2   // windows
	silentLevel   = 127
)

// audioLevel follows one voice's level.
type audioLevel struct {
	mu       sync.Mutex
	smoothed float64 // 0–1, linear
	loudest  uint8
	active   uint32 // ms of the window louder than activeLevel
	observed uint32 // ms of the window
	lastTS   uint32
	lastAt   time.Time
	started  bool
}

// observe takes one packet's level (its 7 bits) and RTP timestamp: the
// level lasts from the last packet's timestamp to this one's.
func (l *audioLevel) observe(level uint8, ts uint32, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.started {
		l.started, l.lastTS, l.loudest = true, ts-960, silentLevel
	}
	d := ts - l.lastTS
	if d >= 1<<31 {
		return // older than what's been counted
	}
	l.lastTS, l.lastAt = ts, now
	ms := uint32(uint64(d) * 1000 / 48000)
	l.observed += ms
	if level <= activeLevel {
		l.active += ms
		l.loudest = min(l.loudest, level)
	}
	if l.observed < levelWindow {
		return
	}
	next := 0.0
	if float64(l.active) >= activeShare*levelWindow {
		// Loud for the whole window counts as the loudest moment; for
		// less, as that much quieter.
		weight := 20 * math.Log10(float64(l.active)/levelWindow)
		linear := linearLevel(float64(l.loudest) - weight)
		next = l.smoothed + (linear-l.smoothed)*2/(levelSmoothed+1)
	}
	l.smoothed, l.loudest, l.active, l.observed = next, silentLevel, 0, 0
}

// level is the voice's level now, 0 to 1, and whether it's talking. A
// voice that has sent nothing for two windows (muted, gone) is quiet.
func (l *audioLevel) level(now time.Time) (float64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastAt) >= 2*levelWindow*time.Millisecond {
		l.smoothed, l.loudest, l.active, l.observed = 0, silentLevel, 0, 0
	}
	return l.smoothed, l.smoothed >= linearLevel(activeLevel)
}

// linearLevel turns −dBov into 0–1.
func linearLevel(dbov float64) float64 { return math.Pow(10, -dbov/20) }

// speakersOf is who's talking in pubs, loudest first: each participant at
// their loudest voice, levels to two places (so a list that hasn't really
// changed isn't sent again).
func speakersOf(pubs []*publication, now time.Time) []signal.Speaker {
	loud := map[string]float64{}
	for _, pub := range pubs {
		if pub.req.Kind != signal.Audio || pub.muted.Load() {
			continue
		}
		if lv, talking := pub.level.level(now); talking {
			who := pub.owner.grant.Identity
			loud[who] = max(loud[who], lv)
		}
	}
	list := make([]signal.Speaker, 0, len(loud))
	for who, lv := range loud {
		list = append(list, signal.Speaker{Identity: who, Level: float32(math.Round(lv*100) / 100)})
	}
	slices.SortFunc(list, func(a, b signal.Speaker) int {
		switch {
		case a.Level > b.Level:
			return -1
		case a.Level < b.Level:
			return 1
		}
		if a.Identity < b.Identity {
			return -1
		}
		return 1
	})
	return list
}

func sameSpeakers(a, b []signal.Speaker) bool {
	return slices.Equal(a, b)
}
