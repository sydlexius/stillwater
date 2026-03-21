package artist

import (
	"context"
	"testing"
)

// insertCompletenessArtist creates an artist with the given fields for use in
// completeness repository tests. It uses the full Service.Create path so that
// the artist row is properly normalised (provider IDs, image metadata).
func insertCompletenessArtist(t *testing.T, svc *Service, a *Artist) {
	t.Helper()
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist %q: %v", a.Name, err)
	}
}

// TestGetCompletenessRows_BasicFlags verifies that NFO, MBID, image, and text
// field flags are populated correctly from a real SQLite row.
func TestGetCompletenessRows_BasicFlags(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := &Artist{
		Name:      "Test Group",
		SortName:  "Test Group",
		Type:      "group",
		Path:      "/music/test-group",
		Formed:    "1990",
		Biography: "A biography.",
		Genres:    []string{"Rock"},
		Styles:    []string{"Alternative"},
		NFOExists: true,
		// ThumbExists will be set via image metadata
		ThumbExists:  true,
		FanartExists: true,
	}
	insertCompletenessArtist(t, svc, a)

	// Set a MusicBrainz ID so has_mbid becomes true.
	a.MusicBrainzID = "aaaaaaaa-0000-0000-0000-000000000001"
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("updating artist: %v", err)
	}

	repo := newSQLiteCompletenessRepo(db)
	rows, err := repo.GetCompletenessRows(ctx, "")
	if err != nil {
		t.Fatalf("GetCompletenessRows: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	row := rows[0]

	if row.Name != "Test Group" {
		t.Errorf("Name = %q, want %q", row.Name, "Test Group")
	}
	if row.Type != "group" {
		t.Errorf("Type = %q, want %q", row.Type, "group")
	}
	if !row.NFOExists {
		t.Error("NFOExists = false, want true")
	}
	if !row.HasMBID {
		t.Error("HasMBID = false, want true")
	}
	if !row.HasThumb {
		t.Error("HasThumb = false, want true")
	}
	if !row.HasFanart {
		t.Error("HasFanart = false, want true")
	}
	if row.Biography == "" {
		t.Error("Biography is empty, want non-empty")
	}
	if row.Formed != "1990" {
		t.Errorf("Formed = %q, want %q", row.Formed, "1990")
	}
}

// TestGetCompletenessRows_ExcludedArtists verifies that artists marked
// is_excluded = 1 are omitted from completeness results.
func TestGetCompletenessRows_ExcludedArtists(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	included := &Artist{
		Name:     "Included Artist",
		SortName: "Included Artist",
		Type:     "group",
		Path:     "/music/included",
	}
	insertCompletenessArtist(t, svc, included)

	excluded := &Artist{
		Name:            "Various Artists",
		SortName:        "Various Artists",
		Type:            "group",
		Path:            "/music/various",
		IsExcluded:      true,
		ExclusionReason: "scanner exclusion",
	}
	insertCompletenessArtist(t, svc, excluded)

	repo := newSQLiteCompletenessRepo(db)
	rows, err := repo.GetCompletenessRows(ctx, "")
	if err != nil {
		t.Fatalf("GetCompletenessRows: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 (excluded artist should be omitted)", len(rows))
	}
	if rows[0].Name != "Included Artist" {
		t.Errorf("rows[0].Name = %q, want %q", rows[0].Name, "Included Artist")
	}
}

// TestGetCompletenessRows_LibraryFilter verifies that the library_id filter
// restricts results to the specified library.
func TestGetCompletenessRows_LibraryFilter(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	libA := &Artist{
		Name:      "Library A Artist",
		SortName:  "Library A Artist",
		Type:      "group",
		Path:      "/music/lib-a/artist",
		LibraryID: "lib-a",
	}
	insertCompletenessArtist(t, svc, libA)

	libB := &Artist{
		Name:      "Library B Artist",
		SortName:  "Library B Artist",
		Type:      "person",
		Path:      "/music/lib-b/artist",
		LibraryID: "lib-b",
	}
	insertCompletenessArtist(t, svc, libB)

	repo := newSQLiteCompletenessRepo(db)

	rowsA, err := repo.GetCompletenessRows(ctx, "lib-a")
	if err != nil {
		t.Fatalf("GetCompletenessRows(lib-a): %v", err)
	}
	if len(rowsA) != 1 {
		t.Fatalf("len(rowsA) = %d, want 1", len(rowsA))
	}
	if rowsA[0].Name != "Library A Artist" {
		t.Errorf("rowsA[0].Name = %q, want %q", rowsA[0].Name, "Library A Artist")
	}

	// Empty library_id returns all non-excluded artists.
	rowsAll, err := repo.GetCompletenessRows(ctx, "")
	if err != nil {
		t.Fatalf("GetCompletenessRows(all): %v", err)
	}
	if len(rowsAll) != 2 {
		t.Fatalf("len(rowsAll) = %d, want 2", len(rowsAll))
	}
}

// TestGetLowestCompleteness_Ordering verifies that artists are returned ordered
// by health_score ascending (lowest score first).
func TestGetLowestCompleteness_Ordering(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	artistLow := &Artist{
		Name:        "Low Score",
		SortName:    "Low Score",
		Type:        "group",
		Path:        "/music/low",
		HealthScore: 10,
	}
	insertCompletenessArtist(t, svc, artistLow)

	artistHigh := &Artist{
		Name:        "High Score",
		SortName:    "High Score",
		Type:        "group",
		Path:        "/music/high",
		HealthScore: 90,
	}
	insertCompletenessArtist(t, svc, artistHigh)

	artistMid := &Artist{
		Name:        "Mid Score",
		SortName:    "Mid Score",
		Type:        "group",
		Path:        "/music/mid",
		HealthScore: 50,
	}
	insertCompletenessArtist(t, svc, artistMid)

	repo := newSQLiteCompletenessRepo(db)
	results, err := repo.GetLowestCompleteness(ctx, "", 10)
	if err != nil {
		t.Fatalf("GetLowestCompleteness: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}

	// Results must be ascending by health_score.
	for i := 1; i < len(results); i++ {
		if results[i].HealthScore < results[i-1].HealthScore {
			t.Errorf("results not sorted ascending at index %d: %.1f < %.1f",
				i, results[i].HealthScore, results[i-1].HealthScore)
		}
	}
	if results[0].Name != "Low Score" {
		t.Errorf("results[0].Name = %q, want %q", results[0].Name, "Low Score")
	}
}

// TestGetLowestCompleteness_DefaultLimit verifies that limit <= 0 defaults to 10.
func TestGetLowestCompleteness_DefaultLimit(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Insert 15 artists.
	for i := 0; i < 15; i++ {
		a := &Artist{
			Name:     "Artist",
			SortName: "Artist",
			Type:     "group",
			Path:     "/music/artist",
		}
		// Make names/paths unique.
		a.Name = a.Name + string(rune('A'+i))
		a.SortName = a.Name
		a.Path = a.Path + string(rune('A'+i))
		insertCompletenessArtist(t, svc, a)
	}

	repo := newSQLiteCompletenessRepo(db)

	// limit = 0 should default to 10.
	results, err := repo.GetLowestCompleteness(ctx, "", 0)
	if err != nil {
		t.Fatalf("GetLowestCompleteness(limit=0): %v", err)
	}
	if len(results) != 10 {
		t.Errorf("len(results) = %d, want 10 (default limit)", len(results))
	}

	// Negative limit should also default to 10.
	results2, err := repo.GetLowestCompleteness(ctx, "", -1)
	if err != nil {
		t.Fatalf("GetLowestCompleteness(limit=-1): %v", err)
	}
	if len(results2) != 10 {
		t.Errorf("len(results2) = %d, want 10 (negative limit defaults to 10)", len(results2))
	}
}

// TestGetLowestCompleteness_ExcludedOmitted verifies that excluded artists are
// not included in the lowest completeness results.
func TestGetLowestCompleteness_ExcludedOmitted(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	included := &Artist{
		Name:        "Normal Artist",
		SortName:    "Normal Artist",
		Type:        "group",
		Path:        "/music/normal",
		HealthScore: 30,
	}
	insertCompletenessArtist(t, svc, included)

	excluded := &Artist{
		Name:            "Various Artists",
		SortName:        "Various Artists",
		Type:            "group",
		Path:            "/music/various",
		HealthScore:     0,
		IsExcluded:      true,
		ExclusionReason: "scanner exclusion",
	}
	insertCompletenessArtist(t, svc, excluded)

	repo := newSQLiteCompletenessRepo(db)
	results, err := repo.GetLowestCompleteness(ctx, "", 10)
	if err != nil {
		t.Fatalf("GetLowestCompleteness: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1 (excluded artist must not appear)", len(results))
	}
	if results[0].Name != "Normal Artist" {
		t.Errorf("results[0].Name = %q, want %q", results[0].Name, "Normal Artist")
	}
}

// TestGetLowestCompleteness_LibraryFilter verifies that library_id filters
// the lowest completeness results.
func TestGetLowestCompleteness_LibraryFilter(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a1 := &Artist{
		Name:        "Lib A Artist",
		SortName:    "Lib A Artist",
		Type:        "group",
		Path:        "/music/lib-a/a1",
		LibraryID:   "lib-a",
		HealthScore: 20,
	}
	insertCompletenessArtist(t, svc, a1)

	a2 := &Artist{
		Name:        "Lib B Artist",
		SortName:    "Lib B Artist",
		Type:        "group",
		Path:        "/music/lib-b/a2",
		LibraryID:   "lib-b",
		HealthScore: 10,
	}
	insertCompletenessArtist(t, svc, a2)

	repo := newSQLiteCompletenessRepo(db)
	results, err := repo.GetLowestCompleteness(ctx, "lib-a", 10)
	if err != nil {
		t.Fatalf("GetLowestCompleteness(lib-a): %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Name != "Lib A Artist" {
		t.Errorf("results[0].Name = %q, want %q", results[0].Name, "Lib A Artist")
	}
}
