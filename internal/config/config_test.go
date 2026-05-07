package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.Server.Port != 1973 {
		t.Errorf("Server.Port = %d, want 1973", cfg.Server.Port)
	}
	if cfg.Database.Path != "/config/stillwater.db" {
		t.Errorf("Database.Path = %q, want /config/stillwater.db", cfg.Database.Path)
	}
	if cfg.Music.LibraryPath != "/music" {
		t.Errorf("Music.LibraryPath = %q, want /music", cfg.Music.LibraryPath)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("Logging.Level = %q, want info", cfg.Logging.Level)
	}
	if cfg.Backup.RetentionCount != 7 {
		t.Errorf("Backup.RetentionCount = %d, want 7", cfg.Backup.RetentionCount)
	}
	if !cfg.Backup.Enabled {
		t.Error("Backup.Enabled = false, want true")
	}
}

// clearSWEnv unsets all SW_* environment variables to prevent env overrides
// from interfering with tests that assert YAML/default behavior.
func clearSWEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SW_PORT", "SW_BASE_PATH", "SW_DB_PATH", "SW_SESSION_SECRET",
		"SW_ENCRYPTION_KEY", "SW_MUSIC_PATH", "SW_SCANNER_EXCLUSIONS",
		"SW_BACKUP_PATH", "SW_BACKUP_RETENTION", "SW_BACKUP_INTERVAL",
		"SW_BACKUP_ENABLED", "SW_LOG_LEVEL", "SW_LOG_FORMAT",
		"SW_TLS_CERT_FILE", "SW_TLS_KEY_FILE", "SW_TLS_PORT",
		"SW_HTTP_REDIRECT_PORT", "SW_HTTP3_ENABLED",
		"SW_ACME_DOMAIN", "SW_ACME_EMAIL", "SW_ACME_CA",
		"SW_ACME_EAB_KEY_ID", "SW_ACME_EAB_MAC_KEY",
		"SW_ACME_IP", "SW_ACME_CACHE_DIR",
	} {
		t.Setenv(key, "")
	}
}

func TestLoad_FromYAML(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(yamlPath, []byte(`
server:
  port: 8080
  base_path: /app
database:
  path: /tmp/test.db
logging:
  level: debug
`), 0o644)
	if err != nil {
		t.Fatalf("writing config file: %v", err)
	}

	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("Server.Port = %d, want 8080", cfg.Server.Port)
	}
	if cfg.Server.BasePath != "/app" {
		t.Errorf("Server.BasePath = %q, want /app", cfg.Server.BasePath)
	}
	if cfg.Database.Path != "/tmp/test.db" {
		t.Errorf("Database.Path = %q, want /tmp/test.db", cfg.Database.Path)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q, want debug", cfg.Logging.Level)
	}
}

func TestLoad_EnvOverridesYAML(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(yamlPath, []byte(`
server:
  port: 8080
database:
  path: /tmp/from-yaml.db
`), 0o644)
	if err != nil {
		t.Fatalf("writing config file: %v", err)
	}

	t.Setenv("SW_PORT", "9090")
	t.Setenv("SW_DB_PATH", "/tmp/from-env.db")

	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Port != 9090 {
		t.Errorf("Server.Port = %d, want 9090 (env override)", cfg.Server.Port)
	}
	if cfg.Database.Path != "/tmp/from-env.db" {
		t.Errorf("Database.Path = %q, want /tmp/from-env.db (env override)", cfg.Database.Path)
	}
}

func TestLoad_MissingFileUsesDefaults(t *testing.T) {
	clearSWEnv(t)
	cfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load with missing file: %v", err)
	}
	if cfg.Server.Port != 1973 {
		t.Errorf("Server.Port = %d, want 1973 (default)", cfg.Server.Port)
	}
}

func TestLoad_EmptyPathUsesDefaults(t *testing.T) {
	clearSWEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with empty path: %v", err)
	}
	if cfg.Server.Port != 1973 {
		t.Errorf("Server.Port = %d, want 1973", cfg.Server.Port)
	}
}

func TestValidate_InvalidPort(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(yamlPath, []byte(`
server:
  port: 0
database:
  path: /tmp/test.db
`), 0o644)
	if err != nil {
		t.Fatalf("writing config file: %v", err)
	}

	_, err = Load(yamlPath)
	if err == nil {
		t.Fatal("expected error for invalid port 0")
	}
}

func TestValidate_EmptyDBPath(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(yamlPath, []byte(`
server:
  port: 1973
database:
  path: ""
`), 0o644)
	if err != nil {
		t.Fatalf("writing config file: %v", err)
	}

	_, err = Load(yamlPath)
	if err == nil {
		t.Fatal("expected error for empty database path")
	}
}

func TestValidate_BasePathTrailingSlash(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(yamlPath, []byte(`
server:
  port: 1973
  base_path: /app/
database:
  path: /tmp/test.db
`), 0o644)
	if err != nil {
		t.Fatalf("writing config file: %v", err)
	}

	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.BasePath != "/app" {
		t.Errorf("Server.BasePath = %q, want /app (trailing slash stripped)", cfg.Server.BasePath)
	}
}

func TestLoad_FromTOML(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	err := os.WriteFile(tomlPath, []byte(`
[server]
port = 8080
base_path = "/app"

[database]
path = "/tmp/test.db"

[logging]
level = "debug"
`), 0o644)
	if err != nil {
		t.Fatalf("writing config file: %v", err)
	}

	cfg, err := Load(tomlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("Server.Port = %d, want 8080", cfg.Server.Port)
	}
	if cfg.Server.BasePath != "/app" {
		t.Errorf("Server.BasePath = %q, want /app", cfg.Server.BasePath)
	}
	if cfg.Database.Path != "/tmp/test.db" {
		t.Errorf("Database.Path = %q, want /tmp/test.db", cfg.Database.Path)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q, want debug", cfg.Logging.Level)
	}
}

// TestLoad_TOMLAndYAMLEquivalent asserts that the two supported file formats
// parse to the same Config when expressing the same configuration. This is
// the round-trip contract documented in #1272.
func TestLoad_TOMLAndYAMLEquivalent(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()

	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
server:
  port: 9000
  base_path: /sw
database:
  path: /var/lib/x.db
music:
  library_path: /srv/music
scanner:
  exclusions:
    - "Various Artists"
    - "OST"
backup:
  enabled: true
  interval_hours: 12
  retention_count: 5
logging:
  level: warn
  format: text
`), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[server]
port = 9000
base_path = "/sw"

[database]
path = "/var/lib/x.db"

[music]
library_path = "/srv/music"

[scanner]
exclusions = ["Various Artists", "OST"]

[backup]
enabled = true
interval_hours = 12
retention_count = 5

[logging]
level = "warn"
format = "text"
`), 0o644); err != nil {
		t.Fatalf("write toml: %v", err)
	}

	yamlCfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load yaml: %v", err)
	}
	tomlCfg, err := Load(tomlPath)
	if err != nil {
		t.Fatalf("Load toml: %v", err)
	}

	// Trim base path to match validate() behavior consistently.
	if yamlCfg.Server.Port != tomlCfg.Server.Port {
		t.Errorf("Port mismatch: yaml=%d toml=%d", yamlCfg.Server.Port, tomlCfg.Server.Port)
	}
	if yamlCfg.Server.BasePath != tomlCfg.Server.BasePath {
		t.Errorf("BasePath mismatch: yaml=%q toml=%q", yamlCfg.Server.BasePath, tomlCfg.Server.BasePath)
	}
	if yamlCfg.Database.Path != tomlCfg.Database.Path {
		t.Errorf("DB Path mismatch: yaml=%q toml=%q", yamlCfg.Database.Path, tomlCfg.Database.Path)
	}
	if yamlCfg.Music.LibraryPath != tomlCfg.Music.LibraryPath {
		t.Errorf("LibraryPath mismatch")
	}
	if len(yamlCfg.Scanner.Exclusions) != len(tomlCfg.Scanner.Exclusions) {
		t.Fatalf("Exclusions length mismatch: yaml=%d toml=%d",
			len(yamlCfg.Scanner.Exclusions), len(tomlCfg.Scanner.Exclusions))
	}
	for i := range yamlCfg.Scanner.Exclusions {
		if yamlCfg.Scanner.Exclusions[i] != tomlCfg.Scanner.Exclusions[i] {
			t.Errorf("Exclusions[%d] mismatch: yaml=%q toml=%q",
				i, yamlCfg.Scanner.Exclusions[i], tomlCfg.Scanner.Exclusions[i])
		}
	}
	if yamlCfg.Backup != tomlCfg.Backup {
		t.Errorf("Backup mismatch: yaml=%+v toml=%+v", yamlCfg.Backup, tomlCfg.Backup)
	}
	if yamlCfg.Logging != tomlCfg.Logging {
		t.Errorf("Logging mismatch: yaml=%+v toml=%+v", yamlCfg.Logging, tomlCfg.Logging)
	}
}

// TestLoad_FormatSniffByContent ensures the loader picks the right parser
// when the path has neither .toml nor .yaml/.yml extension.
func TestLoad_FormatSniffByContent(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()

	tomlSniffPath := filepath.Join(dir, "config.cfg")
	if err := os.WriteFile(tomlSniffPath, []byte(`
# this is a TOML file with an ambiguous extension
[server]
port = 7777

[database]
path = "/tmp/sniff.db"
`), 0o644); err != nil {
		t.Fatalf("write sniff toml: %v", err)
	}

	cfg, err := Load(tomlSniffPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 7777 {
		t.Errorf("Server.Port = %d, want 7777 (TOML sniff)", cfg.Server.Port)
	}
	if cfg.Database.Path != "/tmp/sniff.db" {
		t.Errorf("Database.Path = %q, want /tmp/sniff.db (TOML sniff)", cfg.Database.Path)
	}

	yamlSniffPath := filepath.Join(dir, "config.conf")
	if err := os.WriteFile(yamlSniffPath, []byte(`
# YAML with ambiguous extension
server:
  port: 6666
database:
  path: /tmp/yamlsniff.db
`), 0o644); err != nil {
		t.Fatalf("write sniff yaml: %v", err)
	}

	cfg2, err := Load(yamlSniffPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Server.Port != 6666 {
		t.Errorf("Server.Port = %d, want 6666 (YAML sniff)", cfg2.Server.Port)
	}
	if cfg2.Database.Path != "/tmp/yamlsniff.db" {
		t.Errorf("Database.Path = %q, want /tmp/yamlsniff.db (YAML sniff)", cfg2.Database.Path)
	}
}

func TestLoad_MalformedTOML(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	// Unterminated string is a hard TOML parse error.
	if err := os.WriteFile(tomlPath, []byte(`[server]`+"\nport = \"oops\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(tomlPath); err == nil {
		t.Fatal("expected parse error on malformed TOML")
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	// Tab indent is invalid in YAML.
	if err := os.WriteFile(yamlPath, []byte("server:\n\tport: 8080\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(yamlPath); err == nil {
		t.Fatal("expected parse error on malformed YAML")
	}
}

// TestLoadFromEnv_AllRemaining covers the env-var branches that the existing
// tests do not already exercise (session secret, encryption key, music path,
// backup overrides, log format).
func TestLoadFromEnv_AllRemaining(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_SESSION_SECRET", "shh")
	t.Setenv("SW_ENCRYPTION_KEY", "kk")
	t.Setenv("SW_MUSIC_PATH", "/music2")
	t.Setenv("SW_BACKUP_PATH", "/bk")
	t.Setenv("SW_BACKUP_RETENTION", "9")
	t.Setenv("SW_BACKUP_INTERVAL", "6")
	t.Setenv("SW_BACKUP_ENABLED", "false")
	t.Setenv("SW_LOG_FORMAT", "text")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.SessionSecret != "shh" {
		t.Errorf("SessionSecret = %q", cfg.Auth.SessionSecret)
	}
	if cfg.Encryption.Key != "kk" {
		t.Errorf("Encryption.Key = %q", cfg.Encryption.Key)
	}
	if cfg.Music.LibraryPath != "/music2" {
		t.Errorf("LibraryPath = %q", cfg.Music.LibraryPath)
	}
	if cfg.Backup.Path != "/bk" {
		t.Errorf("Backup.Path = %q", cfg.Backup.Path)
	}
	if cfg.Backup.RetentionCount != 9 {
		t.Errorf("Backup.RetentionCount = %d", cfg.Backup.RetentionCount)
	}
	if cfg.Backup.IntervalHours != 6 {
		t.Errorf("Backup.IntervalHours = %d", cfg.Backup.IntervalHours)
	}
	if cfg.Backup.Enabled {
		t.Error("Backup.Enabled should be false (env said false)")
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("Logging.Format = %q", cfg.Logging.Format)
	}
}

func TestLoadFromEnv_ScannerExclusions(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_SCANNER_EXCLUSIONS", "Various Artists, Soundtrack, OST")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.Scanner.Exclusions) != 3 {
		t.Fatalf("Scanner.Exclusions length = %d, want 3", len(cfg.Scanner.Exclusions))
	}
	if cfg.Scanner.Exclusions[0] != "Various Artists" {
		t.Errorf("Exclusions[0] = %q, want Various Artists", cfg.Scanner.Exclusions[0])
	}
	if cfg.Scanner.Exclusions[1] != "Soundtrack" {
		t.Errorf("Exclusions[1] = %q, want Soundtrack", cfg.Scanner.Exclusions[1])
	}
}

func TestEnsureScaffold_CreatesMissingFile(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	created, err := EnsureScaffold(path)
	if err != nil {
		t.Fatalf("EnsureScaffold: %v", err)
	}
	if !created {
		t.Fatal("EnsureScaffold returned created=false on missing file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after scaffold: %v", err)
	}
	if !strings.Contains(string(data), "[server]") || !strings.Contains(string(data), "# port = 1973") {
		t.Errorf("scaffold content missing expected sections; got:\n%s", data)
	}
	// The scaffold must parse as valid TOML so a Load round-trip succeeds.
	if _, err := Load(path); err != nil {
		t.Fatalf("Load after scaffold: %v", err)
	}
}

func TestEnsureScaffold_NoOpWhenFileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	existing := []byte("[server]\nport = 9999\n")
	if err := os.WriteFile(path, existing, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	created, err := EnsureScaffold(path)
	if err != nil {
		t.Fatalf("EnsureScaffold: %v", err)
	}
	if created {
		t.Error("EnsureScaffold returned created=true for existing file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(existing) {
		t.Errorf("existing file was rewritten; got %q, want %q", got, existing)
	}
}

func TestEnsureScaffold_CreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "subdir", "config.toml")

	created, err := EnsureScaffold(path)
	if err != nil {
		t.Fatalf("EnsureScaffold: %v", err)
	}
	if !created {
		t.Fatal("created=false")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("scaffold not created at nested path: %v", err)
	}
}

func TestEnsureScaffold_EmptyPathNoOp(t *testing.T) {
	created, err := EnsureScaffold("")
	if err != nil {
		t.Errorf("EnsureScaffold(\"\"): %v", err)
	}
	if created {
		t.Error("EnsureScaffold(\"\") returned created=true")
	}
}

// TestEnsureScaffold_SkipsYAMLPath verifies that EnsureScaffold treats a
// .yaml/.yml path as a no-op. Writing TOML content under a YAML filename
// would force the loader's extension-based parser selection to fail on
// first boot, so the policy is to leave YAML deployments untouched.
func TestEnsureScaffold_SkipsYAMLPath(t *testing.T) {
	for _, ext := range []string{".yaml", ".yml"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config"+ext)
		created, err := EnsureScaffold(path)
		if err != nil {
			t.Fatalf("EnsureScaffold(%s): %v", ext, err)
		}
		if created {
			t.Errorf("EnsureScaffold(%s) returned created=true; want false", ext)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("EnsureScaffold(%s) wrote file; want no file: stat err=%v", ext, err)
		}
	}
}

// TestLoadFromEnv_TLSAndACMEStubs covers the new env-var branches added in
// #928. Only the SW_TLS_* triple has runtime behavior in this milestone; the
// remaining ACME and HTTP/3 vars are stubs whose only contract is to land in
// the Config struct so #929/#930/#932 can wire behavior without re-touching
// the loader.
func TestLoadFromEnv_TLSAndACMEStubs(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/cert.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/key.pem")
	t.Setenv("SW_TLS_PORT", "443")
	t.Setenv("SW_HTTP_REDIRECT_PORT", "80")
	t.Setenv("SW_HTTP3_ENABLED", "true")
	t.Setenv("SW_ACME_DOMAIN", "stillwater.example.com")
	t.Setenv("SW_ACME_EMAIL", "admin@example.com")
	t.Setenv("SW_ACME_CA", "letsencrypt")
	t.Setenv("SW_ACME_EAB_KEY_ID", "key-id")
	t.Setenv("SW_ACME_EAB_MAC_KEY", "mac-key")
	t.Setenv("SW_ACME_IP", "203.0.113.5")
	t.Setenv("SW_ACME_CACHE_DIR", "/var/lib/acme")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.TLS.CertFile != "/tmp/cert.pem" {
		t.Errorf("TLS.CertFile = %q", cfg.Server.TLS.CertFile)
	}
	if cfg.Server.TLS.KeyFile != "/tmp/key.pem" {
		t.Errorf("TLS.KeyFile = %q", cfg.Server.TLS.KeyFile)
	}
	if cfg.Server.TLS.Port != 443 {
		t.Errorf("TLS.Port = %d", cfg.Server.TLS.Port)
	}
	if cfg.Server.HTTPRedirect.Port != 80 {
		t.Errorf("HTTPRedirect.Port = %d", cfg.Server.HTTPRedirect.Port)
	}
	if !cfg.Server.HTTP3.Enabled {
		t.Error("HTTP3.Enabled = false; want true")
	}
	if cfg.ACME.Domain != "stillwater.example.com" {
		t.Errorf("ACME.Domain = %q", cfg.ACME.Domain)
	}
	if cfg.ACME.Email != "admin@example.com" {
		t.Errorf("ACME.Email = %q", cfg.ACME.Email)
	}
	if cfg.ACME.CA != "letsencrypt" {
		t.Errorf("ACME.CA = %q", cfg.ACME.CA)
	}
	if cfg.ACME.EabKeyID != "key-id" {
		t.Errorf("ACME.EabKeyID = %q", cfg.ACME.EabKeyID)
	}
	if cfg.ACME.EabMacKey != "mac-key" {
		t.Errorf("ACME.EabMacKey = %q", cfg.ACME.EabMacKey)
	}
	if cfg.ACME.IP != "203.0.113.5" {
		t.Errorf("ACME.IP = %q", cfg.ACME.IP)
	}
	if cfg.ACME.CacheDir != "/var/lib/acme" {
		t.Errorf("ACME.CacheDir = %q", cfg.ACME.CacheDir)
	}
}

// TestLoad_TLSFromTOML asserts the new [server.tls], [server.http_redirect],
// [server.http3], and [acme] sections parse cleanly from TOML.
func TestLoad_TLSFromTOML(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[server]
port = 1973

[server.tls]
cert_file = "/etc/ssl/cert.pem"
key_file = "/etc/ssl/key.pem"
port = 443

[server.http_redirect]
port = 80

[server.http3]
enabled = true

[database]
path = "/tmp/test.db"

[acme]
domain = "example.com"
email = "ops@example.com"
ca = "letsencrypt-staging"
cache_dir = "/var/lib/acme"
`), 0o644); err != nil {
		t.Fatalf("write toml: %v", err)
	}

	cfg, err := Load(tomlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.TLS.CertFile != "/etc/ssl/cert.pem" {
		t.Errorf("TLS.CertFile = %q", cfg.Server.TLS.CertFile)
	}
	if cfg.Server.TLS.Port != 443 {
		t.Errorf("TLS.Port = %d", cfg.Server.TLS.Port)
	}
	if cfg.Server.HTTPRedirect.Port != 80 {
		t.Errorf("HTTPRedirect.Port = %d", cfg.Server.HTTPRedirect.Port)
	}
	if !cfg.Server.HTTP3.Enabled {
		t.Error("HTTP3.Enabled = false; want true")
	}
	if cfg.ACME.Domain != "example.com" {
		t.Errorf("ACME.Domain = %q", cfg.ACME.Domain)
	}
	if cfg.ACME.CA != "letsencrypt-staging" {
		t.Errorf("ACME.CA = %q", cfg.ACME.CA)
	}
}

// TestLoad_TLSFromYAML asserts the YAML form parses identically. YAML keys
// use snake_case to mirror the TOML form (server.tls.cert_file etc.).
func TestLoad_TLSFromYAML(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
server:
  port: 1973
  tls:
    cert_file: /etc/ssl/cert.pem
    key_file: /etc/ssl/key.pem
    port: 443
  http_redirect:
    port: 80
  http3:
    enabled: true
database:
  path: /tmp/test.db
acme:
  domain: example.com
  email: ops@example.com
  ca: letsencrypt-staging
  cache_dir: /var/lib/acme
`), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	cfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.TLS.CertFile != "/etc/ssl/cert.pem" {
		t.Errorf("TLS.CertFile = %q", cfg.Server.TLS.CertFile)
	}
	if cfg.Server.TLS.Port != 443 {
		t.Errorf("TLS.Port = %d", cfg.Server.TLS.Port)
	}
	if cfg.Server.HTTPRedirect.Port != 80 {
		t.Errorf("HTTPRedirect.Port = %d", cfg.Server.HTTPRedirect.Port)
	}
	if !cfg.Server.HTTP3.Enabled {
		t.Error("HTTP3.Enabled = false; want true")
	}
	if cfg.ACME.Domain != "example.com" {
		t.Errorf("ACME.Domain = %q", cfg.ACME.Domain)
	}
}

// TestValidate_TLSCertWithoutKey rejects half-configured pairs so a typo in
// one of the two paths can't quietly disable TLS.
func TestValidate_TLSCertWithoutKey(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[server]
port = 1973

[server.tls]
cert_file = "/etc/ssl/cert.pem"

[database]
path = "/tmp/test.db"
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(tomlPath); err == nil {
		t.Fatal("expected error: cert without key")
	}
}

func TestValidate_TLSKeyWithoutCert(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[server]
port = 1973

[server.tls]
key_file = "/etc/ssl/key.pem"

[database]
path = "/tmp/test.db"
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(tomlPath); err == nil {
		t.Fatal("expected error: key without cert")
	}
}

func TestValidate_InvalidTLSPort(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("SW_TLS_PORT", "99999")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: TLS port out of range")
	}
}

func TestValidate_NegativeRedirectPort(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_HTTP_REDIRECT_PORT", "-1")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: negative redirect port")
	}
}

// TestValidate_TLSPortMatchesRedirect rejects a configuration where the HTTPS
// and plain-HTTP redirect listeners would race for the same TCP port.
func TestValidate_TLSPortMatchesRedirect(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("SW_TLS_PORT", "443")
	t.Setenv("SW_HTTP_REDIRECT_PORT", "443")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: TLS port == redirect port")
	}
}

// TestValidate_TLSPortCollapseClashesWithRedirect covers the collapse case
// where TLS reuses Server.Port (TLS.Port unset) and the redirect listener is
// also bound to Server.Port. The effective TLS port collides with the
// redirect, so validation must reject.
func TestValidate_TLSPortCollapseClashesWithRedirect(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_PORT", "1973")
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("SW_HTTP_REDIRECT_PORT", "1973")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: collapse TLS port == redirect port")
	}
}

// TestValidate_TLSConfiguredWithoutPortCollapsesToServerPort exercises the
// happy path: cert and key set, TLS.Port unset. Loader accepts it; the
// listener layer (RunListeners) treats Server.Port as the HTTPS port.
func TestValidate_TLSConfiguredWithoutPortCollapsesToServerPort(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/k.pem")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.TLS.Port != 0 {
		t.Errorf("TLS.Port = %d; want 0 (collapse sentinel)", cfg.Server.TLS.Port)
	}
	if cfg.Server.Port != 1973 {
		t.Errorf("Server.Port = %d; want 1973", cfg.Server.Port)
	}
}

// TestEnsureScaffold_IncludesTLSSection asserts the first-run scaffold
// surfaces the new sections so users discover the knobs without reading the
// docs.
func TestEnsureScaffold_IncludesTLSSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if _, err := EnsureScaffold(path); err != nil {
		t.Fatalf("EnsureScaffold: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	body := string(data)
	for _, want := range []string{"[server.tls]", "[server.http_redirect]", "[server.http3]", "[acme]"} {
		if !strings.Contains(body, want) {
			t.Errorf("scaffold missing %q section", want)
		}
	}
	// Round-trip: the scaffold itself must still parse.
	if _, err := Load(path); err != nil {
		t.Fatalf("Load after scaffold: %v", err)
	}
}
