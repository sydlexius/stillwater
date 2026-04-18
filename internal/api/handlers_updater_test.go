package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/updater"
)

// testRouterWithUpdater creates a minimal Router with a real updater.Service.
func testRouterWithUpdater(t *testing.T) *Router {
	t.Helper()

	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	updSvc := updater.NewService(db, logger)
	// Pin to non-Docker so tests that exercise the apply path are
	// deterministic on containerized CI (where /.dockerenv would otherwise
	// flip the service into Docker mode and silently strip coverage).
	// Tests that specifically cover the Docker path construct their own
	// service via updater.NewDockerService.
	updSvc.SetDockerForTest(false)

	authSvc := auth.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	nfoSnapSvc := nfo.NewSnapshotService(db)

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		UpdaterService:     updSvc,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
	})
	return r
}

func TestHandleGetUpdateConfig_Defaults(t *testing.T) {
	r := testRouterWithUpdater(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/updates/config", nil)
	w := httptest.NewRecorder()

	r.handleGetUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var cfg updater.Config
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Channel != updater.ChannelStable {
		t.Errorf("channel = %q, want %q", cfg.Channel, updater.ChannelStable)
	}
	if cfg.AutoCheck {
		t.Error("auto_check should default to false")
	}
}

func TestHandlePutUpdateConfig_Valid(t *testing.T) {
	r := testRouterWithUpdater(t)

	body := `{"channel":"prerelease","auto_check":true}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/api/v1/updates/config",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handlePutUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var cfg updater.Config
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Channel != updater.ChannelPrerelease {
		t.Errorf("channel = %q, want prerelease", cfg.Channel)
	}
	if !cfg.AutoCheck {
		t.Error("auto_check should be true")
	}
}

func TestHandlePutUpdateConfig_Invalid(t *testing.T) {
	r := testRouterWithUpdater(t)

	body := `{"channel":"nightly","auto_check":false}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/api/v1/updates/config",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handlePutUpdateConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleGetUpdateStatus_Idle(t *testing.T) {
	r := testRouterWithUpdater(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/updates/status", nil)
	w := httptest.NewRecorder()

	r.handleGetUpdateStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var st updater.StatusResult
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Contract check: update_available is declared required in UpdateStatus
	// (openapi.yaml). A typed unmarshal into StatusResult would default the
	// field to false if the key were missing, silently passing an omission
	// regression. Re-unmarshal into a raw map and assert key presence.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw status: %v", err)
	}
	if _, ok := raw["update_available"]; !ok {
		t.Fatal(`missing required field "update_available" in UpdateStatus`)
	}
	if st.State != updater.StateIdle {
		t.Errorf("state = %q, want idle", st.State)
	}
	if st.Progress != 0 {
		t.Errorf("progress = %d, want 0", st.Progress)
	}
	if st.IsDocker {
		t.Error("is_docker = true, want false")
	}
}

func TestHandlePostUpdateApply_Docker(t *testing.T) {
	r := testRouterWithUpdater(t)
	// Replace the updater service with a Docker-mode one.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	db2, err := database.Open(filepath.Join(dir, "docker.db"))
	if err != nil {
		t.Fatalf("opening docker db: %v", err)
	}
	if err := database.Migrate(db2); err != nil {
		t.Fatalf("migrating docker db: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	r.updaterService = updater.NewDockerService(db2, logger)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/updates/apply", nil)
	w := httptest.NewRecorder()

	r.handlePostUpdateApply(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (docker blocked); body: %s", w.Code, w.Body.String())
	}
}

// TestHandlePostUpdateApply_AlreadyRunning verifies that the handler maps
// updater.ErrAlreadyRunning to 409 Conflict. The blocking transport holds the
// first apply goroutine in flight (so applyRunning stays set) while the second
// call races through the CAS and returns the sentinel.
func TestHandlePostUpdateApply_AlreadyRunning(t *testing.T) {
	r := testRouterWithUpdater(t)

	block := make(chan struct{})
	r.updaterService.SetHTTPClient(&http.Client{Transport: &blockingTransport{block: block}})
	t.Cleanup(func() {
		close(block)
		// Wait for the goroutine to drain so t.Cleanup DB close does not race.
		deadline := time.Now().Add(2 * time.Second)
		drained := false
		for time.Now().Before(deadline) {
			st := r.updaterService.Status()
			if st.State == updater.StateIdle || st.State == updater.StateError {
				drained = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !drained {
			t.Errorf("timed out waiting for updater worker to drain")
		}
	})

	// First apply: should return 202 and hold the goroutine in fetchReleases.
	req1 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/updates/apply", nil)
	w1 := httptest.NewRecorder()
	r.handlePostUpdateApply(w1, req1)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first apply: status = %d, want 202; body: %s", w1.Code, w1.Body.String())
	}

	// Poll until the goroutine is observed running. Without this, the second
	// call could race ahead of the first Apply setting applyRunning.
	deadline := time.Now().Add(1 * time.Second)
	enteredChecking := false
	for time.Now().Before(deadline) {
		st := r.updaterService.Status()
		if st.State == updater.StateChecking {
			enteredChecking = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !enteredChecking {
		t.Fatal("timed out waiting for updater goroutine to enter StateChecking")
	}

	// Second apply: should return 409 because the first is still in flight.
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/updates/apply", nil)
	w2 := httptest.NewRecorder()
	r.handlePostUpdateApply(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("second apply: status = %d, want 409; body: %s", w2.Code, w2.Body.String())
	}
}

func TestHandlePostUpdateCheck_NoNetwork(t *testing.T) {
	r := testRouterWithUpdater(t)

	// Override the HTTP client with one that always fails.
	r.updaterService.SetHTTPClient(&http.Client{
		Transport: &alwaysFailTransport{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/updates/check", nil)
	w := httptest.NewRecorder()

	r.handlePostUpdateCheck(w, req)

	// Network failure should return 500.
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
}

// TestHandlePutUpdateConfig_BadJSON verifies that malformed JSON returns 400.
func TestHandlePutUpdateConfig_BadJSON(t *testing.T) {
	r := testRouterWithUpdater(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/api/v1/updates/config",
		strings.NewReader("{not valid json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handlePutUpdateConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleGetUpdateStatus_ContentType verifies the response is JSON.
func TestHandleGetUpdateStatus_ContentType(t *testing.T) {
	r := testRouterWithUpdater(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/updates/status", nil)
	w := httptest.NewRecorder()

	r.handleGetUpdateStatus(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestHandleNilUpdaterService verifies that nil updaterService returns 503 on all endpoints.
func TestHandleNilUpdaterService(t *testing.T) {
	r := testRouterWithUpdater(t)
	r.updaterService = nil

	endpoints := []struct {
		method  string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"POST", "/api/v1/updates/check", r.handlePostUpdateCheck},
		{"GET", "/api/v1/updates/status", r.handleGetUpdateStatus},
		{"POST", "/api/v1/updates/apply", r.handlePostUpdateApply},
		{"GET", "/api/v1/updates/config", r.handleGetUpdateConfig},
		{"PUT", "/api/v1/updates/config", r.handlePutUpdateConfig},
	}

	for _, ep := range endpoints {
		req := httptest.NewRequestWithContext(context.Background(), ep.method, ep.path, nil)
		w := httptest.NewRecorder()
		ep.handler(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503", ep.method, ep.path, w.Code)
		}
	}
}

// TestHandlePostUpdateApply_NonDocker verifies that a non-Docker, non-in-progress
// apply starts successfully and the async goroutine settles before the test exits.
func TestHandlePostUpdateApply_NonDocker(t *testing.T) {
	r := testRouterWithUpdater(t)
	// Make the updater's HTTP client immediately fail so the goroutine exits fast.
	r.updaterService.SetHTTPClient(&http.Client{Transport: &alwaysFailTransport{}})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/updates/apply", nil)
	w := httptest.NewRecorder()

	r.handlePostUpdateApply(w, req)

	// Should return 202 Accepted (async start).
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}

	// Poll the status endpoint until the background goroutine reaches a terminal
	// state (idle or error). This prevents t.Cleanup from closing the database
	// while the goroutine is still running, which would cause flaky failures.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		statusReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/updates/status", nil)
		statusW := httptest.NewRecorder()
		r.handleGetUpdateStatus(statusW, statusReq)

		var st updater.StatusResult
		if err := json.NewDecoder(statusW.Body).Decode(&st); err == nil &&
			(st.State == updater.StateIdle || st.State == updater.StateError) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for updater worker to settle")
}

// TestHandleCheckWithMockServer verifies the full check path with a mock GitHub server.
func TestHandleCheckWithMockServer(t *testing.T) {
	r := testRouterWithUpdater(t)

	releases := []map[string]interface{}{
		{
			"tag_name":     "v999.0.0",
			"prerelease":   false,
			"draft":        false,
			"html_url":     "https://github.com/example/repo/releases/v999.0.0",
			"published_at": "2026-01-01T00:00:00Z",
			"assets":       []interface{}{},
		},
	}
	body, err := json.Marshal(releases)
	if err != nil {
		t.Fatalf("marshal releases fixture: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Validate the upstream request shape so a regression in the
		// method or path emitted by fetchReleases fails loudly here
		// rather than passing via a permissive mock.
		if req.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasSuffix(req.URL.Path, "/releases") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	r.updaterService.SetHTTPClient(&http.Client{
		Transport: &rewriteHostTransport{base: srv.URL},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/updates/check", nil)
	w := httptest.NewRecorder()

	r.handlePostUpdateCheck(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var result map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"current", "latest", "channel", "update_available"} {
		if _, ok := result[key]; !ok {
			t.Fatalf("missing required field %q in UpdateCheckResult", key)
		}
	}
	if result["update_available"] != true {
		t.Errorf("update_available = %v, want true", result["update_available"])
	}
	if result["latest"] != "v999.0.0" {
		t.Errorf("latest = %v, want v999.0.0", result["latest"])
	}
	if got, want := result["channel"], string(updater.ChannelStable); got != want {
		t.Errorf("channel = %v, want %q", got, want)
	}
	if current, ok := result["current"].(string); !ok || current == "" {
		t.Errorf("current = %v, want non-empty string", result["current"])
	}
}

// TestBuildUpdatesTabData_NilService verifies that buildUpdatesTabData returns
// sensible defaults when no updater service is wired in.
func TestBuildUpdatesTabData_NilService(t *testing.T) {
	r := testRouterWithUpdater(t)
	r.updaterService = nil

	data := r.buildUpdatesTabData(context.Background())

	if data.Channel != "stable" {
		t.Errorf("Channel = %q, want \"stable\"", data.Channel)
	}
	if data.AutoCheck {
		t.Error("AutoCheck should default to false")
	}
	if data.IsDocker {
		t.Error("IsDocker should default to false")
	}
}

// TestBuildUpdatesTabData_WithService verifies that buildUpdatesTabData reads
// config values from the updater service.
func TestBuildUpdatesTabData_WithService(t *testing.T) {
	r := testRouterWithUpdater(t)
	ctx := context.Background()

	// Store a prerelease channel so we can verify it is reflected in the data.
	if err := r.updaterService.SetConfig(ctx, updater.Config{
		Channel:   updater.ChannelPrerelease,
		AutoCheck: true,
	}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	data := r.buildUpdatesTabData(ctx)

	if data.Channel != string(updater.ChannelPrerelease) {
		t.Errorf("Channel = %q, want %q", data.Channel, updater.ChannelPrerelease)
	}
	if !data.AutoCheck {
		t.Error("AutoCheck should be true after SetConfig")
	}
}

// TestNormalizeSettingsSectionUpdates verifies that "updates" is a valid
// settings section that routes to the updates tab.
func TestNormalizeSettingsSectionUpdates(t *testing.T) {
	got := normalizeSettingsSection("updates")
	if got != "updates" {
		t.Errorf("normalizeSettingsSection(\"updates\") = %q, want \"updates\"", got)
	}
}

// rewriteHostTransport rewrites all request URLs to point at a specific base
// server. Used in tests to intercept GitHub API calls without DNS overrides.
type rewriteHostTransport struct {
	base string
}

func (t *rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.URL.Scheme = "http"
	host := t.base
	if len(host) > 7 && host[:7] == "http://" {
		host = host[7:]
	}
	req2.URL.Host = host
	return http.DefaultTransport.RoundTrip(req2)
}

// alwaysFailTransport rejects all requests.
type alwaysFailTransport struct{}

func (t *alwaysFailTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, &unavailableError{}
}

type unavailableError struct{}

func (e *unavailableError) Error() string { return "network unavailable (test)" }

// blockingTransport holds every request until the block channel is closed.
// Used to keep an Apply goroutine in flight so the handler's 409 "already
// running" branch can be exercised deterministically.
type blockingTransport struct{ block chan struct{} }

func (b *blockingTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	<-b.block
	return nil, &unavailableError{}
}
