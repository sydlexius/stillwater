package image

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedOriginalWithAFailingSecondName sets up the ONE fault that actually reaches the
// rollback, and returns the artist dir, the original bytes, and the naming list.
//
// Getting here matters more than it looks. The obvious way to fail a save is to feed
// it undecodable bytes -- and that is USELESS as a rollback test, because Save rejects
// them in DetectFormat BEFORE CleanupConflictingFormats deletes anything. The original
// survives whether or not a rollback exists, so such a test passes against the broken
// code. Two tests on this branch were written that way and guarded nothing (#2413).
//
// The fault used here instead lands AFTER the destructive delete:
//
//	naming[0] = "fanart.jpg"          -- decodes fine; Save's cleanup DELETES fanart.png
//	naming[1] = "blocked/fanart.jpg"  -- "blocked" is a regular FILE, so the write to it
//	                                     fails ENOTDIR (a second configured filename that
//	                                     cannot be written is an ordinary IO failure)
//
// So Save returns an error only once the user's original is already gone, which is the
// precise state the rollback exists for. TestSaveUnprotected_DestroysTheOriginal below
// asserts that precondition directly rather than taking this comment's word for it.
func seedOriginalWithAFailingSecondName(t *testing.T) (dir string, original []byte, naming []string) {
	t.Helper()
	dir = t.TempDir()

	original = makePNG(t, 80, 50)
	if err := os.WriteFile(filepath.Join(dir, "fanart.png"), original, 0o644); err != nil {
		t.Fatalf("seeding the original fanart.png: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocked"), []byte("a regular file, not a directory"), 0o644); err != nil {
		t.Fatalf("seeding the write barrier: %v", err)
	}
	return dir, original, []string{"fanart.jpg", "blocked/fanart.jpg"}
}

// TestSaveUnprotected_DestroysTheOriginal is the PRECONDITION for the rollback test
// below: it proves the fault injected by seedOriginalWithAFailingSecondName really does
// destroy the user's artwork when nothing rolls it back.
//
// Without this, TestSaveSlotProtected_RollsBackAFailedWrite could be silently vacuous --
// "the original survived" means nothing if the original was never in danger.
func TestSaveUnprotected_DestroysTheOriginal(t *testing.T) {
	t.Parallel()
	dir, _, naming := seedOriginalWithAFailingSecondName(t)

	// A bare Save: no backup, no rollback. This is what every fanart write used to do.
	_, err := Save(dir, "fanart", makeJPEG(t, 120, 90), naming, false, nil, discardLogger())
	if err == nil {
		t.Fatal("the fault did not fail the save; the rollback test built on it would be vacuous")
	}

	if _, statErr := os.Stat(filepath.Join(dir, "fanart.png")); !os.IsNotExist(statErr) {
		t.Fatalf("the unprotected save did NOT destroy the original (stat err = %v). "+
			"The fault does not land after CleanupConflictingFormats, so it does not reach the "+
			"rollback path and cannot test it", statErr)
	}
}

// TestSaveSlotProtected_RollsBackAFailedWrite is the #2413 rollback guard -- the one
// that goes RED when the RestoreSlot block is deleted from SaveSlotProtected.
//
// It asserts the OUTCOME: the user's original bytes are back on disk. Not an error
// value, not an exit code. Stillwater's dominant bug class is reporting success while
// doing nothing, and this branch's first two rollback "guards" were exactly that.
func TestSaveSlotProtected_RollsBackAFailedWrite(t *testing.T) {
	t.Parallel()
	dir, original, naming := seedOriginalWithAFailingSecondName(t)

	_, err := SaveSlotProtected(dir, "fanart", naming, makeJPEG(t, 120, 90), false, nil, discardLogger())
	if err == nil {
		t.Fatal("expected the unwritable second filename to fail the save")
	}

	// THE ASSERTION THAT MATTERS. Save deleted fanart.png as a conflicting format and
	// then failed. The rollback must have put it back, byte for byte.
	got, readErr := os.ReadFile(filepath.Join(dir, "fanart.png"))
	if readErr != nil {
		t.Fatalf("the user's original fanart is GONE after a failed overwrite (%v). "+
			"The save deleted it as a conflicting format and then failed, and nothing restored it -- "+
			"the artwork is unrecoverable, which is #2413 itself", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("the restored fanart is not the user's original: got %d bytes, want %d",
			len(got), len(original))
	}

	// The half-written post-edit format must not survive alongside it. RestoreSlot goes
	// back through Save so CleanupConflictingFormats drops the fanart.jpg the failed
	// edit left behind; otherwise the gallery still shows the edit that failed.
	if _, statErr := os.Stat(filepath.Join(dir, "fanart.jpg")); !os.IsNotExist(statErr) {
		t.Errorf("the failed edit's fanart.jpg survived the rollback (stat err = %v); "+
			"the restore must leave the slot exactly as it found it", statErr)
	}
}

// TestSaveSlotProtected_ABackupItCannotTakeAbortsTheSave is the #1161 invariant, and it
// is the whole point of the chokepoint: WE NEVER DESTROY AN ORIGINAL WE COULD NOT
// PROTECT. If the backup cannot be written, the destructive save must not run at all --
// abort with the artwork still on disk, rather than delete it and hope the write lands.
//
// Each case breaks the backup in a different way, and every one asserts the same
// outcome: an error, and the user's original untouched.
func TestSaveSlotProtected_ABackupItCannotTakeAbortsTheSave(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// breakBackup sabotages the backup for a dir that already holds fanart.jpg,
		// and returns the image type to save under.
		breakBackup func(t *testing.T, dir string) (imageType string)
	}{
		{
			// backupTypeDir rejects anything outside the artwork-kind allowlist, so the
			// backup path cannot even be computed.
			name:        "the image type has no valid backup path",
			breakBackup: func(*testing.T, string) string { return "not-an-artwork-kind" },
		},
		{
			// .sw-backup/fanart is a regular FILE, so MkdirAll of the backup dir fails
			// ENOTDIR. (A real-world shape: a stray file where the backup dir belongs.)
			name: "the backup directory is a file",
			breakBackup: func(t *testing.T, dir string) string {
				t.Helper()
				typeDir := filepath.Join(dir, BackupDirName, "fanart")
				if err := os.MkdirAll(filepath.Dir(typeDir), 0o750); err != nil {
					t.Fatalf("seeding: %v", err)
				}
				if err := os.WriteFile(typeDir, []byte("not a directory"), 0o644); err != nil {
					t.Fatalf("seeding: %v", err)
				}
				return "fanart"
			},
		},
		{
			// The "original" is a DIRECTORY: it stats as present, so the strict probe
			// finds it, but reading its bytes fails EISDIR. A stat error must never be
			// mistaken for "nothing to back up" (#1161) -- that is exactly how a
			// still-present original gets overwritten with no backup.
			name: "the original cannot be read",
			breakBackup: func(t *testing.T, dir string) string {
				t.Helper()
				slot := filepath.Join(dir, "fanart.jpg")
				if err := os.Remove(slot); err != nil {
					t.Fatalf("clearing the seeded slot: %v", err)
				}
				if err := os.Mkdir(slot, 0o750); err != nil {
					t.Fatalf("seeding an unreadable original: %v", err)
				}
				return "fanart"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			original := makeJPEG(t, 80, 50)
			slot := filepath.Join(dir, "fanart.jpg")
			if err := os.WriteFile(slot, original, 0o644); err != nil {
				t.Fatalf("seeding the original: %v", err)
			}

			imageType := tc.breakBackup(t, dir)

			_, err := SaveSlotProtected(dir, imageType, []string{"fanart.jpg"},
				makeJPEG(t, 120, 90), false, nil, discardLogger())
			if err == nil {
				t.Fatal("the save PROCEEDED despite an unusable backup; it must abort instead")
			}
			if !strings.Contains(err.Error(), "aborting destructive save") {
				t.Errorf("error does not say the save was aborted: %v", err)
			}

			// THE ASSERTION: the artwork is still there. An abort that still destroys the
			// original is not an abort.
			got, readErr := os.ReadFile(slot)
			if readErr != nil {
				// The unreadable-original case seeded a directory, not the file, so there
				// are no bytes to compare -- but the entry must still exist.
				if _, statErr := os.Stat(slot); statErr != nil {
					t.Fatalf("the original was DESTROYED by a save that could not back it up (%v)", statErr)
				}
				return
			}
			if !bytes.Equal(got, original) {
				t.Error("the original was modified by a save that could not back it up")
			}
		})
	}
}

// TestSaveSlotProtected_ReportsAFailedRollbackAsFailed is the worst case: the save
// failed AND the restore failed, so the artwork is gone and Stillwater could not put it
// back. That MUST NOT be reported as an ordinary save failure -- the user needs to know
// manual recovery may be needed, because nothing else is going to tell them.
//
// Contrast TestSaveSlotProtected_FirstEverWriteIsNotAFailedRollback: a missing backup on
// a first-ever write is NOT this, and must not cry wolf.
func TestSaveSlotProtected_ReportsAFailedRollbackAsFailed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// The slot on disk is NOT a decodable image (a truncated download, a corrupt file).
	// It gets backed up verbatim -- BackupSlot copies bytes, it does not decode them --
	// so when the rollback tries to restore it THROUGH Save, Save rejects it. The
	// restore is the thing that fails.
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), []byte("corrupt, undecodable"), 0o644); err != nil {
		t.Fatalf("seeding a corrupt original: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blocked"), []byte("a regular file"), 0o644); err != nil {
		t.Fatalf("seeding the write barrier: %v", err)
	}

	_, err := SaveSlotProtected(dir, "fanart", []string{"fanart.jpg", "blocked/fanart.jpg"},
		makeJPEG(t, 120, 90), false, nil, discardLogger())
	if err == nil {
		t.Fatal("expected the unwritable second filename to fail the save")
	}
	if !strings.Contains(err.Error(), "could not be restored") {
		t.Errorf("a save AND its rollback both failed, but the error reads like an ordinary save "+
			"failure (%v). The original is gone and could not be put back -- say so, or the user "+
			"never learns their artwork needs manual recovery", err)
	}
}

// TestSaveSlotProtected_NilLoggerDoesNotPanic is the cross-package-caller guard: a
// caller in another package (internal/rule, #2433) may not have a logger in hand, and
// SaveSlotProtected must accept nil rather than panic.
//
// Both paths are covered because only one of them can panic. The success path never
// touches logger at all once wrapped, so a success-only test would prove nothing; the
// failed-rollback path is the one that calls logger.Error directly, and that is where a
// nil logger actually panics without the guard.
func TestSaveSlotProtected_NilLoggerDoesNotPanic(t *testing.T) {
	t.Parallel()

	t.Run("success path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if _, err := SaveSlotProtected(dir, "fanart", []string{"fanart.jpg"}, makeJPEG(t, 40, 30), false, nil, nil); err != nil {
			t.Fatalf("unexpected error with a nil logger: %v", err)
		}
	})

	t.Run("failed-rollback path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// A corrupt, undecodable original: BackupSlot copies it verbatim, but the
		// rollback's restore-through-Save rejects it, so the RESTORE itself fails.
		// That is the branch that calls logger.Error directly (see
		// TestSaveSlotProtected_ReportsAFailedRollbackAsFailed) -- the one a nil
		// logger would panic on without the guard.
		if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), []byte("corrupt, undecodable"), 0o644); err != nil {
			t.Fatalf("seeding a corrupt original: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "blocked"), []byte("a regular file"), 0o644); err != nil {
			t.Fatalf("seeding the write barrier: %v", err)
		}

		_, err := SaveSlotProtected(dir, "fanart", []string{"fanart.jpg", "blocked/fanart.jpg"},
			makeJPEG(t, 120, 90), false, nil, nil)
		if err == nil {
			t.Fatal("expected the unwritable second filename to fail the save")
		}
		if !strings.Contains(err.Error(), "could not be restored") {
			t.Errorf("expected a failed-rollback error, got: %v", err)
		}
	})
}

// TestSaveSlotProtected_NoConfiguredNames: naming[0] is the backup key, so an empty
// naming list has no slot to protect. Fail rather than guess.
func TestSaveSlotProtected_NoConfiguredNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := SaveSlotProtected(dir, "fanart", nil, makeJPEG(t, 10, 10), false, nil, discardLogger())
	if err == nil {
		t.Fatal("expected an empty naming list to fail")
	}
	// Assert the SPECIFIC error. "some error came back" would also be satisfied by a
	// backup failure or a save failure, which are different bugs with different fixes --
	// and by a nil-deref panic recovered somewhere upstream.
	const want = `no filenames configured for image type "fanart"`
	if err.Error() != want {
		t.Errorf("wrong error for an empty naming list:\n got: %v\nwant: %s", err, want)
	}
	// And it must fail BEFORE touching the filesystem: with no slot named, there is
	// nothing it could legitimately back up or write.
	if HasBackup(dir, "fanart") {
		t.Error("an empty naming list wrote a backup; it should have failed before any filesystem work")
	}
}

// TestSaveSlotProtected_FirstEverWriteIsNotAFailedRollback: a slot with no prior image
// has nothing to back up, so a failed save finds no backup. That is os.ErrNotExist from
// RestoreSlot and it is NOT a rollback failure -- nothing was lost. The error must
// report the save failure plainly, without the "manual recovery may be needed" wording
// that a genuinely-failed restore earns.
func TestSaveSlotProtected_FirstEverWriteIsNotAFailedRollback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blocked"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seeding the write barrier: %v", err)
	}
	naming := []string{"fanart.jpg", "blocked/fanart.jpg"}

	_, err := SaveSlotProtected(dir, "fanart", naming, makeJPEG(t, 120, 90), false, nil, discardLogger())
	if err == nil {
		t.Fatal("expected the unwritable second filename to fail the save")
	}
	if got := err.Error(); bytes.Contains([]byte(got), []byte("could not be restored")) {
		t.Errorf("a first-ever write reported a FAILED ROLLBACK (%q), but there was no prior "+
			"image to restore and nothing was lost", got)
	}
}
