// Package publish -- phash_platform.go
//
// Platform-side half of the cross-artist backdrop back-out (#2564 PR-4). The
// local rule engine (rule.Pipeline.RemediatePHashMismatches) quarantines and
// removes a polluted backdrop from disk; platform sync is additive, so the copy
// already pushed to Emby/Jellyfin persists and keeps being served as this
// artist's backdrop. This file removes it from the platform and, on a restore,
// puts it back.
//
// # Why this is content-addressed and index-free
//
// The manifest records the slot ordinal the image occupied at removal time, but
// that is PROVENANCE, never an address (see image.RepairEntry.SlotIndex and
// RepairPlatformTarget). The platform re-indexes its backdrops after every
// delete, so a stored ordinal is stale by construction: by the time anything
// runs, index N denotes a different picture or nothing at all. Both the delete
// and the restore therefore re-resolve the target by CONTENT every call.
//
// The match is PERCEPTUAL, at the removal's own tolerance, not byte equality.
// Emby and Jellyfin re-encode an uploaded image, so the bytes read back never
// equal the bytes written -- a stored byte hash would match nothing. That the
// signal is fuzzy is exactly why the destructive direction fails closed: only
// slots this pass phash-matched are ever deleted, a non-matching slot is never
// touched, and a delete is not called successful on the platform's 2xx (both
// peers silently ignore some writes) but only after the item is re-read and the
// matching backdrop is confirmed GONE.
package publish

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"sync"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/image"
)

// phashPlatformClient is what a platform must provide to back a polluted
// backdrop out and restore it: read the backdrops (to re-resolve by content),
// delete one at an index (to remove the pollution), and append one (to restore).
type phashPlatformClient interface {
	connection.BackdropReader      // GetArtistDetail, GetArtistBackdrop
	connection.IndexedImageDeleter // DeleteImageAtIndex
	connection.ImageUploader       // UploadImage (appends a backdrop; peer assigns the ordinal)
}

// newPhashPlatformClient builds a client for the connection type. Mirrors
// newBackdropPruneClient (both Emby and Jellyfin share the mediabrowser image
// API) but additionally requires the uploader for the restore direction.
// Returns nil for unsupported types.
func newPhashPlatformClient(conn *connection.Connection, logger *slog.Logger) phashPlatformClient {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	default:
		return nil
	}
}

// phashPlatformClientFactory is a package-level seam so tests can substitute a
// fake platform client without widening any exported surface. Production always
// calls through to newPhashPlatformClient; tests reassign this (with a
// t.Cleanup restore). Mirrors backdropPruneClientFactory.
var phashPlatformClientFactory = newPhashPlatformClient

// validPHashTolerance rejects a tolerance that cannot be a meaningful cutoff.
//
// This is the single choke point both directions pass through, and it fails
// CLOSED for one specific catastrophe: math.IsNaN is not belt-and-braces
// because every IEEE-754 comparison against NaN is false, so `t <= 0 || t > 1`
// ADMITS NaN, and a NaN tolerance makes `Similarity >= tolerance` false for
// every slot -- which on the delete path would silently match NOTHING and
// report a clean run over an un-remediated library, and worse, any future
// inversion of the comparison would match EVERYTHING and authorize deleting
// every backdrop. Rejecting an unusable tolerance here means neither can happen.
func validPHashTolerance(t float64) error {
	if math.IsNaN(t) || t <= 0 || t > 1 {
		return fmt.Errorf("tolerance must be within (0, 1], got %v", t)
	}
	return nil
}

// lockPhashTarget acquires the per-target mutation guard for one platform item
// and returns its unlock. The key is ConnectionID+PlatformArtistID, so distinct
// targets never contend, but two operations on the SAME target serialize their
// entire read-modify-verify. Callers must hold the lock across the complete
// resolve->mutate->verify (delete or restore) and release only after it
// finishes, so a concurrent duplicate observes the settled artifact rather than
// racing a half-applied one.
func (p *Publisher) lockPhashTarget(connectionID, platformArtistID string) func() {
	key := connectionID + "\x00" + platformArtistID
	m, _ := p.phashTargetLocks.LoadOrStore(key, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// matchingBackdropIndices reads every backdrop for the item, perceptually
// hashes it, and returns the indices within tolerance of want, sorted
// DESCENDING so a caller deleting them does not shift the ordinals of the ones
// it has not deleted yet.
//
// A backdrop that cannot be decoded/hashed is SKIPPED, never matched: we cannot
// prove it is the polluted picture, and a delete must never rest on a slot we
// could not read. A fetch error, by contrast, aborts -- a blind spot in the
// backdrop set could hide the very copy we are trying to remove, and continuing
// past it would let a delete "succeed" while the pollution survives unseen.
//
// Takes connection.BackdropReader rather than the wider phashPlatformClient:
// the body below only ever calls GetArtistDetail/GetArtistBackdrop, and the
// narrower parameter lets a second caller (resolveFanartReplaceTarget, #3125
// F3) reuse this comparator through a client that does NOT implement
// IndexedImageDeleter, without a needless capability requirement. Every
// existing phashPlatformClient argument still satisfies this: the interface
// embeds BackdropReader.
func matchingBackdropIndices(ctx context.Context, client connection.BackdropReader, platformArtistID string, want uint64, tolerance float64) ([]int, error) {
	if err := validPHashTolerance(tolerance); err != nil {
		return nil, err
	}
	detail, err := client.GetArtistDetail(ctx, platformArtistID)
	if err != nil {
		return nil, fmt.Errorf("fetching artist detail: %w", err)
	}
	var matches []int
	for i := 0; i < detail.BackdropCount; i++ {
		data, _, fErr := client.GetArtistBackdrop(ctx, platformArtistID, i)
		if fErr != nil {
			return nil, fmt.Errorf("fetching backdrop %d: %w", i, fErr)
		}
		got, hErr := image.PerceptualHash(bytes.NewReader(data))
		if hErr != nil {
			continue // undecodable slot: cannot prove a match, so never delete it
		}
		if image.Similarity(want, got) >= tolerance {
			matches = append(matches, i)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(matches)))
	return matches, nil
}

// deletePollutedBackdrops removes every backdrop on the item whose perceptual
// hash is within tolerance of want, high-index-first, then re-reads the item
// and CONFIRMS none survives. Returns the number deleted.
//
// No match is not a failure: it means the pollution is already gone from this
// peer (never synced, or removed by a prior run), which makes the operation
// idempotent and safe to retry.
//
// VERIFY-BY-REFETCH is the crux. The platform returning 2xx on the DELETE does
// not prove the image is gone -- Emby and Jellyfin silently ignore some writes
// (documented on emby.Client.GetArtistPath). So after the deletes, the item is
// re-read: any surviving match is returned as an error, never swallowed as
// success. The caller must treat the polluted backdrop as still live until this
// returns nil.
func deletePollutedBackdrops(ctx context.Context, client phashPlatformClient, platformArtistID string, want uint64, tolerance float64) (int, error) {
	matches, err := matchingBackdropIndices(ctx, client, platformArtistID, want, tolerance)
	if err != nil {
		return 0, err
	}
	if len(matches) == 0 {
		return 0, nil
	}
	deleted := 0
	for _, idx := range matches { // descending: earlier deletes never shift a later index
		if delErr := client.DeleteImageAtIndex(ctx, platformArtistID, "fanart", idx); delErr != nil {
			return deleted, fmt.Errorf("deleting backdrop %d: %w", idx, delErr)
		}
		deleted++
	}
	remaining, err := matchingBackdropIndices(ctx, client, platformArtistID, want, tolerance)
	if err != nil {
		return deleted, fmt.Errorf("verifying backdrop removal: %w", err)
	}
	if len(remaining) > 0 {
		return deleted, fmt.Errorf("platform accepted %d delete(s) but %d matching backdrop(s) remain; the platform ignored the delete", deleted, len(remaining))
	}
	return deleted, nil
}

// restoreBackdrop re-uploads data as a NEW backdrop on the item when no
// perceptual match is already present, then confirms the artifact is present by
// re-reading. Reports whether it appended (true) or found the picture already
// present (false).
//
// APPEND, NEVER INDEX-WRITE. The upload targets the type endpoint so the peer
// assigns the next ordinal itself; there is no index to overwrite, which is the
// property that makes a restore unable to clobber a bystander backdrop. The
// recorded provenance ordinal is never used as a write target -- the same
// index-free discipline as the on-disk restore.
//
// IDEMPOTENT. A backdrop already within tolerance of the restored bytes is
// treated as already-present and no upload is made, so a retried restore
// converges instead of stacking duplicates. (Perceptual already-present is the
// contract the on-disk restore uses too: "byte-equal OR perceptual match ->
// no-op". It suppresses a redundant append; it authorizes nothing destructive.)
//
// DELIBERATELY A DIFFERENT THRESHOLD THAN resolveFanartReplaceTarget (#3125
// F3/round 3), which decides at EXACT bytes only -- not an inconsistency, a
// different consequence for the same kind of mistake. Here, a loose match
// costs one skipped duplicate append at worst. There, a loose match
// authorizes an in-place OVERWRITE of a platform slot -- getting it wrong
// destroys operator artwork, irreversibly. Same signal, different threshold
// because the two decisions gate different amounts of damage.
//
// VERIFY-BY-REFETCH, same reason as the delete: a 2xx is not proof. After the
// upload the item is re-read and a still-absent picture is an error, so a peer
// that accepted and dropped the write cannot be reported as a successful
// restore.
func restoreBackdrop(ctx context.Context, client phashPlatformClient, platformArtistID string, data []byte, tolerance float64) (bool, error) {
	if len(data) == 0 {
		// WriteFileAtomic-style guard: an empty upload would install nothing and
		// the verify below would then fail confusingly; refuse up front. Mirrors
		// image.WriteFanartBytes's empty-data refusal on the on-disk side.
		return false, fmt.Errorf("refusing to restore empty backdrop bytes")
	}
	want, err := image.PerceptualHash(bytes.NewReader(data))
	if err != nil {
		return false, fmt.Errorf("hashing bytes to restore: %w", err)
	}
	present, err := matchingBackdropIndices(ctx, client, platformArtistID, want, tolerance)
	if err != nil {
		return false, err
	}
	if len(present) > 0 {
		return false, nil // already present -> no-op, idempotent
	}
	// http.DetectContentType drives the peer's save format; on image bytes it
	// returns image/jpeg, image/png, etc. The same value the acquisition path
	// would send.
	contentType := http.DetectContentType(data)
	if err := client.UploadImage(ctx, platformArtistID, "fanart", data, contentType); err != nil {
		return false, fmt.Errorf("uploading backdrop: %w", err)
	}
	check, err := matchingBackdropIndices(ctx, client, platformArtistID, want, tolerance)
	if err != nil {
		return false, fmt.Errorf("verifying backdrop restore: %w", err)
	}
	if len(check) == 0 {
		return false, fmt.Errorf("platform accepted the upload but no matching backdrop is present; the platform ignored the write")
	}
	return true, nil
}

// fanartReplaceClient is what uploadOneImageForSync's fanart branch needs to
// perform a NON-CLOBBERING replace (#3125 F3): read the current backdrop set
// to identify a target, write at a specific index, and fall back to an
// append when no target can be identified.
type fanartReplaceClient interface {
	connection.BackdropReader       // GetArtistDetail, GetArtistBackdrop
	connection.IndexedImageUploader // UploadImageAtIndex (in-place replace)
	connection.ImageUploader        // UploadImage (append fallback)
}

// fanartResyncClient is what uploadFanartFullResyncForSync needs to perform
// the delete-all-then-reupload sequence a platform reports false for
// connection.SupportsIndexedBackdropReplace actually honors (#3135): read the
// current backdrop count, delete every existing slot, and re-upload the full
// local set in index order. A separate, narrower-than-fanartReplaceClient
// interface rather than widening that one, so fanartReplaceClient's existing
// test doubles (which never exercise this path -- they are all Emby-typed)
// do not need an unused DeleteImageAtIndex method added to keep compiling.
//
// #3146 CR review: embeds connection.ArtistStateGetter (GetArtistDetail
// alone), not the wider connection.BackdropReader (GetArtistDetail +
// GetArtistBackdrop) -- uploadFanartFullResyncForSync never calls
// GetArtistBackdrop, it only needs the platform's current backdrop COUNT to
// drive the delete loop, never any backdrop's actual bytes. Declaring
// exactly the three methods this function calls keeps a future fake
// implementing this interface from being told it satisfies a read
// capability that is never exercised.
type fanartResyncClient interface {
	connection.ArtistStateGetter    // GetArtistDetail
	connection.IndexedImageDeleter  // DeleteImageAtIndex
	connection.IndexedImageUploader // UploadImageAtIndex
}

// resolveFanartTarget is the ALLOW-LIST decision resolveFanartReplaceTarget's
// name comes from: never "no reason to refuse", always "a positive reason to
// believe this index is safe to overwrite".
type resolveFanartTarget int

const (
	// fanartTargetAppend means no index could be positively identified as
	// safe to overwrite; the caller must use the non-indexed, ADD-ONLY
	// UploadImage call. One duplicate is the accepted cost of not being able
	// to prove a safe index -- destroying a bystander backdrop is not.
	fanartTargetAppend resolveFanartTarget = iota
	// fanartTargetNoop means a backdrop already BYTE-IDENTICAL to the new
	// bytes is present; nothing should be uploaded at all.
	fanartTargetNoop
	// fanartTargetIndex means Index is a positively-identified safe
	// overwrite target: either the platform is empty (index 0 creates
	// the first slot, cannot clobber anything) or a backdrop BYTE-IDENTICAL
	// to the artist's PREVIOUS primary was found at Index.
	fanartTargetIndex
)

// fanartReplaceDecision is resolveFanartReplaceTarget's result.
type fanartReplaceDecision struct {
	Kind  resolveFanartTarget
	Index int    // valid only when Kind == fanartTargetIndex
	Why   string // human-readable reason, used in the Warn log on the append fallback
}

// resolveFanartReplaceTarget decides how to write a fanart REPLACE onto a
// platform without clobbering a bystander backdrop (#3125 F3).
//
// THE DEFECT THIS GUARDS. uploadOneImageForSync's fanart branch always wrote
// platform index 0, on the assumption that the local primary fanart file is
// always platform slot 0. That assumption holds only IMMEDIATELY after a
// fresh full sync. Three paths can shift a backdrop's platform index without
// Stillwater's involvement: the phash back-out prune (deletePollutedBackdrops),
// the remote-dedup prune (PrunePlatformBackdropDuplicates), and an operator
// deleting a backdrop directly in the Emby/Jellyfin UI. Any of those can
// delete platform index 0, and the peer re-indexes the survivors down by one
// (Emby measured live, #3125 review) -- so what WAS a bystander at index 1 is
// now sitting at index 0. A subsequent unconditional "write index 0" then
// overwrites that bystander with the new primary, DESTROYING a distinct
// image rather than merely duplicating one.
//
// THE RULE: a destructive index write is authorized ONLY by a POSITIVE
// identification that the target index currently holds either nothing or
// Stillwater's own previous primary -- never by the absence of a reason to
// doubt it. Three outcomes, checked in this order:
//
//  1. NOOP: the platform already holds a backdrop BYTE-IDENTICAL to the NEW
//     data AT THE SLOT THIS REPLACE WOULD WRITE (image.ContentHash, exact
//     SHA-256). Nothing to do.
//  2. INDEX: either the platform has ZERO backdrops (index 0 creates the
//     first slot; there is nothing there to clobber), or a backdrop
//     BYTE-IDENTICAL to previousData (the artist's actual previous-primary
//     bytes, from the pre-save on-disk backup -- see
//     previousFanartPrimaryData's doc comment for why this is sourced from
//     the backup and never from a database column) is found.
//  3. APPEND: neither of the above. The caller must fall back to the
//     non-indexed, add-only UploadImage -- one accepted duplicate rather
//     than a destroyed bystander. Why explains which condition failed, for
//     the Warn log the caller writes. Because an append has no fixed target
//     slot (see writeTarget's doc comment for what "the slot we would
//     write" means when nothing can be positively identified), a RETRY of
//     an append decision -- the sibling Emby-500 defect, #3126, where the
//     upload actually lands but is reported as a failure -- is caught here
//     too: if the exact bytes already exist ANYWHERE, appending again would
//     just be a second copy of an upload that already happened.
//
// ONE NOTION OF "THE SLOT WE WOULD WRITE" (#3125 round 3, H1+H2). Round 2
// computed the NOOP and INDEX decisions independently, and both asked a
// list-membership question ("do these bytes appear somewhere?") where the
// contract needs a slot-identity question ("do these bytes sit at the slot
// this write would target?"). That one conflation surfaced as two separate
// defects:
//
//   - H1 (inverted tie-break): when previousData matched more than one slot
//     (a stray duplicate left by the very append-bug #3125 exists to fix),
//     the code picked the HIGHEST matching index on the theory that
//     duplicates accumulate at the tail. That is backwards: index 0 is the
//     slot a peer actually RENDERS (measured live on Emby 4.9.5.0: the
//     bare, no-index GET returns byte-identical to index 0), so writing the
//     new primary into a higher stray-duplicate slot leaves the rendered
//     slot holding the OLD image -- the operator sees no change at all.
//     The lowest matching index is always the one closest to (or at) the
//     rendered slot on an append-only platform, so it is the correct pick.
//   - H2 (any-index noop): asking "are these bytes ANYWHERE in the list"
//     for the noop decision meant a bystander backdrop that happened to
//     already hold the new bytes (appended earlier, including by the
//     append-bug itself) silently suppressed the write the actual primary
//     slot still needed -- zero POSTs, UI reports success, platform
//     unchanged.
//
// writeTarget below is now the SINGLE function both decisions consult for
// "the slot we would write", so a future third decision cannot reintroduce
// this conflation by computing its own independent list-membership check.
//
// This also restores the APPEND-fallback convergence the doc comment above
// promises ("one accepted duplicate"): under H2's bug, a sync that took the
// append fallback would have its bytes sit at the tail, and the VERY NEXT
// sync of those same bytes would find them there via the any-index check
// and declare NOOP -- index 0 (the rendered slot) never getting repaired,
// permanently. Restricting the any-index match to the append-retry case
// only (outcome 3's own idempotency, never a stand-in for outcome 1) means
// the next sync instead finds previousData at index 0 via writeTarget and
// correctly issues the repair write.
//
// EXACT BYTES, NOT PERCEPTUAL SIMILARITY (round 1 review C2/C3). This
// function used to decide both outcomes at collision.DefaultTolerance (0.90
// similarity, Hamming <= 6 on a 64-bit dHash) -- a threshold designed for a
// COLLISION DETECTOR (internal/collision/notifier.go: "these MIGHT be the
// same picture, tell a human"), not for authorizing a write. Two measured
// failures followed directly from reusing a detector's threshold as a
// decision gate:
//
//   - NOOP too loose (C2): a operator's minor re-crop or brightness nudge --
//     the single most common fanart edit, and Stillwater ships a cropper --
//     measures within 0.90 of the original (Hamming 4-6 in the round-1
//     reproduction). Deciding NOOP at that tolerance silently swallows a
//     real edit: the file changes on disk, the UI reports success, and the
//     platform is never touched. There is no warning, because NOOP is not a
//     failure path -- it looks exactly like the correct "nothing to do"
//     idempotency case that legitimately needs no diagnostic.
//   - INDEX too loose (C3, the more serious direction): two backdrops from
//     the same shoot -- adjacent frames, or the same shot at two crops, an
//     entirely ordinary library -- can measure within 0.90 of each other.
//     If a prune or an operator delete puts the near-duplicate SIBLING at
//     index 0 (having removed the real previous primary), the resolver
//     would authorize overwriting it: a perceptual near-match at a
//     detector's threshold is the ABSENCE of 6 bits of difference, not the
//     positive identification this function's own contract demands. That
//     re-opens the exact clobber class F3 exists to prevent, through the
//     matcher instead of around it -- and unlike NOOP's cost (a missing
//     upload), this direction destroys operator artwork, irreversibly.
//
// Exact-byte equality has neither failure mode: a re-crop or a brightness
// tweak produces different bytes by definition, so it can never be
// mistaken for "no change" or "the same picture as some other slot". It is
// also the ACTUAL retry case both NOOP and INDEX exist to serve -- a retried
// upload after a lost response re-sends the identical bytes, which byte
// equality catches with zero false positives. Measured live (see the round-2
// and round-3 reports): both peers round-trip these fanart uploads
// byte-identical (SHA-256 matches after upload+readback) -- Emby 4.9.5.0 for
// JPEG and PNG, Jellyfin 10.11.10 for JPEG (PNG keeps its magic bytes on
// readback too), so exact equality converges on retry in practice across
// both platforms and both formats measured, not merely in theory; a peer
// caught genuinely re-encoding on write would fall through to the safe
// direction (APPEND) rather than falsely matching, which is the correct
// failure mode for a comparison this function does not control.
//
// Costs one extra platform READ (GetArtistDetail + up to BackdropCount
// GetArtistBackdrop calls) per fanart sync before any write. See the report
// for why this is not expected to matter on the operator-paced UI path, and
// C4 for the bulk-path cost this was measured to add and the fix that keeps
// each backdrop's bytes to a single read.
func resolveFanartReplaceTarget(ctx context.Context, client fanartReplaceClient, platformArtistID string, newData []byte, previousData []byte) (fanartReplaceDecision, error) {
	if len(newData) == 0 {
		return fanartReplaceDecision{}, fmt.Errorf("refusing to resolve a replace target for empty fanart bytes")
	}
	wantHash := image.ContentHash(newData)

	detail, err := client.GetArtistDetail(ctx, platformArtistID)
	if err != nil {
		return fanartReplaceDecision{}, fmt.Errorf("fetching artist detail: %w", err)
	}

	// Outcome 2a: nothing on the platform yet. Index 0 creates the first
	// slot; there is no bystander at any index to clobber, and there is
	// nothing to hash a noop candidate against either.
	if detail.BackdropCount == 0 {
		return fanartReplaceDecision{Kind: fanartTargetIndex, Index: 0}, nil
	}

	// C4: hash every backdrop's bytes exactly ONCE, whichever branch below
	// consults them. The old code called matchingBackdropIndices (a full
	// re-read-and-rehash of every backdrop) up to twice; on an 8-backdrop
	// item that measured GetArtistDetail=3, GetArtistBackdrop=16 across one
	// resolve. Both queries below are simple content-hash comparisons over
	// this one cached slice, so the platform is read at most once per
	// backdrop regardless of how many of the two checks below run.
	hashes, err := backdropContentHashes(ctx, client, platformArtistID, detail.BackdropCount)
	if err != nil {
		return fanartReplaceDecision{}, err
	}

	// writeTarget is the ONE lookup both remaining outcomes consult (H1+H2):
	// "the slot this replace would write", identified only when the
	// previous primary's exact bytes are found on the platform.
	if target, ok := writeTarget(hashes, previousData); ok {
		// Outcome 1 (targeted): the write target itself already holds the
		// new bytes -- this is a retry of an INDEX write whose response was
		// lost, not a fresh replace. Comparing against THIS slot specifically
		// (never "anywhere in the list") is what H2 requires: a bystander
		// slot that happens to already hold the new bytes must never
		// suppress the write the actual target slot still needs.
		if hashes[target] == wantHash {
			return fanartReplaceDecision{Kind: fanartTargetNoop}, nil
		}
		// Outcome 2b: the previous primary's bytes were positively
		// identified at target -- safe to overwrite.
		return fanartReplaceDecision{Kind: fanartTargetIndex, Index: target}, nil
	}

	// No target could be identified (no previousData, or none of its bytes
	// are found on this item). We cannot pick an index to overwrite, but an
	// APPEND still needs to be idempotent against its own retry: if the new
	// bytes already exist ANYWHERE, a prior append already delivered them
	// (including the #3126 shape, where an indexed upload lands but is
	// reported as a failure), and appending again would only add a second
	// copy of an upload that already happened. This is the ONLY place an
	// any-index match is allowed to decide NOOP, precisely because no
	// specific slot is being protected here -- there is no target for a
	// bystander match to wrongly stand in for.
	if indexOfContentHash(hashes, wantHash) >= 0 {
		return fanartReplaceDecision{Kind: fanartTargetNoop}, nil
	}

	// Outcome 3: cannot positively identify a safe index. Append.
	return fanartReplaceDecision{
		Kind: fanartTargetAppend,
		Why:  "no previous-primary bytes matched any current platform backdrop exactly, and the platform already holds at least one backdrop, so no index could be positively identified as safe to overwrite",
	}, nil
}

// writeTarget identifies "the slot this fanart replace would write" -- the
// single notion of target both the NOOP and INDEX outcomes must agree on
// (#3125 round 3, H1+H2). Returns ok=false when no slot can be positively
// identified: no previousData was supplied, or none of its bytes are found
// on this item (the previous primary is gone from the platform, or was
// never captured). Callers must never guess an index in that case -- the
// caller here falls back to append.
//
// Matches on the LOWEST index holding previousData's exact bytes (H1): on
// an append-only platform, duplicates of a slot accumulate at HIGHER
// indices than the original, never lower, and index 0 is the slot a peer
// actually renders (measured live: Emby's bare, no-index GET returns
// byte-identical to index 0). The highest match -- what round 2 used --
// picks the stray duplicate over the rendered slot, so the write lands
// somewhere the operator never sees and the rendered slot never changes.
func writeTarget(hashes []string, previousData []byte) (index int, ok bool) {
	if len(previousData) == 0 {
		// Empty previousData is "cannot identify", never "matches
		// everything" -- there are no bytes to hash, so there is nothing to
		// compare against.
		return -1, false
	}
	prevHash := image.ContentHash(previousData)
	idx := indexOfContentHash(hashes, prevHash)
	if idx < 0 {
		return -1, false
	}
	return idx, true
}

// backdropContentHashes reads every backdrop for the item ONCE and returns
// each slot's exact content hash, indexed by platform position (index i of
// the result is index i's hash; an undecodable-by-hash-standards slot still
// gets a real SHA-256 of its raw bytes, since ContentHash never needs to
// decode the image the way PerceptualHash does -- there is no "cannot hash"
// case for exact bytes). A backdrop fetch error aborts the whole read: a
// blind spot could hide the very slot a caller is trying to identify, and
// silently skipping it would let this resolver authorize a write against an
// incomplete picture of the platform's actual state.
func backdropContentHashes(ctx context.Context, client connection.BackdropReader, platformArtistID string, count int) ([]string, error) {
	hashes := make([]string, count)
	for i := 0; i < count; i++ {
		data, _, fErr := client.GetArtistBackdrop(ctx, platformArtistID, i)
		if fErr != nil {
			return nil, fmt.Errorf("fetching backdrop %d: %w", i, fErr)
		}
		hashes[i] = image.ContentHash(data)
	}
	return hashes, nil
}

// indexOfContentHash returns the FIRST (lowest) index whose hash equals
// want, or -1. Used both by writeTarget (the lowest match is the slot
// closest to what a peer renders -- see writeTarget's doc comment, H1) and
// by the append-retry noop check in resolveFanartReplaceTarget (any match
// suffices there, since no specific slot is being protected).
func indexOfContentHash(hashes []string, want string) int {
	for i, h := range hashes {
		if h == want {
			return i
		}
	}
	return -1
}

// PlatformBackdropOpFailure records one connection whose platform delete or
// restore failed. The artist/op continues past it; the caller decides fatality.
type PlatformBackdropOpFailure struct {
	ConnectionID string
	Err          string
}

// PlatformBackdropDeleteResult summarizes a per-artist platform delete pass.
//
// Targets lists the items a matching backdrop was actually deleted from -- the
// exact set to record on the quarantine entry so a later restore knows where to
// put the picture back. A connection where nothing matched is NOT a target:
// there is nothing there to restore into.
type PlatformBackdropDeleteResult struct {
	Targets  []image.RepairPlatformTarget
	Deleted  int
	Failures []PlatformBackdropOpFailure
}

// DeletePollutedBackdropOnPlatforms removes the backdrop whose perceptual hash
// is phashHex, at the removal's tolerance, from every enabled, healthy,
// image-write-enabled platform the artist is mapped to, and returns the items
// it was deleted from.
//
// Per-connection failures are collected, not fatal to the batch: one peer being
// unreachable must not strand the pollution on the others. The caller (the
// remediation pipeline) records result.Targets on the quarantine entry and
// decides how to treat result.Failures -- the on-disk quarantine already holds
// the bytes, so a failed platform delete is recoverable, not lost data.
func (p *Publisher) DeletePollutedBackdropOnPlatforms(ctx context.Context, artistID, phashHex string, tolerance float64) (PlatformBackdropDeleteResult, error) {
	var result PlatformBackdropDeleteResult
	if p == nil || p.artistService == nil || p.connectionService == nil {
		return result, fmt.Errorf("delete polluted backdrop on platforms: publisher not fully wired")
	}
	if err := validPHashTolerance(tolerance); err != nil {
		return result, err
	}
	want, err := image.ParseHashHex(phashHex)
	if err != nil {
		return result, fmt.Errorf("parsing phash %q: %w", phashHex, err)
	}
	platformIDs, err := p.artistService.GetPlatformIDs(ctx, artistID)
	if err != nil {
		return result, fmt.Errorf("loading platform ids for %s: %w", artistID, err)
	}
	for _, pid := range platformIDs {
		conn, connErr := p.connectionService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			result.Failures = append(result.Failures, PlatformBackdropOpFailure{ConnectionID: pid.ConnectionID, Err: connErr.Error()})
			continue
		}
		if !conn.Enabled || conn.Status != "ok" || !conn.GetFeatureImageWrite() {
			continue
		}
		client := phashPlatformClientFactory(conn, p.logger)
		if client == nil {
			continue
		}
		unlock := p.lockPhashTarget(pid.ConnectionID, pid.PlatformArtistID)
		deleted, delErr := deletePollutedBackdrops(ctx, client, pid.PlatformArtistID, want, tolerance)
		unlock()
		// Record any SUCCESSFUL deletions BEFORE handling delErr: a partial
		// multi-delete that removed slot A and then failed on slot B has really
		// taken A off the platform, so A's target must be recorded to stay
		// RESTORABLE. Dropping the target on a partial failure would strand the
		// removed picture with nowhere for a restore to put it back.
		if deleted > 0 {
			result.Deleted += deleted
			result.Targets = append(result.Targets, image.RepairPlatformTarget{
				ConnectionID: pid.ConnectionID, PlatformArtistID: pid.PlatformArtistID,
			})
			p.logger.Info("phash platform delete removed polluted backdrop",
				slog.String("artist_id", artistID), slog.String("connection", conn.Name),
				slog.Int("deleted", deleted))
		}
		if delErr != nil {
			p.logger.Error("phash platform delete failed",
				slog.String("artist_id", artistID), slog.String("connection", conn.Name),
				slog.String("error", delErr.Error()))
			result.Failures = append(result.Failures, PlatformBackdropOpFailure{ConnectionID: pid.ConnectionID, Err: delErr.Error()})
			continue
		}
	}
	return result, nil
}

// PlatformBackdropRestoreResult summarizes a per-entry platform restore pass.
type PlatformBackdropRestoreResult struct {
	Appended       int
	AlreadyPresent int
	Failures       []PlatformBackdropOpFailure
}

// RestoreBackdropToPlatforms re-uploads data to each recorded target, appending
// it when absent and treating a perceptual match as already-present. tolerance
// is the removal's own cutoff.
//
// It iterates the entry's PlatformTargets rather than re-deriving the current
// mapping, because the target is the item the picture was TAKEN FROM -- the one
// that must get it back. A target whose connection has since been removed,
// disabled, or made unhealthy is recorded as a failure (not silently skipped),
// so the caller keeps the quarantine entry rather than consuming it against a
// restore that did not happen.
func (p *Publisher) RestoreBackdropToPlatforms(ctx context.Context, targets []image.RepairPlatformTarget, data []byte, tolerance float64) (PlatformBackdropRestoreResult, error) {
	var result PlatformBackdropRestoreResult
	if p == nil || p.connectionService == nil {
		return result, fmt.Errorf("restore backdrop to platforms: publisher not fully wired")
	}
	if err := validPHashTolerance(tolerance); err != nil {
		return result, err
	}
	if len(data) == 0 {
		return result, fmt.Errorf("restore backdrop to platforms: refusing to restore empty bytes")
	}
	for _, t := range targets {
		conn, connErr := p.connectionService.GetByID(ctx, t.ConnectionID)
		if connErr != nil {
			result.Failures = append(result.Failures, PlatformBackdropOpFailure{ConnectionID: t.ConnectionID, Err: connErr.Error()})
			continue
		}
		if !conn.Enabled || conn.Status != "ok" || !conn.GetFeatureImageWrite() {
			result.Failures = append(result.Failures, PlatformBackdropOpFailure{
				ConnectionID: t.ConnectionID,
				Err:          "connection not enabled, healthy, and image-write-enabled; cannot restore",
			})
			continue
		}
		client := phashPlatformClientFactory(conn, p.logger)
		if client == nil {
			result.Failures = append(result.Failures, PlatformBackdropOpFailure{ConnectionID: t.ConnectionID, Err: "unsupported connection type"})
			continue
		}
		unlock := p.lockPhashTarget(t.ConnectionID, t.PlatformArtistID)
		appended, rErr := restoreBackdrop(ctx, client, t.PlatformArtistID, data, tolerance)
		unlock()
		if rErr != nil {
			p.logger.Error("phash platform restore failed",
				slog.String("connection", conn.Name), slog.String("error", rErr.Error()))
			result.Failures = append(result.Failures, PlatformBackdropOpFailure{ConnectionID: t.ConnectionID, Err: rErr.Error()})
			continue
		}
		if appended {
			result.Appended++
		} else {
			result.AlreadyPresent++
		}
	}
	return result, nil
}
