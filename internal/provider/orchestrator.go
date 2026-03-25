package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/sydlexius/stillwater/internal/provider/tagdict"
)

// FieldSource records which provider supplied a given field.
type FieldSource struct {
	Field    string       `json:"field"`
	Provider ProviderName `json:"provider"`
}

// FetchResult holds the merged result of querying multiple providers.
type FetchResult struct {
	Metadata           *ArtistMetadata `json:"metadata"`
	Images             []ImageResult   `json:"images"`
	Sources            []FieldSource   `json:"sources"`
	Errors             []string        `json:"errors"`
	AttemptedProviders []ProviderName  `json:"attempted_providers,omitempty"`
	AttemptedFields    []string        `json:"attempted_fields,omitempty"`
}

// ScraperExecutor is implemented by the scraper.Executor to avoid circular imports.
// When set on the Orchestrator, FetchMetadata delegates to it.
type ScraperExecutor interface {
	ScrapeAll(ctx context.Context, mbid, name, scope string, providerIDs map[ProviderName]string) (*FetchResult, error)
}

// Orchestrator queries providers in priority order and merges results.
type Orchestrator struct {
	registry *Registry
	settings *SettingsService
	executor ScraperExecutor
	logger   *slog.Logger
}

// NewOrchestrator creates a new Orchestrator.
func NewOrchestrator(registry *Registry, settings *SettingsService, logger *slog.Logger) *Orchestrator {
	return &Orchestrator{
		registry: registry,
		settings: settings,
		logger:   logger.With(slog.String("component", "orchestrator")),
	}
}

// SetExecutor configures the scraper executor for FetchMetadata delegation.
func (o *Orchestrator) SetExecutor(e ScraperExecutor) {
	o.executor = e
}

// FetchMetadata queries all providers in priority order and merges the results.
// It uses the artist's MBID when available, falling back to name-based search.
// providerIDs supplies provider-specific IDs (AudioDB numeric ID, Discogs ID, etc.)
// so that each provider receives its own stored ID instead of the MBID. A nil or
// empty map is safe: the function allocates an internal map so that IDs discovered
// from earlier providers' URL results (e.g., a Discogs numeric ID extracted from a
// MusicBrainz URL) can be used when calling later providers.
// When a ScraperExecutor is configured, delegates to it for scraper-config-driven
// per-field fetching with fallback chains.
func (o *Orchestrator) FetchMetadata(ctx context.Context, mbid, name string, providerIDs map[ProviderName]string) (*FetchResult, error) {
	if o.executor != nil {
		return o.executor.ScrapeAll(ctx, mbid, name, "global", providerIDs)
	}

	priorities, err := o.settings.GetPriorities(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading priorities: %w", err)
	}

	available, err := o.settings.AvailableProviderNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading available providers: %w", err)
	}

	result := &FetchResult{
		Metadata: &ArtistMetadata{
			URLs: make(map[string]string),
		},
	}

	// Ensure providerIDs is writable so EnrichProviderIDs can populate it
	// with IDs extracted from earlier providers' URL results.
	if providerIDs == nil {
		providerIDs = make(map[ProviderName]string)
	}

	// Cache provider results to avoid duplicate calls
	var mu sync.Mutex
	cache := make(map[ProviderName]*providerResult)

	for _, pri := range priorities {
		queried := false
		isImageField := isImageFieldName(pri.Field)
		for _, provName := range pri.EnabledProviders() {
			if !available[provName] {
				continue
			}

			pr := o.getProviderResult(ctx, provName, mbid, name, providerIDs, cache, &mu)
			if pr.err != nil {
				continue
			}

			// After each successful provider call, extract any provider IDs
			// from the returned URLs and feed them to subsequent calls.
			// For example, MusicBrainz returns Discogs URLs containing the
			// numeric Discogs ID, so we extract it here before Discogs is
			// called, avoiding a wasted MBID-based request that always 404s.
			EnrichProviderIDs(pr.meta, providerIDs)

			queried = true
			if applyField(result, pri.Field, pr, provName) {
				// For image fields and aggregated tag fields (genres/styles/moods),
				// continue collecting candidates from all providers instead of
				// stopping at the first match. Text fields use first-match-wins
				// since the priority order determines the preferred source.
				if !isImageField && !isAggregatedField(pri.Field) {
					break
				}
			}
		}
		if queried {
			result.AttemptedFields = append(result.AttemptedFields, pri.Field)
		}
	}

	// Final backfill pass for the merged metadata (catches any IDs not yet
	// populated from earlier per-provider enrichment).
	extractProviderIDsFromURLs(result.Metadata)

	// Record which providers were successfully queried so callers can update
	// per-provider fetch timestamps on the artist record. Providers with
	// transient errors (timeouts, 5xx) are excluded to avoid hiding outages
	// behind misleading "attempted" markers -- consistent with the executor
	// path which already applies this filter.
	for provName, pr := range cache {
		if pr.err != nil {
			continue
		}
		result.AttemptedProviders = append(result.AttemptedProviders, provName)
	}

	return result, nil
}

// FetchImages queries all configured, image-capable providers and collects
// every image candidate they return. All providers are always queried so that
// callers (image search UI, ImageFixer quality sorting) receive the full set
// of candidates to choose from.
// providerIDs supplies provider-specific IDs for providers that do not accept MBIDs
// (e.g. Deezer uses its own numeric ID). Providers without an entry in providerIDs
// receive the MBID. Providers with an empty entry are skipped.
func (o *Orchestrator) FetchImages(ctx context.Context, mbid string, providerIDs map[ProviderName]string) (*FetchResult, error) {
	result := &FetchResult{
		Metadata: &ArtistMetadata{},
	}

	providers, err := o.availableProviders(ctx)
	if err != nil {
		return nil, err
	}

	for _, p := range providers {
		id := mbid
		if pid, ok := providerIDs[p.Name()]; ok {
			if pid == "" {
				continue // provider-specific ID not known; skip rather than fail
			}
			id = pid
		}
		images, err := p.GetImages(ctx, id)
		if err != nil {
			var notFound *ErrNotFound
			if errors.As(err, &notFound) {
				o.logger.Debug("provider has no images for artist",
					slog.String("provider", string(p.Name())),
					slog.String("id", id))
				continue
			}
			o.logger.Warn("provider image fetch failed",
				slog.String("provider", string(p.Name())),
				slog.String("error", err.Error()))
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", p.Name(), err.Error()))
			continue
		}
		result.Images = append(result.Images, images...)
	}

	return result, nil
}

// Search queries all configured providers that support search and merges results.
func (o *Orchestrator) Search(ctx context.Context, name string) ([]ArtistSearchResult, error) {
	var allResults []ArtistSearchResult

	providers, err := o.availableProviders(ctx)
	if err != nil {
		return nil, err
	}

	for _, p := range providers {
		results, err := p.SearchArtist(ctx, name)
		if err != nil {
			o.logger.Warn("provider search failed",
				slog.String("provider", string(p.Name())),
				slog.String("error", err.Error()))
			continue
		}
		allResults = append(allResults, results...)
	}

	return allResults, nil
}

// availableProviders returns only the registered providers whose API keys are
// configured (or that do not require a key). This prevents the orchestrator
// from calling unconfigured providers and producing noisy WARN logs.
func (o *Orchestrator) availableProviders(ctx context.Context) ([]Provider, error) {
	available, err := o.settings.AvailableProviderNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading available providers: %w", err)
	}
	var result []Provider
	for _, p := range o.registry.All() {
		if available[p.Name()] {
			result = append(result, p)
		}
	}
	return result, nil
}

type providerResult struct {
	meta   *ArtistMetadata
	images []ImageResult
	err    error
}

func (o *Orchestrator) getProviderResult(ctx context.Context, name ProviderName, mbid string, artistName string, providerIDs map[ProviderName]string, cache map[ProviderName]*providerResult, mu *sync.Mutex) *providerResult {
	mu.Lock()
	if pr, ok := cache[name]; ok {
		mu.Unlock()
		return pr
	}
	mu.Unlock()

	p := o.registry.Get(name)
	if p == nil {
		pr := &providerResult{err: fmt.Errorf("provider %s not registered", name)}
		mu.Lock()
		cache[name] = pr
		mu.Unlock()
		return pr
	}

	pr := &providerResult{}

	// Lookup precedence: provider-specific ID > MBID > artist name.
	// Providers like AudioDB, Discogs, and Deezer have their own numeric IDs
	// that are more reliable than passing an MBID they may not recognize.
	usedProviderID := false
	id := mbid
	if pid, ok := providerIDs[name]; ok && pid != "" {
		id = pid
		usedProviderID = true
	} else if id == "" {
		id = artistName
	}

	// queryID tracks the identifier actually passed to the most recent
	// GetArtist call. It may differ from id after a name-based retry.
	queryID := id

	if id != "" {
		meta, err := p.GetArtist(ctx, id)
		// If we used an MBID (not a provider-specific ID) and the provider
		// returned not-found, retry with the artist name -- but only for
		// providers that support name lookups (e.g. Genius, Last.fm).
		if err != nil && !usedProviderID && mbid != "" && artistName != "" {
			var notFound *ErrNotFound
			if errors.As(err, &notFound) {
				if nlp, ok := p.(NameLookupProvider); ok && nlp.SupportsNameLookup() {
					o.logger.Debug("retrying with artist name after MBID not-found",
						slog.String("provider", string(name)),
						slog.String("name", artistName))
					queryID = artistName
					meta, err = p.GetArtist(ctx, artistName)
				}
			}
		}
		if err != nil {
			// ErrNotFound means the provider was reached and definitively said
			// "no data". Treat this as a successful query (no error) so the
			// field is marked as attempted and stale data can be cleared.
			// Transient failures (timeouts, 5xx) remain as real errors.
			var notFound *ErrNotFound
			if errors.As(err, &notFound) {
				o.logger.Debug("provider has no data for artist",
					slog.String("provider", string(name)),
					slog.String("id", queryID))
			} else {
				o.logger.Debug("provider GetArtist failed",
					slog.String("provider", string(name)),
					slog.String("error", err.Error()))
				pr.err = err
			}
		} else {
			pr.meta = meta
		}
	}

	// Fetch images using the same precedence: provider ID > MBID.
	imgID := mbid
	if pid, ok := providerIDs[name]; ok && pid != "" {
		imgID = pid
	}
	if imgID != "" {
		images, err := p.GetImages(ctx, imgID)
		if err != nil {
			var notFound *ErrNotFound
			if errors.As(err, &notFound) {
				o.logger.Debug("provider has no images for artist",
					slog.String("provider", string(name)),
					slog.String("id", imgID))
			} else {
				o.logger.Debug("provider GetImages failed",
					slog.String("provider", string(name)),
					slog.String("error", err.Error()))
			}
		} else {
			pr.images = images
		}
	}

	mu.Lock()
	cache[name] = pr
	mu.Unlock()
	return pr
}

// applyField applies data from a provider result to the merged result for a specific field.
// Returns true if data was applied (i.e., the field was populated).
func applyField(result *FetchResult, field string, pr *providerResult, source ProviderName) bool {
	if pr.meta == nil {
		return false
	}

	meta := pr.meta

	switch field {
	case "biography":
		if meta.Biography != "" && result.Metadata.Biography == "" && !IsJunkBiography(meta.Biography) {
			result.Metadata.Biography = meta.Biography
			result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
			return true
		}
	case "genres":
		if len(meta.Genres) > 0 {
			before := len(result.Metadata.Genres)
			result.Metadata.Genres = tagdict.MergeAndDeduplicate(result.Metadata.Genres, meta.Genres)
			if len(result.Metadata.Genres) > before && !hasFieldSource(result.Sources, field) {
				result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
			}
			return len(result.Metadata.Genres) > before
		}
	case "styles":
		if len(meta.Styles) > 0 {
			before := len(result.Metadata.Styles)
			result.Metadata.Styles = tagdict.MergeAndDeduplicate(result.Metadata.Styles, meta.Styles)
			if len(result.Metadata.Styles) > before && !hasFieldSource(result.Sources, field) {
				result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
			}
			return len(result.Metadata.Styles) > before
		}
	case "moods":
		if len(meta.Moods) > 0 {
			before := len(result.Metadata.Moods)
			result.Metadata.Moods = tagdict.MergeAndDeduplicate(result.Metadata.Moods, meta.Moods)
			if len(result.Metadata.Moods) > before && !hasFieldSource(result.Sources, field) {
				result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
			}
			return len(result.Metadata.Moods) > before
		}
	case "members":
		if len(meta.Members) > 0 && len(result.Metadata.Members) == 0 {
			result.Metadata.Members = meta.Members
			result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
			return true
		}
	case "formed":
		if meta.Formed != "" && result.Metadata.Formed == "" {
			result.Metadata.Formed = meta.Formed
			result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
			return true
		}
	case "thumb", "fanart", "logo", "banner":
		// For image fields, collect all matching candidates from this provider.
		// Unlike text fields, images aggregate across providers so users can
		// choose from multiple candidates. Individual images carry their own
		// .Source for per-image provenance.
		imgType := fieldToImageType(field)
		found := false
		for _, img := range pr.images {
			if img.Type == imgType {
				result.Images = append(result.Images, img)
				found = true
			}
		}
		if found {
			// Only record the first (highest-priority) provider as the
			// field source. MetadataSources is map[field]provider (last
			// write wins), so appending multiple providers would record
			// the lowest-priority one instead of the preferred one.
			if !hasFieldSource(result.Sources, field) {
				result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
			}
			return true
		}
	}

	// Also merge provider IDs when available
	if meta.MusicBrainzID != "" && result.Metadata.MusicBrainzID == "" {
		result.Metadata.MusicBrainzID = meta.MusicBrainzID
	}
	if meta.AudioDBID != "" && result.Metadata.AudioDBID == "" {
		result.Metadata.AudioDBID = meta.AudioDBID
	}
	if meta.DiscogsID != "" && result.Metadata.DiscogsID == "" {
		result.Metadata.DiscogsID = meta.DiscogsID
	}
	if meta.WikidataID != "" && result.Metadata.WikidataID == "" {
		result.Metadata.WikidataID = meta.WikidataID
	}
	if meta.DeezerID != "" && result.Metadata.DeezerID == "" {
		result.Metadata.DeezerID = meta.DeezerID
	}
	if meta.AllMusicID != "" && result.Metadata.AllMusicID == "" {
		result.Metadata.AllMusicID = meta.AllMusicID
	}
	if meta.SpotifyID != "" && result.Metadata.SpotifyID == "" {
		result.Metadata.SpotifyID = meta.SpotifyID
	}
	if meta.Name != "" && result.Metadata.Name == "" {
		result.Metadata.Name = meta.Name
	}

	// Merge URLs
	for k, v := range meta.URLs {
		if _, exists := result.Metadata.URLs[k]; !exists {
			result.Metadata.URLs[k] = v
		}
	}

	// Merge aliases (deduplicated)
	for _, alias := range meta.Aliases {
		if !containsString(result.Metadata.Aliases, alias) {
			result.Metadata.Aliases = append(result.Metadata.Aliases, alias)
		}
	}

	return false
}

// FieldProviderResult holds one provider's value for a single metadata field.
type FieldProviderResult struct {
	Provider ProviderName `json:"provider"`
	Value    string       `json:"value,omitempty"`
	Values   []string     `json:"values,omitempty"`
	Members  []MemberInfo `json:"members,omitempty"`
	HasData  bool         `json:"has_data"`
	Error    string       `json:"error,omitempty"`
}

// FetchFieldFromProviders queries all configured providers for a given field
// and returns each provider's result without merging. This enables a
// side-by-side comparison UI where the user picks which provider's value to use.
func (o *Orchestrator) FetchFieldFromProviders(ctx context.Context, mbid, name, field string, providerIDs map[ProviderName]string) ([]FieldProviderResult, error) {
	priorities, err := o.settings.GetPriorities(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading priorities: %w", err)
	}

	available, err := o.settings.AvailableProviderNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading available providers: %w", err)
	}

	// Find which providers are enabled and available for this field
	var providers []ProviderName
	for _, pri := range priorities {
		if pri.Field == field {
			for _, p := range pri.EnabledProviders() {
				if available[p] {
					providers = append(providers, p)
				}
			}
			break
		}
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers configured for field %s", field)
	}

	var mu sync.Mutex
	cache := make(map[ProviderName]*providerResult)
	var results []FieldProviderResult

	for _, provName := range providers {
		pr := o.getProviderResult(ctx, provName, mbid, name, providerIDs, cache, &mu)
		fpr := FieldProviderResult{
			Provider: provName,
		}
		if pr.err != nil {
			// Only real errors reach pr.err; ErrNotFound is handled in
			// getProviderResult and does not set pr.err.
			fpr.Error = pr.err.Error()
		} else if pr.meta != nil {
			extractFieldForComparison(&fpr, field, pr.meta)
		}
		results = append(results, fpr)
	}

	return results, nil
}

// extractFieldForComparison populates a FieldProviderResult from provider metadata.
func extractFieldForComparison(fpr *FieldProviderResult, field string, meta *ArtistMetadata) {
	switch field {
	case "biography":
		if meta.Biography != "" {
			fpr.Value = meta.Biography
			fpr.HasData = true
		}
	case "genres":
		if len(meta.Genres) > 0 {
			fpr.Values = meta.Genres
			fpr.HasData = true
		}
	case "styles":
		if len(meta.Styles) > 0 {
			fpr.Values = meta.Styles
			fpr.HasData = true
		}
	case "moods":
		if len(meta.Moods) > 0 {
			fpr.Values = meta.Moods
			fpr.HasData = true
		}
	case "members":
		if len(meta.Members) > 0 {
			fpr.Members = meta.Members
			fpr.HasData = true
		}
	case "formed":
		if meta.Formed != "" {
			fpr.Value = meta.Formed
			fpr.HasData = true
		}
	case "born":
		if meta.Born != "" {
			fpr.Value = meta.Born
			fpr.HasData = true
		}
	case "died":
		if meta.Died != "" {
			fpr.Value = meta.Died
			fpr.HasData = true
		}
	case "disbanded":
		if meta.Disbanded != "" {
			fpr.Value = meta.Disbanded
			fpr.HasData = true
		}
	case "years_active":
		if meta.YearsActive != "" {
			fpr.Value = meta.YearsActive
			fpr.HasData = true
		}
	case "type":
		if meta.Type != "" {
			fpr.Value = meta.Type
			fpr.HasData = true
		}
	case "gender":
		if meta.Gender != "" {
			fpr.Value = meta.Gender
			fpr.HasData = true
		}
	}
}

// SearchForLinking queries only the specified providers for disambiguation.
// Unlike Search (which queries all providers), this targets only providers
// whose IDs need to be linked (e.g., MusicBrainz for MBID, Discogs for DiscogsID).
func (o *Orchestrator) SearchForLinking(ctx context.Context, name string, providers []ProviderName) ([]ArtistSearchResult, error) {
	var allResults []ArtistSearchResult

	for _, provName := range providers {
		p := o.registry.Get(provName)
		if p == nil {
			continue
		}
		results, err := p.SearchArtist(ctx, name)
		if err != nil {
			o.logger.Warn("provider search failed",
				slog.String("provider", string(provName)),
				slog.String("error", err.Error()))
			continue
		}
		allResults = append(allResults, results...)
	}

	return allResults, nil
}

// isImageFieldName returns true for metadata fields that represent image slots.
// Image fields aggregate candidates from all providers (unlike text fields
// which use first-match-wins).
//
// NOTE: This list must stay in sync with the image cases in applyField and
// fieldToImageType in this file. Only fields that appear as priority field
// names need to be listed here.
func isImageFieldName(field string) bool {
	switch field {
	case "thumb", "fanart", "logo", "banner":
		return true
	default:
		return false
	}
}

// isAggregatedField returns true for metadata fields that accumulate values
// from all providers rather than stopping at the first match. Tag fields
// (genres, styles, moods) are aggregated and deduplicated across providers
// so that each provider's unique tags contribute to the final result.
func isAggregatedField(field string) bool {
	return field == "genres" || field == "styles" || field == "moods"
}

// hasFieldSource returns true if the Sources slice already contains an entry
// for the given field name.
func hasFieldSource(sources []FieldSource, field string) bool {
	for _, s := range sources {
		if s.Field == field {
			return true
		}
	}
	return false
}

func fieldToImageType(field string) ImageType {
	switch field {
	case "thumb":
		return ImageThumb
	case "fanart":
		return ImageFanart
	case "logo":
		return ImageLogo
	case "banner":
		return ImageBanner
	default:
		return ImageType(field)
	}
}

func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// EnrichProviderIDs extracts provider-specific IDs from a single provider
// result's metadata and updates the providerIDs map in-place. This allows
// IDs discovered from one provider (e.g., a Discogs URL in MusicBrainz's
// response) to be available before the corresponding provider is called.
//
// This solves the sequencing problem where MusicBrainz returns a Discogs URL
// containing the numeric ID, but that ID was previously only extracted after
// all providers had already been called (too late for Discogs to use it).
func EnrichProviderIDs(meta *ArtistMetadata, providerIDs map[ProviderName]string) {
	if meta == nil || providerIDs == nil {
		return
	}

	// Temporarily extract IDs from this provider's URLs into a scratch
	// metadata struct, then copy any newly discovered IDs into providerIDs.
	scratch := &ArtistMetadata{
		URLs:       meta.URLs,
		DiscogsID:  meta.DiscogsID,
		DeezerID:   meta.DeezerID,
		AllMusicID: meta.AllMusicID,
		SpotifyID:  meta.SpotifyID,
	}
	extractProviderIDsFromURLs(scratch)

	// Only set IDs that are not already populated. We preserve non-empty
	// stored IDs because the caller's existing ID is authoritative.
	// ProviderIDMap() includes keys with empty-string values for unknown
	// providers, so we treat empty strings as unset.
	if scratch.DiscogsID != "" {
		if current := providerIDs[NameDiscogs]; current == "" {
			providerIDs[NameDiscogs] = scratch.DiscogsID
		}
	}
	if scratch.DeezerID != "" {
		if current := providerIDs[NameDeezer]; current == "" {
			providerIDs[NameDeezer] = scratch.DeezerID
		}
	}
	if scratch.AllMusicID != "" {
		if current := providerIDs[NameAllMusic]; current == "" {
			providerIDs[NameAllMusic] = scratch.AllMusicID
		}
	}
	if scratch.SpotifyID != "" {
		if current := providerIDs[NameSpotify]; current == "" {
			providerIDs[NameSpotify] = scratch.SpotifyID
		}
	}
}

// extractProviderIDsFromURLs backfills provider IDs from URL relations returned
// by MusicBrainz when the IDs are not yet set.
//
// MusicBrainz URL relations look like:
//
//	discogs:  "https://www.discogs.com/artist/24941"       -> "24941"
//	discogs:  "https://www.discogs.com/artist/24941-a-ha"  -> "24941"
//	wikidata: "https://www.wikidata.org/wiki/Q44190"       -> "Q44190"
//	deezer:   "https://www.deezer.com/artist/3106"         -> "3106"
//	allmusic: "https://www.allmusic.com/artist/mn0000505828" -> "mn0000505828"
func extractProviderIDsFromURLs(meta *ArtistMetadata) {
	if meta == nil {
		return
	}

	if meta.DiscogsID == "" {
		if u, ok := meta.URLs["discogs"]; ok && u != "" {
			// Last path segment may be "24941" or "24941-artist-name".
			// Extract only the leading numeric portion.
			if idx := strings.LastIndex(u, "/"); idx >= 0 {
				segment := u[idx+1:]
				end := strings.IndexFunc(segment, func(r rune) bool { return r < '0' || r > '9' })
				if end < 0 {
					end = len(segment)
				}
				if end > 0 {
					meta.DiscogsID = segment[:end]
				}
			}
		}
	}

	if meta.WikidataID == "" {
		if u, ok := meta.URLs["wikidata"]; ok && u != "" {
			// Last path segment is the Q-item ID; strip any query/fragment first.
			if qIdx := strings.IndexAny(u, "?#"); qIdx >= 0 {
				u = u[:qIdx]
			}
			if idx := strings.LastIndex(u, "/"); idx >= 0 {
				candidate := u[idx+1:]
				if len(candidate) > 1 && candidate[0] == 'Q' {
					meta.WikidataID = candidate
				}
			}
		}
	}

	if meta.DeezerID == "" {
		if u, ok := meta.URLs["deezer"]; ok && u != "" {
			// Last path segment is the numeric Deezer artist ID.
			if idx := strings.LastIndex(u, "/"); idx >= 0 {
				segment := u[idx+1:]
				end := strings.IndexFunc(segment, func(r rune) bool { return r < '0' || r > '9' })
				if end < 0 {
					end = len(segment)
				}
				if end > 0 {
					meta.DeezerID = segment[:end]
				}
			}
		}
	}

	if meta.AllMusicID == "" {
		if u, ok := meta.URLs["allmusic"]; ok && u != "" {
			// AllMusic artist URLs: https://www.allmusic.com/artist/mn0000505828
			// or https://www.allmusic.com/artist/dolly-parton-mn0000205560
			// The ID is always the "mn" followed by digits at the end.
			if idx := strings.LastIndex(u, "/"); idx >= 0 {
				segment := u[idx+1:]
				// Strip any query params or fragments
				if qIdx := strings.IndexAny(segment, "?#"); qIdx >= 0 {
					segment = segment[:qIdx]
				}
				// The mn-ID may be the entire segment or suffixed after a slug.
				// Look for "mn" followed by digits.
				if mnIdx := strings.LastIndex(segment, "mn"); mnIdx >= 0 {
					candidate := segment[mnIdx:]
					if isAllMusicID(candidate) {
						meta.AllMusicID = candidate
					}
				}
			}
		}
	}

	if meta.SpotifyID == "" {
		if u, ok := meta.URLs["spotify"]; ok && u != "" {
			// Spotify artist URLs: https://open.spotify.com/artist/{id}
			const prefix = "/artist/"
			if idx := strings.LastIndex(u, prefix); idx >= 0 {
				candidate := u[idx+len(prefix):]
				candidate = strings.TrimRight(candidate, "/")
				// Strip any query params
				if qIdx := strings.IndexAny(candidate, "?#"); qIdx >= 0 {
					candidate = candidate[:qIdx]
				}
				if isSpotifyID(candidate) {
					meta.SpotifyID = candidate
				}
			}
		}
	}
}

// isAllMusicID reports whether s matches the AllMusic artist ID format: "mn" followed by 10 digits.
func isAllMusicID(s string) bool {
	if len(s) != 12 || s[0] != 'm' || s[1] != 'n' {
		return false
	}
	for _, c := range s[2:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// BuildProviderIDMap constructs a provider-specific ID map from individual ID
// strings. Takes strings (not *artist.Artist) to avoid a circular import
// between provider and artist.
//
// Deprecated: Callers with an *artist.Artist should use Artist.ProviderIDMap()
// instead. This function remains for use in contexts without an Artist struct.
//
// All four providers are always included in the map. For FetchMetadata's
// getProviderResult, an empty value causes fallback to MBID. For FetchImages,
// an empty value signals "skip this provider" (it cannot accept MBIDs).
// Omitting the key entirely would cause FetchImages to pass the MBID to
// providers that only accept their own numeric ID format.
func BuildProviderIDMap(audioDBID, discogsID, deezerID, spotifyID string) map[ProviderName]string {
	return map[ProviderName]string{
		NameAudioDB: audioDBID,
		NameDiscogs: discogsID,
		NameDeezer:  deezerID,
		NameSpotify: spotifyID,
	}
}

// isSpotifyID reports whether s is a valid 22-character base62 Spotify ID.
// This is a package-private copy to avoid importing the spotify package
// (which would create a circular dependency).
func isSpotifyID(s string) bool {
	if len(s) != 22 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
