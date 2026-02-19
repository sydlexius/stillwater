package filesystem

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to the target path using the tmp/bak/rename pattern.
// This prevents data corruption if the process is interrupted during the write.
//
// Steps:
//  1. Write data to <target>.tmp
//  2. If <target> exists, rename it to <target>.bak
//  3. Rename <target>.tmp to <target>
//  4. Remove <target>.bak
//
// If rename fails (e.g., cross-mount point), falls back to copy+delete with fsync.
func WriteFileAtomic(target string, data []byte, perm os.FileMode) error {
	tmpPath := target + ".tmp"
	bakPath := target + ".bak"

	// Ensure parent directory exists
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: 0755 is appropriate for application data directories
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Step 1: Write to .tmp
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}

	// Step 2: Backup existing file if it exists
	if _, err := os.Stat(target); err == nil {
		if err := renameSafe(target, bakPath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("backing up existing file: %w", err)
		}
	}

	// Step 3: Move .tmp to target
	if err := renameSafe(tmpPath, target); err != nil {
		// Attempt to restore backup
		if _, bakErr := os.Stat(bakPath); bakErr == nil {
			_ = renameSafe(bakPath, target)
		}
		_ = os.Remove(tmpPath)
		return fmt.Errorf("renaming temp to target: %w", err)
	}

	// Step 4: Clean up .bak
	_ = os.Remove(bakPath)

	return nil
}

// WriteReaderAtomic writes from a reader to the target path using the atomic pattern.
func WriteReaderAtomic(target string, r io.Reader, perm os.FileMode) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading source data: %w", err)
	}
	return WriteFileAtomic(target, data, perm)
}

// renameSafe attempts os.Rename first, then falls back to copy+delete.
func renameSafe(oldPath, newPath string) error {
	err := os.Rename(oldPath, newPath)
	if err == nil {
		return nil
	}
	// Rename may fail on cross-device moves. Fall back to copy+delete.
	if copyErr := copyFile(oldPath, newPath); copyErr != nil {
		return fmt.Errorf("copy fallback: %w (rename error: %w)", copyErr, err)
	}
	_ = os.Remove(oldPath)
	return nil
}

// copyFile copies a file using io.Copy and flushes with fsync.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: src is from trusted internal path
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck

	out, err := os.Create(dst) //nolint:gosec // G304: dst is from trusted internal path
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	// Ensure data is flushed to disk
	if err := out.Sync(); err != nil {
		return err
	}

	return out.Close()
}
