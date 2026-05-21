// Package tagdict -- vocab.go provides user-configurable tag filtering for
// genre/style/mood metadata. It extends the normalization pipeline with a
// pattern-based exclude list and per-field count caps, modeled on the genre
// filtering options in MusicBrainz Picard.
//
// Design summary:
//   - VocabConfig holds the exclude patterns (shared across all three tag
//     fields) and the per-field maximum-count caps.
//   - ApplyVocabFilter is the single shared function called from BOTH the
//     orchestrator (orchestrator.go) and the scraper-executor (executor.go).
//     It takes an already-resolved *VocabConfig so it stays a pure function.
//   - WithMetadataVocab / MetadataVocab thread the config through
//     context.Context from the request handler to the fetch entry points,
//     mirroring the romanization-fallback and language-preference patterns in
//     language.go. Each fetch path resolves it once into its FetchResult.
//
// Filter behavior (applied after MergeAndDeduplicateLocale has already run):
//  1. Drop any tag matching one of the exclude patterns. Matching is
//     case-insensitive; "*" is a wildcard matching any run of characters.
//     Regex is not supported.
//  2. Truncate the surviving tags to the field's maximum count (when set).
//
// When the exclude list is empty AND the field's cap is 0 (unlimited), the
// output is byte-identical to the input. This keeps the default no-op
// guarantee for existing users.
package tagdict

import (
	"context"
	"encoding/json"
	"strings"
)

// Field name constants for the three tag fields this package filters. These
// match the field-name strings used in orchestrator.go and executor.go.
const (
	VocabFieldGenres = "genres"
	VocabFieldStyles = "styles"
	VocabFieldMoods  = "moods"
)

// VocabConfig is the user-configurable tag-filtering configuration stored as
// the `metadata_vocab` settings key.
type VocabConfig struct {
	// Exclude is a list of patterns. Any genre, style, or mood tag that
	// matches any pattern is dropped at tag-merge time. Matching is
	// case-insensitive; "*" is a wildcard matching any run of characters
	// (e.g. "christian*" matches "christian rock"). Regex is not supported.
	// The same list applies to all three tag fields.
	Exclude []string `json:"exclude"`

	// MaxGenres caps how many genres are written. 0 means unlimited. When the
	// cap is exceeded the earliest tags in merge order are kept: the
	// orchestrator merges providers in priority order, and the scraper-executor
	// takes the first provider that supplies the field, so in both paths the
	// survivors come from the highest-priority source.
	MaxGenres int `json:"max_genres"`

	// MaxStyles caps how many styles are written. 0 means unlimited.
	MaxStyles int `json:"max_styles"`

	// MaxMoods caps how many moods are written. 0 means unlimited.
	MaxMoods int `json:"max_moods"`
}

// ctxKeyMetadataVocab is the unexported context key type for VocabConfig.
// Using a struct type prevents key collisions with other packages.
type ctxKeyMetadataVocab struct{}

// WithMetadataVocab returns a child context carrying the given VocabConfig.
// Callers that load the config from the database inject it here so that the
// fetch paths can resolve it without needing a direct dependency on the DB.
// This mirrors the WithNameRomanizationFallback pattern in language.go.
func WithMetadataVocab(ctx context.Context, cfg *VocabConfig) context.Context {
	return context.WithValue(ctx, ctxKeyMetadataVocab{}, cfg)
}

// MetadataVocab retrieves the VocabConfig from the context. Returns nil when
// no config has been injected; ApplyVocabFilter treats nil as a no-op config.
func MetadataVocab(ctx context.Context) *VocabConfig {
	cfg, _ := ctx.Value(ctxKeyMetadataVocab{}).(*VocabConfig)
	return cfg
}

// maxForField returns the configured maximum count for the given field name,
// or 0 (unlimited) for an unrecognized field.
func (c *VocabConfig) maxForField(field string) int {
	switch field {
	case VocabFieldGenres:
		return c.MaxGenres
	case VocabFieldStyles:
		return c.MaxStyles
	case VocabFieldMoods:
		return c.MaxMoods
	default:
		return 0
	}
}

// wildcardMatch reports whether s matches pattern, where "*" in pattern is a
// wildcard matching any run of characters (including the empty string). Both
// arguments must already be lower-cased by the caller. A pattern with no "*"
// is an exact-equality check.
func wildcardMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		// No wildcard: exact match.
		return pattern == s
	}
	// The first segment is a required prefix.
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	rest := s[len(parts[0]):]
	// Interior segments must appear in order anywhere in the remainder.
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, mid)
		if i < 0 {
			return false
		}
		rest = rest[i+len(mid):]
	}
	// The final segment is a required suffix of whatever remains.
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

// ApplyVocabFilter applies the user's tag-filtering configuration to a list of
// already-normalized tags for the given field. A nil cfg returns the input
// slice unchanged.
//
// This function is called from BOTH the orchestrator (applyTagSliceField in
// orchestrator.go) and the scraper-executor (fieldAppliers in executor.go)
// after their existing MergeAndDeduplicateLocale call. It runs AFTER
// deduplication so it always receives normalized, deduplicated input. Each
// caller resolves the VocabConfig once from the request context (into the
// FetchResult at construction time) and passes that resolved value here, so
// this function stays a pure transformation with no context dependency.
//
// Steps:
//  1. Drop tags matching any exclude pattern (case-insensitive, "*" wildcard).
//  2. Truncate to the field's maximum count when one is configured.
//
// When the exclude list is empty AND the field cap is 0, this is a no-op.
func ApplyVocabFilter(cfg *VocabConfig, field string, tags []string) []string {
	if cfg == nil {
		return tags
	}
	maxCount := cfg.maxForField(field)

	// Fast path: nothing configured for this field.
	if len(cfg.Exclude) == 0 && maxCount <= 0 {
		return tags
	}

	// Lower-case and trim the exclude patterns once per call. Blank patterns
	// are skipped so a stray empty entry can never match (and drop) every tag.
	patterns := make([]string, 0, len(cfg.Exclude))
	for _, p := range cfg.Exclude {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			patterns = append(patterns, p)
		}
	}

	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" {
			continue
		}
		if matchesAnyPattern(patterns, strings.ToLower(trimmed)) {
			continue
		}
		result = append(result, tag)
		// Stop once the field cap is reached. The earliest tags in merge
		// order survive; those come from the highest-priority provider.
		if maxCount > 0 && len(result) >= maxCount {
			break
		}
	}
	return result
}

// matchesAnyPattern reports whether the lower-cased tag matches any of the
// (already lower-cased) patterns.
func matchesAnyPattern(patterns []string, lowerTag string) bool {
	for _, p := range patterns {
		if wildcardMatch(p, lowerTag) {
			return true
		}
	}
	return false
}

// ParseVocabConfig decodes a JSON string into a VocabConfig. It is the inverse
// of encoding/json.Marshal on a VocabConfig. An error is returned for
// malformed JSON; unknown JSON keys are ignored by encoding/json. Callers
// should use DefaultVocabConfig() when the input is empty or ParseVocabConfig
// returns an error.
//
// The returned config always has a non-nil Exclude slice, so a stored blob
// that omits the key (or a pre-existing blob from an older schema) still
// serializes back to [] rather than null when surfaced through the API.
func ParseVocabConfig(raw string) (*VocabConfig, error) {
	var cfg VocabConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, err
	}
	if cfg.Exclude == nil {
		cfg.Exclude = []string{}
	}
	return &cfg, nil
}

// DefaultVocabConfig returns the default VocabConfig: an empty (but non-nil,
// so the JSON API surfaces it as [] rather than null) exclude list and zero
// caps for all three fields. It is a complete no-op and is used when
// `metadata_vocab` has never been set.
func DefaultVocabConfig() *VocabConfig {
	return &VocabConfig{Exclude: []string{}}
}
