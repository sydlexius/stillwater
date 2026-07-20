package filesystem

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// osRename is the rename function used by WriteFileAtomic to promote the temp
// file onto the target. It defaults to os.Rename and can be overridden in tests
// to simulate rename failures, following the same injectable-hook pattern used
// by renameFunc in rename.go.
var osRename = os.Rename

// writeTempFile writes data to f, restricts it to perm, and closes it.
// Extracted into a package-level var (rather than inlined in WriteFileAtomic)
// so tests can override it to simulate write/chmod/close failures on the
// temp file, the same injectable-hook pattern osRename uses for the rename.
var writeTempFile = func(f *os.File, data []byte, perm os.FileMode) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return err
	}
	return f.Close()
}

// WriteFileAtomic writes data to the target path atomically: it stages the new
// content in a temp file and installs it with a single rename, so an
// interrupted write never corrupts the target and, crucially, a concurrent
// reader never observes the target absent while a write is in progress.
//
// Steps:
//  1. Write data to a uniquely-named temp file created via os.CreateTemp (O_EXCL)
//     in the TARGET'S OWN DIRECTORY, so concurrent writers targeting the same
//     path never collide on the temp name and the promoting rename below stays
//     on one filesystem
//  2. Rename the temp file onto <target>. POSIX rename(2) is an atomic replace
//     when source and destination share a filesystem, so any existing target is
//     swapped for the new inode in a single step -- the target is never missing
//     at any instant (see #2661)
//
// The old target inode is dropped by the rename itself, so no separate backup
// file is created or removed. Crash/failure recovery is structural: if the
// promoting rename fails, the target is left untouched with its original
// content (only the orphaned temp file is cleaned up), which is a stronger
// guarantee than restoring a moved-away .bak. The earlier design renamed the
// existing target OUT to a .bak before renaming the temp IN, which left a
// window in which the canonical target did not exist -- the bug this fixes.
func WriteFileAtomic(target string, data []byte, perm os.FileMode) error {
	TraceFSWrite("WriteFileAtomic", target, 0)

	// Ensure parent directory exists
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: 0755 is appropriate for application data directories
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Step 1: Write to a uniquely-named temp file (O_EXCL via os.CreateTemp),
	// then chmod to the caller's intended perm since CreateTemp always creates
	// the file 0o600 regardless of perm. The temp file lives in dir (the
	// target's directory), so the promoting rename below is same-filesystem and
	// cannot degrade to a non-atomic cross-device copy.
	tmpFile, err := os.CreateTemp(dir, filepath.Base(target)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if err := writeTempFile(tmpFile, data, perm); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}

	// Step 2: Promote the temp file onto the target with a single atomic rename.
	// Because tmp and target are in the same directory (same filesystem), this
	// is a true rename that replaces any existing target in place -- the target
	// is never absent, and osRename never returns EXDEV here. We deliberately do
	// NOT wrap this in a copy-based cross-device fallback: a copy fallback
	// truncates-then-writes the destination, which is NOT atomic and would
	// reintroduce the very absence window this function must avoid. On failure
	// the target keeps its original content untouched; only the temp is removed.
	if err := osRename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("renaming temp to target: %w", err)
	}

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
