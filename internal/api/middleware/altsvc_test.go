package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAltSvc_HeaderSet(t *testing.T) {
	handler := AltSvc(1973)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	got := w.Header().Get("Alt-Svc")
	want := `h3=":1973"; ma=86400`
	if got != want {
		t.Errorf("Alt-Svc = %q, want %q", got, want)
	}
}

func TestAltSvc_PassesThrough(t *testing.T) {
	called := false
	handler := AltSvc(1973)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Error("next handler was not called")
	}
	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTeapot)
	}
}

func TestAltSvc_DifferentPort(t *testing.T) {
	handler := AltSvc(8443)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	got := w.Header().Get("Alt-Svc")
	want := `h3=":8443"; ma=86400`
	if got != want {
		t.Errorf("Alt-Svc = %q, want %q", got, want)
	}
}
