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

func waitForScan(t *testing.T, svc *Service, timeout time.Duration) *ScanResult { //nolint:unparam
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
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Visible Artist")
	svc, artistSvc := setupScanner(t, libDir)

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
	os.WriteFile(filepath.Join(libDir, "Radiohead", "folder.jpg"), []byte("jpg"), 0o644) //nolint:errcheck
	os.WriteFile(filepath.Join(libDir, "Radiohead", "fanart.jpg"), []byte("jpg"), 0o644) //nolint:errcheck

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
	dir := t.TempDir()
	for _, name := range []string{"artist.nfo", "folder.jpg", "backdrop.jpg", "logo.png", "banner.jpg"} {
		os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o644) //nolint:errcheck
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
	// Pass a path that does not exist so os.ReadDir fails.
	_, err := detectFiles(filepath.Join(t.TempDir(), "nonexistent"), nil)
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestScan_NoLibraries_NoFallback(t *testing.T) {
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

// TestScan_LockDataImportedOnRescan verifies that <lockdata>true</lockdata>
// added to an existing artist's NFO (e.g. by another tool, or a later
// Stillwater write under a per-library NFOLockData=true setting) is mirrored
// to the artist-level Locked flag on the next scan, so the artist UI reflects
// that the metadata is locked. Regression coverage for the rescan path that
// previously discarded populateFromNFO's lockdata return value.
func TestScan_LockDataImportedOnRescan(t *testing.T) {
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

	// Drop in an NFO with lockdata=true (simulating an external tool, or a
	// downstream Stillwater write under NFOLockData=true).
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
	if !a.Locked {
		t.Error("Locked should be true after rescan picked up <lockdata>true</lockdata>")
	}
	if a.LockSource != "imported" {
		t.Errorf("LockSource = %q, want \"imported\"", a.LockSource)
	}
	if a.LockedAt == nil {
		t.Error("LockedAt should be set after lockdata import")
	}
}

// TestScan_NoLockDataLeavesUnlocked guards against a false positive: an NFO
// without <lockdata> (or with lockdata=false) must not flip the artist into
// the locked state on rescan.
func TestScan_NoLockDataLeavesUnlocked(t *testing.T) {
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Massive Attack")
	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	nfoContent := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<artist>
  <name>Massive Attack</name>
  <biography>Bristol trip-hop pioneers.</biography>
</artist>`
	nfoPath := filepath.Join(libDir, "Massive Attack", "artist.nfo")
	if err := os.WriteFile(nfoPath, []byte(nfoContent), 0o644); err != nil {
		t.Fatalf("writing nfo: %v", err)
	}

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Massive Attack"))
	if a == nil {
		t.Fatal("artist not found")
	}
	if a.Locked {
		t.Error("Locked must remain false when NFO has no <lockdata>")
	}
}
