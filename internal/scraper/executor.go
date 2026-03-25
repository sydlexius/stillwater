package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/sydlexius/stillwater/internal/provider"
)

// Executor performs per-field metadata scraping with fallback chain execution.
type Executor struct {
	service          *Service
	registry         *provider.Registry
	providerSettings *provider.SettingsService
	logger           *slog.Logger
}

// FieldResult records the outcome of scraping a single field.
type FieldResult struct {
	Field        FieldName               `json:"field"`
	Provider     provider.ProviderName   `json:"provider"`
	Contributors []provider.ProviderName `json:"-"` // providers that contributed images for this field
	WasFallback  bool                    `json:"was_fallback"`
	Queried      bool                    `json:"queried"`
	Err          error                   `json:"-"`
}

// NewExecutor creates a new scraper executor.
func NewExecutor(service *Service, registry *provider.Registry, settings *provider.SettingsService, logger *slog.Logger) *Executor {
	return &Executor{
		service:          service,
		registry:         registry,
		providerSettings: settings,
		logger:           logger.With(slog.String("component", "scraper-executor")),
	}
}

// ScrapeAll scrapes all enabled fields using the scraper configuration for the
// given scope. It returns a merged FetchResult compatible with the
// provider.Orchestrator output.
func (e *Executor) ScrapeAll(ctx context.Context, mbid, name, scope string, providerIDs map[provider.ProviderName]string) (*provider.FetchResult, error) {
	// Ensure providerIDs is writable so EnrichProviderIDs can populate it
	// with IDs extracted from earlier providers' URL results.
	if providerIDs == nil {
		providerIDs = make(map[provider.ProviderName]string)
	}

	cfg, err := e.service.GetConfig(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("loading scraper config: %w", err)
	}

	available, err := e.providerSettings.AvailableProviderNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading available providers: %w", err)
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

		fr := e.scrapeField(ctx, mbid, name, field, *chain, available, providerIDs, cache, &mu, result)
		if fr.Err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("%s: %s", fr.Field, fr.Err.Error()))
			continue
		}

		if fr.Queried {
			result.AttemptedFields = append(result.AttemptedFields, string(fr.Field))
		}

		if fr.Provider != "" {
			selectedProviders[fr.Provider] = true
			result.Sources = append(result.Sources, provider.FieldSource{
				Field:    string(fr.Field),
				Provider: fr.Provider,
			})

			// For image fields, mark all providers that actually
			// contributed images for this specific field as selected
			// so their mergeable fields (IDs, URLs, aliases) are applied.
			if CategoryFor(fr.Field) == CategoryImages {
				for _, provName := range fr.Contributors {
					selectedProviders[provName] = true
				}
			}
		}
	}

	// Apply mergeable fields only from providers that were actually selected.
	// Also populate AttemptedProviders for providers that responded without
	// error, so callers can update per-provider fetch timestamps. Errored
	// providers are excluded to avoid hiding outages behind misleading
	// "attempted" markers.
	mu.Lock()
	for provName, pr := range cache {
		if pr.err != nil {
			continue
		}
		result.AttemptedProviders = append(result.AttemptedProviders, provName)
		if !selectedProviders[provName] || pr.meta == nil {
			continue
		}
		applyMergeableFields(result, pr.meta, provName)
	}
	mu.Unlock()

	return result, nil
}

// scrapeField tries the primary provider for a field, then walks the fallback chain.
// Matched field data is written into the merged result. Queried is set to true if at
// least one provider was successfully reached (no error), even if none returned data
// for this field.
//
// For image fields, all providers in the chain are queried and their results are
// aggregated so users can choose from multiple candidates. For text fields, the
// first provider that returns data wins (priority order determines preference).
func (e *Executor) scrapeField(
	ctx context.Context,
	mbid, name string,
	field FieldConfig,
	chain FallbackChain,
	available map[provider.ProviderName]bool,
	providerIDs map[provider.ProviderName]string,
	cache map[provider.ProviderName]*providerResult,
	mu *sync.Mutex,
	result *provider.FetchResult,
) FieldResult {
	queried := false
	isImage := CategoryFor(field.Field) == CategoryImages

	// For image fields, track the first provider that contributed images
	// so we can report it in the FieldResult, and all contributing
	// providers so ScrapeAll can mark them as selected.
	var firstImageProvider provider.ProviderName
	var firstImageWasFallback bool
	var contributors []provider.ProviderName

	// Try primary provider first (if configured)
	if available[field.Primary] {
		pr := e.getProviderResult(ctx, field.Primary, mbid, name, providerIDs, cache, mu)
		if pr.err == nil {
			provider.EnrichProviderIDs(pr.meta, providerIDs)
			// For image fields, only mark as queried when GetImages was actually
			// invoked and either succeeded or returned ErrNotFound. Skip when
			// GetImages was never called (no MBID and no provider-specific ID)
			// or when it returned a transient error (timeout, 5xx). Transient
			// failures must not mark the field as attempted so that existing
			// image data is preserved rather than cleared.
			if !isImage || (pr.imagesAttempted && pr.imageErr == nil) {
				queried = true
			}
			if applyFieldValue(field.Field, pr, result) {
				if !isImage {
					return FieldResult{Field: field.Field, Provider: field.Primary, Queried: true}
				}
				contributors = append(contributors, field.Primary)
				if firstImageProvider == "" {
					firstImageProvider = field.Primary
				}
			}
		}
	}

	// Walk fallback chain
	for _, provName := range chain.Providers {
		if provName == field.Primary || !available[provName] {
			continue
		}

		pr := e.getProviderResult(ctx, provName, mbid, name, providerIDs, cache, mu)
		if pr.err != nil {
			continue
		}

		provider.EnrichProviderIDs(pr.meta, providerIDs)
		// For image fields, only mark as queried when GetImages was actually
		// invoked and either succeeded or returned ErrNotFound. Skip when
		// GetImages was never called (no MBID and no provider-specific ID)
		// or when it returned a transient error (timeout, 5xx). Transient
		// failures must not mark the field as attempted so that existing
		// image data is preserved rather than cleared.
		if !isImage || (pr.imagesAttempted && pr.imageErr == nil) {
			queried = true
		}
		if applyFieldValue(field.Field, pr, result) {
			if !isImage {
				return FieldResult{
					Field:       field.Field,
					Provider:    provName,
					WasFallback: true,
					Queried:     true,
				}
			}
			contributors = append(contributors, provName)
			if firstImageProvider == "" {
				firstImageProvider = provName
				firstImageWasFallback = true
			}
		}
	}

	// For image fields, return the first contributing provider as the source
	// and all contributors so ScrapeAll can mark them as selected.
	if isImage && firstImageProvider != "" {
		return FieldResult{
			Field:        field.Field,
			Provider:     firstImageProvider,
			Contributors: contributors,
			WasFallback:  firstImageWasFallback,
			Queried:      true,
		}
	}

	return FieldResult{Field: field.Field, Queried: queried}
}

// providerResult caches a single provider's API response.
type providerResult struct {
	meta            *provider.ArtistMetadata
	images          []provider.ImageResult
	err             error
	imageErr        error // non-nil when GetImages returned a transient error (not ErrNotFound)
	imagesAttempted bool  // true only when GetImages was actually invoked (success or ErrNotFound)
}

// getProviderResult fetches and caches results from a single provider.
func (e *Executor) getProviderResult(
	ctx context.Context,
	name provider.ProviderName,
	mbid, artistName string,
	providerIDs map[provider.ProviderName]string,
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

	// Lookup precedence: provider-specific ID > MBID > artist name.
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
			var notFound *provider.ErrNotFound
			if errors.As(err, &notFound) {
				if nlp, ok := p.(provider.NameLookupProvider); ok && nlp.SupportsNameLookup() {
					e.logger.Debug("retrying with artist name after MBID not-found",
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
			var notFound *provider.ErrNotFound
			if errors.As(err, &notFound) {
				e.logger.Debug("provider has no data for artist",
					slog.String("provider", string(name)),
					slog.String("id", queryID))
			} else {
				e.logger.Debug("provider GetArtist failed",
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
		// Mark that GetImages was actually invoked regardless of outcome.
		// This distinguishes "GetImages was called and returned ErrNotFound"
		// (imagesAttempted=true, imageErr=nil) from "GetImages was never called
		// because no ID was available" (imagesAttempted=false).
		pr.imagesAttempted = true
		if err != nil {
			var notFound *provider.ErrNotFound
			if errors.As(err, &notFound) {
				e.logger.Debug("provider has no images for artist",
					slog.String("provider", string(name)),
					slog.String("id", imgID))
				// ErrNotFound means the provider was reached and definitively said
				// "no images". Leave imageErr nil so image fields are marked as
				// attempted and stale image data can be cleared.
			} else {
				e.logger.Warn("provider GetImages failed, preserving existing image data",
					slog.String("provider", string(name)),
					slog.String("error", err.Error()))
				// Transient failure: store in imageErr so image fields are NOT
				// marked as attempted. This prevents clearing existing image data
				// when the provider was merely unreachable.
				pr.imageErr = err
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
		if meta.Biography == "" || provider.IsJunkBiography(meta.Biography) {
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
	if meta.DeezerID != "" && result.Metadata.DeezerID == "" {
		result.Metadata.DeezerID = meta.DeezerID
	}
	if meta.SpotifyID != "" && result.Metadata.SpotifyID == "" {
		result.Metadata.SpotifyID = meta.SpotifyID
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
	if meta.YearsActive != "" && result.Metadata.YearsActive == "" {
		result.Metadata.YearsActive = meta.YearsActive
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
