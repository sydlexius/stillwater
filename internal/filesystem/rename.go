package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
)

// RenameDirAtomic renames src to dst using os.Rename. If that fails (e.g.
// cross-device move), it falls back to a recursive copy followed by removal
// of the source directory.
func RenameDirAtomic(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	// Fallback: recursive copy + delete for cross-device moves.
	if err := copyDirRecursive(src, dst); err != nil {
		// Clean up partial copy on failure.
		_ = os.RemoveAll(dst)
		return fmt.Errorf("copy fallback failed: %w", err)
	}

	if err := os.RemoveAll(src); err != nil {
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
		// Reuse the package-level copyFile from atomic.go.
		return copyFile(path, target)
	})
}
