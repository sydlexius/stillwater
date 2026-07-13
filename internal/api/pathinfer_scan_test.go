package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scanner"
)

// newScanTestRouter builds a Router wired to a REAL scanner.Service over
// libraryPath. It mirrors newConnectionTestRouter but adds ScannerService, which
// is what makes the post-scan path-mapping hook (registered inside NewRouter)
// live: the wiring under test is production's own, not a test-only rig.
func newScanTestRouter(t *testing.T, libraryPath string) *Router {
	t.Helper()

	db := newTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}

	artistSvc := artist.NewService(db)
	scannerSvc := scanner.NewService(artistSvc, nil, nil, logger, libraryPath, nil)
	t.Cleanup(scannerSvc.Shutdown)

	return NewRouter(RouterDeps{
		SessionSecret:      testSessionSecret,
		AuthService:        auth.NewService(db),
		ArtistService:      artistSvc,
		ScannerService:     scannerSvc,
		ConnectionService:  connection.NewService(db, enc),
		LibraryService:     library.NewService(db),
		PlatformService:    platform.NewService(db),
		RuleService:        rule.NewService(db),
		NFOSnapshotService: nfo.NewSnapshotService(db),
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
		ImageCacheDir:      filepath.Join(t.TempDir(), "cache", "images"),
	})
}

// writeArtistDirWithMBID creates <base>/<name>/artist.nfo carrying a MusicBrainz
// artist id, which is what makes the scanned artist visible to ListMBIDPaths and
// therefore joinable to a peer's artist list during inference.
func writeArtistDirWithMBID(t *testing.T, base, name, mbid string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>` + name + `</name>
  <musicbrainzartistid>` + mbid + `</musicbrainzartistid>
</artist>
`
	if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing artist.nfo: %v", err)
	}
}

// runScanAndWait triggers a scan and blocks until it leaves "running". The scan
// marks itself completed only AFTER the post-scan hook returns (see runScan), so a
// settled status is proof the hook already ran. Shutdown is deliberately NOT used
// as the join point: it cancels the scanner context, which would cancel the hook's
// own HTTP calls out from under it.
func runScanAndWait(t *testing.T, r *Router) {
	t.Helper()
	if _, err := r.scannerService.Run(context.Background()); err != nil {
		t.Fatalf("starting scan: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st := r.scannerService.Status()
		if st != nil && st.Status != "running" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scan did not finish within 15s")
}

// TestAdv_OOBEOrder_InferenceNeverRuns is the regression test for the ordering
// hole that made path-mapping inference inert in the NORMAL first-run sequence
// (#2380 follow-up).
//
// The operator adds the connection BEFORE the first library scan. At create time
// the library holds zero artists, so ListMBIDPaths returns no rows, inference has
// nothing to join against and applies nothing. Then the scan lands and the MBIDs
// + host paths finally exist -- but before the fix NOTHING re-ran inference:
// applyInferredPathMappingsIfEmpty had call sites only in the create/update
// handlers. On a split-mount deployment the connection therefore sat at
// path_mappings=null indefinitely, and the fail-closed root guard refused EVERY
// rename/merge push to EVERY peer, forever.
//
// The assertion is on the SCAN, not on a re-save: create against an empty
// library, scan, and require that the connection came out mapped.
func TestAdv_OOBEOrder_InferenceNeverRuns(t *testing.T) {
	// Not parallel: withFakeLister swaps a process-global factory.
	libDir := t.TempDir()
	r := newScanTestRouter(t, libDir)

	// The peer knows both artists under its own container prefix. Two matched
	// pairs is the consensus floor (DefaultPathInferConsensus), so a correct
	// end-of-scan re-run has enough evidence to emit exactly one mapping.
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/data/Alpha"},
		{ID: 2, ForeignArtistID: "mbid-2", Path: "/data/Beta"},
	}})

	// Step 1: connection created FIRST, against an empty library.
	body := strings.NewReader(`{"name":"Lidarr OOBE","type":"lidarr","url":"http://lidarr.local:8686",` +
		`"api_key":"k","enabled":true,"skip_test":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", body)
	req.Header.Set("Content-Type", "application/json")
	w := serveValidated(t, http.HandlerFunc(r.handleCreateConnection), req)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create connection: status %d, body %s", w.Code, w.Body.String())
	}

	conns, err := r.connectionService.List(context.Background())
	if err != nil || len(conns) != 1 {
		t.Fatalf("List after create: conns=%d err=%v", len(conns), err)
	}
	id := conns[0].ID
	if m := conns[0].GetPathMappings(); len(m) != 0 {
		t.Fatalf("precondition broken: connection was mapped at create time with an empty library: %+v", m)
	}

	// Step 2: the first scan lands. NOW the MBIDs and host paths exist.
	writeArtistDirWithMBID(t, libDir, "Alpha", "mbid-1")
	writeArtistDirWithMBID(t, libDir, "Beta", "mbid-2")
	runScanAndWait(t, r)

	// Guard the test itself: if the scan did not record MBID+path rows, the
	// assertion below would be vacuous rather than meaningful.
	mbidPaths, err := r.artistService.ListMBIDPaths(context.Background())
	if err != nil {
		t.Fatalf("ListMBIDPaths: %v", err)
	}
	if len(mbidPaths) != 2 {
		t.Fatalf("scan should have produced 2 (MBID, path) rows, got %d: %+v", len(mbidPaths), mbidPaths)
	}

	// Step 3: the connection must be mapped now. Before the fix it stayed null.
	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload connection: %v", err)
	}
	mappings := got.GetPathMappings()
	if len(mappings) == 0 {
		t.Fatal("connection is STILL unmapped after the first scan: inference never re-ran, " +
			"so on a split mount the root guard would refuse every push forever")
	}
	if mappings[0].HostPrefix != libDir || mappings[0].PlatformPrefix != "/data" {
		t.Fatalf("mappings = %+v, want one %s -> /data", mappings, libDir)
	}
}

// TestPostScanSweep_PersistsOneMappingPerLibraryRoot is the multi-root case a real
// split-mount install has: TWO host library roots collapse into SUBFOLDERS of ONE
// container root on the peer. A sweep that infers a single mapping and calls it
// done leaves every artist under the other root failing the root guard - and that
// failure is invisible to any single-library test, which is exactly why this one
// exists. Both mappings must be persisted, and both must resolve inside the peer's
// single root.
func TestPostScanSweep_PersistsOneMappingPerLibraryRoot(t *testing.T) {
	// Not parallel: withFakeLister swaps a process-global factory.
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/host/media/roota/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/host/media/roota/Beta")
	seedArtistMBIDPath(t, r, "a3", "mbid-3", "/host/media/rootb/Gamma")
	seedArtistMBIDPath(t, r, "a4", "mbid-4", "/host/media/rootb/Delta")
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/music/RootA/Alpha"},
		{ID: 2, ForeignArtistID: "mbid-2", Path: "/music/RootA/Beta"},
		{ID: 3, ForeignArtistID: "mbid-3", Path: "/music/RootB/Gamma"},
		{ID: 4, ForeignArtistID: "mbid-4", Path: "/music/RootB/Delta"},
	}})

	// The post-scan hook itself, not a handler: this is the road the scan takes.
	r.applyInferredPathMappingsAllConnections(context.Background())

	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	mappings := got.GetPathMappings()
	if len(mappings) != 2 {
		t.Fatalf("persisted %d mapping(s): %+v; want 2, one per host library root", len(mappings), mappings)
	}

	// Every artist, from EITHER root, must now translate into the peer's namespace
	// and satisfy the root guard. Asserting only the mapping count would miss a
	// pair that is present but wrong.
	for host, want := range map[string]string{
		"/host/media/roota/Alpha": "/music/RootA/Alpha",
		"/host/media/rootb/Gamma": "/music/RootB/Gamma",
	} {
		mapped := got.MapArtistPath(host)
		if mapped != want {
			t.Errorf("MapArtistPath(%q) = %q, want %q", host, mapped, want)
		}
		if !connection.PathWithinRoots(mapped, []string{"/music"}) {
			t.Errorf("mapped %q is outside the peer root /music: the push would be refused", mapped)
		}
	}
}

// TestPostScanSweep_PartialMappingIsSurfaced covers the half-mapped state: one
// library root has enough matched artists, the other has none yet. The evidenced
// mapping must still be persisted (half-mapped beats unmapped), AND the
// un-inferred root must be reported rather than silently passing for complete.
func TestPostScanSweep_PartialMappingIsSurfaced(t *testing.T) {
	// Not parallel: withFakeLister swaps a process-global factory.
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	mustExec(t, r.db, `INSERT INTO libraries (id, name, path) VALUES (?, ?, ?)`,
		"lib-a", "Root A", "/host/media/roota")
	mustExec(t, r.db, `INSERT INTO libraries (id, name, path) VALUES (?, ?, ?)`,
		"lib-b", "Root B", "/host/media/rootb")

	// Evidence exists for root A only.
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/host/media/roota/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/host/media/roota/Beta")
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/music/RootA/Alpha"},
		{ID: 2, ForeignArtistID: "mbid-2", Path: "/music/RootA/Beta"},
	}})

	r.applyInferredPathMappingsAllConnections(context.Background())

	got, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	mappings := got.GetPathMappings()
	if len(mappings) != 1 || mappings[0].HostPrefix != "/host/media/roota" {
		t.Fatalf("mappings = %+v, want the one evidenced root persisted", mappings)
	}

	unmapped := r.unmappedLibraryRoots(context.Background(), mappings)
	if len(unmapped) != 1 || unmapped[0] != "/host/media/rootb" {
		t.Fatalf("unmappedLibraryRoots = %v, want [/host/media/rootb]: a half-mapped connection "+
			"must not look identical to a fully mapped one", unmapped)
	}
}

// fakeRootLister is a test double for the platformRootLister seam: it returns a
// fixed root list, standing in for Lidarr's root folders or an Emby/Jellyfin
// library's Locations.
type fakeRootLister struct {
	roots []string
	err   error
}

func (f fakeRootLister) ListRoots(context.Context) ([]string, error) { return f.roots, f.err }

// withFakeRootListerByType swaps the platform-root factory so each connection
// TYPE can report its own roots. Process-global: no t.Parallel().
func withFakeRootListerByType(t *testing.T, byType map[string]platformRootLister) {
	t.Helper()
	orig := platformRootListerFactory
	t.Cleanup(func() { platformRootListerFactory = orig })
	platformRootListerFactory = func(conn *connection.Connection, _ *slog.Logger) platformRootLister {
		return byType[conn.Type]
	}
}

// TestPostScanSweep_AllThreePeersMapBothRoots is the test the FUNCTIONAL failure
// demanded: THREE connections (Emby, Jellyfin, Lidarr) and TWO library roots, and
// every connection must end up mapped for BOTH roots. A per-connection or
// single-root test keeps passing while the real system is broken - which is
// exactly what happened: unit tests were green while Emby inferred nothing and
// Jellyfin was half-mapped against the real peers.
//
// The doubles reproduce the peers' REAL behavior, verified against live servers:
//   - EMBY returns artists with an EMPTY Path (no endpoint exposes it), so it
//     yields ZERO usable pairs. Its roots ARE reported.
//   - JELLYFIN returns paths, but only ONE root has enough matched artists to
//     clear the pair-consensus floor; the other root has a single artist.
//   - LIDARR returns paths for everything.
//
// If inference can only learn from artist pairs, Emby gets 0 mappings and
// Jellyfin gets 1. Both must get 2.
func TestPostScanSweep_AllThreePeersMapBothRoots(t *testing.T) {
	// Not parallel: the lister factories are process-global.
	r := newConnectionTestRouter(t)
	lidarrID := seedLidarrConn(t, r)
	embyID := seedEmbyConn(t, r)
	jellyfinID := seedJellyfinConn(t, r)

	mustExec(t, r.db, `INSERT INTO libraries (id, name, path) VALUES (?, ?, ?)`,
		"lib-a", "Root A", "/host/media/roota")
	mustExec(t, r.db, `INSERT INTO libraries (id, name, path) VALUES (?, ?, ?)`,
		"lib-b", "Root B", "/host/media/rootb")

	// Three artists under root A, ONE under root B (the sparse root).
	seedArtistMBIDPath(t, r, "a1", "mbid-1", "/host/media/roota/Alpha")
	seedArtistMBIDPath(t, r, "a2", "mbid-2", "/host/media/roota/Beta")
	seedArtistMBIDPath(t, r, "a3", "mbid-3", "/host/media/rootb/Gamma")

	// Lidarr: full artist paths.
	withFakeLister(t, fakeArtistLister{artists: []lidarr.Artist{
		{ID: 1, ForeignArtistID: "mbid-1", Path: "/peer/RootA/Alpha"},
		{ID: 2, ForeignArtistID: "mbid-2", Path: "/peer/RootA/Beta"},
		{ID: 3, ForeignArtistID: "mbid-3", Path: "/peer/RootB/Gamma"},
	}})
	// Emby and Jellyfin share the media-lister seam, so drive the WORST case for
	// both: artists with MBIDs but NO path - Emby's real, verified behavior.
	withFakeMediaLister(t, fakeMediaLister{artists: []platformArtistPath{
		{MBID: "mbid-1", Path: ""},
		{MBID: "mbid-2", Path: ""},
		{MBID: "mbid-3", Path: ""},
	}})
	// Every peer reports its own roots (Lidarr root folders / Emby+Jellyfin
	// library Locations). Note the trailing slash on one Emby location: the live
	// Emby server reports exactly that.
	roots := fakeRootLister{roots: []string{"/peer/RootA/", "/peer/RootB"}}
	withFakeRootListerByType(t, map[string]platformRootLister{
		connection.TypeLidarr:   roots,
		connection.TypeEmby:     roots,
		connection.TypeJellyfin: roots,
	})

	r.applyInferredPathMappingsAllConnections(context.Background())

	for _, tc := range []struct{ name, id string }{
		{"emby", embyID},
		{"jellyfin", jellyfinID},
		{"lidarr", lidarrID},
	} {
		got, err := r.connectionService.GetByID(context.Background(), tc.id)
		if err != nil {
			t.Fatalf("%s: reload: %v", tc.name, err)
		}
		mappings := got.GetPathMappings()
		if len(mappings) != 2 {
			t.Errorf("%s: %d mapping(s) %+v; want 2 (one per library root). A peer that maps only "+
				"some roots refuses every push under the others.", tc.name, len(mappings), mappings)
			continue
		}
		// Both roots must actually translate and land inside the peer's roots.
		for host, want := range map[string]string{
			"/host/media/roota/Alpha": "/peer/RootA/Alpha",
			"/host/media/rootb/Gamma": "/peer/RootB/Gamma",
		} {
			mapped := got.MapArtistPath(host)
			if mapped != want {
				t.Errorf("%s: MapArtistPath(%q) = %q, want %q", tc.name, host, mapped, want)
			}
			if !connection.PathWithinRoots(mapped, []string{"/peer/RootA", "/peer/RootB"}) {
				t.Errorf("%s: mapped %q is outside the peer roots: the push would be refused", tc.name, mapped)
			}
		}
	}
}

// TestApplyInferred_ZeroMappingsLogsWarning pins the no-silent-failure rule that
// let the Emby hole hide: a connection that infers NOTHING must SAY so. Before
// this, zero mappings returned quietly - no INFO, no WARN - so path_mappings=null
// was indistinguishable from "inference never ran", and it went unnoticed against
// a live peer with 38 linked artists.
func TestApplyInferred_ZeroMappingsLogsWarning(t *testing.T) {
	// Not parallel: the lister factories are process-global.
	r := newConnectionTestRouter(t)
	id := seedLidarrConn(t, r)

	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// No artists, and a peer that reports no roots: nothing to infer from.
	withFakeLister(t, fakeArtistLister{})
	withFakeRootListerByType(t, map[string]platformRootLister{
		connection.TypeLidarr: fakeRootLister{},
	})

	conn, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r.applyInferredPathMappingsIfEmpty(context.Background(), conn)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "NO mappings") {
		t.Fatalf("a zero-mapping inference must log a WARNING naming the connection; got:\n%s", out)
	}
	if !strings.Contains(out, "type=lidarr") {
		t.Errorf("the warning must name the real peer type; got:\n%s", out)
	}
}

// TestApplyInferred_LogNamesTheRealPeerType is the F7 regression: the success line
// said "applied inferred Lidarr path mappings" for EVERY connection type, so an
// operator debugging Emby read "Lidarr" against an Emby connection id.
func TestApplyInferred_LogNamesTheRealPeerType(t *testing.T) {
	// Not parallel: the lister factories are process-global.
	r := newConnectionTestRouter(t)
	id := seedJellyfinConn(t, r)

	var buf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mustExec(t, r.db, `INSERT INTO libraries (id, name, path) VALUES (?, ?, ?)`,
		"lib-a", "Root A", "/host/media/roota")
	withFakeMediaLister(t, fakeMediaLister{})
	withFakeRootListerByType(t, map[string]platformRootLister{
		connection.TypeJellyfin: fakeRootLister{roots: []string{"/peer/roota"}},
	})

	conn, err := r.connectionService.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r.applyInferredPathMappingsIfEmpty(context.Background(), conn)

	out := buf.String()
	if strings.Contains(out, "Lidarr path mappings") {
		t.Errorf("the log still hardcodes Lidarr for a jellyfin connection:\n%s", out)
	}
	if !strings.Contains(out, "applied inferred path mappings") || !strings.Contains(out, "type=jellyfin") {
		t.Errorf("expected an applied-mappings line naming type=jellyfin; got:\n%s", out)
	}
}

// seedJellyfinConn persists an enabled Jellyfin connection (mirrors
// seedEmbyConn / seedLidarrConn).
func seedJellyfinConn(t *testing.T, r *Router) string {
	t.Helper()
	c := &connection.Connection{
		Name:    "Jellyfin Test",
		Type:    connection.TypeJellyfin,
		URL:     "http://jellyfin.local:8096",
		APIKey:  "k",
		Enabled: true,
	}
	newConnectionTestConn(t, r, c)
	return c.ID
}
