// Package gate decides when someone is talking to the whiteboard. It
// opens on the whiteboard's name, said at the start of an utterance
// ("whiteboard, draw…", "ok whiteboard…") or as a vocative ("can you draw
// that, whiteboard?"), but not when the board is talked about ("I'll put
// it on the whiteboard later"); on a typed command; and while someone
// holds the push-to-address key. It finds where a command ends across
// short pauses, ignores the name while the whiteboard is speaking (its
// own voice can leak into a microphone), and always hears "whiteboard,
// stop", which cuts off a spoken answer and ends auto mode.
//
// It is deterministic and pure: it takes transcript segments and key and
// typed events stamped with meeting times (ms), and Tick, and returns
// what happened. Nothing reaches the model without passing through it.
package gate

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Source is how a command reached the whiteboard.
type Source string

const (
	Spoken Source = "wake"  // said with the whiteboard's name
	Typed  Source = "typed" // typed in the app
	Key    Source = "key"   // said while holding the push-to-address key
)

// Config tunes the gate. Zero values take the defaults.
type Config struct {
	// Names are what the whiteboard answers to, as they'd be written
	// ("whiteboard"). Spoken forms that differ by a letter or two, or a
	// space ("white board"), match too.
	Names []string
	// ContinueMs is how long after a speaker stops a command waits for
	// them to go on before it ends.
	ContinueMs int64
	// WakeOnlyMs is how long the name said on its own ("whiteboard…")
	// waits for the command.
	WakeOnlyMs int64
	// KeyGraceMs is how long after the push-to-address key is let go the
	// transcript of what was said while it was held may still arrive.
	KeyGraceMs int64
}

func (c *Config) defaults() {
	if len(c.Names) == 0 {
		c.Names = []string{"whiteboard"}
	}
	if c.ContinueMs == 0 {
		c.ContinueMs = 700
	}
	if c.WakeOnlyMs == 0 {
		c.WakeOnlyMs = 4000
	}
	if c.KeyGraceMs == 0 {
		c.KeyGraceMs = 2500
	}
}

// Segment is a piece of the transcript of one participant. An interim one
// is what is being said, replaced by the next; a final one is settled.
type Segment struct {
	Identity, Speaker string
	Text              string
	StartMs, EndMs    int64
	Final             bool
}

// Command is something said or typed to the whiteboard.
type Command struct {
	ID                string // "c1", "c2", … in the order the gate let them through
	Identity, Speaker string
	Text              string // the words for the whiteboard, its name taken out
	Source            Source
	StartMs, EndMs    int64 // when it was said (typed: when it was sent)
	AtMs              int64 // when the gate let it through
	// Stop asks the whiteboard to stop: stop talking, stop auto mode. It
	// is let through even while the whiteboard speaks.
	Stop bool
}

// Kind is what an Out reports.
type Kind int

const (
	Listening Kind = iota // someone started addressing the whiteboard (Identity, Speaker)
	Dropped               // they stopped without a command (only the name, the key with nothing said, a thank-you)
	Done                  // a command is complete (Command)
)

// Out is something the gate decided.
type Out struct {
	Kind              Kind
	Identity, Speaker string
	Command           Command
}

// open is a command being said.
type open struct {
	speaker  string
	source   Source
	text     []string
	startMs  int64
	lastEnd  int64 // when the last of it was said
	held     bool  // the key is down
	released int64 // when the key was let go
	pending  bool  // speech began that no final has settled yet
	heardAt  int64 // when the last of that speech was heard
	wakeOnly bool  // the name and nothing else so far
	stop     bool
}

// pendingMs is how long speech that never gets a final transcript keeps
// a command open.
const pendingMs = 6000

// Gate is the whiteboard's gate. It is not safe for concurrent use.
type Gate struct {
	cfg     Config
	open    map[string]*open // by identity
	pending map[string]bool  // identities with speech not yet final
	// hearing are identities whose interim transcript has the name: they
	// are addressing the whiteboard before the final settles it.
	hearing  map[string]heard
	speaking bool
	n        int
}

// New returns a gate.
func New(cfg Config) *Gate {
	cfg.defaults()
	return &Gate{cfg: cfg, open: map[string]*open{}, pending: map[string]bool{}, hearing: map[string]heard{}}
}

// SetSpeaking says whether the whiteboard is speaking: while it is, its
// name opens nothing but a stop.
func (g *Gate) SetSpeaking(on bool) { g.speaking = on }

// Speaking reports what SetSpeaking said.
func (g *Gate) Speaking() bool { return g.speaking }

// heard is a name heard in an interim transcript.
type heard struct{ startMs, atMs int64 }

// OldestOpen is when the oldest command still being said began,
// counting ones only heard so far in interim transcripts.
func (g *Gate) OldestOpen() (startMs int64, ok bool) {
	for _, o := range g.open {
		if !ok || o.startMs < startMs {
			startMs, ok = o.startMs, true
		}
	}
	for _, h := range g.hearing {
		if !ok || h.startMs < startMs {
			startMs, ok = h.startMs, true
		}
	}
	return startMs, ok
}

// Open lists the identities addressing the whiteboard now, oldest first,
// then the ones heard so far only in interim transcripts.
func (g *Gate) Open() []string {
	ids := g.order()
	var more []string
	for id := range g.hearing {
		if g.open[id] == nil {
			more = append(more, id)
		}
	}
	sort.Strings(more)
	return append(ids, more...)
}

// Addressing reports whether identity is addressing the whiteboard now.
func (g *Gate) Addressing(identity string) bool { return g.open[identity] != nil }

func (g *Gate) emit(identity string, o *open, at int64) Out {
	g.n++
	text := strings.TrimSpace(strings.Join(o.text, " "))
	return Out{Kind: Done, Identity: identity, Speaker: o.speaker, Command: Command{
		ID: "c" + strconv.Itoa(g.n), Identity: identity, Speaker: o.speaker, Text: text, Source: o.source,
		StartMs: o.startMs, EndMs: o.lastEnd, AtMs: at, Stop: o.stop || IsStop(text),
	}}
}

// Segment takes a piece of the transcript.
func (g *Gate) Segment(s Segment) []Out {
	o := g.open[s.Identity]
	if !s.Final {
		g.pending[s.Identity] = true
		if o != nil {
			o.pending, o.heardAt = true, s.EndMs
			return nil
		}
		// Say "listening" as soon as the name is heard, before the final.
		if _, ok := Wake(s.Text, g.cfg.Names); ok && !g.speaking {
			_, was := g.hearing[s.Identity]
			g.hearing[s.Identity] = heard{startMs: s.StartMs, atMs: s.EndMs}
			if !was {
				return []Out{{Kind: Listening, Identity: s.Identity, Speaker: s.Speaker}}
			}
		}
		return nil
	}
	g.pending[s.Identity] = false
	delete(g.hearing, s.Identity)
	text := strings.TrimSpace(s.Text)
	if o != nil {
		o.pending = false
		if text != "" {
			// A speaker who says the name again mid-command starts it over
			// only when the key isn't held; otherwise it's part of it.
			if rest, ok := Wake(text, g.cfg.Names); ok && o.source == Spoken {
				text = rest
				if o.wakeOnly {
					o.wakeOnly = false
				}
			}
			if text != "" {
				o.text = append(o.text, text)
				o.wakeOnly = false
			}
		}
		o.lastEnd = max(o.lastEnd, s.EndMs)
		if IsStop(strings.Join(o.text, " ")) {
			o.stop = true
		}
		return nil
	}
	rest, ok := Wake(text, g.cfg.Names)
	if !ok {
		return nil
	}
	if g.speaking && !IsStop(rest) {
		return nil
	}
	o = &open{speaker: s.Speaker, source: Spoken, startMs: s.StartMs, lastEnd: s.EndMs, wakeOnly: rest == "", stop: IsStop(rest)}
	if rest != "" {
		o.text = []string{rest}
	}
	g.open[s.Identity] = o
	out := []Out{{Kind: Listening, Identity: s.Identity, Speaker: s.Speaker}}
	if o.stop {
		// A stop doesn't wait for more.
		delete(g.open, s.Identity)
		out = append(out, g.emit(s.Identity, o, s.EndMs))
	}
	return out
}

// Typed takes a typed command. It goes straight through.
func (g *Gate) Typed(identity, speaker, text string, atMs int64) []Out {
	text = strings.TrimSpace(text)
	if rest, ok := Wake(text, g.cfg.Names); ok && rest != "" {
		text = rest
	}
	if text == "" {
		return nil
	}
	o := &open{speaker: speaker, source: Typed, text: []string{text}, startMs: atMs, lastEnd: atMs}
	return []Out{g.emit(identity, o, atMs)}
}

// Address takes the push-to-address key going down or up.
func (g *Gate) Address(identity, speaker string, held bool, atMs int64) []Out {
	o := g.open[identity]
	if held {
		if o != nil {
			o.held, o.source = true, Key
			return nil
		}
		g.open[identity] = &open{speaker: speaker, source: Key, startMs: atMs, lastEnd: atMs, held: true, pending: g.pending[identity], heardAt: atMs}
		return []Out{{Kind: Listening, Identity: identity, Speaker: speaker}}
	}
	if o != nil && o.held {
		o.held, o.released = false, atMs
	}
	return nil
}

// Tick ends the commands whose speakers have stopped.
func (g *Gate) Tick(nowMs int64) []Out {
	var out []Out
	for id, h := range g.hearing {
		if nowMs > h.atMs+pendingMs {
			delete(g.hearing, id) // its final never came
		}
	}
	for _, id := range g.order() {
		o := g.open[id]
		done := false
		switch {
		case o.source == Key && o.held:
		case o.source == Key:
			// Let go: wait for the transcript of what was said while held.
			done = !o.pending && nowMs >= o.released+300 || nowMs >= o.released+g.cfg.KeyGraceMs
		case o.pending && nowMs < o.heardAt+pendingMs:
			// Still talking (or its transcript is on the way).
		case o.wakeOnly:
			done = nowMs >= o.lastEnd+g.cfg.WakeOnlyMs
		default:
			done = nowMs >= o.lastEnd+g.cfg.ContinueMs
		}
		if !done {
			continue
		}
		delete(g.open, id)
		if len(o.text) == 0 || Courtesy(strings.Join(o.text, " ")) {
			out = append(out, Out{Kind: Dropped, Identity: id, Speaker: o.speaker})
			continue
		}
		out = append(out, g.emit(id, o, nowMs))
	}
	return out
}

// order is the open commands' identities, oldest first, so two that end
// on one tick come out in the order they were said.
func (g *Gate) order() []string {
	ids := make([]string, 0, len(g.open))
	for id := range g.open {
		ids = append(ids, id)
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0; j-- {
			a, b := g.open[ids[j-1]], g.open[ids[j]]
			if a.startMs < b.startMs || a.startMs == b.startMs && ids[j-1] < ids[j] {
				break
			}
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
	return ids
}

// ---- the name ----

// fillers can come before the name at the start of an utterance.
var fillers = map[string]bool{
	"hey": true, "ok": true, "okay": true, "so": true, "um": true, "uh": true, "oh": true, "alright": true,
	"all": true, "right": true, "and": true, "hi": true, "yo": true, "please": true, "now": true, "then": true,
	"well": true, "hmm": true, "mm": true, "erm": true,
}

// referential words before the name mean the board is talked about.
var referential = map[string]bool{
	"the": true, "a": true, "an": true, "on": true, "to": true, "this": true, "that": true, "our": true,
	"your": true, "my": true, "in": true, "at": true, "from": true, "of": true, "into": true, "onto": true,
	"their": true, "his": true, "her": true, "its": true, "with": true, "by": true, "for": true, "about": true,
	"any": true, "some": true, "no": true, "use": true, "using": true,
}

// subject verbs after the name, without a comma, mean it's the subject of
// a sentence about it ("whiteboard is down").
var subjectVerbs = map[string]bool{
	"is": true, "was": true, "looks": true, "seems": true, "has": true, "isn't": true, "wasn't": true,
	"doesn't": true, "didn't": true, "won't": true, "works": true, "worked": true, "broke": true,
	"are": true, "were": true,
}

// starters can follow the name at the start of an utterance when no comma
// sets it off: an instruction or a question. Anything else ("whiteboard
// markers are out") is talk about whiteboards.
var starters = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`add draw put make show sort order group connect link join remove delete take
		undo revert go move rename label call note write list keep start stop pause map sketch diagram chart
		lay arrange organize organise summarize summarise explain tell give highlight point clear change update
		split merge swap mark turn track capture record jot lets let's what which how who when where why can could
		would will please do does did is are should shall i we you just also now quick another one first next
		then put erase wipe zoom fit replace fix use try create build plot outline compare rank number edit
		set reset scrap drop flip reverse align sum count read say answer`) {
		starters[w] = true
	}
}

// courtesies said to the whiteboard aren't commands.
var courtesies = map[string]bool{
	"thanks": true, "thank you": true, "great": true, "nice": true, "cool": true, "perfect": true,
	"good job": true, "awesome": true, "thanks a lot": true, "that's great": true, "lovely": true,
}

// Courtesy reports whether text is only a thank-you.
func Courtesy(text string) bool { return courtesies[joined(text)] }

func joined(text string) string {
	var b strings.Builder
	for _, w := range words(text) {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w.w)
	}
	return b.String()
}

// word is one word of an utterance, with where it is and what follows it.
type word struct {
	w          string // lowercased letters, digits and apostrophes
	start, end int    // byte offsets in the text
	commaAfter bool   // a comma, colon, dash or sentence end follows it
	stopBefore bool   // a sentence end or comma comes before it
}

func words(text string) []word {
	var ws []word
	var cur strings.Builder
	start := -1
	breakBefore := true
	flush := func(end int) {
		if start >= 0 {
			ws = append(ws, word{w: cur.String(), start: start, end: end, stopBefore: breakBefore})
			cur.Reset()
			start = -1
			breakBefore = false
		}
	}
	for i, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '’':
			if start < 0 {
				start = i
			}
			if r == '’' {
				r = '\''
			}
			cur.WriteRune(unicode.ToLower(r))
		case r == '-' && start >= 0:
			// white-board: keep going, as one word
		default:
			flush(i)
			if strings.ContainsRune(",.!?;:—–", r) {
				if n := len(ws); n > 0 {
					ws[n-1].commaAfter = true
				}
				breakBefore = true
			}
		}
	}
	flush(len(text))
	return ws
}

// match reports whether words starting at i say a name, and how many
// words it took.
func match(ws []word, i int, names []string) int {
	for _, n := range names {
		n = strings.ToLower(strings.ReplaceAll(n, " ", ""))
		one := strings.ReplaceAll(ws[i].w, "'", "")
		if near(one, n) {
			return 1
		}
		if i+1 < len(ws) && !ws[i].commaAfter && near(one+ws[i+1].w, n) && len(one) >= 3 {
			return 2
		}
	}
	return 0
}

// near is a fuzzy match: the transcript of a name can be off by a letter
// or two ("white bored", "wide board", "whiteboards").
func near(a, name string) bool {
	if a == name {
		return true
	}
	if len(a) < len(name)-2 || len(a) > len(name)+2 || a == "" || name == "" || a[0] != name[0] {
		return false
	}
	return distance(a, name) <= 2
}

func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// Wake finds the whiteboard's name used to address it: at the start of
// the utterance (after fillers like "ok" or "hey") or as a vocative set
// off by commas. It returns the words for the whiteboard, the name and
// its commas taken out.
func Wake(text string, names []string) (rest string, ok bool) {
	ws := words(text)
	for i := range ws {
		n := match(ws, i, names)
		if n == 0 {
			continue
		}
		last := ws[i+n-1]
		atStart := true
		for _, w := range ws[:i] {
			if !fillers[w.w] {
				atStart = false
				break
			}
		}
		prev := ""
		if i > 0 {
			prev = ws[i-1].w
		}
		next := ""
		if i+n < len(ws) {
			next = ws[i+n].w
		}
		switch {
		case referential[prev] && !ws[i].stopBefore:
			continue // "on the whiteboard"
		case atStart && !last.commaAfter && subjectVerbs[next]:
			continue // "whiteboard is down"
		case atStart && !last.commaAfter && next != "" && !starters[next]:
			continue // "whiteboard markers are out"
		case atStart:
			return strings.TrimSpace(trimLead(text[last.end:])), true
		case ws[i].stopBefore && (last.commaAfter || i+n == len(ws)),
			// A question ending in the name with no comma before it ("how
			// many are there now whiteboard?"), unless a word just before it
			// makes it a thing ("do we have a new whiteboard?").
			i+n == len(ws) && strings.HasSuffix(strings.TrimSpace(text), "?") && !referential[prev] && (i < 2 || !referential[ws[i-2].w]):
			// A vocative: ", whiteboard," or ", whiteboard?" at the end.
			before := strings.TrimRight(strings.TrimSpace(text[:ws[i].start]), ",;:—– ")
			after := strings.TrimSpace(text[last.end:])
			if after == "?" || after == "." || after == "!" {
				before += after
				after = ""
			}
			after = strings.TrimSpace(trimLead(after))
			return strings.TrimSpace(strings.TrimSpace(before) + " " + after), true
		}
	}
	return "", false
}

// trimLead drops the punctuation after the name.
func trimLead(s string) string {
	return strings.TrimLeft(s, " ,;:—–-.!?")
}

// stops are what "whiteboard, stop" can be said as.
var stops = []string{
	"stop", "stop talking", "stop it", "be quiet", "quiet", "shush", "hush", "cancel", "never mind", "nevermind",
	"that's enough", "enough", "stop auto mode", "stop diagramming", "stop taking notes", "you can stop",
	"ok stop", "okay stop", "stop please", "please stop", "thanks stop", "thank you stop", "pause",
}

// IsStop reports whether a command asks the whiteboard to stop.
func IsStop(text string) bool {
	t := joined(text)
	for _, s := range stops {
		if t == s {
			return true
		}
	}
	return false
}
