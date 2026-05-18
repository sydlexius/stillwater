package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/config"
	"github.com/sydlexius/stillwater/internal/database"
)

// --- Helpers ---

// newTestApp builds a minimal Application with testing-safe defaults.
// The dbOpener is wired to use a temp-dir SQLite file unless overridden.
func newTestApp(t *testing.T, opts ...Option) *Application {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	defaultOpener := func(path string) (*sql.DB, error) {
		return database.Open(path)
	}
	base := []Option{WithDBOpener(defaultOpener)}
	app := newApplication(append(base, opts...)...)
	app.cfg = &config.Config{}
	app.cfg.Database.Path = dbPath
	app.cfg.Logging.Level = "error" // quiet during tests
	return app
}

// initLogging is a helper that runs setupLogging on app, returning a cleanup
// function that closes the log manager.
func initLogging(t *testing.T, app *Application) func() {
	t.Helper()
	if err := app.setupLogging(); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	return func() { app.logManager.Close() }
}

// --- loadConfig tests ---

func TestLoadConfig_DefaultPath(t *testing.T) {
	// Unset SW_CONFIG_PATH so that loadConfig falls back to the default
	// path. The default (/config/config.toml) will not exist in CI, but
	// config.Load is expected to succeed with defaults when the file is
	// absent.
	t.Setenv("SW_CONFIG_PATH", "")
	app := newApplication()
	// config.Load returns defaults when file does not exist, so this must
	// succeed even in a clean environment.
	if err := app.loadConfig(); err != nil {
		t.Fatalf("loadConfig with absent file: %v", err)
	}
	if app.cfg == nil {
		t.Fatal("cfg must not be nil after loadConfig")
	}
}

func TestLoadConfig_ExplicitPath(t *testing.T) {
	// Create a minimal TOML config file.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\nport = 9999\n"), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	t.Setenv("SW_CONFIG_PATH", cfgPath)
	app := newApplication()
	if err := app.loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if app.cfg == nil {
		t.Fatal("cfg must not be nil")
	}
}

// --- setupLogging tests ---

func TestSetupLogging_HappyPath(t *testing.T) {
	app := newTestApp(t)
	if err := app.setupLogging(); err != nil {
		t.Fatalf("setupLogging: %v", err)
	}
	defer app.logManager.Close()
	if app.logger == nil {
		t.Fatal("logger must not be nil after setupLogging")
	}
	if app.logManager == nil {
		t.Fatal("logManager must not be nil after setupLogging")
	}
}

func TestSetupLogging_ScaffoldWarnLogged(t *testing.T) {
	// Verify that a non-nil scaffoldErr is tolerated (only logs a warning,
	// does not return an error).
	app := newTestApp(t)
	app.scaffoldErr = errors.New("simulated scaffold error")
	if err := app.setupLogging(); err != nil {
		t.Fatalf("setupLogging must succeed even with scaffoldErr: %v", err)
	}
	defer app.logManager.Close()
}

// --- openStorage tests ---

func TestOpenStorage_HappyPath(t *testing.T) {
	app := newTestApp(t)
	defer initLogging(t, app)()

	if err := app.openStorage(); err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer app.db.Close()
	if app.db == nil {
		t.Fatal("db must not be nil after openStorage")
	}
	if app.imageCacheDir == "" {
		t.Fatal("imageCacheDir must not be empty after openStorage")
	}
}

func TestOpenStorage_DBOpenFailure(t *testing.T) {
	app := newTestApp(t, WithDBOpener(func(path string) (*sql.DB, error) {
		return nil, errors.New("simulated open failure")
	}))
	defer initLogging(t, app)()

	err := app.openStorage()
	if err == nil {
		t.Fatal("expected error from openStorage when db open fails")
	}
	if app.db != nil {
		t.Fatal("db must be nil when openStorage fails")
	}
}

func TestOpenStorage_ImageCacheDirDerived(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "stillwater.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	app := newTestApp(t)
	app.cfg.Database.Path = dbPath
	defer initLogging(t, app)()

	if err := app.openStorage(); err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer app.db.Close()

	wantDir := filepath.Join(dir, "data", "cache", "images")
	if app.imageCacheDir != wantDir {
		t.Errorf("imageCacheDir = %q; want %q", app.imageCacheDir, wantDir)
	}
}

// --- wireSecurity tests ---

func TestWireSecurity_HappyPath(t *testing.T) {
	app := newTestApp(t, WithEncKeyResolver(func(_ *config.Config, _ *slog.Logger) (string, error) {
		// base64-encoded 32-byte key (valid for encryption.NewEncryptor)
		return "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE=", nil
	}))
	defer initLogging(t, app)()
	if err := app.openStorage(); err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer app.db.Close()

	if err := app.wireSecurity(); err != nil {
		t.Fatalf("wireSecurity: %v", err)
	}
	if app.encryptor == nil {
		t.Fatal("encryptor must not be nil after wireSecurity")
	}
}

func TestWireSecurity_KeyResolverError(t *testing.T) {
	app := newTestApp(t, WithEncKeyResolver(func(_ *config.Config, _ *slog.Logger) (string, error) {
		return "", errors.New("simulated key resolver error")
	}))
	defer initLogging(t, app)()
	if err := app.openStorage(); err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer app.db.Close()

	err := app.wireSecurity()
	if err == nil {
		t.Fatal("expected error when key resolver fails")
	}
	if app.encryptor != nil {
		t.Fatal("encryptor must be nil when wireSecurity fails")
	}
}

// --- resolveEncryptionKey tests ---

func TestResolveEncryptionKey_ConfigKeyWins(t *testing.T) {
	cfg := &config.Config{}
	cfg.Encryption.Key = "configkey"
	logger := slog.Default()
	key, err := resolveEncryptionKey(cfg, logger)
	if err != nil {
		t.Fatalf("resolveEncryptionKey: %v", err)
	}
	if key != "configkey" {
		t.Errorf("key = %q; want %q", key, "configkey")
	}
}

func TestResolveEncryptionKey_GeneratesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "data", "stillwater.db")
	logger := slog.Default()
	key, err := resolveEncryptionKey(cfg, logger)
	if err != nil {
		t.Fatalf("resolveEncryptionKey: %v", err)
	}
	if key == "" {
		t.Fatal("generated key must not be empty")
	}
	// The key should have been written to the data directory.
	keyFile := filepath.Join(dir, "data", "encryption.key")
	if _, statErr := os.Stat(keyFile); statErr != nil {
		t.Errorf("expected key file at %s: %v", keyFile, statErr)
	}
}

func TestResolveEncryptionKey_LoadsFromFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "encryption.key")
	const want = "fileloadedkey"
	if err := os.WriteFile(keyFile, []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("writing key file: %v", err)
	}

	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	logger := slog.Default()
	key, err := resolveEncryptionKey(cfg, logger)
	if err != nil {
		t.Fatalf("resolveEncryptionKey: %v", err)
	}
	if key != want {
		t.Errorf("key = %q; want %q", key, want)
	}
}

// --- applyPersistedBasePath tests ---

func TestApplyPersistedBasePath_EnvWins(t *testing.T) {
	db := openTestDB(t)
	cfg := &config.Config{}
	cfg.Server.BasePath = "/original"
	cfg.Server.BasePathFromEnv = true
	// Even if there is a DB override, env-derived value must not be replaced.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO settings (key, value, updated_at) VALUES ('server.base_path', '/override', '2024-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("inserting test row: %v", err)
	}
	logger := slog.Default()
	applyPersistedBasePath(context.Background(), db, cfg, logger)
	if cfg.Server.BasePath != "/original" {
		t.Errorf("BasePath = %q; want %q", cfg.Server.BasePath, "/original")
	}
}

func TestApplyPersistedBasePath_AppliesValidOverride(t *testing.T) {
	db := openTestDB(t)
	cfg := &config.Config{}
	cfg.Server.BasePath = ""
	cfg.Server.BasePathFromEnv = false
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO settings (key, value, updated_at) VALUES ('server.base_path', '/sw', '2024-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("inserting test row: %v", err)
	}
	logger := slog.Default()
	applyPersistedBasePath(context.Background(), db, cfg, logger)
	if cfg.Server.BasePath != "/sw" {
		t.Errorf("BasePath = %q; want %q", cfg.Server.BasePath, "/sw")
	}
}

func TestApplyPersistedBasePath_RejectsInvalidOverride(t *testing.T) {
	db := openTestDB(t)
	cfg := &config.Config{}
	cfg.Server.BasePath = "/original"
	cfg.Server.BasePathFromEnv = false
	// Double-slash is invalid.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO settings (key, value, updated_at) VALUES ('server.base_path', '//evil', '2024-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("inserting test row: %v", err)
	}
	logger := slog.Default()
	applyPersistedBasePath(context.Background(), db, cfg, logger)
	if cfg.Server.BasePath != "/original" {
		t.Errorf("BasePath should remain %q after invalid override, got %q", "/original", cfg.Server.BasePath)
	}
}

// --- isValidPersistedBasePath tests (already had indirect coverage; add explicit) ---

func TestIsValidPersistedBasePath(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"/app", true},
		{"/my-app/v2", true},
		{"app", false},          // missing leading /
		{"//evil", false},       // double slash
		{"/evil\\ path", false}, // backslash
		{"/has space", false},
	}
	for _, tc := range cases {
		if got := isValidPersistedBasePath(tc.input); got != tc.want {
			t.Errorf("isValidPersistedBasePath(%q) = %v; want %v", tc.input, got, tc.want)
		}
	}
}

// --- getDBStringSetting / getDBBoolSetting / getDBIntSetting tests ---

func TestGetDBStringSetting_FallbackOnMiss(t *testing.T) {
	db := openTestDB(t)
	got := getDBStringSetting(context.Background(), db, "nonexistent.key", "default")
	if got != "default" {
		t.Errorf("got %q; want %q", got, "default")
	}
}

func TestGetDBStringSetting_ReturnsStoredValue(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO settings (key, value, updated_at) VALUES ('test.key', 'hello', '2024-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("inserting test row: %v", err)
	}
	got := getDBStringSetting(context.Background(), db, "test.key", "fallback")
	if got != "hello" {
		t.Errorf("got %q; want %q", got, "hello")
	}
}

func TestGetDBBoolSetting_TrueValues(t *testing.T) {
	db := openTestDB(t)
	for _, v := range []string{"true", "1"} {
		if _, err := db.ExecContext(context.Background(),
			`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES ('b.key', ?, '2024-01-01T00:00:00Z')`, v); err != nil {
			t.Fatalf("inserting bool test row for %q: %v", v, err)
		}
		if !getDBBoolSetting(context.Background(), db, "b.key", false) {
			t.Errorf("expected true for stored value %q", v)
		}
	}
}

// TestGetDBBoolSetting_FalseValues verifies that values not in the true-set
// ("true", "1") return false, preventing silent parser regressions.
func TestGetDBBoolSetting_FalseValues(t *testing.T) {
	db := openTestDB(t)
	for _, v := range []string{"false", "0", "no", "off", ""} {
		if _, err := db.ExecContext(context.Background(),
			`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES ('b2.key', ?, '2024-01-01T00:00:00Z')`, v); err != nil {
			t.Fatalf("inserting bool test row for %q: %v", v, err)
		}
		if getDBBoolSetting(context.Background(), db, "b2.key", true) {
			t.Errorf("expected false for stored value %q", v)
		}
	}
}

func TestGetDBBoolSetting_FallbackOnMiss(t *testing.T) {
	db := openTestDB(t)
	if getDBBoolSetting(context.Background(), db, "missing.bool", true) != true {
		t.Error("expected fallback true")
	}
}

func TestGetDBIntSetting_ReturnsStoredValue(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO settings (key, value, updated_at) VALUES ('int.key', '42', '2024-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("inserting test row: %v", err)
	}
	if got := getDBIntSetting(context.Background(), db, "int.key", 0); got != 42 {
		t.Errorf("got %d; want 42", got)
	}
}

func TestGetDBIntSetting_FallbackOnMiss(t *testing.T) {
	db := openTestDB(t)
	if got := getDBIntSetting(context.Background(), db, "missing.int", 7); got != 7 {
		t.Errorf("got %d; want 7", got)
	}
}

func TestGetDBIntSetting_FallbackOnNonNumeric(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO settings (key, value, updated_at) VALUES ('bad.int', 'notanumber', '2024-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("inserting test row: %v", err)
	}
	if got := getDBIntSetting(context.Background(), db, "bad.int", 99); got != 99 {
		t.Errorf("got %d; want fallback 99", got)
	}
}

// --- resolveEncryptionKey edge-case tests ---

// TestResolveEncryptionKey_EmptyFile verifies that an empty key file does not
// produce an empty key; the function must generate a new one instead.
func TestResolveEncryptionKey_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "encryption.key")
	if err := os.WriteFile(keyFile, []byte(""), 0o600); err != nil {
		t.Fatalf("writing empty key file: %v", err)
	}
	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	key, err := resolveEncryptionKey(cfg, slog.Default())
	if err != nil {
		t.Fatalf("resolveEncryptionKey: %v", err)
	}
	if key == "" {
		t.Fatal("expected a non-empty generated key when key file is empty")
	}
}

// TestResolveEncryptionKey_WhitespaceOnlyFile verifies that a whitespace-only
// key file is treated as absent and a new key is generated.
func TestResolveEncryptionKey_WhitespaceOnlyFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "encryption.key")
	if err := os.WriteFile(keyFile, []byte("   \n\t  \n"), 0o600); err != nil {
		t.Fatalf("writing whitespace key file: %v", err)
	}
	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	key, err := resolveEncryptionKey(cfg, slog.Default())
	if err != nil {
		t.Fatalf("resolveEncryptionKey: %v", err)
	}
	if key == "" {
		t.Fatal("expected a non-empty generated key when key file is whitespace-only")
	}
}

// TestResolveEncryptionKey_TrailingNewlineStripped verifies that a key stored
// with a trailing newline is loaded exactly, without the newline, so roundtrip
// behavior is stable across restarts.
func TestResolveEncryptionKey_TrailingNewlineStripped(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "encryption.key")
	const want = "mybase64encodedkey=="
	if err := os.WriteFile(keyFile, []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("writing key file: %v", err)
	}
	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	key, err := resolveEncryptionKey(cfg, slog.Default())
	if err != nil {
		t.Fatalf("resolveEncryptionKey: %v", err)
	}
	if key != want {
		t.Errorf("key = %q; want %q (trailing newline must be stripped)", key, want)
	}
}

// --- loadConfig error tests ---

// TestLoadConfig_MalformedFile verifies that a syntactically invalid config
// file produces a wrapped error rather than silently returning defaults.
func TestLoadConfig_MalformedFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	// Write TOML that is syntactically invalid (unclosed bracket).
	if err := os.WriteFile(cfgPath, []byte("[server\nport = 9999\n"), 0o600); err != nil {
		t.Fatalf("writing malformed config: %v", err)
	}
	t.Setenv("SW_CONFIG_PATH", cfgPath)
	app := newApplication()
	err := app.loadConfig()
	if err == nil {
		t.Fatal("expected an error for a malformed config file, got nil")
	}
}
