package artist

import (
	"context"
	"testing"
)

func TestHealthStats_EmptyDB(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	stats, err := svc.GetHealthStats(ctx, "")
	if err != nil {
		t.Fatalf("GetHealthStats: %v", err)
	}

	if stats.TotalArtists != 0 {
		t.Errorf("TotalArtists = %d, want 0", stats.TotalArtists)
	}
	if stats.CompliantArtists != 0 {
		t.Errorf("CompliantArtists = %d, want 0", stats.CompliantArtists)
	}
	// AVG of zero rows returns NULL, which defaults to 100.0
	if stats.Score != 100.0 {
		t.Errorf("Score = %f, want 100.0", stats.Score)
	}
}

func TestHealthStats_MixedArtists(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Compliant artist: has NFO, thumb, fanart, MBID, score = 100
	compliant := &Artist{
		Name:          "Compliant",
		SortName:      "Compliant",
		Path:          "/music/compliant",
		NFOExists:     true,
		ThumbExists:   true,
		FanartExists:  true,
		MusicBrainzID: "aaaaaaaa-0000-0000-0000-000000000001",
		HealthScore:   100.0,
	}
	if err := svc.Create(ctx, compliant); err != nil {
		t.Fatalf("creating compliant artist: %v", err)
	}

	// Non-compliant artist: missing NFO, thumb, fanart, MBID, score = 40
	nonCompliant := &Artist{
		Name:        "Non-Compliant",
		SortName:    "Non-Compliant",
		Path:        "/music/non-compliant",
		HealthScore: 40.0,
	}
	if err := svc.Create(ctx, nonCompliant); err != nil {
		t.Fatalf("creating non-compliant artist: %v", err)
	}

	stats, err := svc.GetHealthStats(ctx, "")
	if err != nil {
		t.Fatalf("GetHealthStats: %v", err)
	}

	if stats.TotalArtists != 2 {
		t.Errorf("TotalArtists = %d, want 2", stats.TotalArtists)
	}
	if stats.CompliantArtists != 1 {
		t.Errorf("CompliantArtists = %d, want 1", stats.CompliantArtists)
	}
	// AVG(100, 40) = 70.0
	if stats.Score != 70.0 {
		t.Errorf("Score = %f, want 70.0", stats.Score)
	}
	if stats.MissingNFO != 1 {
		t.Errorf("MissingNFO = %d, want 1", stats.MissingNFO)
	}
	if stats.MissingThumb != 1 {
		t.Errorf("MissingThumb = %d, want 1", stats.MissingThumb)
	}
	if stats.MissingFanart != 1 {
		t.Errorf("MissingFanart = %d, want 1", stats.MissingFanart)
	}
	if stats.MissingMBID != 1 {
		t.Errorf("MissingMBID = %d, want 1", stats.MissingMBID)
	}
}

func TestHealthStats_ExcludedArtistsOmitted(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	included := &Artist{
		Name:        "Included",
		SortName:    "Included",
		Path:        "/music/included",
		HealthScore: 80.0,
	}
	if err := svc.Create(ctx, included); err != nil {
		t.Fatalf("creating included artist: %v", err)
	}

	excluded := &Artist{
		Name:            "Excluded",
		SortName:        "Excluded",
		Path:            "/music/excluded",
		IsExcluded:      true,
		ExclusionReason: "test",
		HealthScore:     10.0,
	}
	if err := svc.Create(ctx, excluded); err != nil {
		t.Fatalf("creating excluded artist: %v", err)
	}

	stats, err := svc.GetHealthStats(ctx, "")
	if err != nil {
		t.Fatalf("GetHealthStats: %v", err)
	}

	if stats.TotalArtists != 1 {
		t.Errorf("TotalArtists = %d, want 1 (excluded should be omitted)", stats.TotalArtists)
	}
	if stats.Score != 80.0 {
		t.Errorf("Score = %f, want 80.0", stats.Score)
	}
}

func TestHealthStats_LibraryFilter(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	lib1 := &Artist{
		Name:        "Library1 Artist",
		SortName:    "Library1 Artist",
		Path:        "/music/lib1/artist",
		LibraryID:   "lib-1",
		HealthScore: 90.0,
	}
	if err := svc.Create(ctx, lib1); err != nil {
		t.Fatalf("creating lib1 artist: %v", err)
	}

	lib2 := &Artist{
		Name:        "Library2 Artist",
		SortName:    "Library2 Artist",
		Path:        "/music/lib2/artist",
		LibraryID:   "lib-2",
		HealthScore: 50.0,
	}
	if err := svc.Create(ctx, lib2); err != nil {
		t.Fatalf("creating lib2 artist: %v", err)
	}

	// Filter to lib-1
	stats, err := svc.GetHealthStats(ctx, "lib-1")
	if err != nil {
		t.Fatalf("GetHealthStats(lib-1): %v", err)
	}

	if stats.TotalArtists != 1 {
		t.Errorf("TotalArtists = %d, want 1", stats.TotalArtists)
	}
	if stats.Score != 90.0 {
		t.Errorf("Score = %f, want 90.0", stats.Score)
	}

	// All libraries
	all, err := svc.GetHealthStats(ctx, "")
	if err != nil {
		t.Fatalf("GetHealthStats(all): %v", err)
	}

	if all.TotalArtists != 2 {
		t.Errorf("TotalArtists (all) = %d, want 2", all.TotalArtists)
	}
}

func TestListUnevaluatedIDs(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Never evaluated: health_evaluated_at IS NULL -- should be returned.
	neverEvaluated := &Artist{
		Name:        "Never Evaluated",
		SortName:    "Never Evaluated",
		Path:        "/music/never-evaluated",
		HealthScore: 0.0,
	}
	if err := svc.Create(ctx, neverEvaluated); err != nil {
		t.Fatalf("creating never-evaluated artist: %v", err)
	}

	// Evaluated with score 0: has health_evaluated_at set -- should NOT be returned.
	evaluatedZero := &Artist{
		Name:        "Evaluated Zero",
		SortName:    "Evaluated Zero",
		Path:        "/music/evaluated-zero",
		HealthScore: 0.0,
	}
	if err := svc.Create(ctx, evaluatedZero); err != nil {
		t.Fatalf("creating evaluated-zero artist: %v", err)
	}
	// Simulate evaluation by calling UpdateHealthScore (sets health_evaluated_at).
	if err := svc.UpdateHealthScore(ctx, evaluatedZero.ID, 0.0); err != nil {
		t.Fatalf("UpdateHealthScore for evaluated-zero: %v", err)
	}

	// Evaluated with non-zero score -- should NOT be returned.
	evaluatedNonZero := &Artist{
		Name:        "Evaluated NonZero",
		SortName:    "Evaluated NonZero",
		Path:        "/music/evaluated-nonzero",
		HealthScore: 75.0,
	}
	if err := svc.Create(ctx, evaluatedNonZero); err != nil {
		t.Fatalf("creating evaluated-nonzero artist: %v", err)
	}
	if err := svc.UpdateHealthScore(ctx, evaluatedNonZero.ID, 75.0); err != nil {
		t.Fatalf("UpdateHealthScore for evaluated-nonzero: %v", err)
	}

	// Excluded and never evaluated -- should NOT be returned (excluded).
	excludedNeverEvaluated := &Artist{
		Name:            "Excluded Never Evaluated",
		SortName:        "Excluded Never Evaluated",
		Path:            "/music/excluded-never",
		HealthScore:     0.0,
		IsExcluded:      true,
		ExclusionReason: "test",
	}
	if err := svc.Create(ctx, excludedNeverEvaluated); err != nil {
		t.Fatalf("creating excluded never-evaluated artist: %v", err)
	}

	ids, err := svc.ListUnevaluatedIDs(ctx)
	if err != nil {
		t.Fatalf("ListUnevaluatedIDs: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("len(ids) = %d, want 1 (only non-excluded never-evaluated)", len(ids))
	}
	if ids[0] != neverEvaluated.ID {
		t.Errorf("id = %q, want %q", ids[0], neverEvaluated.ID)
	}
}
