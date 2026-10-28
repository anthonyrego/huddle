package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/Zyko0/go-sdl3/sdl"
	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"github.com/anthonyrego/huddle/internal/pcm"
)

// Voice processing is macOS's echo canceller (the one FaceTime uses), on an
// AVAudioEngine whose input node has it turned on. It can only cancel what
// the engine itself plays, so the mic and the speakers both live on the
// engine: a tap on the input node gives the cleaned mic, and an
// AVAudioPlayerNode plays the model. Go never runs on the real-time audio
// thread: the tap and the player's completion handlers are called on the
// engine's own threads.

// OpenVoiceProcessing starts an engine with voice processing on the default
// devices and returns its mic and speaker. Close both. It fails where voice
// processing can't run: no AVFAudio, or devices it won't pair (some
// Bluetooth headphones with the built-in mic).
func OpenVoiceProcessing() (Mic, Speaker, error) {
	if err := loadAVF(); err != nil {
		return nil, nil, err
	}
	conv, err := newMicConverter()
	if err != nil {
		return nil, nil, err
	}
	e := &vpEngine{conv: conv, quit: make(chan struct{})}
	if e.restoreSpeakers, err = matchSpeakersToMic(); err != nil {
		slog.Warn("audio: couldn't match the speakers' rate to the mic's", "err", err)
	}
	e.tap = objc.NewBlock(e.tapData)
	e.done = e.newDone(e.gen.Load())
	var startErr error
	withPool(func() { startErr = e.start() })
	if startErr != nil {
		e.close()
		return nil, nil, startErr
	}
	go e.watch()
	return &vpMic{e}, &vpSpeaker{e}, nil
}

// vpChunk is the most audio one scheduled buffer holds, so Played moves in
// steps this small.
const vpChunk = 20 * time.Millisecond

// completionPlayedBack is AVAudioPlayerNodeCompletionDataPlayedBack: the
// handler runs once the buffer has been heard, output latency included.
const completionPlayedBack = 2

// duckingMin is AVAudioVoiceProcessingOtherAudioDuckingLevelMin.
const duckingMin = 10

// duckingConfig is AVAudioVoiceProcessingOtherAudioDuckingConfiguration.
type duckingConfig struct {
	EnableAdvancedDucking bool
	DuckingLevel          int64
}

type vpEngine struct {
	mu     sync.Mutex // the engine's objects and calls, closed
	engine objc.ID
	input  objc.ID
	output objc.ID
	player objc.ID
	fmt24  objc.ID // what the player is fed: float32 mono at OutRate
	tap    objc.Block
	tapped bool       // the tap is installed
	done   objc.Block // the completion handler for buffers of the current generation
	closed bool
	quit   chan struct{}

	restoreSpeakers func() // puts the speakers back at their own rate

	tapMu     sync.Mutex // the converter, between the tap and close
	conv      *sdl.AudioStream
	scratch   []float32 // the tap's: channel 0 of an interleaved buffer
	tapLogged atomic.Bool

	// For the watchdog, in Unix nanoseconds: when the engine last started,
	// when the tap last delivered, and since when the oldest pending buffer
	// has been waiting to be heard (0: none pending).
	startedAt   atomic.Int64
	lastTap     atomic.Int64
	waiting     atomic.Int64
	heardLogged atomic.Bool

	// Playback positions in bytes at OutRate. A generation ends at a clear
	// or a restart: completions of buffers from an older one are ignored.
	// posMu is never held across a call into AVFoundation: the player may
	// call its completion handlers while holding locks of its own.
	gen     atomic.Int64
	posMu   sync.Mutex
	written atomic.Int64
	heard   atomic.Int64
	pending []int64 // sizes of scheduled buffers not yet heard, oldest first (posMu)
	latency atomic.Int64
	micRate atomic.Int64
}

// start builds the engine and starts it. Call inside withPool.
func (e *vpEngine) start() error {
	e.engine = objc.ID(cls.engine).Send(sel.alloc).Send(sel.init)
	if e.engine == 0 {
		return errors.New("couldn't create an AVAudioEngine")
	}
	e.input = e.engine.Send(sel.inputNode)
	var nerr objc.ID
	if !objc.Send[bool](e.input, sel.setVoiceProcessingEnabledError, true, &nerr) {
		return fmt.Errorf("turn on voice processing: %s", nsError(nerr))
	}
	if !objc.Send[bool](e.input, sel.isVoiceProcessingEnabled) {
		return errors.New("voice processing didn't turn on")
	}
	// No automatic gain: it lifts the room's noise to within a few dB of
	// the person's voice (-35 dBFS against -55 without it), which hides
	// where speech ends from Live and from the turn timing.
	if objc.Send[bool](e.input, sel.respondsToSelector, sel.setVoiceProcessingAGCEnabled) {
		e.input.Send(sel.setVoiceProcessingAGCEnabled, false)
	}
	// Other apps' audio is turned down only as much as needed (macOS 14+).
	if objc.Send[bool](e.input, sel.respondsToSelector, sel.setOtherAudioDucking) {
		e.input.Send(sel.setOtherAudioDucking, duckingConfig{EnableAdvancedDucking: true, DuckingLevel: duckingMin})
	}
	e.player = objc.ID(cls.playerNode).Send(sel.alloc).Send(sel.init)
	e.engine.Send(sel.attachNode, e.player)
	e.fmt24 = objc.ID(cls.format).Send(sel.alloc).Send(sel.initWithCommonFormat, uint(pcmFormatFloat32), float64(OutRate), uint32(1), false)
	e.output = e.engine.Send(sel.outputNode)
	if err := e.wire(); err != nil {
		return err
	}
	return e.run()
}

// wire connects the player to the mixer and the mixer to the output, and
// taps the input, all in the formats the devices have now. Call with the
// engine stopped, inside withPool.
func (e *vpEngine) wire() error {
	mixer := e.engine.Send(sel.mainMixerNode)
	e.engine.Send(sel.connectToFormat, e.player, mixer, e.fmt24)
	// Voice processing needs the audio it plays and the audio it records
	// at the same rate on our side ("client-side input and output formats
	// do not match", -10875). The engine gives each side its device's
	// format, and the built-in mic and speakers can run at different rates
	// (48 and 44.1 kHz), so the mixer feeds the output at the mic's rate,
	// with the speakers' channels (the mic side can have 3: the mic array).
	micFmt := e.input.Send(sel.outputFormatForBus, uint(0))
	if micFmt == 0 {
		return errors.New("the input node has no format")
	}
	spkDevice := e.output.Send(sel.outputFormatForBus, uint(0))
	channels := objc.Send[uint32](spkDevice, sel.channelCount)
	if channels == 0 {
		channels = 2
	}
	spkFmt := objc.ID(cls.format).Send(sel.alloc).Send(sel.initStandardFormat, objc.Send[float64](micFmt, sel.sampleRate), channels)
	if spkFmt == 0 {
		return errors.New("couldn't make the speakers' format")
	}
	e.engine.Send(sel.connectToFormat, mixer, e.output, spkFmt)
	spkFmt.Send(sel.release)
	slog.Info("audio: voice processing formats",
		"micDevice", formatDesc(e.input.Send(sel.inputFormatForBus, uint(0))), "mic", formatDesc(micFmt),
		"speakers", formatDesc(e.output.Send(sel.inputFormatForBus, uint(0))), "speakerDevice", formatDesc(spkDevice))
	return e.installTap()
}

// formatDesc is an AVAudioFormat as rate/channels, for the log.
func formatDesc(f objc.ID) string {
	if f == 0 {
		return "none"
	}
	return fmt.Sprintf("%.0fHz/%dch", objc.Send[float64](f, sel.sampleRate), objc.Send[uint32](f, sel.channelCount))
}

// pcmFormatFloat32 is AVAudioPCMFormatFloat32.
const pcmFormatFloat32 = 1

// installTap taps the input node in its own format and points the mic
// converter at it.
func (e *vpEngine) installTap() error {
	f := e.input.Send(sel.outputFormatForBus, uint(0))
	if f == 0 {
		return errors.New("the input node has no format")
	}
	rate := objc.Send[float64](f, sel.sampleRate)
	if rate <= 0 {
		return fmt.Errorf("the input node runs at %.0f Hz", rate)
	}
	if err := e.setMicRate(int(rate)); err != nil {
		return err
	}
	// Asks for 20 ms; AVAudioEngine may deliver more at a time.
	e.input.Send(sel.installTap, uint(0), uint32(rate/50), f, e.tap)
	e.tapped = true
	return nil
}

// removeTap takes the tap off the input node, if it is on. Installing a
// second tap raises an Objective-C exception.
func (e *vpEngine) removeTap() {
	if e.tapped {
		e.input.Send(sel.removeTapOnBus, uint(0))
		e.tapped = false
	}
}

// run prepares and starts the engine and the player.
func (e *vpEngine) run() error {
	e.engine.Send(sel.prepare)
	var nerr objc.ID
	if !objc.Send[bool](e.engine, sel.startAndReturnError, &nerr) {
		return fmt.Errorf("start the audio engine: %s", nsError(nerr))
	}
	e.player.Send(sel.play)
	e.startedAt.Store(time.Now().UnixNano())
	e.tapLogged.Store(false)
	e.heardLogged.Store(false)
	e.latency.Store(int64(objc.Send[float64](e.output, sel.presentationLatency) * float64(time.Second)))
	slog.Info("audio: voice processing on", "micRate", e.micRate.Load(), "outputLatency", time.Duration(e.latency.Load()),
		"agc", objc.Send[bool](e.input, sel.isVoiceProcessingAGCEnabled))
	return nil
}

// newMicConverter is the SDL stream that turns the tap's float32 into s16 at
// InRate (no device: SDL streams convert on their own).
func newMicConverter() (*sdl.AudioStream, error) {
	conv, err := sdl.CreateAudioStream(
		&sdl.AudioSpec{Format: sdl.AUDIO_F32LE, Channels: 1, Freq: 48000},
		&sdl.AudioSpec{Format: sdl.AUDIO_S16LE, Channels: 1, Freq: InRate})
	if err != nil {
		return nil, fmt.Errorf("mic converter: %w", err)
	}
	return conv, nil
}

// setMicRate tells the converter the rate the tap delivers.
func (e *vpEngine) setMicRate(rate int) error {
	e.tapMu.Lock()
	err := e.conv.SetFormat(&sdl.AudioSpec{Format: sdl.AUDIO_F32LE, Channels: 1, Freq: int32(rate)}, nil)
	e.tapMu.Unlock()
	if err != nil {
		return fmt.Errorf("mic converter: %w", err)
	}
	e.micRate.Store(int64(rate))
	return nil
}

// tapData is the input tap: channel 0 of each buffer goes to the
// converter.
func (e *vpEngine) tapData(_ objc.Block, buf, _ objc.ID) {
	n := int(objc.Send[uint32](buf, sel.frameLength))
	data := objc.Send[**float32](buf, sel.floatChannelData)
	if n == 0 || data == nil || *data == nil {
		return
	}
	stride := max(int(objc.Send[uint](buf, sel.stride)), 1)
	e.lastTap.Store(time.Now().UnixNano())
	if !e.tapLogged.Swap(true) {
		// Once per start: the mic is flowing, and how much arrives at a
		// time (the tap's added latency).
		rate := max(e.micRate.Load(), 1)
		slog.Info("audio: voice processing mic", "frames", n, "every", time.Duration(int64(n)*int64(time.Second)/rate), "stride", stride)
	}
	samples := unsafe.Slice(*data, n*stride)
	if stride > 1 {
		e.scratch = e.scratch[:0]
		for i := 0; i < n; i++ {
			e.scratch = append(e.scratch, samples[i*stride])
		}
		samples = e.scratch
	}
	e.tapMu.Lock()
	defer e.tapMu.Unlock()
	if e.conv != nil {
		e.conv.PutData(unsafe.Slice((*byte)(unsafe.Pointer(&samples[0])), len(samples)*4))
	}
}

// newDone makes the completion handler for buffers scheduled in generation
// gen: each call means the oldest pending buffer has been heard.
func (e *vpEngine) newDone(gen int64) objc.Block {
	return objc.NewBlock(func(_ objc.Block, _ int) {
		if e.gen.Load() != gen {
			return // cleared: AVAudioPlayerNode.stop completes what it drops
		}
		e.posMu.Lock()
		defer e.posMu.Unlock()
		if e.gen.Load() == gen && len(e.pending) > 0 {
			e.heard.Add(e.pending[0])
			e.pending = e.pending[1:]
			if len(e.pending) == 0 {
				e.waiting.Store(0)
			} else {
				e.waiting.Store(time.Now().UnixNano())
			}
			if !e.heardLogged.Swap(true) {
				slog.Info("audio: voice processing played the first buffer since it started")
			}
		}
	})
}

// newGeneration forgets every scheduled buffer: what wasn't heard no longer
// counts as written. Call with mu held, before stopping the player.
func (e *vpEngine) newGeneration() int64 {
	gen := e.gen.Add(1)
	e.posMu.Lock()
	heard := e.heard.Load()
	dropped := e.written.Load() - heard
	e.written.Store(heard)
	e.pending = e.pending[:0]
	e.waiting.Store(0)
	e.posMu.Unlock()
	old := e.done
	e.done = e.newDone(gen)
	old.Release()
	return max(dropped, 0)
}

// watch keeps the engine going. AVAudioEngine stops itself when the
// devices change, and voice processing changes them once, about a second
// after it starts. A stopped engine is restarted in place: the player
// stopped (playing on from where it was leaves it silent), everything
// connected and tapped again in the formats the devices have now. Tearing
// it down instead would make voice processing change the devices again
// and stop the next engine. If the engine runs but no audio flows (no mic
// for 2 s, or a buffer not heard within 2 s), it is built anew.
func (e *vpEngine) watch() {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	failing, restarts, rebuilds := false, 0, 0
	for {
		select {
		case <-e.quit:
			return
		case <-t.C:
		}
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return
		}
		var err error
		switch {
		case !objc.Send[bool](e.engine, sel.isRunning):
			restarts++
			withPool(func() { err = e.restart() })
			if err != nil {
				slog.Warn("audio: voice processing didn't restart; building a new engine", "err", err)
				rebuilds++
				withPool(func() { err = e.rebuild() })
			}
			if err != nil && !failing {
				slog.Error("audio: voice processing stopped and won't start again", "err", err)
			} else if err == nil {
				slog.Info("audio: voice processing restarted after a device change", "restarts", restarts)
			}
			failing = err != nil
		default:
			if why := e.stalled(time.Now()); why != "" && rebuilds < maxRebuilds {
				rebuilds++
				slog.Warn("audio: voice processing is running but "+why+"; building a new engine", "rebuilds", rebuilds)
				withPool(func() { err = e.rebuild() })
				if err != nil {
					slog.Error("audio: voice processing won't start again", "err", err)
				}
			}
		}
		e.mu.Unlock()
	}
}

// maxRebuilds is how many times the watchdog builds a new engine before it
// gives up and leaves the problem in the log.
const maxRebuilds = 5

// stallAfter is how long the mic may be silent, or a buffer wait to be
// heard, before the engine counts as stalled.
const stallAfter = 2 * time.Second

// stalled says why a running engine isn't moving audio, or "".
func (e *vpEngine) stalled(now time.Time) string {
	since := func(t int64) time.Duration { return now.Sub(time.Unix(0, max(t, e.startedAt.Load()))) }
	if since(e.lastTap.Load()) >= stallAfter {
		return "the mic has gone silent"
	}
	if w := e.waiting.Load(); w != 0 && since(w) >= stallAfter {
		return "nothing it plays is heard"
	}
	return ""
}

// restart starts the stopped engine again. Call inside withPool with mu
// held.
func (e *vpEngine) restart() error {
	e.newGeneration()
	e.player.Send(sel.stop)
	e.removeTap()
	if err := e.wire(); err != nil {
		return err
	}
	return e.run()
}

// rebuild replaces the engine with a new one. Call inside withPool with mu
// held.
func (e *vpEngine) rebuild() error {
	e.newGeneration()
	e.teardown()
	return e.start()
}

func (e *vpEngine) close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.gen.Add(1)
	close(e.quit)
	withPool(func() {
		e.teardown()
		e.tap.Release()
		e.done.Release()
	})
	if e.restoreSpeakers != nil {
		e.restoreSpeakers()
	}
	e.mu.Unlock()
	e.tapMu.Lock()
	e.conv.Destroy()
	e.conv = nil
	e.tapMu.Unlock()
}

// teardown stops the engine and lets go of it and its nodes. Call inside
// withPool with mu held.
func (e *vpEngine) teardown() {
	if e.input != 0 {
		e.removeTap()
	}
	if e.player != 0 {
		e.player.Send(sel.stop)
	}
	if e.engine != 0 {
		e.engine.Send(sel.stop)
	}
	for _, id := range []objc.ID{e.player, e.fmt24, e.engine} {
		if id != 0 {
			id.Send(sel.release)
		}
	}
	e.engine, e.input, e.output, e.player, e.fmt24 = 0, 0, 0, 0, 0
}

// vpMic is the engine's mic: the cleaned input, mono s16 at InRate.
type vpMic struct{ e *vpEngine }

func (m *vpMic) Available() int {
	m.e.tapMu.Lock()
	defer m.e.tapMu.Unlock()
	if m.e.conv == nil {
		return 0
	}
	n, err := m.e.conv.Available()
	if err != nil {
		return 0
	}
	return int(n)
}

func (m *vpMic) Read(buf []byte) (int, error) {
	m.e.tapMu.Lock()
	defer m.e.tapMu.Unlock()
	if m.e.conv == nil {
		return 0, errors.New("voice processing is closed")
	}
	n, err := m.e.conv.Data(buf)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (m *vpMic) Close() { m.e.close() }

// vpSpeaker is the engine's player. Positions come from the player's
// completion handlers, which run once a buffer has been heard, so Played
// needs no latency estimate and moves in vpChunk steps.
type vpSpeaker struct{ e *vpEngine }

func (s *vpSpeaker) Put(b []byte) error {
	e := s.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("voice processing is closed")
	}
	step := int(pcm.Bytes(vpChunk, OutRate))
	var err error
	withPool(func() {
		for len(b) >= 2 && err == nil {
			n := min(len(b), step) &^ 1
			err = e.schedule(b[:n])
			b = b[n:]
		}
	})
	return err
}

// schedule plays one buffer after the ones already scheduled. Call inside
// withPool with mu held.
func (e *vpEngine) schedule(b []byte) error {
	frames := len(b) / 2
	buf := objc.ID(cls.pcmBuffer).Send(sel.alloc).Send(sel.initWithPCMFormat, e.fmt24, uint32(frames))
	if buf == 0 {
		return errors.New("couldn't create an AVAudioPCMBuffer")
	}
	defer buf.Send(sel.release)
	buf.Send(sel.setFrameLength, uint32(frames))
	data := objc.Send[**float32](buf, sel.floatChannelData)
	if data == nil || *data == nil {
		return errors.New("AVAudioPCMBuffer has no float data")
	}
	out := unsafe.Slice(*data, frames)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(b[i*2:]))) / 32768
	}
	e.posMu.Lock()
	if len(e.pending) == 0 {
		e.waiting.Store(time.Now().UnixNano())
	}
	e.pending = append(e.pending, int64(len(b)))
	e.written.Add(int64(len(b)))
	e.posMu.Unlock()
	e.player.Send(sel.scheduleBuffer, buf, int(completionPlayedBack), e.done)
	return nil
}

func (s *vpSpeaker) Written() int64 { return s.e.written.Load() }

// Queued is what was written and not yet heard, less what the device holds.
func (s *vpSpeaker) Queued() int64 {
	return max(s.e.written.Load()-s.e.heard.Load()-pcm.Bytes(s.Latency(), OutRate), 0)
}

func (s *vpSpeaker) Played() int64 { return s.e.heard.Load() }

func (s *vpSpeaker) Latency() time.Duration { return time.Duration(s.e.latency.Load()) }

// Flush does nothing: the player holds nothing back.
func (s *vpSpeaker) Flush() error { return nil }

// Clear stops the player, dropping everything scheduled, and starts it
// again. It returns how many bytes were dropped.
func (s *vpSpeaker) Clear() int64 {
	e := s.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return 0
	}
	dropped := e.newGeneration()
	withPool(func() {
		e.player.Send(sel.stop)
		e.player.Send(sel.play)
	})
	return dropped
}

func (s *vpSpeaker) Close() { s.e.close() }

// withPool runs fn on one locked OS thread inside an autorelease pool, so
// the objects AppKit autoreleases on a Go thread are freed.
func withPool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p := avf.poolPush()
	defer avf.poolPop(p)
	fn()
}

// nsError describes an NSError.
func nsError(err objc.ID) string {
	if err == 0 {
		return "no error given"
	}
	desc := cString(objc.Send[*byte](err.Send(sel.localizedDescription), sel.utf8String))
	return fmt.Sprintf("%s (%d)", desc, objc.Send[int](err, sel.code))
}

// cString copies a NUL-terminated C string.
func cString(p *byte) string {
	if p == nil {
		return ""
	}
	var b []byte
	for ; *p != 0; p = (*byte)(unsafe.Add(unsafe.Pointer(p), 1)) {
		b = append(b, *p)
	}
	return string(b)
}

var avf struct {
	once     sync.Once
	err      error
	poolPush func() uintptr
	poolPop  func(uintptr)
}

var cls struct {
	engine, playerNode, format, pcmBuffer objc.Class
}

var sel struct {
	alloc, init, release, respondsToSelector                                                                  objc.SEL
	inputNode, outputNode, mainMixerNode, attachNode                                                          objc.SEL
	connectToFormat, prepare, startAndReturnError, stop                                                       objc.SEL
	isRunning, setVoiceProcessingEnabledError                                                                 objc.SEL
	isVoiceProcessingEnabled, setOtherAudioDucking, setVoiceProcessingAGCEnabled, isVoiceProcessingAGCEnabled objc.SEL
	outputFormatForBus, inputFormatForBus, channelCount                                                       objc.SEL
	installTap, removeTapOnBus, initStandardFormat                                                            objc.SEL
	presentationLatency, initWithCommonFormat, sampleRate                                                     objc.SEL
	initWithPCMFormat, setFrameLength, frameLength                                                            objc.SEL
	floatChannelData, stride, play, scheduleBuffer                                                            objc.SEL
	localizedDescription, utf8String, code                                                                    objc.SEL
}

// loadAVF loads AVFAudio and looks up the classes and selectors once.
func loadAVF() error {
	avf.once.Do(func() { avf.err = loadAVFOnce() })
	return avf.err
}

func loadAVFOnce() error {
	var err error
	for _, path := range []string{
		"/System/Library/Frameworks/AVFAudio.framework/AVFAudio",
		"/System/Library/Frameworks/AVFoundation.framework/AVFoundation",
	} {
		if _, err = purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("load AVFAudio: %w", err)
	}
	lib, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("load libobjc: %w", err)
	}
	purego.RegisterLibFunc(&avf.poolPush, lib, "objc_autoreleasePoolPush")
	purego.RegisterLibFunc(&avf.poolPop, lib, "objc_autoreleasePoolPop")

	for _, c := range []struct {
		dst  *objc.Class
		name string
	}{
		{&cls.engine, "AVAudioEngine"},
		{&cls.playerNode, "AVAudioPlayerNode"},
		{&cls.format, "AVAudioFormat"},
		{&cls.pcmBuffer, "AVAudioPCMBuffer"},
	} {
		if *c.dst = objc.GetClass(c.name); *c.dst == 0 {
			return fmt.Errorf("AVFAudio has no %s", c.name)
		}
	}
	for _, s := range []struct {
		dst  *objc.SEL
		name string
	}{
		{&sel.alloc, "alloc"},
		{&sel.init, "init"},
		{&sel.release, "release"},
		{&sel.respondsToSelector, "respondsToSelector:"},
		{&sel.inputNode, "inputNode"},
		{&sel.outputNode, "outputNode"},
		{&sel.mainMixerNode, "mainMixerNode"},
		{&sel.attachNode, "attachNode:"},
		{&sel.connectToFormat, "connect:to:format:"},
		{&sel.prepare, "prepare"},
		{&sel.startAndReturnError, "startAndReturnError:"},
		{&sel.stop, "stop"},
		{&sel.isRunning, "isRunning"},
		{&sel.setVoiceProcessingEnabledError, "setVoiceProcessingEnabled:error:"},
		{&sel.isVoiceProcessingEnabled, "isVoiceProcessingEnabled"},
		{&sel.setVoiceProcessingAGCEnabled, "setVoiceProcessingAGCEnabled:"},
		{&sel.isVoiceProcessingAGCEnabled, "isVoiceProcessingAGCEnabled"},
		{&sel.setOtherAudioDucking, "setVoiceProcessingOtherAudioDuckingConfiguration:"},
		{&sel.outputFormatForBus, "outputFormatForBus:"},
		{&sel.inputFormatForBus, "inputFormatForBus:"},
		{&sel.channelCount, "channelCount"},
		{&sel.installTap, "installTapOnBus:bufferSize:format:block:"},
		{&sel.removeTapOnBus, "removeTapOnBus:"},
		{&sel.presentationLatency, "presentationLatency"},
		{&sel.initWithCommonFormat, "initWithCommonFormat:sampleRate:channels:interleaved:"},
		{&sel.initStandardFormat, "initStandardFormatWithSampleRate:channels:"},
		{&sel.sampleRate, "sampleRate"},
		{&sel.initWithPCMFormat, "initWithPCMFormat:frameCapacity:"},
		{&sel.setFrameLength, "setFrameLength:"},
		{&sel.frameLength, "frameLength"},
		{&sel.floatChannelData, "floatChannelData"},
		{&sel.stride, "stride"},
		{&sel.play, "play"},
		{&sel.scheduleBuffer, "scheduleBuffer:completionCallbackType:completionHandler:"},
		{&sel.localizedDescription, "localizedDescription"},
		{&sel.utf8String, "UTF8String"},
		{&sel.code, "code"},
	} {
		*s.dst = objc.RegisterName(s.name)
	}
	return nil
}
