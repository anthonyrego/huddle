package room

// Dynacast through the room: a camera published on huddle's SFU (in
// process, on loopback) pauses the layers nobody is sent, and starts one
// again the moment a viewer asks for it.

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/rtc"
)

// fakeCam is a VideoSource of stand-in access units: every 33 ms a
// keyframe (SPS, PPS and an IDR slice) on each layer given a bitrate.
type fakeCam struct {
	layers []rtc.Layer
	stop   chan struct{}
	done   chan struct{}

	mu   sync.Mutex
	bps  []int
	sets []bpsSet // every SetBitrates, in order
}

type bpsSet struct {
	bps []int
	at  time.Time
}

func newFakeCam() *fakeCam {
	ls := []rtc.Layer{{W: 1280, H: 720, Bitrate: 1_700_000}, {W: 640, H: 360, Bitrate: 450_000}, {W: 320, H: 180, Bitrate: 160_000}}
	return &fakeCam{layers: ls, bps: make([]int, len(ls)), stop: make(chan struct{}), done: make(chan struct{})}
}

func (c *fakeCam) Layers() []rtc.Layer { return c.layers }
func (c *fakeCam) KeyFrame(int)        {}

func (c *fakeCam) Start(write func(layer int, au []byte, pts time.Duration)) error {
	au := append([]byte{0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x1f, 0xab, 0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80, 0, 0, 0, 1, 0x65}, make([]byte, 1500)...)
	for i := 22; i < len(au); i++ {
		au[i] = 0x55
	}
	start := time.Now()
	go func() {
		defer close(c.done)
		for {
			select {
			case <-c.stop:
				return
			case <-time.After(33 * time.Millisecond):
			}
			c.mu.Lock()
			bps := slices.Clone(c.bps)
			c.mu.Unlock()
			for i, b := range bps {
				if b > 0 {
					write(i, au, time.Since(start))
				}
			}
		}
	}()
	return nil
}

func (c *fakeCam) SetBitrates(bps []int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bps = slices.Clone(bps)
	c.sets = append(c.sets, bpsSet{slices.Clone(bps), time.Now()})
}

func (c *fakeCam) Stop() {
	close(c.stop)
	<-c.done
}

// when is the first SetBitrates after at that ok accepts.
func (c *fakeCam) when(at time.Time, ok func([]int) bool) (time.Time, []int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sets {
		if s.at.After(at) && ok(s.bps) {
			return s.at, s.bps, true
		}
	}
	return time.Time{}, nil, false
}

// A camera with no viewer pauses every layer once the SFU's delay after a
// publish has passed; a viewer asking for 180p has that layer back within a
// moment (the wanted change shares the estimate out at once, not at the
// next tick), the others still paused; turned off, it pauses again.
func TestBuiltinDynacast(t *testing.T) {
	ts := newTestSFU(t)
	ctx, cancel := context.WithTimeout(context.Background(), sfuWait)
	defer cancel()
	ann, err := (&Connector{StatsEvery: -1}).Connect(ctx, ts.join(t, "d", "ann", false), rtc.Events{})
	if err != nil {
		t.Fatal(err)
	}
	defer ann.Leave()
	cam := newFakeCam()
	published := time.Now()
	vp, err := ann.PublishVideo(cam, rtc.Camera)
	if err != nil {
		t.Fatal(err)
	}
	defer vp.Stop()
	allOff := func(b []int) bool { return slices.Equal(b, []int{0, 0, 0}) }
	var off time.Time
	waitUntil(t, 2*sfuWait, "every layer paused with nobody watching", func() bool {
		at, _, ok := cam.when(published, allOff)
		off = at
		return ok
	})
	t.Logf("nobody watching: every layer paused %v after the publish", off.Sub(published).Round(time.Millisecond))
	if d := off.Sub(published); d < 2500*time.Millisecond {
		t.Errorf("every layer paused %v after the publish, before the SFU's delay", d)
	}
	st := VideoSendStats(ann).Videos[0]
	if !slices.Equal(st.Wanted, []bool{false, false, false}) || !slices.Equal(st.Paused, []string{"unwanted", "unwanted", "unwanted"}) {
		t.Errorf("wanted %v, paused %v", st.Wanted, st.Paused)
	}

	bob := newRecorder()
	dialTest(t, ts, "d", "bob", false, bob.handlers())
	waitUntil(t, sfuWait, "bob to be sent ann's camera", func() bool { return bob.track("ann", webrtc.RTPCodecTypeVideo) != nil })
	hv := bob.track("ann", webrtc.RTPCodecTypeVideo)
	asked := time.Now()
	hv.rt.video.setSize(320, 180)
	var on time.Time
	var bps []int
	waitUntil(t, sfuWait, "180p back", func() bool {
		var ok bool
		on, bps, ok = cam.when(asked, func(b []int) bool { return b[2] > 0 })
		return ok
	})
	t.Logf("bob at 320x180: 180p back at %v, %v after asking", bps, on.Sub(asked).Round(100*time.Microsecond))
	if d := on.Sub(asked); d > 300*time.Millisecond || bps[0] != 0 || bps[1] != 0 {
		t.Errorf("180p back %v after asking, at %v", d, bps)
	}
	waitUntil(t, sfuWait, "bob to be sent 180p", func() bool { return hv.packets.Load() > 10 })
	st = VideoSendStats(ann).Videos[0]
	if !slices.Equal(st.Wanted, []bool{false, false, true}) || !slices.Equal(st.Paused, []string{"unwanted", "unwanted", "-"}) {
		t.Errorf("bob at 320x180: wanted %v, paused %v", st.Wanted, st.Paused)
	}

	stopped := time.Now()
	hv.rt.video.setEnabled(false)
	waitUntil(t, sfuWait, "180p paused again", func() bool {
		at, _, ok := cam.when(stopped, allOff)
		off = at
		return ok
	})
	t.Logf("bob off: 180p paused %v after", off.Sub(stopped).Round(time.Millisecond))
	if d := off.Sub(stopped); d < 2500*time.Millisecond {
		t.Errorf("180p paused %v after bob turned it off, before the shrink delay", d)
	}
}
