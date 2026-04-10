package lidarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
func (c *Client) CheckNFOWriterEnabled(ctx context.Context) (bool, string, error) {
	configs, err := c.getMetadataProviderConfigs(ctx)
	if err != nil {
		// Some Lidarr versions may not expose this endpoint; treat as unknown
		c.Logger.Warn("could not check metadata provider config", "error", err)
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.BuildRequestURL(c.BaseURL, path), nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.AuthFunc(req)

	resp, err := c.HTTPClient.Do(req) //nolint:gosec // URL constructed from trusted base + API path
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}

	// Determine shape by checking the first byte: '[' means array, '{' means object
	if body[0] == '[' {
		var configs []MetadataProviderConfig
		if err := json.Unmarshal(body, &configs); err != nil {
			return nil, fmt.Errorf("decoding array response: %w", err)
		}
		return configs, nil
	}

	var single MetadataProviderConfig
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, fmt.Errorf("decoding object response: %w", err)
	}
	return []MetadataProviderConfig{single}, nil
}

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("X-Api-Key", c.APIKey)
}
