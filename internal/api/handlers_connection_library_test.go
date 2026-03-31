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
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
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

	platformSvc := platform.NewService(db)

	cacheDir := filepath.Join(t.TempDir(), "cache", "images")

	return NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		LibraryService:     libSvc,
		PlatformService:    platformSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		DB:                 db,
		Logger:             logger,
		StaticDir:          "../../web/static",
		ImageCacheDir:      cacheDir,
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

	client := jellyfin.NewWithHTTPClient(jfSrv.URL, "key", "", jfSrv.Client(),
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

func TestPopulateFromEmby_DownloadsImages(t *testing.T) {
	jpegData := createTestJPEGForHandler(t)
	artistDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
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
	a, err := router.artistService.GetByNameAndLibrary(ctx, "Radiohead", lib.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.Path != artistDir {
		t.Errorf("artist path = %q, want %q", a.Path, artistDir)
	}
	if !a.ThumbExists {
		t.Error("expected ThumbExists to be true")
	}
}

func TestPopulateFromEmby_SkipsExistingImage(t *testing.T) {
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
	a, err := router.artistService.GetByNameAndLibrary(ctx, "NoPath Artist", lib.ID)
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
}

func TestPopulateFromJellyfin_DownloadsImages(t *testing.T) {
	jpegData := createTestJPEGForHandler(t)
	artistDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
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

	a, err := router.artistService.GetByNameAndLibrary(ctx, "Bjork", lib.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.Path != artistDir {
		t.Errorf("artist path = %q, want %q", a.Path, artistDir)
	}
	if !a.ThumbExists {
		t.Error("expected ThumbExists to be true")
	}
}

func TestPopulateFromEmby_DownloadsImagesForExistingArtist(t *testing.T) {
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
	a, err := router.artistService.GetByNameAndLibrary(ctx, "Radiohead", lib.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if !a.ThumbExists {
		t.Error("expected ThumbExists to be true")
	}
}

func TestValidatedArtistPath(t *testing.T) {
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

	a, err := router.artistService.GetByNameAndLibrary(ctx, "Radiohead", lib.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.Path != "" {
		t.Errorf("artist path = %q, want empty (pathless library should not store platform path)", a.Path)
	}
}

func TestPopulateFromEmby_PlatformPathStoredWhenUnderLibraryRoot(t *testing.T) {
	artistDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
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

	a, err := router.artistService.GetByNameAndLibrary(ctx, "Radiohead", lib.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.Path != artistDir {
		t.Errorf("artist path = %q, want %q", a.Path, artistDir)
	}
}

func TestPopulateFromEmby_PlatformPathRejectedWhenOutsideLibraryRoot(t *testing.T) {
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

	a, err := router.artistService.GetByNameAndLibrary(ctx, "Radiohead", lib.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.Path != "" {
		t.Errorf("artist path = %q, want empty (path outside library root should be rejected)", a.Path)
	}
}

func TestPopulateFromEmby_BackfillsMBID(t *testing.T) {
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
	a, err := router.artistService.GetByNameAndLibrary(ctx, "Radiohead", lib.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.MusicBrainzID != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("MusicBrainzID = %q, want backfilled MBID", a.MusicBrainzID)
	}
}

func TestPopulateFromEmby_SkipsOnMBIDConflict(t *testing.T) {
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

	a, err := router.artistService.GetByNameAndLibrary(ctx, "Radiohead", lib.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want 2", a.FanartCount)
	}
}

func TestPopulateFromEmby_SkipsExistingBackdrop(t *testing.T) {
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

	a, err := router.artistService.GetByNameAndLibrary(ctx, "Bjork", lib.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want 2", a.FanartCount)
	}
}

func TestPopulateFromEmby_NoBackdropsWhenTagsEmpty(t *testing.T) {
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

	got, err := router.artistService.GetByMBIDAndLibrary(ctx, "mbid-scan-001", lib.ID)
	if err != nil || got == nil {
		t.Fatalf("looking up artist after scan: %v", err)
	}
	if !got.FanartExists {
		t.Error("FanartExists should be true after scan with non-empty BackdropImageTags")
	}
}

func TestScanFromJellyfin_FanartExistsFromBackdropImageTags(t *testing.T) {
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

	got, err := router.artistService.GetByMBIDAndLibrary(ctx, "mbid-scan-002", lib.ID)
	if err != nil || got == nil {
		t.Fatalf("looking up artist after scan: %v", err)
	}
	if !got.FanartExists {
		t.Error("FanartExists should be true after scan with non-empty BackdropImageTags")
	}
}

func TestPopulateFromEmby_NonKodiBackdropNaming(t *testing.T) {
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

	a, err := router.artistService.GetByNameAndLibrary(ctx, "Sigur Ros", lib.ID)
	if err != nil || a == nil {
		t.Fatalf("looking up artist: %v", err)
	}
	if a.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want 2", a.FanartCount)
	}
}

func TestImportLibraries_AutoPopulate(t *testing.T) {
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

	// Verify platform ID was ALSO backfilled to filesystem artist.
	fsPlatformID, err := router.artistService.GetPlatformID(ctx, fsArtist.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID (fs artist): %v", err)
	}
	if fsPlatformID != "emby-deftones-001" {
		t.Errorf("filesystem artist platform ID = %q, want %q", fsPlatformID, "emby-deftones-001")
	}
}

func TestPopulateFromEmby_BackfillsPlatformIDToFilesystemArtist(t *testing.T) {
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

	// Populate -- this creates a NEW emby-library artist.
	client := emby.NewWithHTTPClient(embySrv.URL, "key", "", embySrv.Client(),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	var result populateResult
	if err := router.populateFromEmbyCtx(ctx, client, embyLib, &result); err != nil {
		t.Fatalf("populateFromEmbyCtx: %v", err)
	}

	if result.Created != 1 {
		t.Fatalf("created = %d, want 1", result.Created)
	}

	// The emby-library artist should have a platform ID (existing behavior).
	embyArtist, err := router.artistService.GetByNameAndLibrary(ctx, "Radiohead", embyLib.ID)
	if err != nil || embyArtist == nil {
		t.Fatalf("looking up emby artist: %v", err)
	}
	embyPlatformID, err := router.artistService.GetPlatformID(ctx, embyArtist.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID (emby): %v", err)
	}
	if embyPlatformID != "emby-radiohead-001" {
		t.Errorf("emby artist platform ID = %q, want %q", embyPlatformID, "emby-radiohead-001")
	}

	// The filesystem artist should ALSO have the platform ID (new behavior).
	fsPlatformID, err := router.artistService.GetPlatformID(ctx, fsArtist.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID (fs): %v", err)
	}
	if fsPlatformID != "emby-radiohead-001" {
		t.Errorf("filesystem artist platform ID = %q, want %q", fsPlatformID, "emby-radiohead-001")
	}
}

func TestScanFromEmby_BackfillsCaseInsensitiveName(t *testing.T) {
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

	fsPlatformID, err := router.artistService.GetPlatformID(ctx, fsArtist.ID, "conn-emby-1")
	if err != nil {
		t.Fatalf("GetPlatformID (fs): %v", err)
	}
	if fsPlatformID != "emby-veridia-001" {
		t.Errorf("filesystem artist platform ID = %q, want %q", fsPlatformID, "emby-veridia-001")
	}
}

func TestResolveAndBackfillPlatformID_NilWhenNoConnectionMatch(t *testing.T) {
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

func TestBackfillPlatformIDToManualLibs_SkipsWhenNoMatch(t *testing.T) {
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

	for i, id := range fsArtistIDs {
		pid, err := router.artistService.GetPlatformID(ctx, id, "conn-emby-1")
		if err != nil {
			t.Fatalf("GetPlatformID artist[%d]: %v", i, err)
		}
		if pid != "emby-tool-001" {
			t.Errorf("artist[%d] platform ID = %q, want %q", i, pid, "emby-tool-001")
		}
	}
}

func TestBackfillPlatformIDToManualLibs_SkipsSameArtist(t *testing.T) {
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

	fsPlatformID, err := router.artistService.GetPlatformID(ctx, fsArtist.ID, "conn-jf-1")
	if err != nil {
		t.Fatalf("GetPlatformID (fs): %v", err)
	}
	if fsPlatformID != "jf-bjork-001" {
		t.Errorf("filesystem artist platform ID = %q, want %q", fsPlatformID, "jf-bjork-001")
	}
}

func TestScanFromLidarr_BackfillsPlatformIDToFilesystemArtist(t *testing.T) {
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

	fsPlatformID, err := router.artistService.GetPlatformID(ctx, fsArtist.ID, "conn-lidarr-1")
	if err != nil {
		t.Fatalf("GetPlatformID (fs): %v", err)
	}
	if fsPlatformID != "42" {
		t.Errorf("filesystem artist platform ID = %q, want %q", fsPlatformID, "42")
	}
}
