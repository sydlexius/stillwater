package publish

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// fakePathUpdater records the calls SyncRename routes to it and can be
// rigged to fail. Lets the rename-sync tests cover both ok and failed
// branches without standing up real HTTP fixtures for each platform type;
// the actual emby/jellyfin/lidarr per-client HTTP marshaling is covered
// in their own packages.
type fakePathUpdater struct {
	called  int
	gotID   string
	gotPath string
	err     error
}

func (f *fakePathUpdater) UpdateArtistPath(_ context.Context, platformArtistID, newPath string) error {
	f.called++
	f.gotID = platformArtistID
	f.gotPath = newPath
	return f.err
}

// withFakePathUpdater swaps renamePathUpdaterFactory for the duration of a
// test. The factory is a package-level var so a test can inject a single
// fake for every connection by capturing it in the closure. Restoring on
// cleanup keeps tests parallel-safe relative to one another only when they
// do not actually run in parallel; the rename-sync tests serialize.
func withFakePathUpdater(t *testing.T, fake *fakePathUpdater) {
	t.Helper()
	orig := renamePathUpdaterFactory
	renamePathUpdaterFactory = func(_ *connection.Connection, _ *slog.Logger) (pathUpdater, bool) {
		return fake, true
	}
	t.Cleanup(func() { renamePathUpdaterFactory = orig })
	// The pre-flight root guard (#2380) is fail-closed, so a test that only
	// stubs the updater would have every push refused before reaching it.
	// Install the roots this test corpus actually pushes into ("/music" for the
	// shared-mount cases, "/data..." for the mapped ones) so the guard genuinely
	// PASSES rather than being bypassed -- the guard's own refusal branches get
	// dedicated tests below.
	withFakeRootLister(t, fakeRootLister{roots: []string{"/music", "/data", "/data/media", "/new", "/old"}})
	// The post-update read-back verifier (#2380) is likewise unconditional, so a
	// test that stubs only the updater would fall through to a REAL HTTP client
	// aimed at a bogus host. Install a peer that HONORS the path write, which is
	// what these pre-existing cases were implicitly assuming when they asserted
	// "ok" off a fake updater's nil return. The peers that LIE about the write
	// (Emby, Jellyfin -- both proven live) get dedicated tests in relink_test.go.
	withFakePeer(t, &fakePeer{honorsPath: true, updater: fake,
		roots: []string{"/music", "/data", "/data/media", "/new", "/old"}})
}

// fakePeer models a peer's item store rather than a single endpoint's status
// code. That distinction is the whole lesson of #2380: a fake that answers 204
// and echoes nothing is NOT a peer, and a test built on one asserts only that
// Stillwater SENT something -- never that the peer HONORED it. So this double
// stores state and can be told to LIE the way the real media servers do.
//
// honorsPath=false reproduces the proven Emby/Jellyfin behavior: accept the
// write, report success, discard the path.
type fakePeer struct {
	// honorsPath: when false, UpdateArtistPath succeeds but the stored path is
	// left untouched -- exactly what Jellyfin 10.11.10 and Emby 4.9.5.0 do.
	honorsPath bool
	// updater, when set, is the fakePathUpdater whose last-sent path this peer
	// echoes back on a read. Only consulted when honorsPath is true.
	updater *fakePathUpdater
	// storedPath is the peer's own current path for the linked item.
	storedPath string
	// items is what ListLibraryArtists reports.
	items []connection.PeerArtist
	roots []string
	// onScan mutates the peer when a library scan is triggered, modeling the
	// ASYNCHRONOUS scanner: the moved directory only becomes visible after it runs.
	onScan   func(p *fakePeer)
	scans    int32
	lists    int32
	readErr  error
	listErr  error
	rootsErr error
	scanErr  error
}

func (f *fakePeer) GetArtistPath(_ context.Context, _ string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	if f.honorsPath && f.updater != nil {
		return f.updater.gotPath, nil
	}
	return f.storedPath, nil
}

func (f *fakePeer) ListLibraryArtists(_ context.Context) ([]connection.PeerArtist, error) {
	atomic.AddInt32(&f.lists, 1)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.items, nil
}

func (f *fakePeer) ListRoots(_ context.Context) ([]string, error) {
	if f.rootsErr != nil {
		return nil, f.rootsErr
	}
	return f.roots, nil
}

func (f *fakePeer) TriggerLibraryScan(_ context.Context) error {
	atomic.AddInt32(&f.scans, 1)
	if f.scanErr != nil {
		return f.scanErr
	}
	if f.onScan != nil {
		f.onScan(f)
	}
	return nil
}

// withFakePeer swaps relinkResolverFactory for the duration of a test.
func withFakePeer(t *testing.T, peer *fakePeer) {
	t.Helper()
	orig := relinkResolverFactory
	relinkResolverFactory = func(_ *connection.Connection, _ *slog.Logger) (peerArtistResolver, bool) {
		return peer, true
	}
	t.Cleanup(func() { relinkResolverFactory = orig })
}

// fakeRootLister is a test double for the rootLister seam: it reports a fixed
// set of peer root folders, or a fixed error (to drive the guard's fail-closed
// "cannot verify" branch).
type fakeRootLister struct {
	roots  []string
	err    error
	called int32
}

func (f *fakeRootLister) ListRoots(context.Context) ([]string, error) {
	atomic.AddInt32(&f.called, 1)
	return f.roots, f.err
}

// withFakeRootLister swaps renameRootListerFactory for the duration of a test.
// Passing a fakeRootLister value (not pointer) is fine for the read-only cases;
// the pointer form lets a test assert the guard actually consulted the peer.
func withFakeRootLister(t *testing.T, lister fakeRootLister) {
	t.Helper()
	orig := renameRootListerFactory
	l := &lister
	renameRootListerFactory = func(_ *connection.Connection, _ *slog.Logger) (rootLister, bool) {
		return l, true
	}
	t.Cleanup(func() { renameRootListerFactory = orig })
}

// TestSyncRename_AllPlatformsOK is the happy path: three connections
// (Emby+Jellyfin+Lidarr) each return ok. The handler test
// (handlers_rename_directory_test.go) covers the empty-mappings case
// separately so we focus here on the multi-platform fan-out.
func TestSyncRename_AllPlatformsOK(t *testing.T) {
	fake := &fakePathUpdater{}
	withFakePathUpdater(t, fake)

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p-emby"},
			{ArtistID: "a1", ConnectionID: "c-jf", PlatformArtistID: "p-jf"},
			{ArtistID: "a1", ConnectionID: "c-lid", PlatformArtistID: "42"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "emby", Type: connection.TypeEmby, URL: "http://emby", Enabled: true},
			"c-jf":   {ID: "c-jf", Name: "jf", Type: connection.TypeJellyfin, URL: "http://jf", Enabled: true},
			"c-lid":  {ID: "c-lid", Name: "lid", Type: connection.TypeLidarr, URL: "http://lid", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results: got %d, want 3", len(results))
	}
	for _, r := range results {
		if r.Result != artist.PlatformRemapOK {
			t.Errorf("connection %s: got %q (err=%q), want ok", r.ConnectionID, r.Result, r.Error)
		}
	}
	if fake.called != 3 {
		t.Errorf("fake called %d times, want 3", fake.called)
	}
}

// TestSyncRename_PartialFailureDoesNotStopFanout is the regression guard for
// the per-platform best-effort contract from #1222: one platform's HTTP
// failure must NOT block the remaining platforms or roll back anything.
// The error string lands inside the failed entry; the loop keeps going.
func TestSyncRename_PartialFailureDoesNotStopFanout(t *testing.T) {
	// Per-connection fakes so we can rig one to fail while the other
	// succeeds. The factory dispatches by conn.ID rather than conn.Type
	// because Type alone is not unique across the test connections.
	okFake := &fakePathUpdater{}
	failFake := &fakePathUpdater{err: errors.New("simulated 500 from peer")}

	orig := renamePathUpdaterFactory
	renamePathUpdaterFactory = func(c *connection.Connection, _ *slog.Logger) (pathUpdater, bool) {
		if c.ID == "c-fail" {
			return failFake, true
		}
		return okFake, true
	}
	t.Cleanup(func() { renamePathUpdaterFactory = orig })
	withFakeRootLister(t, fakeRootLister{roots: []string{"/music", "/data", "/data/media", "/new", "/old"}})
	// Only c-ok reaches the read-back (c-fail errors out at the update), so a peer
	// echoing okFake's sent path is the right double for the verify step here.
	withFakePeer(t, &fakePeer{honorsPath: true, updater: okFake,
		roots: []string{"/music", "/data", "/data/media", "/new", "/old"}})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-fail", PlatformArtistID: "p1"},
			{ArtistID: "a1", ConnectionID: "c-ok", PlatformArtistID: "p2"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-fail": {ID: "c-fail", Name: "fail", Type: connection.TypeEmby, URL: "http://fail", Enabled: true},
			"c-ok":   {ID: "c-ok", Name: "ok", Type: connection.TypeJellyfin, URL: "http://ok", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results: got %d, want 2 (failure must not short-circuit)", len(results))
	}
	byConn := map[string]artist.PlatformRemapResult{}
	for _, r := range results {
		byConn[r.ConnectionID] = r
	}
	if got := byConn["c-fail"].Result; got != artist.PlatformRemapFailed {
		t.Errorf("c-fail Result = %q, want failed", got)
	}
	if byConn["c-fail"].Error == "" {
		t.Error("c-fail Error empty; expected wrapped peer error string")
	}
	if got := byConn["c-ok"].Result; got != artist.PlatformRemapOK {
		t.Errorf("c-ok Result = %q, want ok (failure on c-fail must not skip it)", got)
	}
	if okFake.called != 1 {
		t.Errorf("c-ok updater called %d times, want 1", okFake.called)
	}
}

// TestSyncRename_AppliesPathMapping is the #2303 guard: a Lidarr connection
// with a configured PathMapping must send the platform-namespace path to
// UpdateArtistPath, while a connection with no mapping sends the host path
// verbatim. The translation happens in syncOne, independent of the per-client
// factory, so a single fake covers both connections.
func TestSyncRename_AppliesPathMapping(t *testing.T) {
	fake := &fakePathUpdater{}

	orig := renamePathUpdaterFactory
	renamePathUpdaterFactory = func(_ *connection.Connection, _ *slog.Logger) (pathUpdater, bool) {
		return fake, true
	}
	t.Cleanup(func() { renamePathUpdaterFactory = orig })
	withFakeRootLister(t, fakeRootLister{roots: []string{"/music", "/data", "/data/media", "/new", "/old"}})
	withFakePeer(t, &fakePeer{honorsPath: true, updater: fake,
		roots: []string{"/music", "/data", "/data/media", "/new", "/old"}})

	// Mapped connection: /music -> /data/media.
	mapped := &connection.Connection{
		ID: "c-mapped", Name: "mapped", Type: connection.TypeLidarr, URL: "http://lid", Enabled: true,
		PathMappings: []connection.PathMapping{
			{HostPrefix: "/music", PlatformPrefix: "/data/media"},
		},
	}
	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-mapped", PlatformArtistID: "42"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-mapped": mapped}},
		Logger:            silentLogger(),
	})
	if _, err := p.SyncRename(context.Background(), "a1", "/music/OldName", "/music/NewName"); err != nil {
		t.Fatalf("SyncRename (mapped): %v", err)
	}
	if fake.gotPath != "/data/media/NewName" {
		t.Errorf("mapped path sent = %q, want %q", fake.gotPath, "/data/media/NewName")
	}

	// Unmapped connection: same host path must reach the platform verbatim.
	fake2 := &fakePathUpdater{}
	renamePathUpdaterFactory = func(_ *connection.Connection, _ *slog.Logger) (pathUpdater, bool) {
		return fake2, true
	}
	withFakePeer(t, &fakePeer{honorsPath: true, updater: fake2,
		roots: []string{"/music", "/data", "/data/media", "/new", "/old"}})
	unmapped := &connection.Connection{
		ID: "c-plain", Name: "plain", Type: connection.TypeLidarr, URL: "http://lid2", Enabled: true,
		Lidarr: &connection.LidarrConfig{},
	}
	p2 := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-plain", PlatformArtistID: "43"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c-plain": unmapped}},
		Logger:            silentLogger(),
	})
	if _, err := p2.SyncRename(context.Background(), "a1", "/music/OldName", "/music/NewName"); err != nil {
		t.Fatalf("SyncRename (unmapped): %v", err)
	}
	if fake2.gotPath != "/music/NewName" {
		t.Errorf("unmapped path sent = %q, want verbatim %q", fake2.gotPath, "/music/NewName")
	}
}

// TestSyncRename_NoPlatformIDs covers the early-return when the artist has
// no mappings: nil slice, nil error, no factory invocation.
func TestSyncRename_NoPlatformIDs(t *testing.T) {
	fake := &fakePathUpdater{}
	withFakePathUpdater(t, fake)

	p := New(Deps{
		ArtistService:     &fakePlatformLister{ids: nil},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if results != nil {
		t.Errorf("results = %v, want nil for no-mappings case", results)
	}
	if fake.called != 0 {
		t.Errorf("updater called %d times, want 0", fake.called)
	}
}

// TestSyncRename_ConnectionFetchError covers the GetByID-failure branch:
// when the connections table is missing an ID the platform_ids row
// references, the per-platform result must surface the lookup error
// without panicking or short-circuiting the rest of the fan-out.
func TestSyncRename_ConnectionFetchError(t *testing.T) {
	fake := &fakePathUpdater{}
	withFakePathUpdater(t, fake)

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-missing", PlatformArtistID: "p1"},
		}},
		// fakeConnectionGetter returns "no connection <id>" when the map
		// lookup misses; the empty map produces that error for c-missing.
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results: got %d, want 1", len(results))
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Errorf("Result = %q, want failed for missing-connection branch", results[0].Result)
	}
	if !strings.Contains(results[0].Error, "fetching connection") {
		t.Errorf("Error = %q, want wrap mentioning the failed step", results[0].Error)
	}
	if fake.called != 0 {
		t.Errorf("updater called %d times when connection lookup failed, want 0", fake.called)
	}
}

// TestSyncRename_UnsupportedConnectionType covers the factory miss branch:
// a connection type that no factory knows about (here, a made-up "kodi"
// string) must record Result=failed with an error mentioning the type, so
// a future connection-type addition that forgets to extend the factory is
// caught in the rename response instead of silently passing.
func TestSyncRename_UnsupportedConnectionType(t *testing.T) {
	// Override the factory to return (nil, false) so this test exercises
	// the production miss branch instead of standing up a real new type.
	orig := renamePathUpdaterFactory
	renamePathUpdaterFactory = func(_ *connection.Connection, _ *slog.Logger) (pathUpdater, bool) {
		return nil, false
	}
	t.Cleanup(func() { renamePathUpdaterFactory = orig })
	withFakeRootLister(t, fakeRootLister{roots: []string{"/music", "/data", "/data/media", "/new", "/old"}})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-mystery", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-mystery": {ID: "c-mystery", Name: "kodi", Type: "kodi", URL: "http://k", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results: got %d, want 1", len(results))
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Errorf("Result = %q, want failed for unsupported type", results[0].Result)
	}
	if !strings.Contains(results[0].Error, "does not support") {
		t.Errorf("Error = %q, want mention of unsupported type", results[0].Error)
	}
}

// TestSyncRename_EnumerationFailureInBand verifies that when the artist
// service's GetPlatformIDs lookup itself fails (e.g. DB read error),
// SyncRename surfaces the failure IN BAND via a synthesized
// PlatformRemapResult rather than returning a nil slice + non-nil error.
// An empty slice is indistinguishable from "no platforms exist" in the
// HTTP response, so the in-band signal is what lets the user see that the
// rename succeeded but the post-rename platform reconciliation could not
// even start. ConnectionID is empty to mark the synthesized entry; the
// Error string names the failed step ("platform mappings unavailable").
func TestSyncRename_EnumerationFailureInBand(t *testing.T) {
	withFakePathUpdater(t, &fakePathUpdater{})

	p := New(Deps{
		ArtistService:     &errPlatformLister{},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{}},
		Logger:            silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename: want nil outer error (failure surfaces in-band), got %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results: got %d, want 1 synthesized entry for enumeration failure", len(results))
	}
	if results[0].ConnectionID != "" {
		t.Errorf("ConnectionID = %q, want empty (sentinel for synthesized enumeration-failure entry)", results[0].ConnectionID)
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Errorf("Result = %q, want failed", results[0].Result)
	}
	if !strings.Contains(results[0].Error, "platform mappings unavailable") {
		t.Errorf("Error = %q, want contains \"platform mappings unavailable\"", results[0].Error)
	}
}

// TestSyncRename_NilPublisher covers the early-return guard at the top of
// SyncRename: callers (artist.Service) check the syncer for nil but the
// method itself also self-guards, so a future caller that passes a typed
// nil does not panic. Method on a nil pointer is intentional here -- Go
// allows it as long as the body returns before any field access.
func TestSyncRename_NilPublisher(t *testing.T) {
	var p *Publisher
	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename on nil publisher: %v", err)
	}
	if results != nil {
		t.Errorf("results = %v, want nil from nil publisher", results)
	}
}

// TestSyncRename_FactoryProductionDispatch exercises the production
// renamePathUpdaterFactory (not the fake) by pointing a real connection at
// a closed httptest server. The factory must hand back a non-nil updater
// for each of the three supported types; the HTTP call then fails fast on
// the closed URL, producing a failed result with a meaningful error. This
// keeps the production factory body covered without needing real Emby /
// Jellyfin / Lidarr peers in the test environment.
func TestSyncRename_FactoryProductionDispatch(t *testing.T) {
	// Run a server then close it so any HTTP request to its URL fails
	// immediately with "connection refused". Closed URLs are how we drive
	// the GET-error branch in each per-client UpdateArtistPath from the
	// rename-sync orchestrator's vantage point.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close()

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
			{ArtistID: "a1", ConnectionID: "c-jf", PlatformArtistID: "p2"},
			{ArtistID: "a1", ConnectionID: "c-lid", PlatformArtistID: "42"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Emby: &connection.EmbyConfig{PlatformUserID: "u1"}},
			"c-jf":   {ID: "c-jf", Name: "jf", Type: connection.TypeJellyfin, URL: srv.URL, Enabled: true, Jellyfin: &connection.JellyfinConfig{PlatformUserID: "u1"}},
			"c-lid":  {ID: "c-lid", Name: "lid", Type: connection.TypeLidarr, URL: srv.URL, Enabled: true},
		}},
		Logger: silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results: got %d, want 3 (one per platform mapping)", len(results))
	}
	// Each platform should land in failed (the server is dead) but with a
	// non-empty error string sourced from its own client's wrap.
	for _, r := range results {
		if r.Result != artist.PlatformRemapFailed {
			t.Errorf("connection %s: Result = %q, want failed", r.ConnectionID, r.Result)
		}
		if r.Error == "" {
			t.Errorf("connection %s: Error empty, want non-empty diagnostic", r.ConnectionID)
		}
	}
}

// TestSyncRename_DisabledConnectionSkipped: a disabled connection records
// Result=ok with an "disabled" note instead of attempting the HTTP call.
// Mirrors PushLocks' disabled-connection semantics so a user opting a peer
// out via the Enabled flag does not surface a noisy rename failure.
func TestSyncRename_DisabledConnectionSkipped(t *testing.T) {
	fake := &fakePathUpdater{}
	withFakePathUpdater(t, fake)

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-off", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-off": {ID: "c-off", Name: "off", Type: connection.TypeEmby, URL: "http://off", Enabled: false},
		}},
		Logger: silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results: got %d, want 1", len(results))
	}
	if results[0].Result != artist.PlatformRemapOK {
		t.Errorf("Result = %q, want ok for disabled connection", results[0].Result)
	}
	// OpenAPI documents `error` as present only when result is "failed";
	// the disabled-skip path therefore must not populate it. Asserting on
	// the struct field is sufficient because PlatformRemapResult.Error has
	// `omitempty` on its JSON tag, so empty here implies absent in the
	// rendered response.
	if results[0].Error != "" {
		t.Errorf("Error = %q, want empty for disabled connection (OpenAPI: error present only when result=failed)", results[0].Error)
	}
	if fake.called != 0 {
		t.Errorf("updater called %d times on disabled connection, want 0", fake.called)
	}
}

// blockingPathUpdater holds UpdateArtistPath open until ctx is canceled.
// Lets the deadline test exercise the per-platform timeout branch without
// depending on real HTTP behavior or network conditions.
type blockingPathUpdater struct{}

func (blockingPathUpdater) UpdateArtistPath(ctx context.Context, _, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestSyncRename_PerPlatformTimeoutFires guards the renameSyncTimeout
// constant from a "perf tweak" silently lowering it to a value that breaks
// healthy peers. We swap the package-level renameSyncTimeout var down to a
// short duration, rig an updater that blocks on the context, and assert the
// per-platform result lands in failed with an error mentioning the deadline.
// Without this test the timeout has no behavioral coverage and a future
// edit to the constant would only be caught by manual UAT.
func TestSyncRename_PerPlatformTimeoutFires(t *testing.T) {
	// Restore the production timeout after the test so siblings keep their
	// real-world wait semantics. Not parallel: the swap is package-global.
	origTimeout := renameSyncTimeout
	renameSyncTimeout = 50 * time.Millisecond
	t.Cleanup(func() { renameSyncTimeout = origTimeout })

	orig := renamePathUpdaterFactory
	renamePathUpdaterFactory = func(_ *connection.Connection, _ *slog.Logger) (pathUpdater, bool) {
		return blockingPathUpdater{}, true
	}
	t.Cleanup(func() { renamePathUpdaterFactory = orig })
	withFakeRootLister(t, fakeRootLister{roots: []string{"/music", "/data", "/data/media", "/new", "/old"}})

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-slow", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-slow": {ID: "c-slow", Name: "slow", Type: connection.TypeEmby, URL: "http://slow", Enabled: true},
		}},
		Logger: silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results: got %d, want 1", len(results))
	}
	if results[0].Result != artist.PlatformRemapFailed {
		t.Errorf("Result = %q, want failed after deadline", results[0].Result)
	}
	// context.DeadlineExceeded stringifies as "context deadline exceeded"; we
	// accept either substring so the test does not depend on the precise
	// wrapping any future per-client wrapper might apply.
	if !strings.Contains(results[0].Error, "deadline") {
		t.Errorf("Error = %q, want substring \"deadline\" (context.DeadlineExceeded)", results[0].Error)
	}
}

// lidarrVerifyServer is a minimal httptest fixture that counts GET vs PUT
// hits against /api/v1/artist/{id}. Shared by the two wiring tests below so
// the only delta between them is the Connection.VerifyPathAfterUpdate value.
// Echoes the requested path on the verify GET so the match branch
// succeeds without each test needing to script per-request bodies.
func lidarrVerifyServer(t *testing.T, newPath string) (*httptest.Server, func() int) {
	t.Helper()
	// atomic.Int32 because the counter is written from the httptest
	// handler goroutine and read from the test goroutine; a plain int
	// trips -race under the project's race-test rule.
	var getCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The pre-flight root guard (#2380) reads /api/v1/rootfolder through the
		// REAL lidarr client on this production-dispatch path. Serve a root that
		// contains newPath so the guard passes, and do NOT count it: the GET
		// assertions below are about the artist-resource round-trips (pre-PUT +
		// verify), and folding the guard's read into that count would silently
		// change what the verify-wiring assertion means.
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/rootfolder" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":1,"path":"/new"}]`))
			return
		}
		switch r.Method {
		case http.MethodGet:
			n := getCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				_, _ = w.Write([]byte(`{"id":42,"path":"/old/X"}`))
				return
			}
			// Subsequent GET (the verify-after-PUT branch) echoes the new
			// path so the match check inside lidarr.Client succeeds.
			_, _ = w.Write([]byte(`{"id":42,"path":"` + newPath + `"}`))
		case http.MethodPut:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { return int(getCount.Load()) }
}

// TestSyncRename_LidarrVerifyWiringEnabled asserts the load-bearing wiring for
// #1640: when conn.VerifyPathAfterUpdate is true the real
// renamePathUpdaterFactory must call client.SetVerifyPathAfterUpdate(true), so
// lidarr.Client issues its own in-client verify GET. The factory is NOT swapped
// here so the test exercises the actual production path. Without this coverage
// the Connection field could be silently dropped on its way to the client and
// the toggle would be dead code.
//
// The expected GET count is 3 since #2380, not 2: pre-PUT + the in-client verify
// + the publish-layer read-back, which now runs on EVERY connection type
// unconditionally. See the Disabled twin for why that last one cannot be
// switched off any more.
func TestSyncRename_LidarrVerifyWiringEnabled(t *testing.T) {
	srv, gets := lidarrVerifyServer(t, "/new/X")

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-lid", PlatformArtistID: "42"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-lid": {
				ID:      "c-lid",
				Name:    "lid",
				Type:    connection.TypeLidarr,
				URL:     srv.URL,
				APIKey:  "k",
				Enabled: true,
				Lidarr:  &connection.LidarrConfig{VerifyPathAfterUpdate: true},
			},
		}},
		Logger: silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new/X")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 || results[0].Result != artist.PlatformRemapOK {
		t.Fatalf("results = %+v, want one ok entry", results)
	}
	if got := gets(); got != 3 {
		t.Errorf("GET count = %d, want 3 (pre-PUT + in-client verify + publish read-back); wiring did not reach client", got)
	}
}

// TestSyncRename_LidarrVerifyWiringDisabled is the negative-branch twin of the
// enabled wiring test, and since #2380 it guards the more important half of the
// contract: with conn.VerifyPathAfterUpdate FALSE the rename must still produce
// 2 GETs, because the pre-PUT fetch is followed by the publish-layer read-back
// that NO connection setting can turn off.
//
// The old expectation here was 1 GET -- "verify must remain opt-in". That is
// precisely the defect this issue exists to close. The only read-back in the
// codebase was Lidarr's, it defaulted to OFF, and Emby/Jellyfin had none at all,
// so the one mechanism that could have caught a peer silently discarding a path
// was disabled on the two peers that silently discard paths. A read-back is a
// correctness guard, not a preference; the toggle now only controls Lidarr's
// EXTRA in-client check (1 GET more, asserted by the twin above).
//
// A regression that makes the publish-layer verifier conditional again would
// surface here as a 1-GET observation.
func TestSyncRename_LidarrVerifyWiringDisabled(t *testing.T) {
	srv, gets := lidarrVerifyServer(t, "/new/X")

	p := New(Deps{
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-lid", PlatformArtistID: "42"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-lid": {
				ID:      "c-lid",
				Name:    "lid",
				Type:    connection.TypeLidarr,
				URL:     srv.URL,
				APIKey:  "k",
				Enabled: true,
				// VerifyPathAfterUpdate defaults to false.
			},
		}},
		Logger: silentLogger(),
	})

	results, err := p.SyncRename(context.Background(), "a1", "/old", "/new/X")
	if err != nil {
		t.Fatalf("SyncRename: %v", err)
	}
	if len(results) != 1 || results[0].Result != artist.PlatformRemapOK {
		t.Fatalf("results = %+v, want one ok entry", results)
	}
	if got := gets(); got != 2 {
		t.Errorf("GET count = %d, want 2 (pre-PUT + the publish-layer read-back, which is unconditional); "+
			"a count of 1 means the read-back became opt-in again -- the #2380 defect", got)
	}
}
