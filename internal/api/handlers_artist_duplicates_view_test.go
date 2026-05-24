package api

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestBuildArtistDuplicatesView_RecommendsSurvivor pins the contract between
// the detection layer and the page view-model: exactly one member per group
// carries Recommended=true, and the reason string mirrors what
// artist.ChooseSurvivor returns. The duplicates UI uses both fields to mark
// the recommended row's badge and tooltip; if this drifts, the user can no
// longer tell which artist the merge endpoint will pick by default.
func TestBuildArtistDuplicatesView_RecommendsSurvivor(t *testing.T) {
	groups := []artist.NearDuplicateGroup{
		{
			Key:    "the cure",
			Reason: "name_key",
			Members: []artist.NearDuplicateArtist{
				// Non-canonical basename; would lose precedence-a.
				{ID: "id-a", Name: "The Cure", Path: "/music/Cure"},
				// Canonical basename ("The Cure"); should win.
				{ID: "id-b", Name: "The Cure", Path: "/music/The Cure"},
			},
		},
	}

	view := buildArtistDuplicatesView(groups, "prefix")
	if len(view.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(view.Groups))
	}
	g := view.Groups[0]
	if len(g.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(g.Members))
	}

	var recommended int
	for _, m := range g.Members {
		if m.Recommended {
			recommended++
			if m.ID != "id-b" {
				t.Errorf("recommended member id = %q, want %q", m.ID, "id-b")
			}
			if m.RecommendedReason != "canonical_basename" {
				t.Errorf("recommended reason = %q, want %q",
					m.RecommendedReason, "canonical_basename")
			}
		}
	}
	if recommended != 1 {
		t.Errorf("recommended count = %d, want 1", recommended)
	}
}

// TestBuildArtistDuplicatesView_EmptyGroups guards the no-duplicates path:
// the view must round-trip an empty slice without panicking on a missing
// recommended survivor (ChooseSurvivor returns "" for empty members).
func TestBuildArtistDuplicatesView_EmptyGroups(t *testing.T) {
	view := buildArtistDuplicatesView(nil, "")
	if view.Groups == nil {
		t.Errorf("Groups slice should be non-nil even when empty")
	}
	if len(view.Groups) != 0 {
		t.Errorf("expected 0 groups, got %d", len(view.Groups))
	}
}
