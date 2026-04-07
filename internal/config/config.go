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

// TLSConfig holds TLS/HTTPS settings.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"` // SW_TLS_CERT_FILE
	KeyFile  string `yaml:"key_file"`  // SW_TLS_KEY_FILE
	Port     int    `yaml:"port"`      // SW_TLS_PORT (default: same as Server.Port)
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port     int       `yaml:"port"`      // SW_PORT
	BasePath string    `yaml:"base_path"` // SW_BASE_PATH
	TLS      TLSConfig `yaml:"tls"`
}

// DatabaseConfig holds SQLite settings.
type DatabaseConfig struct {
	Path string `yaml:"path"` // SW_DB_PATH
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	SessionSecret string `yaml:"session_secret"` //nolint:gosec // G117: not a hardcoded secret, this is a config field -- SW_SESSION_SECRET
}

// EncryptionConfig holds encryption key settings.
type EncryptionConfig struct {
	Key string `yaml:"key"` // SW_ENCRYPTION_KEY
}

// MusicConfig holds music library path settings.
type MusicConfig struct {
	LibraryPath string `yaml:"library_path"` // SW_MUSIC_PATH
}

// ScannerConfig holds scanner behavior settings.
type ScannerConfig struct {
	Depth      int      `yaml:"depth"`
	Exclusions []string `yaml:"exclusions"` // SW_SCANNER_EXCLUSIONS (comma-separated)
}

// BackupConfig holds database backup settings.
type BackupConfig struct {
	Path           string `yaml:"path"`            // SW_BACKUP_PATH
	RetentionCount int    `yaml:"retention_count"` // SW_BACKUP_RETENTION
	IntervalHours  int    `yaml:"interval_hours"`  // SW_BACKUP_INTERVAL
	Enabled        bool   `yaml:"enabled"`         // SW_BACKUP_ENABLED ("true" or "1")
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	Level  string `yaml:"level"`  // SW_LOG_LEVEL
	Format string `yaml:"format"` // SW_LOG_FORMAT
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Port:     1973,
			BasePath: "/",
		},
		Database: DatabaseConfig{
			Path: "/data/stillwater.db",
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
	if v := os.Getenv("SW_BACKUP_ENABLED"); v != "" {
		c.Backup.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("SW_LOG_LEVEL"); v != "" {
		c.Logging.Level = v
	}
	if v := os.Getenv("SW_LOG_FORMAT"); v != "" {
		c.Logging.Format = v
	}
	if v := os.Getenv("SW_TLS_CERT_FILE"); v != "" {
		c.Server.TLS.CertFile = v
	}
	if v := os.Getenv("SW_TLS_KEY_FILE"); v != "" {
		c.Server.TLS.KeyFile = v
	}
	if v := os.Getenv("SW_TLS_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			c.Server.TLS.Port = port
		}
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

	// TLS validation: cert and key must both be set or neither.
	certSet := c.Server.TLS.CertFile != ""
	keySet := c.Server.TLS.KeyFile != ""
	if certSet != keySet {
		return fmt.Errorf("server.tls.cert_file and server.tls.key_file must both be set or both be empty")
	}

	// Default TLS port to the plain HTTP port when not explicitly set.
	// Only validate the TLS port range when TLS is actually enabled.
	if c.Server.TLS.Port == 0 {
		c.Server.TLS.Port = c.Server.Port
	}
	if certSet && keySet && (c.Server.TLS.Port < 1 || c.Server.TLS.Port > 65535) {
		return fmt.Errorf("invalid TLS port: %d", c.Server.TLS.Port)
	}

	return nil
}

// TLSEnabled reports whether TLS is configured (both cert and key are set).
func (c *Config) TLSEnabled() bool {
	return c.Server.TLS.CertFile != "" && c.Server.TLS.KeyFile != ""
}
