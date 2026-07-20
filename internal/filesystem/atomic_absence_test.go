package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// TestWriteFileAtomic_TargetNeverAbsentDuringOverwrite is the regression test
// for #2661. WriteFileAtomic must overwrite an existing target atomically: at
// no instant may a concurrent reader observe the target file absent (ENOENT)
// while a write is in progress, given the target existed before the first
// write.
//
// The original tmp/bak/rename implementation renamed the existing target OUT to
// a .bak path BEFORE renaming the temp file INTO place, leaving a window in
// which the canonical target did not exist on disk. A concurrent reader
// (handleServeImage -> FindExistingImageStrict) that stat'd the target in that
// window got a clean ENOENT and treated a present file as absent -- firing a
// flag-clear on a file that was actually there.
//
// On the buggy code this test FAILS (records absences); after the fix (a single
// atomic rename that replaces the target in place) it PASSES. It is also run
// under -race by the package's -race suite.
func TestWriteFileAtomic_TargetNeverAbsentDuringOverwrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "artwork.jpg")

	// Precondition: the target exists before any write. This is the invariant
	// under test -- an overwrite must never make an already-present file
	// momentarily vanish.
	if err := os.WriteFile(target, []byte("initial"), 0o644); err != nil {
		t.Fatalf("seeding target: %v", err)
	}

	const writes = 400

	var absences int64 // count of ENOENT observations by the reader
	var stop int64     // set to 1 when the writer is done

	var wg sync.WaitGroup
	wg.Add(2)

	// Reader: tight-loop stat the target and record every ENOENT. Mirrors the
	// real hot reader that treats a missing file as "absent".
	go func() {
		defer wg.Done()
		for atomic.LoadInt64(&stop) == 0 {
			if _, err := os.Stat(target); err != nil {
				if os.IsNotExist(err) {
					atomic.AddInt64(&absences, 1)
				}
			}
		}
	}()

	// Writer: overwrite the same target many times.
	go func() {
		defer wg.Done()
		defer atomic.StoreInt64(&stop, 1)
		for i := 0; i < writes; i++ {
			payload := []byte(fmt.Sprintf("revision-%d", i))
			if err := WriteFileAtomic(target, payload, 0o644); err != nil {
				t.Errorf("WriteFileAtomic write %d: %v", i, err)
				return
			}
		}
	}()

	wg.Wait()

	if got := atomic.LoadInt64(&absences); got != 0 {
		t.Fatalf("reader observed the target absent %d times during concurrent overwrite; "+
			"the file must never be missing while a write is in progress", got)
	}
}
