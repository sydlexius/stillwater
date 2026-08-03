//go:build unix

package rule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// This file covers the F1 ctx-propagation branch in quarantinedImagePresence
// (phash_repair.go) directly at the unit level. The api-package wedge tests
// (handlers_phash_repair_wedge_test.go) prove the end-to-end handler
// property -- returns, releases the singleton, re-claimable -- but drive
// through RestorePHashQuarantine's "exact" fast path, which never reaches
// the loop's mid-iteration ctx.Err() check added by this fix (a canceled
// ctx encountered on a candidate that is NOT the first). This test isolates
// that branch: a directory with TWO fanart files, the first a live file that
// reads fine, the second a FIFO that blocks, forcing the loop to observe a
// wedged read after already processing one candidate.

// wedgeUnitFifo plants a FIFO with no writer at path. Skips (rather than
// fails) on a platform without mkfifo. A writer opens at cleanup so the
// abandoned read can drain.
func wedgeUnitFifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}
	t.Cleanup(func() {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = f.Close()
		}
	})
}

// TestQuarantinedImagePresence_CanceledCtxPropagatesRatherThanContinuing is
// the F1 regression for the ctx-propagation branch specifically: with a FIFO
// standing in for a stalled mount, the function must return the ctx error
// rather than silently reporting "no duplicate found" (exact=false,
// similar=false, err=nil), because that false-negative result feeds
// restoreOneQuarantined's decision to WRITE the quarantined bytes back to
// disk as a brand-new slot.
func TestQuarantinedImagePresence_CanceledCtxPropagatesRatherThanContinuing(t *testing.T) {
	p, _ := newPHashRepairPipeline(t)
	dir := t.TempDir()

	// A first, perfectly healthy candidate so the loop has already iterated
	// at least once before it reaches the wedged one -- proving the ctx
	// check fires mid-loop, not merely on the first iteration.
	healthy := pollutionJPEG(t, 0)
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), healthy, 0o644); err != nil {
		t.Fatalf("writing fanart.jpg: %v", err)
	}

	fifoPath := filepath.Join(dir, "fanart2.jpg")
	wedgeUnitFifo(t, fifoPath)

	// The quarantined bytes being tested for presence: distinct from the
	// healthy file, so no early "exact" return short-circuits the loop
	// before it reaches the FIFO.
	data := pollutionJPEG(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	done := make(chan struct{})
	var exact, similar bool
	var err error
	go func() {
		exact, similar, err = p.quarantinedImagePresence(ctx, dir, "fanart.jpg", data)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("quarantinedImagePresence did not return within 5s of a 200ms ctx deadline; " +
			"the FIFO read is not ctx-bound")
	}
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a canceled ctx mid-loop must PROPAGATE as an error, not be swallowed as "+
			"exact=%v similar=%v -- a false negative here authorizes restoreOneQuarantined "+
			"to write a duplicate slot", exact, similar)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected the error to wrap context.DeadlineExceeded, got: %v", err)
	}
	if exact || similar {
		t.Errorf("on the propagated-error path, exact/similar must both be false (the zero value), got exact=%v similar=%v", exact, similar)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("quarantinedImagePresence took %v to honor a 200ms ctx deadline; the bound is not engaging", elapsed)
	}
}

// TestQuarantinedImagePresence_HealthyDirectoryStillWorks is the sibling
// GREEN case at the unit level: with no wedge present, the function still
// correctly reports byte-identical presence. Guards against a fix that
// propagates every read error (even a benign missing/oversize file) rather
// than only a genuine ctx cancellation, which would turn ordinary skip
// cases into hard failures.
func TestQuarantinedImagePresence_HealthyDirectoryStillWorks(t *testing.T) {
	p, _ := newPHashRepairPipeline(t)
	dir := t.TempDir()

	data := pollutionJPEG(t, 0)
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), data, 0o644); err != nil {
		t.Fatalf("writing fanart.jpg: %v", err)
	}

	exact, similar, err := p.quarantinedImagePresence(context.Background(), dir, "fanart.jpg", data)
	if err != nil {
		t.Fatalf("quarantinedImagePresence on a healthy directory: %v", err)
	}
	if !exact {
		t.Error("a byte-identical file on disk must report exact=true")
	}
	if !similar {
		t.Error("an exact match must also count as similar (exact is a stronger form of similar)")
	}
}
