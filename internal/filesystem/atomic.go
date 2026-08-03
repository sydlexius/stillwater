package filesystem

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// osRename is the rename function used by WriteFileAtomic to promote the temp
// file onto the target, and by RemoveFileSafe for both the move-to-tomb and the
// restore-from-tomb. It defaults to os.Rename and can be overridden in tests to
// simulate rename failures, following the same injectable-hook pattern used by
// renameFunc in rename.go. RemoveFileSafe routes through it (rather than calling
// os.Rename directly) so a test can fail the restore leg specifically, which is
// the only way to exercise the "file exists ONLY at the tomb path" outcome.
var osRename = os.Rename

// syncFile flushes a file's data to stable storage. It defaults to
// (*os.File).Sync and is a package-level var (same injectable-hook pattern as
// osRename) so a test can simulate an fsync failure, which real filesystems
// surface only on I/O errors that are impractical to provoke in a unit test.
var syncFile = (*os.File).Sync

// removeTomb unlinks the ".removing" tomb in RemoveFileSafe. It defaults to
// os.Remove and exists as an injectable hook (same pattern as osRename) because
// the recovery path it guards -- the unlink failing after the file has already
// been moved out of its real name -- cannot otherwise be provoked: an unlink of
// a plain file in a writable directory does not fail on demand, and making the
// directory unwritable is a no-op when the tests run as root.
var removeTomb = os.Remove

// writeTempFile writes data to f, restricts it to perm, flushes it to stable
// storage, and closes it.
// Extracted into a package-level var (rather than inlined in WriteFileAtomic)
// so tests can override it to simulate write/chmod/sync/close failures on the
// temp file, the same injectable-hook pattern osRename uses for the rename.
//
// The f.Sync() before Close is what makes the atomic replace crash-durable: a
// bare write() only lands data in the page cache, so a crash/power-loss between
// the write and the promoting rename could otherwise leave the renamed-in
// target zero-length or truncated (the classic ext3/ext4 zero-length-file
// problem). fsync forces the temp's data to disk before the rename promotes it,
// upholding this file's guarantee that an interrupted write never corrupts the
// target. The matching durability step for the rename ITSELF -- fsyncing the
// parent directory so the new directory entry survives a crash -- is done by
// SyncDir after the rename in WriteFileAtomic (issue #2673). Data fsync alone
// keeps the bytes and can still lose the name.
var writeTempFile = func(f *os.File, data []byte, perm os.FileMode) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if err := syncFile(f); err != nil {
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
//
// Every failure branch below emits an always-on record naming the step that
// failed and the state the filesystem was left in (issue #2636). The records
// are deliberately not gated behind the STILLWATER_TRACE_FS tracer: an operator
// investigating artwork that vanished needs them from the log they already
// have, not from a run they would have to reproduce with an env var set.
func WriteFileAtomic(target string, data []byte, perm os.FileMode) error {
	TraceFSWrite("WriteFileAtomic", target, 0)
	log := fsLog().With(slog.String("op", "WriteFileAtomic"), slog.String("path", target))

	// Ensure parent directory exists
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: 0755 is appropriate for application data directories
		log.Error("atomic write failed creating parent directory; target untouched",
			slog.String("step", "mkdir_parent"),
			slog.String("dir", dir),
			slog.String("error", err.Error()))
		return fmt.Errorf("creating parent directory: %w", err)
	}

	// Step 1: Write to a uniquely-named temp file (O_EXCL via os.CreateTemp),
	// then chmod to the caller's intended perm since CreateTemp always creates
	// the file 0o600 regardless of perm. The temp file lives in dir (the
	// target's directory), so the promoting rename below is same-filesystem and
	// cannot degrade to a non-atomic cross-device copy.
	tmpFile, err := os.CreateTemp(dir, filepath.Base(target)+".*.tmp")
	if err != nil {
		log.Error("atomic write failed creating temp file; target untouched",
			slog.String("step", "create_temp"),
			slog.String("dir", dir),
			slog.String("error", err.Error()))
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if err := writeTempFile(tmpFile, data, perm); err != nil {
		_ = tmpFile.Close()
		removeErr := os.Remove(tmpPath)
		// The target is untouched here -- nothing has been promoted yet -- so
		// this is a failed write, not a destruction. It is still Error, because
		// the caller's content did not reach disk and an orphaned temp may
		// remain if the cleanup itself failed.
		log.Error("atomic write failed staging temp file; target untouched",
			slog.String("step", "write_temp"),
			slog.String("temp_path", tmpPath),
			slog.Bool("temp_removed", removeErr == nil),
			slog.String("error", err.Error()))
		return fmt.Errorf("writing temp file: %w", err)
	}
	log.Debug("atomic write staged temp file",
		slog.String("step", "write_temp"),
		slog.String("temp_path", tmpPath),
		slog.Int("bytes", len(data)))

	// Step 2: Promote the temp file onto the target with a single atomic rename.
	// Because tmp and target are in the same directory (same filesystem), this
	// is a true rename that replaces any existing target in place -- the target
	// is never absent, and osRename never returns EXDEV here. We deliberately do
	// NOT wrap this in a copy-based cross-device fallback: a copy fallback
	// truncates-then-writes the destination, which is NOT atomic and would
	// reintroduce the very absence window this function must avoid. On failure
	// the target keeps its original content untouched; only the temp is removed.
	//
	// This is the loud case. Every other branch leaves the caller with the file
	// they started with; this one is the moment the old inode is dropped and the
	// new one takes the name. A failure here is the closest this design comes to
	// the "original moved aside and the replacement never arrived" window that
	// the pre-#2661 tmp/bak writer had, so it records at Error and states
	// explicitly what survived (issue #2636).
	if err := osRename(tmpPath, target); err != nil {
		removeErr := os.Remove(tmpPath)
		log.Error("atomic write failed promoting temp onto target; target retains its ORIGINAL content",
			slog.String("step", "rename_promote"),
			slog.String("temp_path", tmpPath),
			// target_intact is true unconditionally, and that is a claim this
			// writer's structure earns rather than an assumption: the original
			// is never moved aside, so a failed rename cannot have consumed it.
			// It is recorded rather than left implicit because the operator
			// reading this line is asking exactly one question -- is my file
			// gone -- and must not have to know the implementation to answer it.
			slog.Bool("target_intact", true),
			slog.Bool("temp_removed", removeErr == nil),
			slog.String("error", err.Error()))
		return fmt.Errorf("renaming temp to target: %w", err)
	}

	// The rename is done and the target now names the new inode. Make that
	// directory entry durable (issue #2673). A failure is recorded, not
	// returned: see syncTargetDir.
	syncTargetDir("WriteFileAtomic", dir, target)

	log.Debug("atomic write promoted temp onto target",
		slog.String("step", "rename_promote"),
		slog.Int("bytes", len(data)))
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
//
// The window between the rename-to-tomb and the unlink is the one interval in
// which the caller's file exists under a name nobody looks for. Both the entry
// into that window and every way out of it are recorded (issue #2636), and a
// failed unlink now attempts to put the file BACK at its original name rather
// than abandoning it at the tomb: the remove failed, so the correct end state
// is the file still present where it was, not present under a mangled name
// where the next scan will not find it.
func RemoveFileSafe(target string) error {
	TraceFSWrite("RemoveFileSafe", target, 0)
	log := fsLog().With(slog.String("op", "RemoveFileSafe"), slog.String("path", target))
	info, err := os.Lstat(target)
	if err != nil {
		// A missing target is the documented "already removed" answer and is
		// not an incident, so it stays at Debug; anything else is a genuine
		// probe failure and the removal did not happen.
		if os.IsNotExist(err) {
			log.Debug("safe remove found nothing to remove",
				slog.String("step", "lstat"))
		} else {
			log.Error("safe remove failed probing target; nothing removed",
				slog.String("step", "lstat"),
				slog.String("error", err.Error()))
		}
		return fmt.Errorf("removing %s: %w", target, err)
	}
	// Reject directory targets up front. Without this, the rename-then-unlink
	// flow can move a directory to "<dir>.removing" and then fail to unlink
	// it, leaving the user's tree in a half-renamed state.
	if info.IsDir() {
		log.Error("safe remove refused a directory target; nothing removed",
			slog.String("step", "lstat"))
		return fmt.Errorf("removing %s: target is a directory", target)
	}
	tomb := target + ".removing"
	// Best-effort cleanup of any prior tomb left over from a crash.
	_ = os.Remove(tomb)
	if err := osRename(target, tomb); err != nil {
		// Fall back to direct removal; better to remove than to abort. The
		// target was never moved, so either outcome here is unambiguous.
		if rerr := os.Remove(target); rerr != nil {
			log.Error("safe remove failed; file is still present at its original path",
				slog.String("step", "rename_to_tomb"),
				slog.String("tomb_path", tomb),
				slog.String("error", err.Error()),
				slog.String("direct_remove_error", rerr.Error()))
			return fmt.Errorf("removing %s: rename: %w; direct remove: %w", target, err, rerr)
		}
		log.Warn("safe remove fell back to a direct unlink; file removed",
			slog.String("step", "direct_remove"),
			slog.String("rename_error", err.Error()))
		return nil
	}
	// The file now exists only under the tomb name. From here until the unlink
	// below it is invisible to anything looking for the original path.
	log.Debug("safe remove moved file to tomb",
		slog.String("step", "rename_to_tomb"),
		slog.String("tomb_path", tomb))
	if err := removeTomb(tomb); err != nil {
		// The unlink failed, so the caller's file still exists -- but under a
		// name nothing else in the application looks for. Put it back.
		restoreErr := osRename(tomb, target)
		if restoreErr != nil {
			log.Error("safe remove failed to unlink the tomb AND failed to restore it; the file now exists ONLY at the tomb path",
				slog.String("step", "remove_tomb"),
				slog.String("tomb_path", tomb),
				slog.Bool("restored", false),
				slog.String("error", err.Error()),
				slog.String("restore_error", restoreErr.Error()))
			return fmt.Errorf("removing tomb %s: %w", tomb, err)
		}
		log.Warn("safe remove failed to unlink the tomb; file restored to its original path",
			slog.String("step", "remove_tomb"),
			slog.String("tomb_path", tomb),
			slog.Bool("restored", true),
			slog.String("error", err.Error()))
		return fmt.Errorf("removing tomb %s: %w", tomb, err)
	}
	log.Debug("safe remove unlinked file",
		slog.String("step", "remove_tomb"))
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
