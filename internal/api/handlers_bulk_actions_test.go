package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
)

// errBulkPipelineTest is a sentinel error used to exercise the failed-outcome
// path in applyBulkAction.
var errBulkPipelineTest = errors.New("bulk pipeline test failure")

// TestBulkAction_InvalidAction rejects unknown action values with 400.
func TestBulkAction_InvalidAction(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)
	body := strings.NewReader(`{"action":"delete_everything","ids":["abc"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestBulkAction_EmptyIDs rejects empty id lists with 400.
func TestBulkAction_EmptyIDs(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)
	body := strings.NewReader(`{"action":"run_rules","ids":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestBulkAction_InvalidIDFormat rejects IDs that fail the format regex.
func TestBulkAction_InvalidIDFormat(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)
	body := strings.NewReader(`{"action":"run_rules","ids":["../../etc/passwd"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestBulkAction_ConcurrentReject ensures a second bulk action while one is
// already running returns 409 Conflict, matching the fix-all pattern.
func TestBulkAction_ConcurrentReject(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	// Simulate an in-flight run by claiming the progress slot directly.
	r.bulkActionMu.Lock()
	r.bulkActionProgress = &BulkActionProgress{Status: "running", Action: "run_rules", Total: 5}
	r.bulkActionMu.Unlock()

	body := strings.NewReader(`{"action":"run_rules","ids":["abc123"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "running" {
		t.Errorf("status = %v, want running", resp["status"])
	}
}

// TestBulkActionStatus_Idle returns idle when no progress is set.
func TestBulkActionStatus_Idle(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/bulk-actions/status", nil)
	w := httptest.NewRecorder()

	r.handleBulkActionStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "idle" {
		t.Errorf("status = %v, want idle", resp["status"])
	}
}

// TestBulkAction_TooManyIDs bounds the request size so a single call cannot
// monopolize the singleton slot indefinitely.
func TestBulkAction_TooManyIDs(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	var b strings.Builder
	b.WriteString(`{"action":"run_rules","ids":[`)
	for i := 0; i < MaxBulkActionIDs+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"abc"`)
	}
	b.WriteString(`]}`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(b.String()))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// TestBulkAction_ScanRequiresPipeline verifies that scan is gated at the
// availability check when the rule pipeline is not configured. The previous
// implementation relied on applyBulkAction to silently skip, which returned a
// misleading 202 + completed snapshot; the fix routes this case through the
// upfront 503 path so the progress slot is released immediately.
func TestBulkAction_ScanRequiresPipeline(t *testing.T) {
	// testRouterWithIdentify wires an artistService but no pipeline, so scan
	// must fail availability.
	r, _, _ := testRouterWithIdentify(t)

	body := strings.NewReader(`{"action":"scan","ids":["abc123"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}

	r.bulkActionMu.RLock()
	progress := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	if progress != nil {
		t.Errorf("bulkActionProgress not released after 503; got %+v", progress)
	}
}

// TestBulkAction_RunRulesRequiresPipeline locks in the 503 + slot-release
// path for run_rules when the pipeline is absent.
func TestBulkAction_RunRulesRequiresPipeline(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)

	body := strings.NewReader(`{"action":"run_rules","ids":["abc123"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}
	r.bulkActionMu.RLock()
	progress := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	if progress != nil {
		t.Errorf("bulkActionProgress not released after 503; got %+v", progress)
	}
}

// waitBulkActionCompleted polls the progress snapshot the same way fix-all
// tests do, returning once the goroutine flags completion or the deadline
// elapses.
func waitBulkActionCompleted(t *testing.T, r *Router) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.bulkActionMu.RLock()
		p := r.bulkActionProgress
		r.bulkActionMu.RUnlock()
		if p != nil {
			p.mu.RLock()
			done := p.Status == "completed"
			p.mu.RUnlock()
			if done {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("bulk action did not reach completed within 5s")
}

// TestBulkAction_SuccessDedupKickoff posts run_rules with a duplicate ID in
// the payload and verifies:
//   - the handler returns 202 with deduped total (2, not 3)
//   - the subsequent /status call reports total=2 and a running/completed
//     status (either is valid depending on goroutine scheduling)
//   - the background goroutine finishes cleanly and the pipeline is invoked
//     exactly once per unique ID (no arbitrary sleeps; mirrors the fix-all
//     polling pattern)
func TestBulkAction_SuccessDedupKickoff(t *testing.T) {
	var calls atomic.Int32
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			calls.Add(1)
			return &rule.RunResult{ArtistsProcessed: 1}, nil
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)

	a1 := addTestArtist(t, artistSvc, "Dedup Artist A")
	a2 := addTestArtist(t, artistSvc, "Dedup Artist B")

	payload := `{"action":"run_rules","ids":["` + a1.ID + `","` + a1.ID + `","` + a2.ID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["total"] != float64(2) {
		t.Errorf("total = %v, want 2 (deduped)", resp["total"])
	}
	if resp["status"] != "running" {
		t.Errorf("status = %v, want running", resp["status"])
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/artists/bulk-actions/status", nil)
	statusW := httptest.NewRecorder()
	r.handleBulkActionStatus(statusW, statusReq)
	if statusW.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", statusW.Code)
	}
	var status map[string]any
	if err := json.NewDecoder(statusW.Body).Decode(&status); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	if status["total"] != float64(2) {
		t.Errorf("status.total = %v, want 2", status["total"])
	}
	if s := status["status"]; s != "running" && s != "completed" {
		t.Errorf("status.status = %v, want running or completed", s)
	}

	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	if p == nil {
		t.Fatalf("expected progress snapshot, got nil")
	}
	p.mu.RLock()
	finalStatus := p.Status
	finalTotal := p.Total
	finalProcessed := p.Processed
	finalSucceeded := p.Succeeded
	p.mu.RUnlock()

	if finalStatus != "completed" {
		t.Errorf("final status = %q, want completed", finalStatus)
	}
	if finalTotal != 2 {
		t.Errorf("final total = %d, want 2", finalTotal)
	}
	if finalProcessed != 2 {
		t.Errorf("final processed = %d, want 2", finalProcessed)
	}
	if finalSucceeded != 2 {
		t.Errorf("final succeeded = %d, want 2", finalSucceeded)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("pipeline invocations = %d, want 2", got)
	}
}

// TestBulkAction_Scan_Success exercises the scan action end-to-end through
// the pipeline so the success branch of applyBulkAction is covered.
func TestBulkAction_Scan_Success(t *testing.T) {
	stub := &stubPipeline{}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Scan Artist")

	payload := `{"action":"scan","ids":["` + a.ID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}

	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	if p == nil {
		t.Fatalf("expected progress snapshot, got nil")
	}
	snap := p.snapshot()
	if snap["status"] != "completed" {
		t.Errorf("status = %v, want completed", snap["status"])
	}
	if snap["succeeded"] != 1 {
		t.Errorf("succeeded = %v, want 1", snap["succeeded"])
	}
}

// TestBulkAction_MissingArtistSkipped exercises the not-found branch in the
// per-artist loop so coverage reflects the skipped outcome.
func TestBulkAction_MissingArtistSkipped(t *testing.T) {
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)

	payload := `{"action":"run_rules","ids":["nonexistent-id"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleBulkAction(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}

	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	if p == nil {
		t.Fatalf("expected progress snapshot, got nil")
	}
	snap := p.snapshot()
	if snap["status"] != "completed" {
		t.Errorf("status = %v, want completed", snap["status"])
	}
	if snap["skipped"] != 1 {
		t.Errorf("skipped = %v, want 1", snap["skipped"])
	}
}

// TestBulkAction_FetchImages_Success covers the fetch_images branch of
// applyBulkAction (same pipeline hop as run_rules, but a distinct action
// label so progress reports the right value).
func TestBulkAction_FetchImages_Success(t *testing.T) {
	stub := &stubPipeline{}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Fetch Images Artist")

	payload := `{"action":"fetch_images","ids":["` + a.ID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}

	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	snap := p.snapshot()
	if snap["action"] != "fetch_images" {
		t.Errorf("action = %v, want fetch_images", snap["action"])
	}
	if snap["succeeded"] != 1 {
		t.Errorf("succeeded = %v, want 1", snap["succeeded"])
	}
}

// TestBulkAction_PipelineError marks the per-artist work as failed when the
// pipeline returns an error so operators see the real outcome.
func TestBulkAction_PipelineError(t *testing.T) {
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			return nil, errBulkPipelineTest
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Pipeline Error Artist")

	payload := `{"action":"run_rules","ids":["` + a.ID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	snap := p.snapshot()
	if snap["failed"] != 1 {
		t.Errorf("failed = %v, want 1", snap["failed"])
	}
}

// TestBulkAction_ReIdentifyNoOrchestrator verifies the skipped path when
// neither an orchestrator nor a connection index is available.
func TestBulkAction_ReIdentifyNoOrchestrator(t *testing.T) {
	// The stub-pipeline router has no orchestrator and no connections, so the
	// per-artist re-identify branch will return bulkOutcomeSkipped.
	stub := &stubPipeline{}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Re-Identify Artist")

	payload := `{"action":"re_identify","ids":["` + a.ID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	waitBulkActionCompleted(t, r)

	r.bulkActionMu.RLock()
	p := r.bulkActionProgress
	r.bulkActionMu.RUnlock()
	snap := p.snapshot()
	if snap["skipped"] != 1 {
		t.Errorf("skipped = %v, want 1", snap["skipped"])
	}
}

// TestBulkAction_StatusAfterCompletion checks the snapshot semantics: a
// completed run must still be visible via /status (not idle) so callers can
// see per-action outcome without an active job.
func TestBulkAction_StatusAfterCompletion(t *testing.T) {
	stub := &stubPipeline{}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Status Artist")

	payload := `{"action":"run_rules","ids":["` + a.ID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}

	waitBulkActionCompleted(t, r)

	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/artists/bulk-actions/status", nil)
	statusW := httptest.NewRecorder()
	r.handleBulkActionStatus(statusW, statusReq)
	if statusW.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", statusW.Code)
	}
	var snap map[string]any
	if err := json.NewDecoder(statusW.Body).Decode(&snap); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if snap["status"] != "completed" {
		t.Errorf("status = %v, want completed", snap["status"])
	}
	if snap["action"] != "run_rules" {
		t.Errorf("action = %v, want run_rules", snap["action"])
	}
}
