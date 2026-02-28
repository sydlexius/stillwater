package settingsio

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/webhook"
	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, stmt := range []string{
		"PRAGMA journal_mode=WAL",
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS connections (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			url TEXT NOT NULL,
			encrypted_api_key TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'unknown',
			status_message TEXT NOT NULL DEFAULT '',
			last_checked_at TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			feature_library_import INTEGER NOT NULL DEFAULT 1,
			feature_nfo_write INTEGER NOT NULL DEFAULT 1,
			feature_image_write INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS platform_profiles (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			is_builtin INTEGER NOT NULL DEFAULT 0,
			is_active INTEGER NOT NULL DEFAULT 0,
			nfo_enabled INTEGER NOT NULL DEFAULT 1,
			nfo_format TEXT NOT NULL DEFAULT 'kodi',
			image_naming TEXT NOT NULL DEFAULT '{}',
			use_symlinks INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS webhooks (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			url TEXT NOT NULL,
			type TEXT NOT NULL DEFAULT 'generic',
			events TEXT NOT NULL DEFAULT '[]',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
	} {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return db
}

// newTestServices creates the dependent services needed by settingsio.Service.
// The encryption.Encryptor is still needed by connection.Service and
// provider.SettingsService for at-rest encryption of API keys; it is NOT used
// by settingsio.Service itself.
func newTestServices(t *testing.T, db *sql.DB) (*provider.SettingsService, *connection.Service, *platform.Service, *webhook.Service) {
	t.Helper()
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	return provider.NewSettingsService(db, enc),
		connection.NewService(db, enc),
		platform.NewService(db),
		webhook.NewService(db)
}

func TestRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)

	// Seed some test data
	now := time.Now().UTC().Format(time.RFC3339)
	db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ('test.key', 'test.value', ?)`, now)

	c := &connection.Connection{
		Name:    "Test Emby",
		Type:    "emby",
		URL:     "http://localhost:8096",
		APIKey:  "my-api-key",
		Enabled: true,
	}
	if err := connSvc.Create(ctx, c); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	p := &platform.Profile{
		Name:       "Test Profile",
		NFOEnabled: true,
		NFOFormat:  "kodi",
		ImageNaming: platform.ImageNaming{
			Thumb: []string{"folder.jpg"},
		},
	}
	if err := platSvc.Create(ctx, p); err != nil {
		t.Fatalf("creating profile: %v", err)
	}

	wh := &webhook.Webhook{
		Name:    "Test Hook",
		URL:     "https://example.com/hook",
		Type:    "generic",
		Events:  []string{"artist.new"},
		Enabled: true,
	}
	if err := whSvc.Create(ctx, wh); err != nil {
		t.Fatalf("creating webhook: %v", err)
	}

	// Export with passphrase
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	passphrase := "test-export-passphrase"
	envelope, err := svc.Export(ctx, passphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	if envelope.Version != "1.0" {
		t.Errorf("expected version 1.0, got %s", envelope.Version)
	}
	if envelope.Data == "" {
		t.Error("expected non-empty encrypted data")
	}
	if envelope.Salt == "" {
		t.Error("expected non-empty salt")
	}

	// Set up a fresh DB to import into (different instance, different encryptor)
	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)

	// Import with the same passphrase
	result, err := svc2.Import(ctx, envelope, passphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if result.Settings == 0 {
		t.Error("expected at least one setting imported")
	}
	if result.Connections != 1 {
		t.Errorf("expected 1 connection, got %d", result.Connections)
	}
	if result.Profiles != 1 {
		t.Errorf("expected 1 profile, got %d", result.Profiles)
	}
	if result.Webhooks != 1 {
		t.Errorf("expected 1 webhook, got %d", result.Webhooks)
	}

	// Verify imported data
	var testVal string
	db2.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'test.key'`).Scan(&testVal)
	if testVal != "test.value" {
		t.Errorf("expected test.value, got %s", testVal)
	}

	conns, _ := connSvc2.List(ctx)
	if len(conns) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(conns))
	}
	if conns[0].Name != "Test Emby" {
		t.Errorf("expected 'Test Emby', got %s", conns[0].Name)
	}
}

func TestImport_CorruptedData(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	// Try importing with corrupted data
	env := &Envelope{
		Version: "1.0",
		Salt:    "AAAAAAAAAAAAAAAAAAAAAA==",
		Data:    "not-valid-base64-encrypted-data",
	}

	_, err := svc.Import(ctx, env, "some-passphrase")
	if err == nil {
		t.Error("expected error for corrupted data")
	}
}

func TestImport_WrongPassphrase(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ('x', 'y', datetime('now'))`)

	envelope, err := svc.Export(ctx, "correct-passphrase")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Try importing with a different passphrase
	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)

	_, err = svc2.Import(ctx, envelope, "wrong-passphrase")
	if err == nil {
		t.Error("expected error when importing with wrong passphrase")
	}
}

func TestImport_UpsertNoDuplication(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	// Pre-populate with a connection
	c := &connection.Connection{
		Name:    "Emby",
		Type:    "emby",
		URL:     "http://emby:8096",
		APIKey:  "key1",
		Enabled: true,
	}
	connSvc.Create(ctx, c)

	// Export
	passphrase := "upsert-test"
	envelope, err := svc.Export(ctx, passphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Import again (should upsert, not duplicate)
	result, err := svc.Import(ctx, envelope, passphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Verify no duplication
	conns, _ := connSvc.List(ctx)
	if len(conns) != 1 {
		t.Errorf("expected 1 connection after upsert, got %d", len(conns))
	}

	_ = result
}

func TestImport_EmptyData(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	env := &Envelope{Version: "1.0", Data: ""}
	_, err := svc.Import(ctx, env, "any-passphrase")
	if err == nil {
		t.Error("expected error for empty data")
	}
}

func TestEnvelope_JSON(t *testing.T) {
	env := Envelope{
		Version:    "1.0",
		AppVersion: "0.20.0",
		CreatedAt:  "2024-01-01T00:00:00Z",
		Salt:       "c29tZS1zYWx0",
		Data:       "encrypted-data",
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	var decoded Envelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}
	if decoded.Version != "1.0" {
		t.Errorf("expected 1.0, got %s", decoded.Version)
	}
	if decoded.Salt != "c29tZS1zYWx0" {
		t.Errorf("expected salt preserved, got %s", decoded.Salt)
	}
}
