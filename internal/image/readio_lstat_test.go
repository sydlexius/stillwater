package image

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLstatBounded_DanglingSymlinkReadsAsOccupied pins the Lstat-not-Stat
// choice in LstatBounded (#2930).
//
// Its caller -- the phash-quarantine restore's occupancy check -- treats a nil
// error as "something is here, refuse to write" and an IsNotExist as "the path
// is free, safe to write". A dangling symlink is a file the operator created,
// so it must read as OCCUPIED. os.Stat follows the link, fails with ENOENT, and
// would report the path free, licensing a write that replaces their link.
//
// Asserted here rather than left to the doc comment because the two calls are a
// one-word edit apart and every other test in the suite would stay green after
// the swap: the difference appears only on this input.
func TestLstatBounded_DanglingSymlinkReadsAsOccupied(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(dir, "dangling.jpg")
	if err := os.Symlink(filepath.Join(dir, "nonexistent-target.jpg"), link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	info, err := LstatBounded(context.Background(), link)
	if err != nil {
		t.Fatalf("LstatBounded on a dangling symlink = %v, want nil "+
			"(the caller reads a nil error as OCCUPIED and refuses to clobber; "+
			"an error here reports the path FREE and authorizes overwriting the operator's link)", err)
	}
	if info == nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("expected symlink mode bits, got %v", info)
	}

	// PRECONDITION on the assertion above: os.Stat really does disagree here,
	// so "Lstat returns nil" is a meaningful choice rather than a property both
	// calls happen to share on this input.
	if _, statErr := statCtx(context.Background(), link); !os.IsNotExist(statErr) {
		t.Errorf("precondition: os.Stat on a dangling symlink should report IsNotExist, got %v -- "+
			"without that disagreement this test proves nothing about the Lstat choice", statErr)
	}
}

// TestLstatBounded_AbsentPathReportsNotExist is the sibling case: a genuinely
// absent path must still report IsNotExist, or the occupancy check would refuse
// every restore and the feature would never write anything.
func TestLstatBounded_AbsentPathReportsNotExist(t *testing.T) {
	t.Parallel()
	_, err := LstatBounded(context.Background(), filepath.Join(t.TempDir(), "not-there.jpg"))
	if !os.IsNotExist(err) {
		t.Errorf("LstatBounded on an absent path = %v, want an IsNotExist error", err)
	}
}

// TestLstatBounded_HonorsCanceledContext proves the bound actually engages --
// the whole point of routing this call through the primitive rather than
// leaving it a raw os.Lstat.
func TestLstatBounded_HonorsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LstatBounded(ctx, t.TempDir()); err == nil {
		t.Error("LstatBounded with a canceled ctx returned nil; the ctx bound is not engaging")
	}
}
