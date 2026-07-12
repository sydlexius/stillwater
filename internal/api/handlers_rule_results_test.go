package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/rule"
)

// mustSeedAPI fails the test immediately if a test-fixture
// UpsertRuleResult* call returns an error. Same intent as
// mustSeed in internal/rule/sqlite_rule_results_slice2_test.go;
// duplicated here because helpers do not cross packages.
func mustSeedAPI(t *testing.T, step string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", step, err)
	}
}

func seedArtistRow(t *testing.T, r *Router, id, name string) {
	t.Helper()
	// sort_name is included because artist.Service.GetByID scans it as a
	// non-nullable string; tests that route through the service (e.g.
	// /api/v1/artists/{id}/rule-results) would otherwise hit a NULL scan
	// error. Other Task 5 tests only query via repository methods that
	// do not touch sort_name, so this change is backward compatible.
	if _, err := r.db.ExecContext(context.Background(),
		`INSERT INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, '')`, id, name, name); err != nil {
		t.Fatalf("inserting artist %s: %v", id, err)
	}
}

func seedRuleRow(t *testing.T, r *Router, id, name string) {
	t.Helper()
	if _, err := r.db.ExecContext(context.Background(), `
		INSERT INTO rules (id, name, description, category, enabled, automation_mode, config)
		VALUES (?, ?, 'desc', 'nfo', 1, 'auto', '{}')
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, enabled = 1`,
		id, name); err != nil {
		t.Fatalf("inserting rule %s: %v", id, err)
	}
}

// TestHandleRuleResults_ReturnsPaginatedRows seeds three artists evaluated
// against one rule and confirms the handler honors page/page_size and
// emits the {rows, total, page, page_size} envelope, matching the
// /reports/compliance precedent.
func TestHandleRuleResults_ReturnsPaginatedRows(t *testing.T) {
	r, _ := testRouter(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedArtistRow(t, r, "p-a", "Alpha")
	seedArtistRow(t, r, "p-b", "Bravo")
	seedArtistRow(t, r, "p-c", "Charlie")
	seedRuleRow(t, r, "rule-d", "Drill Rule")

	if err := r.ruleService.UpsertRuleResultPass(ctx, "p-a", "rule-d", now); err != nil {
		t.Fatalf("seed pass: %v", err)
	}
	if err := r.ruleService.UpsertRuleResultFail(ctx, "p-b", "rule-d", "vb", "msg-b", now); err != nil {
		t.Fatalf("seed fail b: %v", err)
	}
	if err := r.ruleService.UpsertRuleResultFail(ctx, "p-c", "rule-d", "vc", "msg-c", now); err != nil {
		t.Fatalf("seed fail c: %v", err)
	}

	// page_size=10 is the minimum accepted value; use it to get exactly 2 of the
	// 3 rows back (page=1 returns the first 2 but we only have 3 total so page=2
	// is needed to see the third). Use page_size=10 and assert on total/page.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/rules/rule-d/results?page=1&page_size=10", nil)
	req.SetPathValue("id", "rule-d")
	w := httptest.NewRecorder()
	r.handleRuleResults(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Rows     []rule.RuleResultWithArtist `json:"rows"`
		Total    int                         `json:"total"`
		Page     int                         `json:"page"`
		PageSize int                         `json:"page_size"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding: %v\nbody=%s", err, w.Body.String())
	}
	if env.Total != 3 {
		t.Errorf("total: got %d, want 3", env.Total)
	}
	if env.Page != 1 || env.PageSize != 10 {
		t.Errorf("page/size: got %d/%d, want 1/10", env.Page, env.PageSize)
	}
	// All 3 rows fit within page_size=10, so we expect all three in order.
	if len(env.Rows) != 3 {
		t.Fatalf("rows len: got %d, want 3", len(env.Rows))
	}
	if env.Rows[0].ArtistID != "p-a" || env.Rows[1].ArtistID != "p-b" {
		t.Errorf("ordering: got [%s %s], want [p-a p-b]", env.Rows[0].ArtistID, env.Rows[1].ArtistID)
	}
}

// TestHandleRuleResults_PassedFilter exercises the passed= query
// parameter; "passed" -> only Passed=true rows; "failed" -> only
// Passed=false rows; absent/empty/any -> both.
func TestHandleRuleResults_PassedFilter(t *testing.T) {
	r, _ := testRouter(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedArtistRow(t, r, "f-a", "Alpha")
	seedArtistRow(t, r, "f-b", "Bravo")
	seedRuleRow(t, r, "rule-f", "Filter Rule")
	mustSeedAPI(t, "seed pass f-a rule-f", r.ruleService.UpsertRuleResultPass(ctx, "f-a", "rule-f", now))
	mustSeedAPI(t, "seed fail f-b rule-f", r.ruleService.UpsertRuleResultFail(ctx, "f-b", "rule-f", "vfb", "msg", now))

	cases := []struct {
		query string
		want  int
	}{
		{"passed=passed", 1},
		{"passed=failed", 1},
		{"passed=any", 2},
		{"", 2},
	}
	for _, tc := range cases {
		u := "/api/v1/rules/rule-f/results"
		if tc.query != "" {
			u += "?" + tc.query
		}
		req := httptest.NewRequest(http.MethodGet, u, nil)
		req.SetPathValue("id", "rule-f")
		w := httptest.NewRecorder()
		r.handleRuleResults(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("query %q: status %d body=%s", tc.query, w.Code, w.Body.String())
		}
		var env struct {
			Rows []rule.RuleResultWithArtist `json:"rows"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("query %q: decoding: %v body=%s", tc.query, err, w.Body.String())
		}
		if len(env.Rows) != tc.want {
			t.Errorf("query %q: got %d rows, want %d", tc.query, len(env.Rows), tc.want)
		}
	}
}

// TestHandleRuleResults_UnknownRuleReturns200WithEmptyRows: an unknown
// rule id yields total=0, rows=[], page=1 -- the same shape the front-end
// always parses. We do NOT 404 because the rule_results table may be
// empty for a freshly-disabled rule, and the UI's clearer signal is
// "no data" rather than "not found".
func TestHandleRuleResults_UnknownRuleReturns200WithEmptyRows(t *testing.T) {
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/rules/does-not-exist/results", nil)
	req.SetPathValue("id", "does-not-exist")
	w := httptest.NewRecorder()
	r.handleRuleResults(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Rows  []rule.RuleResultWithArtist `json:"rows"`
		Total int                         `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding: %v body=%s", err, w.Body.String())
	}
	if env.Total != 0 || len(env.Rows) != 0 {
		t.Errorf("got total=%d rows=%d, want 0/0", env.Total, len(env.Rows))
	}
}

// TestHandleRuleResults_PageAndPageSizeClamps exercises the clamp
// branches around page / page_size. Covers:
//   - page < 1 normalizes to 1
//   - page_size=0 (explicit zero) falls through to the user-preference path
//     and returns PageSizeDefault (50) when no preference is stored
//   - page_size > PageSizeMax (500) clamps to PageSizeMax via getUserPageSize
//   - page_size below PageSizeMin (10) clamps to PageSizeMin via getUserPageSize
//
// The body shape is checked rather than the row payload; the seed has
// one passing artist, so the row count is at most 1 in every case.
func TestHandleRuleResults_PageAndPageSizeClamps(t *testing.T) {
	r, _ := testRouter(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedArtistRow(t, r, "clamp-a", "Alpha")
	seedRuleRow(t, r, "rule-clamp", "Clamp Rule")
	mustSeedAPI(t, "seed pass", r.ruleService.UpsertRuleResultPass(ctx, "clamp-a", "rule-clamp", now))

	cases := []struct {
		name         string
		query        string
		wantPage     int
		wantPageSize int
	}{
		{"page<1 normalizes to 1", "page=0&page_size=10", 1, 10},
		// page_size=0 signals "no override" to getUserPageSize; no stored
		// preference means PageSizeDefault (50) is returned.
		{"page_size=0 falls through to default", "page=1&page_size=0", 1, PageSizeDefault},
		// getUserPageSize clamps above PageSizeMax (500) to PageSizeMax.
		{"page_size>PageSizeMax clamps to PageSizeMax", "page=1&page_size=999", 1, PageSizeMax},
		// getUserPageSize clamps below PageSizeMin (10) to PageSizeMin.
		{"page_size<PageSizeMin clamps to PageSizeMin", "page=1&page_size=1", 1, PageSizeMin},
		// page > math.MaxInt/pageSize triggers the overflow guard;
		// pick a value comfortably above the clamp for pageSize=10.
		{"page overflow clamps to maxPage", "page=999999999999999999&page_size=10", math.MaxInt / 10, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet,
				"/api/v1/rules/rule-clamp/results?"+tc.query, nil)
			req.SetPathValue("id", "rule-clamp")
			w := httptest.NewRecorder()
			r.handleRuleResults(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
			}
			var env struct {
				Page     int `json:"page"`
				PageSize int `json:"page_size"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("decoding: %v body=%s", err, w.Body.String())
			}
			if env.Page != tc.wantPage || env.PageSize != tc.wantPageSize {
				t.Errorf("got page=%d page_size=%d, want %d/%d",
					env.Page, env.PageSize, tc.wantPage, tc.wantPageSize)
			}
		})
	}
}

// TestHandleArtistRuleResults_ReturnsEnabledOnly seeds two rules (one
// enabled, one disabled) for the same artist, both evaluated, and asserts
// the disabled rule is filtered out and the response includes the joined
// rule name + severity.
func TestHandleArtistRuleResults_ReturnsEnabledOnly(t *testing.T) {
	r, _ := testRouter(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedArtistRow(t, r, "abc-123", "Twelve Pebbles")
	seedRuleRow(t, r, "rule-on", "Enabled Rule")
	seedRuleRow(t, r, "rule-off", "Disabled Rule")
	if _, err := r.db.ExecContext(ctx, `UPDATE rules SET enabled = 0 WHERE id = ?`, "rule-off"); err != nil {
		t.Fatalf("disable rule-off: %v", err)
	}
	mustSeedAPI(t, "seed pass abc-123 rule-on", r.ruleService.UpsertRuleResultPass(ctx, "abc-123", "rule-on", now))
	mustSeedAPI(t, "seed pass abc-123 rule-off", r.ruleService.UpsertRuleResultPass(ctx, "abc-123", "rule-off", now))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/abc-123/rule-results", nil)
	req.SetPathValue("id", "abc-123")
	w := httptest.NewRecorder()
	r.handleArtistRuleResults(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var env struct {
		RuleResults []rule.RuleResultWithRule `json:"rule_results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding: %v body=%s", err, w.Body.String())
	}
	if len(env.RuleResults) != 1 {
		t.Fatalf("got %d rows, want 1 (disabled rule filtered)", len(env.RuleResults))
	}
	if env.RuleResults[0].RuleID != "rule-on" || env.RuleResults[0].RuleName != "Enabled Rule" {
		t.Errorf("row: %+v want {rule-on, Enabled Rule}", env.RuleResults[0])
	}
}

// TestHandleArtistRuleResults_UnknownArtistReturns404 mirrors the existing
// /api/v1/artists/{id}/health 404 contract so callers can rely on a
// single not-found shape across the artist-detail endpoints.
func TestHandleArtistRuleResults_UnknownArtistReturns404(t *testing.T) {
	r, _ := testRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/nope/rule-results", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	r.handleArtistRuleResults(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}
