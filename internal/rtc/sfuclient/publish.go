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

// Adapted from livekit/server-sdk-go v2.19.0 localparticipant.go; changed:
// one Publish for a voice or a video's layers (huddle's Track, sendonly),
// the publish sent before the transceiver is added (so no offer can carry
// a track the server hasn't heard of), no backup codecs or encryption.

package sfuclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc/codecs"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// localTrack is something the session publishes.
type localTrack struct {
	req    signal.Publish
	tracks []*Track
	tx     *webrtc.RTPTransceiver
}

// PublishAudio publishes a microphone, named name: an Opus track to write
// 20 ms frames to (Track.WriteFrame, with each frame's level), and the SID
// the server gave it. Frames written before the track is negotiated are
// dropped (ErrNotBound).
func (s *Session) PublishAudio(ctx context.Context, name string) (*Track, string, error) {
	cid := s.newCID("mic")
	t, err := NewTrack(codecs.Opus, cid, s.self.Identity, "", nil)
	if err != nil {
		return nil, "", err
	}
	sid, err := s.Publish(ctx, signal.Publish{CID: cid, Kind: signal.Audio, Source: signal.Microphone, Name: name}, t)
	if err != nil {
		return nil, "", err
	}
	return t, sid, nil
}

// Publish publishes a track: a voice, or a video's simulcast layers (one
// Track each, made with NewTrack under req.CID with the layer's rid; the
// first is the transceiver's, the rest its further encodings). It asks the
// server, waits for the SID, then offers the publish connection with the
// track on it. For a video, the server may say later which layers anyone
// is sent (Events.Wanted), and the caller pauses the rest.
func (s *Session) Publish(ctx context.Context, req signal.Publish, tracks ...*Track) (string, error) {
	if len(tracks) == 0 {
		return "", errors.New("sfuclient: publish needs a track")
	}
	kind := webrtc.RTPCodecTypeAudio
	if req.Kind == signal.Video {
		kind = webrtc.RTPCodecTypeVideo
	}
	for _, t := range tracks {
		if t.ID() != req.CID || t.Kind() != kind {
			return "", fmt.Errorf("sfuclient: track %s (%s) isn't the publish's %s (%s)", t.ID(), t.Kind(), req.CID, kind)
		}
	}
	ch := make(chan published, 1)
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return "", errors.New("sfuclient: the session has ended")
	}
	if s.waiting[req.CID] != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("sfuclient: %s is already being published", req.CID)
	}
	s.waiting[req.CID] = ch
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		if s.waiting[req.CID] == ch {
			delete(s.waiting, req.CID)
		}
		s.mu.Unlock()
	}

	// The publish goes out before the transceiver is on the connection, so
	// any offer that carries the track reaches the server after it.
	if err := s.send(signal.KindPublish, req); err != nil {
		forget()
		return "", fmt.Errorf("sfuclient: publish: %w", err)
	}
	tx, err := s.pub.pc.AddTransceiverFromTrack(tracks[0], webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err == nil {
		for _, t := range tracks[1:] {
			if err = tx.Sender().AddEncoding(t); err != nil {
				break
			}
		}
	}
	if err != nil {
		forget()
		if tx != nil {
			s.pub.pc.RemoveTrack(tx.Sender())
		}
		return "", fmt.Errorf("sfuclient: publish: %w", err)
	}
	for _, t := range tracks {
		t.SetTransceiver(tx)
	}

	var res published
	select {
	case res = <-ch:
	case <-ctx.Done():
		res.err = ctx.Err()
	case <-time.After(publishWait):
		res.err = errors.New("no answer from the server")
	}
	if res.err != nil {
		gaveUp()
	}
	forget()
	if res.err != nil {
		// The answer may have come between giving up and forget: once
		// forgotten, nothing more is sent on ch. A track published after
		// all is unpublished, or it would stay on the server.
		select {
		case late := <-ch:
			if late.err == nil {
				s.unpublishLate(req.CID, late.sid)
			}
		default:
		}
		s.pub.pc.RemoveTrack(tx.Sender())
		return "", fmt.Errorf("sfuclient: publish %s: %w", req.CID, res.err)
	}
	s.mu.Lock()
	s.local[res.sid] = &localTrack{req: req, tracks: tracks, tx: tx}
	s.mu.Unlock()
	s.pub.negotiate()
	s.log.Debug("sfuclient: published", "cid", req.CID, "sid", res.sid, "kind", req.Kind)
	return res.sid, nil
}

// Unpublish stops publishing a track: the server hears unpublish, then the
// track comes off the publish connection.
func (s *Session) Unpublish(sid string) error {
	s.mu.Lock()
	lt := s.local[sid]
	delete(s.local, sid)
	s.mu.Unlock()
	if lt == nil {
		return fmt.Errorf("sfuclient: not publishing %s", sid)
	}
	err := s.send(signal.KindUnpublish, signal.Unpublish{SID: sid})
	if rerr := s.pub.pc.RemoveTrack(lt.tx.Sender()); rerr != nil && err == nil {
		err = rerr
	}
	s.pub.negotiate()
	return err
}

// gaveUp runs when a publish has given up waiting, before it's forgotten
// (tests widen the gap between the two).
var gaveUp = func() {}

// unpublishLate unpublishes a track the server published after its
// publish was given up on (timed out, or failed by an error from the
// server): nobody writes to it, and nobody would unpublish it.
func (s *Session) unpublishLate(cid, sid string) {
	s.log.Info("sfuclient: published after giving up; unpublishing", "cid", cid, "sid", sid)
	if err := s.send(signal.KindUnpublish, signal.Unpublish{SID: sid}); err != nil {
		s.log.Debug("sfuclient: unpublish", "sid", sid, "err", err)
	}
}

// Mute tells the server a published track is muted, or not; it shows so
// to others. The caller stops writing to it.
func (s *Session) Mute(sid string, muted bool) error {
	s.mu.Lock()
	lt := s.local[sid]
	s.mu.Unlock()
	if lt == nil {
		return fmt.Errorf("sfuclient: not publishing %s", sid)
	}
	return s.send(signal.KindMute, signal.Mute{SID: sid, Muted: muted})
}

// newCID names a track the session publishes.
func (s *Session) newCID(what string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cids++
	return fmt.Sprintf("%s-%d", what, s.cids)
}
