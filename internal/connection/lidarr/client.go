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
)

// Client communicates with a Lidarr server.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	logger     *slog.Logger
}

// New creates a Lidarr client with default HTTP settings.
func New(baseURL, apiKey string, logger *slog.Logger) *Client {
	return NewWithHTTPClient(baseURL, apiKey, &http.Client{Timeout: 10 * time.Second}, logger)
}

// NewWithHTTPClient creates a Lidarr client with a custom HTTP client (for testing).
func NewWithHTTPClient(baseURL, apiKey string, httpClient *http.Client, logger *slog.Logger) *Client {
	return &Client{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		logger:     logger.With(slog.String("integration", "lidarr")),
	}
}

// TestConnection verifies connectivity by calling GET /api/v1/system/status.
func (c *Client) TestConnection(ctx context.Context) error {
	var status SystemStatus
	if err := c.get(ctx, "/api/v1/system/status", &status); err != nil {
		return fmt.Errorf("testing connection: %w", err)
	}
	c.logger.Debug("lidarr connection ok", "version", status.Version)
	return nil
}

// GetArtists returns all artists from Lidarr.
func (c *Client) GetArtists(ctx context.Context) ([]Artist, error) {
	var artists []Artist
	if err := c.get(ctx, "/api/v1/artist", &artists); err != nil {
		return nil, fmt.Errorf("getting artists: %w", err)
	}
	return artists, nil
}

// GetMetadataProfiles returns all metadata profiles.
func (c *Client) GetMetadataProfiles(ctx context.Context) ([]MetadataProfile, error) {
	var profiles []MetadataProfile
	if err := c.get(ctx, "/api/v1/metadataprofile", &profiles); err != nil {
		return nil, fmt.Errorf("getting metadata profiles: %w", err)
	}
	return profiles, nil
}

// CheckNFOWriterEnabled checks if Lidarr is configured to write NFO files.
// Returns true if any metadata consumer with NFO/Kodi type is enabled.
func (c *Client) CheckNFOWriterEnabled(ctx context.Context) (bool, error) {
	var configs []MetadataProviderConfig
	if err := c.get(ctx, "/api/v1/config/metadataprovider", &configs); err != nil {
		// Some Lidarr versions may not expose this endpoint; treat as unknown
		c.logger.Warn("could not check metadata provider config", "error", err)
		return false, nil
	}

	for _, cfg := range configs {
		if cfg.Enable && (strings.Contains(strings.ToLower(cfg.MetadataType), "kodi") ||
			strings.Contains(strings.ToLower(cfg.ConsumerName), "kodi")) {
			return true, nil
		}
	}
	return false, nil
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
	if err := c.postJSON(ctx, "/api/v1/command", bytes.NewReader(body), &resp); err != nil {
		return nil, fmt.Errorf("triggering artist refresh: %w", err)
	}
	return &resp, nil
}

func (c *Client) get(ctx context.Context, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, path string, body io.Reader, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("X-Api-Key", c.apiKey)
}
