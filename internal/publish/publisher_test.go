package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/library"
)

// --- test doubles ---

type fakePlatformLister struct{ ids []artist.PlatformID }

func (f *fakePlatformLister) GetPlatformIDs(_ context.Context, _ string) ([]artist.PlatformID, error) {
	return f.ids, nil
}

type fakeConnectionGetter struct {
	conns map[string]*connection.Connection
	mu    sync.Mutex
	calls int
}

func (f *fakeConnectionGetter) GetByID(_ context.Context, id string) (*connection.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	c, ok := f.conns[id]
	if !ok {
		return nil, fmt.Errorf("no connection %s", id)
	}
	return c, nil
}

// waitForPosts spins up to 2s for the given number of POSTs to arrive at the
// test server. Lock-push dispatches goroutines, so we cannot assert
// synchronously after PushLocks returns.
func waitForPosts(t *testing.T, got *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d POSTs, got %d", want, got.Load())
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- tests ---

// TestPushLocks_RoutesToEachConnection verifies that a multi-platform artist
// results in one UpdateArtistLocks POST per enabled connection carrying the
// artist's current lock state.
func TestPushLocks_RoutesToEachConnection(t *testing.T) {
	var posts atomic.Int32
	type received struct {
		LockData     bool     `json:"LockData"`
		LockedFields []string `json:"LockedFields"`
	}
	bodies := make(chan received, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// Any GET (fetch current item) returns a minimal body.
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/Items" {
				// Jellyfin fetch shape
				_, _ = w.Write([]byte(`{"Items":[{"Id":"p1","LockData":false,"LockedFields":[]}]}`))
			} else {
				// Emby fetch shape
				_, _ = w.Write([]byte(`{"Id":"p1","LockData":false,"LockedFields":[]}`))
			}
			return
		}
		var body received
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies <- body
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-emby": {ID: "c-emby", Name: "emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
		"c-jf":   {ID: "c-jf", Name: "jf", Type: connection.TypeJellyfin, URL: srv.URL, Enabled: true, PlatformUserID: "u1"},
	}}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
			{ArtistID: "a1", ConnectionID: "c-jf", PlatformArtistID: "p1"},
		}},
		ConnectionService: conns,
		Logger:            silentLogger(),
	})

	a := &artist.Artist{ID: "a1", Locked: true, LockedFields: []string{"biography", "genres"}}
	p.PushLocks(context.Background(), a)

	waitForPosts(t, &posts, 2)
	close(bodies)

	// Emby body should carry LockData=true and canonicalized LockedFields
	// (Overview+Genres). Jellyfin body carries LockData=true only; its
	// LockedFields come from the fetched server value (empty).
	seenEmbyLocked := false
	seenJellyfinLocked := false
	for body := range bodies {
		if !body.LockData {
			t.Errorf("LockData = false on a POST body, want true")
		}
		if len(body.LockedFields) == 2 {
			seenEmbyLocked = true
		}
		if len(body.LockedFields) == 0 {
			seenJellyfinLocked = true
		}
	}
	if !seenEmbyLocked {
		t.Error("did not observe Emby POST with 2 canonicalized LockedFields (Overview, Genres)")
	}
	if !seenJellyfinLocked {
		t.Error("did not observe Jellyfin POST preserving server-side empty LockedFields")
	}
}

// TestPushLocks_DisabledConnectionSkipped verifies that a connection with
// Enabled=false is not contacted.
func TestPushLocks_DisabledConnectionSkipped(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-off", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-off": {ID: "c-off", Name: "off", Type: connection.TypeEmby, URL: srv.URL, Enabled: false, PlatformUserID: "u1"},
		}},
		Logger: silentLogger(),
	})
	p.PushLocks(context.Background(), &artist.Artist{ID: "a1"})

	// Give goroutine a chance to hit the server if the guard is broken.
	time.Sleep(200 * time.Millisecond)
	if got := hits.Load(); got != 0 {
		t.Errorf("disabled connection was contacted %d times, want 0", got)
	}
}

// TestPushLocks_UnsupportedConnectionTypeSkipped verifies that connection
// types without a LockSyncer (e.g. Lidarr) are skipped silently.
func TestPushLocks_UnsupportedConnectionTypeSkipped(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-lid", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-lid": {ID: "c-lid", Name: "lid", Type: connection.TypeLidarr, URL: srv.URL, Enabled: true},
		}},
		Logger: silentLogger(),
	})
	p.PushLocks(context.Background(), &artist.Artist{ID: "a1"})

	time.Sleep(200 * time.Millisecond)
	if got := hits.Load(); got != 0 {
		t.Errorf("Lidarr connection was contacted %d times, want 0 (no LockSyncer)", got)
	}
}

// TestPushLocks_NoPlatformIDs verifies the early-return when the artist has
// no connection mappings (no goroutines spawned, no panic).
func TestPushLocks_NoPlatformIDs(t *testing.T) {
	p := New(Deps{
		ArtistService:     &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})
	p.PushLocks(context.Background(), &artist.Artist{ID: "a1"})
}

// alwaysOnResolver / alwaysOffResolver / nilResolver / errorResolver
// satisfy the publisher's libraryResolver interface for resolveLockNFO
// branch coverage. Returning *library.Library directly keeps the test
// surface narrow without instantiating a real Service.

type alwaysOnResolver struct{}

func (alwaysOnResolver) FindForArtistPath(_ context.Context, _ string) (*library.Library, error) {
	return &library.Library{ID: "lib-on", NFOLockData: true}, nil
}

type alwaysOffResolver struct{}

func (alwaysOffResolver) FindForArtistPath(_ context.Context, _ string) (*library.Library, error) {
	return &library.Library{ID: "lib-off", NFOLockData: false}, nil
}

type nilResolver struct{}

func (nilResolver) FindForArtistPath(_ context.Context, _ string) (*library.Library, error) {
	return nil, nil
}

type errorResolver struct{}

func (errorResolver) FindForArtistPath(_ context.Context, _ string) (*library.Library, error) {
	return nil, fmt.Errorf("resolver boom")
}

// TestResolveLockNFO covers every branch of Publisher.resolveLockNFO:
// nil libraryService, nil artist, empty path, lookup error, no match, and
// matched library with NFOLockData on or off. The default in every
// non-true case must be false (issue #1264 safe default).
func TestResolveLockNFO(t *testing.T) {
	logger := silentLogger()

	a := &artist.Artist{ID: "a1", Path: "/music/jazz/Coltrane"}

	t.Run("nil libraryService -> false", func(t *testing.T) {
		p := New(Deps{Logger: logger})
		if got := p.resolveLockNFO(context.Background(), a); got {
			t.Error("nil libraryService must default to false")
		}
	})

	t.Run("nil artist -> false", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &alwaysOnResolver{}})
		if got := p.resolveLockNFO(context.Background(), nil); got {
			t.Error("nil artist must default to false")
		}
	})

	t.Run("empty artist path -> false", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &alwaysOnResolver{}})
		if got := p.resolveLockNFO(context.Background(), &artist.Artist{ID: "x"}); got {
			t.Error("empty artist path must default to false")
		}
	})

	t.Run("resolver error -> false (best-effort)", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &errorResolver{}})
		if got := p.resolveLockNFO(context.Background(), a); got {
			t.Error("resolver error must default to false (best-effort)")
		}
	})

	t.Run("no matching library -> false", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &nilResolver{}})
		if got := p.resolveLockNFO(context.Background(), a); got {
			t.Error("no matching library must default to false")
		}
	})

	t.Run("library with NFOLockData=true -> true", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &alwaysOnResolver{}})
		if got := p.resolveLockNFO(context.Background(), a); !got {
			t.Error("matched library with NFOLockData=true must return true")
		}
	})

	t.Run("library with NFOLockData=false -> false", func(t *testing.T) {
		p := New(Deps{Logger: logger, LibraryService: &alwaysOffResolver{}})
		if got := p.resolveLockNFO(context.Background(), a); got {
			t.Error("matched library with NFOLockData=false must return false")
		}
	})
}
