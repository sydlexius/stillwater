package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
)

// noopRevert is a RevertFunc that records whether it was called.
type noopRevert struct{ called bool }

func (n *noopRevert) fn(_ context.Context) error {
	n.called = true
	return nil
}

func TestHandleFixViolation_Success(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{
		fixViolationFn: func(_ context.Context, _ string) (*rule.FixResult, error) {
			return &rule.FixResult{RuleID: "nfo_exists", Fixed: true, Message: "NFO created"}, nil
		},
	}
	r, _ := testRouterWithStubPipeline(t, stub)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/test-id/fix", nil)
	req.SetPathValue("id", "test-id")
	w := httptest.NewRecorder()

	r.handleFixViolation(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "fixed" {
		t.Errorf("status = %v, want fixed", resp["status"])
	}
	if resp["message"] != "NFO created" {
		t.Errorf("message = %v, want 'NFO created'", resp["message"])
	}
}

func TestHandleFixViolation_NotFixable(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{
		fixViolationFn: func(_ context.Context, _ string) (*rule.FixResult, error) {
			return &rule.FixResult{RuleID: "nfo_exists", Fixed: false, Message: "not fixable"}, nil
		},
	}
	r, _ := testRouterWithStubPipeline(t, stub)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/test-id/fix", nil)
	req.SetPathValue("id", "test-id")
	w := httptest.NewRecorder()

	r.handleFixViolation(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "failed" {
		t.Errorf("status = %v, want failed", resp["status"])
	}
}

func TestHandleFixViolation_PipelineError(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{
		fixViolationFn: func(_ context.Context, _ string) (*rule.FixResult, error) {
			return nil, fmt.Errorf("db error")
		},
	}
	r, _ := testRouterWithStubPipeline(t, stub)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/test-id/fix", nil)
	req.SetPathValue("id", "test-id")
	w := httptest.NewRecorder()

	r.handleFixViolation(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestHandleFixAll_StartsJob(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	ctx := context.Background()

	// Create real artists so violations are not treated as orphaned.
	a1 := addTestArtist(t, artistSvc, "Alpha")
	a2 := addTestArtist(t, artistSvc, "Beta")

	// Seed fixable violations.
	for _, v := range []*rule.RuleViolation{
		{RuleID: rule.RuleNFOExists, ArtistID: a1.ID, ArtistName: a1.Name,
			Severity: "error", Message: "missing nfo", Fixable: true, Status: rule.ViolationStatusOpen},
		{RuleID: rule.RuleThumbExists, ArtistID: a2.ID, ArtistName: a2.Name,
			Severity: "warning", Message: "missing thumb", Fixable: true, Status: rule.ViolationStatusOpen},
	} {
		if err := r.ruleService.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("seeding violation: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/fix-all", nil)
	w := httptest.NewRecorder()

	r.handleFixAll(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusAccepted, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "running" {
		t.Errorf("status = %v, want running", resp["status"])
	}
	if resp["total"] != float64(2) {
		t.Errorf("total = %v, want 2", resp["total"])
	}

	// Wait for the background goroutine to finish so cleanup does not race
	// with router-state mutations after the test returns. Fail loudly if the
	// worker never settles -- a silent timeout would reintroduce the very
	// flake this wait is meant to remove.
	deadline := time.Now().Add(5 * time.Second)
	settled := false
	for time.Now().Before(deadline) {
		r.fixAllMu.RLock()
		p := r.fixAllProgress
		r.fixAllMu.RUnlock()
		if p == nil {
			settled = true
			break
		}
		p.mu.RLock()
		done := p.Status == "completed"
		p.mu.RUnlock()
		if done {
			settled = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !settled {
		t.Fatal("timed out waiting for fix-all background job to finish")
	}
}

func TestHandleFixAll_NoFixable(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)
	ctx := context.Background()

	// Seed only non-fixable violations.
	v := &rule.RuleViolation{
		RuleID: rule.RuleBioExists, ArtistID: "a1", ArtistName: "Alpha",
		Severity: "info", Message: "missing bio", Fixable: false, Status: rule.ViolationStatusOpen,
	}
	if err := r.ruleService.UpsertViolation(ctx, v); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/fix-all", nil)
	w := httptest.NewRecorder()

	r.handleFixAll(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "completed" {
		t.Errorf("status = %v, want completed", resp["status"])
	}
}

func TestHandleFixAllStatus_NoJob(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/fix-all/status", nil)
	w := httptest.NewRecorder()

	r.handleFixAllStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "idle" {
		t.Errorf("status = %v, want idle", resp["status"])
	}
}

func TestHandleFixAllStatus_WithProgress(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)

	// Simulate an in-progress fix-all by setting progress directly.
	progress := &FixAllProgress{
		Status:    "running",
		Total:     10,
		Processed: 5,
		Fixed:     3,
		Skipped:   1,
		Failed:    1,
	}
	r.fixAllMu.Lock()
	r.fixAllProgress = progress
	r.fixAllMu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/fix-all/status", nil)
	w := httptest.NewRecorder()

	r.handleFixAllStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "running" {
		t.Errorf("status = %v, want running", resp["status"])
	}
	if resp["total"] != float64(10) {
		t.Errorf("total = %v, want 10", resp["total"])
	}
	if resp["fixed"] != float64(3) {
		t.Errorf("fixed = %v, want 3", resp["fixed"])
	}
}

func TestHandleFixAll_Completion(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{
		fixViolationFn: func(_ context.Context, _ string) (*rule.FixResult, error) {
			return &rule.FixResult{Fixed: true, Message: "fixed"}, nil
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)

	// Create a real artist so the violation is not treated as orphaned.
	a := addTestArtist(t, artistSvc, "Fix Completion Artist")

	// Seed a fixable violation via the rule service.
	v := &rule.RuleViolation{
		RuleID: rule.RuleNFOExists, ArtistID: a.ID, ArtistName: a.Name,
		Severity: "error", Message: "missing nfo", Fixable: true, Status: rule.ViolationStatusOpen,
	}
	if err := r.ruleService.UpsertViolation(context.Background(), v); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/fix-all", nil)
	w := httptest.NewRecorder()
	r.handleFixAll(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}

	// Poll until completed (max 5s).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.fixAllMu.RLock()
		p := r.fixAllProgress
		r.fixAllMu.RUnlock()
		if p != nil {
			p.mu.RLock()
			done := p.Status == "completed"
			p.mu.RUnlock()
			if done {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify final status.
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/fix-all/status", nil)
	statusW := httptest.NewRecorder()
	r.handleFixAllStatus(statusW, statusReq)

	var resp map[string]any
	if err := json.NewDecoder(statusW.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "completed" {
		t.Errorf("status = %v, want completed", resp["status"])
	}
	if resp["fixed"] != float64(1) {
		t.Errorf("fixed = %v, want 1", resp["fixed"])
	}
}

// --- Undo handler tests ---

func TestHandleUndoFix_Success(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)

	// Register a noop undo entry directly into the store.
	nr := &noopRevert{}
	undoID := r.undoStore.Register("v-undo-1", nr.fn)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fix-undo/"+undoID, nil)
	req.SetPathValue("undoId", undoID)
	w := httptest.NewRecorder()

	r.handleUndoFix(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !nr.called {
		t.Error("expected revert function to have been called")
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "reverted" {
		t.Errorf("status = %v, want reverted", resp["status"])
	}
}

func TestHandleUndoFix_Expired(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)

	// Register an entry and then expire it manually.
	nr := &noopRevert{}
	undoID := r.undoStore.Register("v-undo-exp", nr.fn)
	r.undoStore.ForceExpire(undoID)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fix-undo/"+undoID, nil)
	req.SetPathValue("undoId", undoID)
	w := httptest.NewRecorder()

	r.handleUndoFix(w, req)

	if w.Code != http.StatusGone {
		t.Errorf("status = %d, want %d (Gone)", w.Code, http.StatusGone)
	}
	if nr.called {
		t.Error("expected revert function NOT to have been called for expired entry")
	}
}

func TestHandleUndoFix_NotFound(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fix-undo/nonexistent", nil)
	req.SetPathValue("undoId", "nonexistent")
	w := httptest.NewRecorder()

	r.handleUndoFix(w, req)

	if w.Code != http.StatusGone {
		t.Errorf("status = %d, want %d (Gone)", w.Code, http.StatusGone)
	}
}

func TestHandleUndoFix_AlreadyUsed(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)

	nr := &noopRevert{}
	undoID := r.undoStore.Register("v-undo-used", nr.fn)

	// First call: succeeds.
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/fix-undo/"+undoID, nil)
	req1.SetPathValue("undoId", undoID)
	w1 := httptest.NewRecorder()
	r.handleUndoFix(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first undo: status = %d, want %d", w1.Code, http.StatusOK)
	}

	// Second call: undo already consumed.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/fix-undo/"+undoID, nil)
	req2.SetPathValue("undoId", undoID)
	w2 := httptest.NewRecorder()
	r.handleUndoFix(w2, req2)
	if w2.Code != http.StatusGone {
		t.Errorf("second undo: status = %d, want %d (Gone)", w2.Code, http.StatusGone)
	}
}

func TestHandleUndoFix_ConcurrentSameID(t *testing.T) {
	t.Parallel()
	// Two goroutines calling undo with the same ID concurrently: exactly one
	// should get 200 and the other should get 410. The UndoStore.Pop is
	// atomic (mutex-protected), so this verifies no double-revert.
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)

	nr := &noopRevert{}
	undoID := r.undoStore.Register("v-concurrent-undo", nr.fn)

	var wg sync.WaitGroup
	codes := make([]int, 2)

	for i := range 2 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/fix-undo/"+undoID, nil)
			req.SetPathValue("undoId", undoID)
			w := httptest.NewRecorder()
			r.handleUndoFix(w, req)
			codes[idx] = w.Code
		}(i)
	}
	wg.Wait()

	got200 := 0
	got410 := 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			got200++
		case http.StatusGone:
			got410++
		default:
			t.Errorf("unexpected status code: %d", code)
		}
	}
	if got200 != 1 {
		t.Errorf("expected exactly 1 status 200, got %d", got200)
	}
	if got410 != 1 {
		t.Errorf("expected exactly 1 status 410, got %d", got410)
	}
}

func TestHandleFixViolation_NoUndo_PathlessArtist(t *testing.T) {
	t.Parallel()
	// A fix that succeeds for a pathless artist (no on-disk directory) should
	// not return undo_id because there are no files to snapshot or revert.
	stub := &stubPipeline{
		fixViolationFn: func(_ context.Context, _ string) (*rule.FixResult, error) {
			return &rule.FixResult{RuleID: "nfo_exists", Fixed: true, Message: "NFO created"}, nil
		},
	}
	r, _ := testRouterWithStubPipeline(t, stub)

	// Create a pathless artist (path is empty).
	a := &artist.Artist{
		Name:     "Pathless Artist",
		SortName: "Pathless Artist",
		Type:     "group",
		Path:     "",
		Genres:   []string{"Rock"},
	}
	if err := r.artistService.Create(context.Background(), a); err != nil {
		t.Fatalf("creating pathless artist: %v", err)
	}

	// Seed a violation for the pathless artist.
	v := &rule.RuleViolation{
		RuleID:     rule.RuleNFOExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "error",
		Message:    "missing nfo",
		Fixable:    true,
		Status:     rule.ViolationStatusOpen,
	}
	if err := r.ruleService.UpsertViolation(context.Background(), v); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}
	violations, err := r.ruleService.ListViolationsFiltered(context.Background(), rule.ViolationListParams{Status: "active"})
	if err != nil || len(violations) == 0 {
		t.Fatalf("listing violations: %v (count=%d)", err, len(violations))
	}
	violationID := violations[0].ID

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+violationID+"/fix", nil)
	req.SetPathValue("id", violationID)
	w := httptest.NewRecorder()

	r.handleFixViolation(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "fixed" {
		t.Errorf("status = %v, want fixed", resp["status"])
	}
	if resp["undo_id"] != nil {
		t.Errorf("undo_id = %v, want nil for pathless artist", resp["undo_id"])
	}
	if resp["undo_expires_in"] != nil {
		t.Errorf("undo_expires_in = %v, want nil for pathless artist", resp["undo_expires_in"])
	}
}

func TestHandleFixViolation_ReturnsUndoID(t *testing.T) {
	t.Parallel()
	// A fix that succeeds for an artist with a path should return undo_id
	// and undo_expires_in in the response.
	stub := &stubPipeline{
		fixViolationFn: func(_ context.Context, _ string) (*rule.FixResult, error) {
			return &rule.FixResult{RuleID: "nfo_exists", Fixed: true, Message: "NFO created"}, nil
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)

	a := addTestArtist(t, artistSvc, "Undo Test Artist")

	// Seed a violation for the artist so capturePreFixState can look it up.
	v := &rule.RuleViolation{
		RuleID:     rule.RuleNFOExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "error",
		Message:    "missing nfo",
		Fixable:    true,
		Status:     rule.ViolationStatusOpen,
	}
	if err := r.ruleService.UpsertViolation(context.Background(), v); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}
	// Retrieve the persisted violation to get its generated ID.
	violations, err := r.ruleService.ListViolationsFiltered(context.Background(), rule.ViolationListParams{Status: "active"})
	if err != nil || len(violations) == 0 {
		t.Fatalf("listing violations: %v (count=%d)", err, len(violations))
	}
	violationID := violations[0].ID

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+violationID+"/fix", nil)
	req.SetPathValue("id", violationID)
	w := httptest.NewRecorder()

	r.handleFixViolation(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp["status"] != "fixed" {
		t.Errorf("status = %v, want fixed", resp["status"])
	}
	if resp["undo_id"] == nil {
		t.Error("expected undo_id to be present for path-bearing artist fix")
	}
	if resp["undo_expires_in"] == nil {
		t.Error("expected undo_expires_in to be present for path-bearing artist fix")
	} else if resp["undo_expires_in"] != float64(int(rule.UndoWindowDuration.Seconds())) {
		t.Errorf("undo_expires_in = %v, want %v", resp["undo_expires_in"], rule.UndoWindowDuration.Seconds())
	}
}

// TestHandleFixViolation_HTMX_FailureCarriesMessage pins the user-facing
// error contract: when a fix attempt does not resolve the violation, the
// HTMX response must be 422 with the rule-fixer's message in the body so
// the global htmx:responseError listener (in layout.templ) can surface it
// as a toast. Without this guard a future refactor could silently regress
// to status-only errors and the dashboard card would fail without
// explanation.
func TestHandleFixViolation_HTMX_FailureCarriesMessage(t *testing.T) {
	t.Parallel()
	const failureMsg = "no image found from any configured provider"
	stub := &stubPipeline{
		fixViolationFn: func(_ context.Context, _ string) (*rule.FixResult, error) {
			// Fixed=false and Dismissed=false drives the failure branch.
			return &rule.FixResult{RuleID: "thumb_exists", Fixed: false, Message: failureMsg}, nil
		},
	}
	r, _ := testRouterWithStubPipeline(t, stub)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/test-id/fix", nil)
	req.SetPathValue("id", "test-id")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleFixViolation(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d (Unprocessable Entity); body: %s", w.Code, http.StatusUnprocessableEntity, w.Body.String())
	}
	// Body must contain the user-visible rule-fixer message so the global
	// toast surface in layout.templ has something to display. We compare
	// against the HTML-escaped form because handleFixViolation runs the
	// message through html.EscapeString to stop a malicious provider name
	// from breaking out of the error toast. The message we picked has no
	// special characters, so the escaped form equals the raw text.
	if got := w.Body.String(); got != failureMsg {
		t.Errorf("body = %q, want %q (the rule-fixer's reason must be the response body verbatim so the global htmx:responseError handler can render it)", got, failureMsg)
	}
	// HX-Trigger MUST NOT be set on the failure path: a queue refresh would
	// destroy the action card before the user could read the toast and
	// retry, defeating the user-facing error contract.
	if trigger := w.Header().Get("HX-Trigger"); trigger != "" {
		t.Errorf("HX-Trigger = %q, want empty on fix-failure path", trigger)
	}
}

// TestHandleFixViolation_HTMX_WithUndo verifies that when a fix is applied
// via HTMX and an undo entry exists, the response contains UndoToast HTML
// and does NOT set the HX-Trigger header (to avoid destroying the toast).
func TestHandleFixViolation_HTMX_WithUndo(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{
		fixViolationFn: func(_ context.Context, _ string) (*rule.FixResult, error) {
			return &rule.FixResult{RuleID: "nfo_exists", Fixed: true, Message: "NFO created"}, nil
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)

	a := addTestArtist(t, artistSvc, "HTMX Undo Artist")

	v := &rule.RuleViolation{
		RuleID:     rule.RuleNFOExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "error",
		Message:    "missing nfo",
		Fixable:    true,
		Status:     rule.ViolationStatusOpen,
	}
	if err := r.ruleService.UpsertViolation(context.Background(), v); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}
	violations, err := r.ruleService.ListViolationsFiltered(context.Background(), rule.ViolationListParams{Status: "active"})
	if err != nil || len(violations) == 0 {
		t.Fatalf("listing violations: %v (count=%d)", err, len(violations))
	}
	violationID := violations[0].ID

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+violationID+"/fix", nil)
	req.SetPathValue("id", violationID)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleFixViolation(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// The response should contain undo toast HTML (the templ component).
	body := w.Body.String()
	if body == "" {
		t.Fatal("expected non-empty HTML body with undo toast")
	}
	if !strings.Contains(body, "undo-toast-") {
		t.Errorf("response body does not contain undo toast element; got: %s", body)
	}

	// HX-Trigger must NOT be set -- the toast's auto-dismiss handles it.
	if trigger := w.Header().Get("HX-Trigger"); trigger != "" {
		t.Errorf("HX-Trigger = %q, want empty (should not trigger queue reload when undo toast is present)", trigger)
	}
}

// TestHandleFixViolation_HTMX_NoUndo verifies that when a fix is applied
// via HTMX and there is no undo entry (pathless artist), the response sets
// HX-Trigger to refresh the queue and returns an empty body.
func TestHandleFixViolation_HTMX_NoUndo(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{
		fixViolationFn: func(_ context.Context, _ string) (*rule.FixResult, error) {
			return &rule.FixResult{RuleID: "nfo_exists", Fixed: true, Message: "NFO created"}, nil
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)

	// Create a pathless artist so no undo entry is registered.
	a := &artist.Artist{
		Name:     "Pathless HTMX Artist",
		SortName: "Pathless HTMX Artist",
		Type:     "group",
		Genres:   []string{"Rock"},
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	v := &rule.RuleViolation{
		RuleID:     rule.RuleNFOExists,
		ArtistID:   a.ID,
		ArtistName: a.Name,
		Severity:   "error",
		Message:    "missing nfo",
		Fixable:    true,
		Status:     rule.ViolationStatusOpen,
	}
	if err := r.ruleService.UpsertViolation(context.Background(), v); err != nil {
		t.Fatalf("seeding violation: %v", err)
	}
	violations, err := r.ruleService.ListViolationsFiltered(context.Background(), rule.ViolationListParams{Status: "active"})
	if err != nil || len(violations) == 0 {
		t.Fatalf("listing violations: %v (count=%d)", err, len(violations))
	}
	violationID := violations[0].ID

	req := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/"+violationID+"/fix", nil)
	req.SetPathValue("id", violationID)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleFixViolation(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// HX-Trigger must be set to refresh the queue.
	if trigger := w.Header().Get("HX-Trigger"); trigger != "dashboard:action-resolved" {
		t.Errorf("HX-Trigger = %q, want %q", trigger, "dashboard:action-resolved")
	}

	// Body should be empty (no undo toast for pathless artist).
	if body := w.Body.String(); body != "" {
		t.Errorf("expected empty body, got: %s", body)
	}
}

// TestHandleUndoFix_HTMX verifies that when the undo endpoint is called via
// HTMX, it returns an empty body and sets HX-Trigger to refresh the queue.
func TestHandleUndoFix_HTMX(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{}
	r, _ := testRouterWithStubPipeline(t, stub)

	nr := &noopRevert{}
	undoID := r.undoStore.Register("v-undo-htmx", nr.fn)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/fix-undo/"+undoID, nil)
	req.SetPathValue("undoId", undoID)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleUndoFix(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !nr.called {
		t.Error("expected revert function to have been called")
	}

	// HX-Trigger must be set to refresh the action queue.
	if trigger := w.Header().Get("HX-Trigger"); trigger != "dashboard:action-resolved" {
		t.Errorf("HX-Trigger = %q, want %q", trigger, "dashboard:action-resolved")
	}

	// Body should be empty so outerHTML swap removes the toast.
	if body := w.Body.String(); body != "" {
		t.Errorf("expected empty body, got: %s", body)
	}
}
