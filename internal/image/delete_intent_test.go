package image

import (
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// These tests live in package image on purpose. MarkDeleteIntent and
// DeleteIntentAfter are consumed by internal/publish, and testing them ONLY
// through that consumer would leave the contract guarded by an accident of who
// happens to call it: a change to the boundary semantics (the inclusive since
// comparison, the retention cut-off, the key shape) would surface as a failure
// in a different package, or not at all if that caller stopped exercising the
// edge. They are white-box where they need to be, reaching into the package
// deleteIntent map to plant a marker with a controlled timestamp, because the
// retention window is five minutes and no test should sleep for it.

// markAt plants a marker with an exact timestamp, which time.Now cannot do.
func markAt(dir, imageType string, at time.Time) {
	deleteIntent.Store(deleteIntentKey(dir, imageType), at)
}

func TestDeleteIntent_NoMarker_ReportsNothing(t *testing.T) {
	dir := t.TempDir()

	if DeleteIntentAfter(dir, "thumb", time.Now().Add(-time.Minute)) {
		t.Fatal("reported a delete for a directory nothing was ever deleted in")
	}
}

// TestDeleteIntent_MarkThenQuery is the ordinary case: the delete is recorded,
// and a push whose snapshot predates it sees it.
func TestDeleteIntent_MarkThenQuery(t *testing.T) {
	dir := t.TempDir()
	snapAt := time.Now().Add(-time.Second)

	MarkDeleteIntent(dir, "thumb")

	if !DeleteIntentAfter(dir, "thumb", snapAt) {
		t.Fatal("a delete recorded after the snapshot was not reported")
	}
}

// TestDeleteIntent_OlderThanSince_NotReported is the guard against
// over-suppression: a delete that happened BEFORE the push looked at the file
// says nothing about what happened during the push, so the repair must still run.
func TestDeleteIntent_OlderThanSince_NotReported(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	markAt(dir, "thumb", now.Add(-30*time.Second))

	if DeleteIntentAfter(dir, "thumb", now.Add(-10*time.Second)) {
		t.Fatal("a delete that predates the snapshot was reported as concurrent with it")
	}
}

// TestDeleteIntent_SinceBoundaryIsInclusive pins the comparison at exactly
// equal times. A marker landing in the same clock tick as the snapshot cannot be
// ordered against it, and the safe reading of an ambiguous order is that the
// operator meant it -- so equal must report TRUE, not false.
func TestDeleteIntent_SinceBoundaryIsInclusive(t *testing.T) {
	dir := t.TempDir()
	at := time.Now()
	markAt(dir, "thumb", at)

	if !DeleteIntentAfter(dir, "thumb", at) {
		t.Fatal("a delete recorded at exactly the snapshot instant was not reported; the boundary must be inclusive")
	}
	// One nanosecond later and the marker is genuinely older than the snapshot.
	if DeleteIntentAfter(dir, "thumb", at.Add(time.Nanosecond)) {
		t.Fatal("a delete one nanosecond before the snapshot was reported as concurrent")
	}
}

// TestDeleteIntent_ExpiredMarkerIgnored proves retention is enforced on READ,
// not only by the write-time sweep. A process that deletes once and then goes
// quiet never runs the sweep again, so a stale marker would otherwise stay
// consultable for the life of the process.
func TestDeleteIntent_ExpiredMarkerIgnored(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-DeleteIntentRetention - time.Minute)
	markAt(dir, "thumb", old)

	// since is older still, so the ONLY thing that can suppress the report is
	// the retention cut-off. Without asserting that, this test would pass for
	// the wrong reason.
	if DeleteIntentAfter(dir, "thumb", old.Add(-time.Hour)) {
		t.Fatal("a marker older than the retention window was still reported")
	}
}

// TestDeleteIntent_MarkPrunesExpiredEntries proves the write-time sweep actually
// removes storage, which the read-side cut-off alone would hide: an unpruned map
// still answers correctly and grows forever.
func TestDeleteIntent_MarkPrunesExpiredEntries(t *testing.T) {
	stale := t.TempDir()
	fresh := t.TempDir()

	staleKey := deleteIntentKey(stale, "banner")
	markAt(stale, "banner", time.Now().Add(-DeleteIntentRetention-time.Minute))
	if _, ok := deleteIntent.Load(staleKey); !ok {
		t.Fatal("precondition failed: the stale marker was not stored")
	}

	// Any Mark runs the sweep, including one for an unrelated key.
	MarkDeleteIntent(fresh, "logo")

	if _, ok := deleteIntent.Load(staleKey); ok {
		t.Error("the expired marker survived a MarkDeleteIntent sweep")
	}
	if _, ok := deleteIntent.Load(deleteIntentKey(fresh, "logo")); !ok {
		t.Error("the sweep removed the marker that was just written")
	}
}

// TestDeleteIntent_MarkDoesNotPruneLiveEntries is the mutation guard on the
// sweep: a sweep that deleted everything would pass the test above.
func TestDeleteIntent_MarkDoesNotPruneLiveEntries(t *testing.T) {
	live := t.TempDir()
	other := t.TempDir()

	MarkDeleteIntent(live, "thumb")
	MarkDeleteIntent(other, "fanart")

	if _, ok := deleteIntent.Load(deleteIntentKey(live, "thumb")); !ok {
		t.Error("a marker inside the retention window was pruned by a later sweep")
	}
}

// TestDeleteIntent_KeyedByTypeAndDir proves the two key components both
// discriminate. Without this, a key that ignored imageType (or ignored dir)
// would pass every other test in this file.
func TestDeleteIntent_KeyedByTypeAndDir(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	snapAt := time.Now().Add(-time.Second)

	MarkDeleteIntent(dirA, "thumb")

	if DeleteIntentAfter(dirA, "logo", snapAt) {
		t.Error("a thumb delete was reported for the logo type in the same directory")
	}
	if DeleteIntentAfter(dirB, "thumb", snapAt) {
		t.Error("a delete in one directory was reported for a different directory")
	}
}

// TestDeleteIntent_KeyIsPathNormalized covers the reason the key is cleaned: the
// writer is an API handler using Router.imageDir and the reader is the publisher
// using filepath.Dir of a discovered file path. Those arrive by different routes
// and must agree on one key.
func TestDeleteIntent_KeyIsPathNormalized(t *testing.T) {
	dir := t.TempDir()
	snapAt := time.Now().Add(-time.Second)

	MarkDeleteIntent(dir+string(filepath.Separator), "fanart")

	if !DeleteIntentAfter(dir, "fanart", snapAt) {
		t.Fatal("a trailing separator on the write side produced a key the read side could not find")
	}
}

// TestDeleteIntent_HasNoSlotComponent is the shape assertion that the whole
// design rests on: a delete of ANY fanart slot must be visible when the repair
// asks about the fanart TYPE, because RenumberFanart shifts survivors and the
// slot index the repair holds is from a pre-renumber snapshot. There is no
// per-slot API to test against; the test is that the exported surface takes no
// slot at all and one mark answers for the type.
func TestDeleteIntent_HasNoSlotComponent(t *testing.T) {
	dir := t.TempDir()
	snapAt := time.Now().Add(-time.Second)

	// The operator deleted "slot 1". Nothing in the call records which one.
	MarkDeleteIntent(dir, "fanart")

	// The repair asks about the file it snapshotted as slot 2, which the
	// renumber has since moved. It gets a positive answer anyway.
	if !DeleteIntentAfter(dir, "fanart", snapAt) {
		t.Fatal("a fanart delete was not visible type-wide; a slot-scoped key would let the repair resurrect a renumbered backdrop")
	}
}

// TestDeleteIntent_ConcurrentMarkAndQuery is the race-detector test for the
// package-level map, and the one that pins MarkDeleteIntent's CompareAndDelete.
//
// WHY IT IS NEEDED. deleteIntent is shared mutable state with two distinct
// classes of writer and reader in production: API delete handlers call
// MarkDeleteIntent while publish goroutines call DeleteIntentAfter, and nothing
// serializes them. Every other test in this file is single-goroutine, so the
// race detector never sees this file's concurrency at all and the repo's own
// concurrency guideline is unmet.
//
// WHAT IT ACTUALLY EXERCISES, which is the part that took thought. The sweep in
// MarkDeleteIntent only touches entries older than DeleteIntentRetention, so a
// test whose keys are all fresh walks the Range and prunes NOTHING -- it would
// run clean against any sweep, including a deleted one, and pin nothing. So the
// fixture PLANTS A STALE MARKER ON EVERY KEY THE WRITERS USE before the
// goroutines start. Each writer then overwrites its own key with a fresh time
// while other writers' sweeps are walking that same key and finding the stale
// value they read a moment ago.
//
// That is exactly the interleaving CompareAndDelete exists for:
//
//	Sweeper G1 Ranges and reads (k, stale).
//	Writer G2 Stores a FRESH time at k and, being the delete handler, now
//	  depends on that marker being visible to an in-flight push.
//	G1 removes k.
//
// With a bare Delete, G1's removal takes G2's fresh marker with it, the push
// consults a key that is no longer there, and the operator's delete is
// resurrected -- #2712 reopened, intermittently and only under load.
// CompareAndDelete makes G1's removal conditional on the value it actually
// read, so a fresher Store simply wins.
//
// The assertion is therefore each writer re-reading ITS OWN key immediately
// after writing it. Nothing else in the map may make that read fail: the sweep
// is the only thing that removes entries, and this key's value is fresh.
func TestDeleteIntent_ConcurrentMarkAndQuery(t *testing.T) {
	const (
		writers  = 8
		readers  = 4
		rounds   = 200
		imgType  = "fanart"
		staleAge = DeleteIntentRetention + time.Minute
	)

	base := t.TempDir()
	dirs := make([]string, writers)
	for i := range dirs {
		dirs[i] = filepath.Join(base, "artist"+strconv.Itoa(i))
	}

	// Plant a stale marker on every key, so the sweep has real pruning work to
	// do on exactly the keys the writers are racing to refresh.
	stale := time.Now().Add(-staleAge)
	for _, dir := range dirs {
		markAt(dir, imgType, stale)
	}
	// PRECONDITION: without live stale entries the Range below never enters its
	// pruning branch and this test degenerates into a plain concurrency smoke
	// test that pins nothing about CompareAndDelete.
	for _, dir := range dirs {
		v, ok := deleteIntent.Load(deleteIntentKey(dir, imgType))
		if !ok {
			t.Fatalf("precondition failed: no planted marker for %s", dir)
		}
		at, ok := v.(time.Time)
		if !ok || !at.Before(time.Now().Add(-DeleteIntentRetention)) {
			t.Fatalf("precondition failed: the planted marker for %s is not older than the retention "+
				"window, so no sweep will ever try to prune it", dir)
		}
	}

	var wg sync.WaitGroup
	// vanished collects keys whose fresh marker disappeared. Collected rather
	// than t.Fatal'd from the goroutine, since Fatal from a non-test goroutine
	// is undefined.
	var mu sync.Mutex
	var vanished []string

	for w := range writers {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			for range rounds {
				// RE-PLANT THE STALE VALUE EVERY ROUND, and this is what makes
				// the test a reliable detector rather than an occasional one.
				// After the first MarkDeleteIntent a key holds a FRESH time, and
				// every later sweep skips it as not-yet-expired -- so with a
				// one-time plant the vulnerable interleaving is only reachable
				// during the opening rounds, and a bare-Delete sweep survives
				// the run more often than not. Re-planting keeps this key
				// eligible for pruning at the instant its owner overwrites it,
				// so the "sweeper read stale, writer stored fresh" window is
				// open on every iteration of every writer.
				markAt(dir, imgType, stale)

				before := time.Now()
				MarkDeleteIntent(dir, imgType)
				if !DeleteIntentAfter(dir, imgType, before) {
					mu.Lock()
					vanished = append(vanished, dir)
					mu.Unlock()
					return
				}
			}
		}(dirs[w])
	}

	// Readers add no assertion of their own -- their job is to put concurrent
	// Loads against the Stores and CompareAndDeletes for the race detector.
	for r := range readers {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			for range rounds {
				_ = DeleteIntentAfter(dir, imgType, time.Time{})
			}
		}(dirs[r%writers])
	}

	wg.Wait()

	if len(vanished) > 0 {
		t.Fatalf("a marker written by MarkDeleteIntent was gone by the time its own caller read it back, "+
			"for %d of %d writers (first: %s). A concurrent sweep removed a value it had not read: the "+
			"prune must be a CompareAndDelete against the exact stale value, never a bare Delete, or a "+
			"delete handler's intent can be swept away between its Store and an in-flight push's Load",
			len(vanished), writers, vanished[0])
	}

	// The stale plants must actually be GONE, which proves the sweep ran and
	// pruned rather than merely being walked. A CompareAndDelete that never
	// matched would leave them all in place.
	for _, dir := range dirs {
		v, ok := deleteIntent.Load(deleteIntentKey(dir, imgType))
		if !ok {
			t.Errorf("%s has no marker at all after the run; the last write for it went missing", dir)
			continue
		}
		at, ok := v.(time.Time)
		if !ok {
			t.Errorf("%s holds a %T rather than a time.Time", dir, v)
			continue
		}
		if at.Equal(stale) {
			t.Errorf("%s still holds the planted stale marker, so no sweep ever pruned it and this test "+
				"never exercised the CompareAndDelete branch", dir)
		}
	}
}
