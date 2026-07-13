package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// saveDeadlockBudget is how long a two-slot save gets before the test calls it wedged.
// The work itself is a few milliseconds of tempdir IO; the failure mode is a PERMANENT
// block on a non-reentrant mutex, not slowness, so anything in this neighborhood
// separates the two cleanly without making a slow CI machine flake.
const saveDeadlockBudget = 20 * time.Second

// TestSaveSlotProtected_DottedNamesDoNotDeadlock is the guard for the double-normalization
// deadlock in lockSlots.
//
// THE BUG: lockSlots normalized each configured name with slotBase, then passed the
// RESULT to a slotMutex that called slotBase AGAIN. filepath.Ext trims whatever follows
// the LAST dot -- extension or not -- so a second pass over an already-trimmed base keeps
// eating. Two DISTINCT configured names:
//
//	backdrop.wide.jpg -> "backdrop.wide" -> "backdrop"
//	backdrop.tall.jpg -> "backdrop.tall" -> "backdrop"
//
// collapse onto the SAME key. lockSlots then called Lock() twice on the IDENTICAL
// non-reentrant *sync.Mutex from ONE goroutine. The second call blocks forever; lockSlots
// never returns, so SaveSlotProtected's deferred unlock is never even registered and that
// slot's mutex stays held for the life of the process -- every later save to it deadlocks
// too. Only a restart clears it.
//
// NOT hypothetical. platform.ValidateImageNaming rejects empty names, path separators,
// non-image extensions and duplicates -- it says NOTHING about dots inside the base. Custom
// image-naming profiles are a supported feature, and a dotted name is legal in one. (The
// app's OWN numbering is dotless -- fanart2.jpg -- which is exactly why this never fired in
// the default configuration and why no existing test caught it.)
//
// The test runs the save in a goroutine and races it against a timeout, because a
// deadlocked save does not fail -- it HANGS, and a naive assertion would hang the whole
// package's test binary with it (a guard nobody can run is not a guard).
func TestSaveSlotProtected_DottedNamesDoNotDeadlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Two distinct dotted names, both legal under ValidateImageNaming, whose single
	// normalization keeps a dot -- which is what the second (buggy) pass then eats.
	naming := []string{"backdrop.wide.jpg", "backdrop.tall.jpg"}
	if slotBase(naming[0]) == slotBase(naming[1]) {
		t.Fatalf("the two names are the SAME slot after one normalization (%q); they must be "+
			"distinct slots, or the test is asserting nothing", slotBase(naming[0]))
	}
	// The precondition that makes this test non-vacuous: the names collide only on a
	// SECOND normalization. If they did not, the buggy code would not have deadlocked and
	// this test would pass against it.
	if slotBase(slotBase(naming[0])) != slotBase(slotBase(naming[1])) {
		t.Fatalf("double-normalizing %v does NOT collapse them onto one key; this input cannot "+
			"reproduce the deadlock and the guard is vacuous", naming)
	}

	type result struct {
		saved []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		saved, err := SaveSlotProtected(dir, "fanart", naming, makeJPEG(t, 120, 90), false, nil, discardLogger())
		done <- result{saved, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("SaveSlotProtected: %v", got.err)
		}
		// It completed -- and it actually did the work, rather than returning early.
		if len(got.saved) != 2 {
			t.Fatalf("the save wrote %d names (%v); want both configured slots", len(got.saved), got.saved)
		}
		for _, name := range naming {
			if _, statErr := os.Stat(filepath.Join(dir, name)); statErr != nil {
				t.Errorf("configured name %s was never written: %v", name, statErr)
			}
		}
	case <-time.After(saveDeadlockBudget):
		// Do NOT t.Fatal from a helper goroutine or leave the test hanging: report the
		// wedge from the test goroutine itself. The leaked goroutine is blocked forever on
		// the mutex, which is precisely the defect being reported.
		t.Fatalf("SaveSlotProtected DEADLOCKED on %v (no result in %s). Two distinct configured "+
			"names collapsed onto one slot key, so lockSlots locked the SAME non-reentrant mutex "+
			"twice in one goroutine. The mutex is now held permanently: every future save to this "+
			"slot hangs until the process restarts", naming, saveDeadlockBudget)
	}
}

// TestSaveSlotProtected_DottedNamesLockDistinctSlots proves the FIX did not paper over the
// deadlock by collapsing the two names onto one lock (which would also "not hang", while
// quietly serializing unrelated slots and, worse, backing up only one of them).
//
// Each dotted name must still be its OWN slot: its own mutex, its own backup. So both
// originals must be recoverable after an overwrite.
func TestSaveSlotProtected_DottedNamesLockDistinctSlots(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	wideOrig := makeJPEG(t, 80, 50)
	tallOrig := makeJPEG(t, 50, 80)
	if bytes.Equal(wideOrig, tallOrig) {
		t.Fatal("the two originals are byte-identical; a wrong-slot backup could pass by accident")
	}
	if err := os.WriteFile(filepath.Join(dir, "backdrop.wide.jpg"), wideOrig, 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backdrop.tall.jpg"), tallOrig, 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	naming := []string{"backdrop.wide.jpg", "backdrop.tall.jpg"}
	if _, err := SaveSlotProtected(dir, "fanart", naming, makeJPEG(t, 200, 120), false, nil, discardLogger()); err != nil {
		t.Fatalf("SaveSlotProtected: %v", err)
	}

	// Both overwritten originals are separately recoverable: two slots, two backups.
	for _, tc := range []struct {
		backup string
		want   []byte
	}{
		{"backdrop.wide.jpg", wideOrig},
		{"backdrop.tall.jpg", tallOrig},
	} {
		got, readErr := os.ReadFile(filepath.Join(dir, BackupDirName, "fanart", tc.backup))
		if readErr != nil {
			t.Fatalf("no backup of the overwritten %s (%v): the two dotted names were treated as ONE "+
				"slot, so one user image was destroyed with no recovery copy", tc.backup, readErr)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("the backup of %s holds the wrong bytes (%d, want %d): the slots were keyed "+
				"together and one backup overwrote the other", tc.backup, len(got), len(tc.want))
		}
	}
}

// TestLockSlots_RepeatedSlotDoesNotDeadlock covers the other way one goroutine can lock the
// same mutex twice: a naming list naming the SAME slot in two formats.
//
// protectedSlotNames collapses those before lockSlots ever sees them, so SaveSlotProtected
// is safe today -- but lockSlots is a general helper and must not depend on its caller
// having de-duplicated. This calls it directly.
func TestLockSlots_RepeatedSlotDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	done := make(chan struct{})
	go func() {
		// fanart.jpg and fanart.png are the SAME slot; "logo.a.png"/"logo.a.jpg" likewise,
		// and they additionally exercise the dotted-base path through the same helper.
		unlock := lockSlots(dir, "fanart", []string{"fanart.jpg", "fanart.png", "logo.a.png", "logo.a.jpg"})
		unlock()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(saveDeadlockBudget):
		t.Fatalf("lockSlots DEADLOCKED on a naming list that names one slot twice: it locked the "+
			"same non-reentrant mutex twice in one goroutine (no result in %s)", saveDeadlockBudget)
	}

	// And the locks were genuinely RELEASED, not leaked: a second call must also return.
	// Without this, a lockSlots that skipped locking entirely would pass the check above.
	second := make(chan struct{})
	go func() {
		unlock := lockSlots(dir, "fanart", []string{"fanart.jpg"})
		unlock()
		close(second)
	}()
	select {
	case <-second:
	case <-time.After(saveDeadlockBudget):
		t.Fatalf("a second lockSlots on the same slot never returned (no result in %s): the first "+
			"call's unlock did not release every mutex it took", saveDeadlockBudget)
	}
}
