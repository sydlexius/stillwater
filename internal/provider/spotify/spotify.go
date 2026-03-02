package spotify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

const (
	defaultBaseURL  = "https://api.spotify.com/v1"
	defaultTokenURL = "https://accounts.spotify.com/api/token" //nolint:gosec // URL, not a credential
)

// SettingsProvider reads API keys from the settings store.
type SettingsProvider interface {
	GetAPIKey(ctx context.Context, name provider.ProviderName) (string, error)
}

// Adapter implements provider.Provider and provider.TestableProvider for Spotify.
type Adapter struct {
	client   *http.Client
	limiter  *provider.RateLimiterMap
	settings SettingsProvider
	logger   *slog.Logger
	baseURL  string
	tokenURL string

	mu          sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

// New creates a Spotify adapter with the default base URL.
func New(limiter *provider.RateLimiterMap, settings SettingsProvider, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, settings, logger, defaultBaseURL, defaultTokenURL)
}

// NewWithBaseURL creates a Spotify adapter with custom URLs (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, settings SettingsProvider, logger *slog.Logger, baseURL, tokenURL string) *Adapter {
	return &Adapter{
		client:   &http.Client{Timeout: 10 * time.Second},
		limiter:  limiter,
		settings: settings,
		logger:   logger.With(slog.String("provider", "spotify")),
		baseURL:  strings.TrimRight(baseURL, "/"),
		tokenURL: tokenURL,
	}
}

// Name returns the provider identifier.
func (a *Adapter) Name() provider.ProviderName { return provider.NameSpotify }

// RequiresAuth returns true since Spotify needs OAuth client credentials.
func (a *Adapter) RequiresAuth() bool { return true }

// SearchArtist searches Spotify for artists matching the given name.
func (a *Adapter) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	if name == "" {
		return nil, nil
	}

	params := url.Values{
		"q":     {name},
		"type":  {"artist"},
		"limit": {"10"},
	}
	reqURL := a.baseURL + "/search?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing search response: %w", err)
	}

	results := make([]provider.ArtistSearchResult, 0, len(resp.Artists.Items))
	for _, item := range resp.Artists.Items {
		results = append(results, provider.ArtistSearchResult{
			ProviderID: item.ID,
			Name:       item.Name,
			Score:      100,
			Source:     string(provider.NameSpotify),
		})
	}

	a.logger.Debug("artist search completed",
		slog.String("query", name),
		slog.Int("results", len(results)))

	return results, nil
}

// GetArtist fetches metadata for an artist by their Spotify ID.
// Returns ErrNotFound for IDs that are not valid Spotify format.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if !IsSpotifyID(id) {
		return nil, &provider.ErrNotFound{Provider: provider.NameSpotify, ID: id}
	}

	reqURL := fmt.Sprintf("%s/artists/%s", a.baseURL, url.PathEscape(id))
	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var artist spotifyArtist
	if err := json.Unmarshal(body, &artist); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	meta := &provider.ArtistMetadata{
		ProviderID: artist.ID,
		SpotifyID:  artist.ID,
		Name:       artist.Name,
		Genres:     artist.Genres,
	}

	if len(artist.ExternalURLs) > 0 {
		meta.URLs = make(map[string]string)
		if spotifyURL, ok := artist.ExternalURLs["spotify"]; ok {
			meta.URLs["spotify"] = spotifyURL
		}
	}

	return meta, nil
}

// GetImages fetches artist images by Spotify ID.
// Returns ErrNotFound for IDs that are not valid Spotify format.
func (a *Adapter) GetImages(ctx context.Context, id string) ([]provider.ImageResult, error) {
	if !IsSpotifyID(id) {
		return nil, &provider.ErrNotFound{Provider: provider.NameSpotify, ID: id}
	}

	reqURL := fmt.Sprintf("%s/artists/%s", a.baseURL, url.PathEscape(id))
	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var artist spotifyArtist
	if err := json.Unmarshal(body, &artist); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	return imagesFromArtist(&artist), nil
}

// TestConnection verifies that the configured credentials are valid.
func (a *Adapter) TestConnection(ctx context.Context) error {
	creds, err := a.getCredentials(ctx)
	if err != nil {
		return err
	}

	// Force a fresh token to test the credentials
	if _, err := a.refreshToken(ctx, creds); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// Try a simple search to verify API access
	params := url.Values{
		"q":     {"test"},
		"type":  {"artist"},
		"limit": {"1"},
	}
	reqURL := a.baseURL + "/search?" + params.Encode()
	_, err = a.doRequest(ctx, reqURL)
	return err
}

// getCredentials retrieves and parses the Spotify client credentials.
func (a *Adapter) getCredentials(ctx context.Context) (*spotifyCredentials, error) {
	raw, err := a.settings.GetAPIKey(ctx, provider.NameSpotify)
	if err != nil {
		return nil, fmt.Errorf("reading credentials: %w", err)
	}
	if raw == "" {
		return nil, &provider.ErrAuthRequired{Provider: provider.NameSpotify}
	}

	var creds spotifyCredentials
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		return nil, fmt.Errorf("parsing credentials JSON: %w", err)
	}
	if creds.ClientID == "" || creds.ClientSecret == "" {
		return nil, &provider.ErrAuthRequired{Provider: provider.NameSpotify}
	}
	return &creds, nil
}

// getToken returns a valid access token, refreshing if expired or about to expire.
func (a *Adapter) getToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Return cached token if still valid (with 60s buffer)
	if a.cachedToken != "" && time.Now().Before(a.tokenExpiry.Add(-60*time.Second)) {
		return a.cachedToken, nil
	}

	creds, err := a.getCredentials(ctx)
	if err != nil {
		return "", err
	}

	token, err := a.refreshToken(ctx, creds)
	if err != nil {
		return "", err
	}

	return token, nil
}

// refreshToken exchanges client credentials for a new access token.
func (a *Adapter) refreshToken(ctx context.Context, creds *spotifyCredentials) (string, error) {
	data := url.Values{
		"grant_type": {"client_credentials"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(
		[]byte(creds.ClientID+":"+creds.ClientSecret),
	))

	resp, err := a.client.Do(req) //nolint:gosec // URL is from adapter config
	if err != nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameSpotify,
			Cause:    fmt.Errorf("token request: %w", err),
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("reading token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request failed: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parsing token response: %w", err)
	}

	a.cachedToken = tokenResp.AccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	return tokenResp.AccessToken, nil
}

// doRequest executes an authenticated GET request with rate limiting.
// On 401, invalidates the cached token and retries once.
func (a *Adapter) doRequest(ctx context.Context, reqURL string) ([]byte, error) {
	if err := a.limiter.Wait(ctx, provider.NameSpotify); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameSpotify,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	token, err := a.getToken(ctx)
	if err != nil {
		return nil, err
	}

	body, statusCode, err := a.executeRequest(ctx, reqURL, token)
	if err != nil {
		return nil, err
	}

	// On 401, invalidate token and retry once
	if statusCode == http.StatusUnauthorized {
		a.mu.Lock()
		a.cachedToken = ""
		a.tokenExpiry = time.Time{}
		a.mu.Unlock()

		token, err = a.getToken(ctx)
		if err != nil {
			return nil, err
		}
		body, statusCode, err = a.executeRequest(ctx, reqURL, token)
		if err != nil {
			return nil, err
		}
	}

	switch statusCode {
	case http.StatusOK:
		return body, nil
	case http.StatusNotFound:
		return nil, &provider.ErrNotFound{Provider: provider.NameSpotify, ID: reqURL}
	case http.StatusTooManyRequests:
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameSpotify,
			Cause:    fmt.Errorf("rate limited by server"),
		}
	default:
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameSpotify,
			Cause:    fmt.Errorf("unexpected status %d", statusCode),
		}
	}
}

// executeRequest performs a single HTTP GET with the given bearer token.
func (a *Adapter) executeRequest(ctx context.Context, reqURL, token string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from adapter config
	if err != nil {
		return nil, 0, &provider.ErrProviderUnavailable{
			Provider: provider.NameSpotify,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	// For non-200 responses, drain body
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, resp.StatusCode, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response body: %w", err)
	}
	return body, resp.StatusCode, nil
}

// IsSpotifyID reports whether id is a valid Spotify artist ID.
// Spotify IDs are 22-character base62 strings. UUIDs (contain hyphens) and
// pure numeric IDs (Deezer format) are rejected.
func IsSpotifyID(id string) bool {
	if id == "" || len(id) != 22 {
		return false
	}
	for _, r := range id {
		if !isBase62(r) {
			return false
		}
	}
	return true
}

// isBase62 reports whether the rune is a valid base62 character (0-9, a-z, A-Z).
func isBase62(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// imagesFromArtist converts Spotify artist images to ImageResult values.
// Spotify only provides artist profile photos, mapped to ImageThumb.
// Prefers the largest image available.
func imagesFromArtist(artist *spotifyArtist) []provider.ImageResult {
	if len(artist.Images) == 0 {
		return nil
	}

	source := string(provider.NameSpotify)

	// Find the largest image
	best := artist.Images[0]
	for _, img := range artist.Images[1:] {
		if img.Width > best.Width {
			best = img
		}
	}

	return []provider.ImageResult{
		{
			URL:    best.URL,
			Type:   provider.ImageThumb,
			Width:  best.Width,
			Height: best.Height,
			Source: source,
		},
	}
}
