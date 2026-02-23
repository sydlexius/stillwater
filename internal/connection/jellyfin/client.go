package jellyfin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Client communicates with a Jellyfin server.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	logger     *slog.Logger
}

// New creates a Jellyfin client with default HTTP settings.
func New(baseURL, apiKey string, logger *slog.Logger) *Client {
	return NewWithHTTPClient(baseURL, apiKey, &http.Client{Timeout: 10 * time.Second}, logger)
}

// NewWithHTTPClient creates a Jellyfin client with a custom HTTP client (for testing).
func NewWithHTTPClient(baseURL, apiKey string, httpClient *http.Client, logger *slog.Logger) *Client {
	return &Client{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		logger:     logger.With(slog.String("integration", "jellyfin")),
	}
}

// TestConnection verifies connectivity by calling GET /System/Info.
func (c *Client) TestConnection(ctx context.Context) error {
	var info SystemInfo
	if err := c.get(ctx, "/System/Info", &info); err != nil {
		return fmt.Errorf("testing connection: %w", err)
	}
	c.logger.Debug("jellyfin connection ok", "server", info.ServerName, "version", info.Version)
	return nil
}

// GetMusicLibraries returns virtual folders with CollectionType "music".
func (c *Client) GetMusicLibraries(ctx context.Context) ([]VirtualFolder, error) {
	var folders []VirtualFolder
	if err := c.get(ctx, "/Library/VirtualFolders", &folders); err != nil {
		return nil, fmt.Errorf("getting virtual folders: %w", err)
	}

	var music []VirtualFolder
	for _, f := range folders {
		if strings.EqualFold(f.CollectionType, "music") {
			music = append(music, f)
		}
	}
	return music, nil
}

// CheckNFOWriterEnabled checks if any Jellyfin music library has an NFO metadata saver enabled.
// Returns true and the library name if found. On error, logs a warning and returns false.
func (c *Client) CheckNFOWriterEnabled(ctx context.Context) (bool, string, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		c.logger.Warn("could not check jellyfin library options", "error", err)
		return false, "", nil
	}

	for _, lib := range libs {
		for _, saver := range lib.LibraryOptions.MetadataSavers {
			if strings.Contains(strings.ToLower(saver), "nfo") {
				return true, lib.Name, nil
			}
		}
	}
	return false, "", nil
}

// GetArtists returns artists from a specific library with pagination.
func (c *Client) GetArtists(ctx context.Context, libraryID string, startIndex, limit int) (*ItemsResponse, error) {
	path := fmt.Sprintf("/Artists?ParentId=%s&StartIndex=%d&Limit=%d&Recursive=true", libraryID, startIndex, limit)
	var resp ItemsResponse
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, fmt.Errorf("getting artists: %w", err)
	}
	return &resp, nil
}

// TriggerLibraryScan triggers a full library scan.
func (c *Client) TriggerLibraryScan(ctx context.Context) error {
	if err := c.post(ctx, "/Library/Refresh", nil); err != nil {
		return fmt.Errorf("triggering library scan: %w", err)
	}
	return nil
}

// TriggerArtistRefresh refreshes metadata for a specific artist.
func (c *Client) TriggerArtistRefresh(ctx context.Context, artistID string) error {
	path := fmt.Sprintf("/Items/%s/Refresh", artistID)
	if err := c.post(ctx, path, nil); err != nil {
		return fmt.Errorf("triggering artist refresh: %w", err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req) //nolint:gosec // URL constructed from trusted base + API path
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req) //nolint:gosec // URL constructed from trusted base + API path
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token="%s"`, c.apiKey))
}
