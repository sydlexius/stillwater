package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAltSvc_AddsHeaderWhenEnabled(t *testing.T) {
	tests := []struct {
		name string
		port int
		want string
	}{
		{name: "default https", port: 443, want: `h3=":443"; ma=86400`},
		{name: "stillwater default", port: 1973, want: `h3=":1973"; ma=86400`},
		{name: "high port", port: 65535, want: `h3=":65535"; ma=86400`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			h := AltSvc(tt.port)(next)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			h.ServeHTTP(rec, req)
			got := rec.Header().Get("Alt-Svc")
			if got != tt.want {
				t.Errorf("Alt-Svc header = %q; want %q", got, tt.want)
			}
			if !strings.HasPrefix(got, "h3=") {
				t.Errorf("Alt-Svc must advertise h3 (got %q)", got)
			}
		})
	}
}

func TestAltSvc_PassThroughWhenDisabled(t *testing.T) {
	for _, port := range []int{0, -1} {
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		})
		h := AltSvc(port)(next)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Alt-Svc"); got != "" {
			t.Errorf("port=%d: expected no Alt-Svc header; got %q", port, got)
		}
		if !called {
			t.Errorf("port=%d: next handler not invoked", port)
		}
	}
}

func TestAltSvc_DoesNotOverrideOtherHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Custom", "value")
		w.WriteHeader(http.StatusOK)
	})
	h := AltSvc(443)(next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Alt-Svc") == "" {
		t.Error("Alt-Svc missing")
	}
	if got := rec.Header().Get("X-Custom"); got != "value" {
		t.Errorf("X-Custom = %q; want value", got)
	}
}
