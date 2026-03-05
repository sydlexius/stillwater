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

// readErrorBody reads up to 1 KB of body for use in error messages, then
// drains any remaining bytes so the HTTP transport can reuse the connection.
func readErrorBody(r io.Reader) string {
	buf, _ := io.ReadAll(io.LimitReader(r, 1024))
	_, _ = io.Copy(io.Discard, r)
	return string(buf)
}

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
	l := logger.With(slog.String("integration", integration))
	cleaned, err := connection.ValidateBaseURL(baseURL)
	if err != nil {
		l.Warn("base URL failed validation, requests will fail", "error", err)
		cleaned = ""
	}
	return BaseClient{
		HTTPClient: httpClient,
		BaseURL:    cleaned,
		APIKey:     apiKey,
		Logger:     l,
		AuthFunc:   func(*http.Request) {}, // no-op; overridden by each platform client
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
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
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
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
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
		return nil, "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}

	const maxImageSize = 25 << 20 // 25 MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading response body: %w", err)
	}
	if len(data) > maxImageSize {
		_, _ = io.Copy(io.Discard, resp.Body)
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
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, readErrorBody(resp.Body))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
