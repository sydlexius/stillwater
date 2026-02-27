package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/auth"
)

type contextKey string

const (
	userIDKey      contextKey = "userID"
	authMethodKey  contextKey = "authMethod"
	tokenScopesKey contextKey = "tokenScopes"
)

// OptionalAuth returns middleware that populates the user context if a valid
// session exists but does not reject unauthenticated requests. Use this for
// public pages that change behavior based on auth state.
func OptionalAuth(authService *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token := extractToken(r); token != "" {
				if strings.HasPrefix(token, auth.APITokenPrefix) {
					if userID, scopes, err := authService.ValidateAPIToken(r.Context(), token); err == nil {
						ctx := context.WithValue(r.Context(), userIDKey, userID)
						ctx = context.WithValue(ctx, authMethodKey, "api_token")
						ctx = context.WithValue(ctx, tokenScopesKey, scopes)
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				} else {
					if userID, err := authService.ValidateSession(r.Context(), token); err == nil {
						ctx := context.WithValue(r.Context(), userIDKey, userID)
						ctx = context.WithValue(ctx, authMethodKey, "session")
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Auth returns middleware that requires a valid session or API token.
func Auth(authService *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractToken(r)
			if token == "" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}

			if strings.HasPrefix(token, auth.APITokenPrefix) {
				userID, scopes, err := authService.ValidateAPIToken(r.Context(), token)
				if err != nil {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
				ctx := context.WithValue(r.Context(), userIDKey, userID)
				ctx = context.WithValue(ctx, authMethodKey, "api_token")
				ctx = context.WithValue(ctx, tokenScopesKey, scopes)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			userID, err := authService.ValidateSession(r.Context(), token)
			if err != nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), userIDKey, userID)
			ctx = context.WithValue(ctx, authMethodKey, "session")
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

// AuthMethodFromContext returns "session" or "api_token".
func AuthMethodFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(authMethodKey).(string); ok {
		return v
	}
	return ""
}

// TokenScopesFromContext returns the comma-separated scopes string for API token auth.
func TokenScopesFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tokenScopesKey).(string); ok {
		return v
	}
	return ""
}

// HasScope checks if the current auth context includes the given scope.
// Session auth has all scopes. Admin scope grants all permissions.
func HasScope(ctx context.Context, scope string) bool {
	method := AuthMethodFromContext(ctx)
	if method == "session" {
		return true
	}
	scopes := TokenScopesFromContext(ctx)
	for _, s := range strings.Split(scopes, ",") {
		if strings.TrimSpace(s) == scope || strings.TrimSpace(s) == string(auth.ScopeAdmin) {
			return true
		}
	}
	return false
}

// RequireScope returns middleware that checks for a specific token scope.
func RequireScope(scope string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !HasScope(r.Context(), scope) {
				http.Error(w, `{"error":"forbidden: missing scope `+scope+`"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		}
	}
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

	// Check query parameter (for webhook URLs)
	if apikey := r.URL.Query().Get("apikey"); apikey != "" {
		return apikey
	}

	return ""
}
