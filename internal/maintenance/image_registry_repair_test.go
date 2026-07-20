package maintenance

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const fixtureDir = "testdata/image_registry_repair"

// --- helpers ------------------------------------------------------------------

// writeImage synthesizes a decodable image; the extension picks the encoding so
// a .png slot really holds PNG bytes.
func writeImage(t *testing.T, path string, w, h int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	var err error
	if strings.EqualFold(filepath.Ext(path), ".png") {
		err = png.Encode(&buf, im)
	} else {
		err = jpeg.Encode(&buf, im, &jpeg.Options{Quality: 80})
	}
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// writeHeaderValidBodyCorrupt writes a JPEG whose header parses but whose pixel
// data is truncated mid-scan. This is the only construction that separates the
// two verification gates: measured against internal/image, GetDimensions
// returns nil error and reports the full 1920x1080 geometry here, while
// GeneratePlaceholder fails with a short-Huffman decode error. A zero-byte file
// or "SOI + garbage" fails BOTH gates, so a test built on those passes against
// a header-only check and proves nothing.
func writeHeaderValidBodyCorrupt(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1920, 1080)), &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encoding: %v", err)
	}
	b := buf.Bytes()
	if len(b) < 2000 {
		t.Fatalf("encoded JPEG unexpectedly short (%d bytes)", len(b))
	}
	if err := os.WriteFile(path, b[:2000], 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// readFixtureLines parses a pipe-delimited fixture file, dropping comments and
// requiring exactly `fields` columns.
func readFixtureLines(t *testing.T, name string, fields int) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	var out [][]string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "--") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != fields {
			t.Fatalf("%s: malformed line %q", name, line)
		}
		out = append(out, parts)
	}
	if len(out) == 0 {
		t.Fatalf("%s parsed to zero rows -- fixture not loaded", name)
	}
	return out
}

// applyFixture loads damaged.sql, repoints each artist path into libRoot, and
// synthesizes every file named in files.txt.
func applyFixture(t *testing.T, db *sql.DB, libRoot string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "damaged.sql"))
	if err != nil {
		t.Fatalf("reading damaged.sql: %v", err)
	}
	if _, err := db.Exec(string(raw)); err != nil {
		t.Fatalf("applying damaged.sql: %v", err)
	}
	// Read the ids through a closure so the cursor is closed before the
	// path rewrite below runs: modernc.org/sqlite is single-writer.
	ids := func() []string {
		rows, err := db.Query(`SELECT id FROM artists`)
		if err != nil {
			t.Fatalf("listing fixture artists: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scanning artist id: %v", err)
			}
			got = append(got, id)
		}
		return got
	}()
	if len(ids) != 4 {
		t.Fatalf("fixture should define 4 artists, got %d", len(ids))
	}
	for _, id := range ids {
		if _, err := db.Exec(`UPDATE artists SET path = ? WHERE id = ?`,
			filepath.Join(libRoot, "artist-"+id[:8]), id); err != nil {
			t.Fatalf("repointing artist path: %v", err)
		}
	}
	for _, f := range readFixtureLines(t, "files.txt", 4) {
		writeImage(t, filepath.Join(libRoot, "artist-"+f[0], f[1]), 1920, 1080)
	}
}

// slotStates returns "prefix|type|slot|exists_flag|locked" per row, sorted --
// the shape expected.sql is written in, so the headline assertion is set
// equality rather than a count.
func slotStates(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return queryStrings(t, db,
		`SELECT artist_id, image_type, slot_index, exists_flag, locked FROM artist_images`, true)
}

// fullRows returns every column of every row, sorted. Used where the assertion
// is byte-identical: comparing counts, or even (type, slot) sets, would pass
// against a repair that deleted rows and reinserted equivalents under fresh
// primary keys, so the id column has to be in the comparison.
func fullRows(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return queryStrings(t, db, `SELECT id, artist_id, image_type, slot_index, exists_flag,
		low_res, placeholder, width, height, phash, content_hash, file_format, source,
		last_written_at, locked FROM artist_images`, false)
}

// queryStrings renders each row as one string. truncIDs trims artist ids to
// their 8-char prefix so the result lines up with the redacted fixture.
func queryStrings(t *testing.T, db *sql.DB, query string, truncIDs bool) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			s := fmt.Sprintf("%v", v)
			if b, ok := v.([]byte); ok {
				s = string(b)
			}
			if truncIDs && cols[i] == "artist_id" && len(s) > 8 {
				s = s[:8]
			}
			parts[i] = s
		}
		out = append(out, strings.Join(parts, "|"))
	}
	sort.Strings(out)
	return out
}

func expectedStates(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, r := range readFixtureLines(t, "expected.sql", 5) {
		out = append(out, strings.Join(r, "|"))
	}
	sort.Strings(out)
	return out
}

// requireSameSet fails with the symmetric difference rather than a raw dump.
func requireSameSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") == strings.Join(want, "\n") {
		return
	}
	inGot, inWant := map[string]bool{}, map[string]bool{}
	for _, g := range got {
		inGot[g] = true
	}
	for _, w := range want {
		inWant[w] = true
	}
	var missing, extra []string
	for _, w := range want {
		if !inGot[w] {
			missing = append(missing, w)
		}
	}
	for _, g := range got {
		if !inWant[g] {
			extra = append(extra, g)
		}
	}
	t.Fatalf("%s\n  missing (%d): %v\n  unexpected (%d): %v", label, len(missing), missing, len(extra), extra)
}

func newRepairService(t *testing.T, db *sql.DB, dbPath, cacheDir string) *Service {
	t.Helper()
	return NewService(db, dbPath, cacheDir,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

func seedArtist(t *testing.T, db *sql.DB, id, path string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`,
		id, "artist-"+id[:8], path); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
}

func seedImageRow(t *testing.T, db *sql.DB, artistID, imageType string, slot, flag, locked int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO artist_images
		(id, artist_id, image_type, slot_index, exists_flag, locked) VALUES (?, ?, ?, ?, ?, ?)`,
		artistID[:8]+"-"+imageType+"-"+strconv.Itoa(slot), artistID, imageType, slot, flag, locked); err != nil {
		t.Fatalf("seeding image row: %v", err)
	}
}

func slotFlags(t *testing.T, db *sql.DB, artistID, imageType string, slot int) (flag, locked int) {
	t.Helper()
	if err := db.QueryRow(`SELECT exists_flag, locked FROM artist_images
		WHERE artist_id = ? AND image_type = ? AND slot_index = ?`,
		artistID, imageType, slot).Scan(&flag, &locked); err != nil {
		t.Fatalf("reading slot %s/%d: %v", imageType, slot, err)
	}
	return flag, locked
}

func rowExists(t *testing.T, db *sql.DB, artistID, imageType string, slot int) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM artist_images
		WHERE artist_id = ? AND image_type = ? AND slot_index = ?`,
		artistID, imageType, slot).Scan(&n); err != nil {
		t.Fatalf("counting slot: %v", err)
	}
	return n > 0
}

func hasOutcome(res *ImageRepairResult, artistID, action, reason string) bool {
	for _, o := range res.Outcomes {
		if o.ArtistID == artistID && o.Action == action && o.Reason == reason {
			return true
		}
	}
	return false
}

// planKeys renders the outcome set for one action as a sorted comparable string.
func planKeys(res *ImageRepairResult, action string) string {
	var keys []string
	for _, o := range res.Outcomes {
		if o.Action == action {
			keys = append(keys, fmt.Sprintf("%s|%s|%d|%s", o.ArtistID, o.ImageType, o.SlotIndex, o.FileName))
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// --- tests --------------------------------------------------------------------

// keyOf returns the "prefix|type|slot" identity of a "prefix|type|slot|..."
// state line.
func keyOf(line string) string {
	p := strings.Split(line, "|")
	return strings.Join(p[:3], "|")
}

// insertOnlyExpected builds the exact state an INSERT-ONLY repair must produce
// from this fixture. The full set of (type, slot) keys is expected.sql; a key
// that already existed in the damaged pre-state keeps its damaged row verbatim
// (untouched -- the additive guarantee), and a key that was missing appears as
// a freshly inserted row: exists_flag=1, locked=0.
//
// This is deliberately NOT expected.sql as written. expected.sql encodes the
// full-repair outcome (every flag raised to 1); insert-only leaves the 13
// surviving rows at exists_flag=0 and only the 16 rebuilt rows read 1. Asserting
// the insert-only shape proves both halves at once: the missing rows came back,
// and the surviving rows were not touched.
func insertOnlyExpected(t *testing.T, preState []string) []string {
	t.Helper()
	pre := map[string]string{}
	for _, line := range preState {
		pre[keyOf(line)] = line
	}
	var want []string
	for _, exp := range expectedStates(t) {
		k := keyOf(exp)
		if survived, ok := pre[k]; ok {
			want = append(want, survived)
		} else {
			want = append(want, k+"|1|0")
		}
	}
	sort.Strings(want)
	return want
}

// TestRepairEvidenceReplay is the headline test: apply the real post-incident
// state, put the real filenames on disk, run an insert-only repair, and require
// the resulting (artist, type, slot, exists_flag, locked) SET to equal the
// derived insert-only state exactly.
//
// Set equality is load-bearing -- a row-count assertion passes against a repair
// that writes nine rows at slot 0, or nine rows for the wrong artist. Asserting
// exists_flag is equally load-bearing in the other direction: the 16 rebuilt
// rows must read 1 (they were rebuilt from a decoded file) while the 13
// surviving rows must still read 0 (insert-only must not touch them). A test
// that asserted every flag == 1 would wrongly demand the flag-restore behavior
// that belongs to a later, opt-in pass.
func TestRepairEvidenceReplay(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	libRoot := t.TempDir()
	applyFixture(t, db, libRoot)

	preState := slotStates(t, db) // the 13 surviving damaged rows, all flag=0
	want := insertOnlyExpected(t, preState)
	wantInserts := len(want) - len(preState)

	res, err := newRepairService(t, db, dbPath, "").RepairImageRegistry(
		context.Background(), ImageRepairOpts{Commit: true})
	if err != nil {
		t.Fatalf("RepairImageRegistry: %v", err)
	}

	requireSameSet(t, "repaired registry does not match the insert-only expected state", slotStates(t, db), want)

	if res.ArtistsScanned != 4 || res.ArtistsFailed != 0 || res.ArtistsSkipped != 0 || res.ArtistsAbsent != 0 {
		t.Errorf("artists scanned/failed/skipped/absent = %d/%d/%d/%d, want 4/0/0/0",
			res.ArtistsScanned, res.ArtistsFailed, res.ArtistsSkipped, res.ArtistsAbsent)
	}
	if res.RowsPlanned != wantInserts || res.RowsInserted != wantInserts {
		t.Errorf("rows planned/inserted = %d/%d, want %d/%d",
			res.RowsPlanned, res.RowsInserted, wantInserts, wantInserts)
	}
	if res.DryRun {
		t.Error("DryRun should be false for a Commit run")
	}
}

// TestRepairDryRunChangesNothing covers both halves of the dry-run contract.
// The snapshot half catches a preview that writes; the plan-equality half
// catches a preview that short-circuits before discovery and returns an empty
// plan. Either half alone is passable by a broken implementation.
func TestRepairDryRunChangesNothing(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	libRoot := t.TempDir()
	applyFixture(t, db, libRoot)
	svc := newRepairService(t, db, dbPath, "")

	before := fullRows(t, db)
	dry, err := svc.RepairImageRegistry(context.Background(), ImageRepairOpts{})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got := fullRows(t, db); strings.Join(got, "\n") != strings.Join(before, "\n") {
		requireSameSet(t, "dry run mutated artist_images", got, before)
	}
	if !dry.DryRun {
		t.Error("DryRun should be true when Commit is unset")
	}
	if dry.RowsInserted != 0 {
		t.Errorf("dry run reported writes: inserted %d", dry.RowsInserted)
	}
	if dry.RowsPlanned == 0 {
		t.Fatal("dry run planned nothing; the preview is not the plan")
	}

	wet, err := svc.RepairImageRegistry(context.Background(), ImageRepairOpts{Commit: true})
	if err != nil {
		t.Fatalf("commit run: %v", err)
	}
	if got, want := planKeys(wet, "inserted"), planKeys(dry, "would_insert"); got != want {
		t.Errorf("commit plan differs from preview\n preview: %s\n  commit: %s", want, got)
	}
}

// TestRepairIsIdempotent asserts the second run is a no-op down to the id
// column. Comparing only (type, slot) would pass against a repair that deletes
// and reinserts with fresh UUIDs while reporting zero because it counts
// RowsAffected.
func TestRepairIsIdempotent(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	libRoot := t.TempDir()
	applyFixture(t, db, libRoot)
	svc := newRepairService(t, db, dbPath, "")
	opts := ImageRepairOpts{Commit: true}

	if _, err := svc.RepairImageRegistry(context.Background(), opts); err != nil {
		t.Fatalf("first run: %v", err)
	}
	afterFirst := fullRows(t, db)

	second, err := svc.RepairImageRegistry(context.Background(), opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.RowsPlanned != 0 || second.RowsInserted != 0 {
		t.Errorf("second run was not a no-op: rows planned/inserted %d/%d",
			second.RowsPlanned, second.RowsInserted)
	}
	if got := fullRows(t, db); strings.Join(got, "\n") != strings.Join(afterFirst, "\n") {
		requireSameSet(t, "second run changed rows", got, afterFirst)
	}
}

// TestRepairSkipsUnreadableDirectory is the invariant test. An unreadable
// directory must be reported as failed and leave the artist's rows exactly as
// they were. It must never be read as "no files here" -- the assertion that
// destroyed the registry in the first place.
func TestRepairSkipsUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	db, dbPath := setupTestDBWithImages(t)
	libRoot := t.TempDir()
	applyFixture(t, db, libRoot)

	var blindedID string
	if err := db.QueryRow(`SELECT id FROM artists ORDER BY id LIMIT 1`).Scan(&blindedID); err != nil {
		t.Fatalf("picking artist: %v", err)
	}
	blindedDir := filepath.Join(libRoot, "artist-"+blindedID[:8])
	if err := os.Chmod(blindedDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(blindedDir, 0o755) })

	before := rowsForArtist(t, db, blindedID)
	res, err := newRepairService(t, db, dbPath, "").RepairImageRegistry(
		context.Background(), ImageRepairOpts{Commit: true})
	if err != nil {
		t.Fatalf("RepairImageRegistry: %v", err)
	}

	if got := rowsForArtist(t, db, blindedID); strings.Join(got, "\n") != strings.Join(before, "\n") {
		requireSameSet(t, "unreadable artist's rows were modified", got, before)
	}
	if res.ArtistsFailed != 1 {
		t.Errorf("ArtistsFailed = %d, want 1", res.ArtistsFailed)
	}
	// The counts must be measured, not assumed: 3 artists were genuinely
	// scanned and 1 was not. Reporting 4 scanned is the dishonesty this
	// feature exists to avoid.
	if res.ArtistsScanned != 3 {
		t.Errorf("ArtistsScanned = %d, want 3 (the blinded artist must not count as clean)", res.ArtistsScanned)
	}
	if !hasOutcome(res, blindedID, "failed", "dir_unreadable") {
		t.Errorf("no failed/dir_unreadable outcome for the blinded artist; got %+v", res.Outcomes)
	}
	// One bad directory must not abort the pass.
	if res.RowsInserted == 0 {
		t.Error("no rows restored for the readable artists")
	}
}

func rowsForArtist(t *testing.T, db *sql.DB, artistID string) []string {
	t.Helper()
	var out []string
	for _, r := range fullRows(t, db) {
		if strings.Contains(r, artistID) {
			out = append(out, r)
		}
	}
	return out
}

// TestRepairDistinguishesAbsentFromUnreadable is the reason-code test. A
// directory that is definitively absent (ENOENT) and one that cannot be read
// (EACCES) are different facts, and conflating "cannot tell" with "nothing
// here" is the exact mistake that caused the incident. The repair must report
// them as two distinct outcomes, not one, and must count only the unreadable
// one as a failure.
func TestRepairDistinguishesAbsentFromUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	db, dbPath := setupTestDBWithImages(t)
	libRoot := t.TempDir()

	// Artist A: path points at a directory that does not exist -> ENOENT.
	const absentID = "aaaa0000-0000-0000-0000-000000000001"
	seedArtist(t, db, absentID, filepath.Join(libRoot, "does-not-exist"))

	// Artist B: path is a real directory made unreadable -> EACCES.
	const unreadableID = "bbbb0000-0000-0000-0000-000000000001"
	unreadableDir := filepath.Join(libRoot, "artist-unreadable")
	if err := os.MkdirAll(unreadableDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(unreadableDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadableDir, 0o755) })
	seedArtist(t, db, unreadableID, unreadableDir)

	// Artist C: a healthy, scannable directory, so ArtistsScanned > 0 and the
	// library-wide mount-down guard does not fire -- this test is about the
	// per-artist reason codes, not the whole-library outage.
	const presentID = "cccc0000-0000-0000-0000-000000000099"
	presentDir := filepath.Join(libRoot, "artist-present")
	seedArtist(t, db, presentID, presentDir)
	writeImage(t, filepath.Join(presentDir, "backdrop.jpg"), 1920, 1080)

	res, err := newRepairService(t, db, dbPath, "").RepairImageRegistry(
		context.Background(), ImageRepairOpts{Commit: true})
	if err != nil {
		t.Fatalf("RepairImageRegistry: %v", err)
	}

	// Absent: skipped/dir_absent, counted absent, NOT failed.
	if res.ArtistsAbsent != 1 {
		t.Errorf("ArtistsAbsent = %d, want 1", res.ArtistsAbsent)
	}
	if !hasOutcome(res, absentID, "skipped", "dir_absent") {
		t.Errorf("absent directory not reported as skipped/dir_absent; got %+v", res.Outcomes)
	}
	// Unreadable: failed/dir_unreadable, counted failed, NOT absent.
	if res.ArtistsFailed != 1 {
		t.Errorf("ArtistsFailed = %d, want 1", res.ArtistsFailed)
	}
	if !hasOutcome(res, unreadableID, "failed", "dir_unreadable") {
		t.Errorf("unreadable directory not reported as failed/dir_unreadable; got %+v", res.Outcomes)
	}
	// The absent artist must not be miscounted as an unreadable failure, and
	// vice versa -- that miscounting is the whole point of the split.
	if hasOutcome(res, absentID, "failed", "dir_unreadable") {
		t.Error("an absent directory was reported as a failure -- the conflation this test forbids")
	}
	if hasOutcome(res, unreadableID, "skipped", "dir_absent") {
		t.Error("an unreadable directory was reported as absent -- 'cannot tell' downgraded to 'nothing here'")
	}
	// Neither the absent nor the unreadable artist gained a row -- only the
	// present one was rebuilt.
	if rows := rowsForArtist(t, db, absentID); len(rows) != 0 {
		t.Errorf("absent artist gained rows: %v", rows)
	}
	if rows := rowsForArtist(t, db, unreadableID); len(rows) != 0 {
		t.Errorf("unreadable artist gained rows: %v", rows)
	}
}

// TestRepairNeverResurrectsDeletedFile guards the direction of authority:
// desired state comes from the files on disk, never from the registry or a
// cached count. An operator who deleted fanart files must not have rows
// recreated, and the surviving rows for those slots must not be touched.
func TestRepairNeverResurrectsDeletedFile(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	dir := filepath.Join(t.TempDir(), "artist-a")
	const id = "aaaaaaaa-0000-0000-0000-000000000001"
	seedArtist(t, db, id, dir)
	for slot := range 4 {
		seedImageRow(t, db, id, "fanart", slot, 1, 0)
	}
	// Only slots 0 and 1 still have files.
	writeImage(t, filepath.Join(dir, "backdrop.jpg"), 1920, 1080)
	writeImage(t, filepath.Join(dir, "backdrop2.jpg"), 1920, 1080)

	before := fullRows(t, db)
	res, err := newRepairService(t, db, dbPath, "").RepairImageRegistry(
		context.Background(), ImageRepairOpts{Commit: true})
	if err != nil {
		t.Fatalf("RepairImageRegistry: %v", err)
	}
	if got := fullRows(t, db); strings.Join(got, "\n") != strings.Join(before, "\n") {
		requireSameSet(t, "registry changed for an artist with deleted files", got, before)
	}
	if res.RowsPlanned != 0 {
		t.Errorf("RowsPlanned = %d, want 0", res.RowsPlanned)
	}
	for _, o := range res.Outcomes {
		if o.SlotIndex >= 2 && o.Action != "skipped" {
			t.Errorf("outcome for a slot with no file on disk: %+v", o)
		}
	}
}

// TestRepairPreservesExistingLockedRow asserts an insert-only pass never
// touches a row that already exists, even to "fix" a stale exists_flag, and
// that newly inserted rows are never locked. Only ON CONFLICT DO NOTHING
// survives this; an Upsert or a MergeAll with a SET list would rewrite the
// existing row.
func TestRepairPreservesExistingLockedRow(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	dir := filepath.Join(t.TempDir(), "artist-l")
	const id = "11111111-0000-0000-0000-000000000001"
	seedArtist(t, db, id, dir)
	// Slot 0 exists, is LOCKED, and carries a stale exists_flag=0. The file is
	// present, so a repair that "helpfully" corrected the flag would rewrite a
	// locked row -- exactly what an insert-only pass must not do.
	seedImageRow(t, db, id, "fanart", 0, 0, 1)
	for _, n := range []string{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg", "backdrop4.jpg"} {
		writeImage(t, filepath.Join(dir, n), 1920, 1080)
	}

	if _, err := newRepairService(t, db, dbPath, "").RepairImageRegistry(
		context.Background(), ImageRepairOpts{Commit: true}); err != nil {
		t.Fatalf("RepairImageRegistry: %v", err)
	}

	// The existing locked row is untouched: stale flag stays stale, lock stays.
	if flag, locked := slotFlags(t, db, id, "fanart", 0); flag != 0 || locked != 1 {
		t.Errorf("existing locked slot 0 = flag %d, locked %d; want 0, 1 (insert-only must not touch it)", flag, locked)
	}
	// The rebuilt rows exist, read present, and are never locked.
	for slot := 1; slot <= 3; slot++ {
		if flag, locked := slotFlags(t, db, id, "fanart", slot); flag != 1 || locked != 0 {
			t.Errorf("inserted slot %d = flag %d, locked %d; want 1, 0", slot, flag, locked)
		}
	}
}

// TestRepairSkipsCorruptFile proves verification is a full pixel decode. The
// corrupt file's header parses and reports plausible 1920x1080 geometry, so a
// repair verifying with GetDimensions writes a correct-looking row for an
// undisplayable file and this test fails.
func TestRepairSkipsCorruptFile(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	dir := filepath.Join(t.TempDir(), "artist-c")
	const id = "22222222-0000-0000-0000-000000000001"
	seedArtist(t, db, id, dir)
	writeImage(t, filepath.Join(dir, "backdrop.jpg"), 1920, 1080)
	writeHeaderValidBodyCorrupt(t, filepath.Join(dir, "backdrop2.jpg"))
	writeImage(t, filepath.Join(dir, "backdrop3.jpg"), 1920, 1080)

	res, err := newRepairService(t, db, dbPath, "").RepairImageRegistry(
		context.Background(), ImageRepairOpts{Commit: true})
	if err != nil {
		t.Fatalf("RepairImageRegistry: %v", err)
	}

	if rowExists(t, db, id, "fanart", 1) {
		t.Error("a row was written for a file that cannot be decoded")
	}
	// One bad file must not abort the artist.
	if !rowExists(t, db, id, "fanart", 0) || !rowExists(t, db, id, "fanart", 2) {
		t.Error("the valid files either side of the corrupt one were not restored")
	}
	if res.FilesSkipped != 1 {
		t.Errorf("FilesSkipped = %d, want 1", res.FilesSkipped)
	}
	found := false
	for _, o := range res.Outcomes {
		if o.Action == "skipped" && o.Reason == "decode_failed" && o.FileName == "backdrop2.jpg" {
			found = true
		}
	}
	if !found {
		t.Errorf("the corrupt file's skip was not reported by name; got %+v", res.Outcomes)
	}
}

// TestRepairPathlessArtistUsesCacheDir covers the population a library scan
// cannot reach: artists with no filesystem path, whose images live under
// <imageCacheDir>/<artistID>.
func TestRepairPathlessArtistUsesCacheDir(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	cacheDir := t.TempDir()
	const id = "33333333-0000-0000-0000-000000000001"
	seedArtist(t, db, id, "")
	writeImage(t, filepath.Join(cacheDir, id, "backdrop.jpg"), 1920, 1080)
	writeImage(t, filepath.Join(cacheDir, id, "folder.jpg"), 1000, 1000)

	res, err := newRepairService(t, db, dbPath, cacheDir).RepairImageRegistry(
		context.Background(), ImageRepairOpts{Commit: true})
	if err != nil {
		t.Fatalf("RepairImageRegistry: %v", err)
	}
	if !rowExists(t, db, id, "fanart", 0) || !rowExists(t, db, id, "thumb", 0) {
		t.Error("pathless artist's rows were not restored from the cache dir")
	}
	if res.RowsInserted != 2 {
		t.Errorf("RowsInserted = %d, want 2", res.RowsInserted)
	}

	// With no cache dir configured the directory is unresolvable, which must
	// be a reported skip rather than a silent clean result.
	db2, dbPath2 := setupTestDBWithImages(t)
	seedArtist(t, db2, id, "")
	res2, err := newRepairService(t, db2, dbPath2, "").RepairImageRegistry(
		context.Background(), ImageRepairOpts{Commit: true})
	if err != nil {
		t.Fatalf("second RepairImageRegistry: %v", err)
	}
	if res2.ArtistsSkipped != 1 || res2.ArtistsScanned != 0 {
		t.Errorf("unresolvable artist: skipped %d, scanned %d; want 1, 0",
			res2.ArtistsSkipped, res2.ArtistsScanned)
	}
	if !hasOutcome(res2, id, "skipped", "no_image_dir") {
		t.Error("unresolvable image dir was not reported")
	}
}

// TestRepairLibraryUnreachableFailsClosed is the mount-down guard test, and it
// guards the heart of this feature: a total library outage must not be reported
// as a clean no-op.
//
// When the media mount is gone, every artist directory returns ENOENT, which
// per-artist reads as "definitively absent, nothing to restore". Left alone
// that turns a catastrophe into "all absent, 0 inserted, clean" -- the exact
// mistake, one layer up, that this whole feature exists to prevent. A
// library-wide pass that scans nothing yet sees absences must FAIL CLOSED.
//
// The second half is equally load-bearing: a single genuinely-absent artist
// under a healthy library must still skip cleanly. The guard must fire only on
// a whole-library miss, never punish one missing folder.
func TestRepairLibraryUnreachableFailsClosed(t *testing.T) {
	// Phase 1: an entire library under a mount root that does not exist.
	db, dbPath := setupTestDBWithImages(t)
	missingRoot := filepath.Join(t.TempDir(), "unmounted", "library")
	ids := []string{
		"cccc0000-0000-0000-0000-000000000001",
		"cccc0000-0000-0000-0000-000000000002",
		"cccc0000-0000-0000-0000-000000000003",
	}
	for i, id := range ids {
		seedArtist(t, db, id, filepath.Join(missingRoot, "artist-"+strconv.Itoa(i)))
	}

	res, err := newRepairService(t, db, dbPath, "").RepairImageRegistry(
		context.Background(), ImageRepairOpts{Commit: true})
	if !errors.Is(err, ErrLibraryUnreachable) {
		t.Fatalf("whole library unreachable: err = %v, want ErrLibraryUnreachable", err)
	}
	if res.RowsInserted != 0 {
		t.Errorf("RowsInserted = %d, want 0 -- nothing may be written when the library is unreachable", res.RowsInserted)
	}
	if res.ArtistsScanned != 0 {
		t.Errorf("ArtistsScanned = %d, want 0", res.ArtistsScanned)
	}

	// A single-artist scope must NOT fail closed on that same absent artist:
	// one data point is not evidence about the mount.
	single, err := newRepairService(t, db, dbPath, "").RepairImageRegistry(
		context.Background(), ImageRepairOpts{Commit: true, ArtistID: ids[0]})
	if err != nil {
		t.Fatalf("single-artist scope must not fail closed: %v", err)
	}
	if single.ArtistsAbsent != 1 {
		t.Errorf("single absent artist: ArtistsAbsent = %d, want 1", single.ArtistsAbsent)
	}

	// Phase 2: a HEALTHY library where one artist's folder is genuinely absent.
	// Because at least one artist scans, the guard must not fire, and the absent
	// artist must skip cleanly rather than being reported as a failure.
	db2, dbPath2 := setupTestDBWithImages(t)
	libRoot := t.TempDir()
	const presentID = "dddd0000-0000-0000-0000-000000000001"
	const absentID = "dddd0000-0000-0000-0000-000000000002"
	presentDir := filepath.Join(libRoot, "artist-present")
	seedArtist(t, db2, presentID, presentDir)
	writeImage(t, filepath.Join(presentDir, "backdrop.jpg"), 1920, 1080)
	seedArtist(t, db2, absentID, filepath.Join(libRoot, "artist-absent")) // never created

	res2, err := newRepairService(t, db2, dbPath2, "").RepairImageRegistry(
		context.Background(), ImageRepairOpts{Commit: true})
	if err != nil {
		t.Fatalf("healthy library with one absent artist must not fail closed: %v", err)
	}
	if res2.ArtistsScanned != 1 || res2.ArtistsAbsent != 1 {
		t.Errorf("scanned/absent = %d/%d, want 1/1", res2.ArtistsScanned, res2.ArtistsAbsent)
	}
	if !hasOutcome(res2, absentID, "skipped", "dir_absent") {
		t.Error("the genuinely-absent artist was not reported as a clean skip")
	}
	if !rowExists(t, db2, presentID, "fanart", 0) {
		t.Error("the present artist's row was not rebuilt")
	}
}

// TestRepairIsStructurallyInsertOnly turns "this repair only ever inserts" from
// a review-time observation into a permanent property. This pass writes rows
// that are missing and touches nothing else: no DELETE, and -- because it never
// modifies an existing row, not even to correct a flag -- no UPDATE either.
// Nothing else in this suite would necessarily catch a future patch that added
// an "update the stale ones while we're here" branch, which is precisely the
// row-mutating behavior that belongs behind a separate opt-in.
func TestRepairIsStructurallyInsertOnly(t *testing.T) {
	src, err := os.ReadFile("image_registry_repair.go")
	if err != nil {
		t.Fatalf("reading source: %v", err)
	}
	body := stripComments(string(src))
	for _, forbidden := range []string{"DELETE", "UPDATE", "DROP", "TRUNCATE", "os.Remove", "os.Rename", "os.Truncate"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("image_registry_repair.go contains %q; this pass must be insert-only", forbidden)
		}
	}
}

// stripComments removes // comments so the structural test reads the code
// rather than the prose about the code -- the file's doc comment names every
// forbidden token.
func stripComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if i := strings.Index(line, "//"); i >= 0 && !strings.Contains(line[:i], `"`) {
			line = line[:i]
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
