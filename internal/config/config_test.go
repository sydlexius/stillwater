package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.Server.Port != 1973 {
		t.Errorf("Server.Port = %d, want 1973", cfg.Server.Port)
	}
	if cfg.Database.Path != "/data/stillwater.db" {
		t.Errorf("Database.Path = %q, want /data/stillwater.db", cfg.Database.Path)
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

func TestTLS_DefaultPortMatchesServerPort(t *testing.T) {
	clearSWEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.TLS.Port != cfg.Server.Port {
		t.Errorf("TLS.Port = %d, want %d (same as Server.Port)", cfg.Server.TLS.Port, cfg.Server.Port)
	}
	if cfg.TLSEnabled() {
		t.Error("TLSEnabled() = true, want false when no cert/key configured")
	}
}

func TestTLS_EnvVarsLoaded(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/certs/fullchain.pem")
	t.Setenv("SW_TLS_KEY_FILE", "/certs/privkey.pem")
	t.Setenv("SW_TLS_PORT", "443")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.TLS.CertFile != "/certs/fullchain.pem" {
		t.Errorf("TLS.CertFile = %q, want /certs/fullchain.pem", cfg.Server.TLS.CertFile)
	}
	if cfg.Server.TLS.KeyFile != "/certs/privkey.pem" {
		t.Errorf("TLS.KeyFile = %q, want /certs/privkey.pem", cfg.Server.TLS.KeyFile)
	}
	if cfg.Server.TLS.Port != 443 {
		t.Errorf("TLS.Port = %d, want 443", cfg.Server.TLS.Port)
	}
	if !cfg.TLSEnabled() {
		t.Error("TLSEnabled() = false, want true when cert and key are set")
	}
}

func TestTLS_YAMLLoaded(t *testing.T) {
	clearSWEnv(t)
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(yamlPath, []byte(`
server:
  port: 1973
  tls:
    cert_file: /certs/cert.pem
    key_file: /certs/key.pem
    port: 443
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
	if cfg.Server.TLS.CertFile != "/certs/cert.pem" {
		t.Errorf("TLS.CertFile = %q, want /certs/cert.pem", cfg.Server.TLS.CertFile)
	}
	if cfg.Server.TLS.KeyFile != "/certs/key.pem" {
		t.Errorf("TLS.KeyFile = %q, want /certs/key.pem", cfg.Server.TLS.KeyFile)
	}
	if cfg.Server.TLS.Port != 443 {
		t.Errorf("TLS.Port = %d, want 443", cfg.Server.TLS.Port)
	}
	if !cfg.TLSEnabled() {
		t.Error("TLSEnabled() = false, want true")
	}
}

func TestTLS_OnlyCertSetReturnsError(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_CERT_FILE", "/certs/fullchain.pem")

	_, err := Load("")
	if err == nil {
		t.Fatal("expected error when only cert is set without key")
	}
}

func TestTLS_OnlyKeySetReturnsError(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_TLS_KEY_FILE", "/certs/privkey.pem")

	_, err := Load("")
	if err == nil {
		t.Fatal("expected error when only key is set without cert")
	}
}

func TestTLS_CustomPortWithoutTLSDefaultsToServerPort(t *testing.T) {
	clearSWEnv(t)
	t.Setenv("SW_PORT", "8080")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.TLS.Port != 8080 {
		t.Errorf("TLS.Port = %d, want 8080 (same as Server.Port)", cfg.Server.TLS.Port)
	}
}
