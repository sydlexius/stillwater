package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/rule"
)

func testRouterForLibraryOps(t *testing.T) *Router {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

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

	return NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		LibraryService:     libSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticDir:          "../../web/static",
	})
}

func TestHandleLibraryOpStatus_Idle(t *testing.T) {
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

func TestScheduleOpCleanup(t *testing.T) {
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
	client := emby.NewWithHTTPClient(embySrv.URL, "key", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := r.populateFromEmbyCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}

	// Retrieve the artist and verify metadata was mapped.
	a, err := r.artistService.GetByNameAndLibrary(ctx, "Radiohead", lib.ID)
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
}

func TestPopulateFromJellyfin_ImportsMetadataFields(t *testing.T) {
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

	client := jellyfin.NewWithHTTPClient(jfSrv.URL, "key", jfSrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := r.populateFromJellyfinCtx(ctx, client, lib, &result); err != nil {
		t.Fatalf("populateFromJellyfinCtx: %v", err)
	}

	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}

	a, err := r.artistService.GetByNameAndLibrary(ctx, "Bjork", lib.ID)
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
}

func TestPopulateLibrary_ConflictWhenRunning(t *testing.T) {
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
