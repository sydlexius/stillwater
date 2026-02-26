package library

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestCreateAndGetByID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	lib := &Library{
		Name: "Main Library",
		Path: "/music/main",
		Type: TypeRegular,
	}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if lib.ID == "" {
		t.Fatal("expected ID to be set after Create")
	}
	if lib.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}

	got, err := svc.GetByID(ctx, lib.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "Main Library" {
		t.Errorf("Name = %q, want %q", got.Name, "Main Library")
	}
	if got.Path != "/music/main" {
		t.Errorf("Path = %q, want %q", got.Path, "/music/main")
	}
	if got.Type != TypeRegular {
		t.Errorf("Type = %q, want %q", got.Type, TypeRegular)
	}
}

func TestGetByID_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	_, err := svc.GetByID(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent ID")
	}
}

func TestGetByPath(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	lib := &Library{Name: "Path Test", Path: "/music/test", Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.GetByPath(ctx, "/music/test")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got == nil || got.Name != "Path Test" {
		t.Errorf("GetByPath: got %v, want Path Test", got)
	}

	// Not found returns nil, nil
	got, err = svc.GetByPath(ctx, "/music/nonexistent")
	if err != nil {
		t.Fatalf("GetByPath not found: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent path, got %+v", got)
	}
}

func TestList(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create libraries in non-alphabetical order
	for _, name := range []string{"Charlie", "Alpha", "Bravo"} {
		lib := &Library{Name: name, Path: "/music/" + name, Type: TypeRegular}
		if err := svc.Create(ctx, lib); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	libs, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(libs) != 3 {
		t.Fatalf("List count = %d, want 3", len(libs))
	}
	// Should be ordered by name
	if libs[0].Name != "Alpha" {
		t.Errorf("first library = %q, want Alpha", libs[0].Name)
	}
	if libs[1].Name != "Bravo" {
		t.Errorf("second library = %q, want Bravo", libs[1].Name)
	}
}

func TestUpdate(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	lib := &Library{Name: "Original", Path: "/music/orig", Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	lib.Name = "Updated"
	lib.Path = "/music/updated"
	lib.Type = TypeClassical
	if err := svc.Update(ctx, lib); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := svc.GetByID(ctx, lib.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.Name != "Updated" {
		t.Errorf("Name = %q, want Updated", got.Name)
	}
	if got.Type != TypeClassical {
		t.Errorf("Type = %q, want %q", got.Type, TypeClassical)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	lib := &Library{ID: "nonexistent", Name: "Test", Type: TypeRegular}
	err := svc.Update(context.Background(), lib)
	if err == nil {
		t.Fatal("expected error for nonexistent update")
	}
}

func TestDelete(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	lib := &Library{Name: "To Delete", Path: "/music/delete", Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Delete(ctx, lib.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := svc.GetByID(ctx, lib.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestDelete_WithArtists(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	lib := &Library{Name: "Has Artists", Path: "/music/has-artists", Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create library: %v", err)
	}

	// Insert an artist referencing this library
	_, err := db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, library_id, created_at, updated_at)
		VALUES ('art-1', 'Test Artist', 'Test Artist', '/music/test', ?, datetime('now'), datetime('now'))
	`, lib.ID)
	if err != nil {
		t.Fatalf("inserting artist: %v", err)
	}

	// Delete should succeed and dereference the artist.
	if err := svc.Delete(ctx, lib.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	// Library should be gone.
	if _, err := svc.GetByID(ctx, lib.ID); err == nil {
		t.Error("library should not exist after delete")
	}

	// Artist should still exist but with a cleared library_id.
	var libID *string
	err = db.QueryRowContext(ctx,
		`SELECT library_id FROM artists WHERE id = 'art-1'`).Scan(&libID)
	if err != nil {
		t.Fatalf("querying artist: %v", err)
	}
	if libID != nil {
		t.Errorf("artist library_id = %q, want NULL", *libID)
	}
}

func TestDelete_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	err := svc.Delete(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent delete")
	}
}

func TestCountArtists(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	lib := &Library{Name: "Count Test", Path: "/music/count", Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	count, err := svc.CountArtists(ctx, lib.ID)
	if err != nil {
		t.Fatalf("CountArtists: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}

	// Add an artist
	_, err = db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, library_id, created_at, updated_at)
		VALUES ('art-2', 'Artist 2', 'Artist 2', '/music/art2', ?, datetime('now'), datetime('now'))
	`, lib.ID)
	if err != nil {
		t.Fatalf("inserting artist: %v", err)
	}

	count, err = svc.CountArtists(ctx, lib.ID)
	if err != nil {
		t.Fatalf("CountArtists: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestCreate_Validation(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Missing name
	err := svc.Create(ctx, &Library{Type: TypeRegular})
	if err == nil {
		t.Error("expected error for missing name")
	}

	// Invalid type
	err = svc.Create(ctx, &Library{Name: "Test", Type: "invalid"})
	if err == nil {
		t.Error("expected error for invalid type")
	}
}

func TestIsDegraded(t *testing.T) {
	lib := Library{Name: "API Only", Path: "", Type: TypeRegular}
	if !lib.IsDegraded() {
		t.Error("expected IsDegraded() = true for empty path")
	}

	lib.Path = "/music/lib"
	if lib.IsDegraded() {
		t.Error("expected IsDegraded() = false for non-empty path")
	}
}

func TestCreate_DuplicateName(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	lib1 := &Library{Name: "Unique", Path: "/music/a", Type: TypeRegular}
	if err := svc.Create(ctx, lib1); err != nil {
		t.Fatalf("Create first: %v", err)
	}

	lib2 := &Library{Name: "Unique", Path: "/music/b", Type: TypeRegular}
	err := svc.Create(ctx, lib2)
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
}
