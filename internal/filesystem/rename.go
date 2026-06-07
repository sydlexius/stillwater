package filesystem

import (
	"fmt"
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
func RenameDirAtomic(src, dst string) error {
	if err := renameFunc(src, dst); err == nil {
		return nil
	}

	// Snapshot dst state so we only clean up our own partial copy on failure.
	_, statErr := os.Stat(dst)

	// Fallback: recursive copy + delete for cross-device moves.
	if err := copyDirRecursive(src, dst); err != nil {
		if os.IsNotExist(statErr) {
			// dst was created by us; safe to clean up.
			_ = os.RemoveAll(dst)
		}
		return fmt.Errorf("copy fallback failed: %w", err)
	}

	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("removing source after copy: %w", err)
	}
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
func RenameFileAtomic(src, dst string) error {
	if err := renameFunc(src, dst); err == nil {
		return nil
	}

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
		if os.IsNotExist(statErr) {
			// dst was created by us; safe to clean up.
			_ = os.Remove(dst)
		}
		return fmt.Errorf("copy fallback failed: %w", err)
	}

	if err := os.Remove(src); err != nil {
		return fmt.Errorf("removing source after copy: %w", err)
	}
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
