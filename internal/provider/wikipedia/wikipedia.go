package wikipedia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/sydlexius/stillwater/internal/provider"
)

const (
	defaultActionEndpoint      = "https://en.wikipedia.org/w/api.php"
	defaultWikidataEndpoint    = "https://query.wikidata.org/sparql"
	defaultWikidataAPIEndpoint = "https://www.wikidata.org/w/api.php"
	userAgent                  = "Stillwater/1.0 (https://github.com/sydlexius/stillwater)"
)

// Adapter implements the provider.Provider interface for Wikipedia.
// It fetches artist biographies from Wikipedia article extracts and
// structured metadata (members, genres, years active, origin) from
// infobox templates parsed out of article wikitext.
//
// The ID can be a MusicBrainz UUID, a Wikidata Q-ID, or a Wikipedia
// article title. UUIDs are resolved via Wikidata SPARQL, Q-IDs via
// the Wikidata entity API sitelinks.
type Adapter struct {
	client              *http.Client
	limiter             *provider.RateLimiterMap
	logger              *slog.Logger
	actionEndpoint      string // MediaWiki Action API (extracts + wikitext)
	wikidataEndpoint    string // SPARQL (MBID resolution)
	wikidataAPIEndpoint string // Wikidata entity API (Q-ID sitelink resolution)
}

// New creates a Wikipedia adapter with default endpoints.
func New(limiter *provider.RateLimiterMap, logger *slog.Logger) *Adapter {
	return NewWithEndpoints(limiter, logger,
		defaultActionEndpoint, defaultWikidataEndpoint, defaultWikidataAPIEndpoint)
}

// NewWithEndpoints creates a Wikipedia adapter with custom endpoints (for testing).
func NewWithEndpoints(
	limiter *provider.RateLimiterMap,
	logger *slog.Logger,
	actionEndpoint, wikidataEndpoint, wikidataAPIEndpoint string,
) *Adapter {
	return &Adapter{
		client:              &http.Client{Timeout: 15 * time.Second},
		limiter:             limiter,
		logger:              logger.With(slog.String("provider", "wikipedia")),
		actionEndpoint:      actionEndpoint,
		wikidataEndpoint:    wikidataEndpoint,
		wikidataAPIEndpoint: wikidataAPIEndpoint,
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

// GetArtist fetches metadata from Wikipedia for the given ID.
// The ID can be:
//   - A MusicBrainz UUID: resolved to article title via Wikidata SPARQL
//   - A Wikidata Q-ID (e.g. "Q44190"): resolved via Wikidata sitelinks
//   - A Wikipedia article title: used directly
//
// Returns biography text from the article intro section and structured
// metadata (members, genres, years active, origin) from infobox parsing.
// When the user has metadata language preferences set in the context,
// the adapter tries the preferred language's Wikipedia first, falling back
// to English if the preferred language article does not exist.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	// Determine the preferred Wikipedia language from context.
	wikiLang := "en"
	if langPrefs := provider.MetadataLanguages(ctx); len(langPrefs) > 0 {
		base := strings.SplitN(strings.ToLower(langPrefs[0]), "-", 2)[0]
		if len(base) == 2 || len(base) == 3 {
			wikiLang = base
		}
	}

	title, err := a.resolveToTitle(ctx, id)
	if err != nil {
		return nil, err
	}

	// Build a language-specific action endpoint for the preferred language.
	actionEP := a.actionEndpointForLang(wikiLang)

	// Fetch the article intro extract for biography text.
	if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}
	name, extract, err := a.fetchExtractFrom(ctx, actionEP, title)
	if err != nil {
		// If the preferred language wiki returned not-found, fall back to English
		// rather than failing outright. The title from resolveToTitle is an enwiki
		// title and may not exist on the localized wiki.
		var notFound *provider.ErrNotFound
		if wikiLang == "en" || !errors.As(err, &notFound) {
			return nil, err
		}
		extract = "" // trigger the fallback below
	}

	// If the preferred language returned an empty extract and it is not English,
	// fall back to English.
	if strings.TrimSpace(extract) == "" && wikiLang != "en" {
		if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
			return nil, &provider.ErrProviderUnavailable{
				Provider: provider.NameWikipedia,
				Cause:    fmt.Errorf("rate limiter: %w", err),
			}
		}
		wikiLang = "en"
		actionEP = a.actionEndpointForLang("en")
		name, extract, err = a.fetchExtractFrom(ctx, actionEP, title)
		if err != nil {
			return nil, err
		}
	}

	if strings.TrimSpace(extract) == "" {
		return nil, &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: id}
	}
	if name == "" {
		name = strings.ReplaceAll(title, "_", " ")
	}

	meta := &provider.ArtistMetadata{
		ProviderID: title,
		Name:       name,
		Biography:  strings.TrimSpace(extract),
		URLs: map[string]string{
			"wikipedia": "https://" + wikiLang + ".wikipedia.org/wiki/" + url.PathEscape(title),
		},
	}

	// Fetch wikitext for infobox parsing. This is best-effort: if it fails,
	// we still return the biography. However, context cancellation is
	// propagated because it signals the caller wants to stop entirely.
	if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		a.logger.Warn("rate limiter wait failed for wikitext fetch",
			slog.String("title", title), slog.Any("error", err))
		return meta, nil
	}
	wikitext, err := a.fetchWikitext(ctx, title)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		a.logger.Warn("wikitext fetch failed, returning biography only",
			slog.String("title", title), slog.Any("error", err))
		return meta, nil
	}

	infobox := parseInfobox(wikitext)
	if infobox == nil {
		a.logger.Debug("no recognized infobox found in wikitext",
			slog.String("title", title))
		return meta, nil
	}

	if infobox.YearsActive != "" {
		meta.YearsActive = infobox.YearsActive
	}
	if infobox.Origin != "" {
		meta.Country = infobox.Origin
	}
	if len(infobox.Genres) > 0 {
		meta.Genres = infobox.Genres
	}

	// Combine current and past members into the Members slice.
	for _, memberName := range infobox.Members {
		meta.Members = append(meta.Members, provider.MemberInfo{
			Name:     memberName,
			IsActive: true,
		})
	}
	for _, memberName := range infobox.PastMembers {
		meta.Members = append(meta.Members, provider.MemberInfo{
			Name:     memberName,
			IsActive: false,
		})
	}

	return meta, nil
}

// GetImages returns nil; Wikipedia is not used for artist images.
func (a *Adapter) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// TestConnection verifies connectivity to the Wikipedia Action API, the
// Wikidata SPARQL endpoint, and the Wikidata entity API, since GetArtist
// depends on all three services for MBID, Q-ID, and article title lookups.
func (a *Adapter) TestConnection(ctx context.Context) error {
	// Probe 1: Wikipedia Action API
	if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	params := url.Values{
		"action": {"query"},
		"meta":   {"siteinfo"},
		"siprop": {"general"},
		"format": {"json"},
	}
	wikiReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.actionEndpoint+"?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	wikiReq.Header.Set("User-Agent", userAgent)
	wikiResp, err := a.client.Do(wikiReq) //nolint:gosec // URL constructed from trusted endpoint
	if err != nil {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("wikipedia Action API: %w", err),
		}
	}
	defer wikiResp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, wikiResp.Body)
	if wikiResp.StatusCode != http.StatusOK {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("wikipedia Action API HTTP %d", wikiResp.StatusCode),
		}
	}

	// Probe 2: Wikidata SPARQL endpoint
	if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	sparqlQuery := url.Values{
		"query":  {`ASK { wd:Q5 wdt:P31 wd:Q16521 }`},
		"format": {"json"},
	}
	sparqlReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.wikidataEndpoint+"?"+sparqlQuery.Encode(), nil)
	if err != nil {
		return err
	}
	sparqlReq.Header.Set("User-Agent", userAgent)
	sparqlReq.Header.Set("Accept", "application/sparql-results+json")
	sparqlResp, err := a.client.Do(sparqlReq) //nolint:gosec // URL constructed from trusted SPARQL endpoint
	if err != nil {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("wikidata SPARQL: %w", err),
		}
	}
	defer sparqlResp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, sparqlResp.Body)
	if sparqlResp.StatusCode != http.StatusOK {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("wikidata SPARQL HTTP %d", sparqlResp.StatusCode),
		}
	}

	// Probe 3: Wikidata entity API (used for Q-ID sitelink resolution)
	if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	entityParams := url.Values{
		"action": {"wbgetentities"},
		"ids":    {"Q5"},
		"props":  {"sitelinks"},
		"format": {"json"},
	}
	entityReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.wikidataAPIEndpoint+"?"+entityParams.Encode(), nil)
	if err != nil {
		return err
	}
	entityReq.Header.Set("User-Agent", userAgent)
	entityResp, err := a.client.Do(entityReq) //nolint:gosec // URL constructed from trusted Wikidata endpoint
	if err != nil {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("wikidata entity API: %w", err),
		}
	}
	defer entityResp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, entityResp.Body)
	if entityResp.StatusCode != http.StatusOK {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("wikidata entity API HTTP %d", entityResp.StatusCode),
		}
	}

	return nil
}

// resolveToTitle determines the ID type and resolves it to a Wikipedia article title.
func (a *Adapter) resolveToTitle(ctx context.Context, id string) (string, error) {
	switch {
	case provider.IsUUID(id):
		if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
			return "", &provider.ErrProviderUnavailable{
				Provider: provider.NameWikipedia,
				Cause:    fmt.Errorf("rate limiter: %w", err),
			}
		}
		return a.resolveFromMBID(ctx, id)

	case isQID(id):
		if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
			return "", &provider.ErrProviderUnavailable{
				Provider: provider.NameWikipedia,
				Cause:    fmt.Errorf("rate limiter: %w", err),
			}
		}
		return a.resolveFromQID(ctx, id)

	default:
		// Treat as a Wikipedia article title directly.
		return id, nil
	}
}

// isQID returns true if id looks like a Wikidata Q-ID (e.g. "Q44190").
func isQID(id string) bool {
	if len(id) < 2 || (id[0] != 'Q' && id[0] != 'q') {
		return false
	}
	for _, r := range id[1:] {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// resolveFromMBID uses a Wikidata SPARQL query to find the English Wikipedia
// article title for an artist identified by MusicBrainz ID.
func (a *Adapter) resolveFromMBID(ctx context.Context, mbid string) (string, error) {
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

	return extractArticleTitle(sparql.Results.Bindings[0].Article.Value, mbid, a.logger)
}

// resolveFromQID uses the Wikidata entity API to resolve a Q-ID to a
// Wikipedia article title via sitelinks.
func (a *Adapter) resolveFromQID(ctx context.Context, qid string) (string, error) {
	params := url.Values{
		"action":     {"wbgetentities"},
		"ids":        {strings.ToUpper(qid)},
		"props":      {"sitelinks"},
		"sitefilter": {"enwiki"},
		"format":     {"json"},
	}
	reqURL := a.wikidataAPIEndpoint + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("creating Wikidata API request: %w", err),
		}
	}
	req.Header.Set("User-Agent", userAgent)

	a.logger.Debug("resolving Q-ID to Wikipedia title via Wikidata sitelinks", slog.String("qid", qid))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted Wikidata endpoint
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
			Cause:    fmt.Errorf("wikidata API HTTP %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("reading Wikidata API response: %w", err),
		}
	}

	var wbResp wbEntityResponse
	if err := json.Unmarshal(body, &wbResp); err != nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("parsing Wikidata API response: %w", err),
		}
	}

	upperQID := strings.ToUpper(qid)
	entity, ok := wbResp.Entities[upperQID]
	if !ok {
		return "", &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: qid}
	}

	enwiki, ok := entity.Sitelinks["enwiki"]
	if !ok || enwiki.Title == "" {
		return "", &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: qid}
	}

	return enwiki.Title, nil
}

// actionEndpointForLang returns a Wikipedia Action API endpoint for the given
// language code. If the adapter was constructed with a custom (non-default)
// endpoint (e.g. for testing), the custom endpoint is always returned.
func (a *Adapter) actionEndpointForLang(lang string) string {
	// If using the default endpoint, construct a language-specific URL.
	if a.actionEndpoint == defaultActionEndpoint && lang != "" && lang != "en" {
		return "https://" + lang + ".wikipedia.org/w/api.php"
	}
	return a.actionEndpoint
}

// fetchExtractFrom fetches the article intro section from the given endpoint.
// Returns the article display name and plain-text extract.
func (a *Adapter) fetchExtractFrom(ctx context.Context, endpoint, title string) (string, string, error) {
	params := url.Values{
		"action":      {"query"},
		"titles":      {title},
		"prop":        {"extracts"},
		"explaintext": {"true"},
		"exintro":     {"true"},
		"format":      {"json"},
	}
	reqURL := endpoint + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("creating extract request: %w", err),
		}
	}
	req.Header.Set("User-Agent", userAgent)

	a.logger.Debug("fetching Wikipedia extract", slog.String("title", title))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted Wikipedia endpoint
	if err != nil {
		return "", "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", "", &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: title}
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("extract HTTP %d", resp.StatusCode),
		}
	}

	// Intro extracts are typically a few KB but can be larger for well-known artists.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return "", "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("reading extract response: %w", err),
		}
	}

	var extResp extractResponse
	if err := json.Unmarshal(body, &extResp); err != nil {
		return "", "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("parsing extract response: %w", err),
		}
	}

	// The pages map uses page IDs as keys. Extract the first (and only) page.
	for pageID, page := range extResp.Query.Pages {
		if pageID == "-1" {
			return "", "", &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: title}
		}
		name := strings.ReplaceAll(page.Title, "_", " ")
		return name, page.Extract, nil
	}

	return "", "", &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: title}
}

// fetchWikitext fetches the raw wikitext of article section 0 (lead section
// containing the infobox) from the MediaWiki Action API.
func (a *Adapter) fetchWikitext(ctx context.Context, title string) (string, error) {
	params := url.Values{
		"action":  {"parse"},
		"page":    {title},
		"prop":    {"wikitext"},
		"section": {"0"},
		"format":  {"json"},
	}
	reqURL := a.actionEndpoint + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("creating wikitext request: %w", err),
		}
	}
	req.Header.Set("User-Agent", userAgent)

	a.logger.Debug("fetching Wikipedia wikitext", slog.String("title", title))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted Wikipedia endpoint
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
			Cause:    fmt.Errorf("wikitext HTTP %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("reading wikitext response: %w", err),
		}
	}

	var parseResp parseResponse
	if err := json.Unmarshal(body, &parseResp); err != nil {
		return "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("parsing wikitext response: %w", err),
		}
	}

	return parseResp.Parse.Wikitext.Text, nil
}

// extractArticleTitle extracts and URL-decodes the article title from a full
// Wikipedia URL returned by Wikidata SPARQL.
func extractArticleTitle(articleURL, id string, logger *slog.Logger) (string, error) {
	const wikiPrefix = "/wiki/"
	if idx := strings.LastIndex(articleURL, wikiPrefix); idx >= 0 {
		raw := articleURL[idx+len(wikiPrefix):]
		decoded, err := url.PathUnescape(raw)
		if err != nil {
			logger.Warn("failed to unescape article title from Wikidata URL",
				slog.String("id", id),
				slog.String("url", articleURL),
				slog.Any("error", err))
			return "", &provider.ErrProviderUnavailable{
				Provider: provider.NameWikipedia,
				Cause:    fmt.Errorf("invalid percent-encoding in Wikidata article title: %w", err),
			}
		}
		return decoded, nil
	}

	logger.Warn("unexpected article URL format from Wikidata",
		slog.String("id", id),
		slog.String("url", articleURL))
	return "", &provider.ErrProviderUnavailable{
		Provider: provider.NameWikipedia,
		Cause:    fmt.Errorf("unexpected Wikidata article URL format: %s", articleURL),
	}
}
