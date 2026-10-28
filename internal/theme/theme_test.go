package theme

import "testing"

// Translucent lets the background through the surfaces only, and Opaque
// gives back the palette as defined.
func TestTranslucent(t *testing.T) {
	for _, p := range []Palette{Light, Dark} {
		tp := p.Translucent()
		for _, c := range tp.surfaces() {
			if c.A != p.Glass {
				t.Fatalf("dark=%v: a surface has alpha %#x, want Glass %#x", p.Dark, c.A, p.Glass)
			}
		}
		if tp.Text != p.Text || tp.Border != p.Border || tp.Accent != p.Accent || tp.Bar != p.Bar {
			t.Fatalf("dark=%v: Translucent changed more than the surfaces", p.Dark)
		}
		if tp.Opaque() != p {
			t.Fatalf("dark=%v: Opaque doesn't undo Translucent", p.Dark)
		}
	}
}
