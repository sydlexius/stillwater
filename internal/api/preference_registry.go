package api

import (
	"sort"
	"strconv"
)

// PreferenceDef is the exported representation of a single user preference
// entry. It is the element type returned by PreferenceRegistry and is used by
// cmd/gen-prefs-reference (doc generator) and the JS-sync test
// (TestJSDefaultsMatchGoRegistry).
//
// Enum-type entries have AllowedValues set and RangeMin/RangeMax/RangeStep
// zero. Range-type entries (bg_opacity, page_size) have RangeMin/RangeMax
// non-zero and AllowedValues nil.
type PreferenceDef struct {
	// Key is the preference key string (e.g. "theme", "bg_opacity").
	Key string
	// Default is the canonical default value as a string (e.g. "dark" or "85").
	Default string
	// AllowedValues lists the valid string values for enum-type preferences.
	// Nil for range-type preferences.
	AllowedValues []string
	// RangeMin and RangeMax define the inclusive integer bounds for range-type
	// preferences. Both are zero for enum-type preferences.
	RangeMin int
	RangeMax int
	// RangeStep is the UI slider step for range-type preferences (informational;
	// the API itself accepts any integer in [RangeMin, RangeMax]).
	RangeStep int
}

// PreferenceRegistry returns a snapshot of all documented user preference
// definitions sorted alphabetically by key. It is the single source of truth
// consulted by doc generators and cross-validation tests.
//
// Enum-type preferences come from the internal preferenceDefaults map.
// Range-type preferences (bg_opacity, page_size) are not in preferenceDefaults
// -- they are validated as integers via dedicated normalizer functions rather
// than a fixed enum set -- and are added here from the package-level range
// constants so the registry presents a unified view to consumers.
//
// Intentionally omitted (complex keys that cannot be represented as a flat
// key/default/allowed-values table):
//   - metadata_languages: JSON-array of BCP 47 tags, structural validation
//   - artist_detail_section_order, artist_detail_hidden_sections: free-form JSON arrays
//   - suppress_confirm_*: dynamically created per-action keys
func PreferenceRegistry() []PreferenceDef {
	result := make([]PreferenceDef, 0, len(preferenceDefaults)+2)
	for key, def := range preferenceDefaults {
		pd := PreferenceDef{
			Key:     key,
			Default: def.defaultValue,
		}
		if len(def.allowedValues) > 0 {
			pd.AllowedValues = make([]string, len(def.allowedValues))
			copy(pd.AllowedValues, def.allowedValues)
		}
		result = append(result, pd)
	}
	// Range-type preferences are validated as integers rather than fixed enum
	// strings and are therefore excluded from preferenceDefaults. They are added
	// here explicitly so the registry is the single place a consumer looks.
	result = append(result,
		PreferenceDef{
			Key:       PrefBgOpacity,
			Default:   strconv.Itoa(BgOpacityDefault),
			RangeMin:  BgOpacityMin,
			RangeMax:  BgOpacityMax,
			RangeStep: 5, // slider increments by 5% in the preferences UI
		},
		PreferenceDef{
			Key:       PrefPageSize,
			Default:   strconv.Itoa(PageSizeDefault),
			RangeMin:  PageSizeMin,
			RangeMax:  PageSizeMax,
			RangeStep: 1,
		},
	)
	sort.Slice(result, func(i, j int) bool {
		return result[i].Key < result[j].Key
	})
	return result
}
