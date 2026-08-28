package connection

import "testing"

// TestSupportsIndexedBackdropReplace pins the allow-list #3135 introduces:
// Emby's indexed backdrop endpoint honors the URL index for placement
// (measured live in #3125), Jellyfin's identical-looking endpoint does not
// (measured live in #3135 -- it always appends, ignoring the index). Mirrors
// TestSupportsFeatureToggles' shape and rationale in this same package: a
// POSITIVE allow-list so a new/unrecognized connection type defaults to
// false (the safe direction -- assuming index-honoring semantics that do not
// exist silently duplicates backdrops) rather than true.
func TestSupportsIndexedBackdropReplace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		connType string
		want     bool
	}{
		{TypeEmby, true},
		{TypeJellyfin, false},
		{TypeLidarr, false},
		{"", false},
		{"plex", false},
		{"EMBY", false}, // case-sensitive: callers pass the stored lowercase type
	}
	for _, tc := range cases {
		t.Run(tc.connType, func(t *testing.T) {
			t.Parallel()
			if got := SupportsIndexedBackdropReplace(tc.connType); got != tc.want {
				t.Errorf("SupportsIndexedBackdropReplace(%q) = %v, want %v", tc.connType, got, tc.want)
			}
		})
	}
}
