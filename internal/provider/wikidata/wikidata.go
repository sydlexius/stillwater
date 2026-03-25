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

const (
	defaultEndpoint        = "https://query.wikidata.org/sparql"
	defaultCommonsEndpoint = "https://commons.wikimedia.org/w/api.php"
)

// Adapter implements the provider.Provider interface for Wikidata.
type Adapter struct {
	client          *http.Client
	limiter         *provider.RateLimiterMap
	logger          *slog.Logger
	endpoint        string
	commonsEndpoint string
}

// New creates a Wikidata adapter with the default endpoints.
func New(limiter *provider.RateLimiterMap, logger *slog.Logger) *Adapter {
	return NewWithEndpoints(limiter, logger, defaultEndpoint, defaultCommonsEndpoint)
}

// NewWithEndpoint creates a Wikidata adapter with a custom SPARQL endpoint (for testing).
// The Commons endpoint defaults to the production URL.
func NewWithEndpoint(limiter *provider.RateLimiterMap, logger *slog.Logger, endpoint string) *Adapter {
	return NewWithEndpoints(limiter, logger, endpoint, defaultCommonsEndpoint)
}

// NewWithEndpoints creates a Wikidata adapter with custom SPARQL and Commons
// endpoints. Use this in tests that need to mock both APIs.
func NewWithEndpoints(limiter *provider.RateLimiterMap, logger *slog.Logger, endpoint, commonsEndpoint string) *Adapter {
	return &Adapter{
		client:          &http.Client{Timeout: 15 * time.Second},
		limiter:         limiter,
		logger:          logger.With(slog.String("provider", "wikidata")),
		endpoint:        endpoint,
		commonsEndpoint: commonsEndpoint,
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
	// Validate MBID format before interpolating into SPARQL query to prevent injection.
	if !provider.IsUUID(mbid) {
		return nil, &provider.ErrNotFound{Provider: provider.NameWikidata, ID: mbid}
	}

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

// GetImages fetches artist images from Wikimedia Commons via Wikidata properties
// P18 (image/photo) and P154 (logo). The SPARQL query returns Commons filenames
// which are then resolved to direct URLs via the Wikimedia Commons API.
func (a *Adapter) GetImages(ctx context.Context, mbid string) ([]provider.ImageResult, error) {
	// Validate MBID format before interpolating into SPARQL query to prevent injection.
	if !provider.IsUUID(mbid) {
		return nil, &provider.ErrNotFound{Provider: provider.NameWikidata, ID: mbid}
	}

	if err := a.limiter.Wait(ctx, provider.NameWikidata); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikidata,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	query := buildImageQuery(mbid)
	bindings, err := a.executeSPARQL(ctx, query)
	if err != nil {
		return nil, err
	}

	if len(bindings) == 0 {
		return nil, &provider.ErrNotFound{Provider: provider.NameWikidata, ID: mbid}
	}

	// Collect unique filenames from P18 (image) and P154 (logo) properties.
	// The SPARQL response may contain multiple rows if both properties exist.
	// Dedupe by (imageType, filename) so the same Commons file can appear as
	// both a thumb (P18) and a logo (P154) without the second being dropped.
	type imageEntry struct {
		filename string
		imgType  provider.ImageType
	}
	seen := make(map[string]bool)
	var entries []imageEntry

	for _, b := range bindings {
		if b.Image.Value != "" {
			fn := extractCommonsFilename(b.Image.Value)
			key := string(provider.ImageThumb) + ":" + fn
			if fn != "" && !seen[key] {
				seen[key] = true
				entries = append(entries, imageEntry{filename: fn, imgType: provider.ImageThumb})
			}
		}
		if b.Logo.Value != "" {
			fn := extractCommonsFilename(b.Logo.Value)
			key := string(provider.ImageLogo) + ":" + fn
			if fn != "" && !seen[key] {
				seen[key] = true
				entries = append(entries, imageEntry{filename: fn, imgType: provider.ImageLogo})
			}
		}
	}

	if len(entries) == 0 {
		return nil, &provider.ErrNotFound{Provider: provider.NameWikidata, ID: mbid}
	}

	// Resolve each filename to a direct URL via the Commons API.
	// Track whether any resolution attempt returned an error so we can
	// distinguish "no images found" from "all resolutions failed."
	var results []provider.ImageResult
	var hadResolveErrors bool
	for _, e := range entries {
		if err := a.limiter.Wait(ctx, provider.NameWikidata); err != nil {
			return nil, &provider.ErrProviderUnavailable{
				Provider: provider.NameWikidata,
				Cause:    fmt.Errorf("rate limiter: %w", err),
			}
		}

		info, err := a.resolveCommonsURL(ctx, e.filename)
		if err != nil {
			hadResolveErrors = true
			a.logger.Warn("failed to resolve commons image",
				slog.String("filename", e.filename),
				slog.String("error", err.Error()),
			)
			continue
		}
		if info == nil {
			continue
		}

		results = append(results, provider.ImageResult{
			URL:    info.URL,
			Type:   e.imgType,
			Width:  info.Width,
			Height: info.Height,
			Source: string(provider.NameWikidata),
		})
	}

	if len(results) == 0 {
		// If resolution errors occurred, the failure is transient, not "not found."
		if hadResolveErrors {
			return nil, &provider.ErrProviderUnavailable{
				Provider: provider.NameWikidata,
				Cause:    fmt.Errorf("commons image resolution failed for all candidates"),
			}
		}
		return nil, &provider.ErrNotFound{Provider: provider.NameWikidata, ID: mbid}
	}

	return results, nil
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
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikidata,
			Cause:    fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
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

// buildImageQuery creates a SPARQL query that fetches P18 (image) and P154 (logo)
// properties for an artist identified by their MusicBrainz ID (P434).
func buildImageQuery(mbid string) string {
	return fmt.Sprintf(`
SELECT ?image ?logo WHERE {
  ?item wdt:P434 "%s" .
  OPTIONAL { ?item wdt:P18 ?image . }
  OPTIONAL { ?item wdt:P154 ?logo . }
}`, mbid)
}

// extractCommonsFilename extracts the filename from a Wikimedia Commons URI.
// Wikidata returns image values as full URIs like:
//
//	http://commons.wikimedia.org/wiki/Special:FilePath/Radiohead.jpg
//
// This function extracts "Radiohead.jpg" from such a URI. The path tail is
// URL-decoded to prevent double percent-encoding when passed to url.Values.
// Any leading "File:" prefix is stripped since the Commons API adds it itself.
func extractCommonsFilename(uri string) string {
	name := uri
	if idx := strings.LastIndex(uri, "/"); idx >= 0 {
		name = uri[idx+1:]
	}
	// URL-decode percent-encoded characters (e.g. "Band%20Logo.png" -> "Band Logo.png").
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	// Strip "File:" prefix if present; the Commons API prepends it in the titles parameter.
	name = strings.TrimPrefix(name, "File:")
	return name
}

// resolveCommonsURL fetches image metadata (direct URL, dimensions) from the
// Wikimedia Commons API for a given filename. Returns nil if the file is not found.
func (a *Adapter) resolveCommonsURL(ctx context.Context, filename string) (*CommonsImageInfo, error) {
	params := url.Values{
		"action": {"query"},
		"titles": {"File:" + filename},
		"prop":   {"imageinfo"},
		"iiprop": {"url|size"},
		"format": {"json"},
	}
	reqURL := a.commonsEndpoint + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating commons request: %w", err)
	}
	req.Header.Set("User-Agent", "Stillwater/1.0 (https://github.com/sydlexius/stillwater)")

	a.logger.Debug("resolving commons image", slog.String("filename", filename))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted Commons endpoint
	if err != nil {
		return nil, fmt.Errorf("commons request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("commons HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, fmt.Errorf("reading commons response: %w", err)
	}

	var commonsResp CommonsResponse
	if err := json.Unmarshal(body, &commonsResp); err != nil {
		return nil, fmt.Errorf("parsing commons response: %w", err)
	}

	// The response has pages keyed by page ID. A missing page has ID -1.
	for pageID, page := range commonsResp.Query.Pages {
		if pageID == "-1" {
			continue
		}
		if len(page.ImageInfo) > 0 {
			return &page.ImageInfo[0], nil
		}
	}

	return nil, nil
}
