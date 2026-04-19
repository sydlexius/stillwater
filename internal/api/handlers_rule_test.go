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

func TestHandleRunRule_Returns202(t *testing.T) {
	r, _, ruleSvc := testRouterWithPipelineFull(t)
	ruleID := firstRuleID(t, ruleSvc)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rules/"+ruleID+"/run", nil)
	req.SetPathValue("id", ruleID)
	w := httptest.NewRecorder()

	r.handleRunRule(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body["status"] != "running" {
		t.Errorf("status = %v, want running", body["status"])
	}
	if body["rule_id"] != ruleID {
		t.Errorf("rule_id = %v, want %s", body["rule_id"], ruleID)
	}
}

func TestHandleRunRule_409WhenAlreadyRunning(t *testing.T) {
	// Use a blocking stub so the first run stays in-progress until we release it.
	blockCh := make(chan struct{})
	stub := &stubPipeline{
		runRuleFn: func(_ context.Context, _ string) (*rule.RunResult, error) {
			<-blockCh
			return &rule.RunResult{}, nil
		},
	}
	r, _ := testRouterWithStubPipeline(t, stub)

	rules, err := r.ruleService.List(context.Background())
	if err != nil || len(rules) == 0 {
		t.Fatalf("listing rules: err=%v, count=%d", err, len(rules))
	}
	ruleID := rules[0].ID

	// Start a run (returns 202, goroutine blocks on blockCh)
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/rules/"+ruleID+"/run", nil)
	req1.SetPathValue("id", ruleID)
	w1 := httptest.NewRecorder()
	r.handleRunRule(w1, req1)

	if w1.Code != http.StatusAccepted {
		t.Fatalf("first run: status = %d, want %d", w1.Code, http.StatusAccepted)
	}

	// Second run while the first is still blocked -- must get 409.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/rules/"+ruleID+"/run", nil)
	req2.SetPathValue("id", ruleID)
	w2 := httptest.NewRecorder()
	r.handleRunRule(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Fatalf("second run: status = %d, want %d; body: %s", w2.Code, http.StatusConflict, w2.Body.String())
	}

	// Release the blocked goroutine so it can clean up.
	close(blockCh)
}

// TestHandleRunRule_InvalidScope400 exercises the parseRunScope error path on
// POST /rules/{id}/run. The handler must warn-log the raw error and return a
// generic 400 without echoing the bad input back to the client. Covers the
// scope-validation branch added for #698.
func TestHandleRunRule_InvalidScope400(t *testing.T) {
	r, _, ruleSvc := testRouterWithPipelineFull(t)
	ruleID := firstRuleID(t, ruleSvc)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rules/"+ruleID+"/run?scope=xyz", nil)
	req.SetPathValue("id", ruleID)
	w := httptest.NewRecorder()

	r.handleRunRule(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	requireErrorBody(t, w, "invalid scope parameter")
}

// TestHandleRunAllRules_NilPipeline503 exercises the early-return path when
// the pipeline is not wired. Distinct from the single-rule run because
// handleRunAllRules has its own pipeline-nil check.
func TestHandleRunAllRules_NilPipeline503(t *testing.T) {
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rules/run-all", nil)
	w := httptest.NewRecorder()

	r.handleRunAllRules(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	requireErrorBody(t, w, "rule pipeline not configured")
}

// TestHandleRunAllRules_InvalidScope400 covers the 400 branch added to
// POST /rules/run-all so the spec's new 400 response is backed by the
// implementation.
func TestHandleRunAllRules_InvalidScope400(t *testing.T) {
	r, _, _ := testRouterWithPipelineFull(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rules/run-all?scope=xyz", nil)
	w := httptest.NewRecorder()

	r.handleRunAllRules(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	requireErrorBody(t, w, "invalid scope parameter")
}

// TestHandleRunAllRules_Returns202 covers the happy-path acknowledgment on
// POST /rules/run-all. Uses the stub pipeline so the background goroutine
// returns immediately and the test does not race on real evaluation work.
func TestHandleRunAllRules_Returns202(t *testing.T) {
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rules/run-all", nil)
	w := httptest.NewRecorder()

	r.handleRunAllRules(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body["status"] != "running" {
		t.Errorf("status = %v, want running", body["status"])
	}
	if body["scope"] != "incremental" {
		t.Errorf("scope = %v, want incremental (default)", body["scope"])
	}
	// Full RuleRunStatus contract: artists_processed and artists_total are
	// always present; artists_skipped is optional (omitempty) but if present
	// must be numeric. Guards against silent field drops on the 202 payload.
	if _, ok := body["artists_processed"]; !ok {
		t.Error("response missing artists_processed")
	}
	if _, ok := body["artists_total"]; !ok {
		t.Error("response missing artists_total")
	}
	if v, ok := body["artists_skipped"]; ok {
		if _, numeric := v.(float64); !numeric {
			t.Errorf("artists_skipped type = %T, want number when present", v)
		}
	}
}

// TestHandleRunAllRules_409WhenAlreadyRunning exercises the ruleRunMu gate on
// POST /rules/run-all. A blocking stub keeps the first run in-progress so the
// second call must observe r.ruleRun.Running == true and return 409.
func TestHandleRunAllRules_409WhenAlreadyRunning(t *testing.T) {
	blockCh := make(chan struct{})
	// Register cleanup immediately so a later t.Fatalf cannot strand the
	// blocked goroutine. Closing an already-closed channel panics, so this
	// must only run once -- t.Cleanup is guaranteed to fire exactly once per
	// registration and close(blockCh) at the bottom of the test is removed.
	t.Cleanup(func() { close(blockCh) })
	stub := &stubPipeline{
		runRuleFn: func(_ context.Context, _ string) (*rule.RunResult, error) {
			<-blockCh
			return &rule.RunResult{}, nil
		},
	}
	// Wrap RunAllScoped too so the goroutine blocks instead of returning fast.
	stub2 := &blockingStubPipeline{stubPipeline: *stub, block: blockCh}
	r, _ := testRouterWithStubPipeline(t, &stub2.stubPipeline)
	// Swap the pipeline to the blocking variant so handleRunAllRules waits.
	r.pipeline = stub2

	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/rules/run-all", nil)
	w1 := httptest.NewRecorder()
	r.handleRunAllRules(w1, req1)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first run: status = %d, want %d", w1.Code, http.StatusAccepted)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/rules/run-all", nil)
	w2 := httptest.NewRecorder()
	r.handleRunAllRules(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("second run: status = %d, want %d; body: %s", w2.Code, http.StatusConflict, w2.Body.String())
	}
}

// blockingStubPipeline wraps stubPipeline with a RunAllScoped that blocks on a
// channel until the test releases it. Used by the 409 test above.
type blockingStubPipeline struct {
	stubPipeline
	block chan struct{}
}

func (b *blockingStubPipeline) RunAllScoped(_ context.Context, _ rule.RunScope) (*rule.RunResult, error) {
	<-b.block
	return &rule.RunResult{}, nil
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
