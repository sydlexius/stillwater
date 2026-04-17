package rule

import (
	"context"
	"testing"
)

// TestCountActiveViolationsForArtist exercises the per-artist count used by
// the artist detail tab badge. Verifies it counts only open and pending_choice
// rows for the requested artist, ignoring other artists and dismissed/resolved.
func TestCountActiveViolationsForArtist(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	const target = "artist-target"
	const other = "artist-other"

	seed := []*RuleViolation{
		{RuleID: RuleNFOExists, ArtistID: target, ArtistName: "Target", Severity: "error", Message: "open", Status: ViolationStatusOpen},
		{RuleID: RuleBioExists, ArtistID: target, ArtistName: "Target", Severity: "info", Message: "pending", Status: ViolationStatusPendingChoice,
			Candidates: []ImageCandidate{{URL: "http://example.com/x.jpg", ImageType: "thumb"}}},
		// Dismissed and resolved for the same artist must not be counted.
		{RuleID: RuleNFOHasMBID, ArtistID: target, ArtistName: "Target", Severity: "info", Message: "dismissed", Status: ViolationStatusDismissed},
		{RuleID: RuleLogoPadding, ArtistID: target, ArtistName: "Target", Severity: "warning", Message: "resolved", Status: ViolationStatusResolved},
		// Other artist's open violation must not leak into the target's count.
		{RuleID: RuleNFOExists, ArtistID: other, ArtistName: "Other", Severity: "error", Message: "other-open", Status: ViolationStatusOpen},
	}
	for _, v := range seed {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	got, err := svc.CountActiveViolationsForArtist(ctx, target)
	if err != nil {
		t.Fatalf("CountActiveViolationsForArtist: %v", err)
	}
	if got != 2 {
		t.Errorf("count = %d, want 2 (open + pending_choice)", got)
	}

	empty, err := svc.CountActiveViolationsForArtist(ctx, "artist-none")
	if err != nil {
		t.Fatalf("CountActiveViolationsForArtist(artist-none): %v", err)
	}
	if empty != 0 {
		t.Errorf("count for unknown artist = %d, want 0", empty)
	}
}

// TestListViolationsFiltered_ArtistID verifies that passing ArtistID limits
// results to that artist's rows, intersected with the existing Status filter.
func TestListViolationsFiltered_ArtistID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	const target = "art-target"
	const other = "art-other"

	seed := []*RuleViolation{
		{RuleID: RuleNFOExists, ArtistID: target, ArtistName: "Target", Severity: "error", Message: "t1", Status: ViolationStatusOpen},
		{RuleID: RuleBioExists, ArtistID: target, ArtistName: "Target", Severity: "info", Message: "t2", Status: ViolationStatusDismissed},
		{RuleID: RuleNFOExists, ArtistID: other, ArtistName: "Other", Severity: "error", Message: "o1", Status: ViolationStatusOpen},
	}
	for _, v := range seed {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}

	active, err := svc.ListViolationsFiltered(ctx, ViolationListParams{
		ArtistID: target,
		Status:   "active",
	})
	if err != nil {
		t.Fatalf("ListViolationsFiltered: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active count = %d, want 1; rows: %+v", len(active), active)
	}
	if active[0].ArtistID != target || active[0].RuleID != RuleNFOExists {
		t.Errorf("got %+v, want target/NFOExists", active[0])
	}

	// Status "" returns all rows for that artist, including dismissed.
	all, err := svc.ListViolationsFiltered(ctx, ViolationListParams{ArtistID: target})
	if err != nil {
		t.Fatalf("ListViolationsFiltered(all): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("all count = %d, want 2; rows: %+v", len(all), all)
	}
}
