package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
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
	if !reflect.DeepEqual(before, after) {
		t.Errorf("rule was mutated on rejected request:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func TestHandleUpdateRule_ValidModesAccepted(t *testing.T) {
	r, _ := testRouter(t)

	ruleID := firstRuleID(t, r.ruleService)

	for _, mode := range []string{"auto", "manual"} {
		t.Run(mode, func(t *testing.T) {
			body := strings.NewReader(`{"automation_mode":"` + mode + `"}`)
			req := httptest.NewRequest(http.MethodPut, "/api/v1/rules/"+ruleID, body)
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", ruleID)
			w := httptest.NewRecorder()

			r.handleUpdateRule(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
			}

			var resp rule.Rule
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			if resp.AutomationMode != mode {
				t.Errorf("response automation_mode = %q, want %q", resp.AutomationMode, mode)
			}

			persisted, err := r.ruleService.GetByID(context.Background(), ruleID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if persisted.AutomationMode != mode {
				t.Errorf("persisted automation_mode = %q, want %q", persisted.AutomationMode, mode)
			}
		})
	}
}

// requireErrorBody decodes the JSON response body and verifies the "error" field matches want.
func requireErrorBody(t *testing.T, w *httptest.ResponseRecorder, want string) {
	t.Helper()
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if got := resp["error"]; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestHandleUpdateRule_NotFound(t *testing.T) {
	r, _ := testRouter(t)

	body := strings.NewReader(`{"enabled":true}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/rules/nonexistent", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()

	r.handleUpdateRule(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
	requireErrorBody(t, w, "rule not found")
}

func TestHandleRunRule_NotFound(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rules/nonexistent/run", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()

	r.handleRunRule(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
	requireErrorBody(t, w, "rule not found")
}

func TestHandleRunRule_NilPipeline(t *testing.T) {
	r, _ := testRouter(t)
	// testRouter has nil pipeline; use a valid (seeded) rule ID.
	ruleID := firstRuleID(t, r.ruleService)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rules/"+ruleID+"/run", nil)
	req.SetPathValue("id", ruleID)
	w := httptest.NewRecorder()

	r.handleRunRule(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	requireErrorBody(t, w, "rule pipeline not configured")
}

func TestHandleEvaluateArtist_NotFound(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/nonexistent/health", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()

	r.handleEvaluateArtist(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
	requireErrorBody(t, w, "artist not found")
}

func TestHandleEvaluateArtist_ReturnsHealthScore(t *testing.T) {
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "Test Artist")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/"+a.ID+"/health", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleEvaluateArtist(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if _, ok := resp["health_score"]; !ok {
		t.Error("response missing health_score field")
	}
	if _, ok := resp["violations"]; !ok {
		t.Error("response missing violations field")
	}
	// No persistence error, so warning should be absent.
	if _, ok := resp["warning"]; ok {
		t.Error("response should not contain warning field when update succeeds")
	}
}

func TestHandleBulkJobStatus_NotFound(t *testing.T) {
	r, _ := testRouter(t)
	r.bulkService = rule.NewBulkService(r.db)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/bulk/jobs/nonexistent", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()

	r.handleBulkJobStatus(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
	requireErrorBody(t, w, "job not found")
}

func TestHandleBulkJobList_ReturnsJobs(t *testing.T) {
	r, _ := testRouter(t)
	r.bulkService = rule.NewBulkService(r.db)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/bulk/jobs", nil)
	w := httptest.NewRecorder()

	r.handleBulkJobList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if _, ok := resp["jobs"]; !ok {
		t.Error("response missing jobs field")
	}
}

// testRouterWithBulkJob creates a router with a bulk service and a pre-created job.
func testRouterWithBulkJob(t *testing.T) (*Router, *artist.Service, string) {
	t.Helper()
	r, artistSvc := testRouter(t)
	r.bulkService = rule.NewBulkService(r.db)
	job, err := r.bulkService.CreateJob(context.Background(), rule.BulkTypeFetchMetadata, rule.BulkModePromptNoMatch, 0)
	if err != nil {
		t.Fatalf("creating test bulk job: %v", err)
	}
	return r, artistSvc, job.ID
}

func TestHandleBulkJobStatus_WithJob(t *testing.T) {
	r, _, jobID := testRouterWithBulkJob(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/bulk/jobs/"+jobID, nil)
	req.SetPathValue("id", jobID)
	w := httptest.NewRecorder()

	r.handleBulkJobStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["job"] == nil {
		t.Error("response missing job field")
	}
	if _, ok := resp["items"]; !ok {
		t.Error("response missing items field")
	}
}
