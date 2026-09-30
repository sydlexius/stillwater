package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/provider/aiblock"
)

const aiListBody = "*://*.one.example/*\n*://*.two.example/*\n"

// aiListServer serves a fixed status and list body as the fake list host and
// counts the requests that reach it.
func aiListServer(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := new(atomic.Int32)
	ls := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(aiListBody))
	}))
	t.Cleanup(ls.Close)
	return ls, hits
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

// aiMux returns a caller that sends requests through the REAL mux (auth
// middleware, CSRF and the route's RequireAdmin wrapper). The Bearer token
// belongs to the seeded administrator, or to a freshly inserted operator when
// role is "operator". A Bearer request has no session cookie, so CSRF does not
// apply to it; an empty role sends no credential at all.
func aiMux(t *testing.T, role string) func(method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return aiMuxCtx(t, role, context.Background())
}

// aiMuxCtx is aiMux with every request carrying reqCtx, so a test can cancel it
// the way a disconnecting client does.
func aiMuxCtx(t *testing.T, role string, reqCtx context.Context) func(method, path string) *httptest.ResponseRecorder {
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
	mux := r.Handler(ctx)
	return func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil).WithContext(reqCtx)
		if role != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
}

const (
	aiStatusPath  = "/api/v1/images/ai-blocklist/status"
	aiRefreshPath = "/api/v1/images/ai-blocklist/refresh"
)

// aiWait bounds a channel receive so a stuck test fails instead of hanging.
// It must be called on the test goroutine (it may t.Fatal).
func aiWait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// aiHeldServer is a list host that answers only after unblock is called.
// started closes when the first request arrives. Cleanup unblocks BEFORE
// closing the server (cleanups run last-in first-out, and Close waits for
// active requests), so a failed test cannot hang on teardown.
func aiHeldServer(t *testing.T) (srv *httptest.Server, started <-chan struct{}, unblock func(), hits *atomic.Int32) {
	t.Helper()
	st, release := make(chan struct{}), make(chan struct{})
	markStarted := sync.OnceFunc(func() { close(st) })
	unblock = sync.OnceFunc(func() { close(release) })
	hits = new(atomic.Int32)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		markStarted()
		<-release
		_, _ = w.Write([]byte(aiListBody))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(unblock)
	return srv, st, unblock, hits
}

func TestAIBlocklistStatus(t *testing.T) {
	good, _ := aiListServer(t, http.StatusOK)
	bad, _ := aiListServer(t, http.StatusInternalServerError)
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
			w := aiMux(t, "administrator")(http.MethodGet, aiStatusPath)
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
	if w := aiMux(t, "operator")(http.MethodGet, aiStatusPath); w.Code != http.StatusForbidden {
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

func TestAIBlocklistRefresh(t *testing.T) {
	good, goodHits := aiListServer(t, http.StatusOK)
	bad, _ := aiListServer(t, http.StatusInternalServerError)

	t.Run("refreshes once, then rate-limits", func(t *testing.T) {
		goodHits.Store(0)
		installAIStore(t, aiblock.Options{URL: good.URL, Client: good.Client()})
		do := aiMux(t, "administrator")
		w := do(http.MethodPost, aiRefreshPath)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"rules":2`) {
			t.Fatalf("first refresh: status %d, body %s", w.Code, w.Body.String())
		}
		w = do(http.MethodPost, aiRefreshPath)
		if ra := w.Header().Get("Retry-After"); w.Code != http.StatusTooManyRequests || ra == "" || ra == "0" {
			t.Errorf("second refresh: status %d, Retry-After %q; want 429 and a positive wait", w.Code, ra)
		}
		if n := goodHits.Load(); n != 1 {
			t.Errorf("list server got %d requests, want 1", n)
		}
	})

	t.Run("failed download is 200 with a sanitized error", func(t *testing.T) {
		installAIStore(t, aiblock.Options{URL: bad.URL, Client: bad.Client()})
		w := aiMux(t, "administrator")(http.MethodPost, aiRefreshPath)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "HTTP 500") {
			t.Fatalf("status %d, body %s", w.Code, w.Body.String())
		}
	})

	t.Run("disabled is 409 and never touches the network", func(t *testing.T) {
		goodHits.Store(0)
		installAIStore(t, aiblock.Options{URL: good.URL, Client: good.Client(), Disabled: true})
		if w := aiMux(t, "administrator")(http.MethodPost, aiRefreshPath); w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body %s", w.Code, w.Body.String())
		}
		if n := goodHits.Load(); n != 0 {
			t.Errorf("list server got %d requests, want 0", n)
		}
	})

	t.Run("concurrent calls fetch once", func(t *testing.T) {
		held, started, unblock, hits := aiHeldServer(t)
		installAIStore(t, aiblock.Options{URL: held.URL, Client: held.Client()})
		do := aiMux(t, "administrator")
		codes := make(chan int, 5)
		for range 5 {
			go func() { codes <- do(http.MethodPost, aiRefreshPath).Code }()
		}
		// The winner is held upstream, so the other four must answer 429 now.
		aiWait(t, started, "the first refresh to reach the list server")
		for i := range 4 {
			if c := aiWait(t, codes, "a rejected refresh"); c != http.StatusTooManyRequests {
				t.Errorf("rejected call %d: status = %d, want 429", i, c)
			}
		}
		unblock()
		if c := aiWait(t, codes, "the winning refresh"); c != http.StatusOK {
			t.Errorf("winning call: status = %d, want 200", c)
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("list server got %d requests, want exactly 1", n)
		}
	})
}

// TestAIBlocklistRefresh_RejectedCallsNeverFetch covers the two guards in front
// of the handler, both through the real mux: a non-admin is refused with 403,
// and a browser-shaped POST with no CSRF token is refused by the CSRF
// middleware (also 403) before auth or the handler run.
func TestAIBlocklistRefresh_RejectedCallsNeverFetch(t *testing.T) {
	good, hits := aiListServer(t, http.StatusOK)
	installAIStore(t, aiblock.Options{URL: good.URL, Client: good.Client()})
	for _, role := range []string{"operator", ""} {
		if w := aiMux(t, role)(http.MethodPost, aiRefreshPath); w.Code != http.StatusForbidden {
			t.Errorf("role %q: status = %d, want 403; body %s", role, w.Code, w.Body.String())
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("list server got %d requests from rejected calls, want 0", n)
	}
}

// TestAIBlocklistRefresh_NoStoreInstalled calls the handler directly: with no
// default store it must answer 409 like the disabled case, not panic.
func TestAIBlocklistRefresh_NoStoreInstalled(t *testing.T) {
	r, _ := testRouter(t)
	w := httptest.NewRecorder()
	r.handleAIBlocklistRefresh(w, httptest.NewRequest(http.MethodPost, aiRefreshPath, nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

// TestAIBlocklistRefresh_SessionPostNeedsCSRFToken uses a VALID session cookie
// (no Bearer, so the CSRF middleware applies) through the real mux: a POST with
// no token or a wrong token is refused with 403 and never fetches, while the
// same session with the token the server issued succeeds. The last step proves
// the session itself is good, so the 403s are CSRF and not an auth failure.
func TestAIBlocklistRefresh_SessionPostNeedsCSRFToken(t *testing.T) {
	good, hits := aiListServer(t, http.StatusOK)
	installAIStore(t, aiblock.Options{URL: good.URL, Client: good.Client()})
	r, authSvc, _ := testRouterWithAuth(t)
	session, err := authSvc.Login(context.Background(), "admin", "password")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := r.Handler(ctx)
	post := func(csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, aiRefreshPath, nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: session})
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	for name, tok := range map[string]string{"no token": "", "wrong token": "not-a-real-token"} {
		if w := post(tok); w.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403; body %s", name, w.Code, w.Body.String())
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("list server got %d requests from CSRF-rejected calls, want 0", n)
	}
	// Any GET through the mux issues a CSRF token cookie; use it.
	get := httptest.NewRequest(http.MethodGet, aiStatusPath, nil)
	get.AddCookie(&http.Cookie{Name: "session", Value: session})
	gw := httptest.NewRecorder()
	mux.ServeHTTP(gw, get)
	var token string
	for _, c := range gw.Result().Cookies() {
		if c.Name == "csrf_token" {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("the mux issued no csrf_token cookie on GET")
	}
	if w := post(token); w.Code != http.StatusOK {
		t.Errorf("valid session and token: status = %d, want 200; body %s", w.Code, w.Body.String())
	}
}

// TestAIBlocklistRefresh_ClientDisconnectDoesNotCancelFetch cancels the request
// context while the list server is holding the response. The fetch must still
// finish: a disconnect must not record a false last_error or start the 429
// lockout.
func TestAIBlocklistRefresh_ClientDisconnectDoesNotCancelFetch(t *testing.T) {
	srv, started, unblock, _ := aiHeldServer(t)
	installAIStore(t, aiblock.Options{URL: srv.URL, Client: srv.Client()})
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Setup can t.Fatal, so it runs here on the test goroutine; the worker only
	// calls the returned function.
	do := aiMuxCtx(t, "administrator", reqCtx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		do(http.MethodPost, aiRefreshPath)
	}()
	aiWait(t, started, "the refresh to reach the list server")
	cancel()
	unblock()
	aiWait(t, done, "the refresh handler to return")
	w := aiMux(t, "administrator")(http.MethodGet, aiStatusPath)
	if body := w.Body.String(); !strings.Contains(body, `"loaded":true`) || strings.Contains(body, "last_error") {
		t.Errorf("after a client disconnect the list must be loaded with no error; body %s", body)
	}
}
