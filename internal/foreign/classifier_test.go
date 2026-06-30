// classifier_test.go pins the branch coverage gaps in processCandidate and
// classifyExisting introduced by the #1549 scanArtist refactor. Each test
// exercises one or more previously-uncovered branches of the two classifier
// helpers, using the same real-SQLite + temp-filesystem harness as the rest
// of the foreign package tests.
package foreign

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
)

// provenanceJPEG returns bytes for a minimal JPEG file with Stillwater EXIF
// provenance injected. img.ReadProvenance on a file written with these bytes
// returns a non-nil *img.ExifMeta (no error), so processCandidate classifies
// it as candidateKeep and classifyExisting returns rowClear (re-provenanced).
func provenanceJPEG(t *testing.T) []byte {
	t.Helper()
	src := image.NewRGBA(image.Rect(0, 0, 1, 1))
	src.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, nil); err != nil {
		t.Fatalf("encode JPEG: %v", err)
	}
	meta := &img.ExifMeta{Source: "fanarttv", Mode: "auto"}
	data, err := img.InjectMeta(buf.Bytes(), meta)
	if err != nil {
		t.Fatalf("InjectMeta: %v", err)
	}
	return data
}

// TestProcessCandidate_ProvenanceFileIsKept pins candidateKeep: a
// foreign-candidate filename ("backdrop.jpg") carrying Stillwater EXIF
// provenance must never be recorded in the alert ledger. Covers
// processCandidate line 438: "return candidateKeep // has Stillwater
// provenance; not foreign".
func TestProcessCandidate_ProvenanceFileIsKept(t *testing.T) {
	db := newTestDB(t) // baseline already marked done -> alert mode
	repo := NewRepository(db)
	dir := t.TempDir()

	mustWrite(t, filepath.Join(dir, "backdrop.jpg"), provenanceJPEG(t))

	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("file with Stillwater provenance must not enter the ledger; got %d entry(s)", len(got))
	}
}

// TestClassifyExisting_ReProvenancedRowIsCleared pins rowClear via
// re-provenanced: a pre-existing ledger row whose on-disk file has since
// gained Stillwater provenance must be removed by the reconcile pass.
// Covers classifyExisting lines 571-573: "return rowClear, clearing
// re-provenanced foreign-file row failed".
// Also exercises the empty-hash rehash SUCCESS path in classifyExisting
// (hash == "" -> hashFile succeeds -> continues to IsAllowlisted).
func TestClassifyExisting_ReProvenancedRowIsCleared(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	dir := t.TempDir()

	target := filepath.Join(dir, "backdrop.jpg")
	mustWrite(t, target, provenanceJPEG(t))

	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Pre-seed a ledger row with empty content_hash so the reconcile pass
	// exercises the hash-backfill path before reaching the provenance check.
	if _, err := db.Exec(`INSERT INTO foreign_files
		(id, artist_id, file_path, file_name, content_hash, size_bytes, detected_at)
		VALUES ('r1','a1',?,'backdrop.jpg',NULL,0,datetime('now'))`, target); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("re-provenanced row must be cleared by reconcile pass; got %d row(s)", len(got))
	}
}

// TestClassifyExisting_AllowlistedRowIsCleared pins rowClear via allowlist:
// a pre-existing ledger row whose file is still on disk but whose content
// hash has since been allowlisted must be removed by the reconcile pass.
// Covers classifyExisting lines 556-558: "return rowClear, clearing
// allowlisted foreign-file row failed".
func TestClassifyExisting_AllowlistedRowIsCleared(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	dir := t.TempDir()

	body := []byte("allowlisted-image-bytes")
	target := filepath.Join(dir, "fanart.jpg")
	mustWrite(t, target, body)
	hash := sha256Hex(body)

	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Seed the ledger row with the file's content hash so classifyExisting
	// skips the rehash branch and goes straight to IsAllowlisted.
	if _, err := db.Exec(`INSERT INTO foreign_files
		(id, artist_id, file_path, file_name, content_hash, size_bytes, detected_at)
		VALUES ('r1','a1',?,'fanart.jpg',?,0,datetime('now'))`, target, hash); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	// Allowlist the file so both the forward pass (candidateKeep) and the
	// reconcile pass (rowClear) hit the allowlist branch.
	if err := repo.AddAllowlist(context.Background(), AllowlistEntry{
		Scope: ScopeGlobal, FileName: "fanart.jpg", ContentHash: hash,
	}); err != nil {
		t.Fatalf("AddAllowlist: %v", err)
	}

	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("allowlisted row must be cleared by reconcile pass; got %d row(s)", len(got))
	}
}

// TestClassifyExisting_EmptyHashRehashFailIsRowSkip pins rowSkip via
// empty-hash + rehash failure: a pre-008 ledger row (content_hash NULL)
// whose on-disk file is unreadable must be left in place. Covers
// classifyExisting lines 535-541 (the herr != nil branch).
func TestClassifyExisting_EmptyHashRehashFailIsRowSkip(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0 cannot deny self-read")
	}
	db := newTestDB(t)
	repo := NewRepository(db)
	dir := t.TempDir()

	target := filepath.Join(dir, "backdrop.jpg")
	mustWrite(t, target, []byte("placeholder"))
	if err := os.Chmod(target, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o600) })

	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Seed a legacy row with empty content_hash so the reconcile pass tries
	// to rehash (hashFile fails because of the chmod) and returns rowSkip.
	if _, err := db.Exec(`INSERT INTO foreign_files
		(id, artist_id, file_path, file_name, content_hash, size_bytes, detected_at)
		VALUES ('r1','a1',?,'backdrop.jpg',NULL,0,datetime('now'))`, target); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("row must persist when rehash fails (rowSkip); got %d row(s)", len(got))
	}
}

// TestClassifyExisting_ProvenanceReadFailIsRowSkip pins rowSkip via
// provenance-read failure in the reconcile pass: when a file exists on disk
// and is not allowlisted, but ReadProvenance errors, the ledger row must be
// preserved. Covers classifyExisting lines 561-570.
func TestClassifyExisting_ProvenanceReadFailIsRowSkip(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0 cannot deny self-read")
	}
	db := newTestDB(t)
	repo := NewRepository(db)
	dir := t.TempDir()

	body := []byte("some-image-bytes")
	target := filepath.Join(dir, "poster.jpg")
	mustWrite(t, target, body)
	hash := sha256Hex(body)

	if err := os.Chmod(target, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o600) })

	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Seed the row with a non-empty hash NOT on the allowlist so the
	// reconcile pass skips the rehash, checks allowlist (false), then
	// tries ReadProvenance (fails on chmod 0) -> rowSkip.
	if _, err := db.Exec(`INSERT INTO foreign_files
		(id, artist_id, file_path, file_name, content_hash, size_bytes, detected_at)
		VALUES ('r1','a1',?,'poster.jpg',?,0,datetime('now'))`, target, hash); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("row must persist when provenance read fails in reconcile (rowSkip); got %d row(s)", len(got))
	}
}

// TestProcessCandidate_BaselineAllowlistFailIsSkipped pins candidateSkip in
// baseline mode when AddAllowlist fails with a non-unique-constraint error.
// A BEFORE INSERT trigger blocks inserts while leaving SELECT queries intact,
// so IsAllowlisted succeeds (returns false) but AddAllowlist errors, driving
// processCandidate to return candidateSkip. Covers lines 483-489.
func TestProcessCandidate_BaselineAllowlistFailIsSkipped(t *testing.T) {
	db := newTestDB(t)
	markBaselinePending(t, db)
	repo := NewRepository(db)
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "backdrop.jpg"), []byte("image-data"))

	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Block INSERT into the allowlist so AddAllowlist returns a non-nil,
	// non-unique-constraint error. SELECT (IsAllowlisted) is not affected.
	if _, err := db.Exec(`CREATE TRIGGER block_allowlist_insert
		BEFORE INSERT ON foreign_file_allowlist
		BEGIN SELECT RAISE(ABORT, 'test: insert blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Path: dir}}}
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// Scan must not propagate per-file errors; it logs and continues.
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// Baseline mode never writes to the alert ledger (foreign_files), so
	// the ledger must be empty regardless of the allowlist failure.
	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("baseline AllowlistFail: expected 0 ledger rows; got %d", len(rows))
	}
}

// TestProcessCandidate_UpsertFailIsSkipped pins candidateSkip when Upsert
// fails in non-baseline alert mode. Dropping the foreign_files table causes
// both Upsert (forward pass) and listForArtist (reconcile pass) to fail,
// covering processCandidate lines 501-507 and the scanArtist listForArtist
// error path. The scanner must not panic or return an error for per-file
// and per-artist failures -- they are logged and skipped.
func TestProcessCandidate_UpsertFailIsSkipped(t *testing.T) {
	db := newTestDB(t) // baseline already done -> alert mode
	repo := NewRepository(db)
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "fanart.jpg"), []byte("image-data"))

	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Drop foreign_files: Upsert fails (forward pass) and listForArtist
	// fails (reconcile pass). Scan must continue without error.
	if _, err := db.Exec(`DROP TABLE foreign_files`); err != nil {
		t.Fatalf("drop table: %v", err)
	}

	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Path: dir}}}
	var logBuf strings.Builder
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan must not surface per-file errors: %v", err)
	}
	if !strings.Contains(logBuf.String(), "upsert foreign-file entry") {
		t.Errorf("expected warn log %q; got: %s", "upsert foreign-file entry", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "listing existing entries for reconcile; skipping clear") {
		t.Errorf("expected warn log %q; got: %s", "listing existing entries for reconcile; skipping clear", logBuf.String())
	}
}

// TestScanArtist_ContextCancelBeforeFileLoop pins the ctx.Err() early-return
// inside scanArtist's per-file loop: a context canceled before scanArtist
// processes any file must cause the loop to return immediately without
// recording anything. Covers scanArtist line 373.
func TestScanArtist_ContextCancelBeforeFileLoop(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "backdrop.jpg"), []byte("data"))

	scanner := NewScanner(repo, stubArtistLister{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before scanArtist runs

	rec, clr, _, bl := scanner.scanArtist(ctx,
		artist.Artist{ID: "a1", Path: dir}, false)
	if rec != 0 || clr != 0 || bl != 0 {
		t.Errorf("ctx.Err() early return: got (rec=%d clr=%d bl=%d), want all 0",
			rec, clr, bl)
	}
}

// TestScanArtist_DeleteByPathErrorLogged pins the per-row delete-failure log
// path in the reconcile loop: when classifyExisting returns rowClear but
// DeleteByPath fails, the scanner must log the failure and continue rather
// than aborting. Covers scanArtist lines 406-411 (the derr != nil body).
//
// A BEFORE DELETE trigger blocks the delete while allowing listForArtist's
// SELECT to succeed, so the reconcile loop reaches DeleteByPath with a
// stale row and the error body fires.
func TestScanArtist_DeleteByPathErrorLogged(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	dir := t.TempDir()

	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES ('a1','x',?)`, dir); err != nil {
		t.Fatalf("insert artist: %v", err)
	}
	// Seed a stale row whose file is gone so classifyExisting returns
	// rowClear (missing-file branch).
	missingPath := filepath.Join(dir, "backdrop.jpg")
	if err := repo.Upsert(context.Background(), Entry{
		ArtistID: "a1", FilePath: missingPath, FileName: "backdrop.jpg",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Block DELETE so DeleteByPath fails; listForArtist (SELECT) still works.
	if _, err := db.Exec(`CREATE TRIGGER block_delete
		BEFORE DELETE ON foreign_files
		BEGIN SELECT RAISE(ABORT, 'test: delete blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	listing := stubArtistLister{artists: []artist.Artist{{ID: "a1", Path: dir}}}
	var logBuf strings.Builder
	scanner := NewScanner(repo, listing, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	// Scan must survive a DeleteByPath error -- the row is left in place
	// and the scan continues without returning an error.
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan must not surface reconcile-delete errors: %v", err)
	}
	if !strings.Contains(logBuf.String(), "clearing missing-file foreign-file row failed") {
		t.Errorf("expected warn log %q; got: %s", "clearing missing-file foreign-file row failed", logBuf.String())
	}
	// The trigger prevented the delete, so the row must still be present.
	if _, err := db.Exec(`DROP TRIGGER block_delete`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("row must remain when DeleteByPath fails; got %d row(s)", len(rows))
	}
}
