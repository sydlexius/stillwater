package middleware

import "net/http"

// RequireAdmin returns middleware that requires the authenticated user to have
// the "administrator" role. Returns 403 Forbidden for all other roles.
func RequireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role := RoleFromContext(r.Context())
		if role != "administrator" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden: administrator role required"}`))
			return
		}
		next.ServeHTTP(w, r)
	}
}
