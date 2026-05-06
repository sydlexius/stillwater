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
}

// ServerConfig holds HTTP server settings.
//
// Struct tags drive the env-var reference codegen in cmd/gen-env-reference:
//   - env:  the environment variable name (SW_*)
//   - desc: a concise user-facing description (one or two sentences); a second
//     sentence is acceptable when it captures a constraint or runtime caveat
//   - default: rendered default; "unset" or "" when there is no default
type ServerConfig struct {
	Port            int    `yaml:"port" toml:"port" env:"SW_PORT" default:"1973" desc:"TCP port the HTTP server listens on. Numeric values outside 1-65535 are rejected at startup."`
	BasePath        string `yaml:"base_path" toml:"base_path" env:"SW_BASE_PATH" default:"/" desc:"URL prefix for subfolder reverse-proxy deployments (for example /stillwater). When set from the environment the Settings UI marks the field read-only."`
	BasePathFromEnv bool   `yaml:"-" toml:"-"`
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
	// Atomic create-only: O_CREATE|O_EXCL avoids the TOCTOU window between
	// a separate Stat and Write, and converges concurrent first-runs on a
	// single scaffold (the loser sees ErrExist, treated as a no-op).
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G306: config file is intentionally world-readable
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("writing config scaffold: %w", err)
	}
	if _, werr := f.Write([]byte(scaffoldTOML)); werr != nil {
		_ = f.Close()
		return false, fmt.Errorf("writing config scaffold: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		return false, fmt.Errorf("writing config scaffold: %w", cerr)
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

	cfg.loadFromEnv()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

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

func (c *Config) loadFromEnv() {
	if v := os.Getenv("SW_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			c.Server.Port = port
		}
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
}

func (c *Config) validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("invalid port: %d", c.Server.Port)
	}
	if c.Database.Path == "" {
		return fmt.Errorf("database path is required")
	}
	c.Server.BasePath = strings.TrimRight(c.Server.BasePath, "/")
	if c.Server.BasePath == "" {
		c.Server.BasePath = ""
	}
	return nil
}
