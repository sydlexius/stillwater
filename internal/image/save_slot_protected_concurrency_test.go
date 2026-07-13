package image

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The concurrency tests for SaveSlotProtected.
//
// The bug these guard is the nastiest shape this branch has: the ROLLBACK -- the very
// mechanism added to PREVENT data loss -- becomes the thing that causes it. Two writes
// to the same slot interleave their backup/save/restore steps, and the failing one's
// RestoreSlot puts a STALE original back on top of the other one's SUCCESSFUL write.
// The user's chosen image is silently destroyed and the API reported 200.
//
// Note what the fault injection has to be. Feeding a racer undecodable bytes is
// USELESS here for the same reason it is useless in the rollback tests
// (see seedOriginalWithAFailingSecondName): Save rejects such bytes in DetectFormat
// BEFORE it deletes or writes anything, so that racer never reaches RestoreSlot and
// the destructive interleaving never happens. The failing racer below therefore uses
// the same "second configured name cannot be written" fault, which fails only AFTER
// the write has landed -- so it really does roll back, and really can eat the winner.

// seedSlotRace sets up one round: an artist dir holding the ORIGINAL fanart.png, plus
// the unwritableName barrier that makes a second configured filename fail to write.
func seedSlotRace(t *testing.T) (dir string, original []byte) {
	t.Helper()
	dir = t.TempDir()
	original = makePNG(t, 80, 50)
	if err := os.WriteFile(filepath.Join(dir, "fanart.png"), original, 0o644); err != nil {
		t.Fatalf("seeding the original fanart.png: %v", err)
	}
	return dir, original
}

// TestSaveSlotProtected_ConcurrentSameSlot_RollbackCannotEatAGoodWrite is the test for
// the per-slot lock. Two writers hit the SAME slot at once:
//
//	winner -- a plain valid write. It MUST succeed and its bytes MUST be what survives.
//	loser  -- a write that fails on its second configured name, AFTER its first write
//	          landed, so it rolls back.
//
// Whatever order the lock serializes them in, the outcome is the same and it is
// deterministic:
//
//	loser first:  loser backs up the original, writes, fails, restores the original.
//	              Then the winner overwrites it. Disk = winner.
//	winner first: winner writes. Then the loser backs up THE WINNER'S image, writes,
//	              fails, and restores THE WINNER'S image. Disk = winner.
//
// Unserialized, the loser can back up the ORIGINAL, then the winner's save lands, then
// the loser's rollback restores that original over it. Disk = the stale original, and
// the winner's image is gone. That is the data loss.
//
// REVERT-AND-REPROVE: deleting the slotMutex lock/unlock in SaveSlotProtected turns
// this test RED (measured: it catches the stale restore within the first few rounds).
func TestSaveSlotProtected_ConcurrentSameSlot_RollbackCannotEatAGoodWrite(t *testing.T) {
	t.Parallel()

	// The race window is a handful of filesystem ops wide, so one round is not a
	// reliable trial. Rounds are cheap (a temp dir and two small images) and any
	// single bad round fails the test.
	const rounds = 60

	winnerImage := makeJPEG(t, 120, 90)

	for round := range rounds {
		dir, original := seedSlotRace(t)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var winnerErr, loserErr error

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, winnerErr = SaveSlotProtected(dir, "fanart", []string{"fanart.jpg"}, winnerImage, false, nil, discardLogger())
		}()
		go func() {
			defer wg.Done()
			<-start
			// Same slot ("fanart"), and its second name is unwritable, so it rolls back.
			_, loserErr = SaveSlotProtected(dir, "fanart",
				[]string{"fanart.jpg", unwritableName}, makeJPEG(t, 60, 40), false, nil, discardLogger())
		}()
		close(start)
		wg.Wait()

		if winnerErr != nil {
			t.Fatalf("round %d: the valid write failed: %v", round, winnerErr)
		}
		if loserErr == nil {
			t.Fatalf("round %d: the write with an unwritable second name was expected to fail, "+
				"but succeeded -- the fault injection is not working and this test proves nothing", round)
		}

		// The winner's image, and ONLY the winner's image, must be on disk.
		got, err := os.ReadFile(filepath.Join(dir, "fanart.jpg"))
		if err != nil {
			t.Fatalf("round %d: reading the slot after both writes: %v", round, err)
		}
		if !bytes.Equal(got, winnerImage) {
			t.Fatalf("round %d: the slot does NOT hold the successfully-written image. "+
				"The loser's rollback overwrote a good write with stale bytes (got %d bytes, want the winner's %d).",
				round, len(got), len(winnerImage))
		}
		// The original was a .png and the winner wrote a .jpg: Save's conflicting-format
		// cleanup must have removed it. A resurrected fanart.png is the stale restore.
		if _, statErr := os.Stat(filepath.Join(dir, "fanart.png")); statErr == nil {
			t.Fatalf("round %d: the stale ORIGINAL fanart.png is back on disk -- a rollback "+
				"restored the pre-edit image over the winner's write (original was %d bytes)",
				round, len(original))
		}
	}
}

// TestSaveSlotProtected_ConcurrentSameSlot_CrossFormat is the test that makes the LOCK KEY
// enforceable instead of merely incidental.
//
// The key is (filepath.Clean(dir), imageType, slotBase(naming[0])) -- extension-LESS on
// purpose. fanart.png and fanart.jpg are the SAME SLOT in two formats: Save's
// CleanupConflictingFormats has each one DELETING the other. Two writers configured with
// different formats therefore contend, and must serialize.
//
// Every other concurrency test here gives both racers "fanart.jpg", so they pass as long as
// the key is STABLE -- whether or not it is CORRECT. An over-separated key (one that keeps
// the extension, or uses the raw un-Cleaned dir) hands the two racers DIFFERENT mutexes,
// they never contend, and the whole suite still goes green. This case is what closes that:
// a mutant keyed on the full basename gives the racers different locks, the loser's rollback
// restores the stale ORIGINAL over the winner's successful write, and this goes RED.
//
// Serialized, either order lands on the winner's image:
//
//	loser first:  loser backs up the original, writes fanart.jpg, fails, restores fanart.png.
//	              Then the winner writes fanart.png. Disk = the winner.
//	winner first: winner writes fanart.png. Then the loser backs up THE WINNER'S image,
//	              writes fanart.jpg (deleting the winner's png as a conflicting format),
//	              fails, and restores THE WINNER'S image. Disk = the winner.
func TestSaveSlotProtected_ConcurrentSameSlot_CrossFormat(t *testing.T) {
	t.Parallel()

	const rounds = 60

	// The winner writes PNG bytes to the png name; the loser writes JPEG bytes to the jpg
	// name. Same slot, different formats -- which is precisely the contention the
	// extension-less key exists to catch.
	winnerImage := makePNG(t, 120, 90)

	for round := range rounds {
		dir, original := seedSlotRace(t)
		if bytes.Equal(winnerImage, original) {
			t.Fatal("the winner's image equals the seeded original; a stale restore would be undetectable")
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		var winnerErr, loserErr error

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, winnerErr = SaveSlotProtected(dir, "fanart", []string{"fanart.png"}, winnerImage, false, nil, discardLogger())
		}()
		go func() {
			defer wg.Done()
			<-start
			// The SAME slot, spelled with the other extension, and an unwritable second name
			// so it fails AFTER its write lands and therefore really does roll back.
			_, loserErr = SaveSlotProtected(dir, "fanart",
				[]string{"fanart.jpg", unwritableName}, makeJPEG(t, 60, 40), false, nil, discardLogger())
		}()
		close(start)
		wg.Wait()

		if winnerErr != nil {
			t.Fatalf("round %d: the valid write failed: %v", round, winnerErr)
		}
		if loserErr == nil {
			t.Fatalf("round %d: the write with an unwritable second name was expected to fail, "+
				"but succeeded -- the fault injection is not working and this test proves nothing", round)
		}

		// THE ASSERTION: the winner's bytes, on disk. Not an error value.
		got, err := os.ReadFile(filepath.Join(dir, "fanart.png"))
		if err != nil {
			t.Fatalf("round %d: the winner's image is GONE from the slot: %v", round, err)
		}
		if bytes.Equal(got, original) {
			t.Fatalf("round %d: the slot holds the STALE ORIGINAL. The cross-format loser's rollback "+
				"restored the pre-edit image OVER the winner's successful write -- the two racers did not "+
				"share a lock, so the key is separating fanart.png from fanart.jpg. They are ONE slot: "+
				"CleanupConflictingFormats has each format deleting the other.", round)
		}
		if !bytes.Equal(got, winnerImage) {
			t.Fatalf("round %d: the slot holds neither the winner's image nor the original (%d bytes); "+
				"a rollback or a torn write has corrupted it", round, len(got))
		}
		// The loser's half-written jpg must not survive next to the winner's png: they are the
		// same slot, and a slot holds ONE image.
		if _, statErr := os.Stat(filepath.Join(dir, "fanart.jpg")); statErr == nil {
			t.Fatalf("round %d: the loser's fanart.jpg survived alongside the winner's fanart.png; "+
				"the rollback must leave the slot holding exactly one image", round)
		}
	}
}

// TestSaveSlotProtected_ConcurrentSameSlot_LastWriteIsIntact covers the all-succeed case:
// N racers all writing valid images to one slot. Every write must succeed, and the file
// left on disk must be EXACTLY one of them, whole -- not a mix, not a truncation, and not
// some earlier image resurrected from a backup.
func TestSaveSlotProtected_ConcurrentSameSlot_LastWriteIsIntact(t *testing.T) {
	t.Parallel()

	const writers = 8
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), makeJPEG(t, 200, 200), 0o644); err != nil {
		t.Fatalf("seeding the original: %v", err)
	}

	// Distinct sizes give distinct bytes, so the survivor is identifiable.
	payloads := make([][]byte, writers)
	for i := range payloads {
		payloads[i] = makeJPEG(t, 100+i*10, 80)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, writers)
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = SaveSlotProtected(dir, "fanart", []string{"fanart.jpg"}, payloads[i], false, nil, discardLogger())
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d failed: %v", i, err)
		}
	}

	got, err := os.ReadFile(filepath.Join(dir, "fanart.jpg"))
	if err != nil {
		t.Fatalf("reading the slot: %v", err)
	}
	for _, want := range payloads {
		if bytes.Equal(got, want) {
			return // intact, and one of the images actually written
		}
	}
	t.Fatalf("the slot holds %d bytes matching NONE of the %d concurrently-written images: "+
		"it is a stale restore, a mix, or a torn write", len(got), writers)
}
