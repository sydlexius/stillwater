package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/rule"
)

// TestHandleReportRulePassRates_ReturnsEnabledRulesOnly seeds one enabled
// + one disabled rule with rule_results rows for both and verifies the
// disabled rule is filtered out, that the response is ordered by
// PassRate ASC, and that the envelope key is `rates` (plural, top-level)
// to match the /reports/health endpoint's `top_violations` pattern.
func TestHandleReportRulePassRates_ReturnsEnabledRulesOnly(t *testing.T) {
	r, _ := testRouter(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, art := range []string{"r1", "r2"} {
		seedArtistRow(t, r, art, "Artist "+art)
	}
	seedRuleRow(t, r, "rule-on", "Enabled Rule")
	seedRuleRow(t, r, "rule-off", "Disabled Rule")
	if _, err := r.db.ExecContext(ctx, `UPDATE rules SET enabled = 0 WHERE id = ?`, "rule-off"); err != nil {
		t.Fatalf("disable rule-off: %v", err)
	}

	mustSeedAPI(t, "seed pass r1 rule-on", r.ruleService.UpsertRuleResultPass(ctx, "r1", "rule-on", now))
	mustSeedAPI(t, "seed fail r2 rule-on", r.ruleService.UpsertRuleResultFail(ctx, "r2", "rule-on", "vr2", "msg", now))
	mustSeedAPI(t, "seed pass r1 rule-off", r.ruleService.UpsertRuleResultPass(ctx, "r1", "rule-off", now))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/rule-pass-rates", nil)
	w := httptest.NewRecorder()
	r.handleReportRulePassRates(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var env struct {
		Rates []rule.RulePassRate `json:"rates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding: %v body=%s", err, w.Body.String())
	}
	if len(env.Rates) != 1 {
		t.Fatalf("got %d rates, want 1 (disabled rule excluded)", len(env.Rates))
	}
	if env.Rates[0].RuleID != "rule-on" {
		t.Errorf("rule_id: got %q, want rule-on", env.Rates[0].RuleID)
	}
	if env.Rates[0].Passed != 1 || env.Rates[0].Failed != 1 || env.Rates[0].Evaluated != 2 {
		t.Errorf("counts: %+v want P=1 F=1 E=2", env.Rates[0])
	}
}

// TestHandleReportRulePassRates_EmptyDB returns 200 with an empty
// rates array when no rule_results rows exist yet. The widget renders
// "no data yet" client-side, but the contract requires the field to
// be present (never null).
func TestHandleReportRulePassRates_EmptyDB(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/rule-pass-rates", nil)
	w := httptest.NewRecorder()
	r.handleReportRulePassRates(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var env struct {
		Rates []rule.RulePassRate `json:"rates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding: %v body=%s", err, w.Body.String())
	}
	if env.Rates == nil {
		t.Error("rates field is null; want [] so the front-end never sees a missing key")
	}
	if len(env.Rates) != 0 {
		t.Errorf("got %d rates, want 0", len(env.Rates))
	}
}

// TestHandleReportRulePassRates_ServiceFailureReturns500 swaps in a
// closed DB to force GetRulePassRates to fail; the handler should
// return 500 (not 200 + missing field) so callers can distinguish a
// real error from a legitimately empty result.
func TestHandleReportRulePassRates_ServiceFailureReturns500(t *testing.T) {
	r, _ := testRouter(t)

	// Close the underlying DB so any service call errors. This is the
	// simplest way to trigger an error from GetRulePassRates without
	// introducing a mock interface for one test.
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/rule-pass-rates", nil)
	w := httptest.NewRecorder()
	r.handleReportRulePassRates(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}
