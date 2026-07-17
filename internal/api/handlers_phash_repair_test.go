package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/rule"
)

// pHashCapablePipeline embeds stubPipeline (satisfying rule.PipelineRunner) and
// adds the two pHashRemediator methods, so it satisfies both the interface
// r.pipeline is declared as and the capability the phash-repair handlers narrow
// to. Mirrors fanartCapablePipeline.
type pHashCapablePipeline struct {
	*stubPipeline
	remediateFn func(ctx context.Context, scope rule.PHashMismatchScope, opts rule.PHashRemediateOpts) (rule.PHashRemediateResult, error)
	restoreFn   func(ctx context.Context, artistID, opID string) (rule.PHashRestoreResult, error)
}

func (f *pHashCapablePipeline) RemediatePHashMismatches(ctx context.Context, scope rule.PHashMismatchScope, opts rule.PHashRemediateOpts) (rule.PHashRemediateResult, error) {
	if f.remediateFn != nil {
		return f.remediateFn(ctx, scope, opts)
	}
	return rule.PHashRemediateResult{}, nil
}

func (f *pHashCapablePipeline) RestorePHashQuarantine(ctx context.Context, artistID, opID string) (rule.PHashRestoreResult, error) {
	if f.restoreFn != nil {
		return f.restoreFn(ctx, artistID, opID)
	}
	return rule.PHashRestoreResult{}, nil
}

func newPHashRepairRouter(t *testing.T, pipeline *pHashCapablePipeline) *Router {
	t.Helper()
	if pipeline == nil {
		pipeline = &pHashCapablePipeline{stubPipeline: &stubPipeline{}}
	}
	return testRouterWithFanartPipeline(t, pipeline)
}

// --------------------------------------------------------------------------
// Remediate
// --------------------------------------------------------------------------

// TestPHashMismatchRemediate_NonAdminForbidden: the back-out is admin-only via
// requireForeignAdmin, so an authenticated non-admin must get 403.
func TestPHashMismatchRemediate_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := newPHashRepairRouter(t, nil)

	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/reports/phash-mismatch/remediate", strings.NewReader(`{"artist_id":"art-a"}`))
	w := httptest.NewRecorder()
	r.handlePHashMismatchRemediate(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin should get 403; got %d", w.Code)
	}
}

// TestPHashMismatchRemediate_RequiresScope: a request with neither artist_id nor
// all_artists is a 400. A forgotten scope must never become a library-wide
// delete.
func TestPHashMismatchRemediate_RequiresScope(t *testing.T) {
	t.Parallel()
	r := newPHashRepairRouter(t, nil)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/phash-mismatch/remediate", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	r.handlePHashMismatchRemediate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a scopeless request", w.Code)
	}
	if !strings.Contains(w.Body.String(), "artist_id is required") {
		t.Errorf("body should explain the scope requirement; got %s", w.Body.String())
	}
}

// TestPHashMismatchRemediate_RejectsBothScopes: a request that sets BOTH
// artist_id AND all_artists is a 400. The two scopes are mutually exclusive --
// their intent (one artist vs the whole library) is ambiguous and must never be
// guessed on a path that deletes files. The error must be the distinct
// exclusivity message, not the "neither" one, so a caller can tell them apart.
func TestPHashMismatchRemediate_RejectsBothScopes(t *testing.T) {
	t.Parallel()
	r := newPHashRepairRouter(t, nil)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/phash-mismatch/remediate", strings.NewReader(`{"artist_id":"art-a","all_artists":true}`))
	w := httptest.NewRecorder()
	r.handlePHashMismatchRemediate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when both artist_id and all_artists are set", w.Code)
	}
	if !strings.Contains(w.Body.String(), "artist_id and all_artists are mutually exclusive") {
		t.Errorf("body should carry the exclusivity error; got %s", w.Body.String())
	}
}

// TestPHashMismatchRemediate_RejectsUnusableTolerance: a NaN/out-of-range
// tolerance on the destructive path is a 400, never a silent fallback.
func TestPHashMismatchRemediate_RejectsUnusableTolerance(t *testing.T) {
	t.Parallel()
	r := newPHashRepairRouter(t, nil)

	for _, body := range []string{
		`{"artist_id":"art-a","tolerance":1.5}`,
		`{"artist_id":"art-a","tolerance":-0.5}`,
	} {
		req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/phash-mismatch/remediate", strings.NewReader(body))
		w := httptest.NewRecorder()
		r.handlePHashMismatchRemediate(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, w.Code)
		}
	}
}

// TestPHashMismatchRemediate_ConflictWhenRepairRunning: a request returns 409
// while the shared destructive-fanart singleton is already claimed.
func TestPHashMismatchRemediate_ConflictWhenRepairRunning(t *testing.T) {
	t.Parallel()
	r := newPHashRepairRouter(t, nil)

	r.bulkActionMu.Lock()
	r.backdropRepairRunning = true
	r.bulkActionMu.Unlock()

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/phash-mismatch/remediate", strings.NewReader(`{"artist_id":"art-a"}`))
	w := httptest.NewRecorder()
	r.handlePHashMismatchRemediate(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while a repair is running", w.Code)
	}
}

// TestPHashMismatchRemediate_ConflictWhenBulkActionRunning: the back-out shares
// bulkActionMu with the bulk actions because both write/renumber the same
// artist's fanart on disk, so a running bulk action must 409 a back-out.
func TestPHashMismatchRemediate_ConflictWhenBulkActionRunning(t *testing.T) {
	t.Parallel()
	r := newPHashRepairRouter(t, nil)

	r.bulkActionMu.Lock()
	r.bulkActionProgress = &BulkActionProgress{Status: bulkActionRunning, Action: "run_rules", Total: 1}
	r.bulkActionMu.Unlock()

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/phash-mismatch/remediate", strings.NewReader(`{"all_artists":true}`))
	w := httptest.NewRecorder()
	r.handlePHashMismatchRemediate(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
}

// TestPHashMismatchRemediate_ReleasesSlotOnError: a failed back-out must release
// the singleton so a later repair is not permanently blocked.
func TestPHashMismatchRemediate_ReleasesSlotOnError(t *testing.T) {
	t.Parallel()
	pipeline := &pHashCapablePipeline{
		stubPipeline: &stubPipeline{},
		remediateFn: func(context.Context, rule.PHashMismatchScope, rule.PHashRemediateOpts) (rule.PHashRemediateResult, error) {
			return rule.PHashRemediateResult{}, errors.New("boom")
		},
	}
	r := newPHashRepairRouter(t, pipeline)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/phash-mismatch/remediate", strings.NewReader(`{"artist_id":"art-a"}`))
	w := httptest.NewRecorder()
	r.handlePHashMismatchRemediate(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	r.bulkActionMu.Lock()
	running := r.backdropRepairRunning
	r.bulkActionMu.Unlock()
	if running {
		t.Error("the singleton must be released after a failed back-out")
	}
}

// TestPHashMismatchRemediate_Success: an admin POST reaches
// RemediatePHashMismatches and the JSON body reports its result (asserted on
// the distinctive response shape -- op_id, counters, outcomes).
func TestPHashMismatchRemediate_Success(t *testing.T) {
	t.Parallel()
	var gotScope rule.PHashMismatchScope
	var gotOpts rule.PHashRemediateOpts
	pipeline := &pHashCapablePipeline{
		stubPipeline: &stubPipeline{},
		remediateFn: func(_ context.Context, scope rule.PHashMismatchScope, opts rule.PHashRemediateOpts) (rule.PHashRemediateResult, error) {
			gotScope, gotOpts = scope, opts
			return rule.PHashRemediateResult{
				OpID: "op-42", DryRun: true, ArtistsProcessed: 1, SlotsRemoved: 0, Quarantined: 0,
				Outcomes: []rule.PHashSlotOutcome{{ArtistID: "art-a", SlotIndex: 1, Action: "would-remove"}},
			}, nil
		},
	}
	r := newPHashRepairRouter(t, pipeline)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/phash-mismatch/remediate", strings.NewReader(`{"artist_id":"art-a","dry_run":true,"tolerance":0.95}`))
	w := httptest.NewRecorder()
	r.handlePHashMismatchRemediate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if gotScope.ArtistID != "art-a" || gotScope.Tolerance != 0.95 {
		t.Errorf("scope not threaded through: %+v", gotScope)
	}
	if !gotOpts.DryRun || gotOpts.AllArtists {
		t.Errorf("opts not threaded through: %+v", gotOpts)
	}
	body := w.Body.String()
	for _, want := range []string{`"op_id":"op-42"`, `"dry_run":true`, `"would-remove"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body: %s", want, body)
		}
	}
	// The singleton is released after a successful (deferred) run.
	r.bulkActionMu.Lock()
	running := r.backdropRepairRunning
	r.bulkActionMu.Unlock()
	if running {
		t.Error("the singleton must be released after a successful back-out")
	}
}

// --------------------------------------------------------------------------
// Restore
// --------------------------------------------------------------------------

// TestPHashMismatchRestore_NonAdminForbidden: restore is admin-only.
func TestPHashMismatchRestore_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := newPHashRepairRouter(t, nil)

	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/reports/phash-mismatch/restore", strings.NewReader(`{"artist_id":"art-a","op_id":"op-1"}`))
	w := httptest.NewRecorder()
	r.handlePHashMismatchRestore(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin should get 403; got %d", w.Code)
	}
}

// TestPHashMismatchRestore_RequiresBothFields: artist_id and op_id are both
// required; a missing one is a 400.
func TestPHashMismatchRestore_RequiresBothFields(t *testing.T) {
	t.Parallel()
	r := newPHashRepairRouter(t, nil)

	for _, body := range []string{`{"artist_id":"art-a"}`, `{"op_id":"op-1"}`, `{}`} {
		req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/phash-mismatch/restore", strings.NewReader(body))
		w := httptest.NewRecorder()
		r.handlePHashMismatchRestore(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, w.Code)
		}
	}
}

// TestPHashMismatchRestore_ConflictWhenRepairRunning: restore shares the same
// destructive-fanart singleton, so a running repair 409s it.
func TestPHashMismatchRestore_ConflictWhenRepairRunning(t *testing.T) {
	t.Parallel()
	r := newPHashRepairRouter(t, nil)

	r.bulkActionMu.Lock()
	r.backdropRepairRunning = true
	r.bulkActionMu.Unlock()

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/phash-mismatch/restore", strings.NewReader(`{"artist_id":"art-a","op_id":"op-1"}`))
	w := httptest.NewRecorder()
	r.handlePHashMismatchRestore(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while a repair is running", w.Code)
	}
}

// TestPHashMismatchRestore_Success: an admin POST reaches RestorePHashQuarantine
// and the JSON body reports its three-state result.
func TestPHashMismatchRestore_Success(t *testing.T) {
	t.Parallel()
	var gotArtist, gotOp string
	pipeline := &pHashCapablePipeline{
		stubPipeline: &stubPipeline{},
		restoreFn: func(_ context.Context, artistID, opID string) (rule.PHashRestoreResult, error) {
			gotArtist, gotOp = artistID, opID
			return rule.PHashRestoreResult{OpID: opID, Restored: 1, AlreadyPresent: 0, NeedsReview: 1}, nil
		},
	}
	r := newPHashRepairRouter(t, pipeline)

	req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, "/api/v1/reports/phash-mismatch/restore", strings.NewReader(`{"artist_id":"art-a","op_id":"op-42"}`))
	w := httptest.NewRecorder()
	r.handlePHashMismatchRestore(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if gotArtist != "art-a" || gotOp != "op-42" {
		t.Errorf("args not threaded through: artist=%q op=%q", gotArtist, gotOp)
	}
	body := w.Body.String()
	for _, want := range []string{`"op_id":"op-42"`, `"restored":1`, `"needs_review":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; body: %s", want, body)
		}
	}
}

// TestPHashMismatchRepair_UnimplementedPipelineFailsLoud: a pipeline that does
// not implement pHashRemediator is a wiring bug, surfaced as a 500 (fail loud),
// never a silent no-op. Both endpoints share the guard.
func TestPHashMismatchRepair_UnimplementedPipelineFailsLoud(t *testing.T) {
	t.Parallel()
	// A bare stubPipeline satisfies rule.PipelineRunner but NOT pHashRemediator.
	r := testRouterWithFanartPipeline(t, &stubPipeline{})

	for _, path := range []string{
		"/api/v1/reports/phash-mismatch/remediate",
		"/api/v1/reports/phash-mismatch/restore",
	} {
		req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, path, strings.NewReader(`{"artist_id":"art-a","op_id":"op-1"}`))
		w := httptest.NewRecorder()
		switch path {
		case "/api/v1/reports/phash-mismatch/remediate":
			r.handlePHashMismatchRemediate(w, req)
		default:
			r.handlePHashMismatchRestore(w, req)
		}
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s: status = %d, want 500 for an unimplemented pipeline", path, w.Code)
		}
	}
}

// TestPHashMismatchRepair_MalformedBodyIsRejected: a body that is not a single
// JSON object is a 400, on both endpoints.
func TestPHashMismatchRepair_MalformedBodyIsRejected(t *testing.T) {
	t.Parallel()
	r := newPHashRepairRouter(t, nil)

	cases := []struct {
		path string
		body string
	}{
		{"/api/v1/reports/phash-mismatch/remediate", `{"artist_id":`},
		{"/api/v1/reports/phash-mismatch/remediate", `{"artist_id":"a"}{"artist_id":"b"}`},
		{"/api/v1/reports/phash-mismatch/restore", `not json`},
		{"/api/v1/reports/phash-mismatch/restore", `{"artist_id":"a","op_id":"o"} trailing`},
	}
	for _, tc := range cases {
		req := httptest.NewRequestWithContext(adminContext(), http.MethodPost, tc.path, strings.NewReader(tc.body))
		w := httptest.NewRecorder()
		if strings.Contains(tc.path, "remediate") {
			r.handlePHashMismatchRemediate(w, req)
		} else {
			r.handlePHashMismatchRestore(w, req)
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s body %q: status = %d, want 400", tc.path, tc.body, w.Code)
		}
	}
}
