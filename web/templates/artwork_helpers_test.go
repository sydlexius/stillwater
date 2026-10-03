package templates

import "testing"

// TestArtworkKindType pins the full kind -> API image-type table, including the
// unknown/empty fallback to thumb (#2214 merged two divergent copies).
func TestArtworkKindType(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"primary":   "thumb",
		"logo":      "logo",
		"banner":    "banner",
		"backdrops": "fanart",
		"":          "thumb",
		"bogus":     "thumb",
	}
	for kind, want := range cases {
		if got := ArtworkKindType(kind); got != want {
			t.Errorf("ArtworkKindType(%q) = %q, want %q", kind, got, want)
		}
	}
}
