package main

// voice: the voice path with files for devices. It joins a room through the
// server's dev join, publishes a WAV as its mic and records what it hears,
// in real time, with no audio devices; -sim-* make the network worse on
// receive, before the jitter buffer. Two of these in one room (or one and
// a bot) are a file-to-file test; voice-check runs the measured ones.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anthonyrego/huddle/internal/config"
	"github.com/anthonyrego/huddle/internal/logx"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/fileio"
	"github.com/anthonyrego/huddle/internal/rtc/lkrtc"
	"github.com/anthonyrego/huddle/internal/wire"
)

func init() {
	register(&command{name: "voice", summary: "voice with files for devices: publish a WAV, record what's heard (no mic, no speakers)", run: runVoice})
}

// simFlags are the network simulation flags voice and voice-check share.
type simFlags struct {
	loss, reorder float64
	jitter        time.Duration
}

func (s *simFlags) register(fs *flag.FlagSet) {
	fs.Float64Var(&s.loss, "sim-loss", 0, "drop this share of received packets (0.05 is 5%)")
	fs.DurationVar(&s.jitter, "sim-jitter", 0, "hold each received packet a random extra 0 to 2× this (±J around J), in order")
	fs.Float64Var(&s.reorder, "sim-reorder", 0, "hold this share of received packets past the next one")
}

func (s simFlags) connector() *lkrtc.Connector {
	c := &lkrtc.Connector{}
	if s.loss > 0 || s.jitter > 0 || s.reorder > 0 {
		c.Sim = lkrtc.NewSim(s.loss, s.jitter, s.reorder)
	}
	return c
}

func runVoice(args []string) error {
	fs := flag.NewFlagSet("voice", flag.ContinueOnError)
	in := fs.String("in", "", "the WAV to publish (48 kHz mono s16); empty publishes nothing")
	out := fs.String("out", "", "where to write what's heard (WAV)")
	room := fs.String("room", "", "the room")
	server := fs.String("server", envOr("HUDDLE_SERVER", "http://localhost:8080"), "the huddle server (HUDDLE_SERVER)")
	name := fs.String("name", "voice", "the name to join as")
	dur := fs.Duration("for", 0, "how long to stay (default: the input's length plus 3 s, or until interrupted without -in)")
	arrivals := fs.String("arrivals", "", "write every received packet (identity, seq, timestamp, bytes, arrival ns) to this CSV")
	var sim simFlags
	sim.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *room == "" || (*in == "" && *out == "") {
		return errors.New("need -room and at least one of -in and -out")
	}
	paths, err := config.Resolve()
	if err != nil {
		return err
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	logf, err := logx.Setup(paths.Logs, "voice", false)
	if err != nil {
		return err
	}
	defer logf.Close()

	var src []float32
	if *in != "" {
		if src, err = fileio.ReadWAV(*in); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	j, err := devJoin(ctx, *server, *room, *name)
	if err != nil {
		return err
	}
	conn := sim.connector()
	if *arrivals != "" {
		f, err := os.Create(*arrivals)
		if err != nil {
			return err
		}
		defer f.Close()
		var mu sync.Mutex
		w := bufio.NewWriter(f)
		defer func() { mu.Lock(); w.Flush(); mu.Unlock() }()
		fmt.Fprintln(w, "identity,seq,ts,bytes,at_ns")
		conn.Arrival = func(id string, seq uint16, ts uint32, size int, at time.Time) {
			mu.Lock()
			fmt.Fprintf(w, "%s,%d,%d,%d,%d\n", id, seq, ts, size, at.UnixNano())
			mu.Unlock()
		}
	}
	rm, err := conn.Connect(ctx, rtc.Join{URL: j.LiveKitURL, Token: j.Token, Identity: j.Identity}, rtc.Events{})
	if err != nil {
		return err
	}
	defer rm.Leave()
	spk := &fileio.Speaker{}
	if *out != "" {
		if err := rm.Play(spk); err != nil {
			return err
		}
	}
	mic := &fileio.Mic{Samples: src}
	if *in != "" {
		if err := rm.PublishMic(mic); err != nil {
			return err
		}
	}
	length := *dur
	if length == 0 && *in != "" {
		length = time.Duration(len(src))*time.Second/rtc.SampleRate + 3*time.Second
	}
	fmt.Printf("voice: in room %s as %s for %v\n", j.Room, j.Identity, length)
	if length > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(length):
		}
	} else {
		<-ctx.Done()
	}
	printStats(rm)
	if !mic.Start().IsZero() {
		fmt.Printf("voice: mic started %s\n", mic.Start().Format(time.RFC3339Nano))
	}
	if *out != "" {
		fmt.Printf("voice: speaker started %s\n", spk.Start().Format(time.RFC3339Nano))
		if err := fileio.WriteWAV(*out, spk.Samples()); err != nil {
			return err
		}
		fmt.Printf("voice: wrote %s (%.1f s)\n", *out, float64(len(spk.Samples()))/rtc.SampleRate)
	}
	return nil
}

// printStats prints each participant's jitter-buffer numbers.
func printStats(rm rtc.Room) {
	ts := lkrtc.TrackStats(rm)
	who := make([]string, 0, len(ts))
	for w := range ts {
		who = append(who, w)
	}
	sort.Strings(who)
	for _, w := range who {
		s := ts[w]
		fmt.Printf("voice: heard %-20s jitter delay %v, jitter %v, received %d, lost %d (FEC %d, PLC %d), late %d, underrun %v\n",
			w, s.Target.Round(time.Millisecond), s.Jitter.Round(100*time.Microsecond), s.Received, s.Lost, s.FEC, s.PLC, s.Late,
			(time.Duration(s.Underrun) * time.Second / rtc.SampleRate).Round(time.Millisecond))
	}
}

// devJoin asks the server's dev join for a LiveKit token.
func devJoin(ctx context.Context, server, room, name string) (wire.Joined, error) {
	var j wire.Joined
	body, _ := json.Marshal(wire.DevJoin{Room: room, Name: name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+"/dev/join", bytes.NewReader(body))
	if err != nil {
		return j, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return j, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var e wire.Error
		json.NewDecoder(res.Body).Decode(&e)
		return j, fmt.Errorf("join: %s: %s", res.Status, e.Message)
	}
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil {
		return j, fmt.Errorf("join: %w", err)
	}
	slog.Info("voice: dev join", "room", j.Room, "identity", j.Identity, "livekit", j.LiveKitURL)
	return j, nil
}
