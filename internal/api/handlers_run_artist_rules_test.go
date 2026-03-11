package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

// testRouterWithPipeline creates a Router that includes a rule pipeline for run-rules tests.
func testRouterWithPipeline(t *testing.T) (*Router, *artist.Service) {
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
	ruleEngine := rule.NewEngine(ruleSvc, db, nil, logger)
	pipeline := rule.NewPipeline(ruleEngine, artistSvc, ruleSvc, nil, logger)
	nfoSnapSvc := nfo.NewSnapshotService(db)
	providerSettings := provider.NewSettingsService(db, nil)

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
		StaticDir:          "../../web/static",
	})

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
	if _, ok := resp["notifications_url"]; !ok {
		t.Error("response missing notifications_url field")
	}
}

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
}
