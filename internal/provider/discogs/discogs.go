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

// SupportsNameLookup returns true because Discogs can search by artist name
// and use the top result's numeric ID to fetch metadata. This is a last-resort
// fallback: the orchestrator first tries to extract the Discogs numeric ID from
// MusicBrainz/Wikidata URL relations before falling back to name-based search.
func (a *Adapter) SupportsNameLookup() bool { return true }

// GetArtist fetches full metadata for an artist by their Discogs numeric ID.
// Non-numeric IDs (such as MusicBrainz UUIDs) are rejected immediately with
// ErrNotFound to avoid a wasted HTTP round-trip. When the orchestrator's
// MBID-to-name retry fires (because this adapter implements NameLookupProvider),
// GetArtist is called again with the artist name, which routes through
// getArtistByName for a search-then-fetch flow.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if !isNumericID(id) {
		// If the id is not numeric but also not a UUID, treat it as an
		// artist name and fall back to name-based search.
		if !provider.IsUUID(id) {
			a.logger.Debug("non-numeric non-UUID ID, falling back to name search",
				slog.String("id", id))
			return a.getArtistByName(ctx, id)
		}
		return nil, &provider.ErrNotFound{Provider: provider.NameDiscogs, ID: id}
	}

	return a.getArtistByID(ctx, id)
}

// getArtistByID fetches artist metadata directly using a numeric Discogs ID.
func (a *Adapter) getArtistByID(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
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

// getArtistByName searches Discogs by artist name and fetches metadata for the
// top result. This is the last-resort fallback when no numeric Discogs ID was
// found via MusicBrainz/Wikidata URL extraction.
func (a *Adapter) getArtistByName(ctx context.Context, name string) (*provider.ArtistMetadata, error) {
	results, err := a.SearchArtist(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, &provider.ErrNotFound{Provider: provider.NameDiscogs, ID: name}
	}
	a.logger.Debug("name search selected top result",
		slog.String("query", name),
		slog.String("result_name", results[0].Name),
		slog.String("result_id", results[0].ProviderID))
	return a.getArtistByID(ctx, results[0].ProviderID)
}

// GetImages fetches available images for an artist by their Discogs numeric ID.
// Returns ErrNotFound for non-numeric IDs (such as MusicBrainz UUIDs) without
// making an HTTP request.
func (a *Adapter) GetImages(ctx context.Context, id string) ([]provider.ImageResult, error) {
	if !isNumericID(id) {
		return nil, &provider.ErrNotFound{Provider: provider.NameDiscogs, ID: id}
	}

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
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrNotFound{Provider: provider.NameDiscogs, ID: reqURL}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrAuthRequired{Provider: provider.NameDiscogs}
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDiscogs,
			Cause:    fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	return io.ReadAll(io.LimitReader(resp.Body, 512*1024))
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

// isNumericID reports whether id contains only ASCII digits.
// Discogs uses numeric-only artist IDs; MusicBrainz UUIDs and artist names
// are not valid Discogs IDs.
func isNumericID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
