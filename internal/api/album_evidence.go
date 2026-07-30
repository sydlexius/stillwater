package api

import (
	"context"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// Candidate "reason" strings for the two cases that used to be one.
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
)

// albumEvidenceReason maps an evidence state to the operator-facing reason for a
// candidate that could not be album-scored.
//
// EvidenceFound never reaches here: a found set IS comparable, so its callers
// score it instead of explaining why they did not.
func albumEvidenceReason(e artist.AlbumEvidence) string {
	if e == artist.EvidenceUnknown {
		return reasonLocalAlbumsUnreadable
	}
	return reasonNoAlbumData
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
		attrs = append(attrs, "reason", err.Error())
	}
	r.logger.Warn("album evidence unknown: candidate scoring has no album signal", attrs...)
	return set
}

// enrichAndScoreTier2Set is the evidence-aware entry point to the shared
// MusicBrainz cross-MBID scorer.
//
// It deliberately does NOT change enrichAndScoreTier2's signature. That helper
// is also called by the bulk-identify path in handlers_identify.go, whose
// hasAlbums gate routes an artist to an ungated auto-link and is therefore
// migrated separately, under its own replay test. Gating here instead keeps this
// change confined to display surfaces.
//
// The gate is the point: only an EvidenceFound set is ever handed to the
// []string-based scorer, so a set that carries no determination cannot be
// laundered into an empty local album list somewhere further down. That is a
// stronger guarantee than passing the AlbumSet through and trusting the callee
// to read Evidence.
func (r *Router) enrichAndScoreTier2Set(ctx context.Context, results []provider.ArtistSearchResult, local artist.AlbumSet) []ScoredCandidate {
	if local.Evidence != artist.EvidenceFound {
		return convertToScoredCandidatesReason(results, albumEvidenceReason(local.Evidence))
	}
	return r.enrichAndScoreTier2(ctx, results, local.Titles)
}

// convertToScoredCandidatesReason wraps raw search results as ScoredCandidates
// with zero confidence and an explicit reason.
//
// convertToScoredCandidates hardcodes reasonNoAlbumData, which is the wrong
// words for an Unknown album set; this variant lets the caller say which of the
// two it means. The zero Confidence is identical either way -- the reason string
// is what tells the two apart.
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
// when LocalCount > 0, so Unknown and None both render as "no badge" -- silent
// rather than false, but still indistinguishable to an operator reading the
// page. The JSON surface can carry the distinction today without a template
// change, so it does; making the HTML say it out loud is tracked separately.
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
