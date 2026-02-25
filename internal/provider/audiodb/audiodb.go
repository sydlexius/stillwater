package audiodb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

const (
	defaultV1BaseURL = "https://www.theaudiodb.com/api/v1/json"
	defaultV2BaseURL = "https://www.theaudiodb.com/api/v2/json"

	// freeAPIKey is TheAudioDB's public free-tier key (30 req/min, no sign-up required).
	freeAPIKey = "123"
)

// Adapter implements the provider.Provider interface for TheAudioDB.
// Without a configured API key it uses the free v1 API (30 req/min).
// With a premium Patreon key it upgrades to the v2 API (100 req/min).
type Adapter struct {
	client    *http.Client
	limiter   *provider.RateLimiterMap
	settings  *provider.SettingsService
	logger    *slog.Logger
	v1BaseURL string
	v2BaseURL string
}

// New creates a TheAudioDB adapter with the default base URLs.
func New(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, settings, logger, defaultV1BaseURL)
}

// NewWithBaseURL creates a TheAudioDB adapter with a custom base URL (for testing).
// Both v1 and v2 paths are rooted at baseURL in test mode.
func NewWithBaseURL(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger, baseURL string) *Adapter {
	base := strings.TrimRight(baseURL, "/")
	return &Adapter{
		client:    &http.Client{Timeout: 10 * time.Second},
		limiter:   limiter,
		settings:  settings,
		logger:    logger.With(slog.String("provider", "audiodb")),
		v1BaseURL: base,
		v2BaseURL: base,
	}
}

// Name returns the provider name.
func (a *Adapter) Name() provider.ProviderName { return provider.NameAudioDB }

// RequiresAuth returns false because TheAudioDB has a free tier (key 123, 30 req/min).
// A premium Patreon key can be configured to unlock the v2 API (100 req/min).
func (a *Adapter) RequiresAuth() bool { return false }

// SearchArtist searches TheAudioDB by artist name.
func (a *Adapter) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	apiKey, err := a.getAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	if err := a.limiter.Wait(ctx, provider.NameAudioDB); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameAudioDB,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	reqURL := a.buildSearchURL(apiKey, name)
	artists, err := a.fetchArtists(ctx, reqURL, apiKey)
	if err != nil {
		return nil, err
	}

	results := make([]provider.ArtistSearchResult, 0, len(artists))
	for _, art := range artists {
		results = append(results, provider.ArtistSearchResult{
			ProviderID:    art.IDArtist,
			Name:          art.Artist,
			Country:       art.Country,
			Score:         100,
			MusicBrainzID: art.MusicBrainzID,
			Source:        string(provider.NameAudioDB),
		})
	}
	return results, nil
}

// GetArtist fetches full metadata for an artist by their MusicBrainz ID.
func (a *Adapter) GetArtist(ctx context.Context, mbid string) (*provider.ArtistMetadata, error) {
	apiKey, err := a.getAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	if err := a.limiter.Wait(ctx, provider.NameAudioDB); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameAudioDB,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	reqURL := a.buildLookupURL(apiKey, mbid)
	artists, err := a.fetchArtists(ctx, reqURL, apiKey)
	if err != nil {
		return nil, err
	}
	if len(artists) == 0 {
		return nil, &provider.ErrNotFound{Provider: provider.NameAudioDB, ID: mbid}
	}

	return mapArtist(&artists[0]), nil
}

// GetImages fetches available images for an artist by their MusicBrainz ID.
func (a *Adapter) GetImages(ctx context.Context, mbid string) ([]provider.ImageResult, error) {
	apiKey, err := a.getAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	if err := a.limiter.Wait(ctx, provider.NameAudioDB); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameAudioDB,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	reqURL := a.buildLookupURL(apiKey, mbid)
	artists, err := a.fetchArtists(ctx, reqURL, apiKey)
	if err != nil {
		return nil, err
	}
	if len(artists) == 0 {
		return nil, &provider.ErrNotFound{Provider: provider.NameAudioDB, ID: mbid}
	}

	return mapImages(&artists[0]), nil
}

// TestConnection verifies connectivity by searching for a known artist.
func (a *Adapter) TestConnection(ctx context.Context) error {
	apiKey, err := a.getAPIKey(ctx)
	if err != nil {
		return err
	}
	reqURL := a.buildSearchURL(apiKey, "Radiohead")
	_, err = a.fetchArtists(ctx, reqURL, apiKey)
	return err
}

// getAPIKey returns the stored API key, falling back to the free-tier key.
func (a *Adapter) getAPIKey(ctx context.Context) (string, error) {
	apiKey, err := a.settings.GetAPIKey(ctx, provider.NameAudioDB)
	if err != nil {
		return "", fmt.Errorf("getting API key: %w", err)
	}
	if apiKey == "" {
		return freeAPIKey, nil
	}
	return apiKey, nil
}

// isPremiumKey returns true when the key is a paid Patreon key (not the free key).
func isPremiumKey(apiKey string) bool {
	return apiKey != freeAPIKey
}

// buildSearchURL constructs the search endpoint URL for the appropriate API tier.
func (a *Adapter) buildSearchURL(apiKey, name string) string {
	if isPremiumKey(apiKey) {
		return fmt.Sprintf("%s/search/artist/%s", a.v2BaseURL, url.PathEscape(name))
	}
	return fmt.Sprintf("%s/%s/search.php?s=%s", a.v1BaseURL, apiKey, url.QueryEscape(name))
}

// buildLookupURL constructs the MBID lookup endpoint URL for the appropriate API tier.
func (a *Adapter) buildLookupURL(apiKey, mbid string) string {
	if isPremiumKey(apiKey) {
		return fmt.Sprintf("%s/lookup/artist_mb/%s", a.v2BaseURL, url.PathEscape(mbid))
	}
	return fmt.Sprintf("%s/%s/artist-mb.php?i=%s", a.v1BaseURL, apiKey, url.QueryEscape(mbid))
}

// fetchArtists performs an HTTP GET and parses the artist list from the response.
// Premium keys are sent in the X-API-KEY header (v2); the free key is embedded in the URL (v1).
func (a *Adapter) fetchArtists(ctx context.Context, reqURL string, apiKey string) ([]AudioDBArtist, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	if isPremiumKey(apiKey) {
		req.Header.Set("X-API-KEY", apiKey)
	}

	a.logger.Debug("requesting", slog.String("url", reqURL))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted base + API params
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameAudioDB,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameAudioDB,
			Cause:    fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var artistResp ArtistResponse
	if err := json.Unmarshal(body, &artistResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return artistResp.results(), nil
}

func mapArtist(art *AudioDBArtist) *provider.ArtistMetadata {
	meta := &provider.ArtistMetadata{
		ProviderID:    art.IDArtist,
		AudioDBID:     art.IDArtist,
		MusicBrainzID: art.MusicBrainzID,
		Name:          art.Artist,
		Gender:        strings.ToLower(art.Gender),
		Country:       art.Country,
		Biography:     art.BiographyEN,
	}

	if art.Genre != "" {
		meta.Genres = splitAndTrim(art.Genre)
	}
	if art.Style != "" {
		meta.Styles = splitAndTrim(art.Style)
	}
	if art.Mood != "" {
		meta.Moods = splitAndTrim(art.Mood)
	}

	if art.FormedYear != "" && art.FormedYear != "0" {
		meta.Formed = art.FormedYear
	}
	if art.BornYear != "" && art.BornYear != "0" {
		meta.Born = art.BornYear
	}
	if art.DiedYear != "" && art.DiedYear != "0" {
		meta.Died = art.DiedYear
	}
	if art.Disbanded != "" {
		meta.Disbanded = art.Disbanded
	}

	if art.ArtistAlternate != "" {
		meta.Aliases = splitAndTrim(art.ArtistAlternate)
	}

	return meta
}

func mapImages(art *AudioDBArtist) []provider.ImageResult {
	var images []provider.ImageResult
	source := string(provider.NameAudioDB)

	addImage := func(url string, imgType provider.ImageType) {
		if url != "" {
			images = append(images, provider.ImageResult{
				URL:    url,
				Type:   imgType,
				Source: source,
			})
		}
	}

	addImage(art.ArtistThumb, provider.ImageThumb)
	addImage(art.ArtistLogo, provider.ImageLogo)
	addImage(art.ArtistWideThumb, provider.ImageWideThumb)
	addImage(art.ArtistBanner, provider.ImageBanner)
	addImage(art.ArtistFanart, provider.ImageFanart)
	addImage(art.ArtistFanart2, provider.ImageFanart)
	addImage(art.ArtistFanart3, provider.ImageFanart)
	addImage(art.ArtistFanart4, provider.ImageFanart)

	return images
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, "/")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
