package scraper

import (
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

// FieldName identifies a scrapeable metadata or image field.
type FieldName string

// Known field names.
const (
	FieldBiography FieldName = "biography"
	FieldGenres    FieldName = "genres"
	FieldStyles    FieldName = "styles"
	FieldMoods     FieldName = "moods"
	FieldMembers   FieldName = "members"
	FieldFormed    FieldName = "formed"
	FieldBorn      FieldName = "born"
	FieldDied      FieldName = "died"
	FieldDisbanded FieldName = "disbanded"
	FieldThumb     FieldName = "thumb"
	FieldFanart    FieldName = "fanart"
	FieldLogo      FieldName = "logo"
	FieldBanner    FieldName = "banner"
)

// AllFieldNames returns all known field names in display order.
func AllFieldNames() []FieldName {
	return []FieldName{
		FieldBiography, FieldGenres, FieldStyles, FieldMoods,
		FieldMembers, FieldFormed, FieldBorn, FieldDied, FieldDisbanded,
		FieldThumb, FieldFanart, FieldLogo, FieldBanner,
	}
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
			{Field: FieldStyles, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryMetadata},
			{Field: FieldMoods, Primary: provider.NameAudioDB, Enabled: true, Category: CategoryMetadata},
			{Field: FieldMembers, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldFormed, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldBorn, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldDied, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
			{Field: FieldDisbanded, Primary: provider.NameMusicBrainz, Enabled: true, Category: CategoryMetadata},
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
					provider.NameLastFM,
					provider.NameDiscogs,
					provider.NameAudioDB,
					provider.NameWikidata,
				},
			},
			{
				Category: CategoryImages,
				Providers: []provider.ProviderName{
					provider.NameFanartTV,
					provider.NameAudioDB,
					provider.NameDiscogs,
				},
			},
		},
	}
}

// ProviderCapabilities returns the static capability map for all known providers.
func ProviderCapabilities() []ProviderCapability {
	return []ProviderCapability{
		{
			Provider:     provider.NameMusicBrainz,
			DisplayName:  provider.NameMusicBrainz.DisplayName(),
			RequiresAuth: false,
			MetadataFields: []FieldName{
				FieldBiography, FieldGenres, FieldMembers,
				FieldFormed, FieldBorn, FieldDied, FieldDisbanded,
			},
		},
		{
			Provider:     provider.NameFanartTV,
			DisplayName:  provider.NameFanartTV.DisplayName(),
			RequiresAuth: true,
			ImageFields:  []FieldName{FieldThumb, FieldFanart, FieldLogo, FieldBanner},
		},
		{
			Provider:     provider.NameAudioDB,
			DisplayName:  provider.NameAudioDB.DisplayName(),
			RequiresAuth: true,
			MetadataFields: []FieldName{
				FieldBiography, FieldGenres, FieldStyles, FieldMoods, FieldFormed,
			},
			ImageFields: []FieldName{FieldThumb, FieldFanart, FieldLogo, FieldBanner},
		},
		{
			Provider:       provider.NameDiscogs,
			DisplayName:    provider.NameDiscogs.DisplayName(),
			RequiresAuth:   true,
			MetadataFields: []FieldName{FieldBiography, FieldGenres, FieldStyles},
		},
		{
			Provider:       provider.NameLastFM,
			DisplayName:    provider.NameLastFM.DisplayName(),
			RequiresAuth:   true,
			MetadataFields: []FieldName{FieldBiography, FieldGenres},
		},
		{
			Provider:     provider.NameWikidata,
			DisplayName:  provider.NameWikidata.DisplayName(),
			RequiresAuth: false,
			MetadataFields: []FieldName{
				FieldMembers, FieldFormed, FieldBorn, FieldDied, FieldDisbanded,
			},
		},
	}
}
