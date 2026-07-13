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
}

// PathMapping translates one host-filesystem path prefix (as Stillwater sees
// the artist directory) to the prefix the platform expects for the same
// location. ANY peer - Emby, Jellyfin or Lidarr - may mount the shared library
// under a different path than Stillwater's container: Stillwater sees
// "/host/music/Artist" while the peer container addresses it as
// "/music/Artist", so sending the host path verbatim pushes a path that is
// meaningless in the peer's filesystem view. Lidarr stores it verbatim,
// Jellyfin rejects it and keeps the old path (whose NFO saver then re-creates
// the merged-away directory, which the next Stillwater scan re-imports as a
// duplicate artist) - and all three report success. #2303 / #2380.
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
	// PathMappings translates the artist-directory path prefixes Stillwater
	// sees on disk into the prefixes THIS peer expects for the same location
	// (see MapArtistPath). Empty (the default) sends the path verbatim -
	// correct only for shared-mount deployments where Stillwater and the peer
	// address the library identically.
	//
	// Lives on Connection rather than on the Lidarr sub-config (where #2303
	// first put it) because the split-mount problem is not Lidarr-specific:
	// an Emby or Jellyfin container mounts the library under its own prefix
	// exactly the same way, and leaving them unmapped was the #2380
	// showstopper - Stillwater pushed a host path into their container
	// namespace and every peer reported ok while two silently did the wrong
	// thing. Persisted in the existing connections.path_mappings column for
	// every type, so promoting it needs no schema migration.
	PathMappings []PathMapping `json:"path_mappings,omitempty"`
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

// GetPathMappings returns the connection's host-to-platform path-mapping list.
// Valid for every connection type since #2380 promoted the field off the Lidarr
// sub-config. Nil-safe.
func (c *Connection) GetPathMappings() []PathMapping {
	if c == nil {
		return nil
	}
	return c.PathMappings
}

// SetPathMappings replaces the connection's path-mapping list. Valid for every
// connection type. Nil or empty clears the mappings, restoring verbatim path
// propagation (correct for a shared-mount deployment).
func (c *Connection) SetPathMappings(m []PathMapping) {
	if c == nil {
		return
	}
	c.PathMappings = m
}

// MapArtistPath translates a host artist path into the platform's namespace
// using the connection's PathMappings. It applies the mapping whose HostPrefix
// is the longest path-prefix of hostPath - a separator-boundary match, so
// "/music/jazz" does not claim "/music/jazzfusion" (the same rule
// library.pathContains uses) - replacing that prefix with the corresponding
// PlatformPrefix. When no mapping matches, the path is returned unchanged apart
// from the POSIX separator fold (see toPosixPath), preserving the verbatim
// behavior for shared-mount deployments. An empty hostPath is returned as-is.
//
// Applies to EVERY connection type. Before #2380 this short-circuited on a nil
// Lidarr sub-config, so Emby and Jellyfin were never path-mapped at all and
// received the raw host path; that verbatim push into the peers' container
// namespace is exactly the bug this fix closes. A mapping that fails to move
// the path inside a peer root is caught downstream by the pre-flight root
// guard (see publish.guardPlatformPath) rather than silently pushed.
//
// Comparison uses forward-slash semantics because platform paths cross the
// wire in POSIX form regardless of the host OS. Nil-safe: callers holding a
// mixed-type or unresolved connection may call it unconditionally.
func (c *Connection) MapArtistPath(hostPath string) string {
	if c == nil || hostPath == "" {
		return hostPath
	}
	// Fold to POSIX form FIRST, on every input, so the mapping and the
	// downstream root guard compare the same shape. On Windows the host path
	// arrives from filepath.Join/Clean as `C:\music\Artist` while a root (and
	// often the operator's own HostPrefix) is forward-slashed; without this fold
	// NO mapping can ever match, the raw backslash path is pushed, the guard
	// normalizes it to `C:/music/Artist` and finds it outside every Linux peer
	// root - so every push is permanently refused with no path-mapping value the
	// operator could enter to fix it (#2380 follow-up). The output is POSIX
	// because platform paths cross the wire in POSIX form regardless of host OS,
	// and MapArtistPath's result is only ever sent to a peer (never used for a
	// local filesystem operation).
	posixHost := toPosixPath(hostPath)
	bestLen := -1
	mapped := posixHost
	for _, m := range c.PathMappings {
		host := strings.TrimRight(toPosixPath(m.HostPrefix), "/")
		if host == "" {
			continue
		}
		remainder, ok := pathRemainder(posixHost, host)
		if !ok {
			continue
		}
		if len(host) > bestLen {
			bestLen = len(host)
			mapped = strings.TrimRight(toPosixPath(m.PlatformPrefix), "/") + remainder
		}
	}
	return mapped
}

// toPosixPath folds Windows backslash separators to forward slashes. It is the
// single normalization both halves of the push path-check share: MapArtistPath
// (which produces the path) and connection.normalizeRootPath (which the root
// guard compares it against). Keeping the fold in one helper is what stops the
// two from disagreeing on the comparison form.
func toPosixPath(p string) string {
	return strings.ReplaceAll(p, `\`, "/")
}

// pathRemainder reports whether prefix is a separator-bounded path prefix of p
// (forward-slash semantics; BOTH arguments must already be POSIX-folded by
// toPosixPath); on a match it returns the trailing portion, which is "" for an
// exact match or begins with "/" otherwise. Grafting the platform prefix onto
// this remainder reconstructs the translated path.
func pathRemainder(p, prefix string) (string, bool) {
	if p == prefix {
		return "", true
	}
	if strings.HasPrefix(p, prefix+"/") {
		return p[len(prefix):], true
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
