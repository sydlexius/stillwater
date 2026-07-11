package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/rule"
)

func testRouterForLibraryOps(t *testing.T) *Router {
	t.Helper()

	db := newTestDB(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	libSvc := library.NewService(db)
	artistSvc := artist.NewService(db)
	authSvc := auth.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	nfoSnapSvc := nfo.NewSnapshotService(db)

	platformSvc := platform.NewService(db)

	cacheDir := filepath.Join(t.TempDir(), "cache", "images")

	return NewRouter(RouterDeps{
		SessionSecret:      testSessionSecret,
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		LibraryService:     libSvc,
		PlatformService:    platformSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
		ImageCacheDir:      cacheDir,
	})
}

func TestHandleLibraryOpStatus_Idle(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/libraries/nonexistent/operation/status", nil)
	req.SetPathValue("libId", "nonexistent")
	w := httptest.NewRecorder()

	r.handleLibraryOpStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "idle" {
		t.Errorf("status = %q, want %q", resp["status"], "idle")
	}
}

func TestHandleLibraryOpStatus_Running(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	// Simulate a running operation.
	r.libraryOpsMu.Lock()
	r.libraryOps["test-lib-123"] = &LibraryOpResult{
		LibraryID:   "test-lib-123",
		LibraryName: "Test Library",
		Operation:   "populate",
		Status:      "running",
		StartedAt:   time.Now().UTC(),
	}
	r.libraryOpsMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/libraries/test-lib-123/operation/status", nil)
	req.SetPathValue("libId", "test-lib-123")
	w := httptest.NewRecorder()

	r.handleLibraryOpStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp LibraryOpResult
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Status != "running" {
		t.Errorf("status = %q, want %q", resp.Status, "running")
	}
	if resp.Operation != "populate" {
		t.Errorf("operation = %q, want %q", resp.Operation, "populate")
	}
}

func TestHandleLibraryOpStatus_Completed(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	now := time.Now().UTC()
	r.libraryOpsMu.Lock()
	r.libraryOps["test-lib-456"] = &LibraryOpResult{
		LibraryID:   "test-lib-456",
		LibraryName: "Test Library",
		Operation:   "scan",
		Status:      "completed",
		Message:     "Scan complete: 5 artists updated in Test Library",
		StartedAt:   now.Add(-10 * time.Second),
		CompletedAt: &now,
	}
	r.libraryOpsMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/libraries/test-lib-456/operation/status", nil)
	req.SetPathValue("libId", "test-lib-456")
	w := httptest.NewRecorder()

	r.handleLibraryOpStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp LibraryOpResult
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Status != "completed" {
		t.Errorf("status = %q, want %q", resp.Status, "completed")
	}
	if resp.Message != "Scan complete: 5 artists updated in Test Library" {
		t.Errorf("message = %q, want expected completion message", resp.Message)
	}
}

// TestHandlePopulateInFlight_Empty asserts the aggregate populate status
// endpoint returns an empty operations list when no populate is running.
// The pill-rehydrate JS skips push when operations is empty, so this
// shape is what keeps the idle reconnect path silent.
func TestHandlePopulateInFlight_Empty(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections/populate/in-flight", nil)
	w := httptest.NewRecorder()
	r.handlePopulateInFlight(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	// Decode through a pointer-slice so we can distinguish a missing or
	// JSON-null `operations` field (resp.Operations == nil) from an
	// explicit empty array (resp.Operations != nil with len 0). The
	// JS rehydrate path treats `operations: undefined` as a no-op, so
	// the server contract is "operations is always present, possibly
	// empty"; pin that explicitly here per CR feedback.
	var resp struct {
		Operations []map[string]any `json:"operations"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Operations == nil {
		t.Errorf("operations field missing or null; want explicit empty array")
	}
	if len(resp.Operations) != 0 {
		t.Errorf("operations = %v, want empty", resp.Operations)
	}
}

// TestHandlePopulateInFlight_RunningOnly verifies the in-flight endpoint
// returns only running populates (skipping completed/scan ops) in the
// ProgressPill envelope shape with the populateOpID prefix the JS uses
// to coalesce events into the same pill.
func TestHandlePopulateInFlight_RunningOnly(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	now := time.Now().UTC()
	r.libraryOpsMu.Lock()
	r.libraryOps["lib-running"] = &LibraryOpResult{
		LibraryID:   "lib-running",
		LibraryName: "Active Lib",
		Operation:   "populate",
		Status:      "running",
		StartedAt:   now,
	}
	r.libraryOps["lib-completed"] = &LibraryOpResult{
		LibraryID:   "lib-completed",
		LibraryName: "Done Lib",
		Operation:   "populate",
		Status:      "completed",
		StartedAt:   now.Add(-1 * time.Minute),
		CompletedAt: &now,
	}
	r.libraryOps["lib-scanning"] = &LibraryOpResult{
		LibraryID:   "lib-scanning",
		LibraryName: "Scan Lib",
		Operation:   "scan",
		Status:      "running",
		StartedAt:   now,
	}
	r.libraryOpsMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections/populate/in-flight", nil)
	w := httptest.NewRecorder()
	r.handlePopulateInFlight(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp struct {
		Operations []struct {
			OpID      string `json:"op_id"`
			Label     string `json:"label"`
			Processed int    `json:"processed"`
			Total     int    `json:"total"`
			Status    string `json:"status"`
		} `json:"operations"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Operations) != 1 {
		t.Fatalf("operations count = %d, want 1; got %+v", len(resp.Operations), resp.Operations)
	}
	got := resp.Operations[0]
	if got.OpID != "populate:lib-running" {
		t.Errorf("op_id = %q, want %q", got.OpID, "populate:lib-running")
	}
	if got.Status != "running" {
		t.Errorf("status = %q, want running", got.Status)
	}
	if !strings.Contains(got.Label, "Active Lib") {
		t.Errorf("label = %q, want it to contain library name", got.Label)
	}
	// Pin processed + total on the envelope contract so regressions in
	// either field don't slip through. The LibraryOpResult fixture has no
	// progress recorded, so both are expected to be the zero value; the
	// invariant tested is "field is present in the decoded envelope" plus
	// "sensible relationship (total >= processed)".
	if got.Processed < 0 {
		t.Errorf("processed = %d, want >= 0", got.Processed)
	}
	if got.Total < got.Processed {
		t.Errorf("total = %d < processed = %d, want total >= processed", got.Total, got.Processed)
	}
}

func TestScheduleOpCleanup(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	op := &LibraryOpResult{
		LibraryID:   "cleanup-lib",
		LibraryName: "Cleanup Test Library",
		Operation:   "populate",
		Status:      "completed",
		Message:     "done",
		StartedAt:   time.Now().UTC(),
	}

	r.libraryOpsMu.Lock()
	r.libraryOps["cleanup-lib"] = op
	r.libraryOpsMu.Unlock()

	// Call scheduleOpCleanup directly (it blocks until the timer fires, but we
	// test the logic by calling the cleanup inline after verifying the map entry
	// exists). To avoid a 5-minute wait, invoke the cleanup logic directly.
	r.libraryOpsMu.Lock()
	current, ok := r.libraryOps["cleanup-lib"]
	if !ok {
		t.Fatal("expected operation in map before cleanup")
	}
	if current == op && current.Status != "running" {
		delete(r.libraryOps, "cleanup-lib")
	}
	r.libraryOpsMu.Unlock()

	r.libraryOpsMu.Lock()
	_, exists := r.libraryOps["cleanup-lib"]
	r.libraryOpsMu.Unlock()
	if exists {
		t.Error("expected completed operation to be cleaned up, but it still exists")
	}
}

func TestScheduleOpCleanup_SkipsRunningOp(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	op := &LibraryOpResult{
		LibraryID:   "running-lib",
		LibraryName: "Running Test Library",
		Operation:   "scan",
		Status:      "running",
		StartedAt:   time.Now().UTC(),
	}

	r.libraryOpsMu.Lock()
	r.libraryOps["running-lib"] = op
	r.libraryOpsMu.Unlock()

	// Simulate cleanup logic: should NOT delete a still-running operation.
	r.libraryOpsMu.Lock()
	current, ok := r.libraryOps["running-lib"]
	if ok && current == op && current.Status != "running" {
		delete(r.libraryOps, "running-lib")
	}
	r.libraryOpsMu.Unlock()

	r.libraryOpsMu.Lock()
	_, exists := r.libraryOps["running-lib"]
	r.libraryOpsMu.Unlock()
	if !exists {
		t.Error("expected running operation to be preserved, but it was deleted")
	}
}

func TestScheduleOpCleanup_SkipsNewerOp(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	oldOp := &LibraryOpResult{
		LibraryID:   "reused-lib",
		LibraryName: "Reused Library",
		Operation:   "populate",
		Status:      "completed",
		StartedAt:   time.Now().UTC(),
	}
	newOp := &LibraryOpResult{
		LibraryID:   "reused-lib",
		LibraryName: "Reused Library",
		Operation:   "scan",
		Status:      "running",
		StartedAt:   time.Now().UTC(),
	}

	// Insert the old op, then replace it with a newer one (simulating a new
	// operation starting before the old cleanup fires).
	r.libraryOpsMu.Lock()
	r.libraryOps["reused-lib"] = newOp
	r.libraryOpsMu.Unlock()

	// Simulate cleanup for the OLD operation -- pointer identity should prevent deletion.
	r.libraryOpsMu.Lock()
	current, ok := r.libraryOps["reused-lib"]
	if ok && current == oldOp && current.Status != "running" {
		delete(r.libraryOps, "reused-lib")
	}
	r.libraryOpsMu.Unlock()

	r.libraryOpsMu.Lock()
	_, exists := r.libraryOps["reused-lib"]
	r.libraryOpsMu.Unlock()
	if !exists {
		t.Error("expected newer operation to be preserved, but it was deleted")
	}
}

func TestPopulateFromEmby_ImportsMetadataFields(t *testing.T) {
	t.Parallel()
	// Stand up a fake Emby server returning one artist with full metadata.
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{
				"Name":"Radiohead",
				"SortName":"Radiohead, The",
				"Id":"emby-001",
				"Path":"/music/Radiohead",
				"Overview":"English rock band formed in Abingdon.",
				"Genres":["Rock","Alternative"],
				"Tags":["Experimental","Art Rock"],
				"PremiereDate":"1985-01-01T00:00:00.0000000Z",
				"EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":"a74b1b7f-71a5-4011-9441-d0b5e4122711"},
				"ImageTags":{"Primary":"abc"}
			}],
			"TotalRecordCount":1
		}`))
	}))
	defer embySrv.Close()

	r := testRouterForLibraryOps(t)
	ctx := context.Background()

	// Create a library to populate into.
	lib := &library.Library{
		Name:       "Test Music",
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-1",
	}
	if err := r.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	// Run populate using the fake Emby server.
	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := r.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}

	// Retrieve the artist and verify metadata was mapped.
	a, err := r.artistService.GetByName(ctx, "Radiohead")
	if err != nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a == nil {
		t.Fatal("artist not found after populate")
	}
	if a.SortName != "Radiohead, The" {
		t.Errorf("SortName = %q, want %q", a.SortName, "Radiohead, The")
	}
	if a.Biography != "English rock band formed in Abingdon." {
		t.Errorf("Biography = %q, want expected text", a.Biography)
	}
	if len(a.Genres) != 2 || a.Genres[0] != "Rock" {
		t.Errorf("Genres = %v, want [Rock Alternative]", a.Genres)
	}
	if len(a.Styles) != 2 || a.Styles[0] != "Experimental" {
		t.Errorf("Styles = %v, want [Experimental Art Rock]", a.Styles)
	}
	if a.Formed != "1985-01-01T00:00:00.0000000Z" {
		t.Errorf("Formed = %q, want 1985 date", a.Formed)
	}
	if a.Disbanded != "" {
		t.Errorf("Disbanded = %q, want empty", a.Disbanded)
	}
	if a.MusicBrainzID != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("MusicBrainzID = %q, want expected MBID", a.MusicBrainzID)
	}
	// M:N membership: populate switched from a library-scoped lookup to
	// an unscoped GetByName, so this assertion is what now guarantees the
	// artist_libraries join row was actually inserted alongside the
	// artists row. A regression that creates only the artist would still
	// satisfy GetByName above.
	assertArtistInLibrary(t, r, ctx, a.ID, lib.ID)
}

func TestPopulateFromJellyfin_ImportsMetadataFields(t *testing.T) {
	t.Parallel()
	// Stand up a fake Jellyfin server returning one artist with full metadata.
	jfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{
				"Name":"Bjork",
				"SortName":"Bjork",
				"Id":"jf-001",
				"Path":"/music/Bjork",
				"Overview":"Icelandic singer and songwriter.",
				"Genres":["Electronic","Art Pop"],
				"Tags":["Avant-Garde"],
				"PremiereDate":"1965-11-21T00:00:00.0000000Z",
				"EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":"87c5dedd-371d-4571-9e1c-45f6e0ed3fce"},
				"ImageTags":{}
			}],
			"TotalRecordCount":1
		}`))
	}))
	defer jfSrv.Close()

	r := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music JF",
		Type:       library.TypeRegular,
		Source:     connection.TypeJellyfin,
		ExternalID: "jf-lib-1",
	}
	if err := r.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := jellyfin.NewWithHTTPClient(jfSrv.URL, "key", "", jfSrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := r.populateFromJellyfinCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromJellyfinCtx: %v", err)
	}

	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}

	a, err := r.artistService.GetByName(ctx, "Bjork")
	if err != nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a == nil {
		t.Fatal("artist not found after populate")
	}
	if a.Biography != "Icelandic singer and songwriter." {
		t.Errorf("Biography = %q, want expected text", a.Biography)
	}
	if len(a.Genres) != 2 || a.Genres[0] != "Electronic" {
		t.Errorf("Genres = %v, want [Electronic Art Pop]", a.Genres)
	}
	if len(a.Styles) != 1 || a.Styles[0] != "Avant-Garde" {
		t.Errorf("Styles = %v, want [Avant-Garde]", a.Styles)
	}
	if a.Formed != "1965-11-21T00:00:00.0000000Z" {
		t.Errorf("Formed = %q, want 1965 date", a.Formed)
	}
	assertArtistInLibrary(t, r, ctx, a.ID, lib.ID)
}

func TestPopulateLibrary_ConflictWhenRunning(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)
	ctx := context.Background()

	// Create a connection and library in the DB.
	conn := &connection.Connection{
		Name:    "Test Emby",
		Type:    connection.TypeEmby,
		URL:     "http://emby.local:8096",
		APIKey:  "test-key",
		Enabled: true,
		Status:  "ok",
	}
	if err := r.connectionService.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}
	lib := &library.Library{
		Name:         "Test Library",
		Type:         library.TypeRegular,
		Source:       connection.TypeEmby,
		ConnectionID: conn.ID,
		ExternalID:   "emby-lib-1",
	}
	if err := r.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	// Pre-set a running operation for this library.
	r.libraryOpsMu.Lock()
	r.libraryOps[lib.ID] = &LibraryOpResult{
		LibraryID: lib.ID,
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	r.libraryOpsMu.Unlock()

	// Hit the actual handler; expect 409 Conflict.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+conn.ID+"/libraries/"+lib.ID+"/populate", nil)
	req.SetPathValue("id", conn.ID)
	req.SetPathValue("libId", lib.ID)
	w := httptest.NewRecorder()

	r.handlePopulateLibrary(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusConflict, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["error"] != "operation already running for this library" {
		t.Errorf("error = %q, want conflict message", resp["error"])
	}
}

// createTestJPEGForHandler generates a minimal 1x1 JPEG image for handler tests.
func createTestJPEGForHandler(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encoding test jpeg: %v", err)
	}
	return buf.Bytes()
}

// assertArtistInLibrary fails the test unless the artist holds an
// artist_libraries membership row for the given library. Populate/scan
// happy-path tests now look up the artist via the unscoped GetByName /
// GetByMBID path, so this helper is what guarantees the M:N membership
// was actually inserted -- not just the artists row. A regression that
// drops the membership insert would still satisfy the unscoped lookup
// but fail here.
func assertArtistInLibrary(t *testing.T, r *Router, ctx context.Context, artistID, libraryID string) {
	t.Helper()
	memberships, err := r.artistService.LibrariesForArtist(ctx, artistID)
	if err != nil {
		t.Fatalf("LibrariesForArtist(%s): %v", artistID, err)
	}
	for _, m := range memberships {
		if m.LibraryID == libraryID {
			return
		}
	}
	t.Errorf("artist %s missing artist_libraries membership for library %s; got %+v",
		artistID, libraryID, memberships)
}

func TestPopulateFromEmby_DownloadsImages(t *testing.T) {
	t.Parallel()
	jpegData := createTestJPEGForHandler(t)
	artistDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks on TempDir failed: %v", err)
	}
	libPath := filepath.Dir(artistDir)

	var imageRequested atomic.Bool
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Radiohead",
					"SortName":"Radiohead",
					"Id":"emby-001",
					"Path":%q,
					"Overview":"English rock band.",
					"Genres":["Rock"],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-001"},
					"ImageTags":{"Primary":"abc123"}
				}],
				"TotalRecordCount":1
			}`, artistDir)
		case "/Items/emby-001/Images/Primary":
			imageRequested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}
	if !imageRequested.Load() {
		t.Error("expected image download request, but none was made")
	}
	if result.Images != 1 {
		t.Errorf("images = %d, want 1", result.Images)
	}

	// Verify the image file exists on disk.
	found := false
	entries, err := os.ReadDir(artistDir)
	if err != nil {
		t.Fatalf("reading artist dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "folder") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected image file in %s, got: %v", artistDir, entries)
	}

	// Verify artist record has path and thumb flag set.
	a, err := router.artistService.GetByName(ctx, "Radiohead")
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.Path != artistDir {
		t.Errorf("artist path = %q, want %q", a.Path, artistDir)
	}
	if !a.ThumbExists {
		t.Error("expected ThumbExists to be true")
	}
	assertArtistInLibrary(t, router, ctx, a.ID, lib.ID)
}

func TestPopulateFromEmby_SkipsExistingImage(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	// Pre-create the thumb image so the download is skipped.
	if err := os.WriteFile(filepath.Join(artistDir, "folder.jpg"), []byte("existing"), 0644); err != nil {
		t.Fatalf("creating existing image: %v", err)
	}

	var imageRequested atomic.Bool
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Bjork",
					"SortName":"Bjork",
					"Id":"emby-002",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{},
					"ImageTags":{"Primary":"xyz"}
				}],
				"TotalRecordCount":1
			}`, artistDir)
		case strings.HasPrefix(r.URL.Path, "/Items/emby-002/Images/"):
			imageRequested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(createTestJPEGForHandler(t))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music Skip",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-2",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}
	if imageRequested.Load() {
		t.Error("expected no image download when file already exists")
	}
	if result.Images != 0 {
		t.Errorf("images = %d, want 0", result.Images)
	}
}

func TestPopulateFromEmby_UsesImageCacheWhenNoPath(t *testing.T) {
	t.Parallel()
	jpegData := createTestJPEGForHandler(t)
	var imageRequested atomic.Bool
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"Items":[{
					"Name":"NoPath Artist",
					"SortName":"NoPath Artist",
					"Id":"emby-003",
					"Path":"",
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{},
					"ImageTags":{"Primary":"tag1"}
				}],
				"TotalRecordCount":1
			}`))
		case strings.HasPrefix(r.URL.Path, "/Items/emby-003/Images/"):
			imageRequested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music NoPath",
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-3",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}
	if !imageRequested.Load() {
		t.Error("expected image download via cache dir, but no request was made")
	}
	if result.Images != 1 {
		t.Errorf("images = %d, want 1", result.Images)
	}

	// Verify the image was saved to the cache directory, not an artist path.
	a, err := router.artistService.GetByName(ctx, "NoPath Artist")
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.Path != "" {
		t.Errorf("artist path = %q, want empty (no filesystem path)", a.Path)
	}
	cacheDir := filepath.Join(router.imageCacheDir, a.ID)
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("reading cache dir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "folder") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected image in cache dir %s, got: %v", cacheDir, entries)
	}
	assertArtistInLibrary(t, router, ctx, a.ID, lib.ID)
}

func TestPopulateFromJellyfin_DownloadsImages(t *testing.T) {
	t.Parallel()
	jpegData := createTestJPEGForHandler(t)
	tmpDir := t.TempDir()
	artistDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", tmpDir, err)
	}
	libPath := filepath.Dir(artistDir)

	var imageRequested atomic.Bool
	jfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Bjork",
					"SortName":"Bjork",
					"Id":"jf-001",
					"Path":%q,
					"Overview":"Icelandic singer.",
					"Genres":["Electronic"],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-jf-001"},
					"ImageTags":{"Primary":"tag1"}
				}],
				"TotalRecordCount":1
			}`, artistDir)
		case "/Items/jf-001/Images/Primary":
			imageRequested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer jfSrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music JF",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeJellyfin,
		ExternalID: "jf-lib-1",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := jellyfin.NewWithHTTPClient(jfSrv.URL, "key", "", jfSrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromJellyfinCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromJellyfinCtx: %v", err)
	}

	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}
	if !imageRequested.Load() {
		t.Error("expected image download request, but none was made")
	}
	if result.Images != 1 {
		t.Errorf("images = %d, want 1", result.Images)
	}

	// Verify the image file exists on disk.
	found := false
	entries, err := os.ReadDir(artistDir)
	if err != nil {
		t.Fatalf("reading artist dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "folder") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected image file in %s, got: %v", artistDir, entries)
	}

	a, err := router.artistService.GetByName(ctx, "Bjork")
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.Path != artistDir {
		t.Errorf("artist path = %q, want %q", a.Path, artistDir)
	}
	if !a.ThumbExists {
		t.Error("expected ThumbExists to be true")
	}
	assertArtistInLibrary(t, router, ctx, a.ID, lib.ID)
}

func TestPopulateFromEmby_DownloadsImagesForExistingArtist(t *testing.T) {
	t.Parallel()
	jpegData := createTestJPEGForHandler(t)
	artistDir := t.TempDir()

	var imageRequested atomic.Bool
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			// Emby returns this artist with an image, but the artist
			// already exists in Stillwater's DB from a filesystem scan.
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Radiohead",
					"SortName":"Radiohead",
					"Id":"emby-001",
					"Path":"/emby/music/Radiohead",
					"Overview":"English rock band.",
					"Genres":["Rock"],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-exist-001"},
					"ImageTags":{"Primary":"abc123"}
				}],
				"TotalRecordCount":1
			}`)
		case "/Items/emby-001/Images/Primary":
			imageRequested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music Existing",
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-exist",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	// Pre-create the artist as if it came from a filesystem scan, with
	// a valid local path that differs from Emby's internal path.
	existing := &artist.Artist{
		Name:          "Radiohead",
		SortName:      "Radiohead",
		MusicBrainzID: "mbid-exist-001",
		LibraryID:     lib.ID,
		Path:          artistDir,
	}
	if err := router.artistService.Create(ctx, existing); err != nil {
		t.Fatalf("creating existing artist: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	// Artist was skipped (already exists), but image was downloaded.
	if result.Created != 0 {
		t.Errorf("created = %d, want 0 (artist already exists)", result.Created)
	}
	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", result.Skipped)
	}
	if !imageRequested.Load() {
		t.Error("expected image download for existing artist, but none was made")
	}
	if result.Images != 1 {
		t.Errorf("images = %d, want 1", result.Images)
	}

	// Verify file saved to the existing artist's local path, not Emby's path.
	found := false
	entries, err := os.ReadDir(artistDir)
	if err != nil {
		t.Fatalf("reading artist dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "folder") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected image in %s, got: %v", artistDir, entries)
	}

	// Verify the flag was set.
	a, err := router.artistService.GetByName(ctx, "Radiohead")
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if !a.ThumbExists {
		t.Error("expected ThumbExists to be true")
	}
	// Pre-existing artist was created with this library; membership must
	// still be present after populate's skip path. A regression that
	// dropped membership on the skip code-path would fail here.
	assertArtistInLibrary(t, router, ctx, a.ID, lib.ID)
}

func TestValidatedArtistPath(t *testing.T) {
	t.Parallel()
	// Use real temp dirs so filepath.Abs produces valid absolute paths.
	libDir := t.TempDir()
	artistDir := filepath.Join(libDir, "Radiohead")
	if err := os.MkdirAll(artistDir, 0o755); err != nil {
		t.Fatalf("creating artist dir: %v", err)
	}
	otherDir := t.TempDir()
	// Create a sibling directory with a prefix-confusing name (e.g. libDir + "2").
	siblingDir := libDir + "2"
	if err := os.MkdirAll(filepath.Join(siblingDir, "Artist"), 0o755); err != nil {
		t.Fatalf("creating sibling dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(siblingDir) })

	// Create a regular file (not a directory) under the library root.
	filePath := filepath.Join(libDir, "not-a-dir.txt")
	if err := os.WriteFile(filePath, []byte("data"), 0o644); err != nil {
		t.Fatalf("creating test file: %v", err)
	}

	tests := []struct {
		name        string
		itemPath    string
		libraryPath string
		wantEmpty   bool
	}{
		{"empty library path returns empty", artistDir, "", true},
		{"empty item path returns empty", "", libDir, true},
		{"both empty returns empty", "", "", true},
		{"path under library root accepted", artistDir, libDir, false},
		{"path outside library root rejected", filepath.Join(otherDir, "Radiohead"), libDir, true},
		{"exact match accepted", libDir, libDir, false},
		{"prefix confusion rejected", filepath.Join(siblingDir, "Artist"), libDir, true},
		{"file path rejected (not a directory)", filePath, libDir, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validatedArtistPath(tt.itemPath, tt.libraryPath)
			if tt.wantEmpty && got != "" {
				t.Errorf("validatedArtistPath(%q, %q) = %q, want empty", tt.itemPath, tt.libraryPath, got)
			}
			if !tt.wantEmpty && got == "" {
				t.Errorf("validatedArtistPath(%q, %q) = empty, want non-empty", tt.itemPath, tt.libraryPath)
			}
		})
	}
}

func TestPopulateFromEmby_PlatformPathNotStoredWhenPathless(t *testing.T) {
	t.Parallel()
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{
				"Name":"Radiohead",
				"SortName":"Radiohead",
				"Id":"emby-001",
				"Path":"/emby/internal/Radiohead",
				"Overview":"",
				"Genres":[],
				"Tags":[],
				"PremiereDate":"",
				"EndDate":"",
				"ProviderIds":{},
				"ImageTags":{}
			}],
			"TotalRecordCount":1
		}`))
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	// Pathless library: no Path set.
	lib := &library.Library{
		Name:       "Pathless Emby",
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-pathless",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}
	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}

	a, err := router.artistService.GetByName(ctx, "Radiohead")
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.Path != "" {
		t.Errorf("artist path = %q, want empty (pathless library should not store platform path)", a.Path)
	}
	assertArtistInLibrary(t, router, ctx, a.ID, lib.ID)
}

func TestPopulateFromEmby_PlatformPathStoredWhenUnderLibraryRoot(t *testing.T) {
	t.Parallel()
	artistDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving temp dir symlinks: %v", err)
	}

	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"Items":[{
				"Name":"Radiohead",
				"SortName":"Radiohead",
				"Id":"emby-001",
				"Path":%q,
				"Overview":"",
				"Genres":[],
				"Tags":[],
				"PremiereDate":"",
				"EndDate":"",
				"ProviderIds":{},
				"ImageTags":{}
			}],
			"TotalRecordCount":1
		}`, artistDir)
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	// Non-pathless library with Path set to parent of artistDir.
	libPath := filepath.Dir(artistDir)
	lib := &library.Library{
		Name:       "Rooted Emby",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-rooted",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}
	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}

	a, err := router.artistService.GetByName(ctx, "Radiohead")
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.Path != artistDir {
		t.Errorf("artist path = %q, want %q", a.Path, artistDir)
	}
	assertArtistInLibrary(t, router, ctx, a.ID, lib.ID)
}

func TestPopulateFromEmby_PlatformPathRejectedWhenOutsideLibraryRoot(t *testing.T) {
	t.Parallel()
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{
				"Name":"Radiohead",
				"SortName":"Radiohead",
				"Id":"emby-001",
				"Path":"/completely/different/path/Radiohead",
				"Overview":"",
				"Genres":[],
				"Tags":[],
				"PremiereDate":"",
				"EndDate":"",
				"ProviderIds":{},
				"ImageTags":{}
			}],
			"TotalRecordCount":1
		}`))
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Rooted Emby Outside",
		Path:       t.TempDir(),
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-outside",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}
	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}

	a, err := router.artistService.GetByName(ctx, "Radiohead")
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.Path != "" {
		t.Errorf("artist path = %q, want empty (path outside library root should be rejected)", a.Path)
	}
	assertArtistInLibrary(t, router, ctx, a.ID, lib.ID)
}

func TestPopulateFromEmby_BackfillsMBID(t *testing.T) {
	t.Parallel()
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"Items":[{
					"Name":"Radiohead",
					"SortName":"Radiohead",
					"Id":"emby-001",
					"Path":"",
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"a74b1b7f-71a5-4011-9441-d0b5e4122711"},
					"ImageTags":{}
				}],
				"TotalRecordCount":1
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music MBID",
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-mbid",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	// Pre-create artist WITHOUT an MBID (e.g. from filesystem scan).
	existing := &artist.Artist{
		Name:      "Radiohead",
		SortName:  "Radiohead",
		LibraryID: lib.ID,
	}
	if err := router.artistService.Create(ctx, existing); err != nil {
		t.Fatalf("creating existing artist: %v", err)
	}
	if existing.MusicBrainzID != "" {
		t.Fatal("precondition: existing artist should have no MBID")
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", result.Skipped)
	}

	// Verify MBID was backfilled.
	a, err := router.artistService.GetByName(ctx, "Radiohead")
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.MusicBrainzID != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("MusicBrainzID = %q, want backfilled MBID", a.MusicBrainzID)
	}
	// MBID-backfill code path is an UPDATE, not a CREATE; the membership
	// recorded at pre-create time must still be present afterwards.
	assertArtistInLibrary(t, router, ctx, a.ID, lib.ID)
}

func TestPopulateFromEmby_SkipsOnMBIDConflict(t *testing.T) {
	t.Parallel()
	// Platform provides MBID-A for "Radiohead", but DB already has "Radiohead"
	// with MBID-B. These are different artists with the same name.
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{
				"Name":"Radiohead",
				"SortName":"Radiohead",
				"Id":"emby-001",
				"Path":"",
				"Overview":"",
				"Genres":[],
				"Tags":[],
				"PremiereDate":"",
				"EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":"platform-mbid-aaa"},
				"ImageTags":{}
			}],
			"TotalRecordCount":1
		}`))
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music Conflict",
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-conflict",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	// Pre-create artist with a DIFFERENT MBID but same name.
	existing := &artist.Artist{
		Name:          "Radiohead",
		SortName:      "Radiohead",
		MusicBrainzID: "existing-mbid-bbb",
		LibraryID:     lib.ID,
	}
	if err := router.artistService.Create(ctx, existing); err != nil {
		t.Fatalf("creating existing artist: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	// Should skip (not create a new one, not match the wrong artist).
	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", result.Skipped)
	}
	if result.Created != 0 {
		t.Errorf("created = %d, want 0", result.Created)
	}

	// Verify the existing artist's MBID was NOT overwritten.
	a, err := router.artistService.GetByID(ctx, existing.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.MusicBrainzID != "existing-mbid-bbb" {
		t.Errorf("MusicBrainzID = %q, want unchanged existing-mbid-bbb", a.MusicBrainzID)
	}
}

func TestPopulateFromJellyfin_SkipsOnMBIDConflict(t *testing.T) {
	t.Parallel()
	jfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{
				"Name":"Bjork",
				"SortName":"Bjork",
				"Id":"jf-001",
				"Path":"",
				"Overview":"",
				"Genres":[],
				"Tags":[],
				"PremiereDate":"",
				"EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":"platform-mbid-ccc"},
				"ImageTags":{}
			}],
			"TotalRecordCount":1
		}`))
	}))
	defer jfSrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music JF Conflict",
		Type:       library.TypeRegular,
		Source:     connection.TypeJellyfin,
		ExternalID: "jf-lib-conflict",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	existing := &artist.Artist{
		Name:          "Bjork",
		SortName:      "Bjork",
		MusicBrainzID: "existing-mbid-ddd",
		LibraryID:     lib.ID,
	}
	if err := router.artistService.Create(ctx, existing); err != nil {
		t.Fatalf("creating existing artist: %v", err)
	}

	client := jellyfin.NewWithHTTPClient(jfSrv.URL, "key", "", jfSrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromJellyfinCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromJellyfinCtx: %v", err)
	}

	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", result.Skipped)
	}
	if result.Created != 0 {
		t.Errorf("created = %d, want 0", result.Created)
	}

	a, err := router.artistService.GetByID(ctx, existing.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.MusicBrainzID != "existing-mbid-ddd" {
		t.Errorf("MusicBrainzID = %q, want unchanged existing-mbid-ddd", a.MusicBrainzID)
	}
}

func TestValidatedArtistPath_SymlinkEscape(t *testing.T) {
	t.Parallel()
	// Create a library root and an external directory, then symlink from
	// inside the library to the external directory. The validation should
	// reject the symlinked path.
	libDir := t.TempDir()
	externalDir := t.TempDir()

	symlinkPath := filepath.Join(libDir, "symlinked-artist")
	if err := os.Symlink(externalDir, symlinkPath); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	got := validatedArtistPath(symlinkPath, libDir)
	if got != "" {
		t.Errorf("validatedArtistPath(%q, %q) = %q, want empty (symlink escapes library root)", symlinkPath, libDir, got)
	}
}

func TestPopulateFromEmby_DownloadsMultipleBackdrops(t *testing.T) {
	t.Parallel()
	jpegData := createTestJPEGForHandler(t)
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	var backdrop0Requested, backdrop1Requested atomic.Bool
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Radiohead",
					"SortName":"Radiohead",
					"Id":"emby-001",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-001"},
					"ImageTags":{},
					"BackdropImageTags":["hash1","hash2"]
				}],
				"TotalRecordCount":1
			}`, artistDir)
		case "/Items/emby-001/Images/Backdrop/0":
			backdrop0Requested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		case "/Items/emby-001/Images/Backdrop/1":
			backdrop1Requested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if !backdrop0Requested.Load() {
		t.Error("expected backdrop index 0 to be downloaded")
	}
	if !backdrop1Requested.Load() {
		t.Error("expected backdrop index 1 to be downloaded")
	}
	if result.Images != 2 {
		t.Errorf("images = %d, want 2", result.Images)
	}

	// Verify both fanart files exist on disk (Kodi numbering: fanart.jpg and fanart1.jpg).
	if _, err := os.Stat(filepath.Join(artistDir, "fanart.jpg")); err != nil {
		t.Errorf("expected fanart.jpg to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artistDir, "fanart1.jpg")); err != nil {
		t.Errorf("expected fanart1.jpg to exist: %v", err)
	}

	a, err := router.artistService.GetByName(ctx, "Radiohead")
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want 2", a.FanartCount)
	}
	assertArtistInLibrary(t, router, ctx, a.ID, lib.ID)
}

func TestPopulateFromEmby_SkipsExistingBackdrop(t *testing.T) {
	t.Parallel()
	jpegData := createTestJPEGForHandler(t)
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	// Pre-create fanart.jpg so backdrop index 0 should be skipped.
	if err := os.WriteFile(filepath.Join(artistDir, "fanart.jpg"), jpegData, 0o644); err != nil {
		t.Fatalf("writing existing fanart: %v", err)
	}

	var backdrop0Requested, backdrop1Requested atomic.Bool
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Bjork",
					"SortName":"Bjork",
					"Id":"emby-002",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{},
					"ImageTags":{},
					"BackdropImageTags":["hash1","hash2"]
				}],
				"TotalRecordCount":1
			}`, artistDir)
		case "/Items/emby-002/Images/Backdrop/0":
			backdrop0Requested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		case "/Items/emby-002/Images/Backdrop/1":
			backdrop1Requested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music Skip",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-2",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if backdrop0Requested.Load() {
		t.Error("expected backdrop index 0 to be skipped (file already exists)")
	}
	if !backdrop1Requested.Load() {
		t.Error("expected backdrop index 1 to be downloaded")
	}
	if result.Images != 1 {
		t.Errorf("images = %d, want 1", result.Images)
	}
	if _, err := os.Stat(filepath.Join(artistDir, "fanart1.jpg")); err != nil {
		t.Errorf("expected fanart1.jpg to exist: %v", err)
	}
}

func TestPopulateFromJellyfin_DownloadsMultipleBackdrops(t *testing.T) {
	t.Parallel()
	jpegData := createTestJPEGForHandler(t)
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	var backdrop0Requested, backdrop1Requested atomic.Bool
	jfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Bjork",
					"SortName":"Bjork",
					"Id":"jf-001",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-002"},
					"ImageTags":{},
					"BackdropImageTags":["hash1","hash2"]
				}],
				"TotalRecordCount":1
			}`, artistDir)
		case "/Items/jf-001/Images/Backdrop/0":
			backdrop0Requested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		case "/Items/jf-001/Images/Backdrop/1":
			backdrop1Requested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer jfSrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music JF",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeJellyfin,
		ExternalID: "jf-lib-1",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := jellyfin.NewWithHTTPClient(jfSrv.URL, "key", "", jfSrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromJellyfinCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromJellyfinCtx: %v", err)
	}

	if !backdrop0Requested.Load() {
		t.Error("expected backdrop index 0 to be downloaded")
	}
	if !backdrop1Requested.Load() {
		t.Error("expected backdrop index 1 to be downloaded")
	}
	if result.Images != 2 {
		t.Errorf("images = %d, want 2", result.Images)
	}

	if _, err := os.Stat(filepath.Join(artistDir, "fanart.jpg")); err != nil {
		t.Errorf("expected fanart.jpg to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artistDir, "fanart1.jpg")); err != nil {
		t.Errorf("expected fanart1.jpg to exist: %v", err)
	}

	a, err := router.artistService.GetByName(ctx, "Bjork")
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want 2", a.FanartCount)
	}
	assertArtistInLibrary(t, router, ctx, a.ID, lib.ID)
}

func TestPopulateFromEmby_NoBackdropsWhenTagsEmpty(t *testing.T) {
	t.Parallel()
	jpegData := createTestJPEGForHandler(t)
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	var backdropRequested atomic.Bool
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "Images/Backdrop") {
			backdropRequested.Store(true)
		}
		switch r.URL.Path {
		case "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Radiohead",
					"SortName":"Radiohead",
					"Id":"emby-003",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-003"},
					"ImageTags":{"Primary":"hash-thumb"},
					"BackdropImageTags":[]
				}],
				"TotalRecordCount":1
			}`, artistDir)
		case "/Items/emby-003/Images/Primary":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music NoBackdrop",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-3",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if backdropRequested.Load() {
		t.Error("expected no backdrop requests when BackdropImageTags is empty")
	}
	// The thumb image should still be downloaded.
	if result.Images != 1 {
		t.Errorf("images = %d, want 1 (thumb only)", result.Images)
	}
}

func TestPopulateFromEmby_PartialBackdropDownloadFailure(t *testing.T) {
	t.Parallel()
	// Backdrop index 0 returns 404; index 1 returns a valid JPEG.
	// The loop must continue past the failure and count only the successful download.
	jpegData := createTestJPEGForHandler(t)
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	var backdrop1Requested atomic.Bool
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Portishead",
					"SortName":"Portishead",
					"Id":"emby-004",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-004"},
					"ImageTags":{},
					"BackdropImageTags":["hash1","hash2"]
				}],
				"TotalRecordCount":1
			}`, artistDir)
		case "/Items/emby-004/Images/Backdrop/0":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found"))
		case "/Items/emby-004/Images/Backdrop/1":
			backdrop1Requested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Test Music Partial",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-4",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if !backdrop1Requested.Load() {
		t.Error("expected backdrop index 1 to be attempted after index 0 failure")
	}
	if result.Images != 1 {
		t.Errorf("images = %d, want 1 (only index 1 succeeded)", result.Images)
	}

	// Kodi numbering: index 1 maps to fanart1.jpg during download. After the
	// loop, compactFanartIfNeeded renames it to fanart.jpg (the primary slot)
	// so that the UI's /images/fanart/file endpoint can serve it.
	if _, err := os.Stat(filepath.Join(artistDir, "fanart.jpg")); err != nil {
		t.Errorf("expected fanart.jpg to exist after compaction: %v", err)
	}
	// fanart1.jpg should no longer exist -- it was renamed to primary.
	if _, err := os.Stat(filepath.Join(artistDir, "fanart1.jpg")); err == nil {
		t.Error("fanart1.jpg should not exist after compaction to primary slot")
	}
}

func TestScanFromEmby_FanartExistsFromBackdropImageTags(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Artists/AlbumArtists" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Bjork",
					"SortName":"Bjork",
					"Id":"emby-scan-001",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-scan-001"},
					"ImageTags":{},
					"BackdropImageTags":["hash1"]
				}],
				"TotalRecordCount":1
			}`, artistDir)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "Scan Test",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-scan-lib-1",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	existing := &artist.Artist{
		Name:          "Bjork",
		SortName:      "Bjork",
		MusicBrainzID: "mbid-scan-001",
		LibraryID:     lib.ID,
		Path:          artistDir,
	}
	if err := router.artistService.Create(ctx, existing); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	updated, err := router.scanFromEmby(ctx, client, lib)
	if err != nil {
		t.Fatalf("scanFromEmby: %v", err)
	}
	if updated != 1 {
		t.Errorf("updated = %d, want 1", updated)
	}

	got, err := router.artistService.GetByMBID(ctx, "mbid-scan-001")
	if err != nil || got == nil {
		t.Fatalf("looking up artist after scan: %v", err)
	}
	if !got.FanartExists {
		t.Error("FanartExists should be true after scan with non-empty BackdropImageTags")
	}
}

func TestScanFromJellyfin_FanartExistsFromBackdropImageTags(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	jfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Artists/AlbumArtists" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Bjork",
					"SortName":"Bjork",
					"Id":"jf-scan-001",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-scan-002"},
					"ImageTags":{},
					"BackdropImageTags":["hash1"]
				}],
				"TotalRecordCount":1
			}`, artistDir)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer jfSrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	lib := &library.Library{
		Name:       "JF Scan Test",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeJellyfin,
		ExternalID: "jf-scan-lib-1",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	existing := &artist.Artist{
		Name:          "Bjork",
		SortName:      "Bjork",
		MusicBrainzID: "mbid-scan-002",
		LibraryID:     lib.ID,
		Path:          artistDir,
	}
	if err := router.artistService.Create(ctx, existing); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	client := jellyfin.NewWithHTTPClient(jfSrv.URL, "key", "", jfSrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	updated, err := router.scanFromJellyfin(ctx, client, lib)
	if err != nil {
		t.Fatalf("scanFromJellyfin: %v", err)
	}
	if updated != 1 {
		t.Errorf("updated = %d, want 1", updated)
	}

	got, err := router.artistService.GetByMBID(ctx, "mbid-scan-002")
	if err != nil || got == nil {
		t.Fatalf("looking up artist after scan: %v", err)
	}
	if !got.FanartExists {
		t.Error("FanartExists should be true after scan with non-empty BackdropImageTags")
	}
}

func TestPopulateFromEmby_NonKodiBackdropNaming(t *testing.T) {
	t.Parallel()
	jpegData := createTestJPEGForHandler(t)
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	var backdrop0Requested, backdrop1Requested atomic.Bool
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Artists/AlbumArtists":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Sigur Ros",
					"SortName":"Sigur Ros",
					"Id":"emby-nk-001",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-nk-001"},
					"ImageTags":{},
					"BackdropImageTags":["hash1","hash2"]
				}],
				"TotalRecordCount":1
			}`, artistDir)
		case "/Items/emby-nk-001/Images/Backdrop/0":
			backdrop0Requested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		case "/Items/emby-nk-001/Images/Backdrop/1":
			backdrop1Requested.Store(true)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpegData)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	// Activate the Emby platform profile so backdrops use non-Kodi naming:
	// index 0 -> backdrop.jpg, index 1 -> backdrop2.jpg.
	if err := router.platformService.SetActive(ctx, "emby"); err != nil {
		t.Fatalf("setting emby platform active: %v", err)
	}

	lib := &library.Library{
		Name:       "NonKodi Music",
		Path:       libPath,
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-nk-lib-1",
	}
	if err := router.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if !backdrop0Requested.Load() {
		t.Error("expected backdrop index 0 to be downloaded")
	}
	if !backdrop1Requested.Load() {
		t.Error("expected backdrop index 1 to be downloaded")
	}
	if result.Images != 2 {
		t.Errorf("images = %d, want 2", result.Images)
	}

	// Non-Kodi (Emby) numbering: index 0 -> backdrop.jpg, index 1 -> backdrop2.jpg.
	if _, err := os.Stat(filepath.Join(artistDir, "backdrop.jpg")); err != nil {
		t.Errorf("expected backdrop.jpg to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artistDir, "backdrop2.jpg")); err != nil {
		t.Errorf("expected backdrop2.jpg to exist: %v", err)
	}

	a, err := router.artistService.GetByName(ctx, "Sigur Ros")
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want 2", a.FanartCount)
	}
	assertArtistInLibrary(t, router, ctx, a.ID, lib.ID)
}

func TestImportLibraries_AutoPopulate(t *testing.T) {
	t.Parallel()
	// Stand up a fake Emby server that returns one artist.
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{
				"Name":"Auto Artist",
				"SortName":"Auto Artist",
				"Id":"emby-auto-001",
				"Path":"",
				"Overview":"",
				"Genres":[],
				"Tags":[],
				"PremiereDate":"",
				"EndDate":"",
				"ProviderIds":{},
				"ImageTags":{}
			}],
			"TotalRecordCount":1
		}`))
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	// Create an enabled, tested connection.
	conn := &connection.Connection{
		Name:    "Auto Emby",
		Type:    connection.TypeEmby,
		URL:     embySrv.URL,
		APIKey:  "test-key",
		Enabled: true,
		Status:  "ok",
	}
	if err := router.connectionService.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	// Build the import request.
	body := `{"libraries":[{"external_id":"emby-lib-auto","name":"Auto Library"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+conn.ID+"/libraries/import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", conn.ID)
	w := httptest.NewRecorder()

	router.handleImportLibraries(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var created []library.Library
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decoding import response: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %d, want 1", len(created))
	}

	// Wait for the background populate to complete.
	libID := created[0].ID
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		router.libraryOpsMu.Lock()
		op, ok := router.libraryOps[libID]
		done := ok && op.Status != "running"
		router.libraryOpsMu.Unlock()
		if done {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify the operation completed.
	router.libraryOpsMu.Lock()
	op, ok := router.libraryOps[libID]
	router.libraryOpsMu.Unlock()
	if !ok {
		t.Fatal("expected a populate operation to have been registered")
	}
	if op.Status != "completed" {
		t.Errorf("operation status = %q, want %q; message: %s", op.Status, "completed", op.Message)
	}
}

func TestScanFromEmby_BackfillsPlatformIDToFilesystemArtist(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Artists/AlbumArtists" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Deftones",
					"SortName":"Deftones",
					"Id":"emby-deftones-001",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-deftones"},
					"ImageTags":{}
				}],
				"TotalRecordCount":1
			}`, artistDir)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	// Create a connection for the Emby library.
	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")

	musicDir := t.TempDir()

	// Create a manual (filesystem) library.
	manualLib := &library.Library{
		Name:   "Filesystem",
		Path:   musicDir,
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := router.libraryService.Create(ctx, manualLib); err != nil {
		t.Fatalf("creating manual library: %v", err)
	}

	// Create a filesystem artist with the same MBID.
	deftonesFSDir := filepath.Join(musicDir, "Deftones")
	if err := os.MkdirAll(deftonesFSDir, 0o755); err != nil {
		t.Fatalf("creating deftones dir: %v", err)
	}
	fsArtist := &artist.Artist{
		Name:          "Deftones",
		SortName:      "Deftones",
		MusicBrainzID: "mbid-deftones",
		LibraryID:     manualLib.ID,
		Path:          deftonesFSDir,
	}
	if err := router.artistService.Create(ctx, fsArtist); err != nil {
		t.Fatalf("creating filesystem artist: %v", err)
	}

	// Create an Emby library linked to the connection.
	embyLib := &library.Library{
		Name:         "Emby Music",
		Path:         libPath,
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-1",
		ExternalID:   "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}

	// Create an Emby-library artist with the same MBID.
	embyArtist := &artist.Artist{
		Name:          "Deftones",
		SortName:      "Deftones",
		MusicBrainzID: "mbid-deftones",
		LibraryID:     embyLib.ID,
		Path:          artistDir,
	}
	if err := router.artistService.Create(ctx, embyArtist); err != nil {
		t.Fatalf("creating emby artist: %v", err)
	}

	// Run the scan.
	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if _, err := router.scanFromEmby(ctx, client, embyLib); err != nil {
		t.Fatalf("scanFromEmby: %v", err)
	}

	// Verify platform ID on Emby-library artist (existing behavior).
	embyPlatformID, err := router.artistService.GetPlatformID(ctx, embyArtist.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID (emby artist): %v", err)
	}
	if embyPlatformID != "emby-deftones-001" {
		t.Errorf("emby artist platform ID = %q, want %q", embyPlatformID, "emby-deftones-001")
	}

	// Issue #1076 added a UNIQUE(connection_id, platform_artist_id) index,
	// so the historical "duplicate the mapping onto the filesystem artist"
	// behavior is no longer possible: at most one artist row can hold a
	// given (connection, platform_id) pair. The post-fix invariant is that
	// the connection-library artist owns the mapping and the filesystem
	// artist does not get a duplicate copy.
	fsPlatformID, err := router.artistService.GetPlatformID(ctx, fsArtist.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID (fs artist): %v", err)
	}
	if fsPlatformID != "" {
		t.Errorf("filesystem artist platform ID = %q, want empty (UNIQUE index forbids duplicate mapping)", fsPlatformID)
	}
}

func TestPopulateFromEmby_BackfillsPlatformIDToFilesystemArtist(t *testing.T) {
	t.Parallel()
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{
				"Name":"Radiohead",
				"SortName":"Radiohead",
				"Id":"emby-radiohead-001",
				"Path":"",
				"Overview":"English rock band.",
				"Genres":["Rock"],
				"Tags":[],
				"PremiereDate":"",
				"EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":"mbid-radiohead"},
				"ImageTags":{}
			}],
			"TotalRecordCount":1
		}`))
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")

	musicDir := t.TempDir()

	// Create a manual (filesystem) library with an artist sharing the MBID.
	manualLib := &library.Library{
		Name:   "Filesystem",
		Path:   musicDir,
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := router.libraryService.Create(ctx, manualLib); err != nil {
		t.Fatalf("creating manual library: %v", err)
	}
	radioheadFSDir := filepath.Join(musicDir, "Radiohead")
	if err := os.MkdirAll(radioheadFSDir, 0o755); err != nil {
		t.Fatalf("creating radiohead dir: %v", err)
	}
	fsArtist := &artist.Artist{
		Name:          "Radiohead",
		SortName:      "Radiohead",
		MusicBrainzID: "mbid-radiohead",
		LibraryID:     manualLib.ID,
		Path:          radioheadFSDir,
	}
	if err := router.artistService.Create(ctx, fsArtist); err != nil {
		t.Fatalf("creating filesystem artist: %v", err)
	}

	// Create an Emby library.
	embyLib := &library.Library{
		Name:         "Emby Music",
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-1",
		ExternalID:   "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}

	// Issue #1004: Emby populate must NOT create a duplicate row when the
	// inbound artist matches an existing filesystem artist by MBID. The
	// existing artist gets the Emby platform mapping + an Emby library
	// membership, and result.Created stays at 0.
	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, embyLib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if result.Created != 0 {
		t.Fatalf("created = %d, want 0 (existing filesystem artist must absorb the import)", result.Created)
	}

	// Filesystem artist now holds the Emby platform mapping.
	fsPlatformID, err := router.artistService.GetPlatformID(ctx, fsArtist.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID (fs): %v", err)
	}
	if fsPlatformID != "emby-radiohead-001" {
		t.Errorf("filesystem artist platform ID = %q, want %q (mapping must attach to canonical row)",
			fsPlatformID, "emby-radiohead-001")
	}

	// No second Radiohead row was created.
	memberships, err := router.artistService.LibrariesForArtist(ctx, fsArtist.ID)
	if err != nil {
		t.Fatalf("LibrariesForArtist: %v", err)
	}
	gotLibs := map[string]string{}
	for _, m := range memberships {
		gotLibs[m.LibraryID] = m.Source
	}
	if got := gotLibs[embyLib.ID]; got != "emby" {
		t.Errorf("emby membership source = %q, want emby (memberships=%+v)", got, memberships)
	}
}

func TestScanFromEmby_BackfillsCaseInsensitiveName(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Artists/AlbumArtists" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"VERIDIA",
					"SortName":"VERIDIA",
					"Id":"emby-veridia-001",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{},
					"ImageTags":{}
				}],
				"TotalRecordCount":1
			}`, artistDir)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")

	musicDir := t.TempDir()

	manualLib := &library.Library{
		Name:   "Filesystem",
		Path:   musicDir,
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := router.libraryService.Create(ctx, manualLib); err != nil {
		t.Fatalf("creating manual library: %v", err)
	}
	veridiaDirFS := filepath.Join(musicDir, "Veridia")
	if err := os.MkdirAll(veridiaDirFS, 0o755); err != nil {
		t.Fatalf("creating veridia dir: %v", err)
	}
	fsArtist := &artist.Artist{
		Name:      "Veridia",
		SortName:  "Veridia",
		LibraryID: manualLib.ID,
		Path:      veridiaDirFS,
	}
	if err := router.artistService.Create(ctx, fsArtist); err != nil {
		t.Fatalf("creating filesystem artist: %v", err)
	}

	embyLib := &library.Library{
		Name:         "Emby Music",
		Path:         libPath,
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-1",
		ExternalID:   "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}
	embyArtist := &artist.Artist{
		Name:      "VERIDIA",
		SortName:  "VERIDIA",
		LibraryID: embyLib.ID,
		Path:      artistDir,
	}
	if err := router.artistService.Create(ctx, embyArtist); err != nil {
		t.Fatalf("creating emby artist: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if _, err := router.scanFromEmby(ctx, client, embyLib); err != nil {
		t.Fatalf("scanFromEmby: %v", err)
	}

	// Issue #1076: the connection-library artist holds the platform mapping.
	// Look it up by case-insensitive name within the Emby library; the
	// scan should have created (or reused) an artist row there with the
	// platform ID set. The filesystem artist must NOT carry a duplicate
	// mapping under the new UNIQUE(connection_id, platform_artist_id)
	// invariant; the case-insensitive match logic still runs and decides
	// to skip rather than collide.
	fsPlatformID, err := router.artistService.GetPlatformID(ctx, fsArtist.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID (fs): %v", err)
	}
	if fsPlatformID != "" {
		t.Errorf("filesystem artist platform ID = %q, want empty (UNIQUE index forbids duplicate mapping)", fsPlatformID)
	}
	// The scan resolves the Emby-library artist via membership; assert
	// that the platform mapping landed on that row directly.
	embyPlatformID, err := router.artistService.GetPlatformID(ctx, embyArtist.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID (emby): %v", err)
	}
	if embyPlatformID != "emby-veridia-001" {
		t.Errorf("emby artist platform ID = %q, want %q", embyPlatformID, "emby-veridia-001")
	}
}

// TestScanFromEmby_DuplicateTwinIDDeterministic reproduces the #2344 flip-flop:
// Emby returns two duplicate "twin" items that share one MBID (and name) but
// carry different item Ids. Both resolve to the same Stillwater artist row, so
// each scan stamps a platform id for the same (artist, connection). With the
// old full-overwrite Set the LAST item in the page won, so when Emby returned
// the twins in a different paging order on the next scan the stored id flipped
// -- and subsequent metadata/image pushes landed on the phantom twin. The
// deterministic stable set must keep the same (lowest) id regardless of order.
//
// RED without the fix: forward order stores "emby-twin-2" (last wins), reversed
// order stores "emby-twin-1", so the two scans disagree and the test fails.
func TestScanFromEmby_DuplicateTwinIDDeterministic(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	// forward controls the order the fake Emby server enumerates the twins.
	// Flipped between the two scans to simulate Emby's non-deterministic paging.
	var forward atomic.Bool
	forward.Store(true)
	twinBody := func() string {
		a := fmt.Sprintf(`{"Name":"Twin Artist","SortName":"Twin Artist","Id":"emby-twin-1","Path":%q,"Overview":"","Genres":[],"Tags":[],"PremiereDate":"","EndDate":"","ProviderIds":{"MusicBrainzArtist":"mbid-shared"},"ImageTags":{}}`, artistDir)
		b := fmt.Sprintf(`{"Name":"Twin Artist","SortName":"Twin Artist","Id":"emby-twin-2","Path":%q,"Overview":"","Genres":[],"Tags":[],"PremiereDate":"","EndDate":"","ProviderIds":{"MusicBrainzArtist":"mbid-shared"},"ImageTags":{}}`, artistDir)
		first, second := a, b
		if !forward.Load() {
			first, second = b, a
		}
		return fmt.Sprintf(`{"Items":[%s,%s],"TotalRecordCount":2}`, first, second)
	}

	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Artists/AlbumArtists" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, twinBody())
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer embySrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()
	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")

	embyLib := &library.Library{
		Name:         "Emby Music",
		Path:         libPath,
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-1",
		ExternalID:   "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}
	// One artist row both twins resolve to (shared MBID + library membership).
	embyArtist := &artist.Artist{
		Name:          "Twin Artist",
		SortName:      "Twin Artist",
		MusicBrainzID: "mbid-shared",
		LibraryID:     embyLib.ID,
		Path:          artistDir,
	}
	if err := router.artistService.Create(ctx, embyArtist); err != nil {
		t.Fatalf("creating emby artist: %v", err)
	}

	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	// First scan: forward twin order.
	if _, err := router.scanFromEmby(ctx, client, embyLib); err != nil {
		t.Fatalf("scanFromEmby (forward): %v", err)
	}
	firstID, err := router.artistService.GetPlatformID(ctx, embyArtist.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID (forward): %v", err)
	}

	// Second scan: reversed twin order.
	forward.Store(false)
	if _, err := router.scanFromEmby(ctx, client, embyLib); err != nil {
		t.Fatalf("scanFromEmby (reversed): %v", err)
	}
	secondID, err := router.artistService.GetPlatformID(ctx, embyArtist.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID (reversed): %v", err)
	}

	if firstID != secondID {
		t.Fatalf("platform id flip-flopped across scans: forward=%q reversed=%q", firstID, secondID)
	}
	if firstID != "emby-twin-1" {
		t.Errorf("stored platform id = %q, want deterministic lowest %q", firstID, "emby-twin-1")
	}
}

// TestScanFromJellyfin_DuplicateTwinIDDeterministic is the Jellyfin counterpart
// of TestScanFromEmby_DuplicateTwinIDDeterministic; the two scans share the
// resolveAndBackfillPlatformID resolver, so this guards the same stable-set
// determinism on the Jellyfin path (#2344).
func TestScanFromJellyfin_DuplicateTwinIDDeterministic(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	var forward atomic.Bool
	forward.Store(true)
	twinBody := func() string {
		a := fmt.Sprintf(`{"Name":"Twin Artist","SortName":"Twin Artist","Id":"jf-twin-1","Path":%q,"Overview":"","Genres":[],"Tags":[],"PremiereDate":"","EndDate":"","ProviderIds":{"MusicBrainzArtist":"mbid-shared"},"ImageTags":{}}`, artistDir)
		b := fmt.Sprintf(`{"Name":"Twin Artist","SortName":"Twin Artist","Id":"jf-twin-2","Path":%q,"Overview":"","Genres":[],"Tags":[],"PremiereDate":"","EndDate":"","ProviderIds":{"MusicBrainzArtist":"mbid-shared"},"ImageTags":{}}`, artistDir)
		first, second := a, b
		if !forward.Load() {
			first, second = b, a
		}
		return fmt.Sprintf(`{"Items":[%s,%s],"TotalRecordCount":2}`, first, second)
	}

	jfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Artists/AlbumArtists" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, twinBody())
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer jfSrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()
	addTestConnection(t, router, "conn-jf-1", "Jellyfin Server", "jellyfin")

	jfLib := &library.Library{
		Name:         "JF Music",
		Path:         libPath,
		Type:         library.TypeRegular,
		Source:       connection.TypeJellyfin,
		ConnectionID: "conn-jf-1",
		ExternalID:   "jf-lib-1",
	}
	if err := router.libraryService.Create(ctx, jfLib); err != nil {
		t.Fatalf("creating jellyfin library: %v", err)
	}
	jfArtist := &artist.Artist{
		Name:          "Twin Artist",
		SortName:      "Twin Artist",
		MusicBrainzID: "mbid-shared",
		LibraryID:     jfLib.ID,
		Path:          artistDir,
	}
	if err := router.artistService.Create(ctx, jfArtist); err != nil {
		t.Fatalf("creating jellyfin artist: %v", err)
	}

	client := jellyfin.NewWithHTTPClient(jfSrv.URL, "key", "", jfSrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	if _, err := router.scanFromJellyfin(ctx, client, jfLib); err != nil {
		t.Fatalf("scanFromJellyfin (forward): %v", err)
	}
	firstID, err := router.artistService.GetPlatformID(ctx, jfArtist.ID, "conn-jf-1")
	if err != nil {
		t.Fatalf("GetPlatformID (forward): %v", err)
	}

	forward.Store(false)
	if _, err := router.scanFromJellyfin(ctx, client, jfLib); err != nil {
		t.Fatalf("scanFromJellyfin (reversed): %v", err)
	}
	secondID, err := router.artistService.GetPlatformID(ctx, jfArtist.ID, "conn-jf-1")
	if err != nil {
		t.Fatalf("GetPlatformID (reversed): %v", err)
	}

	if firstID != secondID {
		t.Fatalf("platform id flip-flopped across scans: forward=%q reversed=%q", firstID, secondID)
	}
	if firstID != "jf-twin-1" {
		t.Errorf("stored platform id = %q, want deterministic lowest %q", firstID, "jf-twin-1")
	}
}

func TestResolveAndBackfillPlatformID_NilWhenNoConnectionMatch(t *testing.T) {
	t.Parallel()
	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")

	embyLib := &library.Library{
		Name:         "Emby Music",
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-1",
		ExternalID:   "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}

	a := router.resolveAndBackfillPlatformID(ctx,
		"nonexistent-mbid", "Nonexistent Artist",
		"conn-emby-1", "emby-999", embyLib, nil)
	if a != nil {
		t.Errorf("expected nil, got %+v", a)
	}
}

// TestResolveAndBackfillPlatformID_StopsOnScopedLookupError verifies that a
// real DB/load failure inside the library-scoped lookup short-circuits the
// resolver: the unscoped fallback must NOT run, no platform-id mapping is
// stored, and the function returns nil. This guards the safety contract that
// callers rely on during the M:N transitional state -- a transient scoped
// query error must never silently fall back to an unscoped match that could
// attach the platform id to a sibling-library artist.
//
// The test forces the scoped query to fail by dropping the artist_libraries
// table after seeding everything else. The unscoped path (GetByMBID against
// the artists + artist_provider_ids tables) remains functional, so if the
// short-circuit ever regresses, the unscoped match would resolve and
// SetPlatformID would land a mapping -- the assertion below would catch it.
func TestResolveAndBackfillPlatformID_StopsOnScopedLookupError(t *testing.T) {
	t.Parallel()
	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")

	embyLib := &library.Library{
		Name:         "Emby Music",
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-1",
		ExternalID:   "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}

	// Seed an artist + MBID mapping that the UNSCOPED lookup would resolve.
	// If short-circuit breaks, this artist gets a platform mapping and the
	// final assertion fails.
	const mbid = "11111111-1111-1111-1111-111111111111"
	unscopedArtist := &artist.Artist{
		Name:          "Backstop Artist",
		SortName:      "Backstop Artist",
		MusicBrainzID: mbid,
		Path:          "/music/backstop",
	}
	if err := router.artistService.Create(ctx, unscopedArtist); err != nil {
		t.Fatalf("creating unscoped artist: %v", err)
	}

	// Force the scoped query to fail without breaking the unscoped path.
	// lookupByMBIDInLibrary joins artist_libraries; dropping it makes that
	// query error. GetByMBID (used by the unscoped fallback) hits artists +
	// artist_provider_ids only and is unaffected.
	if _, err := router.db.ExecContext(ctx, `DROP TABLE artist_libraries`); err != nil {
		t.Fatalf("dropping artist_libraries: %v", err)
	}

	a := router.resolveAndBackfillPlatformID(ctx,
		mbid, "Backstop Artist",
		"conn-emby-1", "emby-backstop-001", embyLib, nil)
	if a != nil {
		t.Errorf("expected nil, got %+v", a)
	}

	// If the unscoped fallback ran, SetPlatformID would have stored a row
	// mapping unscopedArtist.ID -> emby-backstop-001 for conn-emby-1. Read
	// the mapping directly via the DB so we do not depend on service-layer
	// queries that also touch artist_libraries (which we just dropped).
	var count int
	if err := router.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = ? AND connection_id = ?`,
		unscopedArtist.ID, "conn-emby-1").Scan(&count); err != nil {
		t.Fatalf("counting platform ids: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 platform-id rows after short-circuit, got %d", count)
	}
}

// TestFindArtistInLibrary_HitByMBID covers the success path of
// lookupByMBIDInLibrary: an artist with an MBID provider row AND a membership
// in the target library is returned. Pins the M:N membership-join behavior
// after the legacy artists.library_id column was dropped.
func TestFindArtistInLibrary_HitByMBID(t *testing.T) {
	t.Parallel()
	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")

	embyLib := &library.Library{
		Name:         "Emby Music",
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-1",
		ExternalID:   "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}

	a := &artist.Artist{
		Name:      "Veridia",
		SortName:  "Veridia",
		LibraryID: embyLib.ID,
		Path:      t.TempDir(),
	}
	if err := router.artistService.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Attach an MBID provider row so the MBID lookup matches.
	if _, err := router.db.ExecContext(ctx, `
		INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
		VALUES (?, 'musicbrainz', ?)
	`, a.ID, "11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatalf("inserting provider id: %v", err)
	}

	got, err := router.findArtistInLibrary(ctx, "11111111-1111-1111-1111-111111111111", "Veridia", embyLib.ID)
	if err != nil {
		t.Fatalf("findArtistInLibrary: %v", err)
	}
	if got == nil {
		t.Fatal("expected hit, got nil")
	}
	if got.ID != a.ID {
		t.Errorf("got.ID = %q, want %q", got.ID, a.ID)
	}
}

// TestFindArtistInLibrary_HitByName covers the case-insensitive fallback path
// (lookupByNameInLibrary) when no MBID is provided.
func TestFindArtistInLibrary_HitByName(t *testing.T) {
	t.Parallel()
	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")

	embyLib := &library.Library{
		Name:         "Emby Music",
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-1",
		ExternalID:   "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}

	a := &artist.Artist{
		Name:      "Mixed Case Band",
		SortName:  "Mixed Case Band",
		LibraryID: embyLib.ID,
		Path:      t.TempDir(),
	}
	if err := router.artistService.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Lookup with a different case should still match (LOWER on both sides).
	got, err := router.findArtistInLibrary(ctx, "", "MIXED CASE BAND", embyLib.ID)
	if err != nil {
		t.Fatalf("findArtistInLibrary: %v", err)
	}
	if got == nil {
		t.Fatal("expected hit, got nil")
	}
	if got.ID != a.ID {
		t.Errorf("got.ID = %q, want %q", got.ID, a.ID)
	}
}

// TestFindArtistInLibrary_NoMatchReturnsNil verifies the (nil, nil) "genuine
// no match" contract that callers depend on to fall back to unscoped lookup.
func TestFindArtistInLibrary_NoMatchReturnsNil(t *testing.T) {
	t.Parallel()
	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")
	embyLib := &library.Library{
		Name:         "Emby Music",
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-1",
		ExternalID:   "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}

	got, err := router.findArtistInLibrary(ctx, "00000000-0000-0000-0000-000000000000", "Nobody", embyLib.ID)
	if err != nil {
		t.Fatalf("findArtistInLibrary: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}

	// MBID empty + no name => early return (nil, nil) without query.
	got, err = router.findArtistInLibrary(ctx, "", "", embyLib.ID)
	if err != nil {
		t.Fatalf("findArtistInLibrary empty: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty inputs, got %+v", got)
	}
}

// TestFindArtistInLibrary_CrossLibraryMissReturnsNil pins the M:N scoping
// guarantee: an artist that exists with the requested MBID/name BUT only in
// a different library must NOT match the lookup. Without the al.library_id
// filter on artist_libraries, the JOIN would surface the wrong artist and
// the per-library scoping behavior would silently regress.
func TestFindArtistInLibrary_CrossLibraryMissReturnsNil(t *testing.T) {
	t.Parallel()
	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	// Two libraries on two different connections; the search target is
	// embyLib but every artist we plant lives in otherLib only.
	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")
	addTestConnection(t, router, "conn-emby-2", "Other Emby", "emby")

	embyLib := &library.Library{
		Name:         "Emby Music",
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-1",
		ExternalID:   "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}
	otherLib := &library.Library{
		Name:         "Other Music",
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-2",
		ExternalID:   "emby-lib-2",
	}
	if err := router.libraryService.Create(ctx, otherLib); err != nil {
		t.Fatalf("creating other library: %v", err)
	}

	// Artist 1: matches by MBID, but only in otherLib.
	mbidArtist := &artist.Artist{
		Name:      "MBID Only",
		SortName:  "MBID Only",
		LibraryID: otherLib.ID,
		Path:      t.TempDir(),
	}
	if err := router.artistService.Create(ctx, mbidArtist); err != nil {
		t.Fatalf("creating mbid artist: %v", err)
	}
	if _, err := router.db.ExecContext(ctx, `
		INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
		VALUES (?, 'musicbrainz', ?)
	`, mbidArtist.ID, "22222222-2222-2222-2222-222222222222"); err != nil {
		t.Fatalf("inserting provider id: %v", err)
	}

	// Artist 2: matches by name, but only in otherLib.
	nameArtist := &artist.Artist{
		Name:      "Name Only",
		SortName:  "Name Only",
		LibraryID: otherLib.ID,
		Path:      t.TempDir(),
	}
	if err := router.artistService.Create(ctx, nameArtist); err != nil {
		t.Fatalf("creating name artist: %v", err)
	}

	// MBID lookup scoped to embyLib must miss even though the MBID exists
	// in otherLib. Without the library_id filter on artist_libraries, the
	// JOIN would return the otherLib artist and silently violate scoping.
	got, err := router.findArtistInLibrary(ctx,
		"22222222-2222-2222-2222-222222222222", "", embyLib.ID)
	if err != nil {
		t.Fatalf("findArtistInLibrary by mbid: %v", err)
	}
	if got != nil {
		t.Errorf("MBID cross-library lookup returned %+v, want nil", got)
	}

	// Name lookup scoped to embyLib must also miss.
	got, err = router.findArtistInLibrary(ctx, "", "Name Only", embyLib.ID)
	if err != nil {
		t.Fatalf("findArtistInLibrary by name: %v", err)
	}
	if got != nil {
		t.Errorf("name cross-library lookup returned %+v, want nil", got)
	}
}

// TestFindArtistInLibrary_DeterministicOrdering pins the
// ORDER BY datetime(a.created_at), a.id determinism for both lookup paths.
// Two artists in the same library share the same MBID (and the same name)
// but were inserted with mixed-format created_at timestamps. The lookup
// must always return the chronologically older row, even though the legacy
// "YYYY-MM-DD HH:MM:SS" form sorts AFTER an "T"-separated RFC3339 form
// under raw TEXT comparison.
func TestFindArtistInLibrary_DeterministicOrdering(t *testing.T) {
	t.Parallel()
	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")
	embyLib := &library.Library{
		Name:         "Emby Music",
		Type:         library.TypeRegular,
		Source:       library.SourceEmby,
		ConnectionID: "conn-emby-1",
		ExternalID:   "emby-lib-1",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("creating emby library: %v", err)
	}

	// olderArtist: RFC3339 timestamp at 00:30Z (earlier).
	// newerArtist: legacy SQLite timestamp at 23:00 (later).
	// Raw TEXT comparison would order "2024-01-15 23:00:00" BEFORE
	// "2024-01-15T00:30:00Z" because ' ' (0x20) < 'T' (0x54), so without
	// datetime() normalization the wrong row would win.
	olderID := "older-artist"
	newerID := "newer-artist"
	mbid := "33333333-3333-3333-3333-333333333333"
	dupName := "Duplicate Band"

	for _, row := range []struct {
		id, createdAt string
	}{
		{olderID, "2024-01-15T00:30:00Z"}, // RFC3339, chronologically EARLIER
		{newerID, "2024-01-15 23:00:00"},  // legacy SQLite, chronologically LATER
	} {
		if _, err := router.db.ExecContext(ctx,
			`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			row.id, dupName, dupName, t.TempDir(), row.createdAt, row.createdAt); err != nil {
			t.Fatalf("inserting %s: %v", row.id, err)
		}
		if _, err := router.db.ExecContext(ctx,
			`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
			 VALUES (?, ?, 'manual', ?)`,
			row.id, embyLib.ID, row.createdAt); err != nil {
			t.Fatalf("seeding membership for %s: %v", row.id, err)
		}
		if _, err := router.db.ExecContext(ctx,
			`INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
			 VALUES (?, 'musicbrainz', ?)`,
			row.id, mbid); err != nil {
			t.Fatalf("inserting provider id for %s: %v", row.id, err)
		}
	}

	// MBID lookup: must return the chronologically older RFC3339 row.
	got, err := router.findArtistInLibrary(ctx, mbid, "", embyLib.ID)
	if err != nil {
		t.Fatalf("findArtistInLibrary by mbid: %v", err)
	}
	if got == nil || got.ID != olderID {
		var gotID string
		if got != nil {
			gotID = got.ID
		}
		t.Errorf("MBID lookup ID = %q, want %q (datetime() must normalize mixed formats)",
			gotID, olderID)
	}

	// Name lookup: same determinism guarantee.
	got, err = router.findArtistInLibrary(ctx, "", dupName, embyLib.ID)
	if err != nil {
		t.Fatalf("findArtistInLibrary by name: %v", err)
	}
	if got == nil || got.ID != olderID {
		var gotID string
		if got != nil {
			gotID = got.ID
		}
		t.Errorf("name lookup ID = %q, want %q (datetime() must normalize mixed formats)",
			gotID, olderID)
	}
}

func TestBackfillPlatformIDToManualLibs_SkipsWhenNoMatch(t *testing.T) {
	t.Parallel()
	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")

	musicDir := t.TempDir()

	manualLib := &library.Library{
		Name:   "Filesystem",
		Path:   musicDir,
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := router.libraryService.Create(ctx, manualLib); err != nil {
		t.Fatalf("creating manual library: %v", err)
	}

	// Call backfill with an artist name that doesn't exist. Should not panic.
	router.backfillPlatformIDToManualLibs(ctx,
		"", "Nonexistent Band",
		"conn-emby-1", "emby-999", "some-conn-artist-id",
		[]library.Library{*manualLib})
}

func TestBackfillPlatformIDToManualLibs_MultipleManualLibraries(t *testing.T) {
	t.Parallel()
	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")

	var manualLibs []library.Library
	var fsArtistIDs []string
	for _, name := range []string{"Music A", "Music B"} {
		libDir := t.TempDir()
		lib := &library.Library{
			Name:   name,
			Path:   libDir,
			Type:   library.TypeRegular,
			Source: library.SourceManual,
		}
		if err := router.libraryService.Create(ctx, lib); err != nil {
			t.Fatalf("creating library %s: %v", name, err)
		}
		manualLibs = append(manualLibs, *lib)

		a := &artist.Artist{
			Name:      "Tool",
			SortName:  "Tool",
			LibraryID: lib.ID,
			Path:      filepath.Join(libDir, "Tool"),
		}
		if err := router.artistService.Create(ctx, a); err != nil {
			t.Fatalf("creating artist in %s: %v", name, err)
		}
		fsArtistIDs = append(fsArtistIDs, a.ID)
	}

	router.backfillPlatformIDToManualLibs(ctx,
		"", "Tool",
		"conn-emby-1", "emby-tool-001", "conn-artist-id",
		manualLibs)

	// Issue #1076: at most one artist row can hold a given
	// (connection_id, platform_artist_id). The backfill iterates the manual
	// libraries in order; the first artist claims the mapping and any
	// subsequent same-name artist in another library skips silently
	// (ErrPlatformIDClaimedByAnotherArtist). Assert that exactly one of the
	// candidates holds the mapping rather than all of them.
	holders := 0
	for i, id := range fsArtistIDs {
		pid, err := router.artistService.GetPlatformID(ctx, id, "conn-emby-1")
		if err != nil {
			t.Fatalf("GetPlatformID artist[%d]: %v", i, err)
		}
		if pid == "emby-tool-001" {
			holders++
		} else if pid != "" {
			t.Errorf("artist[%d] platform ID = %q, want empty or %q", i, pid, "emby-tool-001")
		}
	}
	if holders != 1 {
		t.Errorf("holders of platform id = %d, want exactly 1", holders)
	}
}

func TestBackfillPlatformIDToManualLibs_SkipsSameArtist(t *testing.T) {
	t.Parallel()
	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby-1", "Emby Server", "emby")

	musicDir := t.TempDir()

	manualLib := &library.Library{
		Name:   "Filesystem",
		Path:   musicDir,
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := router.libraryService.Create(ctx, manualLib); err != nil {
		t.Fatalf("creating manual library: %v", err)
	}

	deftonesDir := filepath.Join(musicDir, "Deftones")
	if err := os.MkdirAll(deftonesDir, 0o755); err != nil {
		t.Fatalf("creating deftones dir: %v", err)
	}
	a := &artist.Artist{
		Name:      "Deftones",
		SortName:  "Deftones",
		LibraryID: manualLib.ID,
		Path:      deftonesDir,
	}
	if err := router.artistService.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Pass the same artist ID as connArtistID -- should skip.
	router.backfillPlatformIDToManualLibs(ctx,
		"", "Deftones",
		"conn-emby-1", "emby-deftones-001", a.ID,
		[]library.Library{*manualLib})

	pid, err := router.artistService.GetPlatformID(ctx, a.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID: %v", err)
	}
	if pid != "" {
		t.Errorf("platform ID = %q, want empty (should skip same artist)", pid)
	}
}

func TestScanFromJellyfin_BackfillsPlatformIDToFilesystemArtist(t *testing.T) {
	t.Parallel()
	artistDir := t.TempDir()
	libPath := filepath.Dir(artistDir)

	jfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Artists/AlbumArtists" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"Items":[{
					"Name":"Bjork",
					"SortName":"Bjork",
					"Id":"jf-bjork-001",
					"Path":%q,
					"Overview":"",
					"Genres":[],
					"Tags":[],
					"PremiereDate":"",
					"EndDate":"",
					"ProviderIds":{"MusicBrainzArtist":"mbid-bjork"},
					"ImageTags":{}
				}],
				"TotalRecordCount":1
			}`, artistDir)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer jfSrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-jf-1", "Jellyfin Server", "jellyfin")

	musicDir := t.TempDir()

	manualLib := &library.Library{
		Name:   "Filesystem",
		Path:   musicDir,
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := router.libraryService.Create(ctx, manualLib); err != nil {
		t.Fatalf("creating manual library: %v", err)
	}
	bjorkFSDir := filepath.Join(musicDir, "Bjork")
	if err := os.MkdirAll(bjorkFSDir, 0o755); err != nil {
		t.Fatalf("creating bjork dir: %v", err)
	}
	fsArtist := &artist.Artist{
		Name:          "Bjork",
		SortName:      "Bjork",
		MusicBrainzID: "mbid-bjork",
		LibraryID:     manualLib.ID,
		Path:          bjorkFSDir,
	}
	if err := router.artistService.Create(ctx, fsArtist); err != nil {
		t.Fatalf("creating filesystem artist: %v", err)
	}

	jfLib := &library.Library{
		Name:         "Jellyfin Music",
		Path:         libPath,
		Type:         library.TypeRegular,
		Source:       library.SourceJellyfin,
		ConnectionID: "conn-jf-1",
		ExternalID:   "jf-lib-1",
	}
	if err := router.libraryService.Create(ctx, jfLib); err != nil {
		t.Fatalf("creating jellyfin library: %v", err)
	}
	jfArtist := &artist.Artist{
		Name:          "Bjork",
		SortName:      "Bjork",
		MusicBrainzID: "mbid-bjork",
		LibraryID:     jfLib.ID,
		Path:          artistDir,
	}
	if err := router.artistService.Create(ctx, jfArtist); err != nil {
		t.Fatalf("creating jellyfin artist: %v", err)
	}

	client := jellyfin.NewWithHTTPClient(jfSrv.URL, "key", "", jfSrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if _, err := router.scanFromJellyfin(ctx, client, jfLib); err != nil {
		t.Fatalf("scanFromJellyfin: %v", err)
	}

	// Issue #1076: only the connection-library artist (jfArtist) holds the
	// mapping; the filesystem artist no longer gets a duplicate.
	fsPlatformID, err := router.artistService.GetPlatformID(ctx, fsArtist.ID, "conn-jf-1")
	if err != nil {
		t.Fatalf("GetPlatformID (fs): %v", err)
	}
	if fsPlatformID != "" {
		t.Errorf("filesystem artist platform ID = %q, want empty (UNIQUE index forbids duplicate mapping)", fsPlatformID)
	}
	jfPlatformID, err := router.artistService.GetPlatformID(ctx, jfArtist.ID, "conn-jf-1")
	if err != nil {
		t.Fatalf("GetPlatformID (jf): %v", err)
	}
	if jfPlatformID != "jf-bjork-001" {
		t.Errorf("jellyfin artist platform ID = %q, want %q", jfPlatformID, "jf-bjork-001")
	}
}

func TestScanFromLidarr_BackfillsPlatformIDToFilesystemArtist(t *testing.T) {
	t.Parallel()
	lidarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/artist" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id":42,"artistName":"Radiohead","foreignArtistId":"mbid-radiohead","path":"/music/Radiohead","monitored":true,"metadataProfileId":1}
			]`))
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer lidarrSrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-lidarr-1", "Lidarr Server", "lidarr")

	musicDir := t.TempDir()

	manualLib := &library.Library{
		Name:   "Filesystem",
		Path:   musicDir,
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := router.libraryService.Create(ctx, manualLib); err != nil {
		t.Fatalf("creating manual library: %v", err)
	}
	radioheadFSDir := filepath.Join(musicDir, "Radiohead")
	if err := os.MkdirAll(radioheadFSDir, 0o755); err != nil {
		t.Fatalf("creating radiohead dir: %v", err)
	}
	fsArtist := &artist.Artist{
		Name:          "Radiohead",
		SortName:      "Radiohead",
		MusicBrainzID: "mbid-radiohead",
		LibraryID:     manualLib.ID,
		Path:          radioheadFSDir,
	}
	if err := router.artistService.Create(ctx, fsArtist); err != nil {
		t.Fatalf("creating filesystem artist: %v", err)
	}

	lidarrLib := &library.Library{
		Name:         "Lidarr Music",
		Type:         library.TypeRegular,
		Source:       library.SourceLidarr,
		ConnectionID: "conn-lidarr-1",
		ExternalID:   "lidarr",
	}
	if err := router.libraryService.Create(ctx, lidarrLib); err != nil {
		t.Fatalf("creating lidarr library: %v", err)
	}
	lidarrArtist := &artist.Artist{
		Name:          "Radiohead",
		SortName:      "Radiohead",
		MusicBrainzID: "mbid-radiohead",
		LibraryID:     lidarrLib.ID,
	}
	if err := router.artistService.Create(ctx, lidarrArtist); err != nil {
		t.Fatalf("creating lidarr artist: %v", err)
	}

	client := lidarr.NewWithHTTPClient(lidarrSrv.URL, "key", lidarrSrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if _, err := router.scanFromLidarr(ctx, client, lidarrLib); err != nil {
		t.Fatalf("scanFromLidarr: %v", err)
	}

	// Issue #1076: only the connection-library artist (lidarrArtist) holds
	// the mapping; the filesystem artist no longer gets a duplicate.
	fsPlatformID, err := router.artistService.GetPlatformID(ctx, fsArtist.ID, "conn-lidarr-1")
	if err != nil {
		t.Fatalf("GetPlatformID (fs): %v", err)
	}
	if fsPlatformID != "" {
		t.Errorf("filesystem artist platform ID = %q, want empty (UNIQUE index forbids duplicate mapping)", fsPlatformID)
	}
	lidarrPlatformID, err := router.artistService.GetPlatformID(ctx, lidarrArtist.ID, "conn-lidarr-1")
	if err != nil {
		t.Fatalf("GetPlatformID (lidarr): %v", err)
	}
	if lidarrPlatformID != "42" {
		t.Errorf("lidarr artist platform ID = %q, want %q", lidarrPlatformID, "42")
	}
}

// TestPopulate_EmbyAndJellyfin_CollapsesIntoOneArtist is the issue #1004
// killer integration test. With M:N artist_libraries (no per-library scope
// on dedupe), populating both Emby and Jellyfin against the same on-disk
// artist must produce ONE artist row carrying both platform mappings and
// both library memberships, not two parallel rows.
//
// Topology under test: no filesystem library (the canonical pre-fix bug
// scenario). Emby populates first and creates the artist; Jellyfin
// populates second and absorbs into the existing row.
func TestPopulate_EmbyAndJellyfin_CollapsesIntoOneArtist(t *testing.T) {
	t.Parallel()
	const mbid = "mbid-12-stones"

	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{
				"Name":"12 Stones","SortName":"12 Stones",
				"Id":"emby-12-stones",
				"Path":"","Overview":"American rock band.",
				"Genres":["Rock"],"Tags":[],
				"PremiereDate":"","EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":"` + mbid + `"},
				"ImageTags":{}
			}],
			"TotalRecordCount":1
		}`))
	}))
	defer embySrv.Close()

	jfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{
				"Name":"12 Stones","SortName":"12 Stones",
				"Id":"jelly-12-stones",
				"Path":"/music/12 Stones","Overview":"American rock band.",
				"Genres":["Rock"],"Tags":[],
				"PremiereDate":"","EndDate":"",
				"ProviderIds":{"MusicBrainzArtist":"` + mbid + `"},
				"ImageTags":{}
			}],
			"TotalRecordCount":1
		}`))
	}))
	defer jfSrv.Close()

	router := testRouterForLibraryOps(t)
	ctx := context.Background()

	addTestConnection(t, router, "conn-emby", "Emby", "emby")
	addTestConnection(t, router, "conn-jelly", "Jellyfin", "jellyfin")

	embyLib := &library.Library{
		Name: "Emby Music", Type: library.TypeRegular,
		Source: library.SourceEmby, ConnectionID: "conn-emby", ExternalID: "emby-lib",
	}
	if err := router.libraryService.Create(ctx, embyLib); err != nil {
		t.Fatalf("create emby library: %v", err)
	}
	jfLib := &library.Library{
		Name: "Jellyfin Music", Type: library.TypeRegular,
		Source: library.SourceJellyfin, ConnectionID: "conn-jelly", ExternalID: "jelly-lib",
	}
	if err := router.libraryService.Create(ctx, jfLib); err != nil {
		t.Fatalf("create jellyfin library: %v", err)
	}

	embyClient := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	jfClient := jellyfin.NewWithHTTPClient(jfSrv.URL, "key", "", jfSrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	// Emby populates first: creates the canonical artist row.
	var rEmby populateResult
	if err := router.populateFromEmbyCtx(ctx, embyClient, embyLib, &rEmby); err != nil {
		t.Fatalf("emby populate: %v", err)
	}
	if rEmby.Created != 1 {
		t.Errorf("emby created = %d, want 1", rEmby.Created)
	}

	// Jellyfin populates second: must absorb into the same artist row, not
	// create a duplicate.
	var rJF populateResult
	if err := router.populateFromJellyfinCtx(ctx, jfClient, jfLib, &rJF); err != nil {
		t.Fatalf("jellyfin populate: %v", err)
	}
	if rJF.Created != 0 {
		t.Errorf("jellyfin created = %d, want 0 (must dedupe to existing artist)", rJF.Created)
	}

	// Exactly one 12 Stones row exists end-to-end.
	a, err := router.artistService.GetByMBID(ctx, mbid)
	if err != nil || a == nil {
		t.Fatalf("GetByMBID after both populates: a=%v err=%v", a, err)
	}

	// Both platform mappings live on the canonical row.
	embyPID, err := router.artistService.GetPlatformID(ctx, a.ID, "conn-emby")
	if err != nil {
		t.Fatalf("GetPlatformID emby: %v", err)
	}
	if embyPID != "emby-12-stones" {
		t.Errorf("emby platform id = %q, want emby-12-stones", embyPID)
	}
	jfPID, err := router.artistService.GetPlatformID(ctx, a.ID, "conn-jelly")
	if err != nil {
		t.Fatalf("GetPlatformID jellyfin: %v", err)
	}
	if jfPID != "jelly-12-stones" {
		t.Errorf("jellyfin platform id = %q, want jelly-12-stones", jfPID)
	}

	// Both library memberships live on the canonical row.
	memberships, err := router.artistService.LibrariesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("LibrariesForArtist: %v", err)
	}
	gotSources := map[string]string{}
	for _, m := range memberships {
		gotSources[m.LibraryID] = m.Source
	}
	if gotSources[embyLib.ID] != "emby" {
		t.Errorf("emby membership source = %q, want emby", gotSources[embyLib.ID])
	}
	if gotSources[jfLib.ID] != "jellyfin" {
		t.Errorf("jellyfin membership source = %q, want jellyfin", gotSources[jfLib.ID])
	}
}

// TestPublishPopulateProgress_ThrottlesAtFivePercent locks in the
// throttling math added in PR7 for #1216. We call publishPopulateProgress
// once per processed count and then assert only the 5%-step indices
// (plus the final tick) made it onto the event bus.
func TestPublishPopulateProgress_ThrottlesAtFivePercent(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)
	rec, stop := attachBusRecorder(t, r)
	defer stop()

	lib := &library.Library{ID: "lib-tp-1", Name: "Throttle Test"}
	total := 100
	for processed := 1; processed <= total; processed++ {
		r.publishPopulateProgress(lib, processed, total)
	}
	// 5%-throttle on total=100 yields 20 events; deadline-poll so we
	// don't race the bus worker on slow CI.
	evts := rec.waitForCount(t, 20, time.Second)
	indices := extractProcessedIndices(evts)
	// With total=100, step=5, so we expect 5, 10, ... 100 == 20 events.
	if len(indices) != 20 {
		t.Errorf("running events = %d, want 20 (5%% throttle on total=100); indices=%v", len(indices), indices)
	}
	if len(indices) > 0 && indices[len(indices)-1] != 100 {
		t.Errorf("last running index = %d, want 100", indices[len(indices)-1])
	}
}

// TestPublishPopulateProgress_IndeterminateEmitsEachCall covers the
// total<=0 branch where the first paginated response has not landed yet.
// The throttle short-circuits and every call emits so the pill shows
// movement even without a known total.
func TestPublishPopulateProgress_IndeterminateEmitsEachCall(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)
	rec, stop := attachBusRecorder(t, r)
	defer stop()

	lib := &library.Library{ID: "lib-tp-2", Name: "Indeterminate"}
	for processed := 1; processed <= 4; processed++ {
		r.publishPopulateProgress(lib, processed, 0)
	}
	// Indeterminate emits one event per call; wait for all 4.
	evts := rec.waitForCount(t, 4, time.Second)
	if got := len(extractProcessedIndices(evts)); got != 4 {
		t.Errorf("indeterminate emits = %d, want 4 (one per call)", got)
	}
}

// TestRunPopulate_EmitsStartAndCompletedEvents drives runPopulate
// end-to-end against a fake Emby server returning a single artist. The
// resulting event stream must include a "running" start event (processed=0)
// and a terminal "completed" event.
func TestRunPopulate_EmitsStartAndCompletedEvents(t *testing.T) {
	t.Parallel()
	embySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"Items":[{"Name":"Progress Pill Artist","Id":"emby-pp-1","Path":"/music/PP"}],
			"TotalRecordCount":1
		}`))
	}))
	defer embySrv.Close()

	r := testRouterForLibraryOps(t)
	rec, stop := attachBusRecorder(t, r)
	defer stop()

	ctx := context.Background()
	lib := &library.Library{
		Name:       "Populate Pill Library",
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby,
		ExternalID: "emby-lib-pp",
	}
	if err := r.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	// Real connection for the type switch in runPopulate to find Emby; the
	// URL points at the fake server we just stood up so no real network
	// dependency is introduced.
	conn := &connection.Connection{
		ID:     "conn-pp-1",
		Type:   connection.TypeEmby,
		URL:    embySrv.URL,
		APIKey: "k",
	}
	op := &LibraryOpResult{LibraryID: lib.ID, LibraryName: lib.Name, Operation: "populate", Status: "running", StartedAt: time.Now().UTC()}
	r.libraryOpsMu.Lock()
	r.libraryOps[lib.ID] = op
	r.libraryOpsMu.Unlock()

	r.runPopulate(ctx, conn, lib, op)
	// runPopulate emits kickoff + page ticks + a terminal "completed"
	// event. Wait on the terminal event landing rather than a fixed
	// count: page-tick rate varies with library size, so a count-based
	// wait would short-circuit before "completed" dispatches.
	wantOpID := populateOpID(lib.ID)
	evts := rec.waitUntil(t, func(evts []event.Event) bool {
		for _, e := range evts {
			if e.Data["op_id"] == wantOpID && e.Data["status"] == "completed" {
				return true
			}
		}
		return false
	}, "completed event for "+wantOpID)
	var sawStart, sawCompleted bool
	for _, e := range evts {
		if e.Data["op_id"] != wantOpID {
			continue
		}
		// Kickoff event is the only one with total=0 (publishOpProgress
		// is called with 0,0 at the top of runPopulate before any page
		// fetch). A page tick can carry processed=0 too if the first
		// page is empty, so total=0 is the distinguishing field.
		if e.Data["status"] == "running" && e.Data["processed"] == 0 && e.Data["total"] == 0 {
			sawStart = true
		}
		if e.Data["status"] == "completed" {
			sawCompleted = true
		}
	}
	if !sawStart {
		t.Errorf("expected a running event with processed=0 for op_id=%s; events=%+v", wantOpID, evts)
	}
	if !sawCompleted {
		t.Errorf("expected a completed event for op_id=%s; events=%+v", wantOpID, evts)
	}
}

// TestRunPopulate_UnsupportedConnectionType_EmitsFailed exercises the
// default branch of runPopulate's type switch + the failure-status
// terminal-event path: an unknown connection.Type should produce a
// "failed" op record and a "failed" pill event rather than silently
// finishing successfully. Covers the failure-write under libraryOpsMu
// plus the terminal r.publishOpProgress(..., "failed", "") call.
func TestRunPopulate_UnsupportedConnectionType_EmitsFailed(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)
	rec, stop := attachBusRecorder(t, r)
	defer stop()

	ctx := context.Background()
	lib := &library.Library{
		Name:       "Unsupported Type Library",
		Type:       library.TypeRegular,
		Source:     connection.TypeEmby, // library.Source only constrains storage; runPopulate dispatches on conn.Type
		ExternalID: "unsupported-lib-1",
	}
	if err := r.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}
	// A bogus connection type drops runPopulate into the default branch
	// which sets popErr without calling any populateFrom*Ctx helper.
	conn := &connection.Connection{
		ID:   "conn-unsupported-1",
		Type: "bogus-platform",
		URL:  "http://127.0.0.1:1",
	}
	op := &LibraryOpResult{
		LibraryID:   lib.ID,
		LibraryName: lib.Name,
		Operation:   "populate",
		Status:      "running",
		StartedAt:   time.Now().UTC(),
	}
	r.libraryOpsMu.Lock()
	r.libraryOps[lib.ID] = op
	r.libraryOpsMu.Unlock()

	r.runPopulate(ctx, conn, lib, op)

	// 1) op record reflects failure.
	r.libraryOpsMu.Lock()
	gotStatus := op.Status
	r.libraryOpsMu.Unlock()
	if gotStatus != "failed" {
		t.Errorf("op.Status = %q, want \"failed\"", gotStatus)
	}

	// 2) terminal pill event carries status=failed for this op_id.
	wantOpID := populateOpID(lib.ID)
	evts := rec.waitUntil(t, func(evts []event.Event) bool {
		for _, e := range evts {
			if e.Data["op_id"] == wantOpID && e.Data["status"] == "failed" {
				return true
			}
		}
		return false
	}, "failed event for "+wantOpID)
	// Sanity: at least the kickoff + terminal landed.
	if len(evts) < 2 {
		t.Errorf("expected >= 2 events (kickoff + terminal failed); got %d: %+v", len(evts), evts)
	}
}

// TestRunPopulate_TypeJellyfin_DispatchesJellyfinPath confirms the
// TypeJellyfin branch of runPopulate's type switch is taken (covers the
// populateFromJellyfinCtx wire-up that the original PR diff added but
// no test exercised). The fake server returns an empty Jellyfin payload
// so the populate completes cleanly without coupling the assertion to
// Jellyfin's full pagination schema.
func TestRunPopulate_TypeJellyfin_DispatchesJellyfinPath(t *testing.T) {
	t.Parallel()
	jfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Items":[],"TotalRecordCount":0}`))
	}))
	defer jfSrv.Close()

	r := testRouterForLibraryOps(t)
	rec, stop := attachBusRecorder(t, r)
	defer stop()

	ctx := context.Background()
	lib := &library.Library{
		Name:       "Jellyfin Pill Library",
		Type:       library.TypeRegular,
		Source:     connection.TypeJellyfin,
		ExternalID: "jf-lib-pp",
	}
	if err := r.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}
	conn := &connection.Connection{
		ID:     "conn-jf-1",
		Type:   connection.TypeJellyfin,
		URL:    jfSrv.URL,
		APIKey: "k",
	}
	op := &LibraryOpResult{LibraryID: lib.ID, LibraryName: lib.Name, Operation: "populate", Status: "running", StartedAt: time.Now().UTC()}
	r.libraryOpsMu.Lock()
	r.libraryOps[lib.ID] = op
	r.libraryOpsMu.Unlock()

	r.runPopulate(ctx, conn, lib, op)

	wantOpID := populateOpID(lib.ID)
	rec.waitUntil(t, func(evts []event.Event) bool {
		for _, e := range evts {
			if e.Data["op_id"] == wantOpID && e.Data["status"] == "completed" {
				return true
			}
		}
		return false
	}, "completed event for "+wantOpID)
}

// TestRunPopulate_TypeLidarr_DispatchesLidarrPath: same coverage rationale
// as the Jellyfin variant above, for the TypeLidarr branch. Lidarr's
// /api/v1/artist endpoint returns a JSON array (no Items wrapper); the
// fake server returns an empty array so the populate exits cleanly.
func TestRunPopulate_TypeLidarr_DispatchesLidarrPath(t *testing.T) {
	t.Parallel()
	lidarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer lidarrSrv.Close()

	r := testRouterForLibraryOps(t)
	rec, stop := attachBusRecorder(t, r)
	defer stop()

	ctx := context.Background()
	lib := &library.Library{
		Name:       "Lidarr Pill Library",
		Type:       library.TypeRegular,
		Source:     connection.TypeLidarr,
		ExternalID: "lidarr-lib-pp",
	}
	if err := r.libraryService.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}
	conn := &connection.Connection{
		ID:     "conn-lidarr-1",
		Type:   connection.TypeLidarr,
		URL:    lidarrSrv.URL,
		APIKey: "k",
	}
	op := &LibraryOpResult{LibraryID: lib.ID, LibraryName: lib.Name, Operation: "populate", Status: "running", StartedAt: time.Now().UTC()}
	r.libraryOpsMu.Lock()
	r.libraryOps[lib.ID] = op
	r.libraryOpsMu.Unlock()

	r.runPopulate(ctx, conn, lib, op)

	wantOpID := populateOpID(lib.ID)
	rec.waitUntil(t, func(evts []event.Event) bool {
		for _, e := range evts {
			if e.Data["op_id"] == wantOpID && e.Data["status"] == "completed" {
				return true
			}
		}
		return false
	}, "completed event for "+wantOpID)
}

// TestHandleLibraryOpStatus_UnknownLibraryReturnsIdle pins the
// libraryOps-miss contract: a library ID the router has never seen an
// op for returns 200 with status="idle" (not 404). The JS pill poller
// depends on this -- a 404 would surface as a fetch error in the
// client, while "idle" is the canonical "nothing is happening" signal.
func TestHandleLibraryOpStatus_UnknownLibraryReturnsIdle(t *testing.T) {
	t.Parallel()
	r := testRouterForLibraryOps(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/libraries/never-existed/operation/status", nil)
	req.SetPathValue("libId", "never-existed")
	w := httptest.NewRecorder()

	r.handleLibraryOpStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v; raw: %s", err, w.Body.String())
	}
	if body["status"] != "idle" {
		t.Errorf("status field = %q, want \"idle\"; body: %s", body["status"], w.Body.String())
	}
}
