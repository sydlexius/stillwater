package middleware

import "net/http"

// cspHeader is the Content-Security-Policy applied to all responses.
// unsafe-inline is required for inline <script> blocks used by HTMX event
// handlers (hx-on::after-request) and chart/cropper initialization, and for
// inline styles used by Tailwind and template markup.
// unsafe-eval is required by HTMX for hx-on::* event handler attributes.
const cspHeader = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' 'unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https:; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"frame-ancestors 'none'; " +
	"form-action 'self'"

// SecurityHeaders adds standard security headers to all responses.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("X-XSS-Protection", "0")
		w.Header().Set("Content-Security-Policy", cspHeader)

		if isSecureRequest(r) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}
