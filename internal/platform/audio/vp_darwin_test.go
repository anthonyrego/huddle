package audio

import (
	"math"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego/objc"

	"github.com/anthonyrego/huddle/internal/pcm"
	"github.com/anthonyrego/huddle/internal/platform"
)

// TestVoiceProcessingSelectors checks every class and selector the engine
// uses against the AVFAudio on this Mac, without creating an engine or
// touching a device: a misspelled selector would otherwise raise an
// Objective-C exception, and kill the process, only when the user runs it.
func TestVoiceProcessingSelectors(t *testing.T) {
	if err := loadAVF(); err != nil {
		t.Fatal(err)
	}
	responds := objc.RegisterName("instancesRespondToSelector:")
	for class, names := range map[string][]string{
		"AVAudioEngine": {"inputNode", "outputNode", "mainMixerNode", "attachNode:", "connect:to:format:",
			"prepare", "startAndReturnError:", "stop", "isRunning"},
		"AVAudioInputNode": {"setVoiceProcessingEnabled:error:", "isVoiceProcessingEnabled", "outputFormatForBus:",
			"inputFormatForBus:", "installTapOnBus:bufferSize:format:block:", "removeTapOnBus:", "presentationLatency",
			"setVoiceProcessingAGCEnabled:", "isVoiceProcessingAGCEnabled"},
		"AVAudioOutputNode": {"presentationLatency", "inputFormatForBus:", "outputFormatForBus:"},
		"AVAudioPlayerNode": {"play", "stop", "scheduleBuffer:completionCallbackType:completionHandler:"},
		"AVAudioFormat": {"initWithCommonFormat:sampleRate:channels:interleaved:",
			"initStandardFormatWithSampleRate:channels:", "sampleRate", "channelCount"},
		"AVAudioPCMBuffer": {"initWithPCMFormat:frameCapacity:", "setFrameLength:", "frameLength", "floatChannelData", "stride"},
		"NSError":          {"localizedDescription", "code"},
		"NSString":         {"UTF8String"},
	} {
		c := objc.GetClass(class)
		if c == 0 {
			t.Errorf("no class %s", class)
			continue
		}
		for _, name := range names {
			if !objc.Send[bool](objc.ID(c), responds, objc.RegisterName(name)) {
				t.Errorf("%s doesn't respond to %s", class, name)
			}
		}
	}
	// Ducking is set only where it exists (macOS 14+): say which this is.
	ducking := objc.Send[bool](objc.ID(objc.GetClass("AVAudioInputNode")), responds, sel.setOtherAudioDucking)
	t.Logf("other-audio ducking configuration available: %v", ducking)
}

// TestVoiceProcessingBuffers fills an AVAudioPCMBuffer as the speaker does
// and reads it as the tap does, through the converter to s16 at InRate. No
// engine, no device.
func TestVoiceProcessingBuffers(t *testing.T) {
	if err := loadAVF(); err != nil {
		t.Fatal(err)
	}
	platform.LoadSDL()
	withPool(func() {
		const rate, frames = 48000, 4800 // 100 ms
		f := objc.ID(cls.format).Send(sel.alloc).Send(sel.initWithCommonFormat, uint(pcmFormatFloat32), float64(rate), uint32(1), false)
		if f == 0 {
			t.Fatal("no AVAudioFormat")
		}
		defer f.Send(sel.release)
		if got := formatDesc(f); got != "48000Hz/1ch" {
			t.Fatalf("format %s, want 48000Hz/1ch", got)
		}
		stereo := objc.ID(cls.format).Send(sel.alloc).Send(sel.initStandardFormat, float64(rate), uint32(2))
		if got := formatDesc(stereo); got != "48000Hz/2ch" {
			t.Fatalf("standard format %s, want 48000Hz/2ch", got)
		}
		stereo.Send(sel.release)
		buf := objc.ID(cls.pcmBuffer).Send(sel.alloc).Send(sel.initWithPCMFormat, f, uint32(frames))
		if buf == 0 {
			t.Fatal("no AVAudioPCMBuffer")
		}
		defer buf.Send(sel.release)
		buf.Send(sel.setFrameLength, uint32(frames))
		if n := objc.Send[uint32](buf, sel.frameLength); n != frames {
			t.Fatalf("frameLength %d, want %d", n, frames)
		}
		if s := objc.Send[uint](buf, sel.stride); s != 1 {
			t.Fatalf("stride %d, want 1 (non-interleaved mono)", s)
		}
		data := objc.Send[**float32](buf, sel.floatChannelData)
		samples := unsafe.Slice(*data, frames)
		for i := range samples { // a 440 Hz tone at half scale: -9 dBFS RMS
			samples[i] = float32(0.5 * math.Sin(2*math.Pi*440*float64(i)/rate))
		}

		e := &vpEngine{}
		var err error
		if e.conv, err = newMicConverter(); err != nil {
			t.Fatal(err)
		}
		defer e.conv.Destroy()
		if err := e.setMicRate(rate); err != nil {
			t.Fatal(err)
		}
		e.tapData(0, buf, 0)
		e.conv.Flush()
		m := &vpMic{e}
		want := int(pcm.Bytes(100_000_000, InRate)) // 100 ms at InRate
		got := m.Available()
		if got < want*9/10 || got > want*11/10 {
			t.Fatalf("%d bytes at InRate from 100 ms, want about %d", got, want)
		}
		out := make([]byte, got)
		if _, err := m.Read(out); err != nil {
			t.Fatal(err)
		}
		if l := pcm.Level(out[len(out)/4 : len(out)*3/4]); math.Abs(l+9.03) > 0.5 {
			t.Fatalf("converted level %.2f dBFS, want -9.03", l)
		}
	})
}

// TestNSError reads an NSError the way a failed start is reported.
func TestNSError(t *testing.T) {
	if err := loadAVF(); err != nil {
		t.Fatal(err)
	}
	withPool(func() {
		domain := objc.ID(objc.GetClass("NSString")).Send(objc.RegisterName("stringWithUTF8String:"), "com.apple.coreaudio.avfaudio\x00")
		nerr := objc.ID(objc.GetClass("NSError")).Send(objc.RegisterName("errorWithDomain:code:userInfo:"), domain, -10875, objc.ID(0))
		got := nsError(nerr)
		if !strings.Contains(got, "(-10875)") {
			t.Fatalf("nsError = %q, want the code -10875 in it", got)
		}
		t.Log(got)
	})
}

func TestVoiceProcessingStalled(t *testing.T) {
	t0 := time.Unix(100, 0)
	at := func(d time.Duration) int64 { return t0.Add(d).UnixNano() }
	e := &vpEngine{}
	e.startedAt.Store(at(0))
	if why := e.stalled(t0.Add(time.Second)); why != "" {
		t.Fatalf("1 s after a start with no mic yet: %q, want nothing (too soon)", why)
	}
	if why := e.stalled(t0.Add(2 * time.Second)); why != "the mic has gone silent" {
		t.Fatalf("2 s after a start with no mic: %q", why)
	}
	e.lastTap.Store(at(1900 * time.Millisecond))
	if why := e.stalled(t0.Add(2 * time.Second)); why != "" {
		t.Fatalf("mic 100 ms ago: %q, want nothing", why)
	}
	e.waiting.Store(at(500 * time.Millisecond))
	if why := e.stalled(t0.Add(2500 * time.Millisecond)); why != "nothing it plays is heard" {
		t.Fatalf("a buffer waiting 2 s: %q", why)
	}
	// A restart resets the clocks: what waited before it doesn't count.
	e.startedAt.Store(at(2400 * time.Millisecond))
	if why := e.stalled(t0.Add(2500 * time.Millisecond)); why != "" {
		t.Fatalf("100 ms after a restart: %q, want nothing", why)
	}
}
