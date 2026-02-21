package scraper

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/sydlexius/stillwater/internal/provider"
)

// Executor performs per-field metadata scraping with fallback chain execution.
type Executor struct {
	service  *Service
	registry *provider.Registry
	logger   *slog.Logger
}

// FieldResult records the outcome of scraping a single field.
type FieldResult struct {
	Field       FieldName             `json:"field"`
	Provider    provider.ProviderName `json:"provider"`
	WasFallback bool                  `json:"was_fallback"`
	Err         error                 `json:"-"`
}

// NewExecutor creates a new scraper executor.
func NewExecutor(service *Service, registry *provider.Registry, logger *slog.Logger) *Executor {
	return &Executor{
		service:  service,
		registry: registry,
		logger:   logger.With(slog.String("component", "scraper-executor")),
	}
}

// ScrapeAll scrapes all enabled fields using the scraper configuration for the
// given scope. It returns a merged FetchResult compatible with the
// provider.Orchestrator output.
func (e *Executor) ScrapeAll(ctx context.Context, mbid, name, scope string) (*provider.FetchResult, error) {
	cfg, err := e.service.GetConfig(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("loading scraper config: %w", err)
	}

	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			URLs: make(map[string]string),
		},
	}

	// Cache provider results to avoid duplicate API calls
	var mu sync.Mutex
	cache := make(map[provider.ProviderName]*providerResult)
	selectedProviders := make(map[provider.ProviderName]bool)

	for _, field := range cfg.Fields {
		if !field.Enabled {
			continue
		}

		chain := cfg.FallbackChainFor(field.Category)
		if chain == nil {
			continue
		}

		fr := e.scrapeField(ctx, mbid, name, field, *chain, cache, &mu, result)
		if fr.Err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("%s: %s", fr.Field, fr.Err.Error()))
			continue
		}

		if fr.Provider != "" {
			selectedProviders[fr.Provider] = true
			result.Sources = append(result.Sources, provider.FieldSource{
				Field:    string(fr.Field),
				Provider: fr.Provider,
			})
		}
	}

	// Apply mergeable fields only from providers that were actually selected
	mu.Lock()
	for provName, pr := range cache {
		if !selectedProviders[provName] {
			continue
		}
		if pr.err != nil || pr.meta == nil {
			continue
		}
		applyMergeableFields(result, pr.meta, provName)
	}
	mu.Unlock()

	return result, nil
}

// scrapeField tries the primary provider for a field, then walks the fallback chain.
// Matched field data is written into the merged result.
func (e *Executor) scrapeField(
	ctx context.Context,
	mbid, name string,
	field FieldConfig,
	chain FallbackChain,
	cache map[provider.ProviderName]*providerResult,
	mu *sync.Mutex,
	result *provider.FetchResult,
) FieldResult {
	// Try primary provider first
	pr := e.getProviderResult(ctx, field.Primary, mbid, name, cache, mu)
	if pr.err == nil && applyFieldValue(field.Field, pr, result) {
		return FieldResult{Field: field.Field, Provider: field.Primary}
	}

	// Walk fallback chain
	for _, provName := range chain.Providers {
		if provName == field.Primary {
			continue // already tried
		}

		pr = e.getProviderResult(ctx, provName, mbid, name, cache, mu)
		if pr.err != nil {
			continue
		}

		if applyFieldValue(field.Field, pr, result) {
			return FieldResult{
				Field:       field.Field,
				Provider:    provName,
				WasFallback: true,
			}
		}
	}

	return FieldResult{Field: field.Field}
}

// providerResult caches a single provider's API response.
type providerResult struct {
	meta   *provider.ArtistMetadata
	images []provider.ImageResult
	err    error
}

// getProviderResult fetches and caches results from a single provider.
func (e *Executor) getProviderResult(
	ctx context.Context,
	name provider.ProviderName,
	mbid, artistName string,
	cache map[provider.ProviderName]*providerResult,
	mu *sync.Mutex,
) *providerResult {
	mu.Lock()
	if pr, ok := cache[name]; ok {
		mu.Unlock()
		return pr
	}
	mu.Unlock()

	p := e.registry.Get(name)
	if p == nil {
		pr := &providerResult{err: fmt.Errorf("provider %s not registered", name)}
		mu.Lock()
		cache[name] = pr
		mu.Unlock()
		return pr
	}

	pr := &providerResult{}

	// Use MBID first, fall back to name
	id := mbid
	if id == "" {
		id = artistName
	}

	if id != "" {
		meta, err := p.GetArtist(ctx, id)
		if err != nil {
			e.logger.Debug("provider GetArtist failed",
				slog.String("provider", string(name)),
				slog.String("error", err.Error()))
			pr.err = err
		} else {
			pr.meta = meta
		}
	}

	// Also fetch images if MBID is available
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

// applyFieldValue checks whether a provider result has data for a given field
// and writes the value into the merged FetchResult. Returns true if the field
// has a non-empty value that was applied.
func applyFieldValue(field FieldName, pr *providerResult, result *provider.FetchResult) bool {
	if pr.meta == nil && len(pr.images) == 0 {
		return false
	}

	meta := pr.meta
	if meta == nil {
		meta = &provider.ArtistMetadata{}
	}

	switch field {
	case FieldBiography:
		if meta.Biography == "" {
			return false
		}
		result.Metadata.Biography = meta.Biography
		return true
	case FieldGenres:
		if len(meta.Genres) == 0 {
			return false
		}
		result.Metadata.Genres = meta.Genres
		return true
	case FieldStyles:
		if len(meta.Styles) == 0 {
			return false
		}
		result.Metadata.Styles = meta.Styles
		return true
	case FieldMoods:
		if len(meta.Moods) == 0 {
			return false
		}
		result.Metadata.Moods = meta.Moods
		return true
	case FieldMembers:
		if len(meta.Members) == 0 {
			return false
		}
		result.Metadata.Members = meta.Members
		return true
	case FieldFormed:
		if meta.Formed == "" {
			return false
		}
		result.Metadata.Formed = meta.Formed
		return true
	case FieldBorn:
		if meta.Born == "" {
			return false
		}
		result.Metadata.Born = meta.Born
		return true
	case FieldDied:
		if meta.Died == "" {
			return false
		}
		result.Metadata.Died = meta.Died
		return true
	case FieldDisbanded:
		if meta.Disbanded == "" {
			return false
		}
		result.Metadata.Disbanded = meta.Disbanded
		return true
	case FieldThumb, FieldFanart, FieldLogo, FieldBanner:
		imgType := fieldToImageType(field)
		found := false
		for _, img := range pr.images {
			if img.Type == imgType {
				result.Images = append(result.Images, img)
				found = true
			}
		}
		return found
	}

	return false
}

// applyMergeableFields copies non-field-specific data (IDs, URLs, aliases) from
// a provider result into the merged FetchResult.
func applyMergeableFields(result *provider.FetchResult, meta *provider.ArtistMetadata, source provider.ProviderName) {
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
	if meta.SortName != "" && result.Metadata.SortName == "" {
		result.Metadata.SortName = meta.SortName
	}
	if meta.Type != "" && result.Metadata.Type == "" {
		result.Metadata.Type = meta.Type
	}
	if meta.Gender != "" && result.Metadata.Gender == "" {
		result.Metadata.Gender = meta.Gender
	}
	if meta.Disambiguation != "" && result.Metadata.Disambiguation == "" {
		result.Metadata.Disambiguation = meta.Disambiguation
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

	_ = source // reserved for future per-source tracking
}

func fieldToImageType(field FieldName) provider.ImageType {
	switch field {
	case FieldThumb:
		return provider.ImageThumb
	case FieldFanart:
		return provider.ImageFanart
	case FieldLogo:
		return provider.ImageLogo
	case FieldBanner:
		return provider.ImageBanner
	default:
		return provider.ImageType(field)
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
