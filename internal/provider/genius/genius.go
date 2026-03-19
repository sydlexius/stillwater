package genius

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
	"unicode"

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
			Score:      nameSimilarity(name, art.Name),
			Source:     string(provider.NameGenius),
		})
	}
	return results, nil
}

// GetArtist fetches full metadata for an artist. If id is a numeric Genius ID,
// it fetches directly; otherwise it searches by name and uses the top result.
// UUIDs (MusicBrainz IDs) are rejected immediately since Genius cannot use them
// and searching by UUID would always return no results.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if isUUID(id) {
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
	// Pick the highest-scoring result, not the first in API order.
	best := results[0]
	for _, r := range results[1:] {
		if r.Score > best.Score {
			best = r
		}
	}
	if best.Score < minNameSimilarity {
		a.logger.Warn("rejecting search result: name similarity too low",
			slog.String("search_term", name),
			slog.String("result_name", best.Name),
			slog.Int("similarity", best.Score),
			slog.Int("threshold", minNameSimilarity),
		)
		return nil, &provider.ErrNotFound{
			Provider: provider.NameGenius,
			ID:       fmt.Sprintf("%s (best match %q scored %d/%d)", name, best.Name, best.Score, minNameSimilarity),
		}
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

// minNameSimilarity is the threshold (0-100) below which a search result is
// considered a mismatch. Genius search returns song hits, not artist hits, so
// the primary_artist of the top result can be completely unrelated to the
// search term (e.g., searching "Adele" can return Kim Kardashian).
const minNameSimilarity = 60

// nameSimilarity returns a 0-100 score indicating how similar two artist names
// are. The comparison is case-insensitive and strips common prefixes like "The".
func nameSimilarity(a, b string) int {
	// Fast path: case-insensitive exact match before normalization.
	// Handles punctuation-heavy names like "!!!" that normalize to empty.
	if strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) {
		return 100
	}
	a = normalizeName(a)
	b = normalizeName(b)
	if a == b {
		return 100
	}
	if a == "" || b == "" {
		return 0
	}
	ra, rb := []rune(a), []rune(b)
	maxLen := len(ra)
	if len(rb) > maxLen {
		maxLen = len(rb)
	}
	dist := levenshtein(a, b)
	if dist >= maxLen {
		return 0
	}
	return 100 - (dist*100)/maxLen
}

// normalizeName lowercases, strips "the " prefix, and removes non-alphanumeric
// characters for comparison purposes.
func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "the ")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// levenshtein computes the Levenshtein edit distance between two strings.
// It operates on runes so that multi-byte Unicode characters (accented letters,
// CJK, Cyrillic) are counted as single characters.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	// Use a single-row DP approach.
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr := make([]int, len(rb)+1)
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			ins := curr[j-1] + 1
			del := prev[j] + 1
			sub := prev[j-1] + cost
			curr[j] = ins
			if del < curr[j] {
				curr[j] = del
			}
			if sub < curr[j] {
				curr[j] = sub
			}
		}
		prev = curr
	}
	return prev[len(rb)]
}

// isUUID returns true if s looks like a UUID (8-4-4-4-12 hex format).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}
