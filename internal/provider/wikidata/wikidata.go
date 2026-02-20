package wikidata

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

const defaultEndpoint = "https://query.wikidata.org/sparql"

// Adapter implements the provider.Provider interface for Wikidata.
type Adapter struct {
	client   *http.Client
	limiter  *provider.RateLimiterMap
	logger   *slog.Logger
	endpoint string
}

// New creates a Wikidata adapter with the default endpoint.
func New(limiter *provider.RateLimiterMap, logger *slog.Logger) *Adapter {
	return NewWithEndpoint(limiter, logger, defaultEndpoint)
}

// NewWithEndpoint creates a Wikidata adapter with a custom endpoint (for testing).
func NewWithEndpoint(limiter *provider.RateLimiterMap, logger *slog.Logger, endpoint string) *Adapter {
	return &Adapter{
		client:   &http.Client{Timeout: 15 * time.Second},
		limiter:  limiter,
		logger:   logger.With(slog.String("provider", "wikidata")),
		endpoint: endpoint,
	}
}

// Name returns the provider name.
func (a *Adapter) Name() provider.ProviderName { return provider.NameWikidata }

// RequiresAuth returns whether this provider needs an API key.
func (a *Adapter) RequiresAuth() bool { return false }

// SearchArtist is not directly supported by Wikidata SPARQL (use GetArtist with MBID instead).
func (a *Adapter) SearchArtist(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}

// GetArtist fetches metadata for an artist from Wikidata by their MusicBrainz ID.
func (a *Adapter) GetArtist(ctx context.Context, mbid string) (*provider.ArtistMetadata, error) {
	if err := a.limiter.Wait(ctx, provider.NameWikidata); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikidata,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	query := buildArtistQuery(mbid)
	bindings, err := a.executeSPARQL(ctx, query)
	if err != nil {
		return nil, err
	}

	if len(bindings) == 0 {
		return nil, &provider.ErrNotFound{Provider: provider.NameWikidata, ID: mbid}
	}

	return mapArtist(mbid, bindings), nil
}

// GetImages returns nil since Wikidata does not host artist images.
func (a *Adapter) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// TestConnection verifies connectivity to the Wikidata SPARQL endpoint.
func (a *Adapter) TestConnection(ctx context.Context) error {
	query := `SELECT ?item WHERE { ?item wdt:P31 wd:Q5 } LIMIT 1`
	_, err := a.executeSPARQL(ctx, query)
	return err
}

func (a *Adapter) executeSPARQL(ctx context.Context, query string) ([]SPARQLBinding, error) {
	params := url.Values{
		"query":  {query},
		"format": {"json"},
	}
	reqURL := a.endpoint + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", "Stillwater/1.0 (https://github.com/sydlexius/stillwater)")
	req.Header.Set("Accept", "application/sparql-results+json")

	a.logger.Debug("executing SPARQL query")

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted SPARQL endpoint
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikidata,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikidata,
			Cause:    fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var sparqlResp SPARQLResponse
	if err := json.Unmarshal(body, &sparqlResp); err != nil {
		return nil, fmt.Errorf("parsing SPARQL response: %w", err)
	}

	return sparqlResp.Results.Bindings, nil
}

func buildArtistQuery(mbid string) string {
	return fmt.Sprintf(`
SELECT ?item ?itemLabel ?inception ?dissolved ?countryLabel ?genreLabel WHERE {
  ?item wdt:P434 "%s" .
  OPTIONAL { ?item wdt:P571 ?inception . }
  OPTIONAL { ?item wdt:P576 ?dissolved . }
  OPTIONAL { ?item wdt:P495 ?country . }
  OPTIONAL { ?item wdt:P136 ?genre . }
  SERVICE wikibase:label { bd:serviceParam wikibase:language "en" . }
}`, mbid)
}

func mapArtist(mbid string, bindings []SPARQLBinding) *provider.ArtistMetadata {
	meta := &provider.ArtistMetadata{
		MusicBrainzID: mbid,
	}

	// Extract the Wikidata Q-ID and label from the first binding
	if len(bindings) > 0 {
		first := bindings[0]
		meta.WikidataID = extractQID(first.Item.Value)
		meta.ProviderID = meta.WikidataID
		meta.Name = first.ItemLabel.Value

		if first.Inception.Value != "" {
			meta.Formed = extractYear(first.Inception.Value)
		}
		if first.Dissolved.Value != "" {
			meta.Disbanded = extractYear(first.Dissolved.Value)
		}
		if first.Country.Value != "" {
			meta.Country = first.Country.Value
		}
	}

	// Collect unique genres from all bindings
	seen := make(map[string]bool)
	for _, b := range bindings {
		genre := b.Genre.Value
		if genre != "" && !seen[genre] {
			seen[genre] = true
			meta.Genres = append(meta.Genres, genre)
		}
	}

	return meta
}

// extractQID extracts the Q-item ID from a full Wikidata URI.
// e.g. "http://www.wikidata.org/entity/Q44190" -> "Q44190"
func extractQID(uri string) string {
	if idx := strings.LastIndex(uri, "/"); idx >= 0 {
		return uri[idx+1:]
	}
	return uri
}

// extractYear extracts the year from a date string.
// e.g. "1985-01-01T00:00:00Z" -> "1985"
func extractYear(date string) string {
	if idx := strings.Index(date, "-"); idx > 0 {
		return date[:idx]
	}
	return date
}
