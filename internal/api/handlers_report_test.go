package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/internal/rule"
)

// testRouter creates a minimal Router for handler tests with an in-memory DB.
func testRouter(t *testing.T) (*Router, *artist.Service) {
	t.Helper()

	db := newTestDB(t)

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
	// Match production wiring so tests exercise the rename->platform-sync
	// hook on Service.RenameDirectory. Tests without platform mappings
	// see an empty platforms slice, so this is safe to enable by default.
	artistSvc.SetPlatformRenameSyncer(pub)

	i18nBundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatalf("loading i18n bundle: %v", err)
	}

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		ArtistService:      artistSvc,
		ConnectionService:  connSvc,
		RuleService:        ruleSvc,
		RuleEngine:         ruleEngine,
		NFOSnapshotService: nfoSnapSvc,
		ProviderSettings:   providerSettings,
		I18nBundle:         i18nBundle,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
		Publisher:          pub,
	})

	// Override the auto-wired conflict detector with the no-op NewForTest
	// variant so handler tests that create connection fixtures (without
	// standing up real peer stubs) do not trip the fail-closed CheckErr
	// contract in ledger.AnyImageConflict / AnyNFOConflict. Tests that
	// exercise the gate itself build their own router.
	r.conflictDetector = conflict.NewForTest(connSvc, logger)
	r.conflictGate = conflict.NewGate(r.conflictDetector)

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestHandleReportCompliance_IncludesRulesPassedCount covers the #699 slice 1
// addition: each compliance row now carries rules_passed_count and
// rules_evaluated_count, sourced from the rule_results table. Rows for
// artists with no stored outcomes should default to zero so the field is
// always present in the response (stable for clients that rely on its
// existence).
func TestHandleReportCompliance_IncludesRulesPassedCount(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	ctx := context.Background()

	passing := addTestArtist(t, artistSvc, "Passing Artist")
	partial := addTestArtist(t, artistSvc, "Partial Artist")
	// Seed rule_results rows directly: passing artist has 3 pass rows,
	// partial artist has 2 pass + 1 fail.
	now := time.Now().UTC()
	for _, rid := range []string{rule.RuleNFOExists, rule.RuleThumbExists, rule.RuleFanartExists} {
		if err := r.ruleService.UpsertRuleResultPass(ctx, passing.ID, rid, now); err != nil {
			t.Fatalf("seeding pass %s: %v", rid, err)
		}
	}
	if err := r.ruleService.UpsertRuleResultPass(ctx, partial.ID, rule.RuleNFOExists, now); err != nil {
		t.Fatalf("seeding partial pass 1: %v", err)
	}
	if err := r.ruleService.UpsertRuleResultPass(ctx, partial.ID, rule.RuleThumbExists, now); err != nil {
		t.Fatalf("seeding partial pass 2: %v", err)
	}
	// Simulate a fail by inserting directly (skips the transactional pair
	// that would also write a violation; this keeps the test focused on
	// the compliance handler aggregating rule_results correctly).
	if _, err := testRuleResultsDBFromRouter(r).ExecContext(ctx, `
		INSERT INTO rule_results (artist_id, rule_id, passed, evaluated_at, first_failed_at)
		VALUES (?, ?, 0, ?, ?)`,
		partial.ID, rule.RuleFanartExists,
		now.Format("2006-01-02T15:04:05Z"),
		now.Format("2006-01-02T15:04:05Z")); err != nil {
		t.Fatalf("seeding partial fail: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/compliance?page=1&page_size=10", nil)
	w := httptest.NewRecorder()
	r.handleReportCompliance(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Rows []struct {
			Artist struct {
				ID string `json:"id"`
			} `json:"artist"`
			RulesPassedCount    int `json:"rules_passed_count"`
			RulesEvaluatedCount int `json:"rules_evaluated_count"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(resp.Rows))
	}

	byID := make(map[string]struct{ passed, evaluated int }, len(resp.Rows))
	for _, row := range resp.Rows {
		byID[row.Artist.ID] = struct{ passed, evaluated int }{row.RulesPassedCount, row.RulesEvaluatedCount}
	}
	if got := byID[passing.ID]; got.passed != 3 || got.evaluated != 3 {
		t.Errorf("passing artist counts = %+v, want {passed:3, evaluated:3}", got)
	}
	if got := byID[partial.ID]; got.passed != 2 || got.evaluated != 3 {
		t.Errorf("partial artist counts = %+v, want {passed:2, evaluated:3}", got)
	}
}

// testRuleResultsDBFromRouter pulls the *sql.DB out of the Router's
// RuleService for raw inserts. Kept here so the backdoor stays local to
// this test rather than exporting an accessor from the package.
func testRuleResultsDBFromRouter(r *Router) interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
} {
	return r.db
}

func TestSanitizeCSV(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	} else if _, err := time.Parse(time.DateOnly, dateStr); err != nil {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// seedHealthSnapshot inserts a row directly into health_history with an
// explicit recorded_at timestamp. The Service.RecordHealthSnapshot API uses
// time.Now() and a 5-minute throttle, neither of which is suitable for testing
// time-range filtering. Direct DB insert is the only way to plant snapshots
// at arbitrary historical instants.
func seedHealthSnapshot(t *testing.T, db *sql.DB, id string, recordedAt time.Time, score float64) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO health_history (id, total_artists, compliant_artists, score, recorded_at) VALUES (?, ?, ?, ?, ?)`,
		id, 100, int(score), score, recordedAt.UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("seeding health snapshot %s: %v", id, err)
	}
}

// TestHandleReportHealthHistory_DateOnlyParams verifies that date-only query
// parameters (time.DateOnly format) drive correct from/to filtering, including
// the inclusive end-of-day contract: "to=2026-12-31" must include any snapshot
// recorded during 2026-12-31 (right up to 23:59:59Z) and exclude snapshots at
// 2027-01-01T00:00:00Z and later. Seeds snapshots before, within, at the
// boundary, and after the queried range.
func TestHandleReportHealthHistory_DateOnlyParams(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	// Seeded snapshots: 2 outside range (50, 80), 3 inside (60, 70, 75 at
	// end-of-day). Query window: 2026-01-01 to 2026-12-31 (date-only).
	seedHealthSnapshot(t, r.db, "before", time.Date(2025, 12, 15, 0, 0, 0, 0, time.UTC), 50.0)
	seedHealthSnapshot(t, r.db, "in-1", time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), 60.0)
	seedHealthSnapshot(t, r.db, "in-2", time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), 70.0)
	seedHealthSnapshot(t, r.db, "end-of-day", time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), 75.0)
	seedHealthSnapshot(t, r.db, "after", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 80.0)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/health/history?from=2026-01-01&to=2026-12-31", nil)
	w := httptest.NewRecorder()

	r.handleReportHealthHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string][]rule.HealthSnapshot
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	history, ok := resp["history"]
	if !ok {
		t.Fatal("response missing 'history' key")
	}
	if len(history) != 3 {
		t.Fatalf("history length = %d, want 3 (only in-range snapshots); got: %+v", len(history), history)
	}
	// Service returns ascending by recorded_at: in-1 (March), in-2 (September), end-of-day (Dec 31 23:59:59).
	if history[0].Score != 60.0 {
		t.Errorf("history[0].Score = %v, want 60.0 (in-1)", history[0].Score)
	}
	if history[1].Score != 70.0 {
		t.Errorf("history[1].Score = %v, want 70.0 (in-2)", history[1].Score)
	}
	if history[2].Score != 75.0 {
		t.Errorf("history[2].Score = %v, want 75.0 (end-of-day)", history[2].Score)
	}
}

// TestHandleReportHealthHistory_RFC3339Params verifies the primary RFC3339
// parse branch with the same filtering-semantics assertions as the date-only
// test. Same seeded data, equivalent query window expressed as full RFC3339
// timestamps -- both code paths must enforce the same inclusive day-range
// contract (end-of-day snapshot included, start-of-next-day snapshot excluded).
func TestHandleReportHealthHistory_RFC3339Params(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	seedHealthSnapshot(t, r.db, "before", time.Date(2025, 12, 15, 0, 0, 0, 0, time.UTC), 50.0)
	seedHealthSnapshot(t, r.db, "in-1", time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), 60.0)
	seedHealthSnapshot(t, r.db, "in-2", time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), 70.0)
	seedHealthSnapshot(t, r.db, "end-of-day", time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), 75.0)
	seedHealthSnapshot(t, r.db, "after", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 80.0)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/reports/health/history?from=2026-01-01T00:00:00Z&to=2026-12-31T23:59:59Z", nil)
	w := httptest.NewRecorder()

	r.handleReportHealthHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string][]rule.HealthSnapshot
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	history, ok := resp["history"]
	if !ok {
		t.Fatal("response missing 'history' key")
	}
	if len(history) != 3 {
		t.Fatalf("history length = %d, want 3 (only in-range snapshots); got: %+v", len(history), history)
	}
	if history[0].Score != 60.0 {
		t.Errorf("history[0].Score = %v, want 60.0 (in-1)", history[0].Score)
	}
	if history[1].Score != 70.0 {
		t.Errorf("history[1].Score = %v, want 70.0 (in-2)", history[1].Score)
	}
	if history[2].Score != 75.0 {
		t.Errorf("history[2].Score = %v, want 75.0 (end-of-day)", history[2].Score)
	}
}

// TestHandleReportHealth_StoredScoresReflectNewArtists verifies that adding
// artists changes the health endpoint response because it reads stored scores
// from the database, not from a cache.
func TestHandleReportHealth_StoredScoresReflectNewArtists(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	db := newTestDB(t)

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
		StaticFS:           os.DirFS("../../web/static"),
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

// TestHandleCompliancePage_HXPushURL verifies that an HTMX request to the
// compliance HTML page emits HX-Push-Url carrying every active filter param
// so the address bar reflects the post-swap state. The header is the
// load-bearing piece for shareable filtered URLs, so each canonical key
// must round-trip without renaming or dropping.
func TestHandleCompliancePage_HXPushURL(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(
		http.MethodGet,
		"/reports/compliance?status=non_compliant&filter=missing_nfo&library_id=lib-1&health_min=40&health_max=80",
		nil,
	)
	req.Header.Set("HX-Request", "true")
	req = withTestUser(req)
	w := httptest.NewRecorder()

	r.handleCompliancePage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	push := w.Header().Get("HX-Push-Url")
	if push == "" {
		t.Fatalf("expected HX-Push-Url on HTMX request")
	}
	// Every active filter must appear in the pushed URL so the address bar
	// can be copy-pasted and re-loaded.
	wantSubs := []string{
		"status=non_compliant",
		"filter=missing_nfo",
		"library_id=lib-1",
		"health_min=40",
		"health_max=80",
	}
	for _, s := range wantSubs {
		if !strings.Contains(push, s) {
			t.Errorf("HX-Push-Url missing %q; got %q", s, push)
		}
	}
}

// TestComplianceURLValues verifies the per-param URL-encoding behavior:
// empty / default values are dropped, non-defaults are written, and the
// status `all` synonym is treated as a no-op (matches the rest of the page).
func TestComplianceURLValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		params   artist.ListParams
		status   string
		filter   string
		wantKeys map[string]string
	}{
		{
			name:     "all defaults",
			params:   artist.ListParams{},
			status:   "",
			filter:   "",
			wantKeys: map[string]string{},
		},
		{
			name:   "status all is treated as default",
			params: artist.ListParams{},
			status: "all",
			filter: "",
			// `all` means "no filter" so we must not echo it.
			wantKeys: map[string]string{},
		},
		{
			name:   "full set",
			params: artist.ListParams{Search: "indie", LibraryID: "lib-1", HealthScoreMin: 40, HealthScoreMax: 80, Sort: "health_score", Order: "desc"},
			status: "non_compliant",
			filter: "missing_nfo",
			wantKeys: map[string]string{
				"search":     "indie",
				"status":     "non_compliant",
				"filter":     "missing_nfo",
				"library_id": "lib-1",
				"health_min": "40",
				"health_max": "80",
				"sort":       "health_score",
				"order":      "desc",
			},
		},
		{
			name:     "sort=name and order=asc are default and dropped",
			params:   artist.ListParams{Sort: "name", Order: "asc"},
			status:   "",
			filter:   "",
			wantKeys: map[string]string{},
		},
		{
			// Regression for CR finding on PR #1653: pagination must survive
			// HTMX swaps so the address bar reflects the current page when a
			// chip is dismissed mid-listing.
			name:   "non-default pagination is preserved",
			params: artist.ListParams{Page: 3, PageSize: 100},
			status: "",
			filter: "",
			wantKeys: map[string]string{
				"page":      "3",
				"page_size": "100",
			},
		},
		{
			name:     "page=1 and default page_size are dropped",
			params:   artist.ListParams{Page: 1, PageSize: compliancePageSizeDefault},
			status:   "",
			filter:   "",
			wantKeys: map[string]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := complianceURLValues(tc.params, tc.status, tc.filter)
			if len(got) != len(tc.wantKeys) {
				t.Errorf("len = %d, want %d (got=%v)", len(got), len(tc.wantKeys), got)
			}
			for k, v := range tc.wantKeys {
				if g := got.Get(k); g != v {
					t.Errorf("key %q = %q, want %q", k, g, v)
				}
			}
		})
	}
}
