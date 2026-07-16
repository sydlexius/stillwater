// Package publish -- backdrop_prune.go
// Platform-side backdrop de-duplication (#2540 remote prune). The local rule
// engine collapses byte-identical fanart on disk, but platform sync is
// additive (SyncAllFanartToPlatforms never deletes surplus indices), so the
// copies already pushed to Emby/Jellyfin persist. This prunes them: content-
// matched (sha256), exact-only, admin-triggered.
package publish

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sort"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
)

// backdropPruneClient is what the engine needs from a platform: read backdrops
// and delete one at an index.
type backdropPruneClient interface {
	connection.BackdropReader
	connection.IndexedImageDeleter
}

// newBackdropPruneClient builds a prune client for the connection type, mirroring
// newIndexedImageUploader. Returns nil for unsupported types.
func newBackdropPruneClient(conn *connection.Connection, logger *slog.Logger) backdropPruneClient {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	default:
		return nil
	}
}

// backdropPruneClientFactory is a package-level seam so tests can substitute
// a fake platform client without widening any exported surface or touching
// the Publisher constructor. Production always calls through to
// newBackdropPruneClient; tests reassign this var (with a t.Cleanup restore).
var backdropPruneClientFactory = newBackdropPruneClient

// artistPageLister is the narrow slice of *artist.Service used to page
// through the whole library for a scan. It is intentionally NOT added to the
// artistPlatformLister interface: that interface already has a large fake
// surface across this package's test files (fakePlatformLister,
// errPlatformLister, reconcilePlatformLister, panicPlatformLister, and more),
// and widening it would force every one of them to grow a List method they
// don't otherwise need. Instead it is wired as its own explicit Deps.ArtistLister
// / Publisher.artistLister field: p.artistService's concrete production type
// (*artist.Service) already implements List, but a compile-time-checked field
// is used instead of a runtime type assertion so a future decorator around
// *artist.Service that satisfies artistPlatformLister but not List fails to
// compile rather than failing (or being silently skipped) at scan time.
type artistPageLister interface {
	List(ctx context.Context, params artist.ListParams) ([]artist.Artist, int, error)
}

// ArtistPlatformBackdropDup is one artist's redundant backdrops on one
// platform connection.
type ArtistPlatformBackdropDup struct {
	ArtistID     string
	Name         string
	ConnectionID string
	Connection   string
	Backdrops    int // total backdrops read from the platform
	Redundant    int // byte-identical redundant copies (deletable)
}

// PlatformBackdropDupReport is the library-wide platform-side blast radius.
// ArtistsAffected counts artist/connection PAIRS with redundant backdrops
// (an artist mapped to two platforms with duplicates counts as 2), matching
// one entry in PerArtist per pair.
type PlatformBackdropDupReport struct {
	ConnectionsAffected int
	ArtistsAffected     int
	RedundantBackdrops  int
	ScanErrors          int // artist/connection scans that failed and were SKIPPED (no silent truncation)
	PerArtist           []ArtistPlatformBackdropDup
}

// scanBackdropPageSize bounds each artist-list page during a scan. Must be
// within artist.ListParams.Validate's [10, 500] range.
const scanBackdropPageSize = 200

// redundantBackdrop is one redundant (deletable) backdrop index together
// with the sha256 it had at detection time. The hash is carried forward so
// the prune loop can re-verify, immediately before deleting, that the
// index's content has not changed since detection (closing the TOCTOU
// window between hashing and delete: a concurrent platform write could
// otherwise replace the index's content and this guard would delete a
// now-possibly-unique image).
type redundantBackdrop struct {
	Index int
	Hash  [32]byte
}

// dedupBackdropIndices returns the indices to delete: every index except the
// lowest in each byte-identical group, each carrying the hash it had at
// detection. Sorted DESCENDING by Index so callers delete high-index-first
// (Emby/Jellyfin re-index the remaining backdrops after each delete, so a
// low-to-high delete order would shift later indices out from under the
// caller).
func dedupBackdropIndices(hashes [][32]byte) []redundantBackdrop {
	seen := make(map[[32]byte]bool, len(hashes))
	var redundant []redundantBackdrop
	for i, h := range hashes {
		if seen[h] {
			redundant = append(redundant, redundantBackdrop{Index: i, Hash: h})
			continue
		}
		seen[h] = true
	}
	sort.Slice(redundant, func(a, b int) bool { return redundant[a].Index > redundant[b].Index })
	return redundant
}

// platformBackdropDup holds one artist/connection's detection result.
type platformBackdropDup struct {
	connID, connName string
	backdrops        int
	redundantCount   int
}

// backdropRedundantIndices reads every backdrop for the artist on the given
// connection, hashes it, and returns the redundant (deletable, descending by
// index) entries along with the total backdrop count. A read error for ANY
// index aborts the whole connection (returns an error) rather than risking a
// partial/blind result -- the caller counts this as a skipped scan, never a
// partial one.
func backdropRedundantIndices(ctx context.Context, client backdropPruneClient, platformArtistID string) (redundant []redundantBackdrop, total int, err error) {
	detail, err := client.GetArtistDetail(ctx, platformArtistID)
	if err != nil {
		return nil, 0, fmt.Errorf("fetching artist detail: %w", err)
	}
	count := detail.BackdropCount
	if count <= 1 {
		return nil, count, nil
	}
	hashes := make([][32]byte, 0, count)
	for i := 0; i < count; i++ {
		data, _, fErr := client.GetArtistBackdrop(ctx, platformArtistID, i)
		if fErr != nil {
			return nil, 0, fmt.Errorf("fetching backdrop %d: %w", i, fErr)
		}
		hashes = append(hashes, sha256.Sum256(data))
	}
	return dedupBackdropIndices(hashes), count, nil
}

// detectArtistPlatformDups reads every backdrop for the artist on each of its
// enabled, healthy connections and returns the redundant (deletable) indices
// per connection. A read error for any backdrop skips that whole connection
// (counted via scanErrs) rather than risking a partial delete.
func (p *Publisher) detectArtistPlatformDups(ctx context.Context, artistID string) (dups []platformBackdropDup, scanErrs int) {
	platformIDs, err := p.artistService.GetPlatformIDs(ctx, artistID)
	if err != nil {
		p.logger.Warn("platform backdrop scan: platform IDs unavailable",
			slog.String("artist_id", artistID), slog.String("error", err.Error()))
		return nil, 1
	}
	for _, pid := range platformIDs {
		conn, connErr := p.connectionService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			p.logger.Warn("platform backdrop scan: connection load failed",
				slog.String("connection_id", pid.ConnectionID), slog.String("error", connErr.Error()))
			scanErrs++
			continue
		}
		if !conn.Enabled || conn.Status != "ok" {
			continue
		}
		client := backdropPruneClientFactory(conn, p.logger)
		if client == nil {
			continue
		}
		redundant, count, detErr := backdropRedundantIndices(ctx, client, pid.PlatformArtistID)
		if detErr != nil {
			p.logger.Warn("platform backdrop scan: backdrop read failed; skipping connection",
				slog.String("artist_id", artistID), slog.String("connection", conn.Name), slog.String("error", detErr.Error()))
			scanErrs++
			continue
		}
		if len(redundant) == 0 {
			continue
		}
		dups = append(dups, platformBackdropDup{connID: pid.ConnectionID, connName: conn.Name, backdrops: count, redundantCount: len(redundant)})
	}
	return dups, scanErrs
}

// ScanPlatformBackdropDuplicates walks every artist and reports byte-identical
// redundant backdrops on each connected platform. Read-only.
func (p *Publisher) ScanPlatformBackdropDuplicates(ctx context.Context) (PlatformBackdropDupReport, error) {
	if p == nil || p.artistService == nil || p.connectionService == nil || p.artistLister == nil {
		return PlatformBackdropDupReport{}, fmt.Errorf("scan platform backdrop duplicates: publisher not fully wired")
	}

	var report PlatformBackdropDupReport
	conns := make(map[string]bool)
	page := 1
	for {
		artists, _, err := p.artistLister.List(ctx, artist.ListParams{Page: page, PageSize: scanBackdropPageSize})
		if err != nil {
			return PlatformBackdropDupReport{}, fmt.Errorf("listing artists at page %d: %w", page, err)
		}
		if len(artists) == 0 {
			break
		}
		for i := range artists {
			a := &artists[i]
			dups, scanErrs := p.detectArtistPlatformDups(ctx, a.ID)
			report.ScanErrors += scanErrs
			for _, d := range dups {
				report.ArtistsAffected++ // per artist/connection pair with dups
				report.RedundantBackdrops += d.redundantCount
				conns[d.connID] = true
				report.PerArtist = append(report.PerArtist, ArtistPlatformBackdropDup{
					ArtistID: a.ID, Name: a.Name, ConnectionID: d.connID, Connection: d.connName,
					Backdrops: d.backdrops, Redundant: d.redundantCount,
				})
			}
		}
		if len(artists) < scanBackdropPageSize {
			break
		}
		page++
	}
	report.ConnectionsAffected = len(conns)
	return report, nil
}

// PlatformBackdropPruneFailure records one artist/connection whose prune
// failed (detection or delete).
type PlatformBackdropPruneFailure struct {
	ArtistID     string
	ConnectionID string
	Err          string
}

// PlatformBackdropPruneResult summarizes a prune run.
type PlatformBackdropPruneResult struct {
	ArtistsProcessed int
	BackdropsRemoved int
	// SkippedChanged counts indices that were detected as redundant but were
	// NOT deleted because the immediate-pre-delete re-verify found their
	// content had changed since detection (re-fetch error or hash mismatch) --
	// a concurrent platform write closed the TOCTOU window. These are never
	// deleted; a skip is not a failure, just a missed opportunity this run.
	SkippedChanged int
	Failures       []PlatformBackdropPruneFailure
}

// PrunePlatformBackdropDuplicates deletes byte-identical redundant backdrops
// on every connected, image-write-enabled platform, high-index-first.
// Re-detects from the platform on every call (does not trust a prior scan
// result, which may be stale by the time an operator triggers the prune).
// Exact-only: a deleted copy is byte-identical to a kept one, so nothing is
// lost. Per artist/connection failures are collected and the batch
// continues; only artist paging and nil-wiring guards are hard returns.
func (p *Publisher) PrunePlatformBackdropDuplicates(ctx context.Context) (PlatformBackdropPruneResult, error) {
	if p == nil || p.artistService == nil || p.connectionService == nil || p.artistLister == nil {
		return PlatformBackdropPruneResult{}, fmt.Errorf("prune platform backdrop duplicates: publisher not fully wired")
	}
	var result PlatformBackdropPruneResult
	page := 1
	for {
		artists, _, err := p.artistLister.List(ctx, artist.ListParams{Page: page, PageSize: scanBackdropPageSize})
		if err != nil {
			return result, fmt.Errorf("listing artists at page %d: %w", page, err)
		}
		if len(artists) == 0 {
			break
		}
		for i := range artists {
			p.pruneOneArtist(ctx, &artists[i], &result)
		}
		if len(artists) < scanBackdropPageSize {
			break
		}
		page++
	}
	return result, nil
}

// pruneOneArtist detects and deletes redundant backdrops for one artist
// across its image-write-enabled platforms, updating result in place.
func (p *Publisher) pruneOneArtist(ctx context.Context, a *artist.Artist, result *PlatformBackdropPruneResult) {
	platformIDs, err := p.artistService.GetPlatformIDs(ctx, a.ID)
	if err != nil {
		result.Failures = append(result.Failures, PlatformBackdropPruneFailure{ArtistID: a.ID, Err: err.Error()})
		return
	}
	for _, pid := range platformIDs {
		conn, connErr := p.connectionService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			result.Failures = append(result.Failures, PlatformBackdropPruneFailure{ArtistID: a.ID, ConnectionID: pid.ConnectionID, Err: connErr.Error()})
			continue
		}
		if !conn.Enabled || conn.Status != "ok" || !conn.GetFeatureImageWrite() {
			continue
		}
		client := backdropPruneClientFactory(conn, p.logger)
		if client == nil {
			continue
		}
		redundant, _, detErr := backdropRedundantIndices(ctx, client, pid.PlatformArtistID)
		if detErr != nil {
			p.logger.Warn("platform backdrop prune: detection failed; skipping connection",
				slog.String("artist_id", a.ID), slog.String("connection", conn.Name), slog.String("error", detErr.Error()))
			result.Failures = append(result.Failures, PlatformBackdropPruneFailure{ArtistID: a.ID, ConnectionID: pid.ConnectionID, Err: detErr.Error()})
			continue
		}
		if len(redundant) == 0 {
			continue
		}
		removed := 0
		for _, rb := range redundant { // already descending by Index
			// Re-verify immediately before deleting: a concurrent platform
			// write between detection (hashing, above) and this delete could
			// have replaced the index's content. Re-fetch and re-hash; only
			// delete if the content still matches what detection hashed. A
			// skip here performs no delete, so lower indices are unaffected
			// and the connection continues (`continue`) -- this is distinct
			// from an actual delete error below, which leaves platform state
			// ambiguous and must `break`.
			data, _, ferr := client.GetArtistBackdrop(ctx, pid.PlatformArtistID, rb.Index)
			if ferr != nil {
				p.logger.Warn("platform backdrop prune: re-verify fetch failed; skipping delete",
					slog.String("artist_id", a.ID), slog.String("connection", conn.Name),
					slog.Int("index", rb.Index), slog.String("error", ferr.Error()))
				result.SkippedChanged++
				continue
			}
			if sha256.Sum256(data) != rb.Hash {
				p.logger.Warn("platform backdrop prune: backdrop content changed since detection; skipping delete to avoid deleting a now-unique image",
					slog.String("artist_id", a.ID), slog.String("connection", conn.Name),
					slog.Int("index", rb.Index))
				result.SkippedChanged++
				continue
			}
			if delErr := client.DeleteImageAtIndex(ctx, pid.PlatformArtistID, "fanart", rb.Index); delErr != nil {
				p.logger.Error("platform backdrop prune: delete failed",
					slog.String("artist_id", a.ID), slog.String("connection", conn.Name),
					slog.Int("index", rb.Index), slog.String("error", delErr.Error()))
				result.Failures = append(result.Failures, PlatformBackdropPruneFailure{ArtistID: a.ID, ConnectionID: pid.ConnectionID, Err: delErr.Error()})
				break // stop: later indices may have shifted after the failed delete
			}
			removed++
		}
		if removed > 0 {
			result.ArtistsProcessed++
			result.BackdropsRemoved += removed
			p.logger.Info("platform backdrops pruned",
				slog.String("artist_id", a.ID), slog.String("artist", a.Name),
				slog.String("connection", conn.Name), slog.Int("removed", removed))
		}
	}
}
