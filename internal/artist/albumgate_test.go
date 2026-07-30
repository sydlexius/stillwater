package artist

import (
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// foundGate builds the gate input for a healthy comparison: a real local
// determination and a candidate whose catalogue was retrieved. Subtests
// override only the axis they are discriminating on, so a fixture never
// coincidentally agrees along two axes at once.
func foundGate(overlap, releases int) AlbumGateInput {
	return AlbumGateInput{
		Evidence:               EvidenceFound,
		OverlapPercent:         overlap,
		CandidateReleaseCount:  releases,
		CandidateReleasesKnown: true,
		UncontestedBest:        true,
	}
}

// TestEvaluateAlbumGateUnknownDeclines is THE test of issue #2828.
//
// The candidate here is as strong as a candidate can be short of the album
// check: an exact-name, uncontested match with a full catalogue of its own.
// Every name-confidence gate in mbidcandidate.go passes it. The ONLY thing that
// refuses it is the local side's Evidence being Unknown, which is the state 43%
// of a production library is in.
//
// The precondition matters as much as the assertion: the same input with
// Evidence flipped to EvidenceFound must PERMIT. Without that half, a gate that
// declined everything would pass this test while protecting nothing.
func TestEvaluateAlbumGateUnknownDeclines(t *testing.T) {
	t.Parallel()

	in := foundGate(100, 12)
	in.Evidence = EvidenceUnknown

	got, reason := EvaluateAlbumGate(in)
	if got != AlbumGateDecline {
		t.Errorf("EvaluateAlbumGate(Unknown) = %v (%s), want decline: an album set that was never read is not permission to write",
			got, reason)
	}

	// The discriminating half: identical input, Evidence Found, must permit.
	// This is what proves the decline above came from the evidence state rather
	// than from some other leg of the gate.
	in.Evidence = EvidenceFound
	if got, reason := EvaluateAlbumGate(in); got != AlbumGatePermit {
		t.Fatalf("precondition failed: the same candidate with EvidenceFound = %v (%s), want permit; "+
			"the Unknown decline above proves nothing if this input never passes", got, reason)
	}
}

// TestEvaluateAlbumGateZeroReleaseCandidateDeclines covers the measured 18/18
// case in isolation: a candidate matching the artist's name perfectly while
// carrying NO release groups at all.
//
// An entity with an empty catalogue can never be contradicted by a catalogue
// comparison, so a name-only gate waves it through every time. 100% overlap is
// deliberately paired with 0 releases here even though that pairing cannot
// arise organically: it isolates the release-count leg from the overlap leg, so
// a pass could only come from the gate ignoring the count.
func TestEvaluateAlbumGateZeroReleaseCandidateDeclines(t *testing.T) {
	t.Parallel()

	if got, reason := EvaluateAlbumGate(foundGate(100, 0)); got != AlbumGateDecline {
		t.Errorf("EvaluateAlbumGate(0 release groups) = %v (%s), want decline",
			got, reason)
	}

	// Discriminator: one release group, everything else identical, permits.
	if got, reason := EvaluateAlbumGate(foundGate(100, 1)); got != AlbumGatePermit {
		t.Fatalf("precondition failed: 1 release group = %v (%s), want permit", got, reason)
	}
}

// TestEvaluateAlbumGateUnknownCandidateCatalogueDeclines pins the
// candidate-side twin of the Unknown rule: a release-group fetch that was not a
// determination must decline, independently of what the count happens to hold.
//
// The count is deliberately NON-ZERO here. A failed fetch leaves it at zero in
// practice, so a zero fixture would be caught by the zero-release guard whether
// or not the CandidateReleasesKnown leg exists -- the two axes would coincide
// and the test would pass vacuously against a gate that ignores the flag
// entirely (measured: it did). A non-zero count is the only fixture that
// isolates this leg.
func TestEvaluateAlbumGateUnknownCandidateCatalogueDeclines(t *testing.T) {
	t.Parallel()

	in := foundGate(100, 9)
	in.CandidateReleasesKnown = false

	if got, reason := EvaluateAlbumGate(in); got != AlbumGateDecline {
		t.Errorf("EvaluateAlbumGate(catalogue not retrieved) = %v (%s), want decline", got, reason)
	}

	// Discriminator: the same non-zero count, marked as a determination, permits.
	in.CandidateReleasesKnown = true
	if got, reason := EvaluateAlbumGate(in); got != AlbumGatePermit {
		t.Fatalf("precondition failed: same count marked known = %v (%s), want permit", got, reason)
	}
}

// TestEvaluateAlbumGateEvidenceNoneDeclines covers both EvidenceNone rows, and
// it is the DIRECT guard on the EvidenceNone branch.
//
// The two-empties row is the interesting one. Arithmetically an empty local set
// and an empty candidate catalogue "agree" perfectly, and a naive comparison
// scores that 100%. It is not agreement: there is no evidence on either side,
// so there is nothing to agree about.
//
// WHY THE REASON IS ASSERTED AND NOT JUST THE DECISION. Deleting the
// `case EvidenceNone` arm entirely does not change the DECISION: execution
// falls to the default arm, which also declines. So a decision-only assertion
// passes against a gate that has lost the branch it exists to hold -- the
// mutation is decision-equivalent and only the reason string can tell the two
// apart. The default arm's reason names an unrecognized state, which is a
// materially different thing to tell an operator than "this artist has no local
// albums", so pinning the reason is what gives this test teeth.
func TestEvaluateAlbumGateEvidenceNoneDeclines(t *testing.T) {
	t.Parallel()

	// The substring the EvidenceNone arm is responsible for producing. Matched
	// as a substring rather than compared whole so rewording the sentence does
	// not fail the test, while losing the branch still does.
	const wantReasonFragment = "no local albums"

	for _, tc := range []struct {
		name string
		// releases is the CANDIDATE's release-group count.
		releases int
		// permitsUnderFound records whether the same fixture would PERMIT if the
		// local evidence were EvidenceFound.
		//
		// It is true only for the first case, and the asymmetry is the point. A
		// zero-release candidate is refused by the zero-release guard whatever
		// the local evidence says, so that fixture cannot isolate EvidenceNone
		// on the DECISION axis and it would be dishonest to assert it does. The
		// reason assertion below is what isolates it in both cases, which is
		// precisely why this test pins the reason rather than only the decision.
		permitsUnderFound bool
	}{
		{name: "candidate has albums", releases: 9, permitsUnderFound: true},
		{name: "candidate also has none", releases: 0, permitsUnderFound: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := foundGate(100, tc.releases)

			// PRECONDITION. Where the fixture CAN permit, assert that it does
			// before the evidence state is changed: otherwise a fixture that
			// already declined for an unrelated reason would make the assertion
			// below pass without EvidenceNone doing any work.
			base, baseReason := EvaluateAlbumGate(in)
			if tc.permitsUnderFound && base != AlbumGatePermit {
				t.Fatalf("precondition failed: EvidenceFound fixture = %v (%s), want permit; "+
					"an already-declining fixture cannot prove EvidenceNone is what declines", base, baseReason)
			}
			if !tc.permitsUnderFound && base != AlbumGateDecline {
				t.Fatalf("precondition failed: EvidenceFound fixture = %v (%s), want decline; "+
					"this case is declared non-isolating on the decision axis", base, baseReason)
			}

			in.Evidence = EvidenceNone
			got, reason := EvaluateAlbumGate(in)
			if got != AlbumGateDecline {
				t.Errorf("EvaluateAlbumGate(EvidenceNone, %d candidate releases) = %v (%s), want decline",
					tc.releases, got, reason)
			}
			if !strings.Contains(reason, wantReasonFragment) {
				t.Errorf("EvaluateAlbumGate(EvidenceNone, %d candidate releases) reason = %q, want it to contain %q; "+
					"a decline from the default arm is not the EvidenceNone branch doing its job",
					tc.releases, reason, wantReasonFragment)
			}
		})
	}
}

// TestEvaluateAlbumGateOverlapThresholds pins the three overlap bands against
// the shared constants rather than against literals, so re-tuning a constant
// moves the test with the code instead of leaving a stale number behind.
func TestEvaluateAlbumGateOverlapThresholds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		overlap int
		want    AlbumGateDecision
	}{
		{name: "at the auto-link floor permits", overlap: AlbumOverlapAutoLinkFloor, want: AlbumGatePermit},
		{name: "one below the auto-link floor reviews", overlap: AlbumOverlapAutoLinkFloor - 1, want: AlbumGateReview},
		{name: "at the review floor reviews", overlap: AlbumOverlapReviewFloor, want: AlbumGateReview},
		{name: "one below the review floor declines", overlap: AlbumOverlapReviewFloor - 1, want: AlbumGateDecline},
		{name: "no overlap declines", overlap: 0, want: AlbumGateDecline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, reason := EvaluateAlbumGate(foundGate(tc.overlap, 5)); got != tc.want {
				t.Errorf("overlap %d%% = %v (%s), want %v", tc.overlap, got, reason, tc.want)
			}
		})
	}
}

// TestEvaluateAlbumGateContestedBestReviews: two candidates whose catalogues
// both clear the auto-link floor are ambiguity, not corroboration. The album
// evidence cannot discriminate between them, so neither may be written
// unattended.
func TestEvaluateAlbumGateContestedBestReviews(t *testing.T) {
	t.Parallel()

	in := foundGate(95, 7)
	in.UncontestedBest = false

	if got, reason := EvaluateAlbumGate(in); got != AlbumGateReview {
		t.Errorf("EvaluateAlbumGate(contested) = %v (%s), want review", got, reason)
	}
}

// TestEvaluateAlbumGateRedFlagWithheldFromAutoWrite covers the tribute-band
// case: a real catalogue that overlaps the local library heavily, because a
// tribute act genuinely releases covers of the same albums. The album
// comparison therefore CANNOT catch it, and the disambiguation red flag is the
// secondary signal that does.
//
// It routes to REVIEW rather than DECLINE on purpose. A disambiguation string
// is free-form text authored by volunteers, so it is strong enough to withhold
// an unattended write and too weak to throw a candidate away outright.
func TestEvaluateAlbumGateRedFlagWithheldFromAutoWrite(t *testing.T) {
	t.Parallel()

	in := foundGate(100, 11)
	in.RedFlag = "a tribute act"

	got, reason := EvaluateAlbumGate(in)
	if got != AlbumGateReview {
		t.Errorf("EvaluateAlbumGate(tribute red flag) = %v (%s), want review", got, reason)
	}

	// Discriminator: the identical candidate without the flag permits, so the
	// review above is attributable to the flag and nothing else.
	in.RedFlag = ""
	if got, reason := EvaluateAlbumGate(in); got != AlbumGatePermit {
		t.Fatalf("precondition failed: same candidate without the flag = %v (%s), want permit", got, reason)
	}
}

// TestEvaluateAlbumGateZeroValueDeclines pins the zero-value contract that
// makes a forgotten field safe: an AlbumGateInput nobody filled in must
// decline, because AlbumGateDecline and EvidenceUnknown both sit at iota 0.
func TestEvaluateAlbumGateZeroValueDeclines(t *testing.T) {
	t.Parallel()

	if got, reason := EvaluateAlbumGate(AlbumGateInput{}); got != AlbumGateDecline {
		t.Errorf("EvaluateAlbumGate(zero value) = %v (%s), want decline", got, reason)
	}
	if AlbumGateDecline != 0 {
		t.Error("AlbumGateDecline is no longer the zero value; a caller that forgets to assign a decision now authorizes a write")
	}
}

// TestEvaluateAlbumGateIgnoresAlbumSetOrigin is the SOURCE-UNIFORMITY guard.
//
// AlbumSet.Origin is diagnostics only: no confidence decision may branch on it.
// The gate cannot read Origin at all -- AlbumGateInput has no such field -- so
// this test proves the property end to end through CompareAlbumSet, which is
// the function that turns an AlbumSet into the overlap the gate reads. The same
// titles delivered under three different Origins must produce one decision.
//
// Enforcing this structurally matters because per-source exceptions are exactly
// how a "trusted source skips the check" special case gets reintroduced.
func TestEvaluateAlbumGateIgnoresAlbumSetOrigin(t *testing.T) {
	t.Parallel()

	titles := []string{"First Record", "Second Record", "Third Record"}
	remote := []string{"First Record", "Second Record", "Third Record"}

	var (
		firstDecision AlbumGateDecision
		firstOverlap  int
	)
	for i, origin := range []string{"filesystem", "peer:emby", "peer:jellyfin"} {
		set := AlbumSet{Titles: titles, Evidence: EvidenceFound, Origin: origin}
		overlap := CompareAlbumSet(set, remote).MatchPercent
		decision, reason := EvaluateAlbumGate(AlbumGateInput{
			Evidence:               set.Evidence,
			OverlapPercent:         overlap,
			CandidateReleaseCount:  len(remote),
			CandidateReleasesKnown: true,
			UncontestedBest:        true,
		})
		if i == 0 {
			firstDecision, firstOverlap = decision, overlap
			if decision != AlbumGatePermit {
				t.Fatalf("precondition failed: a full-overlap set = %v (%s), want permit; "+
					"a uniformity test over three declines proves nothing", decision, reason)
			}
			continue
		}
		if decision != firstDecision || overlap != firstOverlap {
			t.Errorf("Origin %q produced decision %v at %d%% overlap, want %v at %d%%; "+
				"Origin is diagnostics only and must never move a decision",
				origin, decision, overlap, firstDecision, firstOverlap)
		}
	}
}

// TestCandidateRedFlag covers the disambiguation scan, including the cases that
// must NOT fire: the check reads the Disambiguation field only, so an artist
// whose NAME contains a flagged word is untouched.
func TestCandidateRedFlag(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		res      *provider.ArtistSearchResult
		wantFlag bool
	}{
		{name: "nil result", res: nil, wantFlag: false},
		{
			name:     "tribute in disambiguation",
			res:      &provider.ArtistSearchResult{Name: "Northern Lights", Disambiguation: "Tribute band"},
			wantFlag: true,
		},
		{
			name:     "karaoke in disambiguation",
			res:      &provider.ArtistSearchResult{Name: "Northern Lights", Disambiguation: "karaoke versions"},
			wantFlag: true,
		},
		{
			name:     "clean disambiguation",
			res:      &provider.ArtistSearchResult{Name: "Northern Lights", Disambiguation: "UK folk group"},
			wantFlag: false,
		},
		{
			name:     "empty disambiguation",
			res:      &provider.ArtistSearchResult{Name: "Northern Lights"},
			wantFlag: false,
		},
		{
			// The signal is a claim by the SOURCE about the entity, not an
			// inference from the artist's own name. A band legitimately called
			// "Tribute" must not be flagged.
			name:     "flag word in the name only",
			res:      &provider.ArtistSearchResult{Name: "Tribute", Disambiguation: "Swedish rock group"},
			wantFlag: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CandidateRedFlag(tc.res)
			if (got != "") != tc.wantFlag {
				t.Errorf("CandidateRedFlag = %q, wantFlag = %v", got, tc.wantFlag)
			}
		})
	}
}
