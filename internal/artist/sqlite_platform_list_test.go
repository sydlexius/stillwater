package artist

import (
	"context"
	"testing"
)

// setupPlatformListTest returns a Service backed by a real SQLite DB with two
// connections pre-inserted so foreign-key constraints on artist_platform_ids pass.
func setupPlatformListTest(t *testing.T) *Service {
	t.Helper()
	db := newTestDB(t)
	ctx := context.Background()

	for _, row := range []struct{ id, name, connType string }{
		{"conn-a", "Emby A", "emby"},
		{"conn-b", "Jellyfin B", "jellyfin"},
	} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
			VALUES (?, ?, ?, 'http://host:8096', 'key', 1, 'ok', datetime('now'), datetime('now'))`,
			row.id, row.name, row.connType)
		if err != nil {
			t.Fatal(err)
		}
	}
	return NewService(db)
}

// TestListArtistsWithPlatformMappings_Empty verifies that an empty table
// returns a nil (or empty) slice without error.
func TestListArtistsWithPlatformMappings_Empty(t *testing.T) {
	t.Parallel()
	svc := setupPlatformListTest(t)

	ids, err := svc.ListArtistsWithPlatformMappings(context.Background())
	if err != nil {
		t.Fatalf("expected no error; got %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected empty slice; got %v", ids)
	}
}

// TestListArtistsWithPlatformMappings_Distinct verifies that artists with
// mappings to multiple connections appear exactly once and that the result is
// sorted ascending.
func TestListArtistsWithPlatformMappings_Distinct(t *testing.T) {
	t.Parallel()
	svc := setupPlatformListTest(t)
	ctx := context.Background()

	// Create two artists.
	a1 := createTestArtist(t, svc, "Alpha")
	a2 := createTestArtist(t, svc, "Beta")

	// Map a1 to both connections (should appear once via DISTINCT).
	if err := svc.SetPlatformID(ctx, a1.ID, "conn-a", "p-a1-emby"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetPlatformID(ctx, a1.ID, "conn-b", "p-a1-jf"); err != nil {
		t.Fatal(err)
	}
	// Map a2 to one connection.
	if err := svc.SetPlatformID(ctx, a2.ID, "conn-a", "p-a2-emby"); err != nil {
		t.Fatal(err)
	}

	ids, err := svc.ListArtistsWithPlatformMappings(ctx)
	if err != nil {
		t.Fatalf("expected no error; got %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 distinct artist IDs; got %d: %v", len(ids), ids)
	}
	// Both artist IDs must be present (order is by ID string ascending).
	found := make(map[string]bool, 2)
	for _, id := range ids {
		found[id] = true
	}
	if !found[a1.ID] {
		t.Errorf("expected artist %q in results; got %v", a1.ID, ids)
	}
	if !found[a2.ID] {
		t.Errorf("expected artist %q in results; got %v", a2.ID, ids)
	}
	// Verify monotone sort (each element >= previous).
	for i := 1; i < len(ids); i++ {
		if ids[i] < ids[i-1] {
			t.Errorf("results not sorted: ids[%d]=%q < ids[%d]=%q", i, ids[i], i-1, ids[i-1])
		}
	}
}

// TestListArtistsWithPlatformMappings_OnlyMappedArtistsReturned verifies that
// artists without any platform mapping are excluded from the result.
func TestListArtistsWithPlatformMappings_OnlyMappedArtistsReturned(t *testing.T) {
	t.Parallel()
	svc := setupPlatformListTest(t)
	ctx := context.Background()

	mapped := createTestArtist(t, svc, "Mapped")
	_ = createTestArtist(t, svc, "Unmapped") // no platform ID set

	if err := svc.SetPlatformID(ctx, mapped.ID, "conn-a", "p1"); err != nil {
		t.Fatal(err)
	}

	ids, err := svc.ListArtistsWithPlatformMappings(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 1 || ids[0] != mapped.ID {
		t.Errorf("expected only mapped artist %q; got %v", mapped.ID, ids)
	}
}
