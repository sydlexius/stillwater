package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/auth"
)

type contextKey string

const userIDKey contextKey = "userID"

// OptionalAuth returns middleware that populates the user context if a valid
// session exists but does not reject unauthenticated requests. Use this for
// public pages that change behavior based on auth state.
func OptionalAuth(authService *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token := extractToken(r); token != "" {
				if userID, err := authService.ValidateSession(r.Context(), token); err == nil {
					ctx := context.WithValue(r.Context(), userIDKey, userID)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Auth returns middleware that requires a valid session.
// It checks for a session cookie or Authorization header.
func Auth(authService *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractToken(r)
			if token == "" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}

			userID, err := authService.ValidateSession(r.Context(), token)
			if err != nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), userIDKey, userID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserIDFromContext extracts the authenticated user ID from the context.
func UserIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userIDKey).(string); ok {
		return v
	}
	return ""
}

func extractToken(r *http.Request) string {
	// Check cookie first (web UI)
	if cookie, err := r.Cookie("session"); err == nil {
		return cookie.Value
	}

	// Check Authorization header (API clients)
	header := r.Header.Get("Authorization")
	if strings.HasPrefix(header, "Bearer ") {
		return strings.TrimPrefix(header, "Bearer ")
	}

	return ""
}
