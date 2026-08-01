package rule

import (
	"context"
	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// This file applies the #2828 ALBUM-EVIDENCE gate to the two automated
// MusicBrainz-ID write paths inside internal/rule (issue #2858):
//
//   - MetadataFixer.fixMBID          -- the nfo_has_mbid rule fixer.
//   - BulkExecutor.fetchImages       -- the bulk image job's MBID self-heal.
//
// Both already ask artist.EvaluateMBIDCandidate "is this search hit confident
// enough?", which reads only the search RESULT (score, name similarity,
// ambiguity margin). Neither asked the catalogue question -- "do this
// candidate's release groups actually look like this artist's albums?" -- which
// is the only one that can catch a confident name match on an empty MusicBrainz
// stub. That is the measured 18/18 failure behind #2828.
//
// FAIL-OPEN ON UNKNOWN, GATE ONLY ON CONTRADICTION. This is the deliberate
// policy difference from the internal/api identify tiers, which decline when
// they cannot look:
//
//	no album evidence available -> ALLOW  (behavior unchanged)
//	overlap >= AlbumOverlapAutoLinkFloor -> ALLOW
//	overlap in [AlbumOverlapReviewFloor, floor) -> BLOCK (review, no auto-write)
//	overlap <  AlbumOverlapReviewFloor -> BLOCK (decline)
//	red flag on an otherwise-permitted candidate -> BLOCK (needs a human)
//
// "No album evidence available" covers every state in which nothing can
// CONTRADICT the candidate: no album source wired, no release-group fetcher
// wired, an unreadable album directory (EvidenceUnknown), an artist that
// genuinely has no local albums (EvidenceNone), and a release-group fetch that
// failed. Roughly 43% of a production library sits in EvidenceUnknown, and a
// MusicBrainz provider is not a precondition for running the rule engine at
// all, so declining those would not harden nfo_has_mbid -- it would stop it
// doing anything on an Emby-only install. A gate that silently disables the
// rule it guards is a regression, not a fix.
//
// The contradiction cases still route through artist.EvaluateAlbumGate rather
// than re-deciding here, so there remains exactly ONE definition of the
// thresholds and the decision table in the tree.

// ruleAlbumGate is the album-evidence gate as the internal/rule write paths see
// it: an optional local album source plus an optional candidate release-group
// fetcher.
//
// Both collaborators are OPTIONAL by design and a nil either side makes the
// whole gate inert (every candidate allowed). That is the fail-open policy
// expressed structurally: an unwired gate cannot silently stop a rule from
// fixing anything. It also keeps every existing construction site -- and the
// many tests that build a bare MetadataFixer -- behaving exactly as before.
type ruleAlbumGate struct {
	// albums resolves the artist's LOCAL album titles with an evidence state.
	// Production wires artist.NewFilesystemAlbumSource(); nil disables the gate.
	albums artist.AlbumSource

	// fetcher resolves the CANDIDATE's release groups. Production wires the
	// registry-resolved MusicBrainz adapter; nil disables the gate.
	//
	// This is injected rather than obtained through Engine.releaseGroupFetcherFor
	// on purpose. That accessor is the #2476 capability gate for the CHECKER
	// surface, and it returns nil for any rule absent from
	// ruleProviderCapabilities -- which nfo_has_mbid is. Routing through it would
	// hand this gate a permanently nil fetcher, and granting nfo_has_mbid
	// capExternalProvider to work around that would widen a rule's declared
	// network reach for a reason that has nothing to do with its checker. Fixers
	// are explicitly out of that gate's scope (see the note near
	// ruleProviderCapabilities), so constructor/setter injection is the
	// established shape here.
	fetcher ReleaseGroupFetcher

	logger *slog.Logger
}

// wired reports whether both collaborators are present, i.e. whether the gate
// can produce a determination at all.
func (g ruleAlbumGate) wired() bool {
	return g.albums != nil && g.fetcher != nil
}

// log returns the gate's logger, falling back to the default so a gate built
// without one (tests, and the zero value) still records its decisions.
func (g ruleAlbumGate) log() *slog.Logger {
	if g.logger != nil {
		return g.logger
	}
	return slog.Default()
}

// permits reports whether an automated pass may adopt best's MusicBrainz ID for
// a, plus an operator-facing reason when it may not.
//
// EVERY early return here is an ALLOW, and that is the point: the only way to
// reach a block is to have read the artist's albums successfully, retrieved the
// candidate's catalogue successfully, and found that the two CONTRADICT. Any
// step that could not produce an answer falls through to allow with the
// behavior the caller had before this gate existed.
//
// The bool is deliberately COARSER than artist.AlbumGateDecision: both
// AlbumGateReview and AlbumGateDecline collapse to false, i.e. "do not
// auto-write". That is correct on this path specifically because neither caller
// has a review queue to route a candidate INTO -- unlike internal/api, where
// REVIEW means "show the candidate to an operator". Here the rule violation
// simply stays open, which is already the operator-visible surface: the artist
// keeps showing as missing an MBID and can still be linked by hand. Preserving
// the two states separately would therefore buy the caller nothing it could
// act on, so the distinction stays in the log line (which records the decision
// verbatim) rather than in the return type.
func (g ruleAlbumGate) permits(ctx context.Context, ruleID string, a *artist.Artist, best *provider.ArtistSearchResult) (bool, string) {
	if !g.wired() || a == nil || best == nil {
		return true, ""
	}

	// A locked artist is one the operator has declared finished, and #2754's
	// invariant is that Stillwater makes no outbound provider call on its behalf.
	// Allowing here rather than blocking keeps that invariant without inventing a
	// new refusal: the callers already skip locked artists, so this is the
	// belt-and-braces branch, not the live one.
	if a.Locked {
		return true, ""
	}

	local, err := g.albums.LocalAlbums(ctx, a)
	if err != nil {
		// Diagnostic only. Evidence is the contract (see AlbumSource), and the
		// evidence check below is what decides -- but a source that is quietly
		// broken must leave a trace rather than degrade every candidate's album
		// signal invisibly.
		g.log().Debug("album-evidence gate: reading local albums failed",
			slog.String("rule_id", ruleID),
			slog.String("artist", a.Name),
			slog.String("error", err.Error()))
	}
	if local.Evidence != artist.EvidenceFound {
		// EvidenceUnknown: nobody could look, so nothing contradicts.
		// EvidenceNone: the artist genuinely has no albums, so there is no
		// catalogue to disagree with the candidate's. The internal/api tiers
		// decline both; here they allow, per the fail-open policy above.
		return true, ""
	}

	titles, ok := g.candidateTitles(ctx, ruleID, a, best.MusicBrainzID)
	if !ok {
		// No determination was produced: either there was no MBID to look up, or
		// the fetch failed. A catalogue nobody could read cannot contradict
		// anything.
		return true, ""
	}

	in := artist.AlbumGateInput{
		Evidence:              local.Evidence,
		OverlapPercent:        artist.CompareAlbumSet(local, titles).MatchPercent,
		CandidateReleaseCount: len(titles),

		// True because this branch is only reached after a fetch that SUCCEEDED;
		// the failure path returned above. Never hardcoded past a failed fetch --
		// that would be the exact could-not-look-reads-as-agreement inversion the
		// gate exists to remove.
		CandidateReleasesKnown: true,

		// TRUE, deliberately, and it is a constant here rather than a
		// measurement. UncontestedBest asks "is this the ONLY candidate whose own
		// catalogue clears the overlap floor?", which needs a release-group fetch
		// per rival. Nothing in the rule path fetches rival catalogues (the
		// runner-up that reaches artist.EvaluateMBIDCandidate carries a relevance
		// SCORE, not a catalogue), so the honest answer here is "not measured".
		//
		// Under a fail-open policy an unmeasured input must take the value that
		// cannot manufacture a refusal. false would route EVERY otherwise-
		// permitted candidate to the contested branch -- a REVIEW for every
		// artist, i.e. nfo_has_mbid never auto-fixing again -- which is a
		// spurious decline in exactly the shape this issue forbids. true
		// therefore leaves the decision resting on the overlap evidence that WAS
		// measured. What it does not do is weaken any measured signal: the
		// overlap floor, the empty-catalogue check and the red flag all still
		// apply. Measuring it properly means fetching rival catalogues from a
		// rule pass, which is its own scope.
		UncontestedBest: true,

		// NOT a constant, unlike UncontestedBest: this one has a real source in a
		// rule context. CandidateRedFlag reads MusicBrainz's OWN disambiguation
		// string on the candidate already in hand, so it costs no extra call and
		// cannot fire on an absence -- a candidate with no disambiguation yields
		// "", which is the permissive value. It can only block a candidate the
		// SOURCE has labeled a tribute, cover, karaoke or soundalike act, and
		// then only into review rather than away.
		RedFlag: artist.CandidateRedFlag(best),
	}

	decision, reason := artist.EvaluateAlbumGate(in)
	if decision == artist.AlbumGatePermit {
		return true, ""
	}

	// Info, matching how the internal/api tiers log the same refusal: a candidate
	// the gate withheld is an operator-visible event, not a fault. The violation
	// stays open, so the operator can still link the artist by hand.
	g.log().Info("declined to adopt MusicBrainz ID: album-evidence gate",
		slog.String("rule_id", ruleID),
		slog.String("artist", a.Name),
		slog.String("candidate_mbid", best.MusicBrainzID),
		slog.String("album_evidence", local.Evidence.String()),
		slog.String("decision", decision.String()),
		slog.String("reason", reason))
	return false, reason
}

// candidateTitles fetches the candidate's release-group titles, reporting
// whether the lookup was a determination at all.
//
// known=false is returned for an empty MBID or a failed fetch, and is
// deliberately distinct from an empty title list: a provider that is down and a
// candidate with no catalogue both yield zero titles, and only one of them is a
// finding. Collapsing them would let a MusicBrainz outage read as "this
// candidate is an empty stub" and decline every write during it.
func (g ruleAlbumGate) candidateTitles(ctx context.Context, ruleID string, a *artist.Artist, mbid string) ([]string, bool) {
	if mbid == "" {
		return nil, false
	}

	// The round-trip is capped at discographyFetchTimeout for the same reason
	// countMBReleaseGroups does: a slow MusicBrainz response must not stall a
	// library-wide fix pass. A timeout degrades to "not a determination", which
	// allows. The cap is applied inside fetchGroups rather than here because the
	// two paths need it derived from DIFFERENT parents -- see fetchGroups.
	groups, err := g.fetchGroups(ctx, mbid)
	if err != nil {
		g.log().Warn("album-evidence gate: fetching the candidate's release groups failed, so its catalogue cannot contradict anything",
			slog.String("rule_id", ruleID),
			slog.String("artist", a.Name),
			slog.String("candidate_mbid", mbid),
			slog.String("error", err.Error()))
		return nil, false
	}

	titles := make([]string, len(groups))
	for i, grp := range groups {
		titles[i] = grp.Title
	}
	return titles, true
}

// fetchGroups routes the release-group fetch through the per-artist
// EvaluationContext coalescer when one is attached to ctx, matching
// Engine.fetchReleaseGroupsCoalesced and the coalescedSearch family in
// fixers.go. On the canonical pipeline path this collapses the gate's fetch
// with the discography checker's fetch for the same artist into one upstream
// call; off it (single-violation fixes, the bulk executor) it falls back to the
// bare fetcher, which is the pre-existing behavior.
//
// WHO OWNS THE DEADLINE is the whole reason the timeout lives here rather than
// in candidateTitles, and the two branches answer it differently:
//
//   - COALESCED branch: the upstream fetch is SHARED. Whoever loses the
//     singleflight race waits on the winner's result, so a deadline (or a
//     cancellation) belonging to ONE caller must never reach that fetch -- it
//     would cancel work other callers are waiting on, and the canceled error
//     would then be CACHED in the coalescer and served to them as though their
//     own fetch had failed. context.WithoutCancel severs the caller's
//     cancellation while KEEPING the context values (the EvaluationContext and
//     any PassContext ride along), and the timeout is then re-applied on top so
//     the shared fetch is still bounded -- by a cap the coalescer owns rather
//     than by whichever caller happened to arrive first.
//
//   - DIRECT branch: the fetch is caller-scoped by construction (no coalescer,
//     so nobody else can be waiting on it), and canceling it when the caller
//     goes away is correct. It keeps the caller's context and the caller's
//     15s cap, exactly as before.
//
// Both branches remain bounded by discographyFetchTimeout, so a slow
// MusicBrainz still cannot stall a library-wide pass, and a timeout on either
// still surfaces as an error -> known=false -> ALLOW (see candidateTitles).
func (g ruleAlbumGate) fetchGroups(ctx context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
	if ec := EvaluationContextFromContext(ctx); ec != nil {
		return ec.GetReleaseGroups(ctx, mbid, func(fetchCtx context.Context) ([]provider.ReleaseGroupInfo, error) {
			sharedCtx, cancel := context.WithTimeout(context.WithoutCancel(fetchCtx), discographyFetchTimeout)
			defer cancel()
			return g.fetcher.GetReleaseGroups(sharedCtx, mbid)
		})
	}

	fetchCtx, cancel := context.WithTimeout(ctx, discographyFetchTimeout)
	defer cancel()
	return g.fetcher.GetReleaseGroups(fetchCtx, mbid)
}
