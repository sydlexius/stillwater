package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireAdminAllows(t *testing.T) {
	handler := RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), userRoleKey, "administrator")
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
}

func TestRequireAdminBlocksOperator(t *testing.T) {
	handler := RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), userRoleKey, "operator")
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestRequireAdminBlocksNoRole(t *testing.T) {
	handler := RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestRoleFromContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), userRoleKey, "operator")
	if got := RoleFromContext(ctx); got != "operator" {
		t.Errorf("RoleFromContext() = %q, want %q", got, "operator")
	}
}

func TestRoleFromContextEmpty(t *testing.T) {
	if got := RoleFromContext(context.Background()); got != "" {
		t.Errorf("RoleFromContext() = %q, want empty", got)
	}
}
