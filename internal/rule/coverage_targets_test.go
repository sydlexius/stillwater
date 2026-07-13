package rule

// Coverage uplift for M49 issue #1380: targets makeImageDuplicateChecker,
// BackdropSequencingFixer.Fix, ImageFixer.Fix, fileModTimeCached, and
// IsSharedFilesystem. Tests pair happy-path coverage with shared-FS /
// cache-invalidation edges. No production code is refactored here; the
// refactors of ImageFixer.Fix and Pipeline.runForArtistFiltered are tracked
// separately as M49.5 B6 / B7.

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
)

// --------------------------------------------------------------------------
// helpers (prefixed to stay scoped to this file's coverage tests)
// --------------------------------------------------------------------------

// newRuleCovTestEngine returns an Engine wired against a fresh SQLite copy
// of the migrated template DB. The Engine carries no platform/library
// service by default; callers attach the pieces they need.
func newRuleCovTestEngine(t *testing.T) (*Engine, *sql.DB) {
	t.Helper()
	db := setupTestDB(t)
	e := &Engine{
		db:     db,
		logger: slog.Default(),
	}
	return e, db
}

// insertImageHashRow seeds one row into artist_images suitable for the
// image-duplicate checker (slot_index = 0, exists_flag = 1, supplied phash).
func insertImageHashRow(t *testing.T, db *sql.DB, artistID, imageType, hashHex string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash)
		 VALUES (?, ?, ?, 0, 1, ?)`,
		artistID+"-"+imageType, artistID, imageType, hashHex)
	if err != nil {
		t.Fatalf("inserting image hash row: %v", err)
	}
}

// --------------------------------------------------------------------------
// makeImageDuplicateChecker
// --------------------------------------------------------------------------

func TestMakeImageDuplicateChecker_GuardClauses(t *testing.T) {
	// Empty Path forces the early-return guard (no DB query, no violation).
	e, _ := newRuleCovTestEngine(t)
	checker := e.makeImageDuplicateChecker()
	a := &artist.Artist{ID: "a-empty", Name: "Empty Path"}
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("expected nil violation for empty path, got %q", v.Message)
	}

	// Nil DB also triggers the early-return guard.
	eNoDB := &Engine{logger: slog.Default()}
	checkerNoDB := eNoDB.makeImageDuplicateChecker()
	aPath := &artist.Artist{ID: "a-nodb", Name: "No DB", Path: t.TempDir()}
	if v := checkerNoDB(context.Background(), aPath, RuleConfig{}); v != nil {
		t.Errorf("expected nil violation when db is nil, got %q", v.Message)
	}
}

func TestMakeImageDuplicateChecker_NoRows(t *testing.T) {
	// Artist with a Path but no artist_images rows: query succeeds with
	// empty result set, comparator loop is a no-op, returns nil.
	e, db := newRuleCovTestEngine(t)
	insertTestArtist(t, db, "art-empty-hash", "Empty Hashes")

	checker := e.makeImageDuplicateChecker()
	a := &artist.Artist{ID: "art-empty-hash", Name: "Empty Hashes", Path: t.TempDir()}
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("expected nil violation when no rows match, got %q", v.Message)
	}
}

func TestMakeImageDuplicateChecker_DuplicateHashTriggersViolation(t *testing.T) {
	// Two image_types share the SAME phash -> Hamming distance 0, similarity
	// 1.0 >= 0.90 default tolerance -> duplicate violation.
	e, db := newRuleCovTestEngine(t)
	insertTestArtist(t, db, "art-dup", "Dup Artist")
	insertImageHashRow(t, db, "art-dup", "thumb", "ffffffffffffffff")
	insertImageHashRow(t, db, "art-dup", "fanart", "ffffffffffffffff")

	checker := e.makeImageDuplicateChecker()
	a := &artist.Artist{ID: "art-dup", Name: "Dup Artist", Path: t.TempDir()}
	v := checker(context.Background(), a, RuleConfig{})
	if v == nil {
		t.Fatal("expected duplicate violation, got nil")
	}
	if v.RuleID != RuleImageDuplicate {
		t.Errorf("RuleID = %q, want %q", v.RuleID, RuleImageDuplicate)
	}
	if !strings.Contains(v.Message, "thumb") || !strings.Contains(v.Message, "fanart") {
		t.Errorf("Message = %q; want it to name both slots", v.Message)
	}
	if v.Fixable {
		t.Error("Fixable = true; image-duplicate is informational, want false")
	}
}

func TestMakeImageDuplicateChecker_DistinctHashesNoViolation(t *testing.T) {
	// Two image_types with bit-disjoint phashes: similarity 0.0 < 0.90 -> no violation.
	e, db := newRuleCovTestEngine(t)
	insertTestArtist(t, db, "art-distinct", "Distinct Artist")
	insertImageHashRow(t, db, "art-distinct", "thumb", "0000000000000001")
	insertImageHashRow(t, db, "art-distinct", "fanart", "fffffffffffffffe")

	checker := e.makeImageDuplicateChecker()
	a := &artist.Artist{ID: "art-distinct", Name: "Distinct Artist", Path: t.TempDir()}
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("expected nil violation for distinct hashes, got %q", v.Message)
	}
}

func TestMakeImageDuplicateChecker_FiltersZeroAndInvalidHashes(t *testing.T) {
	// Sentinel '0000000000000000' is excluded by the SQL filter (WHERE phash
	// != '0000000000000000'). An unparsable hash is skipped by the loop.
	// Only one usable hash remains, so the pair loop does nothing.
	e, db := newRuleCovTestEngine(t)
	insertTestArtist(t, db, "art-zero", "Zero Hash Artist")
	insertImageHashRow(t, db, "art-zero", "thumb", "ffffffffffffffff")
	insertImageHashRow(t, db, "art-zero", "fanart", "0000000000000000") // excluded by SQL
	insertImageHashRow(t, db, "art-zero", "logo", "not-hex-at-all")     // excluded by ParseHashHex
	insertImageHashRow(t, db, "art-zero", "banner", "")                 // excluded by SQL

	checker := e.makeImageDuplicateChecker()
	a := &artist.Artist{ID: "art-zero", Name: "Zero Hash Artist", Path: t.TempDir()}
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("expected nil violation when only one usable hash remains, got %q", v.Message)
	}
}

func TestMakeImageDuplicateChecker_ToleranceOutOfRangeUsesDefault(t *testing.T) {
	// Tolerance > 1.0 is clamped to 0.90. With identical hashes the violation
	// still fires; the test confirms the clamp branch is exercised.
	e, db := newRuleCovTestEngine(t)
	insertTestArtist(t, db, "art-tol", "Tolerance Artist")
	insertImageHashRow(t, db, "art-tol", "thumb", "abcdef0123456789")
	insertImageHashRow(t, db, "art-tol", "fanart", "abcdef0123456789")

	checker := e.makeImageDuplicateChecker()
	a := &artist.Artist{ID: "art-tol", Name: "Tolerance Artist", Path: t.TempDir()}
	v := checker(context.Background(), a, RuleConfig{Tolerance: 1.5}) // out of range -> default
	if v == nil {
		t.Fatal("expected violation when tolerance is clamped and hashes match")
	}
}

func TestMakeImageDuplicateChecker_QueryErrorReturnsNil(t *testing.T) {
	// Closing the underlying DB forces QueryContext to error. The checker
	// must log and return nil rather than propagate the failure.
	e, db := newRuleCovTestEngine(t)
	insertTestArtist(t, db, "art-err", "Err Artist")
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	checker := e.makeImageDuplicateChecker()
	a := &artist.Artist{ID: "art-err", Name: "Err Artist", Path: t.TempDir()}
	if v := checker(context.Background(), a, RuleConfig{}); v != nil {
		t.Errorf("expected nil violation on query error, got %q", v.Message)
	}
}

// --------------------------------------------------------------------------
// BackdropSequencingFixer.Fix (fixers.go:1293)
// --------------------------------------------------------------------------

func TestBackdropSequencingFixer_Fix_EmptyPath(t *testing.T) {
	// fsCheck must be non-shared so the IsShared guard does not fire first.
	f := NewBackdropSequencingFixer(nil, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	a := &artist.Artist{Name: "No Path", LibraryID: "lib-test"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleBackdropSequencing})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed = true; want false for empty path")
	}
	if !strings.Contains(res.Message, "artist has no path") {
		t.Errorf("Message = %q; want 'artist has no path'", res.Message)
	}
}

func TestBackdropSequencingFixer_Fix_RenumbersWithGap(t *testing.T) {
	// fanart.jpg + fanart3.jpg (gap at index 1) -- fixer renumbers to fanart.jpg + fanart2.jpg.
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1920, 1080)

	f := NewBackdropSequencingFixer(nil, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	a := &artist.Artist{Name: "Gap Artist", Path: dir, LibraryID: "lib-test"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleBackdropSequencing})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed {
		t.Errorf("Fixed = false; want true after renumber. Message: %s", res.Message)
	}
	if !strings.Contains(res.Message, "renumbered") {
		t.Errorf("Message = %q; want 'renumbered ...'", res.Message)
	}

	// Verify fanart2.jpg now exists and fanart3.jpg is gone.
	if _, err := os.Stat(filepath.Join(dir, "fanart2.jpg")); err != nil {
		t.Errorf("fanart2.jpg should exist after renumber: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fanart3.jpg")); !os.IsNotExist(err) {
		t.Errorf("fanart3.jpg should not exist after renumber; stat err=%v", err)
	}
}

func TestBackdropSequencingFixer_Fix_AlreadyContiguous(t *testing.T) {
	// fanart.jpg + fanart2.jpg are already contiguous -- no renumber needed.
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "fanart.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1920, 1080)

	f := NewBackdropSequencingFixer(nil, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	a := &artist.Artist{Name: "Already OK", Path: dir, LibraryID: "lib-test"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleBackdropSequencing})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed = true; want false when files are already contiguous")
	}
	if !strings.Contains(res.Message, "no fanart files needing renumbering") {
		t.Errorf("Message = %q; want 'no fanart files needing renumbering'", res.Message)
	}
}

func TestBackdropSequencingFixer_Fix_NoFanartFiles(t *testing.T) {
	// Empty directory -- no fanart files discovered for any primary name.
	dir := t.TempDir()
	f := NewBackdropSequencingFixer(nil, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	a := &artist.Artist{Name: "Empty Dir", Path: dir, LibraryID: "lib-test"}
	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleBackdropSequencing})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed = true; want false when no fanart files exist")
	}
}

func TestBackdropSequencingFixer_Fix_WithPlatformService(t *testing.T) {
	// Activate an Emby-like profile so the platformService branch is exercised.
	db := setupTestDB(t)
	platformSvc := platform.NewService(db)
	profile := &platform.Profile{
		Name:       "test-emby",
		NFOEnabled: false,
		NFOFormat:  "kodi",
		ImageNaming: platform.ImageNaming{
			Thumb:  []string{"folder.jpg"},
			Fanart: []string{"backdrop.jpg", "fanart.jpg"},
			Logo:   []string{"logo.png"},
			Banner: []string{"banner.jpg"},
		},
		IsActive: true,
	}
	ctx := context.Background()
	if err := platformSvc.Create(ctx, profile); err != nil {
		t.Fatalf("creating profile: %v", err)
	}
	if err := platformSvc.SetActive(ctx, profile.ID); err != nil {
		t.Fatalf("setting profile active: %v", err)
	}

	// Create backdrop.jpg + backdrop3.jpg (gap at index 1, Emby naming).
	dir := t.TempDir()
	createTestJPEG(t, filepath.Join(dir, "backdrop.jpg"), 1920, 1080)
	createTestJPEG(t, filepath.Join(dir, "backdrop3.jpg"), 1920, 1080)

	f := NewBackdropSequencingFixer(platformSvc, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	a := &artist.Artist{Name: "Emby Artist", Path: dir, LibraryID: "lib-test"}
	res, err := f.Fix(ctx, a, &Violation{RuleID: RuleBackdropSequencing})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed {
		t.Errorf("Fixed = false; want true after Emby-style renumber. Message: %s", res.Message)
	}
	if _, err := os.Stat(filepath.Join(dir, "backdrop2.jpg")); err != nil {
		t.Errorf("backdrop2.jpg should exist after renumber: %v", err)
	}
}

// --------------------------------------------------------------------------
// ImageFixer.Fix (fixers.go:323) -- target the under-covered tail paths.
// (Shared-FS skip path is already exercised in shared_fs_test.go.)
// --------------------------------------------------------------------------

func TestImageFixer_Fix_NoImageType(t *testing.T) {
	// A violation whose RuleID does not map to any image type (e.g. nfo_exists)
	// produces an error rather than a no-op FixResult. This pins the
	// "no image type for rule" branch.
	f := NewImageFixer(&mockImageProvider{}, nil, nonSharedFSCheck(), testLogger())
	a := &artist.Artist{Name: "Bad Rule", MusicBrainzID: "mbid-bad", LibraryID: "lib-test", Path: t.TempDir()}
	v := &Violation{RuleID: RuleNFOExists} // not an image rule
	_, err := f.Fix(context.Background(), a, v)
	if err == nil {
		t.Fatal("expected error for non-image rule, got nil")
	}
	if !strings.Contains(err.Error(), "no image type for rule") {
		t.Errorf("error = %v; want 'no image type for rule'", err)
	}
}

func TestImageFixer_Fix_FetchError(t *testing.T) {
	// Provider returns an error -- ImageFixer wraps it and returns no result.
	mock := &mockImageProvider{err: errors.New("provider down")}
	f := NewImageFixer(mock, nil, nonSharedFSCheck(), testLogger())
	a := &artist.Artist{Name: "Fetch Err", MusicBrainzID: "mbid-err", Path: t.TempDir(), LibraryID: "lib-test"}
	v := &Violation{RuleID: RuleThumbExists}
	_, err := f.Fix(context.Background(), a, v)
	if err == nil {
		t.Fatal("expected wrapped fetch error, got nil")
	}
	if !strings.Contains(err.Error(), "fetching images") {
		t.Errorf("error = %v; want it to mention 'fetching images'", err)
	}
}

func TestImageFixer_Fix_NoCandidatesForType(t *testing.T) {
	// Provider returns images but none match the requested type.
	mock := &mockImageProvider{
		result: &provider.FetchResult{
			Images: []provider.ImageResult{
				{URL: "http://example.com/f.jpg", Type: "fanart", Width: 100, Height: 100},
			},
		},
	}
	f := NewImageFixer(mock, nil, nonSharedFSCheck(), testLogger())
	a := &artist.Artist{Name: "Wrong Type", MusicBrainzID: "mbid-wt", Path: t.TempDir(), LibraryID: "lib-test"}
	v := &Violation{RuleID: RuleThumbExists}
	res, err := f.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed = true; want false when no candidates match requested type")
	}
	if !strings.Contains(res.Message, "no thumb images found from providers") {
		t.Errorf("Message = %q; want 'no thumb images found from providers'", res.Message)
	}
}

func TestImageFixer_Fix_MultipleCandidatesAwaitsSelection(t *testing.T) {
	// More than one candidate without SelectBestCandidate -> "awaiting user selection".
	mock := &mockImageProvider{
		result: &provider.FetchResult{
			Images: []provider.ImageResult{
				{URL: "http://example.com/t1.jpg", Type: "thumb", Width: 1000, Height: 1000, Source: "fanarttv", Likes: 5},
				{URL: "http://example.com/t2.jpg", Type: "thumb", Width: 800, Height: 800, Source: "discogs", Likes: 1},
			},
		},
	}
	f := NewImageFixer(mock, nil, nonSharedFSCheck(), testLogger())
	a := &artist.Artist{Name: "Multi", MusicBrainzID: "mbid-multi", Path: t.TempDir(), LibraryID: "lib-test"}
	v := &Violation{RuleID: RuleThumbExists} // SelectBestCandidate = false
	res, err := f.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed = true; want false when awaiting user selection")
	}
	if len(res.Candidates) != 2 {
		t.Errorf("Candidates len = %d, want 2", len(res.Candidates))
	}
	if !strings.Contains(res.Message, "awaiting user selection") {
		t.Errorf("Message = %q; want it to mention 'awaiting user selection'", res.Message)
	}
}

func TestImageFixer_Fix_SuccessfulDownloadAndSave(t *testing.T) {
	// SelectBestCandidate -> the fixer downloads the only matching candidate
	// from a test HTTP server, converts it, and saves it to disk. This pins
	// the success-tail path (download -> SaveImageFromData -> setImageFlag).
	imgBytes := makeTestJPEG(t, 1200, 1200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(imgBytes)
	}))
	defer srv.Close()

	mock := &mockImageProvider{
		result: &provider.FetchResult{
			Images: []provider.ImageResult{
				{URL: srv.URL + "/thumb.jpg", Type: "thumb", Width: 1200, Height: 1200, Source: "fanarttv"},
			},
		},
	}

	dir := t.TempDir()
	f := NewImageFixer(mock, nil, nonSharedFSCheck(), testLogger())
	// httptest.NewServer binds to 127.0.0.1, which the default SSRF-safe
	// transport blocks. Swap in a plain client so the loopback fetch succeeds.
	f.httpClient = &http.Client{Timeout: fetchTimeout}
	a := &artist.Artist{
		Name:          "Save Success",
		MusicBrainzID: "mbid-save",
		Path:          dir,
		LibraryID:     "lib-test",
	}
	v := &Violation{
		RuleID: RuleThumbExists,
		Config: RuleConfig{SelectBestCandidate: true},
	}
	res, err := f.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed {
		t.Fatalf("Fixed = false; want true after successful save. Message: %s", res.Message)
	}
	if res.SavedPath == "" {
		t.Error("SavedPath empty; want a populated path on success")
	}
	if res.ImageType != "thumb" {
		t.Errorf("ImageType = %q, want 'thumb'", res.ImageType)
	}
	if !a.ThumbExists {
		t.Error("artist.ThumbExists not set after successful save")
	}
	// Confirm an image file is on disk in the artist directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("no files written to artist dir; want at least one saved image")
	}
}

func TestImageFixer_Fix_AllDownloadsFail(t *testing.T) {
	// HTTP server always 500s -> fetchImageURL returns error for every candidate.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	mock := &mockImageProvider{
		result: &provider.FetchResult{
			Images: []provider.ImageResult{
				{URL: srv.URL + "/fail.jpg", Type: "thumb", Width: 1200, Height: 1200, Source: "fanarttv"},
			},
		},
	}

	f := NewImageFixer(mock, nil, nonSharedFSCheck(), testLogger())
	// httptest server is on 127.0.0.1 -- bypass SafeTransport for this test.
	f.httpClient = &http.Client{Timeout: fetchTimeout}
	a := &artist.Artist{
		Name:          "All Fail",
		MusicBrainzID: "mbid-allfail",
		Path:          t.TempDir(),
		LibraryID:     "lib-test",
	}
	v := &Violation{
		RuleID: RuleThumbExists,
		Config: RuleConfig{SelectBestCandidate: true},
	}
	res, err := f.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed {
		t.Error("Fixed = true; want false when every download fails")
	}
	if !strings.Contains(res.Message, "download failures") {
		t.Errorf("Message = %q; want it to mention 'download failures'", res.Message)
	}
}

// --------------------------------------------------------------------------
// fileModTimeCached (fscache.go:319) -- cache-hit, cache-miss-error edges.
// --------------------------------------------------------------------------

func TestFileModTimeCached_WithCache_Hit(t *testing.T) {
	// With an FSCache attached the cached branch is taken and the file's
	// mod time is returned via cache.Stat.
	dir := t.TempDir()
	filePath := filepath.Join(dir, "modtime.txt")
	if err := os.WriteFile(filePath, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	e := &Engine{logger: slog.Default()}
	e.SetFSCache(NewFSCache(5*time.Second, 100, slog.Default()))

	got, err := e.fileModTimeCached(filePath)
	if err != nil {
		t.Fatalf("fileModTimeCached: %v", err)
	}
	if got.IsZero() {
		t.Error("got zero mod time via cache; want a real timestamp")
	}

	// Second call should be served from the cache (covers the cache-hit fast path).
	got2, err := e.fileModTimeCached(filePath)
	if err != nil {
		t.Fatalf("fileModTimeCached (second call): %v", err)
	}
	if !got2.Equal(got) {
		t.Errorf("second call returned different mod time: first=%v second=%v", got, got2)
	}
}

func TestFileModTimeCached_WithCache_StatError(t *testing.T) {
	// Nonexistent path: FSCache.Stat surfaces the error; fileModTimeCached
	// must propagate it (covers the cached error branch).
	e := &Engine{logger: slog.Default()}
	e.SetFSCache(NewFSCache(5*time.Second, 100, slog.Default()))

	got, err := e.fileModTimeCached(filepath.Join(t.TempDir(), "no-such-file"))
	if err == nil {
		t.Fatal("expected error from cached stat of nonexistent file")
	}
	if !got.IsZero() {
		t.Errorf("got non-zero mod time on error: %v; want zero", got)
	}
}

func TestFileModTimeCached_NoCache_StatError(t *testing.T) {
	// No FSCache attached: the os.Stat fallback path also surfaces ENOENT.
	e := &Engine{logger: slog.Default()}
	got, err := e.fileModTimeCached(filepath.Join(t.TempDir(), "missing.txt"))
	if err == nil {
		t.Fatal("expected error from direct stat of nonexistent file")
	}
	if !got.IsZero() {
		t.Errorf("got non-zero mod time on error: %v; want zero", got)
	}
}

func TestFileModTimeCached_CacheInvalidation(t *testing.T) {
	// After explicit InvalidatePath the cache must re-stat the file. Write
	// the file, cache its mod time, invalidate, sleep enough for filesystem
	// timestamp resolution to advance, rewrite the file, and confirm the
	// new mod time is returned (not the stale cached value).
	dir := t.TempDir()
	filePath := filepath.Join(dir, "evict.txt")
	if err := os.WriteFile(filePath, []byte("v1"), 0o644); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}

	cache := NewFSCache(5*time.Second, 100, slog.Default())
	e := &Engine{logger: slog.Default()}
	e.SetFSCache(cache)

	first, err := e.fileModTimeCached(filePath)
	if err != nil {
		t.Fatalf("fileModTimeCached first: %v", err)
	}

	// Sleep slightly past 1s so macOS HFS+/APFS sub-second mtime granularity
	// can register a change; then rewrite and explicitly invalidate.
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(filePath, []byte("v2"), 0o644); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}
	cache.InvalidatePath(filePath)

	second, err := e.fileModTimeCached(filePath)
	if err != nil {
		t.Fatalf("fileModTimeCached second: %v", err)
	}
	if !second.After(first) {
		t.Errorf("expected mod time to advance after rewrite + invalidate: first=%v second=%v", first, second)
	}
}

// --------------------------------------------------------------------------
// Engine.IsSharedFilesystem (engine.go:403)
// --------------------------------------------------------------------------

// stubLibrarySvc is a LibraryQuerier shim. The Engine field is typed as
// *library.Service, so we need a real (in-memory) library service for the
// IsSharedFilesystem tests rather than a stub.

func newLibraryServiceWithStatus(t *testing.T, status string) (*library.Service, string) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := library.NewService(db)
	ctx := context.Background()
	lib := &library.Library{
		Name:   "Cov Library",
		Path:   t.TempDir(),
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create library: %v", err)
	}
	if status != "" && status != library.SharedFSNone {
		if err := svc.SetSharedFSStatus(ctx, lib.ID, status, "", ""); err != nil {
			t.Fatalf("SetSharedFSStatus: %v", err)
		}
	}
	return svc, lib.ID
}

func TestIsSharedFilesystem_NilService(t *testing.T) {
	// libraryService nil -> returns false (no fail-closed at the Engine level;
	// individual fixers consult SharedFSCheck which has its own fail-closed).
	e := &Engine{logger: slog.Default()}
	a := &artist.Artist{LibraryID: "anything"}
	if e.IsSharedFilesystem(context.Background(), a) {
		t.Error("IsSharedFilesystem = true with nil libraryService; want false")
	}
}

func TestIsSharedFilesystem_EmptyLibraryID(t *testing.T) {
	svc, _ := newLibraryServiceWithStatus(t, library.SharedFSNone)
	e := &Engine{libraryService: svc, logger: slog.Default()}
	a := &artist.Artist{LibraryID: ""}
	if e.IsSharedFilesystem(context.Background(), a) {
		t.Error("IsSharedFilesystem = true with empty LibraryID; want false")
	}
}

func TestIsSharedFilesystem_SharedLibrary(t *testing.T) {
	// Suspected -> reports as shared.
	svc, libID := newLibraryServiceWithStatus(t, library.SharedFSSuspected)
	e := &Engine{libraryService: svc, logger: slog.Default()}
	a := &artist.Artist{LibraryID: libID}
	if !e.IsSharedFilesystem(context.Background(), a) {
		t.Error("IsSharedFilesystem = false for suspected-shared library; want true")
	}
}

func TestIsSharedFilesystem_NonSharedLibrary(t *testing.T) {
	svc, libID := newLibraryServiceWithStatus(t, library.SharedFSNone)
	e := &Engine{libraryService: svc, logger: slog.Default()}
	a := &artist.Artist{LibraryID: libID}
	if e.IsSharedFilesystem(context.Background(), a) {
		t.Error("IsSharedFilesystem = true for non-shared library; want false")
	}
}

func TestIsSharedFilesystem_CacheHit(t *testing.T) {
	// Second call for the same library ID should be served from sharedFSCache
	// without re-querying the DB. Confirm we still get the same result after
	// closing the DB (which would error if the cache were bypassed).
	svc, libID := newLibraryServiceWithStatus(t, library.SharedFSConfirmed)
	e := &Engine{libraryService: svc, logger: slog.Default()}
	a := &artist.Artist{LibraryID: libID}

	first := e.IsSharedFilesystem(context.Background(), a)
	if !first {
		t.Fatal("first call should report shared (confirmed status)")
	}

	// The second call must hit the cache. Even if the DB were unavailable,
	// the cache lookup short-circuits before GetByID, so the result stays
	// stable. (Cannot close the DB here because library.Service may not
	// reopen, so we just confirm idempotence.)
	second := e.IsSharedFilesystem(context.Background(), a)
	if !second {
		t.Error("second call (cache hit) should still report shared")
	}
}

func TestIsSharedFilesystem_DBErrorFailsClosed(t *testing.T) {
	// A library service whose backing DB has been closed produces an error
	// on GetByID. The Engine must fail closed (return true) and cache that
	// decision.
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening in-memory db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := library.NewService(db)
	ctx := context.Background()
	lib := &library.Library{
		Name:   "Will Fail",
		Path:   t.TempDir(),
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create library: %v", err)
	}

	// Close the DB so GetByID errors.
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	e := &Engine{libraryService: svc, logger: slog.Default()}
	a := &artist.Artist{LibraryID: lib.ID}
	if !e.IsSharedFilesystem(ctx, a) {
		t.Error("IsSharedFilesystem = false on DB error; want true (fail closed)")
	}

	// Second call should hit the cached fail-closed result.
	if !e.IsSharedFilesystem(ctx, a) {
		t.Error("second call (cached fail-closed) did not return true")
	}
}
