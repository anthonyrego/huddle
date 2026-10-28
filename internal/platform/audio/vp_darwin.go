package audio

import (
	"context"
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
)

// Voice processing is macOS's echo canceller (the one FaceTime uses), on an
// AVAudioEngine whose input node has it turned on. It can only cancel what
// the engine itself plays, so the mic and the speakers both live on the
// engine: a tap on the input node gives the cleaned mic, and an
// AVAudioPlayerNode plays the mix. Go never runs on the real-time audio
// thread: the tap and the player's completion handlers are called on the
// engine's own threads, and Go only copies there.
//
// The mic: the tap delivers 100 ms at a time (the floor AVAudioEngine puts
// on taps), converted to float32 mono at MicRate and released evenly (see
// paced). The speakers: Write schedules each 10 ms frame as its own buffer
// on the player and blocks while Queue's worth is waiting to be rendered,
// so the player's completions pace the caller on the device's clock.
//
// SpeakerOnly opens just the output side: a player on an engine whose input
// node is never touched, so no mic is opened and nothing is asked of the
// user; there is no voice processing (it lives on the input).

// OpenVoice starts an engine on the default devices. With voice processing
// it fails where that can't run: no AVFAudio, or devices it won't pair
// (some Bluetooth headphones with the built-in mic).
func OpenVoice(cfg VoiceConfig) (*Voice, error) {
	cfg = cfg.withDefaults()
	if err := loadAVF(); err != nil {
		return nil, err
	}
	e := &vpEngine{cfg: cfg, quit: make(chan struct{}), spkSig: make(chan struct{}, 1)}
	if !cfg.SpeakerOnly {
		conv, err := newMicConverter(cfg.MicRate)
		if err != nil {
			return nil, err
		}
		e.conv = conv
		e.mic = newPaced(cfg.MicRate, 500*time.Millisecond)
		var err2 error
		if e.restoreSpeakers, err2 = matchSpeakersToMic(); err2 != nil {
			slog.Warn("audio: couldn't match the speakers' rate to the mic's", "err", err2)
		}
		e.tap = objc.NewBlock(e.tapData)
	}
	e.done = e.newDone(e.gen.Load())
	var startErr error
	withPool(func() { startErr = e.start() })
	if startErr != nil {
		e.close()
		return nil, startErr
	}
	go e.watch()
	return &Voice{e}, nil
}

// completionRendered is AVAudioPlayerNodeCompletionDataRendered: the
// handler runs once the player has rendered the buffer into the engine.
const completionRendered = 1

// duckingMin is AVAudioVoiceProcessingOtherAudioDuckingLevelMin.
const duckingMin = 10

// duckingConfig is AVAudioVoiceProcessingOtherAudioDuckingConfiguration.
type duckingConfig struct {
	EnableAdvancedDucking bool
	DuckingLevel          int64
}

type vpEngine struct {
	cfg    VoiceConfig
	mu     sync.Mutex // the engine's objects and calls, closed
	engine objc.ID
	input  objc.ID // 0 when SpeakerOnly
	output objc.ID
	player objc.ID
	fmtOut objc.ID // what the player is fed: float32 mono at SpeakerRate
	tap    objc.Block
	tapped bool       // the tap is installed
	done   objc.Block // the completion handler for buffers of the current generation
	closed bool
	quit   chan struct{}

	restoreSpeakers func() // puts the speakers back at their own rate

	tapMu     sync.Mutex // the converter, between the tap and close
	conv      *sdl.AudioStream
	scratch   []float32 // the tap's: channel 0 of an interleaved buffer
	convOut   []float32
	tapLogged atomic.Bool
	mic       *paced

	// For the watchdog, in Unix nanoseconds: when the engine last started,
	// when the tap last delivered, and since when the oldest scheduled
	// buffer has been waiting to be rendered (0: none).
	startedAt   atomic.Int64
	lastTap     atomic.Int64
	waiting     atomic.Int64
	heardLogged atomic.Bool

	// The player's queue, in samples at SpeakerRate. A generation ends at
	// a restart: completions of buffers from an older one are ignored.
	// posMu is never held across a call into AVFoundation: the player may
	// call its completion handlers while holding locks of its own.
	gen     atomic.Int64
	posMu   sync.Mutex
	queued  atomic.Int64
	pending []int64 // sizes of scheduled buffers not yet rendered, oldest first (posMu)
	spkSig  chan struct{}
	latency atomic.Int64
	micRate atomic.Int64
	written atomic.Int64 // samples scheduled since opening
	dropped atomic.Int64 // samples Write let go because the engine wasn't taking them
}

// start builds the engine and starts it. Call inside withPool.
func (e *vpEngine) start() error {
	e.engine = objc.ID(cls.engine).Send(sel.alloc).Send(sel.init)
	if e.engine == 0 {
		return errors.New("couldn't create an AVAudioEngine")
	}
	if !e.cfg.SpeakerOnly {
		e.input = e.engine.Send(sel.inputNode)
		var nerr objc.ID
		if !objc.Send[bool](e.input, sel.setVoiceProcessingEnabledError, true, &nerr) {
			return fmt.Errorf("turn on voice processing: %s", nsError(nerr))
		}
		if !objc.Send[bool](e.input, sel.isVoiceProcessingEnabled) {
			return errors.New("voice processing didn't turn on")
		}
		// No automatic gain: it lifts the room's noise to within a few dB
		// of the person's voice, which hides where speech ends (DTX, the
		// talking indicators).
		if objc.Send[bool](e.input, sel.respondsToSelector, sel.setVoiceProcessingAGCEnabled) {
			e.input.Send(sel.setVoiceProcessingAGCEnabled, false)
		}
		// Other apps' audio is turned down only as much as needed (macOS 14+).
		if objc.Send[bool](e.input, sel.respondsToSelector, sel.setOtherAudioDucking) {
			e.input.Send(sel.setOtherAudioDucking, duckingConfig{EnableAdvancedDucking: true, DuckingLevel: duckingMin})
		}
	}
	e.player = objc.ID(cls.playerNode).Send(sel.alloc).Send(sel.init)
	e.engine.Send(sel.attachNode, e.player)
	e.fmtOut = objc.ID(cls.format).Send(sel.alloc).Send(sel.initWithCommonFormat, uint(pcmFormatFloat32), float64(e.cfg.SpeakerRate), uint32(1), false)
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
	e.engine.Send(sel.connectToFormat, e.player, mixer, e.fmtOut)
	spkDevice := e.output.Send(sel.outputFormatForBus, uint(0))
	if e.cfg.SpeakerOnly {
		slog.Info("audio: speakers only", "player", formatDesc(e.fmtOut), "speakerDevice", formatDesc(spkDevice))
		return nil
	}
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
		"speakers", formatDesc(e.output.Send(sel.inputFormatForBus, uint(0))), "speakerDevice", formatDesc(spkDevice),
		"player", formatDesc(e.fmtOut))
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
	// Asks for 10 ms; AVAudioEngine delivers 100 ms at a time regardless.
	e.input.Send(sel.installTap, uint(0), uint32(rate/100), f, e.tap)
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
	if e.cfg.SpeakerOnly {
		slog.Info("audio: speakers on", "rate", e.cfg.SpeakerRate, "outputLatency", time.Duration(e.latency.Load()), "queue", e.cfg.Queue)
		return nil
	}
	slog.Info("audio: voice processing on", "deviceMicRate", e.micRate.Load(), "micRate", e.cfg.MicRate, "speakerRate", e.cfg.SpeakerRate,
		"outputLatency", time.Duration(e.latency.Load()), "queue", e.cfg.Queue,
		"agc", objc.Send[bool](e.input, sel.isVoiceProcessingAGCEnabled))
	return nil
}

// newMicConverter is the SDL stream that turns the tap's float32 at the
// device's rate into float32 at rate (no device: SDL streams convert on
// their own).
func newMicConverter(rate int) (*sdl.AudioStream, error) {
	conv, err := sdl.CreateAudioStream(
		&sdl.AudioSpec{Format: sdl.AUDIO_F32LE, Channels: 1, Freq: 48000},
		&sdl.AudioSpec{Format: sdl.AUDIO_F32LE, Channels: 1, Freq: int32(rate)})
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

// tapData is the input tap: channel 0 of each buffer, converted, goes to
// the paced queue Read takes from.
func (e *vpEngine) tapData(_ objc.Block, buf, _ objc.ID) {
	n := int(objc.Send[uint32](buf, sel.frameLength))
	data := objc.Send[**float32](buf, sel.floatChannelData)
	if n == 0 || data == nil || *data == nil {
		return
	}
	now := time.Now()
	stride := max(int(objc.Send[uint](buf, sel.stride)), 1)
	e.lastTap.Store(now.UnixNano())
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
	if e.conv == nil {
		return
	}
	e.conv.PutData(unsafe.Slice((*byte)(unsafe.Pointer(&samples[0])), len(samples)*4))
	avail, err := e.conv.Available()
	if err != nil || avail < 4 {
		return
	}
	if cap(e.convOut) < int(avail)/4 {
		e.convOut = make([]float32, int(avail)/4)
	}
	out := e.convOut[:int(avail)/4]
	got, err := e.conv.Data(unsafe.Slice((*byte)(unsafe.Pointer(&out[0])), len(out)*4))
	if err != nil || got < 4 {
		return
	}
	e.mic.push(out[:got/4], now)
}

// newDone makes the completion handler for buffers scheduled in generation
// gen: each call means the oldest pending buffer has been rendered.
func (e *vpEngine) newDone(gen int64) objc.Block {
	return objc.NewBlock(func(_ objc.Block, _ int) {
		if e.gen.Load() != gen {
			return // a restart: AVAudioPlayerNode.stop completes what it drops
		}
		e.posMu.Lock()
		if e.gen.Load() == gen && len(e.pending) > 0 {
			e.queued.Add(-e.pending[0])
			e.pending = e.pending[1:]
			if len(e.pending) == 0 {
				e.waiting.Store(0)
			} else {
				e.waiting.Store(time.Now().UnixNano())
			}
			if !e.heardLogged.Swap(true) {
				slog.Info("audio: the player rendered its first buffer since the engine started")
			}
		}
		e.posMu.Unlock()
		select {
		case e.spkSig <- struct{}{}:
		default:
		}
	})
}

// newGeneration forgets every scheduled buffer. Call with mu held, before
// stopping the player.
func (e *vpEngine) newGeneration() {
	gen := e.gen.Add(1)
	e.posMu.Lock()
	e.queued.Store(0)
	e.pending = e.pending[:0]
	e.waiting.Store(0)
	e.posMu.Unlock()
	old := e.done
	e.done = e.newDone(gen)
	old.Release()
	select {
	case e.spkSig <- struct{}{}:
	default:
	}
}

// watch keeps the engine going. AVAudioEngine stops itself when the
// devices change (so it follows the system's default devices), and voice
// processing changes them once, about a second after it starts. A stopped
// engine is restarted in place: the player stopped (playing on from where
// it was leaves it silent), everything connected and tapped again in the
// formats the devices have now. Tearing it down instead would make voice
// processing change the devices again and stop the next engine. If the
// engine runs but no audio flows (no mic for 2 s, or a buffer not rendered
// within 2 s), it is built anew.
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
				slog.Warn("audio: the engine didn't restart; building a new one", "err", err)
				rebuilds++
				withPool(func() { err = e.rebuild() })
			}
			if err != nil && !failing {
				slog.Error("audio: the engine stopped and won't start again", "err", err)
			} else if err == nil {
				slog.Info("audio: the engine restarted after a device change", "restarts", restarts)
			}
			failing = err != nil
		default:
			if why := e.stalled(time.Now()); why != "" && rebuilds < maxRebuilds {
				rebuilds++
				slog.Warn("audio: the engine is running but "+why+"; building a new one", "rebuilds", rebuilds)
				withPool(func() { err = e.rebuild() })
				if err != nil {
					slog.Error("audio: the engine won't start again", "err", err)
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
// rendered, before the engine counts as stalled.
const stallAfter = 2 * time.Second

// stalled says why a running engine isn't moving audio, or "".
func (e *vpEngine) stalled(now time.Time) string {
	since := func(t int64) time.Duration { return now.Sub(time.Unix(0, max(t, e.startedAt.Load()))) }
	if !e.cfg.SpeakerOnly && since(e.lastTap.Load()) >= stallAfter {
		return "the mic has gone silent"
	}
	if w := e.waiting.Load(); w != 0 && since(w) >= stallAfter {
		return "nothing it plays is rendered"
	}
	return ""
}

// restart starts the stopped engine again. Call inside withPool with mu
// held.
func (e *vpEngine) restart() error {
	e.newGeneration()
	e.player.Send(sel.stop)
	if e.input != 0 {
		e.removeTap()
	}
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
		if !e.cfg.SpeakerOnly {
			e.tap.Release()
		}
		e.done.Release()
	})
	if e.restoreSpeakers != nil {
		e.restoreSpeakers()
	}
	e.mu.Unlock()
	e.tapMu.Lock()
	if e.conv != nil {
		e.conv.Destroy()
		e.conv = nil
	}
	e.tapMu.Unlock()
	select {
	case e.spkSig <- struct{}{}:
	default:
	}
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
	for _, id := range []objc.ID{e.player, e.fmtOut, e.engine} {
		if id != 0 {
			id.Send(sel.release)
		}
	}
	e.engine, e.input, e.output, e.player, e.fmtOut = 0, 0, 0, 0, 0
}

// read waits for the next paced mic frame.
func (e *vpEngine) read(ctx context.Context, frame []float32) error {
	if e.cfg.SpeakerOnly {
		return errors.New("audio: speakers only, no mic")
	}
	for {
		ok, wait := e.mic.take(frame, time.Now())
		if ok {
			return nil
		}
		if wait == 0 {
			wait = time.Second // until a push signals
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-e.quit:
			t.Stop()
			return errors.New("audio: closed")
		case <-e.mic.sig:
		case <-t.C:
		}
		t.Stop()
	}
}

// writeWait is how long Write waits for the player before scheduling
// anyway, and writeDrop how much may queue before it starts dropping (the
// engine stopped or stalled: the watchdog is on it).
const (
	writeWait = 200 * time.Millisecond
	writeDrop = 500 * time.Millisecond
)

// write schedules one frame on the player once the queue has room.
func (e *vpEngine) write(ctx context.Context, frame []float32) error {
	limit := int64(e.cfg.SpeakerRate) * int64(e.cfg.Queue) / int64(time.Second)
	deadline := time.Now().Add(writeWait)
	for e.queued.Load()+int64(len(frame)) > limit {
		left := time.Until(deadline)
		if left <= 0 {
			if e.queued.Load() > int64(e.cfg.SpeakerRate)*int64(writeDrop)/int64(time.Second) {
				e.dropped.Add(int64(len(frame)))
				return nil
			}
			break
		}
		t := time.NewTimer(left)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-e.quit:
			t.Stop()
			return errors.New("audio: closed")
		case <-e.spkSig:
		case <-t.C:
		}
		t.Stop()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return errors.New("audio: closed")
	}
	var err error
	withPool(func() { err = e.schedule(frame) })
	return err
}

// schedule plays one buffer after the ones already scheduled. Call inside
// withPool with mu held.
func (e *vpEngine) schedule(frame []float32) error {
	n := len(frame)
	buf := objc.ID(cls.pcmBuffer).Send(sel.alloc).Send(sel.initWithPCMFormat, e.fmtOut, uint32(n))
	if buf == 0 {
		return errors.New("couldn't create an AVAudioPCMBuffer")
	}
	defer buf.Send(sel.release)
	buf.Send(sel.setFrameLength, uint32(n))
	data := objc.Send[**float32](buf, sel.floatChannelData)
	if data == nil || *data == nil {
		return errors.New("AVAudioPCMBuffer has no float data")
	}
	copy(unsafe.Slice(*data, n), frame)
	e.posMu.Lock()
	if len(e.pending) == 0 {
		e.waiting.Store(time.Now().UnixNano())
	}
	e.pending = append(e.pending, int64(n))
	e.queued.Add(int64(n))
	e.posMu.Unlock()
	e.written.Add(int64(n))
	e.player.Send(sel.scheduleBuffer, buf, int(completionRendered), e.done)
	return nil
}

func (e *vpEngine) info() VoiceInfo {
	return VoiceInfo{
		SpeakerOnly:  e.cfg.SpeakerOnly,
		DeviceMic:    int(e.micRate.Load()),
		Latency:      time.Duration(e.latency.Load()),
		Queued:       time.Duration(e.queued.Load()) * time.Second / time.Duration(e.cfg.SpeakerRate),
		Written:      time.Duration(e.written.Load()) * time.Second / time.Duration(e.cfg.SpeakerRate),
		Dropped:      time.Duration(e.dropped.Load()) * time.Second / time.Duration(e.cfg.SpeakerRate),
		MicHeld:      e.micHeld(),
		FirstRender:  e.heardLogged.Load(),
		EngineAlive:  e.alive(),
		VoiceProcess: !e.cfg.SpeakerOnly,
	}
}

func (e *vpEngine) micHeld() time.Duration {
	if e.mic == nil {
		return 0
	}
	return time.Duration(e.mic.held()) * time.Second / time.Duration(e.cfg.MicRate)
}

func (e *vpEngine) alive() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.closed && e.engine != 0 && objc.Send[bool](e.engine, sel.isRunning)
}

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
