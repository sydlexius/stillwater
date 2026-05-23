package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	templates "github.com/sydlexius/stillwater/web/templates"
)

// TestApplyDecision_Idempotent verifies the wizard decision helper. The
// Back button lets a user revisit a step and change their mind, so the
// helper must normalize the session counters against the previous
// step.Decision before recording the new one; same-decision resubmits are
// a no-op.
func TestApplyDecision_Idempotent(t *testing.T) {
	t.Parallel()
	t.Run("no_prior_decision", func(t *testing.T) {
		sess := &reIdentifyWizardSession{}
		step := &reIdentifyWizardStep{}
		applyDecision(sess, step, wizardDecisionAccepted)
		if step.Decision != wizardDecisionAccepted {
			t.Errorf("Decision = %q, want accepted", step.Decision)
		}
		if sess.Accepted != 1 || sess.Skipped != 0 || sess.Declined != 0 {
			t.Errorf("counters = (a=%d s=%d d=%d), want (1, 0, 0)",
				sess.Accepted, sess.Skipped, sess.Declined)
		}
	})

	t.Run("resubmit_same_is_noop", func(t *testing.T) {
		sess := &reIdentifyWizardSession{Accepted: 1}
		step := &reIdentifyWizardStep{Decision: wizardDecisionAccepted}
		applyDecision(sess, step, wizardDecisionAccepted)
		if sess.Accepted != 1 {
			t.Errorf("Accepted = %d, want 1 (resubmit must no-op)", sess.Accepted)
		}
	})

	t.Run("change_accepted_to_skipped", func(t *testing.T) {
		sess := &reIdentifyWizardSession{Accepted: 1}
		step := &reIdentifyWizardStep{Decision: wizardDecisionAccepted}
		applyDecision(sess, step, wizardDecisionSkipped)
		if sess.Accepted != 0 {
			t.Errorf("Accepted = %d, want 0 after change", sess.Accepted)
		}
		if sess.Skipped != 1 {
			t.Errorf("Skipped = %d, want 1 after change", sess.Skipped)
		}
	})

	t.Run("change_skipped_to_declined", func(t *testing.T) {
		sess := &reIdentifyWizardSession{Skipped: 1}
		step := &reIdentifyWizardStep{Decision: wizardDecisionSkipped}
		applyDecision(sess, step, wizardDecisionDeclined)
		if sess.Skipped != 0 || sess.Declined != 1 {
			t.Errorf("counters = (s=%d d=%d), want (0, 1)", sess.Skipped, sess.Declined)
		}
	})

	t.Run("accept_to_accept_is_noop", func(t *testing.T) {
		// Extra invariant: repeated accept decisions must not double-count.
		sess := &reIdentifyWizardSession{Accepted: 3}
		step := &reIdentifyWizardStep{Decision: wizardDecisionAccepted}
		applyDecision(sess, step, wizardDecisionAccepted)
		applyDecision(sess, step, wizardDecisionAccepted)
		if sess.Accepted != 3 {
			t.Errorf("Accepted = %d, want 3", sess.Accepted)
		}
	})

	t.Run("declined_to_accepted", func(t *testing.T) {
		sess := &reIdentifyWizardSession{Declined: 1}
		step := &reIdentifyWizardStep{Decision: wizardDecisionDeclined}
		applyDecision(sess, step, wizardDecisionAccepted)
		if sess.Declined != 0 || sess.Accepted != 1 {
			t.Errorf("counters = (a=%d d=%d), want (1, 0)", sess.Accepted, sess.Declined)
		}
	})

	t.Run("triple_flip", func(t *testing.T) {
		// Decision cycles through all three states; counters must end up
		// reflecting only the final state.
		sess := &reIdentifyWizardSession{}
		step := &reIdentifyWizardStep{}
		applyDecision(sess, step, wizardDecisionAccepted)
		applyDecision(sess, step, wizardDecisionSkipped)
		applyDecision(sess, step, wizardDecisionDeclined)
		if sess.Accepted != 0 || sess.Skipped != 0 || sess.Declined != 1 {
			t.Errorf("counters = (a=%d s=%d d=%d), want (0, 0, 1)",
				sess.Accepted, sess.Skipped, sess.Declined)
		}
	})

	t.Run("underflow_guard", func(t *testing.T) {
		// If counters are somehow already zero (e.g. a bug elsewhere), the
		// helper must not go negative and must still record the new
		// decision with its matching counter increment. Assert the full
		// post-state so a future drift bug (e.g. skipping the increment
		// whenever the previous counter was zero) is caught.
		sess := &reIdentifyWizardSession{Accepted: 0}
		step := &reIdentifyWizardStep{Decision: wizardDecisionAccepted}
		applyDecision(sess, step, wizardDecisionSkipped)
		if step.Decision != wizardDecisionSkipped {
			t.Errorf("Decision = %q, want skipped", step.Decision)
		}
		if sess.Accepted != 0 || sess.Skipped != 1 || sess.Declined != 0 {
			t.Errorf("counters = (a=%d s=%d d=%d), want (0, 1, 0)",
				sess.Accepted, sess.Skipped, sess.Declined)
		}
	})
}

// TestWizardStore exercises the in-memory session store directly: create,
// get, explicit delete, and TTL-based eviction on access. The TTL cases
// backdate Updated manually so the test does not have to sleep for 30
// minutes.
func TestWizardStore(t *testing.T) {
	t.Parallel()
	t.Run("create_and_get", func(t *testing.T) {
		s := newReIdentifyWizardStore()
		sess, err := s.create([]*reIdentifyWizardStep{{ArtistID: "a1"}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if sess.ID == "" {
			t.Fatal("expected non-empty session id")
		}
		if got := s.get(sess.ID); got != sess {
			t.Errorf("get returned %v, want %v", got, sess)
		}
	})

	t.Run("get_missing_returns_nil", func(t *testing.T) {
		s := newReIdentifyWizardStore()
		if got := s.get("nonexistent"); got != nil {
			t.Errorf("get unknown id = %v, want nil", got)
		}
	})

	t.Run("delete_removes_session", func(t *testing.T) {
		s := newReIdentifyWizardStore()
		sess, err := s.create([]*reIdentifyWizardStep{{ArtistID: "a1"}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		s.delete(sess.ID)
		if got := s.get(sess.ID); got != nil {
			t.Errorf("get after delete = %v, want nil", got)
		}
	})

	t.Run("expired_session_evicted_on_get", func(t *testing.T) {
		s := newReIdentifyWizardStore()
		sess, err := s.create([]*reIdentifyWizardStep{{ArtistID: "a1"}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// Backdate past TTL so the next get drops the session.
		sess.mu.Lock()
		sess.Updated = time.Now().Add(-2 * reIdentifyWizardSessionTTL)
		sess.mu.Unlock()
		if got := s.get(sess.ID); got != nil {
			t.Errorf("expected expired session to evict, got %v", got)
		}
	})

	t.Run("create_prunes_expired_siblings", func(t *testing.T) {
		s := newReIdentifyWizardStore()
		stale, err := s.create([]*reIdentifyWizardStep{{ArtistID: "old"}})
		if err != nil {
			t.Fatalf("create stale: %v", err)
		}
		stale.mu.Lock()
		stale.Updated = time.Now().Add(-2 * reIdentifyWizardSessionTTL)
		stale.mu.Unlock()
		fresh, err := s.create([]*reIdentifyWizardStep{{ArtistID: "new"}})
		if err != nil {
			t.Fatalf("create fresh: %v", err)
		}
		// Assert stale removal directly from the store's internal map so
		// we measure create-time pruning rather than the side-effect
		// eviction performed by get().
		s.mu.Lock()
		_, staleStillPresent := s.sessions[stale.ID]
		_, freshPresent := s.sessions[fresh.ID]
		s.mu.Unlock()
		if staleStillPresent {
			t.Errorf("stale session should have been pruned during create")
		}
		if !freshPresent {
			t.Errorf("fresh session missing after create")
		}
	})
}

// TestProjectWizardCandidates checks the per-step candidate projection used
// by the wizard template. The "not ready" branch returns nil so the template
// renders its loading state; ready=true populates the flat view model with
// the AlbumComparison percent when present, falling back to Confidence *
// 100.
func TestProjectWizardCandidates(t *testing.T) {
	t.Parallel()
	t.Run("not_ready_returns_nil", func(t *testing.T) {
		step := &reIdentifyWizardStep{state: wizardStepPending, Candidates: []ScoredCandidate{{}}}
		if got := projectWizardCandidates(step); got != nil {
			t.Errorf("got %v, want nil when !ready", got)
		}
	})

	t.Run("ready_empty_returns_empty_slice", func(t *testing.T) {
		step := &reIdentifyWizardStep{state: wizardStepReady}
		got := projectWizardCandidates(step)
		if got == nil || len(got) != 0 {
			t.Errorf("got %v, want non-nil empty slice", got)
		}
	})

	t.Run("confidence_maps_to_percent", func(t *testing.T) {
		step := &reIdentifyWizardStep{
			state: wizardStepReady,
			Candidates: []ScoredCandidate{{
				ArtistSearchResult: provider.ArtistSearchResult{
					Name:           "Pink Floyd",
					MusicBrainzID:  "mbid-1",
					Origin:         "GB",
					Disambiguation: "rock band",
				},
				Confidence: 0.85,
			}},
		}
		got := projectWizardCandidates(step)
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
		want := templates.WizardCandidateView{
			Name: "Pink Floyd", MBID: "mbid-1", Origin: "GB",
			Disambiguation: "rock band", ConfidencePct: 85,
		}
		if got[0] != want {
			t.Errorf("projection = %+v, want %+v", got[0], want)
		}
	})

	t.Run("album_comparison_overrides_confidence", func(t *testing.T) {
		step := &reIdentifyWizardStep{
			state: wizardStepReady,
			Candidates: []ScoredCandidate{{
				ArtistSearchResult: provider.ArtistSearchResult{Name: "A"},
				AlbumComparison:    &artist.AlbumComparison{MatchPercent: 92},
				Confidence:         0.10,
			}},
		}
		got := projectWizardCandidates(step)
		if got[0].ConfidencePct != 92 {
			t.Errorf("ConfidencePct = %d, want 92 (AlbumComparison wins)", got[0].ConfidencePct)
		}
	})
}

// TestHandleReIdentifyWizardStart_Validation covers every 400/503 branch of
// the start endpoint without a real orchestrator. These paths account for
// the bulk of unhappy-path code in the handler.
func TestHandleReIdentifyWizardStart_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		body     string
		wantCode int
		wantMsg  string
	}{
		{"invalid_json", "{not-json", http.StatusBadRequest, "invalid JSON"},
		{"empty_ids", `{"ids":[]}`, http.StatusBadRequest, "non-empty"},
		{"invalid_id_format", `{"ids":["../../etc/passwd"]}`, http.StatusBadRequest, "invalid id format"},
		{"no_valid_artists", `{"ids":["does-not-exist-123"]}`, http.StatusBadRequest, "no valid artists"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, _, _ := testRouterWithIdentify(t)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/re-identify/wizard", strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.handleReIdentifyWizardStart(w, req)
			if w.Code != c.wantCode {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, c.wantCode, w.Body.String())
			}
			if c.wantMsg != "" && !strings.Contains(w.Body.String(), c.wantMsg) {
				t.Errorf("body %q does not contain %q", w.Body.String(), c.wantMsg)
			}
		})
	}
}

func TestHandleReIdentifyWizardStart_TooManyIDs(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	// Build a payload with MaxBulkActionIDs+1 valid IDs so the cap fires
	// before any per-ID work runs.
	ids := make([]string, 0, MaxBulkActionIDs+1)
	for i := 0; i <= MaxBulkActionIDs; i++ {
		// Use unique IDs so the test intent (length cap) cannot be
		// confused with deduplication behavior if the handler ever adds
		// an up-front dedup pass.
		ids = append(ids, fmt.Sprintf("id%04d", i))
	}
	body, _ := json.Marshal(map[string][]string{"ids": ids})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/re-identify/wizard", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardStart(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "too many") {
		t.Errorf("body %q does not contain 'too many'", w.Body.String())
	}
}

func TestHandleReIdentifyWizardStart_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	// Zero-value Router has nil artistService, which is the 503 branch.
	r := &Router{reIdentifyWizardStore: newReIdentifyWizardStore()}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/re-identify/wizard", strings.NewReader(`{"ids":["x"]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardStart(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestHandleReIdentifyWizardStart_Success(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithIdentify(t)
	a := &artist.Artist{ID: "startHappy1", Name: "Start Happy"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("seed artist: %v", err)
	}
	body := strings.NewReader(`{"ids":["` + a.ID + `"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/re-identify/wizard", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardStart(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sid, _ := resp["session_id"].(string)
	if sid == "" {
		t.Errorf("session_id missing or empty: %v", resp["session_id"])
	}
	if total, ok := resp["total"].(float64); !ok || total != 1 {
		t.Errorf("total = %v, want 1", resp["total"])
	}
	if idx, ok := resp["index"].(float64); !ok || idx != 0 {
		t.Errorf("index = %v, want 0", resp["index"])
	}
	if sid != "" && r.reIdentifyWizardStore.get(sid) == nil {
		t.Errorf("store missing session %q returned in response", sid)
	}
}

// TestHandleReIdentifyWizardStart_ReportsSkippedIDs verifies the start
// endpoint surfaces the IDs it had to drop, with a coarse reason class so
// the UI can render distinct copy for "stale ID" vs "backend failure". The
// previous behavior silently swallowed these in the per-ID loop.
func TestHandleReIdentifyWizardStart_ReportsSkippedIDs(t *testing.T) {
	t.Parallel()

	t.Run("not_found_reason_for_missing_artist", func(t *testing.T) {
		t.Parallel()
		r, _, artistSvc := testRouterWithIdentify(t)
		// One real artist plus one ID the service has never seen. The
		// missing ID must surface as a skipped_errors entry with the
		// not_found reason; the wizard still starts with one step.
		real := &artist.Artist{ID: "skipReal1", Name: "Real Artist"}
		if err := artistSvc.Create(context.Background(), real); err != nil {
			t.Fatalf("seed: %v", err)
		}
		body := strings.NewReader(`{"ids":["` + real.ID + `","ghostArtistXYZ"]}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/re-identify/wizard", body)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.handleReIdentifyWizardStart(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
		}
		var resp struct {
			SessionID     string                `json:"session_id"`
			Total         int                   `json:"total"`
			Index         int                   `json:"index"`
			SkippedIDs    []string              `json:"skipped_ids"`
			SkippedErrors []SkippedWizardArtist `json:"skipped_errors"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total != 1 {
			t.Errorf("total = %d, want 1 (one valid artist after the skip)", resp.Total)
		}
		if len(resp.SkippedIDs) != 1 || resp.SkippedIDs[0] != "ghostArtistXYZ" {
			t.Errorf("skipped_ids = %v, want [ghostArtistXYZ]", resp.SkippedIDs)
		}
		if len(resp.SkippedErrors) != 1 ||
			resp.SkippedErrors[0].ID != "ghostArtistXYZ" ||
			resp.SkippedErrors[0].Reason != skippedReasonNotFound {
			t.Errorf("skipped_errors = %+v, want one entry {ghostArtistXYZ, not_found}", resp.SkippedErrors)
		}
	})

	t.Run("load_error_reason_for_backend_failure", func(t *testing.T) {
		t.Parallel()
		// Drive the generic-error branch by closing the underlying DB
		// after seeding the artist: GetByID then returns a wrapped
		// sql.ErrConnDone-class error, which is not ErrNotFound. This
		// exercises the load_error reason class without standing up a
		// hand-rolled repository fake.
		r, _, artistSvc := testRouterWithIdentify(t)
		dead := &artist.Artist{ID: "deadArtist1", Name: "Dead Artist"}
		if err := artistSvc.Create(context.Background(), dead); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := r.db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
		body := strings.NewReader(`{"ids":["` + dead.ID + `"]}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/re-identify/wizard", body)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.handleReIdentifyWizardStart(w, req)
		// Every ID failed to load so the handler still returns its
		// "no valid artists" 400. The handler-level branch is the
		// existing behavior; the unit-level test below asserts the
		// per-ID classification reaches the skipped slice.
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 when all IDs fail to load", w.Code)
		}
	})

	t.Run("load_error_classified_in_builder", func(t *testing.T) {
		t.Parallel()
		// Unit-level assertion: calling the extracted builder with a
		// closed DB must classify the failure as load_error rather
		// than not_found, so a future re-shuffle of the handler can't
		// regress the reason mapping without failing this test.
		r, _, artistSvc := testRouterWithIdentify(t)
		brokenID := "brokenArtist1"
		if err := artistSvc.Create(context.Background(), &artist.Artist{ID: brokenID, Name: "Broken"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := r.db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
		steps, skipped, badID := r.buildWizardStartSteps(context.Background(), []string{brokenID})
		if badID {
			t.Fatal("badID = true; ID is well-formed")
		}
		if len(steps) != 0 {
			t.Errorf("steps = %d, want 0 when GetByID fails", len(steps))
		}
		if len(skipped) != 1 || skipped[0].ID != brokenID || skipped[0].Reason != skippedReasonLoadError {
			t.Errorf("skipped = %+v, want one {brokenArtist1, load_error}", skipped)
		}
	})

	t.Run("empty_skip_serializes_as_array_not_null", func(t *testing.T) {
		t.Parallel()
		// The OpenAPI start-response schema declares skipped_ids and
		// skipped_errors as required arrays. A nil Go slice marshals to
		// JSON null, which would break the contract. Lock in that the
		// happy path (no IDs dropped) still serializes [] for both.
		r, _, artistSvc := testRouterWithIdentify(t)
		real := &artist.Artist{ID: "allGoodArtist1", Name: "All Good"}
		if err := artistSvc.Create(context.Background(), real); err != nil {
			t.Fatalf("seed: %v", err)
		}
		body := strings.NewReader(`{"ids":["` + real.ID + `"]}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/re-identify/wizard", body)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.handleReIdentifyWizardStart(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
		}
		// Assert against the raw JSON so a future struct-level coalesce
		// to []SkippedWizardArtist{} on decode cannot hide a nil being
		// emitted on the wire.
		raw := w.Body.String()
		if !strings.Contains(raw, `"skipped_ids":[]`) {
			t.Errorf("skipped_ids must serialize as []; body was: %s", raw)
		}
		if !strings.Contains(raw, `"skipped_errors":[]`) {
			t.Errorf("skipped_errors must serialize as []; body was: %s", raw)
		}
	})
}

func TestHandleReIdentifyWizardStep_NotFound(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	req := httptest.NewRequest(http.MethodGet, "/artists/re-identify/wizard/unknown/step/0", nil)
	req.SetPathValue("sid", "unknown")
	req.SetPathValue("idx", "0")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardStep(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleReIdentifyWizardStep_InvalidIndex(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{ArtistID: "a1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/artists/re-identify/wizard/"+sess.ID+"/step/-1", nil)
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "nope")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardStep(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestWizardStepFromRequest_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r := &Router{reIdentifyWizardStore: newReIdentifyWizardStore()}
	req := httptest.NewRequest(http.MethodPost, "/any", nil)
	req.SetPathValue("sid", "x")
	req.SetPathValue("idx", "0")
	w := httptest.NewRecorder()
	if _, _, _, ok := r.wizardStepFromRequest(w, req); ok {
		t.Fatal("expected ok=false when artistService is nil")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestWizardStepFromRequest_IndexOutOfRange(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{ArtistID: "a1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/any", nil)
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "99")
	w := httptest.NewRecorder()
	if _, _, _, ok := r.wizardStepFromRequest(w, req); ok {
		t.Fatal("expected ok=false when idx out of range")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestHandleReIdentifyWizardSkip exercises the full skip handler: advance the
// decision counter and return status=advanced for the non-HTMX caller.
func TestHandleReIdentifyWizardSkip(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{
		{ArtistID: "a1"}, {ArtistID: "a2"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/any", nil)
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "0")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardSkip(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	// Lock in the OpenAPI-documented response contract so a regression
	// away from {status: advanced, index: N} surfaces here, not later in
	// the client.
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "advanced" {
		t.Errorf("status = %v, want advanced", resp["status"])
	}
	if idx, ok := resp["index"].(float64); !ok || idx != 1 {
		t.Errorf("index = %v, want 1", resp["index"])
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", sess.Skipped)
	}
	if sess.Steps[0].Decision != "skipped" {
		t.Errorf("Decision = %q, want skipped", sess.Steps[0].Decision)
	}
}

func TestHandleReIdentifyWizardDecline(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{
		{ArtistID: "a1"}, {ArtistID: "a2"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/any", nil)
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "0")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardDecline(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "advanced" {
		t.Errorf("status = %v, want advanced", resp["status"])
	}
	if idx, ok := resp["index"].(float64); !ok || idx != 1 {
		t.Errorf("index = %v, want 1", resp["index"])
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.Declined != 1 {
		t.Errorf("Declined = %d, want 1", sess.Declined)
	}
}

func TestHandleReIdentifyWizardAccept_MissingMBID(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{ArtistID: "a1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/any", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "0")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardAccept(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "mbid is required") {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestHandleReIdentifyWizardAccept_InvalidJSON(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{ArtistID: "a1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/any", strings.NewReader(`{broken`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "0")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardAccept(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleReIdentifyWizardAccept_ArtistNotFound(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	// Step references an artist that does not exist in the DB, so the
	// accept handler's GetByID returns ErrNotFound.
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{ArtistID: "ghost"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/any", strings.NewReader(`{"mbid":"abc"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "0")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardAccept(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

func TestHandleReIdentifyWizardAccept_FormBody(t *testing.T) {
	t.Parallel()
	// Form-encoded hx-vals path. We expect the handler to reject missing
	// mbid here too; this exercises the non-JSON branch of the content-type
	// switch.
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{ArtistID: "a1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/any", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "0")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardAccept(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (empty form body)", w.Code)
	}
}

func TestHandleReIdentifyWizardAccept_AdvancedSuccess(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithIdentify(t)
	a := &artist.Artist{ID: "acceptAdv1", Name: "Accept Advanced"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("seed artist: %v", err)
	}
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{
		{ArtistID: a.ID}, {ArtistID: "other"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/any", strings.NewReader(`{"mbid":"mbid-abc"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "0")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardAccept(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "advanced" {
		t.Errorf("status = %v, want advanced", resp["status"])
	}
	if idx, ok := resp["index"].(float64); !ok || idx != 1 {
		t.Errorf("index = %v, want 1", resp["index"])
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.Accepted != 1 {
		t.Errorf("Accepted = %d, want 1", sess.Accepted)
	}
	if sess.Steps[0].Decision != "accepted" {
		t.Errorf("Steps[0].Decision = %q, want accepted", sess.Steps[0].Decision)
	}
}

func TestHandleReIdentifyWizardAccept_DoneSuccess(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithIdentify(t)
	a := &artist.Artist{ID: "acceptDone1", Name: "Accept Done"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("seed artist: %v", err)
	}
	// Single-step session so accepting step 0 terminates the wizard.
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{ArtistID: a.ID}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/any", strings.NewReader(`{"mbid":"mbid-xyz"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("sid", sess.ID)
	req.SetPathValue("idx", "0")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardAccept(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "done" {
		t.Errorf("status = %v, want done", resp["status"])
	}
	if sid, ok := resp["session_id"].(string); !ok || sid != sess.ID {
		t.Errorf("session_id = %v, want %q", resp["session_id"], sess.ID)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.Accepted != 1 {
		t.Errorf("Accepted = %d, want 1", sess.Accepted)
	}
	if sess.Steps[0].Decision != "accepted" {
		t.Errorf("Steps[0].Decision = %q, want accepted", sess.Steps[0].Decision)
	}
}

func TestHandleReIdentifyWizardSaveExit(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{
		{ArtistID: "a1", ArtistName: "A One", Decision: wizardDecisionNone},
		{ArtistID: "a2", ArtistName: "A Two", Decision: wizardDecisionAccepted},
		{ArtistID: "a3", ArtistName: "A Three", Decision: wizardDecisionNone},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sess.mu.Lock()
	sess.Accepted = 1
	sess.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/any", nil)
	req.SetPathValue("sid", sess.ID)
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardSaveExit(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "saved" {
		t.Errorf("status = %v, want saved", resp["status"])
	}
	if resp["accepted"].(float64) != 1 {
		t.Errorf("accepted = %v, want 1", resp["accepted"])
	}
	if resp["skipped"].(float64) != 0 {
		t.Errorf("skipped = %v, want 0", resp["skipped"])
	}
	if resp["declined"].(float64) != 0 {
		t.Errorf("declined = %v, want 0", resp["declined"])
	}
	if resp["leftover"].(float64) != 2 {
		t.Errorf("leftover = %v, want 2", resp["leftover"])
	}
	// Session should be deleted after save-exit.
	if got := r.reIdentifyWizardStore.get(sess.ID); got != nil {
		t.Errorf("session should be deleted after save-exit")
	}
	// Leftover should land on the identify ReviewQueue so the main page
	// can pick the undecided artists back up.
	r.identifyMu.Lock()
	defer r.identifyMu.Unlock()
	if r.identifyProgress == nil {
		t.Fatal("identifyProgress not initialized by save-exit")
	}
	r.identifyProgress.mu.RLock()
	defer r.identifyProgress.mu.RUnlock()
	if len(r.identifyProgress.ReviewQueue) != 2 {
		t.Errorf("ReviewQueue len = %d, want 2", len(r.identifyProgress.ReviewQueue))
	}
	gotIDs := map[string]bool{}
	for _, q := range r.identifyProgress.ReviewQueue {
		if q.Tier != "wizard" {
			t.Errorf("leftover tier = %q, want wizard", q.Tier)
		}
		gotIDs[q.ArtistID] = true
	}
	for _, want := range []string{"a1", "a3"} {
		if !gotIDs[want] {
			t.Errorf("leftover queue missing ArtistID %q; got %v", want, gotIDs)
		}
	}
	if gotIDs["a2"] {
		t.Errorf("leftover queue unexpectedly contains accepted ArtistID a2")
	}
}

func TestHandleReIdentifyWizardSaveExit_SessionNotFound(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	req := httptest.NewRequest(http.MethodPost, "/any", nil)
	req.SetPathValue("sid", "nope")
	w := httptest.NewRecorder()
	r.handleReIdentifyWizardSaveExit(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestEnsureWizardCandidates_NoOrchestrator exercises the nil-orchestrator
// branch: fetch cannot happen, so the step ends up ready=true, errored=true
// with a sanitized errMsg. The full template-surface for errored is tracked
// as an M46.5 follow-up but the state on the session is already populated.
func TestEnsureWizardCandidates_NoOrchestrator(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{ArtistID: "a1", ArtistName: "A"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r.ensureWizardCandidates(context.Background(), sess, 0)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	step := sess.Steps[0]
	// Orchestrator is nil so fetchErr is set; the step must land in the
	// Failed terminal state with a sanitized message. The state enum makes
	// Loading/Ready/Failed mutually exclusive, so confirming Failed also
	// proves Loading was cleared.
	if step.state != wizardStepFailed {
		t.Errorf("step.state = %d, want wizardStepFailed when orchestrator is nil", step.state)
	}
	if step.errMsg == "" {
		t.Error("step.errMsg should be populated on failure")
	}
}

// TestEnsureWizardCandidates_RetryClearsPriorError locks in the refactor
// invariant that a step in wizardStepFailed is retryable: a fresh call to
// ensureWizardCandidates must not short-circuit on the failed state, and
// it must clear the prior errMsg before attempting the new fetch. The
// orchestrator is nil here so the retry fails identically (state lands
// back at wizardStepFailed with the standard sanitized errMsg), but the
// distinct seeded errMsg lets us prove the reclaim path ran (i.e. the
// claim block at the top of ensureWizardCandidates fired, which is
// otherwise indistinguishable from a no-op early-return when start and
// end states are both Failed).
func TestEnsureWizardCandidates_RetryClearsPriorError(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{ArtistID: "a1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed a Failed step with an unmistakable prior error message so the
	// retry path's errMsg-clearing behavior is observable.
	const seededErrMsg = "PRIOR error message from earlier attempt"
	sess.mu.Lock()
	sess.Steps[0].state = wizardStepFailed
	sess.Steps[0].errMsg = seededErrMsg
	sess.mu.Unlock()

	r.ensureWizardCandidates(context.Background(), sess, 0)

	sess.mu.Lock()
	defer sess.mu.Unlock()
	step := sess.Steps[0]
	if step.state != wizardStepFailed {
		t.Errorf("state = %d, want wizardStepFailed (orchestrator nil so retry also fails)", step.state)
	}
	if step.errMsg == seededErrMsg {
		t.Error("errMsg still carries the seeded prior message; retry path did not clear it before re-fetching")
	}
	if step.errMsg == "" {
		t.Error("errMsg empty; retry-then-fail must reset to the standard sanitized message, not leave blank")
	}
}

func TestEnsureWizardCandidates_OutOfRange(t *testing.T) {
	t.Parallel()
	// idx < 0 or >= len(Steps) is a silent no-op; safe to call from
	// pre-fetch goroutines without bounds checks.
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{ArtistID: "a1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r.ensureWizardCandidates(context.Background(), sess, 99)
	r.ensureWizardCandidates(context.Background(), sess, -1)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.Steps[0].state != wizardStepPending {
		t.Errorf("state = %d, want wizardStepPending (out-of-range idx must not touch step state)", sess.Steps[0].state)
	}
}

// TestHandleBulkAction_ReIdentifyAliasNormalization verifies the legacy
// alias re_identify is normalized to re_identify_auto in the 202 response.
// This locks in the contract covered by the openapi round-3 update.
func TestHandleBulkAction_ReIdentifyAliasNormalization(t *testing.T) {
	t.Parallel()
	r, _, artistSvc := testRouterWithIdentify(t)
	a := &artist.Artist{ID: "aliasArtist1", Name: "Alias Artist"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("seed artist: %v", err)
	}
	body := strings.NewReader(`{"action":"re_identify","ids":["` + a.ID + `"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-actions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkAction(w, req)
	// testRouterWithIdentify wires an artistService, so the re_identify_auto
	// availability check passes and we must get the normalized 202 snapshot.
	// Allowing 503 would let a regression in the start path silently mask
	// the normalization assertion.
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["action"] != "re_identify_auto" {
		t.Errorf("action = %v, want re_identify_auto", resp["action"])
	}
}

// TestWizardErroredStepRendersBanner verifies the template surfaces a step
// in the wizardStepFailed state with the dedicated error banner and a Retry
// control, rather than the ambiguous "no candidates" message. Renders the
// templ component directly with Errored=true so the assertion does not
// depend on orchestrator wiring or HTTP plumbing.
func TestWizardErroredStepRendersBanner(t *testing.T) {
	t.Parallel()
	data := templates.ReIdentifyWizardStepData{
		SessionID:  "sid-test",
		Index:      0,
		Total:      1,
		ArtistID:   "a1",
		ArtistName: "Test Artist",
		Errored:    true,
		ErrMsg:     "candidate lookup failed; retry or skip this artist",
	}
	var buf bytes.Buffer
	if err := templates.ReIdentifyWizardStep(data).Render(testI18nCtx(t, context.Background()), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	// The error banner uses role=alert so screen readers and tests can
	// locate it unambiguously; assert on that contract rather than the
	// underlying Tailwind class soup so a future restyle does not break
	// the test.
	if !strings.Contains(out, `role="alert"`) {
		t.Errorf("rendered output missing role=alert banner; got:\n%s", out)
	}
	if !strings.Contains(out, data.ErrMsg) {
		t.Errorf("rendered output missing sanitized ErrMsg %q", data.ErrMsg)
	}
	if !strings.Contains(out, "Could not load candidates") {
		t.Errorf("rendered output missing localized error heading; got:\n%s", out)
	}
	// The Retry control posts to the new per-step retry endpoint; assert
	// the URL so a renamed route is caught here, not at runtime.
	if !strings.Contains(out, "/api/v1/artists/re-identify/wizard/sid-test/step/0/retry") {
		t.Errorf("rendered output missing retry POST URL; got:\n%s", out)
	}
	if !strings.Contains(out, "Retry") {
		t.Errorf("rendered output missing Retry button label; got:\n%s", out)
	}
	// The errored render path must NOT also fall through to the "no
	// candidates" or "loading" branches, which would produce a confusing
	// mixed UI. Search for the canonical strings from those branches.
	if strings.Contains(out, "No candidates found for this artist.") {
		t.Errorf("errored render leaked the no-candidates message; got:\n%s", out)
	}
	if strings.Contains(out, "Searching providers for candidates...") {
		t.Errorf("errored render leaked the loading spinner copy; got:\n%s", out)
	}
}

// TestHandleReIdentifyWizardRetry covers the new retry endpoint. The step
// is seeded with Failed state; orchestrator is nil so the retry deliberately
// fails again (no real provider in this test rig). The endpoint must still
// 200 and return a rendered fragment that surfaces the error banner so the
// user can see the retry didn't fix it.
func TestHandleReIdentifyWizardRetry(t *testing.T) {
	t.Parallel()
	t.Run("rerenders_error_banner_when_retry_fails", func(t *testing.T) {
		t.Parallel()
		r, _, _ := testRouterWithIdentify(t)
		sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{
			ArtistID:   "ra1",
			ArtistName: "Retry Artist",
		}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// Seed Failed state directly so the test exercises the retry path
		// without first having to invoke ensureWizardCandidates.
		sess.mu.Lock()
		sess.Steps[0].state = wizardStepFailed
		sess.Steps[0].errMsg = "prior error"
		sess.mu.Unlock()

		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/artists/re-identify/wizard/"+sess.ID+"/step/0/retry", nil)
		req.SetPathValue("sid", sess.ID)
		req.SetPathValue("idx", "0")
		// Make it an HTMX request so renderTempl emits the fragment
		// rather than the full page wrapper.
		req.Header.Set("HX-Request", "true")
		req.Header.Set("Accept-Language", "en")
		w := httptest.NewRecorder()
		r.handleReIdentifyWizardRetry(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, `role="alert"`) {
			t.Errorf("retry response missing error banner; got:\n%s", body)
		}
		// The prior errMsg must have been replaced by the standard
		// sanitized message after the retry's own failure.
		if strings.Contains(body, "prior error") {
			t.Errorf("retry response leaked prior errMsg; got:\n%s", body)
		}

		// State on the session is now Failed again with the standard
		// sanitized message (orchestrator nil so retry could not
		// succeed). Asserting on the session, not the body, locks in
		// the contract independent of template churn.
		sess.mu.Lock()
		got := sess.Steps[0]
		state := got.state
		msg := got.errMsg
		sess.mu.Unlock()
		if state != wizardStepFailed {
			t.Errorf("state after retry = %d, want wizardStepFailed", state)
		}
		if msg == "prior error" || msg == "" {
			t.Errorf("errMsg after retry = %q, want the standard sanitized message", msg)
		}
	})

	t.Run("session_not_found", func(t *testing.T) {
		t.Parallel()
		r, _, _ := testRouterWithIdentify(t)
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/artists/re-identify/wizard/missing/step/0/retry", nil)
		req.SetPathValue("sid", "missing")
		req.SetPathValue("idx", "0")
		w := httptest.NewRecorder()
		r.handleReIdentifyWizardRetry(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})
}
