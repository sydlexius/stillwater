package deleteintenttest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	img "github.com/sydlexius/stillwater/internal/image"
)

// The probe is a test helper whose verdict gates seven production call sites'
// assertions, so its own selection rules need pinning: a helper that silently
// counted the wrong unlink would hand every caller a green light for free.
//
// Only the NON-FAILING behavior is exercised here. The failure branches call
// t.Fatalf on the real *testing.T, which cannot be driven from inside a test
// without a fake harness; they are instead proven by the two-sided relocation
// proofs in internal/api and internal/rule, where the same branches fire
// against real production code and were measured RED.

// TestAround_IgnoresPathsOutsideTheWatchedDirectory pins the scoping rule. A
// handler may remove a temp file elsewhere in the same operation, and counting
// that as "the first unlink" would make the assertion depend on incidental
// ordering rather than on where the mark sits.
func TestAround_IgnoresPathsOutsideTheWatchedDirectory(t *testing.T) {
	watched := t.TempDir()
	other := t.TempDir()
	probe := NewUnlinkProbe(t, watched, "fanart")

	// A sibling directory, and a SUBdirectory of the watched one: the marker's
	// key is the image directory itself, so neither bears on the claim.
	sub := filepath.Join(watched, "nested")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("creating nested dir: %v", err)
	}
	for _, p := range []string{filepath.Join(other, "a.jpg"), filepath.Join(sub, "b.jpg")} {
		if err := probe.Around(p, func() error { return nil }); err != nil {
			t.Fatalf("Around(%s) returned %v; it must pass an unwatched removal through unchanged", p, err)
		}
	}
	if got := probe.UnlinkCount(); got != 0 {
		t.Errorf("UnlinkCount = %d after removals outside the watched directory, want 0; a probe that "+
			"counted them would report the wrong instant as the first unlink", got)
	}
}

// TestAround_DoesNotCountAFailedRemoval pins the success rule, which is what
// keeps a no-op attempt from presenting itself as the first unlink. The
// duplicate fixer clears a possibly-absent stale tomb before it stages
// anything, so this case is reached in production, not just in theory.
func TestAround_DoesNotCountAFailedRemoval(t *testing.T) {
	dir := t.TempDir()
	probe := NewUnlinkProbe(t, dir, "fanart")

	sentinel := errors.New("no such file")
	err := probe.Around(filepath.Join(dir, "gone.jpg"), func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("Around returned %v, want the removal's own error unchanged: a probe that swallowed or "+
			"replaced it would change the behavior of the code under test", err)
	}
	if got := probe.UnlinkCount(); got != 0 {
		t.Errorf("UnlinkCount = %d after a FAILED removal, want 0; nothing vanished, so no window opened "+
			"and there is no instant to sample", got)
	}
}

// TestAround_CountsEverySuccessfulWatchedRemoval is the positive control for
// the two above: without it they would pass for a probe that counted nothing at
// all, which is the vacuity shape this whole mechanism exists to catch.
func TestAround_CountsEverySuccessfulWatchedRemoval(t *testing.T) {
	dir := t.TempDir()
	probe := NewUnlinkProbe(t, dir, "fanart")
	img.MarkDeleteIntent(dir, "fanart")

	for _, name := range []string{"fanart.jpg", "fanart1.jpg"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		if err := probe.Around(p, func() error { return os.Remove(p) }); err != nil {
			t.Fatalf("Around(%s): %v", name, err)
		}
	}
	if got := probe.UnlinkCount(); got != 2 {
		t.Errorf("UnlinkCount = %d, want 2", got)
	}
	probe.AssertMarkedBeforeUnlink("a marker written before both removals")
}

// TestAssertNeverMarked_PassesWhenNoTypeCarriesAMarker pins AssertNeverMarked's
// non-failing path -- the loop runs to completion and reports nothing when none
// of the watched types carry a marker of any age. It is the negative sibling of
// TestAround_CountsEverySuccessfulWatchedRemoval, watching for the opposite
// outcome instead of a removal.
//
// This is the ONE branch of AssertNeverMarked drivable from a normal test: the
// failing branch calls t.Errorf on the real *testing.T, and stays proven the
// way this file's header says every failure branch is proven here -- by the
// two-sided relocation proofs in internal/api and internal/rule, where the
// duplicate fixer's rollback path was measured to leave a marker behind when it
// should not have, and the corresponding assertion went red against real
// production code.
func TestAssertNeverMarked_PassesWhenNoTypeCarriesAMarker(t *testing.T) {
	dir := t.TempDir()

	// PRECONDITION, asserted directly against the marker store rather than
	// inferred from NewUnlinkProbe's own construction-time check: without this,
	// a probe silently reading the wrong directory or the wrong types would
	// still pass below for having asserted nothing.
	for _, ty := range []string{"fanart", "thumb"} {
		if img.DeleteIntentAfter(dir, ty, time.Time{}) {
			t.Fatalf("precondition failed: %s already carries a %s delete marker, so this test would not "+
				"be observing AssertNeverMarked's non-failing path", dir, ty)
		}
	}

	// Watching two types, not one: AssertNeverMarked loops over p.types, and a
	// probe that returned after checking only the first would pass this test
	// for the wrong reason.
	probe := NewUnlinkProbe(t, dir, "fanart", "thumb")
	probe.AssertNeverMarked("no delete occurred for either watched type")
}
