package i18n

import (
	"net/http"
)

// Middleware returns HTTP middleware that selects the appropriate locale and
// stores the corresponding Translator in the request context.
//
// Locale selection priority:
//  1. User preference from context/session (reserved for future use)
//  2. Accept-Language request header
//  3. Bundle fallback (defaults to "en")
func Middleware(bundle *Bundle) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Phase 1: use Accept-Language header to pick locale.
			// Future phases will check user preference from session/context first.
			locale := bundle.ParseAcceptLanguage(r.Header.Get("Accept-Language"))
			translator := bundle.Translator(locale)
			ctx := WithTranslator(r.Context(), translator)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
