package provider

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// FieldSource records which provider supplied a given field.
type FieldSource struct {
	Field    string       `json:"field"`
	Provider ProviderName `json:"provider"`
}

// FetchResult holds the merged result of querying multiple providers.
type FetchResult struct {
	Metadata *ArtistMetadata `json:"metadata"`
	Images   []ImageResult   `json:"images"`
	Sources  []FieldSource   `json:"sources"`
	Errors   []string        `json:"errors"`
}

// Orchestrator queries providers in priority order and merges results.
type Orchestrator struct {
	registry *Registry
	settings *SettingsService
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

// FetchMetadata queries all providers in priority order and merges the results.
// It uses the artist's MBID when available, falling back to name-based search.
func (o *Orchestrator) FetchMetadata(ctx context.Context, mbid string, name string) (*FetchResult, error) {
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
		for _, provName := range pri.Providers {
			pr := o.getProviderResult(ctx, provName, mbid, name, cache, &mu)
			if pr.err != nil {
				continue
			}

			if applyField(result, pri.Field, pr, provName) {
				break
			}
		}
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
