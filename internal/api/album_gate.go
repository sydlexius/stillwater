package api

import (
	"context"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// releaseGroupCache fetches a candidate's release-group titles once per MBID for
// the lifetime of one identify decision.
//
// Caching here rather than widening provider.ArtistSearchResult with a release
// count is deliberate. A count on the search result would make EVERY search pay
// for an extra provider round-trip per hit, including the many searches that
// never go on to make an identity decision (the typeahead, the display
// surfaces). The gate is the only consumer, so the fetch belongs at the gate.
//
// It is NOT safe for concurrent use and is not meant to be: one instance is
// created per artist inside identifyArtist, which processes artists serially.
type releaseGroupCache struct {
	fetcher provider.ReleaseGroupFetcher

	// entries maps a MusicBrainz ID to what we learned about its catalogue.
	// A present entry with known=false records a FAILED lookup, so a provider
	// that is down is asked once per artist rather than once per candidate.
	entries map[string]releaseGroupEntry
}

// releaseGroupEntry is one cached catalogue lookup.
//
// known is separate from len(titles) for the same reason AlbumEvidence exists:
// a failed fetch and an artist with an empty catalogue both produce zero
// titles, and only one of them is a determination. Collapsing them would let a
// MusicBrainz outage read as "this candidate has no albums" -- or, worse under
// an inverted gate, as "nothing objected".
type releaseGroupEntry struct {
	titles []string
	known  bool
}

// newReleaseGroupCache returns a cache backed by the registered MusicBrainz
// adapter, or nil when no release-group fetcher is available.
//
// A nil cache is usable: its lookup reports "not known", which the gate treats
// as a decline. That is the intended behavior for an install with no
// MusicBrainz provider configured -- the catalogue check cannot be performed,
// so an unattended write is not authorized. An operator can still link by hand.
func (r *Router) newReleaseGroupCache() *releaseGroupCache {
	if r.providerRegistry == nil {
		return nil
	}
	mbProvider := r.providerRegistry.Get(provider.NameMusicBrainz)
	if mbProvider == nil {
		return nil
	}
	fetcher, ok := mbProvider.(provider.ReleaseGroupFetcher)
	if !ok {
		return nil
	}
	return &releaseGroupCache{fetcher: fetcher, entries: make(map[string]releaseGroupEntry)}
}

// titles returns the candidate's release-group titles and whether the lookup
// was a determination at all. A nil receiver, an empty MBID, or a fetch error
// all report known=false.
func (c *releaseGroupCache) titles(ctx context.Context, mbid string) ([]string, bool) {
	if c == nil || c.fetcher == nil || mbid == "" {
		return nil, false
	}
	if entry, ok := c.entries[mbid]; ok {
		return entry.titles, entry.known
	}

	groups, err := c.fetcher.GetReleaseGroups(ctx, mbid)
	if err != nil {
		c.entries[mbid] = releaseGroupEntry{known: false}
		return nil, false
	}

	titles := make([]string, len(groups))
	for i, g := range groups {
		titles[i] = g.Title
	}
	c.entries[mbid] = releaseGroupEntry{titles: titles, known: true}
	return titles, true
}

// tier2GatePermits reports whether the album-evidence gate authorizes Tier 2's
// unattended write, logging the reason when it does not.
//
// Tier 2 already fetched the candidate's release groups to compute the album
// comparison, so the counts are read off the ScoredCandidate rather than
// re-fetched. That is why enrichAndScoreTier2 records them: the alternative is
// a second provider call for data the scorer just had in hand.
//
// UncontestedBest is true unconditionally because the caller only reaches here
// with exactly one candidate above the auto-link floor -- that IS the
// uncontested condition, checked by the caller rather than restated here.
func (r *Router) tier2GatePermits(a *artist.Artist, local artist.AlbumSet, cand *ScoredCandidate) bool {
	in := artist.AlbumGateInput{
		Evidence:               local.Evidence,
		CandidateReleaseCount:  cand.releaseCount,
		CandidateReleasesKnown: cand.releasesKnown,
		UncontestedBest:        true,
		RedFlag:                artist.CandidateRedFlag(&cand.ArtistSearchResult),
	}
	if cand.AlbumComparison != nil {
		in.OverlapPercent = cand.AlbumComparison.MatchPercent
	}

	decision, reason := artist.EvaluateAlbumGate(in)
	if decision == artist.AlbumGatePermit {
		return true
	}
	r.logger.Info("identify: Tier 2 candidate declined by the album-evidence gate",
		"artist", a.Name,
		"proposed_mbid", cand.MusicBrainzID,
		"album_evidence", local.Evidence.String(),
		"decision", decision.String(),
		"reason", reason)
	return false
}
