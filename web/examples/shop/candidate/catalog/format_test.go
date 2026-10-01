package catalog

import "testing"

func TestFormatPrice(t *testing.T) {
	for cents, want := range map[int64]string{1250: "12.50 EUR", 5: "0.05 EUR", -300: "-3.00 EUR"} {
		if got := FormatPrice(cents, "EUR"); got != want {
			t.Errorf("FormatPrice(%d) = %q, want %q", cents, got, want)
		}
	}
}

func TestSlug(t *testing.T) {
	for name, want := range map[string]string{"Blue Mug (L)": "blue-mug-l", "  Tea  & Co ": "tea-co", "Café": "café"} {
		if got := Slug(name); got != want {
			t.Errorf("Slug(%q) = %q, want %q", name, got, want)
		}
	}
}
