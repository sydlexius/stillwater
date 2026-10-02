// Package publish -- backdrop_prune.go
// Platform-side backdrop de-duplication (#2540 remote prune). The local rule
// engine collapses byte-identical fanart on disk, but on Emby platform sync is
// additive (the per-file indexed upload never deletes surplus indices), so the
// copies already pushed there persist. A Jellyfin full-set sync replaces the
// artist's whole backdrop list with the local set (#3145), including any
// backdrop that exists only on the platform. This prunes them: content-
// matched (sha256), exact-only, admin-triggered.
//
// SCOPED and PREVIEWABLE (#3139). Every run names exactly one of a single
// artist or an explicit library-wide flag -- see PlatformBackdropPruneScope --
// so a forgotten scope cannot become a library-wide delete; and a DryRun
// returns the full per-group plan while deleting nothing.
package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/httpclient"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	img "github.com/sydlexius/stillwater/internal/image"
)

// errArtistAbsentOnPlatform signals that the platform gave a definitive
// "this artist does not exist" answer (a 404/410 on the artist-detail
// lookup) rather than an indeterminate read failure. Distinct from every
// other backdropRedundantIndices error, which still means "could not
// determine whether the artist has duplicates" and must fail closed.
var errArtistAbsentOnPlatform = errors.New("artist not present on platform")

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
// with the sha256 it had at detection time and the index of the surviving
// (kept) copy it duplicates. Both the hash and the survivor index are
// carried forward so the prune loop can re-verify, immediately before
// deleting, that NEITHER the candidate NOR the survivor's content has
// changed since detection (closing the TOCTOU window between hashing and
// delete: a concurrent platform write could otherwise replace either
// image's content -- if only the candidate were re-checked, a write that
// replaced the SURVIVOR's content would go undetected and the last
// remaining copy of the survivor's original image would be deleted).
type redundantBackdrop struct {
	Index    int
	Hash     [32]byte
	Survivor int
	// SurvivorHash is the sha256 the SURVIVOR had at detection. On the exact
	// tier it equals Hash by construction; on the perceptual tier the two
	// copies are deliberately NOT byte-identical, so re-verifying the survivor
	// against Hash would refuse every perceptual delete.
	SurvivorHash [32]byte
	// Tier is the evidence that authorized this delete: PruneTierExact or
	// PruneTierPerceptual.
	Tier string
}

// Prune tiers: which evidence authorized a planned delete (#3138).
const (
	// PruneTierExact: byte-identical (sha256) to the survivor. Nothing can be
	// lost, so this tier needs no survivor choice.
	PruneTierExact = "exact"
	// PruneTierPerceptual: the same picture by perceptual hash at the run's
	// tolerance, but not byte-identical. A similarity judgement, so it is
	// opt-in and its survivor is chosen deliberately.
	PruneTierPerceptual = "perceptual"
)

// dedupBackdropIndices returns the indices to delete: every index except the
// lowest in each byte-identical group, each carrying the hash it had at
// detection and the index of the group's surviving (lowest, kept) index.
// Sorted DESCENDING by Index so callers delete high-index-first
// (Emby/Jellyfin re-index the remaining backdrops after each delete, so a
// low-to-high delete order would shift later indices out from under the
// caller).
func dedupBackdropIndices(hashes [][32]byte) []redundantBackdrop {
	seen := make(map[[32]byte]int, len(hashes))
	var redundant []redundantBackdrop
	for i, h := range hashes {
		if survivor, ok := seen[h]; ok {
			redundant = append(redundant, redundantBackdrop{
				Index: i, Hash: h, Survivor: survivor, SurvivorHash: h, Tier: PruneTierExact,
			})
			continue
		}
		seen[h] = i
	}
	sortRedundantDescending(redundant)
	return redundant
}

// sortRedundantDescending enforces the delete-order contract: highest index
// first, because both peers renumber the remaining backdrops after each delete.
// Shared by both tiers and their union so they cannot drift apart on it.
//
// Descending order keeps every CANDIDATE stable until its turn. It says nothing
// about a SURVIVOR: the exact tier's survivor is its group's lowest index and
// so never moves, but a perceptual survivor can sit ABOVE its candidates, and
// the delete loop must shift it (see shiftAfterDelete).
func sortRedundantDescending(rs []redundantBackdrop) {
	sort.SliceStable(rs, func(a, b int) bool { return rs[a].Index > rs[b].Index })
}

// backdropFingerprint is what the perceptual tier learned about one platform
// backdrop from the single fetch of its bytes.
type backdropFingerprint struct {
	index   int
	content [32]byte
	phash   uint64
	area    int64 // width*height
	size    int   // encoded byte length
	// localTwin: byte-identical to a fanart file in the artist's local folder.
	localTwin bool
}

// fingerprintBackdrop derives the perceptual inputs from bytes already in
// memory: no extra round trip (#3138). Any decode failure is an ERROR, never a
// zero-valued fingerprint, so the caller can fail the whole tier closed rather
// than compare an image it could not read.
func fingerprintBackdrop(index int, data []byte, content [32]byte, localTwins map[[32]byte]bool) (backdropFingerprint, error) {
	w, h, err := img.GetDimensions(bytes.NewReader(data))
	if err != nil {
		return backdropFingerprint{}, err
	}
	if w <= 0 || h <= 0 {
		return backdropFingerprint{}, fmt.Errorf("non-positive dimensions %dx%d", w, h)
	}
	ph, err := img.PerceptualHash(bytes.NewReader(data))
	if err != nil {
		return backdropFingerprint{}, err
	}
	return backdropFingerprint{
		index: index, content: content, phash: ph,
		area: int64(w) * int64(h), size: len(data), localTwin: localTwins[content],
	}, nil
}

// preferSurvivor reports whether a should be KEPT over b when both are the same
// picture. THE SURVIVOR RULE (#3138), in order:
//
//  1. A copy byte-identical to a local fanart file. The local folder is
//     authoritative; it is the image the operator chose and the one the
//     reconciler checks the platform for, so deleting it would also be undone.
//  2. Larger pixel area: two copies of one picture differ mostly in resolution.
//  3. Larger byte size, at equal dimensions (less compression loss). This is
//     the measured Emby re-save case: same dimensions, a few bytes apart.
//  4. Lower index. Total and deterministic, so a re-run over the same platform
//     state builds the same plan.
//
// It deliberately does NOT prefer the lowest index first. That is the exact
// tier's rule, free there because the copies are identical, and here it would
// routinely keep a downscale and delete the better image.
func preferSurvivor(a, b backdropFingerprint) bool {
	if a.localTwin != b.localTwin {
		return a.localTwin
	}
	if a.area != b.area {
		return a.area > b.area
	}
	if a.size != b.size {
		return a.size > b.size
	}
	return a.index < b.index
}

// perceptualRedundant returns the perceptual tier's deletions among fps, at
// image.DefaultDuplicateTolerance -- the local image_duplicate rule's
// threshold and meaning, not a second number.
//
// Slots in claimed (already deleted by the exact tier) take no part. A slot
// whose perceptual hash is 0 also takes no part: Similarity(0, 0) is 1, so an
// unhashable-looking image would otherwise match every other one (the rule
// package excludes it for the same reason). Excluded means KEPT.
//
// Non-transitivity is handled by image.RepresentativeDeletionSet, the same
// representative walk the local rule uses, walked in preferSurvivor order so
// each cluster keeps its best copy. A local twin is never a deletion
// candidate, only a survivor: the platform should mirror the local folder, and
// a deleted twin would read to the reconciler as a missing local image and be
// re-pushed.
func perceptualRedundant(fps []backdropFingerprint, claimed map[int]bool) []redundantBackdrop {
	return perceptualRedundantAt(fps, claimed, img.DefaultDuplicateTolerance)
}

// perceptualRedundantAt is perceptualRedundant at an explicit tolerance.
func perceptualRedundantAt(fps []backdropFingerprint, claimed map[int]bool, tolerance float64) []redundantBackdrop {
	byIndex := make(map[int]backdropFingerprint, len(fps))
	cands := make([]backdropFingerprint, 0, len(fps))
	for _, f := range fps {
		if claimed[f.index] || f.phash == 0 {
			continue
		}
		byIndex[f.index] = f
		cands = append(cands, f)
	}
	sort.SliceStable(cands, func(a, b int) bool { return preferSurvivor(cands[a], cands[b]) })
	order := make([]int, len(cands))
	for i, f := range cands {
		order[i] = f.index
	}
	absorbed := img.RepresentativeDeletionSet(order, func(rep, member int) bool {
		m := byIndex[member]
		return !m.localTwin && img.Similarity(byIndex[rep].phash, m.phash) >= tolerance
	})
	out := make([]redundantBackdrop, 0, len(absorbed))
	for idx, survivor := range absorbed {
		out = append(out, redundantBackdrop{
			Index: idx, Hash: byIndex[idx].content,
			Survivor: survivor, SurvivorHash: byIndex[survivor].content,
			Tier: PruneTierPerceptual,
		})
	}
	// absorbed is a map, so out's order is random until sorted. Sorted here as
	// well as by the caller, so this function's own result honors the delete
	// order contract rather than relying on every caller to remember it.
	sortRedundantDescending(out)
	return out
}

// perceptualPruneOpts turns on the perceptual tier for one detection pass. The
// zero value is the exact-only behavior shipped in #2540.
type perceptualPruneOpts struct {
	Enabled bool
	// LocalTwins is the set of sha256 content hashes of the artist's local
	// fanart files (see preferSurvivor and perceptualRedundant).
	LocalTwins map[[32]byte]bool
	// Tolerance: same picture when Similarity >= it. Zero means the default;
	// PrunePlatformBackdropsForArtist validates any other value.
	Tolerance float64
}

func (o perceptualPruneOpts) tolerance() float64 {
	if o.Tolerance == 0 {
		return img.DefaultDuplicateTolerance
	}
	return o.Tolerance
}

// platformBackdropDup holds one artist/connection's detection result.
type platformBackdropDup struct {
	connID, connName string
	backdrops        int
	redundantCount   int
}

// backdropRedundantIndices reads every backdrop for the artist on the given
// connection, hashes it, and returns the redundant (deletable, descending by
// index) entries along with the total backdrop count. A non-404/410 read
// error for ANY index aborts the whole connection (returns an error) rather
// than risking a partial/blind result -- the caller counts this as a skipped
// scan, never a partial one. A 404/410 on the artist-detail lookup
// specifically is a third outcome: it returns an error that wraps
// errArtistAbsentOnPlatform (checkable via errors.Is) alongside the original
// status error, so callers can distinguish "artist definitively absent" from
// "could not determine" while the message still carries the original status
// text for diagnosability.
func backdropRedundantIndices(ctx context.Context, client backdropPruneClient, platformArtistID string) (redundant []redundantBackdrop, total int, err error) {
	redundant, total, _, err = detectBackdropRedundancy(ctx, client, platformArtistID, perceptualPruneOpts{})
	return redundant, total, err
}

// detectBackdropRedundancy is backdropRedundantIndices with the perceptual tier
// available. The perceptual hash is computed from the same bytes the exact tier
// already fetches, so enabling it costs a decode per backdrop, not a request.
//
// FAIL CLOSED, per tier. A fetch error still aborts the whole connection (err),
// exactly as before. A backdrop that was fetched but could not be decoded turns
// the perceptual tier OFF for this connection and is reported in
// perceptualErr, while the exact tier's result -- which needs no decode -- is
// still returned. A judgement made with one image unreadable could delete the
// copy that unreadable image was the better twin of.
func detectBackdropRedundancy(ctx context.Context, client backdropPruneClient, platformArtistID string, opts perceptualPruneOpts) (redundant []redundantBackdrop, total int, perceptualErr, err error) {
	detail, err := client.GetArtistDetail(ctx, platformArtistID)
	if err != nil {
		var statusErr *httpclient.StatusError
		if errors.As(err, &statusErr) && statusErr.IsNotFound() {
			return nil, 0, nil, fmt.Errorf("fetching artist detail: %w: %w", errArtistAbsentOnPlatform, err)
		}
		return nil, 0, nil, fmt.Errorf("fetching artist detail: %w", err)
	}
	count := detail.BackdropCount
	if count <= 1 {
		return nil, count, nil, nil
	}
	hashes := make([][32]byte, 0, count)
	var fps []backdropFingerprint
	for i := 0; i < count; i++ {
		data, _, fErr := client.GetArtistBackdrop(ctx, platformArtistID, i)
		if fErr != nil {
			return nil, 0, nil, fmt.Errorf("fetching backdrop %d: %w", i, fErr)
		}
		content := sha256.Sum256(data)
		hashes = append(hashes, content)
		if opts.Enabled && perceptualErr == nil {
			fp, fpErr := fingerprintBackdrop(i, data, content, opts.LocalTwins)
			if fpErr != nil {
				perceptualErr = fmt.Errorf("perceptual tier skipped: backdrop %d could not be decoded: %w", i, fpErr)
				continue
			}
			fps = append(fps, fp)
		}
	}
	redundant = dedupBackdropIndices(hashes)
	if !opts.Enabled || perceptualErr != nil {
		return redundant, count, perceptualErr, nil
	}
	claimed := make(map[int]bool, len(redundant))
	for _, rb := range redundant {
		claimed[rb.Index] = true
	}
	redundant = append(redundant, perceptualRedundantAt(fps, claimed, opts.tolerance())...)
	sortRedundantDescending(redundant)
	return redundant, count, nil, nil
}

// connAbsenceTally accumulates, per connection, how many artists carried a
// mapped platform ID and how many of those turned out to be absent on the
// platform. It exists because the systemic case can only be recognized ACROSS
// a whole sweep: detectArtistPlatformDups sees one artist at a time, and a
// single 404 is ordinary (#2692's F1 established that an absent artist is not
// a scan error).
//
// What it bounds is the hole F1 opened. When EVERY artist on a connection is
// absent, the scan reports ScanErrors 0, the cache gate treats the result as
// authoritative, and it caches zero duplicate counts -- so a stale
// PlatformUserID renders as a verified-clean library. Connection.Status cannot
// catch that: it is stored at connection-test time from /System/Info, which
// never exercises the user ID.
//
// IMPORTANT, and worth stating plainly in review: this does NOT detect a
// misconfiguration. "Every artist is genuinely absent" and "the connection is
// misconfigured" emit an identical stream of per-artist 404s, and nothing
// inside a single scan can separate them. So this treats the AMBIGUITY the way
// the existing partial-scan guard already treats "unreachable" -- it fails
// closed, preferring a stale-but-true cached count over a confident zero.
type connAbsenceTally struct {
	mapped int
	absent int
}

// detectArtistPlatformDups reads every backdrop for the artist on each of its
// enabled, healthy connections and returns the redundant (deletable) indices
// per connection. A read error for any backdrop skips that whole connection
// (counted via scanErrs) rather than risking a partial delete.
//
// tally, when non-nil, accumulates the per-connection mapped/absent counts the
// caller needs for the systemic-absence bound above. It is an optional
// out-parameter rather than a third return value so the six existing direct
// call sites -- which test this function in isolation and have no sweep to
// accumulate across -- keep compiling unchanged.
func (p *Publisher) detectArtistPlatformDups(ctx context.Context, artistID string, tally map[string]*connAbsenceTally) (dups []platformBackdropDup, scanErrs int) {
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
		// Counted here, AFTER the connection is known usable, so a disabled or
		// unhealthy connection never contributes to the ratio. Counting earlier
		// would let a connection that was skipped entirely read as "mapped but
		// never resolved" and fire the systemic bound on a healthy sweep.
		if tally != nil {
			t := tally[pid.ConnectionID]
			if t == nil {
				t = &connAbsenceTally{}
				tally[pid.ConnectionID] = t
			}
			t.mapped++
		}
		redundant, count, detErr := backdropRedundantIndices(ctx, client, pid.PlatformArtistID)
		if detErr != nil {
			if errors.Is(detErr, errArtistAbsentOnPlatform) {
				p.logger.Warn("platform backdrop scan: artist not present on platform; skipping",
					slog.String("artist_id", artistID), slog.String("connection", conn.Name))
				if tally != nil {
					tally[pid.ConnectionID].absent++
				}
				continue
			}
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
	// Accumulated across the WHOLE sweep, not per page and not per artist: the
	// systemic-absence signal only exists in aggregate. See connAbsenceTally.
	absence := make(map[string]*connAbsenceTally)
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
			dups, scanErrs := p.detectArtistPlatformDups(ctx, a.ID, absence)
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
	// Systemic absence: a connection where every mapped artist resolved to
	// "not present" saw no transport error, so without this the sweep reports
	// ScanErrors 0, the cache gate accepts it as authoritative, and a stale
	// PlatformUserID caches as a verified-clean library (#2692).
	//
	// The condition is deliberately narrow. It fires ONLY on the pure-absent
	// case: a connection carrying a mix of absent artists and genuine read
	// errors already fails closed through the existing scanErrs path, so
	// widening this to `absent > 0` would double-count that population and
	// would also fire on the ordinary case of one artist legitimately missing.
	//
	// No `mapped > 0` guard, deliberately: an entry is created only on the line
	// immediately before its first mapped++, so a zero-mapped entry cannot
	// exist and such a guard would be unreachable code dressed as a safety
	// check. If that construction ever moves, restore the guard -- `absent ==
	// mapped` would otherwise be trivially true (0 == 0) for a phantom entry
	// and would report a scan error for a connection nobody mapped.
	for connID, t := range absence {
		if t.absent == t.mapped {
			report.ScanErrors++
			p.logger.Warn("platform backdrop scan: every mapped artist absent on this connection; "+
				"treating the sweep as partial so cached counts are not cleared",
				slog.String("connection_id", connID), slog.Int("mapped", t.mapped))
		}
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
	// Skipped: the perceptual tier was declined by POLICY (the exact tier still
	// ran), so a caller can tell that from a failure. For API compatibility each
	// cause but the locked artist is ALSO still in Failures, as before.
	Skipped []PlatformBackdropPruneSkip
	// DryRun echoes the scope's DryRun so a caller cannot mistake a rehearsal
	// for a run that actually deleted. On a dry run BackdropsRemoved stays 0
	// and Plan carries what WOULD have been deleted.
	DryRun bool
	// Plan is the per-deletion detail: which index goes, which survives it,
	// and -- once the run is over -- what actually became of it. Populated on
	// both dry and live runs: a plan is worth recording after the fact too.
	Plan []PlatformBackdropPrunePlanEntry
}

// PlatformBackdropPruneSkip is one perceptual tier skipped by policy.
// ConnectionID is empty when the skip covers every connection of the artist.
type PlatformBackdropPruneSkip struct {
	ArtistID, ConnectionID string
	Reason                 string // a PruneSkip* constant
}

// Policy-skip reasons for PlatformBackdropPruneSkip.Reason.
const (
	PruneSkipLockedArtist     = "locked_artist"
	PruneSkipProtectedFanart  = "protected_fanart"
	PruneSkipNoLocalFolder    = "no_local_folder"
	PruneSkipEmptyLocalFolder = "empty_local_folder"
	PruneSkipUnreadableLocal  = "unreadable_local_fanart"
)

// pruneSkipError is a perceptualOptsFor refusal by policy, not a read failure.
type pruneSkipError struct{ reason, msg string }

func (e *pruneSkipError) Error() string { return e.msg }

// PlatformBackdropPruneScope narrows a prune run.
//
// EXACTLY ONE of ArtistID or AllArtists is required, and a run carrying
// NEITHER is refused rather than defaulting to the whole library (#3139). The
// precedent is PHashMismatchScope on the sibling destructive endpoint, and the
// reason is the same: a forgotten scope must not become a library-wide delete.
// Enforced in the publisher as well as the handler, so the guarantee does not
// depend on which caller reached it.
type PlatformBackdropPruneScope struct {
	// ArtistID scopes the run to one artist.
	ArtistID string
	// AllArtists must be set explicitly to page the whole library.
	AllArtists bool
	// Perceptual adds the perceptual tier (#3138): backdrops that are the same
	// picture without being byte-identical, kept by the survivor rule in
	// preferSurvivor. Off by default; the exact tier always runs. Skipped for a
	// locked artist.
	Perceptual bool
	// DryRun computes and returns the full plan without deleting anything.
	// It is the rehearsal that makes an irreversible sweep validatable: the
	// operator sees which index survives each group BEFORE any artwork is
	// removed, and no cache is invalidated because nothing changed.
	DryRun bool
	// tolerance: zero (every API-path scope) means the default. Only
	// PrunePlatformBackdropsForArtist sets it, after validating it.
	tolerance float64
}

// The scope-validation sentinels. Exported and distinguishable (via
// errors.Is) so an HTTP layer can map each to its OWN fixed, client-safe
// message rather than echoing the error's text -- raw error text on a response
// is how internals leak.
var (
	// ErrPruneScopeMissing: neither ArtistID nor AllArtists was set.
	ErrPruneScopeMissing = errors.New("an artist scope is required; set AllArtists to run library-wide")
	// ErrPruneScopeAmbiguous: both were set.
	ErrPruneScopeAmbiguous = errors.New("scope is mutually exclusive: set exactly one of ArtistID or AllArtists")
)

// Validate enforces the scope invariants. Every failure is an error, never a
// silent normalization: on a path that deletes artwork, an unusable parameter
// is a refusal, not a nudge back to a default nobody asked for.
//
// Exported so an HTTP layer can reject a bad scope with a 400 before claiming
// the singleton, WITHOUT that being the only place the rule holds:
// PrunePlatformBackdropDuplicates calls it again itself.
func (s PlatformBackdropPruneScope) Validate() error {
	if s.ArtistID == "" && !s.AllArtists {
		return ErrPruneScopeMissing
	}
	if s.ArtistID != "" && s.AllArtists {
		return ErrPruneScopeAmbiguous
	}
	return nil
}

// Plan-entry outcomes. Each entry records what ACTUALLY happened to it, so a
// plan can never drift from the run that produced it.
//
// Reporting a plan of three next to a removed-count of one gives an operator
// two numbers and no way to tell which describes their library. These are
// written by the same loop that performs the work, so they are evidence
// rather than prediction.
const (
	// PrunePlanPlanned: the entry was computed but no delete was attempted.
	// The only outcome a DRY RUN can produce, and what a live entry keeps if
	// an earlier failure stopped the connection before reaching it.
	PrunePlanPlanned = "planned"
	// PrunePlanDeleted: the platform confirmed the delete.
	PrunePlanDeleted = "deleted"
	// PrunePlanSkipped: a pre-delete re-verify refused it (the candidate or
	// its survivor changed since detection). Nothing was deleted.
	PrunePlanSkipped = "skipped"
	// PrunePlanFailed: the delete was attempted and the platform returned an
	// error, leaving that connection's remaining entries unattempted.
	PrunePlanFailed = "failed"
)

// PlatformBackdropPrunePlanEntry is one planned deletion: which index goes,
// which survives, and what became of it.
//
// Index and Survivor are the slots AT DETECTION TIME, which is what a dry run
// showed the operator. On the exact tier they need no post-delete correction:
// the survivor is always the LOWEST index of its byte-identical group (see
// dedupBackdropIndices), so it sits below every candidate and the
// high-index-first delete order never renumbers it. A perceptual survivor can
// sit ABOVE its candidates; the delete loop tracks its moving slot internally
// (shiftAfterDelete) and this record keeps the detection-time numbers.
type PlatformBackdropPrunePlanEntry struct {
	ArtistID     string
	ConnectionID string
	Index        int
	Survivor     int
	// Tier is PruneTierExact or PruneTierPerceptual: which evidence the delete
	// rests on, so an operator reading a dry run can tell them apart.
	Tier string
	// Outcome is one of the PrunePlan* constants above.
	Outcome string
}

// PrunePlatformBackdropDuplicates deletes byte-identical redundant backdrops
// on connected, image-write-enabled platforms, high-index-first, WITHIN THE
// GIVEN SCOPE.
//
// Re-detects from the platform on every call (does not trust a prior scan
// result, which may be stale by the time an operator triggers the prune).
// Exact-only: a deleted copy is byte-identical to a kept one, so nothing is
// lost. Per artist/connection failures are collected and the batch
// continues; scope validation, artist paging, and nil-wiring guards are hard
// returns.
//
// scope.Validate is called HERE as well as at the handler, so the either/or
// requirement holds for every caller rather than for one door (#3139).
func (p *Publisher) PrunePlatformBackdropDuplicates(ctx context.Context, scope PlatformBackdropPruneScope) (PlatformBackdropPruneResult, error) {
	// Stamped BEFORE the wiring and scope guards below, not after. DryRun is
	// documented as an echo of the request, and the handler serializes it onto
	// the 500 body via platformPruneResponse -- so a zero-valued early return
	// would answer a dry_run=true request with "dry_run": false, telling an
	// operator their rehearsal was a live run at the one moment they are
	// already being told something went wrong.
	var result PlatformBackdropPruneResult
	result.DryRun = scope.DryRun

	if p == nil || p.artistService == nil || p.connectionService == nil || p.artistLister == nil {
		return result, fmt.Errorf("prune platform backdrop duplicates: publisher not fully wired")
	}
	if err := scope.Validate(); err != nil {
		return result, fmt.Errorf("prune platform backdrop duplicates: %w", err)
	}

	if scope.ArtistID != "" {
		if p.artistGetter == nil {
			return result, fmt.Errorf("prune platform backdrop duplicates: artist lookup not wired; cannot resolve a scoped artist")
		}
		// The prune reads base columns only (ID, Name, Locked, Path): no side-table hydration.
		a, err := p.artistGetter.GetByID(ctx, scope.ArtistID, artist.HydrateOpts{})
		if err != nil {
			return result, fmt.Errorf("prune platform backdrop duplicates: loading artist %s: %w", scope.ArtistID, err)
		}
		// A scoped run whose artist cannot be resolved FAILS rather than
		// falling through to the library-wide loop below -- the same
		// fail-closed direction as a missing scope.
		if a == nil {
			return result, fmt.Errorf("prune platform backdrop duplicates: artist %s not found", scope.ArtistID)
		}
		p.pruneOneArtist(ctx, a, scope, &result)
		return result, nil
	}

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
			p.pruneOneArtist(ctx, &artists[i], scope, &result)
		}
		if len(artists) < scanBackdropPageSize {
			break
		}
		page++
	}
	return result, nil
}

// ArtistBackdropPruneOptions configures PrunePlatformBackdropsForArtist.
type ArtistBackdropPruneOptions struct {
	Perceptual, DryRun bool // as on PlatformBackdropPruneScope
	// Tolerance, in (0, 1], is required: zero is refused, not defaulted, so a
	// caller that forgot its configured value fails loudly.
	Tolerance float64
}

// ErrPruneToleranceInvalid is returned for a tolerance that is NaN, <= 0 or > 1.
var ErrPruneToleranceInvalid = errors.New("prune tolerance must be in (0, 1]")

// PrunePlatformBackdropsForArtist prunes one artist (the "No duplicate images"
// rule, #3138) through pruneOneArtist, so every API-path invariant holds:
// exact-only when locked, the fail-closed skips, lockPhashTarget, re-verify,
// preferSurvivor, descending deletes. Only a.ID is trusted: the artist is
// re-loaded first, as the scoped API path does, so a lock set after the
// caller's snapshot still holds.
//
// Concurrency: the API singleton lives on the Router and is not taken here.
// lockPhashTarget serializes each target's detect-to-delete, so a concurrent
// API prune and rule fix each re-detect after the other; a publisher-wide
// singleton would just fail a rule fix during an unrelated library prune.
func (p *Publisher) PrunePlatformBackdropsForArtist(ctx context.Context, a *artist.Artist, opts ArtistBackdropPruneOptions) (PlatformBackdropPruneResult, error) {
	result := PlatformBackdropPruneResult{DryRun: opts.DryRun}
	if t := opts.Tolerance; math.IsNaN(t) || t <= 0 || t > 1 { // as image's validTolerance
		return result, fmt.Errorf("prune platform backdrops for artist: %w (got %v)", ErrPruneToleranceInvalid, opts.Tolerance)
	}
	if p == nil || p.artistService == nil || p.connectionService == nil || p.artistGetter == nil {
		return result, fmt.Errorf("prune platform backdrops for artist: publisher not fully wired")
	}
	if a == nil || a.ID == "" {
		return result, fmt.Errorf("prune platform backdrops for artist: artist is required")
	}
	// The prune reads base columns only (ID, Name, Locked, Path): no side-table hydration.
	fresh, err := p.artistGetter.GetByID(ctx, a.ID, artist.HydrateOpts{})
	if err != nil {
		return result, fmt.Errorf("prune platform backdrops for artist: loading artist %s: %w", a.ID, err)
	}
	if fresh == nil {
		return result, fmt.Errorf("prune platform backdrops for artist: artist %s not found", a.ID)
	}
	scope := PlatformBackdropPruneScope{ArtistID: fresh.ID, Perceptual: opts.Perceptual, DryRun: opts.DryRun, tolerance: opts.Tolerance}
	p.pruneOneArtist(ctx, fresh, scope, &result)
	return result, nil
}

// verifyBackdropUnchanged re-fetches the backdrop at index and confirms it
// still hashes to want, returning a descriptive error otherwise (either a
// fetch failure or a content change) suitable for direct logging by the
// caller. Factored out of pruneOneArtist's delete loop so that loop can
// re-verify both the candidate and its surviving counterpart without
// duplicating the fetch/hash/compare branches inline.
func verifyBackdropUnchanged(ctx context.Context, client backdropPruneClient, platformArtistID string, index int, want [32]byte) error {
	data, _, err := client.GetArtistBackdrop(ctx, platformArtistID, index)
	if err != nil {
		return fmt.Errorf("re-verify fetch failed: %w", err)
	}
	if sha256.Sum256(data) != want {
		return fmt.Errorf("content changed since detection")
	}
	return nil
}

// shiftAfterDelete rewrites the pending entries' slots after the platform
// removed deletedIndex and renumbered every slot above it down by one. Measured
// on Emby 4.9.5.0 only; that Jellyfin renumbers the same way is ASSUMED, not
// measured. If a peer did not renumber, the shifted re-verify would read a slot
// whose bytes no longer match the recorded hash, so the entry is SKIPPED rather
// than the wrong slot deleted -- the failure mode is a short run, not data loss.
//
// The SURVIVOR half is the one that matters. Descending order already keeps
// every candidate in place until its turn, but a perceptual survivor can sit
// above the slot just deleted; unshifted, its re-verify reads a neighbor (or
// off the end), fails, and the rest of the cluster is skipped as if it had been
// concurrently modified. That was the earlier branch's DO NOT SHIP defect.
func shiftAfterDelete(rs []redundantBackdrop, deletedIndex int) {
	for i := range rs {
		if rs[i].Index > deletedIndex {
			rs[i].Index--
		}
		if rs[i].Survivor > deletedIndex {
			rs[i].Survivor--
		}
	}
}

// perceptualOptsFor builds the perceptual tier's options for one artist, or
// reports why the tier must stay off; every doubt fails it CLOSED. Protected
// fanart (#2533) skips the whole artist: platform indices do not map to local
// slots, and a re-encoded user image is no sha256 twin. No local folder, an
// empty one, or an unhashable file leaves the local twin set (the sha256 of
// every local fanart file) unknown.
func (p *Publisher) perceptualOptsFor(ctx context.Context, a *artist.Artist) (perceptualPruneOpts, error) {
	if err := p.checkNoProtectedFanart(ctx, a.ID); err != nil {
		return perceptualPruneOpts{}, err
	}
	twins := make(map[[32]byte]bool)
	dir := p.ImageDir(a)
	if dir == "" {
		return perceptualPruneOpts{}, &pruneSkipError{PruneSkipNoLocalFolder, "perceptual tier skipped: artist has no local image folder"}
	}
	paths, err := img.DiscoverFanart(ctx, dir, p.getActiveFanartPrimary(ctx))
	if err != nil {
		return perceptualPruneOpts{}, fmt.Errorf("perceptual tier skipped: listing local fanart: %w", err)
	}
	// An artist with a folder but NO fanart in it is indistinguishable from a
	// folder on a mount that is down. Treating it as "no local twins" would
	// strip the twin protection exactly when the local copy cannot be seen, so
	// it is treated as unknown and the tier is skipped.
	if len(paths) == 0 {
		return perceptualPruneOpts{}, &pruneSkipError{PruneSkipEmptyLocalFolder, "perceptual tier skipped: no local fanart found in the artist folder"}
	}
	local, unreadable := p.localFanartHashes(ctx, paths)
	if unreadable > 0 {
		return perceptualPruneOpts{}, &pruneSkipError{PruneSkipUnreadableLocal, fmt.Sprintf("perceptual tier skipped: %d local fanart file(s) could not be hashed", unreadable)}
	}
	for h := range local {
		var sum [32]byte
		if b, decErr := hex.DecodeString(h); decErr == nil && len(b) == len(sum) {
			copy(sum[:], b)
			twins[sum] = true
		}
	}
	return perceptualPruneOpts{Enabled: true, LocalTwins: twins}, nil
}

// checkNoProtectedFanart errors when any fanart row is locked or user-set, or
// when that cannot be read.
func (p *Publisher) checkNoProtectedFanart(ctx context.Context, artistID string) error {
	if p.artistImages == nil {
		p.logger.Error("platform backdrop prune: artist image reader not wired; perceptual tier unavailable")
		return fmt.Errorf("perceptual tier skipped: fanart protection state unavailable")
	}
	imgs, err := p.artistImages.GetImagesForArtist(ctx, artistID)
	if err != nil {
		return fmt.Errorf("perceptual tier skipped: reading fanart protection state: %w", err)
	}
	for i := range imgs {
		if im := &imgs[i]; im.ImageType == "fanart" && (im.Locked || im.Source == artist.ImageSourceUser) {
			return &pruneSkipError{PruneSkipProtectedFanart, "perceptual tier skipped: artist has locked or user-set fanart"}
		}
	}
	return nil
}

// pruneOneArtist detects and deletes redundant backdrops for one artist
// across its image-write-enabled platforms, updating result in place.
func (p *Publisher) pruneOneArtist(ctx context.Context, a *artist.Artist, scope PlatformBackdropPruneScope, result *PlatformBackdropPruneResult) {
	platformIDs, err := p.artistService.GetPlatformIDs(ctx, a.ID)
	if err != nil {
		result.Failures = append(result.Failures, PlatformBackdropPruneFailure{ArtistID: a.ID, Err: err.Error()})
		return
	}
	// A locked artist's artwork is operator-owned: the perceptual tier, which
	// is a similarity judgement, never touches it. The exact tier still runs --
	// it removes only byte-identical copies, so nothing the operator chose is
	// lost.
	var opts perceptualPruneOpts
	if scope.Perceptual && a.Locked && len(platformIDs) > 0 {
		// Skipped only: the API never reported a locked artist in Failures.
		result.Skipped = append(result.Skipped, PlatformBackdropPruneSkip{ArtistID: a.ID, Reason: PruneSkipLockedArtist})
	}
	if scope.Perceptual && !a.Locked && len(platformIDs) > 0 {
		var optsErr error
		if opts, optsErr = p.perceptualOptsFor(ctx, a); optsErr != nil {
			p.logger.Warn("platform backdrop prune: perceptual tier skipped for artist",
				slog.String("artist_id", a.ID), slog.String("error", optsErr.Error()))
			result.Failures = append(result.Failures, PlatformBackdropPruneFailure{ArtistID: a.ID, Err: optsErr.Error()})
			var skip *pruneSkipError
			if errors.As(optsErr, &skip) {
				result.Skipped = append(result.Skipped, PlatformBackdropPruneSkip{ArtistID: a.ID, Reason: skip.reason})
			}
		}
		opts.Tolerance = scope.tolerance
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
		p.pruneOneTarget(ctx, a, conn, pid, client, scope, opts, result)
	}
}

// pruneOneTarget runs detect, plan and delete for one artist on one connection
// under lockPhashTarget (#3316), which every Stillwater indexed backdrop writer
// also holds: the polluted-backdrop delete and restore, uploadFanartForSync,
// both branches of pushFanartSetToPeer, and the push handler (via
// LockBackdropTarget). A write by the platform itself is outside it, so each
// delete still re-verifies. Detect-to-delete is one read-modify-verify, and the
// perceptual tier's per-backdrop decode widens the window a concurrent resync
// could otherwise interleave with. Re-entrancy: no caller of pruneOneArtist
// (the prune handler, PrunePlatformBackdropsForArtist) holds the lock, and
// nothing below takes it.
func (p *Publisher) pruneOneTarget(ctx context.Context, a *artist.Artist, conn *connection.Connection, pid artist.PlatformID, client backdropPruneClient, scope PlatformBackdropPruneScope, opts perceptualPruneOpts, result *PlatformBackdropPruneResult) {
	unlock := p.lockPhashTarget(pid.ConnectionID, pid.PlatformArtistID)
	defer unlock()
	redundant, _, perceptualErr, detErr := detectBackdropRedundancy(ctx, client, pid.PlatformArtistID, opts)
	if perceptualErr != nil {
		p.logger.Warn("platform backdrop prune: perceptual tier skipped for connection",
			slog.String("artist_id", a.ID), slog.String("connection", conn.Name), slog.String("error", perceptualErr.Error()))
		result.Failures = append(result.Failures, PlatformBackdropPruneFailure{ArtistID: a.ID, ConnectionID: pid.ConnectionID, Err: perceptualErr.Error()})
	}
	if detErr != nil {
		p.logger.Warn("platform backdrop prune: detection failed; skipping connection",
			slog.String("artist_id", a.ID), slog.String("connection", conn.Name), slog.String("error", detErr.Error()))
		result.Failures = append(result.Failures, PlatformBackdropPruneFailure{ArtistID: a.ID, ConnectionID: pid.ConnectionID, Err: detErr.Error()})
		return
	}
	if len(redundant) == 0 {
		return
	}
	// The plan is recorded from the DETECTION-TIME numbering, which is
	// what a dry run shows the operator, and each entry's Outcome is
	// filled in below by the loop that actually does the work -- so the
	// plan cannot drift from what happened. planBase is where this
	// connection's entries start, so the loop can address its own entries
	// without a second pass or a parallel slice.
	planBase := len(result.Plan)
	for _, rb := range redundant {
		result.Plan = append(result.Plan, PlatformBackdropPrunePlanEntry{
			ArtistID: a.ID, ConnectionID: pid.ConnectionID,
			Index: rb.Index, Survivor: rb.Survivor, Tier: rb.Tier,
			Outcome: PrunePlanPlanned,
		})
	}
	if scope.DryRun {
		// Rehearsal: the plan above is the entire deliverable, and every
		// entry keeps the "planned" outcome so a dry run can never be read
		// as having deleted anything. Nothing is re-fetched and nothing is
		// deleted, so ArtistsProcessed and BackdropsRemoved stay at zero
		// and no cache needs invalidating.
		return
	}
	removed := 0
	// live is the loop's mutable copy: the platform renumbers after every
	// confirmed delete, so detection-time slots stop being true mid-loop.
	// result.Plan keeps the detection-time numbering the operator saw.
	live := append([]redundantBackdrop(nil), redundant...)
	for i := range live { // already descending by Index
		rb := live[i]
		entry := &result.Plan[planBase+i]
		// Re-verify immediately before deleting: a concurrent platform
		// write between detection (hashing, above) and this delete could
		// have replaced the index's content. Re-check BOTH the candidate
		// (about to be deleted) and its surviving (kept) counterpart;
		// only delete if BOTH still match what detection hashed.
		// Checking only the candidate would miss a write that instead
		// replaced the SURVIVOR's content -- the candidate would still
		// look redundant and deleting it would then destroy the last
		// remaining copy of the survivor's original image. A skip here
		// performs no delete, so lower indices are unaffected and the
		// connection continues (`continue`) -- this is distinct from an
		// actual delete error below, which leaves platform state
		// ambiguous and must `break`.
		if verr := verifyBackdropUnchanged(ctx, client, pid.PlatformArtistID, rb.Index, rb.Hash); verr != nil {
			p.logger.Warn("platform backdrop prune: candidate re-verify failed; skipping delete",
				slog.String("artist_id", a.ID), slog.String("connection", conn.Name),
				slog.Int("index", rb.Index), slog.String("error", verr.Error()))
			result.SkippedChanged++
			entry.Outcome = PrunePlanSkipped
			continue
		}
		if verr := verifyBackdropUnchanged(ctx, client, pid.PlatformArtistID, rb.Survivor, rb.SurvivorHash); verr != nil {
			p.logger.Warn("platform backdrop prune: survivor re-verify failed; skipping delete to avoid deleting its last remaining copy",
				slog.String("artist_id", a.ID), slog.String("connection", conn.Name),
				slog.Int("index", rb.Index), slog.Int("survivor_index", rb.Survivor), slog.String("error", verr.Error()))
			result.SkippedChanged++
			entry.Outcome = PrunePlanSkipped
			continue
		}
		if delErr := client.DeleteImageAtIndex(ctx, pid.PlatformArtistID, "fanart", rb.Index); delErr != nil {
			p.logger.Error("platform backdrop prune: delete failed",
				slog.String("artist_id", a.ID), slog.String("connection", conn.Name),
				slog.Int("index", rb.Index), slog.String("error", delErr.Error()))
			result.Failures = append(result.Failures, PlatformBackdropPruneFailure{ArtistID: a.ID, ConnectionID: pid.ConnectionID, Err: delErr.Error()})
			entry.Outcome = PrunePlanFailed
			break // stop: later indices may have shifted after the failed delete
		}
		shiftAfterDelete(live[i+1:], rb.Index)
		entry.Outcome = PrunePlanDeleted
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
