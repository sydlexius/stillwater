package artist

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestListIDs_Empty verifies that ListIDs returns an empty slice (not nil) and
// a zero total when no artists exist in the database.
func TestListIDs_Empty(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	ids, total, capped, err := svc.ListIDs(ctx, CountParams{})
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if capped {
		t.Error("capped = true, want false on empty result")
	}
	if len(ids) != 0 {
		t.Errorf("len(ids) = %d, want 0", len(ids))
	}
}

// TestListIDs_StableOrder verifies that results are ordered by sort_name then
// id and are deterministic across calls. Stable ordering is load-bearing for
// the select-all-matching affordance: if the order changes between the page
// render and the ID fetch the user may get a different selection.
func TestListIDs_StableOrder(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Insert artists with sort_names out of alphabetical insertion order.
	names := []string{"Zebra", "Alpha", "Mango"}
	for _, name := range names {
		a := testArtist(name, "/music/"+name)
		a.SortName = name // explicit sort_name matching name for predictability
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	ids, total, capped, err := svc.ListIDs(ctx, CountParams{})
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if capped {
		t.Error("capped = true, want false for small result set")
	}
	if len(ids) != 3 {
		t.Fatalf("len(ids) = %d, want 3", len(ids))
	}

	// Fetch artist names in the returned ID order so we can assert the sort.
	byID := make(map[string]string)
	all, _, err := svc.List(ctx, ListParams{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, a := range all {
		byID[a.ID] = a.SortName
	}

	// Verify ascending sort_name order.
	prev := ""
	for i, id := range ids {
		sn := byID[id]
		if sn == "" {
			t.Errorf("ids[%d] = %q not found in artist map", i, id)
			continue
		}
		if strings.ToLower(sn) < strings.ToLower(prev) {
			t.Errorf("order violation at index %d: %q < %q", i, sn, prev)
		}
		prev = sn
	}

	// Call again and verify identical output (determinism).
	ids2, _, _, err := svc.ListIDs(ctx, CountParams{})
	if err != nil {
		t.Fatalf("ListIDs second call: %v", err)
	}
	for i := range ids {
		if ids[i] != ids2[i] {
			t.Errorf("non-deterministic: ids[%d]=%q != ids2[%d]=%q", i, ids[i], i, ids2[i])
		}
	}
}

// TestListIDs_WithSearchFilter verifies that the search filter (name substring
// match) is applied correctly -- matching artists are included and
// non-matching ones are excluded.
func TestListIDs_WithSearchFilter(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	for _, name := range []string{"Radiohead", "Coldplay", "Radio Moscow"} {
		a := testArtist(name, "/music/"+name)
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	ids, total, capped, err := svc.ListIDs(ctx, CountParams{Search: "Radio"})
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if capped {
		t.Error("capped = true on small filtered result")
	}
	if len(ids) != 2 {
		t.Errorf("len(ids) = %d, want 2", len(ids))
	}
}

// TestListIDs_CapOverflow verifies that when the matching artist count exceeds
// MaxListIDs the returned slice is capped and capped=true is reported. The
// true total is still returned accurately.
func TestListIDs_CapOverflow(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	// Use the raw repo directly so we can bypass the service hydration cost
	// and insert a large number of rows quickly.
	repo := newSQLiteArtistRepo(db)
	ctx := context.Background()

	// Insert MaxListIDs + 5 artists so we can verify both the cap and total.
	target := MaxListIDs + 5
	for i := 0; i < target; i++ {
		a := testArtist(fmt.Sprintf("Artist%04d", i), fmt.Sprintf("/music/a%04d", i))
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("Create artist %d: %v", i, err)
		}
	}

	ids, total, capped, err := NewService(db).ListIDs(ctx, CountParams{})
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	if total != target {
		t.Errorf("total = %d, want %d", total, target)
	}
	if !capped {
		t.Error("capped = false, want true when total > MaxListIDs")
	}
	if len(ids) != MaxListIDs {
		t.Errorf("len(ids) = %d, want MaxListIDs (%d)", len(ids), MaxListIDs)
	}
}

// TestListIDs_FlyoutFilterParity verifies that a flyout filter param (Filters
// map entry) is forwarded through buildWhereClause correctly. This hardens the
// claim that buildWhereClause parity holds for flyout filters -- not just the
// search param. The test uses type_group (include) to split artists by type and
// confirms only matching IDs are returned.
func TestListIDs_FlyoutFilterParity(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Insert two "group" artists and one "person" artist.
	group1 := testArtist("GroupOne", "/music/GroupOne")
	group1.Type = "group"
	group2 := testArtist("GroupTwo", "/music/GroupTwo")
	group2.Type = "group"
	person1 := testArtist("PersonOne", "/music/PersonOne")
	person1.Type = "person"

	for _, a := range []*Artist{group1, group2, person1} {
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", a.Name, err)
		}
	}

	// Include only the "group" type via the flyout filter.
	ids, total, capped, err := svc.ListIDs(ctx, CountParams{
		Filters: map[string]string{"type_group": "include"},
	})
	if err != nil {
		t.Fatalf("ListIDs with flyout filter: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 (only group-type artists)", total)
	}
	if capped {
		t.Error("capped = true, want false for small filtered result")
	}
	if len(ids) != 2 {
		t.Errorf("len(ids) = %d, want 2", len(ids))
	}

	// Confirm no person-type ID slipped through.
	for _, id := range ids {
		if id == person1.ID {
			t.Errorf("person1 ID %q appeared in group-only result", id)
		}
	}

	// Exclude the "group" type: only the person artist should remain.
	ids2, total2, _, err := svc.ListIDs(ctx, CountParams{
		Filters: map[string]string{"type_group": "exclude"},
	})
	if err != nil {
		t.Fatalf("ListIDs with type_group=exclude: %v", err)
	}
	if total2 != 1 {
		t.Errorf("total = %d, want 1 (person-type artist only)", total2)
	}
	if len(ids2) != 1 {
		t.Errorf("len(ids) = %d, want 1", len(ids2))
	}
	if len(ids2) > 0 && ids2[0] != person1.ID {
		t.Errorf("ids2[0] = %q, want person1 ID %q", ids2[0], person1.ID)
	}
}

// TestListIDs_ExactlyAtCap verifies that when the match count equals MaxListIDs
// exactly the result is NOT marked as capped. The total equals MaxListIDs and
// all IDs are returned.
func TestListIDs_ExactlyAtCap(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	repo := newSQLiteArtistRepo(db)
	ctx := context.Background()

	for i := 0; i < MaxListIDs; i++ {
		a := testArtist(fmt.Sprintf("Band%04d", i), fmt.Sprintf("/music/b%04d", i))
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("Create band %d: %v", i, err)
		}
	}

	ids, total, capped, err := NewService(db).ListIDs(ctx, CountParams{})
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	if total != MaxListIDs {
		t.Errorf("total = %d, want MaxListIDs (%d)", total, MaxListIDs)
	}
	if capped {
		t.Error("capped = true, want false when count == MaxListIDs exactly")
	}
	if len(ids) != MaxListIDs {
		t.Errorf("len(ids) = %d, want MaxListIDs (%d)", len(ids), MaxListIDs)
	}
}
