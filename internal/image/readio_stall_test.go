//go:build unix

package image

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO with no writer is the reproduction the issue used: opening it for
// reading blocks in the kernel until a writer appears, which is the same shape
// a network mount that has stopped answering produces, and it is reproducible
// without a network. Unix-only because mkfifo is.
//
// The bound below is what every test in this file asserts: the CALL returns
// promptly with the context's error. It deliberately does NOT assert that the
// underlying read stopped -- it has not, and cannot be made to (see readio.go).
// Returning control to the caller while the read stays stuck is the entire
// mechanism, so a test demanding the goroutine exit would be asserting
// something the fix does not and cannot claim.
func mkfifo(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stalled.jpg")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}
	// Open a writer at test end so the abandoned reader can drain and exit
	// rather than outliving the test binary and tripping a goroutine leak
	// check in some later package.
	t.Cleanup(func() {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = f.Close()
		}
	})
	return path
}

// stallBound is how long a call under a sub-second deadline is given before
// the test declares it wedged. It is a shared constant rather than a per-call
// argument because every caller wants the same thing: an interval long enough
// that CI scheduling noise cannot produce a false failure, yet far shorter than
// the go test timeout, so a genuine regression is reported as THIS test failing
// rather than as an unrelated binary-wide panic. The individual tests still
// assert PROMPTNESS separately against their own much tighter bound; this
// constant only bounds the hang.
const stallBound = 5 * time.Second

// returnsWithin fails unless fn returns within stallBound. It runs fn on its
// own goroutine precisely because a REGRESSION here means fn never returns: a
// plain call would hang the test binary until the go test timeout and report
// as an unrelated panic rather than as this test failing.
func returnsWithin(t *testing.T, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(stallBound):
		t.Fatalf("call did not return within %v; the read is still wedging its caller", stallBound)
		return nil
	}
}

// TestHashFile_StalledRead_ReturnsOnDeadline is the issue's exact repro
// (#2689): HashFile against a FIFO with no writer was still blocked 3 seconds
// after a 1-second deadline had expired. It must now return at the deadline.
func TestHashFile_StalledRead_ReturnsOnDeadline(t *testing.T) {
	path := mkfifo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := returnsWithin(t, func() error {
		_, err := HashFile(ctx, path, true)
		return err
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HashFile on a stalled read: got %v, want it to wrap context.DeadlineExceeded", err)
	}
	// Promptness, not merely eventual return: a fix that returned only after
	// the read completed would still satisfy the error assertion above on a
	// mount that recovers, and would satisfy nothing on one that does not.
	if elapsed > 2*time.Second {
		t.Fatalf("HashFile took %v to honor a 100ms deadline; the bound is not engaging", elapsed)
	}
}

// TestDiscoverFanart_CanceledContext_DoesNotScan pins the directory reader to
// the same contract. A canceled context must not be answered with an
// error-free empty listing: an empty result is a POSITIVE claim that no fanart
// is on disk, and callers delete registry rows on the strength of it (#2635).
func TestDiscoverFanart_CanceledContext_DoesNotScan(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding fanart: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	paths, err := DiscoverFanart(ctx, dir, "fanart.jpg")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DiscoverFanart with a canceled context: got err %v, want context.Canceled", err)
	}
	if paths != nil {
		t.Fatalf("DiscoverFanart with a canceled context returned paths %v; a canceled scan must never report a result", paths)
	}
}

// TestFindExistingImageStrict_CanceledContext_IsUnverifiable is the same
// invariant for the stat probe, and it is the one with teeth: this function's
// callers clear exists_flag and skip restores on found==false, so a canceled
// probe reported as a clean miss would be read as "the artwork is gone".
func TestFindExistingImageStrict_CanceledContext_IsUnverifiable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "folder.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding image: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, found, err := FindExistingImageStrict(ctx, dir, []string{"folder.jpg"})
	if found {
		t.Fatal("FindExistingImageStrict with a canceled context reported found=true without probing")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FindExistingImageStrict with a canceled context: got err %v, want context.Canceled -- "+
			"a clean (false, nil) miss would license a caller to clear the flag", err)
	}
}

// TestReadFileBounded_PreservesLimitBoundary pins the MaxDecodeBytes+1
// semantics through the new primitive.
//
// This is the landmine the refactor had to walk past: the old inline read used
// io.LimitReader(f, MaxDecodeBytes+1) and compared len(data) > MaxDecodeBytes,
// so "exactly at the limit" passes and "one byte over" is rejected. A helper
// taking a raw read length would move that boundary by one byte silently, and
// nothing else in the suite would notice. Both sides of the boundary are
// asserted, because a test of only the over-limit case passes just as happily
// against an off-by-one that rejects the exact-limit file too.
func TestReadFileBounded_PreservesLimitBoundary(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, n int64) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return p
	}

	atLimit := write("at.bin", MaxDecodeBytes)
	data, err := readFileBounded(context.Background(), atLimit)
	if err != nil {
		t.Fatalf("readFileBounded at exactly the limit: unexpected error %v", err)
	}
	if int64(len(data)) != MaxDecodeBytes {
		t.Fatalf("readFileBounded at the limit returned %d bytes, want %d", len(data), MaxDecodeBytes)
	}

	overLimit := write("over.bin", MaxDecodeBytes+1)
	if _, err := readFileBounded(context.Background(), overLimit); !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("readFileBounded one byte over the limit: got %v, want ErrImageTooLarge", err)
	}
}

// TestStalledReadCap_RefusesRatherThanAccumulating is the accumulation bound
// (#2689's open question). One canceled operation abandons at most one read,
// but nothing about that bounds a SEQUENCE of operator retries against a
// permanently-dead mount, so the count is capped process-wide.
//
// The assertion that matters is the DIRECTION of the refusal: past the cap the
// helper returns an ERROR, which every consumer treats as "could not look --
// skip and report", never as "nothing is there". A cap that returned an empty
// result instead would turn a dead mount into a license to delete.
func TestStalledReadCap_RefusesRatherThanAccumulating(t *testing.T) {
	path := mkfifo(t)

	var lastErr error
	for i := 0; i < maxStalledReads+1; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		lastErr = returnsWithin(t, func() error {
			_, err := HashFile(ctx, path, false)
			return err
		})
		cancel()
	}

	if !errors.Is(lastErr, ErrTooManyStalledReads) {
		t.Fatalf("after %d retries against a dead read: got %v, want ErrTooManyStalledReads -- "+
			"the accumulation cap is not engaging", maxStalledReads+1, lastErr)
	}
	if got := StalledReadCount(); got < maxStalledReads {
		t.Fatalf("StalledReadCount() = %d after saturating the cap, want >= %d", got, maxStalledReads)
	}

	// Unblock the abandoned readers and wait for the gauge to come back down,
	// INLINE rather than in a t.Cleanup: cleanups run LIFO, so a drain
	// registered here would run BEFORE mkfifo's writer-open and would wait on
	// reads nothing had released yet.
	//
	// Two things ride on this. Practically, the gauge is process-wide, so
	// leaving it saturated would make every later test in this package start
	// against a refusing primitive -- an order-dependent failure `-shuffle`
	// would surface as a mystery somewhere else entirely. Substantively, it
	// asserts the SELF-HEALING claim readio.go makes: a mount that comes back
	// must return the gauge to where it started, or the cap is a one-way
	// ratchet that eventually bricks the process.
	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening the FIFO write end to release the stalled reads: %v", err)
	}
	_ = w.Close()

	deadline := time.Now().Add(10 * time.Second)
	for StalledReadCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("StalledReadCount() = %d after the reads were unblocked, want 0; "+
				"the cap does not self-heal when the mount recovers", StalledReadCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRunCancellable_CompletedReadDoesNotLeakCount guards the gauge against
// ratcheting on a near-miss. A read that completes normally must leave the
// count where it found it; otherwise a busy-but-HEALTHY mount would creep
// toward the cap and start refusing reads it has no reason to refuse.
func TestRunCancellable_CompletedReadDoesNotLeakCount(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ok.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatalf("seeding file: %v", err)
	}

	before := StalledReadCount()
	for range 50 {
		if _, err := readFileBounded(context.Background(), p); err != nil {
			t.Fatalf("readFileBounded on a healthy file: %v", err)
		}
	}
	if after := StalledReadCount(); after != before {
		t.Fatalf("StalledReadCount moved from %d to %d across 50 successful reads; "+
			"a completed read must not be counted as abandoned", before, after)
	}
}
