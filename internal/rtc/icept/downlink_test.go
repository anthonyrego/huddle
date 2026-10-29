package icept

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
)

// TestDownlink reads two streams through one narrow link: what's queued
// comes out at the link's rate, the two streams sharing it, the tail of a
// burst past the queue is dropped, and lifted, the link lets everything
// through at once.
func TestDownlink(t *testing.T) {
	var link *RecvLink
	f := &Downlink{BPS: 1_000_000, Slots: UplinkSlots, Made: func(l *RecvLink) { link = l }}
	ic, err := f.NewInterceptor("")
	if err != nil {
		t.Fatal(err)
	}
	defer ic.Close()

	// Each stream's network: 1200-byte packets, numbered, written in
	// bursts by the test.
	type net struct{ in chan []byte }
	nets := [2]net{{make(chan []byte, 100)}, {make(chan []byte, 100)}}
	var readers [2]interceptor.RTPReader
	for i := range nets {
		in := nets[i].in
		readers[i] = ic.BindRemoteStream(&interceptor.StreamInfo{SSRC: uint32(i)}, interceptor.RTPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
			return copy(b, <-in), a, nil
		}))
	}
	var mu sync.Mutex
	var got [2][]int
	var at [2][]time.Time
	var n atomic.Int64
	for i, r := range readers {
		go func() {
			buf := make([]byte, 1500)
			for {
				k, _, err := r.Read(buf, nil)
				if err != nil || k != 1200 {
					return
				}
				mu.Lock()
				got[i] = append(got[i], int(binary.BigEndian.Uint16(buf)))
				at[i] = append(at[i], time.Now())
				mu.Unlock()
				n.Add(1)
			}
		}()
	}
	pkt := func(seq int) []byte {
		b := make([]byte, 1200)
		binary.BigEndian.PutUint16(b, uint16(seq))
		return b
	}
	start := time.Now()
	for seq := range 30 { // 60 packets at once, alternating: 10 past the queue
		nets[0].in <- pkt(seq)
		nets[1].in <- pkt(seq)
	}
	waitN := func(want int64, d time.Duration) time.Duration {
		for time.Since(start) < d && n.Load() < want {
			time.Sleep(5 * time.Millisecond)
		}
		return time.Since(start)
	}
	took := waitN(50, 2*time.Second)
	time.Sleep(100 * time.Millisecond)
	if link.Dropped() != 10 || n.Load() != 50 {
		t.Fatalf("dropped %d and read %d of 60 into a 50-packet queue", link.Dropped(), n.Load())
	}
	// 50 × 1200 bytes at 1 Mbps is 480 ms (one goes at once).
	if took < 400*time.Millisecond || took > 650*time.Millisecond {
		t.Fatalf("50 packets through 1 Mbps in %v", took)
	}
	mu.Lock()
	t.Logf("through 1 Mbps: stream 0 got %d (the last at %v), stream 1 got %d (the last at %v)", len(got[0]),
		at[0][len(at[0])-1].Sub(start).Round(time.Millisecond), len(got[1]), at[1][len(at[1])-1].Sub(start).Round(time.Millisecond))
	for i := range got {
		for k := 1; k < len(got[i]); k++ {
			if got[i][k] <= got[i][k-1] {
				t.Errorf("stream %d: packet %d after %d", i, got[i][k], got[i][k-1])
			}
		}
	}
	if len(got[0]) < 20 || len(got[1]) < 20 {
		t.Errorf("the streams got %d and %d of 50: the link wasn't shared", len(got[0]), len(got[1]))
	}
	mu.Unlock()

	// Lifted: 40 more go through at once.
	link.SetRate(0)
	start = time.Now()
	for seq := 100; seq < 120; seq++ {
		nets[0].in <- pkt(seq)
		nets[1].in <- pkt(seq)
	}
	if took := waitN(90, time.Second); took > 100*time.Millisecond {
		t.Fatalf("lifted, 40 packets took %v (read %d)", took, n.Load()-50)
	}
	if link.Dropped() != 10 {
		t.Errorf("lifted, the link dropped %d more", link.Dropped()-10)
	}
}
