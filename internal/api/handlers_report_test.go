package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/library"
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

// TestHandleReportHealth_StoredScoresReflectNewArtists verifies that adding
// artists changes the health endpoint response because it reads stored scores
// from the database, not from a cache.
func TestHandleReportHealth_StoredScoresReflectNewArtists(t *testing.T) {
	r, artistSvc := testRouter(t)

	addTestArtist(t, artistSvc, "Artist A")

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

	// Add a second artist and re-query. Since the handler reads stored
	// scores from SQL, it should immediately reflect the new artist.
	addTestArtist(t, artistSvc, "Artist B")

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
		t.Errorf("second response: TotalArtists = %d, want 2", resp2.TotalArtists)
	}
}

func TestHandleReportHealthByLibrary(t *testing.T) {
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
	libSvc := library.NewService(db)
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
		LibraryService:     libSvc,
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

	ctx := context.Background()

	// Create two libraries with real temp directories
	dir1 := t.TempDir()
	lib1 := &library.Library{Name: "Rock", Path: dir1, Type: "regular", Source: "manual"}
	if err := libSvc.Create(ctx, lib1); err != nil {
		t.Fatalf("creating library 1: %v", err)
	}
	dir2 := t.TempDir()
	lib2 := &library.Library{Name: "Jazz", Path: dir2, Type: "regular", Source: "manual"}
	if err := libSvc.Create(ctx, lib2); err != nil {
		t.Fatalf("creating library 2: %v", err)
	}

	// Add artists to each library
	a1 := &artist.Artist{Name: "Rock Artist", SortName: "Rock Artist", Path: "/music/rock/artist1", LibraryID: lib1.ID, HealthScore: 90.0}
	if err := artistSvc.Create(ctx, a1); err != nil {
		t.Fatalf("creating rock artist: %v", err)
	}
	a2 := &artist.Artist{Name: "Jazz Artist", SortName: "Jazz Artist", Path: "/music/jazz/artist1", LibraryID: lib2.ID, HealthScore: 60.0}
	if err := artistSvc.Create(ctx, a2); err != nil {
		t.Fatalf("creating jazz artist: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health/by-library", nil)
	w := httptest.NewRecorder()
	r.handleReportHealthByLibrary(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Libraries []librarySummary `json:"libraries"`
		Overall   librarySummary   `json:"overall"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(resp.Libraries) != 2 {
		t.Fatalf("len(libraries) = %d, want 2", len(resp.Libraries))
	}

	if resp.Overall.TotalArtists != 2 {
		t.Errorf("overall TotalArtists = %d, want 2", resp.Overall.TotalArtists)
	}

	// Verify each library has exactly 1 artist
	for _, lib := range resp.Libraries {
		if lib.TotalArtists != 1 {
			t.Errorf("library %q TotalArtists = %d, want 1", lib.LibraryName, lib.TotalArtists)
		}
	}
}

func TestHandleReportRules_Empty(t *testing.T) {
r, _ := testRouter(t)

req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/rules", nil)
w := httptest.NewRecorder()

r.handleReportRules(w, req)

if w.Code != http.StatusOK {
t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
}

var resp struct {
Rules []rule.RuleStats `json:"rules"`
}
if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
t.Fatalf("decoding response: %v", err)
}
if resp.Rules == nil {
t.Error("rules field must not be nil (empty array expected)")
}
if len(resp.Rules) != 0 {
t.Errorf("expected 0 rule stats when no evaluations have run, got %d", len(resp.Rules))
}
}

func TestHandleReportRules_WithData(t *testing.T) {
r, artistSvc := testRouter(t)
ctx := context.Background()

a := addTestArtist(t, artistSvc, "Stats Artist")

// Insert a rule_result directly for deterministic test setup.
db := r.db
_, err := db.ExecContext(ctx, `
INSERT INTO rule_results (artist_id, rule_id, passed, evaluated_at)
SELECT ?, id, 1, datetime('now') FROM rules LIMIT 1
`, a.ID)
if err != nil {
t.Fatalf("inserting rule_result: %v", err)
}

req2 := httptest.NewRequest(http.MethodGet, "/api/v1/reports/rules", nil)
w2 := httptest.NewRecorder()
r.handleReportRules(w2, req2)

if w2.Code != http.StatusOK {
t.Fatalf("status = %d, want %d", w2.Code, http.StatusOK)
}

var resp struct {
Rules []rule.RuleStats `json:"rules"`
}
if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
t.Fatalf("decoding response: %v", err)
}
if len(resp.Rules) == 0 {
t.Error("expected at least 1 rule stat row after inserting a result")
}
for _, rs := range resp.Rules {
if rs.TotalArtists <= 0 {
t.Errorf("rule %s: total_artists = %d, want > 0", rs.RuleID, rs.TotalArtists)
}
if rs.PassCount+rs.FailCount != rs.TotalArtists {
t.Errorf("rule %s: pass_count(%d) + fail_count(%d) != total_artists(%d)",
rs.RuleID, rs.PassCount, rs.FailCount, rs.TotalArtists)
}
}
}
