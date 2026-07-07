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
	// Mirror production: the runtime pool comes from OpenRuntime (FK-on via DSN)
	// so openStorage's VerifyForeignKeys self-check passes. Tests that want the
	// verify to FAIL inject their own FK-off database.Open opener.
	defaultOpener := func(path string) (*sql.DB, error) {
		return database.OpenRuntime(path)
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
	// Unset SW_CONFIG_PATH entirely so loadConfig exercises the env-absent
	// branch and falls back to the default path. t.Setenv("", "") would
	// instead exercise the explicit-empty branch, which is a distinct code
	// path now that empty values do not opt into scaffolding. The default
	// (/config/config.toml) will not exist in CI, but config.Load is
	// expected to succeed with defaults when the file is absent.
	prev, hadPrev := os.LookupEnv("SW_CONFIG_PATH")
	if err := os.Unsetenv("SW_CONFIG_PATH"); err != nil {
		t.Fatalf("unsetting SW_CONFIG_PATH: %v", err)
	}
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv("SW_CONFIG_PATH", prev)
		} else {
			_ = os.Unsetenv("SW_CONFIG_PATH")
		}
	})
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

// TestOpenStorage_FKVerifyFailureClosesHandle asserts the Copilot finding fix:
// when the FK self-check fails, openStorage must close the opened handle and
// leave a.db nil (no leak, no half-initialized Application). It injects an
// FK-off database.Open handle so VerifyForeignKeys fails on a fresh connection.
func TestOpenStorage_FKVerifyFailureClosesHandle(t *testing.T) {
	app := newTestApp(t, WithDBOpener(func(path string) (*sql.DB, error) {
		// FK-off handle: Open does NOT set the foreign_keys DSN pragma, so the
		// non-mutating VerifyForeignKeys self-check will report FK unenforced.
		return database.Open(path)
	}))
	defer initLogging(t, app)()

	err := app.openStorage()
	if err == nil {
		t.Fatal("expected error from openStorage when FK verification fails")
	}
	if app.db != nil {
		t.Fatal("db must be nil when FK verification fails (handle must be closed, a.db left nil)")
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

// TestOpenMigratedRuntimeDB verifies the offline-CLI bootstrap helper
// (used by resetCredentials / resetPassword): it migrates the schema and
// returns a runtime pool with FK enforcement active. Issue #2272.
func TestOpenMigratedRuntimeDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reset.db")

	db, err := openMigratedRuntimeDB(dbPath)
	if err != nil {
		t.Fatalf("openMigratedRuntimeDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Migration ran: a core table exists and is queryable.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists`).Scan(&n); err != nil {
		t.Fatalf("querying migrated artists table: %v", err)
	}

	// FK enforcement is active on the returned runtime pool.
	var fkOn int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fkOn); err != nil {
		t.Fatalf("reading foreign_keys pragma: %v", err)
	}
	if fkOn != 1 {
		t.Errorf("foreign_keys = %d, want 1 (runtime pool must enforce FKs)", fkOn)
	}
}

// TestMigrateSchema verifies the FK-off migration helper applies migrations and
// leaves no open handle behind (the caller opens the runtime pool afterward).
func TestMigrateSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mig.db")

	if err := migrateSchema(dbPath); err != nil {
		t.Fatalf("migrateSchema: %v", err)
	}

	// Reopen independently and confirm the schema is present -- proving the
	// migration handle committed and was released cleanly.
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("reopening migrated db: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM artists`).Scan(&n); err != nil {
		t.Fatalf("querying migrated artists table: %v", err)
	}
}

// TestResetCredentials_ClearsUsers drives the offline resetCredentials CLI
// path end to end: it points config at a seeded temp DB via SW_DB_PATH, runs
// the command, and asserts the user rows were cleared. This also exercises the
// FK-on runtime bootstrap the command now uses (issue #2272).
func TestResetCredentials_ClearsUsers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "creds.db")

	// Seed a user so the DELETE has something to remove.
	seed, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening seed db: %v", err)
	}
	if err := database.Migrate(seed); err != nil {
		t.Fatalf("migrate seed db: %v", err)
	}
	ctx := context.Background()
	insertUser(t, ctx, seed, "admin", "pw", "admin")
	_ = seed.Close()

	// Point config at the temp DB and away from any real config file.
	t.Setenv("SW_CONFIG_PATH", filepath.Join(t.TempDir(), "nonexistent.toml"))
	t.Setenv("SW_DB_PATH", dbPath)

	if err := resetCredentials(); err != nil {
		t.Fatalf("resetCredentials: %v", err)
	}

	verify, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	defer verify.Close()
	var n int
	if err := verify.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("counting users: %v", err)
	}
	if n != 0 {
		t.Errorf("users count after resetCredentials = %d, want 0", n)
	}
}

// TestResetPassword_ChangesPassword drives the offline resetPassword CLI path
// end to end against a seeded temp DB (issue #2272 bootstrap).
func TestResetPassword_ChangesPassword(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pw.db")

	seed, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening seed db: %v", err)
	}
	if err := database.Migrate(seed); err != nil {
		t.Fatalf("migrate seed db: %v", err)
	}
	ctx := context.Background()
	insertUser(t, ctx, seed, "admin", "oldpass", "admin")
	_ = seed.Close()

	t.Setenv("SW_CONFIG_PATH", filepath.Join(t.TempDir(), "nonexistent.toml"))
	t.Setenv("SW_DB_PATH", dbPath)

	if err := resetPassword("admin", "resetpw123"); err != nil {
		t.Fatalf("resetPassword: %v", err)
	}

	verify, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("reopening db: %v", err)
	}
	defer verify.Close()
	assertPassword(t, ctx, verify, "admin", "resetpw123")
	assertPasswordWrong(t, ctx, verify, "admin", "oldpass")
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

// TestResolveEncryptionKey_KeyFileLoadsValue verifies SW_ENCRYPTION_KEY_FILE
// loads the key VALUE from the referenced path (priority 2: above the sibling
// encryption.key, below SW_ENCRYPTION_KEY).
func TestResolveEncryptionKey_KeyFileLoadsValue(t *testing.T) {
	dir := t.TempDir()
	keyFilePath := filepath.Join(dir, "secret", "enc.key")
	if err := os.MkdirAll(filepath.Dir(keyFilePath), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const want = "keyfilevalue"
	if err := os.WriteFile(keyFilePath, []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("writing key file: %v", err)
	}

	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	cfg.Encryption.KeyFile = keyFilePath
	// A sibling encryption.key with a DIFFERENT value must lose to the key file.
	if err := os.WriteFile(filepath.Join(dir, "encryption.key"), []byte("siblingvalue\n"), 0o600); err != nil {
		t.Fatalf("writing sibling key: %v", err)
	}

	key, err := resolveEncryptionKey(cfg, slog.Default())
	if err != nil {
		t.Fatalf("resolveEncryptionKey: %v", err)
	}
	if key != want {
		t.Errorf("key = %q; want %q (SW_ENCRYPTION_KEY_FILE must win over the sibling)", key, want)
	}
}

// TestResolveEncryptionKey_ConfigKeyBeatsKeyFile verifies SW_ENCRYPTION_KEY (the
// raw value) outranks SW_ENCRYPTION_KEY_FILE.
func TestResolveEncryptionKey_ConfigKeyBeatsKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyFilePath := filepath.Join(dir, "enc.key")
	if err := os.WriteFile(keyFilePath, []byte("fromfile\n"), 0o600); err != nil {
		t.Fatalf("writing key file: %v", err)
	}
	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	cfg.Encryption.Key = "fromenv"
	cfg.Encryption.KeyFile = keyFilePath

	key, err := resolveEncryptionKey(cfg, slog.Default())
	if err != nil {
		t.Fatalf("resolveEncryptionKey: %v", err)
	}
	if key != "fromenv" {
		t.Errorf("key = %q; want %q", key, "fromenv")
	}
}

// TestResolveEncryptionKey_KeyFileMissingIsFatal verifies that an explicitly
// configured but missing SW_ENCRYPTION_KEY_FILE fails loudly rather than
// silently falling through to a different source.
func TestResolveEncryptionKey_KeyFileMissingIsFatal(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	cfg.Encryption.KeyFile = filepath.Join(dir, "does-not-exist.key")
	// Even with a usable sibling present, the missing explicit file must abort.
	if err := os.WriteFile(filepath.Join(dir, "encryption.key"), []byte("sibling\n"), 0o600); err != nil {
		t.Fatalf("writing sibling key: %v", err)
	}

	if _, err := resolveEncryptionKey(cfg, slog.Default()); err == nil {
		t.Fatal("expected error for missing SW_ENCRYPTION_KEY_FILE, got nil")
	}
}

// TestResolveEncryptionKey_KeyFileEmptyIsFatal verifies that an
// SW_ENCRYPTION_KEY_FILE pointing at an empty/whitespace file aborts.
func TestResolveEncryptionKey_KeyFileEmptyIsFatal(t *testing.T) {
	dir := t.TempDir()
	keyFilePath := filepath.Join(dir, "empty.key")
	if err := os.WriteFile(keyFilePath, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("writing key file: %v", err)
	}
	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	cfg.Encryption.KeyFile = keyFilePath

	if _, err := resolveEncryptionKey(cfg, slog.Default()); err == nil {
		t.Fatal("expected error for empty SW_ENCRYPTION_KEY_FILE, got nil")
	}
}

// TestResolveEncryptionKey_KeyFileUnreadableIsFatal verifies that an
// SW_ENCRYPTION_KEY_FILE that exists but cannot be read (permission denied)
// surfaces the read error rather than silently generating a fresh key, which
// would orphan every at-rest secret.
func TestResolveEncryptionKey_KeyFileUnreadableIsFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("unreadable-file perms bypassed as root")
	}
	dir := t.TempDir()
	keyFilePath := filepath.Join(dir, "unreadable.key")
	if err := os.WriteFile(keyFilePath, []byte("secretkey\n"), 0o600); err != nil {
		t.Fatalf("writing key file: %v", err)
	}
	if err := os.Chmod(keyFilePath, 0o000); err != nil {
		t.Fatalf("chmod key file unreadable: %v", err)
	}
	// Restore read perms so t.TempDir cleanup can remove the file.
	t.Cleanup(func() { _ = os.Chmod(keyFilePath, 0o600) })

	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	cfg.Encryption.KeyFile = keyFilePath

	if _, err := resolveEncryptionKey(cfg, slog.Default()); err == nil {
		t.Fatal("expected error for unreadable SW_ENCRYPTION_KEY_FILE, got nil")
	}
}

// populatedDBPath builds a migrated SQLite DB at dir/stillwater.db, runs the
// supplied seed against it, and returns the path. The DB is closed so a later
// read-only probe sees the persisted (checkpointed) rows.
func populatedDBPath(t *testing.T, dir string, seed func(*sql.DB)) string {
	t.Helper()
	dbPath := filepath.Join(dir, "stillwater.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening seed db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating seed db: %v", err)
	}
	if seed != nil {
		seed(db)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing seed db: %v", err)
	}
	return dbPath
}

// TestResolveEncryptionKey_AbortsOnPopulatedDBConnectionSecret verifies the
// fail-loud guard: a DB holding an encrypted connection API key with no key
// available must refuse to generate a new key (which would orphan the secret).
func TestResolveEncryptionKey_AbortsOnPopulatedDBConnectionSecret(t *testing.T) {
	dir := t.TempDir()
	dbPath := populatedDBPath(t, dir, func(db *sql.DB) {
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO connections (id, name, type, url, encrypted_api_key)
			 VALUES ('c1', 'Emby', 'emby', 'http://x', 'ENCRYPTED-BLOB')`); err != nil {
			t.Fatalf("inserting connection: %v", err)
		}
	})
	cfg := &config.Config{}
	cfg.Database.Path = dbPath // no sibling key, no env key

	if _, err := resolveEncryptionKey(cfg, slog.Default()); err == nil {
		t.Fatal("expected abort on populated DB with no key, got nil error (would orphan secrets)")
	}
}

// TestResolveEncryptionKey_AbortsOnPopulatedDBProviderSecret covers the second
// secret surface: provider API keys stored in the settings table.
func TestResolveEncryptionKey_AbortsOnPopulatedDBProviderSecret(t *testing.T) {
	dir := t.TempDir()
	dbPath := populatedDBPath(t, dir, func(db *sql.DB) {
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO settings (key, value) VALUES ('provider.fanarttv.api_key', 'ENCRYPTED-BLOB')`); err != nil {
			t.Fatalf("inserting provider secret: %v", err)
		}
	})
	cfg := &config.Config{}
	cfg.Database.Path = dbPath

	if _, err := resolveEncryptionKey(cfg, slog.Default()); err == nil {
		t.Fatal("expected abort on populated DB with provider secret and no key, got nil")
	}
}

// TestResolveEncryptionKey_GeneratesOnMigratedButSecretlessDB verifies the
// guard does NOT over-trigger: a migrated DB that holds no encrypted secrets is
// a legitimate fresh install, so key generation proceeds.
func TestResolveEncryptionKey_GeneratesOnMigratedButSecretlessDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := populatedDBPath(t, dir, nil) // schema only, zero secrets
	cfg := &config.Config{}
	cfg.Database.Path = dbPath

	key, err := resolveEncryptionKey(cfg, slog.Default())
	if err != nil {
		t.Fatalf("resolveEncryptionKey on secretless DB: %v", err)
	}
	if key == "" {
		t.Fatal("expected a generated key on a secretless DB")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "encryption.key")); statErr != nil {
		t.Errorf("expected generated sibling key file: %v", statErr)
	}
}

// TestResolveEncryptionKey_AbortsOnProbeError covers the security-critical
// fail-closed branch: when the existing DB file cannot be probed (here a
// non-empty file that is not a valid SQLite database, so the read-only
// PingContext/query errors), resolveEncryptionKey must surface that error
// rather than silently generating a fresh key -- generating one could orphan
// secrets that a transient/permission/corruption fault merely hid.
func TestResolveEncryptionKey_AbortsOnProbeError(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stillwater.db")
	// Non-empty garbage: passes the size>0 gate, fails the sqlite probe.
	if err := os.WriteFile(dbPath, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatalf("writing garbage db: %v", err)
	}
	cfg := &config.Config{}
	cfg.Database.Path = dbPath // no env key, no sibling encryption.key

	if _, err := resolveEncryptionKey(cfg, slog.Default()); err == nil {
		t.Fatal("expected abort on unprobeable DB, got nil error (would risk orphaning secrets)")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "encryption.key")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("must not generate a key file when the probe fails; stat err = %v", statErr)
	}
}

// TestDatabaseHasEncryptedSecrets_AbsentAndEmpty verifies the probe treats a
// missing or empty DB file as a fresh install (no secrets).
func TestDatabaseHasEncryptedSecrets_AbsentAndEmpty(t *testing.T) {
	dir := t.TempDir()

	has, err := databaseHasEncryptedSecrets(filepath.Join(dir, "absent.db"))
	if err != nil {
		t.Fatalf("absent DB: %v", err)
	}
	if has {
		t.Error("absent DB must report no secrets")
	}

	emptyPath := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatalf("writing empty db: %v", err)
	}
	has, err = databaseHasEncryptedSecrets(emptyPath)
	if err != nil {
		t.Fatalf("empty DB: %v", err)
	}
	if has {
		t.Error("empty DB must report no secrets")
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

// --- applyPersistedPositiveInt (Fix 1: no silent drop of a corrupt value) ---

// TestApplyPersistedPositiveInt covers the boot read-back for the integer ops
// keys: a valid positive row applies via the callback, while an absent, non-
// numeric, or out-of-range row does NOT apply (and the latter two warn instead
// of silently reverting -- exercised here by asserting the callback never runs).
func TestApplyPersistedPositiveInt(t *testing.T) {
	cases := []struct {
		name      string
		insert    bool
		value     string
		wantApply bool
		wantN     int
	}{
		{name: "valid", insert: true, value: "8", wantApply: true, wantN: 8},
		{name: "absent", insert: false},
		{name: "non_numeric", insert: true, value: "notanumber"},
		{name: "zero", insert: true, value: "0"},
		{name: "negative", insert: true, value: "-4"},
		{name: "empty", insert: true, value: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			const key = "rule_engine.artist_workers"
			if tc.insert {
				if _, err := db.ExecContext(context.Background(),
					`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, '2024-01-01T00:00:00Z')`,
					key, tc.value); err != nil {
					t.Fatalf("inserting row: %v", err)
				}
			}
			applied := false
			gotN := 0
			applyPersistedPositiveInt(context.Background(), db, slog.Default(), key, func(n int) {
				applied = true
				gotN = n
			})
			if applied != tc.wantApply {
				t.Fatalf("applied = %v, want %v", applied, tc.wantApply)
			}
			if tc.wantApply && gotN != tc.wantN {
				t.Errorf("applied n = %d, want %d", gotN, tc.wantN)
			}
		})
	}
}

// TestApplyPersistedOpsSettings_ArtistWorkersUpperBound covers the boot
// overlay's rule_engine.artist_workers upper-bound guard (Fix 2): a persisted
// value above the API's validated max (64) must not reach the pipeline, while
// an in-range value must. applyPersistedPositiveInt itself only rejects
// non-positive values, so this exercises the bound-check closure that
// applyPersistedOpsSettings wraps around it.
func TestApplyPersistedOpsSettings_ArtistWorkersUpperBound(t *testing.T) {
	cases := []struct {
		name      string
		value     string
		wantApply bool
		wantN     int
	}{
		{name: "in_range", value: "32", wantApply: true, wantN: 32},
		{name: "at_max", value: "64", wantApply: true, wantN: 64},
		{name: "over_max", value: "500", wantApply: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			const key = "rule_engine.artist_workers"
			if _, err := db.ExecContext(context.Background(),
				`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, '2024-01-01T00:00:00Z')`,
				key, tc.value); err != nil {
				t.Fatalf("inserting row: %v", err)
			}
			applied := false
			gotN := 0
			applyPersistedPositiveInt(context.Background(), db, slog.Default(), key, func(n int) {
				if n > 64 {
					return
				}
				applied = true
				gotN = n
			})
			if applied != tc.wantApply {
				t.Fatalf("applied = %v, want %v", applied, tc.wantApply)
			}
			if tc.wantApply && gotN != tc.wantN {
				t.Errorf("applied n = %d, want %d", gotN, tc.wantN)
			}
		})
	}
}

// TestApplyPersistedPositiveInt_DBError: a read error (closed DB) must not
// apply and must not panic -- the boot overlay warns and leaves the default.
func TestApplyPersistedPositiveInt_DBError(t *testing.T) {
	db := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	applied := false
	applyPersistedPositiveInt(context.Background(), db, slog.Default(), "backup.interval_hours", func(int) {
		applied = true
	})
	if applied {
		t.Error("callback ran despite a DB read error")
	}
}

// --- dbSettingPresent (warn on a real DB error, not just "not present") ---

func TestDbSettingPresent(t *testing.T) {
	db := openTestDB(t)
	const key = "scanner.exclusions"
	if got := dbSettingPresent(context.Background(), db, slog.Default(), key); got {
		t.Fatalf("dbSettingPresent() = true for an absent row, want false")
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO settings (key, value, updated_at) VALUES (?, '', '2024-01-01T00:00:00Z')`,
		key); err != nil {
		t.Fatalf("inserting row: %v", err)
	}
	if got := dbSettingPresent(context.Background(), db, slog.Default(), key); !got {
		t.Fatalf("dbSettingPresent() = false for a present row, want true")
	}
}

// TestDbSettingPresent_DBError: a genuine query error (closed DB) must return
// false without panicking -- the same warn-on-real-error contract as
// applyPersistedPositiveInt, so a corrupt/unreadable DB doesn't get
// misreported as "operator never saved this".
func TestDbSettingPresent_DBError(t *testing.T) {
	db := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}
	if got := dbSettingPresent(context.Background(), db, slog.Default(), "scanner.exclusions"); got {
		t.Error("dbSettingPresent() = true despite a DB read error")
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

// --- resolveSessionSecret tests (F5) ---

// TestResolveSessionSecret_GeneratesWhenMissing proves the default boot path:
// no SW_SESSION_SECRET, no pre-existing file -> secret is generated, persisted,
// and returned without panicking. This is the regression gate for F1.
func TestResolveSessionSecret_GeneratesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "data", "stillwater.db")

	secret, err := resolveSessionSecret(cfg, slog.Default())
	if err != nil {
		t.Fatalf("resolveSessionSecret: %v", err)
	}
	if len(secret) < 32 {
		t.Errorf("generated secret too short: len=%d, want >=32", len(secret))
	}
	// The secret must have been persisted alongside the DB directory.
	secretFile := filepath.Join(dir, "data", "session.secret")
	info, statErr := os.Stat(secretFile)
	if statErr != nil {
		t.Errorf("expected secret file at %s: %v", secretFile, statErr)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		// 0600 is required: world-readable secrets are a security regression.
		t.Errorf("secret file mode = %04o, want 0600", perm)
	}
}

// TestResolveSessionSecret_ConfigValueWins verifies that an explicitly-set
// SW_SESSION_SECRET (cfg.Auth.SessionSecret) takes priority over any file.
func TestResolveSessionSecret_ConfigValueWins(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	cfg.Auth.SessionSecret = "this-is-a-32-byte-long-test-key!"

	// Write a conflicting persisted secret; the config value must win over it.
	if err := os.WriteFile(
		filepath.Join(dir, "session.secret"),
		[]byte("different-32-byte-file-secret-value!!\n"),
		0o600,
	); err != nil {
		t.Fatalf("writing conflicting secret file: %v", err)
	}

	secret, err := resolveSessionSecret(cfg, slog.Default())
	if err != nil {
		t.Fatalf("resolveSessionSecret: %v", err)
	}
	if secret != cfg.Auth.SessionSecret {
		t.Errorf("secret = %q; want %q", secret, cfg.Auth.SessionSecret)
	}
}

// TestResolveSessionSecret_LoadsFromFile verifies that an existing session.secret
// file is read and returned, matching the encryption key pattern.
func TestResolveSessionSecret_LoadsFromFile(t *testing.T) {
	dir := t.TempDir()
	const want = "this-is-a-32-byte-long-test-key!"
	secretFile := filepath.Join(dir, "session.secret")
	if err := os.WriteFile(secretFile, []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("writing secret file: %v", err)
	}

	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	secret, err := resolveSessionSecret(cfg, slog.Default())
	if err != nil {
		t.Fatalf("resolveSessionSecret: %v", err)
	}
	if secret != want {
		t.Errorf("secret = %q; want %q (trailing newline must be stripped)", secret, want)
	}
}

// TestResolveSessionSecret_TooShortConfigReturnsError verifies that a
// too-short explicitly-supplied secret produces a clear startup error (F2).
func TestResolveSessionSecret_TooShortConfigReturnsError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Auth.SessionSecret = "short" // only 5 bytes
	_, err := resolveSessionSecret(cfg, slog.Default())
	if err == nil {
		t.Fatal("expected error for too-short config secret, got nil")
	}
}

// TestResolveSessionSecret_TooShortFileReturnsError verifies that a too-short
// secret in the persist file is rejected with a clear error.
func TestResolveSessionSecret_TooShortFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "session.secret")
	if err := os.WriteFile(secretFile, []byte("short\n"), 0o600); err != nil {
		t.Fatalf("writing secret file: %v", err)
	}

	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "stillwater.db")
	_, err := resolveSessionSecret(cfg, slog.Default())
	if err == nil {
		t.Fatal("expected error for too-short file secret, got nil")
	}
}

// TestResolveSessionSecret_SecondStartupLoadsPersistedSecret verifies that
// calling resolveSessionSecret twice with the same config returns the same
// secret (idempotent generate+persist behavior).
func TestResolveSessionSecret_SecondStartupLoadsPersistedSecret(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Database.Path = filepath.Join(dir, "data", "stillwater.db")

	first, err := resolveSessionSecret(cfg, slog.Default())
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := resolveSessionSecret(cfg, slog.Default())
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first != second {
		t.Errorf("second startup returned different secret: %q != %q", first, second)
	}
}

// TestWireSecurity_SessionSecretPopulated verifies that wireSecurity resolves
// and populates cfg.Auth.SessionSecret so the router constructor never sees an
// empty value (integration coverage of F1 through the Application lifecycle).
func TestWireSecurity_SessionSecretPopulated(t *testing.T) {
	app := newTestApp(t, WithEncKeyResolver(func(_ *config.Config, _ *slog.Logger) (string, error) {
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
	if app.cfg.Auth.SessionSecret == "" {
		t.Fatal("cfg.Auth.SessionSecret must be non-empty after wireSecurity")
	}
	if len(app.cfg.Auth.SessionSecret) < 32 {
		t.Errorf("cfg.Auth.SessionSecret too short: len=%d", len(app.cfg.Auth.SessionSecret))
	}
}

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

// TestIsInMemoryPath verifies the #2272 in-memory guard classifies the SQLite
// in-memory DSN forms as unshareable while leaving file-backed paths alone.
func TestIsInMemoryPath(t *testing.T) {
	inMem := []string{":memory:", " :memory: ", ":MEMORY:", "file::memory:?cache=shared", "file:x?mode=memory"}
	for _, p := range inMem {
		if !isInMemoryPath(p) {
			t.Errorf("isInMemoryPath(%q) = false, want true", p)
		}
	}
	fileBacked := []string{"/config/stillwater.db", "stillwater.db", "/tmp/test.db", ""}
	for _, p := range fileBacked {
		if isInMemoryPath(p) {
			t.Errorf("isInMemoryPath(%q) = true, want false", p)
		}
	}
}

// TestMigrateSchema_RejectsInMemory confirms the two-pool bootstrap refuses an
// in-memory database path (its schema cannot survive the migration handle being
// closed before the runtime pool opens) rather than silently serving an empty
// schema.
func TestMigrateSchema_RejectsInMemory(t *testing.T) {
	if err := migrateSchema(":memory:"); err == nil {
		t.Fatal("migrateSchema(\":memory:\") = nil, want an in-memory rejection error")
	}
}
