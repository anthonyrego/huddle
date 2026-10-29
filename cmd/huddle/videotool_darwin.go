package main

// video: the video path with no camera and no screen. It joins a room
// through the server's dev join and publishes the test pattern (as a
// camera, a screen, or both) and/or records the videos it receives,
// through the same encoders, network and decoder as the app. With -in and
// -out together, a second participant in this process receives what the
// first sends, so every frame's capture time is known by its number and
// the CSV has capture → shown latency. Network conditions: -sim-* make
// the receive side worse, -uplink narrows the send side. -flash-every
// puts a beep on the voice track at each flash, for lip sync; -voice
// publishes a WAV beside the video and scores what's heard.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/rtp"

	"github.com/anthonyrego/huddle/internal/config"
	"github.com/anthonyrego/huddle/internal/devjoin"
	"github.com/anthonyrego/huddle/internal/logx"
	"github.com/anthonyrego/huddle/internal/pcm"
	"github.com/anthonyrego/huddle/internal/platform/macvideo"
	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/rtc/fileio"
	rtcroom "github.com/anthonyrego/huddle/internal/rtc/room"
	"github.com/anthonyrego/huddle/internal/video"
)

func init() {
	register(&command{name: "video", summary: "video with the test pattern for a camera: publish it, record what's received, latency and lip sync (no camera, no screen)", run: runVideo})
}

// freezeGap is a pause between shown frames counted as a freeze.
const freezeGap = 250 * time.Millisecond

func runVideo(args []string) error {
	fs := flag.NewFlagSet("video", flag.ContinueOnError)
	room := fs.String("room", "", "the room")
	server := fs.String("server", envOr("HUDDLE_SERVER", "http://localhost:8080"), "the huddle server (HUDDLE_SERVER)")
	name := fs.String("name", "video", "the name to join as")
	in := fs.String("in", "", "what to publish: pattern (empty: nothing)")
	kind := fs.String("kind", "camera", "publish the pattern as a camera or a screen")
	size := fs.String("size", "", "the pattern's size, WxH (default 1280x720; a screen 1920x1080)")
	fps := fs.Int("fps", 0, "the pattern's frames a second (default 30; a screen 15)")
	screenToo := fs.Bool("screen-too", false, "also publish a 1920x1080 screen pattern at 15 fps")
	flashEvery := fs.Duration("flash-every", 0, "light the flash patch this often, with a beep on the voice track at the same moment (lip sync)")
	voiceIn := fs.String("voice", "", "also publish this WAV (48 kHz mono) as the voice; what's heard is scored")
	out := fs.String("out", "", "record what's received in this directory: frames.csv, estimate.csv, PNGs, heard.wav")
	loop := fs.Bool("loop", true, "with -in and -out: receive as a second participant in this process (latency by frame number); false: one participant sends and receives the others")
	dur := fs.Duration("for", 20*time.Second, "how long")
	want := fs.String("want", "", "ask for every received video at this size (SetSize), WxH; off: turn it off (0×0, as the app does a tile it doesn't show)")
	switchTo := fs.String("switch", "", "then ask for WxH@T: this size, T after joining (1280x720@8s)")
	uplink := fs.String("uplink", "", "narrow what's sent to RATE bits a second (1M, 600k)")
	pngs := fs.Int("pngs", 6, "sample frames written as PNGs")
	trace := fs.Bool("trace", false, "with -out: write every received video packet (packets.csv: off the network, read) and access unit (units.csv: last packet, put together, skipped)")
	var sim simFlags
	sim.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *trace && *out == "" {
		return errors.New("-trace writes into -out")
	}
	if *room == "" || (*in == "" && *out == "") {
		return errors.New("need -room and at least one of -in pattern and -out DIR")
	}
	if *in != "" && *in != "pattern" {
		return fmt.Errorf("-in %q: only the pattern (no camera, no screen)", *in)
	}
	if *flashEvery > 0 && *voiceIn != "" {
		return errors.New("-flash-every and -voice both use the voice track: one at a time")
	}
	vk := rtc.Camera
	switch *kind {
	case "camera":
	case "screen":
		vk = rtc.Screen
	default:
		return fmt.Errorf("-kind %q: camera or screen", *kind)
	}
	w, h := 1280, 720
	if vk == rtc.Screen {
		w, h = 1920, 1080
	}
	if *size != "" {
		var err error
		if w, h, err = parseSize(*size); err != nil {
			return err
		}
	}
	if *fps == 0 {
		*fps = 30
		if vk == rtc.Screen {
			*fps = macvideo.ShareFPS
		}
	}
	var wantW, wantH, swW, swH int
	var swAt time.Duration
	switch *want {
	case "":
	case "off":
		wantW, wantH = -1, -1
	default:
		var err error
		if wantW, wantH, err = parseSize(*want); err != nil {
			return err
		}
	}
	if *switchTo != "" {
		sz, at, ok := strings.Cut(*switchTo, "@")
		var err error
		if swW, swH, err = parseSize(sz); err != nil || !ok {
			return fmt.Errorf("-switch %q: want WxH@T", *switchTo)
		}
		if swAt, err = time.ParseDuration(at); err != nil {
			return fmt.Errorf("-switch %q: %w", *switchTo, err)
		}
	}
	upBps := 0
	if *uplink != "" {
		var err error
		if upBps, err = parseRate(*uplink); err != nil {
			return err
		}
	}
	paths, err := config.Resolve()
	if err != nil {
		return err
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	logf, err := logx.Setup(paths.Logs, "video", false)
	if err != nil {
		return err
	}
	defer logf.Close()
	if *out != "" {
		if err := os.MkdirAll(*out, 0o755); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var ru0 syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	wall0 := time.Now()

	run := &videoRun{caps: map[rtc.VideoKind]map[uint32]time.Time{}, pngLeft: *pngs, dir: *out, want: [2]int{wantW, wantH}, seen: map[string][2]int{}}
	audio := *flashEvery > 0 || *voiceIn != ""
	both := *in != "" && *out != ""

	// The receiver, first, so it sees the sender's tracks arrive.
	var recv rtc.Room
	var spk *fileio.Speaker
	recvConn := sim.connector()
	recvConn.NewDecoder = videoDecoder
	recvConn.VideoFrame = run.frame
	if *trace {
		tr, err := newVideoTrace(*out)
		if err != nil {
			return err
		}
		defer tr.close()
		recvConn.VideoPacket, recvConn.VideoUnit = tr.packet, tr.unit
	}
	if *out != "" && (!both || *loop) {
		j, err := devJoin(ctx, *server, *room, *name+"-recv")
		if err != nil {
			return err
		}
		if recv, err = recvConn.Connect(ctx, devjoin.RTC(j), run.events()); err != nil {
			return err
		}
		defer recv.Leave()
		fmt.Printf("video: receiving in %s as %s\n", j.Room, j.Identity)
	}

	// The sender.
	var send rtc.Room
	var mic interface{ Start() time.Time }
	var srcVoice []float32
	if *in != "" {
		conn := &rtcroom.Connector{Uplink: upBps}
		if !*loop && *out != "" {
			conn = recvConn
			conn.Uplink = upBps
		}
		ident := *name + "-send"
		if !*loop && *out != "" {
			ident = *name
		}
		j, err := devJoin(ctx, *server, *room, ident)
		if err != nil {
			return err
		}
		run.sendID = j.Identity
		ev := rtc.Events{}
		if !*loop && *out != "" {
			ev = run.events()
		}
		if send, err = conn.Connect(ctx, devjoin.RTC(j), ev); err != nil {
			return err
		}
		defer send.Leave()
		if !*loop && *out != "" {
			recv = send
		}
		epoch := time.Now().Add(500 * time.Millisecond)
		flashFrames := 0
		if *flashEvery > 0 {
			flashFrames = max(int(flashEvery.Seconds()*float64(*fps)+0.5), 1)
		}
		stopCam, err := run.publishPattern(send, vk, w, h, *fps, flashFrames, epoch)
		if err != nil {
			return err
		}
		defer stopCam()
		if *screenToo {
			stopScr, err := run.publishPattern(send, rtc.Screen, 1920, 1080, macvideo.ShareFPS, 0, epoch)
			if err != nil {
				return err
			}
			defer stopScr()
		}
		switch {
		case flashFrames > 0:
			m := &beepMic{epoch: epoch, fps: *fps, every: flashFrames}
			if err := send.PublishMic(m); err != nil {
				return err
			}
			mic = m
		case *voiceIn != "":
			if srcVoice, err = fileio.ReadWAV(*voiceIn); err != nil {
				return err
			}
			m := &fileio.Mic{Samples: srcVoice}
			if err := send.PublishMic(m); err != nil {
				return err
			}
			mic = m
		}
		fmt.Printf("video: sending in %s as %s: %s %dx%d at %d fps%s\n", j.Room, j.Identity, vk, w, h, *fps, map[bool]string{true: ", and a 1920x1080 screen", false: ""}[*screenToo])
	}
	if recv != nil && audio {
		spk = &fileio.Speaker{}
		if err := recv.Play(spk); err != nil {
			return err
		}
	}

	// A line a second; the estimate and each layer to estimate.csv.
	var est *bufio.Writer
	if *out != "" && send != nil {
		f, err := os.Create(filepath.Join(*out, "estimate.csv"))
		if err != nil {
			return err
		}
		defer f.Close()
		est = bufio.NewWriter(f)
		defer est.Flush()
		fmt.Fprintln(est, "t_s,estimate_bps,usable_bps,sent_bps,pacer_queue_ms,uplink_drops,layer_bps")
	}
	start := time.Now()
	run.start = start
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	switched := false
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
		}
		t := time.Since(start)
		if swW > 0 && !switched && t >= swAt {
			switched = true
			run.setSizes(swW, swH)
			fmt.Printf("video: t=%2.0fs asked for %dx%d\n", t.Seconds(), swW, swH)
		}
		line := fmt.Sprintf("video: t=%2.0fs", t.Seconds())
		if send != nil {
			s := rtcroom.VideoSendStats(send)
			var layers []string
			for _, v := range s.Videos {
				for i, b := range v.Bitrate {
					l := fmt.Sprintf("%s:%dp=%dk/%dkf", v.Kind, v.Layers[i].H, b/1000, v.KeysOut[i])
					if b == 0 && i < len(v.Paused) {
						l += "/" + v.Paused[i] // unwanted (Dynacast) or bandwidth
					}
					layers = append(layers, l)
				}
			}
			line += fmt.Sprintf(" estimate %dk (usable %dk), sent %dk, pacer %v, layers %s", s.Raw/1000, s.Usable/1000, s.Sent/1000,
				s.Queue.Round(time.Millisecond), strings.Join(layers, " "))
			if est != nil {
				fmt.Fprintf(est, "%.0f,%d,%d,%d,%d,%d,%s\n", t.Seconds(), s.Raw, s.Usable, s.Sent, s.Queue.Milliseconds(), s.UplinkDrops, strings.Join(layers, " "))
			}
		}
		if recv != nil {
			for _, ts := range rtcroom.VideoTracks(recv) {
				line += fmt.Sprintf("; got %s %s %dx%d, %d frames", ts.Identity, ts.Kind, ts.W, ts.H, ts.Frames)
			}
			if js, ok := rtcroom.TrackStats(recv)[run.sendID]; ok && audio {
				line += fmt.Sprintf("; voice jitter delay %v, underrun %v", js.Target.Round(time.Millisecond), time.Duration(js.Underrun)*time.Second/rtc.SampleRate)
			}
		}
		fmt.Println(line)
		if t >= *dur {
			break
		}
	}

	// The numbers.
	var ru1 syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	wall := time.Since(wall0)
	cpu := time.Duration(ru1.Utime.Nano() + ru1.Stime.Nano() - ru0.Utime.Nano() - ru0.Stime.Nano())
	if send != nil {
		s := rtcroom.VideoSendStats(send)
		for _, v := range s.Videos {
			fmt.Printf("sent     %s: layers %s, access units %v, keyframes %v (asked for %v), largest %v bytes, probe filler %v bytes\n", v.Kind, layerList(v.Layers), v.Written, v.KeysOut, v.Keys, v.Biggest, v.Padding)
		}
		if s.UplinkDrops > 0 {
			fmt.Printf("sent     the %s uplink dropped %d packets\n", *uplink, s.UplinkDrops)
		}
	}
	if recv != nil {
		run.report(recv, *out, swW, run.switchedAt)
		if *trace {
			writePLIs(filepath.Join(*out, "plis.csv"), recv)
		}
	}
	if recv != nil && spk != nil && mic != nil {
		heard := spk.Samples()
		if *out != "" {
			fileio.WriteWAV(filepath.Join(*out, "heard.wav"), heard)
		}
		if *flashEvery > 0 {
			run.lipSync(heard, spk.Start())
		}
		if srcVoice != nil {
			aligned := heard
			if off := int(mic.Start().Sub(spk.Start()) * rtc.SampleRate / time.Second); off > 0 && off < len(aligned) {
				aligned = aligned[off:]
			}
			c, worst, _ := pcm.Segmental(srcVoice, aligned, rtc.SampleRate, rtc.SampleRate/2)
			js := rtcroom.TrackStats(recv)[run.sendID]
			fmt.Printf("voice    correlation %.3f (worst second %.3f); lost %d of %d (FEC %d, PLC %d), late %d, jitter delay %v\n",
				c, worst, js.Lost, js.Received+js.Lost, js.FEC, js.PLC, js.Late, js.Target.Round(time.Millisecond))
		}
	}
	fmt.Printf("cpu      this process %.0f%% of a core over %v (%v of CPU)\n", 100*cpu.Seconds()/wall.Seconds(), wall.Round(time.Second), cpu.Round(time.Millisecond))
	return nil
}

func parseSize(s string) (int, int, error) {
	var w, h int
	if _, err := fmt.Sscanf(strings.ToLower(s), "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("size %q: want WxH", s)
	}
	return w, h, nil
}

// parseRate reads 1M, 600k or 1000000 as bits a second.
func parseRate(s string) (int, error) {
	mult := 1.0
	switch {
	case strings.HasSuffix(strings.ToLower(s), "m"):
		mult, s = 1e6, s[:len(s)-1]
	case strings.HasSuffix(strings.ToLower(s), "k"):
		mult, s = 1e3, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("rate %q: want e.g. 1M or 600k", s)
	}
	return int(v * mult), nil
}

func layerList(ls []rtc.Layer) string {
	var out []string
	for _, l := range ls {
		out = append(out, fmt.Sprintf("%dx%d", l.W, l.H))
	}
	return strings.Join(out, ",")
}

// videoRun is one run's bookkeeping.
type videoRun struct {
	start, switchedAt time.Time
	sendID            string

	capMu sync.Mutex
	caps  map[rtc.VideoKind]map[uint32]time.Time // capture time by frame number, of what this process sends

	mu      sync.Mutex
	rows    []frameRow
	tracks  []rtc.VideoTrack
	want    [2]int
	pngLeft int
	dir     string
	seen    map[string][2]int // each track's size so far
	pngWG   sync.WaitGroup
}

type frameRow struct {
	who                                  string
	kind                                 rtc.VideoKind
	n, rtp                               uint32
	read                                 bool
	w, h                                 int
	captured, received, assembled, shown time.Time
	held, decode                         time.Duration
	key, flash                           bool
	sync                                 rtcroom.SyncInfo
}

// videoTrace writes every received video packet and access unit (-trace).
type videoTrace struct {
	mu          sync.Mutex
	pf, uf      *os.File
	pkts, units *bufio.Writer
}

func newVideoTrace(dir string) (*videoTrace, error) {
	pf, err := os.Create(filepath.Join(dir, "packets.csv"))
	if err != nil {
		return nil, err
	}
	uf, err := os.Create(filepath.Join(dir, "units.csv"))
	if err != nil {
		pf.Close()
		return nil, err
	}
	t := &videoTrace{pf: pf, uf: uf, pkts: bufio.NewWriter(pf), units: bufio.NewWriter(uf)}
	fmt.Fprintln(t.pkts, "identity,kind,seq,rtp,marker,nal,bytes,wire_ns,read_ns")
	fmt.Fprintln(t.units, "identity,kind,rtp,key,gap,skipped,last_ns,at_ns,lost_ns")
	return t, nil
}

// nalOf names an H.264 RTP payload's first NAL unit: its type, and for a
// fragment (28) the fragmented unit's type with s (its start) or c.
func nalOf(b []byte) string {
	switch {
	case len(b) == 0:
		return "pad"
	case b[0]&0x1f == 28 && len(b) > 1:
		part := "c"
		if b[1]&0x80 != 0 {
			part = "s"
		}
		return fmt.Sprintf("28%s%d", part, b[1]&0x1f)
	}
	return strconv.Itoa(int(b[0] & 0x1f))
}

func (t *videoTrace) packet(v rtc.VideoTrack, p *rtp.Packet, wire, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.pkts, "%s,%s,%d,%d,%v,%s,%d,%d,%d\n", v.Identity(), v.Kind(), p.SequenceNumber, p.Timestamp, p.Marker, nalOf(p.Payload),
		len(p.Payload), nanos(wire), nanos(at))
}

func (t *videoTrace) unit(v rtc.VideoTrack, u rtcroom.VideoUnitInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.units, "%s,%s,%d,%v,%v,%v,%d,%d,%d\n", v.Identity(), v.Kind(), u.RTP, u.Key, u.Gap, u.Skipped, nanos(u.Last), nanos(u.At), nanos(u.Lost))
}

// writePLIs writes when each received video asked for a keyframe (-trace).
func writePLIs(path string, room rtc.Room) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	fmt.Fprintln(w, "identity,kind,at_ns")
	for _, s := range rtcroom.VideoTracks(room) {
		for _, at := range s.PLIAt {
			fmt.Fprintf(w, "%s,%s,%d\n", s.Identity, s.Kind, at.UnixNano())
		}
	}
}

func (t *videoTrace) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pkts.Flush()
	t.units.Flush()
	t.pf.Close()
	t.uf.Close()
}

// publishPattern publishes the pattern as a camera or a screen.
func (r *videoRun) publishPattern(room rtc.Room, k rtc.VideoKind, w, h, fps, flashEvery int, epoch time.Time) (func(), error) {
	layers := macvideo.CameraLayers(w, h)
	if k == rtc.Screen {
		layers = macvideo.ScreenLayers(w, h)
	}
	s, err := macvideo.NewSender(macvideo.SenderConfig{Layers: layers, FPS: fps, Hold: k == rtc.Screen})
	if err != nil {
		return nil, err
	}
	r.capMu.Lock()
	r.caps[k] = map[uint32]time.Time{}
	caps := r.caps[k]
	r.capMu.Unlock()
	p, err := macvideo.StartPattern(macvideo.PatternConfig{W: w, H: h, FPS: fps, FlashEvery: flashEvery, Epoch: epoch,
		OnFrame: func(n uint32, at time.Time, _ bool) {
			r.capMu.Lock()
			caps[n] = at
			r.capMu.Unlock()
		}}, func(b *macvideo.Buffer, at time.Time) {
		s.Feed(b, at)
		b.Release()
	})
	if err != nil {
		return nil, err
	}
	if _, err := room.PublishVideo(s, k); err != nil {
		p.Stop()
		return nil, err
	}
	return p.Stop, nil
}

// events are the receiver's: each video added is asked for at -want, or
// turned off.
func (r *videoRun) events() rtc.Events {
	return rtc.Events{VideoAdded: func(t rtc.VideoTrack) {
		r.mu.Lock()
		r.tracks = append(r.tracks, t)
		want := r.want
		r.mu.Unlock()
		switch {
		case want[0] > 0:
			t.SetSize(want[0], want[1])
		case want[0] < 0:
			t.SetSize(0, 0)
		}
	}}
}

// setSizes asks for every video at w×h.
func (r *videoRun) setSizes(w, h int) {
	r.mu.Lock()
	r.want = [2]int{w, h}
	r.switchedAt = time.Now()
	ts := slices.Clone(r.tracks)
	r.mu.Unlock()
	for _, t := range ts {
		t.SetSize(w, h)
	}
}

// frame is the receiver's hook: every frame shown.
func (r *videoRun) frame(t rtc.VideoTrack, f video.Frame, info rtcroom.VideoFrameInfo) {
	now := time.Now()
	n, ok := video.ReadCode(f)
	row := frameRow{who: t.Identity(), kind: t.Kind(), n: n, read: ok, w: f.W, h: f.H, rtp: info.RTP, received: info.Received, assembled: info.Assembled,
		shown: now, held: info.Held, decode: info.Decode, key: info.Key, flash: ok && video.Flash(f), sync: info.Sync}
	if ok && t.Identity() == r.sendID {
		r.capMu.Lock()
		row.captured = r.caps[t.Kind()][n]
		r.capMu.Unlock()
	}
	key := t.Identity() + "/" + t.Kind().String()
	r.mu.Lock()
	r.rows = append(r.rows, row)
	newSize := r.seen[key] != [2]int{f.W, f.H}
	r.seen[key] = [2]int{f.W, f.H}
	png := newSize && r.pngLeft > 0 && r.dir != ""
	if png {
		r.pngLeft--
	}
	r.mu.Unlock()
	if png {
		var c video.Frame
		c.CopyFrom(f)
		path := filepath.Join(r.dir, fmt.Sprintf("%s-%s-%dx%d-%d.png", t.Identity(), t.Kind(), f.W, f.H, n))
		r.pngWG.Add(1)
		go func() {
			defer r.pngWG.Done()
			writePNG(path, c.RGBA())
		}()
	}
}

// report prints each received video's numbers and writes frames.csv.
func (r *videoRun) report(room rtc.Room, dir string, switchW int, switchedAt time.Time) {
	r.pngWG.Wait()
	r.mu.Lock()
	rows := slices.Clone(r.rows)
	r.mu.Unlock()
	if dir != "" {
		if f, err := os.Create(filepath.Join(dir, "frames.csv")); err == nil {
			w := bufio.NewWriter(f)
			fmt.Fprintln(w, "identity,kind,frame,w,h,captured_ns,received_ns,shown_ns,latency_ms,held_ms,key,flash,rtp,assembled_ns,decode_ms,sync,sync_reports_ms,sync_arrivals_ms")
			for _, x := range rows {
				lat := ""
				if !x.captured.IsZero() {
					lat = fmt.Sprintf("%.1f", float64(x.shown.Sub(x.captured).Microseconds())/1000)
				}
				fmt.Fprintf(w, "%s,%s,%d,%d,%d,%d,%d,%d,%s,%.1f,%v,%v,%d,%d,%.1f,%s,%s,%s\n", x.who, x.kind, x.n, x.w, x.h, nanos(x.captured), nanos(x.received),
					nanos(x.shown), lat, float64(x.held.Microseconds())/1000, x.key, x.flash, x.rtp, nanos(x.assembled), float64(x.decode.Microseconds())/1000, x.sync.How, msIf(x.sync.Reports, x.sync.ReportsOK), msIf(x.sync.Arrivals, x.sync.ArrivalsOK))
			}
			w.Flush()
			f.Close()
		}
	}
	stats := map[string]rtcroom.VideoTrackStats{}
	for _, s := range rtcroom.VideoTracks(room) {
		stats[s.Identity+"/"+s.Kind.String()] = s
	}
	by := map[string][]frameRow{}
	var keys []string
	for _, x := range rows {
		k := x.who + "/" + x.kind.String()
		if by[k] == nil {
			keys = append(keys, k)
		}
		by[k] = append(by[k], x)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fr := by[k]
		sort.Slice(fr, func(i, j int) bool { return fr[i].shown.Before(fr[j].shown) })
		var lats, net, asm []time.Duration
		sizes := map[[2]int]int{}
		var freezes []time.Duration
		nums := map[uint32]bool{}
		var lo, hi uint32 = math.MaxUint32, 0
		for i, x := range fr {
			if !x.captured.IsZero() {
				lats = append(lats, x.shown.Sub(x.captured))
				net = append(net, x.received.Sub(x.captured))
				asm = append(asm, x.assembled.Sub(x.captured))
			}
			sizes[[2]int{x.w, x.h}]++
			if i > 0 {
				if g := x.shown.Sub(fr[i-1].shown); g > freezeGap {
					freezes = append(freezes, g)
				}
			}
			if x.read {
				nums[x.n] = true
				lo, hi = min(lo, x.n), max(hi, x.n)
			}
		}
		span := time.Duration(0)
		if len(fr) > 1 {
			span = fr[len(fr)-1].shown.Sub(fr[0].shown)
		}
		fmt.Printf("received %s: %d frames over %v (%.1f fps)", k, len(fr), span.Round(time.Millisecond), float64(len(fr)-1)/max(span.Seconds(), 1e-9))
		if len(nums) > 0 {
			fmt.Printf(", frame numbers %d to %d, %d missing", lo, hi, int(hi-lo+1)-len(nums))
		}
		var sz []string
		for s, n := range sizes {
			sz = append(sz, fmt.Sprintf("%dx%d×%d", s[0], s[1], n))
		}
		sort.Strings(sz)
		fmt.Printf("; sizes %s\n", strings.Join(sz, " "))
		if len(lats) > 0 {
			fmt.Printf("latency  %s: capture → shown median %v, p95 %v, min %v, max %v (capture → last packet median %v, p95 %v; → put together median %v, p95 %v), %d frames\n", k,
				pctDur(lats, 50), pctDur(lats, 95), pctDur(lats, 0), pctDur(lats, 100), pctDur(net, 50), pctDur(net, 95), pctDur(asm, 50), pctDur(asm, 95), len(lats))
		}
		slices.Sort(freezes)
		fmt.Printf("freezes  %s: %d pauses over %v", k, len(freezes), freezeGap)
		if len(freezes) > 0 {
			fmt.Printf(", longest %v, all %v", freezes[len(freezes)-1].Round(time.Millisecond), roundAll(freezes))
		}
		fmt.Println()
		if s, ok := stats[k]; ok {
			fmt.Printf("track    %s: keyframes %d, gaps %d, PLIs %d, skipped units %d, packets lost for good %d, decode errors %d, first frame %v, lip-sync hold %v",
				k, s.Keyframes, s.Gaps, s.PLIs, s.Skipped, s.LostPkts, s.DecodeErrs, s.FirstFrame.Round(time.Millisecond), s.Held.Round(time.Millisecond))
			if len(s.Recoveries) > 0 {
				rec := slices.Clone(s.Recoveries)
				slices.Sort(rec)
				fmt.Printf("; recoveries (gap → keyframe shown) %d, longest %v, all %v", len(rec), rec[len(rec)-1].Round(time.Millisecond), roundAll(rec))
			}
			fmt.Println()
		}
		if switchW > 0 && !switchedAt.IsZero() {
			for _, x := range fr {
				if x.shown.After(switchedAt) && x.w == switchW {
					fmt.Printf("switch   %s: the first %dx%d frame %v after asking\n", k, x.w, x.h, x.shown.Sub(switchedAt).Round(time.Millisecond))
					break
				}
			}
		}
	}
}

// lipSync pairs each flash shown with the beep heard nearest it.
func (r *videoRun) lipSync(heard []float32, spkStart time.Time) {
	onsets := pcm.Onsets(heard, 0.02, rtc.SampleRate/5, rtc.SampleRate)
	var beeps []time.Time
	for _, o := range onsets {
		beeps = append(beeps, spkStart.Add(time.Duration(o)*time.Second/rtc.SampleRate))
	}
	r.mu.Lock()
	rows := slices.Clone(r.rows)
	r.mu.Unlock()
	var offs []time.Duration
	var flashes int
	how := map[string]int{}
	for i, x := range rows {
		// The first frame of a flash (it lights one frame).
		if !x.flash || x.who != r.sendID || (i > 0 && rows[i-1].flash && rows[i-1].n == x.n) {
			continue
		}
		flashes++
		best := time.Duration(math.MaxInt64)
		for _, b := range beeps {
			if d := x.shown.Sub(b); absDur(d) < absDur(best) {
				best = d
			}
		}
		if absDur(best) < 500*time.Millisecond {
			offs = append(offs, best)
			h := x.sync.How
			if h == "" {
				h = "none"
			}
			how[h]++
		}
	}
	defer fmt.Printf("lip sync timed by reports %d, arrivals %d, none %d\n", how["reports"], how["arrivals"], how["none"])
	if len(offs) == 0 {
		fmt.Printf("lip sync %d flashes, %d beeps heard: none paired\n", flashes, len(beeps))
		return
	}
	worst := time.Duration(0)
	for _, o := range offs {
		if absDur(o) > absDur(worst) {
			worst = o
		}
	}
	fmt.Printf("lip sync %d flashes paired with beeps (of %d flashes, %d beeps): flash − beep median %v, worst %v, all %v\n",
		len(offs), flashes, len(beeps), pctDur(offs, 50), worst.Round(time.Millisecond), roundAll(offs))
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func pctDur(d []time.Duration, p int) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)*p/100, len(s)-1)].Round(time.Millisecond)
}

func roundAll(d []time.Duration) []time.Duration {
	out := make([]time.Duration, len(d))
	for i, v := range d {
		out[i] = v.Round(time.Millisecond)
	}
	return out
}

// beepMic is a voice track of silence with a beep (a 5 ms burst of 1 kHz)
// at every flash of the pattern: frame n of a pattern started at epoch
// is captured at epoch + n/fps, and so is its beep's first sample.
type beepMic struct {
	epoch      time.Time
	fps, every int

	mu    sync.Mutex
	k     int64
	start time.Time
}

func (m *beepMic) Read(ctx context.Context, frame []float32) error {
	m.mu.Lock()
	k := m.k
	m.k++
	if m.start.IsZero() {
		m.start = m.epoch
	}
	m.mu.Unlock()
	n := int64(len(frame))
	// Frame k holds samples k·n to (k+1)·n after the epoch, captured by
	// the end of them.
	due := m.epoch.Add(time.Duration((k+1)*n) * time.Second / rtc.SampleRate)
	if d := time.Until(due); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	clear(frame)
	const burst = rtc.SampleRate / 200
	a := k * n
	// The flashes whose beep overlaps this frame.
	first := max((a-burst)*int64(m.fps)/rtc.SampleRate, 1)
	for f := first; ; f++ {
		s := f * rtc.SampleRate / int64(m.fps)
		if s >= a+n {
			break
		}
		if f%int64(m.every) != 0 {
			continue
		}
		for i := range int64(burst) {
			if j := s + i - a; j >= 0 && j < n {
				env := math.Sin(math.Pi * float64(i) / burst)
				frame[j] = float32(0.5 * env * math.Sin(2*math.Pi*1000*float64(i)/rtc.SampleRate))
			}
		}
	}
	return nil
}

// Start is when the voice's first sample was captured (the epoch).
func (m *beepMic) Start() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.start
}

// msIf is d in milliseconds, or empty.
func msIf(d time.Duration, ok bool) string {
	if !ok {
		return ""
	}
	return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000)
}
