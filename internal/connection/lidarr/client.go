package lidarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/connection/httpclient"
)

// Client communicates with a Lidarr server.
type Client struct {
	httpclient.BaseClient
}

// New creates a Lidarr client with default HTTP settings.
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

// CheckNFOWriterEnabled checks if Lidarr is configured to write NFO files.
// Returns true if any metadata consumer with NFO/Kodi type is enabled.
// The library name is always empty for Lidarr (the setting is global, not per-library).
//
// The /api/v1/config/metadataprovider endpoint returns either a JSON array or a
// single JSON object depending on the Lidarr version. Both shapes are handled.
func (c *Client) CheckNFOWriterEnabled(ctx context.Context) (bool, string, error) {
	var raw json.RawMessage
	if err := c.Get(ctx, "/api/v1/config/metadataprovider", &raw); err != nil {
		// Some Lidarr versions may not expose this endpoint; treat as unknown.
		c.Logger.Warn("could not check metadata provider config", "error", err)
		return false, "", nil
	}

	configs, err := decodeMetadataProviderConfigs(raw)
	if err != nil {
		c.Logger.Warn("could not decode metadata provider config", "error", err)
		return false, "", nil
	}

	for _, cfg := range configs {
		if cfg.Enable && (strings.Contains(strings.ToLower(cfg.MetadataType), "kodi") ||
			strings.Contains(strings.ToLower(cfg.ConsumerName), "kodi")) {
			return true, "", nil
		}
	}
	return false, "", nil
}

// decodeMetadataProviderConfigs decodes a JSON array or single-object response
// from the Lidarr /api/v1/config/metadataprovider endpoint into a slice.
func decodeMetadataProviderConfigs(raw json.RawMessage) ([]MetadataProviderConfig, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty response body")
	}
	if raw[0] == '[' {
		var configs []MetadataProviderConfig
		if err := json.Unmarshal(raw, &configs); err != nil {
			return nil, fmt.Errorf("decoding array response: %w", err)
		}
		return configs, nil
	}
	var single MetadataProviderConfig
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, fmt.Errorf("decoding object response: %w", err)
	}
	return []MetadataProviderConfig{single}, nil
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

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("X-Api-Key", c.APIKey)
}
