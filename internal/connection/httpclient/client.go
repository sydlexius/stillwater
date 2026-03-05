package httpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/connection"
)

// BaseClient holds the common fields and HTTP transport methods shared by all platform clients.
type BaseClient struct {
	HTTPClient *http.Client
	BaseURL    string
	APIKey     string
	Logger     *slog.Logger
	AuthFunc   func(*http.Request)
}

// NewBase creates a BaseClient with URL validation.
// integration is used for structured log context (e.g. "emby", "jellyfin", "lidarr").
func NewBase(baseURL, apiKey string, httpClient *http.Client, logger *slog.Logger, integration string) BaseClient {
	cleaned, err := connection.ValidateBaseURL(baseURL)
	if err != nil {
		logger.Warn(integration+" base URL failed validation, requests will fail", "error", err)
		cleaned = ""
	}
	return BaseClient{
		HTTPClient: httpClient,
		BaseURL:    cleaned,
		APIKey:     apiKey,
		Logger:     logger.With(slog.String("integration", integration)),
	}
}

// Get performs a GET request and JSON-decodes the response body into result.
// Returns an error if the status is not 200 OK.
func (b *BaseClient) Get(ctx context.Context, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.BuildRequestURL(b.BaseURL, path), nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	b.AuthFunc(req)

	resp, err := b.HTTPClient.Do(req) //nolint:gosec // URL constructed from trusted base + API path
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// Post performs a POST request and returns an error if the status is >= 300.
func (b *BaseClient) Post(ctx context.Context, path string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.BuildRequestURL(b.BaseURL, path), body)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	b.AuthFunc(req)

	resp, err := b.HTTPClient.Do(req) //nolint:gosec // URL constructed from trusted base + API path
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// GetRaw performs a GET request and returns the raw response bytes and Content-Type header.
// The success response body is capped at 25 MB.
func (b *BaseClient) GetRaw(ctx context.Context, path string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.BuildRequestURL(b.BaseURL, path), nil)
	if err != nil {
		return nil, "", fmt.Errorf("creating request: %w", err)
	}
	b.AuthFunc(req)

	resp, err := b.HTTPClient.Do(req) //nolint:gosec // URL constructed from trusted base + API path
	if err != nil {
		return nil, "", fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	const maxImageSize = 25 << 20 // 25 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading response body: %w", err)
	}
	if len(data) > maxImageSize {
		return nil, "", fmt.Errorf("image exceeds 25 MB limit")
	}
	return data, resp.Header.Get("Content-Type"), nil
}

// PostJSON performs a POST request with Content-Type application/json and decodes the response.
// Returns an error if the status is >= 300.
func (b *BaseClient) PostJSON(ctx context.Context, path string, body io.Reader, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.BuildRequestURL(b.BaseURL, path), body)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	b.AuthFunc(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.HTTPClient.Do(req) //nolint:gosec // URL constructed from trusted base + API path
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}
