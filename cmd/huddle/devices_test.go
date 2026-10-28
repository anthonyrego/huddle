package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthonyrego/huddle/internal/platform"
	"github.com/anthonyrego/huddle/internal/platform/audio"
	"github.com/anthonyrego/huddle/internal/rtc"
)

func TestDeviceRows(t *testing.T) {
	rows := deviceRows(renderDevices, nil, devicePrefs{Speakers: "monitor"}, callState{}, true)
	var heads []string
	find := func(label string, after int) int {
		for i := after; i < len(rows); i++ {
			if rows[i].kind == rowChoice && rows[i].label == label {
				return i
			}
		}
		t.Fatalf("no row %q after %d", label, after)
		return -1
	}
	for _, r := range rows {
		if r.kind == rowHeader {
			heads = append(heads, r.label)
		}
		if r.kind == rowNote {
			t.Errorf("a note with nothing wrong: %q", r.label)
		}
	}
	if strings.Join(heads, ",") != "Microphone,Speakers,Echo cancelling" {
		t.Fatalf("sections %v", heads)
	}
	def := find("System default", 0)
	if !rows[def].on || rows[def].detail != "Studio Interface" {
		t.Errorf("the mic's default: %+v", rows[def])
	}
	// A mic is offered once per input device; picking one keeps the speakers.
	mic := find("Built-in Microphone", 0)
	if p := rows[mic].pick; p.Mic != "mic" || p.Speakers != "monitor" || rows[mic].on {
		t.Errorf("picking the built-in mic: %+v", rows[mic])
	}
	spk := find("Desk Monitor", mic)
	if !rows[spk].on || find("System default", mic) == -1 || rows[find("System default", mic)].on {
		t.Error("the speakers picked aren't the checked ones")
	}
	off := find("Off", spk)
	if p := rows[off].pick; !p.EchoOff || p.Speakers != "monitor" {
		t.Errorf("echo off: %+v", p)
	}
	// A device picked that isn't plugged in, and echo cancelling that
	// wouldn't start: each says so.
	rows = deviceRows(renderDevices, nil, devicePrefs{Mic: "gone"}, callState{noEcho: true}, true)
	var notes []string
	for _, r := range rows {
		if r.kind == rowNote {
			notes = append(notes, r.label)
		}
	}
	if len(notes) != 2 {
		t.Errorf("notes %q", notes)
	}
	// Without a mic only the speakers are offered.
	rows = deviceRows(renderDevices, nil, devicePrefs{}, callState{}, false)
	for _, r := range rows {
		if r.kind == rowHeader && r.label != "Speakers" {
			t.Errorf("-no-voice offers %q", r.label)
		}
	}
}

func TestDevicePrefsKept(t *testing.T) {
	dir := t.TempDir()
	if p := readDevicePrefs(dir); p != (devicePrefs{}) {
		t.Fatalf("nothing kept reads as %+v", p)
	}
	want := devicePrefs{Mic: "a", Speakers: "b", EchoOff: true}
	if err := writeDevicePrefs(dir, want); err != nil {
		t.Fatal(err)
	}
	if got := readDevicePrefs(dir); got != want {
		t.Fatalf("read back %+v", got)
	}
	os.WriteFile(filepath.Join(dir, "devices.json"), []byte("{"), 0o600)
	if p := readDevicePrefs(dir); p != (devicePrefs{}) {
		t.Fatalf("an unreadable file reads as %+v", p)
	}
}

// TestLiveMicSilenceOnTime: with no devices open the room's mic gives
// silence 10 ms a frame, so the track's clock keeps going.
func TestLiveMicSilenceOnTime(t *testing.T) {
	c := newCall(false, "", nil)
	m := &liveMic{c: c}
	frame := make([]float32, 480)
	start := time.Now()
	for range 30 {
		frame[0] = 1
		if err := m.Read(context.Background(), frame); err != nil {
			t.Fatal(err)
		}
		if frame[0] != 0 {
			t.Fatal("not silence")
		}
	}
	if took := time.Since(start); took < 280*time.Millisecond || took > 400*time.Millisecond {
		t.Fatalf("30 frames took %v, want ~300 ms", took)
	}
}

// nullRoom is a room that does nothing.
type nullRoom struct{}

func (nullRoom) PublishMic(rtc.Mic) error        { return nil }
func (nullRoom) Mute(bool)                       {}
func (nullRoom) Play(rtc.Speaker) error          { return nil }
func (nullRoom) Participants() []rtc.Participant { return nil }
func (nullRoom) Stats() rtc.Stats                { return rtc.Stats{} }
func (nullRoom) Leave()                          {}

// TestSwitchSpeakers plays into the room's speaker while the speakers
// switch between two virtual outputs (no sound in the room, no mic): the
// writes never fail, keep time across the switch, and the second device
// is the one playing after. It needs devices, so it runs only with
// HUDDLE_AUDIO_TEST=1.
func TestSwitchSpeakers(t *testing.T) {
	if os.Getenv("HUDDLE_AUDIO_TEST") != "1" {
		t.Skip("set HUDDLE_AUDIO_TEST=1 to run (opens two virtual outputs and plays silence)")
	}
	platform.LoadSDL()
	devs, err := audio.VoiceDevices()
	if err != nil {
		t.Fatal(err)
	}
	var outs []audio.VoiceDevice
	for _, d := range devs {
		if d.Out > 0 && d.Transport == "virtual" {
			outs = append(outs, d)
		}
	}
	if len(outs) < 2 {
		t.Skip("needs two virtual outputs")
	}
	dir := t.TempDir()
	c := newCall(true, dir, nil)
	room := nullRoom{}
	c.room = room
	dev, ds, err := c.openDevices(devicePrefs{Speakers: outs[0].UID})
	if err != nil {
		t.Fatal(err)
	}
	c.setDevice(room, dev, ds)
	defer c.hangUp()
	if got := c.current().Info().SpeakerName; got != outs[0].Name {
		t.Fatalf("playing on %q, want %q", got, outs[0].Name)
	}
	spk := &liveSpeaker{c: c}
	frame := make([]float32, 480)
	start := time.Now()
	const frames = 200 // 2 s
	for i := range frames {
		if i == 50 {
			c.pick(devicePrefs{Speakers: outs[1].UID})
		}
		if err := spk.Write(context.Background(), frame); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	took := time.Since(start)
	t.Logf("2 s of frames in %v across a switch from %q to %q", took, outs[0].Name, outs[1].Name)
	if took < 1800*time.Millisecond || took > 2300*time.Millisecond {
		t.Errorf("didn't keep time: %v", took)
	}
	deadline := time.Now().Add(3 * time.Second)
	for c.current() == nil || c.current().Info().SpeakerName != outs[1].Name {
		if time.Now().After(deadline) {
			t.Fatal("never switched")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if c.snapshot().status != callListening {
		t.Errorf("status after the switch: %q", c.snapshot().status)
	}
	if p := readDevicePrefs(dir); p.Speakers != outs[1].UID {
		t.Errorf("kept %+v", p)
	}
}
