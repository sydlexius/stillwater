package components

import (
	"context"
	"encoding/json"

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
