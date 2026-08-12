package publish

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		t.Fatalf("sizing sparse fixture %s to %d: %v", path, size, err)
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

// The two halves of the per-file cap produce warnings that differ in one
// phrase, and nothing in the suite used to assert the difference (#2712 review,
// N1/B2). These constants name the phrases once so a wording change breaks in a
// single place rather than in four assertions with four opinions about it.
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
func TestSnapshotFanart_PerFileCap_DegradesLoudly(t *testing.T) {
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
	// every assertion below pass for the wrong reason.
	info, statErr := os.Stat(huge)
	if statErr != nil {
		t.Fatalf("stat'ing the oversize fixture: %v", statErr)
	}
	if info.Size() <= maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: the oversize fixture is %d bytes, not over the %d-byte per-file "+
			"cap, so no refusal is owed", info.Size(), int64(maxFanartSnapshotFileBytes))
	}

	p := boundTestPublisher()
	snapshot, warnings, err := p.snapshotFanart(context.Background(), paths)

	if err != nil {
		t.Fatalf("snapshotFanart returned %v; one over-size file says nothing about the rest of the set", err)
	}
	assertSnapshotShape(t, snapshot, paths)

	if snapshot[1].data != nil {
		t.Errorf("the over-size backdrop was captured anyway (%d bytes held); the cap is not enforced",
			len(snapshot[1].data))
	}
	// Both NEIGHBORS survived. This is what proves the refusal did not abort
	// the loop, and it is the assertion that fails if a future change turns the
	// per-file cap into a set-wide one.
	if snapshot[0].data == nil || snapshot[2].data == nil {
		t.Errorf("a healthy backdrop beside the over-size one was not captured (slot 0 captured=%t, "+
			"slot 2 captured=%t); a per-file refusal must not stop the set, or the surviving backdrops "+
			"become unrepairable too", snapshot[0].data != nil, snapshot[2].data != nil)
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want exactly 1 for the single refused file: %v", len(warnings), warnings)
	}
	// WHICH check refused it, not merely that something did (#2712 review, N1).
	// The pre-read stat and the post-read length both refuse an over-size file
	// and their messages differ in exactly one phrase, so this is the only
	// assertion that can tell them apart. Asserting the shared half alone let
	// three mutations that deleted the pre-read half entirely go unnoticed, all
	// of them still green because refuseResult caught the same files afterwards.
	assertStatRefusal(t, warnings[0])
	// The operator is told the slot is missing from the PUSH too, not only from
	// the restore snapshot (#3017). A message naming just the snapshot sends
	// them looking for a repair problem when a backdrop has actually stopped
	// reaching their peers.
	assertNamesBothLosses(t, warnings[0])
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

	// Each file is just under the per-file cap, so it takes several to cross the
	// total. Sparse, so this costs no real disk.
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
	huge := filepath.Join(dir, "fanart1.jpg")
	sparseFile(t, huge, maxFanartSnapshotFileBytes+1)

	// PRECONDITION, measured off the filesystem: a fixture inside the cap owes
	// no refusal, so there would be no log line to look for and the assertions
	// would pass only because nothing was expected.
	info, statErr := os.Stat(huge)
	if statErr != nil {
		t.Fatalf("stat'ing the oversize fixture: %v", statErr)
	}
	if info.Size() <= maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: the fixture is %d bytes, inside the %d-byte per-file cap",
			info.Size(), int64(maxFanartSnapshotFileBytes))
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
// TOCTOU half of the cap (#2712 review).
//
// WHY A SECOND CHECK EXISTS AT ALL. The pre-read refusal is a stat, and a stat
// is a PREDICTION: the file can grow between the stat and the read, so on its
// own the cap bounds a NUMBER while the read stays unbounded. internal/image's
// readio.go makes exactly this argument for preferring io.LimitReader to a
// stat, and it bites harder here because snapshotFanart holds up to
// maxFanartSnapshotFiles results at once -- believing every stat would allow
// 100 reads of img.MaxDecodeBytes (25 MB) to sit resident against a documented
// 192 MiB bound, roughly 2.5 GB. refuseResult re-applies both size caps to the
// length actually read, and the caller DISCARDS what it refuses.
//
// This drives the predicate directly, at both boundaries in both directions.
// It covers the DECISION only. The WIRING -- that snapshotFanart actually calls
// refuseResult and discards the bytes it refuses -- cannot be reached from a
// portable fixture, because it needs a file whose stat genuinely under-reports
// what the read then delivers, and the only way to build one is a platform
// primitive (a FIFO, which stats at zero bytes). That wiring test is therefore
// carried by the follow-on PR that adds the unix-tagged fixtures for #2712; a
// predicate nobody calls is not a fix, so both halves are needed and only one
// of them is here.
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
			name: "exactly at the per-file cap is still captured",
			read: maxFanartSnapshotFileBytes,
		},
		{
			name:        "one byte over the per-file cap is refused",
			read:        maxFanartSnapshotFileBytes + 1,
			wantRefused: true,
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
		// One byte over the per-file cap, so each is refused by the PRE-READ
		// stat and never allocated. Sparse, so 60 of them cost no disk.
		sparseFile(t, p, maxFanartSnapshotFileBytes+1)
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
	// Deliberately a LOWER bound on the warning count rather than an equality.
	// The excess is exactly what the mutation produces (a starved real backdrop
	// is refused too, and warns), so pinning equality here would make the
	// mutation fail at a precondition whose message blames the fixture. The
	// equality belongs below, where it can say what actually went wrong.
	if len(warnings) < junkCount {
		t.Fatalf("got %d warnings for %d junk files, want at least one each; the junk was not refused as "+
			"expected, so nothing below measures what it claims to", len(warnings), junkCount)
	}
	for i := 0; i < junkCount; i++ {
		if snapshot[i].data != nil {
			t.Fatalf("junk slot %d holds %d bytes, so it was captured rather than refused; the fixture "+
				"is not exercising the refusal path", i, len(snapshot[i].data))
		}
		// The pre-read stat is what refused it, not the post-read length: the
		// junk must never have been allocated. The warnings arrive in slot
		// order, so the first junkCount of them are the junk's.
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
	// And the warning count exactly matches the junk, now that the junk has
	// been shown to account for at least that many. An extra warning means a
	// real backdrop was refused as well, which is the same defect the loop
	// above reports and is worth naming separately because it is the operator's
	// view of it.
	if len(warnings) != junkCount {
		t.Errorf("got %d warnings for %d junk files; the surplus is a REAL backdrop refused as well, so "+
			"the operator is being told a healthy backdrop could not be captured: %v",
			len(warnings), junkCount, warnings[junkCount:])
	}
}
