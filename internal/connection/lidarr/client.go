package lidarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/httpclient"
)

// Client communicates with a Lidarr server.
type Client struct {
	httpclient.BaseClient
}

// New creates a Lidarr client with default HTTP settings.
//
// Uses a raw http.Client (not httpsafe.SafeClient) because Lidarr is a
// user-configured *arr-stack service that typically runs alongside Stillwater
// on loopback (127.0.0.1:8686) or an RFC 1918 LAN address. The
// httpsafe.SafeTransport SSRF guard would reject those destinations,
// breaking the integration for legitimate self-hosted setups. The destination
// URL is operator-supplied via Settings, not user-controlled input.
func New(baseURL, apiKey string, logger *slog.Logger) *Client {
	return NewWithHTTPClient(baseURL, apiKey, &http.Client{Timeout: 10 * time.Second}, logger)
}

// NewWithHTTPClient creates a Lidarr client with a custom HTTP client (for testing).
func NewWithHTTPClient(baseURL, apiKey string, httpClient *http.Client, logger *slog.Logger) *Client {
	c := &Client{
		BaseClient: httpclient.NewBase(baseURL, apiKey, httpClient, logger, "lidarr"),
	}
	c.AuthFunc = c.setAuth
	return c
}

// TestConnection verifies connectivity by calling GET /api/v1/system/status.
func (c *Client) TestConnection(ctx context.Context) error {
	var status SystemStatus
	if err := c.Get(ctx, "/api/v1/system/status", &status); err != nil {
		return fmt.Errorf("testing connection: %w", err)
	}
	c.Logger.Debug("lidarr connection ok", "version", status.Version)
	return nil
}

// GetArtists returns all artists from Lidarr.
func (c *Client) GetArtists(ctx context.Context) ([]Artist, error) {
	var artists []Artist
	if err := c.Get(ctx, "/api/v1/artist", &artists); err != nil {
		return nil, fmt.Errorf("getting artists: %w", err)
	}
	return artists, nil
}

// GetMetadataProfiles returns all metadata profiles.
func (c *Client) GetMetadataProfiles(ctx context.Context) ([]MetadataProfile, error) {
	var profiles []MetadataProfile
	if err := c.Get(ctx, "/api/v1/metadataprofile", &profiles); err != nil {
		return nil, fmt.Errorf("getting metadata profiles: %w", err)
	}
	return profiles, nil
}

// CheckNFOWriterEnabled reports whether Lidarr is configured to write any
// artist-level NFO files. Backed by GET /api/v1/metadata: each row is a
// metadata consumer (Kodi/Emby, Roksbox, WDTV) with an "enable" master
// switch and a "fields" array whose entries like {"name":"artistMetadata",
// "value":true} govern what it writes. We flag a conflict when any enabled
// consumer has a truthy "artistMetadata" (or fallback "artistMetadataKey")
// field. The library name is always empty for Lidarr since these settings
// are global, not per-library.
//
// Historical note: earlier code queried /api/v1/config/metadataprovider,
// which governs audio-tag embedding, not NFO/image writers -- so it could
// never flag the true write-back source. Switched to /api/v1/metadata.
func (c *Client) CheckNFOWriterEnabled(ctx context.Context) (bool, string, error) {
	consumers, err := c.getMetadataConsumers(ctx)
	if err != nil {
		// Propagate the error so the conflict detector can populate
		// ConnectionState.CheckErr and fail closed on a transient Lidarr
		// outage. Silently returning (false, "", nil) would mark the
		// connection clean and reopen NFO writes.
		return false, "", fmt.Errorf("checking lidarr metadata consumers: %w", err)
	}
	for _, m := range consumers {
		if !metadataConsumerEnabled(m) {
			continue
		}
		if consumerWritesArtistField(m, "artistMetadata") {
			name, _ := m["name"].(string)
			return true, name, nil
		}
	}
	return false, "", nil
}

// TriggerArtistRefresh triggers a metadata refresh for a specific artist.
func (c *Client) TriggerArtistRefresh(ctx context.Context, artistID int) (*CommandResponse, error) {
	cmd := CommandBody{
		Name:     "RefreshArtist",
		ArtistID: artistID,
	}
	body, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshaling command: %w", err)
	}

	var resp CommandResponse
	if err := c.PostJSON(ctx, "/api/v1/command", bytes.NewReader(body), &resp); err != nil {
		return nil, fmt.Errorf("triggering artist refresh: %w", err)
	}
	return &resp, nil
}

// MetadataConsumerStatus describes the state of a Lidarr metadata consumer (e.g., Kodi/XBMC).
type MetadataConsumerStatus struct {
	ID           int    `json:"id"`
	ConsumerName string `json:"consumer_name"`
	MetadataType string `json:"metadata_type"`
	Enabled      bool   `json:"enabled"`
}

// GetMetadataConsumers returns the metadata consumer configuration from Lidarr.
// This is a global setting, not per-library.
func (c *Client) GetMetadataConsumers(ctx context.Context) ([]MetadataConsumerStatus, error) {
	configs, err := c.getMetadataProviderConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting metadata provider config: %w", err)
	}

	var results []MetadataConsumerStatus
	for _, cfg := range configs {
		results = append(results, MetadataConsumerStatus{
			ID:           cfg.ID,
			ConsumerName: cfg.ConsumerName,
			MetadataType: cfg.MetadataType,
			Enabled:      cfg.Enable,
		})
	}
	return results, nil
}

// DisableMetadataConsumer disables a specific metadata consumer by config ID.
func (c *Client) DisableMetadataConsumer(ctx context.Context, configID int) error {
	if configID <= 0 {
		return fmt.Errorf("config id must be positive")
	}
	payload := MetadataProviderConfig{ID: configID, Enable: false}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding metadata provider config: %w", err)
	}

	path := fmt.Sprintf("/api/v1/config/metadataprovider/%d", configID)
	return c.PutJSON(ctx, path, bytes.NewReader(body), nil)
}

// getMetadataProviderConfigs fetches the metadata provider config from Lidarr,
// handling both response formats: newer Lidarr versions return a single JSON
// object, while older versions return a JSON array.
func (c *Client) getMetadataProviderConfigs(ctx context.Context) ([]MetadataProviderConfig, error) {
	const path = "/api/v1/config/metadataprovider"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.BuildRequestURL(c.BaseURL, path), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.AuthFunc(req)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode != http.StatusOK {
		// Read a small prefix for diagnostics and drain the rest so the
		// transport can reuse the connection.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // cap at 1 MB
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}

	// Determine shape by checking the first byte: '[' means array, '{' means object.
	switch body[0] {
	case '[':
		var configs []MetadataProviderConfig
		if err := json.Unmarshal(body, &configs); err != nil {
			return nil, fmt.Errorf("decoding array response: %w", err)
		}
		return configs, nil
	case '{':
		var single MetadataProviderConfig
		if err := json.Unmarshal(body, &single); err != nil {
			return nil, fmt.Errorf("decoding object response: %w", err)
		}
		return []MetadataProviderConfig{single}, nil
	default:
		return nil, fmt.Errorf("unexpected JSON root type %q in response", string(body[:1]))
	}
}

// CheckImageSaverEnabled reports whether any enabled Lidarr metadata
// consumer is configured to write artist images to disk. Mirrors
// CheckNFOWriterEnabled but scopes the field check to artistImages.
func (c *Client) CheckImageSaverEnabled(ctx context.Context) (bool, string, error) {
	consumers, err := c.getMetadataConsumers(ctx)
	if err != nil {
		// Same rationale as CheckNFOWriterEnabled above: propagate so the
		// conflict detector fails closed on transient peer error instead
		// of silently treating the connection as clean.
		return false, "", fmt.Errorf("checking lidarr metadata consumers: %w", err)
	}
	for _, m := range consumers {
		if !metadataConsumerEnabled(m) {
			continue
		}
		if consumerWritesArtistField(m, "artistImages") {
			name, _ := m["name"].(string)
			return true, name, nil
		}
	}
	return false, "", nil
}

// ConsumerWriteBackSnapshot records the pre-disable artistMetadata +
// artistImages values for every enabled Lidarr metadata consumer. Unlike
// the previous implementation (which toggled the whole "enable" flag),
// this preserves the consumer's active state and only replays the two
// artist-level fields Stillwater actually mutates.
type ConsumerWriteBackSnapshot struct {
	Version       int                     `json:"version"`
	SnapshottedAt time.Time               `json:"snapshotted_at"`
	Consumers     []ConsumerSnapshotEntry `json:"consumers"`
}

// ConsumerSnapshotEntry is one consumer's pre-disable field state.
type ConsumerSnapshotEntry struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	ArtistMetadata bool   `json:"artist_metadata"`
	ArtistImages   bool   `json:"artist_images"`
}

// SnapshotLibraryOptions captures the artist-write fields for every Lidarr
// metadata consumer so RestoreLibraryOptions can replay them verbatim.
// The function name matches the Emby/Jellyfin equivalents so the conflict
// service can dispatch by connection type without special-casing.
func (c *Client) SnapshotLibraryOptions(ctx context.Context) (string, error) {
	consumers, err := c.getMetadataConsumers(ctx)
	if err != nil {
		return "", fmt.Errorf("getting metadata consumers for snapshot: %w", err)
	}
	snap := ConsumerWriteBackSnapshot{
		Version:       1,
		SnapshottedAt: time.Now().UTC(),
		Consumers:     make([]ConsumerSnapshotEntry, 0, len(consumers)),
	}
	for _, m := range consumers {
		id := consumerID(m)
		name, _ := m["name"].(string)
		snap.Consumers = append(snap.Consumers, ConsumerSnapshotEntry{
			ID:             id,
			Name:           name,
			ArtistMetadata: consumerWritesArtistField(m, "artistMetadata"),
			ArtistImages:   consumerWritesArtistField(m, "artistImages"),
		})
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		return "", fmt.Errorf("encoding snapshot: %w", err)
	}
	return string(buf), nil
}

// DisableFileWriteBack flips artistMetadata and artistImages to false on
// every enabled Lidarr metadata consumer, so the peer stops writing
// artist.nfo and artist image files to the shared library directory. The
// consumer's master "enable" flag is deliberately left alone so the user's
// per-album/release-group settings continue to function; only the
// artist-scope writes are turned off because those are the ones that
// clobber Stillwater's writes.
//
// Best-effort across consumers: records the first error and continues.
func (c *Client) DisableFileWriteBack(ctx context.Context) error {
	consumers, err := c.getMetadataConsumers(ctx)
	if err != nil {
		return fmt.Errorf("getting metadata consumers: %w", err)
	}
	var firstErr error
	for _, m := range consumers {
		if !metadataConsumerEnabled(m) {
			continue
		}
		m = setConsumerArtistField(m, "artistMetadata", false)
		m = setConsumerArtistField(m, "artistImages", false)
		if err := c.putMetadataConsumer(ctx, m); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			name, _ := m["name"].(string)
			c.Logger.Warn("disabling metadata consumer fields failed", "consumer", name, "error", err)
		}
	}
	return firstErr
}

// RestoreLibraryOptions replays each consumer's artistMetadata/artistImages
// field values from the snapshot. Consumers present on the peer but absent
// from the snapshot are left alone. Consumers in the snapshot but no longer
// on the peer are logged and skipped.
func (c *Client) RestoreLibraryOptions(ctx context.Context, snapshotJSON string) error {
	var snap ConsumerWriteBackSnapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snap); err != nil {
		return fmt.Errorf("decoding snapshot: %w", err)
	}
	if snap.Version != 1 {
		return fmt.Errorf("unsupported snapshot version %d", snap.Version)
	}
	consumers, err := c.getMetadataConsumers(ctx)
	if err != nil {
		return fmt.Errorf("getting metadata consumers: %w", err)
	}
	byID := make(map[int]map[string]any, len(consumers))
	for _, m := range consumers {
		byID[consumerID(m)] = m
	}
	var firstErr error
	for _, entry := range snap.Consumers {
		target, ok := byID[entry.ID]
		if !ok {
			c.Logger.Warn("snapshot consumer missing on peer; skipping", "id", entry.ID, "name", entry.Name)
			continue
		}
		target = setConsumerArtistField(target, "artistMetadata", entry.ArtistMetadata)
		target = setConsumerArtistField(target, "artistImages", entry.ArtistImages)
		if err := c.putMetadataConsumer(ctx, target); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			c.Logger.Warn("restoring metadata consumer fields failed", "consumer", entry.Name, "id", entry.ID, "error", err)
		}
	}
	return firstErr
}

// getMetadataConsumers fetches /api/v1/metadata and returns the raw list
// of consumer configs. Using raw map[string]any preserves fields we do not
// model, so putMetadataConsumer can PUT back a lossless body without
// dropping anything the peer cares about.
func (c *Client) getMetadataConsumers(ctx context.Context) ([]map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.BuildRequestURL(c.BaseURL, "/api/v1/metadata"), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.AuthFunc(req)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(body), &out); err != nil {
		return nil, fmt.Errorf("decoding metadata consumers: %w", err)
	}
	for _, m := range out {
		name, _ := m["name"].(string)
		c.Logger.Debug("lidarr metadata consumer discovered",
			"id", consumerID(m),
			"name", name,
			"enable", metadataConsumerEnabled(m),
			"artist_metadata", consumerWritesArtistField(m, "artistMetadata"),
			"artist_images", consumerWritesArtistField(m, "artistImages"),
		)
	}
	return out, nil
}

// putMetadataConsumer PUTs a full consumer config back to the peer. Uses
// the consumer's own ID from the map (no struct surgery) so unknown fields
// survive the round-trip.
func (c *Client) putMetadataConsumer(ctx context.Context, m map[string]any) error {
	id := consumerID(m)
	if id == 0 {
		return fmt.Errorf("consumer missing numeric id")
	}
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding consumer: %w", err)
	}
	path := fmt.Sprintf("/api/v1/metadata/%d", id)
	return c.PutJSON(ctx, path, bytes.NewReader(body), nil)
}

// consumerID normalizes the "id" field from the raw consumer map. Lidarr
// returns it as a JSON number, which decodes to float64 through
// map[string]any. We accept both float64 and int so the helper is robust
// to future Lidarr type changes.
func consumerID(m map[string]any) int {
	switch v := m["id"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

// metadataConsumerEnabled reads the consumer's top-level "enable" flag.
// Defaults to false on any shape mismatch so an unreadable consumer never
// contributes to a false conflict.
func metadataConsumerEnabled(m map[string]any) bool {
	v, _ := m["enable"].(bool)
	return v
}

// consumerWritesArtistField reports whether the consumer's `fields` array
// contains an entry whose "name" equals fieldName and whose "value" is
// truthy. Lidarr encodes those values as booleans; accept both bool and
// stringified "true"/"1" for older versions.
func consumerWritesArtistField(m map[string]any, fieldName string) bool {
	fields, _ := m["fields"].([]any)
	for _, raw := range fields {
		field, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := field["name"].(string)
		if name != fieldName {
			continue
		}
		switch v := field["value"].(type) {
		case bool:
			return v
		case string:
			s := strings.ToLower(strings.TrimSpace(v))
			return s == "true" || s == "1" || s == "yes"
		}
	}
	return false
}

// setConsumerArtistField mutates the consumer's fields slice so the named
// field carries the given value, adding the field if absent. Returns the
// same map for fluent chaining; bool result is unused but kept so callers
// could decide to skip PUT when nothing changed (future optimization).
func setConsumerArtistField(m map[string]any, fieldName string, value bool) map[string]any {
	fields, _ := m["fields"].([]any)
	for i, raw := range fields {
		field, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := field["name"].(string); name == fieldName {
			field["value"] = value
			fields[i] = field
			m["fields"] = fields
			return m
		}
	}
	fields = append(fields, map[string]any{"name": fieldName, "value": value})
	m["fields"] = fields
	return m
}

// UpdateArtistPath rewrites the Path on the given Lidarr artist by GET-modify-
// PUT round-tripping the full ArtistResource. Lidarr's PUT /api/v1/artist/{id}
// accepts a moveFiles query parameter; we pass moveFiles=false because
// Stillwater has already moved the directory on disk -- asking Lidarr to also
// move would either fail (source no longer exists) or race against our own
// rename.
//
// Uses map[string]any rather than the typed Artist struct so unknown fields
// (Lidarr's ArtistResource is large and evolves between minor versions)
// survive the round-trip. The Lidarr REST surface is operator-supplied and
// runs on loopback or LAN; see the New() rationale for why httpsafe is not
// applied.
//
// Used by publish.Publisher.SyncRename after a successful directory rename so
// Lidarr stops looking for files at the old path and resumes import / refresh
// against the new one (#1231).
func (c *Client) UpdateArtistPath(ctx context.Context, platformArtistID, newPath string) error {
	if strings.TrimSpace(platformArtistID) == "" {
		return fmt.Errorf("platformArtistID is required")
	}
	if strings.TrimSpace(newPath) == "" {
		return fmt.Errorf("newPath is required")
	}
	// PathEscape the platform ID so an ID containing reserved characters
	// (slashes, percent signs, etc.) cannot break out of the URL segment.
	// Jellyfin's push.go already does this; bringing Lidarr into parity
	// closes the same class of bug here. Lidarr IDs are numeric in the
	// happy path but the value flows in from the platform_ids table, so a
	// future migration or import path that loaded a non-numeric ID would
	// still produce a well-formed URL.
	escapedID := url.PathEscape(platformArtistID)
	getPath := fmt.Sprintf("/api/v1/artist/%s", escapedID)
	var item map[string]any
	if err := c.Get(ctx, getPath, &item); err != nil {
		return fmt.Errorf("fetching artist for path update: %w", err)
	}
	if item == nil {
		return fmt.Errorf("lidarr returned empty artist body for id %s", platformArtistID)
	}
	item["path"] = newPath

	body, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("encoding path update body: %w", err)
	}
	// moveFiles=false: the on-disk move has already happened; we are only
	// updating Lidarr's record of where the files live. Letting Lidarr try
	// to move them would either fail (source gone) or race our own rename.
	putPath := fmt.Sprintf("/api/v1/artist/%s?moveFiles=false", escapedID)
	if err := c.PutJSON(ctx, putPath, bytes.NewReader(body), nil); err != nil {
		return fmt.Errorf("putting artist path update: %w", err)
	}
	return nil
}

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("X-Api-Key", c.APIKey)
}
