package main

// agent-check: a fixture meeting against the whiteboard. It runs the
// server in this process (the store, the WebSocket hub, the dev join and
// the whiteboard) on a free port, in a room of its own, has a bot join for
// each speaker and play their track, records everything the whiteboard
// does, and scores it against the script's labels.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/board"
	"github.com/anthonyrego/huddle/internal/fixturemeet"
	"github.com/anthonyrego/huddle/internal/server/agent"
	"github.com/anthonyrego/huddle/internal/server/httpapi"
	"github.com/anthonyrego/huddle/internal/server/lktoken"
	"github.com/anthonyrego/huddle/internal/server/store"
	"github.com/anthonyrego/huddle/internal/server/wshub"
	"github.com/anthonyrego/huddle/internal/wire"
)

func init() {
	register(&command{name: "agent-check", summary: "score the whiteboard on a fixture meeting (bots play it into a fresh room)", run: runAgentCheck})
}

// recorder keeps the trace and what was sent to the room.
type recorder struct {
	mu     sync.Mutex
	events []agent.Event
	sent   []sent
	start  time.Time
	waitFn func(agent.Event) bool
	waitCh chan struct{}
}

type sent struct {
	at   time.Time
	kind string
	body any
}

func (r *recorder) trace(e agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	if r.waitFn != nil && r.waitFn(e) {
		close(r.waitCh)
		r.waitFn = nil
	}
}

func (r *recorder) waitFor(f func(agent.Event) bool, d time.Duration) bool {
	r.mu.Lock()
	for _, e := range r.events {
		if f(e) {
			r.mu.Unlock()
			return true
		}
	}
	ch := make(chan struct{})
	r.waitFn, r.waitCh = f, ch
	r.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-time.After(d):
		r.mu.Lock()
		r.waitFn = nil
		r.mu.Unlock()
		return false
	}
}

func (r *recorder) snapshot() []agent.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// recHub records what the whiteboard sends and passes it on.
type recHub struct {
	*wshub.Hub
	r *recorder
}

func (h recHub) Send(room, kind string, body any) error {
	h.r.mu.Lock()
	h.r.sent = append(h.r.sent, sent{time.Now(), kind, body})
	h.r.mu.Unlock()
	return h.Hub.Send(room, kind, body)
}

func runAgentCheck(args []string) error {
	fs := flag.NewFlagSet("agent-check", flag.ContinueOnError)
	meeting := fs.String("meeting", "", "the fixture script (testdata/meetings/x.txt), rendered with make fixtures")
	voice := fs.String("voice", agent.VoiceAnswers, "the voice policy: answers, muted or narrate")
	autoEvery := fs.Duration("auto-every", 20*time.Second, "auto mode: an update turn after this much new speech")
	outDir := fs.String("out", "", "where the report goes (default .dev/check/<meeting>-<time>)")
	huddle := fs.String("huddle", "build/huddle", "the app binary, to render the final board twice")
	settle := fs.Duration("settle", 30*time.Second, "after the meeting, how long to wait for the whiteboard to finish")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *meeting == "" {
		return errors.New("-meeting is required")
	}
	key := os.Getenv("GEMINI_API_KEY")
	dbURL := os.Getenv("DATABASE_URL")
	lkURL := os.Getenv("LIVEKIT_URL")
	minter := lktoken.Minter{Key: os.Getenv("LIVEKIT_API_KEY"), Secret: os.Getenv("LIVEKIT_API_SECRET"), TTL: 10 * time.Minute}
	if key == "" || dbURL == "" || lkURL == "" || minter.Key == "" {
		return errors.New("needs GEMINI_API_KEY, DATABASE_URL, LIVEKIT_URL, LIVEKIT_API_KEY and LIVEKIT_API_SECRET (make agent-check sets them)")
	}
	script, err := fixturemeet.Parse(*meeting)
	if err != nil {
		return err
	}
	dir := filepath.Dir(*meeting)
	timing, err := fixturemeet.ReadTiming(dir, script.Name)
	if err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-150405")
	if *outDir == "" {
		*outDir = filepath.Join(".dev", "check", script.Name+"-"+stamp)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	logf, err := os.Create(filepath.Join(*outDir, "server.log"))
	if err != nil {
		return err
	}
	defer logf.Close()
	slog.SetDefault(slog.New(slog.NewTextHandler(logf, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		return err
	}
	room := fmt.Sprintf("check-%s-%s", script.Name, strings.ReplaceAll(stamp, "-", ""))
	m, err := st.EnsureMeeting(ctx, room)
	if err != nil {
		return err
	}
	if err := st.SetVoice(ctx, room, *voice); err != nil {
		return err
	}

	rec := &recorder{start: time.Now()}
	hub := &wshub.Hub{}
	agents := agent.NewManager(ctx, agent.Config{
		Store: st, Hub: recHub{hub, rec}, APIKey: key, Minter: minter, LiveKitURL: lkURL,
		AutoEvery: *autoEvery, Trace: rec.trace, Idle: 10 * time.Minute,
	})
	hub.OnMessage = func(room, identity string, e wire.Envelope) { agents.Deliver(room, identity, e) }
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	api := &httpapi.Server{Rooms: checkRooms{st}, Minter: minter, LiveKitURL: lkURL, WS: hub, Dev: true}
	srv := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()
	server := "http://" + ln.Addr().String()

	fmt.Printf("agent-check %s: room %s, voice %s, %d lines over %s\n", script.Name, room, *voice, len(timing.Lines),
		(time.Duration(timing.DurationMs) * time.Millisecond).Round(time.Second))
	agents.Ensure(room)
	if !rec.waitFor(func(e agent.Event) bool { return e.Kind == agent.EvConnect }, 30*time.Second) {
		return errors.New("the whiteboard didn't connect to the model in 30 s (see server.log)")
	}
	var bots []*fixturemeet.Bot
	for _, sp := range script.Speakers() {
		ogg, _ := fixturemeet.Paths(dir, script.Name, sp)
		bots = append(bots, &fixturemeet.Bot{Name: sp, Ogg: ogg})
	}
	var t0 time.Time
	progress := make(chan struct{})
	go func() {
		tk := time.NewTicker(time.Minute)
		defer tk.Stop()
		for {
			select {
			case <-progress:
				return
			case <-tk.C:
				evs := rec.snapshot()
				n, v := 0, 0
				for _, e := range evs {
					switch e.Kind {
					case agent.EvCommand:
						n++
					case agent.EvVersion:
						v++
					}
				}
				fmt.Printf("  %s in: %d commands, %d versions\n", time.Since(t0).Round(time.Second), n, v)
			}
		}
	}()
	err = fixturemeet.PlayAll(ctx, server, room, bots, func(t time.Time) { t0 = t })
	close(progress)
	if err != nil {
		return fmt.Errorf("bots: %w", err)
	}
	// Let the last commands finish.
	deadline := time.Now().Add(*settle)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		if idle(rec.snapshot(), 6*time.Second, m.StartedAt) {
			break
		}
	}
	cancel()
	agents.Wait()

	evs := rec.snapshot()
	t0Ms := t0.Sub(m.StartedAt).Milliseconds()
	rctx := context.Background()
	st2, err := store.Open(rctx, dbURL)
	if err != nil {
		return err
	}
	defer st2.Close()
	rep := score(script, timing, evs, t0Ms, *voice)
	head, _ := st2.Head(rctx, room)
	if head > 0 {
		v, err := st2.Version(rctx, room, head)
		if err == nil {
			fv := wire.BoardVersion{Room: room, Seq: v.Seq, Parent: v.Parent, Requester: v.Requester, Command: v.Command, TMs: v.TMs, Doc: v.Doc, Layout: v.Layout}
			b, _ := json.MarshalIndent(fv, "", " ")
			p := filepath.Join(*outDir, "final.json")
			os.WriteFile(p, b, 0o644)
			if doc, err := snapshotDigest(v.Doc); err == nil {
				rep.add("final board (version %d):\n%s", head, indent(doc))
				rep.finalDigest = doc
			}
			rep.renders(*huddle, p, *outDir)
		}
	}
	if script.Name == "soak" {
		rep.continuity(evs)
	}
	rep.spend(evs, *voice)
	ev, _ := json.Marshal(evs)
	os.WriteFile(filepath.Join(*outDir, "events.json"), ev, 0o644)
	out := rep.String()
	os.WriteFile(filepath.Join(*outDir, "report.txt"), []byte(out), 0o644)
	fmt.Print(out)
	fmt.Printf("report, events, final board and renders in %s\n", *outDir)
	appendSpend(script.Name, rep.dollars)
	return nil
}

// idle reports whether no turn is in flight and nothing happened for d.
func idle(evs []agent.Event, d time.Duration, start time.Time) bool {
	open := 0
	var last int64
	for _, e := range evs {
		switch e.Kind {
		case agent.EvTurn:
			open++
		case agent.EvTurnEnd:
			if !strings.HasPrefix(e.Detail, "retry") {
				open--
			}
		}
		last = max(last, e.AtMs)
	}
	now := time.Since(start).Milliseconds()
	return open <= 0 && now-last > d.Milliseconds()
}

type checkRooms struct{ st *store.Store }

func (r checkRooms) EnsureRoom(ctx context.Context, room string) error {
	_, err := r.st.EnsureMeeting(ctx, room)
	return err
}

func snapshotDigest(raw json.RawMessage) (string, error) {
	snap, err := board.UnmarshalSnapshot(raw)
	if err != nil {
		return "", err
	}
	d, _ := snap.Doc()
	return d.Digest(), nil
}

func indent(s string) string { return "    " + strings.ReplaceAll(s, "\n", "\n    ") }

// ---- scoring ----

type report struct {
	b           strings.Builder
	dollars     float64
	finalDigest string
}

func (r *report) add(format string, a ...any) { fmt.Fprintf(&r.b, format+"\n", a...) }
func (r *report) String() string              { return r.b.String() }

type cmdEv struct {
	agent.Event
	start int64
	line  int // the line it matched, -1 for none
}

func score(s *fixturemeet.Script, t *fixturemeet.Timing, evs []agent.Event, t0 int64, voice string) *report {
	r := &report{}
	r.add("== %s (voice %s)", s.Name, voice)
	var cmds []*cmdEv
	turnOf := map[string]int{} // command id -> order its turn started
	firstVersion := map[string]int64{}
	audio := map[string]int{}
	spoke := map[string]bool{}
	versions := 0
	var stops []agent.Event
	nTurn := 0
	for _, e := range evs {
		switch e.Kind {
		case agent.EvCommand:
			cmds = append(cmds, &cmdEv{Event: e, start: int64(e.N), line: -1})
		case agent.EvStop:
			stops = append(stops, e)
		case agent.EvTurn:
			if _, ok := turnOf[e.Command]; !ok {
				turnOf[e.Command] = nTurn
				nTurn++
			}
		case agent.EvVersion:
			versions++
			if _, ok := firstVersion[e.Command]; !ok {
				firstVersion[e.Command] = e.AtMs
			}
		case agent.EvAudio:
			audio[e.Command] += e.N
		case agent.EvSpeak:
			spoke[e.Command] = true
		}
	}
	// Match commands to the lines they were said in: same speaker,
	// overlapping in time.
	lineOf := make([]*cmdEv, len(t.Lines))
	for _, c := range cmds {
		best, bestOverlap := -1, int64(-1)
		for i, l := range t.Lines {
			if l.Speaker != c.Speaker || lineOf[i] != nil {
				continue
			}
			ls, le := t0+l.StartMs, t0+l.EndMs
			ov := min(le, c.AtMs) - max(ls, c.start)
			if ov > bestOverlap && ov > -1500 {
				best, bestOverlap = i, ov
			}
		}
		if best >= 0 {
			c.line = best
			lineOf[best] = c
		}
	}
	var addressed, hits int
	var misses, falses []string
	for i, l := range t.Lines {
		if !l.Addressed() {
			continue
		}
		addressed++
		if lineOf[i] != nil {
			hits++
		} else {
			misses = append(misses, fmt.Sprintf("%s [%s] %q", l.Speaker, l.Label, l.Text))
		}
	}
	trueCmds := 0
	for _, c := range cmds {
		if c.line >= 0 && t.Lines[c.line].Addressed() {
			trueCmds++
			continue
		}
		what := "talk"
		if c.line >= 0 {
			what = t.Lines[c.line].Label + " " + fmt.Sprintf("%q", t.Lines[c.line].Text)
		}
		falses = append(falses, fmt.Sprintf("%s heard %q (in %s)", c.Speaker, c.Text, what))
	}
	r.add("gate: %d commands let through; %d of %d addressed lines heard", len(cmds), hits, addressed)
	r.add("  wake recall %s, precision %s", pct(hits, addressed), pct(trueCmds, len(cmds)))
	for _, m := range misses {
		r.add("  missed: %s", m)
	}
	for _, f := range falses {
		r.add("  false wake: %s", f)
	}

	// Board writes without the gate: versions whose command the gate
	// didn't let through (auto turns count as let through by the command
	// that started auto mode).
	known := map[string]bool{}
	for _, c := range cmds {
		known[c.Command] = true
	}
	ungated, fromFalse := 0, 0
	for _, e := range evs {
		if e.Kind != agent.EvVersion {
			continue
		}
		switch {
		case strings.Contains(e.Command, "-auto"):
		case !known[e.Command]:
			ungated++
		default:
			for _, c := range cmds {
				if c.Command == e.Command && (c.line < 0 || !t.Lines[c.line].Addressed()) {
					fromFalse++
				}
			}
		}
	}
	r.add("board: %d versions; %d written without the gate open; %d from false wakes", versions, ungated, fromFalse)

	// Latency: the end of a drawing command's line to its first version.
	var lat, gateLat []float64
	var slow []string
	for i, l := range t.Lines {
		c := lineOf[i]
		if l.Label != fixturemeet.Command || c == nil {
			continue
		}
		end := t0 + l.EndMs
		gateLat = append(gateLat, float64(c.AtMs-end))
		if v, ok := firstVersion[c.Command]; ok {
			lat = append(lat, float64(v-end))
		} else {
			slow = append(slow, fmt.Sprintf("%q drew nothing", l.Text))
		}
	}
	if len(lat) > 0 {
		r.add("command to first op: median %.2fs, p90 %.2fs over %d commands (gate alone: median %.2fs) — target < 2.5 s",
			q(lat, 0.5)/1000, q(lat, 0.9)/1000, len(lat), q(gateLat, 0.5)/1000)
	}
	for _, s := range slow {
		r.add("  %s", s)
	}

	// The voice policy: answers stays silent on drawing and speaks on
	// questions; muted never speaks; narrate speaks on both.
	var silentOK, silentN, speakOK, speakN int
	var voiceBad []string
	wantDraw, wantQuestion := false, true
	switch voice {
	case agent.VoiceMuted:
		wantQuestion = false
	case agent.VoiceNarrate:
		wantDraw = true
	}
	for i, l := range t.Lines {
		c := lineOf[i]
		if c == nil {
			continue
		}
		frames := audio[c.Command]
		switch l.Label {
		case fixturemeet.Command:
			silentN++
			if frames == 0 {
				silentOK++
			}
			if (frames > 0) != wantDraw {
				voiceBad = append(voiceBad, fmt.Sprintf("against the policy: %.1fs of voice on %q", float64(frames)/50, l.Text))
			}
		case fixturemeet.Question:
			speakN++
			if frames > 0 {
				speakOK++
			}
			if (frames > 0) != wantQuestion {
				voiceBad = append(voiceBad, fmt.Sprintf("against the policy: %.1fs of voice on %q (speak called: %v)", float64(frames)/50, l.Text, spoke[c.Command]))
			}
		}
	}
	r.add("voice (%s): silent on %d of %d drawing commands, spoke on %d of %d questions; %d against the policy", voice, silentOK, silentN, speakOK, speakN, len(voiceBad))
	for _, v := range voiceBad {
		r.add("  %s", v)
	}

	// Two at once: both served, in the order they were said.
	for i, l := range t.Lines {
		if !l.With || i == 0 {
			continue
		}
		a, b := lineOf[i-1], lineOf[i]
		pa, pb := t.Lines[i-1], l
		if !pa.Addressed() || !l.Addressed() {
			continue
		}
		switch {
		case a == nil || b == nil:
			r.add("two at once: %s and %s: not both heard", pa.Speaker, pb.Speaker)
		default:
			ta, oka := turnOf[a.Command]
			tb, okb := turnOf[b.Command]
			_, va := firstVersion[a.Command]
			_, vb := firstVersion[b.Command]
			order := "in order"
			if !oka || !okb || ta > tb {
				order = "OUT OF ORDER"
			}
			r.add("two at once: %s then %s: both served (%v, %v drew), %s", pa.Speaker, pb.Speaker, va, vb, order)
		}
	}
	for i, l := range t.Lines {
		if l.Label != fixturemeet.Stop {
			continue
		}
		heard := false
		for _, e := range stops {
			if e.Speaker == l.Speaker && e.AtMs >= t0+l.StartMs {
				heard = true
			}
		}
		autoOff := false
		for _, e := range evs {
			if e.Kind == agent.EvAuto && e.Detail == "off" && e.AtMs >= t0+l.StartMs {
				autoOff = true
			}
		}
		r.add("stop (line %d): heard %v, auto mode ended %v", i+1, heard, autoOff)
	}
	autoTurns := 0
	for _, e := range evs {
		if e.Kind == agent.EvTurn && e.Detail == "auto" {
			autoTurns++
		}
	}
	if autoTurns > 0 {
		r.add("auto mode: %d update turns", autoTurns)
	}
	return r
}

// continuity reports how the hour held up.
func (r *report) continuity(evs []agent.Event) {
	var connects, resumed, goAways, rotations, errs int
	for _, e := range evs {
		switch e.Kind {
		case agent.EvConnect:
			connects++
			if e.Detail == "resumed" {
				resumed++
			}
		case agent.EvGoAway:
			goAways++
		case agent.EvRotate:
			rotations++
		case agent.EvError:
			errs++
		}
	}
	r.add("continuity: %d go-aways, %d connections (%d resumed), %d transcription rotations, %d errors", goAways, connects, resumed, rotations, errs)
	var rounds []string
	for _, line := range strings.Split(r.finalDigest, "\n") {
		if strings.HasPrefix(line, "- ") && strings.Contains(strings.ToLower(line), "round") {
			rounds = append(rounds, strings.TrimPrefix(line, "- "))
		}
	}
	r.add("  notes hold %d round lines: %s", len(rounds), strings.Join(rounds, " | "))
}

// renders draws the final board twice, as two apps would, and compares.
func (r *report) renders(huddle, version, dir string) {
	if _, err := os.Stat(huddle); err != nil {
		r.add("renders: no %s (make build)", huddle)
		return
	}
	var pngs [][]byte
	for i := range 2 {
		p := filepath.Join(dir, fmt.Sprintf("final-%d.png", i+1))
		out, err := exec.Command(huddle, "render", "-version", version, "-out", p).CombinedOutput()
		if err != nil {
			r.add("renders: %v: %s", err, out)
			return
		}
		b, _ := os.ReadFile(p)
		pngs = append(pngs, b)
	}
	r.add("renders: two renders of the final version are pixel-identical: %v (%s)", bytes.Equal(pngs[0], pngs[1]), filepath.Join(dir, "final-1.png"))
}

// spend estimates what the run cost, from the usage the models reported.
func (r *report) spend(evs []agent.Event, voice string) {
	var prompt, response, summary, ear int
	var audioFrames int
	var speechMs int64
	for _, e := range evs {
		switch e.Kind {
		case agent.EvUsage:
			switch e.Detail {
			case "brain prompt":
				prompt += e.N
			case "brain response":
				response += e.N
			case "summary":
				summary += e.N
			case "ear prompt":
				ear += e.N
			}
		case agent.EvAudio:
			audioFrames += e.N
		case agent.EvSegment:
			if e.Speaker != "Whiteboard" {
				speechMs += e.AtMs - int64(e.N)
			}
		}
	}
	speechMin := float64(speechMs) / 60000
	// Prices (2026-10): Live text in $0.75/M tokens, Live audio out
	// $0.018/min (the model's whole reply, heard or not, is billed:
	// estimated from its response tokens at 25 per second), transcription
	// $0.005/min in and about $0.004/min out, flash-lite about $0.10/M.
	outMin := float64(response) / 25 / 60
	d := float64(prompt)*0.75/1e6 + outMin*0.018 + speechMin*0.009 + float64(summary)*0.10/1e6
	r.dollars = d
	r.add("spend: brain %d prompt + %d response tokens; %.1f min of speech transcribed (%d ear tokens); summary %d tokens; %.1f s of voice published; about $%.3f",
		prompt, response, speechMin, ear, summary, float64(audioFrames)/50, d)
}

func appendSpend(name string, d float64) {
	f, err := os.OpenFile(filepath.Join(".dev", "check", "spend.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s $%.3f\n", time.Now().Format(time.RFC3339), name, d)
}

func pct(a, b int) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%% (%d/%d)", 100*float64(a)/float64(b), a, b)
}

func q(xs []float64, p float64) float64 {
	s := slices.Clone(xs)
	sort.Float64s(s)
	i := int(p * float64(len(s)-1))
	return s[i]
}
