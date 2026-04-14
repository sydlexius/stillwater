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
	ID                    string     `json:"id"`
	Name                  string     `json:"name"`
	Type                  string     `json:"type"`
	URL                   string     `json:"url"`
	APIKey                string     `json:"api_key,omitempty"`
	Enabled               bool       `json:"enabled"`
	Status                string     `json:"status"`
	StatusMessage         string     `json:"status_message,omitempty"`
	LastCheckedAt         *time.Time `json:"last_checked_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
	FeatureLibraryImport  bool       `json:"feature_library_import"`
	FeatureNFOWrite       bool       `json:"feature_nfo_write"`
	FeatureImageWrite     bool       `json:"feature_image_write"`
	FeatureMetadataPush   bool       `json:"feature_metadata_push"`
	FeatureTriggerRefresh bool       `json:"feature_trigger_refresh"`
	PlatformUserID        string     `json:"platform_user_id,omitempty"`
	// PlatformServerID is the Emby/Jellyfin server identity returned by
	// /System/Info. Web deep-links must include serverId=<id> so the
	// platform client loads the correct item view; without it the URL
	// lands on a generic page or an unrelated server in multi-server
	// setups. Empty for connection types that have no server concept
	// (Lidarr) or until the first successful connection test resolves it.
	PlatformServerID string `json:"platform_server_id,omitempty"`
}

// ValidateBaseURL checks that a base URL is safe for use as an HTTP client target.
// It enforces http/https scheme, rejects embedded credentials (userinfo), and
// rejects query strings and fragments. Returns the cleaned URL (scheme lowercased,
// trailing slash stripped) or an error. The returned URL is reconstructed from
// parsed components rather than derived from the original string.
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

	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("base url must not contain a query string")
	}

	if u.Fragment != "" {
		return "", fmt.Errorf("base url must not contain a fragment")
	}

	return rebuildURL(scheme, u.Host, u.Path, u.RawPath), nil
}

// rebuildURL constructs a URL string from individual components using url.URL.
// It trims any trailing slash from the path and propagates RawPath when
// provided so that percent-encoding in the original URL is preserved.
// Building from discrete fields rather than the original input string also
// breaks taint tracking in static analysis (CodeQL go/request-forgery).
func rebuildURL(scheme, host, path, rawPath string) string {
	trimmedPath := strings.TrimRight(path, "/")
	trimmedRawPath := strings.TrimRight(rawPath, "/")

	u := url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   trimmedPath,
	}

	if trimmedRawPath != "" {
		u.RawPath = trimmedRawPath
	}

	return u.String()
}

// BuildRequestURL constructs a full request URL from a validated base URL and
// an API path. It parses both components independently and builds the result
// from a url.URL struct literal, taking scheme and host only from the base URL
// so that the path cannot override the request target. This also breaks taint
// tracking in static analysis tools (CodeQL go/request-forgery).
func BuildRequestURL(baseURL, path string) string {
	if baseURL == "" {
		return path
	}

	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return baseURL + path
	}

	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	rel, err := url.Parse(path)
	if err != nil {
		return baseURL + path
	}

	result := url.URL{
		Scheme:     base.Scheme,
		Host:       base.Host,
		Path:       base.Path + rel.Path,
		RawQuery:   rel.RawQuery,
		ForceQuery: rel.ForceQuery,
	}

	if base.RawPath != "" || rel.RawPath != "" {
		bRaw := base.RawPath
		if bRaw == "" {
			bRaw = base.Path
		}
		rRaw := rel.RawPath
		if rRaw == "" {
			rRaw = rel.Path
		}
		if raw := bRaw + rRaw; raw != result.Path {
			result.RawPath = raw
		}
	}

	return result.String()
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
