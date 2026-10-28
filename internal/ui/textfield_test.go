package ui

import (
	"testing"

	"github.com/anthonyrego/huddle/internal/draw"
	"github.com/anthonyrego/huddle/internal/theme"
)

// newField returns a field measured with the fake measurer (7 units a
// rune, 16 a line) at a width that fits ten runes a line.
func newField(t *testing.T, s string) (*TextField, *Ctx) {
	t.Helper()
	f := NewTextField("Add a note", 2)
	f.Focus(true)
	c := &Ctx{Text: fakeText{}, Pal: theme.Light, Scale: 2}
	f.Input(s)
	f.Measure(c, 2*fieldPadX+70)
	return f, c
}

func (f *TextField) lineTexts() []string {
	var out []string
	for _, ln := range f.lines {
		out = append(out, f.laidOut[ln.start:ln.end])
	}
	return out
}

func key(k Key, mods Mods) Event { return Event{Kind: KeyPress, Key: k, Mods: mods} }

func TestTextFieldWrapsAtWordsAndHardBreaks(t *testing.T) {
	f, c := newField(t, "hello world again\n\nabcdefghijklmnop")
	got := f.lineTexts()
	want := []string{"hello ", "world ", "again", "", "abcdefghij", "klmnop"}
	if len(got) != len(want) {
		t.Fatalf("lines %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
	if h := f.Measure(c, 2*fieldPadX+70); h != 6*16+2*fieldPadY {
		t.Errorf("height %v", h)
	}
	empty, c2 := newField(t, "")
	if h := empty.Measure(c2, 200); h != 2*16+2*fieldPadY {
		t.Errorf("an empty field is MinLines tall: %v", h)
	}
}

func TestTextFieldMovesByGrapheme(t *testing.T) {
	f, _ := newField(t, "a👍🏽b")
	f.Key(key(KeyHome, 0))
	f.Key(key(KeyArrowRight, 0))
	f.Key(key(KeyArrowRight, 0))
	if f.Caret() != 1+8 {
		t.Errorf("right over the emoji with its skin tone: caret %d, want 9", f.Caret())
	}
	f.Key(key(KeyBackspace, 0))
	if f.Text() != "ab" || f.Caret() != 1 {
		t.Errorf("backspace takes the whole cluster: %q caret %d", f.Text(), f.Caret())
	}
	f.Key(key(KeyDelete, 0))
	if f.Text() != "a" {
		t.Errorf("delete: %q", f.Text())
	}
}

func TestTextFieldVerticalKeepsX(t *testing.T) {
	f, _ := newField(t, "abcdefgh\nab\nabcdefgh")
	f.Key(key(KeyArrowUp, Cmd))
	for range 6 {
		f.Key(key(KeyArrowRight, 0))
	}
	f.Key(key(KeyArrowDown, 0)) // the short line: its end
	if f.Caret() != 9+2 {
		t.Errorf("down onto a short line: caret %d, want 11", f.Caret())
	}
	f.Key(key(KeyArrowDown, 0)) // back to column 6
	if f.Caret() != 12+6 {
		t.Errorf("down again keeps the x: caret %d, want 18", f.Caret())
	}
	f.Key(key(KeyArrowRight, Cmd))
	if f.Caret() != len(f.Text()) {
		t.Errorf("⌘→ goes to the line's end: %d", f.Caret())
	}
}

func TestTextFieldKeys(t *testing.T) {
	f, _ := newField(t, "ok")
	if res, _ := f.Key(key('r', 0)); !res.Handled {
		t.Error("a printable key must be handled, or it fires the app's shortcut")
	}
	if res, _ := f.Key(key('r', Cmd)); res.Handled {
		t.Error("⌘ keys are the app's")
	}
	f.Key(key(KeyEnter, 0))
	if f.Text() != "ok\n" {
		t.Errorf("enter inserts a line break: %q", f.Text())
	}
	if _, act := f.Key(key(KeyEnter, Cmd)); act != FieldSubmit || f.Text() != "ok\n" {
		t.Errorf("⌘Enter submits: %v %q", act, f.Text())
	}
	if _, act := f.Key(key(KeyEscape, 0)); act != FieldBlur {
		t.Errorf("escape blurs: %v", act)
	}
	if res, _ := f.Key(key(KeyTab, 0)); res.Handled {
		t.Error("tab moves focus on")
	}
}

func TestTextFieldCleansInput(t *testing.T) {
	f, _ := newField(t, "a\r\nb\tc\x07d")
	if f.Text() != "a\nb    cd" {
		t.Errorf("cleaned: %q", f.Text())
	}
	f.MaxBytes = 12
	f.Input("ééé")
	if f.Text() != "a\nb    cdé" { // 3 bytes left: one é and not half of the next
		t.Errorf("capped on a rune boundary: %q", f.Text())
	}
}

func TestTextFieldComposition(t *testing.T) {
	f, c := newField(t, "ab")
	f.Key(key(KeyArrowLeft, 0)) // between a and b
	f.Compose("にほ", 1)
	f.Measure(c, 200)
	if f.laidOut != "aにほb" {
		t.Errorf("shown: %q", f.laidOut)
	}
	if r := f.CaretRect(); r.X != fieldPadX+2*7 {
		t.Errorf("caret inside the composition: x %v", r.X)
	}
	if res, _ := f.Key(key(KeyBackspace, 0)); !res.Handled || f.Text() != "ab" {
		t.Errorf("keys during composition are the IME's: %q", f.Text())
	}
	f.Input("日本")
	if f.Text() != "a日本b" || f.Caret() != 1+6 || f.comp != "" {
		t.Errorf("committed: %q caret %d comp %q", f.Text(), f.Caret(), f.comp)
	}
}

func TestTextFieldClick(t *testing.T) {
	f, _ := newField(t, "hello world")
	f.ClickAt(draw.Point{X: fieldPadX + 2*7 + 2, Y: fieldPadY + 16 + 3})
	if f.Caret() != 6+2 {
		t.Errorf("click on the second line, third column: caret %d, want 8", f.Caret())
	}
}

func TestTextFieldSelection(t *testing.T) {
	f, _ := newField(t, "hello brave world")
	// Shift-left twice from the end selects "ld"; typing replaces it.
	f.Key(key(KeyArrowLeft, Shift))
	f.Key(key(KeyArrowLeft, Shift))
	if s, ok := f.Copy(false); !ok || s != "ld" {
		t.Fatalf("selected %q", s)
	}
	f.Input("LD")
	if f.Text() != "hello brave worLD" {
		t.Fatalf("typing over the selection gave %q", f.Text())
	}
	// Alt moves by word; shift-alt selects a word.
	f.Key(key(KeyArrowLeft, Alt))
	f.Key(key(KeyArrowLeft, Alt|Shift))
	if s, _ := f.Copy(false); s != "brave " {
		t.Fatalf("word selection %q", s)
	}
	// A plain arrow ends the selection at its edge.
	f.Key(key(KeyArrowRight, 0))
	if _, _, ok := f.Selection(); ok || f.Caret() != len("hello brave ") {
		t.Fatalf("caret %d after right", f.Caret())
	}
	// cmd-A and cut.
	f.Key(key('a', Cmd))
	if s, ok := f.Copy(true); !ok || s != "hello brave worLD" || f.Text() != "" {
		t.Fatalf("cut %q, left %q", s, f.Text())
	}
}

func TestTextFieldDeletesWordsAndSelections(t *testing.T) {
	f, _ := newField(t, "one two three")
	f.Key(key(KeyBackspace, Alt))
	if f.Text() != "one two " {
		t.Fatalf("alt-backspace gave %q", f.Text())
	}
	f.SelectAll()
	f.Key(key(KeyBackspace, 0))
	if f.Text() != "" {
		t.Fatalf("backspace over a selection gave %q", f.Text())
	}
}

func TestTextFieldDragAndDoubleClick(t *testing.T) {
	f, c := newField(t, "pick a word here")
	f.Measure(c, 400)
	at := func(i int) draw.Point {
		return draw.Point{X: fieldPadX + f.xAt(0, i), Y: fieldPadY + 2}
	}
	f.ClickAt(at(5))
	f.DragTo(at(11))
	if s, _ := f.Copy(false); s != "a word" {
		t.Fatalf("dragged %q", s)
	}
	f.SelectWordAt(at(8))
	if s, _ := f.Copy(false); s != "word" {
		t.Fatalf("double-click selected %q", s)
	}
	f.ClickAt(at(2))
	if _, _, ok := f.Selection(); ok {
		t.Fatal("a click left a selection")
	}
}
