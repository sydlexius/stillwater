package image

import "math"

// identity.go -- the shared comparison primitive behind #2540's write-time
// (#2565) and outbound (#2566) phash-identity notifiers.
//
// This mirrors the semantics of the read-only #2564 detector
// (internal/rule/phash_mismatch.go: usablePHash, bestCrossArtistMatch)
// exactly, but stays dependency-free (no import of internal/artist or
// internal/rule) so both the write path (internal/rule, internal/api) and
// the publish path (internal/publish) can share it without an import
// cycle. It answers only "does this candidate phash collide with some
// OTHER artist's fanart", never who owns the picture -- see the detector's
// package doc for why the signal is fanart-to-fanart, not fanart-to-thumb.
//
// # These guards NEVER block, only notify
//
// This primitive itself has no notion of blocking or notifying -- it is
// pure comparison. That is deliberate: the maintainer's design decision
// (design-2540.md section 2, "what happens on match-fail") is that the
// notify/skip action lives in each consumer (#2565, #2566), not here. This
// file provides only the verdict; consumers decide what to do with it.
//
// # Fail-open semantics
//
// Indeterminate means "no finding, allow" to every caller: an unusable or
// missing candidate hash, an empty reference set, or an invalid tolerance
// must never be read as a collision. This mirrors usablePHash treating a
// zero/empty stored hash as "unknown, never matches" (phash_mismatch.go:
// 259-279) and the NaN/out-of-range tolerance guard (phash_mismatch.go:
// 468-480).

// FanartIdentityEntry is one artist's fanart slot in the cross-artist
// comparison registry: an artist id paired with a usable perceptual hash.
// The registry loader (internal/artist) is responsible for excluding
// unusable/zero hashes before entries reach here -- see usablePHash.
type FanartIdentityEntry struct {
	ArtistID string
	PHash    uint64
}

// IdentityVerdict is the 3-state result of comparing a candidate fanart
// phash against a cross-artist reference registry.
type IdentityVerdict int

const (
	// IdentityIndeterminate means the comparison could not be evaluated:
	// the candidate hash is unusable, the reference registry is empty, or
	// the tolerance is invalid. Fail-open: callers treat this as "no
	// finding, allow the write/push".
	IdentityIndeterminate IdentityVerdict = iota

	// IdentityMatch means the candidate hash did not collide with any
	// OTHER artist's fanart entry at or above tolerance.
	IdentityMatch

	// IdentityMismatch means at least one other artist's fanart entry
	// collided with the candidate at or above tolerance.
	IdentityMismatch
)

// String implements fmt.Stringer for logging.
func (v IdentityVerdict) String() string {
	switch v {
	case IdentityIndeterminate:
		return "indeterminate"
	case IdentityMatch:
		return "match"
	case IdentityMismatch:
		return "mismatch"
	default:
		return "unknown"
	}
}

// IdentityResult is the outcome of CompareIdentity.
type IdentityResult struct {
	Verdict IdentityVerdict

	// CollidingArtistID is the OTHER artist whose fanart entry produced the
	// best (highest-similarity) collision. Empty unless Verdict ==
	// IdentityMismatch.
	CollidingArtistID string

	// Similarity is the best (highest) similarity score among all
	// colliding entries, i.e. the score belonging to CollidingArtistID.
	// Zero unless Verdict == IdentityMismatch.
	Similarity float64

	// MatchCount is the number of DISTINCT other artists whose fanart
	// collided with the candidate at or above tolerance -- not the number
	// of colliding slots. A candidate that collides with many distinct
	// artists is more likely to be shared legitimate promo art than a
	// single wrong-artist write (mirrors PHashCollision.MatchCount,
	// phash_mismatch.go:133-136); consumers use this as an escape hatch.
	// Zero unless Verdict == IdentityMismatch.
	MatchCount int
}

// CompareIdentity compares a candidate fanart phash against a cross-artist
// registry of (artistID, phash) reference entries, exactly mirroring
// bestCrossArtistMatch (internal/rule/phash_mismatch.go:416-434): a
// reference entry collides when its similarity to the candidate is >=
// tolerance, entries belonging to destArtistID are excluded, and the best
// (highest-similarity) OTHER artist is reported alongside a count of how
// many DISTINCT other artists collided at all.
//
// candidatePHash == 0 is treated as unusable (the same zero-hash trap
// usablePHash guards against: an unhashed/undecodable image is
// indistinguishable from a genuine all-zero hash, so admitting it would
// manufacture false collisions) and yields IdentityIndeterminate.
//
// tolerance must be in (0, 1]; NaN, <=0, or >1 all yield
// IdentityIndeterminate rather than silently substituting a default,
// because unlike the detector (which owns its own default and NaN
// fallback, phash_mismatch.go:468-480) this primitive has no scan-level
// context to pick one -- callers are expected to pass
// defaultImageDupTolerance-equivalent value themselves. This is
// deliberately fail-open like every other unusable-input case here: a
// broken tolerance must never be read as "everything collides".
func CompareIdentity(candidatePHash uint64, destArtistID string, reference []FanartIdentityEntry, tolerance float64) IdentityResult {
	if candidatePHash == 0 || len(reference) == 0 || !validTolerance(tolerance) {
		return IdentityResult{Verdict: IdentityIndeterminate}
	}

	bestArtistID := ""
	bestSim := 0.0
	distinctOthers := map[string]struct{}{}

	for _, e := range reference {
		if e.ArtistID == destArtistID {
			continue
		}
		// A zero reference hash is exactly as unusable as a zero
		// candidate hash, for the same reason (usablePHash never
		// admits a zero hash into the registry it builds). The
		// registry loader is expected to have already filtered these
		// out, but skip defensively rather than let a stray zero
		// manufacture a perfect collision.
		if e.PHash == 0 {
			continue
		}
		sim := Similarity(candidatePHash, e.PHash)
		if sim < tolerance {
			continue
		}
		distinctOthers[e.ArtistID] = struct{}{}
		if sim > bestSim {
			bestSim, bestArtistID = sim, e.ArtistID
		}
	}

	if len(distinctOthers) == 0 {
		return IdentityResult{Verdict: IdentityMatch}
	}
	return IdentityResult{
		Verdict:           IdentityMismatch,
		CollidingArtistID: bestArtistID,
		Similarity:        bestSim,
		MatchCount:        len(distinctOthers),
	}
}

// validTolerance reports whether tolerance is usable, matching the
// detector's NaN-aware guard (phash_mismatch.go:478): math.IsNaN is
// load-bearing because every IEEE-754 comparison against NaN is false, so
// a plain `tolerance <= 0 || tolerance > 1` range check silently admits
// NaN and defeats every subsequent `sim < tolerance` filter (nothing is
// ever rejected).
func validTolerance(tolerance float64) bool {
	if math.IsNaN(tolerance) {
		return false
	}
	return tolerance > 0 && tolerance <= 1
}
