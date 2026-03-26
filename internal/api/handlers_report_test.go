package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/publish"
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
	nfoSnapSvc := nfo.NewSnapshotService(db)
	providerSettings := provider.NewSettingsService(db, nil)

	pub := publish.New(publish.Deps{
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		NFOSnapshotService: nfoSnapSvc,
		Logger:             logger,
	})

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		RuleService:        ruleSvc,
		RuleEngine:         ruleEngine,
		NFOSnapshotService: nfoSnapSvc,
		ProviderSettings:   providerSettings,
		DB:                 db,
		Logger:             logger,
		StaticDir:          "../../web/static",
		Publisher:          pub,
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

func TestHandleViolationTrend_DefaultRange(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/violations/trend", nil)
	w := httptest.NewRecorder()

	r.handleViolationTrend(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	trend, ok := resp["trend"].([]any)
	if !ok {
		t.Fatal("missing or invalid trend field")
	}
	if len(trend) != 30 {
		t.Errorf("trend length = %d, want 30 (default 30 days)", len(trend))
	}
}

func TestHandleViolationTrend_CustomRange(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/violations/trend?days=7", nil)
	w := httptest.NewRecorder()

	r.handleViolationTrend(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	trend, ok := resp["trend"].([]any)
	if !ok {
		t.Fatal("missing or invalid trend field")
	}
	if len(trend) != 7 {
		t.Errorf("trend length = %d, want 7", len(trend))
	}
}

func TestHandleViolationTrend_PointShape(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/violations/trend?days=1", nil)
	w := httptest.NewRecorder()

	r.handleViolationTrend(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	trend, ok := resp["trend"].([]any)
	if !ok || len(trend) == 0 {
		t.Fatal("expected at least one trend point")
	}

	pt, ok := trend[0].(map[string]any)
	if !ok {
		t.Fatal("expected trend point to be an object")
	}

	// Validate date field exists and has YYYY-MM-DD format.
	dateVal, ok := pt["date"]
	if !ok {
		t.Error("trend point missing 'date' field")
	} else if dateStr, ok := dateVal.(string); !ok {
		t.Errorf("trend point 'date' is %T, want string", dateVal)
	} else if _, err := time.Parse("2006-01-02", dateStr); err != nil {
		t.Errorf("trend point 'date' = %q, not valid YYYY-MM-DD: %v", dateStr, err)
	}

	// Validate created field exists and is a number.
	createdVal, ok := pt["created"]
	if !ok {
		t.Error("trend point missing 'created' field")
	} else if _, ok := createdVal.(float64); !ok {
		t.Errorf("trend point 'created' is %T, want float64", createdVal)
	}

	// Validate resolved field exists and is a number.
	resolvedVal, ok := pt["resolved"]
	if !ok {
		t.Error("trend point missing 'resolved' field")
	} else if _, ok := resolvedVal.(float64); !ok {
		t.Errorf("trend point 'resolved' is %T, want float64", resolvedVal)
	}
}

func TestHandleViolationTrend_InvalidDaysClamped(t *testing.T) {
	r, _ := testRouter(t)

	// days=0 should be clamped to default (30)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/violations/trend?days=0", nil)
	w := httptest.NewRecorder()

	r.handleViolationTrend(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	trend, ok := resp["trend"].([]any)
	if !ok {
		t.Fatal("missing trend field")
	}
	if len(trend) != 30 {
		t.Errorf("trend length = %d, want 30 (clamped from 0)", len(trend))
	}
}

func TestHandleReportMetadataCompleteness_Empty(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/metadata-completeness", nil)
	w := httptest.NewRecorder()

	r.handleReportMetadataCompleteness(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if _, ok := resp["overall_score"]; !ok {
		t.Error("response missing overall_score field")
	}
	if _, ok := resp["total_artists"]; !ok {
		t.Error("response missing total_artists field")
	}
	if _, ok := resp["field_coverage"]; !ok {
		t.Error("response missing field_coverage field")
	}
	if _, ok := resp["lowest_completeness"]; !ok {
		t.Error("response missing lowest_completeness field")
	}

	total, ok := resp["total_artists"].(float64)
	if !ok {
		t.Fatal("total_artists is not a number")
	}
	if int(total) != 0 {
		t.Errorf("total_artists = %d, want 0", int(total))
	}
}

func TestHandleReportMetadataCompleteness_WithArtists(t *testing.T) {
	r, artistSvc := testRouter(t)

	// Add two artists: one with biography and NFO, one without.
	a1 := addTestArtist(t, artistSvc, "Full Artist")
	a1.Biography = "Some biography text"
	a1.NFOExists = true
	if err := artistSvc.Update(context.Background(), a1); err != nil {
		t.Fatalf("updating artist: %v", err)
	}
	addTestArtist(t, artistSvc, "Empty Artist")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/metadata-completeness", nil)
	w := httptest.NewRecorder()

	r.handleReportMetadataCompleteness(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	total, ok := resp["total_artists"].(float64)
	if !ok {
		t.Fatal("total_artists is not a number")
	}
	if int(total) != 2 {
		t.Errorf("total_artists = %d, want 2", int(total))
	}

	overallScore, ok := resp["overall_score"].(float64)
	if !ok {
		t.Fatal("overall_score is not a number")
	}
	if overallScore < 0 || overallScore > 100 {
		t.Errorf("overall_score = %.1f, want between 0 and 100", overallScore)
	}

	fieldCoverage, ok := resp["field_coverage"].([]any)
	if !ok {
		t.Fatal("field_coverage is not an array")
	}
	if len(fieldCoverage) == 0 {
		t.Error("field_coverage is empty, want at least one entry")
	}

	// Verify field coverage entry shape.
	first, ok := fieldCoverage[0].(map[string]any)
	if !ok {
		t.Fatal("field_coverage[0] is not an object")
	}
	for _, key := range []string{"field", "count", "total", "percentage"} {
		if _, ok := first[key]; !ok {
			t.Errorf("field_coverage[0] missing key %q", key)
		}
	}

	lowestCompleteness, ok := resp["lowest_completeness"].([]any)
	if !ok {
		t.Fatal("lowest_completeness is not an array")
	}
	if len(lowestCompleteness) == 0 {
		t.Error("lowest_completeness is empty, want at least one entry")
	}
}

func TestHandleReportMetadataCompleteness_ExcludedArtistsOmitted(t *testing.T) {
	r, artistSvc := testRouter(t)

	// Regular artist.
	addTestArtist(t, artistSvc, "Normal Artist")

	// Excluded artist -- should not appear in the completeness count.
	excluded := addTestArtist(t, artistSvc, "Various Artists")
	excluded.IsExcluded = true
	excluded.ExclusionReason = "default exclusion list"
	if err := artistSvc.Update(context.Background(), excluded); err != nil {
		t.Fatalf("updating excluded artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/metadata-completeness", nil)
	w := httptest.NewRecorder()

	r.handleReportMetadataCompleteness(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	total, ok := resp["total_artists"].(float64)
	if !ok {
		t.Fatal("total_artists is not a number")
	}
	// Only the non-excluded artist should be counted.
	if int(total) != 1 {
		t.Errorf("total_artists = %d, want 1 (excluded artist should be omitted)", int(total))
	}
}

func TestHandleReportMetadataCompleteness_HTMX(t *testing.T) {
	r, artistSvc := testRouter(t)
	addTestArtist(t, artistSvc, "HTMX Artist")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/metadata-completeness", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleReportMetadataCompleteness(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestHandleViolationTrend_UpperBoundClamped(t *testing.T) {
	r, _ := testRouter(t)

	// days=366 exceeds the 365 maximum and should be clamped to 30 (default).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/violations/trend?days=366", nil)
	w := httptest.NewRecorder()

	r.handleViolationTrend(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	trend, ok := resp["trend"].([]any)
	if !ok {
		t.Fatal("missing trend field")
	}
	if len(trend) != 30 {
		t.Errorf("trend length = %d, want 30 (clamped from 366)", len(trend))
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

// TestHandleReportHealth_CacheHit verifies that two rapid requests return the
// same cached data without re-running the expensive EvaluateAll computation.
// The second request should hit the cache (response within the 30s TTL window).
func TestHandleReportHealth_CacheHit(t *testing.T) {
	r, artistSvc := testRouter(t)

	addTestArtist(t, artistSvc, "Cache Artist A")
	addTestArtist(t, artistSvc, "Cache Artist B")

	// First request: populates the cache.
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health", nil)
	w1 := httptest.NewRecorder()
	r.handleReportHealth(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want %d", w1.Code, http.StatusOK)
	}

	var resp1 healthSummary
	if err := json.NewDecoder(w1.Body).Decode(&resp1); err != nil {
		t.Fatalf("decoding first response: %v", err)
	}

	// Second request: should return the cached result with the same data.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health", nil)
	w2 := httptest.NewRecorder()
	r.handleReportHealth(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("second request: status = %d, want %d", w2.Code, http.StatusOK)
	}

	var resp2 healthSummary
	if err := json.NewDecoder(w2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decoding second response: %v", err)
	}

	// Both responses should have identical data because the second one
	// was served from the cache.
	if resp1.TotalArtists != resp2.TotalArtists {
		t.Errorf("TotalArtists mismatch: first=%d, second=%d", resp1.TotalArtists, resp2.TotalArtists)
	}
	if resp1.Score != resp2.Score {
		t.Errorf("Score mismatch: first=%.1f, second=%.1f", resp1.Score, resp2.Score)
	}
	if resp1.CompliantArtists != resp2.CompliantArtists {
		t.Errorf("CompliantArtists mismatch: first=%d, second=%d", resp1.CompliantArtists, resp2.CompliantArtists)
	}

	// Verify the cache is actually populated by checking the internal state.
	r.healthCacheMu.RLock()
	defer r.healthCacheMu.RUnlock()
	if r.healthResult == nil {
		t.Error("healthResult is nil after two requests; cache should be populated")
	}
	if r.healthCachedAt.IsZero() {
		t.Error("healthCachedAt is zero; cache timestamp should be set")
	}
}

// TestHandleReportHealth_CacheInvalidation verifies that calling
// InvalidateHealthCache clears the cached result, forcing the next
// request to recompute from scratch.
func TestHandleReportHealth_CacheInvalidation(t *testing.T) {
	r, artistSvc := testRouter(t)

	addTestArtist(t, artistSvc, "Invalidation Artist")

	// First request: populates the cache.
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health", nil)
	w1 := httptest.NewRecorder()
	r.handleReportHealth(w1, req1)

	if w1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want %d", w1.Code, http.StatusOK)
	}

	var resp1 healthSummary
	if err := json.NewDecoder(w1.Body).Decode(&resp1); err != nil {
		t.Fatalf("decoding first response: %v", err)
	}
	if resp1.TotalArtists != 1 {
		t.Fatalf("first response: TotalArtists = %d, want 1", resp1.TotalArtists)
	}

	// Add another artist and invalidate the cache.
	addTestArtist(t, artistSvc, "New Artist After Invalidation")
	r.InvalidateHealthCache()

	// Verify the cache was actually cleared.
	r.healthCacheMu.RLock()
	cacheCleared := r.healthResult == nil
	r.healthCacheMu.RUnlock()
	if !cacheCleared {
		t.Fatal("cache should be nil after InvalidateHealthCache")
	}

	// Second request: should recompute and include the new artist.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health", nil)
	w2 := httptest.NewRecorder()
	r.handleReportHealth(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("second request: status = %d, want %d", w2.Code, http.StatusOK)
	}

	var resp2 healthSummary
	if err := json.NewDecoder(w2.Body).Decode(&resp2); err != nil {
		t.Fatalf("decoding second response: %v", err)
	}

	if resp2.TotalArtists != 2 {
		t.Errorf("after invalidation: TotalArtists = %d, want 2", resp2.TotalArtists)
	}
}

// TestHandleReportHealth_Singleflight verifies that concurrent requests
// are coalesced so all goroutines receive the same result. This test
// launches multiple goroutines that call the handler simultaneously and
// checks that all responses are identical (proving they shared a single
// computation rather than each running their own).
func TestHandleReportHealth_Singleflight(t *testing.T) {
	r, artistSvc := testRouter(t)

	addTestArtist(t, artistSvc, "Singleflight Artist A")
	addTestArtist(t, artistSvc, "Singleflight Artist B")
	addTestArtist(t, artistSvc, "Singleflight Artist C")

	// Invalidate any pre-existing cache to force a fresh computation.
	r.InvalidateHealthCache()

	const concurrency = 10
	type result struct {
		status    int
		body      healthSummary
		decodeErr error
	}

	// results is a pre-allocated slice (one slot per goroutine) so there
	// are no concurrent writes to the same index -- each goroutine writes
	// only to its own slot.
	results := make([]result, concurrency)
	var wg sync.WaitGroup
	wg.Add(concurrency)

	// Use a channel as a starting gate so all goroutines begin at roughly
	// the same time, maximizing the chance that singleflight coalesces them.
	start := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start // Wait for the starting gate.

			req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health", nil)
			w := httptest.NewRecorder()
			r.handleReportHealth(w, req)

			results[idx].status = w.Code
			if w.Code == http.StatusOK {
				if err := json.NewDecoder(w.Body).Decode(&results[idx].body); err != nil {
					results[idx].decodeErr = err
				}
			}
		}(i)
	}

	// Open the starting gate so all goroutines fire simultaneously.
	close(start)
	wg.Wait()

	// All requests should succeed and return the same data.
	for i := 0; i < concurrency; i++ {
		if results[i].status != http.StatusOK {
			t.Errorf("goroutine %d: status = %d, want %d", i, results[i].status, http.StatusOK)
			continue
		}
		if results[i].decodeErr != nil {
			t.Errorf("goroutine %d: decode error: %v", i, results[i].decodeErr)
			continue
		}
		if results[i].body.TotalArtists != 3 {
			t.Errorf("goroutine %d: TotalArtists = %d, want 3", i, results[i].body.TotalArtists)
		}
		if results[i].body.Score != results[0].body.Score {
			t.Errorf("goroutine %d: Score = %.1f, want %.1f (same as goroutine 0)",
				i, results[i].body.Score, results[0].body.Score)
		}
	}
}
