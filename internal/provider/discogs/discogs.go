package discogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/httpsafe"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/version"
)

const defaultBaseURL = "https://api.discogs.com"

// Adapter implements the provider.Provider interface for Discogs.
type Adapter struct {
	client   *http.Client
	limiter  *provider.RateLimiterMap
	settings *provider.SettingsService
	logger   *slog.Logger
	baseURL  string
}

// New creates a Discogs adapter with the default base URL.
func New(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger) *Adapter {
	return NewWithBaseURL(limiter, settings, logger, defaultBaseURL)
}

// NewWithBaseURL creates a Discogs adapter with a custom base URL (for testing).
func NewWithBaseURL(limiter *provider.RateLimiterMap, settings *provider.SettingsService, logger *slog.Logger, baseURL string) *Adapter {
	return &Adapter{
		client:   httpsafe.SafeClient(10 * time.Second),
		limiter:  limiter,
		settings: settings,
		logger:   logger.With(slog.String("provider", "discogs")),
		baseURL:  strings.TrimRight(baseURL, "/"),
	}
}

// Name returns the provider name.
func (a *Adapter) Name() provider.ProviderName { return provider.NameDiscogs }

// RequiresAuth returns whether this provider needs an API key.
func (a *Adapter) RequiresAuth() bool { return true }

// SearchArtist searches Discogs for artists matching the given name.
func (a *Adapter) SearchArtist(ctx context.Context, name string) ([]provider.ArtistSearchResult, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	token, err := a.getToken(ctx)
	if err != nil {
		return nil, err
	}

	params := url.Values{
		"q":    {name},
		"type": {"artist"},
	}
	reqURL := a.baseURL + "/database/search?" + params.Encode()

	body, err := a.doRequest(ctx, reqURL, token)
	if err != nil {
		return nil, err
	}

	var resp SearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing search response: %w", err)
	}

	results := make([]provider.ArtistSearchResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		results = append(results, provider.ArtistSearchResult{
			ProviderID: strconv.Itoa(r.ID),
			Name:       r.Title,
			Score:      provider.NameSimilarity(name, r.Title),
			Source:     string(provider.NameDiscogs),
		})
	}

	// Sort by score descending so the best match appears first.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results, nil
}

// SupportsNameLookup returns true because Discogs can search by artist name
// and use the top result's numeric ID to fetch metadata. This is a last-resort
// fallback: the orchestrator first tries to extract the Discogs numeric ID from
// MusicBrainz/Wikidata URL relations before falling back to name-based search.
func (a *Adapter) SupportsNameLookup() bool { return true }

// GetArtist fetches full metadata for an artist by their Discogs numeric ID.
// Non-numeric IDs (such as MusicBrainz UUIDs) are rejected immediately with
// ErrNotFound to avoid a wasted HTTP round-trip. When the orchestrator's
// MBID-to-name retry fires (because this adapter implements NameLookupProvider),
// GetArtist is called again with the artist name, which routes through
// getArtistByName for a search-then-fetch flow.
func (a *Adapter) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	if !isNumericID(id) {
		// If the id is not numeric but also not a UUID, treat it as an
		// artist name and fall back to name-based search.
		if !provider.IsUUID(id) {
			a.logger.Debug("non-numeric non-UUID ID, falling back to name search",
				slog.String("id", id))
			return a.getArtistByName(ctx, id)
		}
		return nil, &provider.ErrNotFound{Provider: provider.NameDiscogs, ID: id}
	}

	return a.getArtistByID(ctx, id)
}

// getArtistByID fetches artist metadata directly using a numeric Discogs ID.
func (a *Adapter) getArtistByID(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	token, err := a.getToken(ctx)
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("%s/artists/%s", a.baseURL, url.PathEscape(id))
	body, err := a.doRequest(ctx, reqURL, token)
	if err != nil {
		return nil, err
	}

	var detail ArtistDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	meta := mapArtist(&detail)

	// Fetch styles from releases (secondary API calls).
	styles, err := a.aggregateStyles(ctx, id, token)
	if err != nil {
		a.logger.Warn("failed to fetch Discogs styles from releases",
			slog.String("artist_id", id), slog.String("error", err.Error()))
	} else {
		meta.Styles = styles
	}

	return meta, nil
}

// getArtistByName searches Discogs by artist name and fetches metadata for the
// top result. This is the last-resort fallback when no numeric Discogs ID was
// found via MusicBrainz/Wikidata URL extraction.
func (a *Adapter) getArtistByName(ctx context.Context, name string) (*provider.ArtistMetadata, error) {
	results, err := a.SearchArtist(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, &provider.ErrNotFound{Provider: provider.NameDiscogs, ID: name}
	}
	a.logger.Debug("name search selected top result",
		slog.String("query", name),
		slog.String("result_name", results[0].Name),
		slog.String("result_id", results[0].ProviderID))
	return a.getArtistByID(ctx, results[0].ProviderID)
}

// GetImages fetches available images for an artist by their Discogs numeric ID.
// Returns ErrNotFound for non-numeric IDs (such as MusicBrainz UUIDs) without
// making an HTTP request.
func (a *Adapter) GetImages(ctx context.Context, id string) ([]provider.ImageResult, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	if !isNumericID(id) {
		return nil, &provider.ErrNotFound{Provider: provider.NameDiscogs, ID: id}
	}

	token, err := a.getToken(ctx)
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("%s/artists/%s", a.baseURL, url.PathEscape(id))
	body, err := a.doRequest(ctx, reqURL, token)
	if err != nil {
		return nil, err
	}

	var detail ArtistDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("parsing artist response: %w", err)
	}

	var images []provider.ImageResult
	source := string(provider.NameDiscogs)
	for _, img := range detail.Images {
		imgType := provider.ImageThumb
		if img.Type == "primary" {
			imgType = provider.ImageThumb
		}
		images = append(images, provider.ImageResult{
			URL:    img.URI,
			Type:   imgType,
			Width:  img.Width,
			Height: img.Height,
			Source: source,
		})
	}
	return images, nil
}

// TestConnection verifies the personal access token is valid.
func (a *Adapter) TestConnection(ctx context.Context) error {
	token, err := a.getToken(ctx)
	if err != nil {
		return err
	}
	reqURL := a.baseURL + "/database/search?q=test&type=artist&per_page=1"
	_, err = a.doRequest(ctx, reqURL, token)
	return err
}

func (a *Adapter) getToken(ctx context.Context) (string, error) {
	token, err := a.settings.GetAPIKey(ctx, provider.NameDiscogs)
	if err != nil {
		return "", fmt.Errorf("getting API token: %w", err)
	}
	if token == "" {
		return "", &provider.ErrAuthRequired{Provider: provider.NameDiscogs}
	}
	return token, nil
}

func (a *Adapter) doRequest(ctx context.Context, reqURL, token string) ([]byte, error) {
	// do performs one HTTP attempt. The limiter wait lives inside it so each
	// retry triggered by DoWithRetry still respects the per-provider budget.
	do := func(ctx context.Context) (*http.Response, error) {
		if err := a.limiter.Wait(ctx, provider.NameDiscogs); err != nil {
			return nil, &provider.ErrProviderUnavailable{
				Provider: provider.NameDiscogs,
				Cause:    fmt.Errorf("rate limiter: %w", err),
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("Authorization", "Discogs token="+token)
		req.Header.Set("User-Agent", version.UserAgent("Stillwater", ""))
		req.Header.Set("Accept", "application/json")
		a.logger.Debug("requesting", slog.String("url", reqURL))
		return a.client.Do(req)
	}

	// DoWithRetry consumes 429/503, so the status handling below only sees
	// 200/404/401/403/other.
	resp, err := provider.DoWithRetry(ctx, provider.SystemClock(), provider.NameDiscogs, provider.DefaultRetryPolicy(), do)
	if err != nil {
		var unavailable *provider.ErrProviderUnavailable
		if errors.As(err, &unavailable) {
			return nil, err
		}
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDiscogs,
			Cause:    err,
		}
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrNotFound{Provider: provider.NameDiscogs, ID: reqURL}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrAuthRequired{Provider: provider.NameDiscogs}
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &provider.ErrProviderUnavailable{
			Provider: provider.NameDiscogs,
			Cause:    fmt.Errorf("HTTP %d", resp.StatusCode),
		}
	}

	return io.ReadAll(io.LimitReader(resp.Body, 512*1024))
}

// getArtistReleases fetches a single page of releases for an artist from Discogs.
func (a *Adapter) getArtistReleases(ctx context.Context, artistID, token string, page int) (*ArtistReleasesResponse, error) {
	reqURL := fmt.Sprintf("%s/artists/%s/releases?sort=year&sort_order=desc&per_page=50&page=%d",
		a.baseURL, url.PathEscape(artistID), page)
	body, err := a.doRequest(ctx, reqURL, token)
	if err != nil {
		return nil, err
	}
	var resp ArtistReleasesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing artist releases: %w", err)
	}
	return &resp, nil
}

// GetReleaseGroups implements provider.ReleaseGroupFetcher for Discogs. It
// returns the artist's "master" releases (deduplicated, "Main" role only) as
// provider.ReleaseGroupInfo values so the identify/disambiguation flow can
// score Discogs candidates against the local library the same way it scores
// MusicBrainz candidates. artistID must be the numeric Discogs artist ID.
//
// The role/type filter mirrors aggregateStyles: only "master" releases where
// the artist's role is "Main" are treated as the artist's own albums, so
// compilations and guest appearances do not skew the album-match score. Results
// are capped (maxGroups across maxPages) to bound the number of Discogs API
// calls, matching the existing pagination budget used elsewhere in this
// adapter.
func (a *Adapter) GetReleaseGroups(ctx context.Context, artistID string) ([]provider.ReleaseGroupInfo, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	// Discogs release lookups require a numeric artist ID; reject anything else
	// (e.g. a MusicBrainz UUID) up front to avoid a wasted HTTP round-trip.
	if !isNumericID(artistID) {
		return nil, &provider.ErrNotFound{Provider: provider.NameDiscogs, ID: artistID}
	}

	token, err := a.getToken(ctx)
	if err != nil {
		return nil, err
	}

	const (
		maxGroups = 50
		maxPages  = 5
	)

	var groups []provider.ReleaseGroupInfo
	seen := make(map[int]bool) // deduplicate master IDs across pages

	for page := 1; page <= maxPages && len(groups) < maxGroups; page++ {
		resp, err := a.getArtistReleases(ctx, artistID, token, page)
		if err != nil {
			return nil, fmt.Errorf("fetching artist releases page %d: %w", page, err)
		}
		if len(resp.Releases) == 0 {
			break
		}

		for _, rel := range resp.Releases {
			if len(groups) >= maxGroups {
				break
			}
			if rel.Type != "master" || !strings.EqualFold(rel.Role, "Main") {
				continue
			}
			if seen[rel.ID] {
				continue
			}
			seen[rel.ID] = true

			info := provider.ReleaseGroupInfo{
				ID:    strconv.Itoa(rel.ID),
				Title: rel.Title,
			}
			// Discogs only exposes a release year (not a full date); surface it
			// as the FirstReleaseDate so downstream consumers have it available.
			if rel.Year > 0 {
				info.FirstReleaseDate = strconv.Itoa(rel.Year)
			}
			groups = append(groups, info)
		}

		// Stop if this was the last page.
		if page >= resp.Pagination.Pages {
			break
		}
	}

	return groups, nil
}

// GetMainReleaseTitles implements provider.MainReleaseTitleFetcher for Discogs.
// It returns the deduplicated titles of every entry the artist is credited on as
// role "Main", including BOTH "master" and release-level ("release") entries.
//
// This is intentionally broader than GetReleaseGroups (masters only): Discogs
// catalogs some albums only as a release, never promoting them to a master, so a
// master-only set undercounts the album match (e.g. "12 Stones" / "Beneath the
// Scars", which exists only as a release -- #1831). Titles are deduplicated by
// the same normalization artist.CompareAlbums uses, so a master and one-or-more
// same-title releases count once. Over-inclusion is safe: a remote title only
// raises the match score when it equals a local album title.
//
// Styles aggregation (aggregateStyles) is deliberately untouched -- it keeps its
// own master-only release walk, so broadening the album match does not change
// which masters feed the style counts.
//
// artistID must be the numeric Discogs artist ID. Results are capped
// (maxTitles across maxPages) to bound the number of Discogs API calls, matching
// the pagination budget GetReleaseGroups uses.
func (a *Adapter) GetMainReleaseTitles(ctx context.Context, artistID string) ([]string, error) {
	if provider.ShouldInjectFailure(a.Name()) {
		return nil, provider.ErrInjectedFailure
	}
	// Discogs release lookups require a numeric artist ID; reject anything else
	// (e.g. a MusicBrainz UUID) up front to avoid a wasted HTTP round-trip.
	if !isNumericID(artistID) {
		return nil, &provider.ErrNotFound{Provider: provider.NameDiscogs, ID: artistID}
	}

	token, err := a.getToken(ctx)
	if err != nil {
		return nil, err
	}

	const (
		maxTitles = 50
		maxPages  = 5
	)

	var titles []string
	seen := make(map[string]bool) // deduplicate by normalized title across master+release

	for page := 1; page <= maxPages && len(titles) < maxTitles; page++ {
		resp, err := a.getArtistReleases(ctx, artistID, token, page)
		if err != nil {
			return nil, fmt.Errorf("fetching artist releases page %d: %w", page, err)
		}
		if len(resp.Releases) == 0 {
			break
		}

		for _, rel := range resp.Releases {
			if len(titles) >= maxTitles {
				break
			}
			// Include every Main-role entry regardless of type (master OR
			// release); only the artist's own albums count toward the match.
			if !strings.EqualFold(rel.Role, "Main") {
				continue
			}
			norm := artist.NormalizeAlbumName(rel.Title)
			if norm == "" || seen[norm] {
				continue
			}
			seen[norm] = true
			titles = append(titles, rel.Title)
		}

		// Stop if this was the last page.
		if page >= resp.Pagination.Pages {
			break
		}
	}

	return titles, nil
}

// getMasterRelease fetches genre/style info from a master release.
func (a *Adapter) getMasterRelease(ctx context.Context, masterID int, token string) (*MasterRelease, error) {
	reqURL := fmt.Sprintf("%s/masters/%d", a.baseURL, masterID)
	body, err := a.doRequest(ctx, reqURL, token)
	if err != nil {
		return nil, err
	}
	var master MasterRelease
	if err := json.Unmarshal(body, &master); err != nil {
		return nil, fmt.Errorf("parsing master release: %w", err)
	}
	return &master, nil
}

// aggregateStyles fetches styles from an artist's master releases.
// Only considers "Main" role releases. Caps at 10 masters to limit API calls.
// Paginates through up to 5 pages (250 releases) to find enough masters.
func (a *Adapter) aggregateStyles(ctx context.Context, artistID, token string) ([]string, error) {
	const (
		maxMasters = 10
		maxPages   = 5
	)

	var masterIDs []int
	seen := make(map[int]bool) // deduplicate master IDs across pages

	for page := 1; page <= maxPages && len(masterIDs) < maxMasters; page++ {
		resp, err := a.getArtistReleases(ctx, artistID, token, page)
		if err != nil {
			return nil, fmt.Errorf("fetching artist releases page %d: %w", page, err)
		}
		if len(resp.Releases) == 0 {
			break
		}

		for _, rel := range resp.Releases {
			if len(masterIDs) >= maxMasters {
				break
			}
			if rel.Type != "master" || !strings.EqualFold(rel.Role, "Main") {
				continue
			}
			if seen[rel.ID] {
				continue
			}
			seen[rel.ID] = true
			masterIDs = append(masterIDs, rel.ID)
		}

		// Stop if this was the last page.
		if page >= resp.Pagination.Pages {
			break
		}
	}

	if len(masterIDs) == 0 {
		return nil, nil
	}

	// Aggregate style counts across all selected masters.
	counts := make(map[string]int)
	for _, id := range masterIDs {
		master, err := a.getMasterRelease(ctx, id, token)
		if err != nil {
			a.logger.Warn("failed to fetch master release for styles",
				slog.Int("master_id", id), slog.String("error", err.Error()))
			continue
		}
		for _, style := range master.Styles {
			counts[style]++
		}
	}

	return topStyles(counts, 10), nil
}

// topStyles returns the top N styles sorted by frequency (descending),
// then alphabetically for ties.
func topStyles(counts map[string]int, n int) []string {
	if len(counts) == 0 {
		return nil
	}
	type entry struct {
		name  string
		count int
	}
	entries := make([]entry, 0, len(counts))
	for name, count := range counts {
		entries = append(entries, entry{name, count})
	}
	// Sort by count descending, then name ascending for stability.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].name < entries[j].name
	})
	result := make([]string, 0, n)
	for i, e := range entries {
		if i >= n {
			break
		}
		result = append(result, e.name)
	}
	return result
}

func mapArtist(d *ArtistDetail) *provider.ArtistMetadata {
	meta := &provider.ArtistMetadata{
		ProviderID: strconv.Itoa(d.ID),
		DiscogsID:  strconv.Itoa(d.ID),
		Name:       d.Name,
		Biography:  d.Profile,
		URLs:       make(map[string]string),
	}

	for i, u := range d.URLs {
		meta.URLs[fmt.Sprintf("link_%d", i)] = u
	}

	for _, alias := range d.Aliases {
		meta.Aliases = append(meta.Aliases, alias.Name)
	}

	for _, member := range d.Members {
		meta.Members = append(meta.Members, provider.MemberInfo{
			Name:     member.Name,
			IsActive: member.Active,
		})
	}

	return meta
}

// isNumericID reports whether id contains only ASCII digits.
// Discogs uses numeric-only artist IDs; MusicBrainz UUIDs and artist names
// are not valid Discogs IDs.
func isNumericID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
