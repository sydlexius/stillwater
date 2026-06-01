package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveUX(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		cookie string
		want   UXChannel
	}{
		{"stable mode ignores next cookie", "stable", "next", UXStable},
		{"stable mode default", "stable", "", UXStable},
		{"next mode default is next", "next", "", UXNext},
		{"next mode cookie can opt out to stable", "next", "stable", UXStable},
		{"next mode next cookie stays next", "next", "next", UXNext},
		{"dual mode default is stable", "dual", "", UXStable},
		{"dual mode next cookie opts in", "dual", "next", UXNext},
		{"dual mode stable cookie stays stable", "dual", "stable", UXStable},
		{"unknown mode falls back to stable", "bogus", "next", UXStable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveUX(tt.mode, tt.cookie); got != tt.want {
				t.Errorf("ResolveUX(%q, %q) = %q, want %q", tt.mode, tt.cookie, got, tt.want)
			}
		})
	}
}

func TestUXMiddleware_HeaderAndContext(t *testing.T) {
	const bp = "" // empty base path (root deployment)
	tests := []struct {
		name   string
		mode   string
		cookie string
		path   string
		want   UXChannel
	}{
		{"stable mode stable path", "stable", "", "/dashboard", UXStable},
		{"stable mode next path still stable (preview off)", "stable", "", "/next/dashboard", UXStable},
		{"dual no cookie stable path", "dual", "", "/dashboard", UXStable},
		{"dual next-path forces next", "dual", "", "/next/dashboard", UXNext},
		{"dual cookie next on stable path", "dual", "next", "/dashboard", UXNext},
		{"next mode stable path defaults next", "next", "", "/dashboard", UXNext},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotCtx UXChannel
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				gotCtx = UXChannelFromContext(r.Context())
			})
			h := UX(tt.mode, bp)(next)

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: "sw_ux", Value: tt.cookie})
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if got := rec.Header().Get("X-Stillwater-UX"); got != string(tt.want) {
				t.Errorf("X-Stillwater-UX header = %q, want %q", got, tt.want)
			}
			if gotCtx != tt.want {
				t.Errorf("UXChannelFromContext = %q, want %q", gotCtx, tt.want)
			}
		})
	}
}

// TestUXMiddleware_HeaderOptIn covers the X-Stillwater-UX request-header
// opt-in: a next/ page tags its HTMX sub-requests (which hit shared fetch
// endpoints like /dashboard/actions that are NOT under /next/) with this header
// so they resolve to the issuing page's channel without relying on the sw_ux
// cookie. An explicit "stable" header value opts a request back out, and the
// header is honored only when the preview lane is enabled (next/dual modes);
// stable mode ignores it entirely.
func TestUXMiddleware_HeaderOptIn(t *testing.T) {
	const bp = ""
	tests := []struct {
		name   string
		mode   string
		header string // X-Stillwater-UX request header value ("" = unset)
		path   string
		want   UXChannel
	}{
		{"dual header next on shared path forces next", "dual", "next", "/dashboard/actions", UXNext},
		{"next header stable on shared path opts back out", "next", "stable", "/dashboard/actions", UXStable},
		{"dual header stable on shared path stays stable", "dual", "stable", "/dashboard/actions", UXStable},
		{"stable mode ignores next header (preview off)", "stable", "next", "/dashboard/actions", UXStable},
		{"next mode header next stays next", "next", "next", "/dashboard/actions", UXNext},
		{"dual unrecognized header value falls through to default", "dual", "bogus", "/dashboard/actions", UXStable},
		// Path opt-in + header precedence on the /next lane itself. The path sets
		// the channel to next first, then the X-Stillwater-UX header is applied
		// afterward (see UX): so an explicit "stable" header overrides the path
		// opt-in and forces the request back to stable, while a "next" header
		// agrees with the path and stays next. This locks that the header is the
		// final word, not the path.
		{"dual next-path + stable header", "dual", "stable", "/next/dashboard", UXStable},
		{"dual next-path + next header", "dual", "next", "/next/dashboard", UXNext},
		{"dual next-path + unset header keeps path opt-in", "dual", "", "/next/dashboard", UXNext},
		{"next next-path + stable header", "next", "stable", "/next/dashboard", UXStable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotCtx UXChannel
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				gotCtx = UXChannelFromContext(r.Context())
			})
			h := UX(tt.mode, bp)(next)

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.header != "" {
				req.Header.Set("X-Stillwater-UX", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if got := rec.Header().Get("X-Stillwater-UX"); got != string(tt.want) {
				t.Errorf("X-Stillwater-UX response header = %q, want %q", got, tt.want)
			}
			if gotCtx != tt.want {
				t.Errorf("UXChannelFromContext = %q, want %q", gotCtx, tt.want)
			}
		})
	}
}

// UXChannelFromContext must default to stable when no channel was stashed
// (e.g. requests that never passed through the UX middleware).
func TestUXChannelFromContext_DefaultsStable(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := UXChannelFromContext(req.Context()); got != UXStable {
		t.Errorf("UXChannelFromContext(empty) = %q, want stable", got)
	}
}
