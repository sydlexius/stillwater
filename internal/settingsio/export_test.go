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
			feature_image_write INTEGER NOT NULL DEFAULT 1,
			feature_metadata_push INTEGER NOT NULL DEFAULT 0,
			feature_trigger_refresh INTEGER NOT NULL DEFAULT 0,
			platform_user_id TEXT
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
		`CREATE TABLE IF NOT EXISTS rules (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT,
			category TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			config TEXT NOT NULL DEFAULT '{}',
			automation_mode TEXT NOT NULL DEFAULT 'auto',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS scraper_config (
			id TEXT PRIMARY KEY,
			scope TEXT NOT NULL UNIQUE,
			config_json TEXT NOT NULL DEFAULT '{}',
			overrides_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL DEFAULT '',
			password_hash TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT 'operator',
			auth_provider TEXT NOT NULL DEFAULT 'local',
			provider_id TEXT NOT NULL DEFAULT '',
			is_active INTEGER NOT NULL DEFAULT 1,
			is_protected INTEGER NOT NULL DEFAULT 0,
			invited_by TEXT REFERENCES users(id) ON DELETE SET NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS user_preferences (
			user_id TEXT NOT NULL,
			key TEXT NOT NULL,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (user_id, key),
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS invites (
			id TEXT PRIMARY KEY,
			code TEXT NOT NULL UNIQUE,
			role TEXT NOT NULL DEFAULT 'operator',
			created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TEXT NOT NULL,
			redeemed_by TEXT REFERENCES users(id) ON DELETE SET NULL,
			redeemed_at TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
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
	if _, err := db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ('test.key', 'test.value', ?)`, now); err != nil {
		t.Fatalf("seeding setting: %v", err)
	}

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

	// Seed a rule
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rules (id, name, description, category, enabled, automation_mode, config, created_at, updated_at)
		VALUES ('thumb_exists', 'Thumb exists', 'Test rule', 'image', 0, 'manual', '{"severity":"warning"}', ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding rule: %v", err)
	}

	// Seed a scraper config
	if _, err := db.ExecContext(ctx, `
		INSERT INTO scraper_config (id, scope, config_json, overrides_json, created_at, updated_at)
		VALUES ('sc-1', 'global', '{"fields":[]}', '{}', ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding scraper config: %v", err)
	}

	// Export with passphrase
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	passphrase := "test-export-passphrase"
	envelope, err := svc.Export(ctx, passphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	if envelope.Version != currentVersion {
		t.Errorf("expected version %s, got %s", currentVersion, envelope.Version)
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

	// Seed rules in db2 so the update finds them
	if _, err := db2.ExecContext(ctx, `
		INSERT INTO rules (id, name, description, category, enabled, automation_mode, config, created_at, updated_at)
		VALUES ('thumb_exists', 'Thumb exists', 'Test rule', 'image', 1, 'auto', '{}', ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding rule in db2: %v", err)
	}

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
	if result.Rules != 1 {
		t.Errorf("expected 1 rule updated, got %d", result.Rules)
	}
	if result.ScraperConfigs != 1 {
		t.Errorf("expected 1 scraper config upserted, got %d", result.ScraperConfigs)
	}

	// Verify imported data
	var testVal string
	if err := db2.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'test.key'`).Scan(&testVal); err != nil {
		t.Fatalf("querying imported setting: %v", err)
	}
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

	// Verify rule was updated (enabled was 1 in db2, should be 0 after import)
	var ruleEnabled int
	if err := db2.QueryRowContext(ctx, `SELECT enabled FROM rules WHERE id = 'thumb_exists'`).Scan(&ruleEnabled); err != nil {
		t.Fatalf("querying imported rule: %v", err)
	}
	if ruleEnabled != 0 {
		t.Errorf("expected rule enabled=0 after import, got %d", ruleEnabled)
	}

	// Verify scraper config was upserted
	var scraperScope string
	if err := db2.QueryRowContext(ctx, `SELECT scope FROM scraper_config WHERE scope = 'global'`).Scan(&scraperScope); err != nil {
		t.Fatalf("querying imported scraper config: %v", err)
	}
	if scraperScope != "global" {
		t.Errorf("expected scraper config for 'global' scope, got %q", scraperScope)
	}
}

func TestRoundTrip_WithUsersAndInvites(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	now := time.Now().UTC().Format(time.RFC3339)
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)

	// Seed a user (with bootstrap admin)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, auth_provider,
		                   provider_id, is_active, is_protected, created_at, updated_at)
		VALUES ('u1', 'admin', 'Admin User', 'hash', 'administrator', 'local', '', 1, 1, ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding user: %v", err)
	}

	// Seed user preferences
	if _, err := db.ExecContext(ctx, `
		INSERT INTO user_preferences (user_id, key, value, updated_at) VALUES ('u1', 'theme', 'dark', ?)
	`, now); err != nil {
		t.Fatalf("seeding user preference: %v", err)
	}

	// Seed an unredeemed invite (created_by references existing user)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO invites (id, code, role, created_by, expires_at, created_at)
		VALUES ('inv1', 'sw_inv_abc123', 'operator', 'u1', ?, ?)
	`, future, now); err != nil {
		t.Fatalf("seeding invite: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	passphrase := "test-users-passphrase"
	// Use ExportWithOptions to include users and invites in the export
	envelope, err := svc.ExportWithOptions(ctx, passphrase, ExportOptions{
		ExportUsers:   true,
		ExportInvites: true,
	})
	if err != nil {
		t.Fatalf("ExportWithOptions: %v", err)
	}

	// Import into a fresh DB with users and invites enabled
	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2)

	result, err := svc2.ImportWithOptions(ctx, envelope, passphrase, ImportOptions{
		ImportUsers:   true,
		ImportInvites: true,
	})
	if err != nil {
		t.Fatalf("ImportWithOptions: %v", err)
	}

	if result.Users != 1 {
		t.Errorf("expected 1 user imported, got %d", result.Users)
	}
	if result.Invites != 1 {
		t.Errorf("expected 1 invite imported, got %d", result.Invites)
	}
	// Preferences should have been imported too (user exists now)
	if result.UserPreferences != 1 {
		t.Errorf("expected 1 user preference imported, got %d", result.UserPreferences)
	}

	// Verify user was imported with empty password_hash
	var pwHash string
	if err := db2.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = 'u1'`).Scan(&pwHash); err != nil {
		t.Fatalf("querying imported user: %v", err)
	}
	if pwHash != "" {
		t.Errorf("expected empty password_hash for imported user, got %q", pwHash)
	}

	// Verify warning was issued about password hashes
	const expectedWarning = "imported users have empty password hashes; accounts must use federated auth or password reset"
	foundWarning := false
	for _, w := range result.Warnings {
		if w == expectedWarning {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected warning %q, got %v", expectedWarning, result.Warnings)
	}
}

func TestImport_Rules_SkipsUnknownIDs(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	now := time.Now().UTC().Format(time.RFC3339)
	// Seed one known rule
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rules (id, name, description, category, enabled, automation_mode, config, created_at, updated_at)
		VALUES ('known_rule', 'Known Rule', 'desc', 'nfo', 1, 'auto', '{}', ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding rule: %v", err)
	}

	// Build a payload with one known and one unknown rule ID
	payload := Payload{
		Settings:     map[string]string{},
		ProviderKeys: map[string]string{},
		Rules: []RuleExport{
			{ID: "known_rule", Enabled: false, AutomationMode: "manual", Config: json.RawMessage(`{"severity":"warning"}`)},
			{ID: "unknown_rule", Enabled: true, AutomationMode: "auto", Config: json.RawMessage(`{}`)},
		},
	}

	passphrase := "test-rules"
	data, salt, err := encryptWithPassphrase(mustMarshal(t, payload), passphrase)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	env := &Envelope{Version: currentVersion, Salt: salt, Data: data}

	result, err := svc.Import(ctx, env, passphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Only 1 rule should be counted (the known one); the unknown one is silently skipped
	if result.Rules != 1 {
		t.Errorf("expected 1 rule updated, got %d", result.Rules)
	}

	// Verify the known rule was updated
	var enabled int
	if err := db.QueryRowContext(ctx, `SELECT enabled FROM rules WHERE id = 'known_rule'`).Scan(&enabled); err != nil {
		t.Fatalf("query known_rule enabled: %v", err)
	}
	if enabled != 0 {
		t.Errorf("expected known_rule enabled=0, got %d", enabled)
	}
}

func TestImport_ScraperConfigs_Upsert(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	now := time.Now().UTC().Format(time.RFC3339)
	// Seed an existing scraper config
	if _, err := db.ExecContext(ctx, `
		INSERT INTO scraper_config (id, scope, config_json, overrides_json, created_at, updated_at)
		VALUES ('sc-existing', 'global', '{"old":true}', '{}', ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding scraper config: %v", err)
	}

	newConfig := json.RawMessage(`{"fields":[{"field":"biography","enabled":true}]}`)
	payload := Payload{
		Settings:     map[string]string{},
		ProviderKeys: map[string]string{},
		ScraperConfigs: []ScraperConfigExport{
			{Scope: "global", ConfigJSON: newConfig, OverridesJSON: json.RawMessage(`{}`)},
			{Scope: "conn-123", ConfigJSON: json.RawMessage(`{"fields":[]}`), OverridesJSON: json.RawMessage(`{"fields":{"biography":true}}`)},
		},
	}

	passphrase := "test-scraper"
	data, salt, err := encryptWithPassphrase(mustMarshal(t, payload), passphrase)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	env := &Envelope{Version: currentVersion, Salt: salt, Data: data}

	result, err := svc.Import(ctx, env, passphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if result.ScraperConfigs != 2 {
		t.Errorf("expected 2 scraper configs, got %d", result.ScraperConfigs)
	}

	// Verify global config was updated — compare semantically since normalizeJSONObject
	// re-marshals (which may reorder keys).
	var configJSON string
	if err := db.QueryRowContext(ctx, `SELECT config_json FROM scraper_config WHERE scope = 'global'`).Scan(&configJSON); err != nil {
		t.Fatalf("querying scraper config: %v", err)
	}
	var gotObj, wantObj any
	if err := json.Unmarshal([]byte(configJSON), &gotObj); err != nil {
		t.Fatalf("parsing stored config: %v", err)
	}
	if err := json.Unmarshal(newConfig, &wantObj); err != nil {
		t.Fatalf("parsing expected config: %v", err)
	}
	gotBytes, _ := json.Marshal(gotObj)
	wantBytes, _ := json.Marshal(wantObj)
	if string(gotBytes) != string(wantBytes) {
		t.Errorf("expected updated config %s, got %s", wantBytes, gotBytes)
	}

	// Verify new scope was inserted
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scraper_config WHERE scope = 'conn-123'`).Scan(&count); err != nil {
		t.Fatalf("querying scraper config count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 scraper config for 'conn-123', got %d", count)
	}
}

func TestImport_UserPreferences_SkipsUnknownUsers(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	now := time.Now().UTC().Format(time.RFC3339)
	// Create one user
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, auth_provider,
		                   provider_id, is_active, is_protected, created_at, updated_at)
		VALUES ('u1', 'user1', 'User One', '', 'operator', 'local', '', 1, 0, ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding user: %v", err)
	}

	payload := Payload{
		Settings:     map[string]string{},
		ProviderKeys: map[string]string{},
		UserPreferences: []UserPrefExport{
			{UserID: "u1", Key: "theme", Value: "dark"},
			{UserID: "nonexistent", Key: "theme", Value: "light"},
		},
	}

	passphrase := "test-prefs"
	data, salt, err := encryptWithPassphrase(mustMarshal(t, payload), passphrase)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	env := &Envelope{Version: currentVersion, Salt: salt, Data: data}

	result, err := svc.Import(ctx, env, passphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if result.UserPreferences != 1 {
		t.Errorf("expected 1 preference imported, got %d", result.UserPreferences)
	}
	if len(result.Warnings) != 1 {
		t.Errorf("expected 1 warning for unknown user, got %d", len(result.Warnings))
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

func TestImport_OldVersion_Backward_Compat(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	// Build a v1.0 payload (no rules/scraper/users/invites fields)
	payload := Payload{
		Settings:     map[string]string{"legacy.key": "legacy.value"},
		ProviderKeys: map[string]string{},
	}

	passphrase := "compat-test"
	data, salt, err := encryptWithPassphrase(mustMarshal(t, payload), passphrase)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	env := &Envelope{Version: "1.0", Salt: salt, Data: data}

	result, err := svc.Import(ctx, env, passphrase)
	if err != nil {
		t.Fatalf("Import of v1.0 envelope: %v", err)
	}

	if result.Settings != 1 {
		t.Errorf("expected 1 setting imported, got %d", result.Settings)
	}
	if result.Rules != 0 {
		t.Errorf("expected 0 rules from v1.0 envelope, got %d", result.Rules)
	}
}

func TestEnvelope_JSON(t *testing.T) {
	env := Envelope{
		Version:    currentVersion,
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
	if decoded.Version != currentVersion {
		t.Errorf("expected %s, got %s", currentVersion, decoded.Version)
	}
	if decoded.Salt != "c29tZS1zYWx0" {
		t.Errorf("expected salt preserved, got %s", decoded.Salt)
	}
}

func TestNormalizeJSONObject(t *testing.T) {
	cases := []struct {
		input json.RawMessage
		want  string
	}{
		{nil, "{}"},
		{json.RawMessage(""), "{}"},
		{json.RawMessage("null"), "{}"},
		{json.RawMessage(`"string"`), "{}"},
		{json.RawMessage(`[1,2,3]`), "{}"},
		// Invalid JSON is rejected
		{json.RawMessage(`{`), "{}"},
		{json.RawMessage(`{not json}`), "{}"},
		// Valid objects are accepted and re-marshaled (canonical form)
		{json.RawMessage(`{"key":"value"}`), `{"key":"value"}`},
		{json.RawMessage(`{}`), `{}`},
		// Whitespace-prefixed valid object is accepted (whitespace dropped on re-marshal)
		{json.RawMessage("  {\"a\":1}"), `{"a":1}`},
	}
	for _, tc := range cases {
		got := normalizeJSONObject(tc.input)
		if got != tc.want {
			t.Errorf("normalizeJSONObject(%q) = %q, want %q", string(tc.input), got, tc.want)
		}
	}
}

func TestImport_Rules_InvalidAutomationMode(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rules (id, name, description, category, enabled, automation_mode, config, created_at, updated_at)
		VALUES ('rule1', 'Rule 1', '', 'nfo', 1, 'auto', '{}', ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding rule: %v", err)
	}

	// Payload contains an invalid automation_mode
	payload := Payload{
		Settings:     map[string]string{},
		ProviderKeys: map[string]string{},
		Rules: []RuleExport{
			{ID: "rule1", Enabled: true, AutomationMode: "invalid_mode", Config: json.RawMessage(`{}`)},
		},
	}

	passphrase := "test-automode"
	data, salt, err := encryptWithPassphrase(mustMarshal(t, payload), passphrase)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	env := &Envelope{Version: currentVersion, Salt: salt, Data: data}

	result, err := svc.Import(ctx, env, passphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if result.Rules != 1 {
		t.Errorf("expected rule to be updated, got %d", result.Rules)
	}

	// Verify the mode was coerced to "manual"
	var mode string
	if err := db.QueryRowContext(ctx, `SELECT automation_mode FROM rules WHERE id = 'rule1'`).Scan(&mode); err != nil {
		t.Fatalf("querying rule: %v", err)
	}
	if mode != "manual" {
		t.Errorf("expected automation_mode coerced to 'manual', got %q", mode)
	}
}

func TestImport_Invites_DuplicateCode(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	now := time.Now().UTC().Format(time.RFC3339)
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)

	// Seed creator user and an existing invite with the same code
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, auth_provider,
		                   provider_id, is_active, is_protected, created_at, updated_at)
		VALUES ('u1', 'admin', 'Admin', '', 'administrator', 'local', '', 1, 1, ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO invites (id, code, role, created_by, expires_at, created_at)
		VALUES ('existing-id', 'sw_inv_dupe', 'operator', 'u1', ?, ?)
	`, future, now); err != nil {
		t.Fatalf("seeding invite: %v", err)
	}

	// Payload contains a different id but same code
	payload := Payload{
		Settings:     map[string]string{},
		ProviderKeys: map[string]string{},
		Users: []UserExport{
			{ID: "u1", Username: "admin", DisplayName: "Admin", Role: "administrator", AuthProvider: "local", IsActive: true, IsProtected: true, CreatedAt: now},
		},
		Invites: []InviteExport{
			{ID: "new-id", Code: "sw_inv_dupe", Role: "operator", CreatedBy: "u1", ExpiresAt: future, CreatedAt: now},
		},
	}

	passphrase := "test-dupecode"
	data, salt, err := encryptWithPassphrase(mustMarshal(t, payload), passphrase)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	env := &Envelope{Version: currentVersion, Salt: salt, Data: data}

	// Should not error even though the code already exists
	_, err = svc.ImportWithOptions(ctx, env, passphrase, ImportOptions{ImportUsers: true, ImportInvites: true})
	if err != nil {
		t.Fatalf("ImportWithOptions unexpectedly failed on duplicate code: %v", err)
	}

	// Original invite should still exist; the duplicate was silently ignored
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invites WHERE code = 'sw_inv_dupe'`).Scan(&count); err != nil {
		t.Fatalf("querying invite count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 invite with code sw_inv_dupe, got %d", count)
	}
}

// mustMarshal marshals v to JSON and fails the test on error.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestExport_DefaultExcludesUsersAndInvites(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	now := time.Now().UTC().Format(time.RFC3339)
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)

	// Seed a user and an invite
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, auth_provider,
		                   provider_id, is_active, is_protected, created_at, updated_at)
		VALUES ('u1', 'admin', 'Admin', '', 'administrator', 'local', '', 1, 1, ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO invites (id, code, role, created_by, expires_at, created_at)
		VALUES ('inv1', 'sw_inv_test', 'operator', 'u1', ?, ?)
	`, future, now); err != nil {
		t.Fatalf("seeding invite: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)
	passphrase := "test-default-export"

	// Default Export() must not include users or invites
	envelope, err := svc.Export(ctx, passphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	plaintext, err := decryptWithPassphrase(envelope.Data, envelope.Salt, passphrase)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(payload.Users) != 0 {
		t.Errorf("expected no users in default export, got %d", len(payload.Users))
	}
	if len(payload.Invites) != 0 {
		t.Errorf("expected no invites in default export, got %d", len(payload.Invites))
	}

	// ExportWithOptions with flags set must include them
	envelopeWithOpts, err := svc.ExportWithOptions(ctx, passphrase, ExportOptions{ExportUsers: true, ExportInvites: true})
	if err != nil {
		t.Fatalf("ExportWithOptions: %v", err)
	}
	plaintext2, err := decryptWithPassphrase(envelopeWithOpts.Data, envelopeWithOpts.Salt, passphrase)
	if err != nil {
		t.Fatalf("decrypt opts: %v", err)
	}
	var payload2 Payload
	if err := json.Unmarshal(plaintext2, &payload2); err != nil {
		t.Fatalf("unmarshal opts: %v", err)
	}
	if len(payload2.Users) != 1 {
		t.Errorf("expected 1 user in opt-in export, got %d", len(payload2.Users))
	}
	if len(payload2.Invites) != 1 {
		t.Errorf("expected 1 invite in opt-in export, got %d", len(payload2.Invites))
	}
}

func TestImport_Users_UsernameConflictWarning(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	now := time.Now().UTC().Format(time.RFC3339)

	// Seed a user that will conflict on username
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, auth_provider,
		                   provider_id, is_active, is_protected, created_at, updated_at)
		VALUES ('existing-id', 'admin', 'Existing Admin', '', 'administrator', 'local', '', 1, 1, ?, ?)
	`, now, now); err != nil {
		t.Fatalf("seeding user: %v", err)
	}

	// Payload has a user with same username but different id
	payload := Payload{
		Settings:     map[string]string{},
		ProviderKeys: map[string]string{},
		Users: []UserExport{
			{ID: "new-id", Username: "admin", DisplayName: "Admin", Role: "administrator", AuthProvider: "local", IsActive: true, IsProtected: true, CreatedAt: now},
		},
	}

	passphrase := "test-conflict"
	data, salt, err := encryptWithPassphrase(mustMarshal(t, payload), passphrase)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	env := &Envelope{Version: currentVersion, Salt: salt, Data: data}

	result, err := svc.ImportWithOptions(ctx, env, passphrase, ImportOptions{ImportUsers: true})
	if err != nil {
		t.Fatalf("ImportWithOptions: %v", err)
	}

	if result.Users != 0 {
		t.Errorf("expected 0 users imported (conflict), got %d", result.Users)
	}

	// A warning about the skipped user should be present
	foundWarning := false
	for _, w := range result.Warnings {
		if w == `skipped user "admin": username already exists on target instance` {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected username conflict warning, got warnings: %v", result.Warnings)
	}
}
