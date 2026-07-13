package publish

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// virtualFoldersServer serves an Emby/Jellyfin /Library/VirtualFolders payload
// (the two are identical at the raw-JSON level), or fails with the given status.
func virtualFoldersServer(t *testing.T, status int, locations ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/Library/VirtualFolders") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"Name": "Music", "ItemId": "lib-1", "CollectionType": "music", "Locations": locations},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRenameRootListerFactory_ProductionDispatch pins the PRODUCTION root-lister
// factory the guard depends on. Every supported peer type must report its OWN
// roots (read from a real HTTP fixture), and an unknown type must report none --
// which the fail-closed guard treats as UNVERIFIABLE and refuses. A factory that
// handed back a lister for a type it cannot actually query would silently re-open
// the blind-push hole #2380 closed.
func TestRenameRootListerFactory_ProductionDispatch(t *testing.T) {
	for _, typ := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		t.Run(typ, func(t *testing.T) {
			srv := virtualFoldersServer(t, 0, "/music", "/music2")
			lister, ok := renameRootListerFactory(
				&connection.Connection{ID: "c1", Type: typ, URL: srv.URL}, silentLogger())
			if !ok || lister == nil {
				t.Fatalf("%s: factory returned no root lister", typ)
			}
			roots, err := lister.ListRoots(context.Background())
			if err != nil {
				t.Fatalf("%s: ListRoots: %v", typ, err)
			}
			if len(roots) != 2 || roots[0] != "/music" || roots[1] != "/music2" {
				t.Fatalf("%s: roots = %v, want the peer's library locations", typ, roots)
			}
		})
	}

	t.Run("lidarr", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.URL.Path, "rootfolder") {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			// The blank-path entry must be dropped: an empty root would make
			// PathWithinRoots match everything and neuter the guard.
			_, _ = w.Write([]byte(`[{"id":1,"path":"/music"},{"id":2,"path":""}]`))
		}))
		defer srv.Close()

		lister, ok := renameRootListerFactory(
			&connection.Connection{ID: "c1", Type: connection.TypeLidarr, URL: srv.URL}, silentLogger())
		if !ok || lister == nil {
			t.Fatal("lidarr: factory returned no root lister")
		}
		roots, err := lister.ListRoots(context.Background())
		if err != nil {
			t.Fatalf("lidarr: ListRoots: %v", err)
		}
		if len(roots) != 1 || roots[0] != "/music" {
			t.Fatalf("lidarr: roots = %v, want only the non-blank root folder", roots)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		if _, ok := renameRootListerFactory(
			&connection.Connection{ID: "c1", Type: "unsupported"}, silentLogger()); ok {
			t.Error("unsupported type: factory reported a root lister, want none")
		}
	})
}

// TestRootListers_SurfaceLibraryErrors: an unreadable VirtualFolders endpoint
// must ERROR, not report zero roots. The distinction is load-bearing: the guard
// is fail-closed either way, but an error names the real cause for the operator.
func TestRootListers_SurfaceLibraryErrors(t *testing.T) {
	for _, typ := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		srv := virtualFoldersServer(t, http.StatusInternalServerError)
		lister, ok := renameRootListerFactory(
			&connection.Connection{ID: "c1", Type: typ, URL: srv.URL}, silentLogger())
		if !ok {
			t.Fatalf("%s: factory returned no root lister", typ)
		}
		if roots, err := lister.ListRoots(context.Background()); err == nil {
			t.Errorf("%s: got roots %v and no error, want the peer failure surfaced", typ, roots)
		}
	}
}

// TestSyncRename_GuardRefusesUnverifiableConnectionType is the FAIL-CLOSED
// assertion for a peer type with no root surface: the guard must REFUSE the push
// (a type we cannot check is exactly where a bad path would slip through
// reporting ok). The updater must never be called.
func TestSyncRename_GuardRefusesUnverifiableConnectionType(t *testing.T) {
	fake := &fakePathUpdater{}
	origU := renamePathUpdaterFactory
	renamePathUpdaterFactory = func(*connection.Connection, *slog.Logger) (pathUpdater, bool) {
		return fake, true
	}
	t.Cleanup(func() { renamePathUpdaterFactory = origU })

	// No root lister for this type: the guard cannot verify the path.
	origR := renameRootListerFactory
	renameRootListerFactory = func(*connection.Connection, *slog.Logger) (rootLister, bool) {
		return nil, false
	}
	t.Cleanup(func() { renameRootListerFactory = origR })

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c1": {ID: "c1", Name: "peer", Type: "unsupported", URL: "http://peer.invalid", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/music/Old", "/music/Alpha")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 || results[0].Result != artist.PlatformRemapFailed {
		t.Fatalf("results = %+v, want the push refused", results)
	}
	if !strings.Contains(results[0].Error, "no root-folder list") {
		t.Errorf("Error = %q, want it to say the path could not be verified", results[0].Error)
	}
	if fake.called != 0 {
		t.Errorf("updater called %d times for an unverifiable peer, want 0 (fail-closed)", fake.called)
	}
}
