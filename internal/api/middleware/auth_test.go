package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestExtractToken verifies token extraction from cookie, header, and query.
func TestExtractToken_Cookie(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "session-abc"})

	got := extractToken(req)
	if got != "session-abc" {
		t.Errorf("extractToken(cookie) = %q, want %q", got, "session-abc")
	}
}

func TestExtractToken_BearerHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sw_testtoken123")

	got := extractToken(req)
	if got != "sw_testtoken123" {
		t.Errorf("extractToken(bearer) = %q, want %q", got, "sw_testtoken123")
	}
}

func TestExtractToken_QueryParam(t *testing.T) {
	req := httptest.NewRequest("GET", "/?apikey=qp-token", nil)

	got := extractToken(req)
	if got != "qp-token" {
		t.Errorf("extractToken(query) = %q, want %q", got, "qp-token")
	}
}

func TestExtractToken_CookieTakesPrecedence(t *testing.T) {
	req := httptest.NewRequest("GET", "/?apikey=qp-token", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "cookie-token"})
	req.Header.Set("Authorization", "Bearer header-token")

	got := extractToken(req)
	if got != "cookie-token" {
		t.Errorf("extractToken(precedence) = %q, want cookie-token", got)
	}
}

func TestExtractToken_HeaderOverQuery(t *testing.T) {
	req := httptest.NewRequest("GET", "/?apikey=qp-token", nil)
	req.Header.Set("Authorization", "Bearer header-token")

	got := extractToken(req)
	if got != "header-token" {
		t.Errorf("extractToken(header over query) = %q, want header-token", got)
	}
}

func TestExtractToken_Empty(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	got := extractToken(req)
	if got != "" {
		t.Errorf("extractToken(empty) = %q, want empty", got)
	}
}

// TestHasScope verifies scope checking logic.
func TestHasScope_SessionHasAll(t *testing.T) {
	ctx := context.WithValue(context.Background(), authMethodKey, "session")
	if !HasScope(ctx, "write") {
		t.Error("session auth should have all scopes")
	}
}

func TestHasScope_APITokenWithMatchingScope(t *testing.T) {
	ctx := context.WithValue(context.Background(), authMethodKey, "api_token")
	ctx = context.WithValue(ctx, tokenScopesKey, "read,write")
	if !HasScope(ctx, "write") {
		t.Error("token with 'write' scope should match")
	}
}

func TestHasScope_APITokenMissingScope(t *testing.T) {
	ctx := context.WithValue(context.Background(), authMethodKey, "api_token")
	ctx = context.WithValue(ctx, tokenScopesKey, "read")
	if HasScope(ctx, "write") {
		t.Error("token without 'write' scope should not match")
	}
}

func TestHasScope_AdminGrantsAll(t *testing.T) {
	ctx := context.WithValue(context.Background(), authMethodKey, "api_token")
	ctx = context.WithValue(ctx, tokenScopesKey, "admin")
	if !HasScope(ctx, "write") {
		t.Error("admin scope should grant all permissions")
	}
}

func TestHasScope_NoAuth(t *testing.T) {
	ctx := context.Background()
	if HasScope(ctx, "read") {
		t.Error("no auth context should not have any scope")
	}
}

// TestRequireScope verifies the scope middleware returns 403 on missing scope.
func TestRequireScope_Forbidden(t *testing.T) {
	handler := RequireScope("write")(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// API token without write scope
	ctx := context.WithValue(context.Background(), authMethodKey, "api_token")
	ctx = context.WithValue(ctx, tokenScopesKey, "read")

	req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("RequireScope status = %d, want 403", rec.Code)
	}
}

func TestRequireScope_Allowed(t *testing.T) {
	handler := RequireScope("read")(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx := context.WithValue(context.Background(), authMethodKey, "api_token")
	ctx = context.WithValue(ctx, tokenScopesKey, "read,write")

	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("RequireScope status = %d, want 200", rec.Code)
	}
}

// TestUserIDFromContext verifies user ID extraction.
func TestUserIDFromContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), userIDKey, "user-123")
	if got := UserIDFromContext(ctx); got != "user-123" {
		t.Errorf("UserIDFromContext = %q, want user-123", got)
	}
}

func TestUserIDFromContext_Empty(t *testing.T) {
	if got := UserIDFromContext(context.Background()); got != "" {
		t.Errorf("UserIDFromContext(empty) = %q, want empty", got)
	}
}
