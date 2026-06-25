package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/httpsafe"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/tagclass"
	"github.com/sydlexius/stillwater/internal/version"
)

const defaultBaseURL = "https://musicbrainz.org/ws/2"

// mirrorAllowedHost returns the lowercased hostname of baseURL when it is a
// custom mirror (its trimmed form differs from defaultBaseURL), and "" for the
// public default or an unparsable URL. A non-empty result is the single
// operator-configured host that newMirrorClient exempts from the SSRF guard, so
// a self-hosted mirror on a LAN/loopback address is reachable. The default
// endpoint and the auto-fallback host (always defaultBaseURL) therefore stay
// fully guarded -- they never match this branch.
func mirrorAllowedHost(baseURL string) string {
	if strings.TrimRight(baseURL, "/") == defaultBaseURL {
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// newMirrorClient builds the adapter's HTTP client for baseURL. A custom mirror
// gets a client that allowlists the mirror's host so its private/LAN address is
// reachable (the admin-typed base URL is itself the opt-in); the public default
// and the auto-fallback target get the plain SSRF-guarded client. The 10s
// timeout matches the original construction.
func newMirrorClient(baseURL string) *http.Client {
	if host := mirrorAllowedHost(baseURL); host != "" {
		return httpsafe.SafeClientWithAllowedHosts(10*time.Second, host)
	}
	return httpsafe.SafeClient(10 * time.Second)
}

// Adapter implements the provider.Provider interface for MusicBrainz.
type Adapter struct {
	client  *http.Client
	limiter *provider.RateLimiterMap
	logger  *slog.Logger
	mu      sync.RWMutex
	baseURL string
	// fallbackURL is the retry target when a configured mirror returns an
	// unparsable body. It is set once at construction (to defaultBaseURL) and
	// never mutated outside tests, so unlike baseURL it needs no mu guard.
	fallbackURL string
}

// New creates a MusicBrainz adapter with the default base URL.
func New(limiter *provider.RateLimiterMap, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, logger, defaultBaseURL)
}

// NewWithBaseURL creates a MusicBrainz adapter with a custom base URL (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, logger *slog.Logger, baseURL string) *Adapter {
	return &Adapter{
		client:      newMirrorClient(baseURL),
		limiter:     limiter,
		logger:      logger.With(slog.String("provider", "musicbrainz")),
		baseURL:     strings.TrimRight(baseURL, "/"),
		fallbackURL: defaultBaseURL,
	}
}

// Name returns the provider name.
func (a *Adapter) Name() provider.ProviderName { return provider.NameMusicBrainz }

// RequiresAuth returns whether this provider needs an API key.
func (a *Adapter) RequiresAuth() bool { return false }

// SearchArtist searches MusicBrainz for artists matching the given name.
func (a *Adapter) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	params := url.Values{
		"query": {name},
		"fmt":   {"json"},
		"limit": {"25"},
	}
	a.mu.RLock()
	base := a.baseURL
	a.mu.RUnlock()
	reqURL := base + "/artist?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var resp SearchResponse
	if err := a.unmarshalWithFallback(ctx, base, "/artist?"+params.Encode(), body, &resp); err != nil {
		return nil, fmt.Errorf("parsing search response: %w", err)
	}

	results := make([]provider.ArtistSearchResult, 0, len(resp.Artists))
	for i := range resp.Artists {
		a := &resp.Artists[i]
		// Use the higher of the API's native score and our name similarity
		// score. The API score reflects relevance factors beyond name matching
		// (popularity, tag matches), while name similarity catches cases where
		// the API underscores an exact or near-exact name match.
		score := a.Score
		if ns := provider.NameSimilarity(name, a.Name); ns > score {
			score = ns
		}
		origin := a.Area.Name
		if origin == "" {
			origin = a.Country
		}
		results = append(results, provider.ArtistSearchResult{
			ProviderID:     a.ID,
			Name:           a.Name,
			SortName:       a.SortName,
			Type:           a.Type,
			Disambiguation: a.Disambiguation,
			Origin:         origin,
			Score:          score,
			MusicBrainzID:  a.ID,
			Source:         string(provider.NameMusicBrainz),
		})
	}

	// Sort by score descending so the best match appears first.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results, nil
}

// GetArtist fetches full metadata for an artist by their MusicBrainz ID.
func (a *Adapter) GetArtist(ctx context.Context, mbid string) (*provider.ArtistMetadata, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	params := url.Values{
		"inc": {"aliases+genres+tags+ratings+url-rels+artist-rels"},
		"fmt": {"json"},
	}
	a.mu.RLock()
	base := a.baseURL
	a.mu.RUnlock()
	reqURL := base + "/artist/" + url.PathEscape(mbid) + "?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return nil, err
	}

	var mbArtist MBArtist
	if err := a.unmarshalWithFallback(ctx, base, "/artist/"+url.PathEscape(mbid)+"?"+params.Encode(), body, &mbArtist); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	meta := a.mapArtist(ctx, &mbArtist)

	// Localize member names from each member's primary aliases when the user
	// has set metadata language preferences. MusicBrainz omits aliases from
	// embedded relation artists, so a per-member lookup is required. Failures
	// are logged and the canonical (non-localized) name is retained so a single
	// member fetch error cannot block the artist refresh.
	if langPrefs := provider.MetadataLanguages(ctx); len(langPrefs) > 0 && len(meta.Members) > 0 {
		a.localizeMembers(ctx, meta.Members, langPrefs, newMemberAliasCache())
	}

	return meta, nil
}

// memberAliasLookup is the per-member data we need from a MusicBrainz artist
// lookup for localization: the alias list (for tagged-alias promotion) and
// the sort-name (for romanization fallback when no alias wins).
type memberAliasLookup struct {
	aliases  []MBAlias
	sortName string
}

// memberAliasCache is an in-memory, per-request cache of member MBID to
// lookup results. It avoids refetching the same member when the same MBID
// appears across multiple code paths during a single artist refresh.
type memberAliasCache struct {
	entries map[string]memberAliasLookup
}

func newMemberAliasCache() *memberAliasCache {
	return &memberAliasCache{entries: make(map[string]memberAliasLookup)}
}

// localizeMembers promotes each member's name to its best-matching alias for
// the configured language preferences. Members without an MBID, or whose
// alias fetch fails, retain their canonical name. The shared rate limiter
// ensures MusicBrainz's 1 req/sec policy is honored even when many members
// are resolved in sequence.
//
// Optimization: when a member's canonical name is already in a script
// satisfying the user's TOP language preference AND that preference expects
// only non-Latin scripts, the alias fetch is skipped. The non-Latin gate is
// required because Latin-family prefs (en, es, de, fr, sr, ...) can still
// benefit from alias promotion even when the canonical is already Latin:
// MusicBrainz aliases carry typography, capitalization, and spelling
// refinements that a Latin-form canonical may lack (issue #1137). Matching
// only the first preference (not the full list) preserves the opportunity to
// promote a higher-preference alias when the canonical happens to match a
// secondary preference. For a band whose roster mostly shares the
// canonical-name script with a non-Latin top preference (a Japanese band
// under a [ja, ...] preference, for example), this still skips most members
// and preserves the 1 req/sec rate-limit win the optimization was built for.
func (a *Adapter) localizeMembers(ctx context.Context, members []provider.MemberInfo, langPrefs []string, cache *memberAliasCache) {
	topPref := ""
	if len(langPrefs) > 0 {
		topPref = langPrefs[0]
	}

	for i := range members {
		m := &members[i]
		mbid := m.MBID
		if mbid == "" {
			continue
		}

		// Skip alias fetch when the canonical name is already in a script
		// that matches the user's top language preference AND that preference
		// is non-Latin-only. The LocaleExpectsOnlyNonLatinScript guard keeps
		// Latin-family prefs out of the skip path so typography/spelling
		// refinements in Latin aliases can still be promoted (#1137).
		if topPref != "" &&
			provider.LocaleExpectsOnlyNonLatinScript(topPref) &&
			provider.ScriptSatisfiesLocale(m.Name, []string{topPref}) {
			a.logger.Debug("skipping member alias fetch (canonical already in preferred non-Latin script)",
				slog.String("member_mbid", mbid),
				slog.String("member_name", m.Name),
				slog.String("top_pref", topPref))
			continue
		}

		lookup, cached := cache.entries[mbid]
		if !cached {
			fetched, err := a.fetchMemberAliases(ctx, mbid)
			if err != nil {
				a.logger.Warn("member alias fetch failed; retaining canonical name",
					slog.String("member_mbid", mbid),
					slog.String("member_name", m.Name),
					slog.Any("err", err))
				// Cache the failure as a zero-value lookup so repeated members
				// with the same MBID do not trigger redundant fetches.
				cache.entries[mbid] = memberAliasLookup{}
				continue
			}
			cache.entries[mbid] = fetched
			lookup = fetched
		}

		// Try tagged-alias promotion first: a curator-added alias (especially a
		// primary one) carries stronger intent than a sort-name heuristic.
		if bestAlias, ok := selectMemberAlias(lookup.aliases, langPrefs); ok {
			promoted := normalizeHyphens(bestAlias.Name)
			if promoted != "" && promoted != m.Name {
				a.logger.Debug("promoting localized member name",
					slog.String("from", m.Name),
					slog.String("to", promoted),
					slog.String("locale", bestAlias.Locale),
					slog.String("type", bestAlias.Type),
					slog.Bool("primary", bestAlias.Primary))
				m.Name = promoted
				continue
			}
		}

		// Fall back to the MB sort-name when no alias wins. MusicBrainz
		// stores the curator-canonical romanization in sort-name for
		// non-Latin artists even when they do not carry an explicit en
		// alias (the common case: most Japanese band rosters on MB).
		// Gating (all seven must hold; every one prevents a specific
		// false-positive promotion):
		//   1. a top language preference must be set -- without one we
		//      have no basis for choosing a script,
		//   2. sort-name must be non-empty -- an empty MB sort-name carries
		//      no signal,
		//   3. canonical must be in a non-Latin script -- reversing a
		//      Latin-to-Latin sort-name would just flip first/last for
		//      Western artists (e.g. "Chris Martin" -> "Chris Martin"),
		//   4. canonical script must not be Unknown -- DominantScript
		//      returns Unknown for ambiguous/degenerate inputs, where a
		//      best-guess promotion would be unsafe,
		//   5. sort-name must itself be in Latin script -- MB occasionally
		//      stores a non-Latin sort-name (e.g. "姓, 名"), in which case
		//      the reversal heuristic does not produce a Latin result, and
		//   6. the top pref must accept Latin -- rules out ja, zh, ko, etc.
		//      where the user explicitly asked for the non-Latin form.
		//   7. the metadata_name_romanization_fallback preference must be
		//      enabled (defaults to true when unset, so existing paths are
		//      unaffected; users can opt out via the Providers settings tab).
		if topPref != "" && lookup.sortName != "" && provider.NameRomanizationFallback(ctx) {
			canonicalScript := provider.DominantScript(m.Name)
			sortScript := provider.DominantScript(lookup.sortName)
			if canonicalScript != provider.ScriptLatin &&
				canonicalScript != provider.ScriptUnknown &&
				sortScript == provider.ScriptLatin &&
				provider.ScriptSatisfiesLocale(lookup.sortName, []string{topPref}) {
				reversed, ok := romanizeFromSortName(lookup.sortName)
				candidate := normalizeHyphens(reversed)
				if ok && candidate != "" && candidate != m.Name {
					a.logger.Debug("promoting member name from MB sort-name",
						slog.String("from", m.Name),
						slog.String("to", candidate),
						slog.String("sort_name", lookup.sortName),
						slog.String("top_pref", topPref))
					m.Name = candidate
				}
			}
		}
	}
}

// selectMemberAlias picks the alias that should replace a member's canonical
// name based on the user's language preferences. Returns the zero value and
// false when no alias is a valid promotion target.
//
// Selection rules:
//
//  1. "Legal name" aliases are excluded. MusicBrainz uses this type to
//     distinguish birth/legal names from stage names; surfacing them without
//     explicit user consent is a privacy concern and they are rarely the
//     intended localization target. Observed during #952 UAT on GAMO
//     (3e959bbb): JA primary "Legal name" (蒲生俊貴) coexists with a JA
//     non-primary "Artist name" (ガモー); the stage name is the desired
//     localization, not the legal name.
//
//  2. Composite score, lower is better. Locale-rank dominates; primary
//     status only breaks ties between aliases at the same locale rank:
//
//     score = locale_rank * 2 + (0 if primary else 1)
//
//     where locale_rank is the integer returned by
//     provider.MatchLanguagePreference (see that function for exact-vs-base
//     semantics). The doubling reserves the low bit for the primary flag so
//     a primary alias always beats a non-primary one at the same locale
//     rank, while a top-pref non-primary still outranks a second-pref
//     primary -- honoring the user's language ordering even when MB tagged
//     the locale-matched alias as non-primary.
//
// Aliases with no locale, an unmapped locale, or an empty Name are skipped
// (locale_rank < 0). Ties at the same final score resolve to the first
// alias encountered, preserving caller-visible determinism for a given
// MB response order.
func selectMemberAlias(aliases []MBAlias, langPrefs []string) (MBAlias, bool) {
	bestScore := -1
	var best MBAlias
	for _, al := range aliases {
		if al.Name == "" {
			continue
		}
		if strings.EqualFold(al.Type, "Legal name") {
			continue
		}
		rank := provider.MatchLanguagePreference(al.Locale, langPrefs)
		if rank < 0 {
			continue
		}
		score := rank * 2
		if !al.Primary {
			score++
		}
		if bestScore < 0 || score < bestScore {
			bestScore = score
			best = al
		}
	}
	if bestScore < 0 {
		return MBAlias{}, false
	}
	return best, true
}

// fetchMemberAliases retrieves the alias list and sort-name for a given
// member MBID. Uses the shared rate-limited doRequest path so the
// MusicBrainz 1 req/sec policy is enforced across all provider traffic.
// Returns sort-name alongside aliases so localizeMembers can fall back to
// the MB-published romanization for non-Latin canonical names when no
// tagged alias is available (many MB artists have a Latin sort-name but no
// explicit en alias).
func (a *Adapter) fetchMemberAliases(ctx context.Context, mbid string) (memberAliasLookup, error) {
	params := url.Values{
		"inc": {"aliases"},
		"fmt": {"json"},
	}
	a.mu.RLock()
	base := a.baseURL
	a.mu.RUnlock()
	reqURL := base + "/artist/" + url.PathEscape(mbid) + "?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return memberAliasLookup{}, err
	}

	var resp MBArtist
	if err := a.unmarshalWithFallback(ctx, base, "/artist/"+url.PathEscape(mbid)+"?"+params.Encode(), body, &resp); err != nil {
		return memberAliasLookup{}, fmt.Errorf("parsing member alias response: %w", err)
	}
	return memberAliasLookup{aliases: resp.Aliases, sortName: resp.SortName}, nil
}

// romanizeFromSortName reverses a MusicBrainz sort-name from the curator
// convention "Family, Given" to the display convention "Given Family".
// MusicBrainz stores sort-name as the curator's canonical sort form; for
// Japanese, Chinese, Korean, and other non-Latin-script artists it is
// almost always a Latin romanization even when no en alias exists. This
// lets us surface a usable Latin display name for Latin-family prefs
// without requiring every MB entry to carry a dedicated alias.
//
// Returns (reversed, true) only when the sort-name is a well-formed
// two-part "Family, Given" with both parts non-empty. Any other shape
// (empty, whitespace-only, single token, multi-comma, missing family or
// given) returns ("", false) so the caller can treat malformed inputs as
// "no fallback available" rather than promoting a raw sort-form like
// "Smith, Jr., John" or "Family," into a display name.
func romanizeFromSortName(sortName string) (string, bool) {
	trimmed := strings.TrimSpace(sortName)
	if trimmed == "" {
		return "", false
	}
	parts := strings.Split(trimmed, ",")
	if len(parts) != 2 {
		return "", false
	}
	family := strings.TrimSpace(parts[0])
	given := strings.TrimSpace(parts[1])
	if family == "" || given == "" {
		return "", false
	}
	return given + " " + family, true
}

// GetImages is a documented no-op for MusicBrainz (no image hosting).
// Injection is intentionally NOT consulted here; matching the production
// (nil, nil) contract keeps callers that treat known-no-op providers as
// "not supported, skip" on the same code path under the smoke harness.
func (a *Adapter) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// GetReleaseGroups fetches release groups (albums, EPs, singles) for an artist by MBID.
// Results are paginated in batches of 100 and capped at 500 total to avoid
// runaway loops on prolific artists (classical composers, etc.).
func (a *Adapter) GetReleaseGroups(ctx context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	const (
		pageSize = 100
		maxTotal = 500
	)

	var results []provider.ReleaseGroupInfo
	offset := 0

	for {
		params := url.Values{
			"artist": {mbid},
			"type":   {"album|ep|single"},
			"limit":  {fmt.Sprintf("%d", pageSize)},
			"offset": {fmt.Sprintf("%d", offset)},
			"fmt":    {"json"},
		}
		a.mu.RLock()
		base := a.baseURL
		a.mu.RUnlock()
		reqURL := base + "/release-group?" + params.Encode()

		body, err := a.doRequest(ctx, reqURL)
		if err != nil {
			return nil, err
		}

		var resp MBReleaseGroupSearchResponse
		if err := a.unmarshalWithFallback(ctx, base, "/release-group?"+params.Encode(), body, &resp); err != nil {
			return nil, fmt.Errorf("parsing release-group response: %w", err)
		}

		for _, rg := range resp.ReleaseGroups {
			results = append(results, provider.ReleaseGroupInfo{
				ID:               rg.ID,
				Title:            rg.Title,
				PrimaryType:      rg.PrimaryType,
				FirstReleaseDate: rg.FirstReleaseDate,
			})
		}

		// Stop when we received fewer results than the page size (last page),
		// or we have collected all available release groups, or we hit the cap.
		if len(resp.ReleaseGroups) < pageSize ||
			len(results) >= resp.ReleaseGroupCount ||
			len(results) >= maxTotal {
			break
		}

		offset += pageSize
	}

	return results, nil
}

// TestConnection verifies connectivity to the MusicBrainz API and validates
// that the endpoint returns well-formed JSON in the expected search response
// shape. This catches a common mirror misconfiguration where the server
// returns a 200 OK with an HTML error page instead of JSON. The existing
// POST /api/v1/providers/{name}/test endpoint calls this method, so a parse
// failure here flows directly to the Settings > Providers Test button UI --
// no additional route is needed (enhancement 2: extending the test path is
// preferred over adding a separate health endpoint).
func (a *Adapter) TestConnection(ctx context.Context) error {
	params := url.Values{
		"query": {"test"},
		"fmt":   {"json"},
		"limit": {"1"},
	}
	a.mu.RLock()
	base := a.baseURL
	a.mu.RUnlock()
	reqURL := base + "/artist?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL)
	if err != nil {
		return err
	}

	// Validate that the response body is well-formed JSON matching the search
	// response shape. A mirror returning an HTML error page (e.g. a 404/503
	// page proxied as 200 OK) would fail here with a descriptive error, whereas
	// the previous implementation discarded the body and reported success.
	var resp SearchResponse
	if err := a.unmarshalResponse(base, body, &resp); err != nil {
		return fmt.Errorf("endpoint returned non-JSON response (check mirror configuration): %w", err)
	}
	// Shape check: a genuine MusicBrainz ws/2 search response always carries a
	// "created" timestamp. A bare JSON object served with HTTP 200 (a proxy
	// health page, a JSON error body) unmarshals without error but is not a
	// MusicBrainz endpoint, so reject it rather than report the mirror healthy.
	if resp.Created == "" {
		return fmt.Errorf("endpoint returned JSON that is not a MusicBrainz search response (check mirror configuration)")
	}
	return nil
}

// SetBaseURL updates the adapter's base URL for mirror support and rebuilds the
// HTTP client to match: switching to a custom mirror installs a client that
// exempts the mirror's host from the SSRF guard (a LAN/loopback mirror becomes
// reachable), while reverting to the default re-installs the plain guarded
// client. Client reassignment happens under the same a.mu lock that guards
// baseURL, so doRequest (which captures both under its RLock) never observes a
// torn baseURL/client pair.
func (a *Adapter) SetBaseURL(rawURL string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.baseURL = strings.TrimRight(rawURL, "/")
	a.client = newMirrorClient(a.baseURL)
}

// BaseURL returns the current base URL.
func (a *Adapter) BaseURL() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.baseURL
}

// DefaultBaseURL returns the default MusicBrainz API base URL.
func (a *Adapter) DefaultBaseURL() string {
	return defaultBaseURL
}

// SetHTTPClient replaces the adapter's HTTP client. Intended for tests
// that run against an httptest.NewServer loopback fixture, which
// httpsafe.SafeClient (the production default) rejects by design.
// Production callers should not need this; the constructor wires the
// SafeClient automatically.
//
// Callers must call this before initiating requests, not concurrently
// with them. The write is deliberately lock-free: doRequest captures
// a.client under a.mu.RLock, but the only legitimate caller of this
// method is single-threaded test setup that runs before any request, so
// taking the lock here would add ceremony without benefit. Panics on nil
// to surface the misconfiguration at the wiring site rather than as a
// confusing nil dereference deep inside doRequest.
func (a *Adapter) SetHTTPClient(c *http.Client) {
	if c == nil {
		panic("musicbrainz.SetHTTPClient: client must not be nil")
	}
	a.client = c
}

// setFallbackURL overrides the URL used when auto-fallback triggers on a
// parse error. Only for use in tests; production code always falls back
// to defaultBaseURL (set by the constructor). This must be called before
// any requests are issued (no concurrency guard needed for test setup).
func (a *Adapter) setFallbackURL(u string) {
	a.fallbackURL = strings.TrimRight(u, "/")
}

// unmarshalResponse unmarshals body into dst. On failure it logs a WARN with
// the configured base URL and a short body prefix to help operators diagnose
// mirror misconfiguration (e.g. a server returning an HTML error page with
// a 200 OK status). The base URL is included so the log entry identifies the
// specific mirror endpoint that produced the unexpected response.
func (a *Adapter) unmarshalResponse(base string, body []byte, dst any) error {
	if err := json.Unmarshal(body, dst); err != nil {
		// Surface up to 200 bytes of the body so the log can reveal an HTML
		// snippet or other non-JSON content without flooding the log.
		preview := body
		if len(preview) > 200 {
			preview = preview[:200]
		}
		a.logger.Warn("JSON parse failed; mirror may be returning non-JSON response",
			slog.String("base_url", base),
			slog.String("body_preview", string(preview)),
			slog.Any("err", err))
		return err
	}
	return nil
}

// unmarshalWithFallback attempts to unmarshal body into dst. When parsing
// fails AND a mirror is configured (baseURL differs from fallbackURL), it
// automatically retries the request against the fallback URL (normally the
// official musicbrainz.org API) and unmarshals that response instead. A
// network error from the mirror does NOT trigger fallback -- only a
// successful HTTP 200 with an unparsable body does, because a timeout or
// connection error signals a network problem rather than a mirror serving
// bad content.
//
// The pathAndQuery argument is the path + query string used to reconstruct
// the fallback URL (e.g. "/artist?query=test&fmt=json&limit=1").
func (a *Adapter) unmarshalWithFallback(ctx context.Context, base, pathAndQuery string, body []byte, dst any) error {
	// unmarshalResponse logs a WARN as a side effect, so call it exactly once
	// per response body and reuse the captured error rather than re-running it.
	parseErr := a.unmarshalResponse(base, body, dst)
	if parseErr == nil {
		return nil
	}

	// Parse error: fall back only when a distinct mirror is configured.
	fallback := a.fallbackURL
	if fallback == "" || base == fallback {
		// No distinct fallback configured, or already using the fallback URL.
		return parseErr
	}

	// Retry against the fallback URL, logging the switch so operators can
	// tell why a request went to the official API instead of the mirror.
	fallbackURL := fallback + pathAndQuery
	a.logger.Warn("mirror returned non-JSON response; retrying against fallback URL",
		slog.String("mirror_base_url", base),
		slog.String("fallback_url", fallbackURL))

	fallbackBody, err := a.doRequest(ctx, fallbackURL)
	if err != nil {
		// Fallback request itself failed. Log the fallback error so a double
		// failure (broken mirror AND unreachable fallback) stays visible, but
		// return the original parse error: the operator's actionable problem is
		// the broken mirror, not "can't reach musicbrainz.org".
		a.logger.Warn("fallback request also failed; returning original mirror parse error",
			slog.String("fallback_url", fallbackURL),
			slog.Any("err", err))
		return parseErr
	}

	// Zero dst before decoding the fallback body. A failed mirror decode can
	// leave dst partially populated (json.Unmarshal completes "as best it can"
	// before erroring), and a fallback response that omits those fields would
	// not overwrite the stale values. Zeroing guarantees dst holds only
	// fallback data on success.
	if v := reflect.ValueOf(dst); v.Kind() == reflect.Pointer && !v.IsNil() {
		v.Elem().Set(reflect.Zero(v.Elem().Type()))
	}

	// unmarshalResponse logs a WARN for the fallback body if it also fails.
	if err := a.unmarshalResponse(fallback, fallbackBody, dst); err != nil {
		// Both the mirror and the fallback returned unparsable bodies. Surface
		// the original mirror parse error as the actionable root cause: the
		// configured mirror is the thing the operator can fix.
		return parseErr
	}
	return nil
}

// doRequest executes an HTTP GET with rate limiting and standard headers,
// backing off and retrying on a rate-limited (429) or unavailable (503)
// response via provider.DoWithRetry.
func (a *Adapter) doRequest(ctx context.Context, reqURL string) ([]byte, error) {
	// Capture the client under the same RLock that guards baseURL elsewhere.
	// SetBaseURL reassigns a.client under a.mu.Lock(), so this read must be
	// synchronized to stay -race clean; capturing once also pins a single
	// client for all retries of this request even if SetBaseURL runs midway.
	a.mu.RLock()
	client := a.client
	a.mu.RUnlock()

	// do performs one HTTP attempt. The limiter wait lives inside it so that
	// every retry triggered by DoWithRetry still respects the per-provider
	// request budget.
	do := func(ctx context.Context) (*http.Response, error) {
		if err := a.limiter.Wait(ctx, provider.NameMusicBrainz); err != nil {
			return nil, &provider.ErrProviderUnavailable{
				Provider: provider.NameMusicBrainz,
				Cause:    fmt.Errorf("rate limiter: %w", err),
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("User-Agent", userAgent())
		req.Header.Set("Accept", "application/json")

		a.logger.Debug("requesting", slog.String("url", reqURL))
		return client.Do(req)
	}

	// DoWithRetry honors a 429 Retry-After (and backs off conservatively on a
	// 503) before giving up. It returns the first non-rate-limited response, so
	// the status handling below only sees 200/404/other.
	resp, err := provider.DoWithRetry(ctx, provider.SystemClock(), provider.NameMusicBrainz, provider.DefaultRetryPolicy(), do)
	if err != nil {
		// Either a transport error from client.Do or an exhausted-retry error
		// (already an *ErrProviderUnavailable). Wrap the former so callers still
		// see a provider-unavailable error.
		var unavailable *provider.ErrProviderUnavailable
		if errors.As(err, &unavailable) {
			return nil, err
		}
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameMusicBrainz,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrNotFound{
			Provider: provider.NameMusicBrainz,
			ID:       reqURL,
		}
	}

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameMusicBrainz,
			Cause:    fmt.Errorf("unexpected HTTP %d", resp.StatusCode),
		}
	}

	return io.ReadAll(io.LimitReader(resp.Body, 512*1024))
}

// hyphenReplacer normalizes Unicode hyphen variants to ASCII hyphen-minus.
// MusicBrainz uses U+2010 (HYPHEN) in some artist names (e.g. "a‐ha").
var hyphenReplacer = strings.NewReplacer(
	"\u2010", "-", // HYPHEN
	"\u2011", "-", // NON-BREAKING HYPHEN
)

// normalizeHyphens replaces Unicode hyphen characters with ASCII hyphen-minus.
func normalizeHyphens(s string) string {
	return hyphenReplacer.Replace(s)
}

// newArtistMetadata constructs the seed ArtistMetadata for mapArtist with the
// always-set fields (IDs, normalized name/sort, type, gender, disambiguation,
// origin). Gender is forced empty for non-individual types to match the
// scraper-executor normalization path.
func newArtistMetadata(mb *MBArtist) *provider.ArtistMetadata {
	mappedType := mapArtistType(mb.Type)
	gender := strings.ToLower(mb.Gender)
	if mappedType != "" && !artist.IsIndividualType(mappedType) {
		gender = ""
	}
	origin := mb.Area.Name
	if origin == "" {
		origin = mb.Country
	}
	return &provider.ArtistMetadata{
		ProviderID:     mb.ID,
		MusicBrainzID:  mb.ID,
		Name:           normalizeHyphens(mb.Name),
		SortName:       normalizeHyphens(mb.SortName),
		Type:           mappedType,
		Gender:         gender,
		Disambiguation: mb.Disambiguation,
		Origin:         origin,
		URLs:           make(map[string]string),
	}
}

// applyLifeSpan maps the MB life-span Begin/End onto the formed/born and
// disbanded/died fields, choosing per the artist's group-vs-individual type.
func applyLifeSpan(meta *provider.ArtistMetadata, mb *MBArtist) {
	if mb.LifeSpan.Begin != "" {
		if isGroupType(mb.Type) {
			meta.Formed = mb.LifeSpan.Begin
		} else {
			meta.Born = mb.LifeSpan.Begin
		}
	}
	if mb.LifeSpan.End != "" {
		if isGroupType(mb.Type) {
			meta.Disbanded = mb.LifeSpan.End
		} else {
			meta.Died = mb.LifeSpan.End
		}
	}
}

// applyGenresAndTags fills meta.Genres / meta.Styles / meta.Moods from the
// MusicBrainz response. When structured genres are present they are kept
// verbatim and tags are mined for additional style entries; otherwise the
// tag list is classified into all three buckets so styles and moods are not
// lost as plain genres.
func applyGenresAndTags(meta *provider.ArtistMetadata, mb *MBArtist) {
	for _, g := range mb.Genres {
		if g.Name != "" {
			meta.Genres = append(meta.Genres, g.Name)
		}
	}

	if len(meta.Genres) > 0 {
		// Structured genres exist. Classify all genres + tags together to
		// extract style-level entries, then deduplicate against the genre list.
		var allTagNames []string
		for _, g := range mb.Genres {
			if g.Name != "" {
				allTagNames = append(allTagNames, g.Name)
			}
		}
		for _, t := range mb.Tags {
			if t.Name != "" && t.Count > 0 {
				allTagNames = append(allTagNames, t.Name)
			}
		}
		_, extractedStyles, _ := tagclass.ClassifyTags(allTagNames)
		meta.Styles = deduplicateStyles(extractedStyles, meta.Genres)
		return
	}

	if len(mb.Tags) == 0 {
		return
	}
	// No structured genres -- classify tags into genres/styles/moods
	// instead of dumping everything into genres. Without this split,
	// deduplicateStyles would remove all styles because they were
	// already placed in the genres bucket.
	var tagNames []string
	for _, t := range mb.Tags {
		if t.Name != "" && t.Count > 0 {
			tagNames = append(tagNames, t.Name)
		}
	}
	fallbackGenres, fallbackStyles, fallbackMoods := tagclass.ClassifyTags(tagNames)
	meta.Genres = fallbackGenres
	meta.Styles = append(meta.Styles, fallbackStyles...)
	meta.Moods = append(meta.Moods, fallbackMoods...)
}

// pickBestPrimaryAlias scans MB aliases and returns the highest-priority
// primary alias for the supplied language preference list. ok is false when
// no primary alias matched any preference.
func pickBestPrimaryAlias(aliases []MBAlias, langPrefs []string) (best MBAlias, ok bool) {
	bestScore := -1
	for _, alias := range aliases {
		if alias.Name == "" || !alias.Primary {
			continue
		}
		score := provider.MatchLanguagePreference(alias.Locale, langPrefs)
		if score >= 0 && (bestScore < 0 || score < bestScore) {
			bestScore = score
			best = alias
		}
	}
	return best, bestScore >= 0
}

// promoteLocalizedName replaces meta.Name and/or meta.SortName with the
// best-matching primary alias for the user's language preferences. Returns
// the original canonical name so caller can re-add it as an alias if it was
// displaced.
func (a *Adapter) promoteLocalizedName(meta *provider.ArtistMetadata, mb *MBArtist, langPrefs []string) (canonicalName string) {
	canonicalName = meta.Name
	if len(langPrefs) == 0 {
		return canonicalName
	}
	bestAlias, ok := pickBestPrimaryAlias(mb.Aliases, langPrefs)
	if !ok {
		return canonicalName
	}
	promotedName := normalizeHyphens(bestAlias.Name)
	promotedSort := normalizeHyphens(bestAlias.SortName)
	nameChanged := promotedName != "" && promotedName != canonicalName
	sortChanged := promotedSort != "" && promotedSort != meta.SortName
	if !nameChanged && !sortChanged {
		return canonicalName
	}
	a.logger.Debug("promoting localized name",
		"from", canonicalName,
		"to", bestAlias.Name,
		"locale", bestAlias.Locale)
	if nameChanged {
		meta.Name = promotedName
	}
	if sortChanged {
		meta.SortName = promotedSort
	} else if nameChanged {
		a.logger.Debug("promoted alias has no sort name, retaining canonical",
			"canonical_sort", meta.SortName,
			"locale", bestAlias.Locale)
	}
	return canonicalName
}

// scoredAlias pairs an alias name with its language-preference match score.
// Negative scores indicate "no language match"; lower non-negative scores rank
// higher.
type scoredAlias struct {
	name  string
	score int
}

// collectAndScoreAliases builds the deduplicated alias list. The returned
// `seen` map is shared with applyRelations so "is person" relations can avoid
// re-adding an existing alias. canonicalName is only re-added as an alias
// when promoteLocalizedName replaced meta.Name with a different value.
func collectAndScoreAliases(meta *provider.ArtistMetadata, mb *MBArtist, langPrefs []string, canonicalName string) (scored []scoredAlias, seen map[string]bool) {
	seen = make(map[string]bool)
	seen[meta.Name] = true
	if canonicalName != meta.Name {
		scored = append(scored, scoredAlias{name: canonicalName, score: -1})
		seen[canonicalName] = true
	}
	for _, alias := range mb.Aliases {
		normalizedAlias := normalizeHyphens(alias.Name)
		if normalizedAlias == "" || seen[normalizedAlias] {
			continue
		}
		seen[normalizedAlias] = true
		score := provider.MatchLanguagePreference(alias.Locale, langPrefs)
		scored = append(scored, scoredAlias{name: normalizedAlias, score: score})
	}
	return scored, seen
}

// sortAliasesByLanguagePreference orders scored aliases so locale matches
// come first (lower score wins) and unmatched (-1) entries fall to the end.
// Stable to preserve MB's original ordering within a score group.
func sortAliasesByLanguagePreference(scored []scoredAlias, langPrefs []string) {
	if len(langPrefs) == 0 || len(scored) <= 1 {
		return
	}
	sort.SliceStable(scored, func(i, j int) bool {
		si, sj := scored[i].score, scored[j].score
		// -1 means unmatched -- push to end
		if si < 0 && sj >= 0 {
			return false
		}
		if sj < 0 && si >= 0 {
			return true
		}
		return si < sj
	})
}

// applyRelation maps a single MB relation onto meta. seen is mutated when an
// "is person" relation contributes a fresh alias.
func applyRelation(meta *provider.ArtistMetadata, rel MBRelation, seen map[string]bool) {
	switch {
	case rel.Type == "member of band" && rel.Artist != nil && rel.Direction == "backward":
		member := provider.MemberInfo{
			Name:       rel.Artist.Name,
			MBID:       rel.Artist.ID,
			IsActive:   !rel.Ended,
			DateJoined: rel.Begin,
			DateLeft:   rel.End,
		}
		member.Instruments = append(member.Instruments, rel.Attributes...)
		meta.Members = append(meta.Members, member)

	case rel.Type == "is person" && rel.Artist != nil:
		// "is person" is the MusicBrainz relation type for "also performs as"
		// (legal name <-> stage name links). Capture the related artist name
		// as an alias if it is not already present.
		aliasName := normalizeHyphens(rel.Artist.Name)
		if aliasName != "" && aliasName != meta.Name && !seen[aliasName] {
			meta.Aliases = append(meta.Aliases, aliasName)
			seen[aliasName] = true
		}

	case rel.URL != nil && rel.URL.Resource != "":
		urlType := mapURLType(rel.Type, rel.URL.Resource)
		if urlType != "" {
			meta.URLs[urlType] = rel.URL.Resource
		}
	}
}

// applyRelations dispatches every MB relation to applyRelation. The seen map
// is shared with collectAndScoreAliases so "is person" relations cannot
// duplicate an existing alias.
func applyRelations(meta *provider.ArtistMetadata, mb *MBArtist, seen map[string]bool) {
	for i := range mb.Relations {
		applyRelation(meta, mb.Relations[i], seen)
	}
}

// synthesizeYearsActive fills meta.YearsActive from formed/disbanded (groups)
// or born/died (individuals) when MusicBrainz did not supply one. The
// derivation logic is shared with the orchestrator's per-field fetch via
// provider.SynthesizeYearsActive so both paths agree on the output format.
func synthesizeYearsActive(meta *provider.ArtistMetadata) {
	if meta.YearsActive != "" {
		return
	}
	if synth, ok := provider.SynthesizeYearsActive(meta); ok {
		meta.YearsActive = synth
	}
}

// mapArtist transforms a raw MusicBrainz API response into the internal
// ArtistMetadata model. The transformation is staged so each helper owns a
// self-contained slice of the conversion: seed, life span, tag classification,
// locale-aware name promotion, alias scoring, relation mapping, member dedup,
// and years-active synthesis.
//
// When language preferences are set in the context, mapArtist promotes the
// best-matching primary alias to the Name and SortName fields, placing the
// canonical name behind all language-matched aliases in the aliases list.
// Remaining aliases, including the canonical name, are sorted by preference
// score. Relation-derived aliases ("is person" / "also performs as") are
// appended after the sorted list because they lack locale metadata for scoring.
func (a *Adapter) mapArtist(ctx context.Context, mb *MBArtist) *provider.ArtistMetadata {
	meta := newArtistMetadata(mb)
	applyLifeSpan(meta, mb)
	applyGenresAndTags(meta, mb)

	// Language-aware name promotion happens BEFORE alias collection so the
	// canonical name (when displaced) can be re-added to the alias list.
	langPrefs := provider.MetadataLanguages(ctx)
	canonicalName := a.promoteLocalizedName(meta, mb, langPrefs)

	scored, seen := collectAndScoreAliases(meta, mb, langPrefs, canonicalName)
	sortAliasesByLanguagePreference(scored, langPrefs)
	for _, sa := range scored {
		meta.Aliases = append(meta.Aliases, sa.name)
	}

	// Relations may add more aliases via the "is person" path; share the seen
	// map with collectAndScoreAliases so duplicates are avoided across both
	// passes.
	applyRelations(meta, mb, seen)

	// Deduplicate members by MBID. When the same person appears multiple times
	// (e.g., different active periods), merge their date ranges and instruments.
	meta.Members = deduplicateMembers(meta.Members)

	synthesizeYearsActive(meta)

	// Set MembersAuthoritative for confirmed individual artist types (Person,
	// Character). An individual by definition has no band members, so an empty
	// member list from a Person-type MusicBrainz result is authoritatively
	// complete and may safely clear any stale band-member rows.
	//
	// Safety: we deliberately do NOT set the flag for Group, Orchestra, Choir,
	// Other, or unknown types. A real band has Type="Group" and an empty member
	// list from MusicBrainz usually reflects sparse relation coverage rather
	// than a true empty roster, so we leave it false to avoid accidental data
	// loss. The authoritative-clear path is only safe when the artist type
	// itself rules out the possibility of members existing.
	if mb.Type == "Person" || mb.Type == "Character" {
		meta.MembersAuthoritative = true
	}

	return meta
}

// isGroupType returns true for MusicBrainz types that represent ensembles
// (groups, orchestras, choirs) rather than individual persons.
func isGroupType(mbType string) bool {
	return mbType == "Group" || mbType == "Orchestra" || mbType == "Choir"
}

// mapArtistType normalizes MusicBrainz type strings.
func mapArtistType(mbType string) string {
	switch mbType {
	case "Person":
		return "solo"
	case "Group":
		return "group"
	case "Orchestra":
		return "orchestra"
	case "Choir":
		return "choir"
	case "Character":
		return "character"
	default:
		return strings.ToLower(mbType)
	}
}

// mapURLType maps a MusicBrainz URL relation type to a simple key.
// For "streaming music" relations, the URL is inspected to identify the specific service.
func mapURLType(relType, resourceURL string) string {
	switch relType {
	case "official homepage":
		return "official"
	case "wikipedia":
		return "wikipedia"
	case "wikidata":
		return "wikidata"
	case "bandcamp":
		return "bandcamp"
	case "discogs":
		return "discogs"
	case "last.fm":
		return "lastfm"
	case "allmusic":
		return "allmusic"
	case "social network":
		return "social"
	case "streaming music", "free streaming", "streaming":
		if strings.Contains(resourceURL, "deezer.com") {
			return "deezer"
		}
		if strings.Contains(resourceURL, "open.spotify.com") {
			return "spotify"
		}
		return "streaming"
	default:
		return relType
	}
}

// deduplicateStyles removes any style that is already present in the genres
// list. Comparison is case-insensitive to avoid duplicates like
// "art rock" in both genres and styles.
func deduplicateStyles(styles, genres []string) []string {
	if len(styles) == 0 {
		return nil
	}
	genreSet := make(map[string]bool, len(genres))
	for _, g := range genres {
		genreSet[strings.ToLower(g)] = true
	}
	var result []string
	for _, s := range styles {
		if !genreSet[strings.ToLower(s)] {
			result = append(result, s)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// deduplicateMembers merges duplicate member entries that share the same MBID.
// When duplicates exist, their date ranges and instruments are combined into a
// single entry. The merged entry keeps the first canonical name seen for the
// MBID.
//
// Note: MusicBrainz "member of band" relation stubs carry only "name" and
// "sort-name" fields -- no alias or locale data is present in the response
// (confirmed by live BUCK-TICK query with inc=aliases+artist-rels; see #1020).
// Locale-aware name selection is therefore not possible at this stage and is
// not attempted.
func deduplicateMembers(members []provider.MemberInfo) []provider.MemberInfo {
	if len(members) <= 1 {
		return members
	}

	type mergedMember struct {
		info    provider.MemberInfo
		periods [][2]string // pairs of [joined, left]
	}

	// Track insertion order so the output is deterministic.
	var order []string
	byMBID := make(map[string]*mergedMember, len(members))

	for i, m := range members {
		key := m.MBID
		if key == "" {
			// Members without an MBID cannot be deduplicated reliably
			// (name+date is not unique), so each gets a unique index-based key.
			key = fmt.Sprintf("no-mbid-%d", i)
		}

		existing, ok := byMBID[key]
		if !ok {
			order = append(order, key)
			byMBID[key] = &mergedMember{
				info:    m,
				periods: [][2]string{{m.DateJoined, m.DateLeft}},
			}
			continue
		}

		// Merge: combine date range and instruments from the duplicate.
		// The first canonical name for this MBID is kept as-is.
		existing.periods = append(existing.periods, [2]string{m.DateJoined, m.DateLeft})

		// Merge instruments, avoiding duplicates.
		instrSet := make(map[string]bool, len(existing.info.Instruments))
		for _, inst := range existing.info.Instruments {
			instrSet[inst] = true
		}
		for _, inst := range m.Instruments {
			if !instrSet[inst] {
				existing.info.Instruments = append(existing.info.Instruments, inst)
				instrSet[inst] = true
			}
		}

		// If either entry is active, mark the merged result as active.
		if m.IsActive {
			existing.info.IsActive = true
		}
	}

	// Build the result, picking the earliest joined date and latest left date
	// across all merged periods.
	result := make([]provider.MemberInfo, 0, len(order))
	for _, key := range order {
		mm := byMBID[key]
		earliest, latest := mergeDateRanges(mm.periods)
		mm.info.DateJoined = earliest
		mm.info.DateLeft = latest
		result = append(result, mm.info)
	}
	return result
}

// mergeDateRanges finds the earliest start and latest end from a set of
// [joined, left] date pairs. If any period has an empty end date (open-ended),
// the returned latest is empty to represent an unbounded range.
func mergeDateRanges(periods [][2]string) (earliest, latest string) {
	hasOpenEnd := false
	for _, p := range periods {
		joined, left := p[0], p[1]
		if joined != "" && (earliest == "" || joined < earliest) {
			earliest = joined
		}
		if left == "" {
			hasOpenEnd = true
		} else if latest == "" || left > latest {
			latest = left
		}
	}
	if hasOpenEnd {
		latest = ""
	}
	return earliest, latest
}

func userAgent() string {
	return version.UserAgent("Stillwater", "https://github.com/sydlexius/stillwater")
}
