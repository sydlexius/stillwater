package connection

import "testing"

// TestSupportsFeatureToggles pins the allow-list that gates the three
// per-feature write toggles (#2579).
//
// This test lives here rather than relying on the API/settingsio callers to
// cover it: the predicate is defined in this package but consumed from two
// others, so under the gate's default per-package coverage profile it reads as
// 0% covered despite being exercised. That is a real gap in the local signal,
// not a profile artifact to argue away -- a direct test closes it.
//
// It also states the contract in one place. The switch is a POSITIVE
// allow-list, so an unrecognized type must come back false: a new connection
// type has to be listed deliberately rather than inheriting support (and with
// it the accept-and-discard behavior #2579 fixed).
func TestSupportsFeatureToggles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		connType string
		want     bool
	}{
		{TypeEmby, true},
		{TypeJellyfin, true},
		{TypeLidarr, false},
		// Unrecognized input must default to unsupported -- the safe
		// direction. "" covers the zero value a partially-built Connection
		// would carry.
		{"", false},
		{"plex", false},
		{"EMBY", false}, // the switch is case-sensitive; callers pass the stored lowercase type
	}
	for _, tc := range cases {
		t.Run(tc.connType, func(t *testing.T) {
			t.Parallel()
			if got := SupportsFeatureToggles(tc.connType); got != tc.want {
				t.Errorf("SupportsFeatureToggles(%q) = %v, want %v", tc.connType, got, tc.want)
			}
		})
	}
}
