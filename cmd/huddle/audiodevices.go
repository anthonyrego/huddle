package main

import (
	"fmt"
	"time"

	"github.com/anthonyrego/huddle/internal/platform"
	"github.com/anthonyrego/huddle/internal/platform/audio"
)

func init() {
	register(&command{name: "audio-devices", summary: "list audio devices and open the speakers once (never the microphone)", run: runAudioDevices})
}

func runAudioDevices(args []string) error {
	if err := platform.InitAudio(); err != nil {
		return err
	}
	defer platform.Quit()
	play, rec, driver, err := audio.Devices()
	if err != nil {
		return err
	}
	fmt.Printf("driver %s\n", driver)
	show := func(title string, ds []audio.Device) {
		fmt.Printf("%s:\n", title)
		for _, d := range ds {
			fmt.Printf("  %-40s %6d Hz  %d ch  buffer %d frames\n", d.Name, d.Rate, d.Channels, d.Frames)
		}
	}
	show("playback", play)
	show("recording", rec)

	spk, err := audio.OpenPlayback()
	if err != nil {
		return err
	}
	defer spk.Close()
	fmt.Printf("default speakers open: device buffer ≈ %s\n", ms(spk.Latency()))
	return nil
}

func ms(d time.Duration) string { return fmt.Sprintf("%d ms", d.Milliseconds()) }
