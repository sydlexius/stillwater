package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
)

// testRouter creates a minimal Router for handler tests with an in-memory DB.
func testRouter(t *testing.T) (*Router, *artist.Service) {
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

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	ruleEngine := rule.NewEngine(ruleSvc, db, nil, logger)
	nfoSnapSvc := nfo.NewSnapshotService(db)
	providerSettings := provider.NewSettingsService(db, nil)

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		RuleService:        ruleSvc,
		RuleEngine:         ruleEngine,
		NFOSnapshotService: nfoSnapSvc,
		ProviderSettings:   providerSettings,
		DB:                 db,
		Logger:             logger,
		StaticDir:          "../../web/static",
	})

	return r, artistSvc
}

// addTestArtist inserts a test artist and returns it.
func addTestArtist(t *testing.T, svc *artist.Service, name string) *artist.Artist {
	t.Helper()
	a := &artist.Artist{
		Name:     name,
		SortName: name,
		Type:     "group",
		Path:     "/music/" + name,
		Genres:   []string{"Rock"},
	}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist %s: %v", name, err)
	}
	return a
}

func TestHandleReportHealth_JSON(t *testing.T) {
	r, artistSvc := testRouter(t)

	addTestArtist(t, artistSvc, "Artist A")
	addTestArtist(t, artistSvc, "Artist B")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health", nil)
	w := httptest.NewRecorder()

	r.handleReportHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp healthSummary
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp.TotalArtists != 2 {
		t.Errorf("TotalArtists = %d, want 2", resp.TotalArtists)
	}
	if resp.Score < 0 || resp.Score > 100 {
		t.Errorf("Score = %.1f, want between 0 and 100", resp.Score)
	}
}

func TestHandleReportHealth_HTMX(t *testing.T) {
	r, artistSvc := testRouter(t)

	addTestArtist(t, artistSvc, "HTMX Artist")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleReportHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := w.Body.String()
	if len(body) == 0 {
		t.Error("expected non-empty HTML response")
	}
}

func TestHandleReportHealth_Empty(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health", nil)
	w := httptest.NewRecorder()

	r.handleReportHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp healthSummary
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp.TotalArtists != 0 {
		t.Errorf("TotalArtists = %d, want 0", resp.TotalArtists)
	}
	if resp.Score != 100.0 {
		t.Errorf("Score = %.1f, want 100.0 for empty library", resp.Score)
	}
}

func TestHandleReportHealthHistory(t *testing.T) {
	r, artistSvc := testRouter(t)

	addTestArtist(t, artistSvc, "History Artist")

	// Record a health snapshot first
	if err := r.ruleService.RecordHealthSnapshot(context.Background(), 1, 1, 100.0); err != nil {
		t.Fatalf("recording snapshot: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health/history", nil)
	w := httptest.NewRecorder()

	r.handleReportHealthHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string][]rule.HealthSnapshot
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	history := resp["history"]
	if len(history) == 0 {
		t.Error("expected at least one history entry")
	}
}

func TestHandleReportHealthHistory_Empty(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health/history", nil)
	w := httptest.NewRecorder()

	r.handleReportHealthHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string][]rule.HealthSnapshot
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(resp["history"]) != 0 {
		t.Errorf("expected empty history, got %d entries", len(resp["history"]))
	}
}

func TestHandleReportCompliance(t *testing.T) {
	r, artistSvc := testRouter(t)

	addTestArtist(t, artistSvc, "Compliant Artist")
	a := addTestArtist(t, artistSvc, "Non-Compliant Artist")
	a.NFOExists = true
	a.ThumbExists = true
	if err := artistSvc.Update(context.Background(), a); err != nil {
		t.Fatalf("updating artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/compliance?page=1&page_size=10", nil)
	w := httptest.NewRecorder()

	r.handleReportCompliance(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	total, ok := resp["total"].(float64)
	if !ok {
		t.Fatal("missing total field")
	}
	if int(total) != 2 {
		t.Errorf("total = %d, want 2", int(total))
	}
}

func TestSanitizeCSV(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"plain text", "hello", "hello"},
		{"equals prefix", "=1+1", "'=1+1"},
		{"plus prefix", "+1", "'+1"},
		{"minus prefix", "-1", "'-1"},
		{"at prefix", "@SUM(A1)", "'@SUM(A1)"},
		{"tab then equals", "\t=1+1", "'\t=1+1"},
		{"space then plus", " +cmd", "' +cmd"},
		{"spaces then at", "   @evil", "'   @evil"},
		{"tab space equals", "\t =calc", "'\t =calc"},
		{"whitespace only", "   ", "   "},
		{"safe after whitespace", " hello", " hello"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeCSV(tc.input)
			if got != tc.want {
				t.Errorf("sanitizeCSV(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestBuildHealthSummary(t *testing.T) {
	artists := []artist.Artist{
		{Name: "A", NFOExists: true, ThumbExists: true, FanartExists: true, MusicBrainzID: "id1"},
		{Name: "B", NFOExists: false, ThumbExists: false, FanartExists: false, MusicBrainzID: ""},
	}
	results := []rule.EvaluationResult{
		{HealthScore: 100, RulesPassed: 5, RulesTotal: 5},
		{HealthScore: 40, RulesPassed: 2, RulesTotal: 5, Violations: []rule.Violation{
			{RuleID: "nfo_exists", RuleName: "NFO Exists", Severity: "error"},
			{RuleID: "thumb_exists", RuleName: "Thumb Exists", Severity: "warning"},
		}},
	}

	s := buildHealthSummary(artists, results)

	if s.TotalArtists != 2 {
		t.Errorf("TotalArtists = %d, want 2", s.TotalArtists)
	}
	if s.CompliantArtists != 1 {
		t.Errorf("CompliantArtists = %d, want 1", s.CompliantArtists)
	}
	if s.MissingNFO != 1 {
		t.Errorf("MissingNFO = %d, want 1", s.MissingNFO)
	}
	if s.MissingThumb != 1 {
		t.Errorf("MissingThumb = %d, want 1", s.MissingThumb)
	}
	if s.MissingMBID != 1 {
		t.Errorf("MissingMBID = %d, want 1", s.MissingMBID)
	}
	if len(s.TopViolations) != 2 {
		t.Errorf("TopViolations count = %d, want 2", len(s.TopViolations))
	}
	if s.Score != 70.0 {
		t.Errorf("Score = %.1f, want 70.0", s.Score)
	}
}
