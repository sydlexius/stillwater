package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// quietLog is an error-level logger for the direct-call tests below.
func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestApplyInserts_BeginTxFailureIsNonFatal covers the begin-transaction
// failure branch that is NOT a cancellation (#2670). A begin failure for any
// reason other than a canceled context means nothing could be written, so the
// pass must log it and carry on (confirm() then reports zero inserted) rather
// than aborting the whole library repair over one artist.
//
// The failure is provoked by closing the database before applyInserts runs --
// the same broken-DB style TestRepairReportsRegistryReadFailure uses. Asserts
// the OUTCOME: applyInserts returns nil (the pass is not aborted) and, read
// back through a fresh handle on the same file, no row was written.
func TestApplyInserts_BeginTxFailureIsNonFatal(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	const id = "aaaa1111-0000-0000-0000-000000000001"
	seedArtist(t, db, id, filepath.Join(t.TempDir(), "artist-aaaa1111"))
	svc := newRepairService(t, db, dbPath, "")

	// A second handle on the same file survives the close and lets us prove
	// nothing landed.
	verify, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening verify handle: %v", err)
	}
	t.Cleanup(func() { _ = verify.Close() })

	if err := db.Close(); err != nil {
		t.Fatalf("closing db to force BeginTx failure: %v", err)
	}

	inserts := []candidate{{key: slotKey{"fanart", 0}, fileName: "backdrop.jpg"}}
	if err := svc.applyInserts(context.Background(), quietLog(), id, inserts); err != nil {
		t.Fatalf("applyInserts = %v, want nil: a non-cancellation begin failure must not abort the pass", err)
	}

	if rowExists(t, verify, id, "fanart", 0) {
		t.Error("a row was written despite the begin failure; nothing should have landed")
	}
}

// TestConfirm_ReReadFailureLeavesInsertsUncredited covers confirm's post-write
// re-read failure branch (#2670). If the confirmation read fails, confirm
// cannot prove what landed, so it must credit NOTHING rather than assume the
// planned inserts succeeded -- the file's central "never report success while
// doing nothing" guarantee.
//
// The re-read is forced to fail by closing the database. Asserts the OUTCOME:
// RowsInserted stays 0.
func TestConfirm_ReReadFailureLeavesInsertsUncredited(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	const id = "bbbb2222-0000-0000-0000-000000000001"
	seedArtist(t, db, id, filepath.Join(t.TempDir(), "artist-bbbb2222"))
	svc := newRepairService(t, db, dbPath, "")

	if err := db.Close(); err != nil {
		t.Fatalf("closing db to force re-read failure: %v", err)
	}

	res := &ImageRepairResult{}
	inserts := []candidate{{key: slotKey{"fanart", 0}, fileName: "backdrop.jpg"}}
	svc.confirm(context.Background(), quietLog(), id, inserts, map[slotKey]bool{}, res)

	if res.RowsInserted != 0 {
		t.Errorf("RowsInserted = %d, want 0: a failed confirmation read must credit nothing", res.RowsInserted)
	}
}

// TestConfirm_SkipsKeysPresentBefore covers confirm's "already present before"
// skip (#2670). RowsInserted counts only keys that are present now AND were
// absent before, so a planned key that already existed in the pre-state must
// not be counted as a new insert -- otherwise a re-run would over-credit rows
// it did not create.
//
// Asserts the OUTCOME: with one planned key present before and one genuinely
// new, RowsInserted is exactly 1.
func TestConfirm_SkipsKeysPresentBefore(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	const id = "cccc3333-0000-0000-0000-000000000001"
	seedArtist(t, db, id, filepath.Join(t.TempDir(), "artist-cccc3333"))
	svc := newRepairService(t, db, dbPath, "")

	// Both rows exist NOW (the after-state), but only fanart/0 existed BEFORE.
	seedImageRow(t, db, id, "fanart", 0, 1, 0)
	seedImageRow(t, db, id, "thumb", 0, 1, 0)

	before := map[slotKey]bool{{"fanart", 0}: true}
	inserts := []candidate{
		{key: slotKey{"fanart", 0}, fileName: "backdrop.jpg"}, // present before -> must be skipped
		{key: slotKey{"thumb", 0}, fileName: "folder.jpg"},    // absent before, present now -> counted
	}
	res := &ImageRepairResult{}
	svc.confirm(context.Background(), quietLog(), id, inserts, before, res)

	if res.RowsInserted != 1 {
		t.Errorf("RowsInserted = %d, want 1: a planned key already present before must not be counted", res.RowsInserted)
	}
}

// TestDiscover_PropagatesCancellation covers the cancellation edges in discover
// and appendVerified (#2670). A context canceled before the file decode must
// propagate up as the context error -- NOT be recorded as a failed decode --
// so the pass aborts cleanly instead of mislabeling a real file as corrupt.
//
// Asserts the OUTCOME: discover returns context.Canceled and does not count the
// file as a skipped decode (FilesSkipped stays 0).
func TestDiscover_PropagatesCancellation(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := newRepairService(t, db, dbPath, "")

	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "backdrop.jpg"), 1920, 1080)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := &ImageRepairResult{}
	_, err := svc.discover(ctx, quietLog(), "dddd4444-0000-0000-0000-000000000001", dir, res)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("discover = %v, want context.Canceled propagated from the decode", err)
	}
	if res.FilesSkipped != 0 {
		t.Errorf("FilesSkipped = %d, want 0: a cancellation is not a failed decode", res.FilesSkipped)
	}
}
