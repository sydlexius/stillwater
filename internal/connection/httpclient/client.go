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

// StatusError is returned by the base HTTP helpers when the peer returns a
// non-2xx response. It exposes the raw status code (and a bounded snippet of
// the response body) so per-package callers can route auth-class failures
// (401/403) to a typed sentinel like emby.ErrAuthRequired without re-parsing
// the formatted error string. The string form preserves the historical
// "unexpected status N: body" shape so existing callers and tests that
// substring-match on err.Error() are unaffected.
type StatusError struct {
	StatusCode int
	Body       string
}

// NewStatusError builds a StatusError for the given status and body, or
// returns nil for 2xx codes so callers can use it directly in a return
// statement without re-checking the status. Mirrors the convention used by
// the BaseClient helpers; the hand-rolled HTTP paths in emby/push.go,
// jellyfin/push.go, and lidarr/client.go construct StatusError values via
// this constructor so the 2xx-is-nil contract lives in one place.
func NewStatusError(statusCode int, body string) *StatusError {
	if statusCode >= 200 && statusCode < 300 {
		return nil
	}
	return &StatusError{StatusCode: statusCode, Body: body}
}

// Error renders the status-error in the historical "unexpected status N: body"
// shape so callers that grep err.Error() (notably publish.classifyPushErr,
// which looks for "status 401"/"HTTP 401"/"status 5" substrings) keep
// working untouched. Per-package write methods that already include their
// own "X failed with status %d" prefix attach this StatusError via
// errors.Join so the typed code is reachable through errors.As without
// duplicating the status string in Error().
func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected status %d: %s", e.StatusCode, e.Body)
}

// IsAuth reports whether the status code is 401 or 403.
func (e *StatusError) IsAuth() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// readErrorBody reads up to 1 KB of body for use in error messages, then
// drains any remaining bytes so the HTTP transport can reuse the connection.
func readErrorBody(r io.Reader) string {
	buf, _ := io.ReadAll(io.LimitReader(r, 1024))
	_, _ = io.Copy(io.Discard, r)
	return string(buf)
}

// ReadBoundedStatusError builds a StatusError from a non-2xx response,
// capping the body at 1 MB to guard against a misbehaving peer returning a
// huge HTML error page. Used by the hand-rolled HTTP paths in emby/push.go
// and jellyfin/push.go so write-method errors carry the typed status code
// for ErrAuthRequired detection without re-parsing strings. The 1 MB cap
// is intentionally larger than readErrorBody's 1 KB so write-method errors
// preserve enough diagnostic body for operators to debug a 4xx/5xx from
// the peer; the smaller readErrorBody cap stays in place for the
// BaseClient helpers where errors typically fit in a single sentence.
func ReadBoundedStatusError(resp *http.Response) *StatusError {
	const maxErrBody = 1 << 20 // 1 MB
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	_, _ = io.Copy(io.Discard, resp.Body)
	return &StatusError{StatusCode: resp.StatusCode, Body: string(respBody)}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.BuildRequestURL(b.BaseURL, path), http.NoBody)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	b.AuthFunc(req)

	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode != http.StatusOK {
		// Construct StatusError directly rather than via NewStatusError:
		// NewStatusError treats any 2xx as success (nil) to fit the
		// POST/PUT contract, but Get's documented contract is 200-only,
		// so a 201/204 here must still surface as an error rather than
		// silently fall through to JSON-decoding an unexpected body.
		return &StatusError{StatusCode: resp.StatusCode, Body: readErrorBody(resp.Body)}
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

	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		return NewStatusError(resp.StatusCode, readErrorBody(resp.Body))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// GetRaw performs a GET request and returns the raw response bytes and Content-Type header.
// The success response body is capped at 25 MB.
func (b *BaseClient) GetRaw(ctx context.Context, path string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.BuildRequestURL(b.BaseURL, path), http.NoBody)
	if err != nil {
		return nil, "", fmt.Errorf("creating request: %w", err)
	}
	b.AuthFunc(req)

	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode != http.StatusOK {
		return nil, "", &StatusError{StatusCode: resp.StatusCode, Body: readErrorBody(resp.Body)}
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

// PutJSON performs a PUT request with Content-Type application/json and returns an error
// if the status is >= 300. The response body is optionally decoded into result.
func (b *BaseClient) PutJSON(ctx context.Context, path string, body io.Reader, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, connection.BuildRequestURL(b.BaseURL, path), body)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	b.AuthFunc(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		return NewStatusError(resp.StatusCode, readErrorBody(resp.Body))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Do issues a request with the given method, path, and body, setting the
// Content-Type header only when contentType is non-empty. Unlike Get,
// GetRaw, PostJSON, and PutJSON, it does not interpret the response status
// or decode a body -- the caller owns closing resp.Body and reading the
// status. This is the lower-level primitive the image upload/delete free
// functions in mediabrowser use: their bodies are a base64-encoded plain
// string (not JSON) and DeleteImage/DeleteImageAtIndex send no body at all,
// so neither fits PostJSON's JSON-only contract. A nil body is normalized to
// http.NoBody, matching the convention used elsewhere in this file.
//
// Unlike Get, GetRaw, PostJSON, and PutJSON, Do returns request-construction
// and transport errors UNWRAPPED (no "creating request"/"executing request"
// prefix of its own). Its only callers are the mediabrowser image
// upload/delete free functions, which apply their own single, method-specific
// wrap (e.g. "executing image upload: %w") to match the message the old
// per-package (pre-collapse) code produced. Adding a second Do-level prefix
// here would double that wrap into "executing image upload: executing
// request: <err>", a behavior regression from the old single-prefix message.
func (b *BaseClient) Do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, method, connection.BuildRequestURL(b.BaseURL, path), body)
	if err != nil {
		return nil, err
	}
	b.AuthFunc(req)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
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

	resp, err := b.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		return NewStatusError(resp.StatusCode, readErrorBody(resp.Body))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
