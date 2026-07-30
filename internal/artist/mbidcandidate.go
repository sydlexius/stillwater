package artist

import (
	"fmt"
	"strings"

	"github.com/sydlexius/stillwater/internal/provider"
)

// This file holds the MusicBrainz-ID candidate confidence gates (issue #2813).
// They were lifted here verbatim from internal/rule/fixers.go so that both
// internal/rule and internal/api can apply the SAME gates: two copies of a
// correctness floor is two floors, and the one nobody remembers to update is
// the one that adopts a wrong ID. Behavior is unchanged by the move.

// Confidence gates the nfo_has_mbid fixer applies before adopting a name-search
// hit as an artist's MusicBrainz ID (issue #2715).
//
// A wrong MBID is strictly worse than no MBID. Nothing in Stillwater resolves a
// stored MBID against MusicBrainz or cross-checks it against the artist's album
// catalogue; IsValidMBID only checks that the string is a UUID. So once a
// caller writes an ID, every downstream consumer treats it as fact. These
// constants are deliberately not operator-configurable: they are a correctness
// floor, not a matching preference, and an operator who dialed them to zero
// would silently restore the defect.
const (
	// MBIDMinProviderScore is the minimum provider-reported search score the
	// top hit must carry. Providers populate ArtistSearchResult.Score either
	// from their own relevance ranking (MusicBrainz) or from
	// provider.NameSimilarity (every other adapter), so it is the only
	// cross-provider confidence signal that exists.
	MBIDMinProviderScore = 85

	// MBIDMinNameSimilarity is the minimum locally-computed similarity between
	// the artist's stored name and the hit's name. This is checked SEPARATELY
	// from the provider score because MusicBrainz's score is a relevance rank
	// that folds in popularity and tag matches, so a well-known artist can be
	// returned at score 100 for a name that barely resembles the query. The
	// local comparison cannot be inflated that way.
	MBIDMinNameSimilarity = 85

	// MBIDAmbiguityMargin is how far the top hit must outscore the best hit
	// carrying a DIFFERENT MBID. Two distinct artists sharing a name both come
	// back as exact matches at score 100, and a floor alone happily adopts
	// whichever the provider happened to list first. When the search cannot
	// discriminate between two identities, neither is evidence.
	MBIDAmbiguityMargin = 10
)

// MBIDRejection describes why an MBID candidate was not adopted. A zero value
// means the candidate cleared every gate.
type MBIDRejection struct {
	Reason string
}

// DescribeRunnerUp renders the ambiguity side of the provenance message: the
// next-best hit carrying a different MBID, or a statement that none existed.
// Recording "no rival" explicitly matters as much as recording a rival's score,
// because "uncontested" is the strongest form of this signal and must not be
// indistinguishable from "we forgot to look".
func DescribeRunnerUp(runnerUp *provider.ArtistSearchResult) string {
	if runnerUp == nil {
		return "no rival MusicBrainz ID in results"
	}
	return fmt.Sprintf("next rival %s at confidence %d", runnerUp.MusicBrainzID, runnerUp.Score)
}

// BestMBIDCandidates returns the highest-scoring search hit carrying a
// syntactically valid MusicBrainz ID, plus the highest-scoring MusicBrainz-
// SOURCED hit carrying a DIFFERENT valid MBID (nil when no such rival exists).
//
// Splitting on the ID rather than on list position is load-bearing: several
// providers return the same artist, so the raw second-place row is usually a
// duplicate of the winner and treating it as a rival would reject every
// well-corroborated match.
//
// Restricting RIVALS to MusicBrainz is the other half of that. The ambiguity
// margin exists to catch two genuinely DIFFERENT artists ranked close together
// within MusicBrainz's own relevance ordering. Every non-MusicBrainz adapter
// scores on an incomparable scale -- provider.NameSimilarity, not relevance --
// and carries a MusicBrainzID it did not author, which is routinely stale or
// points at a since-merged entity. Such a hit cannot meaningfully rival
// MusicBrainz's ranking: its agreeing or disagreeing about an MBID is not
// evidence of ambiguity. Left unfiltered, the most common configuration
// (MusicBrainz + Last.fm) declines constantly, because Last.fm returns a stale
// ID for the SAME artist at name-similarity 100 and the margin gate reads the
// two rows as two artists zero points apart.
//
// This narrows only who may be the RUNNER-UP. Any provider's hit may still be
// the best, and the floors are unchanged.
//
// Consequence, accepted deliberately: when MusicBrainz is disabled or returned
// nothing, no hit is rival-eligible, so the ambiguity gate cannot fire and the
// score floor plus the name-similarity floor are the only gates left. Guarding
// that case (declining outright when the best hit is not MusicBrainz-sourced)
// was considered and rejected -- it would stop the rule fixing ANYTHING for
// operators who do not run MusicBrainz, which is a bigger regression than the
// one it prevents, and a non-MusicBrainz rival was never trustworthy evidence
// of ambiguity in the first place. The absent gate is recorded in the
// provenance message as "no rival MusicBrainz ID in results".
func BestMBIDCandidates(results []provider.ArtistSearchResult) (best, runnerUp *provider.ArtistSearchResult) {
	for i := range results {
		r := &results[i]
		if !IsValidMBID(r.MusicBrainzID) {
			continue
		}
		if best == nil || r.Score > best.Score {
			best = r
		}
	}
	if best == nil {
		return nil, nil
	}
	for i := range results {
		r := &results[i]
		if !IsValidMBID(r.MusicBrainzID) || !isMusicBrainzSourcedResult(r) {
			continue
		}
		// Case-insensitive: IsValidMBID accepts A-F as well as a-f, so a
		// provider returning an uppercase UUID would otherwise compare unequal
		// to the very same ID in lowercase and become a rival of itself.
		if strings.EqualFold(r.MusicBrainzID, best.MusicBrainzID) {
			continue
		}
		if runnerUp == nil || r.Score > runnerUp.Score {
			runnerUp = r
		}
	}
	return best, runnerUp
}

// isMusicBrainzSourcedResult reports whether a search hit came from the MusicBrainz
// adapter itself, which is the only source whose Score is a relevance rank
// comparable with another MusicBrainz row's. Compared case-insensitively for
// the same reason the MBID compare is: Source is a free-form string on the
// wire, and a case difference must not silently change gate behavior.
func isMusicBrainzSourcedResult(r *provider.ArtistSearchResult) bool {
	return strings.EqualFold(r.Source, string(provider.NameMusicBrainz))
}

// EvaluateMBIDCandidate applies the confidence gates to a candidate. It returns
// nil when the candidate may be adopted, or the reason it was rejected.
func EvaluateMBIDCandidate(artistName string, best, runnerUp *provider.ArtistSearchResult) *MBIDRejection {
	// BestMBIDCandidates documents that best may be nil (no search hit carried a
	// usable MusicBrainz ID), and this function is now reachable from packages
	// that did not exist when its only caller guarded the nil itself. Rejecting
	// is the correct answer rather than a panic: no candidate is not a candidate
	// that passed, and the whole point of this gate is that the absent case must
	// decline rather than fall through to a write.
	if best == nil {
		return &MBIDRejection{Reason: "no candidate carried a usable MusicBrainz ID"}
	}
	if best.Score < MBIDMinProviderScore {
		return &MBIDRejection{Reason: fmt.Sprintf(
			"top hit %q scored %d, below the %d confidence floor",
			best.Name, best.Score, MBIDMinProviderScore)}
	}
	if sim := provider.NameSimilarity(artistName, best.Name); sim < MBIDMinNameSimilarity {
		return &MBIDRejection{Reason: fmt.Sprintf(
			"top hit %q matches the artist name only %d%%, below the %d%% floor",
			best.Name, sim, MBIDMinNameSimilarity)}
	}
	if runnerUp != nil && best.Score-runnerUp.Score < MBIDAmbiguityMargin {
		return &MBIDRejection{Reason: fmt.Sprintf(
			"ambiguous: %q (%d) and %q (%d) are different artists within %d points",
			best.Name, best.Score, runnerUp.Name, runnerUp.Score, MBIDAmbiguityMargin)}
	}
	return nil
}
