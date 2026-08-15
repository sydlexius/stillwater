package artist

import (
	"context"
	"testing"
)

// TestGetByID_NullSortName is the regression test for the read failure fixed in
// scan.go (#3037). sort_name is declared nullable (a bare `sort_name TEXT`,
// with neither NOT NULL nor a default) and was being scanned straight into a
// string, which made GetByID fail with the driver's converting-NULL-to-string
// error for any row inserted without it, so such an artist could not be read
// at all.
//
// The row is inserted with raw SQL rather than through Service.Create because
// Create always supplies a sort_name -- the defect is only reachable for rows
// written by something else (an external importer, a hand-repaired database, a
// migration that back-filled the row without the column).
func TestGetByID_NullSortName(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	const id = "null-sort-name-artist"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artists (id, name, sort_name, path) VALUES (?, 'Null Sort Artist', NULL, '/music/Null Sort Artist')`,
		id); err != nil {
		t.Fatalf("seeding artist with NULL sort_name: %v", err)
	}

	// Precondition: the column really is NULL. Without this the test would
	// pass vacuously the day an INSERT default or a trigger started filling
	// sort_name in, and it would keep reporting green while covering nothing.
	var isNull bool
	if err := db.QueryRowContext(ctx,
		`SELECT sort_name IS NULL FROM artists WHERE id = ?`, id).Scan(&isNull); err != nil {
		t.Fatalf("checking sort_name nullness: %v", err)
	}
	if !isNull {
		t.Fatal("precondition: sort_name is not NULL for the seeded row; " +
			"this test no longer exercises the NULL read path")
	}

	a, err := svc.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID on an artist whose sort_name is NULL: %v", err)
	}
	if a.SortName != "" {
		t.Errorf("SortName = %q, want %q: a NULL sort_name reads as the empty "+
			"string, matching what this package's own writers store for "+
			"\"unset\"", a.SortName, "")
	}
	if a.Name != "Null Sort Artist" {
		t.Errorf("Name = %q, want %q", a.Name, "Null Sort Artist")
	}
}
