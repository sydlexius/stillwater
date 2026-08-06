package image

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// #2460: the stale-temp sweep must PRESERVE whatever sits at a survivor's
// staging path, not unlink it.
//
// The path holds two very different things, and the code cannot tell them
// apart from the filename alone:
//
//  1. Inert junk from a previously FAILED operation -- removing it is correct.
//  2. Real, ONLY-COPY artwork stranded by a hard crash (power loss, SIGKILL,
//     OOM) landing between the renumber's two rename phases. The file has been
//     moved off its final name but not yet onto its new one, so the staging
//     path is the only place it exists. Removing it destroys it permanently.
//
// The old sweep assumed (1) unconditionally, so every crash of shape (2) had
// its artwork unlinked by the next ordinary renumber. Quarantining costs a
// rename in case (1) and saves the only copy in case (2), which is the right
// side of that trade by a wide margin.
//
// These tests assert the BYTES ON DISK, never the returned error. The defect
// is silent: the renumber succeeded and reported nothing while the file went
// away, so a test that only checked the error would have passed throughout.

// stagedTmpName is the staging path the renumber computes for survivor index 0
// with a .jpg extension. Spelled out rather than derived so the test breaks
// loudly if the production naming changes -- a test that recomputed the name
// from the same expression as the code would follow it silently and stop
// guarding anything.
const stagedTmpName = "fanart_renumber_0.jpg.tmp"

func TestRenumberFanart_CrashStrandedTmpIsPreserved(t *testing.T) {
	dir := t.TempDir()

	survivor := filepath.Join(dir, "fanart1.jpg")
	if err := os.WriteFile(survivor, []byte("SURVIVOR"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Exactly what a crash between the two phases leaves behind. No source
	// mutation and no fault injection: this is a plain pre-existing file at a
	// path the next renumber will want.
	stranded := filepath.Join(dir, stagedTmpName)
	want := []byte("ONLY-COPY-ARTWORK-THAT-EXISTS-NOWHERE-ELSE")
	if err := os.WriteFile(stranded, want, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := renumberFanartFiles(dir, "fanart", []string{survivor}, false); err != nil {
		t.Fatalf("renumber returned an error: %v", err)
	}

	// PRECONDITION: the renumber must actually have run and used that staging
	// path. Without this, a renumber that silently did nothing would pass the
	// preservation assertion below while proving nothing at all.
	if _, err := os.Stat(filepath.Join(dir, "fanart.jpg")); err != nil {
		t.Fatalf("precondition: the survivor was not renumbered to fanart.jpg (%v); "+
			"the preservation assertion below would be vacuous", err)
	}

	quarantined := findQuarantined(t, dir, stagedTmpName)
	got, err := os.ReadFile(quarantined)
	if err != nil {
		t.Fatalf("the crash-stranded only-copy was DESTROYED: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("quarantined content changed:\n got: %q\nwant: %q", got, want)
	}
}

// The quarantine name must keep the ORIGINAL EXTENSION in the terminal
// position. Appending a timestamp suffix after the extension
// (name.jpg.orphan-<ts>) makes filepath.Ext return ".orphan-<ts>", which
// silently breaks every consumer that classifies by extension -- including
// internal/foreign's isForeignCandidate, which rejects such a name outright
// before it ever reaches its prefix matching. Verified against that function.
//
// So the name shape is not cosmetic: it decides whether a quarantined file can
// ever be surfaced to an operator, which is the follow-up this fix defers (#2954).
func TestRenumberFanart_QuarantineNameKeepsExtensionTerminal(t *testing.T) {
	dir := t.TempDir()

	survivor := filepath.Join(dir, "fanart1.jpg")
	if err := os.WriteFile(survivor, []byte("SURVIVOR"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stagedTmpName), []byte("X"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := renumberFanartFiles(dir, "fanart", []string{survivor}, false); err != nil {
		t.Fatalf("renumber returned an error: %v", err)
	}

	quarantined := filepath.Base(findQuarantined(t, dir, stagedTmpName))
	if filepath.Ext(quarantined) != ".jpg" {
		t.Errorf("quarantine name %q has extension %q, want \".jpg\" -- a non-image "+
			"terminal extension makes the file unclassifiable by extension-keyed consumers",
			quarantined, filepath.Ext(quarantined))
	}
	if !strings.Contains(quarantined, quarantineMarker) {
		t.Errorf("quarantine name %q does not contain %q, so it is indistinguishable "+
			"from ordinary artwork", quarantined, quarantineMarker)
	}
}

// Two quarantines of the same staging path must not collide. A crash can
// happen more than once, and a second quarantine that overwrote the first
// would destroy the earlier only-copy -- reintroducing the bug one level up.
func TestRenumberFanart_RepeatedQuarantineDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	survivor := filepath.Join(dir, "fanart1.jpg")

	for i, content := range []string{"FIRST-ONLY-COPY", "SECOND-ONLY-COPY"} {
		if err := os.WriteFile(survivor, []byte("SURVIVOR"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, stagedTmpName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := renumberFanartFiles(dir, "fanart", []string{survivor}, false); err != nil {
			t.Fatalf("round %d: renumber returned an error: %v", i, err)
		}
		// Move the renumbered result back so the next round has a survivor at
		// the same starting name.
		if err := os.Rename(filepath.Join(dir, "fanart.jpg"), survivor); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}

	var found []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), quarantineMarker) {
			found = append(found, e.Name())
		}
	}
	if len(found) != 2 {
		t.Fatalf("want 2 distinct quarantined files after 2 crashes, got %d: %v", len(found), found)
	}

	// Both original contents must still be readable somewhere. Asserting the
	// COUNT alone would pass if the second quarantine overwrote the first and
	// some unrelated file made up the number.
	seen := map[string]bool{}
	for _, name := range found {
		b, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			t.Fatalf("reading %s: %v", name, readErr)
		}
		seen[string(b)] = true
	}
	for _, want := range []string{"FIRST-ONLY-COPY", "SECOND-ONLY-COPY"} {
		if !seen[want] {
			t.Errorf("content %q was lost -- a later quarantine overwrote an earlier one", want)
		}
	}
}

// The ordinary path must stay ordinary: with nothing at the staging path, a
// renumber produces no quarantine files at all. Without this, a fix that
// quarantined unconditionally (or left stray files behind) would pass every
// preservation test above while littering every artist directory.
func TestRenumberFanart_NoQuarantineWhenNothingStranded(t *testing.T) {
	dir := t.TempDir()
	survivor := filepath.Join(dir, "fanart1.jpg")
	if err := os.WriteFile(survivor, []byte("SURVIVOR"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := renumberFanartFiles(dir, "fanart", []string{survivor}, false); err != nil {
		t.Fatalf("renumber returned an error: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), quarantineMarker) {
			t.Errorf("created a quarantine file %q when nothing was stranded", e.Name())
		}
	}
}

// A NON-REGULAR file at a staging path must make the sweep FAIL, not get
// renamed aside. This is where this fix meets #2459's, and the interaction is
// easy to break in the safe-looking direction.
//
// #2459 hoisted the sweep ahead of any staging so a fallible step happens while
// nothing is at risk, and internal/rule's stale-tmp-sweep tests squat the
// staging path with a NON-EMPTY DIRECTORY precisely so os.Remove fails with
// ENOTEMPTY and the whole operation aborts before a single survivor moves.
//
// Quarantining by rename would have quietly retired that guard: rename SUCCEEDS
// on a directory where remove fails, so the sweep would report success and walk
// into a two-phase rename over an occupied path. Caught by running
// internal/rule's suite, not this package's -- three tests there went red.
//
// So the crash-path fix must not make the error path less safe. A directory is
// not crash-stranded artwork; there is nothing to preserve and every reason to
// stop.
func TestRenumberFanart_NonRegularAtStagingPathFails(t *testing.T) {
	dir := t.TempDir()
	survivor := filepath.Join(dir, "fanart1.jpg")
	if err := os.WriteFile(survivor, []byte("SURVIVOR"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A non-empty directory squatting the staging path, exactly as
	// internal/rule's tests construct it.
	squat := filepath.Join(dir, stagedTmpName)
	if err := os.Mkdir(squat, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(squat, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := renumberFanartFiles(dir, "fanart", []string{survivor}, false)
	if err == nil {
		t.Fatal("renumber succeeded with a directory squatting the staging path; " +
			"the sweep must fail there so #2459's error-path guard still aborts before staging")
	}

	// The survivor must not have moved. An abort that already staged something
	// is the data-loss shape #2459 exists to prevent, and asserting only on the
	// error would not see it.
	if got, readErr := os.ReadFile(survivor); readErr != nil || string(got) != "SURVIVOR" {
		t.Errorf("survivor was staged or lost despite the sweep failing: err=%v", readErr)
	}
	// And nothing may have been quarantined -- the squatter is not artwork.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), quarantineMarker) {
			t.Errorf("quarantined %q, but a directory is not crash-stranded artwork", e.Name())
		}
	}
}

// THE COLLISION GUARD, exercised with the clock PINNED.
//
// With a real clock this branch is unreachable: two quarantines land in
// different microseconds and get distinct names from the STAMP, so the guard
// never runs. Measured on the earlier version of this suite -- the case whose
// stated purpose was the anti-clobber property passed with the guard entirely
// deleted, because it was the timestamp doing the work. Pinning the clock is
// what makes the guard the only thing standing between two files and one.
//
// The stamp's format carries nanosecond digits, but darwin's clock is roughly
// microsecond-granular and does not fill them, so identical stamps on
// consecutive calls are ordinary rather than exotic. That is why uniqueness
// must not rest on the timestamp.
func TestQuarantine_CollisionGuardPreservesBothCopies(t *testing.T) {
	orig := quarantineNow
	quarantineNow = func() time.Time { return time.Unix(1735689600, 0).UTC() } // fixed
	t.Cleanup(func() { quarantineNow = orig })

	dir := t.TempDir()
	staging := filepath.Join(dir, stagedTmpName)

	for _, content := range []string{"FIRST-ONLY-COPY", "SECOND-ONLY-COPY"} {
		if err := os.WriteFile(staging, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := quarantineStrandedTemp(staging, ".jpg"); err != nil {
			t.Fatalf("quarantine of %q failed: %v", content, err)
		}
	}

	// PRECONDITION: the two quarantines must have produced the SAME stamp, or
	// the guard was bypassed and this case proves nothing -- which is exactly
	// how the previous version of this test passed against a deleted guard.
	var names []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), quarantineMarker) {
			names = append(names, e.Name())
		}
	}
	if len(names) != 2 {
		t.Fatalf("want 2 quarantined files, got %d: %v", len(names), names)
	}
	// Exactly one name must carry the collision counter. Checked by SEARCHING
	// rather than by index: sorting puts "-1.jpg" BEFORE ".jpg" ('-' is 0x2D,
	// '.' is 0x2E), so an index-based assertion reads the wrong element and
	// fails against correct behavior -- which is what the first draft of this
	// check did.
	sort.Strings(names)
	counted := 0
	for _, n := range names {
		if strings.HasSuffix(n, "-1.jpg") {
			counted++
		}
	}
	if counted != 1 {
		t.Fatalf("expected exactly one name to carry the collision counter, got %d in %v; "+
			"if none does, the stamps differed and the guard was never exercised", counted, names)
	}

	// Both only-copies must survive. os.Rename would have silently destroyed
	// the first here; os.Link refuses with EEXIST, which is what makes the
	// refusal atomic rather than a check-then-act.
	seen := map[string]bool{}
	for _, n := range names {
		b, readErr := os.ReadFile(filepath.Join(dir, n))
		if readErr != nil {
			t.Fatalf("reading %s: %v", n, readErr)
		}
		seen[string(b)] = true
	}
	for _, want := range []string{"FIRST-ONLY-COPY", "SECOND-ONLY-COPY"} {
		if !seen[want] {
			t.Errorf("content %q was lost -- the later quarantine clobbered the earlier", want)
		}
	}
	// The source must be gone, or the caller's two-phase rename walks into an
	// occupied staging path.
	if _, statErr := os.Lstat(staging); !os.IsNotExist(statErr) {
		t.Errorf("staging path still occupied after quarantine: %v", statErr)
	}
}

// The exhaustion path must ABORT rather than clobber. Reached by pinning the
// clock and pre-filling every counter slot, which is the only way to get there
// -- 100 real collisions do not happen by chance.
func TestQuarantine_ExhaustedNamesAbortsRatherThanClobbering(t *testing.T) {
	orig := quarantineNow
	quarantineNow = func() time.Time { return time.Unix(1735689600, 0).UTC() }
	t.Cleanup(func() { quarantineNow = orig })

	dir := t.TempDir()
	staging := filepath.Join(dir, stagedTmpName)
	if err := os.WriteFile(staging, []byte("ONLY-COPY"), 0o600); err != nil {
		t.Fatal(err)
	}

	stamp := time.Unix(1735689600, 0).UTC().Format("20060102T150405.000000000Z")
	base := "fanart_renumber_0"
	// Occupy the un-suffixed name and every counter the loop will try.
	if err := os.WriteFile(filepath.Join(dir, base+quarantineMarker+stamp+".jpg"), []byte("TAKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 100; i++ {
		n := fmt.Sprintf("%s%s%s-%d.jpg", base, quarantineMarker, stamp, i)
		if err := os.WriteFile(filepath.Join(dir, n), []byte("TAKEN"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	err := quarantineStrandedTemp(staging, ".jpg")
	if err == nil {
		t.Fatal("quarantine succeeded with every name taken; it must abort rather than clobber")
	}

	// The source must be untouched, and no occupied name may have been
	// overwritten. Aborting is only safe if it left everything alone.
	got, readErr := os.ReadFile(staging)
	if readErr != nil || string(got) != "ONLY-COPY" {
		t.Errorf("the only-copy was lost while failing to quarantine it: err=%v", readErr)
	}
	taken, readErr := os.ReadFile(filepath.Join(dir, base+quarantineMarker+stamp+".jpg"))
	if readErr != nil || string(taken) != "TAKEN" {
		t.Errorf("an existing quarantined file was clobbered: err=%v got=%q", readErr, taken)
	}
}

// A source that VANISHES mid-quarantine is success, not failure.
//
// Renumbering takes no lock, so a concurrent run can clear the staging path
// between the Lstat and the link. The whole purpose of this function is
// "nothing stranded at this path", and a vanished source means nothing is --
// aborting the renumber there would fail it for having gotten what it wanted.
//
// Note the fail-safe direction is OPPOSITE to the destination case: a
// destination that already exists must NEVER read as success, because it may
// be an earlier crash's only copy. Same race, opposite disposition, which is
// why each branch needs its own reasoning rather than one blanket rule.
//
// The race is constructed deterministically rather than raced for: the file is
// simply absent when the link runs, which is the state a concurrent clear
// leaves behind.
func TestQuarantine_VanishedSourceIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, stagedTmpName)
	if err := os.WriteFile(staging, []byte("ABOUT-TO-VANISH"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The file EXISTS at the Lstat and is gone by the link. Removing it up
	// front instead would return at the Lstat early-return and never reach the
	// branch under test -- measured: with the file simply absent, deleting the
	// ENOENT handling left this test GREEN, proving nothing.
	orig := quarantineRaceHook
	called := false
	quarantineRaceHook = func(p string) {
		called = true
		if err := os.Remove(p); err != nil {
			t.Errorf("hook could not clear the source: %v", err)
		}
	}
	t.Cleanup(func() { quarantineRaceHook = orig })

	err := quarantineStrandedTemp(staging, ".jpg")

	// PRECONDITION: without the hook firing, this case degenerates into the
	// already-absent one and asserts nothing about the race.
	if !called {
		t.Fatal("precondition: the race hook never ran, so the link was not reached " +
			"with a vanished source and this case is vacuous")
	}
	if err != nil {
		t.Fatalf("a source cleared by a concurrent run must not be an error, got: %v", err)
	}

	// Nothing may have been created in response to a file that is gone.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), quarantineMarker) {
			t.Errorf("created %q for a source that vanished", e.Name())
		}
	}
}

// A full renumber must survive the same vanishing act. This exercises the
// caller's path rather than the helper's, because the consequence Copilot
// flagged is at that level: an aborted renumber, not a returned error.
func TestRenumberFanart_SurvivesAStagingPathClearedConcurrently(t *testing.T) {
	dir := t.TempDir()
	survivor := filepath.Join(dir, "fanart1.jpg")
	if err := os.WriteFile(survivor, []byte("SURVIVOR"), 0o600); err != nil {
		t.Fatal(err)
	}

	// PRECONDITION: no staging file exists, standing in for one cleared by a
	// concurrent run between the check and the link.
	if _, err := os.Lstat(filepath.Join(dir, stagedTmpName)); !os.IsNotExist(err) {
		t.Fatalf("precondition: the staging path must start absent, got %v", err)
	}

	if err := renumberFanartFiles(dir, "fanart", []string{survivor}, false); err != nil {
		t.Fatalf("renumber aborted because the staging path was already clear: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fanart.jpg")); err != nil {
		t.Errorf("the renumber did not complete: %v", err)
	}
}

// The one path that reports an error AFTER the bytes are already safe: the
// link succeeded, so the artwork exists under the quarantine name, but the
// source could not be unlinked.
//
// Reporting rather than swallowing is deliberate. The caller's two-phase rename
// is about to want that staging path, so telling it the path is clear when it
// is not would walk the renumber onto an occupied name. But the error must not
// read as "the artwork was lost" -- it was not, and this test pins both halves:
// the failure IS reported, and the bytes ARE safe.
//
// Constructed with a real filesystem fault, and with a seam that fires BETWEEN
// the link and the unlink -- because no single directory mode reaches this
// branch: the unlink needs a read-only parent, while the link that precedes it
// needs that same parent writable. Measured: locking the directory in the
// earlier race hook makes the LINK fail instead, so the branch is never
// reached and the mutation survives.
func TestQuarantine_SourceRemovalFailureIsReportedButBytesSurvive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory mode bits do not restrain unlink")
	}

	dir := t.TempDir()
	staging := filepath.Join(dir, stagedTmpName)
	want := []byte("ONLY-COPY-THAT-MUST-SURVIVE-A-FAILED-UNLINK")
	if err := os.WriteFile(staging, want, 0o600); err != nil {
		t.Fatal(err)
	}

	orig := quarantinePostLinkHook
	fired := false
	quarantinePostLinkHook = func(p string) {
		fired = true
		if err := os.Chmod(filepath.Dir(p), 0o500); err != nil {
			t.Errorf("hook could not lock the directory: %v", err)
		}
	}
	t.Cleanup(func() {
		quarantinePostLinkHook = orig
		_ = os.Chmod(dir, 0o700) // let t.TempDir clean up
	})

	err := quarantineStrandedTemp(staging, ".jpg")

	if !fired {
		t.Fatal("precondition: the post-link hook never ran, so the removal was not " +
			"made to fail and this case asserts nothing")
	}
	if err == nil {
		t.Fatal("a quarantine that could not clear the source must report it: the caller's " +
			"two-phase rename is about to want that path")
	}
	if !strings.Contains(err.Error(), "could not clear the original") {
		t.Errorf("the error does not say the artwork SURVIVED, so it reads as data loss: %v", err)
	}

	// THE POINT: the bytes are safe even though the call failed. Asserting only
	// the error would leave "did we lose the artwork?" unanswered, which is the
	// question that matters on this path.
	_ = os.Chmod(dir, 0o700)
	q := findQuarantined(t, dir, stagedTmpName)
	got, readErr := os.ReadFile(q)
	if readErr != nil || string(got) != string(want) {
		t.Errorf("the only-copy was lost when the unlink failed: err=%v", readErr)
	}
}

// findQuarantined returns the single quarantined file derived from origName,
// failing the test when there is not exactly one. Exactly-one matters: zero
// means the file was destroyed (the bug), and more than one means a previous
// assertion was reading whichever file it happened to find first.
func findQuarantined(t *testing.T, dir, origName string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	stem := strings.TrimSuffix(origName, ".tmp")
	stem = strings.TrimSuffix(stem, filepath.Ext(stem))
	var matches []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stem) && strings.Contains(e.Name(), quarantineMarker) {
			matches = append(matches, filepath.Join(dir, e.Name()))
		}
	}
	if len(matches) != 1 {
		var all []string
		for _, e := range entries {
			all = append(all, e.Name())
		}
		t.Fatalf("want exactly 1 quarantined file for %s, found %d; directory contains: %v",
			origName, len(matches), all)
	}
	return matches[0]
}
