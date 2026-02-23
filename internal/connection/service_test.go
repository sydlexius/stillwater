package connection

import (
	"context"
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
}

func TestCreate_Validation(t *testing.T) {
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
	svc := setupTestService(t)
	if err := svc.Delete(context.Background(), "nonexistent"); err == nil {
		t.Error("expected error deleting nonexistent connection")
	}
}

func TestList_SkipsUndecryptableRows(t *testing.T) {
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

func TestUpdateStatus(t *testing.T) {
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
