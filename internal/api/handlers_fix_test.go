package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/rule"
)

func TestHandleFixViolation_Success(t *testing.T) {
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
}

func TestHandleFixAll_NoFixable(t *testing.T) {
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
