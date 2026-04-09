package musicbrainz

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
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/tagclass"
	"github.com/sydlexius/stillwater/internal/version"
)

const defaultBaseURL = "https://musicbrainz.org/ws/2"

// Adapter implements the provider.Provider interface for MusicBrainz.
type Adapter struct {
	client  *http.Client
	limiter *provider.RateLimiterMap
	logger  *slog.Logger
	mu      sync.RWMutex
	baseURL string
}

// New creates a MusicBrainz adapter with the default base URL.
func New(limiter *provider.RateLimiterMap, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, logger, defaultBaseURL)
}

// NewWithBaseURL creates a MusicBrainz adapter with a custom base URL (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, logger *slog.Logger, baseURL string) *Adapter {
	return &Adapter{
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		limiter: limiter,
		logger:  logger.With(slog.String("provider", "musicbrainz")),
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// Name returns the provider name.
func (a *Adapter) Name() provider.ProviderName { return provider.NameMusicBrainz }

// RequiresAuth returns whether this provider needs an API key.
func (a *Adapter) RequiresAuth() bool { return false }

// SearchArtist searches MusicBrainz for artists matching the given name.
func (a *Adapter) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
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
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing search response: %w", err)
	}

	results := make([]provider.ArtistSearchResult, 0, len(resp.Artists))
	for _, a := range resp.Artists {
		// Use the higher of the API's native score and our name similarity
		// score. The API score reflects relevance factors beyond name matching
		// (popularity, tag matches), while name similarity catches cases where
		// the API underscores an exact or near-exact name match.
		score := a.Score
		if ns := provider.NameSimilarity(name, a.Name); ns > score {
			score = ns
		}
		results = append(results, provider.ArtistSearchResult{
			ProviderID:     a.ID,
			Name:           a.Name,
			SortName:       a.SortName,
			Type:           a.Type,
			Disambiguation: a.Disambiguation,
			Country:        a.Country,
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

	var artist MBArtist
	if err := json.Unmarshal(body, &artist); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	return a.mapArtist(ctx, &artist), nil
}

// GetImages returns nil since MusicBrainz does not host artist images.
func (a *Adapter) GetImages(_ context.Context, _ string) ([]provider.ImageResult, error) {
	return nil, nil
}

// GetReleaseGroups fetches release groups (albums, EPs, singles) for an artist by MBID.
// Results are paginated in batches of 100 and capped at 500 total to avoid
// runaway loops on prolific artists (classical composers, etc.).
func (a *Adapter) GetReleaseGroups(ctx context.Context, mbid string) ([]provider.ReleaseGroupInfo, error) {
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
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("parsing release-group response: %w", err)
		}

		for _, rg := range resp.ReleaseGroups {
			results = append(results, provider.ReleaseGroupInfo{
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

// TestConnection verifies connectivity to the MusicBrainz API.
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
	_, err := a.doRequest(ctx, reqURL)
	return err
}

// SetBaseURL updates the adapter's base URL for mirror support.
func (a *Adapter) SetBaseURL(rawURL string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.baseURL = strings.TrimRight(rawURL, "/")
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

// doRequest executes an HTTP GET with rate limiting and standard headers.
func (a *Adapter) doRequest(ctx context.Context, reqURL string) ([]byte, error) {
	if err := a.limiter.Wait(ctx, provider.NameMusicBrainz); err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameMusicBrainz,
			Cause:    fmt.Errorf("rate limiter: %w", err),
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept", "application/json")

	a.logger.Debug("requesting", slog.String("url", reqURL))

	resp, err := a.client.Do(req) //nolint:gosec // URL constructed from trusted base + user-provided MBID
	if err != nil {
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameMusicBrainz,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrNotFound{
			Provider: provider.NameMusicBrainz,
			ID:       reqURL,
		}
	}

	if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider:   provider.NameMusicBrainz,
			Cause:      fmt.Errorf("HTTP %d", resp.StatusCode),
			RetryAfter: 2 * time.Second,
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

// mapArtist converts a MusicBrainz artist to the common ArtistMetadata type.
// When language preferences are set in the context, mapArtist promotes the
// best-matching primary alias to the Name and SortName fields, demoting the
// canonical name to the end of the aliases list (after all language-matched
// aliases). Remaining aliases are sorted by preference score.
func (a *Adapter) mapArtist(ctx context.Context, mb *MBArtist) *provider.ArtistMetadata {
	meta := &provider.ArtistMetadata{
		ProviderID:     mb.ID,
		MusicBrainzID:  mb.ID,
		Name:           normalizeHyphens(mb.Name),
		SortName:       normalizeHyphens(mb.SortName),
		Type:           mapArtistType(mb.Type),
		Gender:         strings.ToLower(mb.Gender),
		Disambiguation: mb.Disambiguation,
		Country:        mb.Country,
		URLs:           make(map[string]string),
	}

	// Life span
	if mb.LifeSpan.Begin != "" {
		if mb.Type == "Group" || mb.Type == "Orchestra" || mb.Type == "Choir" {
			meta.Formed = mb.LifeSpan.Begin
		} else {
			meta.Born = mb.LifeSpan.Begin
		}
	}
	if mb.LifeSpan.End != "" {
		if mb.Type == "Group" || mb.Type == "Orchestra" || mb.Type == "Choir" {
			meta.Disbanded = mb.LifeSpan.End
		} else {
			meta.Died = mb.LifeSpan.End
		}
	}

	// Genres from the structured genres array.
	for _, g := range mb.Genres {
		if g.Name != "" {
			meta.Genres = append(meta.Genres, g.Name)
		}
	}

	hasStructuredGenres := len(meta.Genres) > 0

	if hasStructuredGenres {
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
	} else if len(mb.Tags) > 0 {
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

	// Language-aware name promotion: if the user has language preferences,
	// look for a primary alias in the preferred language and promote its
	// name (and sort name, if present) to the top-level fields. The original
	// canonical name is demoted into the aliases list so it is not lost.
	langPrefs := provider.MetadataLanguages(ctx)
	canonicalName := meta.Name
	if len(langPrefs) > 0 {
		bestScore := -1
		var bestAlias MBAlias
		for _, alias := range mb.Aliases {
			if alias.Name == "" || !alias.Primary {
				continue
			}
			score := provider.MatchLanguagePreference(alias.Locale, langPrefs)
			if score >= 0 && (bestScore < 0 || score < bestScore) {
				bestScore = score
				bestAlias = alias
			}
		}
		if bestScore >= 0 {
			promotedName := normalizeHyphens(bestAlias.Name)
			promotedSort := normalizeHyphens(bestAlias.SortName)
			nameChanged := promotedName != "" && promotedName != canonicalName
			sortChanged := promotedSort != "" && promotedSort != meta.SortName
			if nameChanged || sortChanged {
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
			}
		}
	}

	// Aliases: collect all, then sort by user's language preference if set.
	// The canonical name is included if it differs from the promoted name.
	type scoredAlias struct {
		name  string
		score int
	}
	var scored []scoredAlias
	// If we promoted a different name, add the original canonical name as an alias.
	if canonicalName != meta.Name {
		scored = append(scored, scoredAlias{name: canonicalName, score: -1})
	}
	for _, alias := range mb.Aliases {
		normalizedAlias := normalizeHyphens(alias.Name)
		if normalizedAlias == "" || normalizedAlias == meta.Name || normalizedAlias == canonicalName {
			continue
		}
		score := provider.MatchLanguagePreference(alias.Locale, langPrefs)
		scored = append(scored, scoredAlias{name: normalizedAlias, score: score})
	}
	// Sort: matched locales first (lower score wins), unmatched last.
	if len(langPrefs) > 0 && len(scored) > 1 {
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
	for _, sa := range scored {
		meta.Aliases = append(meta.Aliases, sa.name)
	}

	// Relations: extract members and URLs
	for _, rel := range mb.Relations {
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
		case rel.URL != nil && rel.URL.Resource != "":
			urlType := mapURLType(rel.Type, rel.URL.Resource)
			if urlType != "" {
				meta.URLs[urlType] = rel.URL.Resource
			}
		}
	}

	return meta
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

func userAgent() string {
	return fmt.Sprintf("Stillwater/%s (https://github.com/sydlexius/stillwater)", version.Version)
}
