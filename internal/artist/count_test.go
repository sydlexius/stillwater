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
// exclude flyout filters (Filters["library_<id>"] = include|exclude).
//
// Issue #1217 changed the meaning of "include": when at least one library is
// set to Include, the filter is a WHITELIST -- results are restricted to
// artists whose memberships fall ENTIRELY within the included set. An artist
// also in some non-included library is dropped, even though it is in an
// included library too. When no library is set to Include, the exclude-only
// behavior is unchanged: each excluded library emits its own NOT EXISTS.
func TestCount_WithLibraryFlyoutFilters(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	seedLibraries(t, db, "lib-a", "lib-b", "lib-c")

	// art-a only in lib-a; art-b only in lib-b; art-ab in BOTH lib-a + lib-b;
	// art-c only in lib-c; art-none has zero memberships. Capture the
	// generated IDs from Create so the membership rows reference the real
	// artist row.
	artistIDs := map[string]string{}
	for _, name := range []string{"art-a", "art-b", "art-ab", "art-c", "art-none"} {
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

	// Whitelist {lib-a}: only artists whose memberships are entirely within
	// {lib-a}. art-a qualifies; art-ab does not (it is also in lib-b).
	includeA, err := svc.Count(ctx, CountParams{
		Filters: map[string]string{"library_lib-a": "include"},
	})
	if err != nil {
		t.Fatalf("Count include lib-a: %v", err)
	}
	if includeA != 1 {
		t.Errorf("include lib-a count = %d, want 1 (art-a only)", includeA)
	}

	// Whitelist {lib-a, lib-b}: art-a, art-b, art-ab all qualify because each
	// of their memberships is within the included set (3). art-c and art-none
	// are excluded.
	includeAB, err := svc.Count(ctx, CountParams{
		Filters: map[string]string{"library_lib-a": "include", "library_lib-b": "include"},
	})
	if err != nil {
		t.Fatalf("Count include lib-a + lib-b: %v", err)
	}
	// A count of 3 also confirms art-none (zero memberships) is not matched:
	// the whitelist requires membership in at least one included library, so a
	// wrongly-matched art-none would push this to 4.
	if includeAB != 3 {
		t.Errorf("include lib-a + lib-b count = %d, want 3", includeAB)
	}

	// Exclude-only mode is unchanged. Exclude lib-c: drop art-c. art-none has
	// no membership in lib-c so it passes through. Total = 4.
	excludeC, err := svc.Count(ctx, CountParams{
		Filters: map[string]string{"library_lib-c": "exclude"},
	})
	if err != nil {
		t.Fatalf("Count exclude lib-c: %v", err)
	}
	if excludeC != 4 {
		t.Errorf("exclude lib-c count = %d, want 4", excludeC)
	}

	// Include lib-a, exclude lib-b: include is present, so whitelist mode is
	// active and the explicit exclude is redundant. Whitelist {lib-a} keeps
	// only art-a.
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
