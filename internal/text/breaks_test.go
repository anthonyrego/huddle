package text

import "testing"

func TestLayoutSurvivesBreakCharacters(t *testing.T) {
	s, err := NewShaper()
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"a\rb", "a\r\nb", "a\u0085b", "a b", "a b", "a\x00b", "a\xffb", "\t\v\f"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%q: panic: %v", in, r)
				}
			}()
			s.Layout([]Run{{Text: in, Style: Style{Font: SansRegular, Size: 14}}}, 200)
			s.Layout([]Run{{Text: in, Style: Style{Font: MonoRegular, Size: 13}}}, 0)
		}()
	}
}

func TestBreakCharactersStartNewLines(t *testing.T) {
	s, err := NewShaper()
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"a\nb", "a\r\nb", "a\rb", "a\u0085b", "a b", "a b"} {
		if got := len(s.Layout([]Run{{Text: in, Style: Style{Font: MonoRegular, Size: 13}}}, 0).Lines); got != 2 {
			t.Errorf("%q: %d lines, want 2", in, got)
		}
	}
}
