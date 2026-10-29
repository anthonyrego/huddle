package sfu

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// The room's clock: sender reports are checked every tick (each down
// track sends one about every srEvery), speakers every speakersEvery.
const (
	tick          = 100 * time.Millisecond
	srEvery       = time.Second
	speakersEvery = 300 * time.Millisecond
)

// room is the people of one room and what each publishes. Its lock guards
// the participants' tables (what they publish and are sent), and messages
// about the room are queued under it, so every client hears changes in the
// order they happened: a track before the offer that carries it, a
// participant before its tracks.
type room struct {
	s    *Server
	name string
	log  *slog.Logger
	quit chan struct{}

	mu       sync.Mutex
	parts    map[string]*participant // by identity
	order    []*participant          // in the order they joined
	closed   bool
	speakers []signal.Speaker // as last sent
}

func newRoom(s *Server, name string) *room {
	r := &room{s: s, name: name, log: s.log.With("room", name), quit: make(chan struct{}), parts: map[string]*participant{}}
	go r.run()
	return r
}

// enter puts p in the room: it hears joined (everyone, with what they
// publish), everyone else hears participant, and p is sent everyone's
// tracks if it subscribes. A session already there under p's identity is
// ended with bye. False if the room has closed (it emptied meanwhile).
func (r *room) enter(p *participant) bool {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	old := r.parts[p.grant.Identity]
	if old != nil {
		r.removeLocked(old)
	}
	p.rm.Store(r)
	p.gone = false
	r.parts[p.grant.Identity] = p
	r.order = append(r.order, p)
	others := make([]signal.Participant, 0, len(r.order)-1)
	for _, o := range r.order {
		if o != p {
			others = append(others, o.infoLocked())
		}
	}
	p.send(signal.KindJoined, signal.Joined{Room: r.name, Self: p.info(), Participants: others})
	subscribed := 0
	for _, o := range r.order {
		if o == p {
			continue
		}
		o.send(signal.KindParticipant, p.info())
		for _, pub := range o.pubsLocked() {
			if p.wants(pub) {
				r.subscribeLocked(p, pub)
				subscribed++
			}
		}
	}
	if subscribed > 0 {
		p.negotiate()
	}
	r.mu.Unlock()
	if old != nil {
		old.log.Info("sfu: replaced by a new session")
		old.kick("another session joined as " + p.grant.Identity)
	}
	p.log.Info("sfu: joined", "others", len(others), "subscribe", p.all, "agent", p.grant.Agent, "local", p.local != nil)
	return true
}

// leave takes p out of the room, if it's still there.
func (r *room) leave(p *participant) {
	r.mu.Lock()
	removed := !p.gone
	if removed {
		r.removeLocked(p)
	}
	r.mu.Unlock()
	if removed {
		p.log.Info("sfu: left")
	}
}

// removeLocked takes p out: its tracks go (everyone hears untrack), what it
// was sent stops, everyone hears left. An empty room closes.
func (r *room) removeLocked(p *participant) {
	p.gone = true
	if r.parts[p.grant.Identity] == p {
		delete(r.parts, p.grant.Identity)
	}
	r.order = slices.DeleteFunc(r.order, func(o *participant) bool { return o == p })
	for _, pub := range p.pubsLocked() {
		r.unpublishLocked(pub)
	}
	for sid, dt := range p.downs {
		dt.pub.removeDown(dt)
		delete(p.downs, sid)
		dt.end()
	}
	for _, o := range r.order {
		o.send(signal.KindLeft, signal.Left{Identity: p.grant.Identity})
	}
	if len(r.order) == 0 && !r.closed {
		r.closed = true
		close(r.quit)
		r.s.dropRoom(r)
	}
}

// close ends the room (the server is closing) and returns who was in it.
func (r *room) close() []*participant {
	r.mu.Lock()
	defer r.mu.Unlock()
	ps := slices.Clone(r.order)
	if !r.closed {
		r.closed = true
		close(r.quit)
	}
	return ps
}

// publish answers a publish: the track gets its SID, the publisher hears
// published, everyone else hears track, and those who subscribe are sent
// it (their down connections are offered it once it's on them). Media
// can't arrive before this: the client offers only after published. src,
// when set, is an in-process publisher's track, the voice's only source.
func (r *room) publish(p *participant, req signal.Publish, src *LocalTrack) (*publication, error) {
	if err := checkPublish(req); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.gone {
		return nil, errors.New("not in the room")
	}
	if p.byCID[req.CID] != nil {
		return nil, fmt.Errorf("already publishing %q", req.CID)
	}
	pub := newPublication(p, newSID(), req)
	if src != nil {
		pub.src = src
	}
	p.pubs[pub.sid], p.byCID[pub.cid] = pub, pub
	p.send(signal.KindPublished, signal.Published{CID: req.CID, SID: pub.sid})
	for _, o := range r.order {
		if o == p {
			continue
		}
		o.send(signal.KindTrack, pub.info)
		if o.wants(pub) {
			r.subscribeLocked(o, pub)
			o.negotiate()
		}
	}
	p.log.Info("sfu: published", "track", pub.sid, "kind", req.Kind, "source", req.Source, "layers", len(req.Layers))
	return pub, nil
}

func checkPublish(req signal.Publish) error {
	if req.CID == "" {
		return errors.New("no cid")
	}
	switch req.Kind {
	case signal.Audio:
		if len(req.Layers) > 0 {
			return errors.New("audio has no layers")
		}
	case signal.Video:
		if len(req.Layers) == 0 {
			return errors.New("a video needs its layers")
		}
		seen := map[string]bool{}
		for _, l := range req.Layers {
			if l.RID == "" || seen[l.RID] {
				return fmt.Errorf("layer rid %q: each layer needs a rid of its own", l.RID)
			}
			seen[l.RID] = true
		}
	default:
		return fmt.Errorf("kind %q", req.Kind)
	}
	return nil
}

// unpublish stops one of p's tracks.
func (r *room) unpublish(p *participant, sid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	pub := p.pubs[sid]
	if pub == nil {
		return fmt.Errorf("no track %q", sid)
	}
	r.unpublishLocked(pub)
	p.log.Info("sfu: unpublished", "track", sid)
	return nil
}

// unpublishLocked ends pub: everyone else hears untrack, and it comes off
// their down connections (an in-process subscriber's Voice ends).
func (r *room) unpublishLocked(pub *publication) {
	p := pub.owner
	delete(p.pubs, pub.sid)
	delete(p.byCID, pub.cid)
	pub.close()
	for _, o := range r.order {
		if o == p {
			continue
		}
		o.send(signal.KindUntrack, signal.Untrack{SID: pub.sid})
		if dt := o.downs[pub.sid]; dt != nil {
			delete(o.downs, pub.sid)
			o.negotiate()
			dt.end()
		}
	}
}

// mute says one of p's tracks is muted, or not; everyone else hears the
// track changed.
func (r *room) mute(p *participant, sid string, muted bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	pub := p.pubs[sid]
	if pub == nil {
		return fmt.Errorf("no track %q", sid)
	}
	if pub.info.Muted == muted {
		return nil
	}
	pub.info.Muted = muted
	pub.muted.Store(muted)
	for _, o := range r.order {
		if o != p {
			o.send(signal.KindTrack, pub.info)
		}
	}
	p.log.Info("sfu: mute", "track", sid, "muted", muted)
	return nil
}

// settings is how p wants a video it's sent (its size, or off). Settings
// for a track p isn't sent (gone meanwhile), or a voice, are ignored: an
// error would fail whatever p waits on.
func (r *room) settings(p *participant, st signal.Settings) {
	r.mu.Lock()
	dt := p.downs[st.SID]
	r.mu.Unlock()
	if dt == nil || dt.fwd == nil {
		p.log.Debug("sfu: settings for a video not sent", "track", st.SID)
		return
	}
	dt.settings(st.W, st.H, st.Enabled, time.Now())
}

// subscribeLocked has sub sent pub: a down track, fed by pub from now on,
// that goes on sub's down connection at its next offer, or, for an
// in-process subscriber, whose Voice it's handed (after the events
// already queued for it, so a person arrives before their voice).
func (r *room) subscribeLocked(sub *participant, pub *publication) {
	dt := newDownTrack(pub, sub)
	sub.downs[pub.sid] = dt
	pub.addDown(dt)
	if sub.local != nil {
		sub.local.voice(pub, dt.voice)
	}
}

// downsOf is what p should be sent now, by SID.
func (r *room) downsOf(p *participant) map[string]*downTrack {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := make(map[string]*downTrack, len(p.downs))
	for sid, dt := range p.downs {
		want[sid] = dt
	}
	return want
}

// infoLocked is p as others are told of it, with what it publishes.
func (p *participant) infoLocked() signal.Participant {
	info := p.info()
	for _, pub := range p.pubsLocked() {
		info.Tracks = append(info.Tracks, pub.info)
	}
	return info
}

// pubsLocked are p's tracks in the order they were published.
func (p *participant) pubsLocked() []*publication {
	pubs := make([]*publication, 0, len(p.pubs))
	for _, pub := range p.pubs {
		pubs = append(pubs, pub)
	}
	slices.SortFunc(pubs, func(a, b *publication) int { return int(a.n - b.n) })
	return pubs
}

// run is the room's clock: sender reports on the down tracks, videos'
// layers paused, viewers who never said a size, and who's talking.
func (r *room) run() {
	t := time.NewTicker(tick)
	defer t.Stop()
	var lastSpeakers time.Time
	for {
		select {
		case <-r.quit:
			return
		case <-t.C:
		}
		now := time.Now()
		r.mu.Lock()
		var dts []*downTrack
		var pubs []*publication
		for _, p := range r.order {
			for _, dt := range p.downs {
				dts = append(dts, dt)
			}
			pubs = append(pubs, p.pubsLocked()...)
		}
		r.mu.Unlock()
		for _, pub := range pubs {
			if len(pub.layers) > 0 {
				pub.checkLayers(now)
			}
		}
		for _, dt := range dts {
			dt.tick(now)
			dt.reportIfDue(now)
		}
		if now.Sub(lastSpeakers) >= speakersEvery-tick/2 {
			lastSpeakers = now
			r.updateSpeakers(pubs, now)
		}
	}
}

// updateSpeakers tells the room who's talking, if that changed.
func (r *room) updateSpeakers(pubs []*publication, now time.Time) {
	list := speakersOf(pubs, now)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || sameSpeakers(list, r.speakers) {
		return
	}
	r.speakers = list
	for _, p := range r.order {
		p.send(signal.KindSpeakers, signal.Speakers{Speakers: list})
	}
}

var sidEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newSID names a track: TR_ and 16 random characters.
func newSID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return "TR_" + sidEncoding.EncodeToString(b)
}
