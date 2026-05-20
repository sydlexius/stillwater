package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/sydlexius/stillwater/internal/provider/tagdict"
)

// sensitiveParamRe matches URL query parameters whose names indicate sensitive
// values (API keys, tokens, passwords). The value runs until the next ampersand,
// whitespace, quote, or end of string.
var sensitiveParamRe = regexp.MustCompile(`(?i)(api_?key|token|secret|password|authorization)=([^&\s"']+)`)

// wikidataQIDRe matches a well-formed Wikidata Q-item identifier. Callers that
// accept a last-path-segment as a QID must validate against this to avoid
// propagating malformed values like "Qabc" or "Qspecial:Random" through
// providerIDs, which would otherwise drive a failed direct-entity SPARQL and
// mask the MBID fallback.
var wikidataQIDRe = regexp.MustCompile(`^Q\d+$`)

// ScrubError removes sensitive query parameter values (API keys, tokens, passwords)
// from error strings before they are written to logs. Provider errors may contain
// full request URLs with credentials in query parameters (e.g. Fanart.tv includes
// api_key in every request URL).
func ScrubError(err error) string {
	return scrubSensitiveParams(err.Error())
}

// scrubSensitiveParams redacts values of sensitive query parameters in s.
func scrubSensitiveParams(s string) string {
	return sensitiveParamRe.ReplaceAllString(s, "${1}=REDACTED")
}

// FieldSource records which provider supplied a given field.
type FieldSource struct {
	Field    string       `json:"field"`
	Provider ProviderName `json:"provider"`
}

// fieldProviderExclusions lists providers that structurally cannot provide data
// for specific fields. MusicBrainz and Wikidata, for example, do not return
// biography text so the field is always empty when sourced from either, even
// if a user explicitly includes them in the biography priority list.
var fieldProviderExclusions = map[string]map[ProviderName]bool{
	"biography": {NameMusicBrainz: true, NameWikidata: true},
}

// IsExcludedForField returns true if a provider is structurally unable to
// provide data for the given field and should be skipped.
func IsExcludedForField(field string, prov ProviderName) bool {
	if ex, ok := fieldProviderExclusions[field]; ok {
		return ex[prov]
	}
	return false
}

// FetchResult holds the merged result of querying multiple providers.
type FetchResult struct {
	Metadata           *ArtistMetadata `json:"metadata"`
	Images             []ImageResult   `json:"images"`
	Sources            []FieldSource   `json:"sources"`
	Errors             []string        `json:"errors"`
	AttemptedProviders []ProviderName  `json:"attempted_providers,omitempty"`
	// AttemptedFields lists fields the orchestrator queried a provider for,
	// regardless of whether any provider returned data. Useful for telemetry
	// and "we tried these" UI signals.
	AttemptedFields []string `json:"attempted_fields,omitempty"`
	// PopulatedFields lists fields where at least one provider actually
	// returned data that was merged into Metadata. Subset of AttemptedFields.
	// Used by the refresh merge path to distinguish "attempted but empty"
	// (preserve existing value) from "attempted and populated" (overwrite).
	// This is the mechanism for #952's graceful-fallback contract: an empty
	// localized lookup must not clobber pre-existing data.
	PopulatedFields []string `json:"populated_fields,omitempty"`
	// MembersAuthoritative is true when the contributing provider can assert
	// that its member list is complete. It distinguishes "provider
	// authoritatively returned zero members" (an empty roster that should
	// clear existing rows) from "provider returned zero due to sparse relation
	// data" (an empty roster that should preserve existing rows).
	// Currently set by MusicBrainz for confirmed individual artist types
	// (Person, Character): an individual by definition has no band members,
	// so an empty member list is definitionally complete and may safely clear
	// stale rows. Group/Orchestra/Choir types never set it because MusicBrainz
	// relation data for real bands can be sparse and an empty roster would
	// represent missing data rather than an authoritative empty result.
	MembersAuthoritative bool `json:"members_authoritative,omitempty"`
	// MetadataLocale is the BCP 47 primary-language subtag of the user's first
	// preferred metadata language at the time of the fetch. It is set from the
	// context's MetadataLanguages value and drives locale-aware tag deduplication
	// in applyTagSliceField. An empty string means English-only dedup.
	MetadataLocale string `json:"-"`
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
//
//nolint:gocognit // Per-field provider iteration in priority order with provider-ID enrichment carry-forward between fields; this is the legacy non-scraper path retained for callers that have no scraper config, and its semantics must match ScrapeAll's outcome on a parallel diagram.
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
		MetadataLocale: FirstMetadataLang(ctx),
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
		// fieldPopulated tracks whether THIS priority iteration produced any
		// applied data, so a field that was populated in a previous iteration
		// (in case the priority list ever lists the same field twice) is not
		// falsely re-credited here. Reset each iteration.
		fieldPopulated := false
		isImageField := isImageFieldName(pri.Field)
		isMembersField := pri.Field == "members"
		for _, provName := range pri.EnabledProviders() {
			if !available[provName] {
				continue
			}
			if IsExcludedForField(pri.Field, provName) {
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

			// For image fields, only mark as queried when GetImages was actually
			// invoked and either succeeded or returned ErrNotFound. Skip when
			// GetImages was never called (no MBID and no provider-specific ID)
			// or when it returned a transient error (timeout, 5xx). Transient
			// failures must not mark the field as attempted so that existing
			// image data is preserved rather than cleared.
			if isImageField && (!pr.imagesAttempted || pr.imageErr != nil) {
				continue
			}

			// For the members field, only mark as queried when the provider
			// actually returned members OR authoritatively asserted an empty
			// roster. A provider that returned zero members without asserting
			// completeness (sparse relation data) must not mark the field as
			// attempted, so existing member rows are preserved rather than
			// cleared. This mirrors the image-field guard above.
			//
			// A nil meta result (transient error, timeout, 5xx) is treated
			// identically to the ErrNotFound path in the scraper executor:
			// the field is NOT marked as queried so existing member rows are
			// preserved. This mirrors membersFieldQueried in executor.go, which
			// also returns false for nil meta.
			if isMembersField {
				if pr.meta == nil {
					continue
				}
				if len(pr.meta.Members) == 0 && !pr.meta.MembersAuthoritative {
					continue
				}
				if pr.meta.MembersAuthoritative {
					result.MembersAuthoritative = true
				}
			}

			queried = true
			if applyField(result, pri.Field, pr, provName) {
				fieldPopulated = true
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
			if fieldPopulated {
				result.PopulatedFields = append(result.PopulatedFields, pri.Field)
			}
		}
	}

	// Final backfill pass for the merged metadata (catches any IDs not yet
	// populated from earlier per-provider enrichment).
	ExtractProviderIDsFromURLs(result.Metadata)

	// MusicBrainz is authoritative for artist Name and SortName: it owns the
	// MBID and applies language-aware alias promotion inline on its meta.Name.
	// The first-provider-wins merge in applyField would otherwise let an
	// earlier-iterated provider (e.g. wikipedia during the biography field)
	// lock in the canonical form, blocking MB's promoted value. Overwrite here
	// so the language preference is honored. Mirrors the pattern in
	// internal/scraper/executor.go's MB-authoritative override.
	if mbResult, ok := cache[NameMusicBrainz]; ok && mbResult.err == nil && mbResult.meta != nil {
		if mbResult.meta.Name != "" {
			result.Metadata.Name = mbResult.meta.Name
		}
		if mbResult.meta.SortName != "" {
			result.Metadata.SortName = mbResult.meta.SortName
		}
	}

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
// every image candidate they return. Providers are queried in the order
// determined by imageProvidersInPriorityOrder, which derives ordering from
// the configured image field priorities (thumb, fanart, logo, banner).
// All providers are always queried so that callers (image search UI, ImageFixer
// quality sorting) receive the full set of candidates to choose from.
// providerIDs supplies provider-specific IDs for providers that do not accept MBIDs
// (e.g. Deezer uses its own numeric ID). Providers without an entry in providerIDs
// receive the MBID. Providers with an empty entry are skipped.
func (o *Orchestrator) FetchImages(ctx context.Context, mbid string, providerIDs map[ProviderName]string) (*FetchResult, error) {
	result := &FetchResult{
		Metadata: &ArtistMetadata{},
	}

	providers, err := o.imageProvidersInPriorityOrder(ctx)
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
				slog.String("error", ScrubError(err)))
			result.Errors = append(result.Errors, fmt.Sprintf("%s: image fetch failed", p.Name()))
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
				slog.String("error", ScrubError(err)))
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

// imageProvidersInPriorityOrder returns available providers ordered by the
// configured image field priorities. Image fields are walked in their default
// order (thumb, fanart, logo, banner) and the first field that lists a
// provider determines that provider's position. This means thumb priorities
// take precedence over fanart priorities and so on (first-field-wins).
// The ordering matches FetchMetadata, which iterates the same priority list.
func (o *Orchestrator) imageProvidersInPriorityOrder(ctx context.Context) ([]Provider, error) {
	priorities, err := o.settings.GetPriorities(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading priorities: %w", err)
	}

	available, err := o.settings.AvailableProviderNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading available providers: %w", err)
	}

	// Collect providers from image field priorities in order, deduplicating.
	// The first image field that mentions a provider determines its position.
	seen := make(map[ProviderName]bool)
	var ordered []Provider
	for _, pri := range priorities {
		if !isImageFieldName(pri.Field) {
			continue
		}
		for _, provName := range pri.EnabledProviders() {
			if seen[provName] || !available[provName] {
				continue
			}
			seen[provName] = true
			if p := o.registry.Get(provName); p != nil {
				ordered = append(ordered, p)
			}
		}
	}

	// Append any remaining available providers not listed in image priorities,
	// so newly registered providers are not silently skipped.
	for _, p := range o.registry.All() {
		if !seen[p.Name()] && available[p.Name()] {
			ordered = append(ordered, p)
		}
	}

	return ordered, nil
}

type providerResult struct {
	meta            *ArtistMetadata
	images          []ImageResult
	err             error
	imageErr        error // non-nil when GetImages returned a transient error (not ErrNotFound)
	imagesAttempted bool  // true whenever GetImages was actually invoked, regardless of outcome
}

//nolint:gocognit // Per-provider cached fetch with lookup-precedence ladder (provider ID > MBID > name), name-based retry only when MBID-not-found AND the provider implements NameLookupProvider, plus ErrNotFound-vs-transient distinction for both GetArtist and GetImages so stale data is cleared on a definitive miss but preserved on a transient failure. Near-identical to scraper.Executor.getProviderResult (cog 34 each); the consolidation is a peripheral concern but worth tracking; refactor tracked in #1554.
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
		// Mark that GetImages was actually invoked regardless of outcome.
		// This distinguishes "GetImages was called and returned ErrNotFound"
		// (imagesAttempted=true, imageErr=nil) from "GetImages was never called
		// because no ID was available" (imagesAttempted=false).
		pr.imagesAttempted = true
		if err != nil {
			var notFound *ErrNotFound
			if errors.As(err, &notFound) {
				o.logger.Debug("provider has no images for artist",
					slog.String("provider", string(name)),
					slog.String("id", imgID))
				// ErrNotFound means the provider was reached and definitively said
				// "no images". Leave imageErr nil so image fields are marked as
				// attempted and stale image data can be cleared.
			} else {
				o.logger.Warn("provider GetImages failed, preserving existing image data",
					slog.String("provider", string(name)),
					slog.String("error", ScrubError(err)))
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

// fieldApplier mutates result with one provider's contribution to the named
// field and reports the outcome:
//
//   - populated: true if this call actually changed result for the target field
//     (e.g. wrote a new value or grew a tag slice).
//   - matched: true if the field-specific code path was entered. The pre-refactor
//     applyField returned from inside its switch in this case, so the
//     downstream provider-ID/URL/alias merge MUST be skipped to preserve
//     behavior. When false, fall through to the merge tail and return false.
//
// Each helper below mirrors one arm of the original switch. `matched` follows
// the original early-return semantics exactly: any case arm that took the
// `return ...` statement counts as matched.
type fieldApplier func(result *FetchResult, field string, pr *providerResult, source ProviderName) (populated, matched bool)

// scalarFieldAccessor describes how to read/write one of the simple scalar
// "set-if-empty" fields handled by applyField. The pattern is identical for
// every entry in scalarFieldAccessors: if the source meta has a non-empty
// value AND the merged result is still empty, copy it across and record the
// provider in the source list.
type scalarFieldAccessor struct {
	get func(*ArtistMetadata) string
	set func(*ArtistMetadata, string)
}

// scalarFieldAccessors maps field names to their getter/setter pair. Keep
// this table in sync with the corresponding fields on ArtistMetadata. Fields
// with custom semantics (biography junk filter, type/gender interaction,
// slice merges, image aggregation) are handled separately.
var scalarFieldAccessors = map[string]scalarFieldAccessor{
	"formed": {
		get: func(m *ArtistMetadata) string { return m.Formed },
		set: func(m *ArtistMetadata, v string) { m.Formed = v },
	},
	"born": {
		get: func(m *ArtistMetadata) string { return m.Born },
		set: func(m *ArtistMetadata, v string) { m.Born = v },
	},
	"died": {
		get: func(m *ArtistMetadata) string { return m.Died },
		set: func(m *ArtistMetadata, v string) { m.Died = v },
	},
	"disbanded": {
		get: func(m *ArtistMetadata) string { return m.Disbanded },
		set: func(m *ArtistMetadata, v string) { m.Disbanded = v },
	},
	"years_active": {
		get: func(m *ArtistMetadata) string { return m.YearsActive },
		set: func(m *ArtistMetadata, v string) { m.YearsActive = v },
	},
	"origin": {
		get: func(m *ArtistMetadata) string { return m.Origin },
		set: func(m *ArtistMetadata, v string) { m.Origin = v },
	},
}

// applyScalarField copies a simple scalar string field from meta into result
// when meta has data and result is still empty. matched is true only when the
// populate condition held, mirroring the original switch arms which only took
// the early-return when both sides were ready.
func applyScalarField(result *FetchResult, field string, pr *providerResult, source ProviderName) (populated, matched bool) {
	acc, ok := scalarFieldAccessors[field]
	if !ok {
		return false, false
	}
	meta := pr.meta
	if acc.get(meta) == "" || acc.get(result.Metadata) != "" {
		return false, false
	}
	acc.set(result.Metadata, acc.get(meta))
	result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
	return true, true
}

// applyBiography copies the biography from meta when the merged result is
// still empty, skipping known junk strings via IsJunkBiography. matched is
// true only when the populate condition held (matching the original switch
// arm's early-return semantics).
func applyBiography(result *FetchResult, _ string, pr *providerResult, source ProviderName) (populated, matched bool) {
	meta := pr.meta
	if meta.Biography == "" || result.Metadata.Biography != "" || IsJunkBiography(meta.Biography) {
		return false, false
	}
	result.Metadata.Biography = meta.Biography
	result.Sources = append(result.Sources, FieldSource{Field: "biography", Provider: source})
	return true, true
}

// tagSliceFieldAccessor is the slice analog of scalarFieldAccessor for the
// genres / styles / moods fields, which merge-and-deduplicate rather than
// first-write-wins.
type tagSliceFieldAccessor struct {
	get func(*ArtistMetadata) []string
	set func(*ArtistMetadata, []string)
}

var tagSliceFieldAccessors = map[string]tagSliceFieldAccessor{
	"genres": {
		get: func(m *ArtistMetadata) []string { return m.Genres },
		set: func(m *ArtistMetadata, v []string) { m.Genres = v },
	},
	"styles": {
		get: func(m *ArtistMetadata) []string { return m.Styles },
		set: func(m *ArtistMetadata, v []string) { m.Styles = v },
	},
	"moods": {
		get: func(m *ArtistMetadata) []string { return m.Moods },
		set: func(m *ArtistMetadata, v []string) { m.Moods = v },
	},
}

// applyTagSliceField merges meta's tag slice into result via tagdict
// deduplication. populated is true when the merge actually grew the result
// slice; matched is true whenever meta supplied any candidates (matching the
// original switch arm, which entered its inner `if len(meta.X) > 0` and then
// always returned). The field source is recorded only on first growth so the
// highest-priority contributor stays first.
//
// When result.MetadataLocale is set, locale-aware deduplication is used so
// that tags for the same concept in different languages (e.g. "Rock" from
// MusicBrainz and "ロック" from Wikidata) collapse to a single preferred form.
func applyTagSliceField(result *FetchResult, field string, pr *providerResult, source ProviderName) (populated, matched bool) {
	acc, ok := tagSliceFieldAccessors[field]
	if !ok {
		return false, false
	}
	meta := pr.meta
	src := acc.get(meta)
	if len(src) == 0 {
		return false, false
	}
	current := acc.get(result.Metadata)
	before := len(current)
	merged := tagdict.MergeAndDeduplicateLocale(current, src, result.MetadataLocale)
	acc.set(result.Metadata, merged)
	grew := len(merged) > before
	if grew && !hasFieldSource(result.Sources, field) {
		result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
	}
	return grew, true
}

// applyMembers copies the members list from meta when result has none. The
// members field is first-write-wins (no merge across providers) because each
// provider returns a complete band roster.
func applyMembers(result *FetchResult, _ string, pr *providerResult, source ProviderName) (populated, matched bool) {
	meta := pr.meta
	if len(meta.Members) == 0 || len(result.Metadata.Members) != 0 {
		return false, false
	}
	result.Metadata.Members = meta.Members
	result.Sources = append(result.Sources, FieldSource{Field: "members", Provider: source})
	return true, true
}

// applyType copies the artist type from meta when result has none. Setting a
// non-individual type (group, orchestra, choir) also clears any previously
// applied gender value and its provenance, mirroring the scraper-executor
// normalization path.
func applyType(result *FetchResult, _ string, pr *providerResult, source ProviderName) (populated, matched bool) {
	meta := pr.meta
	if meta.Type == "" || result.Metadata.Type != "" {
		return false, false
	}
	result.Metadata.Type = meta.Type
	result.Sources = append(result.Sources, FieldSource{Field: "type", Provider: source})
	if !isIndividualTypeValue(meta.Type) {
		result.Metadata.Gender = ""
		result.Sources = removeFieldSource(result.Sources, "gender")
	}
	return true, true
}

// applyGender copies gender from meta when result has none AND the accumulated
// type either is empty (unknown) or refers to an individual.
// Group/orchestra/choir types do not carry gender.
func applyGender(result *FetchResult, _ string, pr *providerResult, source ProviderName) (populated, matched bool) {
	meta := pr.meta
	if meta.Gender == "" || result.Metadata.Gender != "" {
		return false, false
	}
	if result.Metadata.Type != "" && !isIndividualTypeValue(result.Metadata.Type) {
		return false, false
	}
	result.Metadata.Gender = meta.Gender
	result.Sources = append(result.Sources, FieldSource{Field: "gender", Provider: source})
	return true, true
}

// applyImageField appends all images of the matching type from the provider
// result. Image fields aggregate across providers (unlike scalar fields), but
// the field source is recorded only for the first contributor so
// MetadataSources reflects the highest-priority provider. matched mirrors the
// original switch arm: only true when at least one matching image was found.
func applyImageField(result *FetchResult, field string, pr *providerResult, source ProviderName) (populated, matched bool) {
	imgType := fieldToImageType(field)
	found := false
	for _, img := range pr.images {
		if img.Type == imgType {
			result.Images = append(result.Images, img)
			found = true
		}
	}
	if !found {
		return false, false
	}
	if !hasFieldSource(result.Sources, field) {
		result.Sources = append(result.Sources, FieldSource{Field: field, Provider: source})
	}
	return true, true
}

// fieldAppliers dispatches a field name to its specific applier. Field names
// without an entry fall through to applyScalarField / applyTagSliceField via
// dispatchFieldApplier.
var fieldAppliers = map[string]fieldApplier{
	"biography": applyBiography,
	"members":   applyMembers,
	"type":      applyType,
	"gender":    applyGender,
	"thumb":     applyImageField,
	"fanart":    applyImageField,
	"logo":      applyImageField,
	"banner":    applyImageField,
}

// dispatchFieldApplier routes the field to its specific applier or to the
// scalar / tag-slice generic helpers. Unknown fields return matched=false so
// the caller falls through to the provider-ID merge tail.
func dispatchFieldApplier(result *FetchResult, field string, pr *providerResult, source ProviderName) (populated, matched bool) {
	if fn, ok := fieldAppliers[field]; ok {
		return fn(result, field, pr, source)
	}
	if _, ok := scalarFieldAccessors[field]; ok {
		return applyScalarField(result, field, pr, source)
	}
	if _, ok := tagSliceFieldAccessors[field]; ok {
		return applyTagSliceField(result, field, pr, source)
	}
	return false, false
}

// providerIDAccessor names one of the cross-provider identifier fields whose
// "first non-empty wins" merge runs as part of mergeProviderIDsAndExtras.
type providerIDAccessor struct {
	get func(*ArtistMetadata) string
	set func(*ArtistMetadata, string)
}

// providerIDAccessors enumerates all scalar identifier-style fields merged
// when applyField falls through to the post-switch tail. The Name field is
// included here because it shares the same first-write-wins shape; it is not
// a provider ID, but applying it via the same loop keeps cyclomatic
// complexity flat.
var providerIDAccessors = []providerIDAccessor{
	{get: func(m *ArtistMetadata) string { return m.MusicBrainzID }, set: func(m *ArtistMetadata, v string) { m.MusicBrainzID = v }},
	{get: func(m *ArtistMetadata) string { return m.AudioDBID }, set: func(m *ArtistMetadata, v string) { m.AudioDBID = v }},
	{get: func(m *ArtistMetadata) string { return m.DiscogsID }, set: func(m *ArtistMetadata, v string) { m.DiscogsID = v }},
	{get: func(m *ArtistMetadata) string { return m.WikidataID }, set: func(m *ArtistMetadata, v string) { m.WikidataID = v }},
	{get: func(m *ArtistMetadata) string { return m.DeezerID }, set: func(m *ArtistMetadata, v string) { m.DeezerID = v }},
	{get: func(m *ArtistMetadata) string { return m.AllMusicID }, set: func(m *ArtistMetadata, v string) { m.AllMusicID = v }},
	{get: func(m *ArtistMetadata) string { return m.SpotifyID }, set: func(m *ArtistMetadata, v string) { m.SpotifyID = v }},
	{get: func(m *ArtistMetadata) string { return m.Name }, set: func(m *ArtistMetadata, v string) { m.Name = v }},
}

// mergeFirstWinsScalars copies each scalar identifier from meta into result
// when the source has data and the destination is still empty.
func mergeFirstWinsScalars(result *FetchResult, meta *ArtistMetadata) {
	for _, acc := range providerIDAccessors {
		if acc.get(meta) != "" && acc.get(result.Metadata) == "" {
			acc.set(result.Metadata, acc.get(meta))
		}
	}
}

// mergeURLs copies meta's URL map into result, leaving any pre-existing key
// in result untouched (first-writer-per-key wins).
func mergeURLs(result *FetchResult, meta *ArtistMetadata) {
	for k, v := range meta.URLs {
		if _, exists := result.Metadata.URLs[k]; !exists {
			result.Metadata.URLs[k] = v
		}
	}
}

// mergeAliases appends each meta alias to result.Metadata.Aliases unless it
// already exists (linear-scan deduplication).
func mergeAliases(result *FetchResult, meta *ArtistMetadata) {
	for _, alias := range meta.Aliases {
		if !containsString(result.Metadata.Aliases, alias) {
			result.Metadata.Aliases = append(result.Metadata.Aliases, alias)
		}
	}
}

// mergeProviderIDsAndExtras merges provider-specific IDs, the canonical name,
// URLs, and aliases from meta into result. The pre-refactor applyField ran
// these merges only when its switch fell through to the tail (i.e. the
// requested field's case was unmatched or its inner conditions failed).
// First-write-wins for scalars; deduplicated for URLs and aliases.
func mergeProviderIDsAndExtras(result *FetchResult, meta *ArtistMetadata) {
	mergeFirstWinsScalars(result, meta)
	mergeURLs(result, meta)
	mergeAliases(result, meta)
}

// applyField applies data from a provider result to the merged result for a
// specific field. Returns true if the requested field was populated by this
// call. When the field-specific path is not taken (unknown field, or its
// inner conditions failed), this function also merges provider IDs, the
// canonical name, URLs, and aliases from meta into result. This preserves
// the pre-refactor early-return / fall-through behavior exactly: a
// successful field-specific apply does NOT trigger the ID/URL/alias merge.
func applyField(result *FetchResult, field string, pr *providerResult, source ProviderName) bool {
	if pr.meta == nil {
		return false
	}

	populated, matched := dispatchFieldApplier(result, field, pr, source)
	if matched {
		return populated
	}
	mergeProviderIDsAndExtras(result, pr.meta)
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
	// Synthesized is true when Value was derived from other fields rather than
	// returned directly by the provider. It applies to years_active, which the
	// per-field fetch synthesizes from formed/disbanded or born/died when a
	// provider computes the value instead of storing it (MusicBrainz) or its
	// source lacks the literal key (Wikipedia infoboxes). The candidate is
	// still attributed to the originating provider; this flag only records
	// that the value is derived. The per-field providers UI renders
	// synthesized and direct candidates identically.
	Synthesized bool `json:"synthesized,omitempty"`
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
				if available[p] && !IsExcludedForField(field, p) {
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
			fpr.Error = "metadata fetch failed"
		} else if isImageFieldName(field) && pr.imageErr != nil {
			// Transient GetImages failure (timeout, 5xx): log the scrubbed error
			// server-side and return a generic message to the client. Provider
			// errors may contain API keys (e.g. Fanart.tv URLs) or raw HTTP
			// internals that must not be exposed in JSON responses or logs.
			o.logger.Warn("provider image fetch failed for comparison",
				slog.String("provider", string(provName)),
				slog.String("field", field),
				slog.String("error", ScrubError(pr.imageErr)))
			fpr.Error = "image fetch failed"
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
			break
		}
		// The provider returned no literal years_active. Some providers
		// compute the value rather than store it (MusicBrainz) and some
		// sources lack the key entirely (Wikipedia infoboxes without a
		// "years_active" field), so synthesize a candidate from the same
		// provider's formed/disbanded or born/died dates.
		if synth, ok := SynthesizeYearsActive(meta); ok {
			fpr.Value = synth
			fpr.HasData = true
			fpr.Synthesized = true
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
	case "origin":
		if meta.Origin != "" {
			fpr.Value = meta.Origin
			fpr.HasData = true
		}
	}
}

// SynthesizeYearsActive derives a years_active string from an artist's
// formed/disbanded (groups) or born/died (individuals) dates. It returns the
// synthesized value and true when synthesis succeeded, or "" and false when
// the metadata does not support a confident answer.
//
// Group/orchestra/choir types synthesize "YYYY-YYYY" when both formed and
// disbanded are known, or "YYYY-present" when only the formed date is known,
// matching the Wikipedia infobox convention for still-active groups.
//
// Individuals are handled conservatively: a "born-died" range is synthesized
// only when both dates are present. When only the birth date is known the
// person's activity cannot be determined from metadata alone, so synthesis is
// skipped rather than guessing a "YYYY-present" range.
//
// Callers must check meta.YearsActive themselves before calling: this helper
// always synthesizes from the date fields and never reads YearsActive.
func SynthesizeYearsActive(meta *ArtistMetadata) (string, bool) {
	if meta == nil {
		return "", false
	}
	if isGroupTypeValue(meta.Type) {
		formed := yearFromDate(meta.Formed)
		if formed == "" {
			return "", false
		}
		if disbanded := yearFromDate(meta.Disbanded); disbanded != "" {
			return formed + "-" + disbanded, true
		}
		return formed + "-present", true
	}
	// Individuals: only a fully bounded born-died range can be synthesized
	// with confidence. A lone birth date says nothing about whether the
	// artist is still active, so skip rather than guess.
	born := yearFromDate(meta.Born)
	died := yearFromDate(meta.Died)
	if born != "" && died != "" {
		return born + "-" + died, true
	}
	return "", false
}

// yearFromDate extracts the leading 4-digit year from a date string that may
// be "YYYY", "YYYY-MM", or "YYYY-MM-DD". Returns "" for empty or short input,
// and also returns "" when the leading 4 characters are not all ASCII digits
// (e.g. "late 1980s" or "circa 1990" would otherwise produce garbage output).
func yearFromDate(date string) string {
	if len(date) < 4 {
		return ""
	}
	prefix := date[:4]
	for i := range len(prefix) {
		if prefix[i] < '0' || prefix[i] > '9' {
			return ""
		}
	}
	return prefix
}

// isGroupTypeValue reports whether the normalized artist type string
// represents an ensemble (group, orchestra, choir) rather than an individual.
// The vocabulary mirrors the normalized values produced by provider adapters
// (see musicbrainz.mapArtistType): "group", "orchestra", "choir".
//
// This is deliberately NOT the negation of isIndividualTypeValue (below): an
// unknown or empty type is neither group nor individual. years_active synthesis
// routes such artists through the individual (born/died) branch via
// !isGroupTypeValue, so the two predicates are kept separate on purpose.
func isGroupTypeValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "group", "orchestra", "choir":
		return true
	default:
		return false
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
				slog.String("error", ScrubError(err)))
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

// removeFieldSource returns a copy of the slice with any FieldSource entry for
// the given field removed. Used to clear stale provenance when a dependent
// field (e.g. gender) is invalidated by a change to another field (e.g. type).
func removeFieldSource(sources []FieldSource, field string) []FieldSource {
	out := sources[:0]
	for _, s := range sources {
		if s.Field == field {
			continue
		}
		out = append(out, s)
	}
	return out
}

// isIndividualTypeValue reports whether the given artist type string
// represents a single individual (and therefore can carry a gender value).
// The vocabulary mirrors internal/artist.IsIndividualType: "solo", "person",
// and "character" are individual types; group/orchestra/choir are not. The
// check is trimmed and case-insensitive so a stray casing variant from a
// future provider does not silently clear gender.
//
// The orchestrator cannot import internal/artist directly: that would create
// an import cycle via internal/artist/disambiguation.go importing back into
// internal/provider. Keep this list in sync with internal/artist/merge.go's
// IsIndividualType. The two-test surface (TestApplyFieldGenderPreservedFor
// IndividualTypes here, TestIsIndividualType in internal/artist) catches
// drift in either direction.
func isIndividualTypeValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "solo", "person", "character":
		return true
	default:
		return false
	}
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
		WikidataID: meta.WikidataID,
		AllMusicID: meta.AllMusicID,
		SpotifyID:  meta.SpotifyID,
	}
	ExtractProviderIDsFromURLs(scratch)

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
	// Wikidata URLs from MusicBrainz carry the Q-item ID directly
	// (e.g. "https://www.wikidata.org/wiki/Q175044"). Propagating this
	// into providerIDs lets the orchestrator call Wikidata by QID instead
	// of MBID, which is important when the Wikidata entity does not have
	// its P434 (MusicBrainz artist ID) property populated. The P434-based
	// SPARQL lookup returns no bindings in that case, so a pre-resolved
	// QID is the only reliable path.
	if scratch.WikidataID != "" {
		if current := providerIDs[NameWikidata]; current == "" {
			providerIDs[NameWikidata] = scratch.WikidataID
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

// providerURLEntry pairs a URL key with the parser for that provider's URLs and
// a pointer-returning accessor so the dispatch loop can check and set the ID
// without a separate switch.
type providerURLEntry struct {
	key   string
	parse func(string) (string, bool)
	getID func(*ArtistMetadata) string
	setID func(*ArtistMetadata, string)
}

// providerURLParsers is the dispatch table used by ExtractProviderIDsFromURLs.
// Each entry maps a MusicBrainz URL-relation key to its parser and the
// corresponding ID field on ArtistMetadata.
var providerURLParsers = []providerURLEntry{
	{
		key:   "discogs",
		parse: parseDiscogsURL,
		getID: func(m *ArtistMetadata) string { return m.DiscogsID },
		setID: func(m *ArtistMetadata, id string) { m.DiscogsID = id },
	},
	{
		key:   "wikidata",
		parse: parseWikidataURL,
		getID: func(m *ArtistMetadata) string { return m.WikidataID },
		setID: func(m *ArtistMetadata, id string) { m.WikidataID = id },
	},
	{
		key:   "deezer",
		parse: parseDeezerURL,
		getID: func(m *ArtistMetadata) string { return m.DeezerID },
		setID: func(m *ArtistMetadata, id string) { m.DeezerID = id },
	},
	{
		key:   "allmusic",
		parse: parseAllMusicURL,
		getID: func(m *ArtistMetadata) string { return m.AllMusicID },
		setID: func(m *ArtistMetadata, id string) { m.AllMusicID = id },
	},
	{
		key:   "spotify",
		parse: parseSpotifyURL,
		getID: func(m *ArtistMetadata) string { return m.SpotifyID },
		setID: func(m *ArtistMetadata, id string) { m.SpotifyID = id },
	},
}

// ExtractProviderIDsFromURLs backfills provider IDs from URL relations returned
// by MusicBrainz when the IDs are not yet set.
//
// MusicBrainz URL relations look like:
//
//	discogs:  "https://www.discogs.com/artist/24941"       -> "24941"
//	discogs:  "https://www.discogs.com/artist/24941-a-ha"  -> "24941"
//	wikidata: "https://www.wikidata.org/wiki/Q44190"       -> "Q44190"
//	deezer:   "https://www.deezer.com/artist/3106"         -> "3106"
//	allmusic: "https://www.allmusic.com/artist/mn0000505828" -> "mn0000505828"
func ExtractProviderIDsFromURLs(meta *ArtistMetadata) {
	if meta == nil {
		return
	}
	for _, entry := range providerURLParsers {
		if entry.getID(meta) != "" {
			continue
		}
		u, ok := meta.URLs[entry.key]
		if !ok || u == "" {
			continue
		}
		if id, ok := entry.parse(u); ok {
			entry.setID(meta, id)
		}
	}
}

// parseDiscogsURL extracts the numeric artist ID from a Discogs artist URL.
// The last path segment may be "24941" or "24941-artist-name"; only the
// leading numeric portion is returned.
func parseDiscogsURL(rawURL string) (string, bool) {
	idx := strings.LastIndex(rawURL, "/")
	if idx < 0 {
		return "", false
	}
	segment := rawURL[idx+1:]
	end := strings.IndexFunc(segment, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(segment)
	}
	if end == 0 {
		return "", false
	}
	return segment[:end], true
}

// parseWikidataURL extracts the Q-item ID from a Wikidata entity URL.
// Query strings and fragments are stripped before the last path segment is
// validated against wikidataQIDRe.
func parseWikidataURL(rawURL string) (string, bool) {
	if qIdx := strings.IndexAny(rawURL, "?#"); qIdx >= 0 {
		rawURL = rawURL[:qIdx]
	}
	idx := strings.LastIndex(rawURL, "/")
	if idx < 0 {
		return "", false
	}
	candidate := rawURL[idx+1:]
	if !wikidataQIDRe.MatchString(candidate) {
		return "", false
	}
	return candidate, true
}

// parseDeezerURL extracts the numeric artist ID from a Deezer artist URL.
// The last path segment is expected to be a pure numeric string.
func parseDeezerURL(rawURL string) (string, bool) {
	idx := strings.LastIndex(rawURL, "/")
	if idx < 0 {
		return "", false
	}
	segment := rawURL[idx+1:]
	end := strings.IndexFunc(segment, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(segment)
	}
	if end == 0 {
		return "", false
	}
	return segment[:end], true
}

// parseAllMusicURL extracts the AllMusic artist ID from an AllMusic artist URL.
// The ID is "mn" followed by 10 digits and may appear as the entire last path
// segment or as a suffix after a slug (e.g. "dolly-parton-mn0000205560").
func parseAllMusicURL(rawURL string) (string, bool) {
	idx := strings.LastIndex(rawURL, "/")
	if idx < 0 {
		return "", false
	}
	segment := rawURL[idx+1:]
	if qIdx := strings.IndexAny(segment, "?#"); qIdx >= 0 {
		segment = segment[:qIdx]
	}
	mnIdx := strings.LastIndex(segment, "mn")
	if mnIdx < 0 {
		return "", false
	}
	candidate := segment[mnIdx:]
	if !isAllMusicID(candidate) {
		return "", false
	}
	return candidate, true
}

// parseSpotifyURL extracts the Spotify artist ID from an open.spotify.com URL.
// The ID is the 22-character base62 path component after "/artist/".
func parseSpotifyURL(rawURL string) (string, bool) {
	const prefix = "/artist/"
	idx := strings.LastIndex(rawURL, prefix)
	if idx < 0 {
		return "", false
	}
	candidate := rawURL[idx+len(prefix):]
	candidate = strings.TrimRight(candidate, "/")
	if qIdx := strings.IndexAny(candidate, "?#"); qIdx >= 0 {
		candidate = candidate[:qIdx]
	}
	if !isSpotifyID(candidate) {
		return "", false
	}
	return candidate, true
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
