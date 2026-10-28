package audio

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// CoreAudio's device properties, for the one thing AVAudioEngine can't do:
// put the speakers at the mic's rate before voice processing starts. Voice
// processing runs both at one rate; when they differ it switches the
// speakers itself, and that switch is a device change that stops the
// engine (and switching back when the engine is torn down stops the next
// one).

// caAddress is AudioObjectPropertyAddress.
type caAddress struct {
	Selector, Scope, Element uint32
}

// caRange is AudioValueRange.
type caRange struct {
	Min, Max float64
}

const (
	caSystemObject        = 1
	caDefaultInputDevice  = 'd'<<24 | 'I'<<16 | 'n'<<8 | ' '
	caDefaultOutputDevice = 'd'<<24 | 'O'<<16 | 'u'<<8 | 't'
	caNominalSampleRate   = 'n'<<24 | 's'<<16 | 'r'<<8 | 't'
	caAvailableRates      = 'n'<<24 | 's'<<16 | 'r'<<8 | '#'
	caScopeGlobal         = 'g'<<24 | 'l'<<16 | 'o'<<8 | 'b'
)

var ca struct {
	once sync.Once
	err  error
	get  func(id uint32, addr *caAddress, qualSize uint32, qual unsafe.Pointer, size *uint32, out unsafe.Pointer) int32
	set  func(id uint32, addr *caAddress, qualSize uint32, qual unsafe.Pointer, size uint32, data unsafe.Pointer) int32
}

func loadCoreAudio() error {
	ca.once.Do(func() {
		lib, err := purego.Dlopen("/System/Library/Frameworks/CoreAudio.framework/CoreAudio", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			ca.err = fmt.Errorf("load CoreAudio: %w", err)
			return
		}
		purego.RegisterLibFunc(&ca.get, lib, "AudioObjectGetPropertyData")
		purego.RegisterLibFunc(&ca.set, lib, "AudioObjectSetPropertyData")
	})
	return ca.err
}

// defaultDevice is the default input or output device's ID.
func defaultDevice(input bool) (uint32, error) {
	addr := caAddress{caDefaultOutputDevice, caScopeGlobal, 0}
	if input {
		addr.Selector = caDefaultInputDevice
	}
	var id uint32
	size := uint32(unsafe.Sizeof(id))
	if st := ca.get(caSystemObject, &addr, 0, nil, &size, unsafe.Pointer(&id)); st != 0 || id == 0 {
		return 0, fmt.Errorf("no default device (OSStatus %d)", st)
	}
	return id, nil
}

// nominalRate is a device's sample rate.
func nominalRate(dev uint32) (float64, error) {
	addr := caAddress{caNominalSampleRate, caScopeGlobal, 0}
	var rate float64
	size := uint32(unsafe.Sizeof(rate))
	if st := ca.get(dev, &addr, 0, nil, &size, unsafe.Pointer(&rate)); st != 0 {
		return 0, fmt.Errorf("device %d rate (OSStatus %d)", dev, st)
	}
	return rate, nil
}

// availableRates is the rates a device can run at.
func availableRates(dev uint32) ([]caRange, error) {
	addr := caAddress{caAvailableRates, caScopeGlobal, 0}
	rs := make([]caRange, 32)
	size := uint32(len(rs)) * uint32(unsafe.Sizeof(rs[0]))
	if st := ca.get(dev, &addr, 0, nil, &size, unsafe.Pointer(&rs[0])); st != 0 {
		return nil, fmt.Errorf("device %d rates (OSStatus %d)", dev, st)
	}
	return rs[:size/uint32(unsafe.Sizeof(rs[0]))], nil
}

// setNominalRate asks a device to run at rate and waits up to a second for
// it to.
func setNominalRate(dev uint32, rate float64) error {
	addr := caAddress{caNominalSampleRate, caScopeGlobal, 0}
	if st := ca.set(dev, &addr, 0, nil, uint32(unsafe.Sizeof(rate)), unsafe.Pointer(&rate)); st != 0 {
		return fmt.Errorf("set device %d to %.0f Hz (OSStatus %d)", dev, rate, st)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if r, err := nominalRate(dev); err == nil && r == rate {
			return nil
		}
	}
	return fmt.Errorf("device %d didn't switch to %.0f Hz", dev, rate)
}

// matchSpeakersToMic puts the default output at the default input's rate
// when they differ and the output can run at it. restore puts it back; it
// is a no-op when nothing was changed.
func matchSpeakersToMic() (restore func(), err error) {
	restore = func() {}
	if err := loadCoreAudio(); err != nil {
		return restore, err
	}
	in, err := defaultDevice(true)
	if err != nil {
		return restore, err
	}
	out, err := defaultDevice(false)
	if err != nil || out == in {
		return restore, err
	}
	micRate, err := nominalRate(in)
	if err != nil {
		return restore, err
	}
	spkRate, err := nominalRate(out)
	if err != nil || spkRate == micRate {
		return restore, err
	}
	rates, err := availableRates(out)
	if err != nil {
		return restore, err
	}
	if !slices.ContainsFunc(rates, func(r caRange) bool { return r.Min <= micRate && micRate <= r.Max }) {
		slog.Info("audio: the speakers can't run at the mic's rate", "mic", micRate, "speakers", spkRate)
		return restore, nil
	}
	if err := setNominalRate(out, micRate); err != nil {
		return restore, err
	}
	slog.Info("audio: speakers set to the mic's rate for voice processing", "rate", micRate, "was", spkRate)
	return func() {
		if err := setNominalRate(out, spkRate); err != nil {
			slog.Warn("audio: couldn't put the speakers back", "rate", spkRate, "err", err)
		}
	}, nil
}
