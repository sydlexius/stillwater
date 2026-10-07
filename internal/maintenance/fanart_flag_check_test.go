package maintenance

// fanart_flag_check_test.go -- the hourly flag check must judge a fanart slot
// by the same slot-aware rule the restore pass uses (#3456). Real SQLite, real
// files; no filesystem mocks.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/scanner"
)

// fanartFlagFixture seeds one artist whose folder holds `files` (real, decodable
// images) and one fanart row per slot in `slots`, all flagged present. It
// returns the artist id and folder.
func fanartFlagFixture(t *testing.T, svc *Service, files []string, slots int) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "artist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, f := range files {
		writeImage(t, filepath.Join(dir, f), 100, 100)
	}
	const id = "33333333-0000-0000-0000-000000000456"
	seedArtist(t, svc.db, id, dir)
	for s := 0; s < slots; s++ {
		seedImageRow(t, svc.db, id, "fanart", s, 1, 0)
	}
	return id, dir
}

func flagOf(t *testing.T, svc *Service, id, imageType string, slot int) int {
	t.Helper()
	f, _ := slotFlags(t, svc.db, id, imageType, slot)
	return f
}

// A numbered run with no primary file is on disk, so the hourly check must keep
// every slot. This is the defect: the primary-name probe cleared all three.
func TestScanExistsFlags_NumberedRunWithoutPrimaryKeepsFlags(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	id, dir := fanartFlagFixture(t, svc, []string{"backdrop2.jpg", "backdrop3.jpg", "backdrop4.jpg"}, 3)

	// Preconditions: the files exist, the primaries do not, the flags are set.
	for _, primary := range []string{"backdrop.jpg", "fanart.jpg"} {
		if _, err := os.Stat(filepath.Join(dir, primary)); !os.IsNotExist(err) {
			t.Fatalf("fixture must have no %s (stat err = %v)", primary, err)
		}
	}
	for s := 0; s < 3; s++ {
		if got := flagOf(t, svc, id, "fanart", s); got != 1 {
			t.Fatalf("precondition: slot %d flag = %d, want 1", s, got)
		}
	}

	if err := svc.ScanExistsFlags(context.Background()); err != nil {
		t.Fatalf("ScanExistsFlags: %v", err)
	}
	for s := 0; s < 3; s++ {
		if got := flagOf(t, svc, id, "fanart", s); got != 1 {
			t.Errorf("slot %d flag = %d, want 1: the file is on disk, only the primary is missing", s, got)
		}
	}
}

// The check must not become blind: a slot past the files on disk is still
// cleared, and the slots that do have a file are kept. Also covers a gap in the
// numbering (backdrop2, backdrop4 are ordinals 0 and 1).
func TestScanExistsFlags_SlotBeyondFilesStillCleared(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	id, _ := fanartFlagFixture(t, svc, []string{"backdrop2.jpg", "backdrop4.jpg"}, 4)

	if err := svc.ScanExistsFlags(context.Background()); err != nil {
		t.Fatalf("ScanExistsFlags: %v", err)
	}
	want := []int{1, 1, 0, 0}
	for s, w := range want {
		if got := flagOf(t, svc, id, "fanart", s); got != w {
			t.Errorf("slot %d flag = %d, want %d (2 files on disk)", s, got, w)
		}
	}
}

// A genuinely empty folder is a definitive absence: every fanart flag clears.
func TestScanExistsFlags_EmptyFolderClearsFanartFlags(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	id, dir := fanartFlagFixture(t, svc, nil, 2)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("precondition: folder must be readable and empty (%v, %d entries)", err, len(entries))
	}

	if err := svc.ScanExistsFlags(context.Background()); err != nil {
		t.Fatalf("ScanExistsFlags: %v", err)
	}
	for s := 0; s < 2; s++ {
		if got := flagOf(t, svc, id, "fanart", s); got != 0 {
			t.Errorf("slot %d flag = %d, want 0: nothing is on disk", s, got)
		}
	}
}

// An unreadable folder is "cannot tell", never "absent": nothing is cleared,
// even though the files really are inside it. Guards the direction inversion:
// the restore pass treats an error as "do not set", this pass must treat it as
// "do not clear".
func TestScanExistsFlags_UnreadableFolderClearsNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o000 semantics are Unix-specific")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	id, dir := fanartFlagFixture(t, svc, []string{"backdrop2.jpg", "backdrop3.jpg"}, 3)

	// Dropping the parent's permission bits makes ReadDir(dir) fail with EACCES.
	parent := filepath.Dir(dir)
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Fatal("precondition: the folder must be unreadable")
	}

	if err := svc.ScanExistsFlags(context.Background()); err != nil {
		t.Fatalf("ScanExistsFlags: %v", err)
	}
	for s := 0; s < 3; s++ {
		if got := flagOf(t, svc, id, "fanart", s); got != 1 {
			t.Errorf("slot %d flag = %d, want 1: an unreadable folder must clear nothing", s, got)
		}
	}
}

// thumb, logo and banner keep their old rule: judged by their own file names,
// so a folder of numbered fanart does not keep a missing thumb's flag alive,
// and a present thumb keeps its flag whatever fanart does.
func TestScanExistsFlags_OtherTypesUnchanged(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	id, _ := fanartFlagFixture(t, svc, []string{"backdrop2.jpg", "folder.jpg"}, 1)
	seedImageRow(t, db, id, "logo", 0, 1, 0)   // no logo file: cleared
	seedImageRow(t, db, id, "banner", 0, 1, 0) // no banner file: cleared
	seedImageRow(t, db, id, "thumb", 0, 1, 0)  // folder.jpg present: kept

	if err := svc.ScanExistsFlags(context.Background()); err != nil {
		t.Fatalf("ScanExistsFlags: %v", err)
	}
	for typ, want := range map[string]int{"thumb": 1, "logo": 0, "banner": 0, "fanart": 1} {
		if got := flagOf(t, svc, id, typ, 0); got != want {
			t.Errorf("%s flag = %d, want %d", typ, got, want)
		}
	}
}

// The checker lists a directory once for all of its fanart rows. Proved by
// removing the files after the first answer: a later slot is still answered
// from the first listing, so no second read happened.
func TestSlotChecker_ListsFanartDirectoryOnce(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"backdrop2.jpg", "backdrop3.jpg"} {
		writeImage(t, filepath.Join(dir, f), 10, 10)
	}
	c := newSlotChecker()
	ctx := context.Background()
	if ok, err := c.confirm(ctx, dir, "fanart", 0); err != nil || !ok {
		t.Fatalf("slot 0 = %v, %v; want true, nil", ok, err)
	}
	for _, f := range []string{"backdrop2.jpg", "backdrop3.jpg"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := c.confirm(ctx, dir, "fanart", 1); err != nil || !ok {
		t.Fatalf("slot 1 = %v, %v; want true, nil from the cached listing", ok, err)
	}
	// A fresh checker (the next pass) must see the real, now-empty folder.
	if ok, err := newSlotChecker().confirm(ctx, dir, "fanart", 1); err != nil || ok {
		t.Fatalf("fresh checker slot 1 = %v, %v; want false, nil", ok, err)
	}
}

// A missing directory is an ERROR (cannot tell), never a clean "absent".
func TestSlotChecker_MissingDirIsErrorNotAbsent(t *testing.T) {
	ok, err := newSlotChecker().confirm(context.Background(), filepath.Join(t.TempDir(), "gone"), "fanart", 0)
	if err == nil || ok {
		t.Fatalf("confirm on a missing dir = %v, %v; want false and an error", ok, err)
	}
}

// libraryScan runs the real library scanner over libDir and waits for it.
func libraryScan(t *testing.T, svc *scanner.Service) {
	t.Helper()
	if _, err := svc.Run(context.Background()); err != nil {
		t.Fatalf("scanner Run: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st := svc.Status(); st != nil && st.Status != "running" {
			if st.Status != "completed" {
				t.Fatalf("scan finished with status %q", st.Status)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("library scan did not finish")
}

// Fixed point (#3456): on a folder with a numbered fanart run and no primary,
// repair -> library scan -> hourly check leaves the detector at 0, and a second
// trip through the same three steps still does. Before the fix the hourly check
// cleared the flags the repair and the scan had just set.
func TestFanartFlagCycle_ReachesFixedPoint(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	libDir := t.TempDir()
	artistDir := filepath.Join(libDir, "Fixture Artist")
	for _, f := range []string{"backdrop2.jpg", "backdrop3.jpg", "backdrop4.jpg"} {
		writeImage(t, filepath.Join(artistDir, f), 100, 100)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	artistSvc := artist.NewService(db)
	scan := scanner.NewService(artistSvc, nil, nil, logger, libDir, nil)
	t.Cleanup(scan.Shutdown)
	ctx := context.Background()

	// The scan creates the artist and its three fanart rows. Then put the
	// registry in the damaged state the operator saw: every flag cleared.
	libraryScan(t, scan)
	a, err := artistSvc.GetByPath(ctx, artistDir)
	if err != nil || a == nil {
		t.Fatalf("artist not created by the scan: %v", err)
	}
	flags := func() [3]int {
		var out [3]int
		for s := range out {
			out[s] = flagOf(t, svc, a.ID, "fanart", s)
		}
		return out
	}
	if got := flags(); got != [3]int{1, 1, 1} {
		t.Fatalf("precondition: the scan must flag all three slots, got %v", got)
	}
	if _, err := db.ExecContext(ctx, `UPDATE artist_images SET exists_flag = 0 WHERE artist_id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.scanRegistryRepair(ctx)
	if err != nil || plan.Restore != 3 {
		t.Fatalf("precondition: the detector must plan to restore 3 flags, got %+v, %v", plan, err)
	}

	for pass := 1; pass <= 2; pass++ {
		if _, err := svc.RepairImageRegistry(ctx, ImageRepairOpts{Commit: true}); err != nil {
			t.Fatalf("pass %d repair rebuild: %v", pass, err)
		}
		if _, err := svc.RestoreExistsFlags(ctx, ExistsFlagRestoreOpts{Commit: true}); err != nil {
			t.Fatalf("pass %d repair restore: %v", pass, err)
		}
		if got := flags(); got != [3]int{1, 1, 1} {
			t.Fatalf("pass %d: flags after repair = %v, want all 1", pass, got)
		}
		libraryScan(t, scan)
		if err := svc.ScanExistsFlags(ctx); err != nil {
			t.Fatalf("pass %d hourly check: %v", pass, err)
		}
		if got := flags(); got != [3]int{1, 1, 1} {
			t.Errorf("pass %d: flags after the hourly check = %v, want all 1", pass, got)
		}
		plan, err := svc.scanRegistryRepair(ctx)
		if err != nil {
			t.Fatalf("pass %d detector: %v", pass, err)
		}
		if plan != (RegistryRepairPlan{}) {
			t.Errorf("pass %d: detector = %+v, want {0 0}", pass, plan)
		}
	}
}

// A cache hit must still observe cancellation: after the context is done, the
// next row of an already-listed directory returns the context error, never the
// remembered answer.
func TestSlotChecker_CacheHitObservesCancellation(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "backdrop2.jpg"), 10, 10)
	c := newSlotChecker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if ok, err := c.confirm(ctx, dir, "fanart", 0); err != nil || !ok {
		t.Fatalf("priming slot 0 = %v, %v; want true, nil", ok, err)
	}
	if len(c.fanart) != 1 {
		t.Fatalf("precondition: the listing must be cached, have %d entries", len(c.fanart))
	}
	cancel()
	if ok, err := c.confirm(ctx, dir, "fanart", 0); !errors.Is(err, context.Canceled) || ok {
		t.Fatalf("confirm after cancel = %v, %v; want false, context.Canceled", ok, err)
	}
}
