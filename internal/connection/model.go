package connection

import (
	"encoding/json"
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

// LidarrConfig holds the fields that are only meaningful for a Lidarr
// connection. Keeping them on a dedicated sub-struct (rather than flat on
// Connection) makes invalid combinations - e.g. VerifyPathAfterUpdate set on
// an Emby connection - unrepresentable in the type system instead of merely
// discouraged by a doc comment (#1686).
type LidarrConfig struct {
	// VerifyPathAfterUpdate enables a follow-up GET after the
	// UpdateArtistPath PUT that confirms the returned path field matches
	// what Stillwater sent. Mismatch produces an error with "sent X, got Y"
	// context so the operator can identify that Lidarr coerced the path
	// against its Root Folder list. Default false (opt-in): a healthy Lidarr
	// rarely drifts and the extra request roughly doubles the per-rename
	// HTTP cost.
	VerifyPathAfterUpdate bool `json:"verify_path_after_update,omitempty"`
	// PathMappings translates the artist-directory path prefixes Stillwater
	// sees on disk into the prefixes the Lidarr peer expects for the same
	// location (see Connection.MapArtistPath). Empty (the default) sends the
	// path verbatim - correct for shared-mount deployments where Stillwater's
	// container and Lidarr address the library identically. Operators on split
	// mounts enter the pair(s) so a rename/merge propagates a path Lidarr can
	// resolve instead of one it rejects or silently coerces against its Root
	// Folder list (#2303).
	PathMappings []PathMapping `json:"path_mappings,omitempty"`
}

// PathMapping translates one host-filesystem path prefix (as Stillwater sees
// the artist directory) to the prefix the platform expects for the same
// location. A peer such as Lidarr may mount the shared library under a
// different path than Stillwater's container - e.g. Stillwater sees
// "/music/Artist" while Lidarr addresses it as "/data/Artist" - so sending the
// host path verbatim breaks the platform-side rename. #2303.
type PathMapping struct {
	// HostPrefix is the leading path segment as Stillwater sees it on disk.
	HostPrefix string `json:"host_prefix"`
	// PlatformPrefix is the corresponding leading segment on the platform.
	PlatformPrefix string `json:"platform_prefix"`
}

// EmbyConfig holds the fields that are only meaningful for an Emby
// connection: the resolved platform identity and the per-feature write
// toggles.
type EmbyConfig struct {
	// PlatformUserID is the Emby user the API key resolves to. Empty until
	// the first successful connection test resolves it.
	PlatformUserID string `json:"platform_user_id,omitempty"`
	// PlatformServerID is the Emby server identity returned by /System/Info.
	// Web deep-links must include serverId=<id> so the platform client loads
	// the correct item view; without it the URL lands on a generic page or
	// an unrelated server in multi-server setups. Empty until the first
	// successful connection test resolves it.
	PlatformServerID      string `json:"platform_server_id,omitempty"`
	FeatureImageWrite     bool   `json:"feature_image_write,omitempty"`
	FeatureMetadataPush   bool   `json:"feature_metadata_push,omitempty"`
	FeatureTriggerRefresh bool   `json:"feature_trigger_refresh,omitempty"`
}

// JellyfinConfig holds the Jellyfin-only fields. It is structurally identical
// to EmbyConfig today but kept as a distinct type so the two platforms can
// diverge later without a second refactor (matches the ticket's spec).
type JellyfinConfig struct {
	PlatformUserID        string `json:"platform_user_id,omitempty"`
	PlatformServerID      string `json:"platform_server_id,omitempty"`
	FeatureImageWrite     bool   `json:"feature_image_write,omitempty"`
	FeatureMetadataPush   bool   `json:"feature_metadata_push,omitempty"`
	FeatureTriggerRefresh bool   `json:"feature_trigger_refresh,omitempty"`
}

// Connection represents an external service connection. Platform-specific
// state lives on exactly one of the Lidarr/Emby/Jellyfin sub-configs (the one
// matching Type); Validate enforces that invariant and lazily allocates the
// matching empty config when a caller leaves it nil. Persistence still uses
// the original flat columns (see service.go scanConnection / Create / Update):
// the sub-structs are purely the in-memory representation, so no schema
// migration is required (#1686).
type Connection struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Type          string     `json:"type"`
	URL           string     `json:"url"`
	APIKey        string     `json:"api_key,omitempty"`
	Enabled       bool       `json:"enabled"`
	Status        string     `json:"status"`
	StatusMessage string     `json:"status_message,omitempty"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	// FeatureManageServerFiles is the per-connection opt-in toggle
	// "Let Stillwater manage images and NFO files on this server". When
	// true, Stillwater has patched the peer's library options to disable
	// its NFO saver + image saver (SaveLocalMetadata=false) and snapshotted
	// the prior config into PreStillwaterConfigJSON for restore on opt-out
	// or connection delete. Default false. Kept on the main struct because
	// it is demonstrably cross-platform.
	FeatureManageServerFiles bool `json:"feature_manage_server_files"`
	// PreStillwaterConfigJSON is the JSON snapshot of the peer's library
	// options taken when FeatureManageServerFiles was flipped on. Empty
	// string when the toggle has never been flipped or has been restored.
	PreStillwaterConfigJSON string `json:"pre_stillwater_config_json,omitempty"`
	// Lidarr/Emby/Jellyfin hold the platform-specific config. Exactly one is
	// non-nil after Validate, corresponding to Type.
	Lidarr   *LidarrConfig   `json:"lidarr,omitempty"`
	Emby     *EmbyConfig     `json:"emby,omitempty"`
	Jellyfin *JellyfinConfig `json:"jellyfin,omitempty"`
}

// GetPlatformUserID returns the resolved platform user ID for an Emby or
// Jellyfin connection, or "" for Lidarr / an unresolved connection. Nil-safe:
// callers iterating a mixed-type connection list can call it unconditionally.
func (c *Connection) GetPlatformUserID() string {
	switch {
	case c.Emby != nil:
		return c.Emby.PlatformUserID
	case c.Jellyfin != nil:
		return c.Jellyfin.PlatformUserID
	default:
		return ""
	}
}

// GetPlatformServerID returns the resolved platform server ID for an Emby or
// Jellyfin connection, or "" otherwise. Nil-safe.
func (c *Connection) GetPlatformServerID() string {
	switch {
	case c.Emby != nil:
		return c.Emby.PlatformServerID
	case c.Jellyfin != nil:
		return c.Jellyfin.PlatformServerID
	default:
		return ""
	}
}

// GetVerifyPathAfterUpdate returns the Lidarr verify-after-PUT toggle, or
// false for non-Lidarr / unresolved connections. Nil-safe.
func (c *Connection) GetVerifyPathAfterUpdate() bool {
	if c.Lidarr != nil {
		return c.Lidarr.VerifyPathAfterUpdate
	}
	return false
}

// GetPathMappings returns the Lidarr host-to-platform path-mapping list, or
// nil for non-Lidarr / unresolved connections. Nil-safe.
func (c *Connection) GetPathMappings() []PathMapping {
	if c.Lidarr != nil {
		return c.Lidarr.PathMappings
	}
	return nil
}

// MapArtistPath translates a host artist path into the platform's namespace
// using the connection's Lidarr PathMappings. It applies the mapping whose
// HostPrefix is the longest path-prefix of hostPath - a separator-boundary
// match, so "/music/jazz" does not claim "/music/jazzfusion" (the same rule
// library.pathContains uses) - replacing that prefix with the corresponding
// PlatformPrefix. When no mapping matches, the Lidarr config is nil, or
// hostPath is empty, hostPath is returned unchanged, preserving the pre-#2303
// verbatim behavior for shared-mount deployments.
//
// Comparison uses forward-slash semantics because platform paths cross the
// wire in POSIX form regardless of the host OS. Nil-safe: callers holding a
// mixed-type or unresolved connection may call it unconditionally.
func (c *Connection) MapArtistPath(hostPath string) string {
	if c == nil || c.Lidarr == nil || hostPath == "" {
		return hostPath
	}
	bestLen := -1
	mapped := hostPath
	for _, m := range c.Lidarr.PathMappings {
		host := strings.TrimRight(m.HostPrefix, "/")
		if host == "" {
			continue
		}
		remainder, ok := pathRemainder(hostPath, host)
		if !ok {
			continue
		}
		if len(host) > bestLen {
			bestLen = len(host)
			mapped = strings.TrimRight(m.PlatformPrefix, "/") + remainder
		}
	}
	return mapped
}

// pathRemainder reports whether prefix is a separator-bounded path prefix of
// path (forward-slash semantics); on a match it returns the trailing portion,
// which is "" for an exact match or begins with "/" otherwise. Grafting the
// platform prefix onto this remainder reconstructs the translated path.
func pathRemainder(path, prefix string) (string, bool) {
	if path == prefix {
		return "", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		return path[len(prefix):], true
	}
	return "", false
}

// EncodePathMappings serializes a path-mapping list to the JSON stored in the
// connections.path_mappings column. An empty or nil list encodes as "" so the
// column default and a verbatim connection are indistinguishable on disk.
func EncodePathMappings(m []PathMapping) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encoding path mappings: %w", err)
	}
	return string(b), nil
}

// DecodePathMappings parses the connections.path_mappings column back into a
// list. "" (the column default / a verbatim connection) decodes to nil.
func DecodePathMappings(s string) ([]PathMapping, error) {
	if s == "" {
		return nil, nil
	}
	var m []PathMapping
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("decoding path mappings: %w", err)
	}
	return m, nil
}

// GetFeatureImageWrite reports the image-write toggle. Nil-safe.
func (c *Connection) GetFeatureImageWrite() bool {
	switch {
	case c.Emby != nil:
		return c.Emby.FeatureImageWrite
	case c.Jellyfin != nil:
		return c.Jellyfin.FeatureImageWrite
	default:
		return false
	}
}

// GetFeatureMetadataPush reports the metadata-push toggle. Nil-safe.
func (c *Connection) GetFeatureMetadataPush() bool {
	switch {
	case c.Emby != nil:
		return c.Emby.FeatureMetadataPush
	case c.Jellyfin != nil:
		return c.Jellyfin.FeatureMetadataPush
	default:
		return false
	}
}

// GetFeatureTriggerRefresh reports the trigger-refresh toggle. Nil-safe.
func (c *Connection) GetFeatureTriggerRefresh() bool {
	switch {
	case c.Emby != nil:
		return c.Emby.FeatureTriggerRefresh
	case c.Jellyfin != nil:
		return c.Jellyfin.FeatureTriggerRefresh
	default:
		return false
	}
}

// SetPlatformUserID stores the resolved platform user ID on the matching
// sub-config, allocating it if nil. No-op for connection types without a
// platform identity (Lidarr).
func (c *Connection) SetPlatformUserID(id string) {
	switch c.Type {
	case TypeEmby:
		if c.Emby == nil {
			c.Emby = &EmbyConfig{}
		}
		c.Emby.PlatformUserID = id
	case TypeJellyfin:
		if c.Jellyfin == nil {
			c.Jellyfin = &JellyfinConfig{}
		}
		c.Jellyfin.PlatformUserID = id
	}
}

// SetPlatformServerID stores the resolved platform server ID on the matching
// sub-config, allocating it if nil. No-op for Lidarr.
func (c *Connection) SetPlatformServerID(id string) {
	switch c.Type {
	case TypeEmby:
		if c.Emby == nil {
			c.Emby = &EmbyConfig{}
		}
		c.Emby.PlatformServerID = id
	case TypeJellyfin:
		if c.Jellyfin == nil {
			c.Jellyfin = &JellyfinConfig{}
		}
		c.Jellyfin.PlatformServerID = id
	}
}

// SetFeatures writes the Emby/Jellyfin write-feature toggles onto the matching
// media sub-config, allocating it if nil. No-op for Lidarr (which has no such
// features). Mirrors Service.UpdateFeatures' parameter order so callers holding
// an in-memory Connection (e.g. the update handler) set features the same way
// the targeted DB updater does.
func (c *Connection) SetFeatures(imageWrite, metadataPush, triggerRefresh bool) {
	switch c.Type {
	case TypeEmby:
		if c.Emby == nil {
			c.Emby = &EmbyConfig{}
		}
		c.Emby.FeatureImageWrite = imageWrite
		c.Emby.FeatureMetadataPush = metadataPush
		c.Emby.FeatureTriggerRefresh = triggerRefresh
	case TypeJellyfin:
		if c.Jellyfin == nil {
			c.Jellyfin = &JellyfinConfig{}
		}
		c.Jellyfin.FeatureImageWrite = imageWrite
		c.Jellyfin.FeatureMetadataPush = metadataPush
		c.Jellyfin.FeatureTriggerRefresh = triggerRefresh
	}
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
	if err := c.normalizeConfig(); err != nil {
		return err
	}
	return nil
}

// normalizeConfig enforces the type-discriminated config invariant: exactly
// one of Lidarr/Emby/Jellyfin is non-nil and corresponds to Type. A sub-config
// belonging to a different platform is rejected (that is the invalid state the
// type system now makes loud); the matching config is lazily allocated when
// the caller left it nil so construction sites that set no platform-specific
// fields (e.g. a fresh Lidarr connection or the federated-login auto-provision
// path) still validate. Type is already known valid by the time this runs.
func (c *Connection) normalizeConfig() error {
	switch c.Type {
	case TypeLidarr:
		if c.Emby != nil || c.Jellyfin != nil {
			return fmt.Errorf("lidarr connection must not carry emby or jellyfin config")
		}
		if c.Lidarr == nil {
			c.Lidarr = &LidarrConfig{}
		}
	case TypeEmby:
		if c.Lidarr != nil || c.Jellyfin != nil {
			return fmt.Errorf("emby connection must not carry lidarr or jellyfin config")
		}
		if c.Emby == nil {
			c.Emby = &EmbyConfig{}
		}
	case TypeJellyfin:
		if c.Lidarr != nil || c.Emby != nil {
			return fmt.Errorf("jellyfin connection must not carry lidarr or emby config")
		}
		if c.Jellyfin == nil {
			c.Jellyfin = &JellyfinConfig{}
		}
	}
	return nil
}

func isValidType(t string) bool {
	return t == TypeEmby || t == TypeJellyfin || t == TypeLidarr
}
