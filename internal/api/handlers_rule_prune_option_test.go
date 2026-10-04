package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/rule"
)

// pruneOptionPut sends a PUT for the duplicate-images rule through a locally
// applied middleware.RequireAdmin around handleUpdateRule, as a caller holding
// the given role. It pins RequireAdmin plus the handler's behavior; it does NOT
// pin the route wiring (see TestUpdateRule_PruneOption_OperatorForbiddenThroughMux).
func pruneOptionPut(t *testing.T, h *ruleCacheHarness, role, body string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := middleware.WithTestRole(middleware.WithTestUserID(context.Background(), "test-user"), role)
	req := httptest.NewRequestWithContext(ctx, http.MethodPut, "/api/v1/rules/"+rule.RuleImageDuplicate, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", rule.RuleImageDuplicate)
	w := httptest.NewRecorder()
	middleware.RequireAdmin(h.r.handleUpdateRule)(w, req)
	return w
}

// storedRuleConfig reads the rule back from the store, not from the response.
func storedRuleConfig(t *testing.T, h *ruleCacheHarness) rule.RuleConfig {
	t.Helper()
	r, err := h.r.ruleService.GetByID(context.Background(), rule.RuleImageDuplicate)
	if err != nil {
		t.Fatalf("reading rule: %v", err)
	}
	return r.Config
}

// TestUpdateRule_PruneOption_OperatorForbidden (#3138 S4b): with RequireAdmin
// applied locally, an operator PUT is refused and the stored value does not
// move. This pins RequireAdmin and the handler, not the route.
func TestUpdateRule_PruneOption_OperatorForbidden(t *testing.T) {
	t.Parallel()
	h := newRuleCacheHarness(t)
	if cfg := storedRuleConfig(t, h); cfg.PrunePlatformCopies {
		t.Fatal("precondition: the seeded rule must have the option off")
	}

	w := pruneOptionPut(t, h, "operator", `{"config":{"severity":"warning","prune_platform_copies":true}}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("operator PUT: status %d, want 403; body %s", w.Code, w.Body.String())
	}
	if storedRuleConfig(t, h).PrunePlatformCopies {
		t.Error("operator PUT was refused but the stored option is on")
	}
}

// TestUpdateRule_PruneOption_OperatorForbiddenThroughMux drives a real operator
// credential through the real mux, so it fails if the PUT route loses
// RequireAdmin. The option is the consent for irreversible platform deletes.
func TestUpdateRule_PruneOption_OperatorForbiddenThroughMux(t *testing.T) {
	t.Parallel()
	r, authSvc, _ := testRouterWithAuth(t)
	if _, err := r.db.Exec(`INSERT INTO users (id, username, role) VALUES ('op-user', 'op', 'operator')`); err != nil {
		t.Fatalf("inserting operator: %v", err)
	}
	token, _, err := authSvc.CreateAPIToken(context.Background(), "op-user", "prune-opt-test", string(auth.ScopeAdmin))
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := r.Handler(ctx)
	read := func() bool {
		ru, err := r.ruleService.GetByID(context.Background(), rule.RuleImageDuplicate)
		if err != nil {
			t.Fatalf("reading rule: %v", err)
		}
		return ru.Config.PrunePlatformCopies
	}
	if read() {
		t.Fatal("precondition: the seeded rule must have the option off")
	}

	body := `{"config":{"severity":"warning","prune_platform_copies":true}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/rules/"+rule.RuleImageDuplicate, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("operator PUT through the mux: status %d, want 403; body %s", w.Code, w.Body.String())
	}
	if read() {
		t.Error("operator PUT was refused but the stored option is on")
	}
}

// TestUpdateRule_PruneOption_AdminOnThenOffReplaces: an admin can turn the
// option on, and the next PUT that omits the key turns it off, because the
// handler replaces the whole config rather than merging it.
func TestUpdateRule_PruneOption_AdminOnThenOffReplaces(t *testing.T) {
	t.Parallel()
	h := newRuleCacheHarness(t)
	if storedRuleConfig(t, h).PrunePlatformCopies {
		t.Fatal("precondition: the seeded rule must have the option off")
	}

	w := pruneOptionPut(t, h, "administrator", `{"config":{"severity":"warning","prune_platform_copies":true}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin PUT on: status %d; body %s", w.Code, w.Body.String())
	}
	var got rule.Rule
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !got.Config.PrunePlatformCopies {
		t.Error("response does not report the option on")
	}
	if !storedRuleConfig(t, h).PrunePlatformCopies {
		t.Fatal("stored option is not on after the admin PUT")
	}

	w = pruneOptionPut(t, h, "administrator", `{"config":{"severity":"warning"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin PUT without the key: status %d; body %s", w.Code, w.Body.String())
	}
	if storedRuleConfig(t, h).PrunePlatformCopies {
		t.Error("a PUT without the key left the option on: config was merged, not replaced")
	}
}

// TestUpdateRule_PruneOption_ToleranceCarriedAndPolicy: the stored tolerance
// survives the PUT, and the sweep policy (the same producer the background
// sweep reads) accepts 0.95 but reports disabled for 0.80 even though the PUT
// itself succeeds.
func TestUpdateRule_PruneOption_ToleranceCarriedAndPolicy(t *testing.T) {
	t.Parallel()
	h := newRuleCacheHarness(t)
	ctx := context.Background()

	w := pruneOptionPut(t, h, "administrator", `{"enabled":true,"config":{"severity":"warning","tolerance":0.95,"prune_platform_copies":true}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin PUT tolerance 0.95: status %d; body %s", w.Code, w.Body.String())
	}
	cfg := storedRuleConfig(t, h)
	if cfg.Tolerance != 0.95 || !cfg.PrunePlatformCopies {
		t.Fatalf("stored config = tolerance %v, option %v; want 0.95 and on", cfg.Tolerance, cfg.PrunePlatformCopies)
	}
	tol, enabled, err := h.r.ruleService.PlatformDupSweepPolicy(ctx)
	if err != nil || !enabled || tol != 0.95 {
		t.Fatalf("precondition: policy at 0.95 = (%v, %v, %v), want (0.95, true, nil)", tol, enabled, err)
	}

	// Boundary probes: the floor is inclusive at 0.85 and refuses 0.84.
	for _, tc := range []struct {
		body    string
		stored  float64
		want    float64
		enabled bool
	}{
		{`{"config":{"severity":"warning","tolerance":0.85,"prune_platform_copies":true}}`, 0.85, 0.85, true},
		{`{"config":{"severity":"warning","tolerance":0.84,"prune_platform_copies":true}}`, 0.84, 0, false},
	} {
		if w := pruneOptionPut(t, h, "administrator", tc.body); w.Code != http.StatusOK {
			t.Fatalf("admin PUT %s: status %d", tc.body, w.Code)
		}
		if got := storedRuleConfig(t, h).Tolerance; got != tc.stored {
			t.Errorf("stored tolerance for %s = %v, want %v", tc.body, got, tc.stored)
		}
		tol, enabled, err := h.r.ruleService.PlatformDupSweepPolicy(ctx)
		if err != nil || enabled != tc.enabled || tol != tc.want {
			t.Errorf("policy for %s = (%v, %v, %v), want (%v, %v, nil)", tc.body, tol, enabled, err, tc.want, tc.enabled)
		}
	}

	w = pruneOptionPut(t, h, "administrator", `{"config":{"severity":"warning","tolerance":0.80,"prune_platform_copies":true}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin PUT tolerance 0.80: status %d, want 200; body %s", w.Code, w.Body.String())
	}
	if got := storedRuleConfig(t, h).Tolerance; got != 0.80 {
		t.Errorf("stored tolerance = %v, want 0.80", got)
	}
	tol, enabled, err = h.r.ruleService.PlatformDupSweepPolicy(ctx)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if enabled || tol != 0 {
		t.Errorf("policy at 0.80 = (%v, %v), want (0, false): the prune must be reported disabled", tol, enabled)
	}
}
