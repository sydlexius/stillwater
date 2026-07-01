package filesystem

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// osRename is the rename function used by renameSafe. It defaults to os.Rename
// and can be overridden in tests to simulate cross-device (EXDEV) errors,
// following the same injectable-hook pattern used by renameFunc in rename.go.
var osRename = os.Rename

// WriteFileAtomic writes data to the target path using the tmp/bak/rename pattern.
// This prevents data corruption if the process is interrupted during the write.
//
// Steps:
//  1. Write data to a uniquely-named temp file created via os.CreateTemp (O_EXCL),
//     so concurrent writers targeting the same path never collide on the temp name
//  2. If <target> exists, rename it to <target>.bak
//  3. Rename the temp file to <target>
//  4. Remove <target>.bak
//
// If rename fails (e.g., cross-mount point), falls back to copy+delete with fsync.
func WriteFileAtomic(target string, data []byte, perm os.FileMode) error {
	TraceFSWrite("WriteFileAtomic", target, 0)
	bakPath := target + ".bak"

	// Ensure parent directory exists
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: 0755 is appropriate for application data directories
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Step 1: Write to a uniquely-named temp file (O_EXCL via os.CreateTemp),
	// then chmod to the caller's intended perm since CreateTemp always creates
	// the file 0o600 regardless of perm.
	tmpFile, err := os.CreateTemp(dir, filepath.Base(target)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmpFile.Chmod(perm); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("setting temp file permissions: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}

	// Step 2: Backup existing file if it exists
	if _, err := os.Stat(target); err == nil {
		if err := renameSafe(target, bakPath, perm); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("backing up existing file: %w", err)
		}
	}

	// Step 3: Move .tmp to target
	if err := renameSafe(tmpPath, target, perm); err != nil {
		// Attempt to restore backup
		if _, bakErr := os.Stat(bakPath); bakErr == nil {
			_ = renameSafe(bakPath, target, perm)
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

// renameSafe attempts osRename first, then falls back to copy+delete.
// perm is passed to copyFile so the destination inherits the intended file mode
// even on the cross-device fallback path (where os.Rename would drop the perm).
func renameSafe(oldPath, newPath string, perm os.FileMode) error {
	err := osRename(oldPath, newPath)
	if err == nil {
		return nil
	}
	// Rename may fail on cross-device moves (EXDEV). Fall back to copy+delete.
	if copyErr := copyFile(oldPath, newPath, perm); copyErr != nil {
		return fmt.Errorf("copy fallback: %w (rename error: %w)", copyErr, err)
	}
	_ = os.Remove(oldPath)
	return nil
}

// RemoveFileSafe deletes a single file using a "rename to .removing then
// unlink" pattern so the unlink is the only operation that can leave a
// partially-named file behind. This matches the tmp/bak/rename discipline
// used by WriteFileAtomic in spirit: the visible file disappears in one
// atomic rename, then the .removing tomb is unlinked. If the rename fails
// we fall back to a direct os.Remove so callers always get the file gone
// when possible.
//
// Returns os.ErrNotExist (wrapped) when the target does not exist so callers
// can distinguish "already removed" from a real failure.
func RemoveFileSafe(target string) error {
	TraceFSWrite("RemoveFileSafe", target, 0)
	info, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("removing %s: %w", target, err)
	}
	// Reject directory targets up front. Without this, the rename-then-unlink
	// flow can move a directory to "<dir>.removing" and then fail to unlink
	// it, leaving the user's tree in a half-renamed state.
	if info.IsDir() {
		return fmt.Errorf("removing %s: target is a directory", target)
	}
	tomb := target + ".removing"
	// Best-effort cleanup of any prior tomb left over from a crash.
	_ = os.Remove(tomb)
	if err := os.Rename(target, tomb); err != nil {
		// Fall back to direct removal; better to remove than to abort.
		if rerr := os.Remove(target); rerr != nil {
			return fmt.Errorf("removing %s: rename: %w; direct remove: %w", target, err, rerr)
		}
		return nil
	}
	if err := os.Remove(tomb); err != nil {
		return fmt.Errorf("removing tomb %s: %w", tomb, err)
	}
	return nil
}

// copyFile copies a file using io.Copy and flushes with fsync.
// perm is applied when creating the destination file so the intended mode
// is preserved on the cross-device fallback path. Using os.OpenFile with
// O_WRONLY|O_CREATE|O_TRUNC mirrors what os.Create does, but with the
// caller-specified mode rather than the default 0666.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // G304: src is from trusted internal path
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // Close error not actionable on read path

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm) //nolint:gosec // G304: dst is from trusted internal path
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck // Safety-net close for error paths; success path closes explicitly via the return below

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	// Ensure data is flushed to disk
	if err := out.Sync(); err != nil {
		return err
	}

	return out.Close()
}
