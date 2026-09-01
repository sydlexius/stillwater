package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	img "github.com/sydlexius/stillwater/internal/image"
)

// #2712: snapshotFanart holds the WHOLE fanart set in memory for the duration of
// a push, and until this change nothing capped how much that could be -- not the
// file count, not any one file's size, not the cumulative total, and concurrent
// syncs multiply all three. The largest artist in the production library that
// surfaced the issue holds 42 backdrops.
//
// WHAT THESE TESTS ARE REALLY GUARDING, and it is not the arithmetic. A cap is
// easy; a cap that degrades SILENTLY is worse than no cap at all, because a
// skipped slot holds no bytes and the repair loop skips every nil-data entry --
// so a peer delete of a capped file during that push is unrepairable, and the
// operator is never told. Each test below therefore asserts all three signals of
// the degrade convention this function already uses for an unreadable file: a
// warning the caller surfaces, a nil-data entry, and the TRUE index preserved
// for every later slot.
//
// EVERY TEST HERE IS SERIAL -- no t.Parallel, deliberately. These fixtures
// present hundreds of megabytes of file, and internal/image's stalled-read cap
// is a PROCESS-GLOBAL gauge that the tests in snapshot_stall_cap_unix_test.go
// deliberately drive to within ONE of its refusal threshold. Running heavy I/O
// in parallel with a gauge balanced that finely is a way to make an unrelated
// test in another file fail for reasons that have nothing to do with it, so
// these stay out of its way. (That neighbor was measured flaking on roughly
// 1 in 16 shuffled -race runs on this branch's base WITHOUT this file present,
// so the flakiness is pre-existing rather than introduced here and is tracked
// in #3016; being serial simply avoids adding to it.)
//
// These call snapshotFanart directly. It takes its paths as a parameter (the
// caller owns discovery), so a direct call hands it exactly the arguments the
// sync does, and it is the only way to build a set that trips the count cap
// without planting 101 real files per case.

// boundTestPublisher builds the minimal publisher snapshotFanart needs.
//
// Deliberately NOT syncTestPublisher: that helper lives behind a `//go:build
// unix` tag, and the caps are platform-independent, so borrowing it would make
// these tests silently absent on any platform the tag excludes.
func boundTestPublisher() *Publisher {
	return New(Deps{Logger: silentLogger()})
}

// boundTestPublisherLogging is boundTestPublisher with its log output captured,
// for the tests that assert the degrade is AUDIBLE (#2712 review, N2).
//
// The degrade's doc comment calls it "three simultaneous signals" and treats the
// loudness as the thing standing between a memory guard and invisible data
// loss. Two of those signals -- the warning and the nil-data entry -- are
// asserted everywhere in this file; the Error log was asserted nowhere, so
// deleting the p.logger.Error call left the suite green. It does not now.
func boundTestPublisherLogging(buf *bytes.Buffer) *Publisher {
	return New(Deps{Logger: captureLogger(buf)})
}

// sparseFile plants a file of the requested size without writing its bytes.
// A truncate produces a sparse file on both APFS and every filesystem CI runs
// on, so a 13 MiB fixture costs no disk and no time. The caps are checked from
// a stat, which reports the apparent size, so sparseness is invisible to the
// code under test.
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating sparse fixture %s: %v", path, err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatalf("sizing sparse fixture %s to %d: %v", path, size, err)
	}
	// The Close error is REPORTED rather than deferred away: two tests below
	// derive their cap expectations from the size requested here rather than
	// from an os.Stat, so a flush failure that left a short fixture would
	// surface as a confusing assertion about which cap fired instead of as the
	// fixture failure it is.
	if err := f.Close(); err != nil {
		t.Fatalf("closing sparse fixture %s: %v", path, err)
	}
}

// assertSnapshotShape checks the two structural invariants every degrade path
// owes, on EVERY entry: the returned set covers every input path, and each
// entry's index is its position in the CALLER'S list.
//
// The index check is the load-bearing one and it is why this is shared rather
// than written per test. A cap that dropped refused files instead of keeping a
// nil-data placeholder would leave the surviving backdrops with indices shifted
// down, and since the fanart sync only ever ADDS indices on the platform (it
// never deletes surplus ones), the stale image at the tail index survives
// indefinitely. That damage is invisible to any assertion about warnings.
func assertSnapshotShape(t *testing.T, got []fanartSnapshot, paths []string) {
	t.Helper()
	if len(got) != len(paths) {
		t.Fatalf("snapshot has %d entries for %d input paths; a refused slot must be KEPT with nil data, "+
			"never dropped, or every later backdrop's index shifts down", len(got), len(paths))
	}
	for i := range got {
		if got[i].index != i {
			t.Errorf("entry %d carries index %d, want %d: slot indices are no longer the caller's", i, got[i].index, i)
		}
		if got[i].path != paths[i] {
			t.Errorf("entry %d is %q, want %q: the set is out of order", i, got[i].path, paths[i])
		}
	}
}

// The two halves of the TOTAL-bytes cap produce warnings that differ in one
// phrase, and nothing in the suite used to assert the difference (#2712 review,
// N1/B2). These constants name the phrases once so a wording change breaks in a
// single place rather than in four assertions with four opinions about it.
//
// #3017: this pair used to belong to the PER-FILE cap. The per-file cap no
// longer refuses before the read (it governs retention only, checked after
// the upload loop via fanartSnapshot.overRetentionCap), so refuse's "bytes on
// disk" / refuseResult's "bytes read" split now discriminates the TOTAL cap's
// two halves instead. The phrases are unchanged; only what fires them moved.
const (
	// statRefusalPhrase appears only in fanartSnapshotBudget.refuse, the check
	// that runs BEFORE the read and takes the size from os.Stat. It is the
	// evidence that an honestly-huge file was refused without being allocated.
	statRefusalPhrase = "bytes on disk"
	// readRefusalPhrase appears only in fanartSnapshotBudget.refuseResult, the
	// TOCTOU backstop that measures the bytes actually returned by the read.
	readRefusalPhrase = "bytes read"
	// bothLossesPhrase is the #3017 wording every cap refusal carries: the slot
	// is missing from the push, not just from the restore snapshot.
	bothLossesPhrase = "nor synced to platforms"
	// readFailurePhrase is what snapshotFanart emits when the READ itself
	// failed, which is a third and separate outcome: no cap was involved.
	readFailurePhrase = "failed to read fanart"
)

// assertStatRefusal fails unless the warning came from the PRE-READ stat check.
//
// Asserting the positive alone is not enough, and that is the whole point of
// this helper: refuseResult refuses the same files a moment later, so a warning
// can satisfy a loose "it was refused" assertion while the pre-read half of the
// design has been deleted. Requiring the read wording to be ABSENT is what
// closes that.
func assertStatRefusal(t *testing.T, warning string) {
	t.Helper()
	if !strings.Contains(warning, statRefusalPhrase) {
		t.Errorf("warning %q does not carry the PRE-READ refusal wording %q; either the stat check did not "+
			"fire (the file was allocated before being refused, which is the thing the stat exists to "+
			"prevent) or the wording drifted", warning, statRefusalPhrase)
	}
	if strings.Contains(warning, readRefusalPhrase) {
		t.Errorf("warning %q carries the POST-READ wording %q, so the bytes were read before being "+
			"refused; the pre-read stat check is not doing its job", warning, readRefusalPhrase)
	}
}

// assertNamesBothLosses pins the #3017 wording: a refused slot is missing from
// the platform push as well as from the restore snapshot, and the operator is
// told both. See snapshotFanart's KNOWN GAP note.
func assertNamesBothLosses(t *testing.T, warning string) {
	t.Helper()
	if !strings.Contains(warning, bothLossesPhrase) {
		t.Errorf("warning %q does not tell the operator the slot was also not synced to platforms; a "+
			"refusal reads as a restore-only problem while a backdrop has actually stopped reaching "+
			"their peers (#3017)", warning)
	}
}

// capturedCount reports how many entries actually hold bytes.
func capturedCount(snapshot []fanartSnapshot) int {
	n := 0
	for _, sf := range snapshot {
		if sf.data != nil {
			n++
		}
	}
	return n
}

// refusedCount reports how many entries are nil-data placeholders, i.e. slots
// this push neither captured nor synced.
//
// It is the UNCAPPED refusal signal, and that is why it exists rather than
// counting warnings. The warning list is bounded by maxFanartSnapshotWarnings
// (#3018 review), so past that bound it stops being a count of anything; the
// snapshot keeps exactly one placeholder per refused slot no matter how many
// there are, because those placeholders are what preserve every later slot's
// true index.
func refusedCount(snapshot []fanartSnapshot) int {
	n := 0
	for _, sf := range snapshot {
		if sf.data == nil {
			n++
		}
	}
	return n
}

// TestSnapshotFanart_FileCountCap_DegradesLoudly plants more backdrops than the
// count cap admits and asserts the overflow is refused loudly rather than read.
func TestSnapshotFanart_FileCountCap_DegradesLoudly(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	dir := t.TempDir()

	// Two past the cap, so the test distinguishes "stops at the cap" from
	// "refuses everything after the first refusal is decided" -- an
	// implementation that broke out of the loop would produce a snapshot short
	// by two entries and assertSnapshotShape catches it.
	const overflow = 2
	total := maxFanartSnapshotFiles + overflow
	paths := make([]string, 0, total)
	for i := 0; i < total; i++ {
		p := filepath.Join(dir, fmt.Sprintf("fanart%d.jpg", i))
		// Tiny: this case must trip the COUNT cap and nothing else, or a passing
		// test would not tell us which bound fired.
		if err := os.WriteFile(p, []byte("backdrop-bytes"), 0o600); err != nil {
			t.Fatalf("writing fixture %s: %v", p, err)
		}
		paths = append(paths, p)
	}

	// PRECONDITION. Without it, a cap raised above the fixture size turns this
	// into a test that asserts nothing while staying green.
	if len(paths) <= maxFanartSnapshotFiles {
		t.Fatalf("precondition failed: %d fixture files does not exceed the %d-file cap, so no refusal "+
			"is owed and the assertions below are vacuous", len(paths), maxFanartSnapshotFiles)
	}

	p := boundTestPublisher()
	snapshot, warnings, err := p.snapshotFanart(context.Background(), paths)

	if err != nil {
		t.Fatalf("snapshotFanart returned %v; a cap refusal is a per-file degrade, not a reason to abort "+
			"the set and refuse the whole push", err)
	}
	assertSnapshotShape(t, snapshot, paths)

	if got := capturedCount(snapshot); got != maxFanartSnapshotFiles {
		t.Errorf("captured %d files, want exactly the cap of %d", got, maxFanartSnapshotFiles)
	}
	// The refused ones are the LAST ones, and they hold nothing. Asserting the
	// count alone would pass for an implementation that refused an arbitrary
	// subset.
	for i := maxFanartSnapshotFiles; i < total; i++ {
		if snapshot[i].data != nil {
			t.Errorf("entry %d past the cap holds %d bytes; it must be a nil-data placeholder",
				i, len(snapshot[i].data))
		}
	}
	if len(warnings) != overflow {
		t.Errorf("got %d warnings for %d refused files, want one each; a refusal the operator is not told "+
			"about is silent data loss, since a peer delete of that file cannot be repaired",
			len(warnings), overflow)
	}
	if joined := strings.Join(warnings, " | "); !strings.Contains(joined, "file snapshot limit") {
		t.Errorf("warnings %q do not name the file-count limit; the operator cannot act on a refusal "+
			"whose cause is not stated", joined)
	}
	for _, w := range warnings {
		assertNamesBothLosses(t, w)
	}
	// EACH REFUSAL NAMES ITS SLOT (#2712 review). Refusals arrive one per file,
	// so a message that omits the index produces byte-identical sentences -- the
	// operator learns that something was refused but not WHICH backdrops are now
	// unrepairable, which is the only fact the warning carries. Asserting the
	// warnings are distinct is what makes that mechanical: an implementation
	// that dropped the index would emit `overflow` copies of one string.
	for i := maxFanartSnapshotFiles; i < total; i++ {
		want := fmt.Sprintf("fanart %d ", i)
		if joined := strings.Join(warnings, " | "); !strings.Contains(joined, want) {
			t.Errorf("no warning names slot %d; warnings were %q", i, joined)
		}
	}
}

// TestSnapshotFanart_PerFileCap_DegradesLoudly plants one oversized backdrop
// among healthy ones.
//
// The neighbors are the point: a per-file refusal must be per-FILE. An
// implementation that aborted the set on the first over-size file would leave
// every later backdrop uncaptured and therefore unrepairable, which is a strictly
// worse outcome than the memory the cap was protecting.
//
// #3017 RETARGETED THIS TEST. The per-file cap no longer refuses before the
// read -- see snapshotFanart's doc comment -- so a file over
// maxFanartSnapshotFileBytes (and under img.MaxDecodeBytes) is now READ and
// CAPTURED here; snapshotFanart's contract stops at the read, and the
// retention drop only happens later, in syncAllFanartToPlatforms, which this
// unit-level test does not call. What this test now proves is the
// PRECONDITION for that later behavior: snapshotFanart hands back the
// over-cap slot WITH its bytes and WITH fanartSnapshot.overRetentionCap()
// true, which is exactly what syncAllFanartToPlatforms needs to see in
// order to push it and then drop it. The end-to-end pushOnly behavior
// (upload happens, retention does not) is covered by
// TestSyncAllFanart_OverPerFileCap_PushedButNotRetained in peer_clobber_test.go,
// which drives the real sync path with a fake peer.
func TestSnapshotFanart_PerFileCap_ReadAndCapturedNotRefused(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	dir := t.TempDir()

	small0 := filepath.Join(dir, "fanart.jpg")
	huge := filepath.Join(dir, "fanart1.jpg")
	small2 := filepath.Join(dir, "fanart2.jpg")
	if err := os.WriteFile(small0, []byte("backdrop-zero"), 0o600); err != nil {
		t.Fatal(err)
	}
	sparseFile(t, huge, maxFanartSnapshotFileBytes+1)
	if err := os.WriteFile(small2, []byte("backdrop-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := []string{small0, huge, small2}

	// PRECONDITION, measured off the filesystem rather than assumed from the
	// truncate call: a fixture that did not actually exceed the cap would make
	// every assertion below pass for the wrong reason. Also under
	// img.MaxDecodeBytes, or the READ itself would refuse it and this test
	// would be measuring the wrong bound entirely (see snapshot_bound_stat's
	// ReadOvershootsTheStat cases for that arm).
	info, statErr := os.Stat(huge)
	if statErr != nil {
		t.Fatalf("stat'ing the oversize fixture: %v", statErr)
	}
	if info.Size() <= maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: the oversize fixture is %d bytes, not over the %d-byte per-file "+
			"cap, so this test proves nothing about it", info.Size(), int64(maxFanartSnapshotFileBytes))
	}
	if info.Size() > img.MaxDecodeBytes {
		t.Fatalf("precondition failed: the fixture is %d bytes, over img.MaxDecodeBytes (%d); the READ "+
			"would refuse it and this test would measure the wrong bound", info.Size(), img.MaxDecodeBytes)
	}

	p := boundTestPublisher()
	snapshot, warnings, err := p.snapshotFanart(context.Background(), paths)

	if err != nil {
		t.Fatalf("snapshotFanart returned %v; an over-retention-cap file is legal to read", err)
	}
	assertSnapshotShape(t, snapshot, paths)

	// THE #3017 ASSERTION: captured, not refused.
	if snapshot[1].data == nil {
		t.Fatal("the over-retention-cap backdrop was refused before the read; #3017 requires it be read and " +
			"pushed, with retention decided only after every peer has had its upload")
	}
	if len(snapshot[1].data) != int(maxFanartSnapshotFileBytes+1) {
		t.Errorf("captured %d bytes, want exactly the fixture's %d", len(snapshot[1].data), maxFanartSnapshotFileBytes+1)
	}
	if !snapshot[1].overRetentionCap() {
		t.Error("overRetentionCap() = false for a slot one byte over the cap; retention cannot be dropped " +
			"later if this bit is wrong")
	}
	// Neighbors are ordinary and must not be flagged.
	if snapshot[0].overRetentionCap() || snapshot[2].overRetentionCap() {
		t.Errorf("a neighbor under the cap reports overRetentionCap()=true (slot0=%t, slot2=%t)",
			snapshot[0].overRetentionCap(), snapshot[2].overRetentionCap())
	}
	// No warning and no cap-refusal accounting: this file was fully captured,
	// so nothing was lost at the snapshot layer for the caller to be told
	// about here.
	if len(warnings) != 0 {
		t.Errorf("got warnings %v for a file that was captured, not refused", warnings)
	}
}

// TestSnapshotFanart_TotalBytesCap_DegradesLoudly is the cap that actually
// protects the process: the per-file and per-count caps multiply out to over a
// gigabyte, which is not a bound worth having on its own.
//
// The fixture is deliberately built so NO single file trips the per-file cap and
// the count is far under its cap, so a green result can only come from the total
// having been enforced.
func TestSnapshotFanart_TotalBytesCap_DegradesLoudly(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	dir := t.TempDir()

	// Each file sits EXACTLY AT the per-file cap, which both size checks admit
	// because they refuse on a strict `>` rather than `>=` (#3018 review; the
	// comment used to say "just under", which misdescribes the boundary anyone
	// later reasoning about it would rely on). At the cap is the largest a file
	// can be without tripping the per-file bound, so it is also the fewest
	// fixtures needed to cross the TOTAL -- which is the bound this test is
	// about, and the per-file precondition below pins that separation.
	// Sparse, so this costs no real disk.
	const each = int64(maxFanartSnapshotFileBytes)
	count := int(maxFanartSnapshotTotalBytes/each) + 1
	paths := make([]string, 0, count)
	for i := 0; i < count; i++ {
		p := filepath.Join(dir, fmt.Sprintf("fanart%d.jpg", i))
		sparseFile(t, p, each)
		paths = append(paths, p)
	}

	// PRECONDITIONS, all three, because this fixture can go wrong in three ways
	// and each one produces a vacuous pass.
	if int64(count)*each <= maxFanartSnapshotTotalBytes {
		t.Fatalf("precondition failed: %d files of %d bytes is %d, not over the %d-byte total cap",
			count, each, int64(count)*each, int64(maxFanartSnapshotTotalBytes))
	}
	if each > maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: each fixture is %d bytes, over the %d-byte PER-FILE cap, so this "+
			"test would be measuring the wrong bound", each, int64(maxFanartSnapshotFileBytes))
	}
	if count > maxFanartSnapshotFiles {
		t.Fatalf("precondition failed: %d fixtures exceeds the %d-file COUNT cap, so this test would be "+
			"measuring the wrong bound", count, maxFanartSnapshotFiles)
	}

	p := boundTestPublisher()
	snapshot, warnings, err := p.snapshotFanart(context.Background(), paths)

	if err != nil {
		t.Fatalf("snapshotFanart returned %v; the total cap is a degrade, not an abort", err)
	}
	assertSnapshotShape(t, snapshot, paths)

	captured := capturedCount(snapshot)
	if captured == count {
		t.Fatal("every file was captured; the total-bytes cap did not fire at all")
	}
	if captured == 0 {
		t.Fatal("nothing was captured; the total cap refused even the first file, which leaves the whole " +
			"set unrepairable and would also stop the push (hasReadableFanart is false)")
	}
	// EXACTLY the number the cap admits, not merely "some" (#2712 review, N5).
	// Every fixture is the same size and divides the total evenly, so the
	// admitted count is arithmetic rather than an estimate, and pinning it is
	// what makes the boundary direction testable: relaxing the pre-read
	// comparison from `>` to `>=` refuses the file that lands exactly ON the
	// total and this number drops by one. A "captured is between 1 and count"
	// assertion cannot see that.
	wantCaptured := int(maxFanartSnapshotTotalBytes / each)
	if captured != wantCaptured {
		t.Errorf("captured %d files holding %d bytes each, want exactly %d: the total cap admits the file "+
			"that lands ON the limit and refuses only the one that would cross it",
			captured, each, wantCaptured)
	}
	// The bytes actually held are inside the cap. This is the assertion the
	// whole test exists for: a count of refusals proves nothing about memory.
	var held int64
	for _, sf := range snapshot {
		held += int64(len(sf.data))
	}
	if held > maxFanartSnapshotTotalBytes {
		t.Errorf("the snapshot holds %d bytes, over the %d-byte cap", held, int64(maxFanartSnapshotTotalBytes))
	}
	if joined := strings.Join(warnings, " | "); !strings.Contains(joined, "total snapshot limit") {
		t.Errorf("warnings %q do not name the total-bytes limit", joined)
	}
	for _, w := range warnings {
		assertNamesBothLosses(t, w)
	}
	// The TOTAL cap's pre-read half, discriminated the same way the per-file
	// one is (#2712 review, N5). Every fixture here is sparse and honestly
	// sized, so a stat cannot under-report and the refusals must all come from
	// the pre-read check. If the post-read wording shows up, the pre-read total
	// accounting was dropped and each file was read before being refused --
	// exactly the mutation (`b.bytes+size` -> `b.bytes`) that used to survive.
	for _, w := range warnings {
		if strings.Contains(w, readRefusalPhrase) {
			t.Errorf("warning %q came from the POST-read check; with honestly-sized files the pre-read "+
				"total accounting should have refused this slot before any bytes were allocated", w)
		}
	}
}

// TestSnapshotFanart_UnderTheCaps_CapturesEverything is the over-suppression
// guard, and it is the one that fails if a cap is set to a value a real library
// trips.
//
// 42 is not an arbitrary number: it is the measured size of the largest artist in
// the production library that surfaced #2712. A cap tuned below reality would
// silently stop repairing peer clobbers for exactly the artist most likely to
// suffer one, and every test above would still pass.
func TestSnapshotFanart_UnderTheCaps_CapturesEverything(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	dir := t.TempDir()

	const productionWorstCase = 42
	paths := make([]string, 0, productionWorstCase)
	for i := 0; i < productionWorstCase; i++ {
		p := filepath.Join(dir, fmt.Sprintf("fanart%d.jpg", i))
		// 4 MiB each: comfortably above a real 4K JPEG backdrop (1 to 6 MB), so
		// the fixture is a pessimistic stand-in for the real library rather than
		// a token one.
		sparseFile(t, p, 4<<20)
		paths = append(paths, p)
	}

	p := boundTestPublisher()
	snapshot, warnings, err := p.snapshotFanart(context.Background(), paths)

	if err != nil {
		t.Fatalf("snapshotFanart returned %v for an ordinary large artist", err)
	}
	assertSnapshotShape(t, snapshot, paths)
	if got := capturedCount(snapshot); got != productionWorstCase {
		t.Errorf("captured %d of %d backdrops for the measured production worst case; the caps are tuned "+
			"below reality, so this artist's backdrops are now unrepairable after a peer clobber",
			got, productionWorstCase)
	}
	if len(warnings) != 0 {
		t.Errorf("got warnings %v for a set that is inside every cap", warnings)
	}
}

// TestSnapshotFanart_Refusal_IsLoggedAtError pins the third of the degrade's
// "three simultaneous signals" (#2712 review, N2).
//
// The other two -- the operator warning and the nil-data entry -- are asserted
// by every test above. The Error log was asserted nowhere, because every test in
// this file builds its publisher with silentLogger(), so deleting the whole
// p.logger.Error call in degradeFanartSlot left the suite green.
//
// That signal is not decoration. The warning reaches whoever is watching the
// request; the log is what a scheduled or background push leaves behind, and it
// is the only record naming the FILE. Losing it makes a background push drop
// backdrops with nothing on disk to say which ones.
func TestSnapshotFanart_Refusal_IsLoggedAtError(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	dir := t.TempDir()

	small := filepath.Join(dir, "fanart.jpg")
	if err := os.WriteFile(small, []byte("an ordinary backdrop"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	// #3017: the per-file cap no longer refuses anything by itself, so this
	// fixture is sized past the TOTAL-bytes cap instead -- still a pre-read
	// refuse() refusal, which is what degradeFanartSlot's Error log is about.
	huge := filepath.Join(dir, "fanart1.jpg")
	sparseFile(t, huge, maxFanartSnapshotTotalBytes+1)

	// PRECONDITION, measured off the filesystem: a fixture inside the cap owes
	// no refusal, so there would be no log line to look for and the assertions
	// would pass only because nothing was expected.
	info, statErr := os.Stat(huge)
	if statErr != nil {
		t.Fatalf("stat'ing the oversize fixture: %v", statErr)
	}
	if info.Size() <= maxFanartSnapshotTotalBytes {
		t.Fatalf("precondition failed: the fixture is %d bytes, inside the %d-byte total cap",
			info.Size(), int64(maxFanartSnapshotTotalBytes))
	}

	var logs bytes.Buffer
	p := boundTestPublisherLogging(&logs)
	paths := []string{small, huge}
	snapshot, warnings, err := p.snapshotFanart(context.Background(), paths)
	if err != nil {
		t.Fatalf("snapshotFanart returned %v", err)
	}
	// PRECONDITION on the run itself: if nothing was refused, the log below is
	// legitimately empty and this test would be asserting against a case it
	// never produced.
	if snapshot[1].data != nil || len(warnings) != 1 {
		t.Fatalf("precondition failed: the oversize fixture was not refused (captured=%t, warnings=%v), "+
			"so no log line is owed", snapshot[1].data != nil, warnings)
	}

	got := logs.String()
	// LEVEL. The captured logger admits Debug and above, so a degrade demoted
	// to Warn or Info would still appear in the buffer; only the level check
	// catches that, and the level is what decides whether a production
	// deployment logging at Warn ever sees this at all.
	if !strings.Contains(got, "level=ERROR") {
		t.Errorf("the refusal was not logged at ERROR; a demoted level puts silent data loss back out of "+
			"sight for any deployment that does not log every level. Log was:\n%s", got)
	}
	// THE FILE. This is the field the warning deliberately omits, so the log is
	// the only place an operator can learn WHICH backdrop went missing.
	if !strings.Contains(got, huge) {
		t.Errorf("the refusal log does not name the refused file %q, which is the one thing it carries "+
			"that the operator warning does not. Log was:\n%s", huge, got)
	}
	// THE SLOT and THE REASON, so the log line stands on its own rather than
	// needing to be correlated with the API response to be actionable.
	if !strings.Contains(got, "index=1") {
		t.Errorf("the refusal log does not name the slot. Log was:\n%s", got)
	}
	if !strings.Contains(got, statRefusalPhrase) {
		t.Errorf("the refusal log does not carry the reason. Log was:\n%s", got)
	}
	// The log says the slot is missing from the PUSH too, not only from the
	// restore snapshot (#3017), for the same reason the warning does.
	if !strings.Contains(got, "NOT synced to platforms") {
		t.Errorf("the refusal log describes only the restore snapshot; a reader will not learn the "+
			"backdrop also stopped reaching their peers. Log was:\n%s", got)
	}
}

// TestFanartSnapshotBudget_RefuseResult_BoundsWhatWasACTUALLYRead covers the
// TOCTOU half of the TOTAL-bytes cap (#2712 review).
//
// WHY A SECOND CHECK EXISTS AT ALL. The pre-read refusal is a stat, and a stat
// is a PREDICTION: the file can grow between the stat and the read, so on its
// own the cap bounds a NUMBER while the read stays unbounded. internal/image's
// readio.go makes exactly this argument for preferring io.LimitReader to a
// stat, and it bites harder here because snapshotFanart holds up to
// maxFanartSnapshotFiles results at once -- believing every stat would allow
// 100 reads of img.MaxDecodeBytes (25 MB) to sit resident against a documented
// 192 MiB bound, roughly 2.5 GB. refuseResult re-applies the TOTAL cap to the
// length actually read, and the caller DISCARDS what it refuses.
//
// #3017: the per-file cap is no longer one of the two caps refuseResult
// re-applies -- it used to be, alongside the total, but the per-file bound is
// now RETENTION-only (fanartSnapshot.overRetentionCap, consulted after the
// upload loop) rather than a reason to refuse the read result. A file whose
// read overshoots the per-file cap but stays under the total is therefore
// captured here, not refused; the case below that used to prove the opposite
// now proves that directly.
//
// This drives the predicate directly, at both boundaries in both directions.
// It covers the DECISION only. The WIRING -- that snapshotFanart actually calls
// refuseResult and discards the bytes it refuses -- cannot be reached from a
// portable fixture, because it needs a file whose stat genuinely under-reports
// what the read then delivers, and the only way to build one is a platform
// primitive (a FIFO, which stats at zero bytes). That wiring is covered
// separately, in snapshot_bound_stat_unix_test.go. A predicate nobody calls is
// not a fix, so both halves are needed.
func TestFanartSnapshotBudget_RefuseResult_BoundsWhatWasACTUALLYRead(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	cases := []struct {
		name        string
		alreadyHeld int64
		read        int64
		wantRefused bool
	}{
		{
			name: "an ordinary backdrop is captured",
			read: 4 << 20,
		},
		{
			// #3017: over the per-file RETENTION cap but under the total is
			// captured, not refused -- refuseResult no longer consults the
			// per-file bound at all.
			name: "over the per-file cap but under the total is still captured",
			read: maxFanartSnapshotFileBytes + 1,
		},
		{
			name:        "a read that lands exactly on the total is captured",
			alreadyHeld: maxFanartSnapshotTotalBytes - (4 << 20),
			read:        4 << 20,
		},
		{
			name:        "a read that overshoots the total by one byte is refused",
			alreadyHeld: maxFanartSnapshotTotalBytes - (4 << 20),
			read:        (4 << 20) + 1,
			wantRefused: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fanartSnapshotBudget{files: 1, bytes: tc.alreadyHeld}
			reason, refused := b.refuseResult(tc.read, 7)
			if refused != tc.wantRefused {
				t.Fatalf("refuseResult(%d bytes read, %d already held) = %v, want %v",
					tc.read, tc.alreadyHeld, refused, tc.wantRefused)
			}
			if !refused {
				if reason != "" {
					t.Errorf("a captured slot carries the reason %q; it must be empty", reason)
				}
				return
			}
			// The warning must name the SLOT. An artist over the caps produces
			// one of these per refused backdrop, and byte-identical sentences
			// leave the operator unable to tell which backdrops are now
			// unrepairable, which is the one fact the warning exists to carry.
			if !strings.Contains(reason, "fanart 7") {
				t.Errorf("refusal reason %q does not name the slot", reason)
			}
		})
	}
}

// TestSnapshotFanart_RefusedSlots_DoNotConsumeTheCountBudget pins the design
// claim in fanartSnapshotBudget's doc comment: the budget counts only files
// that were actually CAPTURED, so a refused slot holds no bytes and must not
// spend any of the count cap either.
//
// That claim had no test, and it is the difference between a cap that bounds
// memory and a cap that loses backdrops. A directory whose leading files are
// junk (an operator's oversized source images, say) would otherwise spend the
// whole count budget on files nothing is holding, and the REAL backdrops
// sitting behind them would be starved out of their capture -- which, because
// the repair loop skips every nil-data entry, means a peer delete of one of
// those real backdrops during that push cannot be undone.
//
// The mutation this exists to kill is a single line: increment budget.files on
// the pre-read refusal path in snapshotFanart. The whole suite stays green
// without this test, because the two existing cap tests cannot reach the
// interaction -- the count-cap test uses uniformly tiny files, so nothing is
// refused before the cap is hit, and the per-file test has only three files.
//
// The fixture is what makes this discriminating, so its shape is asserted
// rather than assumed: enough junk that junk + real CROSSES the count cap,
// while the real backdrops ALONE stay comfortably under it. Correct behavior
// therefore captures every real backdrop; charging refused slots captures only
// some of them.
func TestSnapshotFanart_RefusedSlots_DoNotConsumeTheCountBudget(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	dir := t.TempDir()

	const junkCount = 60
	const realCount = 50

	paths := make([]string, 0, junkCount+realCount)
	// The junk comes FIRST, which is the only ordering that tests anything: the
	// budget is spent in list order, so junk behind the real files would be
	// refused after every real file was already captured and the mutation would
	// be invisible.
	for i := 0; i < junkCount; i++ {
		p := filepath.Join(dir, fmt.Sprintf("junk%02d.jpg", i))
		// #3017: the per-file cap no longer refuses anything on its own, so
		// each junk file here is sized past the TOTAL-bytes cap BY ITSELF --
		// one byte over maxFanartSnapshotTotalBytes -- which the pre-read
		// stat still refuses on the very first file (b.bytes starts at 0).
		// Sparse, so 60 of them cost no disk.
		sparseFile(t, p, maxFanartSnapshotTotalBytes+1)
		paths = append(paths, p)
	}
	realPaths := make([]string, 0, realCount)
	for i := 0; i < realCount; i++ {
		p := filepath.Join(dir, fmt.Sprintf("fanart%02d.jpg", i))
		// Tiny, so nothing here can trip the per-file or total-bytes caps and a
		// failure can only mean the COUNT budget was spent on the junk.
		if err := os.WriteFile(p, []byte("backdrop-bytes"), 0o600); err != nil {
			t.Fatalf("writing fixture %s: %v", p, err)
		}
		paths = append(paths, p)
		realPaths = append(realPaths, p)
	}

	// PRECONDITIONS. Both halves are needed and either one alone passes
	// vacuously: without the first, no count budget is at stake at all and the
	// mutation has nothing to spend; without the second, the real backdrops
	// would be partly refused even under correct behavior and the assertion
	// below would be measuring the cap rather than the invariant.
	if len(paths) <= maxFanartSnapshotFiles {
		t.Fatalf("precondition failed: %d fixture slots does not exceed the %d-file cap, so charging "+
			"refused slots would cost nothing and this test asserts nothing", len(paths), maxFanartSnapshotFiles)
	}
	if realCount > maxFanartSnapshotFiles {
		t.Fatalf("precondition failed: %d real backdrops already exceeds the %d-file cap on their own, "+
			"so some would be refused for a legitimate reason", realCount, maxFanartSnapshotFiles)
	}

	p := boundTestPublisher()
	snapshot, warnings, err := p.snapshotFanart(context.Background(), paths)

	if err != nil {
		t.Fatalf("snapshotFanart returned %v; a cap refusal is a per-file degrade, not a reason to abort "+
			"the set", err)
	}
	assertSnapshotShape(t, snapshot, paths)

	// PRECONDITION, measured on the RESULT rather than assumed from the fixture
	// sizes: the junk slots really were refused, and refused by the pre-read
	// stat. If a future change let them through, they would be captured rather
	// than starving anything, and the assertions below would pass for a reason
	// that has nothing to do with the invariant.
	//
	// Deliberately a LOWER bound on the REFUSAL count rather than an equality.
	// The excess is exactly what the mutation produces (a starved real backdrop
	// is refused too), so pinning equality here would make the mutation fail at
	// a precondition whose message blames the fixture. The equality belongs
	// below, where it can say what actually went wrong.
	//
	// Measured from the SNAPSHOT, not from the warning list. The junk count
	// (60) is deliberately past maxFanartSnapshotWarnings (25), so the warning
	// list is capped and no longer counts refusals -- see the warning-cap
	// assertions below. The snapshot keeps one nil-data entry per refused slot
	// regardless of the cap, which makes it both the uncapped signal and the
	// one this test's invariant is actually about.
	if refused := refusedCount(snapshot); refused < junkCount {
		t.Fatalf("got %d refused slots for %d junk files, want at least one each; the junk was not refused "+
			"as expected, so nothing below measures what it claims to", refused, junkCount)
	}
	for i := 0; i < junkCount; i++ {
		if snapshot[i].data != nil {
			t.Fatalf("junk slot %d holds %d bytes, so it was captured rather than refused; the fixture "+
				"is not exercising the refusal path", i, len(snapshot[i].data))
		}
	}
	// PRECONDITION for the loop below, which indexes warnings[0:cap] directly.
	// Without it, a regression that stopped collecting warnings at all fails
	// here as an index-out-of-range PANIC rather than as an assertion naming
	// what broke -- and a panic reports the test as failing for a reason that
	// looks like a test bug (#3018 review). The equality is asserted further
	// down, where it can explain the bound; this only has to make the indexing
	// safe and say why the entries are owed.
	if len(warnings) < maxFanartSnapshotWarnings {
		t.Fatalf("got %d warnings for %d refused slots, want at least the %d the cap admits; warnings are "+
			"no longer being collected, so the per-slot assertions below would measure nothing",
			len(warnings), junkCount, maxFanartSnapshotWarnings)
	}
	// The pre-read stat is what refused the junk, not the post-read length: it
	// must never have been allocated. Asserted over the warnings that survived
	// the cap, which arrive in slot order and so are all junk.
	for i := 0; i < maxFanartSnapshotWarnings; i++ {
		assertStatRefusal(t, warnings[i])
		assertNamesBothLosses(t, warnings[i])
	}

	// THE INVARIANT. Every real backdrop behind the junk is still captured,
	// because the refused slots spent no count budget. Under the mutation the
	// junk spends 60 of the 100 and only the first 40 real backdrops survive.
	for i, rp := range realPaths {
		slot := junkCount + i
		if snapshot[slot].data == nil {
			t.Errorf("real backdrop %q at slot %d was not captured; a refused slot has spent count budget "+
				"it holds no bytes for, so junk in front of the real backdrops starves them out of their "+
				"capture and a peer delete of one cannot be repaired", rp, slot)
		}
	}
	// The count as well as the per-slot check: a mutation that captured the
	// right NUMBER of files but the wrong ones is caught by the loop above, and
	// one that captured junk as well as real files is caught by this.
	if got := capturedCount(snapshot); got != realCount {
		t.Errorf("captured %d files, want exactly the %d real backdrops", got, realCount)
	}
	// And the refusal count exactly matches the junk, now that the junk has
	// been shown to account for at least that many. A surplus refusal means a
	// real backdrop was refused as well, which is the same defect the loop
	// above reports and is worth naming separately because it is the operator's
	// view of it.
	if refused := refusedCount(snapshot); refused != junkCount {
		t.Errorf("got %d refused slots for %d junk files; the surplus is a REAL backdrop refused as well, "+
			"so a healthy backdrop could not be captured", refused, junkCount)
	}

	// THE WARNING LIST IS BOUNDED, and this fixture is the one place in the
	// suite where that is visible: 60 refusals is the only case here that
	// exceeds maxFanartSnapshotWarnings. Without these assertions the cap could
	// be deleted and the whole file would stay green, since every other test
	// refuses fewer slots than the cap allows.
	wantWarnings := maxFanartSnapshotWarnings + 1 // the kept slots plus the overflow summary
	if len(warnings) != wantWarnings {
		t.Errorf("got %d warnings for %d refusals, want %d (the first %d slots plus one overflow summary); "+
			"an unbounded list puts a directory's worth of strings in the API response, which is the same "+
			"defect the byte caps close, arriving through the response body: %v",
			len(warnings), junkCount, wantWarnings, maxFanartSnapshotWarnings, warnings)
	}
	// The overflow is COUNTED, not silently dropped. A cut that just stopped
	// appending would report a plausible-looking list understating the loss,
	// which is the invisible-data-loss failure the loud degrade exists to
	// prevent.
	overflow := junkCount - maxFanartSnapshotWarnings
	last := warnings[len(warnings)-1]
	if want := fmt.Sprintf("and %d more", overflow); !strings.Contains(last, want) {
		t.Errorf("final warning %q does not say %q; the operator must be told how many refusals were "+
			"withheld, or a truncated list reads as the complete one", last, want)
	}
}

// TestFanartWarningLog_UnderTheCap_AddsNoSummary pins the other side of the
// bound: a set that refuses fewer slots than the cap allows must be reported
// exactly as it was before the cap existed.
//
// It is separate from the over-cap test above because the failure it catches is
// the opposite mutation -- an off-by-one that appends an "and 0 more" line, or
// a cap applied to a list that never needed one -- and that would be invisible
// in a fixture built to overflow.
func TestFanartWarningLog_UnderTheCap_AddsNoSummary(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	var log fanartWarningLog

	const under = 3
	if under >= maxFanartSnapshotWarnings {
		t.Fatalf("precondition failed: %d warnings is not under the %d cap, so this test would measure "+
			"the overflow path it exists to exclude", under, maxFanartSnapshotWarnings)
	}
	for i := 0; i < under; i++ {
		log.add(fmt.Sprintf("fanart %d refused", i))
	}

	got := log.result()
	if len(got) != under {
		t.Fatalf("got %d warnings for %d added, want them all through untouched: %v", len(got), under, got)
	}
	for _, w := range got {
		if strings.Contains(w, "and ") && strings.Contains(w, " more") {
			t.Errorf("warning %q is an overflow summary, but nothing overflowed", w)
		}
	}
}

// TestFanartWarningLog_Result_DoesNotAliasTheLog pins that the returned slice
// owns its own memory (#3018 review).
//
// The hazard is specific and it is not defensive style. kept reaches the cap by
// repeated append, so it arrives at result() with SPARE CAPACITY (len 25, cap
// 32 as the growth lands). append(w.kept, summary) therefore writes the summary
// into kept's existing backing array rather than a fresh one, and the returned
// slice aliases the log. A SECOND result() call writes to that same index, so
// it mutates a slice the first caller is already holding.
//
// It lands on the single line whose whole job is to stop a truncated list
// reading as a complete one, so the aliasing restores exactly the invisible
// data loss the counted overflow exists to prevent.
//
// Note the mutation is driven by a second result() rather than a later add():
// once the cap is reached add() only increments overflow and never appends
// again, so an add-driven version of this test would wire a clobber the code
// cannot perform and pass against the aliasing implementation.
func TestFanartWarningLog_Result_DoesNotAliasTheLog(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	var log fanartWarningLog

	// The spare capacity is ESTABLISHED here rather than inherited from append's
	// growth (#3018 review). Left to itself, add() grows kept to cap 32 for len
	// 25 and the alias forms because of Go's growth heuristic -- which is a
	// runtime implementation detail, not a promise. A future heuristic landing
	// len == cap at the boundary would make this test fail its own precondition
	// while the production code was still correct, and a test that fails for a
	// reason unrelated to the code under test is worse than no test.
	//
	// Seeding the capacity makes the hazard reachable by construction. It stays
	// faithful to production, where kept likewise arrives at result() with room
	// to spare; it just no longer DEPENDS on that being true.
	log.kept = make([]string, 0, maxFanartSnapshotWarnings+1)

	// Past the cap, so there IS an overflow summary to clobber. Under the cap
	// the append never happens and this test would assert nothing.
	const refusals = maxFanartSnapshotWarnings + 35
	for i := 0; i < refusals; i++ {
		log.add(fmt.Sprintf("fanart %d refused", i))
	}

	// PRECONDITION, now guaranteed by the seed above rather than hoped for.
	// Kept because it is what makes the mechanism explicit: with len == cap the
	// append would allocate a fresh array, the alias would not form, and a green
	// result would say nothing about the version that does alias. It also fails
	// loudly if someone removes the seed.
	if len(log.kept) == cap(log.kept) {
		t.Fatalf("precondition failed: kept has len %d == cap %d, so an append would reallocate and the "+
			"aliasing this test exists to catch cannot occur", len(log.kept), cap(log.kept))
	}

	first := log.result()
	summary := first[len(first)-1]
	if !strings.Contains(summary, "and 35 more") {
		t.Fatalf("precondition failed: last entry %q is not the overflow summary, so the assertion below "+
			"would not be measuring the line that matters", summary)
	}

	// THE MUTATION THE FIX PREVENTS. A second call with a different overflow
	// count: against an aliasing result() its append lands on the same index of
	// the same array and rewrites the summary inside `first`.
	log.overflow += 64
	second := log.result()

	if after := first[len(first)-1]; after != summary {
		t.Errorf("the overflow summary in an already-returned slice changed from %q to %q; result() aliased "+
			"the log's backing array, so a second call rewrote a line the first caller was already "+
			"holding -- and that line is the only thing telling the operator warnings were withheld, so "+
			"a truncated list reads as the complete one", summary, after)
	}
	// The two results must also be independent slices, which is the property
	// the assertion above infers. Checking it directly means a future change
	// that copies only sometimes cannot pass on the strength of one lucky path.
	if &first[0] == &second[0] {
		t.Errorf("two result() calls returned slices over the same backing array; each caller must own " +
			"its own copy or one caller's list mutates under another")
	}
}

// TestFanartWarningLog_Empty_ReturnsNil keeps the no-warnings case byte-identical
// to the pre-cap behavior. A healthy push returns no warnings at all, and an
// empty non-nil slice is a different value for any caller that checks nilness:
// these warnings are marshaled into an API response, where the difference is
// "warnings": null versus "warnings": [].
//
// BOTH SHAPES OF EMPTY ARE TESTED, and the second is the one that matters
// (#3018 review). Asserting only the zero-value log passes for the wrong
// reason: it returns nil because kept ITSELF is nil, so the test holds even
// when result() enforces nothing. A log whose kept was preallocated and never
// added to is empty just the same, and before this round it returned a non-nil
// empty slice -- an invariant the doc comment claimed and the code did not
// enforce. TestFanartWarningLog_Result_DoesNotAliasTheLog preallocates exactly
// that way, so the case is not hypothetical.
func TestFanartWarningLog_Empty_ReturnsNil(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	t.Run("zero value", func(t *testing.T) {
		var log fanartWarningLog
		if got := log.result(); got != nil {
			t.Errorf("empty log returned %#v, want nil; a healthy push must report no warnings at all", got)
		}
	})

	t.Run("preallocated but never added to", func(t *testing.T) {
		var log fanartWarningLog
		log.kept = make([]string, 0, maxFanartSnapshotWarnings)

		// PRECONDITION. Without it the case collapses into the one above and
		// the assertion stops distinguishing "result() returns nil" from
		// "kept happened to be nil".
		if log.kept == nil {
			t.Fatal("precondition failed: kept is nil, so this case is the zero-value one and asserts nothing new")
		}

		if got := log.result(); got != nil {
			t.Errorf("a preallocated but empty log returned %#v (len %d), want nil; the nil contract must be "+
				"enforced by result() rather than inherited from kept, or a caller that preallocates emits "+
				"\"warnings\": [] where every other healthy push emits \"warnings\": null", got, len(got))
		}
	})
}

// TestSnapshotFanart_PreReadStat_IsCancellable pins that the pre-read size
// check goes through a CONTEXT-BOUND stat (#3018 review).
//
// The bound matters because of the ORDERING the cap requires. The stat runs
// before the read on purpose -- that is what keeps an honestly-huge file from
// being allocated at all -- so a raw os.Stat there put an unbounded call in
// FRONT of the bounded read, and a hard-mounted export that stopped answering
// wedged this loop one step before the #2934 bound could apply. snapshotFanart
// reads the whole set before the first upload, so that wedge takes the entire
// push with it and no caller deadline reaches it.
//
// THE ASSERTION HAS TO SEPARATE THE STAT FROM THE READ, which is harder than it
// looks and is why this test asserts on an OVER-SIZE fixture. Every obvious
// assertion passes against a raw os.Stat:
//
//   - "snapshotFanart returns context.Canceled" passes, because with a raw stat
//     the stat merely succeeds and the BOUNDED READ after it reports the
//     cancellation, so the loop aborts either way.
//   - "refuse() does not refuse" passes, because a raw stat on a healthy file
//     also declines to refuse.
//
// The one behavior a raw stat CANNOT produce: on a file that is genuinely over
// the total-bytes cap, a bound stat fails with the cancellation and refuse()
// falls through to "not refused" (a canceled request is not an over-size
// backdrop), whereas a raw stat succeeds, sees the real size, and REFUSES.
// Those outcomes are opposite, so the assertion discriminates.
//
// #3017: this fixture used to be sized past the PER-FILE cap. That cap no
// longer refuses before the read (see refuse's doc comment), so the fixture
// is now sized past the TOTAL cap instead -- the one pre-read check
// remaining in refuse -- which still exercises exactly the ctx-bound stat
// this test is about.
func TestSnapshotFanart_PreReadStat_IsCancellable(t *testing.T) {
	// No t.Parallel; see the note at the top of this file.
	dir := t.TempDir()
	oversize := filepath.Join(dir, "fanart.jpg")
	// One byte past the total-bytes cap, so an UNBOUND stat would refuse it
	// (budget starts empty, so b.bytes+size > total on this single file).
	// Sparse, so it costs no disk.
	sparseFile(t, oversize, maxFanartSnapshotTotalBytes+1)

	// PRECONDITION, measured rather than assumed: the fixture really is over
	// the cap, so a refusal below is attributable to the stat having succeeded.
	info, statErr := os.Stat(oversize)
	if statErr != nil {
		t.Fatalf("stating fixture: %v", statErr)
	}
	if info.Size() <= maxFanartSnapshotTotalBytes {
		t.Fatalf("precondition failed: fixture is %d bytes, not over the %d-byte total cap, so an "+
			"unbound stat would not refuse it and this test could not tell the two apart",
			info.Size(), int64(maxFanartSnapshotTotalBytes))
	}

	// PRECONDITION: uncanceled, this fixture IS refused. Without it, a mutation
	// that broke the size check entirely would make the assertion below pass
	// for the wrong reason.
	var control fanartSnapshotBudget
	if _, refused := control.refuse(context.Background(), oversize, 0); !refused {
		t.Fatal("precondition failed: the over-size fixture was not refused under an uncanceled context, " +
			"so the per-file check is not firing and the cancellation assertion below proves nothing")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var budget fanartSnapshotBudget
	if _, refused := budget.refuse(ctx, oversize, 0); refused {
		t.Error("a canceled context still produced a SIZE REFUSAL on an over-size file, so the stat ran " +
			"unbound: it consulted the filesystem for a request the caller had already abandoned. That " +
			"is the wedge this bound exists to prevent -- on a dead mount the stat hangs with no deadline " +
			"reaching it -- and it also relabels a cancellation as an over-size backdrop, sending the " +
			"operator to look at their file sizes")
	}

	// The whole-loop consequence. This one passes against an unbound stat too
	// (the bounded read reports the cancellation), so it is here to pin the
	// caller-visible contract, NOT to discriminate: the assertion above does
	// that.
	p := boundTestPublisher()
	if _, _, err := p.snapshotFanart(ctx, []string{oversize}); !errors.Is(err, context.Canceled) {
		t.Errorf("snapshotFanart returned %v for a canceled context, want context.Canceled; an abandoned "+
			"request must not walk the fanart set", err)
	}
}
