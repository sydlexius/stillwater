//go:build unix

package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
)

// The stalled-read CAP arm of snapshotFanart, as distinct from the CANCELLATION
// arm its siblings in sync_stall_message_unix_test.go pin.
//
// Both abort the loop through the same predicate, and that shared route is
// exactly why the cap arm needs its own test: a version that aborted correctly
// but reported "the request ended" would pass every existing test in this
// package, because none of them can produce a cap refusal. That is how the
// wrong message survived a round of review in the first place.
//
// WHY THIS CALLS snapshotFanart DIRECTLY rather than going through
// SyncAllFanartToPlatforms. The cap is checked by the SAME primitive that backs
// DiscoverFanart's directory read, so saturating it refuses DISCOVERY first and
// the sync returns "failed to read fanart directory" having never reached a
// single file read. Measured, not assumed: with the gauge at the cap,
// DiscoverFanart returns `reading directory ...: too many stalled filesystem
// reads are still in flight`.
//
// That is not a gap in the fix -- it is the shape of the real condition. In
// production the cap is crossed by a CONCURRENT operation while this loop is
// already running: discovery completed under the cap, and by the time file N is
// reached another request has abandoned the read that tips the gauge over.
// snapshotFanart takes its paths as a parameter precisely because discovery is
// the caller's job, so calling it directly reproduces that state exactly rather
// than approximating it -- the function receives the same arguments the sync
// hands it, with the gauge in the state a concurrent operation would leave.
//
// Driving this through the public entry point would need a rendezvous between
// discovery and the read with no seam to hang it on, and the only way to build
// one is a production hook that exists solely for the test. The function under
// test is unexported and in-package; using that is the honest option.

// saturateStalledReadCap drives the process-wide stalled-read gauge to its cap
// and waits for it to drain again at test end.
//
// The wedge is a FIFO with no writer: the read blocks in the kernel until a
// writer appears, so a caller that gives up abandons it and the gauge goes up
// by one. That is the only way to raise the gauge -- it counts abandoned reads,
// and there is no setter.
//
// The DRAIN is not politeness. The gauge is process-wide and this is the one
// helper that deliberately pins it at the refusal threshold, so leaving it
// elevated makes every LATER test in the package start against a primitive that
// refuses reads -- an order-dependent failure that `-shuffle` would surface
// somewhere else entirely, as a mystery. Cleanup opens each write end so the
// abandoned readers complete and decrement on their way out, then blocks until
// the gauge is back where it started.
//
// Callers must NOT t.Parallel(): the gauge is global, and a parallel sibling
// running against a saturated cap would see reads refused for reasons that have
// nothing to do with it.
func saturateStalledReadCap(t *testing.T) {
	t.Helper()
	// One more than the cap, so the gauge is at or above the refusal threshold
	// even though the check is deliberately approximate (the Load and the Add
	// are separate operations, so the true count can briefly exceed the nominal
	// cap). Overshooting by one costs a few milliseconds and removes the only
	// way this helper could return without the cap actually being in force.
	wedgeStalledReads(t, maxStalledReadsForTest+1)

	// PRECONDITION. Without this the test below could pass for the wrong
	// reason: an ordinary read failure also aborts the loop, so a helper that
	// silently failed to saturate would leave the assertions checking a
	// different branch's behavior while still going green.
	probe := filepath.Join(t.TempDir(), "probe.jpg")
	if err := os.WriteFile(probe, []byte("readable"), 0o600); err != nil {
		t.Fatalf("writing cap probe: %v", err)
	}
	if _, err := img.ReadImageFileBounded(context.Background(), probe); !errors.Is(err, img.ErrTooManyStalledReads) {
		t.Fatalf("precondition: a read of a perfectly readable file returned %v, want %v; "+
			"the cap is not saturated and the test below would exercise the wrong branch",
			err, img.ErrTooManyStalledReads)
	}
}

// maxStalledReadsForTest mirrors the unexported cap in internal/image.
//
// Duplicated rather than exported: the cap is an internal sizing decision, and
// widening the package's API so a test in ANOTHER package can read it would
// make a private tuning constant part of the contract. The mirror is safe
// because nothing here depends on the exact value -- the tests below assert on
// the OBSERVED refusal (saturateStalledReadCap's probe, and
// wedgeBelowStalledReadCap's own precondition), so a change to the real cap
// makes those preconditions fail loudly rather than letting a test pass
// vacuously against a stale number.
const maxStalledReadsForTest = 16

// wedgeStalledReads raises the process-wide stalled-read gauge by exactly n and
// registers the teardown that brings it back down.
//
// MUST be called from the TEST GOROUTINE, never from a spawned one (#2976
// review). It calls t.Skipf when mkfifo is unavailable, and Skipf runs
// runtime.Goexit -- which exits only the goroutine that called it. From a
// spawned goroutine that silently kills the helper mid-way, leaving the gauge
// un-raised while the test proceeds believing it was raised; a deferred
// close(done) then hands the test a green light for a cap that never engaged.
// Vacuous rather than failing, which is worse. prepareWedge below exists so a
// rendezvous goroutine can raise the gauge without touching testing.T at all.
func wedgeStalledReads(t *testing.T, n int) {
	t.Helper()
	before := img.StalledReadCount()
	dir := t.TempDir()

	// Registered BEFORE the wedges, so it runs AFTER them: t.Cleanup is LIFO,
	// and a drain wait registered last would run FIRST -- before a single write
	// end had been opened -- and then block until its own deadline while the
	// gauge sat pinned, with nothing able to release it. Found the hard way;
	// the guard reported a stuck gauge that the ordering, not the code, caused.
	t.Cleanup(func() {
		deadline := time.Now().Add(30 * time.Second)
		for img.StalledReadCount() > before {
			if time.Now().After(deadline) {
				t.Errorf("StalledReadCount() = %d, want it back down to %d after every wedge was released; "+
					"a gauge left elevated makes later tests in this package fail against a saturated cap",
					img.StalledReadCount(), before)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	for i := range n {
		p := filepath.Join(dir, fmt.Sprintf("wedge%d.jpg", i))
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Skipf("mkfifo unavailable on this platform: %v", err)
		}
		// THIS wedge's own baseline, not the helper's. Passing the helper-wide
		// `before` makes each per-wedge cleanup wait for EVERY OTHER wedge's
		// reader too -- readers it cannot release, since they are blocked on
		// different FIFOs owned by other cleanups. Each release then spins its
		// full timeout before giving up, serially, and n wedges turn a 9s
		// package into a 10-minute one. A cleanup must wait only on what it
		// can itself release; the helper-wide drain above owns the aggregate.
		wedgeBefore := img.StalledReadCount()
		t.Cleanup(func() { releaseFifoReaders(t, p, wedgeBefore) })
		// A short deadline per wedge: the read cannot succeed, and the only
		// thing being waited for is the caller giving up on it.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, _ = img.ReadImageFileBounded(ctx, p)
		cancel()
	}
}

// releaseFifoReaders opens the write end of a FIFO, repeatedly, until the
// process-wide stalled-read gauge is back to its pre-wedge reading, so every
// abandoned reader blocked on it gets EOF and gives its slot back.
//
// This mirrors releaseFifoReader in internal/image (unexported there, so it
// cannot be shared) and exists for the same two reasons, both of which fail
// only UNDER LOAD -- standalone runs pass either way (#2976 review):
//
//   - a non-blocking write-open returns ENXIO when no reader is attached AT
//     THAT INSTANT, and the reader is an abandoned goroutine whose caller has
//     already returned, so nothing orders it against this cleanup. The single
//     shot this replaces was `if err == nil { close }`: a capability check
//     whose failing branch did nothing, silently skipping the release;
//   - one open frees only the readers already blocked INSIDE open(2). One
//     scheduled later arrives afterwards and blocks again on a FIFO that no
//     longer has a writer, so a helper that abandons a reader per iteration
//     strands several.
//
// A blocking open is not the alternative: with the readers already gone it
// would wait for one that never comes and hang until the binary timed out.
//
// Exhausting the budget is not reported here. The authoritative assertion is
// the drain loop in wedgeStalledReads, which watches the gauge itself and can
// tell a genuinely stuck reader from a FIFO whose readers were already
// released -- this only has a per-path view and would cry wolf on the latter.
func releaseFifoReaders(t *testing.T, path string, before int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for img.StalledReadCount() > before && time.Now().Before(deadline) {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = f.Close()
		} else if !errors.Is(err, syscall.ENXIO) {
			// ENXIO just means "no reader attached yet" -- expected and
			// retryable. Anything else is a real failure worth surfacing
			// rather than burning the budget on.
			t.Errorf("opening the FIFO write end at %s to release a stalled read: %v", path, err)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// prepareWedge plants a FIFO and returns a function that abandons one read
// against it, raising the gauge by one.
//
// The split exists so a RENDEZVOUS GOROUTINE can raise the gauge without
// touching testing.T (#2976 review): everything that can call t.Skipf happens
// here, on the test goroutine, and the returned closure touches only the
// filesystem and the gauge. Passing t into a goroutine instead risks a Skipf
// that exits only that goroutine, leaving the gauge un-raised while the test
// carries on believing otherwise.
func prepareWedge(t *testing.T, name string) func() {
	t.Helper()
	before := img.StalledReadCount()
	p := filepath.Join(t.TempDir(), name)
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}
	t.Cleanup(func() {
		releaseFifoReaders(t, p, before)
		deadline := time.Now().Add(30 * time.Second)
		for img.StalledReadCount() > before {
			if time.Now().After(deadline) {
				t.Errorf("StalledReadCount() = %d, want it back down to %d after the wedge was released",
					img.StalledReadCount(), before)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, _ = img.ReadImageFileBounded(ctx, p)
		cancel()
	}
}

// TestSnapshotFanart_StalledCap_AbortsAndNamesTheMount is the cap arm.
//
// Two properties, and the second is the one this PR added:
//
//   - it ABORTS rather than skipping the file. The comment on this branch spells
//     out why that matters more here than anywhere else the predicate is used:
//     these bytes are the only copy that can undo a peer's delete, and the
//     restore loop skips any entry whose data is nil. Treating a cap refusal as
//     a per-file skip kept nil-data entries, let the push proceed, and left a
//     peer-deleted file with nothing to restore from.
//   - it names the MOUNT, not the request. Both causes abort; only one of them
//     is the operator's own client walking away.
func TestSnapshotFanart_StalledCap_AbortsAndNamesTheMount(t *testing.T) {
	// No t.Parallel: saturateStalledReadCap pins a process-wide gauge.
	dir := t.TempDir()
	// Two readable files. Readable is the point -- there is nothing wrong with
	// either of them, so anything the loop reports is about the mount and not
	// about their contents. Two of them so "aborted" is distinguishable from
	// "read everything it was given".
	paths := make([]string, 0, 2)
	for _, name := range []string{"fanart.jpg", "fanart1.jpg"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("fake-image"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	saturateStalledReadCap(t)

	p := syncTestPublisher()
	// A LIVE context, never canceled and with no deadline. This is the whole
	// distinction under test: the caller is still waiting, so an abort reported
	// as "the request ended" would be a plain falsehood. It also rules out the
	// cancellation arm reaching these assertions by accident.
	ctx := context.Background()

	snapshot, warnings, err := p.snapshotFanart(ctx, paths)

	if err == nil {
		t.Fatal("snapshotFanart returned no error with the read cap saturated; " +
			"a per-file skip here lets the push proceed with nothing to restore from")
	}
	if !errors.Is(err, img.ErrTooManyStalledReads) {
		t.Errorf("error = %v, want it to wrap %v; the caller branches on this sentinel to pick its message",
			err, img.ErrTooManyStalledReads)
	}
	// The context was never canceled, so an abort attributed to the context
	// would be reporting a cause that did not occur.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v is a context error, but ctx was never canceled", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("precondition: ctx reports %v; it must stay live for this to test the cap arm", ctx.Err())
	}
	// Aborted at the FIRST file: nothing was captured, and the second file was
	// never attempted. A loop that kept going would have two entries here.
	if len(snapshot) != 0 {
		t.Errorf("snapshot has %d entries, want 0; the loop continued past a failure it must not trust",
			len(snapshot))
	}
	// The per-file warning belongs to the per-file branch. A cap refusal takes
	// the abort branch, which returns the cause rather than warning about it --
	// so a warning here means the wrong branch ran.
	joined := strings.Join(warnings, " | ")
	if strings.Contains(joined, "could not be read and was skipped") {
		t.Errorf("warnings %q report a per-file skip; a cap refusal must abort, not skip", joined)
	}
}

// TestSnapshotFanart_OrdinaryReadFailure_SkipsAndContinues is the GREEN sibling,
// and it is what keeps the fix from over-propagating.
//
// The abort branch must fire for the cap and for a cancellation, and for
// NOTHING ELSE. A vanished file, a permissions error, an over-size file: each
// says something about ONE file and nothing about the rest, so the loop skips it
// with a warning and carries on. Without this test, a fix that aborted on every
// read error would pass the test above and silently turn one bad file into a
// refused push for the whole set.
func TestSnapshotFanart_OrdinaryReadFailure_SkipsAndContinues(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// A directory at a fanart filename: it exists, so this is a genuine read
	// failure (EISDIR) rather than a missing file, and it is emphatically not a
	// stalled mount.
	bad := filepath.Join(dir, "fanart.jpg")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "fanart1.jpg")
	if err := os.WriteFile(good, []byte("fake-image"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := syncTestPublisher()
	snapshot, warnings, err := p.snapshotFanart(context.Background(), []string{bad, good})

	if err != nil {
		t.Fatalf("snapshotFanart returned %v; an ordinary per-file failure must not abort the whole set", err)
	}
	if len(warnings) == 0 {
		t.Error("an unreadable file must produce a warning; silence would hide it from the operator")
	}
	// The file AFTER the failure was still read. This is the assertion that
	// fails if the abort branch ever widens to cover ordinary errors.
	var captured int
	for _, s := range snapshot {
		if s.data != nil {
			captured++
		}
	}
	if captured != 1 {
		t.Errorf("captured %d files with data, want 1; the loop stopped at a failure it should have skipped",
			captured)
	}
	// The mount is fine here, and saying otherwise sends the operator to debug
	// their NAS over one bad file.
	if strings.Contains(strings.Join(warnings, " | "), "mount is not responding") {
		t.Errorf("warnings %q blame the mount for an ordinary read failure", strings.Join(warnings, " | "))
	}
}

// TestSyncAllFanart_StalledCap_WarningNamesTheMount is the CALLER arm: the
// message syncAllFanartToPlatforms itself produces when snapshotFanart aborts
// on a cap refusal.
//
// Its sibling in sync_stall_message_unix_test.go pins the CANCELLATION wording
// on this same branch. This one exists because fixing the misattribution in the
// callee and leaving the caller asserting "the request ended" would reproduce
// the wrong cause one level up -- and THIS is the string that reaches the
// operator, since it is what lands in the returned warnings.
//
// THE HARD PART IS DETERMINISM, and the first attempt at this test got it
// wrong. The cap is enforced by the same primitive that backs the DIRECTORY
// read, so a gauge already at the cap refuses DISCOVERY and the sync returns
// "failed to read fanart directory" having never opened a file. Raising the
// gauge partway and relying on a deadline to tip it over instead produced a
// RACE between the two abort causes, and the cancellation arm won -- a flaky
// test that would have passed CI about as often as not.
//
// So the gauge is crossed at an exact, observable moment instead. The primary
// fanart file is a FIFO, and opening its write end BLOCKS until the sync's own
// reader opens the read end. That open is the rendezvous: at that instant the
// snapshot loop is provably inside its first read, and the goroutine below can
// tip the gauge to the cap knowing the SECOND read has not started. It then
// feeds the FIFO so read one SUCCEEDS, leaving read two to be refused outright
// by a cap that is now in force -- with the context alive throughout, so the
// cancellation arm cannot fire at all.
//
// That is also the true production shape: a mount that stops answering PARTWAY
// through a set, with the operator still waiting.
func TestSyncAllFanart_StalledCap_WarningNamesTheMount(t *testing.T) {
	// No t.Parallel: the gauge is process-wide.
	dir := t.TempDir()

	// File one: the FIFO, at the PRIMARY fanart name so it is the first file
	// the snapshot loop reads. assertOnFanartReadPath pins that below rather
	// than trusting it.
	fifo := filepath.Join(dir, fanartPrimaryFixtureName)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}
	// File two: perfectly readable, and the one whose read must be REFUSED.
	// Readable is the point -- there is nothing wrong with it, so an abort
	// blamed on its contents would be a plain falsehood.
	if err := os.WriteFile(filepath.Join(dir, "fanart1.jpg"), []byte("fake-image"), 0o600); err != nil {
		t.Fatal(err)
	}

	// One below the cap: discovery and the first read still succeed.
	wedgeStalledReads(t, maxStalledReadsForTest-1)

	// PRECONDITION: still UNDER the cap. Without this the test would silently
	// degrade into the discovery-refused case and assert nothing about the
	// branch it names.
	//
	// READ THE GAUGE; DO NOT PERFORM A READ TO TEST IT (#3016). The obvious
	// version of this check -- ReadImageFileBounded on a probe file -- is
	// SELF-POISONING: that read occupies a stalled-read slot for its own
	// duration, so with the gauge deliberately at cap-1 the probe is the read
	// that reaches the cap. Standalone it passes, because cap-1 plus one is
	// still admitted. Under -shuffle=on it fails roughly 1 run in 5: a
	// neighboring test's not-yet-drained reader has already contributed, the
	// probe tips over, and the failure blames the fixture for a saturated cap
	// the fixture did not saturate. Measured on this branch before the fix.
	//
	// StalledReadCount() answers the same question by observation and consumes
	// nothing, which is what makes it safe to ask here.
	if got := img.StalledReadCount(); got >= maxStalledReadsForTest {
		t.Fatalf("precondition: the stalled-read gauge is at %d, at or over the %d cap, so DISCOVERY "+
			"would be refused and this would exercise the wrong branch. The wedges raised it to %d; "+
			"the surplus is a neighboring test's reader that has not drained yet",
			got, maxStalledReadsForTest, maxStalledReadsForTest-1)
	}

	assertOnFanartReadPath(t, dir, fanartPrimaryFixtureName)

	// Built HERE, on the test goroutine, and only INVOKED below in the
	// rendezvous goroutine. Everything that can call t.Skipf (mkfifo probing,
	// cleanup registration) therefore stays on the test goroutine; the closure
	// itself touches only the filesystem and the gauge. Handing t to a
	// goroutine instead risks a Skipf that exits just that goroutine, leaving
	// the gauge un-raised while the test proceeds as though it had been raised
	// -- a vacuous pass rather than a failure (#2976 review).
	tipGauge := prepareWedge(t, "tipping-wedge.jpg")

	// The rendezvous goroutine. Everything it does is ordered by the blocking
	// open, so no sleep and no polling is involved.
	tipped := make(chan struct{})
	go func() {
		defer close(tipped)
		// BLOCKS until the snapshot loop opens the read end.
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer func() { _ = w.Close() }()
		// The loop is now provably inside read one, so read two has not begun.
		// Tip the gauge to the cap with one more abandoned read.
		tipGauge()
		// Let read one COMPLETE. A successful first read is what proves the
		// abort that follows belongs to the cap and not to this file.
		_, _ = w.Write([]byte("fake-image"))
	}()

	p := syncTestPublisher()
	// A LIVE context: never canceled, no deadline. This is the whole
	// distinction under test, and it rules the cancellation arm out entirely.
	ctx := context.Background()

	done := make(chan []string, 1)
	go func() {
		done <- p.SyncAllFanartToPlatforms(ctx, &artist.Artist{ID: "a1", Path: dir, Name: "Dest"})
	}()

	var warnings []string
	select {
	case warnings = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("SyncAllFanartToPlatforms did not return within 30s; with a live context the cap refusal " +
			"is the only thing that can end this sync, so a hang means the abort never fired")
	}
	<-tipped

	if ctx.Err() != nil {
		t.Fatalf("precondition: ctx reports %v; it must stay live for this to be the cap arm", ctx.Err())
	}
	if len(warnings) == 0 {
		t.Fatal("a sync that captured nothing must warn; silence would tell the operator the push succeeded")
	}
	joined := strings.Join(warnings, " | ")

	// THE REGRESSION. The context was never canceled, so reporting this abort
	// as "the request ended" sends the operator to look at their browser when
	// their mount is what stopped answering.
	if !strings.Contains(joined, "mount is not responding") {
		t.Errorf("warnings %q do not name the MOUNT; the context was never canceled, so this abort is the cap's",
			joined)
	}
	if strings.Contains(joined, "the request ended") {
		t.Errorf("warnings %q blame the request, which never ended; ctx.Err() = %v", joined, ctx.Err())
	}
	if !strings.Contains(joined, "could not be read") {
		t.Errorf("warnings %q do not tell the operator the fanart could not be READ", joined)
	}
	// Nothing reached a peer, and claiming otherwise is the worst available
	// outcome: the operator believes the push happened.
	if strings.Contains(joined, "uploaded") {
		t.Errorf("warnings %q mention an upload, but the peer was never reached", joined)
	}
}
