package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/auth"
)

func TestSanitizeReturnTo(t *testing.T) {
	t.Parallel()

	longString := "/" + strings.Repeat("a", maxReturnURLLength+10)

	tests := []struct {
		name     string
		raw      string
		basePath string
		want     string
	}{
		// Happy paths (no base path).
		{name: "root", raw: "/", basePath: "", want: "/"},
		{name: "settings", raw: "/settings", basePath: "", want: "/settings"},
		{name: "reports", raw: "/reports/duplicates", basePath: "", want: "/reports/duplicates"},
		{name: "artist detail", raw: "/artists/abc", basePath: "", want: "/artists/abc"},
		{name: "nested settings", raw: "/settings/foreign-files", basePath: "", want: "/settings/foreign-files"},
		{name: "path with query", raw: "/artists?page=2", basePath: "", want: "/artists?page=2"},
		{name: "path with fragment", raw: "/artists#top", basePath: "", want: "/artists#top"},

		// Happy paths under a configured base path.
		{name: "basepath root", raw: "/sw/", basePath: "/sw", want: "/sw/"},
		{name: "basepath exact", raw: "/sw", basePath: "/sw", want: "/sw"},
		{name: "basepath nested", raw: "/sw/reports/duplicates", basePath: "/sw", want: "/sw/reports/duplicates"},
		{name: "basepath with query", raw: "/sw/artists?sort=name", basePath: "/sw", want: "/sw/artists?sort=name"},

		// Reject: empty.
		{name: "empty", raw: "", basePath: "", want: "/"},
		{name: "empty with basepath", raw: "", basePath: "/sw", want: "/sw/"},

		// Reject: protocol-relative open redirects.
		{name: "protocol-relative", raw: "//evil.example.com", basePath: "", want: "/"},
		{name: "protocol-relative with path", raw: "//evil.com/foo", basePath: "", want: "/"},

		// Reject: explicit schemes.
		{name: "https scheme", raw: "https://evil", basePath: "", want: "/"},
		{name: "http scheme", raw: "http://evil/path", basePath: "", want: "/"},
		{name: "javascript scheme", raw: "javascript:alert(1)", basePath: "", want: "/"},
		{name: "data scheme", raw: "data:text/html,evil", basePath: "", want: "/"},
		// "http:./evil" parses as a scheme-prefixed opaque reference; reject.
		{name: "http scheme opaque", raw: "http:./evil", basePath: "", want: "/"},

		// Reject: login and api paths (would loop or hit JSON).
		{name: "login exact", raw: "/login", basePath: "", want: "/"},
		{name: "login sub", raw: "/login/extra", basePath: "", want: "/"},
		{name: "api root", raw: "/api", basePath: "", want: "/"},
		{name: "api v1", raw: "/api/v1/artists", basePath: "", want: "/"},
		{name: "api auth login", raw: "/api/v1/auth/login", basePath: "", want: "/"},
		{name: "basepath login", raw: "/sw/login", basePath: "/sw", want: "/sw/"},
		{name: "basepath api", raw: "/sw/api/v1/x", basePath: "/sw", want: "/sw/"},

		// Reject: header-injection sentinels.
		{name: "CRLF", raw: "/settings\r\nX-Injected: 1", basePath: "", want: "/"},
		{name: "LF only", raw: "/settings\nfoo", basePath: "", want: "/"},
		{name: "CR only", raw: "/settings\rfoo", basePath: "", want: "/"},
		{name: "null byte", raw: "/settings\x00evil", basePath: "", want: "/"},
		{name: "tab", raw: "/settings\tevil", basePath: "", want: "/"},

		// Reject: bare relative paths (no leading slash).
		{name: "no slash", raw: "settings", basePath: "", want: "/"},
		{name: "evil dot com", raw: "evil.com", basePath: "", want: "/"},

		// Reject: backslashes. Some browsers normalize them to forward
		// slashes which could sneak past the no-scheme/no-host checks.
		{name: "backslash protocol-relative", raw: `\\evil.com`, basePath: "", want: "/"},
		{name: "backslash mid-path", raw: `/settings\..\admin`, basePath: "", want: "/"},
		{name: "single backslash", raw: `/\evil`, basePath: "", want: "/"},

		// Reject: too long.
		{name: "oversized", raw: longString, basePath: "", want: "/"},

		// Reject: outside base path.
		{name: "outside basepath", raw: "/other/path", basePath: "/sw", want: "/sw/"},
		// /sw should match exactly, /swrong is a different prefix.
		{name: "basepath prefix collision", raw: "/swrong/path", basePath: "/sw", want: "/sw/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeReturnTo(tt.raw, tt.basePath)
			if got != tt.want {
				t.Errorf("sanitizeReturnTo(%q, %q) = %q, want %q", tt.raw, tt.basePath, got, tt.want)
			}
		})
	}
}

// TestHandleLogin_HonorsReturnURL verifies that handleLogin reads return_url
// from the form body and emits it as the HX-Redirect header on success.
func TestHandleLogin_HonorsReturnURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		returnURL string
		want      string
	}{
		{name: "safe path", returnURL: "/reports/duplicates", want: "/reports/duplicates"},
		{name: "open redirect attempt", returnURL: "//evil.example.com", want: "/"},
		{name: "scheme attempt", returnURL: "https://evil", want: "/"},
		{name: "api blocked", returnURL: "/api/v1/artists", want: "/"},
		{name: "missing falls back to root", returnURL: "", want: "/"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r, _, _ := testRouterWithAuth(t)

			form := url.Values{}
			form.Set("username", "admin")
			form.Set("password", "password")
			form.Set("provider", "local")
			if tt.returnURL != "" {
				form.Set("return_url", tt.returnURL)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			r.handleLogin(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			got := w.Header().Get("HX-Redirect")
			if got != tt.want {
				t.Errorf("HX-Redirect = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestHandleLogin_BasePathRespected verifies that when a base path is
// configured the redirect destination stays inside it, and unsafe inputs
// fall back to basePath+"/" rather than the bare root.
func TestHandleLogin_BasePathRespected(t *testing.T) {
	t.Parallel()

	r, _, _ := testRouterWithAuth(t)
	r.basePath = "/sw"

	cases := []struct {
		name      string
		returnURL string
		want      string
	}{
		{name: "inside basepath", returnURL: "/sw/reports/duplicates", want: "/sw/reports/duplicates"},
		{name: "outside basepath falls back", returnURL: "/other/path", want: "/sw/"},
		{name: "empty falls back to basepath root", returnURL: "", want: "/sw/"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{}
			form.Set("username", "admin")
			form.Set("password", "password")
			form.Set("provider", "local")
			if tt.returnURL != "" {
				form.Set("return_url", tt.returnURL)
			}

			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			r.handleLogin(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			got := w.Header().Get("HX-Redirect")
			if got != tt.want {
				t.Errorf("HX-Redirect = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCompleteLoginRedirect_FormReturnURL verifies the password-flow callers
// that finish with a browser redirect (rather than HX-Redirect JSON) honor a
// return_url form value when present.
func TestCompleteLoginRedirect_FormReturnURL(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithAuth(t)

	provider := &stubFederatedProvider{
		providerType: "test-redir-form",
		identity: &auth.Identity{
			ProviderID:   "remote-uid-1",
			ProviderType: "test-redir-form",
			DisplayName:  "Form User",
		},
	}

	form := url.Values{}
	form.Set("return_url", "/reports/duplicates")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/oidc/callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	r.completeLoginRedirect(w, req, provider, provider.identity)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}
	got := w.Header().Get("Location")
	if got != "/reports/duplicates" {
		t.Errorf("Location = %q, want %q", got, "/reports/duplicates")
	}
}

// TestCompleteLoginRedirect_OIDCCookieFallback exercises the cookie-based
// fallback used by the OIDC callback: when the form has no return_url but an
// oidc_return cookie is present, the cookie value drives the redirect AND
// gets cleared via a Max-Age=-1 Set-Cookie header so the browser drops it.
func TestCompleteLoginRedirect_OIDCCookieFallback(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithAuth(t)

	provider := &stubFederatedProvider{
		providerType: "test-redir-cookie",
		identity: &auth.Identity{
			ProviderID:   "remote-uid-2",
			ProviderType: "test-redir-cookie",
			DisplayName:  "Cookie User",
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback", nil)
	req.AddCookie(&http.Cookie{Name: "oidc_return", Value: "/artists/abc"})
	w := httptest.NewRecorder()

	r.completeLoginRedirect(w, req, provider, provider.identity)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/artists/abc" {
		t.Errorf("Location = %q, want /artists/abc", got)
	}

	// Cookie clear: look for an oidc_return Set-Cookie with Max-Age <= 0.
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == "oidc_return" && c.MaxAge < 0 {
			cleared = true
			break
		}
	}
	if !cleared {
		t.Errorf("expected oidc_return cookie to be cleared (Max-Age<0); got cookies %+v", w.Result().Cookies())
	}
}

// TestCompleteLoginRedirect_RejectsOpenRedirect covers the security envelope:
// an attacker-controlled oidc_return cookie value cannot bounce the user off
// the deployment's origin.
func TestCompleteLoginRedirect_RejectsOpenRedirect(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithAuth(t)

	provider := &stubFederatedProvider{
		providerType: "test-redir-evil",
		identity: &auth.Identity{
			ProviderID:   "remote-uid-3",
			ProviderType: "test-redir-evil",
			DisplayName:  "Evil Probe",
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback", nil)
	req.AddCookie(&http.Cookie{Name: "oidc_return", Value: "//evil.example.com"})
	w := httptest.NewRecorder()

	r.completeLoginRedirect(w, req, provider, provider.identity)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/" {
		t.Errorf("Location = %q, want / (fallback)", got)
	}
}
