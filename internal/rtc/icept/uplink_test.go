package icept

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

// writes records what reaches the network, and when.
type writes struct {
	mu   sync.Mutex
	ssrc []uint32
	at   []time.Time
}

func (w *writes) Write(h *rtp.Header, p []byte, _ interceptor.Attributes) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ssrc = append(w.ssrc, h.SSRC)
	w.at = append(w.at, time.Now())
	return len(p), nil
}

func TestUplink(t *testing.T) {
	f := &Uplink{BPS: 1_000_000, Slots: UplinkSlots}
	ic, _ := f.NewInterceptor("")
	u := ic.(*Link)
	defer u.Close()
	var w writes
	wr := u.BindLocalStream(&interceptor.StreamInfo{}, &w)
	payload := make([]byte, 1188) // 1200 with the header
	start := time.Now()
	for range 60 {
		wr.Write(&rtp.Header{}, payload, nil)
	}
	if u.Dropped() != 10 {
		t.Fatalf("dropped %d of 60 into a 50-packet queue", u.Dropped())
	}
	for time.Since(start) < 2*time.Second {
		w.mu.Lock()
		n := len(w.at)
		w.mu.Unlock()
		if n == 50 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	took := time.Since(start)
	// 50 × 1200 bytes at 1 Mbps is 480 ms (one goes at once).
	if took < 400*time.Millisecond || took > 650*time.Millisecond {
		t.Fatalf("50 packets through 1 Mbps in %v", took)
	}
}
