package icept

import (
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

func TestPacerAudioFirst(t *testing.T) {
	p := NewPacer(400_000) // 600 kbps of video
	defer p.Close()
	var w writes
	p.stream(&interceptor.StreamInfo{SSRC: 1, MimeType: "audio/opus"})
	p.stream(&interceptor.StreamInfo{SSRC: 2, MimeType: "video/H264", SSRCRetransmission: 3})
	p.AddStream(1, &w)
	p.AddStream(2, &w)
	// A 60 KB keyframe, then a voice packet: the voice goes at once.
	payload := make([]byte, 1200)
	start := time.Now()
	for range 50 {
		p.Write(&rtp.Header{SSRC: 2}, payload, nil)
	}
	p.Write(&rtp.Header{SSRC: 1}, payload[:100], nil)
	if _, err := p.Write(&rtp.Header{SSRC: 3}, payload, nil); err != nil {
		t.Fatalf("a retransmission: %v", err)
	}
	if _, err := p.Write(&rtp.Header{SSRC: 9}, payload, nil); err == nil {
		t.Fatal("an unknown stream was taken")
	}
	time.Sleep(50 * time.Millisecond)
	w.mu.Lock()
	audioAt := -1
	for i, s := range w.ssrc {
		if s == 1 {
			audioAt = i
		}
	}
	sentEarly := len(w.ssrc)
	w.mu.Unlock()
	if audioAt < 0 || audioAt > 3 {
		t.Fatalf("the voice went out %dth", audioAt)
	}
	// The rest drain at paceFactor × 400 kbps: 61 KB in about a second.
	if sentEarly > 20 {
		t.Fatalf("%d packets out in 50 ms: not paced", sentEarly)
	}
	for time.Since(start) < 3*time.Second {
		w.mu.Lock()
		n := len(w.ssrc)
		w.mu.Unlock()
		if n == 52 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	took := time.Since(start)
	if took < 600*time.Millisecond || took > 1300*time.Millisecond {
		t.Fatalf("drained in %v, want about a second", took)
	}
	if r := p.SentRate(); r < 400_000 {
		t.Fatalf("sent rate %d", r)
	}
}
