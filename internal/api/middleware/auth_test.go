package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockAuthProvider is a test stub implementing AuthProvider.
type mockAuthProvider struct {
	validateSessionFn  func(ctx context.Context, token string) (string, error)
	validateAPITokenFn func(ctx context.Context, token string) (string, string, error)
	getUserRoleFn      func(ctx context.Context, userID string) (string, error)
}

func (m *mockAuthProvider) ValidateSession(ctx context.Context, token string) (string, error) {
	if m.validateSessionFn != nil {
		return m.validateSessionFn(ctx, token)
	}
	return "", errors.New("not configured")
}

func (m *mockAuthProvider) ValidateAPIToken(ctx context.Context, token string) (string, string, error) {
	if m.validateAPITokenFn != nil {
		return m.validateAPITokenFn(ctx, token)
	}
	return "", "", errors.New("not configured")
}

func (m *mockAuthProvider) GetUserRole(ctx context.Context, userID string) (string, error) {
	if m.getUserRoleFn != nil {
		return m.getUserRoleFn(ctx, userID)
	}
	return "", errors.New("not configured")
}

// --- Auth middleware tests ---

func TestAuth_ValidSession(t *testing.T) {
	mock := &mockAuthProvider{
		validateSessionFn: func(_ context.Context, token string) (string, error) {
			if token == "valid-session" {
				return "user-1", nil
			}
			return "", errors.New("invalid")
		},
		getUserRoleFn: func(_ context.Context, userID string) (string, error) {
			if userID == "user-1" {
				return "administrator", nil
			}
			return "", errors.New("not found")
		},
	}

	var gotUserID, gotMethod, gotRole string
	handler := Auth(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserID = UserIDFromContext(r.Context())
		gotMethod = AuthMethodFromContext(r.Context())
		gotRole = RoleFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "valid-session"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotUserID != "user-1" {
		t.Errorf("userID = %q, want user-1", gotUserID)
	}
	if gotMethod != "session" {
		t.Errorf("authMethod = %q, want session", gotMethod)
	}
	if gotRole != "administrator" {
		t.Errorf("role = %q, want administrator", gotRole)
	}
}

func TestAuth_ValidAPIToken(t *testing.T) {
	mock := &mockAuthProvider{
		validateAPITokenFn: func(_ context.Context, token string) (string, string, error) {
			if token == "sw_valid123" {
				return "user-2", "read,write", nil
			}
			return "", "", errors.New("invalid")
		},
		getUserRoleFn: func(_ context.Context, userID string) (string, error) {
			if userID == "user-2" {
				return "operator", nil
			}
			return "", errors.New("not found")
		},
	}

	var gotUserID, gotMethod, gotRole, gotScopes string
	handler := Auth(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserID = UserIDFromContext(r.Context())
		gotMethod = AuthMethodFromContext(r.Context())
		gotRole = RoleFromContext(r.Context())
		gotScopes = TokenScopesFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sw_valid123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotUserID != "user-2" {
		t.Errorf("userID = %q, want user-2", gotUserID)
	}
	if gotMethod != "api_token" {
		t.Errorf("authMethod = %q, want api_token", gotMethod)
	}
	if gotRole != "operator" {
		t.Errorf("role = %q, want operator", gotRole)
	}
	if gotScopes != "read,write" {
		t.Errorf("scopes = %q, want read,write", gotScopes)
	}
}

func TestAuth_InactiveUser_Session_Returns401(t *testing.T) {
	mock := &mockAuthProvider{
		validateSessionFn: func(_ context.Context, _ string) (string, error) {
			return "user-inactive", nil
		},
		getUserRoleFn: func(_ context.Context, _ string) (string, error) {
			return "", nil // empty role = inactive/not found
		},
	}

	handler := Auth(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next handler should not be called for inactive user")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "some-session"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuth_InactiveUser_APIToken_Returns401(t *testing.T) {
	mock := &mockAuthProvider{
		validateAPITokenFn: func(_ context.Context, _ string) (string, string, error) {
			return "user-deactivated", "read", nil
		},
		getUserRoleFn: func(_ context.Context, _ string) (string, error) {
			return "", nil // empty role = deactivated
		},
	}

	handler := Auth(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next handler should not be called for deactivated API token user")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sw_deactivated")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuth_GetUserRoleError_Session_Returns500(t *testing.T) {
	mock := &mockAuthProvider{
		validateSessionFn: func(_ context.Context, _ string) (string, error) {
			return "user-1", nil
		},
		getUserRoleFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("database connection lost")
		},
	}

	handler := Auth(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next handler should not be called on DB error")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "valid"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestAuth_GetUserRoleError_APIToken_Returns500(t *testing.T) {
	mock := &mockAuthProvider{
		validateAPITokenFn: func(_ context.Context, _ string) (string, string, error) {
			return "user-1", "read", nil
		},
		getUserRoleFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("database connection lost")
		},
	}

	handler := Auth(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next handler should not be called on DB error")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sw_token123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// --- OptionalAuth middleware tests ---

func TestOptionalAuth_ValidSession(t *testing.T) {
	mock := &mockAuthProvider{
		validateSessionFn: func(_ context.Context, _ string) (string, error) {
			return "user-1", nil
		},
		getUserRoleFn: func(_ context.Context, _ string) (string, error) {
			return "administrator", nil
		},
	}

	var gotUserID, gotMethod, gotRole string
	handler := OptionalAuth(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserID = UserIDFromContext(r.Context())
		gotMethod = AuthMethodFromContext(r.Context())
		gotRole = RoleFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "good"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotUserID != "user-1" {
		t.Errorf("userID = %q, want user-1", gotUserID)
	}
	if gotMethod != "session" {
		t.Errorf("authMethod = %q, want session", gotMethod)
	}
	if gotRole != "administrator" {
		t.Errorf("role = %q, want administrator", gotRole)
	}
}

func TestOptionalAuth_InactiveUser_ContinuesUnauthenticated(t *testing.T) {
	mock := &mockAuthProvider{
		validateSessionFn: func(_ context.Context, _ string) (string, error) {
			return "user-inactive", nil
		},
		getUserRoleFn: func(_ context.Context, _ string) (string, error) {
			return "", nil // inactive
		},
	}

	var gotUserID, gotRole string
	called := false
	handler := OptionalAuth(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotUserID = UserIDFromContext(r.Context())
		gotRole = RoleFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "inactive-session"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called for optional auth")
	}
	if gotUserID != "" {
		t.Errorf("userID = %q, want empty (unauthenticated)", gotUserID)
	}
	if gotRole != "" {
		t.Errorf("role = %q, want empty (unauthenticated)", gotRole)
	}
}

func TestOptionalAuth_GetUserRoleError_ContinuesUnauthenticated(t *testing.T) {
	mock := &mockAuthProvider{
		validateAPITokenFn: func(_ context.Context, _ string) (string, string, error) {
			return "user-1", "read", nil
		},
		getUserRoleFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("database error")
		},
	}

	var gotUserID, gotRole string
	called := false
	handler := OptionalAuth(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotUserID = UserIDFromContext(r.Context())
		gotRole = RoleFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sw_token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called for optional auth even on role error")
	}
	if gotUserID != "" {
		t.Errorf("userID = %q, want empty (unauthenticated)", gotUserID)
	}
	if gotRole != "" {
		t.Errorf("role = %q, want empty (unauthenticated)", gotRole)
	}
}

func TestOptionalAuth_NoToken_ContinuesUnauthenticated(t *testing.T) {
	mock := &mockAuthProvider{}

	called := false
	handler := OptionalAuth(mock)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("next handler should be called with no token")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

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
	called := false
	handler := RequireScope("write")(func(w http.ResponseWriter, r *http.Request) {
		called = true
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
	if called {
		t.Error("next handler should not be called on forbidden scope")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
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
