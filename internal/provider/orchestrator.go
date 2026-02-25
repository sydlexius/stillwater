package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
}

// ScraperExecutor is implemented by the scraper.Executor to avoid circular imports.
// When set on the Orchestrator, FetchMetadata delegates to it.
type ScraperExecutor interface {
	ScrapeAll(ctx context.Context, mbid, name, scope string) (*FetchResult, error)
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
// When a ScraperExecutor is configured, delegates to it for scraper-config-driven
// per-field fetching with fallback chains.
func (o *Orchestrator) FetchMetadata(ctx context.Context, mbid string, name string) (*FetchResult, error) {
	if o.executor != nil {
		return o.executor.ScrapeAll(ctx, mbid, name, "global")
	}

	priorities, err := o.settings.GetPriorities(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading priorities: %w", err)
	}

	result := &FetchResult{
		Metadata: &ArtistMetadata{
			URLs: make(map[string]string),
		},
	}

	// Cache provider results to avoid duplicate calls
	var mu sync.Mutex
	cache := make(map[ProviderName]*providerResult)

	for _, pri := range priorities {
		for _, provName := range pri.EnabledProviders() {
			pr := o.getProviderResult(ctx, provName, mbid, name, cache, &mu)
			if pr.err != nil {
				continue
			}

			if applyField(result, pri.Field, pr, provName) {
				break
			}
		}
	}

	// Backfill provider IDs from MusicBrainz URL relations when not already set.
	// MusicBrainz returns discogs and wikidata URLs; extract the numeric/Q IDs.
	extractProviderIDsFromURLs(result.Metadata)

	// Record which providers were actually queried so callers can update
	// per-provider fetch timestamps on the artist record.
	for provName := range cache {
		result.AttemptedProviders = append(result.AttemptedProviders, provName)
	}

	return result, nil
}

// FetchImages queries all image-capable providers and merges results by priority.
func (o *Orchestrator) FetchImages(ctx context.Context, mbid string) (*FetchResult, error) {
	result := &FetchResult{
		Metadata: &ArtistMetadata{},
	}

	for _, p := range o.registry.All() {
		images, err := p.GetImages(ctx, mbid)
		if err != nil {
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

// Search queries all providers that support search and merges results.
func (o *Orchestrator) Search(ctx context.Context, name string) ([]ArtistSearchResult, error) {
	var allResults []ArtistSearchResult

	for _, p := range o.registry.All() {
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

type providerResult struct {
	meta   *ArtistMetadata
	images []ImageResult
	err    error
}

func (o *Orchestrator) getProviderResult(ctx context.Context, name ProviderName, mbid string, artistName string, cache map[ProviderName]*providerResult, mu *sync.Mutex) *providerResult {
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

	// Try to get artist metadata using MBID first, then name
	id := mbid
	if id == "" {
		id = artistName
	}

	if id != "" {
		meta, err := p.GetArtist(ctx, id)
		if err != nil {
			o.logger.Debug("provider GetArtist failed",
				slog.String("provider", string(name)),
				slog.String("error", err.Error()))
			pr.err = err
		} else {
			pr.meta = meta
		}
	}

	// Also fetch images if available
	if mbid != "" {
		images, err := p.GetImages(ctx, mbid)
		if err == nil {
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
		if meta.Biography != "" && result.Metadata.Biography == "" {
			result.Metadata.Biography = meta.Biography
			result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
			return true
		}
	case "genres":
		if len(meta.Genres) > 0 && len(result.Metadata.Genres) == 0 {
			result.Metadata.Genres = meta.Genres
			result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
			return true
		}
	case "styles":
		if len(meta.Styles) > 0 && len(result.Metadata.Styles) == 0 {
			result.Metadata.Styles = meta.Styles
			result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
			return true
		}
	case "moods":
		if len(meta.Moods) > 0 && len(result.Metadata.Moods) == 0 {
			result.Metadata.Moods = meta.Moods
			result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
			return true
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
		// For image fields, collect from provider images
		imgType := fieldToImageType(field)
		for _, img := range pr.images {
			if img.Type == imgType {
				result.Images = append(result.Images, img)
			}
		}
		if len(pr.images) > 0 {
			result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
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

// FetchFieldFromProviders queries all providers configured for a given field
// and returns each provider's result without merging. This enables a
// side-by-side comparison UI where the user picks which provider's value to use.
func (o *Orchestrator) FetchFieldFromProviders(ctx context.Context, mbid, name, field string) ([]FieldProviderResult, error) {
	priorities, err := o.settings.GetPriorities(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading priorities: %w", err)
	}

	// Find which providers are enabled for this field
	var providers []ProviderName
	for _, pri := range priorities {
		if pri.Field == field {
			providers = pri.EnabledProviders()
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
		pr := o.getProviderResult(ctx, provName, mbid, name, cache, &mu)
		fpr := FieldProviderResult{
			Provider: provName,
		}
		if pr.err != nil {
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

// extractProviderIDsFromURLs backfills DiscogsID and WikidataID from URL
// relations returned by MusicBrainz when the IDs are not yet set.
//
// MusicBrainz URL relations look like:
//
//	discogs:  "https://www.discogs.com/artist/24941"       -> "24941"
//	discogs:  "https://www.discogs.com/artist/24941-a-ha"  -> "24941"
//	wikidata: "https://www.wikidata.org/wiki/Q44190"       -> "Q44190"
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
}
