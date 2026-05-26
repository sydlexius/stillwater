package foreign

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestScanner_IsBaselined_FlagUnset pins the negative-case return of the
// exported IsBaselined probe: a fresh install with no row in settings
// returns (false, nil) so the OOBE wizard can render the pre-baseline
// summary panel.
func TestScanner_IsBaselined_FlagUnset(t *testing.T) {
	db := newTestDB(t)
	markBaselinePending(t, db)
	repo := NewRepository(db)
	scanner := NewScanner(repo, stubArtistLister{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	done, err := scanner.IsBaselined(context.Background())
	if err != nil {
		t.Fatalf("IsBaselined: %v", err)
	}
	if done {
		t.Error("IsBaselined returned true with the flag unset")
	}
}

// TestScanner_IsBaselined_FlagSet pins the positive-case return: once
// the first scan has completed, IsBaselined returns (true, nil) so the
// OOBE wizard knows it can advance past the baseline step.
func TestScanner_IsBaselined_FlagSet(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	// newTestDB pre-marks the baseline flag = "true"; assert the probe
	// reads it back without needing a scan run.
	scanner := NewScanner(repo, stubArtistLister{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	done, err := scanner.IsBaselined(context.Background())
	if err != nil {
		t.Fatalf("IsBaselined: %v", err)
	}
	if !done {
		t.Error("IsBaselined returned false with the flag pre-set to true")
	}
}

// TestScanner_IsBaselined_FlagNonTrue pins the "garbage value" case:
// any value other than the literal string "true" is treated as
// not-baselined. Without this guard a stray 'false' or '1' row would
// be misread as completion.
func TestScanner_IsBaselined_FlagNonTrue(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(
		`UPDATE settings SET value = 'false' WHERE key = 'foreign_files.baseline_completed'`,
	); err != nil {
		t.Fatalf("setting baseline flag to false: %v", err)
	}
	repo := NewRepository(db)
	scanner := NewScanner(repo, stubArtistLister{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	done, err := scanner.IsBaselined(context.Background())
	if err != nil {
		t.Fatalf("IsBaselined: %v", err)
	}
	if done {
		t.Error("IsBaselined returned true for a non-'true' value")
	}
}

// TestScanner_BaselineEmptyInstall pins the "no artists / no files"
// path: the baseline scan completes cleanly even when the install has
// no artist rows to walk. The baseline flag still flips so the next
// scan runs in alert mode rather than re-running the baseline path
// indefinitely.
func TestScanner_BaselineEmptyInstall(t *testing.T) {
	db := newTestDB(t)
	markBaselinePending(t, db)
	repo := NewRepository(db)

	scanner := NewScanner(repo, stubArtistLister{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	var done string
	if err := db.QueryRow(
		`SELECT value FROM settings WHERE key = 'foreign_files.baseline_completed'`).Scan(&done); err != nil {
		t.Fatalf("scanning baseline flag: %v", err)
	}
	if done != "true" {
		t.Errorf("baseline_completed: got %q, want true", done)
	}
	var count string
	if err := db.QueryRow(
		`SELECT value FROM settings WHERE key = 'foreign_files.baseline_count'`).Scan(&count); err != nil {
		t.Fatalf("scanning baseline count: %v", err)
	}
	if count != "0" {
		t.Errorf("baseline_count: got %q, want 0", count)
	}
}

// TestWriteBaselineDone_RewritesIdempotently pins that calling the
// internal writer twice converges on the latest count rather than
// failing on a UNIQUE conflict. The scheduler retries baselining on
// failure and this property keeps the retry safe.
func TestWriteBaselineDone_RewritesIdempotently(t *testing.T) {
	db := newTestDB(t)

	ctx := context.Background()
	if err := writeBaselineDone(ctx, db, 7); err != nil {
		t.Fatalf("first writeBaselineDone: %v", err)
	}
	if err := writeBaselineDone(ctx, db, 12); err != nil {
		t.Fatalf("second writeBaselineDone: %v", err)
	}

	var count string
	if err := db.QueryRow(
		`SELECT value FROM settings WHERE key = 'foreign_files.baseline_count'`).Scan(&count); err != nil {
		t.Fatalf("scanning baseline count: %v", err)
	}
	if count != "12" {
		t.Errorf("baseline_count after overwrite: got %q, want 12", count)
	}
}

// TestReadBaseline_DBError pins the DB-error wrap: a closed connection
// surfaces as a wrapped error, not (false, nil). Without this the
// scanner would silently treat any DB failure as "not baselined yet"
// and re-run the baseline path against an unreadable settings table.
func TestReadBaseline_DBError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Close immediately so the next QueryRowContext call fails.
	_ = db.Close()

	if _, err := readBaseline(context.Background(), db); err == nil {
		t.Error("expected error from readBaseline on a closed DB; got nil")
	}
}

// TestWriteBaselineDone_TxBeginFails pins the error wrap when the
// connection is unusable at transaction start. The function must surface
// the failure with a "baseline tx" wrap so the operator sees the
// transactional boundary in the chain, not just the inner driver error.
func TestWriteBaselineDone_TxBeginFails(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = db.Close()

	err = writeBaselineDone(context.Background(), db, 1)
	if err == nil {
		t.Fatal("expected error from writeBaselineDone with closed db; got nil")
	}
	if !strings.Contains(err.Error(), "baseline tx") {
		t.Errorf("expected wrap message containing %q; got %v", "baseline tx", err)
	}
}

// TestWriteBaselineDone_SecondExecFails pins the count-key error wrap.
// We set up a DB where the first exec (baseline_completed key) succeeds
// but the second exec (baseline_count key) fails. The simplest way to
// engineer this is dropping the table between the two writes via a
// hook, but that requires plumbing; instead we use a DB whose settings
// table only allows one specific key by trigger. That is excessive for
// a unit test, so we cover the wrap via the closed-DB path above and
// leave the second-exec branch as a low-value test gap.
func TestWriteBaselineDone_ZeroCount(t *testing.T) {
	db := newTestDB(t)

	if err := writeBaselineDone(context.Background(), db, 0); err != nil {
		t.Fatalf("writeBaselineDone with zero count: %v", err)
	}

	var v string
	if err := db.QueryRow(
		`SELECT value FROM settings WHERE key = 'foreign_files.baseline_count'`).Scan(&v); err != nil {
		t.Fatalf("scanning baseline count: %v", err)
	}
	if v != "0" {
		t.Errorf("baseline_count: got %q, want 0", v)
	}
}

// TestScanner_BaselineMultipleArtists exercises the baseline path with
// more than one artist directory so the per-artist baselined count
// rolls up correctly into the writeBaselineDone count argument.
func TestScanner_BaselineMultipleArtists(t *testing.T) {
	db := newTestDB(t)
	markBaselinePending(t, db)
	repo := NewRepository(db)

	dirA := t.TempDir()
	mustWrite(t, filepath.Join(dirA, "backdrop.jpg"), []byte("artist-a-bytes"))
	dirB := t.TempDir()
	mustWrite(t, filepath.Join(dirB, "fanart.jpg"), []byte("artist-b-bytes"))
	mustWrite(t, filepath.Join(dirB, "clearart.png"), []byte("artist-b-bytes-2"))

	if _, err := db.Exec(
		`INSERT INTO artists (id, name, path) VALUES (?, ?, ?), (?, ?, ?)`,
		"a1", "A", dirA, "a2", "B", dirB); err != nil {
		t.Fatalf("seed artists: %v", err)
	}
	listing := stubArtistLister{artists: []artist.Artist{
		{ID: "a1", Path: dirA},
		{ID: "a2", Path: dirB},
	}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	allow, err := repo.ListAllowlist(context.Background())
	if err != nil {
		t.Fatalf("ListAllowlist: %v", err)
	}
	if len(allow) != 3 {
		t.Errorf("expected 3 allowlist rows; got %d: %#v", len(allow), allow)
	}

	var count string
	if err := db.QueryRow(
		`SELECT value FROM settings WHERE key = 'foreign_files.baseline_count'`).Scan(&count); err != nil {
		t.Fatalf("scanning baseline count: %v", err)
	}
	if count != "3" {
		t.Errorf("baseline_count: got %q, want 3", count)
	}
}
