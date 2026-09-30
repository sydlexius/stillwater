package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/provider/aiblock"
)

const aiListBody = "*://*.one.example/*\n*://*.two.example/*\n"

// aiListServer serves a fixed status and list body as the fake list host.
func aiListServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	ls := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(aiListBody))
	}))
	t.Cleanup(ls.Close)
	return ls
}

// installAIStore installs a store as the process default for one test. These
// tests are not parallel: the default store is process-wide.
func installAIStore(t *testing.T, opts aiblock.Options) *aiblock.Store {
	t.Helper()
	st := aiblock.NewStore(opts)
	aiblock.SetDefault(st)
	t.Cleanup(func() { aiblock.SetDefault(nil) })
	return st
}

// aiMuxCall sends one request through the REAL mux (auth middleware, CSRF and
// the route's RequireAdmin wrapper). The Bearer token belongs to the seeded
// administrator, or to a freshly inserted operator when role is "operator".
// A Bearer request has no session cookie, so CSRF does not apply to it.
func aiMuxCall(t *testing.T, role, method string) *httptest.ResponseRecorder {
	t.Helper()
	r, authSvc, userID := testRouterWithAuth(t)
	if role == "operator" {
		userID = "op-user"
		if _, err := r.db.Exec(`INSERT INTO users (id, username, role) VALUES (?, 'op', 'operator')`, userID); err != nil {
			t.Fatalf("inserting operator: %v", err)
		}
	}
	token, _, err := authSvc.CreateAPIToken(context.Background(), userID, "ai-test", string(auth.ScopeAdmin))
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest(method, "/api/v1/images/ai-blocklist/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.Handler(ctx).ServeHTTP(w, req)
	return w
}

func TestAIBlocklistStatus(t *testing.T) {
	good := aiListServer(t, http.StatusOK)
	bad := aiListServer(t, http.StatusInternalServerError)
	tests := []struct {
		name  string
		store func(t *testing.T)
		check func(t *testing.T, got map[string]any, body string)
	}{
		{"loaded shows host only", func(t *testing.T) {
			st := installAIStore(t, aiblock.Options{URL: good.URL + "/secret/path?token=abc", Client: good.Client()})
			if err := st.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
		}, func(t *testing.T, got map[string]any, body string) {
			if got["enabled"] != true || got["loaded"] != true || got["rules"] != float64(2) ||
				got["last_fetch"] == nil || got["last_checked"] == nil {
				t.Fatalf("unexpected status: %s", body)
			}
			if want := strings.TrimPrefix(good.URL, "http://"); got["source_host"] != want {
				t.Errorf("source_host = %v, want %q", got["source_host"], want)
			}
			if strings.Contains(body, "secret") || strings.Contains(body, "token") {
				t.Errorf("body leaks the URL path or query: %s", body)
			}
		}},
		{"failed download is sanitized", func(t *testing.T) {
			st := installAIStore(t, aiblock.Options{URL: bad.URL + "/list?key=hunter2", Client: bad.Client()})
			_ = st.Refresh(context.Background())
		}, func(t *testing.T, got map[string]any, body string) {
			if got["loaded"] != false || got["last_error"] != "The list server answered with HTTP 500." {
				t.Fatalf("unexpected status: %s", body)
			}
			if strings.Contains(body, "hunter2") || strings.Contains(body, "/list") {
				t.Errorf("body leaks the raw error: %s", body)
			}
		}},
		{"disabled reports off", func(t *testing.T) {
			installAIStore(t, aiblock.Options{URL: good.URL, Disabled: true})
		}, func(t *testing.T, got map[string]any, body string) {
			if got["enabled"] != false || got["loaded"] != false || got["source_host"] != nil {
				t.Fatalf("unexpected disabled status: %s", body)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.store(t)
			w := aiMuxCall(t, "administrator", http.MethodGet)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding %q: %v", w.Body.String(), err)
			}
			tc.check(t, got, w.Body.String())
		})
	}
}

// TestAIBlocklistStatus_NonAdminForbiddenThroughMux drives a real operator
// credential through the real mux, so it fails if the route loses RequireAdmin.
func TestAIBlocklistStatus_NonAdminForbiddenThroughMux(t *testing.T) {
	installAIStore(t, aiblock.Options{Disabled: true})
	if w := aiMuxCall(t, "operator", http.MethodGet); w.Code != http.StatusForbidden {
		t.Errorf("operator: status = %d, want 403; body %s", w.Code, w.Body.String())
	}
}

// TestAIBlocklistStatus_NoStoreInstalled calls the handler directly: with no
// default store (a build that never wired one) it must answer "off", not panic.
func TestAIBlocklistStatus_NoStoreInstalled(t *testing.T) {
	r, _ := testRouter(t)
	w := httptest.NewRecorder()
	r.handleAIBlocklistStatus(w, httptest.NewRequest(http.MethodGet, "/api/v1/images/ai-blocklist/status", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
}

func TestSanitizeAIBlocklistError(t *testing.T) {
	for raw, want := range map[string]string{
		"":                           "",
		"list not cached: disk full": "The list is active but could not be saved to the cache.",
		"cached list: bad line":      "The saved copy of the list was ignored.",
		"fetching list: HTTP 503":    "The list server answered with HTTP 503.",
		"fetching list: HTTP boom":   "The list could not be downloaded.",
		"fetching list: dial tcp":    "The list could not be downloaded.",
		"list has too few rules":     "The downloaded list was rejected.",
		"list has 10 rules, under 50% of the 100 fetched earlier; restart Stillwater to accept it": "The downloaded list was much smaller than the active one and was not used. Restart Stillwater to accept it.",
	} {
		if got := sanitizeAIBlocklistError(raw); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", raw, got, want)
		}
	}
}
