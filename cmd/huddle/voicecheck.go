package main

// voice-check: the agent gate's file-to-file runs through the media server
// the dev join names. Two participants in this process, one publishing a
// file, the other recording what it hears (with the network simulation on
// receive), both on the wall clock, so a recording lines up with its
// source:
//
//   - latency: a click train with a clean network; how long after each
//     click left the sender's mic it reached the receiver's speaker
//     (packetizing, Opus, transit, the jitter buffer and the mix)
//   - speech: the speech fixture clean, then at 5% and 10% loss with 40 ms
//     jitter, scored by correlation with the source a second at a time
//     (each second at its own lag, since the jitter buffer moves the delay)
//
// WAVs of everything heard go to -out-dir for listening.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/anthonyrego/huddle/internal/config"
	"github.com/anthonyrego/huddle/internal/devjoin"
	"github.com/anthonyrego/huddle/internal/logx"
	"github.com/anthonyrego/huddle/internal/pcm"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/fileio"
	rtcroom "github.com/anthonyrego/huddle/internal/rtc/room"
)

func init() {
	register(&command{name: "voice-check", summary: "file-to-file voice through the media server: latency from clicks, speech under loss and jitter", run: runVoiceCheck})
}

func runVoiceCheck(args []string) error {
	fs := flag.NewFlagSet("voice-check", flag.ContinueOnError)
	server := fs.String("server", envOr("HUDDLE_SERVER", "http://localhost:8080"), "the huddle server (HUDDLE_SERVER)")
	room := fs.String("room", "voice-check", "room name prefix (each run gets its own room)")
	outDir := fs.String("out-dir", ".dev/out", "where the WAVs go")
	speech := fs.String("speech", ".dev/fixtures/speech.wav", "the speech fixture (scripts/voice-fixtures.sh)")
	clicks := fs.Int("clicks", 40, "clicks in the latency run, two a second")
	only := fs.String("only", "", "run only latency or speech")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := config.Resolve()
	if err != nil {
		return err
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	logf, err := logx.Setup(paths.Logs, "voice-check", false)
	if err != nil {
		return err
	}
	defer logf.Close()
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stamp := time.Now().Format("150405")

	if *only == "" || *only == "latency" {
		const rate = rtc.SampleRate
		src := pcm.ClickTrain(*clicks, rate, rate/2, rate) // from 1 s, every 500 ms
		res, err := loop(ctx, *server, fmt.Sprintf("%s-%s-latency", *room, stamp), src, simFlags{})
		if err != nil {
			return err
		}
		fileio.WriteWAV(filepath.Join(*outDir, "voice-clicks-sent.wav"), src)
		fileio.WriteWAV(filepath.Join(*outDir, "voice-clicks-heard.wav"), res.heard)
		lats := clickLatencies(src, res)
		if len(lats) < *clicks/2 {
			return fmt.Errorf("heard only %d of %d clicks", len(lats), *clicks)
		}
		slices.Sort(lats)
		med, p95 := lats[len(lats)/2], lats[min(len(lats)*95/100, len(lats)-1)]
		fmt.Printf("latency  clean        %d clicks: median %v, p95 %v, min %v, max %v; jitter delay %v\n",
			len(lats), med.Round(time.Millisecond), p95.Round(time.Millisecond), lats[0].Round(time.Millisecond),
			lats[len(lats)-1].Round(time.Millisecond), res.stats.Target.Round(time.Millisecond))
	}
	if *only == "" || *only == "speech" {
		src, err := fileio.ReadWAV(*speech)
		if err != nil {
			return fmt.Errorf("%w (make it with scripts/voice-fixtures.sh)", err)
		}
		fileio.WriteWAV(filepath.Join(*outDir, "voice-speech-sent.wav"), src)
		for _, run := range []struct {
			name string
			sim  simFlags
		}{
			{"clean", simFlags{}},
			{"loss5", simFlags{loss: 0.05, jitter: 40 * time.Millisecond}},
			{"loss10", simFlags{loss: 0.10, jitter: 40 * time.Millisecond}},
		} {
			res, err := loop(ctx, *server, fmt.Sprintf("%s-%s-%s", *room, stamp, run.name), src, run.sim)
			if err != nil {
				return err
			}
			out := filepath.Join(*outDir, "voice-speech-"+run.name+".wav")
			fileio.WriteWAV(out, res.heard)
			// Line the recording up with the sent file first, then score
			// each second at its own lag.
			aligned := res.heard
			if off := res.offset(); off > 0 && off < len(aligned) {
				aligned = aligned[off:]
			}
			c, worst, lags := pcm.Segmental(src, aligned, rtc.SampleRate, rtc.SampleRate/2)
			lo, hi := 0, 0
			if len(lags) > 0 {
				lo, hi = slices.Min(lags), slices.Max(lags)
			}
			st := res.stats
			fmt.Printf("speech   %-12s correlation %.3f (worst second %.3f), delay moved %d ms; lost %d of %d (FEC %d, PLC %d), late %d, jitter delay %v → %s\n",
				run.name+simDesc(run.sim), c, worst, (hi-lo)*1000/rtc.SampleRate, st.Lost, st.Received+st.Lost, st.FEC, st.PLC, st.Late,
				st.Target.Round(time.Millisecond), out)
		}
	}
	return nil
}

func simDesc(s simFlags) string {
	if s.loss == 0 && s.jitter == 0 {
		return ""
	}
	return fmt.Sprintf(" %.0f%%/%v", s.loss*100, s.jitter)
}

// result is one loop: what the receiver heard and when each side started.
type result struct {
	heard              []float32
	micStart, spkStart time.Time
	stats              jitterStats
}

type jitterStats = struct {
	Received, Late, Lost, FEC, PLC int
	Target                         time.Duration
}

// offset is where the sender's first sample falls in the recording.
func (r result) offset() int {
	return int(r.micStart.Sub(r.spkStart) * rtc.SampleRate / time.Second)
}

// loop sends src from one participant to another in a fresh room and
// records what the second hears, with sim on its receive side.
func loop(ctx context.Context, server, room string, src []float32, sim simFlags) (result, error) {
	var res result
	hj, err := devJoin(ctx, server, room, "check-hear")
	if err != nil {
		return res, err
	}
	sj, err := devJoin(ctx, server, room, "check-send")
	if err != nil {
		return res, err
	}
	hear, err := sim.connector().Connect(ctx, devjoin.RTC(hj), rtc.Events{})
	if err != nil {
		return res, err
	}
	defer hear.Leave()
	spk := &fileio.Speaker{}
	if err := hear.Play(spk); err != nil {
		return res, err
	}
	send, err := (&rtcroom.Connector{}).Connect(ctx, devjoin.RTC(sj), rtc.Events{})
	if err != nil {
		return res, err
	}
	defer send.Leave()
	mic := &fileio.Mic{Samples: src}
	if err := send.PublishMic(mic); err != nil {
		return res, err
	}
	for !mic.Done() {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	time.Sleep(time.Second) // the tail
	res.heard, res.micStart, res.spkStart = spk.Samples(), mic.Start(), spk.Start()
	for _, s := range rtcroom.TrackStats(hear) {
		res.stats = jitterStats{s.Received, s.Late, s.Lost, s.FEC, s.PLC, s.Target}
	}
	if res.stats.Received == 0 {
		return res, errors.New("the receiver heard nothing")
	}
	return res, nil
}

// clickLatencies pairs each click heard with the click sent just before
// it, on the wall clock.
func clickLatencies(src []float32, res result) []time.Duration {
	const rate = rtc.SampleRate
	sent := pcm.Onsets(src, 0.02, rate/10, rate)
	heard := pcm.Onsets(res.heard, 0.02, rate/10, rate)
	at := func(start time.Time, s int) time.Time { return start.Add(time.Duration(s) * time.Second / rate) }
	var lats []time.Duration
	for _, h := range heard {
		th := at(res.spkStart, h)
		var best time.Duration = -1
		for _, s := range sent {
			if d := th.Sub(at(res.micStart, s)); d >= 0 && (best < 0 || d < best) {
				best = d
			}
		}
		if best >= 0 {
			lats = append(lats, best)
		}
	}
	return lats
}
