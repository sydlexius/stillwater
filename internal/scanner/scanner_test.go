package scanner

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
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
		if err.Error() != "scan already in progress" {
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
	ruleEng := rule.NewEngine(ruleSvc, db, logger)

	svc := NewService(artistSvc, ruleEng, ruleSvc, logger, libDir, nil)
	ctx := context.Background()

	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	waitForScan(t, svc, 5*time.Second)

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

	d := detectFiles(dir)

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

	d := detectFiles(dir)

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
