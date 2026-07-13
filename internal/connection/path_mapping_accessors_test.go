package connection

import "testing"

// TestPathMappingAccessors_AnyType pins the #2380 promotion: PathMappings now
// lives on Connection itself, so the accessors must work for EVERY connection
// type -- not only Lidarr, whose sub-config used to own the field. An accessor
// that still gated on c.Lidarr would return nothing for Emby/Jellyfin here,
// which is exactly how the two media servers ended up receiving raw host paths.
func TestPathMappingAccessors_AnyType(t *testing.T) {
	t.Parallel()

	for _, typ := range []string{TypeEmby, TypeJellyfin, TypeLidarr} {
		c := &Connection{ID: "c1", Type: typ}
		if got := c.GetPathMappings(); got != nil {
			t.Errorf("%s: fresh connection mappings = %+v, want nil", typ, got)
		}

		c.SetPathMappings([]PathMapping{{HostPrefix: "/host/music", PlatformPrefix: "/music"}})
		got := c.GetPathMappings()
		if len(got) != 1 || got[0].HostPrefix != "/host/music" || got[0].PlatformPrefix != "/music" {
			t.Fatalf("%s: mappings = %+v, want the set list back", typ, got)
		}
		// The set list must actually be used by the translation, per type.
		if p := c.MapArtistPath("/host/music/Alpha"); p != "/music/Alpha" {
			t.Errorf("%s: MapArtistPath = %q, want %q", typ, p, "/music/Alpha")
		}

		c.SetPathMappings(nil)
		if got := c.GetPathMappings(); got != nil {
			t.Errorf("%s: mappings after clear = %+v, want nil", typ, got)
		}
		if p := c.MapArtistPath("/host/music/Alpha"); p != "/host/music/Alpha" {
			t.Errorf("%s: cleared mappings must propagate verbatim, got %q", typ, p)
		}
	}
}

// TestPathMappingAccessors_NilReceiver: both accessors are documented nil-safe
// (callers reach them from best-effort paths that may hold an unresolved
// connection). A nil dereference here would panic the merge/rename fan-out.
func TestPathMappingAccessors_NilReceiver(t *testing.T) {
	t.Parallel()

	var c *Connection
	if got := c.GetPathMappings(); got != nil {
		t.Errorf("nil receiver: got %+v, want nil", got)
	}
	c.SetPathMappings([]PathMapping{{HostPrefix: "/host/music", PlatformPrefix: "/music"}}) // must not panic
	if got := c.GetPathMappings(); got != nil {
		t.Errorf("nil receiver after set: got %+v, want nil", got)
	}
}
