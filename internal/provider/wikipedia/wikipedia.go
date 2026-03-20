package wikipedia

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
	defaultWikiEndpoint     = "https://en.wikipedia.org/api/rest_v1"
	defaultWikidataEndpoint = "https://query.wikidata.org/sparql"
	userAgent               = "Stillwater/1.0 (https://github.com/sydlexius/stillwater)"
)

// Adapter implements the provider.Provider interface for Wikipedia.
// It fetches artist biographies from Wikipedia article extracts.
//
// The ID must be a MusicBrainz UUID. It is resolved to a Wikipedia article
// title via a Wikidata SPARQL query, then the article extract is fetched.
type Adapter struct {
	client           *http.Client
	limiter          *provider.RateLimiterMap
	logger           *slog.Logger
	wikiEndpoint     string
	wikidataEndpoint string
}

// New creates a Wikipedia adapter with default endpoints.
func New(limiter *provider.RateLimiterMap, logger *slog.Logger) *Adapter {
	return NewWithEndpoints(limiter, logger, defaultWikiEndpoint, defaultWikidataEndpoint)
}

// NewWithEndpoints creates a Wikipedia adapter with custom endpoints (for testing).
func NewWithEndpoints(limiter *provider.RateLimiterMap, logger *slog.Logger, wikiEndpoint, wikidataEndpoint string) *Adapter {
	return &Adapter{
		client:           &http.Client{Timeout: 15 * time.Second},
		limiter:          limiter,
		logger:           logger.With(slog.String("provider", "wikipedia")),
		wikiEndpoint:     wikiEndpoint,
		wikidataEndpoint: wikidataEndpoint,
	}
}

// Name returns the provider name.
func (a *Adapter) Name() provider.ProviderName { return provider.NameWikipedia }

// RequiresAuth returns false; Wikipedia is freely accessible.
func (a *Adapter) RequiresAuth() bool { return false }

// SupportsNameLookup returns false. Wikipedia name search is too fuzzy and
// can return unrelated articles (e.g. "KEDE-R" matching "Nancy Kedersha").
// Only the MBID-to-Wikidata-to-article path is reliable.
func (a *Adapter) SupportsNameLookup() bool { return false }

// SearchArtist is not supported; Wikipedia lookup requires a MusicBrainz ID
// resolved through Wikidata to ensure the correct article is matched.
func (a *Adapter) SearchArtist(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}

// GetArtist fetches biography metadata from Wikipedia.
// The id must be a MusicBrainz UUID; it is resolved to a Wikipedia article
// title via a Wikidata SPARQL query.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if !provider.IsUUID(id) {
		return nil, &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: id}
	}

	if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	title, err := a.resolveFromMBID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Second rate limit wait before the Wikipedia REST API call. The SPARQL
	// endpoint (Wikidata) and the REST API (Wikipedia) are separate services
	// with independent rate limit policies.
	if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	return a.fetchSummary(ctx, title)
}

// GetImages returns nil; Wikipedia is not used for artist images.
func (a *Adapter) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// TestConnection verifies connectivity to the Wikipedia REST API.
func (a *Adapter) TestConnection(ctx context.Context) error {
	if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.wikiEndpoint+"/page/summary/Wikipedia", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted Wikipedia endpoint
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// resolveFromMBID uses a Wikidata SPARQL query to find the English Wikipedia
// article title for an artist identified by MusicBrainz ID.
func (a *Adapter) resolveFromMBID(ctx context.Context, mbid string) (string, error) {
	// SPARQL query: find the English Wikipedia sitelink for this MBID.
	query := fmt.Sprintf(`SELECT ?article WHERE {
  ?item wdt:P434 "%s" .
  ?article schema:about ?item ;
           schema:isPartOf <https://en.wikipedia.org/> .
}`, mbid)

	params := url.Values{
		"query":  {query},
		"format": {"json"},
	}
	reqURL := a.wikidataEndpoint + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("creating SPARQL request: %w", err),
		}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/sparql-results+json")

	a.logger.Debug("resolving MBID to Wikipedia title via Wikidata", slog.String("mbid", mbid))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted SPARQL endpoint
	if err != nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("wikidata SPARQL HTTP %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("reading SPARQL response: %w", err),
		}
	}

	var sparql sparqlResponse
	if err := json.Unmarshal(body, &sparql); err != nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("parsing SPARQL response: %w", err),
		}
	}

	if len(sparql.Results.Bindings) == 0 {
		return "", &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: mbid}
	}

	// Extract article title from the full URL.
	// e.g. "https://en.wikipedia.org/wiki/Noise_Ratchet" -> "Noise_Ratchet"
	articleURL := sparql.Results.Bindings[0].Article.Value
	const wikiPrefix = "/wiki/"
	if idx := strings.LastIndex(articleURL, wikiPrefix); idx >= 0 {
		return articleURL[idx+len(wikiPrefix):], nil
	}

	// The SPARQL query returned a result but the article URL is not in the
	// expected format. Log and return an error so operators can diagnose.
	a.logger.Warn("unexpected article URL format from Wikidata",
		slog.String("mbid", mbid),
		slog.String("url", articleURL))
	return "", &provider.ErrProviderUnavailable{
		Provider: provider.NameWikipedia,
		Cause:    fmt.Errorf("unexpected Wikidata article URL format: %s", articleURL),
	}
}

// fetchSummary fetches the article summary (extract) from the Wikipedia REST API.
func (a *Adapter) fetchSummary(ctx context.Context, title string) (*provider.ArtistMetadata, error) {
	// URL-encode the title for the REST API path.
	encodedTitle := url.PathEscape(title)
	reqURL := a.wikiEndpoint + "/page/summary/" + encodedTitle

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("creating summary request: %w", err),
		}
	}
	req.Header.Set("User-Agent", userAgent)

	a.logger.Debug("fetching Wikipedia summary", slog.String("title", title))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted Wikipedia endpoint
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: title}
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("summary HTTP %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("reading summary response: %w", err),
		}
	}

	var summary summaryResponse
	if err := json.Unmarshal(body, &summary); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("parsing summary response: %w", err),
		}
	}

	if strings.TrimSpace(summary.Extract) == "" {
		return nil, &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: title}
	}

	// Use the display title (cleaned up) as the name.
	name := summary.DisplayName
	if name == "" {
		name = summary.Title
	}
	if name == "" {
		return nil, &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: title}
	}
	// Strip underscores that Wikipedia uses as word separators.
	name = strings.ReplaceAll(name, "_", " ")

	return &provider.ArtistMetadata{
		Name:      name,
		Biography: strings.TrimSpace(summary.Extract),
		URLs: map[string]string{
			"wikipedia": "https://en.wikipedia.org/wiki/" + url.PathEscape(title),
		},
	}, nil
}
