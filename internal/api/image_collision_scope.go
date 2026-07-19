package api

// image_collision_scope.go -- the API-side half of #2565: wiring the operator
// image-write chokepoints into the #2540 cross-artist backdrop collision seam
// that #2613 established.
//
// #2613 wired the platform IMPORT path (handlers_connection_library.go's
// downloadBackdrop). The two chokepoints here cover the OPERATOR upload,
// fetch-by-URL, and crop paths, but ONLY their append-next and overwrite-primary
// branches -- the same fanart slots those flows have always targeted, and so
// can pollute an artist with another artist's backdrop in exactly the same way.
// Everything they need already exists: image.CompareIdentity,
// artist.Service.BuildFanartIdentityIndex, and collision.Notifier. This file
// only packages them for reuse at those call sites.
//
// #2622 closed the remaining gap: the three SLOT-TARGETED writes
// (handleImageFetchFanartSlot and handleImageCropFanartSlot in handlers_image.go,
// and handleFanartSlotAssign in handlers_backdrop.go, which writes a PLATFORM
// backdrop straight into a fanart slot). They now share saveFanartSlotChecked
// below. That gap mattered because the cross_artist_backdrop_collision rule
// checker is a deliberate no-op (see engine.go's
// RuleCrossArtistBackdropCollision registration) -- detection happens ONLY at
// write chokepoints like this one. A write path that isn't wired here is never
// checked for collision: not late, not on the next rule sweep. Never.

import (
	"bytes"
	"context"
	"fmt"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/collision"
	img "github.com/sydlexius/stillwater/internal/image"
)

// imageWriteScope carries the request-scoped state a fanart write needs to run
// the #2540 cross-artist collision check: the DESTINATION artist (the check is
// meaningless without an id to exclude from the registry) and the identity
// index itself, built LAZILY and AT MOST ONCE per scope.
//
// Once-per-scope is not an optimization detail, it is the contract from
// design-2540.md section 4, and internal/artist/fanart_identity.go's doc comment
// names this guard as one of the two callers responsible for honoring it:
// BuildFanartIdentityIndex deliberately does no caching of its own because it is
// a WHOLE-LIBRARY scan. The batch fanart-append handler pushes up to 20 images
// through one request, so a per-image build would turn one library scan into
// twenty.
//
// A nil *imageWriteScope is a valid, fully safe no-op: every method below
// tolerates it and yields "no collision". That is the fail-open posture the
// whole seam takes, and it lets non-fanart and test call sites pass nil rather
// than assemble state the check will not use.
type imageWriteScope struct {
	r      *Router
	artist *artist.Artist

	// built records that an index build has been ATTEMPTED, so a failed build
	// (which legitimately yields a nil index) is not retried once per image.
	built bool
	idx   []img.FanartIdentityEntry
}

// newImageWriteScope builds the collision scope for a write targeting a. It
// returns nil when the collision seam is not wired (headless/test routers) or
// when there is no destination artist, so callers can hand the result straight
// through without a nil check of their own.
func (r *Router) newImageWriteScope(a *artist.Artist) *imageWriteScope {
	if r == nil || a == nil || r.collisionNotifier == nil || r.artistService == nil {
		return nil
	}
	return &imageWriteScope{r: r, artist: a}
}

// identityIndex returns the cross-artist fanart registry, building it on first
// use and reusing that result for every later image in this scope.
//
// A build failure degrades to a nil index -- which CompareIdentity reads as
// Indeterminate -- and is NOT retried. Failing to evaluate the check must never
// cost the operator their write, and re-attempting a failing whole-library scan
// once per image would turn a transient DB error into a pathological one.
func (s *imageWriteScope) identityIndex(ctx context.Context) []img.FanartIdentityEntry {
	if s == nil {
		return nil
	}
	if s.built {
		return s.idx
	}
	s.built = true

	idx, err := s.r.artistService.BuildFanartIdentityIndex(ctx)
	if err != nil {
		s.r.logger.Warn("building fanart identity index; skipping cross-artist collision check for this write",
			"artist", s.artist.Name, "error", err)
		return nil
	}
	s.idx = idx
	return s.idx
}

// collisionVerdict computes -- but deliberately does NOT act on -- the
// cross-artist collision verdict for the converted bytes about to be written.
// It returns nil when there is no collision to report.
//
// It is split from notifyCollision on purpose. The verdict has to be computed
// HERE, while the candidate bytes are in hand, but must not be ANNOUNCED until
// the write is confirmed; see notifyCollision.
//
// Fail-open at every step: no scope, an unhashable image, an empty registry, or
// a Match/Indeterminate verdict all return nil. Nothing in this function may
// prevent the caller's write.
func (s *imageWriteScope) collisionVerdict(ctx context.Context, converted []byte) *img.IdentityResult {
	if s == nil {
		return nil
	}
	idx := s.identityIndex(ctx)
	if len(idx) == 0 {
		return nil
	}

	// Hash the CONVERTED bytes -- what will actually land on disk -- so a format
	// conversion cannot make two copies of one picture look distinct. This
	// matches the import path (handlers_connection_library.go) exactly, and it
	// sidesteps raw-WebP decoding entirely: ConvertFormat special-cases WebP
	// (internal/image/processor.go), so converted bytes are always a format the
	// phash decoder handles.
	phash, err := img.PerceptualHash(bytes.NewReader(converted))
	if err != nil {
		s.r.logger.Debug("perceptual hash for cross-artist collision check failed; skipping check for this image",
			"artist", s.artist.Name, "error", err)
		return nil
	}
	if phash == 0 {
		// A zero hash is indistinguishable from "never hashed" and would
		// manufacture a collision against every unhashed entry.
		return nil
	}

	res := img.CompareIdentity(phash, s.artist.ID, idx, collision.DefaultTolerance)
	if res.Verdict != img.IdentityMismatch {
		return nil
	}
	return &res
}

// notifyCollision emits the #2540 notification for a verdict held from
// collisionVerdict. It MUST be called only after the write it describes has been
// CONFIRMED to have succeeded.
//
// That ordering is load-bearing, not stylistic. The durable half of the
// notification is a fixable Action Queue entry whose auto-fix BACKS ARTWORK OUT
// of the artist. Raising it for a write that then failed would point a
// destructive remediation at a file that was never created, and hand the
// operator a Fix button that acts on nothing.
//
// This is notify-only: it runs AFTER the write, never instead of it. Aliases and
// collaborations legitimately share promo art, so a hard block would
// false-positive; the operator-triggered back-out is the safety valve.
func (s *imageWriteScope) notifyCollision(ctx context.Context, res *img.IdentityResult) {
	if s == nil || res == nil {
		return
	}
	s.r.collisionNotifier.Notify(ctx, s.artist.ID, s.artist.Name, *res)
}

// saveFanartSlotChecked writes data into an EXPLICIT fanart slot with the #2540
// cross-artist collision check wired in (#2622). It is the slot-targeted peer of
// processAndAppendFanart (append-next) and processAndSaveImage (overwrite-primary),
// and it holds the same notify-after-confirmed-write ordering they do.
//
// ONE byte slice serves BOTH the hash and the write, and that is the entire point
// of routing all three slot writes through here rather than repeating the check at
// each of them. The three call sites do not agree on what bytes they write:
// handleFanartSlotAssign converts platform bytes first and writes the CONVERTED
// result, while handleImageFetchFanartSlot and handleImageCropFanartSlot write
// their bytes RAW. Taking the written bytes as the only input makes a divergence
// between "hashed" and "written" unrepresentable at the call sites.
//
// Honest scope of that guarantee: today it is HYGIENE, not a live bug fix.
// img.ConvertFormat is pixel-preserving in every branch (byte passthrough for
// non-WebP, lossless PNG re-encode for WebP) and a perceptual hash reads only the
// decoded pixels, so hashing a conversion of these bytes would currently produce
// the IDENTICAL verdict -- measured against a real lossy WebP, both sides hashed
// 0xe7cf8f9f3f3f7f7f. The single-slice shape is what keeps that true if a future
// step ever transforms pixels (a resize, a crop, an alpha trim) between the check
// and the write, which is exactly when a re-derived hash would start describing a
// file that was never written.
//
// Fail-open, notify-only: the verdict never gates the write. A save failure
// returns before notifyCollision, because the durable half of the notification
// carries an auto-fix that BACKS ARTWORK OUT of the artist -- see notifyCollision.
func (r *Router) saveFanartSlotChecked(ctx context.Context, scope *imageWriteScope, dir string, naming []string, data []byte, meta *img.ExifMeta) ([]string, error) {
	// Decided HERE, while the bytes are in hand, but HELD until the save confirms.
	collisionResult := scope.collisionVerdict(ctx, data)

	saved, err := r.saveFanartSlotProtected(ctx, dir, naming, data, meta)
	if err != nil {
		return nil, err
	}
	if len(saved) == 0 {
		// Save reported success but produced no file. Treat it as a failed write:
		// a back-out fix must never be armed on artwork that is not on disk.
		return nil, fmt.Errorf("saving fanart slot: produced no files in %s", dir)
	}

	// Write confirmed -- safe to announce.
	scope.notifyCollision(ctx, collisionResult)
	return saved, nil
}
