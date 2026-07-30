package api

import (
	"context"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// Candidate "reason" strings for the cases that used to be one.
//
// Before this file existed, every non-comparable case collapsed into
// reasonNoAlbumData -- an artist whose folder could not be read was reported to
// the operator with the same words as an artist that genuinely owns no albums.
// Those are opposite facts: the first says "do not trust this score", the second
// says "this score is all the evidence there is". Splitting the strings is the
// cheapest place the distinction becomes visible, because Reason is already on
// the wire (ScoredCandidate.reason) and already documented.
const (
	// reasonNoAlbumData is a POSITIVE determination: the lookup succeeded and
	// the artist has no album folders, so there was nothing to compare.
	reasonNoAlbumData = "no album data available"

	// reasonLocalAlbumsUnreadable is the absence of a determination: no path is
	// recorded, or the path could not be listed (missing mount, permissions, a
	// file where a directory was expected). The candidate's confidence carries
	// no album signal at all, and MUST NOT be read as "matched nothing".
	reasonLocalAlbumsUnreadable = "local albums could not be read"

	// reasonNoCandidateAlbumSource is the CANDIDATE-side twin of
	// reasonLocalAlbumsUnreadable, and it exists for exactly the same reason.
	//
	// The local albums were read fine; what is missing is the other half of the
	// comparison. No provider registry is wired, the provider is not registered,
	// or the registered adapter does not implement the optional album-fetching
	// interface -- so nothing could supply the candidate's albums and there was
	// nothing to compare the local ones AGAINST.
	//
	// Reporting that as reasonNoAlbumData would be a lie in the operator's face:
	// it claims the artist owns no albums when the artist may own a shelf full,
	// and it is the same could-not-look versus genuinely-none conflation this
	// file exists to remove -- just moved to the other side of the comparison.
	//
	// THE WORDING IS LOAD-BEARING, and an earlier draft of it ("candidate
	// catalogues could not be retrieved") was WRONG in the same way the bug it
	// fixed was wrong. Every caller reaches this string from a MISSING-FETCHER
	// guard, before any provider call is made -- so no retrieval was ever
	// attempted, and saying one failed asserts an event that did not happen. A
	// fetch that IS attempted and fails never lands here: those callers log the
	// error and `continue`, leaving the candidate on "album comparison". So this
	// string means "nothing could look", never "looking failed".
	reasonNoCandidateAlbumSource = "no provider available to look up candidate albums"
)

// albumEvidenceReason maps an evidence state to the operator-facing reason for a
// candidate that could not be album-scored.
//
// All three states reach here. EvidenceFound is the case a reader will not
// expect, so it is worth spelling out: a caller with a perfectly good local
// album list still lands in a fallback when nothing can supply the CANDIDATE's
// albums (no registry, no adapter, an adapter without the album-fetching
// interface). The local evidence is not what is missing there, so the reason
// must not blame it -- and no fetch was attempted, so it must not claim one
// failed either.
func albumEvidenceReason(e artist.AlbumEvidence) string {
	switch e {
	case artist.EvidenceUnknown:
		return reasonLocalAlbumsUnreadable
	case artist.EvidenceFound:
		return reasonNoCandidateAlbumSource
	case artist.EvidenceNone:
		return reasonNoAlbumData
	default:
		// An AlbumEvidence value this function does not know is a new state
		// somebody added without revisiting the reason strings. Claiming "the
		// artist has no albums" for it would be a fabricated positive
		// determination, so answer with the honest non-determination instead.
		return reasonLocalAlbumsUnreadable
	}
}

// localAlbumSet resolves an artist's local albums through the evidence-aware
// AlbumSource instead of artist.ListLocalAlbums.
//
// ListLocalAlbums returns an empty slice for three unrelated situations -- no
// path recorded, an unreadable directory, and a genuinely empty directory -- so
// its callers cannot tell "I could not look" from "there is nothing there".
// FilesystemAlbumSource separates them, and this wrapper adds the one thing
// every display surface needs on top: an Unknown answer is LOGGED with the
// artist and the underlying cause, so a broken mount leaves a trace instead of
// quietly degrading every candidate score in the library to zero album signal.
func (r *Router) localAlbumSet(ctx context.Context, a *artist.Artist) artist.AlbumSet {
	set, err := artist.NewFilesystemAlbumSource().LocalAlbums(ctx, a)
	if set.Evidence != artist.EvidenceUnknown {
		return set
	}

	// WARN, not ERROR: the request still succeeds and still returns candidates.
	// What is lost is the album-comparison signal, which is a degraded result
	// rather than a failure -- but a silent degradation is exactly the defect
	// this series exists to remove, so it is never logged below WARN.
	attrs := []any{"artist_id", "", "artist", "", "path", ""}
	if a != nil {
		attrs = []any{"artist_id", a.ID, "artist", a.Name, "path", a.Path}
	}
	if err != nil {
		attrs = append(attrs, "reason", err)
	}
	r.logger.Warn("album evidence unknown: candidate scoring has no album signal", attrs...)
	return set
}

// enrichAndScoreTier2Set is the DISPLAY-surface entry point to the shared
// MusicBrainz cross-MBID scorer.
//
// Since #2828 migrated enrichAndScoreTier2 to take an artist.AlbumSet directly,
// the evidence branch lives inside the scorer and this wrapper adds only the
// per-request release-group cache. It is kept as a named entry point because
// the five display surfaces have no cache of their own to thread through and no
// identity decision to make -- each one scores a single search's worth of
// candidates and renders them.
func (r *Router) enrichAndScoreTier2Set(ctx context.Context, results []provider.ArtistSearchResult, local artist.AlbumSet) []ScoredCandidate {
	return r.enrichAndScoreTier2(ctx, results, local, r.newReleaseGroupCache())
}

// convertToScoredCandidatesReason wraps raw search results as ScoredCandidates
// with zero confidence and an explicit reason.
//
// convertToScoredCandidates hardcoded reasonNoAlbumData, which is the wrong
// words for an Unknown album set and equally wrong for a Found one with no
// source for the candidate's albums; this variant lets the caller say which of
// the three it means. The zero Confidence is identical in all three cases --
// the reason string is the only thing that tells them apart.
func convertToScoredCandidatesReason(results []provider.ArtistSearchResult, reason string) []ScoredCandidate {
	scored := make([]ScoredCandidate, len(results))
	for i := range results {
		scored[i] = ScoredCandidate{
			ArtistSearchResult: results[i],
			Confidence:         0,
			Reason:             reason,
		}
	}
	return scored
}

// albumsUnavailableKey is the JSON response key that reports an Unknown local
// album set to an API client.
//
// It exists because the HTML candidate fragments render an album badge only
// when LocalCount > 0, so Unknown and None would both render as "no badge" --
// silent rather than false, but still indistinguishable to an operator reading
// the page. The HTML surfaces now carry the distinction as a visible notice
// (see the AlbumsUnavailable view-model fields); this key is the JSON surface's
// equivalent, for clients that never render the fragment.
const albumsUnavailableKey = "local_albums_unavailable"

// markAlbumsUnavailable adds albumsUnavailableKey to a JSON response body when
// the local album set was not a determination. The key is OMITTED rather than
// set to false in the normal case, matching how provider_error / failed_providers
// already behave on these same endpoints: presence means something is wrong.
func markAlbumsUnavailable(resp map[string]any, local artist.AlbumSet) {
	if local.Evidence == artist.EvidenceUnknown {
		resp[albumsUnavailableKey] = true
	}
}
