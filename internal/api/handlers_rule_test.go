package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestHandleRunRule_Returns202 uses a blocking stub so the background
// goroutine cannot flip r.ruleRun.Status from "running" to "completed"
// before the 202 snapshot is written (CI flake on main, issue #1707;
// mirrors the run-all de-flake from PR #1644 round 5).
func TestHandleRunRule_Returns202(t *testing.T) {
	t.Parallel()
	blockCh := make(chan struct{})
	doneCh := make(chan struct{})
	stub := &stubPipeline{
		runRuleFn: func(_ context.Context, _ string) (*rule.RunResult, error) {
			defer close(doneCh)
			<-blockCh
			return &rule.RunResult{}, nil
		},
	}
	r, _ := testRouterWithStubPipeline(t, stub)
	t.Cleanup(func() {
		close(blockCh)
		select {
		case <-doneCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for run-rule goroutine to finish")
		}
	})

	ruleID := firstRuleID(t, r.ruleService)

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
	t.Parallel()
	// Use a blocking stub so the first run stays in-progress until we release it.
	// doneCh is closed by the stub once RunRule has been invoked, so cleanup
	// can join the handler goroutine after closing blockCh -- closing alone
	// only unblocks the stub, it does not wait for the handler's post-run
	// processing to finish before the router/DB tear down.
	blockCh := make(chan struct{})
	doneCh := make(chan struct{})
	stub := &stubPipeline{
		runRuleFn: func(_ context.Context, _ string) (*rule.RunResult, error) {
			defer close(doneCh)
			<-blockCh
			return &rule.RunResult{}, nil
		},
	}
	r, _ := testRouterWithStubPipeline(t, stub)
	t.Cleanup(func() {
		close(blockCh)
		<-doneCh
	})

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

}

// TestHandleRunRule_InvalidScope400 exercises the parseRunScope error path on
// POST /rules/{id}/run. The handler must warn-log the raw error and return a
// generic 400 without echoing the bad input back to the client. Covers the
// scope-validation branch added for #698.
func TestHandleRunRule_InvalidScope400(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
// POST /rules/run-all. Uses a blocking stub so the background goroutine
// does not race ahead of the synchronous 202 response and mutate
// r.ruleRun.Status to "completed" before writeJSON reads it (CI flake
// observed on round 5 of M52 PR #1644).
func TestHandleRunAllRules_Returns202(t *testing.T) {
	t.Parallel()
	blockCh := make(chan struct{})
	doneCh := make(chan struct{})
	stub := &blockingStubPipeline{
		stubPipeline: stubPipeline{},
		block:        blockCh,
		done:         doneCh,
	}
	r, _ := testRouterWithStubPipeline(t, &stub.stubPipeline)
	r.pipeline = stub
	// Release the goroutine after we've inspected w.Body, and join so it
	// cannot mutate router state during test teardown.
	t.Cleanup(func() {
		close(blockCh)
		<-doneCh
	})

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

// TestHandleRunAllRules_StampsLastEvaluated is the handler-level guard for #1796:
// a successful manual "Run rules" must advance the scheduler's lastRunAt (the
// source of the dashboards' "Last evaluated" stat), not just the scheduled tick.
// TestScheduler_MarkEvaluated proves the primitive works; this proves the handler
// actually CALLS it. The call runs in the background goroutine after RunAllScoped
// returns, so the assertion polls until the run-all status reports "completed";
// a dropped MarkEvaluated call would leave last_evaluation_at nil forever and
// fail on timeout.
//
// It also guards the publish ORDERING (#1803 / #2152): the invariant is
// asserted CAUSALLY on a single /rules/run-all/status response body -- the
// instant that response reports "completed", its own last_evaluation_at field
// must already be non-nil, on that SAME read. Earlier versions of this test
// read the scheduler's Status() and the run-all status via two separate
// locked calls, which could observe a stale-nil stamp alongside a fresh
// "completed" purely from the two-mutex read gap, independent of handler
// ordering (#2152). Reading last_evaluation_at off the ruleRunStatus struct
// itself (set atomically under ruleRunMu in the same critical section that
// flips Status to "completed", per handlers_rule.go) closes that gap.
func TestHandleRunAllRules_StampsLastEvaluated(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)
	// A real but un-started scheduler as the MarkEvaluated target -- not Start()ed,
	// so no ticker runs; it serves only as the lastRunAt holder the handler stamps.
	r.ruleScheduler = rule.NewScheduler(stub, r.ruleService, r.artistService, r.logger)

	if r.ruleScheduler.Status().LastEvaluationAt != nil {
		t.Fatalf("precondition: expected no prior evaluation, got %v", r.ruleScheduler.Status().LastEvaluationAt)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rules/run-all", nil)
	w := httptest.NewRecorder()
	r.handleRunAllRules(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	// runAllStatus reads the same handler clients poll, returning the full
	// decoded body so Status and LastEvaluationAt come from one response.
	runAllStatus := func() (status string, lastEvaluationAt *time.Time) {
		sreq := httptest.NewRequest(http.MethodGet, "/api/v1/rules/run-all/status", nil)
		sw := httptest.NewRecorder()
		r.handleRunAllRulesStatus(sw, sreq)
		var body struct {
			Status           string     `json:"status"`
			LastEvaluationAt *time.Time `json:"last_evaluation_at"`
		}
		if err := json.Unmarshal(sw.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding run-all status: %v (body: %s)", err, sw.Body.String())
		}
		return body.Status, body.LastEvaluationAt
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		status, lastEvaluationAt := runAllStatus()
		if status == "completed" {
			// Causal invariant (#1803, #2152): the same response that reports
			// completion must already carry the stamp.
			if lastEvaluationAt == nil {
				t.Fatal("#2152 race: /rules/run-all/status reported \"completed\" with last_evaluation_at still nil on the same response")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("handleRunAllRules did not reach \"completed\" within 3s (#1796 regression: MarkEvaluated not called on a manual run)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHandleRunAllRules_409WhenAlreadyRunning exercises the ruleRunMu gate on
// POST /rules/run-all. A blocking stub keeps the first run in-progress so the
// second call must observe r.ruleRun.Running == true and return 409.
func TestHandleRunAllRules_409WhenAlreadyRunning(t *testing.T) {
	t.Parallel()
	blockCh := make(chan struct{})
	doneCh := make(chan struct{})
	stub := &stubPipeline{
		runRuleFn: func(_ context.Context, _ string) (*rule.RunResult, error) {
			<-blockCh
			return &rule.RunResult{}, nil
		},
	}
	// Wrap RunAllScoped too so the goroutine blocks instead of returning fast.
	// done is closed inside RunAllScoped (via defer) so cleanup can join the
	// background goroutine after unblocking it.
	stub2 := &blockingStubPipeline{stubPipeline: *stub, block: blockCh, done: doneCh}
	r, _ := testRouterWithStubPipeline(t, &stub2.stubPipeline)
	// Swap the pipeline to the blocking variant so handleRunAllRules waits.
	r.pipeline = stub2
	// Register the unblock AFTER router setup so LIFO cleanup releases the
	// blocked goroutine before the router/DB are torn down. Closing alone is
	// not enough -- we must also wait for the handler goroutine to finish so
	// it cannot mutate router state after teardown.
	t.Cleanup(func() {
		close(blockCh)
		<-doneCh
	})

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
// channel until the test releases it. Used by the 409 test above. The done
// channel is closed when RunAllScoped returns so the test can join the
// background goroutine before tearing down router/DB state.
type blockingStubPipeline struct {
	stubPipeline
	block chan struct{}
	done  chan struct{}
}

func (b *blockingStubPipeline) RunAllScoped(_ context.Context, _ rule.RunScope) (*rule.RunResult, error) {
	defer close(b.done)
	<-b.block
	return &rule.RunResult{}, nil
}

func TestHandleEvaluateArtist_NotFound(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestHandleRuleResults_RespectsPageSizePref verifies that the rule-results
// handler respects the per-user page_size preference when no query param is
// provided.
func TestHandleRuleResults_RespectsPageSizePref(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	const testUserID = "test-user-rule-pagesize"

	ruleID := firstRuleID(t, r.ruleService)

	// Seed more artists with rule results than the preference value so the cap
	// is observable.
	evaluatedAt := time.Now().UTC()
	for i := 0; i < 15; i++ {
		a := &artist.Artist{
			Name: fmt.Sprintf("Rule Artist %02d", i),
			Path: fmt.Sprintf("/music/rule-%02d", i),
		}
		if err := artistSvc.Create(context.Background(), a); err != nil {
			t.Fatalf("creating artist %d: %v", i, err)
		}
		if err := r.ruleService.UpsertRuleResultPass(context.Background(), a.ID, ruleID, evaluatedAt); err != nil {
			t.Fatalf("upserting rule result %d: %v", i, err)
		}
	}

	// Store page_size=10 directly in the DB for the test user.
	_, err := r.db.ExecContext(context.Background(),
		`INSERT INTO user_preferences (user_id, key, value, updated_at)
		 VALUES (?, 'page_size', '10', datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		testUserID)
	if err != nil {
		t.Fatalf("storing page_size pref: %v", err)
	}

	ctx := middleware.WithTestUserID(context.Background(), testUserID)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/rules/"+ruleID+"/results", nil)
	req.SetPathValue("id", ruleID)
	w := httptest.NewRecorder()
	r.handleRuleResults(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	pageSize, ok := resp["page_size"].(float64)
	if !ok {
		t.Fatalf("page_size not present or not a number in response")
	}
	if int(pageSize) != 10 {
		t.Errorf("expected page_size=10 from preference, got %d", int(pageSize))
	}

	rows, ok := resp["rows"].([]any)
	if !ok {
		t.Fatalf("rows not present or not an array in response")
	}
	if len(rows) > 10 {
		t.Errorf("expected at most 10 rows, got %d", len(rows))
	}
}

// TestHandleRuleResults_QueryParamOverridesPref verifies that an explicit
// page_size query parameter takes precedence over the stored user preference.
func TestHandleRuleResults_QueryParamOverridesPref(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	const testUserID = "test-user-rule-qparam"

	ruleID := firstRuleID(t, r.ruleService)

	evaluatedAt := time.Now().UTC()
	for i := 0; i < 15; i++ {
		a := &artist.Artist{
			Name: fmt.Sprintf("QP Rule Artist %02d", i),
			Path: fmt.Sprintf("/music/qp-rule-%02d", i),
		}
		if err := artistSvc.Create(context.Background(), a); err != nil {
			t.Fatalf("creating artist %d: %v", i, err)
		}
		if err := r.ruleService.UpsertRuleResultPass(context.Background(), a.ID, ruleID, evaluatedAt); err != nil {
			t.Fatalf("upserting rule result %d: %v", i, err)
		}
	}

	_, err := r.db.ExecContext(context.Background(),
		`INSERT INTO user_preferences (user_id, key, value, updated_at)
		 VALUES (?, 'page_size', '10', datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		testUserID)
	if err != nil {
		t.Fatalf("storing page_size pref: %v", err)
	}

	// Use page_size=12 (valid, in [PageSizeMin, PageSizeMax], different from the
	// stored preference of 10) to verify the query param takes precedence.
	ctx := middleware.WithTestUserID(context.Background(), testUserID)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/rules/"+ruleID+"/results?page_size=12", nil)
	req.SetPathValue("id", ruleID)
	w := httptest.NewRecorder()
	r.handleRuleResults(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	pageSize, ok := resp["page_size"].(float64)
	if !ok {
		t.Fatalf("page_size not present or not a number in response")
	}
	if int(pageSize) != 12 {
		t.Errorf("expected page_size=12 from query param, got %d", int(pageSize))
	}

	rows, ok := resp["rows"].([]any)
	if !ok {
		t.Fatalf("rows not present or not an array in response")
	}
	if len(rows) > 12 {
		t.Errorf("expected at most 12 rows with query param override, got %d", len(rows))
	}
}

// TestHandleRulesStatus_NoScheduler_ReturnsDBValue verifies that when no rule
// scheduler is configured (ruleScheduler == nil, i.e. interval_minutes = 0),
// the GET /api/v1/rules/status endpoint still returns the real last_evaluation_at
// from the DB rather than a hard-coded nil (#1796). This is the path taken by
// every default installation where rule_schedule.interval_minutes is not set.
func TestHandleRulesStatus_NoScheduler_ReturnsDBValue(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	r.ruleScheduler = nil // simulate default no-schedule configuration

	ctx := context.Background()

	// Seed one evaluated artist so the DB has a non-nil MAX(rules_evaluated_at).
	a := &artist.Artist{
		Name: "Status Artist",
		Path: "/music/status",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("Create artist: %v", err)
	}
	evalAt := time.Date(2025, 5, 20, 12, 0, 0, 0, time.UTC)
	if err := artistSvc.MarkRulesEvaluated(ctx, a.ID, evalAt); err != nil {
		t.Fatalf("MarkRulesEvaluated: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/rules/status", nil)
	w := httptest.NewRecorder()
	r.handleRulesStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// scheduler_enabled must be false (no schedule configured).
	seEnabled, ok := resp["scheduler_enabled"].(bool)
	if !ok {
		t.Fatalf("scheduler_enabled missing or not bool: %v", resp["scheduler_enabled"])
	}
	if seEnabled {
		t.Error("scheduler_enabled: got true, want false")
	}
	// interval_minutes must be 0.
	intervalMins, ok := resp["interval_minutes"].(float64)
	if !ok {
		t.Fatalf("interval_minutes missing or not float64: %v", resp["interval_minutes"])
	}
	if int(intervalMins) != 0 {
		t.Errorf("interval_minutes: got %v, want 0", intervalMins)
	}
	// next_evaluation_at must be nil.
	if resp["next_evaluation_at"] != nil {
		t.Errorf("next_evaluation_at: got %v, want nil", resp["next_evaluation_at"])
	}
	// last_evaluation_at must be the DB value, not nil.
	rawTS, ok := resp["last_evaluation_at"].(string)
	if !ok || rawTS == "" {
		t.Fatalf("last_evaluation_at: got %v (%T), want RFC3339 string", resp["last_evaluation_at"], resp["last_evaluation_at"])
	}
	got, err := time.Parse(time.RFC3339, rawTS)
	if err != nil {
		t.Fatalf("parsing last_evaluation_at %q: %v", rawTS, err)
	}
	if !got.Equal(evalAt) {
		t.Errorf("last_evaluation_at: got %v, want %v", got, evalAt)
	}
}

// TestHandleRulesStatus_NoScheduler_DBError verifies that when the scheduler is
// nil and LatestRulesEvaluatedAt returns an error (e.g. a malformed
// rules_evaluated_at value in the DB), handleRulesStatus still returns HTTP 200
// with last_evaluation_at: null and scheduler_enabled: false instead of
// propagating the error (#1796 error-path coverage).
func TestHandleRulesStatus_NoScheduler_DBError(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	r.ruleScheduler = nil

	ctx := context.Background()

	// Seed an artist so the DB has a non-excluded row for the MAX() query.
	a := &artist.Artist{Name: "DBError Artist", Path: "/music/dberror"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Write a non-RFC3339 value directly so time.Parse fails inside
	// LatestRulesEvaluatedAt, exercising the error branch in handleRulesStatus.
	if _, err := r.db.ExecContext(ctx,
		`UPDATE artists SET rules_evaluated_at = 'INVALID-TIMESTAMP' WHERE id = ?`, a.ID); err != nil {
		t.Fatalf("ExecContext: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/rules/status", nil)
	w := httptest.NewRecorder()
	r.handleRulesStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	// Error must be swallowed; last_evaluation_at falls back to nil.
	if resp["last_evaluation_at"] != nil {
		t.Errorf("last_evaluation_at: got %v, want nil on DB parse error", resp["last_evaluation_at"])
	}
	seEnabled, ok := resp["scheduler_enabled"].(bool)
	if !ok {
		t.Fatalf("scheduler_enabled missing or not bool: %v", resp["scheduler_enabled"])
	}
	if seEnabled {
		t.Error("scheduler_enabled: got true, want false")
	}
}

// TestHandleRulesStatus_NoScheduler_NoEvaluations verifies that the nil case
// is preserved when the scheduler is nil AND no artist has been evaluated yet.
func TestHandleRulesStatus_NoScheduler_NoEvaluations(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	r.ruleScheduler = nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/rules/status", nil)
	w := httptest.NewRecorder()
	r.handleRulesStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	seEnabled, ok := resp["scheduler_enabled"].(bool)
	if !ok {
		t.Fatalf("scheduler_enabled missing or not bool: %v", resp["scheduler_enabled"])
	}
	if seEnabled {
		t.Error("scheduler_enabled: got true, want false")
	}
	if resp["last_evaluation_at"] != nil {
		t.Errorf("last_evaluation_at: got %v, want nil (no evaluations in DB)", resp["last_evaluation_at"])
	}
}
