package api

import (
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
	t.Run("no_prior_decision", func(t *testing.T) {
		sess := &reIdentifyWizardSession{}
		step := &reIdentifyWizardStep{}
		applyDecision(sess, step, "accepted")
		if step.Decision != "accepted" {
			t.Errorf("Decision = %q, want accepted", step.Decision)
		}
		if sess.Accepted != 1 || sess.Skipped != 0 || sess.Declined != 0 {
			t.Errorf("counters = (a=%d s=%d d=%d), want (1, 0, 0)",
				sess.Accepted, sess.Skipped, sess.Declined)
		}
	})

	t.Run("resubmit_same_is_noop", func(t *testing.T) {
		sess := &reIdentifyWizardSession{Accepted: 1}
		step := &reIdentifyWizardStep{Decision: "accepted"}
		applyDecision(sess, step, "accepted")
		if sess.Accepted != 1 {
			t.Errorf("Accepted = %d, want 1 (resubmit must no-op)", sess.Accepted)
		}
	})

	t.Run("change_accepted_to_skipped", func(t *testing.T) {
		sess := &reIdentifyWizardSession{Accepted: 1}
		step := &reIdentifyWizardStep{Decision: "accepted"}
		applyDecision(sess, step, "skipped")
		if sess.Accepted != 0 {
			t.Errorf("Accepted = %d, want 0 after change", sess.Accepted)
		}
		if sess.Skipped != 1 {
			t.Errorf("Skipped = %d, want 1 after change", sess.Skipped)
		}
	})

	t.Run("change_skipped_to_declined", func(t *testing.T) {
		sess := &reIdentifyWizardSession{Skipped: 1}
		step := &reIdentifyWizardStep{Decision: "skipped"}
		applyDecision(sess, step, "declined")
		if sess.Skipped != 0 || sess.Declined != 1 {
			t.Errorf("counters = (s=%d d=%d), want (0, 1)", sess.Skipped, sess.Declined)
		}
	})

	t.Run("accept_to_accept_is_noop", func(t *testing.T) {
		// Extra invariant: repeated accept decisions must not double-count.
		sess := &reIdentifyWizardSession{Accepted: 3}
		step := &reIdentifyWizardStep{Decision: "accepted"}
		applyDecision(sess, step, "accepted")
		applyDecision(sess, step, "accepted")
		if sess.Accepted != 3 {
			t.Errorf("Accepted = %d, want 3", sess.Accepted)
		}
	})

	t.Run("declined_to_accepted", func(t *testing.T) {
		sess := &reIdentifyWizardSession{Declined: 1}
		step := &reIdentifyWizardStep{Decision: "declined"}
		applyDecision(sess, step, "accepted")
		if sess.Declined != 0 || sess.Accepted != 1 {
			t.Errorf("counters = (a=%d d=%d), want (1, 0)", sess.Accepted, sess.Declined)
		}
	})

	t.Run("triple_flip", func(t *testing.T) {
		// Decision cycles through all three states; counters must end up
		// reflecting only the final state.
		sess := &reIdentifyWizardSession{}
		step := &reIdentifyWizardStep{}
		applyDecision(sess, step, "accepted")
		applyDecision(sess, step, "skipped")
		applyDecision(sess, step, "declined")
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
		step := &reIdentifyWizardStep{Decision: "accepted"}
		applyDecision(sess, step, "skipped")
		if step.Decision != "skipped" {
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
		if got := s.get(stale.ID); got != nil {
			t.Errorf("stale session should have been pruned during create")
		}
		if got := s.get(fresh.ID); got != fresh {
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
	t.Run("not_ready_returns_nil", func(t *testing.T) {
		step := &reIdentifyWizardStep{ready: false, Candidates: []ScoredCandidate{{}}}
		if got := projectWizardCandidates(step); got != nil {
			t.Errorf("got %v, want nil when !ready", got)
		}
	})

	t.Run("ready_empty_returns_empty_slice", func(t *testing.T) {
		step := &reIdentifyWizardStep{ready: true}
		got := projectWizardCandidates(step)
		if got == nil || len(got) != 0 {
			t.Errorf("got %v, want non-nil empty slice", got)
		}
	})

	t.Run("confidence_maps_to_percent", func(t *testing.T) {
		step := &reIdentifyWizardStep{
			ready: true,
			Candidates: []ScoredCandidate{{
				ArtistSearchResult: provider.ArtistSearchResult{
					Name:           "Pink Floyd",
					MusicBrainzID:  "mbid-1",
					Country:        "GB",
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
			Name: "Pink Floyd", MBID: "mbid-1", Country: "GB",
			Disambiguation: "rock band", ConfidencePct: 85,
		}
		if got[0] != want {
			t.Errorf("projection = %+v, want %+v", got[0], want)
		}
	})

	t.Run("album_comparison_overrides_confidence", func(t *testing.T) {
		step := &reIdentifyWizardStep{
			ready: true,
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

func TestHandleReIdentifyWizardStep_NotFound(t *testing.T) {
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

func TestHandleReIdentifyWizardSaveExit(t *testing.T) {
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{
		{ArtistID: "a1", ArtistName: "A One", Decision: ""},
		{ArtistID: "a2", ArtistName: "A Two", Decision: "accepted"},
		{ArtistID: "a3", ArtistName: "A Three", Decision: ""},
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
	for _, q := range r.identifyProgress.ReviewQueue {
		if q.Tier != "wizard" {
			t.Errorf("leftover tier = %q, want wizard", q.Tier)
		}
	}
}

func TestHandleReIdentifyWizardSaveExit_SessionNotFound(t *testing.T) {
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
	r, _, _ := testRouterWithIdentify(t)
	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{{ArtistID: "a1", ArtistName: "A"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r.ensureWizardCandidates(context.Background(), sess, 0)
	sess.mu.Lock()
	defer sess.mu.Unlock()
	step := sess.Steps[0]
	if !step.ready {
		t.Error("step.ready should be true after fetch terminates")
	}
	if !step.errored {
		t.Error("step.errored should be true when orchestrator is nil")
	}
	if step.errMsg == "" {
		t.Error("step.errMsg should be populated")
	}
	if step.inFlight {
		t.Error("step.inFlight should be cleared after fetch terminates")
	}
}

func TestEnsureWizardCandidates_OutOfRange(t *testing.T) {
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
	if sess.Steps[0].ready || sess.Steps[0].errored {
		t.Error("out-of-range idx should not touch step state")
	}
}

// TestHandleBulkAction_ReIdentifyAliasNormalization verifies the legacy
// alias re_identify is normalized to re_identify_auto in the 202 response.
// This locks in the contract covered by the openapi round-3 update.
func TestHandleBulkAction_ReIdentifyAliasNormalization(t *testing.T) {
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
