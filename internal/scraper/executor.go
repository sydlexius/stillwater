package scraper

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/tagdict"
)

// Executor performs per-field metadata scraping with fallback chain execution.
type Executor struct {
	service          *Service
	registry         *provider.Registry
	providerSettings *provider.SettingsService
	aimd             *provider.AIMDController // may be nil; when nil, AIMD signals are skipped
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

// NewExecutor creates a new scraper executor. aimd may be nil; when nil,
// adaptive rate-limiting signals are skipped and the executor behaves exactly
// as before. In production, pass the same AIMDController instance used by the
// Orchestrator so both code paths share per-provider rate-limit state.
func NewExecutor(service *Service, registry *provider.Registry, settings *provider.SettingsService, logger *slog.Logger, aimd *provider.AIMDController) *Executor {
	return &Executor{
		service:          service,
		registry:         registry,
		providerSettings: settings,
		aimd:             aimd,
		logger:           logger.With(slog.String("component", "scraper-executor")),
	}
}

// ScrapeAll scrapes all enabled fields using the scraper configuration for the
// given scope. It returns a merged FetchResult compatible with the
// provider.Orchestrator output.
//
// Per-field provider ordering is sourced from the UI-configured priority list
// (provider.priority.<field> settings, exposed by SettingsService.GetPriorities)
// rather than the scraper config's hardcoded primary + fallback chain. This
// keeps the refresh path consistent with what the user sees and edits in
// Settings > Providers (#1030). The scraper config still controls which fields
// are enabled and supplies a backup fallback list for any provider absent from
// the priority settings.
//
//nolint:gocognit // Top-level orchestrator: priority resolution, per-field scrape, provider-ID enrichment carry-forward between iterations, and per-field error aggregation; the carry-forward semantics across the per-field loop require sequential flow rather than parallel helpers.
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

	priorities, err := e.providerSettings.GetPriorities(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading provider priorities: %w", err)
	}
	// priorityByField records the enabled provider list for every field
	// that has a priority configured. The map's presence (not just
	// emptiness) carries meaning: a field absent from the map has no
	// configuration and falls back to the scraper-config chain, while a
	// field present with an empty slice means the user explicitly
	// disabled every provider for that field and the field should be
	// skipped entirely. Collapsing the two cases would silently
	// re-enable disabled providers via the chain fallback.
	priorityByField := make(map[FieldName][]provider.ProviderName, len(priorities))
	for _, pri := range priorities {
		priorityByField[FieldName(pri.Field)] = pri.EnabledProviders()
	}

	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			URLs: make(map[string]string),
		},
		// MetadataLocale drives locale-aware genre/style/mood handling in the
		// fieldAppliers below, matching the legacy orchestrator path.
		MetadataLocale: provider.FirstMetadataLang(ctx),
		// MetadataVocabCfg is the user's vocab filtering configuration,
		// resolved once from the context and applied after each tag-slice
		// merge. Mirrors the MetadataLocale pattern; nil means no filtering.
		MetadataVocabCfg: tagdict.MetadataVocab(ctx),
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

		// Build the effective ordered provider list for this field by combining
		// the user-configured priority list (authoritative) with any providers
		// from the scraper config's fallback chain that are not yet listed.
		// The first entry in the resulting list becomes the effective primary.
		//
		// hasPriority distinguishes "no priority configured" (fall back to
		// scraper-config chain) from "priority configured but every
		// provider disabled" (skip the field entirely so disabled
		// providers cannot leak back in via the chain).
		priority, hasPriority := priorityByField[field.Field]
		if hasPriority && len(priority) == 0 {
			continue
		}
		effField, effChain := effectiveFieldOrdering(field, *chain, priority, hasPriority)

		fr := e.scrapeField(ctx, mbid, name, effField, effChain, available, providerIDs, cache, &mu, result)
		if fr.Err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("%s: %s", fr.Field, fr.Err.Error()))
			continue
		}

		if fr.Queried {
			result.AttemptedFields = append(result.AttemptedFields, string(fr.Field))
		}

		if fr.Provider != "" {
			// scrapeField only sets fr.Provider when applyFieldValue returned
			// true, so this is the executor-side signal for "data merged".
			result.PopulatedFields = append(result.PopulatedFields, string(fr.Field))
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

	// MusicBrainz is authoritative for artist names (it owns the MBID), so
	// apply its Name/SortName unconditionally before the selectedProviders
	// gate. This ensures language-aware name promotion always takes effect
	// even when MusicBrainz doesn't win an image field.
	mu.Lock()
	if mbResult, ok := cache[provider.NameMusicBrainz]; ok && mbResult.err == nil && mbResult.meta != nil {
		if mbResult.meta.Name != "" {
			result.Metadata.Name = mbResult.meta.Name
		}
		if mbResult.meta.SortName != "" {
			result.Metadata.SortName = mbResult.meta.SortName
		}
	}

	// Populate AttemptedProviders for providers that responded without error,
	// so callers can update per-provider fetch timestamps. Errored providers
	// are excluded to avoid hiding outages behind misleading "attempted"
	// markers.
	//
	// Mergeable data falls into two buckets:
	//   1. Provider IDs, URL relations, and aliases: orthogonal to field
	//      selection. A provider that loses every field may still be the
	//      only source of the artist's Wikidata/Deezer/Spotify ID or a
	//      relation URL. These must merge for every successful provider so
	//      downstream persistence records them.
	//   2. Name/SortName and classification fields (Type, Gender,
	//      Disambiguation, YearsActive): selection-gated to respect the
	//      priority-wins semantics the scraper config defines.
	//
	// Collapsing both into one gate was the #1158 root cause: Wikidata
	// returned Q175044 but lost every field to MusicBrainz, so nothing was
	// merged from its result and the QID was dropped before persistence.
	for provName, pr := range cache {
		if pr.err != nil {
			continue
		}
		result.AttemptedProviders = append(result.AttemptedProviders, provName)
		if pr.meta == nil {
			continue
		}
		applyProviderIDsAndURLs(result, pr.meta)
		if !selectedProviders[provName] {
			continue
		}
		applyMergeableFields(result, pr.meta, provName)
	}
	// Final pass: backfill provider IDs from URL relations on the aggregated
	// result. MusicBrainz publishes URL relations (deezer, wikidata, spotify,
	// discogs, allmusic) whose last path segment is the provider's native ID;
	// we can persist those even when the corresponding provider adapter was
	// never invoked (e.g. the user has not enabled Deezer as a scraper). The
	// non-scraper orchestrator performs the same sweep in FetchMetadata; the
	// scraper path needs to match it so persistence sees the same IDs.
	provider.ExtractProviderIDsFromURLs(result.Metadata)
	// Post-merge normalization: clear gender for non-individual types regardless
	// of provider iteration order (Go map iteration is non-deterministic).
	if result.Metadata.Type != "" && !artist.IsIndividualType(result.Metadata.Type) {
		result.Metadata.Gender = ""
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
//
//nolint:gocognit // Image-field aggregation and text-field first-wins are two distinct provider-loop policies sharing setup (primary, fallback chain, cache, mu); the branching inside the loop expresses that policy split and refactoring would duplicate the chain-walk logic.
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
	isMembers := field.Field == FieldMembers

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
			// See imageFieldQueried and membersFieldQueried for the rationale.
			if (!isImage || imageFieldQueried(pr)) &&
				(!isMembers || membersFieldQueried(pr, result)) {
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
		// See imageFieldQueried and membersFieldQueried for the rationale.
		if (!isImage || imageFieldQueried(pr)) &&
			(!isMembers || membersFieldQueried(pr, result)) {
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
	imagesAttempted bool  // true whenever GetImages was actually invoked, regardless of outcome
}

// getProviderResult fetches and caches results from a single provider.
// Cache check and registry lookup happen here; the per-provider fetch logic
// is shared with the orchestrator via provider.FetchProviderResult.
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

	fetched := provider.FetchProviderResult(ctx, p, name, mbid, artistName, providerIDs, e.logger, e.aimd)
	pr := &providerResult{
		meta:            fetched.Meta(),
		images:          fetched.Images(),
		err:             fetched.Err(),
		imageErr:        fetched.ImageErr(),
		imagesAttempted: fetched.ImagesAttempted(),
	}
	mu.Lock()
	cache[name] = pr
	mu.Unlock()
	return pr
}

// imageFieldQueried reports whether an image field should be marked as queried
// for this provider result. Only mark as queried when GetImages was actually
// invoked and either succeeded or returned ErrNotFound. Skip when GetImages was
// never called (no MBID and no provider-specific ID) or when it returned a
// transient error (timeout, 5xx). Transient failures must not mark the field
// as attempted so that existing image data is preserved rather than cleared.
func imageFieldQueried(pr *providerResult) bool {
	return pr.imagesAttempted && pr.imageErr == nil
}

// membersFieldQueried reports whether the members field should be marked as
// queried for this provider result. A provider that returned no members
// without asserting completeness (sparse relation data) must not mark the
// field as attempted, so existing member rows are preserved rather than
// cleared. Marking is allowed only when the provider returned members OR
// authoritatively asserted an empty roster; in the latter case the
// authoritative signal is propagated to the merged result so downstream
// consumers can distinguish an intentional clear from missing data.
func membersFieldQueried(pr *providerResult, result *provider.FetchResult) bool {
	if pr.meta == nil {
		return false
	}
	if len(pr.meta.Members) == 0 && !pr.meta.MembersAuthoritative {
		return false
	}
	if pr.meta.MembersAuthoritative {
		result.MembersAuthoritative = true
	}
	return true
}

// fieldApplier copies one field from a provider's metadata into the merged
// FetchResult. It returns true when a non-empty value was applied.
type fieldApplier func(meta *provider.ArtistMetadata, result *provider.FetchResult) bool

// fieldAppliers maps each text/slice field to its apply function. Image fields
// are handled separately in applyFieldValue because they read from pr.images
// rather than pr.meta.
var fieldAppliers = map[FieldName]fieldApplier{
	FieldBiography: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if m.Biography == "" || provider.IsJunkBiography(m.Biography) {
			return false
		}
		r.Metadata.Biography = m.Biography
		return true
	},
	// Genre/style/mood are routed through tagdict: tags are canonicalized to a
	// consistent spelling and, when the user has a metadata-language preference
	// (r.MetadataLocale), translated to that locale. An empty locale degrades to
	// plain canonicalization. This mirrors the legacy orchestrator path so both
	// fetch paths surface tags identically.
	//
	// After the locale-aware dedup, each field additionally passes through
	// tagdict.ApplyVocabFilter with the user's tag-filter config
	// (r.MetadataVocabCfg): exclude patterns drop unwanted tags and a per-field
	// cap limits the count. When the config is nil or default (empty exclude
	// list, zero caps), this is a complete no-op.
	FieldGenres: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if len(m.Genres) == 0 {
			return false
		}
		merged := tagdict.MergeAndDeduplicateLocale(nil, m.Genres, r.MetadataLocale)
		merged = tagdict.ApplyVocabFilter(r.MetadataVocabCfg, tagdict.VocabFieldGenres, merged)
		r.Metadata.Genres = merged
		return true
	},
	FieldStyles: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if len(m.Styles) == 0 {
			return false
		}
		merged := tagdict.MergeAndDeduplicateLocale(nil, m.Styles, r.MetadataLocale)
		merged = tagdict.ApplyVocabFilter(r.MetadataVocabCfg, tagdict.VocabFieldStyles, merged)
		r.Metadata.Styles = merged
		return true
	},
	FieldMoods: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if len(m.Moods) == 0 {
			return false
		}
		merged := tagdict.MergeAndDeduplicateLocale(nil, m.Moods, r.MetadataLocale)
		merged = tagdict.ApplyVocabFilter(r.MetadataVocabCfg, tagdict.VocabFieldMoods, merged)
		r.Metadata.Moods = merged
		return true
	},
	FieldMembers: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if len(m.Members) == 0 {
			return false
		}
		r.Metadata.Members = m.Members
		return true
	},
	FieldFormed: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if m.Formed == "" {
			return false
		}
		r.Metadata.Formed = m.Formed
		return true
	},
	FieldBorn: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if m.Born == "" {
			return false
		}
		r.Metadata.Born = m.Born
		return true
	},
	FieldDied: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if m.Died == "" {
			return false
		}
		r.Metadata.Died = m.Died
		return true
	},
	FieldDisbanded: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if m.Disbanded == "" {
			return false
		}
		r.Metadata.Disbanded = m.Disbanded
		return true
	},
	FieldYearsActive: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if m.YearsActive == "" {
			return false
		}
		r.Metadata.YearsActive = m.YearsActive
		return true
	},
	FieldType: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if m.Type == "" {
			return false
		}
		r.Metadata.Type = m.Type
		return true
	},
	FieldGender: func(m *provider.ArtistMetadata, r *provider.FetchResult) bool {
		if m.Gender == "" {
			return false
		}
		r.Metadata.Gender = m.Gender
		return true
	},
}

// applyFieldValue checks whether a provider result has data for a given field
// and writes the value into the merged FetchResult. Returns true if the field
// has a non-empty value that was applied.
func applyFieldValue(field FieldName, pr *providerResult, result *provider.FetchResult) bool {
	if pr.meta == nil && len(pr.images) == 0 {
		return false
	}

	// Image fields read from pr.images, not pr.meta.
	if CategoryFor(field) == CategoryImages {
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

	meta := pr.meta
	if meta == nil {
		meta = &provider.ArtistMetadata{}
	}

	if apply, ok := fieldAppliers[field]; ok {
		return apply(meta, result)
	}
	return false
}

// applyProviderIDsAndURLs merges provider identifiers, URL relations, and
// aliases from a provider result into the merged FetchResult. These three
// buckets are orthogonal to per-field selection and must merge for every
// provider that succeeded (regardless of whether the provider "won" any
// field), so that downstream persistence sees every ID the query chain
// discovered. See #1158.
func applyProviderIDsAndURLs(result *provider.FetchResult, meta *provider.ArtistMetadata) {
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
	for k, v := range meta.URLs {
		if _, exists := result.Metadata.URLs[k]; !exists {
			result.Metadata.URLs[k] = v
		}
	}
	for _, alias := range meta.Aliases {
		if !containsString(result.Metadata.Aliases, alias) {
			result.Metadata.Aliases = append(result.Metadata.Aliases, alias)
		}
	}
}

// applyMergeableFields copies classification fields (Name, SortName, Type,
// Gender, Disambiguation, YearsActive) from a selected provider's result
// into the merged FetchResult. Callers must gate this on the scraper-config
// selection, because these fields follow priority-wins semantics; IDs and
// URLs belong in applyProviderIDsAndURLs, which is called unconditionally.
func applyMergeableFields(result *provider.FetchResult, meta *provider.ArtistMetadata, source provider.ProviderName) {
	// MusicBrainz is authoritative for artist names (it owns the MBID), so
	// its Name/SortName always win. This is especially important when
	// language-aware name promotion selects a localized alias. Other
	// providers only fill in the Name if MusicBrainz hasn't set one yet.
	if meta.Name != "" {
		if source == provider.NameMusicBrainz || result.Metadata.Name == "" {
			result.Metadata.Name = meta.Name
		}
	}
	if meta.SortName != "" {
		if source == provider.NameMusicBrainz || result.Metadata.SortName == "" {
			result.Metadata.SortName = meta.SortName
		}
	}
	if meta.Type != "" && result.Metadata.Type == "" {
		result.Metadata.Type = meta.Type
	}
	// Only merge gender when the accumulated type is individual or still unknown.
	// Non-individual types (group, orchestra, choir) should not carry gender.
	if meta.Gender != "" && result.Metadata.Gender == "" {
		if result.Metadata.Type == "" || artist.IsIndividualType(result.Metadata.Type) {
			result.Metadata.Gender = meta.Gender
		}
	}
	if meta.Disambiguation != "" && result.Metadata.Disambiguation == "" {
		result.Metadata.Disambiguation = meta.Disambiguation
	}
	if meta.YearsActive != "" && result.Metadata.YearsActive == "" {
		result.Metadata.YearsActive = meta.YearsActive
	}
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

// effectiveFieldOrdering merges the UI-configured priority list for a field
// with the scraper config's primary + fallback chain to produce the final
// ordered provider list to query. The priority list is authoritative for
// ordering: its first enabled provider becomes the effective primary, and any
// providers from the scraper config (primary + fallback chain) that are not
// already covered are appended so newly registered providers are not silently
// dropped.
//
// When the field has no priority configured at all (hasPriority=false), the
// original scraper-config primary and chain are returned unchanged so
// behavior matches the pre-#1030 path. The "configured but empty" case
// (hasPriority=true, len(priority)==0) must be handled by the caller -- it
// means "all providers disabled" and the field should be skipped, never
// passed to this function.
func effectiveFieldOrdering(field FieldConfig, chain FallbackChain, priority []provider.ProviderName, hasPriority bool) (FieldConfig, FallbackChain) {
	if !hasPriority {
		return field, chain
	}

	// Build the effective ordered list: priority entries first, then any
	// scraper-config entries (primary + chain) that are not already present.
	seen := make(map[provider.ProviderName]bool, len(priority)+len(chain.Providers)+1)
	ordered := make([]provider.ProviderName, 0, len(priority)+len(chain.Providers)+1)
	for _, p := range priority {
		if seen[p] {
			continue
		}
		seen[p] = true
		ordered = append(ordered, p)
	}
	if field.Primary != "" && !seen[field.Primary] {
		seen[field.Primary] = true
		ordered = append(ordered, field.Primary)
	}
	for _, p := range chain.Providers {
		if seen[p] {
			continue
		}
		seen[p] = true
		ordered = append(ordered, p)
	}

	// Guard: if every input list was empty, ordered is empty and indexed access
	// below would panic. Return the originals unchanged so downstream callers
	// simply find no providers and skip normally.
	if len(ordered) == 0 {
		return field, chain
	}

	effField := field
	effField.Primary = ordered[0]

	effChain := FallbackChain{
		Category:  chain.Category,
		Providers: ordered,
	}
	return effField, effChain
}
