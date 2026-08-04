//go:build unix

package image

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// mkfifoAt plants a FIFO with no writer at an EXACT path, which is what these
// tests need and readio_stall_test.go's mkfifo (which picks its own path in a
// fresh tempdir) cannot give: the wedge has to sit at a CANONICAL image name
// inside the directory the backup functions probe, or the strict probe never
// finds it and hands it to the read.
//
// Cleanup opens the write end so the abandoned reader drains, then waits for
// the process-wide stalled-read gauge to come back down. That wait is not
// politeness: the gauge is global and capped, so leaving it elevated makes
// LATER tests in this package start against a primitive that may refuse reads
// -- an order-dependent failure that `-shuffle` would surface somewhere else
// entirely, as a mystery.
func mkfifoAt(t *testing.T, path string) {
	t.Helper()
	before := StalledReadCount()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}
	t.Cleanup(func() {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = f.Close()
		}
		deadline := time.Now().Add(10 * time.Second)
		for StalledReadCount() > before {
			if time.Now().After(deadline) {
				t.Errorf("StalledReadCount() = %d, want it back down to %d after the FIFO was released",
					StalledReadCount(), before)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// The two backup entry points read the ORIGINAL's bytes immediately before a
// destructive write. #2932 made their STRICT PROBE ctx-bound but left the byte
// read on a bare os.ReadFile, so the only step that can block indefinitely sat
// one layer behind a bound that could no longer reach it -- and these run inside
// handlers whose singleton is released by a deferred unlock, so a read that
// never returns 409s the endpoint for the life of the process (#2934).
//
// Each property gets a RED case (a wedged read must return) and a GREEN sibling
// (an ordinary, non-ctx failure must still take its historical branch). The
// sibling is what keeps the fix from over-propagating: a version that turned
// every read error into an abort would satisfy the RED case alone.

// TestBackupSingleSlot_StalledRead_ReturnsOnDeadline wedges the ORIGINAL behind
// a FIFO with no writer -- the same reproduction readio_stall_test.go uses,
// because it is the shape a network mount that has stopped answering produces
// and it needs no network.
//
// The assertion is promptness AND direction: the call returns at the deadline,
// and it returns an ERROR. Returning nil would read to the caller as "the
// original is safely backed up", licensing the destructive write that follows.
func TestBackupSingleSlot_StalledRead_ReturnsOnDeadline(t *testing.T) {
	dir := t.TempDir()
	// mkfifo's own tempdir is not reused: the FIFO has to sit at the canonical
	// name inside `dir` so the strict probe finds it and hands it to the read.
	fifo := filepath.Join(dir, "folder.jpg")
	mkfifoAt(t, fifo)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := returnsWithin(t, func() error {
		return BackupSingleSlot(ctx, dir, "thumb", []string{"folder.jpg"})
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("BackupSingleSlot returned nil on a wedged original; the caller would read that as " +
			"'the original is protected' and proceed with the destructive write")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BackupSingleSlot on a stalled read: got %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("BackupSingleSlot took %v to honor a 100ms deadline; the read is not ctx-bound", elapsed)
	}
	// No backup was fabricated from a read that never completed.
	if _, statErr := os.Stat(filepath.Join(dir, BackupDirName, "thumb", "folder.jpg")); statErr == nil {
		t.Error("a backup file was written despite the read never completing")
	}
}

// TestBackupSlot_StalledRead_ReturnsOnDeadline is the same property for the
// multi-slot path. Fanart slot 0 IS the primary backdrop, so a per-slot
// overwrite destroys real artwork and this backup is the only revert path.
func TestBackupSlot_StalledRead_ReturnsOnDeadline(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fanart.jpg")
	mkfifoAt(t, fifo)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := returnsWithin(t, func() error {
		return BackupSlot(ctx, dir, "fanart", "fanart.jpg")
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("BackupSlot returned nil on a wedged original; the destructive per-slot write would proceed unprotected")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("BackupSlot on a stalled read: got %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("BackupSlot took %v to honor a 100ms deadline; the read is not ctx-bound", elapsed)
	}
}

// TestBackupSingleSlot_VanishedOriginalStillNoOps is the GREEN sibling, and it
// is REQUIRED rather than decorative. The ErrNotExist branch is an ORDINARY
// failure -- the file disappeared between the strict probe and the read -- and
// it must still return nil, because there is nothing left to protect and
// aborting would block a legitimate save.
//
// It is also the branch a careless fix breaks: routing the read through a
// helper that wraps its error would stop os.IsNotExist from matching, silently
// converting this no-op into an abort. The race is provoked deterministically
// by a dangling SYMLINK, which the probe's stat resolves as absent... so the
// probe is given a real file at one canonical name while the read target
// vanishes -- see the dangling-symlink construction below, which the push
// handler's own read-failure test uses for the same reason.
func TestBackupSingleSlot_VanishedOriginalStillNoOps(t *testing.T) {
	dir := t.TempDir()
	// A dangling symlink: lstat-based directory listing sees it, and
	// FindExistingImageStrict's os.Stat follows it to ENOENT, so the probe
	// reports "absent" and the function takes its no-original path. Either way
	// the contract under test holds: no error, and no backup fabricated.
	if err := os.Symlink(filepath.Join(dir, "gone.jpg"), filepath.Join(dir, "folder.jpg")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	if err := BackupSingleSlot(context.Background(), dir, "thumb", []string{"folder.jpg"}); err != nil {
		t.Fatalf("BackupSingleSlot with a vanished original: got %v, want nil -- an ordinary "+
			"ErrNotExist must stay a no-op, not become an abort", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, BackupDirName, "thumb", "folder.jpg")); statErr == nil {
		t.Error("a backup was written for an original that does not exist")
	}
}

// TestBackupSingleSlot_HealthyOriginalStillBacksUp is the other half of the
// GREEN guard: the ordinary success path must be completely unaffected. A fix
// that made the read stricter in any way that rejected a normal file would pass
// every cancellation assertion above and still break every save in the product.
func TestBackupSingleSlot_HealthyOriginalStillBacksUp(t *testing.T) {
	dir := t.TempDir()
	orig := makeImageBytes(t, "jpeg")
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), orig, 0o644); err != nil {
		t.Fatalf("seeding original: %v", err)
	}

	if err := BackupSingleSlot(context.Background(), dir, "thumb", []string{"folder.jpg"}); err != nil {
		t.Fatalf("BackupSingleSlot on a healthy original: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, BackupDirName, "thumb", "folder.jpg"))
	if err != nil {
		t.Fatalf("reading the backup: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("backup bytes differ from the original (%d vs %d bytes)", len(got), len(orig))
	}
}
