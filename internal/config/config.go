package config

import (
	"fmt"
	"net"
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
	TLS        TLSConfig        `yaml:"tls"`
	ACME       ACMEConfig       `yaml:"acme"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port     int    `yaml:"port"`      // SW_PORT
	BasePath string `yaml:"base_path"` // SW_BASE_PATH
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

// TLSConfig holds manual TLS certificate settings.
// When SW_TLS_CERT_FILE and SW_TLS_KEY_FILE are set, the server uses those
// files for TLS. These are ignored when ACME is active.
type TLSConfig struct {
	CertFile string `yaml:"cert_file"` // SW_TLS_CERT_FILE
	KeyFile  string `yaml:"key_file"`  // SW_TLS_KEY_FILE
}

// ACMEConfig holds ACME automatic certificate management settings.
//
// Supported CAs:
//   - "letsencrypt" (default): https://acme-v02.api.letsencrypt.org/directory
//   - "zerossl": https://acme.zerossl.com/v2/DV90  (requires EAB credentials)
//   - any other value: treated as a custom ACME directory URL
//
// Domain-based certs (SW_ACME_DOMAIN) use the autocert package and HTTP-01
// challenge. IP-based certs (SW_ACME_IP) use the raw ACME client with an
// IP SAN; only ZeroSSL issues IP address certificates.
type ACMEConfig struct {
	CA        string `yaml:"ca"`          // SW_ACME_CA: "letsencrypt" | "zerossl" | custom URL
	Domain    string `yaml:"domain"`      // SW_ACME_DOMAIN
	Email     string `yaml:"email"`       // SW_ACME_EMAIL
	CacheDir  string `yaml:"cache_dir"`   // SW_ACME_CACHE_DIR (default /data/acme-cache)
	EABKeyID  string `yaml:"eab_key_id"`  // SW_ACME_EAB_KEY_ID  (ZeroSSL EAB key identifier)
	EABMACKey string `yaml:"eab_mac_key"` // SW_ACME_EAB_MAC_KEY (ZeroSSL EAB MAC key, Base64URL)
	IP        string `yaml:"ip"`          // SW_ACME_IP (public IP address for IP SAN cert)
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
		ACME: ACMEConfig{
			CA:       "letsencrypt",
			CacheDir: "/data/acme-cache",
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
		c.TLS.CertFile = v
	}
	if v := os.Getenv("SW_TLS_KEY_FILE"); v != "" {
		c.TLS.KeyFile = v
	}
	if v := os.Getenv("SW_ACME_CA"); v != "" {
		c.ACME.CA = v
	}
	if v := os.Getenv("SW_ACME_DOMAIN"); v != "" {
		c.ACME.Domain = v
	}
	if v := os.Getenv("SW_ACME_EMAIL"); v != "" {
		c.ACME.Email = v
	}
	if v := os.Getenv("SW_ACME_CACHE_DIR"); v != "" {
		c.ACME.CacheDir = v
	}
	if v := os.Getenv("SW_ACME_EAB_KEY_ID"); v != "" {
		c.ACME.EABKeyID = v
	}
	if v := os.Getenv("SW_ACME_EAB_MAC_KEY"); v != "" {
		c.ACME.EABMACKey = v
	}
	if v := os.Getenv("SW_ACME_IP"); v != "" {
		c.ACME.IP = v
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
	if err := c.validateACME(); err != nil {
		return fmt.Errorf("ACME config: %w", err)
	}
	return nil
}

// privateIPNets contains RFC1918 and other non-routable IPv4/IPv6 ranges that
// must not be used as a public ACME IP target.
var privateIPNets = func() []*net.IPNet {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 ULA (fd00::/8 is a subset)
		"fe80::/10",      // IPv6 link-local
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("config: invalid private CIDR " + c + ": " + err.Error())
		}
		nets = append(nets, n)
	}
	return nets
}()

// IsPrivateIP reports whether ip falls within a non-routable address range
// (RFC1918, loopback, link-local, or IPv6 ULA).
func IsPrivateIP(ip net.IP) bool {
	for _, n := range privateIPNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (c *Config) validateACME() error {
	// Nothing to validate if ACME is not in use.
	if c.ACME.Domain == "" && c.ACME.IP == "" {
		return nil
	}

	if c.ACME.IP != "" {
		ip := net.ParseIP(c.ACME.IP)
		if ip == nil {
			return fmt.Errorf("SW_ACME_IP %q is not a valid IP address", c.ACME.IP)
		}
		if IsPrivateIP(ip) {
			return fmt.Errorf("SW_ACME_IP %q is a private/reserved address; a publicly reachable IP is required", c.ACME.IP)
		}

		// Let's Encrypt does not issue IP certificates; a capable CA is required.
		if c.ACME.CA == "letsencrypt" || c.ACME.CA == "" {
			return fmt.Errorf("IP certificates require SW_ACME_CA=zerossl (or a custom CA URL); Let's Encrypt does not issue IP address certificates")
		}
	}

	// ZeroSSL requires EAB credentials.
	if c.ACME.CA == "zerossl" {
		if c.ACME.EABKeyID == "" || c.ACME.EABMACKey == "" {
			return fmt.Errorf("SW_ACME_CA=zerossl requires both SW_ACME_EAB_KEY_ID and SW_ACME_EAB_MAC_KEY")
		}
	}

	return nil
}
