package discogs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

const defaultBaseURL = "https://api.discogs.com"

// Adapter implements the provider.Provider interface for Discogs.
type Adapter struct {
	client   *http.Client
	limiter  *provider.RateLimiterMap
	settings *provider.SettingsService
	logger   *slog.Logger
	baseURL  string
}

// New creates a Discogs adapter with the default base URL.
func New(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, settings, logger, defaultBaseURL)
}

// NewWithBaseURL creates a Discogs adapter with a custom base URL (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger, baseURL string) *Adapter {
	return &Adapter{
		client:   &http.Client{Timeout: 10 * time.Second},
		limiter:  limiter,
		settings: settings,
		logger:   logger.With(slog.String("provider", "discogs")),
		baseURL:  strings.TrimRight(baseURL, "/"),
	}
}

// Name returns the provider name.
func (a *Adapter) Name() provider.ProviderName { return provider.NameDiscogs }

// RequiresAuth returns whether this provider needs an API key.
func (a *Adapter) RequiresAuth() bool { return true }

// SearchArtist searches Discogs for artists matching the given name.
func (a *Adapter) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	token, err := a.getToken(ctx)
	if err != nil {
		return nil, err
	}

	if err := a.limiter.Wait(ctx, provider.NameDiscogs); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDiscogs,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	params := url.Values{
		"q":    {name},
		"type": {"artist"},
	}
	reqURL := a.baseURL + "/database/search?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL, token)
	if err != nil {
		return nil, err
	}

	var resp SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing search response: %w", err)
	}

	results := make([]provider.ArtistSearchResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		results = append(results, provider.ArtistSearchResult{
			ProviderID: strconv.Itoa(r.ID),
			Name:       r.Title,
			Score:      100,
			Source:     string(provider.NameDiscogs),
		})
	}
	return results, nil
}

// GetArtist fetches full metadata for an artist by their Discogs ID.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	token, err := a.getToken(ctx)
	if err != nil {
		return nil, err
	}

	if err := a.limiter.Wait(ctx, provider.NameDiscogs); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDiscogs,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	reqURL := fmt.Sprintf("%s/artists/%s", a.baseURL, url.PathEscape(id))
	body, err := a.doRequest(ctx, reqURL, token)
	if err != nil {
		return nil, err
	}

	var detail ArtistDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	return mapArtist(&detail), nil
}

// GetImages fetches available images for an artist by their Discogs ID.
func (a *Adapter) GetImages(ctx context.Context, id string) ([]provider.ImageResult, error) {
	token, err := a.getToken(ctx)
	if err != nil {
		return nil, err
	}

	if err := a.limiter.Wait(ctx, provider.NameDiscogs); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDiscogs,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	reqURL := fmt.Sprintf("%s/artists/%s", a.baseURL, url.PathEscape(id))
	body, err := a.doRequest(ctx, reqURL, token)
	if err != nil {
		return nil, err
	}

	var detail ArtistDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	var images []provider.ImageResult
	source := string(provider.NameDiscogs)
	for _, img := range detail.Images {
		imgType := provider.ImageThumb
		if img.Type == "primary" {
			imgType = provider.ImageThumb
		}
		images = append(images, provider.ImageResult{
			URL:    img.URI,
			Type:   imgType,
			Width:  img.Width,
			Height: img.Height,
			Source: source,
		})
	}
	return images, nil
}

// TestConnection verifies the personal access token is valid.
func (a *Adapter) TestConnection(ctx context.Context) error {
	token, err := a.getToken(ctx)
	if err != nil {
		return err
	}
	reqURL := a.baseURL + "/database/search?q=test&type=artist&per_page=1"
	_, err = a.doRequest(ctx, reqURL, token)
	return err
}

func (a *Adapter) getToken(ctx context.Context) (string, error) {
	token, err := a.settings.GetAPIKey(ctx, provider.NameDiscogs)
	if err != nil {
		return "", fmt.Errorf("getting API token: %w", err)
	}
	if token == "" {
		return "", &provider.ErrAuthRequired{Provider: provider.NameDiscogs}
	}
	return token, nil
}

func (a *Adapter) doRequest(ctx context.Context, reqURL, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Discogs token="+token)
	req.Header.Set("User-Agent", "Stillwater/1.0")
	req.Header.Set("Accept", "application/json")

	a.logger.Debug("requesting", slog.String("url", reqURL))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted base + API params
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDiscogs,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		return nil, &provider.ErrNotFound{Provider: provider.NameDiscogs, ID: reqURL}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &provider.ErrAuthRequired{Provider: provider.NameDiscogs}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDiscogs,
			Cause:    fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	return io.ReadAll(resp.Body)
}

func mapArtist(d *ArtistDetail) *provider.ArtistMetadata {
	meta := &provider.ArtistMetadata{
		ProviderID: strconv.Itoa(d.ID),
		DiscogsID:  strconv.Itoa(d.ID),
		Name:       d.Name,
		Biography:  d.Profile,
		URLs:       make(map[string]string),
	}

	for i, u := range d.URLs {
		meta.URLs[fmt.Sprintf("link_%d", i)] = u
	}

	for _, alias := range d.Aliases {
		meta.Aliases = append(meta.Aliases, alias.Name)
	}

	for _, member := range d.Members {
		meta.Members = append(meta.Members, provider.MemberInfo{
			Name:     member.Name,
			IsActive: member.Active,
		})
	}

	return meta
}
