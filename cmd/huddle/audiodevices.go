package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/anthonyrego/huddle/internal/platform"
	"github.com/anthonyrego/huddle/internal/platform/audio"
)

func init() {
	register(&command{name: "audio-devices", summary: "list audio devices and the voice processing path, and open the speakers once (never the mic; -speakers picks which)", run: runAudioDevices})
}

func runAudioDevices(args []string) error {
	fs := flag.NewFlagSet("audio-devices", flag.ContinueOnError)
	play := fs.Duration("play", 500*time.Millisecond, "how long to play silence through the speakers")
	speakers := fs.String("speakers", "", "open these speakers (a UID from the list, or a name) instead of the system's default")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := platform.InitAudio(); err != nil {
		return err
	}
	defer platform.Quit()
	playback, rec, driver, err := audio.Devices()
	if err != nil {
		return err
	}
	fmt.Printf("SDL driver %s\n", driver)
	show := func(title string, ds []audio.Device) {
		fmt.Printf("%s:\n", title)
		for _, d := range ds {
			fmt.Printf("  %-40s %6d Hz  %d ch  buffer %d frames\n", d.Name, d.Rate, d.Channels, d.Frames)
		}
	}
	show("playback", playback)
	show("recording", rec)

	// What the device menu offers, from CoreAudio (nothing opened).
	devs, err := audio.VoiceDevices()
	if err != nil {
		return fmt.Errorf("devices: %w", err)
	}
	fmt.Printf("\ndevices (the device menu's; * the system default):\n")
	for _, d := range devs {
		mark := func(on bool) string {
			if on {
				return "*"
			}
			return " "
		}
		fmt.Printf("  %-34s in %d%s out %d%s %6.0f Hz  %-11s %s\n", d.Name, d.In, mark(d.DefaultIn), d.Out, mark(d.DefaultOut), d.Rate, d.Transport, d.UID)
	}
	spkUID := ""
	if *speakers != "" {
		for _, d := range devs {
			if d.Out > 0 && (d.UID == *speakers || strings.EqualFold(d.Name, *speakers)) {
				spkUID = d.UID
			}
		}
		if spkUID == "" {
			return fmt.Errorf("no speakers %q in the list", *speakers)
		}
	}

	// The voice path, read from CoreAudio without opening a device.
	in, out, err := audio.Defaults()
	if err != nil {
		return fmt.Errorf("default devices: %w", err)
	}
	cfg := audio.VoiceConfig{}
	fmt.Printf("\nvoice processing (AVAudioEngine; follows the system's default devices):\n")
	fmt.Printf("  mic       %-30s %6.0f Hz  (not opened; in use by any app now: %v)\n", in.Name, in.Rate, in.Running)
	fmt.Printf("  speakers  %-30s %6.0f Hz\n", out.Name, out.Rate)
	switch {
	case in.Rate == out.Rate:
		fmt.Printf("  both at %.0f Hz: voice processing runs there\n", in.Rate)
	case canRun(out.Rates, in.Rate):
		fmt.Printf("  the speakers will be switched to the mic's %.0f Hz while voice runs, and back after\n", in.Rate)
	default:
		fmt.Printf("  the speakers can't run at the mic's %.0f Hz: the mixer converts\n", in.Rate)
	}
	fmt.Printf("  the app reads the cleaned mic and plays the mix as float32 mono at 48000 Hz both ways;\n")
	fmt.Printf("  the mic arrives in 100 ms taps and is paced out in 10 ms frames; the player keeps ~%v queued\n", 50*time.Millisecond)

	// The speakers alone, through the same player the voice path uses.
	cfg.SpeakerOnly, cfg.Speakers = true, spkUID
	v, err := audio.OpenVoice(cfg)
	if err != nil {
		return err
	}
	defer v.Close()
	frame := make([]float32, 480)
	start, most := time.Now(), time.Duration(0)
	for range int(*play / (10 * time.Millisecond)) {
		if err := v.Speaker().Write(context.Background(), frame); err != nil {
			return err
		}
		most = max(most, v.Info().Queued)
	}
	took := time.Since(start)
	micNow, _ := audio.InputRunning()
	info := v.Info()
	fmt.Printf("\nspeakers open on %s (alone: no mic, no voice processing): output latency %v\n", info.SpeakerName, info.Latency.Round(100*time.Microsecond))
	fmt.Printf("  played %v of silence in %v, at most %v queued on the player, %v dropped\n",
		info.Written, took.Round(time.Millisecond), most, info.Dropped)
	both := ""
	if spkUID == "" && in.Name == out.Name {
		both = " (the mic and the speakers are one device: this counts the speakers playing)"
	}
	fmt.Printf("  the mic's device in use by any app while the speakers played: %v (before: %v)%s\n", micNow, in.Running, both)
	return nil
}

func canRun(rates []float64, r float64) bool {
	for _, x := range rates {
		if x == r {
			return true
		}
	}
	return false
}
