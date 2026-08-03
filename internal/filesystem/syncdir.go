package filesystem

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"syscall"
)

// openDir is the directory-open used by SyncDir. It defaults to os.Open and is
// a package-level var (same injectable-hook pattern as osRename) so a test can
// simulate an open failure without needing an unreadable directory on disk,
// which is impossible to arrange when the test runs as root.
var openDir = os.Open

// syncDirHandle flushes an open directory handle. It is deliberately a
// SEPARATE hook from syncFile (atomic.go) even though both default to
// (*os.File).Sync: a test that overrides syncFile to simulate a temp-file
// fsync failure must not also break the unrelated directory sync, or the
// resulting test proves two things at once and neither cleanly.
var syncDirHandle = (*os.File).Sync

// SyncDir fsyncs a directory so that entries created or replaced in it by a
// preceding rename(2) or link(2) are durable across a crash (issue #2673).
//
// fsync on a FILE flushes that file's DATA. It says nothing about the directory
// entry that gives the data a NAME. On several filesystems a crash in the window
// after a rename but before the directory's own metadata reaches disk loses the
// entry: the bytes survive, the name does not, and the write already reported
// success. Every durability-critical fresh write in this application routes
// through WriteFileAtomic, so syncing the parent there covers the whole class;
// backup.Backup needs its own call because it installs its snapshot with
// os.Link rather than through this package.
//
// Platform reality: directory fsync is meaningful on Linux (where this ships,
// containerized) and on macOS. On Windows a directory cannot be opened for
// this purpose at all, and on some network filesystems the fsync returns
// EINVAL or ENOTSUP. Those are expected non-conditions rather than write
// failures, so they yield a nil error after a debug record. A caller must never
// fail an otherwise-successful write because the extra durability step was
// unavailable; the alternative is refusing to write files at all on those
// platforms.
//
// A genuine I/O error from the fsync IS returned, because that means the
// filesystem tried and failed, which callers should know about.
func SyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		// Windows has no equivalent operation; os.Open on a directory does not
		// yield a handle that can be flushed. Nothing to do, and not an error.
		return nil
	}
	d, err := openDir(dir)
	if err != nil {
		return fmt.Errorf("opening directory for sync: %w", err)
	}
	defer d.Close() //nolint:errcheck // Close error not actionable: Sync above is the durability step and the fd is read-only
	if err := syncDirHandle(d); err != nil {
		if isUnsupportedSync(err) {
			fsLog().Debug("directory fsync unsupported on this filesystem; write is still complete",
				slog.String("op", "SyncDir"),
				slog.String("path", dir),
				slog.String("error", err.Error()))
			return nil
		}
		return fmt.Errorf("syncing directory: %w", err)
	}
	return nil
}

// isUnsupportedSync reports whether err says the filesystem does not implement
// fsync on a directory, as opposed to having tried and failed. EINVAL and
// ENOTSUP are the two shapes seen in practice (some network and virtual
// filesystems); both mean "no such capability here", not "your data is at
// risk", so the write they follow stays successful.
func isUnsupportedSync(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}

// syncTargetDir fsyncs the directory holding path and records the outcome. It
// is the shared tail of every promoting rename in this package.
//
// A failed directory sync does NOT fail the write. The rename already happened
// and the target already holds the new content, so returning an error here
// would tell the caller its write failed when in fact it succeeded and is
// merely less crash-durable than intended. The record is the deliverable: an
// operator investigating a file that vanished after a power loss needs to see
// that the durability step was attempted and did not complete.
func syncTargetDir(op, dir, target string) {
	if err := SyncDir(dir); err != nil {
		fsLog().Warn("directory sync after rename failed; file is in place but its directory entry is not yet durable",
			slog.String("op", op),
			slog.String("path", target),
			slog.String("dir", dir),
			slog.String("error", err.Error()))
	}
}
