package rule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// blockingHashRecorder holds the caller inside InvalidateImageHashes -- the
// FIRST thing img.RenumberFanart does, and the first thing that runs after
// deleteDuplicateFanartWithRollback has finished STAGING its tombs -- until
// release is closed, and then fails with err.
//
// That is the exact instant the concurrency hazard needs: invocation A has
// tombs on disk holding the operator's only copy of the staged bytes, and has
// not yet reached its commit point, so a rollback still has to be able to put
// them back.
//
// The hold is guarded by a sync.Once. img.RenumberFanart calls
// InvalidateImageHashes exactly once today, but a second call would panic on
// the double close and present itself as a mysterious test crash rather than as
// the contract change it is; the Once makes the hold "the FIRST time A reaches
// renumber", which is the property the test actually needs.
type blockingHashRecorder struct {
	once    sync.Once
	entered chan struct{} // closed by the recorder when A is held, post-staging
	release chan struct{} // closed by the test to let A proceed into failure
	err     error
}

func (b *blockingHashRecorder) UpdateImageHashes(_ context.Context, _, _ string, _ int, _, _ string) error {
	return nil
}

func (b *blockingHashRecorder) InvalidateImageHashes(_ context.Context, _, _ string) error {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.err
}

func (b *blockingHashRecorder) InvalidateImageGeometry(_ context.Context, _, _ string) error {
	return nil
}

// TestDeleteDuplicateFanart_ConcurrentInvocationCannotDestroyStagedBytes is the
// #3015 fix-round-3 regression test: it pins that a SECOND invocation against
// the same artist directory cannot destroy a FIRST invocation's live staged
// tomb, which would turn the first invocation's recoverable pre-commit failure
// into permanent loss of distinct artwork.
//
// THE DEFECT IT GUARDS. sweepOrphanedDupTombs is a directory-wide glob on
// dupTombSuffix. It cannot distinguish an orphan from a tomb an in-flight
// invocation staged moments ago, so without the per-directory serialization in
// deleteDuplicateFanartWithRollback the interleave is:
//
//	A stages fanart1.jpg -> fanart1.jpg.dup_pending_delete.tmp
//	B enters, sweeps, and removes A's LIVE tomb
//	A's RenumberFanart fails; A calls restoreStaged
//	A cannot restore -- the only copy of those bytes was in the tomb B deleted
//
// THE ASSERTION IS ON BYTES, NOT FILENAMES. Both invocations run
// RenumberFanart, which renames survivors positionally, so the file that holds
// a given byte sequence at the end need not be the file that held it at the
// start. What must hold is that EVERY original byte sequence is still
// recoverable somewhere in the directory: A rolled back, so nothing was
// deliberately deleted by either invocation.
//
// DETERMINISM. A is held at a real program point (inside RenumberFanart's first
// step, after staging) rather than by a sleep. The one bounded wait is the
// "B made no progress" leg, which is unavoidable: proving a goroutine is BLOCKED
// can only be done by observing that it did not finish. That wait is generous
// relative to what B does when it is NOT blocked (a glob, a discovery, and a
// rename loop over three files in a tmpdir, measured in microseconds), and the
// direction of any misjudgement is safe for the RED proof: with the lock removed
// B always completes long before the deadline, which is exactly the run that has
// to reproduce the loss.
//
// The RED/GREEN proof was measured by deleting the dupFanartDirMutex lock lines
// from deleteDuplicateFanartWithRollback: this test then reports fanart1.jpg's
// bytes missing from the directory (the loss reproduced) and passes with them
// restored.
func TestDeleteDuplicateFanart_ConcurrentInvocationCannotDestroyStagedBytes(t *testing.T) {
	// No t.Parallel: dupArtistDir gives this test its own directory, and the
	// per-directory mutex is keyed on that directory, so nothing here is shared
	// with another test -- but the package's unlinkHook convention is that
	// fixer tests do not run in parallel, and this one keeps to it.
	a, dir := dupArtistDir(t)

	// Every byte sequence that exists before either invocation runs. All three
	// must still be recoverable afterwards: A rolls back rather than committing,
	// and B is given an empty deletion set, so neither invocation is entitled to
	// destroy anything.
	want := map[string][]byte{}
	for _, name := range []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"} {
		want[name] = readBytes(t, filepath.Join(dir, name))
	}

	sentinel := errors.New("db is down")
	held := &blockingHashRecorder{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		err:     sentinel,
	}
	fixerA := newDupFixerFor(t, held)
	fixerB := newDupFixerFor(t, &fakeHashRecorder{})

	// Invocation A: stage fanart1.jpg, then hold inside RenumberFanart's first
	// step with the tomb on disk and the commit point not yet reached.
	aErr := make(chan error, 1)
	go func() {
		_, err := fixerA.deleteDuplicateFanartWithRollback(context.Background(), a, "fanart.jpg", false, map[int]bool{1: true})
		aErr <- err
	}()

	select {
	case <-held.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("invocation A never reached RenumberFanart: it did not stage, so this test never " +
			"establishes the live-tomb state the hazard needs")
	}

	// PRECONDITION on the fixture: A's tomb must actually be on disk right now.
	// Without it the sweep below has nothing to destroy and the whole test
	// passes vacuously, with or without the fix.
	tombs, globErr := filepath.Glob(filepath.Join(dir, "*"+dupTombSuffix))
	if globErr != nil {
		t.Fatalf("globbing for A's staged tomb: %v", globErr)
	}
	if len(tombs) != 1 {
		t.Fatalf("precondition failed: %d staged tomb(s) on disk while invocation A is held, want exactly 1; "+
			"invocation B has nothing of A's to destroy, so this test would pass without the fix", len(tombs))
	}

	// Invocation B: same artist, same directory, EMPTY deletion set. B is not
	// asked to delete anything -- its only destructive act is the orphan sweep,
	// which is precisely the step under test.
	bErr := make(chan error, 1)
	go func() {
		_, err := fixerB.deleteDuplicateFanartWithRollback(context.Background(), a, "fanart.jpg", false, map[int]bool{})
		bErr <- err
	}()

	// Give B every chance to run to completion while A is still held. With the
	// serialization in place B blocks on the directory mutex and this deadline
	// expires, which is the correct outcome; without it B finishes here and its
	// sweep has already destroyed A's tomb.
	bDoneEarly := false
	var bEarlyErr error
	select {
	case bEarlyErr = <-bErr:
		bDoneEarly = true
	case <-time.After(2 * time.Second):
	}
	if bDoneEarly && bEarlyErr != nil {
		t.Errorf("invocation B failed: %v", bEarlyErr)
	}

	// Release A into its RenumberFanart failure, which sends it down
	// restoreStaged -- the path that needs its tomb to still exist.
	close(held.release)

	if err := <-aErr; err == nil {
		t.Fatal("precondition failed: invocation A returned no error, so it never took the rollback path " +
			"this test is about")
	} else if !errors.Is(err, sentinel) {
		// A rollback that could not restore wraps the sentinel with rollback
		// errors, so errors.Is still holds -- a different error means A took
		// some other exit entirely and the run is not the one under test.
		t.Fatalf("precondition failed: invocation A failed with %v, not the injected renumber failure", err)
	}
	if !bDoneEarly {
		if err := <-bErr; err != nil {
			t.Errorf("invocation B failed: %v", err)
		}
	}

	// THE ASSERTION. Every original byte sequence is still somewhere in the
	// directory. A rolled back and B deleted nothing, so nothing may be gone.
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("reading the artist directory: %v", readErr)
	}
	onDisk := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		onDisk[string(readBytes(t, p))] = e.Name()
	}
	for name, bytesWanted := range want {
		if _, ok := onDisk[string(bytesWanted)]; !ok {
			t.Errorf("the bytes originally at %s are no longer anywhere in the artist directory: a "+
				"concurrent invocation's orphan sweep destroyed the live staged tomb holding them, so "+
				"invocation A's recoverable pre-commit failure became permanent data loss (#3015)", name)
		}
	}

	// And the rollback left no tomb behind, which is what makes the byte check
	// above a statement about RESTORED files rather than about bytes that merely
	// happen to still sit inside a tomb.
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("a staged tomb (%s) survived: restoreStaged did not put every staged file back", e.Name())
		}
	}
}
