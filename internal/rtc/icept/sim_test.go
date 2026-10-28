package icept

import (
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"

	"github.com/anthonyrego/huddle/internal/netsim"
)

// TestSimInterceptor reads a stream through the simulator: about the
// asked-for share is lost, the rest arrives in order.
func TestSimInterceptor(t *testing.T) {
	f := Sim{func() *netsim.Sim { return netsim.New(0.2, time.Millisecond, 0, 9) }}
	ic, err := f.NewInterceptor("")
	if err != nil {
		t.Fatal(err)
	}
	defer ic.Close()
	var n atomic.Int64
	src := interceptor.RTPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		time.Sleep(200 * time.Microsecond)
		binary.BigEndian.PutUint16(b, uint16(n.Add(1)))
		return 2, a, nil
	})
	r := ic.BindRemoteStream(&interceptor.StreamInfo{}, src)
	buf := make([]byte, 1500)
	last, got := 0, 0
	deadline := time.Now().Add(2 * time.Second)
	for got < 400 && time.Now().Before(deadline) {
		k, attr, err := r.Read(buf, nil)
		if err != nil || k != 2 || attr == nil {
			t.Fatalf("read %d, %v, %v", k, attr, err)
		}
		seq := int(binary.BigEndian.Uint16(buf))
		if seq <= last {
			t.Fatalf("packet %d after %d", seq, last)
		}
		last = seq
		got++
	}
	if loss := 1 - float64(got)/float64(last); loss < 0.1 || loss > 0.3 {
		t.Fatalf("lost %.0f%% of %d, want about 20%%", loss*100, last)
	}
	if ic.BindRemoteStream(nil, src) == nil {
		t.Fatal("no reader")
	}
	off := Sim{func() *netsim.Sim { return nil }}
	oi, _ := off.NewInterceptor("")
	if r := oi.BindRemoteStream(&interceptor.StreamInfo{}, src); r == nil {
		t.Fatal("an inactive sim should pass the reader through")
	}
}
