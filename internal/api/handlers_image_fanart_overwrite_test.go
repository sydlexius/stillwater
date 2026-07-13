package api

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/watcher"
)

// TestProcessAndSaveImage_FanartOverwrite_BacksUpTheOriginal is the #2413 guard.
//
// Overwriting a fanart image destroyed the existing file with NO backup and NO
// rollback. Every other image type was protected; fanart was carved out of BOTH
// protections by a single `imageType != "fanart"` condition, justified by a comment
// that described the APPEND path ("append writes a new numbered file") while sitting
// in the OVERWRITE path. Appending is a different function entirely.
//
// The destruction is a DELETE, not an overwrite, which is why an atomic write never
// saved anyone: img.Save calls CleanupConflictingFormats, which REMOVES the other
// format of the same slot before writing. Replace a fanart.png with JPEG data and the
// canonical name becomes fanart.jpg -- so the user's original fanart.png is deleted
// outright. With no backup there is nothing to revert to, and if the subsequent write
// then fails the artwork is simply gone: "the old image goes away, and the new one
// never appears", exactly as reported from prod.
//
// The backup machinery already supported fanart (it is in image.backupImageTypes). It
// was simply never called for it.
//
// This test goes RED against the pre-fix code.
func TestProcessAndSaveImage_FanartOverwrite_BacksUpTheOriginal(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	dir := t.TempDir()

	// The user's existing artwork, as a PNG. This is the thing that must survive.
	original := testPNG(t, 80, 50)
	if err := os.WriteFile(filepath.Join(dir, "fanart.png"), original, 0o644); err != nil {
		t.Fatalf("seeding the original fanart: %v", err)
	}

	// Overwrite it with JPEG data. The canonical name becomes fanart.jpg, so the
	// original fanart.png is DELETED as a conflicting format.
	saved, err := r.processAndSaveImage(context.Background(), dir, "fanart", jpegBytes(t, 120, 90), nil)
	if err != nil {
		t.Fatalf("processAndSaveImage: %v", err)
	}
	if len(saved) == 0 {
		t.Fatal("nothing was saved")
	}

	// Precondition: assert the original really was destroyed, or this test proves
	// nothing about backups.
	if _, statErr := os.Stat(filepath.Join(dir, "fanart.png")); !os.IsNotExist(statErr) {
		t.Fatalf("precondition: the original fanart.png should have been removed as a conflicting format "+
			"(stat err = %v); if it survives, this test is not exercising the destructive path", statErr)
	}

	// THE ASSERTION THAT MATTERS. The original was destroyed, so a recoverable backup
	// of it must exist -- the same contract thumb, logo and banner already get.
	backup := filepath.Join(dir, img.BackupDirName, "fanart", "fanart.png")
	got, readErr := os.ReadFile(backup)
	if readErr != nil {
		t.Fatalf("the original fanart was DESTROYED with NO BACKUP (%v). "+
			"A fanart overwrite must back the original up before it deletes it, exactly as thumb, logo and "+
			"banner already do -- otherwise the user's artwork is unrecoverable (#2413)", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("the backup is not the user's original: len(got)=%d len(want)=%d", len(got), len(original))
	}
}

// TestProcessAndSaveImage_FanartOverwrite_UndecodableDataDestroysNothing asserts the
// ABORT-BEFORE-DESTROY property: a payload that cannot be decoded must fail the save
// without having touched the user's artwork.
//
// READ THE NAME LITERALLY. This test does NOT exercise the rollback, and an earlier
// version of it that claimed to ("...RestoresOnFailedSave") was lying: undecodable bytes
// fail in ConvertFormat, upstream of the img.Save call whose CleanupConflictingFormats
// is what actually deletes the original. The original therefore survives with OR without
// the RestoreSlot block, and that version PASSED against main's unfixed code -- a guard
// that cannot fail on the broken code guards nothing.
//
// The real rollback guard is TestSaveSlotProtected_RollsBackAFailedWrite in
// internal/image, which injects a fault that lands AFTER the destructive delete and goes
// RED when the rollback is removed.
func TestProcessAndSaveImage_FanartOverwrite_UndecodableDataDestroysNothing(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	dir := t.TempDir()
	original := jpegBytes(t, 80, 50)
	primary := filepath.Join(dir, "fanart.jpg")
	if err := os.WriteFile(primary, original, 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Undecodable bytes: ConvertFormat rejects them before img.Save runs, so nothing is
	// written -- and, crucially, nothing is deleted either, because the destructive
	// CleanupConflictingFormats is never reached.
	_, err := r.processAndSaveImage(context.Background(), dir, "fanart", []byte("not an image"), nil)
	if err == nil {
		t.Fatal("expected undecodable data to fail")
	}

	got, readErr := os.ReadFile(primary)
	if readErr != nil {
		t.Fatalf("the original fanart is GONE after a failed overwrite (%v)", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("the original fanart was not preserved through a failed overwrite")
	}
}

// TestSaveFanartSlotProtected_BacksUpTheOriginal is the #2413 CRITICAL-1 guard.
//
// The per-slot Crop and Fetch/Replace controls in the artwork gallery called img.Save
// DIRECTLY -- no backup, no rollback -- and FanartFilename(primary, 0, kodi) returns
// the primary name verbatim, so SLOT 0 IS THE PRIMARY BACKDROP. The flagship
// "overwrite this backdrop" affordance destroyed the user's artwork even after
// processAndSaveImage was fixed. This drives the shared chokepoint those handlers now
// use, through the Router wrapper the handlers actually call.
//
// SCOPE: this asserts the BACKUP half (a successful overwrite leaves a recoverable
// original) plus abort-before-destroy on undecodable input. It does NOT assert the
// rollback -- see TestSaveSlotProtected_RollsBackAFailedWrite in internal/image, which
// is the only test that can go red when the rollback is removed.
func TestSaveFanartSlotProtected_BacksUpTheOriginal(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	dir := t.TempDir()
	original := jpegBytes(t, 80, 50)
	slot := "fanart.jpg" // slot 0 == the primary backdrop
	if err := os.WriteFile(filepath.Join(dir, slot), original, 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// A successful overwrite must leave a recoverable backup of the original.
	if _, err := r.saveFanartSlotProtected(context.Background(), dir, []string{slot}, jpegBytes(t, 120, 90), nil); err != nil {
		t.Fatalf("saveFanartSlotProtected: %v", err)
	}
	backup := filepath.Join(dir, img.BackupDirName, "fanart", slot)
	gotBackup, readErr := os.ReadFile(backup)
	if readErr != nil {
		t.Fatalf("a fanart SLOT overwrite left NO BACKUP (%v). Slot 0 is the primary backdrop; "+
			"the gallery's Crop and Fetch controls destroy it (#2413)", readErr)
	}
	if !bytes.Equal(gotBackup, original) {
		t.Error("the slot backup is not the user's original")
	}

	// And an undecodable payload must be rejected without destroying what is on disk.
	// This is ABORT-BEFORE-DESTROY, not rollback: img.Save bails out in DetectFormat,
	// before CleanupConflictingFormats can delete anything. Do not mistake this for a
	// rollback assertion -- it holds with the rollback removed.
	current, _ := os.ReadFile(filepath.Join(dir, slot))
	if _, err := r.saveFanartSlotProtected(context.Background(), dir, []string{slot}, []byte("not an image"), nil); err == nil {
		t.Fatal("expected undecodable data to fail")
	}
	survived, readErr := os.ReadFile(filepath.Join(dir, slot))
	if readErr != nil {
		t.Fatalf("the fanart slot is GONE after a save that failed before it wrote anything (%v)", readErr)
	}
	if !bytes.Equal(survived, current) {
		t.Error("a save that failed before writing still changed the image on disk")
	}
}

// expectedWriteProbe is a slog.Handler that samples the expected-writes tracker at the
// moment img.Save logs a completed write. Registration is added and removed around the
// save (AddAll + deferred RemoveAll), so it is invisible once the call returns -- the
// only honest place to observe it is DURING the write, and Save's per-file "saved image"
// record carries the exact path it just wrote.
type expectedWriteProbe struct {
	tracker  *watcher.ExpectedWrites
	mu       sync.Mutex
	expected map[string]bool // path written -> was it registered at write time
}

func (p *expectedWriteProbe) Enabled(context.Context, slog.Level) bool { return true }
func (p *expectedWriteProbe) WithAttrs([]slog.Attr) slog.Handler       { return p }
func (p *expectedWriteProbe) WithGroup(string) slog.Handler            { return p }

func (p *expectedWriteProbe) Handle(_ context.Context, rec slog.Record) error {
	if rec.Message != "saved image" {
		return nil
	}
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key != "path" {
			return true
		}
		path := a.Value.String()
		p.mu.Lock()
		p.expected[path] = p.tracker.IsExpected(path)
		p.mu.Unlock()
		return false
	})
	return nil
}

// TestSaveFanartSlotProtected_RegistersExpectedWrites.
//
// Before the chokepoint existed, processAndSaveImage registered every path a fanart save
// was about to touch with the filesystem watcher, so the watcher could tell Stillwater's
// own write apart from a user editing the file behind its back. Routing fanart through
// the chokepoint returned EARLY, above that registration block, and silently dropped it.
//
// The loss is inert today (nothing in production calls IsExpected yet) and becomes real
// the moment the watcher consumes it -- at which point Stillwater's own backdrop writes
// would look like external edits and kick off a rescan. Restored, and asserted here at
// the only observable moment: while the write is happening.
func TestSaveFanartSlotProtected_RegistersExpectedWrites(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	dir := t.TempDir()
	slot := "fanart.jpg"
	tracker := watcher.NewExpectedWrites()
	probe := &expectedWriteProbe{tracker: tracker, expected: map[string]bool{}}
	r.expectedWrites = tracker
	r.logger = slog.New(probe)

	if _, err := r.saveFanartSlotProtected(context.Background(), dir, []string{slot}, jpegBytes(t, 120, 90), nil); err != nil {
		t.Fatalf("saveFanartSlotProtected: %v", err)
	}

	target := filepath.Join(dir, slot)
	probe.mu.Lock()
	defer probe.mu.Unlock()
	seen, wrote := probe.expected[target]
	if !wrote {
		t.Fatalf("precondition: %s was never written, so there was nothing to register", target)
	}
	if !seen {
		t.Errorf("the fanart write to %s was NOT registered with the expected-writes tracker. "+
			"main registered it; routing fanart through the chokepoint dropped the registration, so "+
			"the filesystem watcher would read Stillwater's own backdrop write as an external edit", target)
	}
}

// TestBackupSlot_DoesNotClobberAnotherSlotsBackup: fanart is MULTI-slot, and
// BackupSingleSlot's prune is one-deep PER TYPE -- it deletes everything in the type's
// backup dir but the file it just wrote. Reusing it per-slot would mean backing up
// fanart1.jpg DELETES the primary's backup, trading one data-loss bug for another.
func TestBackupSlot_DoesNotClobberAnotherSlotsBackup(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	dir := t.TempDir()
	primaryOrig := jpegBytes(t, 80, 50)
	slot1Orig := jpegBytes(t, 60, 40)
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), primaryOrig, 0o644); err != nil {
		t.Fatalf("seeding primary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fanart1.jpg"), slot1Orig, 0o644); err != nil {
		t.Fatalf("seeding slot 1: %v", err)
	}

	if _, err := r.saveFanartSlotProtected(context.Background(), dir, []string{"fanart.jpg"}, jpegBytes(t, 200, 100), nil); err != nil {
		t.Fatalf("overwriting the primary: %v", err)
	}
	if _, err := r.saveFanartSlotProtected(context.Background(), dir, []string{"fanart1.jpg"}, jpegBytes(t, 210, 110), nil); err != nil {
		t.Fatalf("overwriting slot 1: %v", err)
	}

	// BOTH backups must survive. A per-type prune would have deleted the primary's.
	gotPrimary, err := os.ReadFile(filepath.Join(dir, img.BackupDirName, "fanart", "fanart.jpg"))
	if err != nil {
		t.Fatalf("the PRIMARY's backup was destroyed by backing up another slot (%v) -- "+
			"the one-deep per-type prune is wrong for a multi-slot type", err)
	}
	if !bytes.Equal(gotPrimary, primaryOrig) {
		t.Error("the primary's backup is not the primary's original")
	}
	gotSlot1, err := os.ReadFile(filepath.Join(dir, img.BackupDirName, "fanart", "fanart1.jpg"))
	if err != nil {
		t.Fatalf("slot 1's backup is missing: %v", err)
	}
	if !bytes.Equal(gotSlot1, slot1Orig) {
		t.Error("slot 1's backup is not slot 1's original")
	}
}

// TestBackupSlot_PrunesAStaleBackupSoDeletedArtDoesNotResurrect is the #2413
// CRITICAL-2 guard (a regression introduced by the first fix round).
//
// Backups outlived the images they protected: overwrite a fanart (backup written),
// DELETE the fanart, then start another save that fails -- the rollback restored the
// stale backup and the artwork the user threw away came back on its own.
func TestBackupSlot_PrunesAStaleBackupSoDeletedArtDoesNotResurrect(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	dir := t.TempDir()
	slot := "fanart.jpg"
	if err := os.WriteFile(filepath.Join(dir, slot), jpegBytes(t, 80, 50), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	// Overwrite -> a backup now exists.
	if _, err := r.saveFanartSlotProtected(context.Background(), dir, []string{slot}, jpegBytes(t, 120, 90), nil); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	// PRECONDITION. Without this the whole test can pass vacuously: "the deleted art did
	// not come back" proves nothing if no backup was ever written to come back FROM.
	// Assert the backup FILE itself, not just that some backup exists for the type.
	staleBackup := filepath.Join(dir, img.BackupDirName, "fanart", slot)
	if _, err := os.Stat(staleBackup); err != nil {
		t.Fatalf("precondition: the overwrite should have left a backup at %s, stat err = %v", staleBackup, err)
	}

	// The user deletes the artwork.
	if err := os.Remove(filepath.Join(dir, slot)); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	// A later save fails. The rollback must NOT resurrect the deleted image.
	if _, err := r.saveFanartSlotProtected(context.Background(), dir, []string{slot}, []byte("not an image"), nil); err == nil {
		t.Fatal("expected undecodable data to fail")
	}

	// The MECHANISM: BackupSlot found no original and PRUNED the stale backup, so there
	// is nothing left for any later rollback to restore. Asserting the pruned file
	// directly -- rather than only the absence of the restored artwork -- is what makes
	// this a test of the fix and not merely of this one save's failure mode.
	if _, statErr := os.Stat(staleBackup); !os.IsNotExist(statErr) {
		t.Errorf("the stale backup file %s SURVIVED (stat err = %v). It outlived the image it "+
			"protected, so a later failed save can still resurrect deleted artwork (#2413)",
			staleBackup, statErr)
	}
	if img.HasBackup(dir, "fanart") {
		t.Error("a fanart backup still exists after the original was deleted")
	}

	// The OUTCOME: the artwork the user threw away stayed thrown away.
	if _, statErr := os.Stat(filepath.Join(dir, slot)); !os.IsNotExist(statErr) {
		t.Errorf("the DELETED fanart was RESURRECTED from a stale backup (stat err = %v). "+
			"A backup must not outlive the image it protects (#2413)", statErr)
	}
}

// TestPrimaryOverwriteDoesNotWipeNumberedSlotBackups.
//
// fanart.jpg (non-numbered) is the PRIMARY backdrop, so "replace the main backdrop" is
// the most common destructive action there is. It used to route through
// BackupSingleSlot, whose prune is one-deep PER TYPE: it deletes every file in
// .sw-backup/fanart/ except the one it just wrote. So overwriting the primary silently
// DESTROYED the backups of fanart1.jpg, fanart2.jpg and every other slot -- disarming
// their protection without a word.
//
// One image type, one backup mechanism: fanart is slot-scoped everywhere.
func TestPrimaryOverwriteDoesNotWipeNumberedSlotBackups(t *testing.T) {
	t.Parallel()
	r, _ := newImageHandlerTestServer(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), jpegBytes(t, 80, 50), 0o644); err != nil {
		t.Fatalf("seeding primary: %v", err)
	}
	slot1Orig := jpegBytes(t, 60, 40)
	if err := os.WriteFile(filepath.Join(dir, "fanart1.jpg"), slot1Orig, 0o644); err != nil {
		t.Fatalf("seeding slot 1: %v", err)
	}

	// Overwrite slot 1 -> its backup exists.
	if _, err := r.saveFanartSlotProtected(context.Background(), dir, []string{"fanart1.jpg"}, jpegBytes(t, 200, 100), nil); err != nil {
		t.Fatalf("overwriting slot 1: %v", err)
	}

	// Now overwrite the PRIMARY through the main entry point.
	if _, err := r.processAndSaveImage(context.Background(), dir, "fanart", jpegBytes(t, 300, 150), nil); err != nil {
		t.Fatalf("overwriting the primary: %v", err)
	}

	// Slot 1's backup must SURVIVE the primary's overwrite.
	got, err := os.ReadFile(filepath.Join(dir, img.BackupDirName, "fanart", "fanart1.jpg"))
	if err != nil {
		t.Fatalf("overwriting the PRIMARY backdrop destroyed slot 1's backup (%v). "+
			"The per-type prune wipes every other slot's safety net on the most common "+
			"destructive action in the product (#2413)", err)
	}
	if !bytes.Equal(got, slot1Orig) {
		t.Error("slot 1's backup is no longer slot 1's original")
	}
}
