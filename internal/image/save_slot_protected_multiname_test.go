package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// seedTwoNamedOriginals sets up the state #2434 is about: a fanart slot configured with
// TWO names, each holding a genuinely DIFFERENT image on disk, plus the write barrier
// that makes the save fail after the destructive cleanup has already run.
//
// naming is ["fanart.jpg", "backdrop.jpg", "blocked/fanart.jpg"]:
//
//	fanart.jpg    -- decodes fine; Save's cleanup DELETES the fanart.png original
//	backdrop.jpg  -- decodes fine; Save's cleanup DELETES the backdrop.png original
//	blocked/...   -- "blocked" is a regular FILE, so the write fails ENOTDIR
//
// Fanart is never symlink-eligible, so Save writes each name as its own real file and
// each one runs its own CleanupConflictingFormats DELETE. Both originals are therefore
// destroyed before the save can fail -- which is exactly what makes the rollback the
// only thing standing between the user and losing BOTH images.
//
// DefaultFileNames["fanart"] is a four-name list and ImageNaming.Fanart is user-editable
// and uncapped, so a multi-name fanart list is ordinary configuration, not a contrivance.
func seedTwoNamedOriginals(t *testing.T) (dir string, fanartOrig, backdropOrig []byte, naming []string) {
	t.Helper()
	dir = t.TempDir()

	// Different sizes so the two images are byte-distinct: a restore that puts the WRONG
	// original into a slot must not be able to pass by accident.
	fanartOrig = makePNG(t, 80, 50)
	backdropOrig = makePNG(t, 60, 40)

	if err := os.WriteFile(filepath.Join(dir, "fanart.png"), fanartOrig, 0o644); err != nil {
		t.Fatalf("seeding fanart.png: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backdrop.png"), backdropOrig, 0o644); err != nil {
		t.Fatalf("seeding backdrop.png: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocked"), []byte("a regular file, not a directory"), 0o644); err != nil {
		t.Fatalf("seeding the write barrier: %v", err)
	}
	if bytes.Equal(fanartOrig, backdropOrig) {
		t.Fatal("the two originals are byte-identical; a wrong-slot restore could pass by accident")
	}
	return dir, fanartOrig, backdropOrig, []string{"fanart.jpg", "backdrop.jpg", "blocked/fanart.jpg"}
}

// TestSaveUnprotected_DestroysEveryConfiguredName is the PRECONDITION for the rollback
// test below, and it is what makes that test non-vacuous: it proves a bare Save really
// does destroy BOTH originals, so "both originals survived" afterwards means something.
//
// This is the measured destruction #2434 reports. A backup of naming[0] alone leaves
// backdrop.png deleted with nothing to put back.
func TestSaveUnprotected_DestroysEveryConfiguredName(t *testing.T) {
	t.Parallel()
	dir, _, _, naming := seedTwoNamedOriginals(t)

	_, err := Save(dir, "fanart", makeJPEG(t, 120, 90), naming, false, nil, discardLogger())
	if err == nil {
		t.Fatal("the fault did not fail the save; the rollback test built on it would be vacuous")
	}

	for _, gone := range []string{"fanart.png", "backdrop.png"} {
		if _, statErr := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(statErr) {
			t.Fatalf("the unprotected save did NOT destroy %s (stat err = %v); the fault does not "+
				"reach the rollback path for this name and cannot test it", gone, statErr)
		}
	}
}

// TestSaveSlotProtected_RollsBackEveryConfiguredName is the #2434 guard.
//
// SaveSlotProtected used to back up naming[0] ONLY while handing the FULL naming list to
// Save. Every name is written as a real file for fanart and each write runs its own
// CleanupConflictingFormats DELETE, so backdrop.png was destroyed with no backup and the
// rollback restored only fanart.*. The user lost an image the code claimed to protect.
//
// Asserts the OUTCOME -- the original bytes of BOTH names, back on disk -- not an error
// value.
func TestSaveSlotProtected_RollsBackEveryConfiguredName(t *testing.T) {
	t.Parallel()
	dir, fanartOrig, backdropOrig, naming := seedTwoNamedOriginals(t)

	_, err := SaveSlotProtected(dir, "fanart", naming, makeJPEG(t, 120, 90), false, nil, discardLogger())
	if err == nil {
		t.Fatal("expected the unwritable third filename to fail the save")
	}

	// THE ASSERTION THAT MATTERS: every configured name's original is back, byte for byte.
	for _, tc := range []struct {
		file string
		want []byte
	}{
		{"fanart.png", fanartOrig},
		{"backdrop.png", backdropOrig},
	} {
		got, readErr := os.ReadFile(filepath.Join(dir, tc.file))
		if readErr != nil {
			t.Fatalf("the user's original %s is GONE after a failed overwrite (%v). Save deleted it "+
				"as a conflicting format and then failed; only naming[0] was backed up, so nothing "+
				"restored it -- the artwork is unrecoverable (#2434)", tc.file, readErr)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("%s was restored with the WRONG bytes: got %d, want %d (a slot was restored from "+
				"another slot's backup)", tc.file, len(got), len(tc.want))
		}
	}

	// The failed edit's bytes must not survive alongside the restored originals, for any
	// name: the rollback leaves the slot exactly as it found it.
	for _, leftover := range []string{"fanart.jpg", "backdrop.jpg"} {
		if _, statErr := os.Stat(filepath.Join(dir, leftover)); !os.IsNotExist(statErr) {
			t.Errorf("the failed edit's %s survived the rollback (stat err = %v)", leftover, statErr)
		}
	}
}

// TestSaveSlotProtected_BacksUpEveryConfiguredName covers the SUCCESS path, where there
// is no rollback to lean on and the damage is silent.
//
// A two-name save that SUCCEEDS still overwrites two originals. If only naming[0] was
// backed up, the user can revert their primary fanart but their backdrop is simply gone --
// no error, no rollback, nothing to restore from. Every overwritten slot must leave a
// recoverable backup behind.
func TestSaveSlotProtected_BacksUpEveryConfiguredName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	fanartOrig := makePNG(t, 80, 50)
	backdropOrig := makePNG(t, 60, 40)
	if err := os.WriteFile(filepath.Join(dir, "fanart.png"), fanartOrig, 0o644); err != nil {
		t.Fatalf("seeding fanart.png: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backdrop.png"), backdropOrig, 0o644); err != nil {
		t.Fatalf("seeding backdrop.png: %v", err)
	}

	naming := []string{"fanart.jpg", "backdrop.jpg"}
	saved, err := SaveSlotProtected(dir, "fanart", naming, makeJPEG(t, 200, 120), false, nil, discardLogger())
	if err != nil {
		t.Fatalf("SaveSlotProtected: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("Save wrote %d names (%v); want both configured names, so both were overwritten", len(saved), saved)
	}

	// Both originals were destroyed by the (successful) save's conflicting-format cleanup.
	// Both must therefore be recoverable from the backup dir, under their ORIGINAL names.
	for _, tc := range []struct {
		backup string
		want   []byte
	}{
		{"fanart.png", fanartOrig},
		{"backdrop.png", backdropOrig},
	} {
		got, readErr := os.ReadFile(filepath.Join(dir, BackupDirName, "fanart", tc.backup))
		if readErr != nil {
			t.Fatalf("no backup of the overwritten %s (%v). The save succeeded, so no rollback runs -- "+
				"this original is gone with no way for the user to revert it (#2434)", tc.backup, readErr)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("the backup of %s holds the wrong bytes: got %d, want %d", tc.backup, len(got), len(tc.want))
		}
	}
}

// TestSaveSlotProtected_PrunesOnlyItsOwnSlot re-asserts the #2413 guard the multi-name
// backup could easily have regressed: backing up several names must still not disturb a
// NUMBERED slot's backup.
//
// The prune is one-deep and SLOT-scoped for a reason. A per-TYPE prune (BackupSingleSlot's)
// would wipe the primary's backup while backing up fanart1. Looping the backup over more
// names multiplies the number of prunes, so this is the test that catches a loop that
// prunes the whole type directory.
func TestSaveSlotProtected_PrunesOnlyItsOwnSlot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	numberedOrig := makeJPEG(t, 70, 45)
	if err := os.WriteFile(filepath.Join(dir, "fanart1.jpg"), numberedOrig, 0o644); err != nil {
		t.Fatalf("seeding the numbered slot: %v", err)
	}
	// Give the numbered slot a backup by overwriting it through the chokepoint.
	if _, err := SaveSlotProtected(dir, "fanart", []string{"fanart1.jpg"}, makeJPEG(t, 71, 46), false, nil, discardLogger()); err != nil {
		t.Fatalf("seeding the numbered slot's backup: %v", err)
	}

	// Now do a MULTI-NAME primary overwrite. Its backups and prunes must leave fanart1 alone.
	fanartOrig := makeJPEG(t, 80, 50)
	backdropOrig := makeJPEG(t, 60, 40)
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), fanartOrig, 0o644); err != nil {
		t.Fatalf("seeding fanart.jpg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backdrop.jpg"), backdropOrig, 0o644); err != nil {
		t.Fatalf("seeding backdrop.jpg: %v", err)
	}
	if _, err := SaveSlotProtected(dir, "fanart", []string{"fanart.jpg", "backdrop.jpg"},
		makeJPEG(t, 200, 120), false, nil, discardLogger()); err != nil {
		t.Fatalf("SaveSlotProtected: %v", err)
	}

	got, readErr := os.ReadFile(filepath.Join(dir, BackupDirName, "fanart", "fanart1.jpg"))
	if readErr != nil {
		t.Fatalf("the numbered slot's backup was DESTROYED by a primary overwrite (%v); the prune "+
			"must stay scoped to the slot it backed up (#2413)", readErr)
	}
	if !bytes.Equal(got, numberedOrig) {
		t.Error("the numbered slot's backup no longer holds its own original bytes")
	}
	// And the primary's own backups are still there.
	for _, b := range []string{"fanart.jpg", "backdrop.jpg"} {
		if _, statErr := os.Stat(filepath.Join(dir, BackupDirName, "fanart", b)); statErr != nil {
			t.Errorf("the primary overwrite left no backup of %s: %v", b, statErr)
		}
	}
}

// TestSaveSlotProtected_ThreadsUseSymlinksIntoTheSave is the #2446 guard.
//
// SaveSlotProtected is exported and takes a GENERIC imageType, but it hardcoded
// useSymlinks=false in both the save and the restore. That made the signature a lie: a
// non-fanart caller (the generic chokepoint is the entire reason it lives in
// internal/image, reachable by internal/rule) would silently get non-symlink semantics.
//
// The flag is only OBSERVABLE for a non-fanart type: Save computes
// symlinkEligible := useSymlinks && imageType != "fanart", so no fanart test can tell
// the two apart -- which is precisely how the hardcoded false hid. So this drives "thumb"
// with two names and asserts the second is a real SYMLINK. With the hardcoded false it is
// a regular file, and this test goes RED.
//
// NOT covered, honestly: RestoreSlot's useSymlinks is threaded for symmetry but is not
// behaviorally observable today -- it restores ONE name, which takes Save's i == 0 branch
// (a real file) no matter what the flag says. It is threaded so the rollback cannot
// disagree with the save if that ever changes; the compiler, not this test, is what holds
// it in place.
func TestSaveSlotProtected_ThreadsUseSymlinksIntoTheSave(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	naming := []string{"folder.jpg", "artist.jpg"}
	if _, err := SaveSlotProtected(dir, "thumb", naming, makeJPEG(t, 120, 120), true, nil, discardLogger()); err != nil {
		t.Fatalf("SaveSlotProtected: %v", err)
	}

	info, err := os.Lstat(filepath.Join(dir, "artist.jpg"))
	if err != nil {
		t.Fatalf("the secondary name was not written at all: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the secondary name is a REGULAR FILE despite useSymlinks=true: the chokepoint is not " +
			"forwarding the caller's flag to Save, so its signature is lying to every non-fanart caller (#2446)")
	}

	// The control: the same call with useSymlinks=false must write a real file. Without
	// this, the assertion above would also pass on code that hardcoded symlinks ON.
	plain := t.TempDir()
	if _, err := SaveSlotProtected(plain, "thumb", naming, makeJPEG(t, 120, 120), false, nil, discardLogger()); err != nil {
		t.Fatalf("SaveSlotProtected: %v", err)
	}
	plainInfo, err := os.Lstat(filepath.Join(plain, "artist.jpg"))
	if err != nil {
		t.Fatalf("the secondary name was not written at all: %v", err)
	}
	if plainInfo.Mode()&os.ModeSymlink != 0 {
		t.Error("useSymlinks=false produced a symlink; the flag is not being forwarded, it is hardcoded on")
	}
}

// TestSaveSlotProtected_UnprotectableNameIsRefusedNotSilentlySkipped: a naming entry with
// a path separator cannot be keyed into the flat .sw-backup/<type>/ dir -- the backup is
// stored under filepath.Base(original), so "sub/fanart.jpg" would write straight over the
// TOP-LEVEL fanart.jpg's backup and destroy the primary's only recovery copy.
//
// ValidateImageNaming rejects "/" and "\" in a configured filename, so this cannot come
// from a validated profile. When such a name is the ONLY one, there is no protectable
// slot at all, and the save must ABORT rather than run unprotected -- the same #1161
// contract as a backup that cannot be taken.
func TestSaveSlotProtected_UnprotectableNameIsRefusedNotSilentlySkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	original := makeJPEG(t, 80, 50)
	if err := os.WriteFile(filepath.Join(dir, "sub", "fanart.jpg"), original, 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	_, err := SaveSlotProtected(dir, "fanart", []string{"sub/fanart.jpg"}, makeJPEG(t, 120, 90), false, nil, discardLogger())
	if err == nil {
		t.Fatal("a save with no backup-protectable name PROCEEDED; it must abort instead")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("aborting destructive save")) {
		t.Errorf("error does not say the save was aborted: %v", err)
	}

	// THE ASSERTION: it aborted before touching the image.
	got, readErr := os.ReadFile(filepath.Join(dir, "sub", "fanart.jpg"))
	if readErr != nil {
		t.Fatalf("the original was destroyed by a save that could not protect it: %v", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Error("the original was modified by a save that could not protect it")
	}
}
