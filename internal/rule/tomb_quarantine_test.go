package rule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// onlyCopyBytes stands in for real artwork stranded at a tomb path by a hard
// crash. Deliberately not a decodable image: the quarantine must preserve
// bytes without caring what they are.
var onlyCopyBytes = []byte("ONLY-COPY-OF-REAL-ARTWORK-2956")

// writeStrandedTomb places onlyCopyBytes at path and asserts the fixture, so a
// test cannot pass because the setup silently did nothing.
func writeStrandedTomb(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing stranded tomb: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(data) {
		t.Fatalf("precondition failed: stranded tomb %s does not hold the fixture bytes (err=%v)", path, err)
	}
}

// requireBytesUnderOrphan fails unless exactly one file matching pattern exists
// in dir and it holds want. Matching on bytes (not just name) is the point: a
// fix that renamed the wrong thing or truncated it must not pass.
func requireBytesUnderOrphan(t *testing.T, dir, pattern string, want []byte) {
	t.Helper()
	// ReadDir + Match rather than Glob(dir/pattern): the dir name itself may
	// contain glob metacharacters.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var matches []string
	for _, e := range entries {
		if ok, _ := filepath.Match(pattern, e.Name()); ok {
			matches = append(matches, filepath.Join(dir, e.Name()))
		}
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one file matching %s, got %v (the stranded artwork was destroyed or duplicated)", pattern, matches)
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("reading orphan: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("orphan %s holds %q, want the stranded bytes %q", matches[0], got, want)
	}
}

// TestImageDuplicateFixer_Fix_PreservesStrandedTomb drives the real Fix entry
// point with a crash-stranded tomb in the directory and asserts the exact bytes
// survive under an orphan name (#2956). Reverting the sweep to os.Remove makes
// this RED.
func TestImageDuplicateFixer_Fix_PreservesStrandedTomb(t *testing.T) {
	db := setupTestDB(t)
	insertTestArtist(t, db, "art-2956", "Tomb Artist")
	insertTestImage(t, db, "art-2956", "fanart", 1)
	insertTestImage(t, db, "art-2956", "fanart", 2)

	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1)
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1) // duplicate of slot 1
	tomb := filepath.Join(dir, "fanart7.jpg"+dupTombSuffix)
	writeStrandedTomb(t, tomb, onlyCopyBytes)

	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), &fakeHashRecorder{}, testLogger())
	a := &artist.Artist{ID: "art-2956", Name: "Tomb Artist", Path: dir, LibraryID: "lib-test", FanartExists: true, FanartCount: 3}
	res, err := f.Fix(t.Context(), a, &Violation{RuleID: RuleImageDuplicate, Config: RuleConfig{Tolerance: 0.90}})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed {
		t.Fatalf("precondition failed: Fix removed nothing (%q), so it may not have reached the tomb handling", res.Message)
	}

	if _, statErr := os.Lstat(tomb); !os.IsNotExist(statErr) {
		t.Errorf("the tomb path should be cleared after quarantine, stat err = %v", statErr)
	}
	requireBytesUnderOrphan(t, dir, "fanart7.orphan-*.jpg", onlyCopyBytes)
}

// TestDeleteDuplicateFanart_SweepCollisionNeverOverwrites (sweep path: the
// delete set is empty, so only sweepOrphanedDupTombs runs) pins the clock so
// the orphan name is predictable, pre-places a file at exactly that name, and
// asserts both files survive: the helper must take the counter branch rather
// than overwrite (os.Rename would have clobbered it).
func TestDeleteDuplicateFanart_SweepCollisionNeverOverwrites(t *testing.T) {
	a, dir := dupArtistDir(t)
	pinned := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := newDupFixerFor(t, &fakeHashRecorder{})
	f.now = func() time.Time { return pinned }

	earlier := []byte("EARLIER-ORPHAN-ONLY-COPY")
	colliding := filepath.Join(dir, "fanart7.orphan-20260102T030405.000000000Z.jpg")
	writeStrandedTomb(t, colliding, earlier) // precondition: the EEXIST branch is reachable
	tomb := filepath.Join(dir, "fanart7.jpg"+dupTombSuffix)
	writeStrandedTomb(t, tomb, onlyCopyBytes)

	if _, err := f.deleteDuplicateFanartWithRollback(t.Context(), a, "fanart.jpg", false, map[int]bool{}); err != nil {
		t.Fatalf("deleteDuplicateFanartWithRollback: %v", err)
	}

	if got, err := os.ReadFile(colliding); err != nil || string(got) != string(earlier) {
		t.Errorf("the pre-existing orphan was overwritten or lost (err=%v, bytes=%q)", err, got)
	}
	suffixed := filepath.Join(dir, "fanart7.orphan-20260102T030405.000000000Z-1.jpg")
	if got, err := os.ReadFile(suffixed); err != nil || string(got) != string(onlyCopyBytes) {
		t.Errorf("the stranded tomb was not preserved under the counter name (err=%v, bytes=%q)", err, got)
	}
}

// TestRemediatePHashMismatches_PreservesStrandedTomb is the pHash twin: a
// stranded phash tomb at the path the run is about to stage onto must be
// quarantined, not unlinked, when driven through RemediatePHashMismatches.
func TestRemediatePHashMismatches_PreservesStrandedTomb(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	// fanart2.jpg is the polluted slot, so its tomb path is exactly the one the
	// staging step clears.
	tomb := filepath.Join(dirA, "fanart2.jpg"+phashTombSuffix)
	writeStrandedTomb(t, tomb, onlyCopyBytes)

	res, err := p.RemediatePHashMismatches(t.Context(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("RemediatePHashMismatches: %v", err)
	}
	if res.SlotsRemoved != 1 {
		t.Fatalf("precondition failed: the run removed %d slot(s), want 1, so it did not reach the tomb handling: %+v", res.SlotsRemoved, res)
	}

	if _, statErr := os.Lstat(tomb); !os.IsNotExist(statErr) {
		t.Errorf("the tomb path should be cleared after quarantine, stat err = %v", statErr)
	}
	requireBytesUnderOrphan(t, dirA, "fanart2.orphan-*.jpg", onlyCopyBytes)
}

// TestQuarantinedOrphanSurvivesExtraneousImagesRule runs a stranded tomb
// through the real dup fixer (quarantined to an orphan name that ends in .jpg),
// then through the real extraneous-images checker and fixer. The orphan must
// not be flagged or unlinked; a control file proves both really ran.
func TestQuarantinedOrphanSurvivesExtraneousImagesRule(t *testing.T) {
	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "random.jpg"), 1) // control: genuinely extraneous
	writeStrandedTomb(t, filepath.Join(dir, "fanart7.jpg"+dupTombSuffix), onlyCopyBytes)
	a := &artist.Artist{ID: "art-2956x", Name: "Orphan Rule", Path: dir, LibraryID: "lib-test"}

	if _, err := newDupFixerFor(t, &fakeHashRecorder{}).deleteDuplicateFanartWithRollback(
		t.Context(), a, "fanart.jpg", false, map[int]bool{}); err != nil {
		t.Fatalf("dup fixer: %v", err)
	}
	requireBytesUnderOrphan(t, dir, "fanart7.orphan-*.jpg", onlyCopyBytes) // precondition

	e := &Engine{platformService: nil}
	v := e.makeExtraneousImagesChecker()(t.Context(), a, RuleConfig{Severity: "warning"})
	if v == nil || !strings.Contains(v.Message, "random.jpg") {
		t.Fatalf("precondition failed: the checker did not flag the control file: %+v", v)
	}
	if strings.Contains(v.Message, "orphan-") {
		t.Errorf("the checker flagged the quarantined orphan as extraneous: %s", v.Message)
	}

	res, err := NewExtraneousImagesFixer(nil, nonSharedFSCheck(), testLogger()).Fix(t.Context(), a, &Violation{RuleID: RuleExtraneousImages})
	if err != nil || !res.Fixed {
		t.Fatalf("extraneous fixer: fixed=%v err=%v", res != nil && res.Fixed, err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "random.jpg")); !os.IsNotExist(statErr) {
		t.Fatalf("precondition failed: the control file was not removed (%v)", statErr)
	}
	requireBytesUnderOrphan(t, dir, "fanart7.orphan-*.jpg", onlyCopyBytes)
}

// TestSweepOrphanedDupTombs_GlobMetacharactersInDirName: a directory named
// "Artist [Live]" made the old filepath.Glob sweep match nothing, silently. The
// tomb is for a slot that is NOT being deleted, so only the sweep can reach it.
func TestSweepOrphanedDupTombs_GlobMetacharactersInDirName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Artist [Live]")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for i, name := range []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"} {
		createGradientJPEG(t, filepath.Join(dir, name), i)
	}
	writeStrandedTomb(t, filepath.Join(dir, "fanart7.jpg"+dupTombSuffix), onlyCopyBytes)
	a := &artist.Artist{ID: "art-2956g", Name: "Glob Artist", Path: dir, LibraryID: "lib-test"}

	if _, err := newDupFixerFor(t, &fakeHashRecorder{}).deleteDuplicateFanartWithRollback(
		t.Context(), a, "fanart.jpg", false, map[int]bool{1: true}); err != nil {
		t.Fatalf("deleteDuplicateFanartWithRollback: %v", err)
	}
	requireBytesUnderOrphan(t, dir, "fanart7.orphan-*.jpg", onlyCopyBytes)
}

// squatLiveTomb runs the real dup path with a squatter at the live tomb path of
// the slot being deleted and asserts it aborts before the source moves.
func squatLiveTomb(t *testing.T, mkSquatter func(tomb string)) {
	t.Helper()
	a, dir := dupArtistDir(t)
	source := filepath.Join(dir, "fanart1.jpg") // slot 1, the one being deleted
	sourceBytes := readBytes(t, source)
	tomb := source + dupTombSuffix
	mkSquatter(tomb)

	_, err := newDupFixerFor(t, &fakeHashRecorder{}).deleteDuplicateFanartWithRollback(
		t.Context(), a, "fanart.jpg", false, map[int]bool{1: true})
	if err == nil {
		t.Fatal("expected an error: a non-regular entry at the live tomb path must abort staging")
	}
	if got, readErr := os.ReadFile(source); readErr != nil || string(got) != string(sourceBytes) {
		t.Errorf("the source moved or changed despite the abort (err=%v)", readErr)
	}
}

func TestDeleteDuplicateFanart_SymlinkAtLiveTombPathAborts(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "elsewhere.bin")
	writeStrandedTomb(t, outside, onlyCopyBytes)
	squatLiveTomb(t, func(tomb string) {
		if err := os.Symlink(outside, tomb); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if fi, err := os.Lstat(tomb); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("precondition failed: %s is not a symlink (%v)", tomb, err)
		}
	})
}

func TestDeleteDuplicateFanart_NonEmptyDirAtLiveTombPathAborts(t *testing.T) {
	squatLiveTomb(t, func(tomb string) {
		if err := os.Mkdir(tomb, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeStrandedTomb(t, filepath.Join(tomb, "keep"), []byte("x"))
		if fi, err := os.Lstat(tomb); err != nil || !fi.IsDir() {
			t.Fatalf("precondition failed: %s is not a directory (%v)", tomb, err)
		}
	})
}

// TestCheckExtraneousAgainst_SkipsQuarantinedOrphan drives the shared-filesystem
// checker path (checkExtraneousAgainst), the second of the two checker loops
// that must ignore orphans. A control extraneous file proves the loop ran.
func TestCheckExtraneousAgainst_SkipsQuarantinedOrphan(t *testing.T) {
	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "random.jpg"), 1) // control
	orphan := filepath.Join(dir, "fanart7.orphan-20260102T030405.000000000Z.jpg")
	writeStrandedTomb(t, orphan, onlyCopyBytes)

	e := &Engine{}
	a := &artist.Artist{Name: "Shared", Path: dir}
	v := e.checkExtraneousAgainst(a, map[string]bool{"fanart.jpg": true}, RuleConfig{Severity: "warning"})
	if v == nil || !strings.Contains(v.Message, "random.jpg") {
		t.Fatalf("precondition failed: the control file was not flagged: %+v", v)
	}
	if strings.Contains(v.Message, "orphan-") {
		t.Errorf("the quarantined orphan was flagged as extraneous: %s", v.Message)
	}
}

// TestSweepOrphanedDupTombs_UnreadableDirQuarantinesNothing: a missing artist
// directory makes the sweep's ReadDir fail. Like the old Glob-error path, the
// sweep must return quietly (a Warn) and the caller's own discovery error is
// what surfaces; nothing is created in the missing directory.
func TestSweepOrphanedDupTombs_UnreadableDirQuarantinesNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("precondition failed: %s should not exist (%v)", missing, err)
	}
	buf, logger := capturingLogger()
	f := NewImageDuplicateFixer(nil, nil, nonSharedFSCheck(), &fakeHashRecorder{}, logger)

	f.sweepOrphanedDupTombs(&artist.Artist{Name: "Gone", Path: missing})

	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Errorf("the sweep created or touched the missing directory (%v)", err)
	}
	if !strings.Contains(buf.String(), "listing directory for orphaned duplicate-fanart tombs") {
		t.Errorf("the ReadDir failure was not logged: %s", buf.String())
	}
}

// TestSweepOrphanedDupTombs_NonRegularEntryDoesNotBlockOthers: a directory whose
// name ends in the tomb suffix is refused (left in place, Warn), and the sweep
// continues to quarantine the regular tomb beside it.
func TestSweepOrphanedDupTombs_NonRegularEntryDoesNotBlockOthers(t *testing.T) {
	dir := t.TempDir()
	squat := filepath.Join(dir, "aaa.jpg"+dupTombSuffix) // sorts first, so it is hit before the regular tomb
	if err := os.Mkdir(squat, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if fi, err := os.Lstat(squat); err != nil || !fi.IsDir() {
		t.Fatalf("precondition failed: squatter is not a directory (%v)", err)
	}
	writeStrandedTomb(t, filepath.Join(dir, "zzz.jpg"+dupTombSuffix), onlyCopyBytes)
	buf, logger := capturingLogger()
	f := NewImageDuplicateFixer(nil, nil, nonSharedFSCheck(), &fakeHashRecorder{}, logger)

	f.sweepOrphanedDupTombs(&artist.Artist{Name: "Squat", Path: dir})

	if fi, err := os.Lstat(squat); err != nil || !fi.IsDir() {
		t.Errorf("the non-regular entry must be left in place (%v)", err)
	}
	requireBytesUnderOrphan(t, dir, "zzz.orphan-*.jpg", onlyCopyBytes)
	if !strings.Contains(buf.String(), "sweeping orphaned duplicate-fanart tomb") {
		t.Errorf("the refused entry was not logged: %s", buf.String())
	}
}
