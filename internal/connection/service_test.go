package connection

import (
	"context"
	"reflect"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
)

func setupTestService(t *testing.T) *Service {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	return NewService(db, enc)
}

func TestCreateAndGetByID(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{
		Name:    "Test Emby",
		Type:    TypeEmby,
		URL:     "http://localhost:8096",
		APIKey:  "test-api-key-12345",
		Enabled: true,
	}

	if err := svc.Create(ctx, c); err != nil {
		t.Fatalf("creating connection: %v", err)
	}
	if c.ID == "" {
		t.Fatal("expected ID to be set")
	}

	got, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatalf("getting connection: %v", err)
	}

	if got.Name != "Test Emby" {
		t.Errorf("Name = %q, want %q", got.Name, "Test Emby")
	}
	if got.Type != TypeEmby {
		t.Errorf("Type = %q, want %q", got.Type, TypeEmby)
	}
	if got.APIKey != "test-api-key-12345" {
		t.Errorf("APIKey = %q, want %q", got.APIKey, "test-api-key-12345")
	}
	if !got.Enabled {
		t.Error("expected Enabled to be true")
	}
	if got.Status != "unknown" {
		t.Errorf("Status = %q, want %q", got.Status, "unknown")
	}
	// An Emby connection round-trips with exactly the EmbyConfig populated.
	if got.Emby == nil || got.Lidarr != nil || got.Jellyfin != nil {
		t.Errorf("expected only EmbyConfig populated, got Lidarr=%v Emby=%v Jellyfin=%v", got.Lidarr, got.Emby, got.Jellyfin)
	}
}

func TestCreate_Validation(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	tests := []struct {
		name string
		conn Connection
	}{
		{"missing name", Connection{Type: TypeEmby, URL: "http://localhost", APIKey: "key"}},
		{"missing type", Connection{Name: "Test", URL: "http://localhost", APIKey: "key"}},
		{"invalid type", Connection{Name: "Test", Type: "invalid", URL: "http://localhost", APIKey: "key"}},
		{"missing url", Connection{Name: "Test", Type: TypeEmby, APIKey: "key"}},
		{"missing api_key", Connection{Name: "Test", Type: TypeEmby, URL: "http://localhost"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := svc.Create(ctx, &tt.conn); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestList(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c1 := &Connection{Name: "Alpha Emby", Type: TypeEmby, URL: "http://emby:8096", APIKey: "key1", Enabled: true}
	c2 := &Connection{Name: "Beta Jellyfin", Type: TypeJellyfin, URL: "http://jellyfin:8096", APIKey: "key2", Enabled: true}

	if err := svc.Create(ctx, c1); err != nil {
		t.Fatalf("creating c1: %v", err)
	}
	if err := svc.Create(ctx, c2); err != nil {
		t.Fatalf("creating c2: %v", err)
	}

	conns, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(conns) != 2 {
		t.Fatalf("got %d connections, want 2", len(conns))
	}
	// Should be ordered by name
	if conns[0].Name != "Alpha Emby" {
		t.Errorf("first connection = %q, want Alpha Emby", conns[0].Name)
	}
}

func TestListByType(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	if err := svc.Create(ctx, &Connection{Name: "Emby1", Type: TypeEmby, URL: "http://e1:8096", APIKey: "k1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(ctx, &Connection{Name: "Lidarr1", Type: TypeLidarr, URL: "http://l1:8686", APIKey: "k2", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	conns, err := svc.ListByType(ctx, TypeEmby)
	if err != nil {
		t.Fatalf("listing by type: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("got %d emby connections, want 1", len(conns))
	}
	if conns[0].Type != TypeEmby {
		t.Errorf("Type = %q, want emby", conns[0].Type)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{Name: "Original", Type: TypeEmby, URL: "http://old:8096", APIKey: "oldkey", Enabled: true}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	c.Name = "Updated"
	c.URL = "http://new:8096"
	c.APIKey = "newkey"
	c.Enabled = false

	if err := svc.Update(ctx, c); err != nil {
		t.Fatalf("updating: %v", err)
	}

	got, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Updated" {
		t.Errorf("Name = %q, want Updated", got.Name)
	}
	if got.APIKey != "newkey" {
		t.Errorf("APIKey = %q, want newkey", got.APIKey)
	}
	if got.Enabled {
		t.Error("expected Enabled to be false")
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{Name: "ToDelete", Type: TypeEmby, URL: "http://del:8096", APIKey: "key", Enabled: true}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	if err := svc.Delete(ctx, c.ID); err != nil {
		t.Fatalf("deleting: %v", err)
	}

	_, err := svc.GetByID(ctx, c.ID)
	if err == nil {
		t.Error("expected error getting deleted connection")
	}
}

func TestDelete_NotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	if err := svc.Delete(context.Background(), "nonexistent"); err == nil {
		t.Error("expected error deleting nonexistent connection")
	}
}

func TestList_SkipsUndecryptableRows(t *testing.T) {
	t.Parallel()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	svc := NewService(db, enc)
	ctx := context.Background()

	// Create a valid connection
	if err := svc.Create(ctx, &Connection{Name: "Good", Type: TypeEmby, URL: "http://good:8096", APIKey: "key1", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// Insert a row with garbage encrypted key directly into the database
	_, err = db.ExecContext(ctx, `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, status_message, created_at, updated_at)
		VALUES ('bad-id', 'Bad', 'emby', 'http://bad:8096', 'not-valid-ciphertext', 1, 'unknown', '', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')
	`)
	if err != nil {
		t.Fatalf("inserting bad row: %v", err)
	}

	conns, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List() should not return error with undecryptable rows: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("got %d connections, want 1 (should skip bad row)", len(conns))
	}
	if conns[0].Name != "Good" {
		t.Errorf("Name = %q, want Good", conns[0].Name)
	}
}

func TestList_EmptyEncryptedKey(t *testing.T) {
	t.Parallel()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	svc := NewService(db, enc)
	ctx := context.Background()

	// Insert a row with empty encrypted key (simulates reset-credentials)
	_, err = db.ExecContext(ctx, `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, status_message, created_at, updated_at)
		VALUES ('empty-key-id', 'ResetConn', 'lidarr', 'http://reset:8686', '', 1, 'unknown', '', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')
	`)
	if err != nil {
		t.Fatalf("inserting row with empty key: %v", err)
	}

	conns, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List() should not return error with empty key: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("got %d connections, want 1", len(conns))
	}
	if conns[0].APIKey != "" {
		t.Errorf("APIKey = %q, want empty string", conns[0].APIKey)
	}
	if conns[0].Name != "ResetConn" {
		t.Errorf("Name = %q, want ResetConn", conns[0].Name)
	}
}

func TestGetByTypeAndURL(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{Name: "Emby1", Type: TypeEmby, URL: "http://emby:8096", APIKey: "key1", Enabled: true}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	// Should find the connection
	got, err := svc.GetByTypeAndURL(ctx, TypeEmby, "http://emby:8096")
	if err != nil {
		t.Fatalf("GetByTypeAndURL: %v", err)
	}
	if got == nil {
		t.Fatal("expected to find connection, got nil")
	}
	if got.Name != "Emby1" {
		t.Errorf("Name = %q, want Emby1", got.Name)
	}

	// Should not find a connection with different type
	got, err = svc.GetByTypeAndURL(ctx, TypeJellyfin, "http://emby:8096")
	if err != nil {
		t.Fatalf("GetByTypeAndURL: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}

	// Should not find a connection with different URL
	got, err = svc.GetByTypeAndURL(ctx, TypeEmby, "http://other:8096")
	if err != nil {
		t.Fatalf("GetByTypeAndURL: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestDeduplicateByTypeURL(t *testing.T) {
	t.Parallel()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	svc := NewService(db, enc)
	ctx := context.Background()

	// Create 3 connections with the same type+url
	for i := 0; i < 3; i++ {
		c := &Connection{Name: "Emby", Type: TypeEmby, URL: "http://emby:8096", APIKey: "key", Enabled: true}
		if err := svc.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// Create 1 different connection
	if err := svc.Create(ctx, &Connection{Name: "Lidarr", Type: TypeLidarr, URL: "http://lidarr:8686", APIKey: "key2", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	removed, err := svc.DeduplicateByTypeURL(ctx)
	if err != nil {
		t.Fatalf("DeduplicateByTypeURL: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d rows, want 2", removed)
	}

	conns, err := svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 2 {
		t.Fatalf("got %d connections after dedup, want 2", len(conns))
	}
}

func TestCreatePreservesFeatureFlags(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	// Feature flags are set by the handler based on connection type.
	// Verify that explicitly-false flags are preserved (Lidarr read-only).
	c := &Connection{Name: "Lidarr", Type: TypeLidarr, URL: "http://lidarr:8686", APIKey: "key", Enabled: true}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A Lidarr connection gets only the LidarrConfig; the Emby/Jellyfin
	// feature toggles are unrepresentable rather than merely false.
	if got.Lidarr == nil || got.Emby != nil || got.Jellyfin != nil {
		t.Errorf("Lidarr connection should have only Lidarr config, got Lidarr=%v Emby=%v Jellyfin=%v", got.Lidarr, got.Emby, got.Jellyfin)
	}
	if got.GetFeatureImageWrite() {
		t.Error("expected FeatureImageWrite to remain false for Lidarr")
	}

	// Verify that explicitly-true flags are also preserved.
	c2 := &Connection{
		Name: "Emby", Type: TypeEmby, URL: "http://emby:8096", APIKey: "key2", Enabled: true,
		Emby: &EmbyConfig{FeatureImageWrite: true},
	}
	if err := svc.Create(ctx, c2); err != nil {
		t.Fatal(err)
	}
	got2, err := svc.GetByID(ctx, c2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Emby == nil {
		t.Fatal("Emby connection should have an EmbyConfig populated")
	}
	if !got2.Emby.FeatureImageWrite {
		t.Error("expected FeatureImageWrite to be true for Emby")
	}
}

func TestUpdateFeatures(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{Name: "FeatUpdate", Type: TypeEmby, URL: "http://fu:8096", APIKey: "key", Enabled: true}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	// Enable image write; leave metadata-push and trigger-refresh off.
	if err := svc.UpdateFeatures(ctx, c.ID, true, false, false); err != nil {
		t.Fatalf("UpdateFeatures: %v", err)
	}

	got, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.GetFeatureImageWrite() {
		t.Error("expected FeatureImageWrite to be true")
	}
	if got.GetFeatureMetadataPush() {
		t.Error("expected FeatureMetadataPush to be false")
	}
	if got.GetFeatureTriggerRefresh() {
		t.Error("expected FeatureTriggerRefresh to be false")
	}
}

func TestUpdateFeatures_NotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	if err := svc.UpdateFeatures(context.Background(), "nonexistent", true, false, false); err == nil {
		t.Error("expected error updating features for nonexistent connection")
	}
}

func TestUpdatePlatformUserID(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{Name: "PlatUID", Type: TypeEmby, URL: "http://plat:8096", APIKey: "key", Enabled: true}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	if err := svc.UpdatePlatformUserID(ctx, c.ID, "user-001"); err != nil {
		t.Fatalf("UpdatePlatformUserID: %v", err)
	}

	got, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetPlatformUserID() != "user-001" {
		t.Errorf("PlatformUserID = %q, want user-001", got.GetPlatformUserID())
	}

	// Not-found path.
	if err := svc.UpdatePlatformUserID(ctx, "nonexistent", "uid"); err == nil {
		t.Error("expected error updating platform user ID for nonexistent connection")
	}
}

func TestUpdate_PreservesPlatformUserID(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{Name: "Preserve", Type: TypeEmby, URL: "http://pres:8096", APIKey: "key", Enabled: true}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdatePlatformUserID(ctx, c.ID, "user-preserve"); err != nil {
		t.Fatalf("UpdatePlatformUserID: %v", err)
	}

	// Reload and change the name only, then call Update.
	got, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Name = "PreserveUpdated"
	if err := svc.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}

	after, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "PreserveUpdated" {
		t.Errorf("Name = %q, want PreserveUpdated", after.Name)
	}
	if after.GetPlatformUserID() != "user-preserve" {
		t.Errorf("PlatformUserID = %q, want user-preserve (must be preserved by Update)", after.GetPlatformUserID())
	}
}

func TestUpdateStatus(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{Name: "StatusTest", Type: TypeEmby, URL: "http://st:8096", APIKey: "key", Enabled: true}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	if err := svc.UpdateStatus(ctx, c.ID, "ok", ""); err != nil {
		t.Fatalf("updating status: %v", err)
	}

	got, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
	if got.LastCheckedAt == nil {
		t.Error("expected LastCheckedAt to be set")
	}
}

func TestNewFeatureFlags_DefaultFalse(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{Name: "Defaults", Type: TypeEmby, URL: "http://defaults:8096", APIKey: "key", Enabled: true}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetFeatureMetadataPush() {
		t.Error("expected FeatureMetadataPush to default to false")
	}
	if got.GetFeatureTriggerRefresh() {
		t.Error("expected FeatureTriggerRefresh to default to false")
	}
}

func TestUpdateFeatures_NewFlags(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{Name: "NewFlags", Type: TypeEmby, URL: "http://nf:8096", APIKey: "key", Enabled: true}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	// Enable metadata push but disable trigger refresh to verify independent storage.
	if err := svc.UpdateFeatures(ctx, c.ID, true, true, false); err != nil {
		t.Fatalf("UpdateFeatures: %v", err)
	}

	got, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.GetFeatureMetadataPush() {
		t.Error("expected FeatureMetadataPush to be true")
	}
	if got.GetFeatureTriggerRefresh() {
		t.Error("expected FeatureTriggerRefresh to be false")
	}
}

func TestUpdate_PreservesNewFeatureFlags(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{
		Name: "PreserveNew", Type: TypeEmby, URL: "http://on:8096", APIKey: "key", Enabled: true,
		Emby: &EmbyConfig{FeatureMetadataPush: true, FeatureTriggerRefresh: true},
	}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Name = "PreserveNewUpdated"
	if err := svc.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}

	after, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.GetFeatureMetadataPush() {
		t.Error("expected FeatureMetadataPush to be preserved as true")
	}
	if !after.GetFeatureTriggerRefresh() {
		t.Error("expected FeatureTriggerRefresh to be preserved as true")
	}
}

// TestSetManageServerFiles_RoundTrip covers the ON/OFF toggle path for the
// "Let Stillwater manage" setter plus the DB column read/write. Also pins
// down SetPreStillwaterConfig alongside since they are always paired.
func TestSetManageServerFiles_RoundTrip(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()
	conn := &Connection{Name: "e", Type: TypeEmby, URL: "http://localhost:8096", APIKey: "k"}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := svc.SetPreStillwaterConfig(ctx, conn.ID, `{"v":1}`); err != nil {
		t.Fatalf("set snapshot: %v", err)
	}
	if err := svc.SetManageServerFiles(ctx, conn.ID, true); err != nil {
		t.Fatalf("set toggle: %v", err)
	}

	got, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !got.FeatureManageServerFiles {
		t.Error("toggle should be on")
	}
	if got.PreStillwaterConfigJSON != `{"v":1}` {
		t.Errorf("snapshot round-trip = %q", got.PreStillwaterConfigJSON)
	}

	// Flip off + clear snapshot (mirrors the clearStillwaterManaged flow).
	if err := svc.SetManageServerFiles(ctx, conn.ID, false); err != nil {
		t.Fatalf("flip off: %v", err)
	}
	if err := svc.SetPreStillwaterConfig(ctx, conn.ID, ""); err != nil {
		t.Fatalf("clear snapshot: %v", err)
	}
	got, err = svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload after clear: %v", err)
	}
	if got.FeatureManageServerFiles || got.PreStillwaterConfigJSON != "" {
		t.Errorf("post-clear state = %+v", got)
	}
}

func TestSetManageServerFiles_ErrorOnUnknownID(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	if err := svc.SetManageServerFiles(context.Background(), "ghost", true); err == nil {
		t.Error("want error on unknown id")
	}
	if err := svc.SetPreStillwaterConfig(context.Background(), "ghost", "{}"); err == nil {
		t.Error("want error on unknown id")
	}
}

// TestVerifyPathAfterUpdate_RoundTrip exercises the persistence half of
// the #1640 toggle: a Lidarr connection inserted with VerifyPathAfterUpdate
// = true must read back true after a GetByID and stay sticky across an
// Update() that does not touch the field. Mirrors the
// FeatureManageServerFiles round-trip pattern.
func TestVerifyPathAfterUpdate_RoundTrip(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	conn := &Connection{
		Name:   "Lidarr verify on",
		Type:   TypeLidarr,
		URL:    "http://localhost:8686",
		APIKey: "key",
		Lidarr: &LidarrConfig{VerifyPathAfterUpdate: true},
	}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Lidarr == nil || !got.Lidarr.VerifyPathAfterUpdate {
		t.Error("VerifyPathAfterUpdate should be true after round-trip")
	}

	// Update without touching the field; it must persist.
	got.Name = "renamed"
	if err := svc.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	reloaded, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload after update: %v", err)
	}
	if reloaded.Lidarr == nil || !reloaded.Lidarr.VerifyPathAfterUpdate {
		t.Error("VerifyPathAfterUpdate should survive an Update that did not change it")
	}
}

// TestPathMappings_RoundTrip exercises the persistence half of the #2303
// path-mapping list: a Lidarr connection created with mappings reads them back
// after GetByID, they survive an Update that does not touch them, and clearing
// them (empty slice) reads back as no mappings.
func TestPathMappings_RoundTrip(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	want := []PathMapping{
		{HostPrefix: "/music", PlatformPrefix: "/data/media"},
		{HostPrefix: "/media/audio", PlatformPrefix: "/mnt/audio"},
	}
	conn := &Connection{
		Name:         "Lidarr split mount",
		Type:         TypeLidarr,
		URL:          "http://localhost:8686",
		APIKey:       "key",
		PathMappings: want,
	}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(got.GetPathMappings(), want) {
		t.Fatalf("PathMappings after round-trip = %+v, want %+v", got.GetPathMappings(), want)
	}

	// Update without touching the field; it must persist.
	got.Name = "renamed"
	if err := svc.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	reloaded, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload after update: %v", err)
	}
	if !reflect.DeepEqual(reloaded.GetPathMappings(), want) {
		t.Errorf("PathMappings should survive an Update that did not change it: got %+v", reloaded.GetPathMappings())
	}

	// Clear the mappings; the column returns to empty and reads back nil.
	reloaded.SetPathMappings(nil)
	if err := svc.Update(ctx, reloaded); err != nil {
		t.Fatalf("update (clear): %v", err)
	}
	cleared, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload after clear: %v", err)
	}
	if len(cleared.GetPathMappings()) != 0 {
		t.Errorf("PathMappings after clear = %+v, want none", cleared.GetPathMappings())
	}
}

// TestPathMappings_DefaultsEmpty pins the back-compat default: a Lidarr
// connection created without mappings reads back with none, so shared-mount
// deployments keep verbatim path propagation.
func TestPathMappings_DefaultsEmpty(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	conn := &Connection{
		Name:   "Lidarr shared mount",
		Type:   TypeLidarr,
		URL:    "http://localhost:8687",
		APIKey: "key",
	}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.GetPathMappings()) != 0 {
		t.Errorf("default PathMappings = %+v, want none", got.GetPathMappings())
	}
}

// TestVerifyPathAfterUpdate_DefaultsFalse pins the opt-in default required
// by issue #1640: a connection inserted without setting the field reads
// back false. Existing rows on upgrade hit the same path via the
// ensureConnectionsColumn DEFAULT 0 fallback in migrate.go.
func TestVerifyPathAfterUpdate_DefaultsFalse(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	conn := &Connection{
		Name:   "Lidarr default",
		Type:   TypeLidarr,
		URL:    "http://localhost:8686",
		APIKey: "key",
	}
	if err := svc.Create(ctx, conn); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := svc.GetByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.GetVerifyPathAfterUpdate() {
		t.Error("VerifyPathAfterUpdate should default to false")
	}
}

func TestUpdatePlatformServerID(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	ctx := context.Background()

	c := &Connection{Name: "PlatSID", Type: TypeEmby, URL: "http://platsid:8096", APIKey: "key", Enabled: true}
	if err := svc.Create(ctx, c); err != nil {
		t.Fatal(err)
	}

	if err := svc.UpdatePlatformServerID(ctx, c.ID, "srv-001"); err != nil {
		t.Fatalf("UpdatePlatformServerID: %v", err)
	}

	got, err := svc.GetByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetPlatformServerID() != "srv-001" {
		t.Errorf("PlatformServerID = %q, want srv-001", got.GetPlatformServerID())
	}

	// Not-found path.
	if err := svc.UpdatePlatformServerID(ctx, "nonexistent", "sid"); err == nil {
		t.Error("expected error updating platform server ID for nonexistent connection")
	}
}

func TestUpdate_NotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)

	c := &Connection{
		ID:     "ghost-id",
		Name:   "Ghost",
		Type:   TypeEmby,
		URL:    "http://ghost:8096",
		APIKey: "key",
	}
	if err := svc.Update(context.Background(), c); err == nil {
		t.Error("expected error updating nonexistent connection")
	}
}

func TestListByType_SkipsUndecryptableRows(t *testing.T) {
	t.Parallel()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	svc := NewService(db, enc)
	ctx := context.Background()

	// Create a valid Emby connection.
	if err := svc.Create(ctx, &Connection{Name: "Good", Type: TypeEmby, URL: "http://good:8096", APIKey: "key1", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// Insert an Emby row with a garbage encrypted key directly.
	_, err = db.ExecContext(ctx, `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, status_message, created_at, updated_at)
		VALUES ('bad-sid', 'Bad', 'emby', 'http://bad:8096', 'not-valid-ciphertext', 1, 'unknown', '', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')
	`)
	if err != nil {
		t.Fatalf("inserting bad row: %v", err)
	}

	conns, err := svc.ListByType(ctx, TypeEmby)
	if err != nil {
		t.Fatalf("ListByType() should not return error with undecryptable rows: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("got %d connections, want 1 (should skip undecryptable row)", len(conns))
	}
	if conns[0].Name != "Good" {
		t.Errorf("Name = %q, want Good", conns[0].Name)
	}
}
