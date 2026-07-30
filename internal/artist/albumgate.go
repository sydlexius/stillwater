package artist

import (
	"fmt"
	"strings"

	"github.com/sydlexius/stillwater/internal/provider"
)

// This file holds the ALBUM-EVIDENCE gate (issue #2828): the decision a
// SEARCH-DERIVED automated pass must make before it is allowed to write a
// MusicBrainz ID.
//
// WHICH WRITE PATHS APPLY IT, stated plainly because a gate is only as good as
// its coverage and a reader must not have to infer this:
//
//   - internal/api Tier 2 (album comparison) -- GATED.
//   - internal/api Tier 3 (name-only search) -- GATED. This was the hole.
//   - internal/api Tier 1 (connection-library fill) -- NOT GATED. See the
//     WHY-NOT note below; this is a deliberate scope decision, not an omission.
//   - internal/rule fixMBID and internal/rule bulk_executor fetchImages -- NOT
//     GATED. Both call EvaluateMBIDCandidate with no catalogue leg. That is a
//     genuine remaining gap, tracked separately rather than fixed here.
//
// WHY TIER 1 IS NOT GATED. The gate's two admitting states are the problem: it
// declines EvidenceUnknown and EvidenceNone outright, and it needs a
// release-group fetch to judge the candidate. Tier 1 makes no provider call at
// all -- it reads an identity a connected platform already holds -- so applying
// this gate there would (a) require a configured MusicBrainz provider before an
// Emby-only or Jellyfin-only install could auto-link anything, turning a
// working connection-index fill into a total decline, and (b) refuse the 43% of
// artists in EvidenceUnknown, which is most of what Tier 1 exists to serve.
//
// What makes that acceptable rather than merely cheap is that Tier 1's risk
// shape is different in kind from the one measured here. The 18/18 wrong
// adoptions came from a NAME SEARCH landing on an empty MusicBrainz stub -- an
// entity nobody asserted was this artist. Tier 1's ID is not search-derived: it
// is an identity a platform the operator deliberately connected already
// records for an artist it also holds. And per #2856 Tier 1 may only ever FILL
// a blank, never replace a stored ID, so its worst case is a blank becoming
// wrong rather than a correct identity being destroyed.
//
// That is a smaller risk, NOT no risk: a platform index can carry a namesake,
// and the name match that selects the entry is normalization-only. Closing it
// needs a corroboration signal suited to Tier 1's provenance rather than this
// catalogue gate, which is why it is tracked as its own work.
//
// It is the third leg of the identify confidence story and the only one that
// asks about the artist's catalogue rather than about a name string:
//
//   - EvaluateMBIDCandidate (mbidcandidate.go) asks "is this search hit
//     confident enough?" -- score floor, name-similarity floor, ambiguity
//     margin. All three read the search RESULT.
//   - EvaluateAlbumGate asks "does the candidate's catalogue actually look like
//     this artist's catalogue?" -- which is the only question that can catch a
//     confident name match on the wrong entity (a tribute act, a namesake, an
//     empty MusicBrainz stub).
//
// The two are complementary and both must pass. A candidate that clears the
// name gates and fails this one is exactly the failure #2828 exists to stop:
// in the measured production snapshot, 18 of 18 wrongly-adopted IDs pointed at
// a MusicBrainz entity carrying ZERO release groups while the local artist had
// a full album directory. Every one of them cleared the name gates at 100%.

// Album-overlap thresholds for the identify pipeline.
//
// These are the numbers the Tier 2 album-comparison path has always applied;
// they were bare literals inside evaluateTier2 and are promoted here so there
// is exactly ONE definition of each in the tree, sitting next to the #2813
// name-confidence floors they are applied alongside. They are deliberately not
// operator-configurable for the same reason those are: a correctness floor an
// operator can dial to zero is not a floor.
const (
	// AlbumOverlapAutoLinkFloor is the percentage of the artist's LOCAL albums
	// that must also appear in the candidate's catalogue before an automated
	// pass may adopt the candidate's MusicBrainz ID without asking anybody.
	AlbumOverlapAutoLinkFloor = 70

	// AlbumOverlapReviewFloor is the percentage below which a candidate is not
	// even worth showing an operator. Between this and AlbumOverlapAutoLinkFloor
	// the candidate goes to the review queue: real evidence, not enough of it.
	AlbumOverlapReviewFloor = 30
)

// AlbumGateDecision is what the gate permits a caller to do.
type AlbumGateDecision int

const (
	// AlbumGateDecline means no automated write and nothing worth an
	// operator's attention from this signal.
	//
	// It is iota 0 ON PURPOSE, for the same reason EvidenceUnknown is: a caller
	// that forgets to assign a decision, or a zero-valued struct that reaches a
	// write site, must mean "do not act". A destructive or hard-to-undo action
	// has to be authorized positively; it may never be authorized by the
	// absence of a veto.
	AlbumGateDecline AlbumGateDecision = iota

	// AlbumGateReview means there is genuine evidence but not enough to act on
	// unattended. Show the candidate to an operator.
	AlbumGateReview

	// AlbumGatePermit means an automated pass may adopt this candidate.
	AlbumGatePermit
)

// String renders the decision for logs.
func (d AlbumGateDecision) String() string {
	switch d {
	case AlbumGateDecline:
		return "decline"
	case AlbumGateReview:
		return "review"
	case AlbumGatePermit:
		return "permit"
	default:
		return fmt.Sprintf("AlbumGateDecision(%d)", int(d))
	}
}

// AlbumGateInput is everything the gate is allowed to read.
//
// Note what is NOT here: AlbumSet.Origin. Which source produced the local album
// listing must never change the decision (see the no-branching rule on
// AlbumSet.Origin), so the gate is handed the Evidence state and the computed
// overlap and cannot see the provenance at all. That is enforced structurally
// rather than by convention: there is no field to branch on.
type AlbumGateInput struct {
	// Evidence is the LOCAL side's evidence state, straight off the AlbumSet.
	// The zero value is EvidenceUnknown, which declines.
	Evidence AlbumEvidence

	// OverlapPercent is CompareAlbumSet's MatchPercent: what share of the
	// artist's local albums also appear in the candidate's catalogue. Read only
	// when Evidence is EvidenceFound, because it is 0 for both of the other
	// states and 0 there means "nothing was compared", not "nothing matched".
	OverlapPercent int

	// CandidateReleaseCount is how many release groups the CANDIDATE carries.
	// Zero is the measured 18/18 failure: an empty MusicBrainz stub matches any
	// name perfectly and owns no catalogue to contradict it.
	CandidateReleaseCount int

	// CandidateReleasesKnown says whether CandidateReleaseCount is a
	// determination at all. It is the candidate-side twin of AlbumEvidence: a
	// provider call that failed, timed out, or was never made leaves this false,
	// and a false here declines exactly like EvidenceUnknown does. Without it,
	// a failed release-group fetch would be indistinguishable from a candidate
	// with an empty catalogue -- which is the same conflation on the other side
	// of the comparison.
	CandidateReleasesKnown bool

	// UncontestedBest reports that this candidate is the ONLY one clearing the
	// auto-link overlap floor. Two candidates whose catalogues both overlap the
	// local albums heavily are not corroboration, they are ambiguity: the album
	// evidence does not discriminate between them, so neither may be adopted.
	UncontestedBest bool

	// RedFlag carries a SECONDARY disqualifier discovered outside the album
	// comparison, most often a disambiguation string marking the candidate as a
	// tribute or karaoke act. It is deliberately secondary: a tribute band with
	// a real catalogue can overlap a local library heavily (it releases covers
	// of the same albums), so the album comparison alone cannot catch it, but a
	// disambiguation string is also weak evidence that must never be the ONLY
	// thing standing between a candidate and adoption. A red flag therefore
	// blocks the unattended write and routes to a human; it does not by itself
	// throw the candidate away.
	RedFlag string
}

// EvaluateAlbumGate decides whether an automated pass may adopt a candidate,
// returning the decision plus an operator-facing reason.
//
// The decision table, and why each row lands where it does:
//
//	Evidence  | Candidate catalogue | Overlap | Decision
//	----------+---------------------+---------+---------
//	Unknown   | n/a                 | n/a     | DECLINE
//	Found     | not retrieved       | n/a     | DECLINE
//	Found     | 0 release groups    | n/a     | DECLINE
//	Found     | >= 1                | >= 70   | permit (if uncontested, no red flag)
//	Found     | >= 1                | >= 30   | review
//	Found     | >= 1                | < 30    | decline
//	None      | anything            | n/a     | decline
//
// THE UNKNOWN ROW IS THE WHOLE POINT. Before this gate existed, an artist whose
// album directory could not be read did not merely fail to object to a
// candidate -- the absence of a local album list ROUTED the artist to the one
// tier that performs no catalogue check at all, so "we could not look" acted as
// permission. 43% of a production library is in that state. A predicate that is
// safe for deciding whether to WRITE becomes unsafe the moment its absent case
// is read as a yes.
//
// The two EvidenceNone rows collapse to the same answer for a reason worth
// stating: when the artist genuinely has no albums and the candidate also has
// none, that is not agreement. There is no evidence on either side, so there is
// nothing to agree about, and "two empty sets are equal" is arithmetic, not
// corroboration. An operator can still link such an artist by hand.
func EvaluateAlbumGate(in AlbumGateInput) (AlbumGateDecision, string) {
	switch in.Evidence {
	case EvidenceUnknown:
		return AlbumGateDecline, "local albums could not be read, so the candidate's catalogue was never checked"

	case EvidenceNone:
		// Deliberately one branch, not two. See the doc comment: an empty local
		// catalogue is not evidence for ANY candidate, whether or not the
		// candidate has albums of its own.
		return AlbumGateDecline, "the artist has no local albums, so there is no catalogue evidence to weigh"

	case EvidenceFound:
		// Fall through to the candidate-side checks below.

	default:
		// Unreachable today. An AlbumEvidence value this function does not know
		// is a new state somebody added without revisiting the gate, and the
		// safe answer to "I do not recognize this" is the same as the answer to
		// "I could not look".
		return AlbumGateDecline, fmt.Sprintf("unrecognized album evidence state %s", in.Evidence)
	}

	if !in.CandidateReleasesKnown {
		return AlbumGateDecline, "the candidate's release groups could not be retrieved, so the catalogues were never compared"
	}

	if in.CandidateReleaseCount == 0 {
		// The measured 18/18 case. An entity with no release groups matches
		// every name query it is returned for and can never be contradicted by
		// a catalogue comparison, so a name-only gate always waves it through.
		return AlbumGateDecline, "the candidate carries no release groups, so it cannot corroborate the artist's albums"
	}

	if in.OverlapPercent >= AlbumOverlapAutoLinkFloor {
		switch {
		case !in.UncontestedBest:
			return AlbumGateReview, fmt.Sprintf(
				"several candidates overlap the local albums by %d%% or more, so the catalogue evidence does not discriminate between them",
				AlbumOverlapAutoLinkFloor)
		case in.RedFlag != "":
			return AlbumGateReview, fmt.Sprintf(
				"catalogue overlap is %d%% but the candidate is flagged as %s, which needs a human",
				in.OverlapPercent, in.RedFlag)
		default:
			return AlbumGatePermit, fmt.Sprintf(
				"catalogue overlap is %d%%, at or above the %d%% floor, and no rival candidate matched",
				in.OverlapPercent, AlbumOverlapAutoLinkFloor)
		}
	}

	if in.OverlapPercent >= AlbumOverlapReviewFloor {
		return AlbumGateReview, fmt.Sprintf(
			"catalogue overlap is %d%%, below the %d%% auto-link floor but worth review",
			in.OverlapPercent, AlbumOverlapAutoLinkFloor)
	}

	return AlbumGateDecline, fmt.Sprintf(
		"catalogue overlap is %d%%, below the %d%% review floor",
		in.OverlapPercent, AlbumOverlapReviewFloor)
}

// candidateRedFlagWords are disambiguation substrings that mark a MusicBrainz
// entity as something other than the artist an operator meant to link.
//
// They are matched against the candidate's Disambiguation field only, which
// MusicBrainz uses precisely to tell same-named entities apart, so a hit is a
// statement by the source rather than an inference of ours. The list is short
// and literal on purpose: a broader heuristic (say, matching the words anywhere
// in the artist NAME) would fire on legitimate artists whose name contains one
// of these words.
var candidateRedFlagWords = []string{
	"tribute",
	"cover band",
	"karaoke",
	"soundalike",
}

// CandidateRedFlag returns a short description of why a search hit looks like
// the wrong ENTITY, or "" when nothing is flagged.
//
// This is a SECONDARY signal by design (see AlbumGateInput.RedFlag). A tribute
// act's catalogue legitimately overlaps the original's, so the album comparison
// cannot catch it; but a disambiguation string is free-form text authored by
// volunteers, so it is not solid enough to throw a candidate away on its own.
// The gate uses it to withhold the UNATTENDED write and route to a human, which
// is the level of confidence the signal actually supports.
func CandidateRedFlag(res *provider.ArtistSearchResult) string {
	if res == nil {
		return ""
	}
	dis := strings.ToLower(res.Disambiguation)
	for _, word := range candidateRedFlagWords {
		if strings.Contains(dis, word) {
			return fmt.Sprintf("a %s act", word)
		}
	}
	return ""
}
