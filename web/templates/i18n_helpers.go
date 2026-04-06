package templates

import (
	"context"
	"fmt"

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
