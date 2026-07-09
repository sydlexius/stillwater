package publish

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// refreshRecorder captures the RefreshAfterMerge calls a swapped
// mergeRefresherFactory routes to it, keyed by connection ID. It lets the
// dispatch tests assert which connections were refreshed (and with which
// survivor platform ID) without standing up real Emby/Jellyfin/Lidarr peers;
// the production factory + per-client adapters are covered separately by
// TestSyncMergeRefresh_FactoryProductionDispatch.
type refreshRecorder struct {
	mu    sync.Mutex
	calls map[string]string // connID -> survivorPlatformID passed to RefreshAfterMerge
	err   error             // when non-nil, every recorded call returns it
}

func (rec *refreshRecorder) forConn(conn *connection.Connection) mergeRefresher {
	return &recordingRefresher{rec: rec, connID: conn.ID}
}

type recordingRefresher struct {
	rec    *refreshRecorder
	connID string
}

func (r *recordingRefresher) RefreshAfterMerge(_ context.Context, survivorPlatformID string) error {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	if r.rec.calls == nil {
		r.rec.calls = map[string]string{}
	}
	r.rec.calls[r.connID] = survivorPlatformID
	return r.rec.err
}

func (rec *refreshRecorder) assertCalled(t *testing.T, connID, wantID string) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	got, ok := rec.calls[connID]
	if !ok {
		t.Errorf("connection %s: expected a refresh call, got none", connID)
		return
	}
	if got != wantID {
		t.Errorf("connection %s: refreshed with survivor id %q, want %q", connID, got, wantID)
	}
}

func (rec *refreshRecorder) assertNoCall(t *testing.T, connID string) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if _, ok := rec.calls[connID]; ok {
		t.Errorf("connection %s: expected no refresh call, but one was recorded", connID)
	}
}

// swapMergeRefresherFactory overrides the package-level factory for the
// duration of the test, restoring it on cleanup. Mirrors how the rename-sync
// tests swap renamePathUpdaterFactory. Not parallel-safe (the var is global).
func swapMergeRefresherFactory(t *testing.T, f func(*connection.Connection, *slog.Logger) (mergeRefresher, bool)) {
	t.Helper()
	orig := mergeRefresherFactory
	mergeRefresherFactory = f
	t.Cleanup(func() { mergeRefresherFactory = orig })
}

func assertRefreshResult(t *testing.T, got []artist.PlatformRefreshResult, connID, want string) {
	t.Helper()
	for _, r := range got {
		if r.ConnectionID == connID {
			if r.Result != want {
				t.Errorf("connection %s: Result = %q (err=%q), want %q", connID, r.Result, r.Error, want)
			}
			return
		}
	}
	t.Errorf("connection %s: no result entry found in %+v", connID, got)
}

// TestSyncMergeRefresh_LibraryScanAndArtistRefresh is the happy-path fan-out:
// a survivor mapped on Emby + Lidarr, with a Jellyfin connection that only a
// loser mapped (survivor unmapped there) and a disabled Emby connection.
// Every enabled connection reconciles OK; the disabled one is skipped OK with
// no factory call; the survivor's per-connection platform ID reaches the
// refresher (empty for the connection where the survivor is unmapped).
func TestSyncMergeRefresh_LibraryScanAndArtistRefresh(t *testing.T) {
	rec := &refreshRecorder{}
	swapMergeRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (mergeRefresher, bool) {
		return rec.forConn(conn), true
	})

	p := New(Deps{
		// GetPlatformIDs ignores the artist ID in the fake and returns this
		// flat slice: the survivor is mapped on conn-emby and conn-lidarr only.
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "survivor-1", ConnectionID: "conn-emby", PlatformArtistID: "emby-1"},
			{ArtistID: "survivor-1", ConnectionID: "conn-lidarr", PlatformArtistID: "42"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"conn-emby":   {ID: "conn-emby", Type: connection.TypeEmby, Enabled: true, Name: "Emby"},
			"conn-jf":     {ID: "conn-jf", Type: connection.TypeJellyfin, Enabled: true, Name: "JF"},
			"conn-lidarr": {ID: "conn-lidarr", Type: connection.TypeLidarr, Enabled: true, Name: "Lidarr"},
			"conn-off":    {ID: "conn-off", Type: connection.TypeEmby, Enabled: false, Name: "Off"},
		}},
		Logger: silentLogger(),
	})

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1",
		[]string{"conn-emby", "conn-jf", "conn-lidarr", "conn-off"})
	if err != nil {
		t.Fatalf("SyncMergeRefresh: %v", err)
	}
	assertRefreshResult(t, got, "conn-emby", artist.PlatformRemapOK)
	assertRefreshResult(t, got, "conn-jf", artist.PlatformRemapOK)
	assertRefreshResult(t, got, "conn-lidarr", artist.PlatformRemapOK)
	assertRefreshResult(t, got, "conn-off", artist.PlatformRemapOK)

	// Emby got the survivor's mapped ID; JF got "" (survivor unmapped there but
	// still reconciled to evict the loser); Lidarr got the numeric survivor ID.
	rec.assertCalled(t, "conn-emby", "emby-1")
	rec.assertCalled(t, "conn-jf", "")
	rec.assertCalled(t, "conn-lidarr", "42")
	// Disabled connection: short-circuited to OK before the factory.
	rec.assertNoCall(t, "conn-off")
}

// TestSyncMergeRefresh_PrimitiveFailureIsPerConnection is the best-effort
// guard: one connection's refresh error must land in that entry as failed
// without stopping the fan-out or bubbling an outer error.
func TestSyncMergeRefresh_PrimitiveFailureIsPerConnection(t *testing.T) {
	swapMergeRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (mergeRefresher, bool) {
		if conn.ID == "conn-bad" {
			return &recordingRefresher{rec: &refreshRecorder{err: errors.New("peer 500")}, connID: conn.ID}, true
		}
		return &recordingRefresher{rec: &refreshRecorder{}, connID: conn.ID}, true
	})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"conn-bad": {ID: "conn-bad", Type: connection.TypeEmby, Enabled: true, Name: "Bad"},
			"conn-ok":  {ID: "conn-ok", Type: connection.TypeJellyfin, Enabled: true, Name: "OK"},
		}},
		Logger: silentLogger(),
	})

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-bad", "conn-ok"})
	if err != nil {
		t.Fatalf("SyncMergeRefresh: %v", err)
	}
	assertRefreshResult(t, got, "conn-bad", artist.PlatformRemapFailed)
	assertRefreshResult(t, got, "conn-ok", artist.PlatformRemapOK)
	for _, r := range got {
		if r.ConnectionID == "conn-bad" && r.Error == "" {
			t.Error("conn-bad: Error empty, want the wrapped peer error")
		}
	}
}

// TestSyncMergeRefresh_ConnectionFetchError: a connection ID with no row in
// the connections table surfaces as failed with a "fetching connection" error,
// and the fan-out continues to the remaining connections.
func TestSyncMergeRefresh_ConnectionFetchError(t *testing.T) {
	swapMergeRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (mergeRefresher, bool) {
		return &recordingRefresher{rec: &refreshRecorder{}, connID: conn.ID}, true
	})

	p := New(Deps{
		ArtistService:     &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-missing"})
	if err != nil {
		t.Fatalf("SyncMergeRefresh: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("results: got %d, want 1", len(got))
	}
	if got[0].Result != artist.PlatformRemapFailed {
		t.Errorf("Result = %q, want failed for missing connection", got[0].Result)
	}
	if !strings.Contains(got[0].Error, "fetching connection") {
		t.Errorf("Error = %q, want mention of the failed step", got[0].Error)
	}
}

// TestSyncMergeRefresh_UnsupportedConnectionType covers the factory-miss
// branch: a connection type no factory knows about records failed with an
// error naming the type, so a future connection type that forgets to extend
// the factory is caught here rather than silently passing.
func TestSyncMergeRefresh_UnsupportedConnectionType(t *testing.T) {
	swapMergeRefresherFactory(t, func(_ *connection.Connection, _ *slog.Logger) (mergeRefresher, bool) {
		return nil, false
	})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"conn-kodi": {ID: "conn-kodi", Type: "kodi", Enabled: true, Name: "Kodi"},
		}},
		Logger: silentLogger(),
	})

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-kodi"})
	if err != nil {
		t.Fatalf("SyncMergeRefresh: %v", err)
	}
	if got[0].Result != artist.PlatformRemapFailed {
		t.Errorf("Result = %q, want failed for unsupported type", got[0].Result)
	}
	if !strings.Contains(got[0].Error, "does not support") {
		t.Errorf("Error = %q, want mention of unsupported type", got[0].Error)
	}
}

// TestSyncMergeRefresh_NoConnections covers the early return when the affected
// set is empty: nil slice, nil error, no lookups.
func TestSyncMergeRefresh_NoConnections(t *testing.T) {
	p := New(Deps{
		ArtistService:     &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", nil)
	if err != nil {
		t.Fatalf("SyncMergeRefresh: %v", err)
	}
	if got != nil {
		t.Errorf("results = %v, want nil for empty connection set", got)
	}
}

// TestSyncMergeRefresh_NilPublisher covers the nil-receiver guard: a typed nil
// publisher returns cleanly instead of panicking.
func TestSyncMergeRefresh_NilPublisher(t *testing.T) {
	var p *Publisher
	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-emby"})
	if err != nil {
		t.Fatalf("SyncMergeRefresh on nil publisher: %v", err)
	}
	if got != nil {
		t.Errorf("results = %v, want nil from nil publisher", got)
	}
}

// TestSyncMergeRefresh_SurvivorIDLookupFailureStillReconciles verifies the
// non-fatal survivor-ID enumeration branch: when GetPlatformIDs fails, the
// fan-out still reconciles every connection (Emby/Jellyfin library scan needs
// no ID; Lidarr no-ops without one) rather than aborting.
func TestSyncMergeRefresh_SurvivorIDLookupFailureStillReconciles(t *testing.T) {
	rec := &refreshRecorder{}
	swapMergeRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (mergeRefresher, bool) {
		return rec.forConn(conn), true
	})

	p := New(Deps{
		ArtistService: &errPlatformLister{}, // GetPlatformIDs returns an error
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"conn-emby": {ID: "conn-emby", Type: connection.TypeEmby, Enabled: true, Name: "Emby"},
		}},
		Logger: silentLogger(),
	})

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-emby"})
	if err != nil {
		t.Fatalf("SyncMergeRefresh: %v", err)
	}
	assertRefreshResult(t, got, "conn-emby", artist.PlatformRemapOK)
	// Survivor ID unavailable -> empty ID passed, but the connection still
	// reconciles (library scan evicts the loser regardless).
	rec.assertCalled(t, "conn-emby", "")
}

// blockingRefresher holds RefreshAfterMerge open until ctx is canceled, so the
// deadline test can exercise the per-connection timeout branch.
type blockingRefresher struct{}

func (blockingRefresher) RefreshAfterMerge(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestSyncMergeRefresh_PerConnectionTimeoutFires guards mergeRefreshTimeout
// from a silent lowering that would break healthy peers: swap it short, rig a
// refresher that blocks on the context, and assert the result lands failed
// with a deadline error.
func TestSyncMergeRefresh_PerConnectionTimeoutFires(t *testing.T) {
	orig := mergeRefreshTimeout
	mergeRefreshTimeout = 50 * time.Millisecond
	t.Cleanup(func() { mergeRefreshTimeout = orig })

	swapMergeRefresherFactory(t, func(_ *connection.Connection, _ *slog.Logger) (mergeRefresher, bool) {
		return blockingRefresher{}, true
	})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"conn-slow": {ID: "conn-slow", Type: connection.TypeEmby, Enabled: true, Name: "Slow"},
		}},
		Logger: silentLogger(),
	})

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-slow"})
	if err != nil {
		t.Fatalf("SyncMergeRefresh: %v", err)
	}
	if got[0].Result != artist.PlatformRemapFailed {
		t.Errorf("Result = %q, want failed after deadline", got[0].Result)
	}
	if !strings.Contains(got[0].Error, "deadline") {
		t.Errorf("Error = %q, want substring \"deadline\"", got[0].Error)
	}
}

// TestSyncMergeRefresh_FactoryProductionDispatch exercises the real
// mergeRefresherFactory (not a swap) by pointing live connections at a closed
// httptest server: each supported type must dispatch to a non-nil refresher
// whose HTTP call then fails fast, landing a failed result with a diagnostic.
// Keeps the production factory + adapter bodies covered without real peers.
func TestSyncMergeRefresh_FactoryProductionDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close()

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "survivor-1", ConnectionID: "conn-lidarr", PlatformArtistID: "42"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"conn-emby":   {ID: "conn-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Name: "Emby", Emby: &connection.EmbyConfig{PlatformUserID: "u1"}},
			"conn-jf":     {ID: "conn-jf", Type: connection.TypeJellyfin, URL: srv.URL, Enabled: true, Name: "JF", Jellyfin: &connection.JellyfinConfig{PlatformUserID: "u1"}},
			"conn-lidarr": {ID: "conn-lidarr", Type: connection.TypeLidarr, URL: srv.URL, Enabled: true, Name: "Lidarr"},
		}},
		Logger: silentLogger(),
	})

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1",
		[]string{"conn-emby", "conn-jf", "conn-lidarr"})
	if err != nil {
		t.Fatalf("SyncMergeRefresh: %v", err)
	}
	for _, r := range got {
		if r.Result != artist.PlatformRemapFailed {
			t.Errorf("connection %s: Result = %q, want failed (dead server)", r.ConnectionID, r.Result)
		}
		if r.Error == "" {
			t.Errorf("connection %s: Error empty, want non-empty diagnostic", r.ConnectionID)
		}
	}
}

// TestLidarrRefresher_NonNumericID covers the Lidarr adapter's guard that a
// non-numeric survivor platform ID is a hard error (Lidarr artist IDs are
// integers), not a silent no-op.
func TestLidarrRefresher_NonNumericID(t *testing.T) {
	r := lidarrRefresher{c: nil} // c is unused: the numeric parse fails first.
	err := r.RefreshAfterMerge(context.Background(), "not-a-number")
	if err == nil {
		t.Fatal("RefreshAfterMerge with non-numeric id: got nil, want error")
	}
	if !strings.Contains(err.Error(), "not numeric") {
		t.Errorf("error = %q, want mention of non-numeric id", err.Error())
	}
}

// TestLidarrRefresher_UnmappedNoOps covers the Lidarr no-op branch: an empty
// survivor platform ID (survivor not mapped on this Lidarr connection) returns
// nil without touching the client.
func TestLidarrRefresher_UnmappedNoOps(t *testing.T) {
	r := lidarrRefresher{c: nil} // c is unused: the empty-id guard returns first.
	if err := r.RefreshAfterMerge(context.Background(), ""); err != nil {
		t.Errorf("RefreshAfterMerge with empty id: got %v, want nil no-op", err)
	}
}
