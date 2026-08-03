package maintenance

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	img "github.com/sydlexius/stillwater/internal/image"
)

// The guards in this file cover ONE defect class (#2934): a filesystem probe
// that became cancellable now fails on CANCELLATION as well as on a genuine I/O
// fault, through an error of the same shape. A caller written when only the I/O
// case existed records a per-file SKIP and carries on, which converts "the pass
// was abandoned" into "these files were unreadable" -- and the operator then
// reads a COMPLETED pass whose counts describe a library problem that does not
// exist.
//
// WHY A SWEEP RATHER THAN ONE CANCELED CONTEXT. Canceling before the call is
// not enough here: the very first filesystem operation short-circuits, so the
// function returns from its EARLIEST error branch and every later branch --
// including the ones this fix changes -- is never reached. The branch that
// matters is the one that observes a cancellation MID-PASS, after earlier work
// already succeeded.
//
// So instead of guessing when to cancel, these tests cancel at EVERY possible
// point: they count how many times a healthy pass consults the context, then
// re-run the pass once per consultation index, tripping the cancellation at
// exactly that one. Whichever index lands in the branch under test is covered
// automatically, and the sweep keeps covering it if the surrounding code is
// reordered or a new probe is added -- which a hand-picked cancellation point
// would not.

// cancelAt is a context that reports healthy until its Err has been consulted
// `at` times, then reports canceled forever after.
//
// Err() is the right trip point rather than Done(): the cancellable read
// primitive checks ctx.Err() as a cheap short-circuit BEFORE spending a
// goroutine, so counting Err consultations enumerates exactly the points at
// which a real cancellation could first be observed.
type cancelAt struct {
	context.Context
	at      int64
	seen    atomic.Int64
	tripped atomic.Bool
	once    sync.Once
	done    chan struct{}
}

func newCancelAt(at int64) *cancelAt {
	return &cancelAt{Context: context.Background(), at: at, done: make(chan struct{})}
}

func (c *cancelAt) Err() error {
	if c.seen.Add(1) >= c.at {
		c.trip()
	}
	if c.tripped.Load() {
		return context.Canceled
	}
	return nil
}

func (c *cancelAt) Done() <-chan struct{} { return c.done }

func (c *cancelAt) trip() {
	c.once.Do(func() {
		c.tripped.Store(true)
		close(c.done)
	})
}

// count returns how many consultations happened, which is the sweep's upper
// bound when run with `at` set past any reachable index.
func (c *cancelAt) count() int64 { return c.seen.Load() }

// TestDiscover_CancellationIsNeverRecordedAsASkip is the Family A guard for the
// image-registry repair's single-slot probe.
//
// THE OUTCOME UNDER TEST, stated as the operator sees it: a canceled pass must
// not come back as a completed pass carrying a skip count. FilesSkipped and the
// "skipped" Outcomes rows are what the repair report shows, and a
// stat-error-shaped cancellation used to land in both -- so an aborted run
// reported N unreadable images for a library whose files are all fine, which is
// a plausible-looking claim an operator would go and investigate.
//
// The fixture is entirely HEALTHY, which is what gives the assertion its teeth:
// there is nothing in this directory that could legitimately be skipped, so any
// non-zero FilesSkipped on an err==nil return is necessarily a cancellation
// wearing a skip's clothing.
func TestDiscover_CancellationIsNeverRecordedAsASkip(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	const id = "eeee5555-0000-0000-0000-000000000001"

	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "backdrop.jpg"), 640, 360)
	writeImage(t, filepath.Join(dir, "folder.jpg"), 320, 320)
	writeImage(t, filepath.Join(dir, "logo.png"), 200, 100)

	// Calibrate: a pass that is never canceled, to learn how many consultation
	// points exist. `at` is set past any reachable index so it cannot trip.
	probe := newCancelAt(1 << 40)
	if _, err := svc.discover(probe, quietLog(), id, dir, &ImageRepairResult{}); err != nil {
		t.Fatalf("calibration pass over a healthy directory: %v", err)
	}
	total := probe.count()
	if total == 0 {
		t.Fatal("the healthy pass consulted the context zero times; the sweep below would prove nothing")
	}

	for at := int64(1); at <= total; at++ {
		res := &ImageRepairResult{}
		_, err := svc.discover(newCancelAt(at), quietLog(), id, dir, res)
		if err != nil {
			// Aborted, which is the correct handling. Nothing may have been
			// credited as a skip on the way out.
			if res.FilesSkipped != 0 {
				t.Fatalf("cancel@%d: aborted with FilesSkipped = %d, want 0 -- "+
					"a cancellation must not also leave a skip on the report", at, res.FilesSkipped)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel@%d: err = %v, want context.Canceled", at, err)
			}
			continue
		}
		// Completed. On a directory with three perfectly good images that is
		// only honest if nothing was skipped.
		if res.FilesSkipped != 0 {
			t.Fatalf("cancel@%d: discover returned no error but reported FilesSkipped = %d "+
				"for a healthy directory -- an abandoned pass is being reported as a completed "+
				"pass with unreadable files (outcomes: %+v)", at, res.FilesSkipped, res.Outcomes)
		}
	}
}

// TestDiscover_OrdinaryStatErrorStillSkips is the REQUIRED green sibling: the
// fix must abort on a cancellation WITHOUT turning every stat failure into an
// abort. An ordinary probe error genuinely affects one image type and nothing
// else in the directory, so it must still be recorded as a skip and the pass
// must carry on -- a fix that propagated it would convert a single odd file
// into a failed library repair.
//
// A SELF-REFERENTIAL SYMLINK produces the error shape needed: stat follows it,
// loops, and returns ELOOP, which is a real stat failure and specifically NOT
// fs.ErrNotExist -- so it reaches the branch under test rather than being
// collapsed into a clean "absent" miss. The directory itself stays readable, so
// the fanart phase before it still succeeds and this is genuinely a mid-pass
// per-type failure.
func TestDiscover_OrdinaryStatErrorStillSkips(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")
	const id = "ffff6666-0000-0000-0000-000000000001"

	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "backdrop.jpg"), 640, 360)
	loop := filepath.Join(dir, "folder.jpg")
	if err := os.Symlink("folder.jpg", loop); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	// PRECONDITION: the symlink really does produce a non-ENOENT stat error.
	// Without this the test could pass vacuously against a platform that
	// resolves it to a plain "absent", never entering the branch at all.
	if _, statErr := os.Stat(loop); statErr == nil || errors.Is(statErr, os.ErrNotExist) {
		t.Skipf("self-referential symlink did not yield a non-ENOENT stat error here: %v", statErr)
	}

	res := &ImageRepairResult{}
	out, err := svc.discover(context.Background(), quietLog(), id, dir, res)
	if err != nil {
		t.Fatalf("discover = %v, want nil: an ordinary per-type stat error must skip, not abort the pass", err)
	}
	if res.FilesSkipped == 0 {
		t.Error("FilesSkipped = 0: the unreadable image type was neither skipped nor reported")
	}
	if !hasOutcome(res, id, "skipped", "stat_failed") {
		t.Errorf("no skipped/stat_failed outcome recorded; got %+v", res.Outcomes)
	}
	// The healthy fanart alongside it was still discovered -- proof the pass
	// carried on rather than unwinding.
	if len(out) == 0 {
		t.Error("no candidates discovered: one bad image type aborted the whole directory")
	}
}

// TestBackfillFanartHashes_CancellationIsNeverReportedAsACompletedPass is the
// Family A guard for the fanart hash backfill's two per-artist failure branches
// (directory discovery and file hashing).
//
// THE OUTCOME UNDER TEST: a canceled backfill must return an error rather than
// returning nil with starved rows still starved. Returning nil is what makes
// this dangerous rather than merely untidy -- the caller logs "pass complete",
// and the rows this pass abandoned are indistinguishable from rows it examined
// and legitimately could not fill. The hashing branch is the worse of the two:
// its skip category is the one the function's own doc comment describes as a
// permanent, re-selected residue of UNDECODABLE FILES, so a cancellation
// counted there makes a healthy library look like it holds corrupt artwork.
func TestBackfillFanartHashes_CancellationIsNeverReportedAsACompletedPass(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	seedFanartArtist(t, db, "artist-cancel-sweep", 2)

	// Calibrate against a pass that cannot trip. It also FILLS the rows, so the
	// sweep below re-starves them before each iteration.
	probe := newCancelAt(1 << 40)
	if err := svc.BackfillFanartHashes(probe, embyPrimary, 0); err != nil {
		t.Fatalf("calibration pass: %v", err)
	}
	total := probe.count()
	if total == 0 {
		t.Fatal("the healthy pass consulted the context zero times; the sweep below would prove nothing")
	}

	restarve := func() {
		t.Helper()
		if _, err := db.Exec(
			`UPDATE artist_images SET phash = '', content_hash = '' WHERE artist_id = 'artist-cancel-sweep'`); err != nil {
			t.Fatalf("re-starving rows: %v", err)
		}
	}

	for at := int64(1); at <= total; at++ {
		restarve()
		err := svc.BackfillFanartHashes(newCancelAt(at), embyPrimary, 0)
		if err != nil {
			continue // aborted: the honest outcome
		}
		// Reported complete. Then every starved row it selected must actually
		// have been filled; anything still empty was silently abandoned.
		for slot := range 2 {
			if ph, _ := fanartHashes(t, db, "artist-cancel-sweep", slot); ph == "" {
				t.Fatalf("cancel@%d: BackfillFanartHashes returned nil (pass complete) but slot %d is "+
					"still starved -- an abandoned pass is being reported as a completed one, and the "+
					"skipped row is indistinguishable from a genuinely undecodable file", at, slot)
			}
		}
	}
}

// TestBackfillFanartHashes_OrdinaryFailuresStillSkip is the REQUIRED green
// sibling for BOTH branches the fix touched, exercised in a single pass so the
// no-over-propagation property is proven for each:
//
//   - artist-green-gone: an unreadable directory -> the DISCOVERY branch.
//   - artist-green-corrupt slot 1: bytes that are not an image -> the HASHING
//     branch.
//
// Neither is a cancellation, so both must still be absorbed as per-artist /
// per-file skips, and the pass must return nil AND fill the healthy sibling
// slot. A fix that propagated every error from these branches would abort the
// whole backfill on one unmounted artist directory or one corrupt JPEG.
func TestBackfillFanartHashes_OrdinaryFailuresStillSkip(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	// Discovery-branch fixture: a row whose artist directory is not on disk.
	gone := filepath.Join(t.TempDir(), "unmounted")
	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('artist-green-gone', 'gone', ?)`, gone); err != nil {
		t.Fatalf("seeding vanished-dir artist: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
		 VALUES ('green-gone-0', 'artist-green-gone', 'fanart', 0, 1, '', '')`); err != nil {
		t.Fatalf("seeding vanished-dir row: %v", err)
	}

	// Hashing-branch fixture: slot 0 healthy, slot 1 undecodable.
	dir := seedFanartArtist(t, db, "artist-green-corrupt", 2)
	corrupt := filepath.Join(dir, img.FanartFilename(testPrimary, 1, false))
	if err := os.WriteFile(corrupt, []byte("not an image at all"), 0o644); err != nil {
		t.Fatalf("writing corrupt file: %v", err)
	}

	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 0); err != nil {
		t.Fatalf("BackfillFanartHashes = %v, want nil: ordinary per-artist and per-file failures "+
			"must be skipped, never propagated as a failed pass", err)
	}

	// The healthy slot was still filled -- proof the pass carried on past both
	// failures rather than unwinding at the first one.
	if ph, _ := fanartHashes(t, db, "artist-green-corrupt", 0); ph == "" {
		t.Error("slot 0 was never filled: an ordinary failure elsewhere aborted the pass")
	}
	// And neither failing row was given an invented hash.
	if ph, _ := fanartHashes(t, db, "artist-green-corrupt", 1); ph != "" {
		t.Errorf("the undecodable slot was given phash %q; a skip must record nothing", ph)
	}
	if ph, _ := fanartHashes(t, db, "artist-green-gone", 0); ph != "" {
		t.Errorf("the unreadable-directory slot was given phash %q; a skip must record nothing", ph)
	}
}
