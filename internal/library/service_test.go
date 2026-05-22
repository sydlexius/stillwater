package library

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	t.Parallel()
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
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)

	_, err := svc.GetByID(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent ID")
	}
}

func TestGetByPath(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	lib.Type = TypeRegular
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
	if got.Type != TypeRegular {
		t.Errorf("Type = %q, want %q", got.Type, TypeRegular)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)

	lib := &Library{ID: "nonexistent", Name: "Test", Type: TypeRegular}
	err := svc.Update(context.Background(), lib)
	if err == nil {
		t.Fatal("expected error for nonexistent update")
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	dir := t.TempDir()
	lib := &Library{Name: "Has Artists", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create library: %v", err)
	}

	// Enable FKs so the artist_libraries -> libraries cascade actually
	// fires when the library row is deleted.
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}

	// Insert an artist referencing this library via artist_libraries.
	_, err := db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		VALUES ('art-1', 'Test Artist', 'Test Artist', '/music/test', datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("inserting artist: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_libraries (artist_id, library_id, source) VALUES ('art-1', ?, 'filesystem')`,
		lib.ID); err != nil {
		t.Fatalf("inserting artist_libraries: %v", err)
	}

	// Delete should succeed.
	if err := svc.Delete(ctx, lib.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	// Library should be gone.
	if _, err := svc.GetByID(ctx, lib.ID); err == nil {
		t.Error("library should not exist after delete")
	}

	// Issue #1613: art-1 had only one membership (in the deleted library) and
	// no platform mappings, so pruneStrictOrphans must have removed it. The old
	// behavior was to preserve artists unconditionally; the new behavior deletes
	// true zero-home orphans.
	var artistCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-1'`).Scan(&artistCount); err != nil {
		t.Fatalf("counting artist: %v", err)
	}
	if artistCount != 0 {
		t.Errorf("zero-home orphan artist count = %d, want 0 (pruneStrictOrphans must delete it)", artistCount)
	}

	// Membership row is gone (either via FK CASCADE on the library delete,
	// or via the prune before the artist delete).
	var memberCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = 'art-1'`).Scan(&memberCount); err != nil {
		t.Fatalf("counting memberships: %v", err)
	}
	if memberCount != 0 {
		t.Errorf("memberships after library delete = %d, want 0", memberCount)
	}
}

func TestDelete_NotFound(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)

	err := svc.Delete(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent delete")
	}
}

func TestCountArtists(t *testing.T) {
	t.Parallel()
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

	// Add an artist with a membership.
	_, err = db.ExecContext(ctx, `
		INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		VALUES ('art-2', 'Artist 2', 'Artist 2', '/music/art2', datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("inserting artist: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_libraries (artist_id, library_id, source) VALUES ('art-2', ?, 'filesystem')`,
		lib.ID); err != nil {
		t.Fatalf("inserting artist_libraries: %v", err)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestUpdate_RejectsClassical verifies that Update returns an error when
// the library type is set to "classical", which was removed in v1.3.0.
func TestUpdate_RejectsClassical(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	dir := t.TempDir()
	lib := &Library{Name: "Music", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create: %v", err)
	}

	lib.Type = "classical"
	err := svc.Update(ctx, lib)
	if err == nil {
		t.Fatal("Update with type=classical: expected error, got nil")
	}
	// Lock the rejection contract: the message must name 'regular' as the
	// required type so callers (and future migrations) can rely on it. A
	// bare non-nil error would also satisfy the assertion above but would
	// permit a silent rewording that breaks downstream message-matching.
	if msg := err.Error(); !strings.Contains(msg, "regular") {
		t.Errorf("Update error %q does not name the required type 'regular'", msg)
	}
}

func TestIsSharedFS(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)

	err := svc.SetSharedFSStatus(context.Background(), "nonexistent", SharedFSNone, "", "")
	if err == nil {
		t.Fatal("expected error for nonexistent library")
	}
}

func TestListSharedFS(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// seedArtist inserts an artist row and (when libraryID is non-empty) its
// matching artist_libraries membership. Replaces the legacy library_id
// column setup.
func seedArtist(t *testing.T, db *sql.DB, id, name, libraryID string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
		VALUES (?, ?, ?, ?, datetime('now'), datetime('now'))
	`, id, name, name, "/music/"+name)
	if err != nil {
		t.Fatalf("seeding artist %s: %v", id, err)
	}
	if libraryID == "" {
		return
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		VALUES (?, ?, 'filesystem', datetime('now'))`,
		id, libraryID); err != nil {
		t.Fatalf("seeding artist_libraries for %s: %v", id, err)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	seedConnection(t, db, "conn-emby", "emby")

	// Two libraries on the same connection.
	musicDir := t.TempDir()
	jazzDir := t.TempDir()
	music := &Library{Name: "Music", Path: musicDir, Type: TypeRegular, ConnectionID: "conn-emby", ExternalID: "emby-music", Source: SourceEmby}
	jazz := &Library{Name: "Jazz", Path: jazzDir, Type: TypeRegular, ConnectionID: "conn-emby", ExternalID: "emby-jazz", Source: SourceEmby}
	if err := svc.Create(ctx, music); err != nil {
		t.Fatalf("Create music library: %v", err)
	}
	if err := svc.Create(ctx, jazz); err != nil {
		t.Fatalf("Create jazz library: %v", err)
	}

	// Artist directly attached to the library being deleted.
	seedArtist(t, db, "art-music", "MusicArtist", music.ID)
	seedPlatformID(t, db, "art-music", "conn-emby", "emby-music-1")

	// Artist with NULL library_id but a connection mapping. With only
	// the music library on this connection, the old prune would delete
	// this row. With the jazz library still attached, the new
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

	// The sibling-library orphan is preserved because the jazz
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
	if err := svc.DeleteWithArtists(ctx, jazz.ID); err != nil {
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
	t.Parallel()
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

	// Artist in BOTH libraries via membership rows. seedArtist creates the
	// fs membership; the second INSERT adds the emby one explicitly.
	seedArtist(t, db, "art-multi", "Radiohead", fsLib.ID)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		VALUES ('art-multi', ?, 'emby', datetime('now'))
	`, embyLib.ID); err != nil {
		t.Fatalf("seed emby membership: %v", err)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	base := t.TempDir()
	musicDir := filepath.Join(base, "music")
	jazzDir := filepath.Join(musicDir, "jazz")
	// jazzfusionDir exists on disk but is intentionally NOT registered as a
	// library: this lets the "sibling-name-prefix" case prove that
	// FindForArtistPath does NOT incorrectly accept "/music/jazz" as a
	// prefix of "/music/jazzfusion/...". With a real jazzfusion library
	// registered, the lookup would always succeed regardless of any
	// prefix-collision bug, hiding the regression.
	jazzfusionDir := filepath.Join(musicDir, "jazzfusion")
	for _, d := range []string{musicDir, jazzDir, jazzfusionDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	parent := &Library{Name: "All Music", Path: musicDir, Type: TypeRegular}
	jazz := &Library{Name: "Jazz", Path: jazzDir, Type: TypeRegular}
	pathless := &Library{Name: "API Only", Path: "", Type: TypeRegular}
	for _, lib := range []*Library{parent, jazz, pathless} {
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
		{"sibling-prefix falls back to parent", filepath.Join(jazzfusionDir, "Weather Report"), parent.ID},
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

// TestListByConnectionID_AndCountArtists verifies the per-connection helpers
// after the M:N storage cleanup. CountArtistsByConnectionID joins through
// artist_libraries, so an artist that belongs to TWO libraries on the same
// connection must only be counted once (DISTINCT).
func TestListByConnectionID_AndCountArtists(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Two connections; one of them has two libraries.
	for _, conn := range []struct{ id, name string }{
		{"conn-emby", "Emby"},
		{"conn-jelly", "Jellyfin"},
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
			VALUES (?, ?, 'emby', 'http://x:8096', 'enc', 1, 'ok', datetime('now'), datetime('now'))
		`, conn.id, conn.name); err != nil {
			t.Fatalf("inserting connection %s: %v", conn.id, err)
		}
	}

	libs := []*Library{
		{Name: "Emby Main", Type: TypeRegular, Source: SourceEmby, ConnectionID: "conn-emby", ExternalID: "ext-1"},
		{Name: "Emby Side", Type: TypeRegular, Source: SourceEmby, ConnectionID: "conn-emby", ExternalID: "ext-2"},
		{Name: "Jelly Main", Type: TypeRegular, Source: SourceJellyfin, ConnectionID: "conn-jelly", ExternalID: "ext-3"},
	}
	for _, lib := range libs {
		if err := svc.Create(ctx, lib); err != nil {
			t.Fatalf("Create %s: %v", lib.Name, err)
		}
	}

	// ListByConnectionID returns libraries belonging to a connection, sorted by name.
	embyLibs, err := svc.ListByConnectionID(ctx, "conn-emby")
	if err != nil {
		t.Fatalf("ListByConnectionID(conn-emby): %v", err)
	}
	if len(embyLibs) != 2 {
		t.Fatalf("len(embyLibs) = %d, want 2", len(embyLibs))
	}
	if embyLibs[0].Name != "Emby Main" || embyLibs[1].Name != "Emby Side" {
		t.Errorf("embyLibs names = [%q, %q], want [Emby Main, Emby Side]",
			embyLibs[0].Name, embyLibs[1].Name)
	}

	jellyLibs, err := svc.ListByConnectionID(ctx, "conn-jelly")
	if err != nil {
		t.Fatalf("ListByConnectionID(conn-jelly): %v", err)
	}
	if len(jellyLibs) != 1 {
		t.Fatalf("len(jellyLibs) = %d, want 1", len(jellyLibs))
	}

	// Empty result for an unknown connection.
	none, err := svc.ListByConnectionID(ctx, "nope")
	if err != nil {
		t.Fatalf("ListByConnectionID(nope): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("len(none) = %d, want 0", len(none))
	}

	// Insert artists with memberships:
	//   art-shared belongs to BOTH Emby libraries (must count as 1).
	//   art-side belongs only to Emby Side.
	//   art-jelly belongs only to Jelly Main.
	for _, a := range []struct{ id string }{{"art-shared"}, {"art-side"}, {"art-jelly"}} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
			VALUES (?, ?, ?, '/music/'||?, datetime('now'), datetime('now'))
		`, a.id, a.id, a.id, a.id); err != nil {
			t.Fatalf("inserting artist %s: %v", a.id, err)
		}
	}
	for _, link := range []struct{ artistID, libID string }{
		{"art-shared", embyLibs[0].ID},
		{"art-shared", embyLibs[1].ID},
		{"art-side", embyLibs[1].ID},
		{"art-jelly", jellyLibs[0].ID},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_libraries (artist_id, library_id, source) VALUES (?, ?, 'filesystem')`,
			link.artistID, link.libID); err != nil {
			t.Fatalf("inserting artist_libraries (%s, %s): %v", link.artistID, link.libID, err)
		}
	}

	embyCount, err := svc.CountArtistsByConnectionID(ctx, "conn-emby")
	if err != nil {
		t.Fatalf("CountArtistsByConnectionID(conn-emby): %v", err)
	}
	// art-shared (1) + art-side (1) = 2 distinct, even though art-shared has
	// two membership rows on conn-emby's libraries.
	if embyCount != 2 {
		t.Errorf("embyCount = %d, want 2 (DISTINCT artists across both Emby libraries)", embyCount)
	}

	jellyCount, err := svc.CountArtistsByConnectionID(ctx, "conn-jelly")
	if err != nil {
		t.Fatalf("CountArtistsByConnectionID(conn-jelly): %v", err)
	}
	if jellyCount != 1 {
		t.Errorf("jellyCount = %d, want 1", jellyCount)
	}

	zeroCount, err := svc.CountArtistsByConnectionID(ctx, "nope")
	if err != nil {
		t.Fatalf("CountArtistsByConnectionID(nope): %v", err)
	}
	if zeroCount != 0 {
		t.Errorf("zeroCount = %d, want 0", zeroCount)
	}
}

// -----------------------------------------------------------------------------
// Issue #1613 -- pruneStrictOrphans / orphan GC in Service.Delete
// -----------------------------------------------------------------------------

// TestDelete_PrunesZeroHomeOrphan verifies the primary acceptance criterion:
// when a library removal leaves an artist with zero artist_libraries memberships
// AND zero artist_platform_ids rows, Service.Delete must delete that artist row
// (and let its child rows cascade).
func TestDelete_PrunesZeroHomeOrphan(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	dir := t.TempDir()
	lib := &Library{Name: "Music", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create library: %v", err)
	}

	// Artist belongs only to this library; no platform mappings.
	seedArtist(t, db, "art-orphan", "Orphan", lib.ID)

	// Sanity: artist is present before the delete.
	var pre int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-orphan'`).Scan(&pre); err != nil {
		t.Fatalf("pre-count: %v", err)
	}
	if pre != 1 {
		t.Fatalf("pre-count = %d, want 1", pre)
	}

	if err := svc.Delete(ctx, lib.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Library is gone.
	if _, err := svc.GetByID(ctx, lib.ID); err == nil {
		t.Error("library should not exist after delete")
	}

	// Orphan artist must be gone.
	var post int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-orphan'`).Scan(&post); err != nil {
		t.Fatalf("post-count: %v", err)
	}
	if post != 0 {
		t.Errorf("orphan artist count = %d, want 0 (pruneStrictOrphans must delete it)", post)
	}

	// Membership row is also gone (cascade or prune).
	var mem int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = 'art-orphan'`).Scan(&mem); err != nil {
		t.Fatalf("membership count: %v", err)
	}
	if mem != 0 {
		t.Errorf("membership count = %d, want 0", mem)
	}
}

// TestDelete_PreservesArtistWithSiblingLibrary verifies that an artist with a
// membership in a second library is NOT pruned when the first library is deleted.
func TestDelete_PreservesArtistWithSiblingLibrary(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	base := t.TempDir()
	// Two distinct directories for the two libraries.
	dir1 := filepath.Join(base, "lib1")
	dir2 := filepath.Join(base, "lib2")
	for _, d := range []string{dir1, dir2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	lib1 := &Library{Name: "Lib One", Path: dir1, Type: TypeRegular}
	lib2 := &Library{Name: "Lib Two", Path: dir2, Type: TypeRegular}
	if err := svc.Create(ctx, lib1); err != nil {
		t.Fatalf("Create lib1: %v", err)
	}
	if err := svc.Create(ctx, lib2); err != nil {
		t.Fatalf("Create lib2: %v", err)
	}

	// Artist belongs to both libraries.
	seedArtist(t, db, "art-multi", "Multi", lib1.ID)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		 VALUES ('art-multi', ?, 'filesystem', datetime('now'))`,
		lib2.ID); err != nil {
		t.Fatalf("second membership: %v", err)
	}

	// Delete lib1: art-multi still has a membership in lib2 so it must survive.
	if err := svc.Delete(ctx, lib1.ID); err != nil {
		t.Fatalf("Delete lib1: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-multi'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("artist count = %d, want 1 (sibling library membership must preserve the artist)", n)
	}

	// The lib2 membership must still be intact.
	var lib2Mem int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = 'art-multi' AND library_id = ?`,
		lib2.ID).Scan(&lib2Mem); err != nil {
		t.Fatalf("lib2 membership count: %v", err)
	}
	if lib2Mem != 1 {
		t.Errorf("lib2 membership = %d, want 1", lib2Mem)
	}
}

// TestDelete_PreservesArtistWithPlatformMapping verifies that an artist with no
// library memberships but a live artist_platform_ids row is NOT pruned. Service.Delete
// is deliberately more conservative than DeleteWithArtists: a platform mapping is
// a sufficient anchor to keep the artist.
func TestDelete_PreservesArtistWithPlatformMapping(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	seedConnection(t, db, "conn-emby", "emby")

	dir := t.TempDir()
	lib := &Library{Name: "Platform Test Lib", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create library: %v", err)
	}

	// Artist is in the library and has a platform mapping.
	seedArtist(t, db, "art-mapped", "Mapped Artist", lib.ID)
	seedPlatformID(t, db, "art-mapped", "conn-emby", "emby-art-mapped-1")

	if err := svc.Delete(ctx, lib.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Artist survives because of the platform mapping.
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = 'art-mapped'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("artist count = %d, want 1 (platform mapping must preserve the artist)", n)
	}

	// Platform mapping itself is still intact.
	var pm int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = 'art-mapped'`).Scan(&pm); err != nil {
		t.Fatalf("platform mapping count: %v", err)
	}
	if pm != 1 {
		t.Errorf("platform mapping count = %d, want 1", pm)
	}
}

// TestDelete_CancelledContext verifies that Service.Delete propagates a
// BeginTx failure when the caller's context is already canceled. The
// transactional rewrite introduced in issue #1613 changed Delete from a
// single ExecContext call to a tx.BeginTx + work + tx.Commit sequence; this
// test covers the BeginTx error-return branch.
func TestDelete_CancelledContext(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)

	// A pre-canceled context causes sql.DB.BeginTx to return immediately with
	// context.Canceled before any SQL is executed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := svc.Delete(ctx, "irrelevant-id")
	if err == nil {
		t.Fatal("expected error when context is already canceled, got nil")
	}
}

// TestDelete_PruneArtistDeleteFails exercises the error-propagation path in
// pruneStrictOrphans when the DELETE FROM artists statement fails inside the
// transaction. A BEFORE DELETE trigger on the artists table causes the DELETE
// to raise a constraint error, which pruneStrictOrphans wraps and returns, and
// which Delete returns without committing (the deferred Rollback fires).
//
// This is a SQL-level injection approach rather than a Go-interface mock: it
// uses a standard SQLite trigger, not any test double or interface replacement.
func TestDelete_PruneArtistDeleteFails(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	dir := t.TempDir()
	lib := &Library{Name: "Prune Error Lib", Path: dir, Type: TypeRegular}
	if err := svc.Create(ctx, lib); err != nil {
		t.Fatalf("Create library: %v", err)
	}

	// Create a true zero-home orphan: one library membership, no platform mappings.
	// After the library is deleted the candidate will have zero memberships and
	// zero platform rows, so pruneStrictOrphans will try to DELETE the artist row.
	seedArtist(t, db, "art-blocked", "Blocked Artist", lib.ID)

	// Install a BEFORE DELETE trigger on artists that always raises ABORT.
	// The trigger fires when pruneStrictOrphans tries to remove the orphan
	// after the library row (and its cascade to artist_libraries) is gone.
	// The library delete itself cascades to artist_libraries, not artists,
	// so the trigger does not interfere with the library deletion step.
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER block_artist_delete_test
		BEFORE DELETE ON artists
		BEGIN
			SELECT RAISE(ABORT, 'artist delete blocked by test trigger');
		END
	`); err != nil {
		t.Fatalf("creating delete-blocking trigger: %v", err)
	}

	err := svc.Delete(ctx, lib.ID)
	if err == nil {
		t.Fatal("expected error from pruneStrictOrphans when artist delete is blocked, got nil")
	}

	// The transaction was rolled back, so neither the library nor the
	// artist_libraries membership was actually removed.
	if _, err := svc.GetByID(ctx, lib.ID); err != nil {
		t.Errorf("library should still exist after rollback: %v", err)
	}

	var memCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = 'art-blocked'`).Scan(&memCount); err != nil {
		t.Fatalf("counting memberships after rollback: %v", err)
	}
	if memCount != 1 {
		t.Errorf("membership count = %d, want 1 (rollback must have restored it)", memCount)
	}
}

// TestDelete_MultipleOrphansAndPreserved verifies that pruneStrictOrphans
// handles a library with more than one candidate artist in a single Delete call:
// each candidate is evaluated independently and only true zero-home orphans
// are removed. Artists with a surviving library membership or a platform mapping
// are left untouched.
func TestDelete_MultipleOrphansAndPreserved(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	if err := database.EnableForeignKeys(db); err != nil {
		t.Fatalf("enabling foreign keys: %v", err)
	}
	svc := NewService(db)
	ctx := context.Background()

	base := t.TempDir()
	targetDir := filepath.Join(base, "target")
	siblingDir := filepath.Join(base, "sibling")
	for _, d := range []string{targetDir, siblingDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	targetLib := &Library{Name: "Target", Path: targetDir, Type: TypeRegular}
	siblingLib := &Library{Name: "Sibling", Path: siblingDir, Type: TypeRegular}
	if err := svc.Create(ctx, targetLib); err != nil {
		t.Fatalf("Create target: %v", err)
	}
	if err := svc.Create(ctx, siblingLib); err != nil {
		t.Fatalf("Create sibling: %v", err)
	}

	seedConnection(t, db, "conn-test", "emby")

	// art-orphan1 and art-orphan2: only in targetLib, no platform mappings.
	// Both must be deleted by pruneStrictOrphans.
	seedArtist(t, db, "art-orphan1", "Orphan One", targetLib.ID)
	seedArtist(t, db, "art-orphan2", "Orphan Two", targetLib.ID)

	// art-sibling: in targetLib AND siblingLib. Must survive because siblingLib
	// membership survives the cascade.
	seedArtist(t, db, "art-sibling", "Sibling Artist", targetLib.ID)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_libraries (artist_id, library_id, source, added_at)
		 VALUES ('art-sibling', ?, 'filesystem', datetime('now'))`,
		siblingLib.ID); err != nil {
		t.Fatalf("second membership for art-sibling: %v", err)
	}

	// art-platform: in targetLib only but has a platform mapping. Must survive.
	seedArtist(t, db, "art-platform", "Platform Artist", targetLib.ID)
	seedPlatformID(t, db, "art-platform", "conn-test", "ext-platform-001")

	if err := svc.Delete(ctx, targetLib.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Library is gone.
	if _, err := svc.GetByID(ctx, targetLib.ID); err == nil {
		t.Error("target library should not exist after delete")
	}

	check := func(id string, wantPresent bool) {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM artists WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count artist %s: %v", id, err)
		}
		got := n > 0
		if got != wantPresent {
			t.Errorf("artist %s present = %v, want %v", id, got, wantPresent)
		}
	}
	check("art-orphan1", false)
	check("art-orphan2", false)
	check("art-sibling", true)
	check("art-platform", true)

	// Sibling's surviving library membership is intact.
	var sibMem int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = 'art-sibling' AND library_id = ?`,
		siblingLib.ID).Scan(&sibMem); err != nil {
		t.Fatalf("sibling membership count: %v", err)
	}
	if sibMem != 1 {
		t.Errorf("sibling membership = %d, want 1", sibMem)
	}
}
