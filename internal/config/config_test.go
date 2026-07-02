package config

import (
	"fmt"
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

// TestUX_DefaultEnvAndValidation covers the SW_UX UI-channel flag (M55 #1340):
// it defaults to "stable", accepts the env override for the three legal values,
// and rejects an unknown value at load time.
func TestUX_DefaultEnvAndValidation(t *testing.T) {
	t.Run("default is stable", func(t *testing.T) {
		clearSWEnv(t)
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Server.UX != "stable" {
			t.Errorf("Server.UX = %q, want stable", cfg.Server.UX)
		}
	})

	for _, val := range []string{"stable", "next", "dual"} {
		t.Run("env accepts "+val, func(t *testing.T) {
			clearSWEnv(t)
			t.Setenv("SW_UX", val)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load() with SW_UX=%q error = %v", val, err)
			}
			if cfg.Server.UX != val {
				t.Errorf("Server.UX = %q, want %q", cfg.Server.UX, val)
			}
		})
	}

	t.Run("rejects unknown value", func(t *testing.T) {
		clearSWEnv(t)
		t.Setenv("SW_UX", "v2")
		if _, err := Load(""); err == nil {
			t.Error("Load() with SW_UX=v2 returned nil error, want validation failure")
		}
	})

	t.Run("empty value in file normalizes to stable", func(t *testing.T) {
		clearSWEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte("[server]\nux = \"\"\n"), 0o600); err != nil {
			t.Fatalf("writing config: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Server.UX != "stable" {
			t.Errorf("Server.UX = %q, want stable (empty normalizes to default)", cfg.Server.UX)
		}
	})
}

func TestTrustedProxies_EnvAndValidation(t *testing.T) {
	t.Run("default is empty", func(t *testing.T) {
		clearSWEnv(t)
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if len(cfg.Server.TrustedProxies) != 0 {
			t.Errorf("Server.TrustedProxies = %v, want empty", cfg.Server.TrustedProxies)
		}
	})

	t.Run("valid CIDRs parse and split on commas", func(t *testing.T) {
		clearSWEnv(t)
		t.Setenv("SW_TRUSTED_PROXIES", "10.0.0.0/8, 192.168.0.0/16, ::1/128")
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load() with valid SW_TRUSTED_PROXIES error = %v", err)
		}
		want := []string{"10.0.0.0/8", "192.168.0.0/16", "::1/128"}
		if len(cfg.Server.TrustedProxies) != len(want) {
			t.Fatalf("TrustedProxies = %v, want %v", cfg.Server.TrustedProxies, want)
		}
		for i, w := range want {
			if cfg.Server.TrustedProxies[i] != w {
				t.Errorf("TrustedProxies[%d] = %q, want %q (whitespace should be trimmed)", i, cfg.Server.TrustedProxies[i], w)
			}
		}
	})

	t.Run("malformed CIDR is rejected", func(t *testing.T) {
		clearSWEnv(t)
		t.Setenv("SW_TRUSTED_PROXIES", "10.0.0.0/8, not-a-cidr")
		_, err := Load("")
		if err == nil {
			t.Fatal("Load() with malformed SW_TRUSTED_PROXIES returned nil error, want validation failure")
		}
		if !strings.Contains(err.Error(), "invalid SW_TRUSTED_PROXIES") {
			t.Errorf("error = %q, want it to contain 'invalid SW_TRUSTED_PROXIES'", err.Error())
		}
	})

	t.Run("bare IP without mask is rejected", func(t *testing.T) {
		clearSWEnv(t)
		t.Setenv("SW_TRUSTED_PROXIES", "10.0.0.1")
		if _, err := Load(""); err == nil {
			t.Error("Load() with a bare IP (no /mask) returned nil error, want validation failure")
		}
	})
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
		"SW_RULE_ENGINE_ARTIST_WORKERS",
		"SW_TLS_CERT_FILE", "SW_TLS_KEY_FILE", "SW_TLS_PORT",
		"SW_HTTP_REDIRECT_PORT", "SW_HTTP3_ENABLED", "SW_HTTP3_PORT",
		"SW_ACME_DOMAIN", "SW_ACME_EMAIL", "SW_ACME_CA",
		"SW_ACME_EAB_KEY_ID", "SW_ACME_EAB_MAC_KEY",
		"SW_ACME_IP", "SW_ACME_CACHE_DIR", "SW_UX",
		"SW_TRUSTED_PROXIES",
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

// TestLoad_YAMLSetsDeprecationFlag asserts that loading a YAML config file
// flags the deprecated format on the returned Config (issue #1274), while TOML
// and the missing-file/env-only paths leave the flag false. The startup path
// reads this flag to emit the YAML deprecation WARN.
func TestLoad_YAMLSetsDeprecationFlag(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()

	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
server:
  port: 8080
database:
  path: /tmp/test.db
`), 0o644); err != nil {
		t.Fatalf("writing yaml config: %v", err)
	}
	yamlCfg, err := Load(yamlPath)
	if err != nil {
		t.Fatalf("Load yaml: %v", err)
	}
	if !yamlCfg.DeprecatedYAMLFormat {
		t.Error("DeprecatedYAMLFormat = false for a YAML file, want true")
	}

	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[server]
port = 8080
[database]
path = "/tmp/test.db"
`), 0o644); err != nil {
		t.Fatalf("writing toml config: %v", err)
	}
	tomlCfg, err := Load(tomlPath)
	if err != nil {
		t.Fatalf("Load toml: %v", err)
	}
	if tomlCfg.DeprecatedYAMLFormat {
		t.Error("DeprecatedYAMLFormat = true for a TOML file, want false")
	}

	// Missing file / env-only path must not flag YAML.
	defCfg, err := Load("/nonexistent/path/config.yaml")
	if err != nil {
		t.Fatalf("Load missing file: %v", err)
	}
	if defCfg.DeprecatedYAMLFormat {
		t.Error("DeprecatedYAMLFormat = true for a missing file, want false")
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

// TestValidate_ArtistWorkersNonPositive verifies that a non-positive
// artist_workers from file-backed config (TOML/YAML, which bypass the env
// path's setIntPositive) is normalized to the documented default rather than
// flowing through and collapsing concurrency to sequential.
func TestValidate_ArtistWorkersNonPositive(t *testing.T) {
	wantDefault := Default().RuleEngine.ArtistWorkers
	cases := []struct {
		name    string
		workers int
		want    int
	}{
		{"zero normalizes to default", 0, wantDefault},
		{"negative normalizes to default", -3, wantDefault},
		{"positive passes through", 8, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearSWEnv(t)
			dir := t.TempDir()
			yamlPath := filepath.Join(dir, "config.yaml")
			body := fmt.Sprintf(`
server:
  port: 1973
database:
  path: /tmp/test.db
rule_engine:
  artist_workers: %d
`, tc.workers)
			if err := os.WriteFile(yamlPath, []byte(body), 0o644); err != nil {
				t.Fatalf("writing config file: %v", err)
			}
			cfg, err := Load(yamlPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.RuleEngine.ArtistWorkers != tc.want {
				t.Errorf("RuleEngine.ArtistWorkers = %d, want %d", cfg.RuleEngine.ArtistWorkers, tc.want)
			}
		})
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

func TestLoadFromEnv_BackupEnabledEmptyString(t *testing.T) {
	// SW_BACKUP_ENABLED="" (present-but-empty) must set Enabled = false,
	// matching the original LookupEnv behavior before the table-driven refactor.
	clearSWEnv(t)
	t.Setenv("SW_BACKUP_ENABLED", "")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backup.Enabled {
		t.Error("Backup.Enabled = true; want false when SW_BACKUP_ENABLED is present-but-empty")
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

// TestLoadFromEnv_TLSEnvBranches covers the SW_TLS_* + SW_HTTP_REDIRECT_*
// + SW_HTTP3_* env-var branches. ACME knobs are exercised separately
// because SW_ACME_DOMAIN is mutually exclusive with the BYO cert pair (see
// TestLoadFromEnv_ACMEStubs).
func TestLoadFromEnv_TLSEnvBranches(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/cert.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/key.pem")
	t.Setenv("SW_TLS_PORT", "443")
	t.Setenv("SW_HTTP_REDIRECT_PORT", "80")
	t.Setenv("SW_HTTP3_ENABLED", "true")

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
}

// TestLoadFromEnv_ACMEStubs covers the SW_ACME_* env-var branches in
// isolation from BYO TLS, since ACME is mutually exclusive with
// SW_TLS_CERT_FILE/SW_TLS_KEY_FILE. It uses the EAB-with-DNS combination
// (ZeroSSL against a domain), which is a valid live config: Domain + EAB pair.
// The IP-SAN branch (SW_ACME_IP, mutually exclusive with SW_ACME_DOMAIN) is
// covered separately by TestLoadFromEnv_ACMEIPSAN.
func TestLoadFromEnv_ACMEStubs(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_ACME_DOMAIN", "stillwater.example.com")
	t.Setenv("SW_ACME_EMAIL", "admin@example.com")
	t.Setenv("SW_ACME_CA", "https://acme-staging-v02.api.letsencrypt.org/directory")
	t.Setenv("SW_ACME_EAB_KEY_ID", "key-id")
	t.Setenv("SW_ACME_EAB_MAC_KEY", "mac-key")
	t.Setenv("SW_ACME_CACHE_DIR", "/var/lib/acme")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ACME.Domain != "stillwater.example.com" {
		t.Errorf("ACME.Domain = %q", cfg.ACME.Domain)
	}
	if cfg.ACME.Email != "admin@example.com" {
		t.Errorf("ACME.Email = %q", cfg.ACME.Email)
	}
	if cfg.ACME.CA != "https://acme-staging-v02.api.letsencrypt.org/directory" {
		t.Errorf("ACME.CA = %q", cfg.ACME.CA)
	}
	if cfg.ACME.EabKeyID != "key-id" {
		t.Errorf("ACME.EabKeyID = %q", cfg.ACME.EabKeyID)
	}
	if cfg.ACME.EabMacKey != "mac-key" {
		t.Errorf("ACME.EabMacKey = %q", cfg.ACME.EabMacKey)
	}
	if cfg.ACME.CacheDir != "/var/lib/acme" {
		t.Errorf("ACME.CacheDir = %q", cfg.ACME.CacheDir)
	}
	if !cfg.ACME.UsesEAB() {
		t.Error("ACME.UsesEAB() = false; want true with both EAB fields set")
	}
	if !cfg.ACME.Active() {
		t.Error("ACME.Active() = false; want true with Domain set")
	}
}

// TestLoadFromEnv_ACMEIPSAN covers the SW_ACME_IP env-var branch in isolation.
// IP is mutually exclusive with Domain, so this config sets IP without Domain.
func TestLoadFromEnv_ACMEIPSAN(t *testing.T) {
	clearSWEnv(t)
	// Use a genuinely public IP: the RFC 5737 documentation ranges
	// (203.0.113.0/24 et al.) are now rejected by validateACMEIP as non-routable.
	t.Setenv("SW_ACME_IP", "8.8.8.8")
	t.Setenv("SW_ACME_EMAIL", "admin@example.com")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ACME.IP != "8.8.8.8" {
		t.Errorf("ACME.IP = %q", cfg.ACME.IP)
	}
	if !cfg.ACME.Active() {
		t.Error("ACME.Active() = false; want true with IP set")
	}
	if cfg.ACME.UsesEAB() {
		t.Error("ACME.UsesEAB() = true; want false with no EAB fields")
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
}

// TestLoad_ACMEFromTOML asserts the [acme] section parses cleanly from
// TOML. ACME and BYO TLS are mutually exclusive so the load is exercised
// in isolation.
func TestLoad_ACMEFromTOML(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(`
[database]
path = "/tmp/test.db"

[acme]
domain = "example.com"
email = "ops@example.com"
ca = "https://acme-staging-v02.api.letsencrypt.org/directory"
cache_dir = "/var/lib/acme"
`), 0o644); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	cfg, err := Load(tomlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ACME.Domain != "example.com" {
		t.Errorf("ACME.Domain = %q", cfg.ACME.Domain)
	}
	if cfg.ACME.CA != "https://acme-staging-v02.api.letsencrypt.org/directory" {
		t.Errorf("ACME.CA = %q", cfg.ACME.CA)
	}
	if cfg.ACME.CacheDir != "/var/lib/acme" {
		t.Errorf("ACME.CacheDir = %q", cfg.ACME.CacheDir)
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

// TestValidate_ACMECollapseClashesWithChallenge covers the ACME collapse
// case CR flagged: SW_ACME_DOMAIN set with SW_PORT=80 (and SW_TLS_PORT
// unset). The HTTPS-ACME listener collapses onto SW_PORT (80), and the
// challenge listener defaults to 80 too -- they would race for the
// socket. Validation must catch this before any bind happens.
func TestValidate_ACMECollapseClashesWithChallenge(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_PORT", "80")
	t.Setenv("SW_ACME_DOMAIN", "host.example.com")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: ACME collapsed TLS port == challenge port (both 80)")
	}
}

// TestValidate_ACMECollapseExplicitRedirectClash covers the analogous
// case where SW_HTTP_REDIRECT_PORT is set to the same value as the
// collapsed TLS port (Server.Port). Without ACME-aware collapse the
// validator missed this; now it should reject.
func TestValidate_ACMECollapseExplicitRedirectClash(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_PORT", "1973")
	t.Setenv("SW_ACME_DOMAIN", "host.example.com")
	t.Setenv("SW_HTTP_REDIRECT_PORT", "1973")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: ACME collapsed TLS port == redirect port (both 1973)")
	}
}

// TestValidate_RedirectWithoutTLS rejects a configuration that asks for the
// HTTP-to-HTTPS redirect listener but does not configure TLS. There would be
// nothing to redirect to; quietly skipping the redirect would silently leave
// the deploy on plain HTTP.
func TestValidate_RedirectWithoutTLS(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_HTTP_REDIRECT_PORT", "80")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: redirect port set without TLS")
	}
}

// TestValidate_RedirectWithTLSAccepted covers the happy path: TLS configured
// with a split port, redirect listener on a distinct port. Loader accepts it.
func TestValidate_RedirectWithTLSAccepted(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("SW_TLS_PORT", "443")
	t.Setenv("SW_HTTP_REDIRECT_PORT", "80")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.HTTPRedirect.Port != 80 {
		t.Errorf("HTTPRedirect.Port = %d; want 80", cfg.Server.HTTPRedirect.Port)
	}
	if cfg.Server.TLS.Port != 443 {
		t.Errorf("TLS.Port = %d; want 443", cfg.Server.TLS.Port)
	}
}

// TestValidateACMEIP exercises the SW_ACME_IP validator directly: empty is
// accepted (optional field), public IPv4/IPv6 are accepted, and malformed or
// non-routable addresses are rejected. The non-routable set mirrors the SSRF
// blocklist via httpsafe.IsPublicIP, so this also guards against the two checks
// drifting apart.
func TestValidateACMEIP(t *testing.T) {
	t.Parallel()
	accepted := []string{"", "8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	for _, ip := range accepted {
		if err := validateACMEIP(ip); err != nil {
			t.Errorf("validateACMEIP(%q) = %v; want nil", ip, err)
		}
	}
	rejected := []string{
		"not-an-ip",
		"999.999.999.999",
		"127.0.0.1",    // loopback
		"10.0.0.1",     // RFC1918
		"172.16.5.4",   // RFC1918
		"192.168.1.10", // RFC1918
		"169.254.0.1",  // link-local
		"0.0.0.0",      // unspecified
		"100.64.0.1",   // CGNAT (RFC6598)
		"198.18.0.1",   // RFC2544 benchmark
		"192.0.2.1",    // RFC5737 documentation (TEST-NET-1)
		"198.51.100.1", // RFC5737 documentation (TEST-NET-2)
		"203.0.113.5",  // RFC5737 documentation (TEST-NET-3)
		"224.0.0.1",    // multicast (224.0.0.0/4)
		"233.252.0.1",  // multicast (globally-scoped doc block)
		"::1",          // loopback IPv6
		"fe80::1",      // link-local IPv6
		"2001:db8::1",  // RFC3849 documentation IPv6
		"ff02::1",      // multicast IPv6
	}
	for _, ip := range rejected {
		if err := validateACMEIP(ip); err == nil {
			t.Errorf("validateACMEIP(%q) = nil; want error", ip)
		}
	}
}

// TestValidate_ACMEDomainAndIPMutuallyExclusive rejects a config that sets both
// SW_ACME_DOMAIN and SW_ACME_IP -- an ACME order is for a DNS name or an IP,
// not both.
func TestValidate_ACMEDomainAndIPMutuallyExclusive(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_ACME_DOMAIN", "host.example.com")
	// A genuinely public IP so the per-field IP validator passes and the
	// cross-field exclusivity rule is the one that fires (asserted below).
	t.Setenv("SW_ACME_IP", "8.8.8.8")
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error: SW_ACME_DOMAIN and SW_ACME_IP both set")
	}
	if want := "SW_ACME_DOMAIN and SW_ACME_IP are mutually exclusive"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q; want substring %q (a wrong rule failing first must be caught)", err, want)
	}
}

// TestValidate_ACMEDomainAlonePasses confirms a DNS-only ACME config is valid.
func TestValidate_ACMEDomainAlonePasses(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_ACME_DOMAIN", "host.example.com")
	if _, err := Load(""); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// TestValidate_ACMEIPAlonePasses confirms an IP-SAN-only ACME config is valid.
func TestValidate_ACMEIPAlonePasses(t *testing.T) {
	clearSWEnv(t)
	// Genuinely public IP: the RFC 5737 documentation ranges are now rejected.
	t.Setenv("SW_ACME_IP", "8.8.8.8")
	if _, err := Load(""); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// TestValidate_ACMEPrivateIPRejected confirms validate() rejects a private IP
// through the full Load path (not just the unit validator).
func TestValidate_ACMEPrivateIPRejected(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_ACME_IP", "192.168.1.10")
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error: SW_ACME_IP is a private address")
	}
	if want := "must be a publicly routable address"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q; want substring %q", err, want)
	}
}

// TestValidate_ACMEEabHalfConfigRejected rejects a half-configured EAB pair
// (key id without HMAC, or vice versa).
func TestValidate_ACMEEabHalfConfigRejected(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_ACME_DOMAIN", "host.example.com")
	t.Setenv("SW_ACME_EAB_KEY_ID", "key-id")
	// SW_ACME_EAB_MAC_KEY deliberately unset.
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error: EAB key id set without EAB mac key")
	}
	if want := "SW_ACME_EAB_KEY_ID and SW_ACME_EAB_MAC_KEY must both be set or both be empty"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q; want substring %q", err, want)
	}
}

// TestValidate_ACMEEabWithoutIdentifierRejected rejects EAB credentials with
// neither a domain nor an IP to order against.
func TestValidate_ACMEEabWithoutIdentifierRejected(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_ACME_EAB_KEY_ID", "key-id")
	t.Setenv("SW_ACME_EAB_MAC_KEY", "mac-key")
	// Neither SW_ACME_DOMAIN nor SW_ACME_IP set.
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error: EAB configured without an identifier")
	}
	if want := "require an identifier"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q; want substring %q", err, want)
	}
}

// TestValidate_ACMEIPMutuallyExclusiveWithBYO rejects setting SW_ACME_IP
// alongside a BYO TLS cert/key pair.
func TestValidate_ACMEIPMutuallyExclusiveWithBYO(t *testing.T) {
	clearSWEnv(t)
	// Public IP so the per-field IP validator passes and the ACME-vs-BYO
	// cross-field rule is the one that fires (asserted below).
	t.Setenv("SW_ACME_IP", "8.8.8.8")
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/k.pem")
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error: SW_ACME_IP set alongside BYO TLS")
	}
	if want := "mutually exclusive with SW_TLS_CERT_FILE/SW_TLS_KEY_FILE"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q; want substring %q (a wrong rule failing first must be caught)", err, want)
	}
}

// TestLoad_RedirectPortMalformedFailsLoud asserts that a non-numeric value
// for SW_HTTP_REDIRECT_PORT returns an error from Load instead of silently
// discarding the env var (which would leave the redirect listener disabled
// and the operator unaware).
func TestLoad_RedirectPortMalformedFailsLoud(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_HTTP_REDIRECT_PORT", "80a")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: malformed SW_HTTP_REDIRECT_PORT must not be silently discarded")
	}
}

// TestLoad_HTTP3PortMalformedFailsLoud mirrors the redirect-port malformed
// guard for SW_HTTP3_PORT: a non-numeric value must return an error from Load
// rather than silently leaving HTTP/3 on the effective HTTPS port. Silent
// fallback would mask a typo that an operator expected to bind a dedicated
// QUIC port.
func TestLoad_HTTP3PortMalformedFailsLoud(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_HTTP3_PORT", "443x")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: malformed SW_HTTP3_PORT must not be silently discarded")
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

// TestValidate_HTTP3RequiresTLS rejects SW_HTTP3_ENABLED=true without a
// configured cert/key pair. HTTP/3 mandates TLS 1.3, so the listener layer
// has nothing to bind without TLS material.
func TestValidate_HTTP3RequiresTLS(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_HTTP3_ENABLED", "true")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: HTTP/3 enabled without TLS")
	}
}

// TestValidate_HTTP3WithTLSAccepted is the happy path: HTTP/3 enabled and
// TLS configured -- Load succeeds.
func TestValidate_HTTP3WithTLSAccepted(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("SW_HTTP3_ENABLED", "true")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Server.HTTP3.Enabled {
		t.Error("HTTP3.Enabled = false; want true")
	}
}

// TestValidate_InvalidHTTP3Port rejects an out-of-range explicit HTTP/3 port.
func TestValidate_InvalidHTTP3Port(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("SW_HTTP3_ENABLED", "true")
	t.Setenv("SW_HTTP3_PORT", "70000")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: HTTP/3 port out of range")
	}
}

// TestLoadEnv_HTTP3Port populates the explicit override.
func TestLoadEnv_HTTP3Port(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("SW_HTTP3_ENABLED", "true")
	t.Setenv("SW_HTTP3_PORT", "8443")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.HTTP3.Port != 8443 {
		t.Errorf("HTTP3.Port = %d; want 8443", cfg.Server.HTTP3.Port)
	}
}

// TestValidate_ACMEDomainMutuallyExclusiveWithBYOCert rejects setting
// SW_ACME_DOMAIN alongside a BYO cert/key. The listener layer would have
// to silently pick one source; rejecting the combination at config time
// surfaces the ambiguity loudly.
func TestValidate_ACMEDomainMutuallyExclusiveWithBYOCert(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/tmp/c.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/tmp/k.pem")
	t.Setenv("SW_ACME_DOMAIN", "host.example.com")
	if _, err := Load(""); err == nil {
		t.Fatal("expected error: SW_ACME_DOMAIN cannot coexist with SW_TLS_CERT_FILE/SW_TLS_KEY_FILE")
	}
}

// TestValidate_ACMEDomainAloneAccepted asserts the happy path: ACME on,
// no BYO cert. Loader accepts it; the listener layer wires autocert.
func TestValidate_ACMEDomainAloneAccepted(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_ACME_DOMAIN", "host.example.com")
	t.Setenv("SW_ACME_EMAIL", "admin@example.com")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ACME.Domain != "host.example.com" {
		t.Errorf("ACME.Domain = %q; want %q", cfg.ACME.Domain, "host.example.com")
	}
	if cfg.ACME.Email != "admin@example.com" {
		t.Errorf("ACME.Email = %q; want %q", cfg.ACME.Email, "admin@example.com")
	}
}

// TestEnsureScaffold_IncludesTLSSection asserts the first-run scaffold
// surfaces the new sections so users discover the knobs without reading the
// docs.
func TestEnsureScaffold_IncludesTLSSection(t *testing.T) {
	clearSWEnv(t)
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
