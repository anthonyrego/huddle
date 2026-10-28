package audio

import (
	"context"
	"fmt"
	"math"
	"os"
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
			"inputFormatForBus:", "installTapOnBus:bufferSize:format:block:", "removeTapOnBus:",
			"setVoiceProcessingAGCEnabled:", "isVoiceProcessingAGCEnabled", "audioUnit"},
		"AVAudioOutputNode": {"presentationLatency", "inputFormatForBus:", "outputFormatForBus:", "audioUnit"},
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
// and reads it as the tap does, through the converter and the paced queue
// to float32 at 48 kHz, from a device at 48 kHz and at 44.1 kHz. No
// engine, no device.
func TestVoiceProcessingBuffers(t *testing.T) {
	if err := loadAVF(); err != nil {
		t.Fatal(err)
	}
	platform.LoadSDL()
	for _, rate := range []int{48000, 44100} {
		withPool(func() {
			frames := rate / 10 // 100 ms, as the tap delivers
			f := objc.ID(cls.format).Send(sel.alloc).Send(sel.initWithCommonFormat, uint(pcmFormatFloat32), float64(rate), uint32(1), false)
			if f == 0 {
				t.Fatal("no AVAudioFormat")
			}
			defer f.Send(sel.release)
			if got, want := formatDesc(f), fmt.Sprintf("%dHz/1ch", rate); got != want {
				t.Fatalf("format %s, want %s", got, want)
			}
			stereo := objc.ID(cls.format).Send(sel.alloc).Send(sel.initStandardFormat, float64(rate), uint32(2))
			if got, want := formatDesc(stereo), fmt.Sprintf("%dHz/2ch", rate); got != want {
				t.Fatalf("standard format %s, want %s", got, want)
			}
			stereo.Send(sel.release)
			buf := objc.ID(cls.pcmBuffer).Send(sel.alloc).Send(sel.initWithPCMFormat, f, uint32(frames))
			if buf == 0 {
				t.Fatal("no AVAudioPCMBuffer")
			}
			defer buf.Send(sel.release)
			buf.Send(sel.setFrameLength, uint32(frames))
			if n := objc.Send[uint32](buf, sel.frameLength); n != uint32(frames) {
				t.Fatalf("frameLength %d, want %d", n, frames)
			}
			if s := objc.Send[uint](buf, sel.stride); s != 1 {
				t.Fatalf("stride %d, want 1 (non-interleaved mono)", s)
			}
			data := objc.Send[**float32](buf, sel.floatChannelData)
			samples := unsafe.Slice(*data, frames)
			for i := range samples { // a 440 Hz tone at half scale: -9 dBFS RMS
				samples[i] = float32(0.5 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
			}

			cfg := VoiceConfig{}.withDefaults()
			e := &vpEngine{cfg: cfg, mic: newPaced(cfg.MicRate, time.Second), quit: make(chan struct{})}
			var err error
			if e.conv, err = newMicConverter(cfg.MicRate); err != nil {
				t.Fatal(err)
			}
			defer e.conv.Destroy()
			if err := e.setMicRate(rate); err != nil {
				t.Fatal(err)
			}
			e.tapData(0, buf, 0)
			held := e.mic.held()
			if held < 4800*9/10 || held > 4800 {
				t.Fatalf("%d Hz: %d samples at 48 kHz from 100 ms, want about 4800", rate, held)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			frame := make([]float32, 480)
			var got []float32
			start := time.Now()
			for len(got)+480 <= held {
				if err := e.read(ctx, frame); err != nil {
					t.Fatal(err)
				}
				got = append(got, frame...)
			}
			// Paced: the frames of one 100 ms chunk leave over about 90 ms.
			if d := time.Since(start); d < 70*time.Millisecond {
				t.Fatalf("%d Hz: read %d frames in %v, want them paced over ~90 ms", rate, len(got)/480, d)
			}
			if l := pcm.LevelF32(got[len(got)/4 : len(got)*3/4]); math.Abs(l+9.03) > 0.5 {
				t.Fatalf("%d Hz: converted level %.2f dBFS, want -9.03", rate, l)
			}
		})
	}
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
	if why := e.stalled(t0.Add(2500 * time.Millisecond)); why != "nothing it plays is rendered" {
		t.Fatalf("a buffer waiting 2 s: %q", why)
	}
	// A restart resets the clocks: what waited before it doesn't count.
	e.startedAt.Store(at(2400 * time.Millisecond))
	if why := e.stalled(t0.Add(2500 * time.Millisecond)); why != "" {
		t.Fatalf("100 ms after a restart: %q, want nothing", why)
	}
}

// TestSpeakerOnly opens the speakers alone (no input node, so no mic and
// no voice processing), plays 300 ms of silence through the player and
// checks the completions pace Write with no more than the queue held. It
// needs a device, so it runs only with HUDDLE_AUDIO_TEST=1.
func TestSpeakerOnly(t *testing.T) {
	if os.Getenv("HUDDLE_AUDIO_TEST") != "1" {
		t.Skip("set HUDDLE_AUDIO_TEST=1 to run (opens the speakers and plays silence)")
	}
	platform.LoadSDL()
	v, err := OpenVoice(VoiceConfig{SpeakerOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := v.Mic().Read(context.Background(), make([]float32, 480)); err == nil {
		t.Fatal("a speakers-only path read a mic")
	}
	frame := make([]float32, 480)
	start, most := time.Now(), time.Duration(0)
	for range 30 {
		if err := v.Speaker().Write(context.Background(), frame); err != nil {
			t.Fatal(err)
		}
		most = max(most, v.Info().Queued)
	}
	took := time.Since(start)
	time.Sleep(100 * time.Millisecond)
	info := v.Info()
	t.Logf("300 ms written in %v, most queued %v, output latency %v; %+v", took, most, info.Latency, info)
	if most > 50*time.Millisecond || took < 200*time.Millisecond || !info.FirstRender || info.Dropped != 0 || info.Queued != 0 {
		t.Fatalf("not paced by the player: took %v, most queued %v, %+v", took, most, info)
	}
}
