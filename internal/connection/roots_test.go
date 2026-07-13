package connection

import (
	"strings"
	"testing"
)

// TestMapArtistPath_AppliesToEveryPlatform is THE #2380 regression guard at the
// model layer.
//
// Before the fix, MapArtistPath short-circuited on `c.Lidarr == nil`, so an Emby
// or Jellyfin connection returned the HOST path verbatim no matter what mappings
// were configured -- Stillwater pushed "/host/music/X" into peers whose
// filesystem view is "/music/X". Reverting the guard to the Lidarr-only form
// makes the emby/jellyfin cases below fail with the unmapped host path.
func TestMapArtistPath_AppliesToEveryPlatform(t *testing.T) {
	t.Parallel()

	mappings := []PathMapping{{HostPrefix: "/host/music", PlatformPrefix: "/music"}}

	cases := []struct {
		name string
		conn *Connection
		in   string
		want string
	}{
		{
			name: "emby is path-mapped (was verbatim before #2380)",
			conn: &Connection{Type: TypeEmby, Emby: &EmbyConfig{}, PathMappings: mappings},
			in:   "/host/music/Alpha",
			want: "/music/Alpha",
		},
		{
			name: "jellyfin is path-mapped (was verbatim before #2380)",
			conn: &Connection{Type: TypeJellyfin, Jellyfin: &JellyfinConfig{}, PathMappings: mappings},
			in:   "/host/music/Alpha",
			want: "/music/Alpha",
		},
		{
			name: "lidarr keeps working",
			conn: &Connection{Type: TypeLidarr, Lidarr: &LidarrConfig{}, PathMappings: mappings},
			in:   "/host/music/Alpha",
			want: "/music/Alpha",
		},
		{
			name: "no mappings: verbatim (shared-mount deployment)",
			conn: &Connection{Type: TypeEmby, Emby: &EmbyConfig{}},
			in:   "/music/Alpha",
			want: "/music/Alpha",
		},
		{
			name: "separator boundary respected: sibling prefix not claimed",
			conn: &Connection{Type: TypeJellyfin, Jellyfin: &JellyfinConfig{}, PathMappings: mappings},
			in:   "/host/musicvideos/Alpha",
			want: "/host/musicvideos/Alpha",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.conn.MapArtistPath(tc.in); got != tc.want {
				t.Errorf("MapArtistPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPathWithinRoots(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		path  string
		roots []string
		want  bool
	}{
		{"exact root", "/music", []string{"/music"}, true},
		{"under root", "/music/Alpha", []string{"/music"}, true},
		{"under second root", "/data/Alpha", []string{"/music", "/data"}, true},
		{"nested deeply", "/music/a/b/c", []string{"/music"}, true},
		{"trailing slash on root", "/music/Alpha", []string{"/music/"}, true},
		{
			// The whole point of the guard: a HOST path pushed at a peer whose
			// namespace is /music.
			name:  "host path outside container root",
			path:  "/host/music/Alpha",
			roots: []string{"/music"},
			want:  false,
		},
		{
			// Separator-boundary: /music must not swallow /musicvideos.
			name:  "sibling prefix is not containment",
			path:  "/musicvideos/Alpha",
			roots: []string{"/music"},
			want:  false,
		},
		{"parent of root is not inside it", "/", []string{"/music"}, false},
		{"empty roots is never inside", "/music/Alpha", nil, false},
		{"empty path is never inside", "", []string{"/music"}, false},
		{"blank root entries ignored", "/music/Alpha", []string{"", "  ", "/music"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PathWithinRoots(tc.path, tc.roots); got != tc.want {
				t.Errorf("PathWithinRoots(%q, %v) = %v, want %v", tc.path, tc.roots, got, tc.want)
			}
		})
	}
}

// TestRemedyForOutsideRoots_NamesTheFix asserts the operator-facing message
// carries what an operator actually needs: the refused path, the roots the peer
// reported, and the remedy. A bare "rejected" would leave them guessing at a
// translation they cannot see.
func TestRemedyForOutsideRoots_NamesTheFix(t *testing.T) {
	t.Parallel()

	msg := RemedyForOutsideRoots("Jelly", TypeJellyfin, "/host/music/Alpha", "/host/music/Alpha", []string{"/music"})
	for _, want := range []string{"/host/music/Alpha", "/music", "Jelly", "path mapping", "no path mapping matched"} {
		if !strings.Contains(msg, want) {
			t.Errorf("remedy message missing %q; got:\n%s", want, msg)
		}
	}

	// When a mapping DID apply, the message shows both sides so a wrong mapping
	// is legible rather than looking like no mapping at all.
	mapped := RemedyForOutsideRoots("Jelly", TypeJellyfin, "/host/music/Alpha", "/wrong/Alpha", []string{"/music"})
	if !strings.Contains(mapped, "mapped from") {
		t.Errorf("remedy for a mapped-but-wrong path should show the translation; got:\n%s", mapped)
	}
}

// TestPathWithinRoots_TraversalDoesNotReadAsInRoot is the regression test for
// the guard's missing lexical Clean (#2380 hardening). Before the fix,
// PathWithinRoots was a pure separator-bounded STRING prefix test, so a path
// carrying ".." components literally began with the root and reported in-root
// even though it resolves far outside it -- i.e. the security boundary could be
// walked straight through. Cleaning before the comparison makes the check
// measure the path the peer would actually resolve.
//
// (Not reachable through today's callers -- RenameDirectory gates the name with
// filepath.IsLocal + Clean -- so this locks the boundary itself, independent of
// its callers.)
func TestPathWithinRoots_TraversalDoesNotReadAsInRoot(t *testing.T) {
	t.Parallel()

	roots := []string{"/music"}
	escapes := []string{
		"/music/../../etc/evil",
		"/music/../etc/evil",
		`/music/..\../etc/evil`, // backslash-folded traversal
	}
	for _, p := range escapes {
		if PathWithinRoots(p, roots) {
			t.Errorf("PathWithinRoots(%q, %v) = true; traversal escapes the root and must be refused", p, roots)
		}
	}

	// The Clean must not break the honest cases: a redundant "." or "//" still
	// resolves inside the root and must stay allowed.
	for _, p := range []string{"/music", "/music/Alpha", "/music/./Alpha", "/music//Alpha", "/music/Beta/../Alpha"} {
		if !PathWithinRoots(p, roots) {
			t.Errorf("PathWithinRoots(%q, %v) = false; want true (resolves inside the root)", p, roots)
		}
	}
}
