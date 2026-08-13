//go:build unix

package publish

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// #2712 review, N1: the caps must bound what is RESIDENT, not what a stat
// predicted.
//
// The pre-read refusal in snapshotFanart takes the file's size from os.Stat.
// A stat is a prediction, and internal/image's readio.go argues against exactly
// this pattern for exactly this reason: the file can change between the stat
// and the read, so the check bounds a NUMBER while the read stays unbounded.
// That matters more here than at a single read site, because snapshotFanart
// holds up to maxFanartSnapshotFiles results AT ONCE -- believing every stat
// would permit 100 reads of img.MaxDecodeBytes (25 MB) to sit resident against
// a documented 192 MiB bound, roughly 2.5 GB.
//
// WHY A FIFO. The disagreement between stat and read has to be real, and on an
// ordinary file it is a genuine race with a concurrent writer, which is not
// something a test can arrange deterministically. A named pipe makes the same
// disagreement a PROPERTY rather than a race: os.Stat reports size 0 for a FIFO
// no matter how many bytes are about to come through it, so the pre-read check
// waves it past, and the read then delivers the writer's full payload. That is
// the same shape as a file that grew, arrived at deterministically.
//
// It is a real production shape too, not merely a device for the test: junk
// ends up in artist directories, and DiscoverFanart selects by name.
//
// Unix-only because mkfifo is. The predicate itself is platform-independent and
// is covered directly in snapshot_bound_test.go; what this file adds is the
// WIRING -- that snapshotFanart actually consults it and discards the bytes.

// fifoDrainTimeout bounds every wait on a FIFO writer in this file.
//
// It is LONG on purpose, and 2s was measurably wrong. These helpers push up to
// 25 MiB through the pipe, so a deadline chosen for "the writer is wedged in
// open(2)" also fires on a writer that is simply slow under a loaded CI runner
// -- which reports a healthy fixture as a failure. A timeout that is too long
// only costs wall-clock on a run that was already going to fail; one that is
// too short invents failures on runs that were fine. The sibling FIFO helpers
// in snapshot_stall_cap_unix_test.go use the same deadline.
const fifoDrainTimeout = 30 * time.Second

// fifoDeliveringBytes creates a FIFO at path and starts a writer that sends n
// bytes and closes.
//
// The writer runs in its own goroutine because opening a FIFO for writing
// blocks until a reader opens it, and the reader here is the code under test.
// A t.Cleanup registered here joins the writer, so no goroutine outlives the
// test even when the read side never opens. Nothing is returned: the caller
// takes the fixture and the cleanup happens on its own.
func fifoDeliveringBytes(t *testing.T, path string, n int64) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// O_WRONLY blocks until a reader arrives. If the read side never opens
		// (a failing test, an early return), this open never completes, so the
		// cleanup below cannot join on it alone -- it opens the READ end
		// non-blocking as an escape hatch instead. See the cleanup.
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			// FAIL LOUDLY. Returning silently here leaves nothing feeding the
			// FIFO, so the read under test blocks in open(2) until the package
			// timeout kills the whole binary -- a hang whose reported cause is
			// unrelated to the real one. t.Errorf rather than t.Fatal: this is
			// not the test goroutine, and Fatal from here would not stop it.
			t.Errorf("fixture failed: cannot open the FIFO %q for writing: %v; the read under test "+
				"will block with nothing to deliver", path, err)
			return
		}
		defer func() { _ = f.Close() }()
		buf := make([]byte, 64<<10)
		for written := int64(0); written < n; {
			chunk := int64(len(buf))
			if remaining := n - written; remaining < chunk {
				chunk = remaining
			}
			// A short write or a broken pipe means the reader gave up. That is
			// EXPECTED here: the code under test refuses the over-budget read
			// and closes, which breaks the pipe by design, so this is not
			// reported as a fixture failure the way the open above is.
			w, writeErr := f.Write(buf[:chunk])
			if writeErr != nil {
				return
			}
			written += int64(w)
		}
	}()

	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		// fifoDrainTimeout, NOT a short local guess. This helper delivers up to
		// 25 MiB through the pipe, so a deadline sized for "stuck in open(2)"
		// also fires on a writer that is merely slow on a loaded runner, turning
		// a healthy test red. Every FIFO helper in this package uses the same
		// long deadline for the same reason (snapshot_stall_cap_unix_test.go).
		case <-time.After(fifoDrainTimeout):
			// The writer is still parked in open(2) because nothing ever read.
			// Open the READ end NON-BLOCKING to release it: O_RDONLY|O_NONBLOCK
			// on a FIFO returns immediately whether or not a writer is waiting,
			// which O_RDWR does not guarantee across every Unix.
			if f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = f.Close()
			}
			// And do NOT wait unguarded afterwards. If the release above failed
			// for any reason, a bare wg.Wait() here parks forever and takes the
			// whole test binary down with it on the go-test timeout, reported as
			// an unrelated panic. A leaked writer goroutine in a test process
			// that is exiting anyway is the cheaper failure.
			select {
			case <-done:
			case <-time.After(fifoDrainTimeout):
				t.Errorf("the FIFO writer for %q never drained; releasing the parked open(2) did not "+
					"work on this platform", path)
			}
		}
	})
}

// TestSnapshotFanart_ReadOvershootsTheStat_Discarded is the WIRING test for the
// post-read cap: it proves snapshotFanart consults refuseResult and throws the
// over-budget bytes away, rather than merely owning a correct predicate nobody
// calls.
//
// Without the post-read check the stat says 0, the pre-read refusal passes, and
// the snapshot retains a 13 MiB entry that is over the 12 MiB per-file cap --
// the cap bounding a number while the process holds the bytes.
func TestSnapshotFanart_ReadOvershootsTheStat_Discarded(t *testing.T) {
	// No t.Parallel; see the note at the top of snapshot_bound_test.go.
	dir := t.TempDir()

	small := filepath.Join(dir, "fanart.jpg")
	if err := os.WriteFile(small, []byte("an ordinary backdrop"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	liar := filepath.Join(dir, "fanart1.jpg")
	const delivered = maxFanartSnapshotFileBytes + (1 << 20) // 13 MiB
	fifoDeliveringBytes(t, liar, delivered)

	// PRECONDITIONS. Both are what make the assertion below meaningful: the stat
	// must genuinely under-report (otherwise the pre-read cap catches it and
	// this test says nothing about the post-read one), and the payload must
	// genuinely exceed the cap.
	info, statErr := os.Stat(liar)
	if statErr != nil {
		t.Fatalf("precondition failed: cannot stat the FIFO: %v", statErr)
	}
	if info.Size() > maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: the stat reports %d bytes, which the PRE-read cap already refuses; "+
			"this test needs a stat that under-reports", info.Size())
	}
	if delivered <= maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: the payload of %d bytes is inside the %d-byte per-file cap, so no "+
			"refusal is owed", int64(delivered), int64(maxFanartSnapshotFileBytes))
	}

	paths := []string{small, liar}
	p := boundTestPublisher()
	snapshot, warnings, err := p.snapshotFanart(context.Background(), paths)
	if err != nil {
		t.Fatalf("snapshotFanart returned %v; an over-budget read is a per-file degrade, not a reason to "+
			"abort the set", err)
	}
	assertSnapshotShape(t, snapshot, paths)

	if snapshot[0].data == nil {
		t.Error("the ordinary backdrop was not captured; one over-budget neighbor must not cost it its " +
			"restore bytes")
	}
	if snapshot[1].data != nil {
		t.Fatalf("the over-budget read was RETAINED (%d bytes, cap %d). The stat said %d, so the pre-read "+
			"check waved it past -- only a check on the bytes actually read bounds what this push holds",
			len(snapshot[1].data), int64(maxFanartSnapshotFileBytes), info.Size())
	}
	// The degrade must be loud, exactly as it is for a pre-read refusal: the
	// slot holds no bytes, so a peer delete of it during this push cannot be
	// repaired, and silence would make that invisible.
	if joined := strings.Join(warnings, " | "); !strings.Contains(joined, "fanart 1") {
		t.Errorf("warnings %q do not name the refused slot", joined)
	}
	// WHICH check refused it (#2712 review, N8). Asserting only that slot 1 was
	// refused let an unrelated mutation pass: rejecting the FIFO as a
	// non-regular file BEFORE the read produces a nil-data entry naming slot 1
	// too, so the test went green while never exercising the post-read cap it
	// exists for. The read wording is the only thing that says the bytes were
	// actually delivered and then thrown away.
	if joined := strings.Join(warnings, " | "); !strings.Contains(joined, readRefusalPhrase) {
		t.Errorf("warnings %q do not carry the POST-read refusal wording %q, so slot 1 was refused for "+
			"some other reason; this test says nothing about the TOCTOU backstop unless the refusal came "+
			"from the length actually read", joined, readRefusalPhrase)
	}
	if joined := strings.Join(warnings, " | "); strings.Contains(joined, statRefusalPhrase) {
		t.Errorf("warnings %q carry the PRE-read wording %q, but the stat reported %d bytes and cannot "+
			"have refused this slot", joined, statRefusalPhrase, info.Size())
	}
}

// unreadableOversizeFile plants a file whose STAT is honestly over the per-file
// cap and whose READ cannot succeed, then reports whether this environment can
// actually deny the read.
//
// The combination is the whole trick. A stat needs only search permission on the
// parent directory, so mode 0o000 leaves the size perfectly visible while making
// the bytes unobtainable. Any code path that consults the stat sees an
// over-cap file; any code path that reaches the read gets EACCES. That makes the
// two observable from one fixture, which is what the tests below discriminate
// on.
//
// Sparse (a truncate, no bytes written), so a 13 MiB fixture costs no disk.
func unreadableOversizeFile(t *testing.T, path string, size int64) bool {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating fixture %s: %v", path, err)
	}
	if truncErr := f.Truncate(size); truncErr != nil {
		_ = f.Close()
		t.Fatalf("sizing fixture %s to %d: %v", path, size, truncErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		t.Fatalf("closing fixture %s: %v", path, closeErr)
	}
	if chmodErr := os.Chmod(path, 0o000); chmodErr != nil {
		t.Fatalf("removing read permission from %s: %v", path, chmodErr)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	// Probe rather than infer. os.Geteuid() == 0 is the usual guard but it is
	// too narrow on its own: a non-root user can still read a 0o000 file through
	// an ACL or a capability, and this fixture is worthless in that case because
	// the read would succeed and the test would silently measure the wrong
	// thing. This is a genuine permission-environment skip, not a
	// data-dependent one.
	probe, readErr := os.Open(path)
	if readErr == nil {
		_ = probe.Close()
		return false
	}
	return true
}

// TestSnapshotFanart_HonestlyHugeButUnreadable_RefusedBeforeTheRead is the
// guard for the pre-read half of the cap (#2712 review, B2).
//
// WHAT WAS UNGUARDED. The two-stage design rests on a claim about ORDER: the
// stat refuses an honestly-huge file so its bytes are never allocated at all,
// and refuseResult is only the TOCTOU backstop for a file that lied. Nothing
// tested the claim. Three mutations that deleted the pre-read half outright --
// dropping the per-file size check, dropping the total accounting, and dropping
// the stat and both checks so refuse became count-only -- all left the suite
// green, because refuseResult caught the very same files a moment later and
// every assertion was about the OUTCOME (nil data, a warning, the preserved
// index), which is identical either way.
//
// HOW THIS SEES THE DIFFERENCE. Ordering is not directly observable, so the
// test observes something that follows from it: a file the stat refuses is one
// the read never touches. Make the read IMPOSSIBLE and the two orders diverge
// loudly. With the stat check present the slot is refused for being over-size
// and the warning says so; without it, control reaches the read, gets EACCES,
// and the warning becomes an I/O failure instead. Same nil-data entry, same
// index, different sentence -- and the sentence is what is asserted.
func TestSnapshotFanart_HonestlyHugeButUnreadable_RefusedBeforeTheRead(t *testing.T) {
	// No t.Parallel; see the note at the top of snapshot_bound_test.go.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits, so the read cannot be made to fail")
	}
	dir := t.TempDir()

	small := filepath.Join(dir, "fanart.jpg")
	if err := os.WriteFile(small, []byte("an ordinary backdrop"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	huge := filepath.Join(dir, "fanart1.jpg")
	const size = int64(maxFanartSnapshotFileBytes) + (1 << 20) // 13 MiB
	if !unreadableOversizeFile(t, huge, size) {
		t.Skip("environment can read a 0o000 file (ACL or capabilities); the read cannot be made to fail")
	}

	// PRECONDITIONS, both directions, because this fixture has two properties
	// and losing either one makes the assertion below vacuous rather than
	// failing.
	//
	// One: the stat must genuinely report an over-cap size. If it did not, the
	// pre-read check would have nothing to refuse and the test would be
	// asserting the wording of a check that never ran.
	info, statErr := os.Stat(huge)
	if statErr != nil {
		t.Fatalf("precondition failed: cannot stat the fixture: %v", statErr)
	}
	if info.Size() <= maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: the fixture stats at %d bytes, inside the %d-byte per-file cap, so "+
			"the pre-read check owes no refusal", info.Size(), int64(maxFanartSnapshotFileBytes))
	}
	// Two: the read must genuinely fail. If it could succeed, the pre-read and
	// post-read orders would produce the same outcome again and the test would
	// be back to proving nothing.
	if f, openErr := os.Open(huge); openErr == nil {
		_ = f.Close()
		t.Fatal("precondition failed: the fixture is readable, so a missing pre-read check would be " +
			"invisible exactly as it was before this test existed")
	}

	paths := []string{small, huge}
	p := boundTestPublisher()
	snapshot, warnings, err := p.snapshotFanart(context.Background(), paths)
	if err != nil {
		t.Fatalf("snapshotFanart returned %v; an over-size file is a per-file degrade, not a reason to "+
			"abort the set", err)
	}
	assertSnapshotShape(t, snapshot, paths)

	if snapshot[0].data == nil {
		t.Error("the ordinary backdrop was not captured; one refused neighbor must not cost it its bytes")
	}
	if snapshot[1].data != nil {
		t.Fatalf("the over-size slot holds %d bytes", len(snapshot[1].data))
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want exactly 1: %v", len(warnings), warnings)
	}
	// THE ASSERTION THIS TEST EXISTS FOR. "bytes on disk" can only have come
	// from the stat, because the read never returned any bytes to measure.
	assertStatRefusal(t, warnings[0])
	if strings.Contains(warnings[0], readFailurePhrase) {
		t.Errorf("warning %q reports an I/O failure, so control reached the read: the pre-read stat check "+
			"is gone and an honestly-huge file is now allocated before being refused", warnings[0])
	}
	assertNamesBothLosses(t, warnings[0])
}

// TestSnapshotFanart_StatFailure_IsNotABudgetRefusal pins the deliberate
// fall-through in fanartSnapshotBudget.refuse (#2712 review, N4).
//
// refuse returns "not refused" when os.Stat fails, and its comment argues the
// case: a stat failure is a permissions or I/O problem, the read that follows
// reports it with the real cause, and inventing a budget refusal would send the
// operator to look at their backdrop sizes for what is actually a file
// permission. That reasoning had no test, so a mutation turning a failed stat
// into a refusal was free.
//
// The fixture makes the stat itself fail, which needs the PARENT directory
// stripped of search permission rather than the file. A directory at mode 0o000
// denies both the stat and the read, so what the operator is told comes entirely
// from which branch handled it.
func TestSnapshotFanart_StatFailure_IsNotABudgetRefusal(t *testing.T) {
	// No t.Parallel; see the note at the top of snapshot_bound_test.go.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits, so the stat cannot be made to fail")
	}
	parent := t.TempDir()
	locked := filepath.Join(parent, "artist")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatalf("creating fixture directory: %v", err)
	}
	hidden := filepath.Join(locked, "fanart.jpg")
	if err := os.WriteFile(hidden, []byte("a backdrop behind a locked door"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("locking fixture directory: %v", err)
	}
	// Restore the mode before t.TempDir's own cleanup runs, or the removal of
	// the temp tree fails and leaves the fixture behind.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	// PRECONDITION. Probe rather than assume: an ACL or a capability can leave
	// the stat working, in which case the fall-through never runs and every
	// assertion below would pass for the wrong reason. A genuine
	// permission-environment skip.
	if _, statErr := os.Stat(hidden); statErr == nil {
		t.Skip("environment can stat inside a 0o000 directory (ACL or capabilities); the stat cannot be " +
			"made to fail")
	}

	paths := []string{hidden}
	p := boundTestPublisher()
	snapshot, warnings, err := p.snapshotFanart(context.Background(), paths)
	if err != nil {
		t.Fatalf("snapshotFanart returned %v; an unreadable file is a per-file degrade", err)
	}
	assertSnapshotShape(t, snapshot, paths)
	if snapshot[0].data != nil {
		t.Fatalf("an unreadable file yielded %d bytes", len(snapshot[0].data))
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want exactly 1: %v", len(warnings), warnings)
	}
	// The operator is told what is actually wrong.
	if !strings.Contains(warnings[0], readFailurePhrase) {
		t.Errorf("warning %q does not report the read failure; a file the process cannot open must say so, "+
			"not be reported as something about its size", warnings[0])
	}
	// And is NOT told a cap refused it. Every cap refusal carries the #3017
	// both-losses phrase, so its absence is the mechanical check that no budget
	// branch handled a stat that simply failed.
	if strings.Contains(warnings[0], bothLossesPhrase) {
		t.Errorf("warning %q reports a snapshot-cap refusal for a file whose stat FAILED; the operator "+
			"will go looking at backdrop sizes for what is a permission problem", warnings[0])
	}
	if strings.Contains(warnings[0], statRefusalPhrase) {
		t.Errorf("warning %q quotes a size taken from a stat that failed", warnings[0])
	}
}
