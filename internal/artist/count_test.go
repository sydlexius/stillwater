package artist

import (
	"context"
	"testing"
)

func TestCount_NoArtists(t *testing.T) {
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
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

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
	// Verify that Count returns the same total as List for several filter
	// shapes. This catches drift between buildWhereClause and toListParams.
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

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
