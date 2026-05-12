package config

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzConfigLoad feeds arbitrary byte slices to the config loader to find
// panics or unexpected behavior in Load, loadFromFile, and detectFormat.
// The loader must never panic regardless of input -- returning an error for
// invalid config is expected and correct.
//
// The file is written with no extension so the format-sniffing branch of
// detectFormat is always exercised (extension-based short-circuits only
// trigger for ".toml", ".yaml", and ".yml").
func FuzzConfigLoad(f *testing.F) {
	// Minimal valid YAML config.
	f.Add([]byte("server:\n  port: 1973\n"))

	// Minimal valid TOML config.
	f.Add([]byte("[server]\nport = 1973\n"))

	// File starting with comments only (no config keys).
	f.Add([]byte("# comment only\n# another comment\n"))

	// UTF-8 BOM prefix followed by valid YAML.
	f.Add(append([]byte{0xEF, 0xBB, 0xBF}, []byte("server:\n  port: 8080\n")...))

	// Mixed-indentation YAML (tabs mixed with spaces -- parsers often reject this).
	f.Add([]byte("server:\n\t port: 9090\n"))

	// TOML with table-array conflicts.
	f.Add([]byte("[[server]]\nport = 1973\n[[server]]\nport = 9090\n"))

	// Very long line with no newline (single-line config).
	f.Add([]byte("server: {port: 1973, base_path: /stillwater, tls: {cert_file: /certs/cert.pem, key_file: /certs/key.pem}}"))

	// File where the first non-comment line begins with '=' (invalid in both YAML and TOML).
	f.Add([]byte("# comment\n= not valid\n"))

	// YAML with ':' embedded inside a TOML-style quoted string (format sniffer edge case).
	f.Add([]byte("# sniff me\nkey = \"value:with:colons\"\n"))

	// Purely whitespace input.
	f.Add([]byte("   \n\t\n   "))

	// Empty input.
	f.Add([]byte(""))

	// TOML table header that looks like YAML mapping.
	f.Add([]byte("[server]\n[database]\npath = /db/stillwater.db\n"))

	// YAML block mapping with nested keys.
	f.Add([]byte("database:\n  path: /music/stillwater.db\nlogging:\n  level: debug\n"))

	// Null bytes embedded in what looks like YAML.
	f.Add([]byte("server:\n  port: \x001973\n"))

	// High-byte UTF-8 sequences in string values.
	f.Add([]byte("server:\n  base_path: /\xe2\x80\x9cstillwater\xe2\x80\x9d\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Write the fuzz input to a file with no extension so the format-sniffing
		// path in detectFormat is always reached (extension-based routing would
		// bypass the sniffing heuristic we want to exercise).
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config")
		if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
			// Writing to a tempdir should never fail; surface as a test error.
			t.Fatalf("writing fuzz input to tempfile: %v", err)
		}

		// Clear SW_* env vars so environment overrides don't interfere with
		// the fuzz corpus -- we want to exercise the file-parsing path.
		for _, key := range []string{
			"SW_PORT", "SW_BASE_PATH", "SW_DB_PATH", "SW_SESSION_SECRET",
			"SW_ENCRYPTION_KEY", "SW_MUSIC_PATH", "SW_SCANNER_EXCLUSIONS",
			"SW_BACKUP_PATH", "SW_BACKUP_RETENTION", "SW_BACKUP_INTERVAL",
			"SW_BACKUP_ENABLED", "SW_LOG_LEVEL", "SW_LOG_FORMAT",
			"SW_TLS_CERT_FILE", "SW_TLS_KEY_FILE", "SW_TLS_PORT",
			"SW_HTTP_REDIRECT_PORT", "SW_HTTP3_ENABLED", "SW_HTTP3_PORT",
			"SW_ACME_DOMAIN", "SW_ACME_EMAIL", "SW_ACME_CA",
			"SW_ACME_EAB_KEY_ID", "SW_ACME_EAB_MAC_KEY",
			"SW_ACME_IP", "SW_ACME_CACHE_DIR",
		} {
			t.Setenv(key, "")
		}

		// Load must not panic. Errors for malformed input are expected.
		_, _ = Load(cfgPath)
	})
}
