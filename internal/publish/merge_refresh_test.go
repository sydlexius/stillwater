package publish

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
)

// refreshRecorder captures the RefreshAfterMerge calls a swapped
// mergeRefresherFactory routes to it, keyed by connection ID. It lets the
// dispatch tests assert which connections were refreshed (and with which
// survivor platform ID) without standing up real Emby/Jellyfin/Lidarr peers;
// the production factory + per-client adapters are covered separately by
// TestSyncMergeRefresh_FactoryProductionDispatch.
type refreshRecorder struct {
	mu         sync.Mutex
	calls      map[string]string   // connID -> survivorPlatformID passed to RefreshAfterMerge
	loserCalls map[string][]string // connID -> loserPlatformIDs passed to RefreshAfterMerge
	err        error               // when non-nil, every recorded call returns it
}

func (rec *refreshRecorder) forConn(conn *connection.Connection) mergeRefresher {
	return &recordingRefresher{rec: rec, connID: conn.ID}
}

type recordingRefresher struct {
	rec    *refreshRecorder
	connID string
}

func (r *recordingRefresher) RefreshAfterMerge(_ context.Context, survivorPlatformID string, loserPlatformIDs []string) error {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	if r.rec.calls == nil {
		r.rec.calls = map[string]string{}
	}
	if r.rec.loserCalls == nil {
		r.rec.loserCalls = map[string][]string{}
	}
	r.rec.calls[r.connID] = survivorPlatformID
	r.rec.loserCalls[r.connID] = loserPlatformIDs
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
		[]string{"conn-emby", "conn-jf", "conn-lidarr", "conn-off"}, nil)
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

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-bad", "conn-ok"}, nil)
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

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-missing"}, nil)
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

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-kodi"}, nil)
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

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", nil, nil)
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
	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-emby"}, nil)
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

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-emby"}, nil)
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

func (blockingRefresher) RefreshAfterMerge(ctx context.Context, _ string, _ []string) error {
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

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1", []string{"conn-slow"}, nil)
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
		[]string{"conn-emby", "conn-jf", "conn-lidarr"}, nil)
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

// TestEmbyRefresher_ProductionScopesToSurvivorAndLoser exercises the real
// embyRefresher -> real emby.Client -> scopedMergeRefresh chain against an
// httptest server, recording which HTTP paths were hit. This is the
// production glue TestScopedMergeRefresh_* tests (which use a fake
// itemRescanner) don't cover: proves the real client's TriggerItemRescan is
// what RefreshAfterMerge actually calls, not just the abstraction.
func TestEmbyRefresher_ProductionScopesToSurvivorAndLoser(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := embyRefresher{c: emby.NewWithHTTPClient(srv.URL, "key", "", srv.Client(), silentLogger())}
	if err := r.RefreshAfterMerge(context.Background(), "emby-survivor", []string{"emby-loser"}); err != nil {
		t.Fatalf("RefreshAfterMerge: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), paths...)
	mu.Unlock()
	want := []string{"/Items/emby-survivor/Refresh", "/Items/emby-loser/Refresh"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paths hit = %v, want %v (scoped item rescans, no full /Library/Refresh)", got, want)
	}
}

// TestEmbyRefresher_ProductionUnmappedSurvivorFallsBackToFullScan is the
// production-glue counterpart of TestScopedMergeRefresh_UnmappedSurvivorFallsBackToFullScan
// (which uses a fake itemRescanner): with a real emby.Client, an unmapped
// survivor must still reach /Library/Refresh, the existing full-scan
// eviction/index guarantee, rather than silently skipping reconciliation.
func TestEmbyRefresher_ProductionUnmappedSurvivorFallsBackToFullScan(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := embyRefresher{c: emby.NewWithHTTPClient(srv.URL, "key", "", srv.Client(), silentLogger())}
	if err := r.RefreshAfterMerge(context.Background(), "", []string{"emby-loser"}); err != nil {
		t.Fatalf("RefreshAfterMerge: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), paths...)
	mu.Unlock()
	want := []string{"/Library/Refresh"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paths hit = %v, want %v (unmapped survivor falls back to a full scan, no scoped loser call)", got, want)
	}
}

// fakeItemRescanner is a scriptable itemRescanner recording every call
// scopedMergeRefresh makes, so the scoping tests can assert exactly which
// primitive (scoped item rescan vs. full library scan) was issued for the
// survivor and each loser without a real Emby/Jellyfin peer.
type fakeItemRescanner struct {
	rescanCalls   []string // itemIDs passed to TriggerItemRescan, in call order
	libraryScans  int
	failRescanIDs map[string]bool // itemID -> TriggerItemRescan returns an error
}

func (f *fakeItemRescanner) TriggerItemRescan(_ context.Context, itemID string) error {
	f.rescanCalls = append(f.rescanCalls, itemID)
	if f.failRescanIDs[itemID] {
		return errors.New("peer rejected rescan")
	}
	return nil
}

func (f *fakeItemRescanner) TriggerLibraryScan(_ context.Context) error {
	f.libraryScans++
	return nil
}

// TestScopedMergeRefresh_ScopesToSurvivorAndLosers is the core #2431 guard: a
// normal merge with a known survivor ID and known loser IDs on this
// connection must issue a scoped item rescan for the survivor plus each
// loser, and must NOT fall back to a full library scan. Reverting the
// scoping fix (making scopedMergeRefresh always call TriggerLibraryScan)
// turns this red: libraryScans would be 1 instead of 0 and rescanCalls empty.
func TestScopedMergeRefresh_ScopesToSurvivorAndLosers(t *testing.T) {
	f := &fakeItemRescanner{}
	err := scopedMergeRefresh(context.Background(), f, "survivor-emby-id", []string{"loser-a", "loser-b"})
	if err != nil {
		t.Fatalf("scopedMergeRefresh: %v", err)
	}
	if f.libraryScans != 0 {
		t.Errorf("libraryScans = %d, want 0 (a normal merge must not trigger a full scan)", f.libraryScans)
	}
	want := []string{"survivor-emby-id", "loser-a", "loser-b"}
	if !reflect.DeepEqual(f.rescanCalls, want) {
		t.Errorf("rescanCalls = %v, want %v (survivor first, then each loser)", f.rescanCalls, want)
	}
}

// TestScopedMergeRefresh_UnmappedSurvivorFallsBackToFullScan covers the
// documented fallback: when the survivor is not mapped on this connection
// there is nothing to scope the index-side refresh to, so the full scan
// still runs (and no loser rescans are attempted, since a full scan already
// covers eviction).
func TestScopedMergeRefresh_UnmappedSurvivorFallsBackToFullScan(t *testing.T) {
	f := &fakeItemRescanner{}
	err := scopedMergeRefresh(context.Background(), f, "", []string{"loser-a"})
	if err != nil {
		t.Fatalf("scopedMergeRefresh: %v", err)
	}
	if f.libraryScans != 1 {
		t.Errorf("libraryScans = %d, want 1 (unmapped survivor must fall back)", f.libraryScans)
	}
	if len(f.rescanCalls) != 0 {
		t.Errorf("rescanCalls = %v, want none (fallback skips scoped calls)", f.rescanCalls)
	}
}

// TestScopedMergeRefresh_SurvivorRescanFailureFallsBackToFullScan covers the
// other fallback branch: the survivor is mapped, but the scoped call itself
// fails (peer or network issue), so a full scan is the safety net.
func TestScopedMergeRefresh_SurvivorRescanFailureFallsBackToFullScan(t *testing.T) {
	f := &fakeItemRescanner{failRescanIDs: map[string]bool{"survivor-emby-id": true}}
	err := scopedMergeRefresh(context.Background(), f, "survivor-emby-id", []string{"loser-a"})
	if err != nil {
		t.Fatalf("scopedMergeRefresh: %v", err)
	}
	if f.libraryScans != 1 {
		t.Errorf("libraryScans = %d, want 1 (failed survivor rescan must fall back)", f.libraryScans)
	}
	if len(f.rescanCalls) != 1 || f.rescanCalls[0] != "survivor-emby-id" {
		t.Errorf("rescanCalls = %v, want only the failed survivor attempt (no loser calls after fallback)", f.rescanCalls)
	}
}

// TestScopedMergeRefresh_LoserRescanFailureSurfacesWithoutFullScanFallback
// covers a loser-only failure: the survivor rescan succeeded (so the
// connection's own index is healthy), but one loser rescan failed. This must
// surface as an error -- so the caller records the connection as failed
// rather than silently missing the eviction -- without retriggering a full
// scan (the survivor side already succeeded scoped).
func TestScopedMergeRefresh_LoserRescanFailureSurfacesWithoutFullScanFallback(t *testing.T) {
	f := &fakeItemRescanner{failRescanIDs: map[string]bool{"loser-b": true}}
	err := scopedMergeRefresh(context.Background(), f, "survivor-emby-id", []string{"loser-a", "loser-b"})
	if err == nil {
		t.Fatal("scopedMergeRefresh: got nil error, want a loser-rescan failure")
	}
	if !strings.Contains(err.Error(), "loser-b") {
		t.Errorf("error = %q, want mention of the failed loser item", err.Error())
	}
	if f.libraryScans != 0 {
		t.Errorf("libraryScans = %d, want 0 (survivor side succeeded scoped; no fallback needed)", f.libraryScans)
	}
}

// TestSyncMergeRefresh_LoserPlatformIDsRoutedPerConnection is the fan-out
// wiring guard: loserPlatformIDs is keyed by connection ID, and each
// connection's refresher must receive only ITS OWN loser IDs, not another
// connection's.
func TestSyncMergeRefresh_LoserPlatformIDsRoutedPerConnection(t *testing.T) {
	rec := &refreshRecorder{}
	swapMergeRefresherFactory(t, func(conn *connection.Connection, _ *slog.Logger) (mergeRefresher, bool) {
		return rec.forConn(conn), true
	})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"conn-emby": {ID: "conn-emby", Type: connection.TypeEmby, Enabled: true, Name: "Emby"},
			"conn-jf":   {ID: "conn-jf", Type: connection.TypeJellyfin, Enabled: true, Name: "JF"},
		}},
		Logger: silentLogger(),
	})

	got, err := p.SyncMergeRefresh(context.Background(), "survivor-1",
		[]string{"conn-emby", "conn-jf"},
		map[string][]string{"conn-emby": {"loser-emby-1"}, "conn-jf": {"loser-jf-1", "loser-jf-2"}})
	if err != nil {
		t.Fatalf("SyncMergeRefresh: %v", err)
	}
	assertRefreshResult(t, got, "conn-emby", artist.PlatformRemapOK)
	assertRefreshResult(t, got, "conn-jf", artist.PlatformRemapOK)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !reflect.DeepEqual(rec.loserCalls["conn-emby"], []string{"loser-emby-1"}) {
		t.Errorf("conn-emby loser IDs = %v, want [loser-emby-1]", rec.loserCalls["conn-emby"])
	}
	if !reflect.DeepEqual(rec.loserCalls["conn-jf"], []string{"loser-jf-1", "loser-jf-2"}) {
		t.Errorf("conn-jf loser IDs = %v, want [loser-jf-1 loser-jf-2]", rec.loserCalls["conn-jf"])
	}
}

// TestLidarrRefresher_NonNumericID covers the Lidarr adapter's guard that a
// non-numeric survivor platform ID is a hard error (Lidarr artist IDs are
// integers), not a silent no-op.
func TestLidarrRefresher_NonNumericID(t *testing.T) {
	r := lidarrRefresher{c: nil} // c is unused: the numeric parse fails first.
	err := r.RefreshAfterMerge(context.Background(), "not-a-number", nil)
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
	if err := r.RefreshAfterMerge(context.Background(), "", nil); err != nil {
		t.Errorf("RefreshAfterMerge with empty id: got %v, want nil no-op", err)
	}
}
