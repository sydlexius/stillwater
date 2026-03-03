package connection

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Supported connection types.
const (
	TypeEmby     = "emby"
	TypeJellyfin = "jellyfin"
	TypeLidarr   = "lidarr"
)

// Connection represents an external service connection.
type Connection struct {
	ID                   string     `json:"id"`
	Name                 string     `json:"name"`
	Type                 string     `json:"type"`
	URL                  string     `json:"url"`
	APIKey               string     `json:"api_key,omitempty"`
	Enabled              bool       `json:"enabled"`
	Status               string     `json:"status"`
	StatusMessage        string     `json:"status_message,omitempty"`
	LastCheckedAt        *time.Time `json:"last_checked_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	FeatureLibraryImport bool       `json:"feature_library_import"`
	FeatureNFOWrite      bool       `json:"feature_nfo_write"`
	FeatureImageWrite    bool       `json:"feature_image_write"`
}

// ValidateBaseURL checks that a base URL is safe for use as an HTTP client target.
// It enforces http/https scheme, rejects embedded credentials (userinfo), and
// rejects query strings and fragments. Returns the cleaned URL (scheme lowercased,
// trailing slash stripped) or an error.
func ValidateBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("url is required")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("url is not valid: %w", err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}

	if u.User != nil {
		return "", fmt.Errorf("url must not contain embedded credentials")
	}

	if u.Host == "" {
		return "", fmt.Errorf("url must contain a host")
	}

	if u.RawQuery != "" {
		return "", fmt.Errorf("base url must not contain a query string")
	}

	if u.Fragment != "" {
		return "", fmt.Errorf("base url must not contain a fragment")
	}

	u.Scheme = scheme
	cleaned := strings.TrimRight(u.String(), "/")
	return cleaned, nil
}

// Validate checks required fields and constraints.
func (c *Connection) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !isValidType(c.Type) {
		return fmt.Errorf("type must be one of: emby, jellyfin, lidarr")
	}
	cleaned, err := ValidateBaseURL(c.URL)
	if err != nil {
		return err
	}
	c.URL = cleaned
	if c.APIKey == "" {
		return fmt.Errorf("api_key is required")
	}
	return nil
}

func isValidType(t string) bool {
	return t == TypeEmby || t == TypeJellyfin || t == TypeLidarr
}
