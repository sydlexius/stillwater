package api

// handlers_image_fanart_fallback_test.go -- the request paths that clear a
// fanart slot-0 flag must ask the same question the maintenance passes ask
// (#3456): a folder holding only a numbered run (backdrop2.jpg..) is valid
// fanart, so neither the serve route nor the random-backdrop route may clear it
// or 404 on it.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/maintenance"
)

// seedFanartSlot0 creates an artist whose folder holds `files` (real JPEGs) and
// whose fanart slot 0 is flagged present. It asserts the folder has no primary
// file and the flag really reads 1, so a pass cannot be vacuous.
func seedFanartSlot0(t *testing.T, r *Router, svc *artist.Service, dir string, files []string) *artist.Artist {
	t.Helper()
	for _, f := range files {
		writeJPEG(t, filepath.Join(dir, f), 100, 56)
	}
	for _, primary := range []string{"backdrop.jpg", "fanart.jpg"} {
		if _, err := os.Stat(filepath.Join(dir, primary)); !os.IsNotExist(err) {
			t.Fatalf("precondition: folder must have no %s (stat err = %v)", primary, err)
		}
	}
	a := &artist.Artist{Name: "Fallback " + filepath.Base(t.TempDir()), SortName: "Fallback", Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if _, err := r.db.ExecContext(context.Background(),
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
		 VALUES (lower(hex(randomblob(16))), ?, 'fanart', 0, 1)
		 ON CONFLICT (artist_id, image_type, slot_index) DO UPDATE SET exists_flag = 1`, a.ID); err != nil {
		t.Fatalf("seeding artist_images: %v", err)
	}
	if !fanartSlot0Flag(t, r, a.ID) {
		t.Fatal("precondition: fanart slot 0 must read exists_flag=1")
	}
	return a
}

func fanartSlot0Flag(t *testing.T, r *Router, artistID string) bool {
	t.Helper()
	var flag int
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT exists_flag FROM artist_images WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = 0`,
		artistID).Scan(&flag); err != nil {
		t.Fatalf("reading fanart exists_flag: %v", err)
	}
	return flag == 1
}

func serveFile(r *Router, id, imageType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/artists/%s/images/%s/file", id, imageType), nil)
	req.SetPathValue("id", id)
	req.SetPathValue("type", imageType)
	w := httptest.NewRecorder()
	r.handleServeImage(w, req)
	return w
}

func randomBackdrop(r *Router) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.handleRandomBackdrop(w, httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil))
	return w
}

var numberedRun = []string{"backdrop2.jpg", "backdrop3.jpg", "backdrop4.jpg"}

func TestFanartFallback_RandomBackdropServesNumberedOnlyAndKeepsFlag(t *testing.T) {
	r, svc := testRouterWithPlatform(t)
	a := seedFanartSlot0(t, r, svc, t.TempDir(), numberedRun)

	w := randomBackdrop(r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a numbered-only fanart folder", w.Code)
	}
	if body, _ := io.ReadAll(w.Body); len(body) == 0 {
		t.Fatal("empty body, want image bytes")
	}
	if !fanartSlot0Flag(t, r, a.ID) {
		t.Fatal("slot 0 flag was cleared for a folder the maintenance passes consider valid")
	}
}

func TestFanartFallback_ServeServesNumberedOnlyAndCorroborationKeepsFlag(t *testing.T) {
	shortenCorroborationDelay(t, 50*time.Millisecond)
	r, svc := testRouterWithPlatform(t)
	a := seedFanartSlot0(t, r, svc, t.TempDir(), numberedRun)

	w := serveFile(r, a.ID, "fanart")
	if w.Code != http.StatusOK {
		t.Fatalf("serve status = %d, want 200 for a numbered-only fanart folder", w.Code)
	}

	// The corroborating second look must ask the same widened question: run it
	// directly (the handler spawns it as a goroutine) and wait past the delay.
	patterns := r.getActiveNamingConfig(context.Background(), "fanart")
	r.clearImageFlagAsync(context.Background(), a.ID, "fanart", a.Path, patterns)
	if !fanartSlot0Flag(t, r, a.ID) {
		t.Fatal("the corroboration pass cleared slot 0 on a primary-only miss")
	}
}

func TestFanartFallback_ReadableEmptyFolderStillClears(t *testing.T) {
	shortenCorroborationDelay(t, 20*time.Millisecond)
	r, svc := testRouterWithPlatform(t)
	// Seed with one file, then remove it: a readable folder with no fanart.
	dir := t.TempDir()
	a := seedFanartSlot0(t, r, svc, dir, []string{"backdrop2.jpg"})
	if err := os.Remove(filepath.Join(dir, "backdrop2.jpg")); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("precondition: folder must be readable and empty (%v, %v)", entries, err)
	}

	if w := randomBackdrop(r); w.Code != http.StatusNotFound {
		t.Fatalf("random backdrop status = %d, want 404", w.Code)
	}
	if fanartSlot0Flag(t, r, a.ID) {
		t.Fatal("random backdrop did not clear slot 0 of a readable folder with no fanart (check went blind)")
	}

	// Same for the serve path's corroborated clear.
	if _, err := r.db.ExecContext(context.Background(),
		`UPDATE artist_images SET exists_flag = 1 WHERE artist_id = ? AND image_type = 'fanart'`, a.ID); err != nil {
		t.Fatal(err)
	}
	patterns := r.getActiveNamingConfig(context.Background(), "fanart")
	r.clearImageFlagAsync(context.Background(), a.ID, "fanart", dir, patterns)
	if fanartSlot0Flag(t, r, a.ID) {
		t.Fatal("clearImageFlagAsync did not clear slot 0 of a readable folder with no fanart")
	}
}

func TestFanartFallback_MissingFolderPreservesFlag(t *testing.T) {
	shortenCorroborationDelay(t, 20*time.Millisecond)
	r, svc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := seedFanartSlot0(t, r, svc, dir, numberedRun)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	randomBackdrop(r)
	patterns := r.getActiveNamingConfig(context.Background(), "fanart")
	r.clearImageFlagAsync(context.Background(), a.ID, "fanart", dir, patterns)

	if !fanartSlot0Flag(t, r, a.ID) {
		t.Fatal("a missing folder is unverifiable and must not clear the flag")
	}
}

// A folder that can be stat-ed but not listed (mode 0100) gets past the primary
// probe and fails only in the fallback's listing. That is "could not look", so
// every entry point must leave the flag set; treating the error as a clean miss
// would clear a flag for a folder nobody was able to read.
func TestFanartFallback_UnlistableFolderPreservesFlag(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions are not enforced here")
	}
	shortenCorroborationDelay(t, 20*time.Millisecond)
	r, svc := testRouterWithPlatform(t)
	dir := t.TempDir()
	a := seedFanartSlot0(t, r, svc, dir, numberedRun)
	if err := os.Chmod(dir, 0o100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Fatal("precondition: the folder must not be listable")
	}
	patterns := r.getActiveNamingConfig(context.Background(), "fanart")

	// The serve handler spawns its clear in a goroutine, so this flag read cannot
	// observe it. Its branch is covered via the corroboration entry: the handler
	// only chooses the 404 and spawns that corroborated clear.
	entryPoints := map[string]func(){
		"random":        func() { randomBackdrop(r) },
		"corroboration": func() { r.clearImageFlagAsync(context.Background(), a.ID, "fanart", dir, patterns) },
	}
	for name, call := range entryPoints {
		call()
		if !fanartSlot0Flag(t, r, a.ID) {
			t.Fatalf("%s cleared slot 0 although the folder could not be listed", name)
		}
	}
}

// Thumbs keep the old single-probe behavior: a folder of fanart files is not a
// thumb, and the serve route still 404s for it.
func TestFanartFallback_ThumbUnchanged(t *testing.T) {
	r, svc := testRouterWithPlatform(t)
	dir := t.TempDir()
	writeJPEG(t, filepath.Join(dir, "backdrop2.jpg"), 100, 56)
	a := &artist.Artist{Name: "Thumb Unchanged", SortName: "Thumb Unchanged", Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if w := serveFile(r, a.ID, "thumb"); w.Code != http.StatusNotFound {
		t.Fatalf("thumb serve status = %d, want 404", w.Code)
	}
}

// After a scheduled pass heals the damage, calling both request routes must not
// bring the detector's count back (the measured sequence on a real library).
func TestFanartFallback_ScheduledHealThenRoutesLeaveDetectorAtZero(t *testing.T) {
	shortenCorroborationDelay(t, 20*time.Millisecond)
	r, svc := testRouterWithPlatform(t)
	a := seedFanartSlot0(t, r, svc, t.TempDir(), numberedRun)
	for s := 1; s <= 2; s++ {
		if _, err := r.db.Exec(`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
			VALUES (lower(hex(randomblob(16))), ?, 'fanart', ?, 1)`, a.ID, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.db.Exec(`UPDATE artist_images SET exists_flag = 0 WHERE artist_id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}

	maint := maintenance.NewService(r.db, "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	cache := &maintenance.RegistryRepairCache{}
	maint.SetScheduledRestore(r.TryClaimRegistryRepairCheck, cache)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		maint.StartExistsFlagScanner(ctx, time.Hour, 10*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := cache.Get(); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if n, _, ok := cache.Get(); !ok || n != 0 {
		t.Fatalf("banner cache after the scheduled pass = (%d, ok=%v), want measured 0", n, ok)
	}

	if w := randomBackdrop(r); w.Code != http.StatusOK {
		t.Fatalf("random backdrop = %d, want 200", w.Code)
	}
	if w := serveFile(r, a.ID, "fanart"); w.Code != http.StatusOK {
		t.Fatalf("serve = %d, want 200", w.Code)
	}
	patterns := r.getActiveNamingConfig(context.Background(), "fanart")
	r.clearImageFlagAsync(context.Background(), a.ID, "fanart", a.Path, patterns)

	res, err := maint.RestoreExistsFlags(context.Background(), maintenance.ExistsFlagRestoreOpts{})
	if err != nil || res.Restored != 0 {
		t.Fatalf("detector dry run after the routes = %+v, %v; want 0 to restore", res, err)
	}
	if !fanartSlot0Flag(t, r, a.ID) {
		t.Fatal("slot 0 flag was cleared by the request routes after the heal")
	}
}
