package connection

import (
	"reflect"
	"testing"
)

// TestMapArtistPath exercises the pure prefix-translation logic: longest-prefix
// wins, the match is separator-bounded (a shared name prefix does not match),
// and empty / no-match / nil-config inputs fall through to the verbatim path so
// shared-mount deployments keep today's behavior.
func TestMapArtistPath(t *testing.T) {
	cases := []struct {
		name     string
		mappings []PathMapping
		in       string
		want     string
	}{
		{
			name:     "no mappings returns verbatim",
			mappings: nil,
			in:       "/music/Artist",
			want:     "/music/Artist",
		},
		{
			name:     "single prefix translated",
			mappings: []PathMapping{{HostPrefix: "/music", PlatformPrefix: "/data/media"}},
			in:       "/music/Artist",
			want:     "/data/media/Artist",
		},
		{
			name:     "exact prefix match maps whole path",
			mappings: []PathMapping{{HostPrefix: "/music", PlatformPrefix: "/data"}},
			in:       "/music",
			want:     "/data",
		},
		{
			name: "longest prefix wins",
			mappings: []PathMapping{
				{HostPrefix: "/music", PlatformPrefix: "/data"},
				{HostPrefix: "/music/jazz", PlatformPrefix: "/vault/jazz"},
			},
			in:   "/music/jazz/Miles",
			want: "/vault/jazz/Miles",
		},
		{
			name:     "separator boundary: sibling with shared name prefix does not match",
			mappings: []PathMapping{{HostPrefix: "/music/jazz", PlatformPrefix: "/vault"}},
			in:       "/music/jazzfusion/Album",
			want:     "/music/jazzfusion/Album",
		},
		{
			name:     "no matching prefix returns verbatim",
			mappings: []PathMapping{{HostPrefix: "/media", PlatformPrefix: "/data"}},
			in:       "/music/Artist",
			want:     "/music/Artist",
		},
		{
			name:     "trailing slash on prefixes is normalized",
			mappings: []PathMapping{{HostPrefix: "/music/", PlatformPrefix: "/data/"}},
			in:       "/music/Artist",
			want:     "/data/Artist",
		},
		{
			name:     "empty host prefix is ignored",
			mappings: []PathMapping{{HostPrefix: "", PlatformPrefix: "/data"}},
			in:       "/music/Artist",
			want:     "/music/Artist",
		},
		{
			name:     "empty input returns empty",
			mappings: []PathMapping{{HostPrefix: "/music", PlatformPrefix: "/data"}},
			in:       "",
			want:     "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Connection{Type: TypeLidarr, Lidarr: &LidarrConfig{}, PathMappings: tc.mappings}
			if got := c.MapArtistPath(tc.in); got != tc.want {
				t.Errorf("MapArtistPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMapArtistPath_NilSafe confirms a nil connection, a nil Lidarr config, and
// a non-Lidarr connection all return the path unchanged instead of panicking.
func TestMapArtistPath_NilSafe(t *testing.T) {
	var nilConn *Connection
	if got := nilConn.MapArtistPath("/music/Artist"); got != "/music/Artist" {
		t.Errorf("nil connection: got %q, want verbatim", got)
	}

	noLidarr := &Connection{Type: TypeLidarr}
	if got := noLidarr.MapArtistPath("/music/Artist"); got != "/music/Artist" {
		t.Errorf("nil Lidarr config: got %q, want verbatim", got)
	}

	emby := &Connection{Type: TypeEmby, Emby: &EmbyConfig{}}
	if got := emby.MapArtistPath("/music/Artist"); got != "/music/Artist" {
		t.Errorf("emby connection: got %q, want verbatim", got)
	}
}

// TestEncodeDecodePathMappings round-trips the JSON column encoding and pins the
// empty-list <-> "" equivalence that keeps a verbatim connection's column at its
// default.
func TestEncodeDecodePathMappings(t *testing.T) {
	if got, err := EncodePathMappings(nil); err != nil || got != "" {
		t.Fatalf("EncodePathMappings(nil) = %q, %v; want \"\", nil", got, err)
	}
	if got, err := EncodePathMappings([]PathMapping{}); err != nil || got != "" {
		t.Fatalf("EncodePathMappings(empty) = %q, %v; want \"\", nil", got, err)
	}
	if got, err := DecodePathMappings(""); err != nil || got != nil {
		t.Fatalf("DecodePathMappings(\"\") = %v, %v; want nil, nil", got, err)
	}

	in := []PathMapping{
		{HostPrefix: "/music", PlatformPrefix: "/data"},
		{HostPrefix: "/media/audio", PlatformPrefix: "/mnt/audio"},
	}
	enc, err := EncodePathMappings(in)
	if err != nil {
		t.Fatalf("EncodePathMappings: %v", err)
	}
	got, err := DecodePathMappings(enc)
	if err != nil {
		t.Fatalf("DecodePathMappings: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, in)
	}

	if _, err := DecodePathMappings("{not json"); err == nil {
		t.Error("DecodePathMappings(malformed) = nil error; want decode error")
	}
}

// TestMapArtistPath_WindowsHostPathIsMappable is the regression test for the
// backslash asymmetry (#2380 follow-up). On Windows the host artist path is
// produced by filepath.Clean(filepath.Join(...)) and therefore arrives as
// `C:\music\Artist`, while the root guard (normalizeRootPath) has always folded
// `\` to `/`. MapArtistPath did NOT fold, so:
//
//	HostPrefix `C:\music` vs path `C:\music\Artist`  -> no separator-bounded match
//	HostPrefix `C:/music` vs path `C:\music\Artist`  -> no match either
//
// No mapping could match under ANY value the operator entered; the raw
// backslash path was pushed, the guard normalized it to `C:/music/Artist`, found
// it outside every (Linux, forward-slash) peer root, and refused the push --
// permanently and unfixably. Folding both sides makes the mapping and the guard
// agree on one comparison form, and the result is emitted POSIX because that is
// the form the path crosses the wire in.
func TestMapArtistPath_WindowsHostPathIsMappable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		mappings []PathMapping
		in       string
		want     string
	}{
		{
			name:     "backslash host path, backslash prefix",
			mappings: []PathMapping{{HostPrefix: `C:\music`, PlatformPrefix: "/music"}},
			in:       `C:\music\Artist`,
			want:     "/music/Artist",
		},
		{
			name:     "backslash host path, forward-slash prefix",
			mappings: []PathMapping{{HostPrefix: "C:/music", PlatformPrefix: "/music"}},
			in:       `C:\music\Artist`,
			want:     "/music/Artist",
		},
		{
			name:     "nested backslash remainder is preserved as POSIX",
			mappings: []PathMapping{{HostPrefix: `C:\music`, PlatformPrefix: "/data/media"}},
			in:       `C:\music\Alpha\Beta`,
			want:     "/data/media/Alpha/Beta",
		},
		{
			name:     "no mapping still emits POSIX so the guard compares one form",
			mappings: nil,
			in:       `C:\music\Artist`,
			want:     "C:/music/Artist",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Connection{PathMappings: tc.mappings}
			if got := c.MapArtistPath(tc.in); got != tc.want {
				t.Errorf("MapArtistPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMapArtistPath_WindowsMappedPathPassesRootGuard closes the loop the two
// halves of the bug live in: the mapped path must actually satisfy the guard the
// push chokepoint runs it through. Asserting MapArtistPath's string alone would
// not prove the two agree on a comparison form -- which is the whole defect.
func TestMapArtistPath_WindowsMappedPathPassesRootGuard(t *testing.T) {
	t.Parallel()

	c := &Connection{PathMappings: []PathMapping{{HostPrefix: `C:\music`, PlatformPrefix: "/music"}}}
	mapped := c.MapArtistPath(`C:\music\Artist`)
	if !PathWithinRoots(mapped, []string{"/music"}) {
		t.Fatalf("mapped path %q is outside root /music; every push from a Windows host would be refused", mapped)
	}
}
