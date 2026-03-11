package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/rule"
)

// firstRuleID fetches the first seeded rule ID from the service.
func firstRuleID(t *testing.T, svc *rule.Service) string {
	t.Helper()
	rules, err := svc.List(context.Background())
	if err != nil || len(rules) == 0 {
		t.Fatalf("expected seeded rules, got %v (err: %v)", len(rules), err)
	}
	return rules[0].ID
}

func TestHandleUpdateRule_DisabledModeReturns400(t *testing.T) {
	r, _ := testRouter(t)

	ruleID := firstRuleID(t, r.ruleService)

	before, err := r.ruleService.GetByID(context.Background(), ruleID)
	if err != nil {
		t.Fatalf("GetByID before: %v", err)
	}

	body := strings.NewReader(`{"automation_mode":"disabled"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/rules/"+ruleID, body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", ruleID)
	w := httptest.NewRecorder()

	r.handleUpdateRule(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	after, err := r.ruleService.GetByID(context.Background(), ruleID)
	if err != nil {
		t.Fatalf("GetByID after: %v", err)
	}
	if after.AutomationMode != before.AutomationMode {
		t.Errorf("automation_mode mutated: got %q, want %q", after.AutomationMode, before.AutomationMode)
	}
	if after.Enabled != before.Enabled {
		t.Errorf("enabled mutated: got %v, want %v", after.Enabled, before.Enabled)
	}
}

func TestHandleUpdateRule_ValidModesAccepted(t *testing.T) {
	r, _ := testRouter(t)

	ruleID := firstRuleID(t, r.ruleService)

	for _, mode := range []string{"auto", "manual"} {
		body := strings.NewReader(`{"automation_mode":"` + mode + `"}`)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/rules/"+ruleID, body)
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", ruleID)
		w := httptest.NewRecorder()

		r.handleUpdateRule(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("mode=%q: status = %d, want %d; body: %s", mode, w.Code, http.StatusOK, w.Body.String())
		}

		var resp rule.Rule
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Errorf("mode=%q: decoding response: %v", mode, err)
		}
		if resp.AutomationMode != mode {
			t.Errorf("mode=%q: response automation_mode = %q, want %q", mode, resp.AutomationMode, mode)
		}

		persisted, err := r.ruleService.GetByID(context.Background(), ruleID)
		if err != nil {
			t.Fatalf("mode=%q: GetByID: %v", mode, err)
		}
		if persisted.AutomationMode != mode {
			t.Errorf("mode=%q: persisted automation_mode = %q, want %q", mode, persisted.AutomationMode, mode)
		}
	}
}
