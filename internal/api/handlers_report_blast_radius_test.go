package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

// TestHandleReportPage_BlastRadiusDispatchesToPane verifies that
// serveReportsWorkspace routes ActiveReport=="blast-radius" to the dedicated
// pane rather than falling through to the placeholder. The mutation that
// gives this test teeth: removing the "blast-radius" case from
// serveReportsWorkspace's switch renders the placeholder instead, which this
// test would catch via the placeholder marker's presence and the pane
// marker's absence.
func TestHandleReportPage_BlastRadiusDispatchesToPane(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	h := http.HandlerFunc(r.handleReportPage)
	req := httptest.NewRequest(http.MethodGet, "/reports/blast-radius", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req.SetPathValue("name", "blast-radius")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "blast-radius-tbl") {
		t.Errorf("blast-radius pane (blast-radius-tbl) absent from response; dispatch fell through to a different pane")
	}
	if strings.Contains(body, "sw-rep-placeholder") {
		t.Errorf("blast-radius report rendered the placeholder pane instead of the dedicated one")
	}
}

// TestLoadReportsBlastRadiusData_DelegatesToLoadBlastRadius verifies the
// loader calls the SAME r.loadBlastRadius the JSON report and CSV export use,
// by seeding a fixture and checking the pane's coverage/attribution numbers
// match a direct call to loadBlastRadius for the same request. The mutation
// that gives this test teeth: pointing loadReportsBlastRadiusData at a
// fresh/parallel query (rather than r.loadBlastRadius) would still populate
// Rows, but the CoveredFields/UncoveredFields and Counts would drift the
// moment the two queries disagree -- exactly the failure the shared
// assembly point exists to prevent.
func TestLoadReportsBlastRadiusData_DelegatesToLoadBlastRadius(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastFixture(t, r)

	req := httptest.NewRequest(http.MethodGet, "/reports/blast-radius", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req = withI18nCtx(t, req)

	want, err := r.loadBlastRadius(req)
	if err != nil {
		t.Fatalf("loadBlastRadius: %v", err)
	}

	w := httptest.NewRecorder()
	got, ok := r.loadReportsBlastRadiusData(w, req)
	if !ok {
		t.Fatalf("loadReportsBlastRadiusData returned ok=false; body: %s", w.Body.String())
	}

	if len(got.Rows) != len(want.Rows) {
		t.Errorf("pane rows = %d, want %d (loader diverged from loadBlastRadius)", len(got.Rows), len(want.Rows))
	}
	if got.Counts != want.Counts {
		t.Errorf("pane counts = %+v, want %+v (loader diverged from loadBlastRadius)", got.Counts, want.Counts)
	}
	if strings.Join(got.CoveredFields, ",") != strings.Join(want.CoveredFields, ",") {
		t.Errorf("pane covered fields = %v, want %v", got.CoveredFields, want.CoveredFields)
	}
	if strings.Join(got.UncoveredFields, ",") != strings.Join(want.UncoveredFields, ",") {
		t.Errorf("pane uncovered fields = %v, want %v", got.UncoveredFields, want.UncoveredFields)
	}
}

// TestLoadReportsBlastRadiusData_ServiceUnavailable verifies the loader
// refuses cleanly (503, ok=false) when the history service is nil, matching
// handleReportBlastRadius's own guard, rather than panicking on a nil
// dereference.
func TestLoadReportsBlastRadiusData_ServiceUnavailable(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)
	r.historyService = nil

	req := httptest.NewRequest(http.MethodGet, "/reports/blast-radius", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()

	_, ok := r.loadReportsBlastRadiusData(w, req)
	if ok {
		t.Fatalf("loadReportsBlastRadiusData returned ok=true with a nil history service")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// TestHandleReportPage_BlastRadiusEmptyStateWording locks the "nothing
// recorded" empty-state copy (issue #2750 design requirement): absence of
// records is not evidence of no damage, so the pane must never say
// "clean" or "no damage". The mutation that gives this test teeth: changing
// the empty-state copy to "No damage found" or similar would still render an
// empty table (passing a bare "renders" check) but fails this exact string
// assertion.
func TestHandleReportPage_BlastRadiusEmptyStateWording(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	// No fixture seeded: the report has nothing to show.

	h := http.HandlerFunc(r.handleReportPage)
	req := httptest.NewRequest(http.MethodGet, "/reports/blast-radius", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req.SetPathValue("name", "blast-radius")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Nothing recorded") {
		t.Errorf("empty state must read \"Nothing recorded\"; body: %s", body)
	}
	lower := strings.ToLower(body)
	if strings.Contains(lower, "no damage") || strings.Contains(lower, ">clean<") {
		t.Errorf("empty state must never claim \"no damage\"/\"clean\" -- absence of records is not evidence of no damage; body: %s", body)
	}
}

// TestHandleReportPage_BlastRadiusUnknownCountAlwaysRendered is the
// #2692/#2686 "unknown rendered as clean" guard at the template layer,
// mirroring the API-layer test in handlers_blast_radius_test.go. Both the
// automated and unknown counts must render, unconditionally, in the caveat
// band -- not just the automated headline. The mutation that gives this
// test teeth: dropping the unknown count from repPaneBlastRadius's caveat
// band would still render the automated number (passing a bare "renders a
// number" check) but fails the explicit count-substring assertions below.
func TestHandleReportPage_BlastRadiusUnknownCountAlwaysRendered(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastFixture(t, r)

	h := http.HandlerFunc(r.handleReportPage)
	req := httptest.NewRequest(http.MethodGet, "/reports/blast-radius", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req.SetPathValue("name", "blast-radius")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// seedAPIBlastFixture seeds 2 automated and 1 unknown row.
	if !strings.Contains(body, "2 change(s) attributed to an automated writer") {
		t.Errorf("automated count (2) not rendered in caveat band; body: %s", body)
	}
	if !strings.Contains(body, "1 of unknown origin") {
		t.Errorf("unknown count (1) not rendered in caveat band; the unknown bucket must never be hidden; body: %s", body)
	}
	// The unattributable row itself must also carry a visible per-row
	// attribution label, per design ("every row renders its attribution
	// label ... a bulk restore over a mixed selection is a visibly informed
	// choice").
	if !strings.Contains(body, `data-attribution="unknown"`) {
		t.Errorf("no row carries data-attribution=\"unknown\"; the unattributable row's label was dropped; body: %s", body)
	}
}

// TestHandleReportPage_BlastRadiusCoverageMatchesTrackableFields verifies the
// coverage caveat is generated from artist.TrackableFields() at request time,
// never a value hardcoded in the template. The mutation that gives this test
// teeth: hardcoding the covered-field list in repPaneBlastRadius would pass
// today (the lists happen to match) but silently diverge the moment
// TrackableFields() changes -- exactly the drift this test pins against by
// asserting every current TrackableFields() entry appears in the rendered
// coverage line.
func TestHandleReportPage_BlastRadiusCoverageMatchesTrackableFields(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	h := http.HandlerFunc(r.handleReportPage)
	req := httptest.NewRequest(http.MethodGet, "/reports/blast-radius", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req.SetPathValue("name", "blast-radius")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	const marker = "covers these fields only: "
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("coverage caveat line (%q) not found in rendered body", marker)
	}
	end := strings.Index(body[start:], "</p>")
	if end < 0 {
		t.Fatalf("coverage caveat paragraph did not close with </p>")
	}
	coverageLine := body[start : start+end]
	for _, field := range artist.TrackableFields() {
		if !strings.Contains(coverageLine, field) {
			t.Errorf("covered field %q (from artist.TrackableFields()) missing from rendered coverage caveat line: %q", field, coverageLine)
		}
	}
}
