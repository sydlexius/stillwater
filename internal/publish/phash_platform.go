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
func matchingBackdropIndices(ctx context.Context, client phashPlatformClient, platformArtistID string, want uint64, tolerance float64) ([]int, error) {
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
		deleted, delErr := deletePollutedBackdrops(ctx, client, pid.PlatformArtistID, want, tolerance)
		if delErr != nil {
			p.logger.Error("phash platform delete failed",
				slog.String("artist_id", artistID), slog.String("connection", conn.Name),
				slog.String("error", delErr.Error()))
			result.Failures = append(result.Failures, PlatformBackdropOpFailure{ConnectionID: pid.ConnectionID, Err: delErr.Error()})
			continue
		}
		if deleted > 0 {
			result.Deleted += deleted
			result.Targets = append(result.Targets, image.RepairPlatformTarget{
				ConnectionID: pid.ConnectionID, PlatformArtistID: pid.PlatformArtistID,
			})
			p.logger.Info("phash platform delete removed polluted backdrop",
				slog.String("artist_id", artistID), slog.String("connection", conn.Name),
				slog.Int("deleted", deleted))
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
		appended, rErr := restoreBackdrop(ctx, client, t.PlatformArtistID, data, tolerance)
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
