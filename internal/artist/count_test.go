package artist

import (
	"context"
	"testing"
)

func TestCount_NoArtists(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	total, err := svc.Count(ctx, CountParams{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 0 {
		t.Errorf("Count = %d, want 0", total)
	}
}

func TestCount_AllArtists(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	for _, name := range []string{"Alpha", "Bravo", "Charlie"} {
		a := testArtist(name, "/music/"+name)
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	total, err := svc.Count(ctx, CountParams{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 3 {
		t.Errorf("Count = %d, want 3", total)
	}
}

func TestCount_WithLibraryFilter(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibraries(t, db, "lib-1", "lib-2")

	a1 := testArtist("Alpha", "/music/Alpha")
	a1.LibraryID = "lib-1"
	a2 := testArtist("Bravo", "/music/Bravo")
	a2.LibraryID = "lib-2"
	a3 := testArtist("Charlie", "/music/Charlie")
	a3.LibraryID = "lib-1"

	for _, a := range []*Artist{a1, a2, a3} {
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", a.Name, err)
		}
	}

	total, err := svc.Count(ctx, CountParams{LibraryID: "lib-1"})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 2 {
		t.Errorf("Count with library filter = %d, want 2", total)
	}
}

func TestCount_WithSearchQuery(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	for _, name := range []string{"Radiohead", "Rage Against the Machine", "Pink Floyd"} {
		a := testArtist(name, "/music/"+name)
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	total, err := svc.Count(ctx, CountParams{Search: "Ra"})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 2 {
		t.Errorf("Count with search = %d, want 2", total)
	}
}

func TestCount_WithExcludedFilter(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a1 := testArtist("Active", "/music/Active")
	if err := svc.Create(ctx, a1); err != nil {
		t.Fatalf("Create: %v", err)
	}

	a2 := testArtist("Excluded", "/music/Excluded")
	a2.IsExcluded = true
	if err := svc.Create(ctx, a2); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Count only excluded artists.
	total, err := svc.Count(ctx, CountParams{Filter: "excluded"})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 1 {
		t.Errorf("Count with excluded filter = %d, want 1", total)
	}

	// Count only non-excluded artists.
	total, err = svc.Count(ctx, CountParams{Filter: "not_excluded"})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 1 {
		t.Errorf("Count with not_excluded filter = %d, want 1", total)
	}
}

func TestCount_ConsistentWithList(t *testing.T) {
	t.Parallel()
	// Verify that Count returns the same total as List for several filter
	// shapes. This catches drift between buildWhereClause and toListParams.
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibraries(t, db, "lib-1", "lib-2")

	a1 := testArtist("Alpha", "/music/Alpha")
	a1.LibraryID = "lib-1"

	a2 := testArtist("Bravo", "/music/Bravo")
	a2.LibraryID = "lib-2"
	a2.IsExcluded = true

	a3 := testArtist("Charlie", "/music/Charlie")
	a3.LibraryID = "lib-1"
	a3.IsExcluded = true

	for _, a := range []*Artist{a1, a2, a3} {
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", a.Name, err)
		}
	}

	tests := []struct {
		name  string
		count CountParams
		list  ListParams
	}{
		{
			name:  "library_id",
			count: CountParams{LibraryID: "lib-1"},
			list: ListParams{
				Page:      1,
				PageSize:  50,
				LibraryID: "lib-1",
			},
		},
		{
			name:  "excluded",
			count: CountParams{Filter: "excluded"},
			list: ListParams{
				Page:     1,
				PageSize: 50,
				Filter:   "excluded",
			},
		},
		{
			name:  "not_excluded",
			count: CountParams{Filter: "not_excluded"},
			list: ListParams{
				Page:     1,
				PageSize: 50,
				Filter:   "not_excluded",
			},
		},
		{
			name:  "library_id_and_excluded",
			count: CountParams{LibraryID: "lib-1", Filter: "excluded"},
			list: ListParams{
				Page:      1,
				PageSize:  50,
				LibraryID: "lib-1",
				Filter:    "excluded",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			countTotal, err := svc.Count(ctx, tt.count)
			if err != nil {
				t.Fatalf("Count: %v", err)
			}

			_, listTotal, err := svc.List(ctx, tt.list)
			if err != nil {
				t.Fatalf("List: %v", err)
			}

			if countTotal != listTotal {
				t.Errorf("Count = %d, List total = %d; they should match", countTotal, listTotal)
			}
		})
	}
}

// TestCount_WithLibraryFlyoutFilters covers the M:N membership EXISTS/NOT
// EXISTS clauses emitted by buildWhereClause for the per-library include and
// exclude flyout filters (Filters["library_<id>"] = include|exclude). After
// the legacy library_id column was dropped in migration 004, these clauses
// must filter via artist_libraries; this test pins the M:N behavior so it
// cannot silently regress.
func TestCount_WithLibraryFlyoutFilters(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibraries(t, db, "lib-a", "lib-b", "lib-c")

	// art-a only in lib-a; art-b only in lib-b; art-ab in BOTH lib-a + lib-b;
	// art-c only in lib-c. Capture the generated IDs from Create so the
	// membership rows reference the real artist row.
	artistIDs := map[string]string{}
	for _, name := range []string{"art-a", "art-b", "art-ab", "art-c"} {
		a := testArtist(name, "/music/"+name)
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		artistIDs[name] = a.ID
	}
	for _, link := range []struct{ name, libID string }{
		{"art-a", "lib-a"},
		{"art-b", "lib-b"},
		{"art-ab", "lib-a"},
		{"art-ab", "lib-b"},
		{"art-c", "lib-c"},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_libraries (artist_id, library_id, source) VALUES (?, ?, 'filesystem')`,
			artistIDs[link.name], link.libID); err != nil {
			t.Fatalf("insert artist_libraries (%s, %s): %v", link.name, link.libID, err)
		}
	}

	// Include lib-a OR lib-b: art-a, art-b, art-ab match (3).
	includeAB, err := svc.Count(ctx, CountParams{
		Filters: map[string]string{"library_lib-a": "include", "library_lib-b": "include"},
	})
	if err != nil {
		t.Fatalf("Count include lib-a + lib-b: %v", err)
	}
	if includeAB != 3 {
		t.Errorf("include lib-a + lib-b count = %d, want 3", includeAB)
	}

	// Exclude lib-c: drop art-c, leaving 3.
	excludeC, err := svc.Count(ctx, CountParams{
		Filters: map[string]string{"library_lib-c": "exclude"},
	})
	if err != nil {
		t.Fatalf("Count exclude lib-c: %v", err)
	}
	if excludeC != 3 {
		t.Errorf("exclude lib-c count = %d, want 3", excludeC)
	}

	// Include lib-a, exclude lib-b: only art-a (art-ab is in both, so the
	// exclude clause drops it).
	mixed, err := svc.Count(ctx, CountParams{
		Filters: map[string]string{"library_lib-a": "include", "library_lib-b": "exclude"},
	})
	if err != nil {
		t.Fatalf("Count include lib-a exclude lib-b: %v", err)
	}
	if mixed != 1 {
		t.Errorf("include lib-a exclude lib-b count = %d, want 1 (art-a only)", mixed)
	}
}
