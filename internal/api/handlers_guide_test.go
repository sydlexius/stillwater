package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
)

func TestHandleGuidePage_Authenticated(t *testing.T) {
	r := testRouterForOnboarding(t)

	req := httptest.NewRequest(http.MethodGet, "/guide", nil)
	ctx := middleware.WithTestUserID(req.Context(), "test-user-id")
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	r.handleGuidePage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "User Guide") {
		t.Error("expected body to contain \"User Guide\"")
	}
}

func TestHandleGuidePage_Unauthenticated(t *testing.T) {
	r := testRouterForOnboarding(t)

	req := httptest.NewRequest(http.MethodGet, "/guide", nil)
	w := httptest.NewRecorder()

	r.handleGuidePage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "Sign in") {
		t.Error("expected body to contain \"Sign in\" (login page)")
	}
}
