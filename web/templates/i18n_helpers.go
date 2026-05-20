package templates

import (
	"context"
	"fmt"
	"strings"

	"github.com/sydlexius/stillwater/internal/i18n"
)

// t returns the translated string for the given key from the request context.
// In templ components, ctx is available implicitly, so usage is simply:
//
//	{ t(ctx, "nav.dashboard") }
func t(ctx context.Context, key string) string {
	return i18n.TFromCtx(ctx).T(key)
}

// tn returns a pluralized translation. It looks up "key.one" when count is 1
// and "key.other" for all other counts, substituting {count} in the result.
//
//	{ tn(ctx, "artists.count", len(artists)) }
func tn(ctx context.Context, key string, count int) string {
	return i18n.TFromCtx(ctx).Tn(key, count)
}

// tf returns a translated string with fmt.Sprintf-style interpolation. The
// locale string contains Go format verbs (e.g. "%.0f%% compliant") and args
// are substituted after translation lookup.
//
//	{ tf(ctx, "dashboard.health_compliant", score) }
func tf(ctx context.Context, key string, args ...any) string {
	tmpl := i18n.TFromCtx(ctx).T(key)
	if tmpl == key {
		return key
	}
	return fmt.Sprintf(tmpl, args...)
}

// tInstrument localizes a MusicBrainz instrument attribute string using the
// "instrument.<attr>" key namespace. The stored attribute value (e.g. "bass",
// "vocals") is the canonical MB English string; the Translator maps it to the
// display locale's form via the instrument.* keys in the locale files.
//
// The attribute is lowercased and trimmed before the lookup: MusicBrainz
// relationship attributes vary in casing (e.g. "Hammond organ") and the
// instrument.* catalog keys are all lowercase, so an exact-case lookup would
// silently miss otherwise. The raw (un-normalized) attr is still what gets
// returned on a total miss, so casing is preserved for display.
//
// When no translation exists for the given locale, the Translator falls back
// to the English value from en.json (the key's value), and if the key is also
// absent from en.json, the raw attribute string is returned so no information
// is lost.
func tInstrument(ctx context.Context, attr string) string {
	key := "instrument." + strings.ToLower(strings.TrimSpace(attr))
	translated := i18n.TFromCtx(ctx).T(key)
	// T returns the key itself when absent from all locales. In that case fall
	// back to the raw attribute string so the UI shows "bass" not "instrument.bass".
	if translated == key {
		return attr
	}
	return translated
}
