package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"

	"log/slog"
)

// testRouterWithPipelineFull creates a Router with a rule pipeline and returns the rule service
// so callers can manipulate rule state (e.g., disable all rules for isolation).
func testRouterWithPipelineFull(t *testing.T) (*Router, *artist.Service, *rule.Service) {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	ruleEngine := rule.NewEngine(ruleSvc, db, nil, nil, logger)
	pipeline := rule.NewPipeline(ruleEngine, artistSvc, ruleSvc, nil, nil, logger)
	nfoSnapSvc := nfo.NewSnapshotService(db)
	providerSettings := provider.NewSettingsService(db, enc)

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		RuleService:        ruleSvc,
		RuleEngine:         ruleEngine,
		Pipeline:           pipeline,
		NFOSnapshotService: nfoSnapSvc,
		ProviderSettings:   providerSettings,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
	})

	return r, artistSvc, ruleSvc
}

// testRouterWithPipeline creates a Router that includes a rule pipeline for run-rules tests.
func testRouterWithPipeline(t *testing.T) (*Router, *artist.Service) {
	t.Helper()
	r, artistSvc, _ := testRouterWithPipelineFull(t)
	return r, artistSvc
}

func TestHandleRunArtistRules_NotFound(t *testing.T) {
	r, _ := testRouterWithPipeline(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nonexistent/run-rules", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleRunArtistRules_HTMX_NotFound(t *testing.T) {
	r, _ := testRouterWithPipeline(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/nonexistent/run-rules", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
	}
	body := w.Body.String()
	if !strings.Contains(body, "Artist not found") {
		t.Errorf("HTMX body = %q, want 'Artist not found' fragment", body)
	}
}

func TestHandleRunArtistRules_ReturnsJSON(t *testing.T) {
	r, artistSvc := testRouterWithPipeline(t)

	a := addTestArtist(t, artistSvc, "Test Artist")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if _, ok := resp["violations_found"]; !ok {
		t.Error("response missing violations_found field")
	}
	// The JSON response now points at the dashboard with a search param
	// pre-filled for this artist. Matches the HTMX link below.
	if got, ok := resp["dashboard_url"]; !ok {
		t.Error("response missing dashboard_url field")
	} else if gotStr, _ := got.(string); !strings.HasPrefix(gotStr, "/?search=") {
		t.Errorf("dashboard_url = %q, want prefix %q", gotStr, "/?search=")
	}
}

func TestHandleRunArtistRules_HTMX_NoViolations(t *testing.T) {
	r, artistSvc, ruleSvc := testRouterWithPipelineFull(t)

	// Disable all rules so a fresh artist produces zero violations.
	rules, err := ruleSvc.List(context.Background())
	if err != nil {
		t.Fatalf("listing rules: %v", err)
	}
	for i := range rules {
		rules[i].Enabled = false
		if err := ruleSvc.Update(context.Background(), &rules[i]); err != nil {
			t.Fatalf("disabling rule %s: %v", rules[i].ID, err)
		}
	}

	a := addTestArtist(t, artistSvc, "Clean Artist")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
	}
	body := w.Body.String()
	if !strings.Contains(body, "No violations found.") {
		t.Errorf("HTMX body = %q, want 'No violations found.'", body)
	}
}

func TestHandleRunArtistRules_HTMX_ViolationsFound(t *testing.T) {
	// Enable only RuleBioExists so the test does not depend on filesystem state or
	// pathless-library handling.
	// A fresh artist with no biography always violates this DB-only rule.
	r, artistSvc, ruleSvc := testRouterWithPipelineFull(t)

	rules, err := ruleSvc.List(context.Background())
	if err != nil {
		t.Fatalf("listing rules: %v", err)
	}
	for i := range rules {
		rules[i].Enabled = rules[i].ID == rule.RuleBioExists
		if err := ruleSvc.Update(context.Background(), &rules[i]); err != nil {
			t.Fatalf("updating rule %s: %v", rules[i].ID, err)
		}
	}

	a := addTestArtist(t, artistSvc, "Violation Artist")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
	}
	body := w.Body.String()
	if !strings.Contains(body, "violation(s).") {
		t.Errorf("HTMX body = %q, want violations message with 'violation(s).'", body)
	}
	// The HTMX summary no longer includes the inline dashboard link; the
	// artist:show-violations-tab trigger in the response header switches
	// the caller to the tab directly. Verify the old link is gone so a
	// regression cannot reintroduce two competing navigation targets.
	if strings.Contains(body, "/?search=") {
		t.Errorf("HTMX body still contains dashboard link; expected tab-switch only: %s", body)
	}
	// Run Rules can auto-fix violations, so the sidebar/dashboard badge must
	// refresh and the artist detail page must switch to the Violations tab
	// so the refreshed list is visible inline. Both events ride the same
	// HX-Trigger header; pages that do not listen for the tab-switch event
	// are unaffected.
	wantTrigger := "dashboard:action-resolved, artist:show-violations-tab"
	if trig := w.Header().Get("HX-Trigger"); trig != wantTrigger {
		t.Errorf("HX-Trigger = %q, want %q", trig, wantTrigger)
	}
}

func TestHandleRunArtistRules_BasePath(t *testing.T) {
	r, artistSvc, _ := testRouterWithPipelineFull(t)
	r.basePath = "/app"

	a := addTestArtist(t, artistSvc, "BasePath Artist")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	got, ok := resp["dashboard_url"]
	if !ok {
		t.Error("response missing dashboard_url field")
	} else if gotStr, _ := got.(string); !strings.HasPrefix(gotStr, "/app/?search=") {
		t.Errorf("dashboard_url = %q, want prefix %q", gotStr, "/app/?search=")
	}
}

func TestHandleRunArtistRules_HTMX_DBError(t *testing.T) {
	r, artistSvc := testRouterWithPipeline(t)

	a := addTestArtist(t, artistSvc, "DB Error Artist")

	// Close the DB to force a database error on the lookup.
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "Artist not found") {
		t.Errorf("HTMX body = %q, DB error must not be reported as 'Artist not found'", body)
	}
	if !strings.Contains(body, "Failed") {
		t.Errorf("HTMX body = %q, expected a failure message", body)
	}
}

// TestHandleRunArtistRules_ReturnsHTMLForHTMX verifies content-type switching.
// Branch-level body assertions are covered by TestHandleRunArtistRules_HTMX_NoViolations
// and TestHandleRunArtistRules_HTMX_ViolationsFound.
func TestHandleRunArtistRules_ReturnsHTMLForHTMX(t *testing.T) {
	r, artistSvc := testRouterWithPipeline(t)

	a := addTestArtist(t, artistSvc, "HTMX Artist")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
	}
	body := w.Body.String()
	if !strings.Contains(body, "<div") {
		t.Errorf("HTMX body = %q, want a <div> fragment", body)
	}
}
