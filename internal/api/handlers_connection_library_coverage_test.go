package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/internal/library"
)

// newConnLibTestConn is the connection-library coverage suite's prefixed helper
// for seeding a connection row directly.
func newConnLibTestConn(t *testing.T, r *Router, c *connection.Connection) {
	t.Helper()
	if err := r.connectionService.Create(context.Background(), c); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	// Set status=ok so /libraries and /scan branches that gate on it run.
	if err := r.connectionService.UpdateStatus(context.Background(), c.ID, "ok", ""); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
}

// --- handleDiscoverLibraries --------------------------------------------------

func TestHandleDiscoverLibraries_Emby(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/Library/VirtualFolders" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Name": "Music", "CollectionType": "music", "ItemId": "lib-music"},
				{"Name": "Movies", "CollectionType": "movies", "ItemId": "lib-movies"},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := testRouterForLibraryOps(t)
	c := &connection.Connection{
		Name: "DiscEmby", Type: connection.TypeEmby,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnLibTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/libraries", nil)
	req.SetPathValue("id", c.ID)
	w := httptest.NewRecorder()

	r.handleDiscoverLibraries(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var out []discoveredLibrary
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Movies library is filtered out by the music-only logic in the client.
	if len(out) != 1 || out[0].Name != "Music" {
		t.Errorf("discovered = %+v, want one Music entry", out)
	}
}

func TestHandleDiscoverLibraries_LidarrEmpty(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)
	c := &connection.Connection{
		Name: "DiscLidarr", Type: connection.TypeLidarr,
		URL: "http://l.local:8686", APIKey: "k", Enabled: true,
	}
	newConnLibTestConn(t, r, c)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/libraries", nil)
	req.SetPathValue("id", c.ID)
	w := httptest.NewRecorder()

	r.handleDiscoverLibraries(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var out []discoveredLibrary
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("Lidarr should produce empty list, got %+v", out)
	}
}

func TestHandleDiscoverLibraries_NotFound(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/missing/libraries", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	r.handleDiscoverLibraries(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleDiscoverLibraries_Disabled(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)
	c := &connection.Connection{
		Name: "Disabled", Type: connection.TypeEmby,
		URL: "http://e.local:8096", APIKey: "k", Enabled: false,
	}
	if err := r.connectionService.Create(context.Background(), c); err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/libraries", nil)
	req.SetPathValue("id", c.ID)
	w := httptest.NewRecorder()
	r.handleDiscoverLibraries(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for disabled", w.Code)
	}
}

func TestHandleDiscoverLibraries_NotTested(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)
	c := &connection.Connection{
		Name: "Untested", Type: connection.TypeEmby,
		URL: "http://e.local:8096", APIKey: "k", Enabled: true,
	}
	if err := r.connectionService.Create(context.Background(), c); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Default Status is "unknown" -- handler should refuse.

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/connections/"+c.ID+"/libraries", nil)
	req.SetPathValue("id", c.ID)
	w := httptest.NewRecorder()
	r.handleDiscoverLibraries(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for untested", w.Code)
	}
}

// --- handleScanLibrary --------------------------------------------------------

func TestHandleScanLibrary_AcceptedAndCompletes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Empty result set -- the scan completes with 0 updates and no errors.
		if strings.Contains(req.URL.Path, "/Items") || strings.Contains(req.URL.Path, "/AlbumArtists") {
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}, "TotalRecordCount": 0})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := testRouterForLibraryOps(t)
	c := &connection.Connection{
		Name: "ScanCon", Type: connection.TypeEmby,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnLibTestConn(t, r, c)

	lib := &library.Library{
		Name: "ScanLib", Type: library.TypeRegular, Source: connection.TypeEmby,
		ConnectionID: c.ID, ExternalID: "ext-scan",
	}
	if err := r.libraryService.Create(context.Background(), lib); err != nil {
		t.Fatalf("create lib: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/libraries/"+lib.ID+"/scan", nil)
	req.SetPathValue("id", c.ID)
	req.SetPathValue("libId", lib.ID)
	w := httptest.NewRecorder()

	r.handleScanLibrary(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", w.Code, w.Body.String())
	}

	// Wait briefly for the goroutine to flip the op status to completed.
	finalStatus := waitForOpStatus(t, r, lib.ID, 5*time.Second)
	if finalStatus != "completed" {
		t.Errorf("op status = %q, want completed", finalStatus)
	}
}

// waitForOpStatus polls until the named library's op leaves the running state
// or the deadline elapses. The return value is read under r.libraryOpsMu so
// callers do not race with the runPopulate/runLibraryScan goroutine.
func waitForOpStatus(t *testing.T, r *Router, libID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.libraryOpsMu.Lock()
		op, ok := r.libraryOps[libID]
		status := ""
		if ok {
			status = op.Status
		}
		r.libraryOpsMu.Unlock()
		if status != "" && status != "running" {
			return status
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.libraryOpsMu.Lock()
	defer r.libraryOpsMu.Unlock()
	if op, ok := r.libraryOps[libID]; ok {
		return op.Status
	}
	return ""
}

// TestHandleScanLibrary_FailsOnUpstreamError forces the upstream to return
// errors so runLibraryScan flips the op to "failed" -- this is the
// op.Status == "failed" branch we need for runLibraryScan coverage.
func TestHandleScanLibrary_FailsOnUpstreamError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := testRouterForLibraryOps(t)
	c := &connection.Connection{
		Name: "ScanFail", Type: connection.TypeLidarr,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnLibTestConn(t, r, c)

	lib := &library.Library{
		Name: "ScanFailLib", Type: library.TypeRegular, Source: connection.TypeLidarr,
		ConnectionID: c.ID, ExternalID: "ext-fail",
	}
	if err := r.libraryService.Create(context.Background(), lib); err != nil {
		t.Fatalf("create lib: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/libraries/"+lib.ID+"/scan", nil)
	req.SetPathValue("id", c.ID)
	req.SetPathValue("libId", lib.ID)
	w := httptest.NewRecorder()

	r.handleScanLibrary(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	finalStatus := waitForOpStatus(t, r, lib.ID, 5*time.Second)
	if finalStatus != "failed" {
		t.Errorf("op status = %q, want failed", finalStatus)
	}
}

func TestHandleScanLibrary_ConnectionNotFound(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/missing/libraries/x/scan", nil)
	req.SetPathValue("id", "missing")
	req.SetPathValue("libId", "x")
	w := httptest.NewRecorder()

	r.handleScanLibrary(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleScanLibrary_LibraryWrongConnection(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	c := &connection.Connection{
		Name: "ScanWrong", Type: connection.TypeEmby,
		URL: "http://e.local:8096", APIKey: "k", Enabled: true,
	}
	newConnLibTestConn(t, r, c)

	other := &connection.Connection{
		Name: "OtherConn", Type: connection.TypeLidarr,
		URL: "http://o.local:8686", APIKey: "k", Enabled: true,
	}
	newConnLibTestConn(t, r, other)

	lib := &library.Library{
		Name: "WrongLib", Type: library.TypeRegular, Source: connection.TypeLidarr,
		ConnectionID: other.ID, ExternalID: "ext-wrong",
	}
	if err := r.libraryService.Create(context.Background(), lib); err != nil {
		t.Fatalf("create lib: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/libraries/"+lib.ID+"/scan", nil)
	req.SetPathValue("id", c.ID)
	req.SetPathValue("libId", lib.ID)
	w := httptest.NewRecorder()

	r.handleScanLibrary(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleScanLibrary_AlreadyRunning(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	c := &connection.Connection{
		Name: "ScanBusy", Type: connection.TypeEmby,
		URL: "http://e.local:8096", APIKey: "k", Enabled: true,
	}
	newConnLibTestConn(t, r, c)

	lib := &library.Library{
		Name: "BusyLib", Type: library.TypeRegular, Source: connection.TypeEmby,
		ConnectionID: c.ID, ExternalID: "ext-busy",
	}
	if err := r.libraryService.Create(context.Background(), lib); err != nil {
		t.Fatalf("create lib: %v", err)
	}

	r.libraryOpsMu.Lock()
	r.libraryOps[lib.ID] = &LibraryOpResult{
		LibraryID: lib.ID, Operation: "populate", Status: "running",
	}
	r.libraryOpsMu.Unlock()

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/connections/"+c.ID+"/libraries/"+lib.ID+"/scan", nil)
	req.SetPathValue("id", c.ID)
	req.SetPathValue("libId", lib.ID)
	w := httptest.NewRecorder()

	r.handleScanLibrary(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

// --- populateFromLidarrCtx ----------------------------------------------------

// TestPopulateFromLidarrCtx_CreatesArtists drives populateFromLidarrCtx through
// a stubbed Lidarr GET /api/v1/artist response, asserting that two new artist
// rows are created and result.Created == 2.
func TestPopulateFromLidarrCtx_CreatesArtists(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/artist" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "artistName": "Aardvark", "foreignArtistId": "11111111-1111-1111-1111-111111111111"},
			{"id": 2, "artistName": "Beluga", "foreignArtistId": "22222222-2222-2222-2222-222222222222"},
		})
	}))
	defer srv.Close()

	r := testRouterForLibraryOps(t)
	c := &connection.Connection{
		Name: "Lidarr1", Type: connection.TypeLidarr,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnLibTestConn(t, r, c)

	lib := &library.Library{
		Name: "LidarrLib", Type: library.TypeRegular, Source: connection.TypeLidarr,
		ConnectionID: c.ID, ExternalID: "ext-lidarr",
	}
	if err := r.libraryService.Create(context.Background(), lib); err != nil {
		t.Fatalf("create lib: %v", err)
	}

	client := lidarr.New(srv.URL, "k", r.logger)
	var result populateResult
	if err := r.populateFromLidarrCtx(context.Background(), client, lib, &result); err != nil {
		t.Fatalf("populateFromLidarrCtx: %v", err)
	}
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2", result.Total)
	}
	if result.Created != 2 {
		t.Errorf("Created = %d, want 2", result.Created)
	}
}

// TestPopulateFromLidarrCtx_SkipsExistingByMBID seeds an artist row with the
// same MBID one of the Lidarr items declares; the dedupe path must skip and
// still record a platform-id mapping rather than creating a duplicate.
func TestPopulateFromLidarrCtx_SkipsExistingByMBID(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 99, "artistName": "Cuttlefish", "foreignArtistId": "33333333-3333-3333-3333-333333333333"},
		})
	}))
	defer srv.Close()

	r := testRouterForLibraryOps(t)
	c := &connection.Connection{
		Name: "Lidarr2", Type: connection.TypeLidarr,
		URL: srv.URL, APIKey: "k", Enabled: true,
	}
	newConnLibTestConn(t, r, c)

	lib := &library.Library{
		Name: "LidarrLib2", Type: library.TypeRegular, Source: connection.TypeLidarr,
		ConnectionID: c.ID, ExternalID: "ext-lidarr-2",
	}
	if err := r.libraryService.Create(context.Background(), lib); err != nil {
		t.Fatalf("create lib: %v", err)
	}

	pre := &artist.Artist{
		Name: "Cuttlefish", LibraryID: lib.ID,
		MusicBrainzID: "33333333-3333-3333-3333-333333333333",
	}
	if err := r.artistService.Create(context.Background(), pre); err != nil {
		t.Fatalf("create pre: %v", err)
	}

	client := lidarr.New(srv.URL, "k", r.logger)
	var result populateResult
	if err := r.populateFromLidarrCtx(context.Background(), client, lib, &result); err != nil {
		t.Fatalf("populateFromLidarrCtx: %v", err)
	}
	if result.Created != 0 {
		t.Errorf("Created = %d, want 0 (dedupe should match)", result.Created)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", result.Skipped)
	}
}

// --- checkSyncMtimeEvidence ---------------------------------------------------

// checkSyncMtimeFixture spins up an artist + library + image-on-disk so the
// per-tier helper can be exercised against a deterministic filesystem state.
type checkSyncMtimeFixture struct {
	r         *Router
	lib       *library.Library
	a         *artist.Artist
	imageDir  string
	imageFile string
}

func newCheckSyncMtimeFixture(t *testing.T, lastWrittenISO string) *checkSyncMtimeFixture {
	t.Helper()
	r := testRouterForLibraryOps(t)

	tmp := t.TempDir()
	lib := &library.Library{
		Name: "MtimeLib", Path: tmp, Type: library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := r.libraryService.Create(context.Background(), lib); err != nil {
		t.Fatalf("create lib: %v", err)
	}

	artistDir := filepath.Join(tmp, "ArtistMtime")
	if err := os.MkdirAll(artistDir, 0o755); err != nil {
		t.Fatalf("mkdir artist dir: %v", err)
	}
	a := &artist.Artist{
		Name: "Mtime Artist", LibraryID: lib.ID, Path: artistDir, ThumbExists: true,
	}
	if err := r.artistService.Create(context.Background(), a); err != nil {
		t.Fatalf("create artist: %v", err)
	}

	// Record the last-written-at provenance so NewestWriteTimesByArtist returns
	// something the mtime check can compare against.
	if err := r.artistService.UpdateImageProvenance(context.Background(),
		a.ID, "thumb", 0, "deadbeef", "user", "jpeg", lastWrittenISO); err != nil {
		t.Fatalf("UpdateImageProvenance: %v", err)
	}

	imgPath := filepath.Join(artistDir, "folder.jpg")
	if err := os.WriteFile(imgPath, []byte("fake-jpg"), 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}

	return &checkSyncMtimeFixture{
		r:         r,
		lib:       lib,
		a:         a,
		imageDir:  artistDir,
		imageFile: imgPath,
	}
}

// TestCheckSyncMtimeEvidence_MtimeEqual exercises the "no evidence" branch:
// the file mtime matches the recorded last-written-at within tolerance, so
// the library's shared-FS status must remain unchanged.
func TestCheckSyncMtimeEvidence_MtimeEqual(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	f := newCheckSyncMtimeFixture(t, now.Format(time.RFC3339))

	if err := os.Chtimes(f.imageFile, now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	f.r.checkSyncMtimeEvidence(context.Background(), f.lib)

	got, err := f.r.libraryService.GetByID(context.Background(), f.lib.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.SharedFSStatus == library.SharedFSSuspected || got.SharedFSStatus == library.SharedFSConfirmed {
		t.Errorf("SharedFSStatus = %q, want unchanged (no evidence)", got.SharedFSStatus)
	}
}

// TestCheckSyncMtimeEvidence_MtimeFutureSkewed asserts that a file mtime
// after the recorded last-write triggers the suspected-shared-FS upgrade.
func TestCheckSyncMtimeEvidence_MtimeFutureSkewed(t *testing.T) {
	t.Parallel()
	past := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
	f := newCheckSyncMtimeFixture(t, past.Format(time.RFC3339))

	future := past.Add(5 * time.Minute)
	if err := os.Chtimes(f.imageFile, future, future); err != nil {
		t.Fatalf("chtimes future: %v", err)
	}

	f.r.checkSyncMtimeEvidence(context.Background(), f.lib)

	got, err := f.r.libraryService.GetByID(context.Background(), f.lib.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.SharedFSStatus != library.SharedFSSuspected {
		t.Errorf("SharedFSStatus = %q, want %q", got.SharedFSStatus, library.SharedFSSuspected)
	}
}

// TestCheckSyncMtimeEvidence_MissingFile covers the case where the recorded
// artist directory does not exist on disk -- evidence collection produces no
// rows and the library's shared-FS status remains unchanged.
func TestCheckSyncMtimeEvidence_MissingFile(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	f := newCheckSyncMtimeFixture(t, now.Format(time.RFC3339))

	// Remove the entire artist directory after seeding so the helper sees
	// nothing on disk.
	if err := os.RemoveAll(f.imageDir); err != nil {
		t.Fatalf("remove artist dir: %v", err)
	}

	f.r.checkSyncMtimeEvidence(context.Background(), f.lib)

	got, err := f.r.libraryService.GetByID(context.Background(), f.lib.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.SharedFSStatus == library.SharedFSSuspected || got.SharedFSStatus == library.SharedFSConfirmed {
		t.Errorf("SharedFSStatus = %q, want unchanged (missing file)", got.SharedFSStatus)
	}
}

// TestCheckSyncMtimeEvidence_AlreadyConfirmed exercises the early-return guard
// against downgrading a "confirmed" shared-FS library to "suspected."
func TestCheckSyncMtimeEvidence_AlreadyConfirmed(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	f := newCheckSyncMtimeFixture(t, now.Format(time.RFC3339))

	// Promote to confirmed manually.
	if err := f.r.libraryService.SetSharedFSStatus(context.Background(),
		f.lib.ID, library.SharedFSConfirmed, "[]", ""); err != nil {
		t.Fatalf("SetSharedFSStatus: %v", err)
	}
	// Reload so f.lib reflects the new status.
	updated, err := f.r.libraryService.GetByID(context.Background(), f.lib.ID)
	if err != nil {
		t.Fatalf("reload lib: %v", err)
	}

	// Even with a wildly-future mtime, the helper must not touch the status.
	future := now.Add(time.Hour)
	if err := os.Chtimes(f.imageFile, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	f.r.checkSyncMtimeEvidence(context.Background(), updated)

	got, err := f.r.libraryService.GetByID(context.Background(), f.lib.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.SharedFSStatus != library.SharedFSConfirmed {
		t.Errorf("SharedFSStatus = %q, want %q (must not downgrade)",
			got.SharedFSStatus, library.SharedFSConfirmed)
	}
}
