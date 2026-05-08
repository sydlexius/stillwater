package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"

	"github.com/sydlexius/stillwater/internal/filesystem"
)

// Config holds all application configuration.
type Config struct {
	Server     ServerConfig     `yaml:"server" toml:"server"`
	Database   DatabaseConfig   `yaml:"database" toml:"database"`
	Auth       AuthConfig       `yaml:"auth" toml:"auth"`
	Encryption EncryptionConfig `yaml:"encryption" toml:"encryption"`
	Music      MusicConfig      `yaml:"music" toml:"music"`
	Scanner    ScannerConfig    `yaml:"scanner" toml:"scanner"`
	Backup     BackupConfig     `yaml:"backup" toml:"backup"`
	Logging    LoggingConfig    `yaml:"logging" toml:"logging"`
	ACME       ACMEConfig       `yaml:"acme" toml:"acme"`
}

// ServerConfig holds HTTP server settings.
//
// Struct tags drive the env-var reference codegen in cmd/gen-env-reference:
//   - env:  the environment variable name (SW_*)
//   - desc: a concise user-facing description (one or two sentences); a second
//     sentence is acceptable when it captures a constraint or runtime caveat
//   - default: rendered default; "unset" or "" when there is no default
type ServerConfig struct {
	Port            int                `yaml:"port" toml:"port" env:"SW_PORT" default:"1973" desc:"TCP port the HTTP server listens on. Numeric values outside 1-65535 are rejected at startup."`
	BasePath        string             `yaml:"base_path" toml:"base_path" env:"SW_BASE_PATH" default:"/" desc:"URL prefix for subfolder reverse-proxy deployments (for example /stillwater). When set from the environment the Settings UI marks the field read-only."`
	BasePathFromEnv bool               `yaml:"-" toml:"-"`
	TLS             TLSConfig          `yaml:"tls" toml:"tls"`
	HTTPRedirect    HTTPRedirectConfig `yaml:"http_redirect" toml:"http_redirect"`
	HTTP3           HTTP3Config        `yaml:"http3" toml:"http3"`
}

// TLSConfig holds direct (BYO certificate) TLS settings. When CertFile and
// KeyFile are both set, the HTTP server terminates TLS itself rather than
// relying on a fronting reverse proxy. Port 0 means "reuse Server.Port" so
// single-port deployments can flip to HTTPS without changing SW_PORT.
type TLSConfig struct {
	CertFile string `yaml:"cert_file" toml:"cert_file" env:"SW_TLS_CERT_FILE" default:"unset" desc:"Path to a PEM-encoded TLS certificate. When set together with SW_TLS_KEY_FILE Stillwater serves HTTPS directly instead of plain HTTP."`
	KeyFile  string `yaml:"key_file" toml:"key_file" env:"SW_TLS_KEY_FILE" default:"unset" desc:"Path to the PEM-encoded private key for SW_TLS_CERT_FILE. Both files must be readable by the Stillwater process."`
	Port     int    `yaml:"port" toml:"port" env:"SW_TLS_PORT" default:"unset" desc:"Optional dedicated HTTPS port. When unset Stillwater serves HTTPS on SW_PORT (collapse semantics, single listener). Numeric values outside 1-65535 are rejected at startup."`
}

// Enabled reports whether direct TLS termination is configured. Both CertFile
// and KeyFile must be set; the validate step rejects half-configured pairs.
func (c TLSConfig) Enabled() bool {
	return c.CertFile != "" && c.KeyFile != ""
}

// HTTPRedirectConfig configures the optional plain-HTTP listener that 301s to
// the HTTPS port. Active only when TLS is also configured (cert+key set);
// otherwise validate() rejects the misconfiguration at startup.
type HTTPRedirectConfig struct {
	Port int `yaml:"port" toml:"port" env:"SW_HTTP_REDIRECT_PORT" default:"unset" desc:"Optional plain-HTTP listener port that 301-redirects to the HTTPS listener. Requires TLS to be configured (SW_TLS_CERT_FILE + SW_TLS_KEY_FILE). Typical value 80; must differ from SW_TLS_PORT (or SW_PORT in collapse mode). Numeric values outside 1-65535 are rejected at startup."`
}

// HTTP3Config toggles the QUIC/HTTP3 listener. When Enabled is true and TLS
// is configured, Stillwater binds an HTTP/3 listener on the same port as the
// HTTPS listener (UDP) using the same TLS material. Clients are advertised
// HTTP/3 via the Alt-Svc response header. HTTP/3 requires TLS 1.3.
type HTTP3Config struct {
	Enabled bool `yaml:"enabled" toml:"enabled" env:"SW_HTTP3_ENABLED" default:"false" desc:"Set to true or 1 to enable an HTTP/3 (QUIC) listener over UDP. Requires direct TLS to be configured (SW_TLS_CERT_FILE and SW_TLS_KEY_FILE). The Alt-Svc header is added to all responses so HTTP/3-capable clients upgrade automatically; clients with UDP blocked fall back to HTTP/1.1+HTTP/2 over TCP."`
	Port    int  `yaml:"port" toml:"port" env:"SW_HTTP3_PORT" default:"unset" desc:"Optional dedicated UDP port for HTTP/3. When unset HTTP/3 reuses the effective HTTPS port (SW_TLS_PORT or SW_PORT). Numeric values outside 1-65535 are rejected at startup."`
}

// ACMEConfig holds Automatic Certificate Management Environment settings.
// All fields are stubs today; the autocert (Let's Encrypt / Buypass via
// golang.org/x/crypto/acme/autocert) and ZeroSSL IP-SAN (via go-acme/lego)
// wirings ship in later milestone PRs. Leaving the struct populated but unread
// keeps the env-var loader and reference docs stable across the milestone.
type ACMEConfig struct {
	Domain    string `yaml:"domain" toml:"domain" env:"SW_ACME_DOMAIN" default:"unset" desc:"Reserved for future use; not yet active. DNS name that the future ACME path will request certificates for."`
	Email     string `yaml:"email" toml:"email" env:"SW_ACME_EMAIL" default:"unset" desc:"Reserved for future use; not yet active. Contact email that will be registered with the ACME CA when the ACME path lands."`
	CA        string `yaml:"ca" toml:"ca" env:"SW_ACME_CA" default:"unset" desc:"Reserved for future use; not yet active. Will accept an ACME directory URL or shorthand (letsencrypt, letsencrypt-staging, buypass, zerossl) when the ACME path lands."`
	EabKeyID  string `yaml:"eab_key_id" toml:"eab_key_id" env:"SW_ACME_EAB_KEY_ID" default:"unset" desc:"Reserved for future use; not yet active. External Account Binding key identifier for ACME CAs that require it (for example ZeroSSL)."`
	EabMacKey string `yaml:"eab_mac_key" toml:"eab_mac_key" env:"SW_ACME_EAB_MAC_KEY" default:"unset" desc:"Reserved for future use; not yet active. External Account Binding HMAC key paired with SW_ACME_EAB_KEY_ID. Treat as a secret; will be persisted only after AES-256-GCM encryption when the ACME path lands."`
	IP        string `yaml:"ip" toml:"ip" env:"SW_ACME_IP" default:"unset" desc:"Reserved for future use; not yet active. Public IP address for IP-SAN certificate orders (ZeroSSL). Must not be an RFC1918, loopback, or link-local address."`
	CacheDir  string `yaml:"cache_dir" toml:"cache_dir" env:"SW_ACME_CACHE_DIR" default:"unset" desc:"Reserved for future use; not yet active. Directory where ACME account keys and issued certificates will be cached when the ACME path lands."`
}

// DatabaseConfig holds SQLite settings.
type DatabaseConfig struct {
	Path string `yaml:"path" toml:"path" env:"SW_DB_PATH" default:"/config/stillwater.db" desc:"Filesystem path to the SQLite database file."`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	SessionSecret string `yaml:"session_secret" toml:"session_secret" env:"SW_SESSION_SECRET" default:"unset" desc:"Long random string used to sign session cookies. When unset Stillwater generates one on first run and persists it in the config directory."` //nolint:gosec // G117: not a hardcoded secret, this is a config field
}

// EncryptionConfig holds encryption key settings.
type EncryptionConfig struct {
	Key string `yaml:"key" toml:"key" env:"SW_ENCRYPTION_KEY" default:"unset" desc:"Key used to encrypt provider API keys at rest. When unset Stillwater generates one on first run and persists it in the config directory."`
}

// MusicConfig holds music library path settings.
type MusicConfig struct {
	LibraryPath string `yaml:"library_path" toml:"library_path" env:"SW_MUSIC_PATH" default:"/music" desc:"Default music library path used as a starting point when no library has been added through the UI."`
}

// ScannerConfig holds scanner behavior settings.
type ScannerConfig struct {
	Depth      int      `yaml:"depth" toml:"depth"`
	Exclusions []string `yaml:"exclusions" toml:"exclusions" env:"SW_SCANNER_EXCLUSIONS" default:"Various Artists, Various, VA, Soundtrack, OST" desc:"Comma-separated artist directory names the scanner skips. Whitespace around each token is trimmed."`
}

// BackupConfig holds database backup settings.
type BackupConfig struct {
	Path           string `yaml:"path" toml:"path" env:"SW_BACKUP_PATH" default:"" desc:"Override the directory where automated database backups are written. When empty Stillwater writes to a backups/ subfolder of the config directory."`
	RetentionCount int    `yaml:"retention_count" toml:"retention_count" env:"SW_BACKUP_RETENTION" default:"7" desc:"Number of recent backups to keep. Must be a positive integer; non-positive or non-numeric values are silently ignored."`
	IntervalHours  int    `yaml:"interval_hours" toml:"interval_hours" env:"SW_BACKUP_INTERVAL" default:"24" desc:"Hours between automated backups. Must be a positive integer; non-positive or non-numeric values are silently ignored."`
	Enabled        bool   `yaml:"enabled" toml:"enabled" env:"SW_BACKUP_ENABLED" default:"true" desc:"Set to true or 1 to enable automated backups. Any other value disables them."`
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	Level  string `yaml:"level" toml:"level" env:"SW_LOG_LEVEL" default:"info" desc:"Log level at startup. One of trace, debug, info, warn, error. The runtime can also adjust the live level from the Logs settings tab."`
	Format string `yaml:"format" toml:"format" env:"SW_LOG_FORMAT" default:"json" desc:"Log output format. Use json for log aggregators or text for friendlier console output."`
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Port:     1973,
			BasePath: "/",
		},
		Database: DatabaseConfig{
			Path: "/config/stillwater.db",
		},
		Auth:       AuthConfig{},
		Encryption: EncryptionConfig{},
		Music: MusicConfig{
			LibraryPath: "/music",
		},
		Scanner: ScannerConfig{
			Depth: 1,
			Exclusions: []string{
				"Various Artists", "Various", "VA",
				"Soundtrack", "OST",
			},
		},
		Backup: BackupConfig{
			RetentionCount: 7,
			IntervalHours:  24,
			Enabled:        true,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
	}
}

// scaffoldTOML is the first-run config.toml content. Every field is commented
// out so the file documents the surface without overriding the in-code defaults
// or env-var values that may already be set. Users uncomment + edit the lines
// they want to pin.
const scaffoldTOML = `# Stillwater configuration
# Every field below is commented out; Stillwater applies the in-code default
# until you uncomment a line. Environment variables (SW_*) still override
# whatever this file sets, so the precedence order is:
#   built-in default < this file < SW_* environment variable

[server]
# port = 1973
# base_path = "/"

# Direct TLS (BYO certificate). When both files are set, Stillwater serves
# HTTPS itself instead of plain HTTP. Leave unset to keep terminating TLS at
# a fronting reverse proxy.
# See: https://sydlexius.github.io/stillwater/how-to/direct-tls-setup/
# [server.tls]
# cert_file = "/config/tls/fullchain.pem"
# key_file = "/config/tls/privkey.pem"
# port = 0  # 0 reuses [server].port; set to e.g. 443 for split-port deploys.

# Plain-HTTP redirect listener. When set together with [server.tls], Stillwater
# binds a second listener on this port that 301-redirects every request to the
# HTTPS listener. Requires TLS to be configured; otherwise startup fails.
# See: https://sydlexius.github.io/stillwater/how-to/http-redirect/
# [server.http_redirect]
# port = 80

# HTTP/3 (QUIC) listener. Requires direct TLS to be configured (HTTP/3
# mandates TLS 1.3). Stillwater advertises HTTP/3 via the Alt-Svc response
# header so capable clients upgrade automatically; clients with UDP blocked
# fall back to HTTP/1.1+HTTP/2 over TCP.
# See: https://sydlexius.github.io/stillwater/how-to/direct-tls-setup/#http3-quic-firewall
# [server.http3]
# enabled = false
# port = 0  # 0 reuses the effective HTTPS port (SW_TLS_PORT or SW_PORT).

# Automatic Certificate Management Environment (ACME). Reserved for future
# use; not yet active. Until the ACME path lands, configure direct TLS via
# [server.tls] above (BYO certificate) or terminate at a fronting proxy.
# [acme]
# domain = "stillwater.example.com"
# email = "admin@example.com"
# ca = "letsencrypt"
# cache_dir = "/config/acme-cache"

[database]
# path = "/config/stillwater.db"

[auth]
# session_secret is generated automatically on first run when unset.
# session_secret = ""

[encryption]
# key is generated automatically on first run when unset.
# key = ""

[music]
# library_path = "/music"

[scanner]
# depth = 1
# exclusions = ["Various Artists", "Various", "VA", "Soundtrack", "OST"]

[backup]
# path = ""
# retention_count = 7
# interval_hours = 24
# enabled = true

[logging]
# level = "info"
# format = "json"
`

// EnsureScaffold writes a default config.toml at path if the file does not
// already exist. It returns true when a new file was created. An empty path
// or an existing file is treated as a no-op (false, nil). Missing parent
// directories are created so the function works against a fresh data dir.
//
// Errors writing the file are returned to the caller so startup can decide
// whether to surface a warning or proceed with built-in defaults.
func EnsureScaffold(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	// Skip scaffolding for YAML paths: a TOML scaffold under a .yaml/.yml
	// filename would be unparsable by the YAML loader on first boot. Users
	// who configure SW_CONFIG_PATH to a YAML file are presumed to manage
	// their own config; the policy lives here so callers stay simple.
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return false, nil
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return false, fmt.Errorf("creating config directory: %w", err)
		}
	}
	// Skip if the file already exists. A direct stat is sufficient: this
	// runs once on first boot, the operator sequence does not produce
	// concurrent scaffolders, and the alternative (O_EXCL on the destination)
	// would skip the project's atomic-write contract that requires every
	// file write to go through internal/filesystem's tmp/rename helper.
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("checking for existing config: %w", err)
	}
	if err := filesystem.WriteFileAtomic(path, []byte(scaffoldTOML), 0o644); err != nil {
		return false, fmt.Errorf("writing config scaffold: %w", err)
	}
	return true, nil
}

// Load reads config from a TOML or YAML file (if it exists) and overrides with
// environment variables. Environment variables take precedence.
//
// Format detection: files with a .toml extension are parsed as TOML; .yaml or
// .yml are parsed as YAML. When the extension is missing or ambiguous the
// loader sniffs the first non-comment, non-whitespace byte: '[' or a key=value
// pair triggers TOML parsing, otherwise YAML.
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		if err := cfg.loadFromFile(path); err != nil {
			return nil, fmt.Errorf("loading config file: %w", err)
		}
	}

	if err := cfg.loadFromEnv(); err != nil {
		return nil, fmt.Errorf("loading env: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

// loadFromFile reads the config file at path and merges its contents into c
// using the format detected from the extension (or sniffed from the first
// non-comment byte for ambiguous filenames). A missing file is not an error;
// the caller falls back to env-var overrides plus in-code defaults.
func (c *Config) loadFromFile(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is from trusted config, not user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	switch detectFormat(path, data) {
	case formatTOML:
		if err := toml.Unmarshal(data, c); err != nil {
			return fmt.Errorf("parsing TOML: %w", err)
		}
		return nil
	case formatYAML:
		if err := yaml.Unmarshal(data, c); err != nil {
			return fmt.Errorf("parsing YAML: %w", err)
		}
		return nil
	}
	// Unreachable; detectFormat always returns one of the two formats.
	return nil
}

type configFormat int

const (
	formatYAML configFormat = iota
	formatTOML
)

// detectFormat picks a parser based on the file extension and, when the
// extension is ambiguous (anything other than .toml/.yaml/.yml), the first
// non-comment, non-whitespace byte of the file. TOML files always start with
// '[' (a table header) or a bareword key followed by '='. YAML keys end with
// ':' or the document is a list; in both cases the first significant byte
// will not match the TOML markers below.
func detectFormat(path string, data []byte) configFormat {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".toml":
		return formatTOML
	case ".yaml", ".yml":
		return formatYAML
	}
	// Sniff: find the first non-comment, non-whitespace line.
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		// Skip comments. '#' is valid in both YAML and TOML; '//' is invalid
		// in both but cheap to skip defensively.
		if line[0] == '#' || bytes.HasPrefix(line, []byte("//")) {
			continue
		}
		// TOML signals: a table header '[...]' or a 'key = value' assignment.
		if line[0] == '[' {
			return formatTOML
		}
		if idxEq := bytes.IndexByte(line, '='); idxEq > 0 {
			// TOML uses '=' between key and value. YAML uses ':' so a line
			// containing '=' before any ':' is almost certainly TOML.
			idxColon := bytes.IndexByte(line, ':')
			if idxColon < 0 || idxEq < idxColon {
				return formatTOML
			}
		}
		break
	}
	return formatYAML
}

// loadFromEnv overlays environment variables onto c. Env-var values take
// precedence over any file-loaded values per the API-first contract: an
// operator can override one knob via env without rewriting the whole file.
func (c *Config) loadFromEnv() error {
	if v := os.Getenv("SW_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid SW_PORT %q: %w", v, err)
		}
		c.Server.Port = port
	}
	if v := os.Getenv("SW_BASE_PATH"); v != "" {
		c.Server.BasePath = v
		c.Server.BasePathFromEnv = true
	}
	if v := os.Getenv("SW_DB_PATH"); v != "" {
		c.Database.Path = v
	}
	if v := os.Getenv("SW_SESSION_SECRET"); v != "" {
		c.Auth.SessionSecret = v
	}
	if v := os.Getenv("SW_ENCRYPTION_KEY"); v != "" {
		c.Encryption.Key = v
	}
	if v := os.Getenv("SW_MUSIC_PATH"); v != "" {
		c.Music.LibraryPath = v
	}
	if v := os.Getenv("SW_SCANNER_EXCLUSIONS"); v != "" {
		c.Scanner.Exclusions = strings.Split(v, ",")
		for i := range c.Scanner.Exclusions {
			c.Scanner.Exclusions[i] = strings.TrimSpace(c.Scanner.Exclusions[i])
		}
	}
	if v := os.Getenv("SW_BACKUP_PATH"); v != "" {
		c.Backup.Path = v
	}
	if v := os.Getenv("SW_BACKUP_RETENTION"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Backup.RetentionCount = n
		}
	}
	if v := os.Getenv("SW_BACKUP_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Backup.IntervalHours = n
		}
	}
	if v, ok := os.LookupEnv("SW_BACKUP_ENABLED"); ok {
		c.Backup.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("SW_LOG_LEVEL"); v != "" {
		c.Logging.Level = v
	}
	if v := os.Getenv("SW_LOG_FORMAT"); v != "" {
		c.Logging.Format = v
	}
	// TLS, HTTP redirect, and HTTP/3 settings. Only SW_TLS_CERT_FILE/KEY_FILE/
	// PORT have behavior today; the remaining branches keep the env-var
	// surface stable so the rest of the milestone only adds behavior, not new
	// env knobs.
	if v := os.Getenv("SW_TLS_CERT_FILE"); v != "" {
		c.Server.TLS.CertFile = v
	}
	if v := os.Getenv("SW_TLS_KEY_FILE"); v != "" {
		c.Server.TLS.KeyFile = v
	}
	if v := os.Getenv("SW_TLS_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid SW_TLS_PORT %q: %w", v, err)
		}
		c.Server.TLS.Port = port
	}
	if v := os.Getenv("SW_HTTP_REDIRECT_PORT"); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid SW_HTTP_REDIRECT_PORT %q: %w", v, err)
		}
		c.Server.HTTPRedirect.Port = port
	}
	// Treat empty SW_HTTP3_ENABLED as "do not override"; the rest of the
	// loader follows that convention so a `t.Setenv(key, "")` test helper
	// (or a deliberately-blanked deploy var) does not silently turn the
	// feature off.
	if v := os.Getenv("SW_HTTP3_ENABLED"); v != "" {
		c.Server.HTTP3.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("SW_HTTP3_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			c.Server.HTTP3.Port = port
		}
	}
	// ACME stubs: populated for completeness (and so the env-reference codegen
	// emits stable rows), but no consumer reads them yet.
	if v := os.Getenv("SW_ACME_DOMAIN"); v != "" {
		c.ACME.Domain = v
	}
	if v := os.Getenv("SW_ACME_EMAIL"); v != "" {
		c.ACME.Email = v
	}
	if v := os.Getenv("SW_ACME_CA"); v != "" {
		c.ACME.CA = v
	}
	if v := os.Getenv("SW_ACME_EAB_KEY_ID"); v != "" {
		c.ACME.EabKeyID = v
	}
	if v := os.Getenv("SW_ACME_EAB_MAC_KEY"); v != "" {
		c.ACME.EabMacKey = v
	}
	if v := os.Getenv("SW_ACME_IP"); v != "" {
		c.ACME.IP = v
	}
	if v := os.Getenv("SW_ACME_CACHE_DIR"); v != "" {
		c.ACME.CacheDir = v
	}
	return nil
}

// validate enforces required fields and normalizes a few values that the
// rest of the codebase assumes are well-formed (e.g. trimming a trailing
// slash from BasePath so route registration is unambiguous).
func (c *Config) validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("invalid port: %d", c.Server.Port)
	}
	if c.Database.Path == "" {
		return fmt.Errorf("database path is required")
	}
	c.Server.BasePath = strings.TrimRight(c.Server.BasePath, "/")

	// TLS cert/key must be set as a pair. A half-configured pair would let
	// the binary boot with surprising semantics (e.g. cert path ignored
	// because key is missing); reject loudly instead. Filesystem checks on
	// the paths happen at listener-startup time so config loading stays
	// pure -- a missing or malformed cert surfaces as a startup error
	// when the listener actually tries to bind.
	tlsCertSet := c.Server.TLS.CertFile != ""
	tlsKeySet := c.Server.TLS.KeyFile != ""
	if tlsCertSet != tlsKeySet {
		return fmt.Errorf("TLS cert and key must both be set or both be empty (cert=%q, key=%q)",
			c.Server.TLS.CertFile, c.Server.TLS.KeyFile)
	}

	// 0 is the "unset" sentinel for optional ports; any non-zero value must
	// be a valid TCP port. Reject invalid values rather than silently
	// clamping.
	if c.Server.TLS.Port != 0 && (c.Server.TLS.Port < 1 || c.Server.TLS.Port > 65535) {
		return fmt.Errorf("invalid TLS port: %d", c.Server.TLS.Port)
	}
	if c.Server.HTTPRedirect.Port != 0 && (c.Server.HTTPRedirect.Port < 1 || c.Server.HTTPRedirect.Port > 65535) {
		return fmt.Errorf("invalid HTTP redirect port: %d", c.Server.HTTPRedirect.Port)
	}

	// Resolve the effective TLS port for collision checks: when TLS is
	// configured but TLS.Port is unset, HTTPS reuses Server.Port (the
	// "collapse" mode documented in the M47 plan).
	effectiveTLSPort := c.Server.TLS.Port
	if tlsCertSet && effectiveTLSPort == 0 {
		effectiveTLSPort = c.Server.Port
	}

	// Cross-port collision: the HTTPS listener and the HTTP redirect
	// listener cannot share a port. The plain Server.Port collision is
	// implicit -- when TLS.Port is unset, the TLS listener replaces (does
	// not coexist with) the plain HTTP server on Server.Port, so the
	// redirect listener bound to the same Server.Port would conflict.
	redirectPort := c.Server.HTTPRedirect.Port
	if effectiveTLSPort != 0 && redirectPort != 0 && effectiveTLSPort == redirectPort {
		return fmt.Errorf("TLS port and HTTP redirect port must differ (both=%d)", effectiveTLSPort)
	}

	// The redirect listener only makes sense when there is an HTTPS listener
	// to redirect to. Refuse to start with the misconfiguration rather than
	// quietly skipping the redirect (which would let a deploy that thought it
	// was redirecting silently keep serving plain HTTP only on Server.Port).
	if redirectPort != 0 && !tlsCertSet {
		return fmt.Errorf("HTTP redirect port requires TLS to be configured (set SW_TLS_CERT_FILE and SW_TLS_KEY_FILE)")
	}

	// HTTP/3 prerequisites: requires TLS (HTTP/3 mandates TLS 1.3) and a
	// valid optional port.
	if c.Server.HTTP3.Enabled && !tlsCertSet {
		return fmt.Errorf("HTTP/3 requires TLS to be configured (set SW_TLS_CERT_FILE and SW_TLS_KEY_FILE)")
	}
	if c.Server.HTTP3.Port != 0 && (c.Server.HTTP3.Port < 1 || c.Server.HTTP3.Port > 65535) {
		return fmt.Errorf("invalid HTTP/3 port: %d", c.Server.HTTP3.Port)
	}

	return nil
}
