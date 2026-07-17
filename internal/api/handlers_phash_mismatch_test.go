package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/rule"
)

// phashCapablePipeline embeds stubPipeline (satisfying rule.PipelineRunner)
// and adds the ScanPHashMismatches method, so it satisfies both the interface
// r.pipeline is declared as and the capability interface the handler narrows
// to. Mirrors fanartCapablePipeline.
type phashCapablePipeline struct {
	*stubPipeline
	scanFn func(ctx context.Context, scope rule.PHashMismatchScope) (rule.PHashMismatchReport, error)
	// gotScope records what the handler actually asked for, so the query
	// parsing can be asserted rather than assumed.
	gotScope rule.PHashMismatchScope
	called   bool
}

func (p *phashCapablePipeline) ScanPHashMismatches(ctx context.Context, scope rule.PHashMismatchScope) (rule.PHashMismatchReport, error) {
	p.gotScope = scope
	p.called = true
	if p.scanFn != nil {
		return p.scanFn(ctx, scope)
	}
	return rule.PHashMismatchReport{}, nil
}

func phashReq(ctx context.Context, query string) *http.Request {
	return httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/reports/phash-mismatch"+query, nil)
}

// TestPHashMismatchReport_NonAdminForbidden: the report is admin-only via
// requireForeignAdmin.
func TestPHashMismatchReport_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	p := &phashCapablePipeline{stubPipeline: &stubPipeline{}}
	r := testRouterWithFanartPipeline(t, p)

	ctx := middleware.WithTestUserID(context.Background(), "u1")
	ctx = middleware.WithTestRole(ctx, "operator")
	w := httptest.NewRecorder()
	r.handlePHashMismatchReport(w, phashReq(ctx, ""))

	if w.Code != http.StatusForbidden {
		t.Errorf("non-admin should get 403; got %d", w.Code)
	}
	if p.called {
		t.Error("the scan ran for a non-admin caller; the gate must stop it before the detector")
	}
}

// TestPHashMismatchReport_MissingCapabilityIs500 covers the fail-loud wiring
// guard: a pipeline without the detector must error, never return an empty
// report that reads as a clean library.
func TestPHashMismatchReport_MissingCapabilityIs500(t *testing.T) {
	t.Parallel()
	r := testRouterWithFanartPipeline(t, &stubPipeline{})

	w := httptest.NewRecorder()
	r.handlePHashMismatchReport(w, phashReq(adminContext(), ""))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("unwired pipeline should get 500; got %d", w.Code)
	}
}

// TestPHashMismatchReport_AdminGetsReportJSON is the authenticated happy path:
// the report's substance -- the suspect, its attribution, its similarity score,
// the indeterminate bucket and the registry coverage -- must all survive to the
// client, since each is what an operator judges a deletion by.
func TestPHashMismatchReport_AdminGetsReportJSON(t *testing.T) {
	t.Parallel()
	p := &phashCapablePipeline{
		stubPipeline: &stubPipeline{},
		scanFn: func(_ context.Context, _ rule.PHashMismatchScope) (rule.PHashMismatchReport, error) {
			return rule.PHashMismatchReport{
				Tolerance:          0.90,
				ArtistsScanned:     3,
				ArtistsAffected:    1,
				SlotsEvaluated:     7,
				SuspectSlots:       1,
				IndeterminateSlots: 2,
				FanartRegistry:     rule.PHashRegistryCoverage{ArtistsIndexed: 2, ArtistsSkipped: 1, SlotsIndexed: 7, SlotsSkipped: 2},
				PerArtist: []rule.ArtistPHashMismatch{{
					ArtistID: "artist-1", Name: "Artist One",
					Suspects: []rule.PHashCollision{{
						SlotIndex: 2, PHash: "a1b086a69ff0dccc",
						MatchedArtistID: "artist-2", MatchedArtistName: "Artist Two",
						MatchedSlotIndex: 0, Similarity: 0.96875, MatchCount: 1,
					}},
				}},
				Indeterminate: []rule.PHashIndeterminateSlot{
					{ArtistID: "artist-3", SlotIndex: 0, Reason: "no stored perceptual hash"},
					{ArtistID: "artist-3", SlotIndex: 1, Reason: "no stored perceptual hash"},
				},
			}, nil
		},
	}
	r := testRouterWithFanartPipeline(t, p)

	w := httptest.NewRecorder()
	r.handlePHashMismatchReport(w, phashReq(adminContext(), ""))

	if w.Code != http.StatusOK {
		t.Fatalf("admin should get 200; got %d (%s)", w.Code, w.Body.String())
	}
	var got rule.PHashMismatchReport
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding report: %v; body=%s", err, w.Body.String())
	}
	if got.SuspectSlots != 1 || len(got.PerArtist) != 1 {
		t.Fatalf("report lost its suspects: %+v", got)
	}
	s := got.PerArtist[0].Suspects[0]
	if s.MatchedArtistName != "Artist Two" || s.Similarity != 0.96875 {
		t.Fatalf("suspect attribution/score lost in transit: %+v", s)
	}
	if got.IndeterminateSlots != 2 || len(got.Indeterminate) != 2 {
		t.Fatalf("the could-not-evaluate bucket must reach the client or the report reads "+
			"as clean: %+v", got)
	}
	if got.FanartRegistry.ArtistsSkipped != 1 {
		t.Fatalf("registry coverage lost in transit: %+v", got.FanartRegistry)
	}
}

// TestPHashMismatchReport_ScopeAndToleranceReachTheDetector proves the query
// params are actually plumbed rather than silently ignored.
func TestPHashMismatchReport_ScopeAndToleranceReachTheDetector(t *testing.T) {
	t.Parallel()
	p := &phashCapablePipeline{stubPipeline: &stubPipeline{}}
	r := testRouterWithFanartPipeline(t, p)

	w := httptest.NewRecorder()
	r.handlePHashMismatchReport(w, phashReq(adminContext(), "?artist_id=artist-9&tolerance=0.85"))

	if w.Code != http.StatusOK {
		t.Fatalf("want 200; got %d (%s)", w.Code, w.Body.String())
	}
	if p.gotScope.ArtistID != "artist-9" {
		t.Errorf("artist_id reached the detector as %q, want artist-9", p.gotScope.ArtistID)
	}
	if p.gotScope.Tolerance != 0.85 {
		t.Errorf("tolerance reached the detector as %v, want 0.85", p.gotScope.Tolerance)
	}
}

// TestPHashMismatchReport_BadToleranceIsRejected: an out-of-range or
// unparsable cutoff is a 400, not a silent fallback to the default. A caller
// who asked for one operating point must not be handed another on a report
// that decides what gets deleted.
func TestPHashMismatchReport_BadToleranceIsRejected(t *testing.T) {
	t.Parallel()
	// "NaN"/"nan" are not padding: ParseFloat accepts them, and every IEEE-754
	// comparison against NaN is false, so a `tol <= 0 || tol > 1` guard ADMITS
	// NaN. Without these two literals this table passes against a handler that
	// forwards NaN to the detector, where it makes every slot a false suspect.
	for _, raw := range []string{"abc", "0", "-0.5", "1.5", "NaN", "nan"} {
		p := &phashCapablePipeline{stubPipeline: &stubPipeline{}}
		r := testRouterWithFanartPipeline(t, p)

		w := httptest.NewRecorder()
		r.handlePHashMismatchReport(w, phashReq(adminContext(), "?tolerance="+raw))

		if w.Code != http.StatusBadRequest {
			t.Errorf("tolerance=%q should be 400; got %d", raw, w.Code)
		}
		if p.called {
			t.Errorf("tolerance=%q ran the scan anyway", raw)
		}
	}
}

// TestPHashMismatchReport_ScanErrorIs500 covers the detector's error path
// surfacing rather than degrading into an empty report.
func TestPHashMismatchReport_ScanErrorIs500(t *testing.T) {
	t.Parallel()
	p := &phashCapablePipeline{
		stubPipeline: &stubPipeline{},
		scanFn: func(_ context.Context, _ rule.PHashMismatchScope) (rule.PHashMismatchReport, error) {
			return rule.PHashMismatchReport{}, errors.New("boom")
		},
	}
	r := testRouterWithFanartPipeline(t, p)

	w := httptest.NewRecorder()
	r.handlePHashMismatchReport(w, phashReq(adminContext(), ""))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("a failed scan should get 500; got %d", w.Code)
	}
}
