package api

import (
	"context"
	"log/slog"
	"strings"

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

	// logger records the CAUSE of a failed fetch (#2862). It is held on the
	// cache rather than threaded through titles' signature because all three
	// call sites are Router methods that would pass the same r.logger, and a
	// parameter every caller fills identically is churn rather than choice.
	logger *slog.Logger

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
	return &releaseGroupCache{fetcher: fetcher, logger: r.logger, entries: make(map[string]releaseGroupEntry)}
}

// holds reports whether this MBID has already been looked up, so a caller
// rationing PROVIDER CALLS can tell a free cache hit from one that costs a
// round-trip.
//
// It is a method rather than a raw c.entries read at the call site precisely
// because the receiver can be nil: newReleaseGroupCache returns nil on an
// install with no MusicBrainz provider, and every other access to the map is
// already behind a nil guard. Reading the map from outside would turn a
// no-MusicBrainz install's identify run into a panic.
func (c *releaseGroupCache) holds(mbid string) bool {
	if c == nil {
		return false
	}
	_, ok := c.entries[mbid]
	return ok
}

// titles returns the candidate's release-group titles and whether the lookup
// was a determination at all. A nil receiver, an empty MBID, or a fetch error
// all report known=false.
//
// A failed fetch is LOGGED here, where the error is in hand (#2862). known=false
// stays the answer -- an unretrievable catalogue is not a determination, so the
// gate still declines -- but discarding the error let a broken MusicBrainz
// adapter degrade every candidate's album signal with no trace. Diagnostic
// only: it changes no decision, matching ChainAlbumSource's contract.
//
// WARN, not ERROR: the request still returns candidates, so this is a degraded
// result rather than a failure. The message may say the fetch FAILED because a
// provider call was genuinely ATTEMPTED here -- unlike the missing-fetcher
// guards feeding albumEvidenceReason, which are reached before any call is made
// and must never imply one happened.
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
		if c.logger != nil {
			c.logger.Warn("identify: fetching a candidate's release groups failed, so its catalogue cannot corroborate anything",
				"mbid", mbid, "error", err)
		}
		return nil, false
	}

	titles := make([]string, len(groups))
	for i, g := range groups {
		titles[i] = g.Title
	}
	c.entries[mbid] = releaseGroupEntry{titles: titles, known: true}
	return titles, true
}

// tier3AlbumGateDeclines reports whether the album-evidence gate refuses to let
// Tier 3 auto-link this candidate, logging the reason when it does.
//
// THIS IS THE HOLE #2828 CLOSES. Tier 3 is the name-only tier: it never
// consulted the artist's catalogue, and an artist with no readable album
// directory was ROUTED here precisely because Tier 2 had nothing to compare.
// So the absence of album evidence did not merely fail to object to a
// candidate, it steered the artist to the one tier that could not object at
// all, and 43% of a production library takes that route by default.
//
// SCOPE, so this is not read as more coverage than it is: this closes the hole
// on the two SEARCH-DERIVED tiers. Tier 1 (connection-library fill) remains
// ungated, and the internal/rule write paths are gated on FAIL-OPEN terms
// rather than these -- see the WHICH WRITE PATHS APPLY IT block at the top of
// internal/artist/albumgate.go for which, and why.
//
// Returning true only STOPS THE UNATTENDED WRITE. The caller falls through to
// the review queue, so the operator still sees the candidate and can link it
// by hand; nothing is hidden, only un-automated.
func (r *Router) tier3AlbumGateDeclines(ctx context.Context, a *artist.Artist, local artist.AlbumSet, results []provider.ArtistSearchResult, best *provider.ArtistSearchResult, cache *releaseGroupCache) bool {
	in := artist.AlbumGateInput{
		Evidence: local.Evidence,
		RedFlag:  artist.CandidateRedFlag(best),
		// UncontestedBest is left at its false zero value and only set below
		// once it has actually been MEASURED. It used to be hardcoded true on
		// the argument that artist.EvaluateMBIDCandidate's ambiguity margin had
		// already asked the same question. It has not: that margin compares
		// PROVIDER RELEVANCE SCORES (artist.MBIDAmbiguityMargin, 10 points),
		// while UncontestedBest asks whether more than one candidate's
		// catalogue CLEARS THE OVERLAP FLOOR. A rival 15 points back clears the
		// margin while its catalogue overlaps the local albums identically --
		// two same-named entities both carrying the artist's records -- and the
		// hardcoded true made the gate's contested branch structurally
		// unreachable for exactly that case.
	}

	// Only an EvidenceFound local set can produce a meaningful overlap, and the
	// gate declines the other two states outright, so the provider calls are
	// skipped entirely there. This is a cost saving, not a behavior change: the
	// decision is identical either way.
	if local.Evidence == artist.EvidenceFound {
		remote, known := cache.titles(ctx, best.MusicBrainzID)
		in.CandidateReleasesKnown = known
		in.CandidateReleaseCount = len(remote)
		if known {
			in.OverlapPercent = artist.CompareAlbumSet(local, remote).MatchPercent
		}
		in.UncontestedBest = !r.tier3RivalClearsOverlapFloor(ctx, local, results, best, cache)
	}

	decision, reason := artist.EvaluateAlbumGate(in)
	if decision == artist.AlbumGatePermit {
		return false
	}

	// Info, matching how every other identify decline is logged: a candidate the
	// gate refused is an operator-visible event, not a fault.
	r.logger.Info("identify: Tier 3 candidate declined by the album-evidence gate",
		"artist", a.Name,
		"proposed_mbid", best.MusicBrainzID,
		"album_evidence", local.Evidence.String(),
		"uncontested_best", in.UncontestedBest,
		"decision", decision.String(),
		"reason", reason)
	return true
}

// tier3RivalCatalogueBudget is how many RIVAL catalogues Tier 3 will fetch
// before it stops asking.
//
// Two, so the whole decision costs at most three GetReleaseGroups calls (the
// best candidate plus two rivals) -- the same per-artist provider budget
// enrichAndScoreTier2 already spends. A name-only search returning a long list
// of same-named entities is precisely the ambiguous case, and paying a provider
// round-trip per entity to confirm it would be the most expensive way to reach
// the least surprising answer.
const tier3RivalCatalogueBudget = 2

// tier3RivalClearsOverlapFloor reports whether some candidate OTHER than best
// also has a catalogue clearing the auto-link overlap floor.
//
// This is the measurement behind AlbumGateInput.UncontestedBest, and it is the
// exact condition Tier 2's caller checks with `len(above70) == 1`: not "is the
// best candidate ahead on score", but "is it the only one whose ALBUMS agree".
// Two same-named entities both carrying the local artist's records are not
// corroboration of either; the album evidence simply does not discriminate, and
// an automated pass must hand that to a human.
//
// TWO KINDS OF YES, both returning true, and the second is the one worth
// stating. A rival whose catalogue clears the floor is a measured contest. A
// rival whose catalogue could NOT be retrieved is an UNMEASURED one -- and an
// unmeasured rival must not be scored as an absent rival, because that is the
// same could-not-look-so-nothing-objected inversion this whole issue exists to
// remove. Both therefore contest, which withholds the unattended write and
// routes the candidate to a human. Nothing is hidden, only un-automated.
//
// KNOWN BOUND, stated rather than papered over: candidates past
// tier3RivalCatalogueBudget are never fetched and so never contest. Tier 2
// carries the identical bound (its own 3-candidate fetch cap leaves later
// candidates with releasesKnown false, so they can never join above70), and
// matching it keeps the two tiers refusing on the same terms rather than
// inventing a third standard here.
func (r *Router) tier3RivalClearsOverlapFloor(ctx context.Context, local artist.AlbumSet, results []provider.ArtistSearchResult, best *provider.ArtistSearchResult, cache *releaseGroupCache) bool {
	fetched := 0
	for i := range results {
		res := &results[i]

		// EqualFold, not ==: IsValidMBID accepts either case, so the very same
		// identity returned in uppercase by one row and lowercase by another
		// would otherwise become a rival of itself and contest every write.
		if res.MusicBrainzID == "" || strings.EqualFold(res.MusicBrainzID, best.MusicBrainzID) {
			continue
		}
		if !artist.IsValidMBID(normalizeMBID(res.MusicBrainzID)) {
			// Not a usable identity, so not an identity that could win. It is
			// skipped WITHOUT spending budget: a malformed ID costs no provider
			// call, so counting it would let junk rows crowd out real rivals.
			continue
		}

		// The budget rations PROVIDER CALLS, so only a lookup that actually
		// makes one is charged for -- the same argument the malformed-ID skip
		// above already makes.
		//
		// Which hits are reachable, stated precisely because the obvious answer
		// is wrong: a candidate Tier 2 already fetched is NOT one of them.
		// enrichAndScoreTier2 populates this cache, but it sits on the one Tier 2
		// branch that returns unconditionally (handlers_identify.go), so every
		// path that actually falls through to Tier 3 leaves the cache empty of
		// Tier 2 work. What reaches here warm is `best` (skipped by the EqualFold
		// guard above before this line) and a repeated MBID inside a single
		// result list. The charge-only-on-fetch rule is the correct semantics
		// regardless, and it is what keeps the budget honest if Tier 2 ever does
		// fall through warm -- but it is defensive here, not load-bearing.
		//
		// The budget gates the FETCH, not the iteration. It is checked after
		// cachedAlready is known and only when the lookup would cost a call,
		// because a cached rival is free to evaluate and evaluating it can only
		// make the gate STRICTER. Returning at the top of the loop instead --
		// as this did before -- meant uncached rivals could exhaust the budget
		// and a later CACHED rival whose catalogue clears the overlap floor was
		// never consulted, so a contested identity read as uncontested and the
		// gate permitted an unattended write. That is the dangerous direction,
		// and it is the same shape as the charge-on-cache-hit bug this function
		// was already being fixed for: budget accounting that skips rivals.
		//
		// holds MUST be asked BEFORE titles: titles writes c.entries on BOTH its
		// success and failure paths, so asking after would make holds always true,
		// never increment, and silently render the budget inert.
		// TestTier3RivalBudgetStopsAtTheBudget pins that bound.
		cachedAlready := cache.holds(res.MusicBrainzID)
		if !cachedAlready && fetched >= tier3RivalCatalogueBudget {
			return false
		}
		remote, known := cache.titles(ctx, res.MusicBrainzID)
		if !cachedAlready {
			fetched++
		}
		if !known {
			r.logger.Info("identify: Tier 3 rival catalogue could not be retrieved, treating the best candidate as contested",
				"rival_mbid", res.MusicBrainzID)
			return true
		}
		if len(remote) == 0 {
			// An empty stub cannot corroborate anything, so it cannot contest
			// either. This is the measured 18/18 shape on the rival side.
			continue
		}
		if artist.CompareAlbumSet(local, remote).MatchPercent >= artist.AlbumOverlapAutoLinkFloor {
			return true
		}
	}
	return false
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
