package components

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sydlexius/stillwater/internal/i18n"
)

// hxValsJSON builds a JSON object string from key-value pairs for use in
// hx-vals attributes. Using json.Marshal for the entire object avoids
// unsafe quoting from manual string interpolation.
func hxValsJSON(pairs map[string]string) string {
	b, err := json.Marshal(pairs)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// t returns the translated string for the given key from the request context.
// Mirrors web/templates/i18n_helpers.go so component templates can localize
// without an extra cross-package call.
func t(ctx context.Context, key string) string {
	return i18n.TFromCtx(ctx).T(key)
}

// tf returns a translated string with fmt.Sprintf-style interpolation. The
// locale string contains Go format verbs (e.g. "Remove %s filter") and args
// are substituted after translation lookup. Mirrors web/templates/i18n_helpers.go
// so component templates can localize parameterized strings without an extra
// cross-package call. When the key is missing from every locale the bare key
// is returned (Sprintf is skipped) so a missing translation surfaces visibly
// in the UI rather than as a corrupted format string.
func tf(ctx context.Context, key string, args ...any) string {
	tmpl := i18n.TFromCtx(ctx).T(key)
	if tmpl == key {
		return key
	}
	return fmt.Sprintf(tmpl, args...)
}
