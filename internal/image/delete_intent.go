package image

import (
	"path/filepath"
	"sync"
	"time"
)

// DeleteIntentRetention is how long a delete marker stays consultable before it
// is treated as expired and pruned.
//
// The number is derived from the push window, not chosen for feel. Every handler
// that pushes artwork after a delete wraps that push in
// context.WithTimeout(..., 30*time.Second), so a push that started before the
// delete can still be verifying files up to 30 seconds later. Five minutes is
// ten times that, which leaves room for a push whose deadline was extended by a
// slow peer, for the repair's own detached verify read (which carries its own
// deadline and runs AFTER the push context is already done), and for coarse
// wall-clock behavior on a machine whose time steps.
//
// Retention is deliberately generous because it bounds MEMORY, not CORRECTNESS.
// The correctness test is DeleteIntentAfter's comparison against the caller's
// snapshot time: a marker older than the snapshot is ignored no matter how long
// it has been retained. So over-retaining costs a few map entries and can never
// suppress a repair that should have run. Under-retaining, by contrast, would
// silently reopen the bug this file exists to close.
const DeleteIntentRetention = 5 * time.Minute

// deleteIntent records that an OPERATOR deliberately deleted artwork of a given
// type in a given directory, keyed by "<cleaned dir>\x00<image type>" and valued
// by the wall-clock instant of the delete.
//
// WHY THIS EXISTS (#2712). The post-push repair in internal/publish
// (reassertLocalImage) restores the operator's local file when it finds the file
// missing after handing it to a platform peer. That repair is attribution-blind:
// it sees only "the bytes I captured before the push" and ENOENT afterwards, and
// nothing serializes an in-flight push against a concurrent operator action. So
// an operator who deletes a slot while a background push for that slot is in
// flight gets the artwork put straight back.
//
// Two guards were BUILT AND WITHDRAWN during #2701 before this one: mtime (which
// cannot separate "the peer rewrote it" from "the operator saved again", and
// whose newness gate disabled the crop-clobber repair of #2533) and the
// *_exists flag (derived from DISK, not from intent, so it is poisoned by the
// peer's own deletion in one direction and stale for a first-ever save in the
// other). Do not re-attempt either. The distinguishing property of this marker
// is that it is written by THE ACTOR WHOSE INTENT IT RECORDS -- the delete
// handler -- rather than inferred from filesystem state that both actors mutate.
//
// THE KEY IS (dir, imageType) WITH NO SLOT COMPONENT, AND THAT IS THE WHOLE
// POINT OF THE FILE. A per-slot key is the obvious shape and it fails exactly
// where it matters, because a fanart slot index is unstable by construction:
// RenumberFanart (fanart.go) COMPACTS survivors to contiguous 0-based indices,
// and every fanart slot delete renumbers (handlers_backdrop.go's slot delete and
// handlers_image.go's batch delete both call it). Work the case through:
//
//	The operator deletes slot 2 of 6. A per-slot marker records fanart slot 2.
//	The renumber then shifts 3->2, 4->3, 5->4, leaving five files. A push that
//	snapshotted the set BEFORE the renumber goes on to verify fanart5.jpg, reads
//	ENOENT, finds no marker for slot 5, and RESTORES it -- resurrecting deleted
//	artwork under a different filename and re-growing the set. That is #2712's
//	own bug reproduced by its fix.
//
// Non-fanart types (thumb, logo, banner) have exactly one slot, so the slot
// index never carried information there either. Dropping it loses nothing and
// closes the renumber hazard.
//
// THE TRADE THIS MAKES, stated plainly: a type-wide marker OVER-SUPPRESSES. A
// genuine peer clobber of a DIFFERENT fanart slot inside the same window is not
// repaired by that push, AND NOTHING WILL PUT THOSE BYTES BACK LATER. Be exact
// about that, because the obvious consolation is false: no component in this
// codebase restores a local artwork file from a platform peer.
// maintenance.ScanExistsFlags only clears exists_flag 1->0 for files it confirms
// have vanished, maintenance.RestoreExistsFlags is monotone 0->1 and only for
// files positively confirmed present on disk, and publish's artwork reconciler
// pushes LOCAL bytes OUT to platforms, never the reverse. Fanart has no
// .sw-backup either (img.HasBackup is single-slot only), so there is no local
// copy to revert from. What actually happens is that the exists-flag scan
// surfaces the slot as missing and the operator re-adds the artwork by hand.
//
// The direction of the trade is still right, on the asymmetry of the two
// outcomes rather than on any automatic recovery. A missed repair is a VISIBLE,
// MANUALLY RECOVERABLE loss: the slot reads as empty, and re-adding artwork is
// an ordinary operation the operator already knows how to perform. Resurrecting
// a deliberate delete is INVISIBLE and unrecoverable in any automatic sense:
// nothing will remove the file again, and the operator only learns of it by
// noticing the artwork is back and deleting it a second time. Given a choice
// between a loss the operator can see and fix and a silent undo of what they
// deliberately did, take the former.
//
// Entries are pruned opportunistically on write (see MarkDeleteIntent); nothing
// runs a goroutine to expire them. The same unbounded-sync.Map lifetime caveat
// that slotMu and repairOpMu carry applies here with the retention sweep as its
// mitigation: the live set is bounded by the (dir, type) pairs deleted within
// the retention window, which is a handful of entries even for a busy library.
var deleteIntent sync.Map // map[string]time.Time

// deleteIntentKey builds the marker key. The NUL separator cannot occur in a
// path segment or in an image type, so no (dir, type) pair can be confused for
// another by concatenation -- the same construction slotMutexForBase uses.
//
// dir is cleaned so that callers holding equivalent spellings of one directory
// ("/lib/artist" and "/lib/artist/") agree on the key. The writer is an API
// handler using Router.imageDir and the reader is the publisher using
// filepath.Dir of a discovered file path, so the two arrive by different routes
// and must be normalized to meet.
func deleteIntentKey(dir, imageType string) string {
	return filepath.Clean(dir) + "\x00" + imageType
}

// MarkDeleteIntent records that artwork of imageType in dir is being deleted
// RIGHT NOW by the operator.
//
// Call it IMMEDIATELY BEFORE touching the filesystem, never after. A push that
// is already in flight can perform its post-upload verify at any instant, and a
// marker written after the unlink leaves a window in which the file is gone and
// the intent is not yet visible -- which is the original bug, merely narrowed.
// Marking first is safe in the failure case too: if the delete then fails, the
// only consequence is that one push declines to repair a file nothing damaged.
//
// It also prunes markers older than DeleteIntentRetention. Doing the sweep here
// rather than in a background goroutine keeps the whole mechanism to one package
// variable with no lifecycle: deletes are the only thing that grows the map, so
// they are exactly the right place to shrink it, and a process that stops
// deleting stops needing the memory back.
func MarkDeleteIntent(dir, imageType string) {
	now := time.Now()
	deleteIntent.Store(deleteIntentKey(dir, imageType), now)

	cutoff := now.Add(-DeleteIntentRetention)
	deleteIntent.Range(func(k, v any) bool {
		if at, ok := v.(time.Time); ok && at.Before(cutoff) {
			// A racing Store for this key may have landed a fresher time since
			// the load. Compare-and-delete so the sweep can only ever remove the
			// exact stale value it read, never a newer marker that would then be
			// invisible to a push relying on it.
			deleteIntent.CompareAndDelete(k, v)
		}
		return true
	})
}

// DeleteIntentAfter reports whether an operator delete of imageType in dir was
// recorded at or after since -- that is, whether a delete landed within the
// caller's own window rather than at some earlier, unrelated time.
//
// since is the instant the caller captured the state it is about to act on (for
// the post-push repair, the wall-clock time the pre-push bytes were snapshotted).
// The comparison is inclusive: a marker whose timestamp equals since is treated
// as concurrent, because a delete recorded in the same clock tick as the
// snapshot cannot be ordered against it and the safe reading of an ambiguous
// order is "the operator meant it".
//
// Markers older than DeleteIntentRetention are ignored even if the sweep in
// MarkDeleteIntent has not yet reached them, so an idle process cannot leave a
// stale marker consultable. In practice the since comparison already excludes
// them; this is belt and braces against a caller passing a very old since.
func DeleteIntentAfter(dir, imageType string, since time.Time) bool {
	v, ok := deleteIntent.Load(deleteIntentKey(dir, imageType))
	if !ok {
		return false
	}
	at, ok := v.(time.Time)
	if !ok {
		return false
	}
	if time.Since(at) > DeleteIntentRetention {
		return false
	}
	return !at.Before(since)
}
