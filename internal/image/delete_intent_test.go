package image

import (
	"path/filepath"
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
