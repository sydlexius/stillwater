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
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/event"
	swimage "github.com/sydlexius/stillwater/internal/image"
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
	// "Soundtrack" exercises the operator-editable exclusion list (creates
	// the artist row, flags IsExcluded). It is NOT one of the built-in
	// non-artist names (Various Artists / VA), which are dropped entirely --
	// those are covered by TestScan_SkipsVariousArtists.
	createArtistDir(t, libDir, "Soundtrack", "folder.jpg")
	createArtistDir(t, libDir, "Nirvana", "folder.jpg")
	createArtistDir(t, libDir, "OST", "fanart.jpg")

	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := NewService(artistSvc, nil, nil, logger, libDir, []string{"Soundtrack", "OST"})
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.NewArtists != 3 {
		t.Errorf("NewArtists = %d, want 3", final.NewArtists)
	}

	// Check Soundtrack is excluded (operator exclusion list -> row created, flagged)
	va, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Soundtrack"))
	if va == nil {
		t.Fatal("Soundtrack not found")
	}
	if !va.IsExcluded {
		t.Error("Soundtrack should be excluded")
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

func TestScan_SkipsSystemJunkDirs(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	// OS/NAS junk: none of these should become an artist, and none should be
	// counted as a scanned directory. Mix of dot-prefixed and not.
	for _, junk := range []string{
		"$RECYCLE.BIN",
		"System Volume Information",
		"@eaDir",
		"@__thumb",
		"lost+found",
		".Trashes",
	} {
		createArtistDir(t, libDir, junk, "folder.jpg")
	}
	// A case-variant to confirm matching is case-insensitive.
	createArtistDir(t, libDir, "system volume information", "folder.jpg")
	createArtistDir(t, libDir, "Nirvana", "folder.jpg")

	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.NewArtists != 1 {
		t.Errorf("NewArtists = %d, want 1 (only Nirvana)", final.NewArtists)
	}
	if final.TotalDirectories != 1 {
		t.Errorf("TotalDirectories = %d, want 1 (junk dirs not counted)", final.TotalDirectories)
	}
	// The real artist survives.
	if nir, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Nirvana")); nir == nil {
		t.Error("Nirvana should have been created")
	}
	// A junk dir must not have produced a row.
	if junk, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "@eaDir")); junk != nil {
		t.Error("@eaDir should not have been created as an artist")
	}
}

func TestScan_SkipsVariousArtists(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	// Built-in non-artist placeholder buckets: dropped entirely (no row).
	for _, name := range []string{"Various Artists", "Various", "VA", "various artist"} {
		createArtistDir(t, libDir, name, "folder.jpg")
	}
	// Guard against over-broad matching: a real artist whose name merely
	// starts with "Various" MUST still be scanned.
	createArtistDir(t, libDir, "Various Voices", "folder.jpg")
	createArtistDir(t, libDir, "Nirvana", "folder.jpg")

	svc, artistSvc := setupScanner(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	final := waitForScan(t, svc, 5*time.Second)

	if final.NewArtists != 2 {
		t.Errorf("NewArtists = %d, want 2 (Various Voices + Nirvana)", final.NewArtists)
	}
	if va, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Various Artists")); va != nil {
		t.Error("Various Artists should not have been created as an artist")
	}
	if vv, _ := artistSvc.GetByPath(ctx, filepath.Join(libDir, "Various Voices")); vv == nil {
		t.Error("Various Voices is a real artist and should have been created")
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
// persistNormalized -> images.MergeAll, repopulating the row. This test
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
	repaired, err := artistSvc.ReconcileImages(ctx, a, canonicalEnumerationFor(a))
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
	repaired, err = artistSvc.ReconcileImages(ctx, a, canonicalEnumerationFor(a))
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
	repaired, err = artistSvc.ReconcileImages(ctx, a, canonicalEnumerationFor(a))
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

// membershipSources returns the set of source strings on the artist_libraries
// rows for the given artist, keyed by library_id, so a test can assert which
// library/source pairs exist after a scan.
func membershipSources(t *testing.T, db *sql.DB, artistID string) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT library_id, source FROM artist_libraries WHERE artist_id = ?`, artistID)
	if err != nil {
		t.Fatalf("querying memberships: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	for rows.Next() {
		var lib, src string
		if err := rows.Scan(&lib, &src); err != nil {
			t.Fatalf("scanning membership: %v", err)
		}
		out[lib] = src
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating memberships: %v", err)
	}
	return out
}

// TestScan_ExistingArtistGainsFilesystemMembership proves the issue #1780 fix:
// an artist first created by a connection import (so it holds only an
// emby-sourced membership) and whose path matches a directory under a manual
// filesystem library acquires a 'filesystem'-sourced artist_libraries row after
// that library is scanned. Re-scanning is idempotent (no duplicate row, the
// added_at timestamp does not move), and an artist that already holds the
// filesystem membership is left untouched.
func TestScan_ExistingArtistGainsFilesystemMembership(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	svc, artistSvc, db := setupScannerWithDB(t, libDir)
	ctx := context.Background()

	// Seed an emby connection plus a connection-backed library, and a manual
	// (connection_id NULL) filesystem library that points at libDir. The
	// filesystem library is the one we scan.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO connections (id, name, type, url, encrypted_api_key)
		 VALUES ('conn-emby', 'Emby', 'emby', 'http://emby.local', 'x')`); err != nil {
		t.Fatalf("seeding connection: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, connection_id, created_at, updated_at)
		 VALUES ('lib-emby', 'Emby Music', '', 'regular', 'emby', 'conn-emby', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seeding emby library: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-fs', 'FS Music', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		libDir); err != nil {
		t.Fatalf("seeding filesystem library: %v", err)
	}

	// Create the artist as a connection import would: LibraryID points at the
	// emby library, so Create records an 'emby'-sourced membership only. Its
	// path matches a directory under the filesystem library.
	artistPath := filepath.Join(libDir, "Pink Floyd")
	a := &artist.Artist{
		Name:      "Pink Floyd",
		SortName:  "Pink Floyd",
		Path:      artistPath,
		LibraryID: "lib-emby",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Sanity: only the emby membership exists pre-scan.
	pre := membershipSources(t, db, a.ID)
	if pre["lib-emby"] != "emby" {
		t.Fatalf("pre-scan emby membership = %q, want 'emby'", pre["lib-emby"])
	}
	if _, ok := pre["lib-fs"]; ok {
		t.Fatalf("pre-scan should not yet hold a filesystem membership, got %v", pre)
	}

	createArtistDir(t, libDir, "Pink Floyd")
	svc.SetLibraryLister(&stubLibraryLister{
		libs: []library.Library{
			{ID: "lib-fs", Name: "FS Music", Path: libDir, Type: library.TypeRegular},
		},
	})

	// Scan 1: the existing artist is matched by path and must gain the
	// filesystem membership.
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	after1 := membershipSources(t, db, a.ID)
	if after1["lib-emby"] != "emby" {
		t.Errorf("after scan 1 emby membership = %q, want 'emby'", after1["lib-emby"])
	}
	if after1["lib-fs"] != "filesystem" {
		t.Errorf("after scan 1 filesystem membership = %q, want 'filesystem'", after1["lib-fs"])
	}

	// Capture the filesystem row's added_at to prove idempotency below.
	var addedAt1 string
	if err := db.QueryRowContext(ctx,
		`SELECT added_at FROM artist_libraries WHERE artist_id = ? AND library_id = 'lib-fs'`,
		a.ID).Scan(&addedAt1); err != nil {
		t.Fatalf("reading filesystem added_at: %v", err)
	}

	// Scan 2: idempotent. Exactly one filesystem membership, added_at unchanged.
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	var fsCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = ? AND library_id = 'lib-fs'`,
		a.ID).Scan(&fsCount); err != nil {
		t.Fatalf("counting filesystem memberships: %v", err)
	}
	if fsCount != 1 {
		t.Errorf("after scan 2 filesystem membership count = %d, want 1", fsCount)
	}

	var addedAt2 string
	if err := db.QueryRowContext(ctx,
		`SELECT added_at FROM artist_libraries WHERE artist_id = ? AND library_id = 'lib-fs'`,
		a.ID).Scan(&addedAt2); err != nil {
		t.Fatalf("reading filesystem added_at after scan 2: %v", err)
	}
	if addedAt2 != addedAt1 {
		t.Errorf("filesystem added_at moved on rescan: %q -> %q", addedAt1, addedAt2)
	}

	// The emby membership must be untouched throughout.
	after2 := membershipSources(t, db, a.ID)
	if after2["lib-emby"] != "emby" {
		t.Errorf("after scan 2 emby membership = %q, want 'emby'", after2["lib-emby"])
	}
}

func TestHasNumericSuffix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{"1", true},
		{"42", true},
		{"007", true},
		{"a", false},
		{"1a", false},
		{"a1", false},
		{"1-2", false},
		{"!", false},
	}
	for _, tc := range tests {
		got := hasNumericSuffix(tc.input)
		if got != tc.want {
			t.Errorf("hasNumericSuffix(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// panicLibraryLister panics on List, standing in for an unexpected failure
// (e.g. a corrupt library record) partway through the scan goroutine.
type panicLibraryLister struct{}

func (panicLibraryLister) List(_ context.Context) ([]library.Library, error) {
	panic("simulated scan panic")
}

// TestScan_RecoversFromPanic verifies runScan's recover() defer: a panic
// inside the scan goroutine must not crash the process, must leave the scan
// result as "failed" (never "completed"), and must still release scanWg so
// Shutdown/waitForScan never deadlock waiting on a panicked scan.
func TestScan_RecoversFromPanic(t *testing.T) {
	t.Parallel()
	svc, _ := setupScanner(t, "")
	svc.SetLibraryLister(panicLibraryLister{})

	if _, err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	final := waitForScan(t, svc, 5*time.Second)

	if final.Status != "failed" {
		t.Errorf("status = %q, want failed", final.Status)
	}
	if final.Error == "" {
		t.Error("Error = \"\", want a non-empty panic message")
	}
	if final.CompletedAt == nil {
		t.Error("CompletedAt = nil, want set")
	}

	// A leaked scanWg would hang Shutdown forever; bound it so a
	// regression fails the test instead of hanging the suite.
	done := make(chan struct{})
	go func() {
		svc.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return: scanWg was not released after a panicked scan")
	}
}

// TestPostScanHook_RunsBeforeScanCompletes locks the hook contract the #2380
// path-mapping re-run depends on: the hook fires once per scan, and it runs
// BEFORE the scan's status is stamped "completed" (inside the scan's
// WaitGroup slot), so a caller that observes "completed" has also implicitly
// waited for the hook.
func TestPostScanHook_RunsBeforeScanCompletes(t *testing.T) {
	// An empty library keeps the test on the scan LIFECYCLE (which is what the
	// hook contract is about) rather than on artist persistence, which needs a
	// seeded library row.
	libDir := t.TempDir()
	svc, _ := setupScanner(t, libDir)

	var calls int32
	var statusAtHook string
	svc.SetPostScanHook(func(context.Context) {
		atomic.AddInt32(&calls, 1)
		// The hook runs while the scan is still marked "running": completion is
		// stamped only after the hook returns, so "completed" genuinely means the
		// post-scan work is done.
		if st := svc.Status(); st != nil {
			statusAtHook = st.Status
		}
	})

	if _, err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The hook runs BEFORE the status settles, so a settled status is proof the
	// hook already ran - no Shutdown needed (and Shutdown would cancel the hook's
	// context, which is precisely the race the ordering avoids).
	waitForScan(t, svc, 5*time.Second)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("post-scan hook called %d times, want exactly 1", got)
	}
	if statusAtHook != "running" {
		t.Errorf("scan status seen by the hook = %q, want %q (the hook must run BEFORE completion "+
			"is published, so an observer that sees 'completed' knows the hook already ran)",
			statusAtHook, "running")
	}
	if final := svc.Status(); final == nil || final.Status != "completed" {
		t.Errorf("final scan status = %+v, want completed", final)
	}
}

// TestPostScanHook_PanicIsRecovered guards runScan's hook invocation: the
// hook is documented as best-effort and must never be able to crash the scan
// goroutine, but the recover() defer registered for the SCAN's own panics
// runs (LIFO) BEFORE the hook is even called, so it cannot catch a panic
// inside the hook -- that call needs its own recover. Without one this test
// crashes the whole test binary rather than failing cleanly.
func TestPostScanHook_PanicIsRecovered(t *testing.T) {
	var logBuf bytes.Buffer
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))
	libDir := t.TempDir()
	svc := NewService(artistSvc, nil, nil, logger, libDir, nil)

	svc.SetPostScanHook(func(context.Context) {
		panic("post-scan hook exploded")
	})

	if _, err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	final := waitForScan(t, svc, 5*time.Second)

	// The scan itself must still report success: the hook's panic is not the
	// SCAN's failure, it is the hook's, and the two must not be conflated.
	if final.Status != "completed" {
		t.Errorf("status = %q, want completed (a panicking hook must not fail the scan)", final.Status)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "post-scan hook panicked") {
		t.Errorf("log output = %q, want it to contain the recovered-panic message", logged)
	}
	if !strings.Contains(logged, "post-scan hook exploded") {
		t.Errorf("log output = %q, want the recovered panic value logged, not swallowed", logged)
	}

	// A leaked scanWg here would mean the panic escaped the hook's own defer
	// and unwound past scanWg.Done(), hanging Shutdown -- the same regression
	// guard TestScan_RecoversFromPanic applies to a panic in the scan body.
	done := make(chan struct{})
	go func() {
		svc.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return: scanWg was not released after a panicking hook")
	}
}

// canonicalEnumerationFor models a caller that walked the artist directory for
// every canonical image type and found what the Artist's flat fields describe.
// Test-only: production code builds its enumeration from the directory walk's
// own output (see imageEnumeration), never from the Artist struct, because the
// struct is a lossy re-derivation of that walk.
func canonicalEnumerationFor(a *artist.Artist) []artist.ImageEnumeration {
	count := func(exists bool) int {
		if exists {
			return 1
		}
		return 0
	}
	return []artist.ImageEnumeration{
		{ImageType: "thumb", FoundSlots: count(a.ThumbExists)},
		{ImageType: "fanart", FoundSlots: a.FanartCount},
		{ImageType: "logo", FoundSlots: count(a.LogoExists)},
		{ImageType: "banner", FoundSlots: count(a.BannerExists)},
	}
}

// TestDetectFiles_OrphanNumberedFanart is the detection half of the #2635
// amplification.
//
// Fanart existence used to be decided by the PRIMARY patterns alone
// (fanart.jpg, backdrop.jpg, ...), so an artist holding only fanart1.jpg
// reported FanartExists=false and FanartCount=0. That was already wrong -- the
// artwork is on disk and the UI hid it -- but once the scanner gained a
// destructive reconcile it became data loss, because a count of zero is a
// POSITIVE claim that every stored fanart row is stale.
//
// The state is reachable in normal operation: a slot delete that fails partway
// skips renumbering and leaves exactly this shape (#2644).
func TestDetectFiles_OrphanNumberedFanart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Deliberately NO fanart.jpg: only the numbered variant survives.
	for _, name := range []string{"fanart1.jpg", "fanart2.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o644); err != nil {
			t.Fatalf("writing %s fixture: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "fanart.jpg")); !os.IsNotExist(err) {
		t.Fatalf("precondition: fanart.jpg must be absent, stat err = %v", err)
	}

	d, err := detectFiles(dir, nil)
	if err != nil {
		t.Fatalf("detectFiles: %v", err)
	}

	if !d.FanartExists {
		t.Error("FanartExists = false with two fanart files on disk; the scanner " +
			"reports no artwork and a reconcile then deletes the rows for files " +
			"that are sitting right there")
	}
	if d.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want 2; the count bounds deletion, so an "+
			"under-count destroys rows for real files", d.FanartCount)
	}
}

// TestDetectFilesExistenceOnly_OrphanNumberedFanart holds the mtime fast path
// to the same answer as the full probe. A fast path that disagreed with a full
// re-probe would make the destruction intermittent -- present or absent
// depending on whether a directory's mtime happened to move -- which is the
// hardest possible shape to diagnose from an incident report.
func TestDetectFilesExistenceOnly_OrphanNumberedFanart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"fanart1.jpg", "fanart2.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o644); err != nil {
			t.Fatalf("writing %s fixture: %v", name, err)
		}
	}

	// The fast path only engages when nothing has been touched since the last
	// scan, so the fixture's write time must predate it.
	future := time.Now().Add(time.Hour)
	d, err := detectFilesExistenceOnly(dir, &artist.Artist{LastScannedAt: &future})
	if err != nil {
		t.Fatalf("detectFilesExistenceOnly: %v", err)
	}

	if !d.FanartExists || d.FanartCount != 2 {
		t.Errorf("fast path reported FanartExists=%v FanartCount=%d, want true/2; "+
			"it must not disagree with detectFiles or the registry damage becomes "+
			"dependent on directory mtime", d.FanartExists, d.FanartCount)
	}
}

// TestImageEnumeration_DerivesFromDetectionNotArtist pins where the delete
// bound comes from.
//
// The enumeration MUST be built from the directory walk's own output. Deriving
// it from the Artist struct would route the bound back through
// extractImageMetadata's slot-0 gating -- the very lossiness the bound exists
// to defend against -- and a lossy read would then be authorizing destruction.
func TestImageEnumeration_DerivesFromDetectionNotArtist(t *testing.T) {
	t.Parallel()
	// The orphan shape: files on disk, but no PRIMARY, so an Artist-derived
	// enumeration would report zero.
	d := detectedFiles{FanartExists: true, FanartCount: 3, ThumbExists: true}

	got := make(map[string]int)
	for _, e := range imageEnumeration(d) {
		got[e.ImageType] = e.FoundSlots
	}

	if got["fanart"] != 3 {
		t.Errorf("fanart FoundSlots = %d, want 3", got["fanart"])
	}
	if got["thumb"] != 1 {
		t.Errorf("thumb FoundSlots = %d, want 1", got["thumb"])
	}
	// Every canonical type must be present, including the absent ones: "probed
	// and found none" is what lets a genuinely-emptied type converge, and it is
	// a different statement from omitting the type.
	for _, ty := range []string{"thumb", "fanart", "logo", "banner"} {
		if _, ok := got[ty]; !ok {
			t.Errorf("image type %q missing from the enumeration; an omitted type "+
				"can never converge and its rows strand forever", ty)
		}
	}
	if got["logo"] != 0 || got["banner"] != 0 {
		t.Errorf("logo/banner FoundSlots = %d/%d, want 0/0", got["logo"], got["banner"])
	}
}

// TestScan_PrimaryFanartDeletionDoesNotWipeSurvivingSlots is the #2635
// incident, reproduced end to end through real scans against a real
// filesystem and a real database.
//
// Sequence: an artist has fanart.jpg + fanart1.jpg and is scanned, so the
// registry holds fanart ordinals 0 and 1. The operator then deletes ONLY the
// primary -- the shape a slot delete leaves behind when it fails before
// renumbering (#2644) -- and the library is rescanned.
//
// Before the fix the rescan detected fanart via the PRIMARY patterns alone,
// found none, and reported FanartExists=false / FanartCount=0.
// extractImageMetadata gates the whole fanart tail behind slot 0, so the
// desired set contained no fanart rows at all, and a type-bounded reconcile
// read that as "every fanart row is stale" and deleted BOTH -- including the
// one describing fanart1.jpg, which was never touched and is still on disk.
// That is amplification: one deleted file destroying the registry for a file
// that still exists.
//
// The assertion is on stored DB rows, not on a counter or a return value,
// because the failure being guarded is a scan that reports success while
// destroying data.
func TestScan_PrimaryFanartDeletionDoesNotWipeSurvivingSlots(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Amplified", "fanart.jpg", "fanart1.jpg")
	svc, artistSvc, _ := setupScannerWithDB(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	artistDir := filepath.Join(libDir, "Amplified")
	a, _ := artistSvc.GetByPath(ctx, artistDir)
	if a == nil {
		t.Fatal("artist not found after first scan")
	}

	// Precondition: both ordinals really are stored. Without this the test
	// would pass vacuously against a scan that never wrote the tail.
	if got := fanartSlots(t, artistSvc, a.ID); len(got) != 2 {
		t.Fatalf("precondition: want 2 stored fanart slots after the first scan, got %v; "+
			"nothing below would prove anything", got)
	}

	// Delete ONLY the primary. fanart1.jpg is untouched and still on disk.
	if err := os.Remove(filepath.Join(artistDir, "fanart.jpg")); err != nil {
		t.Fatalf("removing primary fanart: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artistDir, "fanart1.jpg")); err != nil {
		t.Fatalf("precondition: fanart1.jpg must still be on disk: %v", err)
	}

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	// Exactly one fanart row must survive, at ordinal 0, describing the one
	// file that is still there. Zero rows is the amplification. Two rows would
	// mean the deleted file was never retired, which is the opposite failure
	// and equally wrong -- so this assertion pins convergence in BOTH
	// directions rather than merely proving nothing was deleted.
	got := fanartSlots(t, artistSvc, a.ID)
	if len(got) == 0 {
		t.Fatalf("all fanart rows were destroyed, but fanart1.jpg is still on disk: " +
			"deleting one file wiped the registry entry for a file that survived")
	}
	if len(got) != 1 || !got[0] {
		t.Errorf("stored fanart slots = %v, want exactly ordinal 0 present: one file "+
			"remains on disk, so the registry must hold exactly one row for it", got)
	}
}

// fanartSlots returns the artist's stored fanart rows as a slot-index -> exists
// map, read back from the database.
func fanartSlots(t *testing.T, svc *artist.Service, artistID string) map[int]bool {
	t.Helper()
	imgs, err := svc.GetImagesForArtist(context.Background(), artistID)
	if err != nil {
		t.Fatalf("GetImagesForArtist: %v", err)
	}
	out := make(map[int]bool)
	for _, im := range imgs {
		if im.ImageType == "fanart" {
			out[im.SlotIndex] = im.Exists
		}
	}
	return out
}

// TestScan_ReconcileFailureStillPublishesAndCounts pins the ordering around a
// failing image reconcile.
//
// processDirectory used to return the reconcile error immediately, BEFORE
// publishArtistUpdated and result.UpdatedArtists++. But Update() has already
// committed by that point, so the artist genuinely was updated: the early
// return suppressed the SSE fanout for a write that landed, leaving every UI
// subscriber showing stale data with no event ever coming to correct it, and
// it under-counted the scan's own work in the summary the operator reads.
//
// The failure is injected with a trigger that refuses row deletion on
// artist_images. That is surgical: it fails ONLY the reconcile's delete, while
// leaving the artist-row Update free to commit, which is precisely the state
// the ordering bug lives in.
func TestScan_ReconcileFailureStillPublishesAndCounts(t *testing.T) {
	t.Parallel()
	libDir := t.TempDir()
	artistDir := filepath.Join(libDir, "Reconcile Failure")
	createArtistDir(t, libDir, "Reconcile Failure", "fanart.jpg", "fanart1.jpg")
	svc, artistSvc, db := setupScannerWithDB(t, libDir)
	ctx := context.Background()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	bus := event.NewBus(logger, 64)
	go bus.Start()
	t.Cleanup(bus.Stop)

	var updates atomic.Int64
	bus.Subscribe(event.ArtistUpdated, func(event.Event) {
		updates.Add(1)
	})
	svc.SetEventBus(bus)

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	a, _ := artistSvc.GetByPath(ctx, artistDir)
	if a == nil {
		t.Fatal("artist not found after first scan")
	}
	if got := fanartSlots(t, artistSvc, a.ID); len(got) != 2 {
		t.Fatalf("precondition: want 2 fanart slots after the first scan, got %v", got)
	}

	// Remove a file so the rescan has a row to delete, then make that delete
	// fail. Both are required: without the removal the reconcile has nothing
	// to do and would succeed regardless of the trigger, so the test would
	// pass vacuously.
	if err := os.Remove(filepath.Join(artistDir, "fanart1.jpg")); err != nil {
		t.Fatalf("removing fanart1.jpg: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER block_image_delete BEFORE DELETE ON artist_images
		BEGIN SELECT RAISE(ABORT, 'injected reconcile failure'); END;`); err != nil {
		t.Fatalf("installing delete-blocking trigger: %v", err)
	}

	before := updates.Load()
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	finished := waitForScan(t, svc, 5*time.Second)
	if finished == nil {
		t.Fatal("waitForScan returned nil after Run 2")
	}

	// Precondition: the reconcile really did fail. If the trigger silently did
	// not fire, both assertions below would pass for the wrong reason.
	if got := fanartSlots(t, artistSvc, a.ID); len(got) != 2 {
		t.Fatalf("precondition: the delete-blocking trigger did not fire (fanart slots = %v); "+
			"this test proves nothing unless the reconcile genuinely failed", got)
	}

	if finished.UpdatedArtists != 1 {
		t.Errorf("UpdatedArtists = %d, want 1: Update() committed, so the artist WAS "+
			"updated and the scan summary must say so even though the reconcile failed",
			finished.UpdatedArtists)
	}

	deadline := time.Now().Add(2 * time.Second)
	for updates.Load() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if updates.Load() == before {
		t.Error("no ArtistUpdated event published: the artist row was committed, so " +
			"subscribers are now showing stale data with no event coming to correct it")
	}
}

// readDirListing returns the raw os.ReadDir entries, which is what the
// detection paths hand to discoverFanartFiles. Tests use it to hold a listing
// that was genuinely read while the directory was still readable.
func readDirListing(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	return entries
}

// makeUnreadable strips every permission bit from dir and skips the test if the
// process can still read it. Running as root (containers, some CI images)
// ignores the mode bits entirely, and a test that cannot deny the read would
// otherwise pass without ever exercising the failure it exists to pin.
func makeUnreadable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Skipf("cannot chmod %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("chmod 000 does not deny this process (running as root?); " +
			"the unreadable-directory failure cannot be reproduced here")
	}
}

// TestDiscoverFanartFiles_UnreadableDirectoryIsNotAZeroCount pins the #2635
// swallowed-error defect at its source.
//
// discoverFanartFiles used to call image.DiscoverFanart, which performed a
// SECOND os.ReadDir of a directory its caller had already read successfully.
// That second read is an independent chance to fail -- an SMB/NFS blip, fd
// exhaustion mid-scan, a permission change -- and its error was logged at Warn
// and swallowed into nil. nil is indistinguishable from "no fanart on disk", so
// the caller reported FanartCount=0 and imageEnumeration published
// {fanart, FoundSlots: 0}: a positive claim that zero fanart files exist, which
// licenses deleting every fanart row while the files sit untouched on disk.
//
// The fixture holds the listing from a read that SUCCEEDED and then makes the
// directory unreadable, which is exactly the interleaving production hits. The
// count must come from the listing actually in hand.
func TestDiscoverFanartFiles_UnreadableDirectoryIsNotAZeroCount(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o644); err != nil {
			t.Fatalf("writing %s fixture: %v", name, err)
		}
	}

	// The listing the caller already holds, read while the directory was fine.
	files := readDirListing(t, dir)
	if got := discoverFanartFiles(dir, files); len(got) != 3 {
		t.Fatalf("precondition: readable directory discovered %d fanart files, want 3 (%v); "+
			"nothing below would prove anything", len(got), got)
	}

	makeUnreadable(t, dir)

	got := discoverFanartFiles(dir, files)
	if len(got) != 3 {
		t.Errorf("unreadable directory discovered %d fanart files, want 3 (%v); "+
			"three files are on disk and the caller's own listing names all three, so "+
			"any smaller number is a count that was never measured -- and a count of 0 "+
			"is a positive claim of absence that authorizes deleting every fanart row",
			len(got), got)
	}
}

// TestReconcile_UnreadableDirectoryDoesNotDestroyFanartRows is the same defect
// carried to its consequence: stored registry rows.
//
// It runs the production chain from scanner.go's re-scan path --
// discoverFanartFiles -> detectedFiles -> imageEnumeration -> ReconcileImages --
// against a directory that became unreadable after the caller's own ReadDir
// succeeded. The assertion is on rows read back from the database, not on a
// counter, a return value, or the absence of an error, because the failure being
// guarded is code that reports success while destroying data.
func TestReconcile_UnreadableDirectoryDoesNotDestroyFanartRows(t *testing.T) {
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Unreadable", "fanart.jpg", "fanart1.jpg", "fanart2.jpg")
	svc, artistSvc, _ := setupScannerWithDB(t, libDir)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

	artistDir := filepath.Join(libDir, "Unreadable")
	a, _ := artistSvc.GetByPath(ctx, artistDir)
	if a == nil {
		t.Fatal("artist not found after the first scan")
	}
	if got := fanartSlots(t, artistSvc, a.ID); len(got) != 3 {
		t.Fatalf("precondition: want 3 stored fanart slots after the first scan, got %v; "+
			"nothing below would prove anything", got)
	}

	// The listing the scan's own ReadDir returned, captured while readable.
	files := readDirListing(t, artistDir)
	makeUnreadable(t, artistDir)

	// The re-scan's remaining chain, mirroring processExistingArtist.
	var detected detectedFiles
	if paths := discoverFanartFiles(artistDir, files); len(paths) > 0 {
		detected.FanartExists = true
		detected.FanartCount = len(paths)
	}
	// processExistingArtist overwrites the artist's image fields from `detected`
	// BEFORE reconciling, and that step is load-bearing here rather than
	// incidental: ReconcileImages derives its "incoming" set from these flat
	// fields, so leaving them at their pre-rescan values would keep every slot
	// named and the reconcile could not delete anything whatever the
	// enumeration said. Omitting it makes this test pass against the unfixed
	// code -- it must reproduce the state the scanner actually reconciles in.
	a.FanartExists = detected.FanartExists
	a.FanartCount = detected.FanartCount
	if _, err := artistSvc.ReconcileImages(ctx, a, imageEnumeration(detected)); err != nil {
		t.Fatalf("ReconcileImages: %v", err)
	}

	got := fanartSlots(t, artistSvc, a.ID)
	if len(got) != 3 {
		t.Errorf("stored fanart slots = %v, want all 3 to survive; the directory could "+
			"not be re-read, so the reconcile had no measured count and must destroy "+
			"nothing -- the three files are still on disk", got)
	}
}

// TestDiscoverFanartFiles_MatchesDiscoverFanart is the drift alarm for
// answering discovery from memory.
//
// fanartVariants reimplements image.DiscoverFanart's matching rules -- the
// extension allowlist, the base/numbered-suffix split, the ordinal ordering and
// the same-ordinal dedupe -- against a listing already in hand. Two copies of
// one algorithm drift, and a drift here is not cosmetic: the count feeds
// deleteStaleSlots' delete bound. This pins them against a shared fixture set
// so a change to either side fails loudly rather than silently widening or
// narrowing what the scanner believes is on disk.
func TestDiscoverFanartFiles_MatchesDiscoverFanart(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		files []string
	}{
		{"primary only", []string{"fanart.jpg"}},
		{"primary and numbered", []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"}},
		{"orphan numbered, no primary", []string{"fanart1.jpg", "fanart2.jpg"}},
		{"gap in numbering", []string{"fanart.jpg", "fanart3.jpg"}},
		{"png primary", []string{"fanart.png", "fanart1.png"}},
		{"jpeg extension", []string{"fanart.jpeg"}},
		{"mixed extensions at one ordinal", []string{"fanart.jpg", "fanart.png"}},
		{"backdrop convention", []string{"backdrop.jpg", "backdrop1.jpg"}},
		{"excluded extension", []string{"fanart.gif", "fanart.webp", "fanart.bmp"}},
		{"non-numeric suffix", []string{"fanart.jpg", "fanartx.jpg", "fanart-alt.jpg"}},
		{"zero suffix is not a variant", []string{"fanart.jpg", "fanart0.jpg"}},
		{"unrelated images", []string{"fanart.jpg", "folder.jpg", "logo.png", "banner.jpg"}},
		{"no fanart at all", []string{"folder.jpg", "artist.nfo"}},
		{"uppercase on disk", []string{"Fanart.JPG", "FANART1.jpg"}},
		// Two files differing ONLY in case. This is the case that motivated
		// taking raw entries instead of the lowercased filename map, which
		// collapses the pair into one. It cannot be constructed on a
		// case-insensitive filesystem (macOS APFS), so the fixture writer skips
		// it there -- but Stillwater ships in a Linux container where it is real.
		{"case collision at one ordinal", []string{"fanart.jpg", "Fanart1.jpg", "fanart1.jpg"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, name := range tc.files {
				path := filepath.Join(dir, name)
				// A fixture list holding two names that differ only in case
				// needs a case-sensitive filesystem; on APFS the second write
				// silently overwrites the first and the case would test nothing.
				if _, err := os.Stat(path); err == nil {
					t.Skipf("filesystem is case-insensitive: %q collides with an "+
						"earlier fixture, so this case cannot be constructed here", name)
				}
				if err := os.WriteFile(path, []byte("test"), 0o644); err != nil {
					t.Fatalf("writing %s fixture: %v", name, err)
				}
			}
			listing := readDirListing(t, dir)

			// The reference answer: whichever pattern the two-pass resolution
			// selects, resolved by the package this one mirrors.
			var want []string
			for _, p := range fanartPatterns {
				primaryPresent := false
				for _, e := range listing {
					if !e.IsDir() && strings.EqualFold(e.Name(), p) {
						primaryPresent = true
						break
					}
				}
				if primaryPresent {
					var err error
					if want, err = swimage.DiscoverFanart(dir, p); err != nil {
						t.Fatalf("DiscoverFanart(%s): %v", p, err)
					}
					break
				}
			}
			if len(want) == 0 {
				for _, p := range fanartPatterns {
					paths, err := swimage.DiscoverFanart(dir, p)
					if err != nil {
						t.Fatalf("DiscoverFanart(%s): %v", p, err)
					}
					if len(paths) > 0 {
						want = paths
						break
					}
				}
			}

			got := discoverFanartFiles(dir, listing)
			if len(got) != len(want) {
				t.Fatalf("discoverFanartFiles = %v (%d), DiscoverFanart = %v (%d): the two "+
					"matching implementations disagree, and this count is the bound "+
					"deleteStaleSlots deletes by", got, len(got), want, len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("ordinal %d: discoverFanartFiles = %s, DiscoverFanart = %s; "+
						"slot_index is a DiscoverFanart ordinal, so a different file at "+
						"an ordinal mismaps every stored row", i, got[i], want[i])
				}
			}
		})
	}
}
