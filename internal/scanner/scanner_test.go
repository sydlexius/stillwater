package scanner

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/rule"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func setupScanner(t *testing.T, libraryPath string) (*Service, *artist.Service) {
	t.Helper()
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	scannerSvc := NewService(artistSvc, nil, nil, logger, libraryPath, nil)
	return scannerSvc, artistSvc
}

func createArtistDir(t *testing.T, base, name string, files ...string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating dir %s: %v", dir, err)
	}
	for _, f := range files {
		path := filepath.Join(dir, f)
		if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
			t.Fatalf("creating file %s: %v", path, err)
		}
	}
}

func createArtistDirWithNFO(t *testing.T, base, name, nfoContent string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating dir %s: %v", dir, err)
	}
	nfoPath := filepath.Join(dir, "artist.nfo")
	if err := os.WriteFile(nfoPath, []byte(nfoContent), 0o644); err != nil {
		t.Fatalf("creating nfo %s: %v", nfoPath, err)
	}
}

func waitForScan(t *testing.T, svc *Service, timeout time.Duration) *ScanResult { //nolint:unparam // timeout exposed for future per-test customization
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status := svc.Status()
		if status != nil && status.Status != "running" {
			return status
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("scan did not complete within timeout")
	return nil
}

// stubLibraryLister returns a fixed list of libraries.
type stubLibraryLister struct {
	libs []library.Library
}

func (s *stubLibraryLister) List(_ context.Context) ([]library.Library, error) {
	return s.libs, nil
}

func TestScan_PathlessLibrarySkipped(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Visible Artist")
	svc, artistSvc, db := setupScannerWithDB(t, libDir)

	// Seed the libraries the lister will surface so artist_libraries
	// memberships (and the LibraryID hydration that follows) actually
	// land on created artists.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
			VALUES ('lib-1', 'Main', ?, 'regular', 'manual', datetime('now'), datetime('now')),
			       ('lib-2', 'API Only', '', 'regular', 'manual', datetime('now'), datetime('now'))`,
		libDir); err != nil {
		t.Fatalf("seeding libraries: %v", err)
	}

	// Set up a library lister that returns one healthy library and one pathless (empty path).
	svc.SetLibraryLister(&stubLibraryLister{
		libs: []library.Library{
			{ID: "lib-1", Name: "Main", Path: libDir, Type: library.TypeRegular},
			{ID: "lib-2", Name: "API Only", Path: "", Type: library.TypeRegular},
		},
	})

	ctx := context.Background()
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.Status != "completed" {
		t.Errorf("status = %q, want completed", final.Status)
	}
	if final.NewArtists != 1 {
		t.Errorf("NewArtists = %d, want 1 (pathless library should be skipped)", final.NewArtists)
	}

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Visible Artist"))
	if a == nil {
		t.Fatal("expected artist from healthy library to be found")
	}
	if a.LibraryID != "lib-1" {
		t.Errorf("LibraryID = %q, want lib-1", a.LibraryID)
	}
}

func TestScan_EmptyDirectory(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	svc, _ := setupScanner(t, libDir)
	ctx := context.Background()

	result, err := svc.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != "running" {
		t.Errorf("initial status = %q, want running", result.Status)
	}

	final := waitForScan(t, svc, 5*time.Second)
	if final.Status != "completed" {
		t.Errorf("final status = %q, want completed", final.Status)
	}
	if final.TotalDirectories != 0 {
		t.Errorf("TotalDirectories = %d, want 0", final.TotalDirectories)
	}
}

func TestScan_SingleArtist(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Nirvana", "folder.jpg", "fanart.jpg")
	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.NewArtists != 1 {
		t.Errorf("NewArtists = %d, want 1", final.NewArtists)
	}
	if final.TotalDirectories != 1 {
		t.Errorf("TotalDirectories = %d, want 1", final.TotalDirectories)
	}

	a, err := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Nirvana"))
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if a == nil {
		t.Fatal("expected artist to be found")
	}
	if a.Name != "Nirvana" {
		t.Errorf("Name = %q, want Nirvana", a.Name)
	}
	if !a.ThumbExists {
		t.Error("ThumbExists should be true (folder.jpg)")
	}
	if !a.FanartExists {
		t.Error("FanartExists should be true (fanart.jpg)")
	}
	if a.LogoExists {
		t.Error("LogoExists should be false")
	}
}

func TestScan_MultipleArtists(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Artist A", "folder.jpg")
	createArtistDir(t, libDir, "Artist B", "fanart.jpg", "logo.png")
	createArtistDir(t, libDir, "Artist C")
	svc, _ := setupScanner(t, libDir)

	if _, err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.NewArtists != 3 {
		t.Errorf("NewArtists = %d, want 3", final.NewArtists)
	}
	if final.TotalDirectories != 3 {
		t.Errorf("TotalDirectories = %d, want 3", final.TotalDirectories)
	}
}

func TestScan_DetectFiles(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Full",
		"artist.nfo", "folder.jpg", "fanart.jpg", "logo.png", "banner.jpg")
	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Full"))
	if a == nil {
		t.Fatal("artist not found")
	}
	if !a.NFOExists {
		t.Error("NFOExists should be true")
	}
	if !a.ThumbExists {
		t.Error("ThumbExists should be true")
	}
	if !a.FanartExists {
		t.Error("FanartExists should be true")
	}
	if !a.LogoExists {
		t.Error("LogoExists should be true")
	}
	if !a.BannerExists {
		t.Error("BannerExists should be true")
	}
}

func TestScan_NFOParsing(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	nfoContent := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>Radiohead</name>
  <sortname>Radiohead</sortname>
  <musicbrainzartistid>a74b1b7f-71a5-4011-9441-d0b5e4122711</musicbrainzartistid>
  <genre>Alternative Rock</genre>
  <genre>Art Rock</genre>
  <biography>English rock band from Abingdon.</biography>
</artist>`
	createArtistDirWithNFO(t, libDir, "Radiohead", nfoContent)
	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Radiohead"))
	if a == nil {
		t.Fatal("artist not found")
	}
	if a.Name != "Radiohead" {
		t.Errorf("Name = %q, want Radiohead", a.Name)
	}
	if a.MusicBrainzID != "a74b1b7f-71a5-4011-9441-d0b5e4122711" {
		t.Errorf("MusicBrainzID = %q", a.MusicBrainzID)
	}
	if len(a.Genres) != 2 {
		t.Errorf("Genres count = %d, want 2", len(a.Genres))
	}
	if a.NFOExists != true {
		t.Error("NFOExists should be true")
	}
}

func TestScan_UpdateExisting(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Tool")
	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	// First scan
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Tool"))
	if a == nil {
		t.Fatal("artist not found after first scan")
	}
	if a.ThumbExists {
		t.Error("ThumbExists should be false initially")
	}

	// Add an image file
	imgPath := filepath.Join(libDir, "Tool", "folder.jpg")
	if err := os.WriteFile(imgPath, []byte("jpg data"), 0o644); err != nil {
		t.Fatalf("creating image: %v", err)
	}

	// Second scan
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.UpdatedArtists < 1 {
		t.Errorf("UpdatedArtists = %d, want >= 1", final.UpdatedArtists)
	}

	a, _ = artistSvc.GetByPath(ctx, filepath.Join(libDir, "Tool"))
	if a == nil {
		t.Fatal("artist not found after second scan")
	}
	if !a.ThumbExists {
		t.Error("ThumbExists should be true after adding folder.jpg")
	}
}

func TestScan_RemovedArtist(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Temp Band")
	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	// First scan
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Temp Band"))
	if a == nil {
		t.Fatal("artist should exist after first scan")
	}

	// Remove the directory
	if err := os.RemoveAll(filepath.Join(libDir, "Temp Band")); err != nil {
		t.Fatalf("removing dir: %v", err)
	}

	// Second scan
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.RemovedArtists != 1 {
		t.Errorf("RemovedArtists = %d, want 1", final.RemovedArtists)
	}

	a, _ = artistSvc.GetByPath(ctx, filepath.Join(libDir, "Temp Band"))
	if a != nil {
		t.Error("artist should be removed after directory is deleted")
	}
}

func TestScan_ConcurrentPrevention(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	// Create many dirs to keep scan running longer
	for i := 0; i < 20; i++ {
		createArtistDir(t, libDir, fmt.Sprintf("Artist %d", i))
	}
	svc, _ := setupScanner(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}

	// Try to start another scan immediately
	_, err := svc.Run(ctx)
	// Either it fails because scan is still running, or it succeeds because scan already finished
	// We just verify it doesn't panic
	if err != nil {
		if !errors.Is(err, ErrScanInProgress) {
			t.Errorf("unexpected error: %v", err)
		}
	}

	waitForScan(t, svc, 5*time.Second)
}

func TestScan_SkipsHiddenDirs(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, ".hidden")
	createArtistDir(t, libDir, "Visible")
	svc, _ := setupScanner(t, libDir)

	if _, err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.TotalDirectories != 1 {
		t.Errorf("TotalDirectories = %d, want 1 (hidden dir should be skipped)", final.TotalDirectories)
	}
	if final.NewArtists != 1 {
		t.Errorf("NewArtists = %d, want 1", final.NewArtists)
	}
}

func TestScan_Exclusions(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Various Artists", "folder.jpg")
	createArtistDir(t, libDir, "Nirvana", "folder.jpg")
	createArtistDir(t, libDir, "OST", "fanart.jpg")

	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(artistSvc, nil, nil, logger, libDir, []string{"Various Artists", "VA", "OST"})
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.NewArtists != 3 {
		t.Errorf("NewArtists = %d, want 3", final.NewArtists)
	}

	// Check Various Artists is excluded
	va, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Various Artists"))
	if va == nil {
		t.Fatal("Various Artists not found")
	}
	if !va.IsExcluded {
		t.Error("Various Artists should be excluded")
	}
	if va.ExclusionReason == "" {
		t.Error("ExclusionReason should be set")
	}

	// Check Nirvana is NOT excluded
	nir, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Nirvana"))
	if nir == nil {
		t.Fatal("Nirvana not found")
	}
	if nir.IsExcluded {
		t.Error("Nirvana should not be excluded")
	}

	// Check OST is excluded
	ost, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "OST"))
	if ost == nil {
		t.Fatal("OST not found")
	}
	if !ost.IsExcluded {
		t.Error("OST should be excluded")
	}
}

func TestScan_HealthScoreIntegration(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	// Create an artist with NFO, thumb, fanart -- should pass several rules
	nfoContent := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>Radiohead</name>
  <musicbrainzartistid>a74b1b7f-71a5-4011-9441-d0b5e4122711</musicbrainzartistid>
  <biography>English rock band from Abingdon, Oxfordshire, formed in 1985.</biography>
</artist>`
	createArtistDirWithNFO(t, libDir, "Radiohead", nfoContent)
	// Add thumb and fanart files
	if err := os.WriteFile(filepath.Join(libDir, "Radiohead", "folder.jpg"), []byte("jpg"), 0o644); err != nil {
		t.Fatalf("writing folder.jpg fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "Radiohead", "fanart.jpg"), []byte("jpg"), 0o644); err != nil {
		t.Fatalf("writing fanart.jpg fixture: %v", err)
	}

	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	ruleEng := rule.NewEngine(ruleSvc, db, nil, nil, logger)

	// Wire event bus and health subscriber so ArtistUpdated events
	// trigger health score re-evaluation (scanner publishes events
	// instead of calling EvaluateAndPersistHealth synchronously).
	bus := event.NewBus(logger, 64)
	go bus.Start()
	t.Cleanup(bus.Stop)

	healthSub := rule.NewHealthSubscriber(ruleEng, artistSvc, logger)
	bus.Subscribe(event.ArtistUpdated, healthSub.HandleEvent)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go healthSub.Start(ctx)
	t.Cleanup(healthSub.Stop)

	svc := NewService(artistSvc, ruleEng, ruleSvc, logger, libDir, nil)
	svc.SetEventBus(bus)

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	// Poll until health score is updated (replaces time.Sleep for debounce window)
	{
		deadline := time.After(5 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		var lastErr error
		for {
			found := false
			select {
			case <-deadline:
				t.Fatalf("timed out waiting for health score update after scan (last error: %v)", lastErr)
			case <-ticker.C:
				a, err := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Radiohead"))
				if err != nil {
					lastErr = err
				} else if a != nil && a.HealthScore > 0 {
					found = true
				}
			}
			if found {
				break
			}
		}
	}

	a, err := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Radiohead"))
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if a == nil {
		t.Fatal("artist not found")
	}

	// Artist has: NFO, MBID, thumb, fanart, biography -- should have a non-zero health score
	if a.HealthScore <= 0 {
		t.Errorf("HealthScore = %v, want > 0", a.HealthScore)
	}
	// Missing: logo -- should not be 100
	// thumb_square and thumb_min_res cannot read dimensions from the fake jpg data,
	// so they return nil (no violation), which counts as a pass in the health score.
	// 8 rules total: 7 pass (nfo_exists, nfo_has_mbid, thumb_exists, fanart_exists,
	// bio_exists, thumb_square, thumb_min_res), 1 fail (logo_exists) = 7/8 = 87.5%
	if a.HealthScore < 50 {
		t.Errorf("HealthScore = %v, expected at least 50 for an artist with most assets", a.HealthScore)
	}
}

func TestScan_NilRuleEngine(t *testing.T) {
	t.Parallel()
	// Verify scanner works fine when rule engine is nil (backward compat)
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Nirvana", "folder.jpg")
	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Nirvana"))
	if a == nil {
		t.Fatal("artist not found")
	}
	// Health score should be default (0) when rule engine is nil
	if a.HealthScore != 0 {
		t.Errorf("HealthScore = %v, want 0 (no rule engine)", a.HealthScore)
	}
}

func TestDetectFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"artist.nfo", "folder.jpg", "backdrop.jpg", "logo.png", "banner.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o644); err != nil {
			t.Fatalf("writing %s fixture: %v", name, err)
		}
	}

	d, err := detectFiles(dir, nil)
	if err != nil {
		t.Fatalf("detectFiles: %v", err)
	}

	if !d.NFOExists {
		t.Error("NFOExists should be true")
	}
	if !d.ThumbExists {
		t.Error("ThumbExists should be true (folder.jpg)")
	}
	if !d.FanartExists {
		t.Error("FanartExists should be true (backdrop.jpg)")
	}
	if !d.LogoExists {
		t.Error("LogoExists should be true (logo.png)")
	}
	if !d.BannerExists {
		t.Error("BannerExists should be true (banner.jpg)")
	}
}

func TestDetectFiles_LowRes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write real PNG images with known dimensions so probeLowRes can decode them.
	writeTestPNG := func(name string, w, h int) {
		t.Helper()
		data := makeScannerPNG(t, w, h)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	// folder.jpg (thumb) 300x300 - below 500x500 threshold, low-res expected
	writeTestPNG("folder.jpg", 300, 300)
	// fanart.png 1920x1080 - above 960x540, not low-res
	writeTestPNG("fanart.png", 1920, 1080)
	// logo.png 800x310 - above 400x155, not low-res
	writeTestPNG("logo.png", 800, 310)
	// banner.png 1000x185 - above 758x140, not low-res
	writeTestPNG("banner.png", 1000, 185)

	d, err := detectFiles(dir, nil)
	if err != nil {
		t.Fatalf("detectFiles: %v", err)
	}

	if !d.ThumbExists {
		t.Fatal("ThumbExists should be true")
	}
	if !d.ThumbLowRes {
		t.Error("ThumbLowRes should be true for 300x300 thumb")
	}
	if !d.FanartExists {
		t.Fatal("FanartExists should be true")
	}
	if d.FanartLowRes {
		t.Error("FanartLowRes should be false for 1920x1080 fanart")
	}
	if !d.LogoExists {
		t.Fatal("LogoExists should be true")
	}
	if d.LogoLowRes {
		t.Error("LogoLowRes should be false for 800x310 logo")
	}
	if !d.BannerExists {
		t.Fatal("BannerExists should be true")
	}
	if d.BannerLowRes {
		t.Error("BannerLowRes should be false for 1000x185 banner")
	}
}

func TestDetectFiles_ReadDirError(t *testing.T) {
	t.Parallel()
	// Pass a path that does not exist so os.ReadDir fails.
	_, err := detectFiles(filepath.Join(t.TempDir(), "nonexistent"), nil)
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestScan_NoLibraries_NoFallback(t *testing.T) {
	t.Parallel()
	legacyDir := t.TempDir()
	createArtistDir(t, legacyDir, "Ghost Artist")

	svc, artistSvc := setupScanner(t, legacyDir)
	// Set a library lister that returns an empty list (all libraries deleted).
	svc.SetLibraryLister(&stubLibraryLister{libs: []library.Library{}})

	ctx := context.Background()
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.Status != "failed" {
		t.Errorf("status = %q, want failed", final.Status)
	}
	if final.Error == "" {
		t.Error("expected error message on scan result")
	}
	if final.NewArtists != 0 {
		t.Errorf("NewArtists = %d, want 0 (should not fall back to legacy path)", final.NewArtists)
	}

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(legacyDir, "Ghost Artist"))
	if a != nil {
		t.Error("artist should not exist -- scanner should not have fallen back to legacy path")
	}
}

func TestScan_DeletedLibrary_NoRepopulate(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Real Artist", "folder.jpg")

	svc, artistSvc := setupScanner(t, libDir)
	lister := &stubLibraryLister{
		libs: []library.Library{
			{ID: "lib-1", Name: "Music", Path: libDir, Type: library.TypeRegular},
		},
	}
	svc.SetLibraryLister(lister)

	ctx := context.Background()

	// First scan: artist is discovered.
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Real Artist"))
	if a == nil {
		t.Fatal("artist should exist after first scan")
	}

	// Simulate library deletion: lister returns empty, delete artist from DB.
	lister.libs = nil
	if err := artistSvc.Delete(ctx, a.ID); err != nil {
		t.Fatalf("deleting artist: %v", err)
	}

	// Second scan: should NOT re-populate the deleted artist.
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.Status != "failed" {
		t.Errorf("status = %q, want failed (no libraries to scan)", final.Status)
	}
	if final.NewArtists != 0 {
		t.Errorf("NewArtists = %d, want 0", final.NewArtists)
	}

	a, _ = artistSvc.GetByPath(ctx, filepath.Join(libDir, "Real Artist"))
	if a != nil {
		t.Error("artist should not be re-created after library deletion")
	}
}

func TestScan_PlaceholderGenerated(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	artistDir := filepath.Join(libDir, "Placeholders")
	if err := os.MkdirAll(artistDir, 0o755); err != nil {
		t.Fatalf("creating dir: %v", err)
	}

	// Write real PNG images so placeholder generation can decode them.
	thumbData := makeScannerPNG(t, 100, 100)
	fanartData := makeScannerPNG(t, 200, 100)
	if err := os.WriteFile(filepath.Join(artistDir, "folder.jpg"), thumbData, 0o644); err != nil {
		t.Fatalf("writing folder.jpg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artistDir, "fanart.jpg"), fanartData, 0o644); err != nil {
		t.Fatalf("writing fanart.jpg: %v", err)
	}

	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, err := artistSvc.GetByPath(ctx, artistDir)
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if a == nil {
		t.Fatal("artist not found")
	}

	if !strings.HasPrefix(a.ThumbPlaceholder, "data:image/") {
		t.Errorf("ThumbPlaceholder should start with data:image/, got %q", truncate30(a.ThumbPlaceholder))
	}
	if !strings.HasPrefix(a.FanartPlaceholder, "data:image/") {
		t.Errorf("FanartPlaceholder should start with data:image/, got %q", truncate30(a.FanartPlaceholder))
	}
	if a.LogoPlaceholder != "" {
		t.Errorf("LogoPlaceholder should be empty (no logo file), got %q", truncate30(a.LogoPlaceholder))
	}
}

func TestDetectFiles_Placeholders(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write real images so placeholder generation can decode them.
	thumbData := makeScannerPNG(t, 300, 300)
	logoData := makeScannerPNG(t, 800, 310)
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), thumbData, 0o644); err != nil {
		t.Fatalf("writing folder.jpg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), logoData, 0o644); err != nil {
		t.Fatalf("writing logo.png: %v", err)
	}

	d, err := detectFiles(dir, nil)
	if err != nil {
		t.Fatalf("detectFiles: %v", err)
	}

	if !strings.HasPrefix(d.ThumbPlaceholder, "data:image/jpeg;base64,") {
		t.Errorf("ThumbPlaceholder should be a JPEG data URI, got prefix %q", truncate30(d.ThumbPlaceholder))
	}
	if !strings.HasPrefix(d.LogoPlaceholder, "data:image/png;base64,") {
		t.Errorf("LogoPlaceholder should be a PNG data URI, got prefix %q", truncate30(d.LogoPlaceholder))
	}
	if d.FanartPlaceholder != "" {
		t.Errorf("FanartPlaceholder should be empty (no fanart file), got %q", truncate30(d.FanartPlaceholder))
	}
	if d.BannerPlaceholder != "" {
		t.Errorf("BannerPlaceholder should be empty (no banner file), got %q", truncate30(d.BannerPlaceholder))
	}
}

// truncate30 returns the first 30 bytes of s (with "..." suffix), or s if shorter.
func truncate30(s string) string {
	if len(s) <= 30 {
		return s
	}
	return s[:30] + "..."
}

// makeScannerPNG creates a minimal PNG image with the given dimensions.
func makeScannerPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	m := image.NewGray(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatalf("encoding PNG: %v", err)
	}
	return buf.Bytes()
}

func TestDetectFiles_SkipsExistingPlaceholder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write a real PNG so probeImageFile can decode dimensions.
	thumbData := makeScannerPNG(t, 600, 600)
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), thumbData, 0o644); err != nil {
		t.Fatalf("writing folder.jpg: %v", err)
	}

	existingPH := "data:image/jpeg;base64,EXISTING"

	// Pass an existing artist with a placeholder already set.
	existing := &artist.Artist{
		ThumbPlaceholder: existingPH,
	}

	d, err := detectFiles(dir, existing)
	if err != nil {
		t.Fatalf("detectFiles: %v", err)
	}

	if !d.ThumbExists {
		t.Fatal("ThumbExists should be true")
	}
	// The existing placeholder should be reused without regeneration.
	if d.ThumbPlaceholder != existingPH {
		t.Errorf("ThumbPlaceholder = %q, want %q (should reuse existing)", truncate30(d.ThumbPlaceholder), existingPH)
	}
}

func TestProcessDirectory_TransientFailurePreservesPlaceholder(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	artistDir := filepath.Join(libDir, "Transient")
	if err := os.MkdirAll(artistDir, 0o755); err != nil {
		t.Fatalf("creating dir: %v", err)
	}

	// Write a real PNG for the initial scan.
	thumbData := makeScannerPNG(t, 600, 600)
	if err := os.WriteFile(filepath.Join(artistDir, "folder.jpg"), thumbData, 0o644); err != nil {
		t.Fatalf("writing folder.jpg: %v", err)
	}

	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	// First scan: artist is created with a valid placeholder.
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, artistDir)
	if a == nil {
		t.Fatal("artist not found after first scan")
	}
	if !strings.HasPrefix(a.ThumbPlaceholder, "data:image/") {
		t.Fatalf("expected valid placeholder after first scan, got %q", truncate30(a.ThumbPlaceholder))
	}
	savedPH := a.ThumbPlaceholder

	// Replace the image with an invalid file to simulate a transient I/O failure
	// during placeholder generation (file exists but decode fails).
	if err := os.WriteFile(filepath.Join(artistDir, "folder.jpg"), []byte("corrupted"), 0o644); err != nil {
		t.Fatalf("writing corrupted folder.jpg: %v", err)
	}

	// Second scan: placeholder generation will fail on the corrupted file.
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ = artistSvc.GetByPath(ctx, artistDir)
	if a == nil {
		t.Fatal("artist not found after second scan")
	}
	if !a.ThumbExists {
		t.Error("ThumbExists should still be true (file exists on disk)")
	}
	// The existing placeholder should be preserved despite the transient failure.
	if a.ThumbPlaceholder != savedPH {
		t.Errorf("ThumbPlaceholder changed after transient failure: got %q, want %q",
			truncate30(a.ThumbPlaceholder), truncate30(savedPH))
	}
}

func TestShutdownCancelsInProgressScan(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()

	// Create enough artist directories to keep the scan busy.
	for i := range 50 {
		createArtistDir(t, tmp, fmt.Sprintf("Artist %d", i))
	}

	svc, _ := setupScanner(t, tmp)

	// Start a scan.
	_, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Shutdown should cancel the scan and return (not hang).
	done := make(chan struct{})
	go func() {
		svc.Shutdown()
		close(done)
	}()

	select {
	case <-done:
		// Shutdown returned, scan goroutine exited.
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return within 5 seconds")
	}

	// After shutdown, the scan status should not be "running".
	status := svc.Status()
	if status != nil && status.Status == "running" {
		t.Error("scan status is still 'running' after Shutdown")
	}
}

// setupScannerWithDB is like setupScanner but also returns the underlying
// SQL handle so tests can simulate registry-vs-disk drift directly.
func setupScannerWithDB(t *testing.T, libraryPath string) (*Service, *artist.Service, *sql.DB) {
	t.Helper()
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	scannerSvc := NewService(artistSvc, nil, nil, logger, libraryPath, nil)
	return scannerSvc, artistSvc, db
}

// TestScan_ReconcilesArtistImagesRegistry covers issue #1225: when an artist
// directory has a canonical image on disk but the artist_images registry
// has lost the row for that slot (post-migration drift, partial backup
// restore, manual SQL surgery), a subsequent scan must rebuild the row so
// <image>_exists and extraneous_images cannot disagree on the same canonical
// file.
//
// The scanner achieves this through its existing change-detection +
// persistNormalized path: hydrateImages reads the now-empty registry, sets
// existing.FanartExists=false; detectFiles sees fanart.jpg on disk and sets
// detected.FanartExists=true; the disagreement triggers Update() which calls
// persistNormalized -> images.UpsertAll, repopulating the row. This test
// pins that end-to-end recovery as a regression for #1225.
func TestScan_ReconcilesArtistImagesRegistry(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Reconcile Test", "fanart.jpg")
	svc, artistSvc, db := setupScannerWithDB(t, libDir)
	ctx := context.Background()

	// First scan: artist created, fanart row written.
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Reconcile Test"))
	if a == nil {
		t.Fatal("artist not found after first scan")
	}
	if !a.FanartExists {
		t.Fatal("FanartExists should be true after first scan with fanart.jpg present")
	}

	imgs, err := artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("getting images: %v", err)
	}
	hasFanart := false
	for _, img := range imgs {
		if img.ImageType == "fanart" && img.SlotIndex == 0 && img.Exists {
			hasFanart = true
			break
		}
	}
	if !hasFanart {
		t.Fatal("expected artist_images fanart row after first scan")
	}

	// Simulate the registry-vs-disk drift in #1225: registry row vanished
	// while the file is still on disk. Bypass the service layer so the
	// artists row's other columns are untouched.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM artist_images WHERE artist_id = ? AND image_type = 'fanart'`, a.ID); err != nil {
		t.Fatalf("deleting fanart row: %v", err)
	}

	// Second scan reconciles. With the row deleted, hydrateImages sets
	// existing.FanartExists=false, detectFiles sees the file and sets
	// detected.FanartExists=true, so processDirectory marks the artist
	// changed and Update() repopulates the row via persistNormalized.
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	imgs, err = artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("getting images: %v", err)
	}
	hasFanart = false
	for _, img := range imgs {
		if img.ImageType == "fanart" && img.SlotIndex == 0 && img.Exists {
			hasFanart = true
			break
		}
	}
	if !hasFanart {
		t.Fatalf("expected artist_images fanart row to be reconciled by second scan; the registry must converge with disk on every visit (issue #1225)")
	}
}

// TestScan_ReconcileImagesOnUnchangedRescan covers the no-flag-change branch
// of processDirectory: when a second scan finds nothing different about an
// artist (same flags, same files, same dimensions), the artist_images
// registry must still converge through ReconcileImages so any silent
// out-of-band row loss heals on the next visit. Before the fix in this PR
// the no-change branch did nothing, leaving stale registries permanently
// stuck unless some other field happened to flip.
func TestScan_ReconcileImagesOnUnchangedRescan(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Stable Artist", "fanart.jpg")
	svc, artistSvc, _ := setupScannerWithDB(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Stable Artist"))
	if a == nil {
		t.Fatal("artist not found after first scan")
	}

	// Second scan: nothing on disk changed, so processDirectory should hit
	// the no-flag-change branch and call ReconcileImages directly. The call
	// must succeed without error and leave the registry intact. Run() returns
	// the pre-scan snapshot (counters all zero), so we read the finished
	// snapshot from waitForScan to assert UpdatedArtists stayed at zero.
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	finished := waitForScan(t, svc, 5*time.Second)
	if finished == nil {
		t.Fatal("waitForScan returned nil after Run 2")
	}
	if finished.UpdatedArtists != 0 {
		t.Errorf("UpdatedArtists = %d, want 0 (rescan should detect no change)", finished.UpdatedArtists)
	}

	imgs, err := artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("getting images: %v", err)
	}
	hasFanart := false
	for _, img := range imgs {
		if img.ImageType == "fanart" && img.SlotIndex == 0 && img.Exists {
			hasFanart = true
		}
	}
	if !hasFanart {
		t.Error("registry must still hold the fanart row after a no-change rescan")
	}
}

// TestArtistService_ReconcileImages_IdempotentConvergence pins the contract
// of the public ReconcileImages method (issue #1225 support API): given an
// Artist whose image flags reflect filesystem-truth, repeatedly calling
// ReconcileImages converges the artist_images registry on those flags
// without duplicating rows or leaving stale rows behind. This is the surface
// future callers can use for explicit registry repair when they have an
// authoritative image-flag snapshot to write through.
func TestArtistService_ReconcileImages_IdempotentConvergence(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	a := &artist.Artist{Name: "Reconcile Idempotent"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Seed: artist has no rows initially.
	imgs, err := artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("initial GetImagesForArtist: %v", err)
	}
	if len(imgs) != 0 {
		t.Fatalf("expected 0 image rows, got %d", len(imgs))
	}

	// Reconcile with a fanart-present model. Registry should grow a row and
	// the call must report the registry was repaired.
	a.FanartExists = true
	a.FanartCount = 1
	repaired, err := artistSvc.ReconcileImages(ctx, a)
	if err != nil {
		t.Fatalf("ReconcileImages (add fanart): %v", err)
	}
	if !repaired {
		t.Error("expected repaired=true when adding the first fanart row")
	}
	imgs, err = artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist after add: %v", err)
	}
	hasFanart := false
	for _, img := range imgs {
		if img.ImageType == "fanart" && img.SlotIndex == 0 && img.Exists {
			hasFanart = true
		}
	}
	if !hasFanart {
		t.Fatal("ReconcileImages must add a fanart row for FanartExists=true")
	}

	// Idempotent replay: same model should leave the row intact and report
	// no repair.
	repaired, err = artistSvc.ReconcileImages(ctx, a)
	if err != nil {
		t.Fatalf("ReconcileImages (replay): %v", err)
	}
	if repaired {
		t.Error("expected repaired=false on idempotent replay")
	}
	imgs, err = artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist after replay: %v", err)
	}
	if len(imgs) != 1 {
		t.Errorf("idempotent replay must not duplicate rows, got %d", len(imgs))
	}

	// Reconcile with all flags cleared: row should be removed and the call
	// must report a repair.
	a.FanartExists = false
	a.FanartCount = 0
	repaired, err = artistSvc.ReconcileImages(ctx, a)
	if err != nil {
		t.Fatalf("ReconcileImages (remove fanart): %v", err)
	}
	if !repaired {
		t.Error("expected repaired=true when removing the fanart row")
	}
	imgs, err = artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetImagesForArtist after clear: %v", err)
	}
	for _, img := range imgs {
		if img.ImageType == "fanart" {
			t.Errorf("ReconcileImages must remove fanart row when FanartExists=false, found %+v", img)
		}
	}
}

// TestScan_LockDataNotReimportedOnRescan is the regression fixture for
// issue #1726. Before the fix, an NFO carrying <lockdata>true</lockdata> on
// the rescan path silently re-locked the artist on every scan, undoing any
// user unlock. The canonical lock signals are now: user UI toggle,
// per-library NFOLockData setting, and scheduled platform pull -- the
// scanner's re-scan path no longer touches artists.locked.
func TestScan_LockDataNotReimportedOnRescan(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Portishead")
	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	// First scan: bare directory, no NFO. Artist starts unlocked.
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Portishead"))
	if a == nil {
		t.Fatal("artist not found after first scan")
	}
	if a.Locked {
		t.Fatal("artist should start unlocked when no NFO exists")
	}

	// Drop in an NFO with lockdata=true (e.g. a stale on-disk lockdata bit
	// from a pre-fix Stillwater write). The re-scan must NOT re-lock.
	nfoContent := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>Portishead</name>
  <lockdata>true</lockdata>
</artist>`
	nfoPath := filepath.Join(libDir, "Portishead", "artist.nfo")
	if err := os.WriteFile(nfoPath, []byte(nfoContent), 0o644); err != nil {
		t.Fatalf("writing nfo: %v", err)
	}

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ = artistSvc.GetByPath(ctx, filepath.Join(libDir, "Portishead"))
	if a == nil {
		t.Fatal("artist not found after second scan")
	}
	if a.Locked {
		t.Error("Locked must remain false after rescan; NFO lockdata is not a re-scan signal (#1726)")
	}
	if a.LockSource != "" {
		t.Errorf("LockSource = %q, want empty (no lock applied)", a.LockSource)
	}
	if a.LockedAt != nil {
		t.Errorf("LockedAt = %v, want nil (no lock applied -- partial lock-state writes would surface here)", a.LockedAt)
	}
}

// TestScan_LockDataImportedOnInitialDiscovery verifies the new-artist code
// path still imports <lockdata>true</lockdata>: that is the only place the
// scanner gets to seed artists.locked, tagged with "initial_import" so the
// user can distinguish it from a user-toggle or platform-pulled lock
// (#1726).
func TestScan_LockDataImportedOnInitialDiscovery(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Massive Attack")
	nfoContent := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>Massive Attack</name>
  <lockdata>true</lockdata>
</artist>`
	nfoPath := filepath.Join(libDir, "Massive Attack", "artist.nfo")
	if err := os.WriteFile(nfoPath, []byte(nfoContent), 0o644); err != nil {
		t.Fatalf("writing nfo: %v", err)
	}

	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Massive Attack"))
	if a == nil {
		t.Fatal("artist not found")
	}
	if !a.Locked {
		t.Error("initial discovery with lockdata=true should set Locked")
	}
	if a.LockSource != "initial_import" {
		t.Errorf("LockSource = %q, want \"initial_import\"", a.LockSource)
	}
	if a.LockedAt == nil {
		t.Error("LockedAt should be set on initial-import lock")
	}
}

// TestScan_NoLockDataLeavesUnlocked guards against a false positive: an NFO
// without <lockdata> (or with lockdata=false) must not flip the artist into
// the locked state on rescan. Two NFO variants are exercised in sequence so
// both the missing-tag and the explicit-false parsing branches are covered.
func TestScan_NoLockDataLeavesUnlocked(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Massive Attack")
	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	nfoPath := filepath.Join(libDir, "Massive Attack", "artist.nfo")

	cases := []struct {
		name string
		nfo  string
	}{
		{
			name: "missing lockdata",
			nfo: `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>Massive Attack</name>
  <biography>Bristol trip-hop pioneers.</biography>
</artist>`,
		},
		{
			name: "explicit lockdata false",
			nfo: `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>Massive Attack</name>
  <lockdata>false</lockdata>
</artist>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(nfoPath, []byte(tc.nfo), 0o644); err != nil {
				t.Fatalf("writing nfo: %v", err)
			}
			if _, err := svc.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}
			waitForScan(t, svc, 5*time.Second)

			a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Massive Attack"))
			if a == nil {
				t.Fatal("artist not found")
			}
			if a.Locked {
				t.Errorf("Locked must remain false for %q NFO", tc.name)
			}
		})
	}
}

// TestPreservePlaceholders verifies that preservePlaceholders fills in empty
// placeholder values from the existing artist when the image file is present,
// and leaves placeholders unchanged when the image file is absent.
func TestPreservePlaceholders(t *testing.T) {
	t.Parallel()

	existing := &artist.Artist{
		ThumbPlaceholder:  "data:image/jpeg;base64,THUMB",
		FanartPlaceholder: "data:image/jpeg;base64,FANART",
		LogoPlaceholder:   "data:image/png;base64,LOGO",
		BannerPlaceholder: "data:image/jpeg;base64,BANNER",
	}

	t.Run("preserves when probe returned empty and file exists", func(t *testing.T) {
		t.Parallel()
		d := detectedFiles{
			ThumbExists:  true,
			FanartExists: true,
			LogoExists:   true,
			BannerExists: true,
			// All placeholders empty -- simulates a transient decode failure.
		}
		preservePlaceholders(existing, &d)

		if d.ThumbPlaceholder != existing.ThumbPlaceholder {
			t.Errorf("ThumbPlaceholder = %q, want %q", d.ThumbPlaceholder, existing.ThumbPlaceholder)
		}
		if d.FanartPlaceholder != existing.FanartPlaceholder {
			t.Errorf("FanartPlaceholder = %q, want %q", d.FanartPlaceholder, existing.FanartPlaceholder)
		}
		if d.LogoPlaceholder != existing.LogoPlaceholder {
			t.Errorf("LogoPlaceholder = %q, want %q", d.LogoPlaceholder, existing.LogoPlaceholder)
		}
		if d.BannerPlaceholder != existing.BannerPlaceholder {
			t.Errorf("BannerPlaceholder = %q, want %q", d.BannerPlaceholder, existing.BannerPlaceholder)
		}
	})

	t.Run("does not fill when file is absent", func(t *testing.T) {
		t.Parallel()
		d := detectedFiles{
			// No *Exists flags set; placeholders empty.
		}
		preservePlaceholders(existing, &d)

		if d.ThumbPlaceholder != "" {
			t.Errorf("ThumbPlaceholder = %q, want empty (no file)", d.ThumbPlaceholder)
		}
		if d.FanartPlaceholder != "" {
			t.Errorf("FanartPlaceholder = %q, want empty (no file)", d.FanartPlaceholder)
		}
	})

	t.Run("does not overwrite a freshly generated placeholder", func(t *testing.T) {
		t.Parallel()
		fresh := "data:image/jpeg;base64,FRESH"
		d := detectedFiles{
			ThumbExists:      true,
			ThumbPlaceholder: fresh,
		}
		preservePlaceholders(existing, &d)

		if d.ThumbPlaceholder != fresh {
			t.Errorf("ThumbPlaceholder = %q, want %q (fresh placeholder must not be overwritten)", d.ThumbPlaceholder, fresh)
		}
	})
}

// TestPreserveDimensions verifies that preserveDimensions copies stored
// dimensions into detected when a probe failure returns zero dimensions, and
// leaves detected values untouched when a probe succeeded or when the existing
// dimensions are also zero.
func TestPreserveDimensions(t *testing.T) {
	t.Parallel()

	existing := &artist.Artist{
		ThumbWidth:   500,
		ThumbHeight:  500,
		FanartWidth:  1920,
		FanartHeight: 1080,
		LogoWidth:    800,
		LogoHeight:   310,
		BannerWidth:  1000,
		BannerHeight: 185,
	}

	t.Run("fills zero dimensions from existing when file exists and probe failed", func(t *testing.T) {
		t.Parallel()
		// All *Exists flags true (file present), all dims 0 (probe failed).
		d := detectedFiles{
			ThumbExists:  true,
			FanartExists: true,
			LogoExists:   true,
			BannerExists: true,
		}
		preserveDimensions(existing, &d)

		if d.ThumbWidth != 500 || d.ThumbHeight != 500 {
			t.Errorf("thumb dims = %dx%d, want 500x500", d.ThumbWidth, d.ThumbHeight)
		}
		if d.FanartWidth != 1920 || d.FanartHeight != 1080 {
			t.Errorf("fanart dims = %dx%d, want 1920x1080", d.FanartWidth, d.FanartHeight)
		}
		if d.LogoWidth != 800 || d.LogoHeight != 310 {
			t.Errorf("logo dims = %dx%d, want 800x310", d.LogoWidth, d.LogoHeight)
		}
		if d.BannerWidth != 1000 || d.BannerHeight != 185 {
			t.Errorf("banner dims = %dx%d, want 1000x185", d.BannerWidth, d.BannerHeight)
		}
	})

	t.Run("does not overwrite a successful probe result", func(t *testing.T) {
		t.Parallel()
		d := detectedFiles{
			ThumbExists: true,
			ThumbWidth:  600,
			ThumbHeight: 400,
		}
		preserveDimensions(existing, &d)

		if d.ThumbWidth != 600 || d.ThumbHeight != 400 {
			t.Errorf("thumb dims = %dx%d, want 600x400 (probe result must not be overwritten)", d.ThumbWidth, d.ThumbHeight)
		}
	})

	t.Run("does not fill when existing dims are zero", func(t *testing.T) {
		t.Parallel()
		zeroed := &artist.Artist{} // all dims zero
		d := detectedFiles{ThumbExists: true}
		preserveDimensions(zeroed, &d)

		if d.ThumbWidth != 0 || d.ThumbHeight != 0 {
			t.Errorf("thumb dims = %dx%d, want 0x0 (no fallback when existing is also zero)", d.ThumbWidth, d.ThumbHeight)
		}
	})

	t.Run("does not fill when file is absent", func(t *testing.T) {
		t.Parallel()
		// *Exists false means the file was deleted -- stale dims must not be copied.
		d := detectedFiles{
			ThumbExists:  false,
			FanartExists: false,
			LogoExists:   false,
			BannerExists: false,
		}
		preserveDimensions(existing, &d)

		if d.ThumbWidth != 0 || d.ThumbHeight != 0 {
			t.Errorf("thumb dims = %dx%d, want 0x0 when file is absent", d.ThumbWidth, d.ThumbHeight)
		}
		if d.FanartWidth != 0 || d.FanartHeight != 0 {
			t.Errorf("fanart dims = %dx%d, want 0x0 when file is absent", d.FanartWidth, d.FanartHeight)
		}
		if d.LogoWidth != 0 || d.LogoHeight != 0 {
			t.Errorf("logo dims = %dx%d, want 0x0 when file is absent", d.LogoWidth, d.LogoHeight)
		}
		if d.BannerWidth != 0 || d.BannerHeight != 0 {
			t.Errorf("banner dims = %dx%d, want 0x0 when file is absent", d.BannerWidth, d.BannerHeight)
		}
	})
}

// withProbeImageFileFn swaps the package-level probeImageFileFn for the
// duration of the test. Tests use it to count or short-circuit the
// expensive per-file image probe (dimension decode + perceptual-hash
// placeholder generation) that detectFiles runs.
//
// Not safe under t.Parallel since probeImageFileFn is a package-level
// var; callers must run sequentially.
func withProbeImageFileFn(t *testing.T, fn func(filePath, imageType, existingPlaceholder string) (int, int, bool, string)) {
	t.Helper()
	prev := probeImageFileFn
	probeImageFileFn = fn
	t.Cleanup(func() { probeImageFileFn = prev })
}

// forceLastScannedAtFuture pushes the artist's LastScannedAt forward by one
// hour so the mtime fast-path is guaranteed to engage on the next scan
// (mtime <= now < lastScannedAt). Without this, sub-second timing between
// scan-1 finish and scan-2 stat would race the fast-path check, since the
// DB stores last_scanned_at at second precision while filesystem mtime
// carries nanoseconds.
func forceLastScannedAtFuture(t *testing.T, db *sql.DB, artistID string) {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE artists SET last_scanned_at = ? WHERE id = ?`, future, artistID); err != nil {
		t.Fatalf("forcing last_scanned_at future: %v", err)
	}
}

// TestScan_FastPath_ReconcilesArtistImagesRegistry pins the #1225 contract
// against the D7 mtime fast-path (#1413): even when the fast path engages
// (mtime <= lastScannedAt), the registry must still converge with disk on
// every visit. Earlier D7 drafts synthesized detected.FanartExists from
// existing.FanartExists, which hydrateImages had already corrupted to
// match the broken registry -- so detected == existing, no change branch
// fired, and the registry stayed broken forever.
//
// This test forces the fast path to engage by pushing LastScannedAt into
// the future after scan 1, so the legacy timing race (sub-second mtime
// vs. second-precision lastScannedAt) cannot mask the bug.
func TestScan_FastPath_ReconcilesArtistImagesRegistry(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "FastPath Reconcile", "fanart.jpg")
	svc, artistSvc, db := setupScannerWithDB(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "FastPath Reconcile"))
	if a == nil {
		t.Fatal("artist not found after first scan")
	}
	if !a.FanartExists {
		t.Fatal("FanartExists should be true after first scan with fanart.jpg present")
	}

	// Delete the registry row (simulating #1225 drift) and push
	// LastScannedAt into the future so the next scan is guaranteed to
	// take the fast path.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM artist_images WHERE artist_id = ? AND image_type = 'fanart'`, a.ID); err != nil {
		t.Fatalf("deleting fanart row: %v", err)
	}
	forceLastScannedAtFuture(t, db, a.ID)

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	imgs, err := artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("getting images: %v", err)
	}
	hasFanart := false
	for _, img := range imgs {
		if img.ImageType == "fanart" && img.SlotIndex == 0 && img.Exists {
			hasFanart = true
			break
		}
	}
	if !hasFanart {
		t.Fatalf("expected artist_images fanart row to be reconciled by the fast-path scan; the registry must converge with disk on every visit even when the mtime fast path is taken (issue #1225 + #1413)")
	}
}

// TestScan_FastPath_SkipsExpensiveProbe asserts that the D7 fast path does
// NOT invoke probeImageFile (the expensive image-header decode +
// perceptual-hash placeholder generation) on an unchanged-mtime rescan.
// This is the perf win the fast path exists to deliver; if the count is
// non-zero after the second scan, the fast path is silently doing the
// expensive work and the optimization is broken.
//
// The companion contract -- existence checks DO still run -- is pinned by
// TestScan_FastPath_ReconcilesArtistImagesRegistry above and by
// TestScan_FastPath_DetectsFileRemoval below.
func TestScan_FastPath_SkipsExpensiveProbe(t *testing.T) {
	// No t.Parallel: probeImageFileFn is a package-level var.
	var calls int
	withProbeImageFileFn(t, func(filePath, imageType, existingPlaceholder string) (int, int, bool, string) {
		calls++
		// Delegate to real implementation so scan 1 still produces a
		// fully populated artist record.
		return probeImageFile(filePath, imageType, existingPlaceholder)
	})

	libDir := t.TempDir()
	createArtistDir(t, libDir, "FastPath Skip", "fanart.jpg")
	svc, artistSvc, db := setupScannerWithDB(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "FastPath Skip"))
	if a == nil {
		t.Fatal("artist not found after first scan")
	}
	scan1Calls := calls
	if scan1Calls == 0 {
		t.Fatal("scan 1 should have called probeImageFile at least once (sanity check on the test seam)")
	}

	// Force the fast path to engage on scan 2.
	forceLastScannedAtFuture(t, db, a.ID)

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	if got := calls - scan1Calls; got != 0 {
		t.Errorf("probeImageFile was called %d times during the fast-path rescan; want 0 (the perf win of D7 is that image decode + placeholder generation are skipped when mtime is unchanged)", got)
	}
}

// TestScan_FastPath_DetectsFileRemoval pins that the fast path still
// honors disk existence even when it skips the expensive probe. Removing
// fanart.jpg between scans must flip FanartExists to false on the next
// visit -- otherwise the fast path becomes a stale-data cache instead of
// a perf optimization.
//
// Note: physically removing the file bumps the directory's mtime, so a
// strict reading of "mtime fast path" would say this case is actually
// served by the legacy detectFiles path. We explicitly push
// LastScannedAt past the post-delete mtime so the fast path engages
// nonetheless, which is the worst case for the contract.
func TestScan_FastPath_DetectsFileRemoval(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	dir := filepath.Join(libDir, "FastPath Remove")
	createArtistDir(t, libDir, "FastPath Remove", "fanart.jpg")
	svc, artistSvc, db := setupScannerWithDB(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "FastPath Remove"))
	if a == nil {
		t.Fatal("artist not found after first scan")
	}
	if !a.FanartExists {
		t.Fatal("FanartExists should be true after first scan")
	}

	// Physically remove the fanart file (this bumps the dir mtime).
	if err := os.Remove(filepath.Join(dir, "fanart.jpg")); err != nil {
		t.Fatalf("removing fanart.jpg: %v", err)
	}
	// Push LastScannedAt past the new mtime so the fast path still
	// engages despite the disk change. This is the contract-strict
	// test: even when the mtime check would let stale data slip
	// through, the existence probe inside the fast path must catch
	// the removal.
	forceLastScannedAtFuture(t, db, a.ID)

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a2, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "FastPath Remove"))
	if a2 == nil {
		t.Fatal("artist not found after second scan")
	}
	if a2.FanartExists {
		t.Errorf("FanartExists = true after fanart.jpg removed; the fast path must still check disk existence, not just trust cached flags")
	}

	imgs, err := artistSvc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("getting images: %v", err)
	}
	for _, img := range imgs {
		if img.ImageType == "fanart" && img.SlotIndex == 0 && img.Exists {
			t.Errorf("artist_images still reports fanart exists=true after the file was removed; the fast path must converge with disk")
		}
	}
}

// TestScan_NearDuplicateDetected covers the scanner-side near-duplicate check
// introduced in #1614. When two directories in the same library normalize
// (via artist.NormalizeIdentityKey) to the same identity key, the scanner must:
//
//   - increment ScanResult.SuspectedDuplicates (informational counter),
//   - still create the new artist row (the check is non-blocking).
//
// The test uses "AC_DC" (scan 1) and "AC-DC" (scan 2) as the colliding pair.
// NormalizeIdentityKey's separator-fold step (step 7) maps both hyphens and
// underscores to a single space, so both names produce the key "ac dc".
// The two directory names are also distinct to the filesystem (macOS APFS is
// case-insensitive but underscore != hyphen), so both can coexist on disk.
func TestScan_NearDuplicateDetected(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()

	// Seed the library row first so AddDerivingSource (called by artist.Create
	// during scan 1) can insert the artist_libraries membership row that
	// PreloadArtistsByLibrary depends on during scan 2.
	svc, artistSvc, db := setupScannerWithDB(t, libDir)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-dup', 'Dup Test', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		libDir); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	svc.SetLibraryLister(&stubLibraryLister{
		libs: []library.Library{
			{ID: "lib-dup", Name: "Dup Test", Path: libDir, Type: library.TypeRegular},
		},
	})

	// Scan 1: create "AC_DC" so it is persisted in the DB with the library
	// membership row that will land it in the preload map during scan 2.
	createArtistDir(t, libDir, "AC_DC")

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	existing, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "AC_DC"))
	if existing == nil {
		t.Fatal("expected AC_DC to be created in scan 1")
	}

	// Scan 2: add "AC-DC" whose name normalizes to the same key ("ac dc") as
	// "AC_DC" via the separator-fold step in NormalizeIdentityKey.
	createArtistDir(t, libDir, "AC-DC")

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	result2 := waitForScan(t, svc, 5*time.Second)

	// The counter must be incremented for the collision.
	if result2.SuspectedDuplicates != 1 {
		t.Errorf("SuspectedDuplicates = %d, want 1", result2.SuspectedDuplicates)
	}

	// The new artist row must still have been created (the check is informational,
	// not a gate -- the increment happens after Create).
	duplicate, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "AC-DC"))
	if duplicate == nil {
		t.Error("expected AC-DC artist row to be created despite the duplicate flag")
	}
}
