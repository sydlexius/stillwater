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
	Enabled bool `yaml:"enabled" toml:"enabled" env:"SW_HTTP3_ENABLED" default:"false" desc:"Set to true or 1 to enable an HTTP/3 (QUIC) listener over UDP. Requires direct TLS to be configured (SW_TLS_CERT_FILE and SW_TLS_KEY_FILE). The Alt-Svc header is added to HTTPS responses so HTTP/3-capable clients upgrade automatically; clients with UDP blocked fall back to HTTP/1.1+HTTP/2 over TCP."`
	Port    int  `yaml:"port" toml:"port" env:"SW_HTTP3_PORT" default:"unset" desc:"Optional dedicated UDP port for HTTP/3. When unset HTTP/3 reuses the effective HTTPS port (SW_TLS_PORT or SW_PORT). Numeric values outside 1-65535 are rejected at startup."`
}

// ACMEConfig holds Automatic Certificate Management Environment settings.
// Domain/Email/CA/CacheDir drive the autocert (Let's Encrypt / Buypass via
// golang.org/x/crypto/acme/autocert) path today. EabKeyID/EabMacKey/IP are
// reserved for the ZeroSSL IP-SAN wiring (#1564, lego); leaving them
// populated but unread keeps the env-var loader and reference docs stable
// across the milestone.
type ACMEConfig struct {
	Domain    string `yaml:"domain" toml:"domain" env:"SW_ACME_DOMAIN" default:"unset" desc:"DNS name to request certificates for via ACME (Let's Encrypt by default). Setting this turns on autocert; the domain MUST resolve to this server and port 80 MUST be reachable from the public internet."`
	Email     string `yaml:"email" toml:"email" env:"SW_ACME_EMAIL" default:"unset" desc:"Contact email registered with the ACME CA. Used for expiry notifications and account recovery; recommended but not required."`
	CA        string `yaml:"ca" toml:"ca" env:"SW_ACME_CA" default:"unset" desc:"ACME directory URL. Defaults to Let's Encrypt production. Set to https://acme-staging-v02.api.letsencrypt.org/directory for testing without burning rate-limit quota."`
	EabKeyID  string `yaml:"eab_key_id" toml:"eab_key_id" env:"SW_ACME_EAB_KEY_ID" default:"unset" desc:"Reserved for future use; not yet active. External Account Binding key identifier for ACME CAs that require it (for example ZeroSSL)."`
	EabMacKey string `yaml:"eab_mac_key" toml:"eab_mac_key" env:"SW_ACME_EAB_MAC_KEY" default:"unset" desc:"Reserved for future use; not yet active. External Account Binding HMAC key paired with SW_ACME_EAB_KEY_ID. Treat as a secret; will be persisted only after AES-256-GCM encryption when the ACME path lands."`
	IP        string `yaml:"ip" toml:"ip" env:"SW_ACME_IP" default:"unset" desc:"Reserved for future use; not yet active. Public IP address for IP-SAN certificate orders (ZeroSSL). Must not be an RFC1918, loopback, or link-local address."`
	CacheDir  string `yaml:"cache_dir" toml:"cache_dir" env:"SW_ACME_CACHE_DIR" default:"unset" desc:"Directory where ACME account keys and issued certificates are cached. Defaults to the directory containing SW_DB_PATH plus /acme-cache. Persist this across restarts to avoid hitting CA rate limits."`
}

// DatabaseConfig holds SQLite settings.
type DatabaseConfig struct {
	Path string `yaml:"path" toml:"path" env:"SW_DB_PATH" default:"/config/stillwater.db" desc:"Filesystem path to the SQLite database file."`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	SessionSecret string `yaml:"session_secret" toml:"session_secret" env:"SW_SESSION_SECRET" default:"unset" desc:"Long random string used to sign session cookies. When unset Stillwater generates one on first run and persists it in the config directory."`
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
	// MtimeFastPath, when true, lets the scanner skip the per-file image
	// stat + dimension probe loop for artist directories whose mtime has
	// not advanced since the previous scan. Defaults to true; disable it
	// (SW_SCANNER_MTIME_FAST_PATH=false) on filesystems that do not
	// maintain stable directory mtimes -- some network shares, FUSE
	// mounts, and any backup-restored tree where mtimes were not
	// preserved fall into this category.
	MtimeFastPath bool `yaml:"mtime_fast_path" toml:"mtime_fast_path" env:"SW_SCANNER_MTIME_FAST_PATH" default:"true" desc:"When true the scanner reuses cached image flags for artist directories whose mtime has not advanced since the previous scan, eliminating the per-file stat + dimension probe loop. Set to false on filesystems with unreliable mtimes (some network shares, FUSE mounts, backup-restored trees) so every scan re-probes."`
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
			// Default-on; the fast-path is a no-op on the first scan
			// (no prior LastScannedAt) and on directories that have
			// been touched since the last scan, so the only behavioral
			// difference is "second scan of an unchanged directory
			// skips its inner ReadDir + image probe loop".
			MtimeFastPath: true,
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

# Automatic Certificate Management Environment (ACME). Currently marked
# Experimental in the Settings -> General TLS Status card pending end-to-end
# validation against a real public deployment; prefer [server.tls] (BYO
# certificate) above for production until that marker is removed. The
# domain MUST resolve to this server and port 80 MUST be reachable from
# the public internet for the ACME challenge to succeed. ACME and BYO TLS
# are mutually exclusive -- set one or the other, not both.
# See: https://sydlexius.github.io/stillwater/how-to/direct-tls-setup/
# [acme]
# domain = "stillwater.example.com"
# email = "admin@example.com"
# ca = "https://acme-v02.api.letsencrypt.org/directory"  # Let's Encrypt production (default). Use the staging URL while testing to avoid rate limits.
# cache_dir = "/config/acme-cache"
# eab_key_id  = ""  # Reserved for future use (ZeroSSL / IP-SAN); not yet active.
# eab_mac_key = ""  # Reserved for future use (ZeroSSL / IP-SAN); not yet active.
# ip          = ""  # Reserved for future use (IP-SAN certificate orders); not yet active.

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
# mtime_fast_path = true  # set to false on filesystems with unreliable mtimes

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

// envBinding pairs an environment variable key with the function that applies
// its value to the config. Apply is called only when the variable is non-empty
// (Getenv semantics). Use lookupBinding for variables that need LookupEnv
// semantics (i.e. react to an explicitly-empty value).
type envBinding struct {
	Key   string
	Apply func(v string) error
}

// setString returns an Apply func that writes v into *dst.
func setString(dst *string) func(string) error {
	return func(v string) error {
		*dst = v
		return nil
	}
}

// setInt returns an Apply func that parses v as a decimal integer and writes
// the result into *dst. A parse failure is returned as a named error so the
// caller can surface which variable was malformed.
func setInt(key string, dst *int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %w", key, v, err)
		}
		*dst = n
		return nil
	}
}

// setIntPositive returns an Apply func that parses v as a decimal integer and
// writes the result into *dst only when the parsed value is positive. Non-
// positive and non-numeric values are silently ignored, matching the original
// lenient behavior for retry/interval knobs.
func setIntPositive(dst *int) func(string) error {
	return func(v string) error {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			*dst = n
		}
		return nil
	}
}

// setBool returns an Apply func that treats "true" or "1" as true and any
// other non-empty value as false.
func setBool(dst *bool) func(string) error {
	return func(v string) error {
		*dst = v == "true" || v == "1"
		return nil
	}
}

// setCSV returns an Apply func that splits v on commas, trims whitespace from
// each token, and writes the resulting slice into *dst.
func setCSV(dst *[]string) func(string) error {
	return func(v string) error {
		parts := strings.Split(v, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		*dst = parts
		return nil
	}
}

// loadFromEnv overlays environment variables onto c. Env-var values take
// precedence over any file-loaded values per the API-first contract: an
// operator can override one knob via env without rewriting the whole file.
//
// SW_BASE_PATH also sets BasePathFromEnv so the Settings UI can mark the
// field read-only when the operator has pinned it via the environment.
//
// SW_BACKUP_ENABLED uses LookupEnv semantics: a variable that is absent leaves
// the YAML default intact; present-but-empty sets Enabled = false.
func (c *Config) loadFromEnv() error {
	// Standard bindings: applied only when the variable is non-empty.
	bindings := []envBinding{
		// Server
		{Key: "SW_PORT", Apply: setInt("SW_PORT", &c.Server.Port)},
		{Key: "SW_DB_PATH", Apply: setString(&c.Database.Path)},
		// Auth / encryption
		{Key: "SW_SESSION_SECRET", Apply: setString(&c.Auth.SessionSecret)},
		{Key: "SW_ENCRYPTION_KEY", Apply: setString(&c.Encryption.Key)},
		// Music
		{Key: "SW_MUSIC_PATH", Apply: setString(&c.Music.LibraryPath)},
		{Key: "SW_SCANNER_EXCLUSIONS", Apply: setCSV(&c.Scanner.Exclusions)},
		// SW_SCANNER_MTIME_FAST_PATH uses standard non-empty semantics so
		// an unset variable leaves the default-on behavior intact; only
		// an explicit "false" / "0" disables the fast path.
		{Key: "SW_SCANNER_MTIME_FAST_PATH", Apply: setBool(&c.Scanner.MtimeFastPath)},
		// Backup (lenient int; non-positive values are silently ignored)
		{Key: "SW_BACKUP_PATH", Apply: setString(&c.Backup.Path)},
		{Key: "SW_BACKUP_RETENTION", Apply: setIntPositive(&c.Backup.RetentionCount)},
		{Key: "SW_BACKUP_INTERVAL", Apply: setIntPositive(&c.Backup.IntervalHours)},
		// Logging
		{Key: "SW_LOG_LEVEL", Apply: setString(&c.Logging.Level)},
		{Key: "SW_LOG_FORMAT", Apply: setString(&c.Logging.Format)},
		// TLS -- SW_TLS_CERT_FILE/KEY_FILE/PORT have behavior today; the
		// remaining entries keep the env-var surface stable so future
		// milestones only add behavior, not new env knobs.
		{Key: "SW_TLS_CERT_FILE", Apply: setString(&c.Server.TLS.CertFile)},
		{Key: "SW_TLS_KEY_FILE", Apply: setString(&c.Server.TLS.KeyFile)},
		{Key: "SW_TLS_PORT", Apply: setInt("SW_TLS_PORT", &c.Server.TLS.Port)},
		{Key: "SW_HTTP_REDIRECT_PORT", Apply: setInt("SW_HTTP_REDIRECT_PORT", &c.Server.HTTPRedirect.Port)},
		// HTTP/3: empty value means "do not override" (same convention as the
		// rest of the loader; a deliberately-blanked deploy var must not turn
		// the feature off).
		{Key: "SW_HTTP3_ENABLED", Apply: setBool(&c.Server.HTTP3.Enabled)},
		{Key: "SW_HTTP3_PORT", Apply: setInt("SW_HTTP3_PORT", &c.Server.HTTP3.Port)},
		// ACME stubs: populated for completeness so env-reference codegen
		// emits stable rows, but no consumer reads them yet.
		{Key: "SW_ACME_DOMAIN", Apply: setString(&c.ACME.Domain)},
		{Key: "SW_ACME_EMAIL", Apply: setString(&c.ACME.Email)},
		{Key: "SW_ACME_CA", Apply: setString(&c.ACME.CA)},
		{Key: "SW_ACME_EAB_KEY_ID", Apply: setString(&c.ACME.EabKeyID)},
		{Key: "SW_ACME_EAB_MAC_KEY", Apply: setString(&c.ACME.EabMacKey)},
		{Key: "SW_ACME_IP", Apply: setString(&c.ACME.IP)},
		{Key: "SW_ACME_CACHE_DIR", Apply: setString(&c.ACME.CacheDir)},
	}
	for _, b := range bindings {
		if v := os.Getenv(b.Key); v != "" {
			if err := b.Apply(v); err != nil {
				return err
			}
		}
	}

	// SW_BASE_PATH: non-empty value also marks the field as env-pinned.
	if v := os.Getenv("SW_BASE_PATH"); v != "" {
		c.Server.BasePath = v
		c.Server.BasePathFromEnv = true
	}

	// SW_BACKUP_ENABLED: LookupEnv so an unset variable leaves the YAML default
	// intact; a present-but-empty value sets Enabled = false (original behavior).
	if v, ok := os.LookupEnv("SW_BACKUP_ENABLED"); ok {
		c.Backup.Enabled = v == "true" || v == "1"
	}

	return nil
}

// validatePort returns an error when p is outside the valid TCP port range.
func validatePort(label string, p int) error {
	if p < 1 || p > 65535 {
		return fmt.Errorf("invalid %s: %d", label, p)
	}
	return nil
}

// validateOptionalPort returns an error when p is non-zero and outside the
// valid TCP port range. 0 is the "unset" sentinel and is always accepted.
func validateOptionalPort(label string, p int) error {
	if p != 0 && (p < 1 || p > 65535) {
		return fmt.Errorf("invalid %s: %d", label, p)
	}
	return nil
}

// crossFieldRules contains the ordered set of cross-field validation
// functions. Each rule is independently testable. Rules run after per-field
// validators and after BasePath normalization.
//
// Rule ordering matters for the error messages: more specific rules (cert/key
// pairing) run before derived rules (ACME exclusivity) so the first error
// reported is the most actionable.
var crossFieldRules = []func(*Config) error{
	// TLS cert/key must be configured as a pair. A half-configured pair would
	// let the binary boot with surprising semantics (cert path ignored because
	// key is missing); reject loudly instead. Filesystem checks on the paths
	// happen at listener-startup time so config loading stays pure.
	func(c *Config) error {
		certSet := c.Server.TLS.CertFile != ""
		keySet := c.Server.TLS.KeyFile != ""
		if certSet != keySet {
			return fmt.Errorf("TLS cert and key must both be set or both be empty (cert=%q, key=%q)",
				c.Server.TLS.CertFile, c.Server.TLS.KeyFile)
		}
		return nil
	},

	// ACME (autocert) is mutually exclusive with BYO TLS cert/key. The
	// listener layer would have to pick one source and silently ignore the
	// other; rejecting the combination at config time surfaces the ambiguity
	// loudly. Operators who want to migrate from BYO to ACME (or vice versa)
	// flip one set of variables, not both.
	func(c *Config) error {
		if c.ACME.Domain != "" && (c.Server.TLS.CertFile != "" || c.Server.TLS.KeyFile != "") {
			return fmt.Errorf("SW_ACME_DOMAIN is mutually exclusive with SW_TLS_CERT_FILE/SW_TLS_KEY_FILE; pick one TLS source")
		}
		return nil
	},

	// Port collision: the HTTPS listener and the HTTP redirect / ACME
	// challenge listener cannot share a port.
	//
	// effectiveTLSPort collapses to Server.Port when TLS.Port is unset. This
	// applies to both BYO TLS (cert+key) and ACME -- both modes support
	// collapse mode. Without this, an ACME deploy with SW_PORT=80 (or a
	// matching SW_HTTP_REDIRECT_PORT) would pass validation and then the
	// https-acme and acme-challenge listeners would race for the socket.
	//
	// When ACME is on, the challenge listener defaults to port 80 if
	// HTTPRedirect.Port is unset, so we surface that default for the
	// collision check too.
	func(c *Config) error {
		tlsConfigured := c.Server.TLS.CertFile != "" || c.ACME.Domain != ""
		effectiveTLSPort := c.Server.TLS.Port
		if tlsConfigured && effectiveTLSPort == 0 {
			effectiveTLSPort = c.Server.Port
		}
		redirectPort := c.Server.HTTPRedirect.Port
		if c.ACME.Domain != "" && redirectPort == 0 {
			redirectPort = 80
		}
		if effectiveTLSPort != 0 && redirectPort != 0 && effectiveTLSPort == redirectPort {
			return fmt.Errorf("TLS port and HTTP redirect / ACME challenge port must differ (both=%d)", effectiveTLSPort)
		}
		return nil
	},

	// The redirect listener only makes sense when there is an HTTPS listener
	// to redirect to. Refuse to start with the misconfiguration rather than
	// quietly skipping the redirect (which would silently leave the deploy on
	// plain HTTP). Either BYO cert or ACME counts as "TLS configured".
	func(c *Config) error {
		redirectPort := c.Server.HTTPRedirect.Port
		if c.ACME.Domain != "" && redirectPort == 0 {
			redirectPort = 80
		}
		tlsConfigured := c.Server.TLS.CertFile != "" || c.ACME.Domain != ""
		if redirectPort != 0 && !tlsConfigured {
			return fmt.Errorf("HTTP redirect port requires TLS to be configured (set SW_TLS_CERT_FILE and SW_TLS_KEY_FILE, or SW_ACME_DOMAIN)")
		}
		return nil
	},

	// HTTP/3 requires TLS (HTTP/3 mandates TLS 1.3). BYO cert must be
	// configured; ACME is not yet wired to the HTTP/3 listener.
	func(c *Config) error {
		if c.Server.HTTP3.Enabled && c.Server.TLS.CertFile == "" {
			return fmt.Errorf("HTTP/3 requires TLS to be configured (set SW_TLS_CERT_FILE and SW_TLS_KEY_FILE)")
		}
		return nil
	},
}

// validate enforces required fields, normalizes values that the rest of the
// codebase assumes are well-formed (e.g. trimming a trailing slash from
// BasePath), and runs a set of cross-field rules.
func (c *Config) validate() error {
	// Per-field validators.
	if err := validatePort("port", c.Server.Port); err != nil {
		return err
	}
	if c.Database.Path == "" {
		return fmt.Errorf("database path is required")
	}
	if err := validateOptionalPort("TLS port", c.Server.TLS.Port); err != nil {
		return err
	}
	if err := validateOptionalPort("HTTP redirect port", c.Server.HTTPRedirect.Port); err != nil {
		return err
	}
	if err := validateOptionalPort("HTTP/3 port", c.Server.HTTP3.Port); err != nil {
		return err
	}

	// Normalize BasePath: strip trailing slash so route registration is
	// unambiguous (e.g. /app/ becomes /app).
	c.Server.BasePath = strings.TrimRight(c.Server.BasePath, "/")

	// Cross-field rules.
	for _, rule := range crossFieldRules {
		if err := rule(c); err != nil {
			return err
		}
	}
	return nil
}
