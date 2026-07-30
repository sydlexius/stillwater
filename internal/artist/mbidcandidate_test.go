package artist

import (
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

// TestEvaluateMBIDCandidateNilBestDeclines pins the absent-candidate case.
//
// BestMBIDCandidates returns a nil best when no search hit carried a usable
// MusicBrainz ID, and this gate is reachable from packages whose callers did not
// write the nil check that its original single caller happened to have. The
// correct answer is to DECLINE, not to panic and not to fall through: "no
// candidate" is not "a candidate that passed", and this gate exists precisely so
// the absent case cannot become a write.
//
// Written here rather than in internal/rule because the code now lives in this
// package, and a caller in a third package must be able to rely on the contract
// without depending on the rule package's tests.
func TestEvaluateMBIDCandidateNilBestDeclines(t *testing.T) {
	rej := EvaluateMBIDCandidate("Radiohead", nil, nil)
	if rej == nil {
		t.Fatal("EvaluateMBIDCandidate(nil best) returned nil, want a rejection: an absent candidate must decline, never pass")
	}
	if rej.Reason == "" {
		t.Error("Reason is empty; a rejection must say why so an operator log is actionable")
	}
}

// TestEvaluateMBIDCandidateNilBestDoesNotPanic states the panic-freedom half
// separately from the decline half. A future refactor that returned a rejection
// for the wrong reason would still be caught above; this one catches the case
// where the nil guard is removed entirely.
func TestEvaluateMBIDCandidateNilBestDoesNotPanic(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("EvaluateMBIDCandidate panicked on a nil best: %v", p)
		}
	}()
	_ = EvaluateMBIDCandidate("Radiohead", nil, &provider.ArtistSearchResult{
		Name: "Radiohead", MusicBrainzID: "a74b1b7f-71a5-4011-9441-d0b5e4122711", Score: 100,
	})
}

// TestEvaluateMBIDCandidateAcceptsAConfidentUncontestedHit is the precondition
// for the tests above: it proves the gate can PASS, so a nil-best rejection is
// evidence about the nil case rather than about a gate that rejects everything.
func TestEvaluateMBIDCandidateAcceptsAConfidentUncontestedHit(t *testing.T) {
	best := &provider.ArtistSearchResult{
		Name:          "Radiohead",
		MusicBrainzID: "a74b1b7f-71a5-4011-9441-d0b5e4122711",
		Score:         100,
		Source:        "musicbrainz",
	}
	if rej := EvaluateMBIDCandidate("Radiohead", best, nil); rej != nil {
		t.Fatalf("precondition failed: a score-100 exact-name uncontested hit was rejected: %s", rej.Reason)
	}
}

// TestEvaluateMBIDCandidateRejectsBelowScoreFloor keeps one threshold case in
// this package so the lifted constants are exercised from their new home, not
// only through the rule package that used to own them.
func TestEvaluateMBIDCandidateRejectsBelowScoreFloor(t *testing.T) {
	best := &provider.ArtistSearchResult{
		Name:          "Radiohead",
		MusicBrainzID: "a74b1b7f-71a5-4011-9441-d0b5e4122711",
		Score:         MBIDMinProviderScore - 1,
		Source:        "musicbrainz",
	}
	rej := EvaluateMBIDCandidate("Radiohead", best, nil)
	if rej == nil {
		t.Fatal("a hit one point below the score floor was accepted; the floor is not being applied")
	}
	if !strings.Contains(rej.Reason, "confidence floor") {
		t.Errorf("Reason = %q, want it to name the confidence floor that rejected it", rej.Reason)
	}
}
