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

	"github.com/sydlexius/stillwater/internal/httpsafe"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/version"
)

const (
	defaultActionEndpoint      = "https://en.wikipedia.org/w/api.php"
	defaultWikidataEndpoint    = "https://query.wikidata.org/sparql"
	defaultWikidataAPIEndpoint = "https://www.wikidata.org/w/api.php"
)

// userAgent is computed once at package init from the ldflags-injected version.
// var (not const) because it calls a function; package-level vars are fine
// here since version.Version itself is set at build time and never changes.
var userAgent = version.UserAgent("Stillwater", "https://github.com/sydlexius/stillwater")

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
	settings            *provider.SettingsService // nil-safe: nil means use defaults
	actionEndpoint      string                    // MediaWiki Action API (extracts + wikitext)
	wikidataEndpoint    string                    // SPARQL (MBID resolution)
	wikidataAPIEndpoint string                    // Wikidata entity API (Q-ID sitelink resolution)
}

// New creates a Wikipedia adapter with default endpoints.
// settings is used to read field verbosity configuration; pass nil to use
// catalogue defaults (conservative intro-only behavior).
func New(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger) *Adapter {
	return NewWithEndpoints(limiter, settings, logger,
		defaultActionEndpoint, defaultWikidataEndpoint, defaultWikidataAPIEndpoint)
}

// NewWithEndpoints creates a Wikipedia adapter with custom endpoints (for testing).
// settings may be nil; nil means always use the default (intro) verbosity.
func NewWithEndpoints(
	limiter *provider.RateLimiterMap,
	settings *provider.SettingsService,
	logger *slog.Logger,
	actionEndpoint, wikidataEndpoint, wikidataAPIEndpoint string,
) *Adapter {
	return &Adapter{
		client:              httpsafe.SafeClient(15 * time.Second),
		limiter:             limiter,
		settings:            settings,
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

// SearchArtist is a documented no-op for Wikipedia (lookup requires an
// MBID resolved through Wikidata so the correct article is matched).
// Injection is intentionally NOT consulted here; matching the production
// (nil, nil) contract keeps callers that treat known-no-op providers as
// "not supported, skip" on the same code path under the smoke harness.
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
// the adapter walks the full preference list in order: for each language it
// looks up the localized Wikipedia article title via Wikidata sitelinks and
// fetches the extract from that language's Wikipedia. The first language
// that returns a real article wins; missing articles, not-found responses,
// and empty extracts cause a graceful fall-through to the next language.
// English is appended as a fallback when not already present in the
// preference list; if the user has explicitly placed "en" earlier, it
// is tried in that position rather than last.
//
//nolint:gocognit // Resolves an ID to (title, QID), iterates user language preferences with English fallback, and at each language probes extracts, sitelinks, infobox, image, and disambiguation handling; the per-language fallthrough on empty content keeps the policy in one place rather than scattered helpers.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: id}
	}

	// Resolve the input ID to an English-wiki title and, when possible, a
	// Wikidata Q-ID that we can use to look up localized sitelinks.
	enTitle, qid, err := a.resolveToTitleAndQID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Build the ordered language list to try. User preferences first (in order),
	// then "en" as a guaranteed final fallback. Duplicates are removed while
	// preserving order.
	langs := orderedLanguages(provider.MetadataLanguages(ctx))

	var (
		name     string
		extract  string
		wikiLang string
		title    string
		lastErr  error
	)

	for _, lang := range langs {
		// Determine the article title for this language.
		var candidateTitle string
		switch {
		case lang == "en":
			// The enTitle we resolved is already the enwiki title.
			candidateTitle = enTitle
		case qid != "":
			// Look up the localized sitelink via Wikidata.
			localized, err := a.resolveToLocalizedTitle(ctx, qid, lang)
			if err != nil {
				var notFound *provider.ErrNotFound
				if errors.As(err, &notFound) {
					a.logger.Debug("no localized Wikipedia sitelink for language; trying next",
						slog.String("qid", qid),
						slog.String("lang", lang))
					lastErr = err
					continue
				}
				// Provider unavailable or other transport error: record and try next lang.
				a.logger.Debug("localized title lookup failed; trying next language",
					slog.String("qid", qid),
					slog.String("lang", lang),
					slog.Any("error", err))
				lastErr = err
				continue
			}
			candidateTitle = localized
		default:
			// No Q-ID available (input was a plain article title). Only the
			// enwiki title is meaningful here, so skip non-English attempts.
			a.logger.Debug("skipping non-English attempt without Q-ID",
				slog.String("title", enTitle),
				slog.String("lang", lang))
			continue
		}

		// Fetch the article intro extract for biography text.
		actionEP := a.actionEndpointForLang(lang)
		gotName, gotExtract, err := a.fetchExtractFrom(ctx, actionEP, candidateTitle)
		if err != nil {
			var notFound *provider.ErrNotFound
			var unavailable *provider.ErrProviderUnavailable
			if errors.As(err, &notFound) || errors.As(err, &unavailable) {
				a.logger.Debug("extract fetch missed; trying next language",
					slog.String("title", candidateTitle),
					slog.String("lang", lang),
					slog.Any("error", err))
				lastErr = err
				continue
			}
			// Unexpected error type: propagate immediately.
			return nil, err
		}
		if strings.TrimSpace(gotExtract) == "" {
			a.logger.Debug("empty extract; trying next language",
				slog.String("title", candidateTitle),
				slog.String("lang", lang))
			lastErr = &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: candidateTitle}
			continue
		}

		a.logger.Debug("Wikipedia extract hit",
			slog.String("title", candidateTitle),
			slog.String("lang", lang))
		name = gotName
		extract = gotExtract
		wikiLang = lang
		title = candidateTitle
		break
	}

	if strings.TrimSpace(extract) == "" {
		if lastErr != nil {
			return nil, lastErr
		}
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

	// Fetch wikitext for infobox parsing from the English Wikipedia. The
	// infobox parser expects English field names (genre, origin, years_active),
	// so we always use the English endpoint even when the biography came from a
	// localized wiki. This is best-effort: if it fails, we still return the
	// biography. Context cancellation is propagated because it signals the
	// caller wants to stop entirely (handled by the fetchWikitext error check
	// below, which now owns the limiter wait via DoWithRetry).
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
		meta.Origin = infobox.Origin
	}
	if len(infobox.Genres) > 0 {
		meta.Genres = infobox.Genres
	}

	// Map birth/death dates so the years_active synthesis helper can derive a
	// "YYYY-YYYY" range for deceased solo artists whose infobox lacks a
	// literal years_active key. This is the primary fix for #1069: a Wikipedia
	// individual with birth_date + death_date will produce a synthesized
	// years_active in the per-field fetch path via SynthesizeYearsActive.
	if infobox.Born != "" {
		meta.Born = infobox.Born
	}
	if infobox.Died != "" {
		meta.Died = infobox.Died
	}

	// Set meta.Type so SynthesizeYearsActive chooses the correct synthesis
	// path (group vs individual). The vocabulary mirrors the normalized values
	// produced by MusicBrainz's mapArtistType:
	//   "group"  -> uses Formed/Disbanded
	//   "solo"   -> uses Born/Died (individual)
	//
	// Heuristic: if the infobox has members (current or past), it describes a
	// group; if it has birth/death dates (but no members), it describes a solo
	// artist. When neither signal is present, leave Type empty so synthesis
	// does not fire and produces no false positive.
	hasMembers := len(infobox.Members) > 0 || len(infobox.PastMembers) > 0
	hasBirthDeath := infobox.Born != "" || infobox.Died != ""
	if hasMembers {
		meta.Type = "group"
	} else if hasBirthDeath {
		meta.Type = "solo"
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

// GetImages is a documented no-op for Wikipedia (not used for artist
// images). Injection is intentionally NOT consulted here; matching the
// production (nil, nil) contract keeps callers that treat known-no-op
// providers as "not supported, skip" on the same code path under the
// smoke harness.
func (a *Adapter) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// doGet issues a single GET to reqURL, retrying on a rate-limited (429) or
// unavailable (503) response via provider.DoWithRetry. The limiter wait lives
// inside each attempt so retries still respect the per-provider budget. accept,
// when non-empty, sets the Accept header (the User-Agent is always set). The
// caller owns closing the returned body and handling every non-rate-limited
// status (200/404/other); DoWithRetry consumes 429/503.
func (a *Adapter) doGet(ctx context.Context, reqURL, accept string) (*http.Response, error) {
	do := func(ctx context.Context) (*http.Response, error) {
		if err := a.limiter.Wait(ctx, provider.NameWikipedia); err != nil {
			return nil, &provider.ErrProviderUnavailable{
				Provider: provider.NameWikipedia,
				Cause:    fmt.Errorf("rate limiter: %w", err),
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
		if err != nil {
			return nil, &provider.ErrProviderUnavailable{
				Provider: provider.NameWikipedia,
				Cause:    fmt.Errorf("creating request: %w", err),
			}
		}
		req.Header.Set("User-Agent", userAgent)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		return a.client.Do(req)
	}

	resp, err := provider.DoWithRetry(ctx, provider.SystemClock(), provider.NameWikipedia, provider.DefaultRetryPolicy(), do)
	if err != nil {
		var unavailable *provider.ErrProviderUnavailable
		if errors.As(err, &unavailable) {
			return nil, err
		}
		return nil, &provider.ErrProviderUnavailable{Provider: provider.NameWikipedia, Cause: err}
	}
	return resp, nil
}

// TestConnection verifies connectivity to the Wikipedia Action API, the
// Wikidata SPARQL endpoint, and the Wikidata entity API, since GetArtist
// depends on all three services for MBID, Q-ID, and article title lookups.
func (a *Adapter) TestConnection(ctx context.Context) error {
	// Probe 1: Wikipedia Action API
	params := url.Values{
		"action": {"query"},
		"meta":   {"siteinfo"},
		"siprop": {"general"},
		"format": {"json"},
	}
	wikiResp, err := a.doGet(ctx, a.actionEndpoint+"?"+params.Encode(), "")
	if err != nil {
		return err
	}
	defer wikiResp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup
	_, _ = io.Copy(io.Discard, wikiResp.Body)
	if wikiResp.StatusCode != http.StatusOK {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("wikipedia Action API HTTP %d", wikiResp.StatusCode),
		}
	}

	// Probe 2: Wikidata SPARQL endpoint
	sparqlQuery := url.Values{
		"query":  {`ASK { wd:Q5 wdt:P31 wd:Q16521 }`},
		"format": {"json"},
	}
	sparqlResp, err := a.doGet(ctx, a.wikidataEndpoint+"?"+sparqlQuery.Encode(), "application/sparql-results+json")
	if err != nil {
		return err
	}
	defer sparqlResp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup
	_, _ = io.Copy(io.Discard, sparqlResp.Body)
	if sparqlResp.StatusCode != http.StatusOK {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("wikidata SPARQL HTTP %d", sparqlResp.StatusCode),
		}
	}

	// Probe 3: Wikidata entity API (used for Q-ID sitelink resolution)
	entityParams := url.Values{
		"action": {"wbgetentities"},
		"ids":    {"Q5"},
		"props":  {"sitelinks"},
		"format": {"json"},
	}
	entityResp, err := a.doGet(ctx, a.wikidataAPIEndpoint+"?"+entityParams.Encode(), "")
	if err != nil {
		return err
	}
	defer entityResp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup
	_, _ = io.Copy(io.Discard, entityResp.Body)
	if entityResp.StatusCode != http.StatusOK {
		return &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("wikidata entity API HTTP %d", entityResp.StatusCode),
		}
	}

	return nil
}

// resolveToTitleAndQID determines the ID type and resolves it to (enwiki title, Q-ID).
// The Q-ID is returned when available so that callers can later look up localized
// sitelinks for other languages. For plain article-title inputs the Q-ID is "".
func (a *Adapter) resolveToTitleAndQID(ctx context.Context, id string) (string, string, error) {
	switch {
	case provider.IsUUID(id):
		return a.resolveFromMBID(ctx, id)

	case isQID(id):
		title, err := a.resolveFromQID(ctx, id)
		if err != nil {
			return "", "", err
		}
		return title, strings.ToUpper(id), nil

	default:
		// Treat as a Wikipedia article title directly. No Q-ID available.
		return id, "", nil
	}
}

// orderedLanguages builds the language list to attempt. It normalizes each
// preference to a 2- or 3-letter lowercase code, removes duplicates while
// preserving order, and appends "en" at the end if not already present.
func orderedLanguages(prefs []string) []string {
	// Use a small literal preallocation hint. Language preference lists are
	// tiny in practice (typically 1-5 entries), so the slice will rarely need
	// to grow. Avoiding any arithmetic on len(prefs) also keeps CodeQL's
	// "size computation may overflow" rule satisfied.
	const initialHint = 8
	seen := make(map[string]struct{}, initialHint)
	out := make([]string, 0, initialHint)
	for _, p := range prefs {
		base := strings.SplitN(strings.ToLower(strings.TrimSpace(p)), "-", 2)[0]
		if len(base) != 2 && len(base) != 3 {
			continue
		}
		if _, ok := seen[base]; ok {
			continue
		}
		seen[base] = struct{}{}
		out = append(out, base)
	}
	if _, ok := seen["en"]; !ok {
		out = append(out, "en")
	}
	return out
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
// article title and Wikidata Q-ID for an artist identified by MusicBrainz ID.
// The Q-ID is extracted from the bound ?item URI so that callers can look up
// localized sitelinks for other languages.
func (a *Adapter) resolveFromMBID(ctx context.Context, mbid string) (string, string, error) {
	query := fmt.Sprintf(`SELECT ?item ?article WHERE {
  ?item wdt:P434 "%s" .
  ?article schema:about ?item ;
           schema:isPartOf <https://en.wikipedia.org/> .
}`, mbid)

	params := url.Values{
		"query":  {query},
		"format": {"json"},
	}
	reqURL := a.wikidataEndpoint + "?" + params.Encode()

	a.logger.Debug("resolving MBID to Wikipedia title via Wikidata", slog.String("mbid", mbid))

	resp, err := a.doGet(ctx, reqURL, "application/sparql-results+json")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("wikidata SPARQL HTTP %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return "", "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("reading SPARQL response: %w", err),
		}
	}

	var sparql sparqlResponse
	if err := json.Unmarshal(body, &sparql); err != nil {
		return "", "", &provider.ErrProviderUnavailable{
			Provider: provider.NameWikipedia,
			Cause:    fmt.Errorf("parsing SPARQL response: %w", err),
		}
	}

	if len(sparql.Results.Bindings) == 0 {
		return "", "", &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: mbid}
	}

	title, err := extractArticleTitle(sparql.Results.Bindings[0].Article.Value, mbid, a.logger)
	if err != nil {
		return "", "", err
	}
	qid := extractQID(sparql.Results.Bindings[0].Item.Value)
	return title, qid, nil
}

// extractQID pulls the Q-ID out of a Wikidata entity URI such as
// "http://www.wikidata.org/entity/Q44190". Returns "" if the URI does not
// contain a recognizable Q-ID suffix. An empty result is not fatal; callers
// simply skip localized sitelink lookups when the Q-ID is unknown.
func extractQID(entityURI string) string {
	if entityURI == "" {
		return ""
	}
	// Take the segment after the last '/'.
	idx := strings.LastIndex(entityURI, "/")
	if idx < 0 || idx == len(entityURI)-1 {
		return ""
	}
	tail := entityURI[idx+1:]
	if !isQID(tail) {
		return ""
	}
	return strings.ToUpper(tail)
}

// resolveToLocalizedTitle looks up the Wikipedia article title for the given
// Q-ID on a specific language wiki via Wikidata sitelinks. Returns
// provider.ErrNotFound when no sitelink exists for that language.
func (a *Adapter) resolveToLocalizedTitle(ctx context.Context, qid, lang string) (string, error) {
	site := lang + "wiki"
	params := url.Values{
		"action":     {"wbgetentities"},
		"ids":        {strings.ToUpper(qid)},
		"props":      {"sitelinks"},
		"sitefilter": {site},
		"format":     {"json"},
	}
	reqURL := a.wikidataAPIEndpoint + "?" + params.Encode()

	a.logger.Debug("resolving localized Wikipedia title via Wikidata sitelinks",
		slog.String("qid", qid), slog.String("lang", lang))

	resp, err := a.doGet(ctx, reqURL, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

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

	link, ok := entity.Sitelinks[site]
	if !ok || link.Title == "" {
		return "", &provider.ErrNotFound{Provider: provider.NameWikipedia, ID: qid}
	}
	return link.Title, nil
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

	a.logger.Debug("resolving Q-ID to Wikipedia title via Wikidata sitelinks", slog.String("qid", qid))

	resp, err := a.doGet(ctx, reqURL, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

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

// fetchExtractFrom fetches the Wikipedia article text from the given endpoint.
// When the biography verbosity setting is VerbosityIntro (the default),
// the request includes exintro=true so only the lead section is returned.
// When set to VerbosityFull, exintro is omitted and the full article text
// is returned. Existing behavior is preserved when settings is nil.
func (a *Adapter) fetchExtractFrom(ctx context.Context, endpoint, title string) (string, string, error) {
	// Determine whether to request the full article or just the intro.
	// Default to intro (conservative) when settings is unavailable.
	useIntro := true
	if a.settings != nil {
		verbosity, err := a.settings.GetFieldVerbosity(ctx, provider.NameWikipedia, "biography")
		if err != nil {
			// Non-fatal: log and fall back to the conservative default.
			a.logger.Warn("reading biography verbosity setting", "error", err)
		} else if verbosity == provider.VerbosityFull {
			useIntro = false
		}
	}

	params := url.Values{
		"action":      {"query"},
		"titles":      {title},
		"prop":        {"extracts"},
		"explaintext": {"true"},
		"format":      {"json"},
	}
	if useIntro {
		params.Set("exintro", "true")
	}
	reqURL := endpoint + "?" + params.Encode()

	a.logger.Debug("fetching Wikipedia extract", slog.String("title", title))

	resp, err := a.doGet(ctx, reqURL, "")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

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

	a.logger.Debug("fetching Wikipedia wikitext", slog.String("title", title))

	resp, err := a.doGet(ctx, reqURL, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

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
