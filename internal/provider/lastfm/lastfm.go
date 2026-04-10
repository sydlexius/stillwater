package lastfm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/tagclass"
)

const defaultBaseURL = "https://ws.audioscrobbler.com/2.0"

// Adapter implements the provider.Provider interface for Last.fm.
type Adapter struct {
	client   *http.Client
	limiter  *provider.RateLimiterMap
	settings *provider.SettingsService
	logger   *slog.Logger
	baseURL  string
}

// New creates a Last.fm adapter with the default base URL.
func New(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, settings, logger, defaultBaseURL)
}

// NewWithBaseURL creates a Last.fm adapter with a custom base URL (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger, baseURL string) *Adapter {
	return &Adapter{
		client:   &http.Client{Timeout: 10 * time.Second},
		limiter:  limiter,
		settings: settings,
		logger:   logger.With(slog.String("provider", "lastfm")),
		baseURL:  strings.TrimRight(baseURL, "/"),
	}
}

// Name returns the provider name.
func (a *Adapter) Name() provider.ProviderName { return provider.NameLastFM }

// RequiresAuth returns whether this provider needs an API key.
func (a *Adapter) RequiresAuth() bool { return true }

// SupportsNameLookup returns true because Last.fm GetArtist can accept an
// artist name and will use it as the "artist" parameter instead of "mbid".
func (a *Adapter) SupportsNameLookup() bool { return true }

// SearchArtist searches Last.fm for artists matching the given name.
func (a *Adapter) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	apiKey, err := a.getAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	if err := a.limiter.Wait(ctx, provider.NameLastFM); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameLastFM,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	params := url.Values{
		"method":  {"artist.search"},
		"artist":  {name},
		"api_key": {apiKey},
		"format":  {"json"},
		"limit":   {"25"},
	}
	reqURL := a.baseURL + "/?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var resp SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing search response: %w", err)
	}

	results := make([]provider.ArtistSearchResult, 0, len(resp.Results.ArtistMatches.Artist))
	for _, art := range resp.Results.ArtistMatches.Artist {
		results = append(results, provider.ArtistSearchResult{
			ProviderID:    art.Name,
			Name:          art.Name,
			MusicBrainzID: art.MBID,
			Score:         provider.NameSimilarity(name, art.Name),
			Source:        string(provider.NameLastFM),
		})
	}

	// Sort by score descending so the best match appears first.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results, nil
}

// GetArtist fetches full metadata for an artist by name or MBID.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	apiKey, err := a.getAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	if err := a.limiter.Wait(ctx, provider.NameLastFM); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameLastFM,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	params := url.Values{
		"method":  {"artist.getinfo"},
		"api_key": {apiKey},
		"format":  {"json"},
	}
	// If id looks like an MBID (UUID), use mbid parameter; otherwise use artist name
	if provider.IsUUID(id) {
		params.Set("mbid", id)
	} else {
		params.Set("artist", id)
	}
	// Set the lang parameter from the user's first metadata language preference.
	// Last.fm accepts a two- or three-letter language code for biography localization.
	if langPrefs := provider.MetadataLanguages(ctx); len(langPrefs) > 0 {
		// Use the base language of the first preference (e.g. "en-GB" -> "en").
		base := strings.SplitN(langPrefs[0], "-", 2)[0]
		if len(base) == 2 || len(base) == 3 {
			params.Set("lang", strings.ToLower(base))
		}
	}
	reqURL := a.baseURL + "/?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var resp InfoResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing artist info: %w", err)
	}

	if resp.Artist.Name == "" {
		return nil, &provider.ErrNotFound{Provider: provider.NameLastFM, ID: id}
	}

	// For name-based lookups, validate that the returned artist name is
	// sufficiently similar to the search term. Last.fm can return unrelated
	// artists when the exact name is not found (e.g., auto-corrected results).
	if !provider.IsUUID(id) {
		threshold, err := a.getNameSimilarityThreshold(ctx)
		if err != nil {
			return nil, err
		}
		score := provider.NameSimilarity(id, resp.Artist.Name)
		if threshold > 0 && score < threshold {
			a.logger.Warn("rejecting result: name similarity too low",
				slog.String("search_term", id),
				slog.String("result_name", resp.Artist.Name),
				slog.Int("similarity", score),
				slog.Int("threshold", threshold),
			)
			return nil, &provider.ErrNotFound{Provider: provider.NameLastFM, ID: id}
		}
	}

	return mapArtist(&resp.Artist), nil
}

// GetImages returns nil since Last.fm does not host high-quality artist images.
func (a *Adapter) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// TestConnection verifies the API key is valid.
func (a *Adapter) TestConnection(ctx context.Context) error {
	apiKey, err := a.getAPIKey(ctx)
	if err != nil {
		return err
	}
	params := url.Values{
		"method":  {"artist.search"},
		"artist":  {"test"},
		"api_key": {apiKey},
		"format":  {"json"},
		"limit":   {"1"},
	}
	reqURL := a.baseURL + "/?" + params.Encode()
	_, err = a.doRequest(ctx, reqURL)
	return err
}

func (a *Adapter) getAPIKey(ctx context.Context) (string, error) {
	apiKey, err := a.settings.GetAPIKey(ctx, provider.NameLastFM)
	if err != nil {
		return "", fmt.Errorf("getting API key: %w", err)
	}
	if apiKey == "" {
		return "", &provider.ErrAuthRequired{Provider: provider.NameLastFM}
	}
	return apiKey, nil
}

func (a *Adapter) doRequest(ctx context.Context, reqURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", "Stillwater/1.0")
	req.Header.Set("Accept", "application/json")

	a.logger.Debug("requesting", slog.String("url", reqURL))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted base + API params
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameLastFM,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrAuthRequired{Provider: provider.NameLastFM}
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameLastFM,
			Cause:    fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	return io.ReadAll(io.LimitReader(resp.Body, 512*1024))
}

func mapArtist(info *ArtistInfo) *provider.ArtistMetadata {
	meta := &provider.ArtistMetadata{
		ProviderID:    info.Name,
		MusicBrainzID: info.MBID,
		Name:          info.Name,
		Biography:     cleanBio(info.Bio.Content),
	}

	// Classify Last.fm tags into genres, styles, and moods instead of
	// dumping everything into the genres bucket.
	var tagNames []string
	for _, tag := range info.Tags.Tag {
		if tag.Name != "" {
			tagNames = append(tagNames, tag.Name)
		}
	}
	meta.Genres, meta.Styles, meta.Moods = tagclass.ClassifyTags(tagNames)

	for _, similar := range info.Similar.Artist {
		if similar.Name != "" {
			meta.SimilarArtists = append(meta.SimilarArtists, similar.Name)
		}
	}

	if info.URL != "" {
		meta.URLs = map[string]string{"lastfm": info.URL}
	}

	return meta
}

// getNameSimilarityThreshold reads the configurable threshold from settings.
// Returns an error if the context is canceled. Falls back to the default (60)
// if the setting is missing or unreadable for non-context reasons.
func (a *Adapter) getNameSimilarityThreshold(ctx context.Context) (int, error) {
	threshold, err := a.settings.GetNameSimilarityThreshold(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		a.logger.Warn("reading name similarity threshold, using default",
			slog.Int("default", provider.DefaultNameSimilarityThreshold),
			slog.String("error", err.Error()),
		)
		return provider.DefaultNameSimilarityThreshold, nil
	}
	return threshold, nil
}

// cleanBio removes the Last.fm attribution link appended to bios.
func cleanBio(bio string) string {
	if idx := strings.Index(bio, "<a href=\"https://www.last.fm"); idx > 0 {
		bio = strings.TrimSpace(bio[:idx])
	}
	return bio
}
