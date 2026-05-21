package provider

// VerbosityIntro is the conservative default for Wikipedia biographies:
// only the introductory section is fetched (exintro=true in the API call).
const VerbosityIntro = "intro"

// VerbosityFull fetches the complete Wikipedia article text (no exintro param).
const VerbosityFull = "full"

// FieldVerbosityOption describes one verbosity level for a provider field.
// Value is the stable identifier stored in the database; LabelKey is the
// i18n key used to render the human-readable label in the UI.
type FieldVerbosityOption struct {
	Value    string `json:"value"`
	LabelKey string `json:"label_key"`
}

// FieldVerbosity describes the verbosity control for a single (provider, field)
// pair. LabelKey is the i18n key for the field label (e.g. "Biography").
// Options lists the available levels in display order; the first option is the
// default when no value has been persisted.
type FieldVerbosity struct {
	Field    string                 `json:"field"`
	LabelKey string                 `json:"label_key"`
	Options  []FieldVerbosityOption `json:"options"`
}

// DefaultVerbosity returns the default verbosity value for a field's option
// list. Returns an empty string if opts is empty.
func DefaultVerbosity(opts []FieldVerbosityOption) string {
	if len(opts) == 0 {
		return ""
	}
	return opts[0].Value
}

// IsValidVerbosity returns true if value is one of the listed options.
func IsValidVerbosity(opts []FieldVerbosityOption, value string) bool {
	for _, o := range opts {
		if o.Value == value {
			return true
		}
	}
	return false
}

// VerbosityOptionsForField returns the option list for a single field within a
// provider's verbosity catalogue, or nil if the provider has no such field.
func VerbosityOptionsForField(name ProviderName, field string) []FieldVerbosityOption {
	for _, fv := range FieldVerbosityOptions(name) {
		if fv.Field == field {
			return fv.Options
		}
	}
	return nil
}

// FieldVerbosityOptions returns the verbosity controls available for the named
// provider. Returns nil (no verbosity subsection) for providers that do not
// support field-level verbosity in v1.
//
// v1 ships Wikipedia biography only. Future (provider, field) pairs are added
// here as additional cases or additional entries in the Wikipedia case.
func FieldVerbosityOptions(name ProviderName) []FieldVerbosity {
	switch name {
	case NameWikipedia:
		return []FieldVerbosity{
			{
				Field:    "biography",
				LabelKey: "settings.provider_config.verbosity_biography",
				Options: []FieldVerbosityOption{
					{Value: VerbosityIntro, LabelKey: "settings.provider_config.verbosity_intro"},
					{Value: VerbosityFull, LabelKey: "settings.provider_config.verbosity_full"},
				},
			},
		}
	default:
		// All other providers have no field verbosity controls in v1.
		return nil
	}
}
