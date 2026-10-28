package audio

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The devices the voice path can take, read from CoreAudio's properties:
// nothing is opened, so listing never asks for the mic. VoiceConfig names
// a device by its UID, which stays the same across launches and plugging
// it in again (it is also AVCaptureDevice's uniqueID).

const (
	caDevices         = 'd'<<24 | 'e'<<16 | 'v'<<8 | '#' // kAudioHardwarePropertyDevices
	caDeviceUID       = 'u'<<24 | 'i'<<16 | 'd'<<8 | ' ' // kAudioDevicePropertyDeviceUID
	caTransportType   = 't'<<24 | 'r'<<16 | 'a'<<8 | 'n' // kAudioDevicePropertyTransportType
	caStreamConfig    = 's'<<24 | 'l'<<16 | 'a'<<8 | 'y' // kAudioDevicePropertyStreamConfiguration
	caIsHidden        = 'h'<<24 | 'i'<<16 | 'd'<<8 | 'n' // kAudioDevicePropertyIsHidden
	caScopeInput      = 'i'<<24 | 'n'<<16 | 'p'<<8 | 't'
	caScopeOutput     = 'o'<<24 | 'u'<<16 | 't'<<8 | 'p'
	auCurrentDevice   = 2000 // kAudioOutputUnitProperty_CurrentDevice
	auScopeGlobal     = 0
	auElementOutput   = 0 // an IO unit's element 0 is the speakers' side
	auElementInput    = 1 // and element 1 the mic's
	transportBuiltIn  = 'b'<<24 | 'l'<<16 | 't'<<8 | 'n'
	transportUSB      = 'u'<<24 | 's'<<16 | 'b'<<8 | ' '
	transportBT       = 'b'<<24 | 'l'<<16 | 'u'<<8 | 'e'
	transportBLE      = 'b'<<24 | 'l'<<16 | 'e'<<8 | 'o'
	transportVirtual  = 'v'<<24 | 'i'<<16 | 'r'<<8 | 't'
	transportAgg      = 'g'<<24 | 'r'<<16 | 'u'<<8 | 'p'
	transportCont     = 'c'<<24 | 'c'<<16 | 'w'<<8 | 'd' // an iPhone as the Mac's mic (Continuity)
	transportHDMI     = 'h'<<24 | 'd'<<16 | 'm'<<8 | 'i'
	transportDP       = 'd'<<24 | 'p'<<16 | 'r'<<8 | 't'
	transportAirPlay  = 'a'<<24 | 'i'<<16 | 'r'<<8 | 'p'
	transportFireWire = '1'<<24 | '3'<<16 | '9'<<8 | '4'
	transportTB       = 't'<<24 | 'h'<<16 | 'u'<<8 | 'n'
)

var au struct {
	setProp func(unit uintptr, id, scope, element uint32, data unsafe.Pointer, size uint32) int32
	getProp func(unit uintptr, id, scope, element uint32, data unsafe.Pointer, size *uint32) int32
}

var (
	caSize  func(id uint32, addr *caAddress, qualSize uint32, qual unsafe.Pointer, size *uint32) int32
	devOnce sync.Once
	devErr  error
)

// loadDevices loads what listing and choosing devices need: CoreAudio's
// properties, CoreFoundation's strings and AudioToolbox's unit properties.
func loadDevices() error {
	if err := loadCoreAudio(); err != nil {
		return err
	}
	if err := loadCF(); err != nil {
		return fmt.Errorf("load CoreFoundation: %w", err)
	}
	devOnce.Do(func() {
		lib, err := purego.Dlopen("/System/Library/Frameworks/CoreAudio.framework/CoreAudio", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			devErr = err
			return
		}
		purego.RegisterLibFunc(&caSize, lib, "AudioObjectGetPropertyDataSize")
		tb, err := purego.Dlopen("/System/Library/Frameworks/AudioToolbox.framework/AudioToolbox", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			devErr = fmt.Errorf("load AudioToolbox: %w", err)
			return
		}
		purego.RegisterLibFunc(&au.setProp, tb, "AudioUnitSetProperty")
		purego.RegisterLibFunc(&au.getProp, tb, "AudioUnitGetProperty")
	})
	return devErr
}

// VoiceDevices lists the audio devices, inputs and outputs, without
// opening any.
func VoiceDevices() ([]VoiceDevice, error) {
	if err := loadDevices(); err != nil {
		return nil, err
	}
	addr := caAddress{caDevices, caScopeGlobal, 0}
	var size uint32
	if st := caSize(caSystemObject, &addr, 0, nil, &size); st != 0 {
		return nil, fmt.Errorf("list devices (OSStatus %d)", st)
	}
	ids := make([]uint32, size/4+1)
	if st := ca.get(caSystemObject, &addr, 0, nil, &size, unsafe.Pointer(&ids[0])); st != 0 {
		return nil, fmt.Errorf("list devices (OSStatus %d)", st)
	}
	ids = ids[:size/4]
	defIn, _ := defaultDevice(true)
	defOut, _ := defaultDevice(false)
	var out []VoiceDevice
	for _, id := range ids {
		if hidden(id) {
			continue
		}
		d := VoiceDevice{
			UID:        cfStringProp(id, caDeviceUID),
			Name:       deviceName(id),
			Transport:  transportName(uint32Prop(id, caTransportType, caScopeGlobal)),
			In:         channels(id, caScopeInput),
			Out:        channels(id, caScopeOutput),
			DefaultIn:  id == defIn,
			DefaultOut: id == defOut,
			id:         id,
		}
		d.Rate, _ = nominalRate(id)
		if d.UID == "" || d.In+d.Out == 0 {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// findDevice is the ID of the device with uid that has the side asked
// for.
func findDevice(uid string, input bool) (uint32, error) {
	ds, err := VoiceDevices()
	if err != nil {
		return 0, err
	}
	for _, d := range ds {
		if d.UID == uid && (input && d.In > 0 || !input && d.Out > 0) {
			return d.id, nil
		}
	}
	kind := "speakers"
	if input {
		kind = "mic"
	}
	return 0, fmt.Errorf("%w: no %s %q", ErrNoDevice, kind, uid)
}

func hidden(id uint32) bool {
	return uint32Prop(id, caIsHidden, caScopeGlobal) != 0
}

func uint32Prop(id, sel, scope uint32) uint32 {
	addr := caAddress{sel, scope, 0}
	var v uint32
	size := uint32(unsafe.Sizeof(v))
	if ca.get(id, &addr, 0, nil, &size, unsafe.Pointer(&v)) != 0 {
		return 0
	}
	return v
}

// cfStringProp reads a CFString property as a Go string ("" when it
// can't).
func cfStringProp(id, sel uint32) string {
	addr := caAddress{sel, caScopeGlobal, 0}
	var s uintptr
	size := uint32(unsafe.Sizeof(s))
	if st := ca.get(id, &addr, 0, nil, &size, unsafe.Pointer(&s)); st != 0 || s == 0 {
		return ""
	}
	defer cf.release(s)
	buf := make([]byte, 512)
	if !cf.getCStr(s, &buf[0], len(buf), cfStringEncodingUTF8) {
		return ""
	}
	return cString(&buf[0])
}

// channels counts a device's channels on one side, from its stream
// configuration (an AudioBufferList: a count, then 16-byte AudioBuffers
// from offset 8, each starting with its channel count).
func channels(id, scope uint32) int {
	addr := caAddress{caStreamConfig, scope, 0}
	var size uint32
	if caSize(id, &addr, 0, nil, &size) != 0 || size < 8 {
		return 0
	}
	buf := make([]uint64, (size+7)/8)
	if ca.get(id, &addr, 0, nil, &size, unsafe.Pointer(&buf[0])) != 0 {
		return 0
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), size)
	n := int(*(*uint32)(unsafe.Pointer(&b[0])))
	total := 0
	for i := 0; i < n && 8+16*i+4 <= len(b); i++ {
		total += int(*(*uint32)(unsafe.Pointer(&b[8+16*i])))
	}
	return total
}

func transportName(t uint32) string {
	switch t {
	case transportBuiltIn:
		return "built-in"
	case transportUSB:
		return "USB"
	case transportBT, transportBLE:
		return "Bluetooth"
	case transportVirtual:
		return "virtual"
	case transportAgg:
		return "aggregate"
	case transportCont:
		return "iPhone"
	case transportHDMI:
		return "HDMI"
	case transportDP:
		return "DisplayPort"
	case transportAirPlay:
		return "AirPlay"
	case transportFireWire:
		return "FireWire"
	case transportTB:
		return "Thunderbolt"
	}
	return ""
}

// setUnitDevice points an IO unit's element at a device.
func setUnitDevice(unit uintptr, element, dev uint32) error {
	if unit == 0 {
		return fmt.Errorf("no audio unit")
	}
	if st := au.setProp(unit, auCurrentDevice, auScopeGlobal, element, unsafe.Pointer(&dev), 4); st != 0 {
		return fmt.Errorf("set element %d to %s (OSStatus %d)", element, deviceName(dev), st)
	}
	return nil
}

// unitDevice is the device an IO unit's element runs on (0 when it can't
// tell).
func unitDevice(unit uintptr, element uint32) uint32 {
	if unit == 0 {
		return 0
	}
	var dev uint32
	size := uint32(4)
	if au.getProp(unit, auCurrentDevice, auScopeGlobal, element, unsafe.Pointer(&dev), &size) != 0 {
		return 0
	}
	return dev
}
