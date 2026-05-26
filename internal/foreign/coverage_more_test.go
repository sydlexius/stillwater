package foreign

// coverage_more_test.go pins error paths that previously had no test
// coverage. The package-level coverage ratchet (testdata/coverage-floor.json
// at the repo root) requires internal/foreign to stay at or above 84%; the
// new scanner.go branches added by the v1.4 export/import work pulled the
// total down to 83% without these tests because their cancellation,
// hash-failure, and unique-constraint-classification arms were not
// exercised by the existing baseline tests.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestIsUniqueConstraintErr covers all three branches of the helper:
// nil error, matching message, and a non-matching error. Pinning the
// "unique constraint failed" substring at exactly this layer means a
// modernc.org/sqlite driver upgrade that changes the phrasing fails this
// test instead of silently downgrading the allowlist insert's
// idempotency (which would surface as duplicated allowlist rows in
// production).
func TestIsUniqueConstraintErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"matching lower", errors.New("UNIQUE constraint failed: foreign_files.id"), true},
		{"matching mixed case", errors.New("UNIQUE Constraint Failed: x.y"), true},
		{"unrelated", errors.New("disk I/O error"), false},
		{"contains word unique but not phrase", errors.New("the unique id is missing"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUniqueConstraintErr(tc.err); got != tc.want {
				t.Errorf("isUniqueConstraintErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestHashFile_OpenError exercises hashFile's open-fail branch. The
// scanner's per-file pipeline treats a hash failure as skip-don't-clear,
// so this branch must not crash the scan; covering it here keeps the
// per-file error wrapping in sync with that contract.
func TestHashFile_OpenError(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "does-not-exist.jpg")
	if _, err := hashFile(missing); err == nil {
		t.Fatal("hashFile on missing path: err = nil, want non-nil")
	}
}

// TestHashFile_Success confirms the success path returns a deterministic
// hex sha256. The exact value is the canonical sha256 of "abc"; keeping
// this assertion concrete makes a future stream-buffering refactor fail
// loudly rather than silently change the hashing contract.
func TestHashFile_Success(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "abc.bin")
	mustWrite(t, path, []byte("abc"))
	got, err := hashFile(path)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got != want {
		t.Errorf("hashFile(abc) = %q, want %q", got, want)
	}
}

// TestScanArtist_ReadDirError covers the ReadDir-failure branch. The
// scanner skip-don't-clears the ledger on a transient FS error so an
// unplugged NFS share or a removed library path cannot wipe an
// operator's foreign-file history (memory:
// feedback_proactive_cron_blast_radius). Without coverage on this branch,
// a future refactor could accidentally turn it into a clear-on-error
// path and the test suite would not notice.
func TestScanArtist_ReadDirError(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	markBaselinePending(t, db)
	repo := NewRepository(db)
	scanner := NewScanner(repo, stubArtistLister{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Pointing at a path that does not exist forces os.ReadDir to fail
	// inside scanArtist. The function must return (0,0,1,0) -- one
	// skipped artist -- not panic or zero the counters.
	gone := filepath.Join(t.TempDir(), "removed-library-root")
	rec, clr, sk, bl := scanner.scanArtist(context.Background(),
		artist.Artist{ID: "a-missing", Path: gone}, false /* runAsBaseline */)
	if rec != 0 || clr != 0 || sk != 1 || bl != 0 {
		t.Errorf("scanArtist on missing dir: got (rec=%d clr=%d sk=%d bl=%d), want (0 0 1 0)",
			rec, clr, sk, bl)
	}
}

// TestScanArtist_HashFailureIsSkipped checks the inner skip-don't-clear
// branch: when hashFile fails on a candidate (e.g. permissions, vanished
// file mid-scan), the file must be skipped and the artist scan must
// continue without aborting.
func TestScanArtist_HashFailureIsSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		// chmod 0o000 is a no-op for root; the only portable way to drive
		// the hash-open-failure branch on Unix is via filesystem perms, so
		// this test does not apply when the process runs as root (CI on
		// some containers, e.g.).
		t.Skip("running as root: chmod 0o000 cannot deny self-read")
	}
	t.Parallel()
	db := newTestDB(t)
	markBaselinePending(t, db)
	repo := NewRepository(db)
	scanner := NewScanner(repo, stubArtistLister{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// ReadDir only needs read perms on the directory itself, so the
	// candidate is created with mode 0 -- listed by ReadDir, but
	// hashFile's os.Open returns permission-denied. This drives the
	// hash-failure branch deterministically without races.
	dir := t.TempDir()
	candidate := filepath.Join(dir, "backdrop.jpg")
	mustWrite(t, candidate, []byte("placeholder"))
	if err := os.Chmod(candidate, 0); err != nil {
		t.Fatalf("chmod candidate: %v", err)
	}
	// Restore perms before t.TempDir cleanup so the harness can remove
	// the directory; cleanup itself is os-specific without this.
	t.Cleanup(func() { _ = os.Chmod(candidate, 0o600) })

	rec, _, sk, bl := scanner.scanArtist(context.Background(),
		artist.Artist{ID: "a1", Path: dir}, true /* runAsBaseline */)
	if rec != 0 || bl != 0 {
		t.Errorf("scanArtist with unreadable candidate: got (rec=%d bl=%d), want (0 0)", rec, bl)
	}
	if sk == 0 {
		t.Error("expected at least one skipped file when hash fails; got 0")
	}
}

// pagingArtistLister hands back `perPage` artists per call until the
// underlying slice is exhausted, mimicking the real artist service's
// paging contract: returns (page, total, err) where total is the full
// count across all pages. Used by TestScan_PaginationAdvancesPastFileSkips
// to exercise the multi-page loop without depending on production data.
type pagingArtistLister struct {
	artists []artist.Artist
	perPage int
}

func (p pagingArtistLister) List(_ context.Context, params artist.ListParams) ([]artist.Artist, int, error) {
	start := (params.Page - 1) * p.perPage
	if start >= len(p.artists) {
		return nil, len(p.artists), nil
	}
	end := start + p.perPage
	if end > len(p.artists) {
		end = len(p.artists)
	}
	return p.artists[start:end], len(p.artists), nil
}

// TestScan_PaginationAdvancesPastFileSkips pins the round-2 fix to
// scanner.go: pagination must be driven by per-artist progress
// (scanned + artistSkipped) and NOT include per-file skips returned by
// scanArtist. Before the fix, an artist on page 1 whose directory held
// multiple candidates that all hash-failed would push the loop's
// counter past the artist total and stop pagination before page 2 was
// ever requested. The test forces that exact shape: page 1 holds one
// artist with two files whose hash will fail (chmod 0), so the buggy
// counter went from "scanned=1, skipped=2" against a total of 2 and
// exited the loop -- never visiting the artist on page 2.
func TestScan_PaginationAdvancesPastFileSkips(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0 cannot deny self-read")
	}
	t.Parallel()
	db := newTestDB(t)
	markBaselinePending(t, db)
	repo := NewRepository(db)

	// Two artist directories: a1's files all fail to hash, a2 has a
	// readable candidate that must land in the allowlist if (and only
	// if) pagination reaches page 2.
	a1Dir := t.TempDir()
	mustWrite(t, filepath.Join(a1Dir, "backdrop.jpg"), []byte("h"))
	mustWrite(t, filepath.Join(a1Dir, "fanart.jpg"), []byte("h"))
	if err := os.Chmod(filepath.Join(a1Dir, "backdrop.jpg"), 0); err != nil {
		t.Fatalf("chmod a1 backdrop: %v", err)
	}
	if err := os.Chmod(filepath.Join(a1Dir, "fanart.jpg"), 0); err != nil {
		t.Fatalf("chmod a1 fanart: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(a1Dir, "backdrop.jpg"), 0o600)
		_ = os.Chmod(filepath.Join(a1Dir, "fanart.jpg"), 0o600)
	})
	a2Dir := t.TempDir()
	mustWrite(t, filepath.Join(a2Dir, "backdrop.jpg"), []byte("readable-content"))

	if _, err := db.Exec(
		`INSERT INTO artists (id, name, path) VALUES (?, ?, ?), (?, ?, ?)`,
		"a1", "Skipper", a1Dir,
		"a2", "Reader", a2Dir,
	); err != nil {
		t.Fatalf("insert artists: %v", err)
	}

	// perPage=1 forces a real second page round-trip; total=2 means the
	// loop condition must hold after the first page even though a1
	// returned sk=2 from the hash failures.
	lister := pagingArtistLister{
		artists: []artist.Artist{
			{ID: "a1", Path: a1Dir},
			{ID: "a2", Path: a2Dir},
		},
		perPage: 1,
	}
	scanner := NewScanner(repo, lister, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// a2's readable file should have been admitted to the allowlist
	// during the baseline scan. If pagination stopped after a1, the
	// allowlist will be empty.
	rows, err := repo.ListAllowlist(context.Background())
	if err != nil {
		t.Fatalf("ListAllowlist: %v", err)
	}
	var sawA2 bool
	for _, e := range rows {
		if e.ArtistID == "a2" || e.Scope == ScopeGlobal {
			sawA2 = true
			break
		}
	}
	if !sawA2 {
		t.Fatalf("a2 was never scanned -- pagination stopped early. allowlist rows: %d", len(rows))
	}
}

// TestScanArtist_NonCandidateFilesSkipped exercises the filter branch
// inside the onDisk loop: files whose extension is not a foreign
// candidate (e.g. .txt) must never reach the ledger or the allowlist,
// even on a baseline scan that admits everything else by default.
func TestScanArtist_NonCandidateFilesSkipped(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	markBaselinePending(t, db)
	repo := NewRepository(db)
	scanner := NewScanner(repo, stubArtistLister{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	dir := t.TempDir()
	// A subdirectory and an unrelated text file exercise the two
	// pre-filter branches (IsDir() and isForeignCandidate()). The .jpg
	// is the only file that should make it past the filter.
	if err := os.Mkdir(filepath.Join(dir, "ignored-subdir"), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	mustWrite(t, filepath.Join(dir, "notes.txt"), []byte("not an image"))
	mustWrite(t, filepath.Join(dir, "backdrop.jpg"), []byte("image-bytes"))

	rec, _, _, bl := scanner.scanArtist(context.Background(),
		artist.Artist{ID: "a1", Path: dir}, true /* runAsBaseline */)
	// Baseline mode admits the one .jpg to the allowlist. The
	// non-candidate entries must not appear in either bucket.
	if bl != 1 {
		t.Errorf("baselined count: got %d, want 1 (only backdrop.jpg)", bl)
	}
	if rec != 0 {
		t.Errorf("recorded count: got %d, want 0 (baseline scan admits, does not record)", rec)
	}
}
