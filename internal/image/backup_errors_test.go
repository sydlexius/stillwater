package image

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// thumbNaming is the canonical filename list used across the backup error-path
// tests below. The first name is the primary the strict probe locates.
var errPathThumbNaming = []string{"folder.jpg", "folder.png"}

// seedOriginal writes a valid JPEG primary so BackupSingleSlot's strict probe
// finds an original to back up (the no-original case short-circuits before the
// error branches under test).
func seedOriginal(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), makeImageBytes(t, "jpeg"), 0o644); err != nil {
		t.Fatalf("seeding original: %v", err)
	}
}

// TestBackupSingleSlot_BackupDirIsFile covers the error branches that fire when
// the per-type backup directory cannot be read or created because a non-directory
// already occupies the path: removeBackupFiles' ReadDir error (not os.IsNotExist)
// and MkdirAll's failure.
func TestBackupSingleSlot_BackupDirIsFile(t *testing.T) {
	dir := t.TempDir()
	seedOriginal(t, dir)

	// Make the top-level .sw-backup a regular FILE so MkdirAll of
	// .sw-backup/thumb cannot create the parent and ReadDir of the type dir
	// surfaces a non-NotExist error during cleanup.
	if err := os.WriteFile(filepath.Join(dir, BackupDirName), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("seeding .sw-backup as file: %v", err)
	}

	err := BackupSingleSlot(dir, "thumb", errPathThumbNaming)
	if err == nil {
		t.Fatal("BackupSingleSlot must error when .sw-backup is a regular file")
	}
}

// TestBackupSingleSlot_FailedWritePreservesPriorBackup proves the Qodo #1839
// reliability fix: BackupSingleSlot writes the fresh backup BEFORE pruning prior
// backups, so when the write fails the previous backup survives and the edit
// still has a revert path. A read-only per-type dir makes WriteFileAtomic fail
// (it cannot create its tmp sibling) while MkdirAll of the existing dir succeeds.
func TestBackupSingleSlot_FailedWritePreservesPriorBackup(t *testing.T) {
	// Root (UID 0, e.g. Docker/root CI) bypasses the read-only-dir permission
	// check, so WriteFileAtomic would succeed and the intended failure path would
	// not be exercised - skip rather than fail spuriously.
	if os.Getuid() == 0 {
		t.Skip("chmod-based write-failure path is not enforced for root (UID 0)")
	}
	dir := t.TempDir()
	seedOriginal(t, dir)

	// Seed a prior backup with a DIFFERENT basename than the primary, so the old
	// order (prune-first) would have deleted it before the failing write.
	typeDir := filepath.Join(dir, BackupDirName, "thumb")
	if err := os.MkdirAll(typeDir, 0o750); err != nil {
		t.Fatalf("creating type dir: %v", err)
	}
	priorBackup := filepath.Join(typeDir, "stale.png")
	if err := os.WriteFile(priorBackup, []byte("prior-backup-bytes"), 0o644); err != nil {
		t.Fatalf("seeding prior backup: %v", err)
	}

	// Make the type dir read-only so WriteFileAtomic of the fresh backup fails
	// (cannot create the tmp file) while the existing prior backup stays readable.
	if err := os.Chmod(typeDir, 0o500); err != nil {
		t.Fatalf("chmod type dir read-only: %v", err)
	}
	// Restore perms so t.TempDir's RemoveAll can clean up.
	t.Cleanup(func() { _ = os.Chmod(typeDir, 0o750) })

	err := BackupSingleSlot(dir, "thumb", errPathThumbNaming)
	if err == nil {
		t.Fatal("BackupSingleSlot must error when the fresh backup write fails")
	}

	// The prior backup must STILL exist: the failed write must not have pruned it.
	if _, statErr := os.Stat(priorBackup); statErr != nil {
		t.Errorf("prior backup must survive a failed write, stat err = %v", statErr)
	}
}

// TestRestoreSingleSlot_BackupDirIsFile covers findBackupFile's ReadDir error
// branch (a non-NotExist error) reached through RestoreSingleSlot when the
// per-type backup directory path is occupied by a regular file.
func TestRestoreSingleSlot_BackupDirIsFile(t *testing.T) {
	dir := t.TempDir()
	// .sw-backup/thumb is a regular file, so os.ReadDir of it errors with a
	// non-NotExist error rather than reporting "no backup".
	typeParent := filepath.Join(dir, BackupDirName)
	if err := os.MkdirAll(typeParent, 0o750); err != nil {
		t.Fatalf("creating .sw-backup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(typeParent, "thumb"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding type dir as file: %v", err)
	}

	err := RestoreSingleSlot(dir, "thumb", errPathThumbNaming, false, nil, testLogger(t))
	if err == nil {
		t.Fatal("RestoreSingleSlot must error when the type backup dir is a regular file")
	}
	// It is a real ReadDir error, NOT the no-backup sentinel.
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want a non-NotExist ReadDir error", err)
	}
}

// TestRestoreSingleSlot_CorruptBackupSaveFails covers the Save-error branch of
// RestoreSingleSlot: a backup file whose bytes are not a decodable image makes
// img.Save fail, so the restore returns a wrapped (non-NotExist) error.
func TestRestoreSingleSlot_CorruptBackupSaveFails(t *testing.T) {
	dir := t.TempDir()
	typeDir := filepath.Join(dir, BackupDirName, "thumb")
	if err := os.MkdirAll(typeDir, 0o750); err != nil {
		t.Fatalf("creating type dir: %v", err)
	}
	// A backup file that Save cannot decode (DetectFormat fails on junk bytes).
	if err := os.WriteFile(filepath.Join(typeDir, "folder.jpg"), []byte("not-an-image"), 0o644); err != nil {
		t.Fatalf("seeding corrupt backup: %v", err)
	}

	err := RestoreSingleSlot(dir, "thumb", errPathThumbNaming, false, nil, testLogger(t))
	if err == nil {
		t.Fatal("RestoreSingleSlot must error when the backup bytes are not a valid image")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want a Save error, not the no-backup sentinel", err)
	}
}

// TestFindBackupFile_SkipsSubdirectories covers the entry.IsDir() skip branch in
// findBackupFile: a stray subdirectory inside the per-type backup dir is ignored
// and the regular backup file is still located. The subdir is named to sort
// BEFORE the backup file so the IsDir() continue is actually exercised before the
// file is returned.
func TestFindBackupFile_SkipsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	typeDir := filepath.Join(dir, BackupDirName, "thumb")
	// A stray subdirectory ("0-stray" sorts before "folder.jpg") that must be
	// skipped before the real backup file is found.
	if err := os.MkdirAll(filepath.Join(typeDir, "0-stray"), 0o750); err != nil {
		t.Fatalf("creating stray subdir: %v", err)
	}
	// The real backup file.
	if err := os.WriteFile(filepath.Join(typeDir, "folder.jpg"), []byte("backup"), 0o644); err != nil {
		t.Fatalf("seeding backup file: %v", err)
	}

	got, err := findBackupFile(dir, "thumb")
	if err != nil {
		t.Fatalf("findBackupFile: %v", err)
	}
	want := filepath.Join(typeDir, "folder.jpg")
	if got != want {
		t.Errorf("findBackupFile = %q, want %q (subdir must be skipped)", got, want)
	}
}

// TestRemoveBackupFiles_SkipsSubdirectories covers the entry.IsDir() skip branch
// in pruneBackupFiles (reached via BackupSingleSlot's one-deep cleanup): a stray
// subdirectory in the per-type backup dir is left untouched while the prior
// backup file is removed and replaced. The subdir sorts first so the skip runs
// before the file is removed.
func TestRemoveBackupFiles_SkipsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	seedOriginal(t, dir)
	typeDir := filepath.Join(dir, BackupDirName, "thumb")
	if err := os.MkdirAll(filepath.Join(typeDir, "0-stray"), 0o750); err != nil {
		t.Fatalf("creating stray subdir: %v", err)
	}
	// A prior backup file that pruneBackupFiles should clear on the next backup.
	if err := os.WriteFile(filepath.Join(typeDir, "stale.jpg"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("seeding stale backup: %v", err)
	}

	// BackupSingleSlot writes a fresh backup, then runs pruneBackupFiles (one-deep).
	if err := BackupSingleSlot(dir, "thumb", errPathThumbNaming); err != nil {
		t.Fatalf("BackupSingleSlot: %v", err)
	}

	// The stray subdirectory must survive the cleanup.
	if info, err := os.Stat(filepath.Join(typeDir, "0-stray")); err != nil || !info.IsDir() {
		t.Errorf("stray subdir must be preserved by pruneBackupFiles, err=%v", err)
	}
	// The stale prior backup file must be gone (replaced one-deep).
	if _, err := os.Stat(filepath.Join(typeDir, "stale.jpg")); !os.IsNotExist(err) {
		t.Errorf("stale backup should be removed, stat err = %v", err)
	}
}
