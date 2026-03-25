package genius

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

const defaultBaseURL = "https://api.genius.com"

// Adapter implements the provider.Provider interface for Genius.
type Adapter struct {
	client   *http.Client
	limiter  *provider.RateLimiterMap
	settings *provider.SettingsService
	logger   *slog.Logger
	baseURL  string
}

// New creates a Genius adapter with the default base URL.
func New(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, settings, logger, defaultBaseURL)
}

// NewWithBaseURL creates a Genius adapter with a custom base URL (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger, baseURL string) *Adapter {
	return &Adapter{
		client:   &http.Client{Timeout: 10 * time.Second},
		limiter:  limiter,
		settings: settings,
		logger:   logger.With(slog.String("provider", "genius")),
		baseURL:  strings.TrimRight(baseURL, "/"),
	}
}

// Name returns the provider name.
func (a *Adapter) Name() provider.ProviderName { return provider.NameGenius }

// RequiresAuth returns whether this provider needs an API key.
func (a *Adapter) RequiresAuth() bool { return true }

// SupportsNameLookup returns true because Genius GetArtist can accept an
// artist name (non-numeric, non-UUID) and will search by name automatically.
func (a *Adapter) SupportsNameLookup() bool { return true }

// SearchArtist searches Genius for artists matching the given name.
// Genius search returns song hits; we extract and deduplicate primary_artist entries.
func (a *Adapter) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	if err := a.limiter.Wait(ctx, provider.NameGenius); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameGenius,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	params := url.Values{"q": {name}}
	reqURL := a.baseURL + "/search?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var resp SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing search response: %w", err)
	}

	// Deduplicate primary artists by ID and compute name similarity scores.
	seen := make(map[int]bool)
	var results []provider.ArtistSearchResult
	for _, hit := range resp.Response.Hits {
		art := hit.Result.PrimaryArtist
		if art.ID == 0 || seen[art.ID] {
			continue
		}
		seen[art.ID] = true
		results = append(results, provider.ArtistSearchResult{
			ProviderID: strconv.Itoa(art.ID),
			Name:       art.Name,
			Score:      provider.NameSimilarity(name, art.Name),
			Source:     string(provider.NameGenius),
		})
	}

	// Sort by score descending so the best match appears first.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results, nil
}

// GetArtist fetches full metadata for an artist. If id is a numeric Genius ID,
// it fetches directly; otherwise it searches by name and uses the top result.
// UUIDs (MusicBrainz IDs) are rejected immediately since Genius cannot use them
// and searching by UUID would always return no results.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if provider.IsUUID(id) {
		return nil, &provider.ErrNotFound{Provider: provider.NameGenius, ID: id}
	}
	if !isNumeric(id) {
		return a.getArtistByName(ctx, id)
	}
	return a.getArtistByID(ctx, id)
}

// GetImages returns nil since Genius does not host artist images.
func (a *Adapter) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// TestConnection verifies the API key is valid by performing a minimal search.
func (a *Adapter) TestConnection(ctx context.Context) error {
	if err := a.limiter.Wait(ctx, provider.NameGenius); err != nil {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameGenius,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}
	params := url.Values{"q": {"test"}}
	reqURL := a.baseURL + "/search?" + params.Encode()
	_, err := a.doRequest(ctx, reqURL)
	return err
}

func (a *Adapter) getArtistByID(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if err := a.limiter.Wait(ctx, provider.NameGenius); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameGenius,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	reqURL := a.baseURL + "/artists/" + url.PathEscape(id) + "?text_format=plain"

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var resp ArtistResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	art := resp.Response.Artist
	if art.Name == "" {
		return nil, &provider.ErrNotFound{Provider: provider.NameGenius, ID: id}
	}

	return mapArtist(&art), nil
}

func (a *Adapter) getArtistByName(ctx context.Context, name string) (*provider.ArtistMetadata, error) {
	results, err := a.SearchArtist(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, &provider.ErrNotFound{Provider: provider.NameGenius, ID: name}
	}
	// SearchArtist returns results sorted descending by score,
	// so the first entry is the best match.
	best := results[0]
	threshold, err := a.getNameSimilarityThreshold(ctx)
	if err != nil {
		return nil, err
	}
	if threshold > 0 && best.Score < threshold {
		a.logger.Warn("rejecting search result: name similarity too low",
			slog.String("search_term", name),
			slog.String("result_name", best.Name),
			slog.Int("similarity", best.Score),
			slog.Int("threshold", threshold),
		)
		return nil, &provider.ErrNotFound{Provider: provider.NameGenius, ID: name}
	}
	return a.getArtistByID(ctx, best.ProviderID)
}

func (a *Adapter) getAPIKey(ctx context.Context) (string, error) {
	apiKey, err := a.settings.GetAPIKey(ctx, provider.NameGenius)
	if err != nil {
		return "", fmt.Errorf("getting API key: %w", err)
	}
	if apiKey == "" {
		return "", &provider.ErrAuthRequired{Provider: provider.NameGenius}
	}
	return apiKey, nil
}

func (a *Adapter) doRequest(ctx context.Context, reqURL string) ([]byte, error) {
	apiKey, err := a.getAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "Stillwater/1.0")
	req.Header.Set("Accept", "application/json")

	a.logger.Debug("requesting", slog.String("url", reqURL))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted base + API params
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameGenius,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrAuthRequired{Provider: provider.NameGenius}
	}
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrNotFound{Provider: provider.NameGenius, ID: reqURL}
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameGenius,
			Cause:    fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	return io.ReadAll(io.LimitReader(resp.Body, 512*1024))
}

func mapArtist(art *ArtistDetail) *provider.ArtistMetadata {
	meta := &provider.ArtistMetadata{
		ProviderID: strconv.Itoa(art.ID),
		Name:       art.Name,
		Biography:  strings.TrimSpace(art.Description.Plain),
		Aliases:    art.AlternateNames,
	}
	if art.URL != "" {
		meta.URLs = map[string]string{"genius": art.URL}
	}
	return meta
}

// isNumeric returns true if s contains only ASCII digits.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
