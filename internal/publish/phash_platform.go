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
//     data (image.ContentHash, exact SHA-256). Nothing to do -- this also
//     neutralizes the retry path in the sibling Emby-500 defect (#3126).
//  2. INDEX: either the platform has ZERO backdrops (index 0 creates the
//     first slot; there is nothing there to clobber), or a backdrop
//     BYTE-IDENTICAL to previousData (the artist's actual previous-primary
//     bytes, from the pre-save on-disk backup -- see
//     previousFanartPrimaryData's doc comment for why this is sourced from
//     the backup and never from a database column) is found.
//  3. APPEND: neither of the above. The caller must fall back to the
//     non-indexed, add-only UploadImage -- one accepted duplicate rather
//     than a destroyed bystander. Why explains which condition failed, for
//     the Warn log the caller writes.
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
// report): Emby round-trips these fanart uploads byte-identical (SHA-256
// matches after upload+readback) for ordinary JPEG fixtures, so exact
// equality converges on retry in practice, not merely in theory; a peer
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
	// slot; there is no bystander at any index to clobber.
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

	// Outcome 1: already present -- checked before consulting previousData,
	// so a retry (the platform already reflects the new primary from a prior
	// call whose response was lost) is idempotent regardless of whether
	// previousData is available.
	if indexOfContentHash(hashes, wantHash) >= 0 {
		return fanartReplaceDecision{Kind: fanartTargetNoop}, nil
	}

	// Outcome 2b: identify the slot holding the PREVIOUS primary, by EXACT
	// content hash. Empty previousData is "cannot identify", not "matches
	// everything" -- there is no bytes to hash, so there is nothing to
	// compare against.
	if len(previousData) > 0 {
		prevHash := image.ContentHash(previousData)
		if idx := lastIndexOfContentHash(hashes, prevHash); idx >= 0 {
			// The LAST (highest-index) exact match is preferred when more
			// than one slot happens to be byte-identical to the previous
			// primary (an already-present duplicate): duplicates of a
			// primary accumulate at higher indices on an append-only
			// platform, never lower ones, so the highest match is the one
			// most likely to be a stray duplicate rather than the original
			// primary slot itself -- overwriting it leaves the original
			// primary's slot untouched if the two ever diverge again.
			return fanartReplaceDecision{Kind: fanartTargetIndex, Index: idx}, nil
		}
	}

	// Outcome 3: cannot positively identify a safe index. Append.
	return fanartReplaceDecision{
		Kind: fanartTargetAppend,
		Why:  "no previous-primary bytes matched any current platform backdrop exactly, and the platform already holds at least one backdrop, so no index could be positively identified as safe to overwrite",
	}, nil
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

// indexOfContentHash returns the first index whose hash equals want, or -1.
func indexOfContentHash(hashes []string, want string) int {
	for i, h := range hashes {
		if h == want {
			return i
		}
	}
	return -1
}

// lastIndexOfContentHash returns the LAST (highest) index whose hash equals
// want, or -1. See resolveFanartReplaceTarget's outcome 2b for why the
// highest match is preferred among several.
func lastIndexOfContentHash(hashes []string, want string) int {
	for i := len(hashes) - 1; i >= 0; i-- {
		if hashes[i] == want {
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
