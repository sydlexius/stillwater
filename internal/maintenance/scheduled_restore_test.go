package maintenance

// scheduled_restore_test.go -- the hourly pass must heal as well as clear
// (#3456). Real SQLite, real files; the claim and cache are the real types.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// exclusiveClaim is a fake repair claim that, like the real one, is NOT
// re-entrant: a second claim while one is held is refused. That makes a pass
// that holds the claim through the detector refresh fail fast here instead of
// only in a slow end-to-end test.
type exclusiveClaim struct {
	held, grants, refusals int
	onGrant                func()
}

func (c *exclusiveClaim) claim() (func(), bool) {
	if c.held > 0 {
		c.refusals++
		return nil, false
	}
	c.held++
	c.grants++
	if c.onGrant != nil {
		c.onGrant()
	}
	return func() { c.held-- }, true
}

// captureLogs swaps the service logger for one writing to a buffer.
func captureLogs(svc *Service) *bytes.Buffer {
	buf := &bytes.Buffer{}
	svc.logger = slog.New(slog.NewTextHandler(buf, nil))
	return buf
}

// addFanartArtist seeds a second artist whose folder holds files, with `slots`
// cleared fanart rows. It is the readable twin that makes a test two-sided.
func addFanartArtist(t *testing.T, svc *Service, id string, files []string, slots int) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "twin")
	for _, f := range files {
		writeImage(t, filepath.Join(dir, f), 100, 100)
	}
	seedArtist(t, svc.db, id, dir)
	for s := 0; s < slots; s++ {
		seedImageRow(t, svc.db, id, "fanart", s, 0, 0)
	}
}

const twinID = "44444444-0000-0000-0000-000000000456"

var numberedOnly = []string{"backdrop2.jpg", "backdrop3.jpg", "backdrop4.jpg"}

func clearFanartFlags(t *testing.T, svc *Service, id string) {
	t.Helper()
	if _, err := svc.db.Exec(`UPDATE artist_images SET exists_flag = 0 WHERE artist_id = ?`, id); err != nil {
		t.Fatalf("clearing flags: %v", err)
	}
}

func fanartFlags(t *testing.T, svc *Service, id string, slots int) []int {
	t.Helper()
	out := make([]int, slots)
	for s := range out {
		out[s] = flagOf(t, svc, id, "fanart", s)
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Damage left behind (numbered-only folder, flags cleared) heals in ONE
// scheduled pass with no repair call, the detector then reports nothing to do,
// and the banner cache no longer holds the stale count.
func TestScheduledPass_HealsNumberedOnlyAndRefreshesBannerCache(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	id, _ := fanartFlagFixture(t, svc, numberedOnly, 3)
	clearFanartFlags(t, svc, id)
	ctx := context.Background()

	cache := &RegistryRepairCache{}
	ec := &exclusiveClaim{}
	svc.SetScheduledRestore(ec.claim, cache)
	// Seed the cache with the count the banner showed before the pass.
	gen, _ := cache.begin()
	cache.finish(gen, RegistryRepairPlan{Restore: 3}, nil)

	if got := fanartFlags(t, svc, id, 3); !equalInts(got, []int{0, 0, 0}) {
		t.Fatalf("precondition: flags = %v, want all cleared", got)
	}
	if plan, err := svc.scanRegistryRepair(ctx); err != nil || plan.Restore != 3 {
		t.Fatalf("precondition: detector = %+v, %v; want Restore 3", plan, err)
	}

	svc.runScheduledExistsFlagPass(ctx, "scan failed")

	if got := fanartFlags(t, svc, id, 3); !equalInts(got, []int{1, 1, 1}) {
		t.Fatalf("flags after the scheduled pass = %v, want all set", got)
	}
	if plan, err := svc.scanRegistryRepair(ctx); err != nil || plan != (RegistryRepairPlan{}) {
		t.Fatalf("detector after the pass = %+v, %v; want {0 0}", plan, err)
	}
	if n, _, ok := cache.Get(); !ok || n != 0 {
		t.Fatalf("banner cache = (%d, ok=%v), want a measured 0", n, ok)
	}
	// Restore and refresh each take the claim once; a refusal means the pass
	// held the claim through the refresh (the claim is not re-entrant).
	if ec.grants != 2 || ec.refusals != 0 {
		t.Fatalf("claim grants/refusals = %d/%d, want 2/0", ec.grants, ec.refusals)
	}
}

// An unreadable folder is unverifiable: nothing is restored for it, while a
// readable twin artist in the same pass IS restored (so the pass really ran).
func TestScheduledPass_UnreadableFolderRestoresNothing(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions are not enforced here")
	}
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	id, dir := fanartFlagFixture(t, svc, numberedOnly, 3)
	clearFanartFlags(t, svc, id)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Fatal("precondition: the folder must be unreadable")
	}
	addFanartArtist(t, svc, twinID, numberedOnly, 3)
	svc.SetScheduledRestore(noClaim, &RegistryRepairCache{})

	svc.runScheduledExistsFlagPass(context.Background(), "scan failed")

	if got := fanartFlags(t, svc, id, 3); !equalInts(got, []int{0, 0, 0}) {
		t.Fatalf("flags = %v, want all still cleared", got)
	}
	if got := fanartFlags(t, svc, twinID, 3); !equalInts(got, []int{1, 1, 1}) {
		t.Fatalf("readable twin flags = %v, want all restored", got)
	}
}

// A row cleared in a pass is not restored by it, and a restored row is not
// re-cleared: two passes on a mixed fixture change nothing the second time.
func TestScheduledPass_FixedPoint(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	// Three files on disk, four rows: slot 3 is a stale tail row. Slots 0..2
	// start cleared so the first pass has both a restore and a clear to do.
	id, _ := fanartFlagFixture(t, svc, numberedOnly, 4)
	for s := 0; s < 3; s++ {
		if _, err := db.Exec(`UPDATE artist_images SET exists_flag = 0 WHERE artist_id = ? AND slot_index = ?`, id, s); err != nil {
			t.Fatal(err)
		}
	}
	svc.SetScheduledRestore(noClaim, &RegistryRepairCache{})
	if got := fanartFlags(t, svc, id, 4); !equalInts(got, []int{0, 0, 0, 1}) {
		t.Fatalf("precondition: flags = %v", got)
	}

	want := []int{1, 1, 1, 0}
	for pass := 1; pass <= 2; pass++ {
		svc.runScheduledExistsFlagPass(context.Background(), "scan failed")
		if got := fanartFlags(t, svc, id, 4); !equalInts(got, want) {
			t.Fatalf("pass %d: flags = %v, want %v", pass, got, want)
		}
	}
}

// While the repair claim is refused the restore is skipped, but the clearing
// half still runs; once the claim is available the next tick restores.
func TestScheduledPass_ClaimHeldSkipsRestoreButStillClears(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	id, _ := fanartFlagFixture(t, svc, numberedOnly, 4)
	for s := 0; s < 3; s++ {
		if _, err := db.Exec(`UPDATE artist_images SET exists_flag = 0 WHERE artist_id = ? AND slot_index = ?`, id, s); err != nil {
			t.Fatal(err)
		}
	}
	held := true
	released := 0
	claim := func() (func(), bool) {
		if held {
			return nil, false
		}
		return func() { released++ }, true
	}
	svc.SetScheduledRestore(claim, &RegistryRepairCache{})

	svc.runScheduledExistsFlagPass(context.Background(), "scan failed")
	if got := fanartFlags(t, svc, id, 4); !equalInts(got, []int{0, 0, 0, 0}) {
		t.Fatalf("claim held: flags = %v, want the stale tail cleared and nothing restored", got)
	}

	held = false
	svc.runScheduledExistsFlagPass(context.Background(), "scan failed")
	if got := fanartFlags(t, svc, id, 4); !equalInts(got, []int{1, 1, 1, 0}) {
		t.Fatalf("claim free: flags = %v, want the valid slots restored", got)
	}
	// One claim for the restore, one for the post-restore detector refresh.
	if released != 2 {
		t.Fatalf("claim released %d times, want 2", released)
	}
}

// A canceled context ends the restore step without a write or a panic.
func TestScheduledPass_CanceledContextRestoresNothing(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	id, _ := fanartFlagFixture(t, svc, numberedOnly, 3)
	clearFanartFlags(t, svc, id)
	svc.SetScheduledRestore(noClaim, &RegistryRepairCache{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc.runScheduledExistsFlagPass(ctx, "scan failed")

	if got := fanartFlags(t, svc, id, 3); !equalInts(got, []int{0, 0, 0}) {
		t.Fatalf("flags = %v, want unchanged", got)
	}
	// Two-sided: the same fixture is restorable with a live context, so the
	// unchanged flags above came from the cancellation and nothing else.
	svc.runScheduledExistsFlagPass(context.Background(), "scan failed")
	if got := fanartFlags(t, svc, id, 3); !equalInts(got, []int{1, 1, 1}) {
		t.Fatalf("live pass flags = %v, want restored", got)
	}
}

// The scheduled pass restores ONLY fanart (#3456): the serve route probes the
// active profile's names for the other types, so restoring them here with the
// default names would fight its clears. A thumb sitting under a default name
// (artist.jpg) stays cleared in the scheduled pass but is restored by an
// unfiltered pass (the operator repair and detector), and a type filter limits it.
func TestRestoreExistsFlags_ImageTypeFilter(t *testing.T) {
	cases := []struct {
		name                  string
		run                   func(svc *Service, ctx context.Context) error
		wantThumb, wantFanart int
	}{
		{"scheduled pass is fanart only", func(svc *Service, ctx context.Context) error {
			svc.SetScheduledRestore(noClaim, &RegistryRepairCache{})
			svc.runScheduledExistsFlagPass(ctx, "scan failed")
			return nil
		}, 0, 1},
		{"unfiltered restores every type", func(svc *Service, ctx context.Context) error {
			_, err := svc.RestoreExistsFlags(ctx, ExistsFlagRestoreOpts{Commit: true})
			return err
		}, 1, 1},
		{"thumb filter skips fanart", func(svc *Service, ctx context.Context) error {
			_, err := svc.RestoreExistsFlags(ctx, ExistsFlagRestoreOpts{Commit: true, ImageTypes: []string{"thumb"}})
			return err
		}, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, dbPath := setupTestDBWithImages(t)
			svc := newRepairService(t, db, dbPath, "")
			id, dir := fanartFlagFixture(t, svc, numberedOnly, 1)
			clearFanartFlags(t, svc, id)
			writeImage(t, filepath.Join(dir, "artist.jpg"), 100, 100)
			seedImageRow(t, svc.db, id, "thumb", 0, 0, 0)
			if flagOf(t, svc, id, "thumb", 0) != 0 || flagOf(t, svc, id, "fanart", 0) != 0 {
				t.Fatal("precondition: thumb and fanart flags must start cleared")
			}
			if err := tc.run(svc, context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := flagOf(t, svc, id, "thumb", 0); got != tc.wantThumb {
				t.Errorf("thumb flag = %d, want %d", got, tc.wantThumb)
			}
			if got := flagOf(t, svc, id, "fanart", 0); got != tc.wantFanart {
				t.Errorf("fanart flag = %d, want %d", got, tc.wantFanart)
			}
		})
	}
}

// A restore that errors (here: the table is gone) is logged as a failure, the
// claim is released, and the detector refresh does not run.
func TestScheduledPass_RestoreErrorLoggedAndClaimReleased(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	logs := captureLogs(svc)
	ec := &exclusiveClaim{}
	cache := &RegistryRepairCache{}
	svc.SetScheduledRestore(ec.claim, cache)
	if _, err := db.Exec(`DROP TABLE artist_images`); err != nil {
		t.Fatal(err)
	}

	svc.restoreExistsFlagsScheduled(context.Background())

	if !strings.Contains(logs.String(), "scheduled exists_flag restore failed") {
		t.Fatalf("no failure log: %s", logs)
	}
	if ec.held != 0 || ec.grants != 1 {
		t.Fatalf("claim held/grants = %d/%d, want 0/1", ec.held, ec.grants)
	}
	if _, _, ok := cache.Get(); ok {
		t.Fatal("cache refreshed after a failed restore")
	}
}

// A shutdown mid-restore is a stop, not an Error-level failure.
func TestScheduledPass_CancelMidRestoreIsNotAnError(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	id, _ := fanartFlagFixture(t, svc, numberedOnly, 3)
	clearFanartFlags(t, svc, id)
	logs := captureLogs(svc)
	ctx, cancel := context.WithCancel(context.Background())
	ec := &exclusiveClaim{onGrant: cancel} // cancel after the claim, before the restore reads
	svc.SetScheduledRestore(ec.claim, &RegistryRepairCache{})

	svc.restoreExistsFlagsScheduled(ctx)

	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("cancellation logged as an error: %s", logs)
	}
	if !strings.Contains(logs.String(), "stopped: shutting down") {
		t.Fatalf("no stop line: %s", logs)
	}
}

// A restore past its deadline releases the claim and logs ONE warning, not an
// error; the next tick retries.
func TestScheduledPass_RestoreDeadlineReleasesClaim(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	id, _ := fanartFlagFixture(t, svc, numberedOnly, 3)
	clearFanartFlags(t, svc, id)
	logs := captureLogs(svc)
	ec := &exclusiveClaim{}
	svc.SetScheduledRestore(ec.claim, &RegistryRepairCache{})
	svc.restoreTimeout = time.Nanosecond

	svc.restoreExistsFlagsScheduled(context.Background())

	if ec.held != 0 {
		t.Fatal("claim still held after the restore timed out")
	}
	if strings.Contains(logs.String(), "level=ERROR") || strings.Count(logs.String(), "timed out") != 1 {
		t.Fatalf("want exactly one timeout warning and no error: %s", logs)
	}
	if got := fanartFlags(t, svc, id, 3); !equalInts(got, []int{0, 0, 0}) {
		t.Fatalf("flags = %v, want unchanged after a timed-out pass", got)
	}
}
