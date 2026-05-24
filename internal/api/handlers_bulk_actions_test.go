package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/event"
	"github.com/sydlexius/stillwater/internal/rule"
)

// errBulkPipelineTest is a sentinel error used to exercise the failed-outcome
// path in applyBulkAction.
var errBulkPipelineTest = errors.New("bulk pipeline test failure")

// TestBulkAction_Cancel_NoRun verifies the cancel endpoint returns 409 when
// there is no in-flight bulk action to stop.
func TestBulkAction_Cancel_NoRun(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions/cancel", nil)
	w := httptest.NewRecorder()
	r.handleBulkActionCancel(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
}

// TestBulkAction_Cancel_Running covers the happy path: a running progress
// with a non-nil cancelFn returns 200 and invokes the cancel function.
func TestBulkAction_Cancel_Running(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	canceled := false
	cancel := func() { canceled = true }
	r.bulkActionMu.Lock()
	r.bulkActionProgress = &BulkActionProgress{Status: bulkActionRunning, Action: "run_rules", Total: 1, cancelFn: cancel}
	r.bulkActionMu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions/cancel", nil)
	w := httptest.NewRecorder()
	r.handleBulkActionCancel(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !canceled {
		t.Error("cancel function was not invoked")
	}
}

// TestBulkAction_Cancel_StaleProgress handles the case where a completed
// snapshot lingers with a nil cancelFn; cancel must 409 rather than panic.
func TestBulkAction_Cancel_StaleProgress(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	r.bulkActionMu.Lock()
	r.bulkActionProgress = &BulkActionProgress{Status: bulkActionCompleted, Action: "run_rules", Total: 1}
	r.bulkActionMu.Unlock()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions/cancel", nil)
	w := httptest.NewRecorder()
	r.handleBulkActionCancel(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (stale progress); body: %s", w.Code, w.Body.String())
	}
}

// TestBulkAction_InvalidAction rejects unknown action values with 400.
func TestBulkAction_InvalidAction(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)

	// Simulate an in-flight run by claiming the progress slot directly.
	r.bulkActionMu.Lock()
	r.bulkActionProgress = &BulkActionProgress{Status: bulkActionRunning, Action: "run_rules", Total: 5}
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// opEventRecorder collects every event.OperationProgress event the bus
// receives so runBulkAction's emit schedule can be asserted exactly.
// Concurrent appends from the bus's dispatch worker are serialized by mu.
type opEventRecorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *opEventRecorder) handle(e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Defensive copy: the bus owns the Data map and could mutate it.
	dataCopy := make(map[string]any, len(e.Data))
	for k, v := range e.Data {
		dataCopy[k] = v
	}
	cp := e
	cp.Data = dataCopy
	r.events = append(r.events, cp)
}

func (r *opEventRecorder) snapshot() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]event.Event, len(r.events))
	copy(out, r.events)
	return out
}

// attachBusRecorder swaps in a fresh event bus on the router and wires
// an opEventRecorder subscriber for event.OperationProgress. The
// returned cleanup stops the bus. Tests that need to assert on the
// emission schedule call this immediately after testRouterWithStubPipeline.
func attachBusRecorder(t *testing.T, r *Router) (*opEventRecorder, func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Generous buffer so a tight throttle loop never drops events the
	// assertions depend on.
	bus := event.NewBus(logger, 1024)
	rec := &opEventRecorder{}
	bus.Subscribe(event.OperationProgress, rec.handle)
	go bus.Start()
	r.eventBus = bus
	return rec, func() {
		bus.Stop()
		// Give the dispatch goroutine a moment to drain in-flight events.
		time.Sleep(20 * time.Millisecond)
	}
}

// extractProgressEvents filters the recorder to just the running-status
// events so assertions about throttle indices don't have to special-case
// the start event (always processed=0) vs the terminal event.
func extractProcessedIndices(evts []event.Event) []int {
	out := []int{}
	for _, e := range evts {
		if e.Data["status"] != "running" {
			continue
		}
		switch p := e.Data["processed"].(type) {
		case int:
			out = append(out, p)
		}
	}
	return out
}

// TestRunBulkAction_StartEmit verifies that even a 1-artist bulk action
// fires a "running" event with processed=0 before any per-artist work,
// followed by a terminal completion. This is the affordance that makes
// the ProgressPill appear immediately on click instead of after the
// first throttled tick.
func TestRunBulkAction_StartEmit(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	rec, stop := attachBusRecorder(t, r)
	defer stop()

	a := addTestArtist(t, artistSvc, "Single Artist")
	payload := `{"action":"run_rules","ids":["` + a.ID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	waitBulkActionCompleted(t, r)
	// Allow the terminal publish to drain through the bus's worker.
	time.Sleep(50 * time.Millisecond)

	evts := rec.snapshot()
	if len(evts) < 2 {
		t.Fatalf("emitted events = %d, want at least 2 (start + terminal); got %+v", len(evts), evts)
	}
	if evts[0].Data["status"] != "running" {
		t.Errorf("first event status = %v, want running (start emit)", evts[0].Data["status"])
	}
	if evts[0].Data["processed"] != 0 {
		t.Errorf("first event processed = %v, want 0 (start emit)", evts[0].Data["processed"])
	}
	last := evts[len(evts)-1]
	if last.Data["status"] != "completed" {
		t.Errorf("last event status = %v, want completed", last.Data["status"])
	}
}

// TestRunBulkAction_ThrottleBoundaries pins the throttle math
// (step := total/20, capped at >=1) so a refactor cannot silently change
// the emit cadence the ProgressPill animation depends on. Each case
// asserts the exact sequence of running-status processed indices.
func TestRunBulkAction_ThrottleBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		total int
		want  []int // expected running-status processed indices in order
	}{
		// total=1: step=1; emits start(0), then per-artist=1, then terminal.
		{total: 1, want: []int{0, 1}},
		// total=20: step=1; start(0) + 1..20.
		{total: 20, want: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}},
		// total=21: step=1 (21/20=1); start(0) + 1..21.
		{total: 21, want: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21}},
		// total=40: step=2; start(0) + 2,4,...,40.
		{total: 40, want: []int{0, 2, 4, 6, 8, 10, 12, 14, 16, 18, 20, 22, 24, 26, 28, 30, 32, 34, 36, 38, 40}},
		// total=100: step=5; start(0) + 5,10,...,100.
		{total: 100, want: []int{0, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60, 65, 70, 75, 80, 85, 90, 95, 100}},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("total=%d", tc.total), func(t *testing.T) {
			t.Parallel()
			stub := &stubPipeline{}
			r, artistSvc := testRouterWithStubPipeline(t, stub)
			rec, stop := attachBusRecorder(t, r)
			defer stop()
			ids := make([]string, 0, tc.total)
			for i := 0; i < tc.total; i++ {
				a := addTestArtist(t, artistSvc, fmt.Sprintf("Throttle Artist %d-%d", tc.total, i))
				ids = append(ids, a.ID)
			}
			idsJSON, _ := json.Marshal(ids)
			payload := `{"action":"run_rules","ids":` + string(idsJSON) + `}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.handleBulkAction(w, req)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
			}
			waitBulkActionCompleted(t, r)
			time.Sleep(100 * time.Millisecond)
			got := extractProcessedIndices(rec.snapshot())
			if !equalInts(got, tc.want) {
				t.Errorf("running indices = %v, want %v", got, tc.want)
			}
		})
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRunBulkAction_TerminalCompleted locks in the success terminal: when
// every artist succeeds the final emit must carry status=completed
// (driving the green check + auto-dismiss in the pill).
func TestRunBulkAction_TerminalCompleted(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	rec, stop := attachBusRecorder(t, r)
	defer stop()
	a := addTestArtist(t, artistSvc, "Completed Artist")
	payload := `{"action":"run_rules","ids":["` + a.ID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	waitBulkActionCompleted(t, r)
	time.Sleep(50 * time.Millisecond)
	evts := rec.snapshot()
	last := evts[len(evts)-1]
	if last.Data["status"] != "completed" {
		t.Errorf("terminal status = %v, want completed; events=%+v", last.Data["status"], evts)
	}
}

// TestRunBulkAction_TerminalFailed: when at least one artist fails the
// terminal status must be "failed" (driving the red sticky pill the user
// has to manually dismiss). The cancel-after-failed precedence test
// below verifies cancel still wins; this test pins the failure default.
func TestRunBulkAction_TerminalFailed(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			return nil, errBulkPipelineTest
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	rec, stop := attachBusRecorder(t, r)
	defer stop()
	a := addTestArtist(t, artistSvc, "Failed Artist")
	payload := `{"action":"run_rules","ids":["` + a.ID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	// The progress reaches Status=completed even when every per-artist
	// run failed (the goroutine fully processed every ID). Wait on that.
	waitBulkActionCompleted(t, r)
	time.Sleep(50 * time.Millisecond)
	evts := rec.snapshot()
	last := evts[len(evts)-1]
	if last.Data["status"] != "failed" {
		t.Errorf("terminal status = %v, want failed; events=%+v", last.Data["status"], evts)
	}
}

// TestRunBulkAction_TerminalCanceledWithFailures pins the cancel-after-
// failed precedence the current code (lines 519-530 of
// handlers_bulk_actions.go) implements: when a cancel lands mid-run AND
// some artists have already failed, the terminal status is "canceled",
// not "failed". This avoids the misleading red sticky pill for a run
// the user explicitly aborted. A future change that flipped this
// precedence would silently degrade the UX, so the test acts as a pin.
func TestRunBulkAction_TerminalCanceledWithFailures(t *testing.T) {
	t.Parallel()

	// Stub that fails the first artist, then blocks indefinitely on the
	// second so the test can cancel between the two.
	var firstDone sync.WaitGroup
	firstDone.Add(1)
	var seen atomic.Int32
	cancelGate := make(chan struct{})
	stub := &stubPipeline{
		runForArtistFn: func(ctx context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			n := seen.Add(1)
			if n == 1 {
				firstDone.Done()
				return nil, errBulkPipelineTest // first artist fails
			}
			// Subsequent artists block until the test cancels, then
			// return promptly so the loop's cancel-check on the next
			// iteration finalizes as "canceled".
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-cancelGate:
				return &rule.RunResult{}, nil
			}
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	rec, stop := attachBusRecorder(t, r)
	defer stop()
	a1 := addTestArtist(t, artistSvc, "Cancel-Mix Artist A")
	a2 := addTestArtist(t, artistSvc, "Cancel-Mix Artist B")
	a3 := addTestArtist(t, artistSvc, "Cancel-Mix Artist C")
	payload := `{"action":"run_rules","ids":["` + a1.ID + `","` + a2.ID + `","` + a3.ID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}

	// Wait for the first artist's failure to land before canceling so
	// progress.Failed is non-zero when the cancel observes.
	firstDone.Wait()

	cancelReq := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions/cancel", nil)
	cancelW := httptest.NewRecorder()
	r.handleBulkActionCancel(cancelW, cancelReq)
	if cancelW.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200; body: %s", cancelW.Code, cancelW.Body.String())
	}

	// Release any blocked stub goroutines so the run can finalize.
	close(cancelGate)

	// Wait for the canceled terminal state. waitBulkActionCompleted polls
	// for Status=="completed" which won't fire here; spin manually.
	deadline := time.Now().Add(5 * time.Second)
	var finalStatus bulkActionStatus
	var failed int
	for time.Now().Before(deadline) {
		r.bulkActionMu.RLock()
		p := r.bulkActionProgress
		r.bulkActionMu.RUnlock()
		if p != nil {
			p.mu.RLock()
			s := p.Status
			f := p.Failed
			p.mu.RUnlock()
			if s != bulkActionRunning {
				finalStatus = s
				failed = f
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if finalStatus != bulkActionCanceled {
		t.Errorf("final progress status = %q, want %q (cancel must supersede failed)", finalStatus, bulkActionCanceled)
	}
	if failed < 1 {
		t.Errorf("progress.Failed = %d, want >= 1 (first artist failed before cancel)", failed)
	}

	time.Sleep(100 * time.Millisecond)
	evts := rec.snapshot()
	if len(evts) == 0 {
		t.Fatal("no events recorded")
	}
	last := evts[len(evts)-1]
	if last.Data["status"] != "canceled" {
		t.Errorf("terminal event status = %v, want canceled (cancel supersedes failed); events=%+v", last.Data["status"], evts)
	}
}

// TestBulkAction_Lock_Success exercises the lock branch end-to-end. A freshly
// created artist is unlocked; one bulk-lock request must mark it locked,
// land Succeeded=1 on the snapshot, and persist the change in the artist
// service.
func TestBulkAction_Lock_Success(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithIdentify(t)
	a := addTestArtist(t, artistSvc, "Lock Artist A")

	payload := `{"action":"lock","ids":["` + a.ID + `"]}`
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
	p.mu.RLock()
	succeeded := p.Succeeded
	skipped := p.Skipped
	failed := p.Failed
	p.mu.RUnlock()

	if succeeded != 1 || skipped != 0 || failed != 0 {
		t.Errorf("counts = (succeeded=%d skipped=%d failed=%d), want (1,0,0)", succeeded, skipped, failed)
	}

	// Verify the lock landed on the persisted record.
	got, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("re-fetching artist: %v", err)
	}
	if !got.Locked {
		t.Errorf("artist.Locked = false after bulk lock; want true")
	}
}

// TestBulkAction_Lock_Idempotent verifies that locking an already-locked
// artist counts as Skipped, not Succeeded. This matches the per-artist
// POST /artists/{id}/lock idempotency and prevents a misleading
// "N artists locked" toast when the operator picked a mix of locked +
// unlocked.
func TestBulkAction_Lock_Idempotent(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithIdentify(t)
	a := addTestArtist(t, artistSvc, "Already Locked")
	// Pre-lock the artist so the bulk path sees Locked=true.
	if err := artistSvc.Lock(context.Background(), a.ID, "user"); err != nil {
		t.Fatalf("pre-locking: %v", err)
	}

	payload := `{"action":"lock","ids":["` + a.ID + `"]}`
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
	p.mu.RLock()
	succeeded := p.Succeeded
	skipped := p.Skipped
	p.mu.RUnlock()
	if succeeded != 0 || skipped != 1 {
		t.Errorf("counts = (succeeded=%d skipped=%d), want (0,1) for already-locked", succeeded, skipped)
	}
}

// TestBulkAction_Unlock_Success exercises the unlock branch end-to-end on
// a previously-locked artist.
func TestBulkAction_Unlock_Success(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithIdentify(t)
	a := addTestArtist(t, artistSvc, "Unlock Artist")
	if err := artistSvc.Lock(context.Background(), a.ID, "user"); err != nil {
		t.Fatalf("pre-locking: %v", err)
	}

	payload := `{"action":"unlock","ids":["` + a.ID + `"]}`
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
	p.mu.RLock()
	succeeded := p.Succeeded
	skipped := p.Skipped
	p.mu.RUnlock()
	if succeeded != 1 || skipped != 0 {
		t.Errorf("counts = (succeeded=%d skipped=%d), want (1,0)", succeeded, skipped)
	}

	got, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("re-fetching artist: %v", err)
	}
	if got.Locked {
		t.Errorf("artist.Locked = true after bulk unlock; want false")
	}
}

// TestBulkAction_Unlock_Idempotent verifies that unlocking a not-locked
// artist counts as Skipped.
func TestBulkAction_Unlock_Idempotent(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithIdentify(t)
	a := addTestArtist(t, artistSvc, "Already Unlocked")

	payload := `{"action":"unlock","ids":["` + a.ID + `"]}`
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
	p.mu.RLock()
	succeeded := p.Succeeded
	skipped := p.Skipped
	p.mu.RUnlock()
	if succeeded != 0 || skipped != 1 {
		t.Errorf("counts = (succeeded=%d skipped=%d), want (0,1) for already-unlocked", succeeded, skipped)
	}
}

// TestBulkAction_LockUnlock_ConcurrentReject confirms a lock request returns
// 409 when another bulk action is already in flight, matching the existing
// singleton-slot semantics that fix-all also follows.
func TestBulkAction_LockUnlock_ConcurrentReject(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	r.bulkActionMu.Lock()
	r.bulkActionProgress = &BulkActionProgress{Status: bulkActionRunning, Action: "lock", Total: 5}
	r.bulkActionMu.Unlock()

	body := strings.NewReader(`{"action":"unlock","ids":["abc123"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
}
