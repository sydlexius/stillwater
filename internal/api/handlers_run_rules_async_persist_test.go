package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// pollRunStatusPersistFailures drives GET /rules/run-all/status until the run
// reaches a terminal state, then returns the reported persist_failures. Both
// the run-all and single-rule async handlers publish into the same r.ruleRun
// slot and are polled through this one status endpoint.
func pollRunStatusPersistFailures(t *testing.T, r *Router) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		sreq := httptest.NewRequest(http.MethodGet, "/api/v1/rules/run-all/status", nil)
		sw := httptest.NewRecorder()
		r.handleRunAllRulesStatus(sw, sreq)

		var body struct {
			Status          string `json:"status"`
			PersistFailures int    `json:"persist_failures"`
		}
		if err := json.Unmarshal(sw.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding run status: %v (body: %s)", err, sw.Body.String())
		}
		if body.Status == "completed" || body.Status == "failed" {
			return body.PersistFailures
		}
		if time.Now().After(deadline) {
			t.Fatalf("async run did not reach a terminal status within 3s (last: %q)", body.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHandleRunAllRules_AsyncPersistFailureSurfaced covers the run-all async
// completion path for #2724: the background goroutine must carry the
// write-failure count into the polled status. Before the fix, an unattended
// run that lost every write showed "completed" with no failure signal.
func TestHandleRunAllRules_AsyncPersistFailureSurfaced(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithPipeline(t)
	r.pipeline = &persistFailPipeline{persistFailures: 2, violationsFound: 5}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rules/run-all", nil)
	w := httptest.NewRecorder()
	r.handleRunAllRules(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (async run should accept immediately); body: %s",
			w.Code, http.StatusAccepted, w.Body.String())
	}

	if got := pollRunStatusPersistFailures(t, r); got != 2 {
		t.Errorf("polled persist_failures = %d, want 2 -- the async run-all completion "+
			"did not carry the write-failure count into the status", got)
	}
}

// TestHandleRunRule_AsyncPersistFailureSurfaced is the single-rule counterpart:
// its background goroutine shares the same status slot and must likewise
// publish persist_failures.
func TestHandleRunRule_AsyncPersistFailureSurfaced(t *testing.T) {
	t.Parallel()
	r, _, ruleSvc := testRouterWithPipelineFull(t)
	// Any real rule ID satisfies the handler's existence check before it
	// dispatches to the (stubbed) pipeline.
	rules, err := ruleSvc.List(context.Background())
	if err != nil || len(rules) == 0 {
		t.Fatalf("listing rules for a valid id: %v (n=%d)", err, len(rules))
	}
	ruleID := rules[0].ID
	r.pipeline = &persistFailPipeline{persistFailures: 1, violationsFound: 3}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/rules/"+ruleID+"/run", nil)
	req.SetPathValue("id", ruleID)
	w := httptest.NewRecorder()
	r.handleRunRule(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	if got := pollRunStatusPersistFailures(t, r); got != 1 {
		t.Errorf("polled persist_failures = %d, want 1 -- the async single-rule completion "+
			"did not carry the write-failure count into the status", got)
	}
}
