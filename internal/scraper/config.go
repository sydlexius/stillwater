package scraper

import (
	"fmt"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

// FieldName identifies a scrapeable metadata or image field.
type FieldName string

// Known field names.
const (
	FieldBiography   FieldName = "biography"
	FieldGenres      FieldName = "genres"
	FieldStyles      FieldName = "styles"
	FieldMoods       FieldName = "moods"
	FieldMembers     FieldName = "members"
	FieldFormed      FieldName = "formed"
	FieldBorn        FieldName = "born"
	FieldDied        FieldName = "died"
	FieldDisbanded   FieldName = "disbanded"
	FieldYearsActive FieldName = "years_active"
	FieldOrigin      FieldName = "origin"
	FieldType        FieldName = "type"
	FieldGender      FieldName = "gender"
	FieldThumb       FieldName = "thumb"
	FieldFanart      FieldName = "fanart"
	FieldLogo        FieldName = "logo"
	FieldBanner      FieldName = "banner"
)

// AllFieldNames returns all known field names in display order.
func AllFieldNames() []FieldName {
	return []FieldName{
		FieldBiography, FieldGenres, FieldStyles, FieldMoods,
		FieldMembers, FieldFormed, FieldBorn, FieldDied, FieldDisbanded,
		FieldYearsActive, FieldOrigin, FieldType, FieldGender,
		FieldThumb, FieldFanart, FieldLogo, FieldBanner,
	}
}

// IsValidFieldName returns true if the given name is a known field.
func IsValidFieldName(name FieldName) bool {
	for _, f := range AllFieldNames() {
		if f == name {
			return true
		}
	}
	return false
}

// ValidateConfig checks that all provider and field names in the config are valid.
func ValidateConfig(cfg *ScraperConfig) error {
	validProviders := make(map[provider.ProviderName]bool)
	for _, name := range provider.AllProviderNames() {
		validProviders[name] = true
	}

	for _, f := range cfg.Fields {
		if !IsValidFieldName(f.Field) {
			return fmt.Errorf("unknown field name: %q", f.Field)
		}
		if f.Primary != "" && !validProviders[f.Primary] {
			return fmt.Errorf("unknown provider name: %q", f.Primary)
		}
	}

	for _, chain := range cfg.FallbackChains {
		for _, p := range chain.Providers {
			if !validProviders[p] {
				return fmt.Errorf("unknown provider name in fallback chain: %q", p)
			}
		}
	}

	return nil
}

// FieldCategory groups fields into categories for fallback chains.
type FieldCategory string

// Known field categories.
const (
	CategoryMetadata FieldCategory = "metadata"
	CategoryImages   FieldCategory = "images"
)

// CategoryFor returns the category a field belongs to.
func CategoryFor(f FieldName) FieldCategory {
	switch f {
	case FieldThumb, FieldFanart, FieldLogo, FieldBanner:
		return CategoryImages
	default:
		return CategoryMetadata
	}
}

// ScopeGlobal is the scope identifier for the global scraper configuration.
const ScopeGlobal = "global"

// FieldConfig describes the primary provider assignment for a single field.
type FieldConfig struct {
	Field    FieldName             `json:"field"`
	Primary  provider.ProviderName `json:"primary"`
	Enabled  bool                  `json:"enabled"`
	Category FieldCategory         `json:"category"`
}

// FallbackChain defines the ordered list of fallback providers for a category.
type FallbackChain struct {
	Category  FieldCategory           `json:"category"`
	Providers []provider.ProviderName `json:"providers"`
}

// ScraperConfig holds the complete scraper configuration for a scope.
type ScraperConfig struct {
	ID             string          `json:"id"`
	Scope          string          `json:"scope"`
	Fields         []FieldConfig   `json:"fields"`
	FallbackChains []FallbackChain `json:"fallback_chains"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`

	// KnownFields records every field this install has been offered, which is
	// not the same as the fields it currently uses. The startup backfill adds a
	// field only when it is absent from BOTH Fields and KnownFields, so a field
	// the operator deliberately removed stays removed while a field introduced
	// by a later release is still added once. Without it, "deleted" and "never
	// existed" are indistinguishable and every boot resurrects the deletion.
	//
	// Empty on a config written before this existed; backfillMissingFields
	// seeds it from the current Fields in that case, so a pre-existing install
	// does not have its current selection treated as unseen.
	//
	// That seeding carries one unavoidable boundary case: a field deleted
	// BEFORE the roster existed left no record anywhere, so on the seeding boot
	// it cannot be told apart from a field introduced later and is re-added
	// once. Deletions are durable only from that boot onward. Deleting it again
	// sticks.
	KnownFields []FieldName `json:"known_fields,omitempty"`
}

// Overrides tracks which fields and fallback chains have been explicitly
// overridden from the parent scope. Only meaningful for non-global scopes.
type Overrides struct {
	Fields         map[FieldName]bool     `json:"fields,omitempty"`
	FallbackChains map[FieldCategory]bool `json:"fallback_chains,omitempty"`
}

// ProviderCapability describes which fields a provider can supply.
type ProviderCapability struct {
	Provider       provider.ProviderName `json:"provider"`
	DisplayName    string                `json:"display_name"`
	RequiresAuth   bool                  `json:"requires_auth"`
	HasKey         bool                  `json:"has_key"`
	MetadataFields []FieldName           `json:"metadata_fields"`
	ImageFields    []FieldName           `json:"image_fields"`
}

// PrimaryFor returns the configured primary provider for a given field,
// or an empty provider name if the field is not found.
func (c *ScraperConfig) PrimaryFor(field FieldName) provider.ProviderName {
	for _, f := range c.Fields {
		if f.Field == field {
			return f.Primary
		}
	}
	return ""
}

// FallbackChainFor returns the fallback chain for a given category,
// or nil if not found.
func (c *ScraperConfig) FallbackChainFor(cat FieldCategory) *FallbackChain {
	for i := range c.FallbackChains {
		if c.FallbackChains[i].Category == cat {
			return &c.FallbackChains[i]
		}
	}
	return nil
}

// DefaultConfig returns the default global scraper configuration with
// sensible per-field provider assignments and fallback chains.
func DefaultConfig() *ScraperConfig {
	return &ScraperConfig{
		Scope: ScopeGlobal,
		Fields: []FieldConfig{
			{Field: FieldBiography, Primary: provider.NameLastFM, Enabled: true, Category: CategoryMetadata},
			{Field: FieldGenres, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldStyles, Primary: provider.NameDiscogs, Enabled: true, Category: CategoryMetadata},
			{Field: FieldMoods, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryMetadata},
			{Field: FieldMembers, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldFormed, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldBorn, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldDied, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldDisbanded, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldYearsActive, Primary: provider.NameWikipedia, Enabled: true, Category: CategoryMetadata},
			{Field: FieldOrigin, Primary: provider.NameWikipedia, Enabled: true, Category: CategoryMetadata},
			{Field: FieldType, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldGender, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldThumb, Primary: provider.NameFanartTV, Enabled: true, Category: CategoryImages},
			{Field: FieldFanart, Primary: provider.NameFanartTV, Enabled: true, Category: CategoryImages},
			{Field: FieldLogo, Primary: provider.NameFanartTV, Enabled: true, Category: CategoryImages},
			{Field: FieldBanner, Primary: provider.NameFanartTV, Enabled: true, Category: CategoryImages},
		},
		FallbackChains: []FallbackChain{
			{
				Category: CategoryMetadata,
				Providers: []provider.ProviderName{
					provider.NameMusicBrainz,
					provider.NameWikipedia,
					provider.NameLastFM,
					provider.NameDiscogs,
					provider.NameAudioDB,
					provider.NameWikidata,
					provider.NameGenius,
				},
			},
			{
				Category: CategoryImages,
				Providers: []provider.ProviderName{
					provider.NameFanartTV,
					provider.NameAudioDB,
				},
			},
		},
	}
}

// ProviderCapabilities returns the static capability map for all known
// providers, in the display order of provider.AllProviderNames().
//
// This table is a scraper-vocabulary VIEW of provider.ProviderCapabilities(),
// which is the single source of truth for what each adapter can supply (and is
// itself checked against the adapter source by
// TestProviderCapabilitiesMatchAdapterSource). Deriving rather than restating
// it is what #2897 fixed: the two tables were hand-maintained independently and
// had drifted in both directions -- providers advertised for fields their
// adapter never sets, and providers silently never offered for fields they do
// set.
//
// Fields with no scraper counterpart (name, sort_name, disambiguation, aliases,
// similar_artists) and image types the scraper does not model (hdlogo,
// widethumb, background) are dropped by the vocabulary maps below.
func ProviderCapabilities() []ProviderCapability {
	declared := provider.ProviderCapabilities()
	caps := make([]ProviderCapability, 0, len(declared))

	for _, name := range provider.AllProviderNames() {
		dec, ok := declared[name]
		if !ok {
			// AllProviderNames and ProviderCapabilities are both static and
			// kept in agreement by TestProviderCapabilitiesMatchAdapterSource;
			// skipping rather than panicking keeps a config-listing request
			// serving the providers that ARE declared.
			continue
		}

		capability := ProviderCapability{
			Provider:     name,
			DisplayName:  name.DisplayName(),
			RequiresAuth: providerRequiresAuth(name),
		}
		for _, field := range dec.SupportedFields {
			if scraperField, ok := scraperFieldFor(field); ok {
				capability.MetadataFields = append(capability.MetadataFields, scraperField)
			}
		}
		for _, img := range dec.SupportedImages {
			if scraperField, ok := scraperImageFieldFor(img); ok {
				capability.ImageFields = append(capability.ImageFields, scraperField)
			}
		}
		caps = append(caps, capability)
	}
	return caps
}

// scraperFieldFor maps a provider capability field name (an ArtistMetadata
// JSON tag) to its scraper FieldName. It reports false for provider-side
// fields the scraper does not model as routable: name, sort_name,
// disambiguation, aliases and similar_artists, none of which have a
// FieldConfig row or a fieldAppliers entry.
func scraperFieldFor(field string) (FieldName, bool) {
	f := FieldName(field)
	switch f {
	case FieldBiography, FieldGenres, FieldStyles, FieldMoods, FieldMembers,
		FieldFormed, FieldBorn, FieldDied, FieldDisbanded, FieldYearsActive,
		FieldOrigin, FieldType, FieldGender:
		return f, true
	default:
		return "", false
	}
}

// scraperImageFieldFor maps a provider ImageType to its scraper FieldName. It
// reports false for hdlogo, widethumb and background, which the scraper does
// not model as routable image fields.
func scraperImageFieldFor(img provider.ImageType) (FieldName, bool) {
	switch img {
	case provider.ImageThumb:
		return FieldThumb, true
	case provider.ImageFanart:
		return FieldFanart, true
	case provider.ImageLogo:
		return FieldLogo, true
	case provider.ImageBanner:
		return FieldBanner, true
	default:
		return "", false
	}
}

// providerRequiresAuth reports whether a provider needs an API key before it
// can be used. It mirrors the RequiresAuth() reported by each adapter; the
// scraper cannot call that without constructing every adapter, and this table
// is a static declaration.
func providerRequiresAuth(name provider.ProviderName) bool {
	switch name {
	case provider.NameMusicBrainz, provider.NameWikipedia, provider.NameWikidata,
		provider.NameDeezer, provider.NameAudioDB:
		return false
	default:
		return true
	}
}
