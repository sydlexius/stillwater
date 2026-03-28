package middleware

import (
	"context"
	"net/http"
)

// SettingReader reads a single string setting from the application store.
// It is satisfied by any function that matches the signature, allowing the
// middleware to depend on an interface rather than a concrete DB type.
type SettingReader func(ctx context.Context, key, fallback string) string

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

// RequireMultiUser returns middleware that returns 404 when the multi_user.enabled
// setting is not "true". Apply this to invite and user management routes so that
// the multi-user feature can be hidden when running in single-user mode.
func RequireMultiUser(getSetting SettingReader) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			enabled := getSetting(r.Context(), "multi_user.enabled", "false")
			if enabled != "true" && enabled != "1" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not found"}`))
				return
			}
			next.ServeHTTP(w, r)
		}
	}
}
