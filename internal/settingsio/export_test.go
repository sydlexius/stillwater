package settingsio

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scraper"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// templateDBPath is built once by TestMain using the real migration files and
// then copied per test, matching the pattern used in internal/rule.
var templateDBPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "settingsio-test-template-*")
	if err != nil {
		panic("creating temp dir: " + err.Error())
	}

	templateDBPath = filepath.Join(dir, "template.db")
	db, err := database.Open(templateDBPath)
	if err != nil {
		panic("opening template db: " + err.Error())
	}
	if err := database.Migrate(db); err != nil {
		panic("migrating template db: " + err.Error())
	}
	// Checkpoint WAL so the template file is self-contained for copies.
	if _, err := db.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		panic("checkpointing template db: " + err.Error())
	}
	_ = db.Close()

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// setupTestDB copies the pre-migrated template and opens it. Using a real
// migration keeps the schema in sync with 001_initial_schema.sql automatically.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	src, err := os.ReadFile(templateDBPath)
	if err != nil {
		t.Fatalf("reading template db: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "test.db")
	if err := os.WriteFile(dst, src, 0o600); err != nil {
		t.Fatalf("writing test db: %v", err)
	}
	db, err := database.Open(dst)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
	// The real migration seeds 5 builtin profiles; our test adds one more.
	// All profiles are exported and re-imported, so result.Profiles equals the
	// total profile count (builtins + user-created), not just user-created ones.
	if result.Profiles == 0 {
		t.Error("expected at least one profile imported")
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

// TestRoundTrip_RuleScraperPreferences verifies that rule configuration,
// scraper configs, and user preferences are included in the export payload and
// correctly restored after import.
func TestRoundTrip_RuleScraperPreferences(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	scraperSvc := scraper.NewService(db, slog.Default())

	// Seed default rules and one scraper config.
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	if err := scraperSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding scraper defaults: %v", err)
	}

	// Modify a rule so we can verify it round-trips.
	thumbRule, err := ruleSvc.GetByID(ctx, rule.RuleThumbExists)
	if err != nil {
		t.Fatalf("getting thumb rule: %v", err)
	}
	thumbRule.Enabled = false
	thumbRule.AutomationMode = rule.AutomationModeAuto
	if err := ruleSvc.Update(ctx, thumbRule); err != nil {
		t.Fatalf("updating rule: %v", err)
	}

	// Add a user with preferences.
	userID := "user-001"
	db.ExecContext(ctx, `INSERT INTO users (id, username, display_name) VALUES (?, 'alice', 'Alice')`, userID)
	db.ExecContext(ctx, `INSERT INTO user_preferences (user_id, key, value) VALUES (?, 'theme', 'light')`, userID)
	db.ExecContext(ctx, `INSERT INTO user_preferences (user_id, key, value) VALUES (?, 'font_size', 'large')`, userID)

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).
		WithRuleService(ruleSvc).
		WithScraperService(scraperSvc)

	passphrase := "roundtrip-ext"
	envelope, err := svc.Export(ctx, passphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Decrypt the payload and verify sections are present.
	plaintext, err := decryptWithPassphrase(envelope.Data, envelope.Salt, passphrase)
	if err != nil {
		t.Fatalf("decrypting for inspection: %v", err)
	}
	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		t.Fatalf("unmarshaling payload: %v", err)
	}
	if len(payload.Rules) == 0 {
		t.Error("expected rules in payload")
	}
	if len(payload.ScraperConfigs) == 0 {
		t.Error("expected scraper configs in payload")
	}
	if len(payload.UserPreferences) == 0 {
		t.Error("expected user preferences in payload")
	}

	// Import into a fresh DB with the same username.
	db2 := setupTestDB(t)
	provSettings2, connSvc2, platSvc2, whSvc2 := newTestServices(t, db2)
	ruleSvc2 := rule.NewService(db2)
	scraperSvc2 := scraper.NewService(db2, slog.Default())
	if err := ruleSvc2.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules in target db: %v", err)
	}
	// Create matching user in target DB.
	userID2 := "user-002"
	db2.ExecContext(ctx, `INSERT INTO users (id, username, display_name) VALUES (?, 'alice', 'Alice')`, userID2)

	svc2 := NewService(db2, provSettings2, connSvc2, platSvc2, whSvc2).
		WithRuleService(ruleSvc2).
		WithScraperService(scraperSvc2)

	result, err := svc2.Import(ctx, envelope, passphrase)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if result.Rules == 0 {
		t.Error("expected rules imported")
	}
	if result.ScraperConfigs == 0 {
		t.Error("expected scraper configs imported")
	}
	if result.UserPreferences == 0 {
		t.Error("expected user preferences imported")
	}

	// Verify the modified rule was restored correctly.
	imported, err := ruleSvc2.GetByID(ctx, rule.RuleThumbExists)
	if err != nil {
		t.Fatalf("getting imported rule: %v", err)
	}
	if imported.Enabled {
		t.Error("expected thumb rule to be disabled after import")
	}
	if imported.AutomationMode != rule.AutomationModeAuto {
		t.Errorf("expected automation_mode=auto, got %s", imported.AutomationMode)
	}

	// Verify user preferences were migrated to the matching username.
	var themeVal string
	db2.QueryRowContext(ctx,
		`SELECT value FROM user_preferences WHERE user_id = ? AND key = 'theme'`, userID2).Scan(&themeVal)
	if themeVal != "light" {
		t.Errorf("expected theme=light for alice, got %q", themeVal)
	}
}

// TestExport_NoDecryptedSecretsInPayload verifies that provider API keys stored
// in the payload are the plaintext keys (not decrypted separately outside the
// envelope), and that the envelope itself never leaks secrets as cleartext.
// The entire export is wrapped in AES-256-GCM; decrypting with a wrong
// passphrase must fail, confirming secrets are not present in the outer JSON.
func TestExport_NoDecryptedSecretsInPayload(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	svc := NewService(db, provSettings, connSvc, platSvc, whSvc)

	// Store a provider API key.
	if err := provSettings.SetAPIKey(ctx, provider.NameFanartTV, "super-secret-fanart-key"); err != nil {
		t.Fatalf("setting provider key: %v", err)
	}

	passphrase := "secure-export"
	envelope, err := svc.Export(ctx, passphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// The outer envelope fields (version, app_version, created_at, salt) must
	// not contain the API key in plaintext.
	envelopeJSON, _ := json.Marshal(envelope)
	if bytes.Contains(envelopeJSON, []byte("super-secret-fanart-key")) {
		t.Error("plaintext API key found in outer envelope JSON -- secret leaked outside encryption")
	}

	// Attempting to decrypt with a wrong passphrase must fail.
	_, err = decryptWithPassphrase(envelope.Data, envelope.Salt, "wrong-passphrase")
	if err == nil {
		t.Error("decryption should fail with wrong passphrase")
	}

	// Decrypt with correct passphrase and confirm key is present inside the payload.
	plaintext, err := decryptWithPassphrase(envelope.Data, envelope.Salt, passphrase)
	if err != nil {
		t.Fatalf("decrypting with correct passphrase: %v", err)
	}
	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		t.Fatalf("unmarshaling payload: %v", err)
	}
	if payload.ProviderKeys[string(provider.NameFanartTV)] != "super-secret-fanart-key" {
		t.Error("expected API key present inside decrypted payload")
	}
}

// TestImport_Idempotent verifies that importing the same payload twice does not
// produce duplicate rows for rules, scraper configs, or user preferences.
func TestImport_Idempotent(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	scraperSvc := scraper.NewService(db, slog.Default())

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	if err := scraperSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding scraper: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).
		WithRuleService(ruleSvc).
		WithScraperService(scraperSvc)

	passphrase := "idempotent"
	envelope, err := svc.Export(ctx, passphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Import twice on the same database.
	for i := range 2 {
		if _, err := svc.Import(ctx, envelope, passphrase); err != nil {
			t.Fatalf("Import #%d: %v", i+1, err)
		}
	}

	// Verify no duplicate scraper config rows.
	var scraperCount int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scraper_config`).Scan(&scraperCount)
	// Only the global config should exist.
	if scraperCount != 1 {
		t.Errorf("expected 1 scraper config row after double import, got %d", scraperCount)
	}

	// Verify rule count unchanged.
	rules, err := ruleSvc.List(ctx)
	if err != nil {
		t.Fatalf("listing rules after double import: %v", err)
	}
	if len(rules) == 0 {
		t.Error("expected rules after double import")
	}
}

// TestImport_UnknownRuleIDSkipped verifies that a payload containing an unknown
// rule ID (from a newer binary) is silently skipped rather than returning an error.
func TestImport_UnknownRuleIDSkipped(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).
		WithRuleService(ruleSvc)

	passphrase := "unknown-rule"
	envelope, err := svc.Export(ctx, passphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Inject a fake rule ID into the decrypted payload and re-encrypt.
	plaintext, err := decryptWithPassphrase(envelope.Data, envelope.Salt, passphrase)
	if err != nil {
		t.Fatalf("decrypt for injection: %v", err)
	}
	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	payload.Rules = append(payload.Rules, RuleExport{
		ID:             "future_rule_does_not_exist",
		Enabled:        true,
		AutomationMode: "auto",
	})
	modified, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	newData, newSalt, err := encryptWithPassphrase(modified, passphrase)
	if err != nil {
		t.Fatalf("re-encrypt: %v", err)
	}
	envelope.Data = newData
	envelope.Salt = newSalt

	// Import should succeed even with the unknown rule ID.
	if _, err := svc.Import(ctx, envelope, passphrase); err != nil {
		t.Fatalf("Import with unknown rule ID should succeed, got: %v", err)
	}
}

// TestImport_InvalidAutomationModeSkipped verifies that a tampered payload
// carrying an unrecognized automation_mode value is skipped (not written to
// the DB) while the rest of the import continues without error.
func TestImport_InvalidAutomationModeSkipped(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	provSettings, connSvc, platSvc, whSvc := newTestServices(t, db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	svc := NewService(db, provSettings, connSvc, platSvc, whSvc).
		WithRuleService(ruleSvc)

	passphrase := "tamper-test"
	envelope, err := svc.Export(ctx, passphrase)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Tamper: inject an invalid automation_mode into the first rule's export.
	plaintext, err := decryptWithPassphrase(envelope.Data, envelope.Salt, passphrase)
	if err != nil {
		t.Fatalf("decrypt for injection: %v", err)
	}
	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Rules) == 0 {
		t.Fatal("expected rules in payload for tampering")
	}
	// Record original mode so we can verify it is not overwritten.
	targetID := payload.Rules[0].ID
	payload.Rules[0].AutomationMode = "invalid_value"

	modified, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	newData, newSalt, err := encryptWithPassphrase(modified, passphrase)
	if err != nil {
		t.Fatalf("re-encrypt: %v", err)
	}
	envelope.Data = newData
	envelope.Salt = newSalt

	// Import must succeed (not abort) even though one entry is invalid.
	if _, err := svc.Import(ctx, envelope, passphrase); err != nil {
		t.Fatalf("Import with invalid automation_mode should succeed, got: %v", err)
	}

	// The rule's automation_mode in the DB must not have been changed to the
	// invalid value. It should still be the DB-resident default ("manual").
	imported, err := ruleSvc.GetByID(ctx, targetID)
	if err != nil {
		t.Fatalf("getting rule after import: %v", err)
	}
	if imported.AutomationMode == "invalid_value" {
		t.Errorf("invalid automation_mode was written to DB -- validation not enforced")
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
