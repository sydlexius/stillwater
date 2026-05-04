package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds all application configuration.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Database   DatabaseConfig   `yaml:"database"`
	Auth       AuthConfig       `yaml:"auth"`
	Encryption EncryptionConfig `yaml:"encryption"`
	Music      MusicConfig      `yaml:"music"`
	Scanner    ScannerConfig    `yaml:"scanner"`
	Backup     BackupConfig     `yaml:"backup"`
	Logging    LoggingConfig    `yaml:"logging"`
}

// ServerConfig holds HTTP server settings.
//
// Struct tags drive the env-var reference codegen in cmd/gen-env-reference:
//   - env:  the environment variable name (SW_*)
//   - desc: a concise user-facing description (one or two sentences); a second
//     sentence is acceptable when it captures a constraint or runtime caveat
//   - default: rendered default; "unset" or "" when there is no default
type ServerConfig struct {
	Port            int    `yaml:"port" env:"SW_PORT" default:"1973" desc:"TCP port the HTTP server listens on. Numeric values outside 1-65535 are rejected at startup."`
	BasePath        string `yaml:"base_path" env:"SW_BASE_PATH" default:"/" desc:"URL prefix for subfolder reverse-proxy deployments (for example /stillwater). When set from the environment the Settings UI marks the field read-only."`
	BasePathFromEnv bool   `yaml:"-"`
}

// DatabaseConfig holds SQLite settings.
type DatabaseConfig struct {
	Path string `yaml:"path" env:"SW_DB_PATH" default:"/config/stillwater.db" desc:"Filesystem path to the SQLite database file."`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	SessionSecret string `yaml:"session_secret" env:"SW_SESSION_SECRET" default:"unset" desc:"Long random string used to sign session cookies. When unset Stillwater generates one on first run and persists it in the config directory."` //nolint:gosec // G117: not a hardcoded secret, this is a config field
}

// EncryptionConfig holds encryption key settings.
type EncryptionConfig struct {
	Key string `yaml:"key" env:"SW_ENCRYPTION_KEY" default:"unset" desc:"Key used to encrypt provider API keys at rest. When unset Stillwater generates one on first run and persists it in the config directory."`
}

// MusicConfig holds music library path settings.
type MusicConfig struct {
	LibraryPath string `yaml:"library_path" env:"SW_MUSIC_PATH" default:"/music" desc:"Default music library path used as a starting point when no library has been added through the UI."`
}

// ScannerConfig holds scanner behavior settings.
type ScannerConfig struct {
	Depth      int      `yaml:"depth"`
	Exclusions []string `yaml:"exclusions" env:"SW_SCANNER_EXCLUSIONS" default:"Various Artists, Various, VA, Soundtrack, OST" desc:"Comma-separated artist directory names the scanner skips. Whitespace around each token is trimmed."`
}

// BackupConfig holds database backup settings.
type BackupConfig struct {
	Path           string `yaml:"path" env:"SW_BACKUP_PATH" default:"" desc:"Override the directory where automated database backups are written. When empty Stillwater writes to a backups/ subfolder of the config directory."`
	RetentionCount int    `yaml:"retention_count" env:"SW_BACKUP_RETENTION" default:"7" desc:"Number of recent backups to keep. Must be a positive integer; non-positive or non-numeric values are silently ignored."`
	IntervalHours  int    `yaml:"interval_hours" env:"SW_BACKUP_INTERVAL" default:"24" desc:"Hours between automated backups. Must be a positive integer; non-positive or non-numeric values are silently ignored."`
	Enabled        bool   `yaml:"enabled" env:"SW_BACKUP_ENABLED" default:"true" desc:"Set to true or 1 to enable automated backups. Any other value disables them."`
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	Level  string `yaml:"level" env:"SW_LOG_LEVEL" default:"info" desc:"Log level at startup. One of trace, debug, info, warn, error. The runtime can also adjust the live level from the Logs settings tab."`
	Format string `yaml:"format" env:"SW_LOG_FORMAT" default:"json" desc:"Log output format. Use json for log aggregators or text for friendlier console output."`
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

// Load reads config from a YAML file (if it exists) and overrides with
// environment variables. Environment variables take precedence.
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
	return yaml.Unmarshal(data, c)
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
