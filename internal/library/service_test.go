package library

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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

	dir := t.TempDir()
	lib := &Library{
		Name: "Main Library",
		Path: dir,
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
	if got.Path != dir {
		t.Errorf("Path = %q, want %q", got.Path, dir)
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

	dir := t.TempDir()
	lib := &Library{Name: "Path Test", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.GetByPath(ctx, dir)
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got == nil || got.Name != "Path Test" {
		t.Errorf("GetByPath: got %v, want Path Test", got)
	}

	// Not found returns nil, nil
	got, err = svc.GetByPath(ctx, filepath.Join(dir, "nonexistent"))
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

	base := t.TempDir()
	// Create subdirectories for each library
	for _, name := range []string{"Charlie", "Alpha", "Bravo"} {
		dir := filepath.Join(base, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("creating subdir %s: %v", name, err)
		}
		lib := &Library{Name: name, Path: dir, Type: TypeRegular}
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

	base := t.TempDir()
	origDir := filepath.Join(base, "orig")
	updatedDir := filepath.Join(base, "updated")
	if err := os.Mkdir(origDir, 0o755); err != nil {
		t.Fatalf("creating orig dir: %v", err)
	}
	if err := os.Mkdir(updatedDir, 0o755); err != nil {
		t.Fatalf("creating updated dir: %v", err)
	}

	lib := &Library{Name: "Original", Path: origDir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	lib.Name = "Updated"
	lib.Path = updatedDir
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

	dir := t.TempDir()
	lib := &Library{Name: "To Delete", Path: dir, Type: TypeRegular}
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

	dir := t.TempDir()
	lib := &Library{Name: "Has Artists", Path: dir, Type: TypeRegular}
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

	dir := t.TempDir()
	lib := &Library{Name: "Count Test", Path: dir, Type: TypeRegular}
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

func TestIsPathless(t *testing.T) {
	lib := Library{Name: "API Only", Path: "", Type: TypeRegular}
	if !lib.IsPathless() {
		t.Error("expected IsPathless() = true for empty path")
	}

	lib.Path = "/music/lib"
	if lib.IsPathless() {
		t.Error("expected IsPathless() = false for non-empty path")
	}
}

func TestCreate_DuplicateName(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	base := t.TempDir()
	dirA := filepath.Join(base, "a")
	dirB := filepath.Join(base, "b")
	if err := os.Mkdir(dirA, 0o755); err != nil {
		t.Fatalf("creating dir a: %v", err)
	}
	if err := os.Mkdir(dirB, 0o755); err != nil {
		t.Fatalf("creating dir b: %v", err)
	}

	lib1 := &Library{Name: "Unique", Path: dirA, Type: TypeRegular}
	if err := svc.Create(ctx, lib1); err != nil {
		t.Fatalf("Create first: %v", err)
	}

	lib2 := &Library{Name: "Unique", Path: dirB, Type: TypeRegular}
	err := svc.Create(ctx, lib2)
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
}

func TestCreate_InvalidSource(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Source validation occurs before path validation, so path value does
	// not matter for this test. Use an empty path (pathless library).
	lib := &Library{Name: "Bad Source", Type: TypeRegular, Source: "spotify"}
	err := svc.Create(ctx, lib)
	if err == nil {
		t.Fatal("expected error for invalid source")
	}
}

func TestCreate_DefaultSource(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	dir := t.TempDir()
	lib := &Library{Name: "Default Source", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if lib.Source != SourceManual {
		t.Errorf("Source = %q, want %q", lib.Source, SourceManual)
	}

	got, err := svc.GetByID(ctx, lib.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Source != SourceManual {
		t.Errorf("persisted Source = %q, want %q", got.Source, SourceManual)
	}
}

func TestGetByConnectionAndExternalID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create a connection to reference
	_, err := db.ExecContext(ctx, `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		VALUES ('conn-1', 'Emby', 'emby', 'http://emby:8096', 'enc-key', 1, 'ok', datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	lib := &Library{
		Name:         "Emby Music",
		Type:         TypeRegular,
		Source:       SourceEmby,
		ConnectionID: "conn-1",
		ExternalID:   "ext-123",
	}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Found
	got, err := svc.GetByConnectionAndExternalID(ctx, "conn-1", "ext-123")
	if err != nil {
		t.Fatalf("GetByConnectionAndExternalID: %v", err)
	}
	if got == nil || got.Name != "Emby Music" {
		t.Errorf("expected Emby Music, got %v", got)
	}

	// Not found (wrong external ID)
	got, err = svc.GetByConnectionAndExternalID(ctx, "conn-1", "ext-999")
	if err != nil {
		t.Fatalf("GetByConnectionAndExternalID not found: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for wrong external ID, got %+v", got)
	}

	// Different connection
	got, err = svc.GetByConnectionAndExternalID(ctx, "conn-other", "ext-123")
	if err != nil {
		t.Fatalf("GetByConnectionAndExternalID different conn: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for different connection, got %+v", got)
	}
}

func TestClearConnectionID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create a connection to reference
	_, err := db.ExecContext(ctx, `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		VALUES ('conn-2', 'Jellyfin', 'jellyfin', 'http://jf:8096', 'enc-key', 1, 'ok', datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	lib1 := &Library{
		Name:         "JF Music 1",
		Type:         TypeRegular,
		Source:       SourceJellyfin,
		ConnectionID: "conn-2",
		ExternalID:   "jf-1",
	}
	lib2 := &Library{
		Name:         "JF Music 2",
		Type:         TypeRegular,
		Source:       SourceJellyfin,
		ConnectionID: "conn-2",
		ExternalID:   "jf-2",
	}
	lib3 := &Library{
		Name:   "Manual Lib",
		Type:   TypeRegular,
		Source: SourceManual,
	}
	for _, lib := range []*Library{lib1, lib2, lib3} {
		if err := svc.Create(ctx, lib); err != nil {
			t.Fatalf("Create %s: %v", lib.Name, err)
		}
	}

	if err := svc.ClearConnectionID(ctx, "conn-2"); err != nil {
		t.Fatalf("ClearConnectionID: %v", err)
	}

	// Both JF libraries should have cleared connection_id
	for _, lib := range []*Library{lib1, lib2} {
		got, err := svc.GetByID(ctx, lib.ID)
		if err != nil {
			t.Fatalf("GetByID %s: %v", lib.Name, err)
		}
		if got.ConnectionID != "" {
			t.Errorf("%s: ConnectionID = %q, want empty", lib.Name, got.ConnectionID)
		}
	}

	// Manual lib should be unaffected
	got, err := svc.GetByID(ctx, lib3.ID)
	if err != nil {
		t.Fatalf("GetByID manual: %v", err)
	}
	if got.ConnectionID != "" {
		t.Errorf("manual lib: ConnectionID = %q, want empty (was already empty)", got.ConnectionID)
	}
}

func TestCreate_PathValidation(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	tmpDir := t.TempDir()

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name: "empty path allowed (pathless)",
			path: "",
		},
		{
			name:    "relative path rejected",
			path:    "music/lib",
			wantErr: true,
		},
		{
			name:    "traversal rejected",
			path:    tmpDir + "/../etc",
			wantErr: true,
		},
		{
			name:    "nonexistent path rejected",
			path:    tmpDir + "/no-such-dir",
			wantErr: true,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lib := &Library{
				Name: fmt.Sprintf("PathTest-%d", i),
				Path: tt.path,
				Type: TypeRegular,
			}
			err := svc.Create(ctx, lib)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Create with path %q: expected error, got nil", tt.path)
				}
				return
			}
			if err != nil {
				t.Errorf("Create with path %q: unexpected error: %v", tt.path, err)
			}
		})
	}
}

func TestUpdate_PathValidation(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	tmpDir := t.TempDir()

	// Create a valid library to update.
	lib := &Library{Name: "UpdatePathTest", Path: tmpDir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create seed library: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name: "empty path allowed (pathless)",
			path: "",
		},
		{
			name:    "relative path rejected",
			path:    "music/lib",
			wantErr: true,
		},
		{
			name:    "traversal rejected",
			path:    tmpDir + "/../etc",
			wantErr: true,
		},
		{
			name:    "nonexistent path rejected",
			path:    tmpDir + "/no-such-dir",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset to a known-good state before each sub-test.
			lib.Path = tmpDir
			lib.Name = "UpdatePathTest"
			if err := svc.Update(ctx, lib); err != nil {
				t.Fatalf("resetting library: %v", err)
			}

			lib.Path = tt.path
			err := svc.Update(ctx, lib)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Update with path %q: expected error, got nil", tt.path)
				}
				return
			}
			if err != nil {
				t.Errorf("Update with path %q: unexpected error: %v", tt.path, err)
			}
		})
	}
}

func TestUpdate_InvalidSource(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	dir := t.TempDir()
	lib := &Library{Name: "Valid", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	lib.Source = "invalid"
	err := svc.Update(ctx, lib)
	if err == nil {
		t.Fatal("expected error for invalid source on update")
	}
}

func TestUpdate_DefaultsEmptySource(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	dir := t.TempDir()
	lib := &Library{Name: "Source Default", Path: dir, Type: TypeRegular, Source: SourceEmby}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	lib.Source = ""
	if err := svc.Update(ctx, lib); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if lib.Source != SourceManual {
		t.Errorf("Source = %q, want %q", lib.Source, SourceManual)
	}
}

func TestIsSharedFS(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"", false},
		{SharedFSNone, false},
		{SharedFSSuspected, true},
		{SharedFSConfirmed, true},
		{"invalid", false},
	}
	for _, tt := range tests {
		lib := Library{SharedFSStatus: tt.status}
		if got := lib.IsSharedFS(); got != tt.want {
			t.Errorf("IsSharedFS(%q) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestSetSharedFSStatus_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	dir := t.TempDir()
	lib := &Library{Name: "SharedFS Test", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Set status and read back
	if err := svc.SetSharedFSStatus(ctx, lib.ID, SharedFSSuspected, `["mtime mismatch"]`, "peer-1,peer-2"); err != nil {
		t.Fatalf("SetSharedFSStatus: %v", err)
	}

	got, err := svc.GetByID(ctx, lib.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.SharedFSStatus != SharedFSSuspected {
		t.Errorf("SharedFSStatus = %q, want %q", got.SharedFSStatus, SharedFSSuspected)
	}
	if got.SharedFSEvidence != `["mtime mismatch"]` {
		t.Errorf("SharedFSEvidence = %q, want %q", got.SharedFSEvidence, `["mtime mismatch"]`)
	}
	if got.SharedFSPeerLibraryIDs != "peer-1,peer-2" {
		t.Errorf("SharedFSPeerLibraryIDs = %q, want %q", got.SharedFSPeerLibraryIDs, "peer-1,peer-2")
	}

	// Clear status
	if err := svc.SetSharedFSStatus(ctx, lib.ID, "", "", ""); err != nil {
		t.Fatalf("clear SetSharedFSStatus: %v", err)
	}
	got, err = svc.GetByID(ctx, lib.ID)
	if err != nil {
		t.Fatalf("GetByID after clear: %v", err)
	}
	if got.SharedFSStatus != "" {
		t.Errorf("SharedFSStatus after clear = %q, want empty", got.SharedFSStatus)
	}
	if got.SharedFSEvidence != "" {
		t.Errorf("SharedFSEvidence after clear = %q, want empty", got.SharedFSEvidence)
	}
	if got.SharedFSPeerLibraryIDs != "" {
		t.Errorf("SharedFSPeerLibraryIDs after clear = %q, want empty", got.SharedFSPeerLibraryIDs)
	}
}

func TestSetSharedFSStatus_InvalidStatus(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	dir := t.TempDir()
	lib := &Library{Name: "Validation Test", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := svc.SetSharedFSStatus(ctx, lib.ID, "typo", "", "")
	if err == nil {
		t.Fatal("expected error for invalid status")
	}
}

func TestSetSharedFSStatus_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	err := svc.SetSharedFSStatus(context.Background(), "nonexistent", SharedFSNone, "", "")
	if err == nil {
		t.Fatal("expected error for nonexistent library")
	}
}

func TestListSharedFS(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	base := t.TempDir()
	for i, name := range []string{"None", "Suspected", "Confirmed", "Empty"} {
		dir := filepath.Join(base, fmt.Sprintf("dir%d", i))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		lib := &Library{Name: name, Path: dir, Type: TypeRegular}
		if err := svc.Create(ctx, lib); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		var status string
		switch name {
		case "None":
			status = SharedFSNone
		case "Suspected":
			status = SharedFSSuspected
		case "Confirmed":
			status = SharedFSConfirmed
		case "Empty":
			status = ""
		}
		if status != "" {
			if err := svc.SetSharedFSStatus(ctx, lib.ID, status, "", ""); err != nil {
				t.Fatalf("SetSharedFSStatus %s: %v", name, err)
			}
		}
	}

	libs, err := svc.ListSharedFS(ctx)
	if err != nil {
		t.Fatalf("ListSharedFS: %v", err)
	}
	if len(libs) != 2 {
		t.Fatalf("ListSharedFS count = %d, want 2 (suspected + confirmed)", len(libs))
	}
	names := map[string]bool{}
	for _, lib := range libs {
		names[lib.Name] = true
	}
	if !names["Suspected"] {
		t.Error("ListSharedFS missing 'Suspected' library")
	}
	if !names["Confirmed"] {
		t.Error("ListSharedFS missing 'Confirmed' library")
	}
}

func TestHasLocalLibrary(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// No libraries at all: should return false.
	has, err := svc.HasLocalLibrary(ctx)
	if err != nil {
		t.Fatalf("HasLocalLibrary (empty): %v", err)
	}
	if has {
		t.Error("expected HasLocalLibrary = false with no libraries")
	}

	// Add a pathless (API-only) library: should still return false.
	pathlessLib := &Library{Name: "API Only", Path: "", Type: TypeRegular}
	if err := svc.Create(ctx, pathlessLib); err != nil {
		t.Fatalf("Create pathless: %v", err)
	}
	has, err = svc.HasLocalLibrary(ctx)
	if err != nil {
		t.Fatalf("HasLocalLibrary (pathless only): %v", err)
	}
	if has {
		t.Error("expected HasLocalLibrary = false with only pathless library")
	}

	// Add a library with a path: should return true.
	dir := t.TempDir()
	localLib := &Library{Name: "Local", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, localLib); err != nil {
		t.Fatalf("Create local: %v", err)
	}
	has, err = svc.HasLocalLibrary(ctx)
	if err != nil {
		t.Fatalf("HasLocalLibrary (with local): %v", err)
	}
	if !has {
		t.Error("expected HasLocalLibrary = true with a local library")
	}

	// Delete the local library: should return false again.
	if err := svc.Delete(ctx, localLib.ID); err != nil {
		t.Fatalf("Delete local: %v", err)
	}
	has, err = svc.HasLocalLibrary(ctx)
	if err != nil {
		t.Fatalf("HasLocalLibrary (after delete): %v", err)
	}
	if has {
		t.Error("expected HasLocalLibrary = false after deleting the local library")
	}
}

// seedConnection inserts a connection row so libraries can reference it.
// Used by the issue #1072 / #1078 / #1076 cascade and dedup tests below.
func seedConnection(t *testing.T, db *sql.DB, id, connType string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		VALUES (?, ?, ?, 'http://test', 'k', 1, 'ok', datetime('now'), datetime('now'))
	`, id, "Conn-"+id, connType)
	if err != nil {
		t.Fatalf("seeding connection: %v", err)
	}
}

// seedArtist inserts an artist row with the given library_id (may be empty).
func seedArtist(t *testing.T, db *sql.DB, id, name, libraryID string) {
	t.Helper()
	var libArg any
	if libraryID == "" {
		libArg = nil
	} else {
		libArg = libraryID
	}
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO artists (id, name, sort_name, path, library_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'), datetime('now'))
	`, id, name, name, "/music/"+name, libArg)
	if err != nil {
		t.Fatalf("seeding artist %s: %v", id, err)
	}
}

// seedPlatformID inserts a row in artist_platform_ids. Used for cascade /
// orphan tests where we deliberately bypass the repository to set up state.
func seedPlatformID(t *testing.T, db *sql.DB, artistID, connID, platformID string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id, created_at, updated_at)
		VALUES (?, ?, ?, datetime('now'), datetime('now'))
	`, artistID, connID, platformID)
	if err != nil {
		t.Fatalf("seeding platform id: %v", err)
	}
}

// TestDeleteWithArtists_CascadesPlatformIDs covers issue #1078: deleting an
// artist row must remove its artist_platform_ids row via ON DELETE CASCADE.
// Regression guard for the orphan rows that were observed in production.
func TestDeleteWithArtists_CascadesPlatformIDs(t *testing.T) {
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	seedConnection(t, db, "conn-emby", "emby")
	dir := t.TempDir()
	lib := &Library{Name: "Emby Lib", Path: dir, Type: TypeRegular, ConnectionID: "conn-emby", ExternalID: "emby-1", Source: SourceEmby}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create library: %v", err)
	}
	seedArtist(t, db, "art-1", "Deftones", lib.ID)
	seedPlatformID(t, db, "art-1", "conn-emby", "emby-deftones-001")

	// Sanity: row exists.
	var pre int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = 'art-1'`).Scan(&pre); err != nil {
		t.Fatalf("preflight count: %v", err)
	}
	if pre != 1 {
		t.Fatalf("preflight platform id count = %d, want 1", pre)
	}

	if err := svc.DeleteWithArtists(ctx, lib.ID); err != nil {
		t.Fatalf("DeleteWithArtists: %v", err)
	}

	var post int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = 'art-1'`).Scan(&post); err != nil {
		t.Fatalf("post count: %v", err)
	}
	if post != 0 {
		t.Errorf("artist_platform_ids row count = %d, want 0 (cascade should have removed it)", post)
	}

	var artistCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-1'`).Scan(&artistCount); err != nil {
		t.Fatalf("artist count: %v", err)
	}
	if artistCount != 0 {
		t.Errorf("artist row count = %d, want 0", artistCount)
	}
}

// TestDeleteWithArtists_PrunesOrphanedConnectionArtists covers issue #1072:
// when a connected library is unlinked with deleteArtists=true, artists that
// came from the same connection but have a NULL library_id (because some
// earlier code path lost the assignment) should also be pruned.
func TestDeleteWithArtists_PrunesOrphanedConnectionArtists(t *testing.T) {
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	seedConnection(t, db, "conn-jelly", "jellyfin")

	// Library tied to the connection.
	dir := t.TempDir()
	lib := &Library{Name: "Jelly Lib", Path: dir, Type: TypeRegular, ConnectionID: "conn-jelly", ExternalID: "j-1", Source: SourceJellyfin}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create library: %v", err)
	}

	// Artist properly attached to the library.
	seedArtist(t, db, "art-attached", "Attached", lib.ID)
	seedPlatformID(t, db, "art-attached", "conn-jelly", "jp-1")

	// Artist sourced from the same connection but missing library_id.
	seedArtist(t, db, "art-orphan", "Orphan", "")
	seedPlatformID(t, db, "art-orphan", "conn-jelly", "jp-2")

	// Artist with mappings to two connections (this connection plus another).
	// Must NOT be pruned: it still has a non-null mapping after the unlink.
	seedConnection(t, db, "conn-other", "emby")
	seedArtist(t, db, "art-multi", "Multi", "")
	seedPlatformID(t, db, "art-multi", "conn-jelly", "jp-3")
	seedPlatformID(t, db, "art-multi", "conn-other", "ep-3")

	if err := svc.DeleteWithArtists(ctx, lib.ID); err != nil {
		t.Fatalf("DeleteWithArtists: %v", err)
	}

	check := func(id string, wantPresent bool) {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM artists WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("counting artist %s: %v", id, err)
		}
		got := n > 0
		if got != wantPresent {
			t.Errorf("artist %s present = %v, want %v", id, got, wantPresent)
		}
	}
	check("art-attached", false)
	check("art-orphan", false)
	check("art-multi", true)

	// Issue #1072 reopen: when the unlinked library was the last library
	// on its connection, the multi-conn artist's mapping to that
	// connection must be cleaned even though the artist row survives via
	// its other-connection mapping. The connection itself is still alive
	// here, so the FK cascade does not fire; the application-level sweep
	// in DeleteWithArtists is responsible.
	var jellyMaps int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = 'art-multi' AND connection_id = 'conn-jelly'`).Scan(&jellyMaps); err != nil {
		t.Fatalf("counting multi jelly mappings: %v", err)
	}
	if jellyMaps != 0 {
		t.Errorf("multi jelly mapping count = %d, want 0 (last library on connection unlinked, mapping is stale)", jellyMaps)
	}

	// The other-connection mapping must remain.
	var otherMaps int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = 'art-multi' AND connection_id = 'conn-other'`).Scan(&otherMaps); err != nil {
		t.Fatalf("counting multi other mappings: %v", err)
	}
	if otherMaps != 1 {
		t.Errorf("multi other mapping count = %d, want 1 (must survive unlink)", otherMaps)
	}
}

// TestDeleteWithArtists_PrunePreservesSiblingLibraryArtists guards the
// CodeRabbit-flagged regression on PR #1211: when a connection has more
// than one library, unlinking one of them must NOT prune artists that
// are missing library_id but still belong to the connection. Those
// artists may legitimately belong to a sibling library on the same
// connection. The prune fallback is only safe when this is the LAST
// library on the connection.
func TestDeleteWithArtists_PrunePreservesSiblingLibraryArtists(t *testing.T) {
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	seedConnection(t, db, "conn-emby", "emby")

	// Two libraries on the same connection.
	musicDir := t.TempDir()
	classicalDir := t.TempDir()
	music := &Library{Name: "Music", Path: musicDir, Type: TypeRegular, ConnectionID: "conn-emby", ExternalID: "emby-music", Source: SourceEmby}
	classical := &Library{Name: "Classical", Path: classicalDir, Type: TypeClassical, ConnectionID: "conn-emby", ExternalID: "emby-classical", Source: SourceEmby}
	if err := svc.Create(ctx, music); err != nil {
		t.Fatalf("Create music library: %v", err)
	}
	if err := svc.Create(ctx, classical); err != nil {
		t.Fatalf("Create classical library: %v", err)
	}

	// Artist directly attached to the library being deleted.
	seedArtist(t, db, "art-music", "MusicArtist", music.ID)
	seedPlatformID(t, db, "art-music", "conn-emby", "emby-music-1")

	// Artist with NULL library_id but a connection mapping. With only
	// the music library on this connection, the old prune would delete
	// this row. With the classical library still attached, the new
	// guard must preserve it.
	seedArtist(t, db, "art-sibling-orphan", "SiblingOrphan", "")
	seedPlatformID(t, db, "art-sibling-orphan", "conn-emby", "emby-orphan-1")

	if err := svc.DeleteWithArtists(ctx, music.ID); err != nil {
		t.Fatalf("DeleteWithArtists: %v", err)
	}

	// The directly-attached artist is gone.
	var attached int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-music'`).Scan(&attached); err != nil {
		t.Fatalf("counting attached artist: %v", err)
	}
	if attached != 0 {
		t.Errorf("attached artist count = %d, want 0", attached)
	}

	// The sibling-library orphan is preserved because the classical
	// library still hangs off the connection.
	var orphan int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-sibling-orphan'`).Scan(&orphan); err != nil {
		t.Fatalf("counting sibling orphan: %v", err)
	}
	if orphan != 1 {
		t.Errorf("sibling orphan count = %d, want 1 (must be preserved while sibling libraries exist)", orphan)
	}

	// Now delete the last library on the connection. Without siblings
	// remaining, the prune is safe and the orphan must finally go.
	if err := svc.DeleteWithArtists(ctx, classical.ID); err != nil {
		t.Fatalf("DeleteWithArtists last library: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-sibling-orphan'`).Scan(&orphan); err != nil {
		t.Fatalf("counting sibling orphan after last delete: %v", err)
	}
	if orphan != 0 {
		t.Errorf("sibling orphan count after last library delete = %d, want 0 (prune fires when last library is gone)", orphan)
	}
}

// TestDeleteWithArtists_PreservesArtistInOtherLibraries is the issue #1004
// regression for the M:N model: an artist with membership in the unlinked
// library AND in another library must survive the unlink. Only the
// unlinked library's membership row goes; the artist row stays because the
// other library still observes it. Without this guard, the legacy
// "WHERE library_id = ?" delete (or even a naive M:N pruner) would erase
// the artist from under the still-attached library.
func TestDeleteWithArtists_PreservesArtistInOtherLibraries(t *testing.T) {
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	dir := t.TempDir()
	fsLib := &Library{Name: "Filesystem", Path: dir, Type: TypeRegular, Source: SourceManual}
	if err := svc.Create(ctx, fsLib); err != nil {
		t.Fatalf("Create fs library: %v", err)
	}
	seedConnection(t, db, "conn-emby", "emby")
	embyLib := &Library{Name: "Emby Music", Type: TypeRegular, ConnectionID: "conn-emby", ExternalID: "emby-1", Source: SourceEmby}
	if err := svc.Create(ctx, embyLib); err != nil {
		t.Fatalf("Create emby library: %v", err)
	}

	// Artist in BOTH libraries via membership rows.
	seedArtist(t, db, "art-multi", "Radiohead", fsLib.ID)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		VALUES ('art-multi', ?, 'filesystem', datetime('now')),
		       ('art-multi', ?, 'emby', datetime('now'))
	`, fsLib.ID, embyLib.ID); err != nil {
		t.Fatalf("seed memberships: %v", err)
	}
	seedPlatformID(t, db, "art-multi", "conn-emby", "emby-radiohead-1")

	// Unlink the Emby library with "delete artists" semantics.
	if err := svc.DeleteWithArtists(ctx, embyLib.ID); err != nil {
		t.Fatalf("DeleteWithArtists emby: %v", err)
	}

	// The artist must survive because filesystem still observes it.
	var present int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-multi'`).Scan(&present); err != nil {
		t.Fatalf("count survivor: %v", err)
	}
	if present != 1 {
		t.Errorf("artist count = %d, want 1 (filesystem membership must preserve)", present)
	}

	// The Emby library's membership row went via cascade.
	var fsMember, embyMember int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = 'art-multi' AND library_id = ?`,
		fsLib.ID).Scan(&fsMember); err != nil {
		t.Fatalf("count fs membership: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = 'art-multi' AND library_id = ?`,
		embyLib.ID).Scan(&embyMember); err != nil {
		t.Fatalf("count emby membership: %v", err)
	}
	if fsMember != 1 {
		t.Errorf("fs membership count = %d, want 1 (must remain)", fsMember)
	}
	if embyMember != 0 {
		t.Errorf("emby membership count = %d, want 0 (cascade should have removed it)", embyMember)
	}
}

// TestDeleteWithArtists_PrunesStalePlatformIDsForMultiHomeArtist covers
// issue #1072 (reopened post-#1215 M:N): when a multi-home artist has
// memberships in libraries on two different connections and one of those
// libraries is unlinked, the artist row and its other connection's
// membership and platform_id must survive, but the artist_platform_ids
// row pointing at the just-unlinked connection must be removed. The
// connection FK CASCADE does not fire here because the connection itself
// stays alive, and the artist row is preserved by the candidate-prune
// loop because memberships > 0.
func TestDeleteWithArtists_PrunesStalePlatformIDsForMultiHomeArtist(t *testing.T) {
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	seedConnection(t, db, "conn-emby", "emby")
	seedConnection(t, db, "conn-jelly", "jellyfin")

	embyDir := t.TempDir()
	jellyDir := t.TempDir()
	embyLib := &Library{Name: "Emby Music", Path: embyDir, Type: TypeRegular, ConnectionID: "conn-emby", ExternalID: "emby-1", Source: SourceEmby}
	jellyLib := &Library{Name: "Jelly Music", Path: jellyDir, Type: TypeRegular, ConnectionID: "conn-jelly", ExternalID: "jelly-1", Source: SourceJellyfin}
	if err := svc.Create(ctx, embyLib); err != nil {
		t.Fatalf("Create emby library: %v", err)
	}
	if err := svc.Create(ctx, jellyLib); err != nil {
		t.Fatalf("Create jelly library: %v", err)
	}

	// Multi-home artist: membership in BOTH libraries.
	seedArtist(t, db, "art-multihome", "Tool", "")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		VALUES ('art-multihome', ?, 'emby', datetime('now')),
		       ('art-multihome', ?, 'jellyfin', datetime('now'))
	`, embyLib.ID, jellyLib.ID); err != nil {
		t.Fatalf("seed memberships: %v", err)
	}
	seedPlatformID(t, db, "art-multihome", "conn-emby", "emby-tool-1")
	seedPlatformID(t, db, "art-multihome", "conn-jelly", "jelly-tool-1")

	if err := svc.DeleteWithArtists(ctx, embyLib.ID); err != nil {
		t.Fatalf("DeleteWithArtists emby: %v", err)
	}

	// Artist row survives.
	var artistRows int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-multihome'`).Scan(&artistRows); err != nil {
		t.Fatalf("count artist: %v", err)
	}
	if artistRows != 1 {
		t.Errorf("artist count = %d, want 1 (multi-home survival)", artistRows)
	}

	// Jelly membership survives.
	var jellyMember int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = 'art-multihome' AND library_id = ?`,
		jellyLib.ID).Scan(&jellyMember); err != nil {
		t.Fatalf("count jelly membership: %v", err)
	}
	if jellyMember != 1 {
		t.Errorf("jelly membership count = %d, want 1", jellyMember)
	}

	// Platform mapping on the surviving connection is intact.
	var jellyMap int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = 'art-multihome' AND connection_id = 'conn-jelly'`).Scan(&jellyMap); err != nil {
		t.Fatalf("count jelly mapping: %v", err)
	}
	if jellyMap != 1 {
		t.Errorf("jelly mapping count = %d, want 1", jellyMap)
	}

	// Platform mapping on the unlinked connection is gone (the bug).
	var embyMap int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = 'art-multihome' AND connection_id = 'conn-emby'`).Scan(&embyMap); err != nil {
		t.Fatalf("count emby mapping: %v", err)
	}
	if embyMap != 0 {
		t.Errorf("emby mapping count = %d, want 0 (stale platform_id on unlinked connection must be cleaned)", embyMap)
	}
}

// TestDeleteWithArtists_PrunesStalePlatformIDsForMultiConnArtist covers
// the second survival branch of issue #1072: an artist whose only
// library membership is in the unlinked library, but which has
// platform_id mappings on a different connection. The candidate-prune
// loop preserves the artist (otherConnMappings > 0). The mapping on the
// unlinked connection must still be cleaned up because the artist no
// longer has any library backing it on that connection.
func TestDeleteWithArtists_PrunesStalePlatformIDsForMultiConnArtist(t *testing.T) {
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	seedConnection(t, db, "conn-emby", "emby")
	seedConnection(t, db, "conn-jelly", "jellyfin")

	embyDir := t.TempDir()
	embyLib := &Library{Name: "Emby Music", Path: embyDir, Type: TypeRegular, ConnectionID: "conn-emby", ExternalID: "emby-1", Source: SourceEmby}
	if err := svc.Create(ctx, embyLib); err != nil {
		t.Fatalf("Create emby library: %v", err)
	}

	// Artist has a membership ONLY in the Emby library, but holds
	// platform mappings on both Emby and Jelly. After unlink, the artist
	// survives because of the Jelly mapping; the Emby mapping is stale.
	seedArtist(t, db, "art-multiconn", "Mastodon", "")
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		VALUES ('art-multiconn', ?, 'emby', datetime('now'))
	`, embyLib.ID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	seedPlatformID(t, db, "art-multiconn", "conn-emby", "emby-mastodon-1")
	seedPlatformID(t, db, "art-multiconn", "conn-jelly", "jelly-mastodon-1")

	if err := svc.DeleteWithArtists(ctx, embyLib.ID); err != nil {
		t.Fatalf("DeleteWithArtists emby: %v", err)
	}

	// Artist survives because of the Jelly mapping.
	var artistRows int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-multiconn'`).Scan(&artistRows); err != nil {
		t.Fatalf("count artist: %v", err)
	}
	if artistRows != 1 {
		t.Errorf("artist count = %d, want 1 (preserved by other-conn mapping)", artistRows)
	}

	// Jelly mapping retained.
	var jellyMap int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = 'art-multiconn' AND connection_id = 'conn-jelly'`).Scan(&jellyMap); err != nil {
		t.Fatalf("count jelly mapping: %v", err)
	}
	if jellyMap != 1 {
		t.Errorf("jelly mapping count = %d, want 1", jellyMap)
	}

	// Emby mapping cleaned up.
	var embyMap int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = 'art-multiconn' AND connection_id = 'conn-emby'`).Scan(&embyMap); err != nil {
		t.Fatalf("count emby mapping: %v", err)
	}
	if embyMap != 0 {
		t.Errorf("emby mapping count = %d, want 0 (stale platform_id must be cleaned even when artist survives via other-conn mapping)", embyMap)
	}
}

// TestNFOLockData_RoundTrip verifies the new per-library NFOLockData column
// (issue #1264) round-trips through Create, Update, scan, and re-fetch.
// Default is false; explicit true persists; flipping back to false persists.
func TestNFOLockData_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	dir := t.TempDir()
	lib := &Library{Name: "Lock Test", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.GetByID(ctx, lib.ID)
	if err != nil {
		t.Fatalf("GetByID after Create: %v", err)
	}
	if got.NFOLockData {
		t.Error("default NFOLockData must be false on Create (issue #1264)")
	}

	got.NFOLockData = true
	if err := svc.Update(ctx, got); err != nil {
		t.Fatalf("Update enable: %v", err)
	}

	got2, err := svc.GetByID(ctx, lib.ID)
	if err != nil {
		t.Fatalf("GetByID after enable: %v", err)
	}
	if !got2.NFOLockData {
		t.Error("NFOLockData=true did not persist through Update")
	}

	got2.NFOLockData = false
	if err := svc.Update(ctx, got2); err != nil {
		t.Fatalf("Update disable: %v", err)
	}
	got3, err := svc.GetByID(ctx, lib.ID)
	if err != nil {
		t.Fatalf("GetByID after disable: %v", err)
	}
	if got3.NFOLockData {
		t.Error("NFOLockData=false did not persist on flip-back")
	}
}

// TestFindForArtistPath covers the publisher's mechanism for resolving which
// library owns a given artist path. Verifies (1) longest-prefix wins on
// nested libraries, (2) sibling-name-prefix collisions are not matched
// (parent /music/jazz must not claim /music/jazzfusion/album), (3) pathless
// libraries are skipped, and (4) absent ownership returns nil + nil.
func TestFindForArtistPath(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	base := t.TempDir()
	musicDir := filepath.Join(base, "music")
	jazzDir := filepath.Join(musicDir, "jazz")
	jazzfusionDir := filepath.Join(musicDir, "jazzfusion")
	for _, d := range []string{musicDir, jazzDir, jazzfusionDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	parent := &Library{Name: "All Music", Path: musicDir, Type: TypeRegular}
	jazz := &Library{Name: "Jazz", Path: jazzDir, Type: TypeRegular}
	jazzfusion := &Library{Name: "Jazz Fusion", Path: jazzfusionDir, Type: TypeRegular}
	pathless := &Library{Name: "API Only", Path: "", Type: TypeRegular}
	for _, lib := range []*Library{parent, jazz, jazzfusion, pathless} {
		if err := svc.Create(ctx, lib); err != nil {
			t.Fatalf("Create %s: %v", lib.Name, err)
		}
	}

	cases := []struct {
		name       string
		artistPath string
		want       string
	}{
		{"longest-prefix wins (jazz beats parent)", filepath.Join(jazzDir, "Coltrane"), jazz.ID},
		{"sibling-name-prefix not matched", filepath.Join(jazzfusionDir, "Weather Report"), jazzfusion.ID},
		{"parent claim when no nested match", filepath.Join(musicDir, "rock", "Beatles"), parent.ID},
		{"unowned path returns nil", filepath.Join(base, "elsewhere", "Mystery"), ""},
		{"empty artist path returns nil", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.FindForArtistPath(ctx, tc.artistPath)
			if err != nil {
				t.Fatalf("FindForArtistPath(%q): %v", tc.artistPath, err)
			}
			if tc.want == "" {
				if got != nil {
					t.Errorf("FindForArtistPath(%q) = %q, want nil", tc.artistPath, got.ID)
				}
				return
			}
			if got == nil {
				t.Fatalf("FindForArtistPath(%q) returned nil, want %q", tc.artistPath, tc.want)
			}
			if got.ID != tc.want {
				t.Errorf("FindForArtistPath(%q) = %q, want %q", tc.artistPath, got.ID, tc.want)
			}
		})
	}
}
