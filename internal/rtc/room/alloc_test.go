package room

// The allocator under Dynacast: layers nobody is sent, layers wanted
// again, stand-ins, and probes. (TestAllocate and TestSqueeze, in
// video_test.go, are the allocator with every layer wanted.)

import (
	"slices"
	"testing"
	"time"
)

var camFull = []int{1_700_000, 450_000, 160_000}

// layerOf is p's layer i.
func (a *allocator) layerOf(p *videoPub, i int) *allocLayer {
	for _, l := range a.layers {
		if l.pub == p && l.layer == i {
			return l
		}
	}
	return nil
}

// running starts a camera with every layer at its full rate.
func running(t *testing.T, now time.Time) (*allocator, *videoPub) {
	t.Helper()
	cam := &videoPub{}
	a := &allocator{}
	a.add(cam, camFull, now)
	a.allocate(3_000_000, 0, now)
	if got := a.bitrates(cam, 3); !slices.Equal(got, camFull) {
		t.Fatalf("at 3 Mbps from the start: %v", got)
	}
	return a, cam
}

func checkStates(t *testing.T, a *allocator, cam *videoPub, what string, wantBps []int, wantPaused []string) {
	t.Helper()
	got := a.bitrates(cam, 3)
	_, paused := a.states(cam, 3)
	if wantBps != nil && !slices.Equal(got, wantBps) || !slices.Equal(paused, wantPaused) {
		t.Fatalf("%s: %v, paused %v; want %v, %v", what, got, paused, wantBps, wantPaused)
	}
}

// A layer nobody wants gets nothing at once; it isn't a failed try (no
// hold), and doesn't pause the layers above it. Wanted again, it comes
// back at its floor at once, and grows once the cap counts it.
func TestAllocateUnwanted(t *testing.T) {
	now := time.Now()
	a, cam := running(t, now)
	now = now.Add(5 * time.Second)
	a.want(cam, []bool{false, false, true})
	a.allocate(3_000_000, 2_300_000, now)
	checkStates(t, a, cam, "only 180p wanted", []int{0, 0, 160_000}, []string{"unwanted", "unwanted", "-"})
	for i := range 2 {
		if l := a.layerOf(cam, i); !l.holdUntil.IsZero() || l.hold != holdFirst || !l.unwanted || l.ranAt != camFull[i] {
			t.Fatalf("layer %d once unwanted: %+v", i, l)
		}
	}
	// 180p and 720p: 360p, unwanted, doesn't keep 720p from coming back.
	now = now.Add(time.Second)
	a.want(cam, []bool{true, false, true})
	a.allocate(3_000_000, 200_000, now)
	checkStates(t, a, cam, "720p wanted again", []int{680_000, 0, 160_000}, []string{"-", "unwanted", "-"})
	if w := a.whys(cam, 3); w[0] != "wanted" || w[1] != "unwanted" {
		t.Fatalf("why: %v", w)
	}
	now = now.Add(freeFor + time.Second)
	a.allocate(3_000_000, 900_000, now)
	if got := a.bitrates(cam, 3); got[0] != 1_250_000 {
		t.Fatalf("once the cap counts 720p (1.5 × 900k + 100k): %v", got)
	}
	// Nobody at all: everything pauses, none of it failed.
	now = now.Add(5 * time.Second)
	a.want(cam, []bool{false, false, false})
	a.allocate(3_000_000, 1_400_000, now)
	checkStates(t, a, cam, "nothing wanted", []int{0, 0, 0}, []string{"unwanted", "unwanted", "unwanted"})
	for i := range 3 {
		if l := a.layerOf(cam, i); !l.holdUntil.IsZero() {
			t.Fatalf("layer %d held: %+v", i, l)
		}
	}
	a.allocate(3_000_000, 40_000, now.Add(10*time.Second))
	a.want(cam, []bool{false, false, true})
	a.allocate(3_000_000, 40_000, now.Add(10*time.Second))
	checkStates(t, a, cam, "180p wanted after 10 s with nothing", []int{0, 0, 64_000}, []string{"unwanted", "unwanted", "-"})
}

// A layer paused for nobody wanting it doesn't come back at once when the
// estimate has dropped since, when the estimate doesn't leave room for
// resumeMargin times its floor, or when it ran below its floor: it takes
// the probe path, and meanwhile the next smaller layer stands in for it.
func TestAllocateResumeRefused(t *testing.T) {
	t.Run("dropped since", func(t *testing.T) {
		now := time.Now()
		a, cam := running(t, now)
		now = now.Add(5 * time.Second)
		a.want(cam, []bool{false, false, true})
		a.allocate(3_000_000, 2_300_000, now)
		dropped := now.Add(2 * time.Second)
		a.allocate(2_000_000, 200_000, dropped) // under 0.85 of 3 Mbps
		a.want(cam, []bool{true, false, true})
		at := dropped.Add(time.Second)
		a.allocate(2_400_000, 200_000, at)
		checkStates(t, a, cam, "720p wanted after a drop", []int{0, 0, 160_000}, []string{"bandwidth", "unwanted", "-"})
		// Next round 360p stands in for 720p; it can't come back at once
		// either, and waits for calm to be probed for, 180p carrying.
		a.allocate(2_400_000, 200_000, at.Add(500*time.Millisecond))
		checkStates(t, a, cam, "360p standing in", []int{0, 0, 160_000}, []string{"bandwidth", "bandwidth", "-"})
		if !a.layerOf(cam, 1).needed {
			t.Fatal("360p not needed while 720p isn't running")
		}
		var probed time.Time
		for i := 1; i <= 40 && probed.IsZero(); i++ {
			tt := at.Add(time.Duration(i) * 500 * time.Millisecond)
			a.allocate(2_400_000, 200_000, tt)
			if got := a.bitrates(cam, 3); got[2] > 160_000 {
				probed = tt
				if got[0] != 0 || got[1] != 0 || a.probe.target != a.layerOf(cam, 1) {
					t.Fatalf("the probe: %v, for %+v", got, a.probe.target)
				}
			}
		}
		if probed.IsZero() || probed.Sub(dropped) < calm {
			t.Fatalf("360p probed for %v after the drop", probed.Sub(dropped))
		}
	})
	t.Run("no room", func(t *testing.T) {
		now := time.Now()
		a, cam := running(t, now)
		now = now.Add(5 * time.Second)
		a.want(cam, []bool{false, false, true})
		a.allocate(3_000_000, 2_300_000, now)
		a.allocate(2_600_000, 200_000, now.Add(time.Second)) // not a drop
		a.allocate(2_300_000, 200_000, now.Add(2*time.Second))
		a.allocate(1_980_000, 200_000, now.Add(3*time.Second))
		a.allocate(1_700_000, 200_000, now.Add(4*time.Second))
		a.allocate(1_450_000, 200_000, now.Add(5*time.Second))
		a.allocate(1_250_000, 200_000, now.Add(6*time.Second))
		a.allocate(1_070_000, 200_000, now.Add(7*time.Second))
		a.allocate(950_000, 200_000, now.Add(8*time.Second))
		if !a.dropAt.IsZero() {
			t.Fatal("a slide counted as a drop")
		}
		// 950k − 40k − 160k leaves 750k: under 1.15 × 680k.
		a.want(cam, []bool{true, false, true})
		a.allocate(950_000, 200_000, now.Add(9*time.Second))
		checkStates(t, a, cam, "720p wanted at 950 kbps", []int{0, 0, 160_000}, []string{"bandwidth", "unwanted", "-"})
	})
	t.Run("ran below its floor", func(t *testing.T) {
		now := time.Now()
		a, cam := running(t, now)
		// The cap squeezes 720p to 550k (over 0.7 of its floor, under it).
		now = now.Add(5 * time.Second)
		a.allocate(3_000_000, 733_334, now)
		if got := a.bitrates(cam, 3); got[0] < 549_000 || got[0] > 551_000 {
			t.Fatalf("squeezed: %v", got)
		}
		a.want(cam, []bool{false, true, true})
		a.allocate(3_000_000, 733_334, now.Add(time.Second))
		a.want(cam, []bool{true, true, true})
		a.allocate(3_000_000, 733_334, now.Add(2*time.Second))
		checkStates(t, a, cam, "720p wanted again", []int{0, 450_000, 160_000}, []string{"bandwidth", "-", "-"})
	})
}

// A smaller layer wanted again while a larger one of its video runs starts
// at its floor at once, even when it can't resume as an unwanted layer
// does (the estimate dropped since it paused), and never pauses the larger
// one: waiting there, it took a running 720p down with it, counted as a
// failed try, for a phase with GCC's estimate at 5-8 Mbps.
func TestAllocateUnderARunningLayer(t *testing.T) {
	now := time.Now()
	a, cam := running(t, now)
	now = now.Add(5 * time.Second)
	a.want(cam, []bool{true, false, false})
	a.allocate(3_000_000, 2_300_000, now)
	checkStates(t, a, cam, "only 720p wanted", []int{1_700_000, 0, 0}, []string{"-", "unwanted", "unwanted"})
	dropped := now.Add(2 * time.Second)
	a.allocate(2_400_000, 1_700_000, dropped) // under 0.85 of 3 Mbps; 720p still fits
	if a.dropAt.IsZero() || a.bitrates(cam, 3)[0] == 0 {
		t.Fatalf("the drop: at %v, %v", a.dropAt, a.bitrates(cam, 3))
	}
	at := dropped.Add(10 * time.Second)
	a.allocate(4_000_000, 1_700_000, at)
	a.want(cam, []bool{true, false, true})
	a.allocate(4_000_000, 1_700_000, at.Add(time.Second))
	checkStates(t, a, cam, "180p wanted beside 720p", []int{1_700_000, 0, 64_000}, []string{"-", "unwanted", "-"})
	if hold := a.layerOf(cam, 0).holdUntil; !hold.IsZero() {
		t.Fatalf("720p given a hold until %v", hold.Sub(at))
	}
	if w := a.whys(cam, 3); w[2] != "wanted" {
		t.Fatalf("why: %v", w)
	}
	// With no room for 180p's floor beside what runs, it waits; 720p, over
	// it, can't run either.
	b, cam2 := running(t, now)
	b.want(cam2, []bool{true, false, false})
	b.allocate(3_000_000, 2_300_000, now)
	b.allocate(2_400_000, 1_700_000, dropped)
	b.want(cam2, []bool{true, false, true})
	b.allocate(100_000, 1_700_000, at)
	checkStates(t, b, cam2, "180p wanted at 100 kbps", []int{0, 0, 0}, []string{"bandwidth", "unwanted", "bandwidth"})
}

// Video is paced from the cap as though the layers nobody wants still
// sent what they ran at, so a 720p frame alone leaves as fast as with
// every layer running; never over GCC's estimate.
func TestAllocatePace(t *testing.T) {
	now := time.Now()
	a, cam := running(t, now)
	now = now.Add(5 * time.Second)
	a.allocate(4_000_000, 2_350_000, now)
	if got, want := a.pace(4_000_000, 2_350_000), 3_625_000; got != want {
		t.Fatalf("every layer running: paced from %d, want %d", got, want)
	}
	a.want(cam, []bool{true, false, false})
	a.allocate(4_000_000, 1_740_000, now.Add(time.Second))
	if got, want := a.pace(4_000_000, 1_740_000), 3_625_000; got != want {
		t.Fatalf("only 720p wanted: paced from %d, want %d", got, want)
	}
	if got := a.pace(2_000_000, 1_740_000); got != 2_000_000 {
		t.Fatalf("at 2 Mbps: paced from %d", got)
	}
	// Wanted again, 180p sends and counts as sent; 360p, still unwanted,
	// as what it ran at.
	a.want(cam, []bool{true, false, true})
	a.allocate(4_000_000, 1_740_000, now.Add(2*time.Second))
	if got, want := a.pace(4_000_000, 1_804_000), 3_481_000; got != want {
		t.Fatalf("180p back: paced from %d, want %d", got, want)
	}
}

// A wanted layer the estimate can't carry has the next smaller one stand
// in for it, and a probe for it rides the highest wanted layer running;
// once it runs, the stand-in, unwanted, pauses.
func TestAllocateStandIn(t *testing.T) {
	cam := &videoPub{}
	var a allocator
	now := time.Now()
	a.add(cam, camFull, now)
	a.allocate(1_200_000, 0, now) // 180p and 360p at once, 720p waits
	a.want(cam, []bool{true, false, false})
	now = now.Add(time.Second)
	a.allocate(1_200_000, 650_000, now)
	checkStates(t, &a, cam, "only 720p wanted", []int{0, 450_000, 0}, []string{"bandwidth", "-", "unwanted"})
	var on time.Time
	for i := 1; i <= 30 && on.IsZero(); i++ {
		at := now.Add(time.Duration(i) * 500 * time.Millisecond)
		a.allocate(2_500_000, 490_000+min(i, 9)*80_000, at)
		got := a.bitrates(cam, 3)
		if got[2] != 0 {
			t.Fatalf("180p on while only 720p is wanted: %v", got)
		}
		if a.probe != nil && a.probe.carrier != a.layerOf(cam, 1) {
			t.Fatal("the probe isn't carried by 360p")
		}
		if got[0] > 0 {
			on = at
		}
	}
	if on.IsZero() {
		t.Fatal("720p never came")
	}
	// The round 720p comes back in, 360p still stands in; the next, it
	// isn't needed.
	checkStates(t, &a, cam, "720p on", []int{680_000, 450_000, 0}, []string{"-", "-", "unwanted"})
	a.allocate(2_500_000, 1_300_000, on.Add(500*time.Millisecond))
	checkStates(t, &a, cam, "720p running", []int{680_000, 0, 0}, []string{"-", "unwanted", "unwanted"})
}

// A layer nobody wants is never probed for, however much room GCC says
// there is; a probe whose target stops being wanted ends without counting
// against it.
func TestAllocateUnwantedProbes(t *testing.T) {
	cam := &videoPub{}
	var a allocator
	now := time.Now()
	a.add(cam, camFull, now)
	a.allocate(1_200_000, 0, now) // 180p and 360p at once, 720p waits
	a.want(cam, []bool{false, false, true})
	for i := 1; i <= 120; i++ {
		a.allocate(5_000_000, 200_000, now.Add(time.Duration(i)*500*time.Millisecond))
		if got := a.bitrates(cam, 3); !slices.Equal(got, []int{0, 0, 160_000}) || a.probe != nil {
			t.Fatalf("after %v with only 180p wanted: %v, probe %v", time.Duration(i)*500*time.Millisecond, got, a.probe != nil)
		}
	}
	// 720p and 360p wanted: 360p comes back at its floor at once (it ran
	// at its full rate), and a probe for 720p begins over it.
	now = now.Add(time.Minute)
	a.want(cam, []bool{true, true, false})
	a.allocate(5_000_000, 200_000, now)
	if got := a.bitrates(cam, 3); got[1] != 180_000 || got[0] != 0 {
		t.Fatalf("720p and 360p wanted: %v", got)
	}
	var began time.Time
	for i := 1; i <= 20 && began.IsZero(); i++ {
		at := now.Add(time.Duration(i) * 500 * time.Millisecond)
		a.allocate(5_000_000, 500_000, at)
		if a.probe != nil {
			began = at
		}
	}
	if began.IsZero() || a.probe.carrier != a.layerOf(cam, 1) {
		t.Fatal("no probe for 720p over 360p")
	}
	hold := a.layerOf(cam, 0).holdUntil
	a.allocate(5_000_000, 700_000, began.Add(time.Second))
	a.want(cam, []bool{false, true, false})
	a.allocate(5_000_000, 700_000, began.Add(2*time.Second))
	if a.probe != nil || !a.layerOf(cam, 0).holdUntil.Equal(hold) {
		t.Fatalf("720p unwanted mid-probe: probe %v, hold until %v (was %v)", a.probe != nil, a.layerOf(cam, 0).holdUntil, hold)
	}
	// 360p, back in its first seconds, keeps its floor, without the boost.
	if got := a.bitrates(cam, 3); !slices.Equal(got, []int{0, 180_000, 0}) {
		t.Fatalf("after the probe ended: %v", got)
	}
}

// With nothing running to carry a probe (every layer paused by the
// estimate), the lowest needed layer starts at its floor once its hold is
// over: it used to stay paused for good.
func TestAllocateNoCarrier(t *testing.T) {
	now := time.Now()
	a, cam := running(t, now)
	now = now.Add(time.Minute)
	a.allocate(3_000_000, 2_300_000, now)
	collapsed := now.Add(time.Second)
	a.allocate(80_000, 2_300_000, collapsed)
	checkStates(t, a, cam, "at 80 kbps", []int{0, 0, 0}, []string{"bandwidth", "bandwidth", "bandwidth"})
	hold := a.layerOf(cam, 2).holdUntil
	var on time.Time
	for i := 1; i <= 60 && on.IsZero(); i++ {
		at := collapsed.Add(time.Duration(i) * 500 * time.Millisecond)
		a.allocate(400_000, 40_000, at)
		if got := a.bitrates(cam, 3); got[2] > 0 {
			on = at
			if got[2] != 64_000 || got[1] != 0 || got[0] != 0 {
				t.Fatalf("back with nothing to probe with: %v", got)
			}
		}
	}
	if on.IsZero() || on.Before(hold) {
		t.Fatalf("180p back at %v, its hold until %v", on.Sub(collapsed), hold.Sub(collapsed))
	}
	if w := a.whys(cam, 3); w[2] != "started" {
		t.Fatalf("why: %v", w)
	}
}
