package filesystem

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// renameFunc is the function used by RenameDirAtomic and RenameFileAtomic
// for the initial rename attempt. It defaults to os.Rename and can be
// overridden in tests to simulate cross-device (EXDEV) errors.
var renameFunc = os.Rename

// RenameDirAtomic renames src to dst using os.Rename. If that fails (e.g.
// cross-device move), it falls back to a recursive copy followed by removal
// of the source directory. The caller must ensure dst does not already exist;
// if dst exists the behavior is platform-dependent.
//
// The copy-and-delete fallback is instrumented (issue #2636) because it is not
// atomic: between the copy completing and the source removal completing, the
// tree exists twice, and a failure in that window leaves a duplicate the
// operator has to reconcile by hand. A plain rename leaves nothing to explain,
// so only the fallback records.
func RenameDirAtomic(src, dst string) error {
	renameErr := renameFunc(src, dst)
	if renameErr == nil {
		if dir := filepath.Dir(dst); dir != "" {
			// Same durability step RenameFileAtomic and WriteFileAtomic take: the
			// rename created a directory entry (here, for a whole tree) that is
			// not yet on stable storage (issue #2673). A crash before the parent's
			// metadata lands loses the name the tree was just moved to, while the
			// source name is already gone.
			syncTargetDir("RenameDirAtomic", dir, dst)
		}
		return nil
	}

	log := fsLog().With(slog.String("op", "RenameDirAtomic"), slog.String("src", src), slog.String("dst", dst))

	// Snapshot dst state so we only clean up our own partial copy on failure.
	_, statErr := os.Stat(dst)

	// Fallback: recursive copy + delete for cross-device moves.
	if err := copyDirRecursive(src, dst); err != nil {
		cleaned := false
		if os.IsNotExist(statErr) {
			// dst was created by us; safe to clean up.
			cleaned = os.RemoveAll(dst) == nil
		}
		log.Error("cross-device directory move failed during copy; source is intact",
			slog.String("step", "copy_fallback"),
			slog.Bool("src_intact", true),
			slog.Bool("partial_dst_cleaned", cleaned),
			slog.String("error", err.Error()))
		return fmt.Errorf("copy fallback failed: %w", err)
	}

	if err := os.RemoveAll(src); err != nil {
		log.Error("cross-device directory move copied but could not remove the source; the tree now exists at BOTH paths",
			slog.String("step", "remove_source"),
			slog.String("error", err.Error()))
		return fmt.Errorf("removing source after copy: %w", err)
	}
	log.Warn("directory moved via cross-device copy and delete rather than rename",
		slog.String("step", "copy_fallback"),
		slog.String("rename_error", renameErr.Error()))
	return nil
}

// RenameFileAtomic renames a single file from src to dst using os.Rename.
// If that fails (e.g. cross-device move with EXDEV), it falls back to
// copyFile followed by removal of the source. The caller must ensure dst
// does not already exist; the merge orchestrator's loose-file path checks
// for that collision before calling here.
//
// This mirrors RenameDirAtomic but is specialized for files: it avoids the
// recursive directory walk overhead and uses copyFile directly so a single
// loose-file move on a cross-device setup (bind mount, per-letter NAS
// share) completes instead of returning EXDEV up the stack.
//
// Instrumented on the fallback path only, for the same reason as
// RenameDirAtomic: the copy-then-delete is where a partially-completed move can
// leave the file at both paths or at neither (issue #2636).
func RenameFileAtomic(src, dst string) error {
	renameErr := renameFunc(src, dst)
	if renameErr == nil {
		if dir := filepath.Dir(dst); dir != "" {
			// Same durability step WriteFileAtomic takes: the rename created a
			// directory entry that is not yet on stable storage (issue #2673).
			syncTargetDir("RenameFileAtomic", dir, dst)
		}
		return nil
	}

	log := fsLog().With(slog.String("op", "RenameFileAtomic"), slog.String("src", src), slog.String("dst", dst))

	// Snapshot dst state so we only clean up our own partial copy on failure.
	_, statErr := os.Stat(dst)

	// Stat the source to preserve its existing file mode on the copy path.
	// If the stat fails (narrow race: file removed between rename failure and
	// here), copyFile will fail at os.Open and we will propagate that error.
	srcMode := os.FileMode(0o644)
	if srcInfo, srcStatErr := os.Stat(src); srcStatErr == nil {
		srcMode = srcInfo.Mode().Perm()
	}

	if err := copyFile(src, dst, srcMode); err != nil {
		cleaned := false
		if os.IsNotExist(statErr) {
			// dst was created by us; safe to clean up.
			cleaned = os.Remove(dst) == nil
		}
		log.Error("cross-device file move failed during copy; source is intact",
			slog.String("step", "copy_fallback"),
			slog.Bool("src_intact", true),
			slog.Bool("partial_dst_cleaned", cleaned),
			slog.String("error", err.Error()))
		return fmt.Errorf("copy fallback failed: %w", err)
	}

	// The copy is complete and copyFile fsynced the destination's DATA, but the
	// directory ENTRY naming that data is still only in the page cache. The very
	// next statement unlinks the source. A crash in between is the one window on
	// this path where the bytes exist and no name reaches them: the source entry
	// is gone from disk and the destination entry never landed. Sync the
	// destination's directory first so the name is durable before the only other
	// copy of it is destroyed (issue #2673).
	if dir := filepath.Dir(dst); dir != "" {
		syncTargetDir("RenameFileAtomic", dir, dst)
	}

	if err := os.Remove(src); err != nil {
		log.Error("cross-device file move copied but could not remove the source; the file now exists at BOTH paths",
			slog.String("step", "remove_source"),
			slog.String("error", err.Error()))
		return fmt.Errorf("removing source after copy: %w", err)
	}
	log.Warn("file moved via cross-device copy and delete rather than rename",
		slog.String("step", "copy_fallback"),
		slog.String("rename_error", renameErr.Error()))
	return nil
}

func copyDirRecursive(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		// Reuse the package-level copyFile from atomic.go, preserving
		// the source file's mode so directory copies stay permission-faithful.
		return copyFile(path, target, info.Mode().Perm())
	})
}
