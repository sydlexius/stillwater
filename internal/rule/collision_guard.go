package rule

// collision_guard.go -- the #2540 cross-artist backdrop-collision seam for the
// two RULE-ENGINE write chokepoints (#2565): ImageFixer.downloadAndPersist
// (single-artist rule auto-fetch) and BulkExecutor.saveBestImage (library-wide
// bulk auto-fix).
//
// Before this file internal/rule had NO collision notifier wired into it at
// all -- the seam existed only in internal/api (import/populate) and
// internal/publish (outbound push). Both rule-package write paths are
// unattended and destructive (SaveImageFromData's own doc comment calls the
// bulk path out by name), so a misresolved MBID could spray another artist's
// backdrop across the library with nothing raised. This wires the seam once
// and both sites use it.
//
// It is NOTIFY-ONLY and FAIL-OPEN. Every error path here -- a failed index
// build, a failed decode, a failed hash -- yields "no verdict", which lets the
// write proceed exactly as it did before. A failure to EVALUATE a collision
// must never prevent an image from being saved.

import (
	"bytes"
	"context"
	"log/slog"

	"github.com/sydlexius/stillwater/internal/collision"
	img "github.com/sydlexius/stillwater/internal/image"
)

// backdropCollisionNotifier is the subset of *collision.Notifier the rule
// package needs. Declaring it as an interface here keeps the two call sites
// testable with a recording fake and documents that the guard never depends on
// anything but the one Notify call.
type backdropCollisionNotifier interface {
	Notify(ctx context.Context, destArtistID, destArtistName string, res img.IdentityResult)
}

// fanartIdentityIndexer is the subset of *artist.Service the guard needs. It is
// an interface rather than a concrete *artist.Service so tests can assert the
// once-per-scope build count and inject a build failure without a database.
type fanartIdentityIndexer interface {
	BuildFanartIdentityIndex(ctx context.Context) ([]img.FanartIdentityEntry, error)
}

// collisionGuard bundles the two collaborators the #2540 check needs. A nil
// *collisionGuard is a safe no-op at every method, so the rule-engine types can
// hold one unconditionally and the headless/test constructions that never call
// SetCollisionGuard behave exactly as they did before this change.
type collisionGuard struct {
	notifier backdropCollisionNotifier
	indexer  fanartIdentityIndexer
	logger   *slog.Logger
}

// newCollisionGuard builds a guard. It returns nil when either collaborator is
// missing, so "not wired" collapses to the same nil no-op as "no guard".
func newCollisionGuard(notifier backdropCollisionNotifier, indexer fanartIdentityIndexer, logger *slog.Logger) *collisionGuard {
	if notifier == nil || indexer == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &collisionGuard{notifier: notifier, indexer: indexer, logger: logger}
}

// active reports whether the guard should run for this image type. FANART ONLY:
// #2540 is about backdrop pollution, and BuildFanartIdentityIndex deliberately
// loads only fanart rows, so a thumb/logo/banner write has nothing to compare
// against.
func (g *collisionGuard) active(imageType string) bool {
	return g != nil && imageType == "fanart"
}

// buildIndex loads the cross-artist fanart comparison registry ONCE PER SCOPE.
// Callers must hoist this out of their candidate loop -- rebuilding it per
// candidate would re-scan the whole library on every download attempt.
//
// Fail-open: a build error returns a nil index, which makes verdict() return
// nil, which leaves the write untouched. The error is logged, never propagated.
func (g *collisionGuard) buildIndex(ctx context.Context) []img.FanartIdentityEntry {
	if g == nil {
		return nil
	}
	idx, err := g.indexer.BuildFanartIdentityIndex(ctx)
	if err != nil {
		g.logger.Warn("building fanart identity index for collision check; skipping the check",
			slog.String("error", err.Error()))
		return nil
	}
	return idx
}

// fanartIndex is a MUTABLE, scope-lifetime view of the cross-artist fanart
// registry. It exists so the bulk site can APPEND mid-job (see the BULK SCOPE
// DECISION in bulk_executor.go): the slice is passed down four call levels, so a
// bare []FanartIdentityEntry cannot carry an append back up to the job scope. A
// nil *fanartIndex is a safe no-op at every method.
//
// NOT SAFE FOR CONCURRENT USE, and it does not need to be: the only mutator is
// BulkExecutor.run's artist walk, a plain sequential for loop. The ArtistWorkers
// concurrency in this package belongs to Pipeline, which never touches this type.
type fanartIndex struct {
	entries []img.FanartIdentityEntry
}

// list returns the current entries for comparison. Nil-safe.
func (x *fanartIndex) list() []img.FanartIdentityEntry {
	if x == nil {
		return nil
	}
	return x.entries
}

// add records a fanart that THIS scope just wrote, so later writes in the same
// scope are compared against it. Callers must call this only after the write is
// CONFIRMED -- an entry for a save that then failed would poison every
// subsequent comparison in the job with artwork that is not on disk. A zero
// phash is dropped: it means the hash failed, and CompareIdentity treats a zero
// reference hash as unusable anyway.
func (x *fanartIndex) add(artistID string, phash uint64) {
	if x == nil || phash == 0 {
		return
	}
	x.entries = append(x.entries, img.FanartIdentityEntry{ArtistID: artistID, PHash: phash})
}

// verdictAndHash is verdict() that also hands back the perceptual hash it
// computed, so the bulk site can append the just-written fanart to its in-run
// index without hashing the same bytes twice on a library-wide unattended path.
//
// The hash is returned even when there is NO verdict, including when the index
// is EMPTY -- that case is load-bearing: a job against a library with no fanart
// starts empty, and seeding it from the job's own first write is what lets the
// job's SECOND colliding artist be detected. A zero hash means the bytes could
// not be hashed (fail-open: no verdict, nothing worth recording).
func (g *collisionGuard) verdictAndHash(destArtistID string, converted []byte, idx []img.FanartIdentityEntry) (*img.IdentityResult, uint64) {
	if g == nil {
		return nil, 0
	}
	phash, err := img.PerceptualHash(bytes.NewReader(converted))
	if err != nil {
		g.logger.Debug("perceptual hash for collision check failed; skipping the check",
			slog.String("artist_id", destArtistID), slog.String("error", err.Error()))
		return nil, 0
	}
	res := img.CompareIdentity(phash, destArtistID, idx, collision.DefaultTolerance)
	if res.Verdict != img.IdentityMismatch {
		return nil, phash
	}
	return &res, phash
}

// verdict computes the collision verdict for candidate bytes that are ABOUT TO
// BE WRITTEN, and returns non-nil ONLY on IdentityMismatch.
//
// The bytes passed here must be the CONVERTED bytes -- the ones that actually
// land on disk. img.ConvertFormat re-encodes WebP to PNG (processor.go: no WebP
// encoder is available) and passes JPEG/PNG through untouched, so hashing the
// raw download instead would hash a different byte stream than the file on
// disk for WebP sources.
//
// Fail-open at every step: an empty index, a decode/hash failure, or a
// Match/Indeterminate verdict all return nil.
func (g *collisionGuard) verdict(destArtistID string, converted []byte, idx []img.FanartIdentityEntry) *img.IdentityResult {
	if g == nil || len(idx) == 0 {
		return nil
	}
	res, _ := g.verdictAndHash(destArtistID, converted, idx)
	return res
}

// notify emits the collision notifications. It MUST be called only after the
// write it describes is CONFIRMED to have succeeded.
//
// The ordering is load-bearing, not stylistic. The durable half of the
// notification is a fixable Action Queue entry whose auto-fix BACKS ARTWORK OUT
// of the artist. Raising it for a save that then failed would aim a destructive
// remediation at a file that never existed. So the two call sites compute the
// verdict EARLY (while the converted bytes are in hand), hold it across the
// save, and call this only on the confirmed-success branch. This mirrors the
// reference implementation in internal/api/handlers_connection_library.go.
func (g *collisionGuard) notify(ctx context.Context, destArtistID, destArtistName string, res *img.IdentityResult) {
	if g == nil || res == nil {
		return
	}
	g.notifier.Notify(ctx, destArtistID, destArtistName, *res)
}
