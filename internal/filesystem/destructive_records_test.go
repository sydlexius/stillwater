package filesystem

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteFileAtomic_PromotionFailureRecordsTargetIntact covers the loudest
// branch in the writer: the promoting rename fails, which is the instant the
// old inode would have been dropped for the new one.
//
// The assertion is on the ARTIFACT first -- the bytes actually on disk after
// the failure -- and on the record second. A record claiming the target
// survived is worth nothing unless the target did survive, and a surviving
// target is unattributable without the record.
func TestWriteFileAtomic_PromotionFailureRecordsTargetIntact(t *testing.T) {
	// Mutates package-level hooks and the slog default; must not run parallel.
	origRename := osRename
	t.Cleanup(func() { osRename = origRename })
	osRename = func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EIO}
	}

	state := captureFSLogs(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "thumb.jpg")
	original := []byte("the artwork that must survive")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatalf("seeding target: %v", err)
	}
	// Precondition: without this the test could pass against a writer that
	// never had a target to destroy in the first place.
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("precondition: seeded target unreadable or wrong: %q err=%v", got, err)
	}

	if err := WriteFileAtomic(target, []byte("replacement"), 0o644); err == nil {
		t.Fatal("WriteFileAtomic: want an error when the promoting rename fails, got nil")
	}

	// Artifact on disk: the original bytes, unchanged.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target after the failed promotion: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("target content = %q, want %q", got, original)
	}

	e := requireOneFSRecord(t, state,
		"atomic write failed promoting temp onto target; target retains its ORIGINAL content",
		target, slog.LevelError, "rename_promote")
	if e.attrs["target_intact"] != "true" {
		t.Errorf("target_intact = %q, want %q", e.attrs["target_intact"], "true")
	}
	if e.attrs["temp_removed"] != "true" {
		t.Errorf("temp_removed = %q, want %q (the orphaned temp must be cleaned up)", e.attrs["temp_removed"], "true")
	}
	if e.attrs["error"] == "" {
		t.Error("error attribute is empty; the record must name the failure")
	}
	if e.attrs["op"] != "WriteFileAtomic" {
		t.Errorf("op = %q, want %q", e.attrs["op"], "WriteFileAtomic")
	}
}

// TestWriteFileAtomic_TempStagingFailureRecordsUntouchedTarget covers the
// earlier failure branch. It matters separately because the operator's question
// ("is my file gone?") has the same answer but a different reason, and a writer
// that recorded only the rename branch would leave a staging failure silent.
func TestWriteFileAtomic_TempStagingFailureRecordsUntouchedTarget(t *testing.T) {
	origWrite := writeTempFile
	t.Cleanup(func() { writeTempFile = origWrite })
	writeTempFile = func(f *os.File, _ []byte, _ os.FileMode) error {
		_ = f.Close()
		return errors.New("simulated ENOSPC")
	}

	state := captureFSLogs(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "logo.png")
	original := []byte("original logo")
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatalf("seeding target: %v", err)
	}

	if err := WriteFileAtomic(target, []byte("new logo"), 0o644); err == nil {
		t.Fatal("WriteFileAtomic: want an error when the temp staging fails, got nil")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("target content = %q, want %q", got, original)
	}

	e := requireOneFSRecord(t, state,
		"atomic write failed staging temp file; target untouched",
		target, slog.LevelError, "write_temp")
	if e.attrs["temp_removed"] != "true" {
		t.Errorf("temp_removed = %q, want %q", e.attrs["temp_removed"], "true")
	}
	if e.attrs["temp_path"] == "" {
		t.Error("temp_path is empty; the record must name the orphan it cleaned up")
	}
}

// TestWriteFileAtomic_SyncsParentDirectoryAfterRename pins #2673. It wires the
// real machinery -- the directory-handle sync hook the production path calls --
// and asserts both THAT it fired and WHICH directories it was given, IN WHAT
// ORDER, because a dir-fsync of the wrong directory is exactly as useless as
// none at all, and a parent synced before its child is very nearly as useless:
// the child's contents may still be in the page cache when the parent's entry
// lands, so the crash still loses the file.
//
// The table covers the three shapes that differ in how much MkdirAll created,
// which is exactly what determines how far up the chain must be synced.
//
// The precondition assertion (no sync observed before the write) is what stops
// this passing vacuously against a stale observation from an earlier call.
func TestWriteFileAtomic_SyncsParentDirectoryAfterRename(t *testing.T) {
	for _, tc := range []struct {
		name string
		// rel is the target path relative to the fresh temp root.
		rel string
		// wantRel is the expected sequence of synced directories, relative to
		// the temp root; "." is the root itself.
		wantRel []string
	}{
		{
			// Nothing created: the plain-rename shape. One fsync, of the
			// directory that already existed and holds the new entry.
			name:    "existing directory syncs only itself",
			rel:     "banner.jpg",
			wantRel: []string{"."},
		},
		{
			// MkdirAll created one level. The file's entry lives in "nested",
			// and "nested"'s own entry lives in the root -- both are new-ish and
			// both must land, child first.
			name:    "one created level syncs child then its parent",
			rel:     "nested/banner.jpg",
			wantRel: []string{"nested", "."},
		},
		{
			// MkdirAll created three levels. Syncing only one level up would
			// leave "a"'s entry in the root non-durable, and a crash losing "a"
			// loses everything beneath it -- the whole file included.
			name:    "several created levels sync the whole created chain",
			rel:     "a/b/c/banner.jpg",
			wantRel: []string{"a/b/c", "a/b", "a", "."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, filepath.FromSlash(tc.rel))

			want := make([]string, len(tc.wantRel))
			for i, r := range tc.wantRel {
				want[i] = filepath.Join(root, filepath.FromSlash(r))
			}

			var syncedPaths []string
			origOpen := openDir
			origSync := syncDirHandle
			t.Cleanup(func() { openDir = origOpen; syncDirHandle = origSync })
			openDir = func(name string) (*os.File, error) {
				syncedPaths = append(syncedPaths, name)
				return origOpen(name)
			}

			if len(syncedPaths) != 0 {
				t.Fatalf("precondition: expected no directory opens before the write, got %v", syncedPaths)
			}

			if err := WriteFileAtomic(target, []byte("payload"), 0o644); err != nil {
				t.Fatalf("WriteFileAtomic: %v", err)
			}

			if len(syncedPaths) != len(want) {
				t.Fatalf("directory syncs = %v, want exactly %v", syncedPaths, want)
			}
			for i := range want {
				if syncedPaths[i] != want[i] {
					t.Errorf("synced directory[%d] = %q, want %q (full sequence %v, want %v; child must precede parent)",
						i, syncedPaths[i], want[i], syncedPaths, want)
				}
			}

			// The write itself must still have landed. A durability step that ate
			// the write would satisfy the assertions above.
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("reading target: %v", err)
			}
			if !bytes.Equal(got, []byte("payload")) {
				t.Errorf("target content = %q, want %q", got, "payload")
			}
		})
	}
}

// TestWriteFileAtomic_DoesNotSyncAboveTheDirectoriesItCreated is the bound on
// the chain walk. Without it, "sync the parents too" could be satisfied by a
// version that climbs all the way to the filesystem root fsyncing directories
// this write never touched -- correct-looking, and a per-write cost that grows
// with how deep the library happens to be mounted.
//
// The floor is the deepest directory that already existed. Its own entry in ITS
// parent was made durable by whoever created it; this write did not create it
// and does not owe it an fsync.
func TestWriteFileAtomic_DoesNotSyncAboveTheDirectoriesItCreated(t *testing.T) {
	root := t.TempDir()
	// Pre-create the floor so MkdirAll only has to make "new" beneath it.
	floor := filepath.Join(root, "existing")
	if err := os.MkdirAll(floor, 0o755); err != nil {
		t.Fatalf("seeding floor directory: %v", err)
	}
	target := filepath.Join(floor, "new", "banner.jpg")

	var syncedPaths []string
	origOpen := openDir
	t.Cleanup(func() { openDir = origOpen })
	openDir = func(name string) (*os.File, error) {
		syncedPaths = append(syncedPaths, name)
		return origOpen(name)
	}

	if err := WriteFileAtomic(target, []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	want := []string{filepath.Join(floor, "new"), floor}
	if len(syncedPaths) != len(want) {
		t.Fatalf("directory syncs = %v, want exactly %v (the walk must stop at the deepest pre-existing directory)", syncedPaths, want)
	}
	for i := range want {
		if syncedPaths[i] != want[i] {
			t.Errorf("synced directory[%d] = %q, want %q (full sequence %v)", i, syncedPaths[i], want[i], syncedPaths)
		}
	}
	for _, p := range syncedPaths {
		if p == root {
			t.Errorf("synced %q, which already existed and was not created by this write; the walk climbed too far (full sequence %v)", root, syncedPaths)
		}
	}
}

// TestWriteFileAtomic_DirSyncFailureDoesNotFailTheWrite pins the deliberate
// choice that a failed directory fsync is recorded, not returned. The rename
// already happened, so telling the caller its write failed would be a lie that
// triggers a pointless rollback.
func TestWriteFileAtomic_DirSyncFailureDoesNotFailTheWrite(t *testing.T) {
	origSync := syncDirHandle
	t.Cleanup(func() { syncDirHandle = origSync })
	syncDirHandle = func(_ *os.File) error { return syscall.EIO }

	state := captureFSLogs(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "nfo.xml")

	if err := WriteFileAtomic(target, []byte("<artist/>"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: a directory-sync failure must not fail the write, got %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if !bytes.Equal(got, []byte("<artist/>")) {
		t.Errorf("target content = %q, want %q", got, "<artist/>")
	}

	requireOneFSRecord(t, state,
		"directory sync after rename failed; file is in place but its directory entry is not yet durable",
		target, slog.LevelWarn, "")
}

// TestSyncDir_UnsupportedErrorIsNotAFailure pins the platform carve-out. A
// filesystem that does not implement directory fsync answers EINVAL or ENOTSUP;
// treating that as an error would make every atomic write on such a mount log a
// warning it can do nothing about.
func TestSyncDir_UnsupportedErrorIsNotAFailure(t *testing.T) {
	origSync := syncDirHandle
	t.Cleanup(func() { syncDirHandle = origSync })

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"EINVAL", syscall.EINVAL},
		{"ENOTSUP", syscall.ENOTSUP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			syncDirHandle = func(_ *os.File) error { return tc.err }
			if err := SyncDir(t.TempDir()); err != nil {
				t.Errorf("SyncDir with %v = %v, want nil", tc.err, err)
			}
		})
	}

	// And the contrast that gives the carve-out meaning: a real I/O error IS
	// returned. Without this the test above would also pass against a SyncDir
	// that swallowed every error unconditionally.
	syncDirHandle = func(_ *os.File) error { return syscall.EIO }
	if err := SyncDir(t.TempDir()); err == nil {
		t.Error("SyncDir with EIO = nil, want an error (a genuine sync failure must surface)")
	}
}

// TestRemoveFileSafe_UnlinkFailureRestoresFileToOriginalPath is the recovery
// path this whole change exists for. The file has been moved to the tomb, the
// unlink then fails, and the correct end state is the file back where it
// started -- not abandoned under a name nothing in the application looks for.
func TestRemoveFileSafe_UnlinkFailureRestoresFileToOriginalPath(t *testing.T) {
	origRemoveTomb := removeTomb
	t.Cleanup(func() { removeTomb = origRemoveTomb })
	removeTomb = func(_ string) error { return syscall.EIO }

	state := captureFSLogs(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "fanart1.jpg")
	content := []byte("artwork bytes")
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatalf("seeding target: %v", err)
	}

	err := RemoveFileSafe(target)
	if err == nil {
		t.Fatal("RemoveFileSafe: want an error when the unlink fails, got nil")
	}

	// Artifact: the file is back at its real path with its real bytes.
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("target must be restored after a failed unlink, reading it: %v", readErr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("restored content = %q, want %q", got, content)
	}
	// And nothing is stranded at the tomb.
	if _, statErr := os.Stat(target + ".removing"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("tomb should not remain after a successful restore, stat err = %v", statErr)
	}

	e := requireOneFSRecord(t, state,
		"safe remove failed to unlink the tomb; file restored to its original path",
		target, slog.LevelWarn, "remove_tomb")
	if e.attrs["restored"] != "true" {
		t.Errorf("restored = %q, want %q", e.attrs["restored"], "true")
	}
}

// TestRemoveFileSafe_UnlinkAndRestoreFailureRecordsTombOnlyState is the worst
// end state the remover can reach: the file exists, but only under the tomb
// name. That is precisely the case #2636 says must not be silent, because the
// operator sees artwork vanish while the bytes are still on disk a rename away.
func TestRemoveFileSafe_UnlinkAndRestoreFailureRecordsTombOnlyState(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "thumb.jpg")
	tomb := target + ".removing"
	content := []byte("still on disk, wrong name")
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatalf("seeding target: %v", err)
	}

	origRemoveTomb := removeTomb
	origRename := osRename
	t.Cleanup(func() { removeTomb = origRemoveTomb; osRename = origRename })
	removeTomb = func(_ string) error { return syscall.EIO }
	// Let the move-to-tomb through, then fail the restore leg specifically.
	osRename = func(oldPath, newPath string) error {
		if oldPath == tomb {
			return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EIO}
		}
		return origRename(oldPath, newPath)
	}

	state := captureFSLogs(t)

	if err := RemoveFileSafe(target); err == nil {
		t.Fatal("RemoveFileSafe: want an error when both the unlink and the restore fail, got nil")
	}

	// Artifact: the original name is gone and the bytes live at the tomb.
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("target should be absent in the tomb-only state, stat err = %v", statErr)
	}
	got, readErr := os.ReadFile(tomb)
	if readErr != nil {
		t.Fatalf("reading tomb: %v", readErr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("tomb content = %q, want %q", got, content)
	}

	e := requireOneFSRecord(t, state,
		"safe remove failed to unlink the tomb AND failed to restore it; the file now exists ONLY at the tomb path",
		target, slog.LevelError, "remove_tomb")
	if e.attrs["restored"] != "false" {
		t.Errorf("restored = %q, want %q", e.attrs["restored"], "false")
	}
	if e.attrs["tomb_path"] != tomb {
		t.Errorf("tomb_path = %q, want %q (without it the operator cannot find the bytes)", e.attrs["tomb_path"], tomb)
	}
	if e.attrs["restore_error"] == "" {
		t.Error("restore_error is empty; the record must say why the restore failed")
	}
}

// TestRenameFileAtomic_CrossDeviceCopySucceedsAndRecords covers the EXDEV
// fallback completing. It records at Warn rather than Debug because the move
// was NOT atomic: for the duration of the copy the file existed at both paths,
// and an operator reconciling a half-populated library needs to know which
// moves took that route.
func TestRenameFileAtomic_CrossDeviceCopySucceedsAndRecords(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jpg")
	dst := filepath.Join(dir, "dst.jpg")
	content := []byte("moved bytes")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("seeding src: %v", err)
	}

	origRename := renameFunc
	t.Cleanup(func() { renameFunc = origRename })
	renameFunc = func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EXDEV}
	}

	state := captureFSLogs(t)

	if err := RenameFileAtomic(src, dst); err != nil {
		t.Fatalf("RenameFileAtomic: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading dst: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("dst content = %q, want %q", got, content)
	}
	if _, statErr := os.Stat(src); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("src should be gone after a successful cross-device move, stat err = %v", statErr)
	}

	entries := state.entriesWithSrc("file moved via cross-device copy and delete rather than rename", src)
	if len(entries) != 1 {
		t.Fatalf("want exactly 1 cross-device move record for src %s, got %d", src, len(entries))
	}
	if entries[0].level != slog.LevelWarn {
		t.Errorf("level = %v, want %v (a non-atomic move is not routine)", entries[0].level, slog.LevelWarn)
	}
	if entries[0].attrs["rename_error"] == "" {
		t.Error("rename_error is empty; the record must say why the atomic rename was not used")
	}
}

// TestRenameFileAtomic_PlainRenameSyncsDestinationDirectory is the mirror of
// TestWriteFileAtomic_SyncsParentDirectoryAfterRename for the OTHER promoting
// rename in this package. Every other RenameFileAtomic test forces EXDEV, so
// renameFunc never succeeds and the sync on the plain-rename path was never
// executed by any test: an edit deleting that call passed the whole suite.
func TestRenameFileAtomic_PlainRenameSyncsDestinationDirectory(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	dstDir := filepath.Join(root, "dst")
	for _, d := range []string{srcDir, dstDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("seeding %s: %v", d, err)
		}
	}
	src := filepath.Join(srcDir, "cover.jpg")
	dst := filepath.Join(dstDir, "cover.jpg")
	content := []byte("renamed bytes")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("seeding src: %v", err)
	}

	var syncedPaths []string
	origOpen := openDir
	t.Cleanup(func() { openDir = origOpen })
	openDir = func(name string) (*os.File, error) {
		syncedPaths = append(syncedPaths, name)
		return origOpen(name)
	}

	if len(syncedPaths) != 0 {
		t.Fatalf("precondition: expected no directory opens before the rename, got %v", syncedPaths)
	}

	// renameFunc is deliberately NOT overridden here: the point of this test is
	// the path where the real os.Rename succeeds.
	if err := RenameFileAtomic(src, dst); err != nil {
		t.Fatalf("RenameFileAtomic: %v", err)
	}

	// Precondition on the shape: the rename really did take the plain path, not
	// the copy fallback (which would also leave dst readable).
	if _, statErr := os.Stat(src); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("precondition: src should be gone after a plain rename, stat err = %v", statErr)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading dst: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("dst content = %q, want %q", got, content)
	}

	if len(syncedPaths) != 1 {
		t.Fatalf("directory syncs = %v, want exactly one (for %s)", syncedPaths, dstDir)
	}
	if syncedPaths[0] != dstDir {
		t.Errorf("synced directory = %q, want %q (the DESTINATION's directory holds the new entry)", syncedPaths[0], dstDir)
	}
}

// TestRenameFileAtomic_CrossDeviceSyncsDestinationBeforeRemovingSource pins the
// ordering that makes the EXDEV fallback survivable. copyFile fsyncs the
// destination's DATA; nothing fsynced the directory ENTRY naming it. A crash
// between the copy and the source unlink therefore had a window in which the
// bytes existed and no name reached them -- the destination entry never landed
// and the source entry was already gone.
//
// The assertion is on ORDER, not on a count: a sync of the right directory
// AFTER the unlink closes nothing.
func TestRenameFileAtomic_CrossDeviceSyncsDestinationBeforeRemovingSource(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	dstDir := filepath.Join(root, "dst")
	for _, d := range []string{srcDir, dstDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("seeding %s: %v", d, err)
		}
	}
	src := filepath.Join(srcDir, "cover.jpg")
	dst := filepath.Join(dstDir, "cover.jpg")
	if err := os.WriteFile(src, []byte("moved bytes"), 0o644); err != nil {
		t.Fatalf("seeding src: %v", err)
	}

	origRename := renameFunc
	t.Cleanup(func() { renameFunc = origRename })
	renameFunc = func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EXDEV}
	}

	// Ordering is established from INSIDE the sync hook: it records whether the
	// source still exists at the instant the sync fires. Asserting only that the
	// destination directory was synced at some point would pass against a sync
	// placed after the unlink, which closes nothing.
	var syncedPaths []string
	srcStillPresentAtSync := make(map[string]bool)
	origOpen := openDir
	t.Cleanup(func() { openDir = origOpen })
	openDir = func(name string) (*os.File, error) {
		syncedPaths = append(syncedPaths, name)
		_, statErr := os.Lstat(src)
		srcStillPresentAtSync[name] = statErr == nil
		return origOpen(name)
	}

	if err := RenameFileAtomic(src, dst); err != nil {
		t.Fatalf("RenameFileAtomic: %v", err)
	}

	// Outcome preconditions: the move really completed via the fallback.
	if _, statErr := os.Stat(src); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("precondition: src should be gone after a successful cross-device move, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(dst); statErr != nil {
		t.Fatalf("precondition: dst should exist after a successful cross-device move, stat err = %v", statErr)
	}

	if len(syncedPaths) != 1 || syncedPaths[0] != dstDir {
		t.Fatalf("directory syncs = %v, want exactly [%s]", syncedPaths, dstDir)
	}
	if !srcStillPresentAtSync[dstDir] {
		t.Errorf("the destination directory was synced AFTER the source was unlinked; the crash window this closes is between the copy and the unlink, so the sync must come first")
	}
}

// TestRenameDirAtomic_PlainRenameSyncsDestinationDirectory is the directory
// sibling of the file case above. RenameDirAtomic's successful rename installed
// a whole tree under a new name and left that entry non-durable, while
// RenameFileAtomic right beside it did not -- an inconsistency, and on this path
// the thing at risk is an entire artist directory rather than one file.
func TestRenameDirAtomic_PlainRenameSyncsDestinationDirectory(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "library")
	src := filepath.Join(parent, "old-name")
	dst := filepath.Join(parent, "new-name")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("seeding src tree: %v", err)
	}
	content := []byte("artwork bytes")
	if err := os.WriteFile(filepath.Join(src, "folder.jpg"), content, 0o644); err != nil {
		t.Fatalf("seeding src file: %v", err)
	}

	var syncedPaths []string
	origOpen := openDir
	t.Cleanup(func() { openDir = origOpen })
	openDir = func(name string) (*os.File, error) {
		syncedPaths = append(syncedPaths, name)
		return origOpen(name)
	}

	if len(syncedPaths) != 0 {
		t.Fatalf("precondition: expected no directory opens before the rename, got %v", syncedPaths)
	}

	if err := RenameDirAtomic(src, dst); err != nil {
		t.Fatalf("RenameDirAtomic: %v", err)
	}

	// Precondition: the plain rename path, not the copy-and-delete fallback.
	if _, statErr := os.Stat(src); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("precondition: src should be gone after a plain rename, stat err = %v", statErr)
	}
	got, err := os.ReadFile(filepath.Join(dst, "folder.jpg"))
	if err != nil {
		t.Fatalf("reading moved file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("moved content = %q, want %q", got, content)
	}

	if len(syncedPaths) != 1 {
		t.Fatalf("directory syncs = %v, want exactly one (for %s)", syncedPaths, parent)
	}
	if syncedPaths[0] != parent {
		t.Errorf("synced directory = %q, want %q (the directory HOLDING the renamed tree, not the tree itself)", syncedPaths[0], parent)
	}
}

// TestRenameFileAtomic_CopyFailureRecordsSourceIntact provokes the fallback's
// copy leg failing (the destination path is an existing directory, so the
// destination open cannot succeed) and pins that the record says the source
// survived -- which it must, since nothing has removed it yet.
func TestRenameFileAtomic_CopyFailureRecordsSourceIntact(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jpg")
	dst := filepath.Join(dir, "dst")
	content := []byte("source bytes")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatalf("seeding src: %v", err)
	}
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatalf("seeding dst dir: %v", err)
	}

	origRename := renameFunc
	t.Cleanup(func() { renameFunc = origRename })
	renameFunc = func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EXDEV}
	}

	state := captureFSLogs(t)

	if err := RenameFileAtomic(src, dst); err == nil {
		t.Fatal("RenameFileAtomic: want an error when the copy fallback fails, got nil")
	}

	// Artifact: the source is untouched.
	got, readErr := os.ReadFile(src)
	if readErr != nil {
		t.Fatalf("reading src after the failed move: %v", readErr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("src content = %q, want %q", got, content)
	}

	entries := state.entriesWithSrc("cross-device file move failed during copy; source is intact", src)
	if len(entries) != 1 {
		t.Fatalf("want exactly 1 copy-failure record for src %s, got %d", src, len(entries))
	}
	if entries[0].level != slog.LevelError {
		t.Errorf("level = %v, want %v", entries[0].level, slog.LevelError)
	}
	if entries[0].attrs["src_intact"] != "true" {
		t.Errorf("src_intact = %q, want %q", entries[0].attrs["src_intact"], "true")
	}
	if entries[0].attrs["dst"] != dst {
		t.Errorf("dst = %q, want %q", entries[0].attrs["dst"], dst)
	}
}

// entriesWithSrc is the src-keyed sibling of matching(), for the rename helpers
// whose records key on src/dst rather than a single path.
func (s *fsLogState) entriesWithSrc(msg, src string) []fsLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []fsLogEntry
	for _, e := range s.entries {
		if e.msg == msg && e.attrs["src"] == src {
			out = append(out, e)
		}
	}
	return out
}
