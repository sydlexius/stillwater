package maintenance

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	img "github.com/sydlexius/stillwater/internal/image"
)

const testPrimary = "backdrop.jpg"

// embyPrimary resolves the Emby primary fanart name. Using a NON-default name
// on purpose: the default is "fanart.jpg", so a regression that quietly falls
// back to the defaults (as ScanExistsFlags legitimately does) would discover
// zero files here and be caught, instead of passing because the fixture happened
// to agree with the fallback.
func embyPrimary(context.Context) string { return testPrimary }

// backfillJPEG encodes a JPEG with block structure so its perceptual hash is
// non-trivial and variant-specific. A flat fill hashes to all zeros for every
// image, which would make "the right file's hash" indistinguishable from any
// other file's.
func backfillJPEG(t *testing.T, variant int) []byte {
	t.Helper()
	const (
		blocks = 8
		w      = 640
		h      = 360
	)
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			bx, by := x*blocks/w, y*blocks/h
			hsh := uint32(bx)*374761393 + uint32(by)*668265263 + uint32(variant)*2246822519
			hsh ^= hsh >> 13
			hsh *= 1274126177
			hsh ^= hsh >> 16
			v := uint8(hsh >> 8)
			m.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	return buf.Bytes()
}

// seedFanartArtist writes `count` fanart files (primary + numbered variants,
// Emby numbering) and inserts the artist plus one artist_images row per slot
// with an EMPTY phash -- the starved state under test.
func seedFanartArtist(t *testing.T, db *sql.DB, artistID string, count int) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	for i := range count {
		name := img.FanartFilename(testPrimary, i, false)
		if err := os.WriteFile(filepath.Join(dir, name), backfillJPEG(t, i), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES (?, ?, ?)`, artistID, artistID, dir); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
	for i := range count {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
			 VALUES (?, ?, 'fanart', ?, 1, '', '')`,
			artistID+"-fanart-"+string(rune('0'+i)), artistID, i); err != nil {
			t.Fatalf("seeding image row slot %d: %v", i, err)
		}
	}
	return dir
}

func fanartHashes(t *testing.T, db *sql.DB, artistID string, slot int) (phash, content string) {
	t.Helper()
	err := db.QueryRowContext(context.Background(),
		`SELECT phash, content_hash FROM artist_images
		 WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = ?`, artistID, slot).Scan(&phash, &content)
	if err != nil {
		t.Fatalf("reading hashes for slot %d: %v", slot, err)
	}
	return phash, content
}

// TestBackfillFanartHashes_FillsStarvedSlots is the #2564 guard for the
// backfill.
//
// REVERT-AND-RERUN: narrowing the SELECT to slot_index = 0, or dropping the
// UPDATE, must turn this RED on the per-slot hash assertions.
func TestBackfillFanartHashes_FillsStarvedSlots(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	dir := seedFanartArtist(t, db, "artist-starved", 3)

	// PRECONDITION: every slot starts starved. Without this the test could pass
	// against rows that were never empty, proving nothing about backfilling.
	for slot := range 3 {
		if ph, _ := fanartHashes(t, db, "artist-starved", slot); ph != "" {
			t.Fatalf("precondition: slot %d phash = %q, want empty", slot, ph)
		}
	}

	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 0); err != nil {
		t.Fatalf("BackfillFanartHashes: %v", err)
	}

	// Each slot must receive the hash of ITS OWN file. Asserting merely
	// "non-empty" would pass even if every slot got slot 0's hash -- the exact
	// cross-slot mix-up this whole change exists to prevent.
	seen := map[string]int{}
	for slot := range 3 {
		wantPath := filepath.Join(dir, img.FanartFilename(testPrimary, slot, false))
		fh, err := img.HashFile(wantPath, true)
		if err != nil {
			t.Fatalf("hashing expected file for slot %d: %v", slot, err)
		}
		want := img.HashHex(fh.Perceptual)

		gotPH, gotContent := fanartHashes(t, db, "artist-starved", slot)
		if gotPH == "" {
			t.Errorf("slot %d phash is empty -- backfill left the slot starved", slot)
			continue
		}
		if gotPH != want {
			t.Errorf("slot %d phash = %q, want its own file's %q", slot, gotPH, want)
		}
		if gotContent != fh.Content {
			t.Errorf("slot %d content_hash = %q, want %q", slot, gotContent, fh.Content)
		}
		seen[gotPH]++
	}

	// PRECONDITION on the fixture: the three files must hash distinctly, or the
	// per-slot assertions above could not have discriminated anything.
	if len(seen) != 3 {
		t.Errorf("fixture produced %d distinct phashes across 3 slots, want 3; the test cannot discriminate slots", len(seen))
	}
}

// TestBackfillFanartHashes_PreservesExistingHashes proves the pass is
// convergent: a row that already has a phash is not selected, so a healed
// library costs no work and the save path's better (EXIF-derived) value is never
// overwritten by this slower re-read.
func TestBackfillFanartHashes_PreservesExistingHashes(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	seedFanartArtist(t, db, "artist-healed", 2)

	const sentinel = "1234567890abcdef"
	if _, err := db.ExecContext(context.Background(),
		`UPDATE artist_images SET phash = ?, content_hash = 'kept'
		 WHERE artist_id = 'artist-healed' AND slot_index = 0`, sentinel); err != nil {
		t.Fatalf("pre-filling slot 0: %v", err)
	}

	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 0); err != nil {
		t.Fatalf("BackfillFanartHashes: %v", err)
	}

	ph, content := fanartHashes(t, db, "artist-healed", 0)
	if ph != sentinel {
		t.Errorf("slot 0 phash = %q, want the pre-existing %q -- backfill overwrote a hash it should not have selected", ph, sentinel)
	}
	if content != "kept" {
		t.Errorf("slot 0 content_hash = %q, want %q", content, "kept")
	}

	// The genuinely starved sibling must still have been filled, proving the
	// preservation above is selectivity and not the task no-opping entirely.
	if ph1, _ := fanartHashes(t, db, "artist-healed", 1); ph1 == "" {
		t.Error("slot 1 phash is empty -- backfill skipped a starved slot")
	}
}

// TestBackfillFanartHashes_RejectsMissingResolver pins the loud-failure
// contract. A nil or empty resolver must error rather than fall back to the
// default naming: on an Emby library the default discovers nothing, so the pass
// would report success having silently healed zero rows.
func TestBackfillFanartHashes_RejectsMissingResolver(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())

	if err := svc.BackfillFanartHashes(context.Background(), nil, 0); err == nil {
		t.Error("nil resolver: expected an error, got nil -- a silent no-op here heals nothing while reporting success")
	}
	empty := func(context.Context) string { return "" }
	if err := svc.BackfillFanartHashes(context.Background(), empty, 0); err == nil {
		t.Error("empty primary name: expected an error, got nil")
	}
}

// TestBackfillFanartHashes_SkipsVanishedSlot covers a row that outlived its
// file (or whose slots were renumbered between the SELECT and the read). It
// must be skipped, not fail the pass and not fill the row with a neighbour's
// hash.
func TestBackfillFanartHashes_SkipsVanishedSlot(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	seedFanartArtist(t, db, "artist-vanished", 1)

	// A row for a slot with no file behind it.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
		 VALUES ('ghost', 'artist-vanished', 'fanart', 5, 1, '', '')`); err != nil {
		t.Fatalf("seeding ghost row: %v", err)
	}

	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 0); err != nil {
		t.Fatalf("BackfillFanartHashes: %v", err)
	}

	if ph, _ := fanartHashes(t, db, "artist-vanished", 5); ph != "" {
		t.Errorf("ghost slot 5 phash = %q, want empty -- a slot with no file was given some other file's hash", ph)
	}
	// The real slot must still be healed: one bad row must not poison the pass.
	if ph, _ := fanartHashes(t, db, "artist-vanished", 0); ph == "" {
		t.Error("slot 0 phash is empty -- a vanished sibling slot aborted the whole pass")
	}
}

// TestBackfillFanartHashes_RespectsMaxPerPass pins the bound, and that the
// remainder is still reachable on a later pass (convergence, not loss).
func TestBackfillFanartHashes_RespectsMaxPerPass(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	seedFanartArtist(t, db, "artist-capped", 4)

	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 2); err != nil {
		t.Fatalf("BackfillFanartHashes: %v", err)
	}

	filled := 0
	for slot := range 4 {
		if ph, _ := fanartHashes(t, db, "artist-capped", slot); ph != "" {
			filled++
		}
	}
	if filled != 2 {
		t.Errorf("filled %d slots in a capped pass, want exactly 2", filled)
	}

	// A second pass picks up the remainder: the cap defers work, never drops it.
	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 2); err != nil {
		t.Fatalf("second BackfillFanartHashes: %v", err)
	}
	for slot := range 4 {
		if ph, _ := fanartHashes(t, db, "artist-capped", slot); ph == "" {
			t.Errorf("slot %d still starved after two capped passes -- the bound is dropping work rather than deferring it", slot)
		}
	}
}

// TestBackfillFanartHashes_SkipsUndecodableFile covers the corrupt-file skip.
//
// This is the path the maxPerPass bound exists for: a file that cannot be
// decoded stays starved and is retried next pass. The failure mode it guards is
// worse than a skip -- writing a zero/garbage hash would POISON the very
// detector this backfill exists to feed, and it would look like clean data. The
// healthy sibling must still be filled, proving one bad file does not abort the
// pass.
func TestBackfillFanartHashes_SkipsUndecodableFile(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	dir := seedFanartArtist(t, db, "artist-corrupt", 2)

	// Replace slot 1's file with bytes that are not a decodable image, keeping
	// the name so DiscoverFanart still maps slot 1 to it.
	corrupt := filepath.Join(dir, img.FanartFilename(testPrimary, 1, false))
	if err := os.WriteFile(corrupt, []byte("this is not a JPEG"), 0o644); err != nil {
		t.Fatalf("writing corrupt file: %v", err)
	}

	if err := svc.BackfillFanartHashes(context.Background(), embyPrimary, 0); err != nil {
		t.Fatalf("BackfillFanartHashes: %v -- one undecodable file must not fail the pass", err)
	}

	if ph, _ := fanartHashes(t, db, "artist-corrupt", 1); ph != "" {
		t.Errorf("slot 1 phash = %q, want empty -- an undecodable file was given a hash anyway, poisoning the detector with data that looks real", ph)
	}
	if ph, _ := fanartHashes(t, db, "artist-corrupt", 0); ph == "" {
		t.Error("slot 0 phash is empty -- one corrupt sibling aborted the whole pass")
	}
}

// TestBackfillFanartHashes_SkipsUnverifiableDirs covers both "cannot resolve a
// directory" and "cannot read the directory", which must SKIP rather than fail
// the pass or invent a hash.
//
// Both are ordinary production states: a cache-only artist while imageCacheDir
// is unset, and an artist whose directory has been removed or unmounted since
// the row was written.
func TestBackfillFanartHashes_SkipsUnverifiableDirs(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	// Empty imageCacheDir, so an artist with no path has no resolvable dir.
	svc := NewService(db, dbPath, "", slog.Default())
	ctx := context.Background()

	// Artist 1: no path and no cache-dir fallback -> unresolvable.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES ('artist-nodir', 'artist-nodir', '')`); err != nil {
		t.Fatalf("seeding pathless artist: %v", err)
	}
	// Artist 2: a path that does not exist on disk -> DiscoverFanart errors.
	gone := filepath.Join(t.TempDir(), "unmounted")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, path) VALUES ('artist-gone', 'artist-gone', ?)`, gone); err != nil {
		t.Fatalf("seeding vanished-dir artist: %v", err)
	}
	for _, id := range []string{"artist-nodir", "artist-gone"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
			 VALUES (?, ?, 'fanart', 0, 1, '', '')`, id+"-img", id); err != nil {
			t.Fatalf("seeding image row for %s: %v", id, err)
		}
	}

	if err := svc.BackfillFanartHashes(ctx, embyPrimary, 0); err != nil {
		t.Fatalf("BackfillFanartHashes: %v -- unverifiable dirs must be skipped, not fail the pass", err)
	}

	for _, id := range []string{"artist-nodir", "artist-gone"} {
		if ph, _ := fanartHashes(t, db, id, 0); ph != "" {
			t.Errorf("%s slot 0 phash = %q, want empty -- a slot whose directory could not be read was given a hash", id, ph)
		}
	}
}

// TestBackfillFanartHashes_HealedLibraryIsNoOp pins the convergence claim the
// design rests on: the work-set is defined by the starvation itself, so a fully
// healed library SELECTS NOTHING and returns before doing any work. That is what
// makes a run ledger unnecessary -- if a healed library still re-hashed every
// file, this task would re-read the whole library from pixels every interval
// forever.
//
// The assertion is on the "pass complete" log, NOT on the hashes. Asserting the
// hashes are unchanged would be VACUOUS: re-hashing the same file yields the
// same value, and the UPDATE only matches an empty phash anyway, so that
// assertion stays green even if the SELECT stopped filtering. The early return
// fires BEFORE the pass-complete log, so that line's absence is the one
// observable that actually discriminates "selected nothing" from "selected
// everything and rewrote it with identical values".
func TestBackfillFanartHashes_HealedLibraryIsNoOp(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	svc := NewService(db, dbPath, "", lg)
	seedFanartArtist(t, db, "artist-converged", 2)
	ctx := context.Background()

	// First pass has real work to do, and must therefore LOG a completed pass.
	// This is the positive control: it proves the log line the no-op assertion
	// below keys on is one this code actually emits when it does work.
	if err := svc.BackfillFanartHashes(ctx, embyPrimary, 0); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if !strings.Contains(buf.String(), "fanart hash backfill pass complete") {
		t.Fatal("precondition: a pass with work to do did not log completion; the no-op assertion below would be vacuous")
	}
	for slot := range 2 {
		if ph, _ := fanartHashes(t, db, "artist-converged", slot); ph == "" {
			t.Fatalf("precondition: slot %d still starved after the first pass", slot)
		}
	}

	// Second pass over a healed library: the work-set is empty, so it must return
	// early -- no pass, no log.
	buf.Reset()
	if err := svc.BackfillFanartHashes(ctx, embyPrimary, 0); err != nil {
		t.Fatalf("second pass over a healed library: %v", err)
	}
	if strings.Contains(buf.String(), "fanart hash backfill pass complete") {
		t.Errorf("a healed library still ran a full pass (%q) -- the work-set is not converging, so this re-hashes the whole library from pixels every interval forever", buf.String())
	}
}

// -- StartFanartHashBackfill scheduler ----------------------------------------

// waitForPhash polls until the slot's phash is non-empty, returning whether it
// filled before the deadline.
//
// Polling an OBSERVABLE EFFECT is deliberate. The obvious alternative -- sleep
// past the delay, then compare timestamps -- is how issue #2575 was born: two
// time.Now() calls landing in the same microsecond flake the comparison. The
// scheduler's contract is "a run HAPPENED", and a filled hash is that fact
// directly, with no clock in the assertion.
func waitForPhash(t *testing.T, db *sql.DB, artistID string, slot int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		var ph string
		err := db.QueryRowContext(context.Background(),
			`SELECT phash FROM artist_images
			 WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = ?`, artistID, slot).Scan(&ph)
		if err == nil && ph != "" {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestStartFanartHashBackfill_RunsStartupAndTick proves both halves of the
// scheduler: the startup pass heals what is starved at boot, and the TICKER
// keeps healing rows that starve afterwards.
//
// The tick half is the one that matters and the one a fire-once regression would
// silently lose. Per this task's own rationale, fanart discovered by a scan
// (rather than written by Stillwater) arrives with no phash at any time, not
// just at boot -- so a scheduler that ran only its startup pass would leave
// every post-boot arrival starved forever while still logging a clean run. The
// second artist is seeded AFTER the startup pass is observed to have completed,
// so nothing but a genuine subsequent tick can heal it.
func TestStartFanartHashBackfill_RunsStartupAndTick(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	seedFanartArtist(t, db, "artist-boot", 1)

	// PRECONDITION: starved before the scheduler runs, or "filled" below proves
	// nothing about the scheduler having done the filling.
	if ph, _ := fanartHashes(t, db, "artist-boot", 0); ph != "" {
		t.Fatalf("precondition: artist-boot slot 0 phash = %q, want empty", ph)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.StartFanartHashBackfill(runCtx, embyPrimary, 10*time.Millisecond, 10*time.Millisecond)
		close(done)
	}()
	// Cancel unconditionally on the way out so a failed assertion cannot leak the
	// scheduler goroutine into the rest of the package's tests.
	defer func() {
		cancel()
		<-done
	}()

	if !waitForPhash(t, db, "artist-boot", 0, 3*time.Second) {
		t.Fatal("startup pass did not fill the starved slot within 3s")
	}

	// Starve a NEW row only now that the startup pass is provably done, so only a
	// later tick can account for it being healed.
	seedFanartArtist(t, db, "artist-tick", 1)
	if !waitForPhash(t, db, "artist-tick", 0, 3*time.Second) {
		t.Fatal("ticker did not run a backfill pass within 3s of a new row starving -- the scheduler heals only at boot, so anything scanned later stays starved forever")
	}

	// Cancel must stop the loop promptly and cleanly.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop within 2s of cancel")
	}
}

// TestStartFanartHashBackfill_ZeroTimingsDeferAndCancelCleanly pins the
// startup-delay contract together with the zero-value defaults.
//
// Passing 0 must mean "use the 30s default", NOT "start immediately". That
// distinction is load-bearing at boot: this pass hashes files from PIXELS, so an
// immediate run would contend with migrations and the initial scan on exactly
// the I/O they need. Asserting only that cancel returns promptly would NOT catch
// a regression that ran the pass and then exited -- so the assertion is that the
// slot is STILL STARVED, which is false the moment any pass has run.
func TestStartFanartHashBackfill_ZeroTimingsDeferAndCancelCleanly(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	seedFanartArtist(t, db, "artist-deferred", 1)

	// Zero interval and zero startupDelay: both must fall back to their defaults
	// (6h / 30s), which this test then cancels well inside.
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.StartFanartHashBackfill(runCtx, embyPrimary, 0, 0)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop within 2s of cancel during the startup delay")
	}

	if ph, _ := fanartHashes(t, db, "artist-deferred", 0); ph != "" {
		t.Errorf("slot 0 phash = %q, want empty -- a zero startupDelay ran the pass immediately instead of defaulting to 30s, so the backfill contends with boot I/O", ph)
	}
}

// TestStartFanartHashBackfill_NilResolverNoOp pins the nil-resolver guard at the
// SCHEDULER boundary. BackfillFanartHashes already errors on a nil resolver, but
// the scheduler must decline to start at all rather than spawn a loop that
// re-errors every interval forever.
//
// If the guard regresses the call blocks on the startup delay and the test fails
// on the channel deadline rather than hanging the suite.
func TestStartFanartHashBackfill_NilResolverNoOp(t *testing.T) {
	db, dbPath := setupTestDBWithImages(t)
	svc := NewService(db, dbPath, "", slog.Default())
	seedFanartArtist(t, db, "artist-nil-resolver", 1)

	done := make(chan struct{})
	go func() {
		svc.StartFanartHashBackfill(context.Background(), nil, time.Millisecond, time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("nil resolver: scheduler did not return immediately -- it started a loop that can only fail every interval")
	}

	if ph, _ := fanartHashes(t, db, "artist-nil-resolver", 0); ph != "" {
		t.Errorf("slot 0 phash = %q, want empty -- a nil resolver must heal nothing", ph)
	}
}
