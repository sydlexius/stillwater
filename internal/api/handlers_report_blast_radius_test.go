package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/templates"
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

	// Row IDENTITY, not just count: comparing len() alone would pass a loader
	// that returned the right NUMBER of the WRONG rows.
	if len(got.Rows) != len(want.Rows) {
		t.Fatalf("pane rows = %d, want %d (loader diverged from loadBlastRadius)", len(got.Rows), len(want.Rows))
	}
	for i := range want.Rows {
		if got.Rows[i].ID != want.Rows[i].ID {
			t.Errorf("pane row[%d] id = %q, want %q (same count, different rows)", i, got.Rows[i].ID, want.Rows[i].ID)
		}
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

// TestLoadReportsBlastRadiusData_PaginationArithmetic pins the page-count
// arithmetic at the three boundaries that matter.
//
// The exact-multiple case is the one that must not invent a trailing empty
// page: Total==k*PageSize is where a naive `Total/PageSize + 1` produces a page
// the operator can navigate to and find empty.
//
// Page sizes are all >= PageSizeMin (10), because getUserPageSize clamps
// anything smaller up to that floor -- a smaller value would silently test the
// default instead of the boundary the case names.
//
// Precondition asserted: the seeded Total really is what each case assumes.
// Without it, a fixture change would turn every expectation into a comparison
// against the wrong arithmetic.
//
// Mutation proving teeth: changing the loader's `if Total%PageSize > 0` guard
// to an unconditional `totalPages++` fails the exact-multiple case (want 2, got
// 3); dropping the `++` fails the remainder case (want 3, got 2).
func TestLoadReportsBlastRadiusData_PaginationArithmetic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		rows      int
		pageSize  int
		wantPages int
	}{
		{"total zero, genuinely empty report", 0, 10, 0},
		{"total less than one page", 6, 10, 1},
		{"total an exact multiple of page size", 20, 10, 2},
		{"total greater than one page with a remainder", 21, 10, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, _, _ := testRouterWithHistory(t)
			seedAPIBlastRows(t, r, tc.rows)

			got := loadBlastPane(t, r, fmt.Sprintf("?page_size=%d", tc.pageSize))

			if got.Counts.Total != tc.rows {
				t.Fatalf("precondition: seeded Total = %d, want %d; the page-count expectation assumes %d",
					got.Counts.Total, tc.rows, tc.rows)
			}
			// Total==0 must yield ZERO pages, not one empty page: the pager is
			// hidden behind TotalPages > 1, but a phantom page 1 would still
			// make "Page 1 of 1" true of a report with nothing in it.
			if got.Pagination.TotalPages != tc.wantPages {
				t.Errorf("TotalPages = %d, want %d (Total=%d, PageSize=%d)",
					got.Pagination.TotalPages, tc.wantPages, tc.rows, tc.pageSize)
			}
		})
	}
}

// TestLoadReportsBlastRadiusData_ClampsPagePastTheEnd covers the defect this
// branch fixes: a page beyond the last one used to return zero rows, which made
// the template print the LIBRARY-WIDE "nothing recorded" all-clear over a report
// that was simultaneously reporting damage.
//
// Reached by the ordinary recovery flow, not just a typed URL -- restoring the
// rows on the last page shrinks the set below the page the operator is on.
//
// Mutation proving teeth: deleting the clamp block in loadReportsBlastRadiusData
// returns CurrentPage=99 with zero rows, failing both assertions here and the
// rendered all-clear assertion in the companion test below.
func TestLoadReportsBlastRadiusData_ClampsPagePastTheEnd(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastRows(t, r, 25)

	got := loadBlastPane(t, r, "?page_size=10&page=99")

	if got.Pagination.TotalPages == 0 {
		t.Fatalf("precondition: report has no pages, so there is no out-of-range page to clamp")
	}
	if got.Pagination.CurrentPage != got.Pagination.TotalPages {
		t.Errorf("CurrentPage = %d, want %d (page past the end must clamp to the last real page)",
			got.Pagination.CurrentPage, got.Pagination.TotalPages)
	}
	// The rows must be re-fetched at the clamped offset. Clamping only the
	// caption would leave this empty and render "Page N of N" above an empty
	// table -- a different false statement rather than a fix.
	if len(got.Rows) == 0 {
		t.Errorf("clamped page returned 0 rows; the caption would claim a page that shows nothing")
	}
}

// TestHandleReportPage_BlastRadiusEmptyStateIsHonest asserts the rendered copy
// in both directions, because only one of them is ever true.
//
// Mutation proving teeth: deleting the clamp makes the out-of-range case render
// the all-clear (test fails); changing the template's len(Rows)==0 branch to
// always render, or never render, fails one arm or the other.
func TestHandleReportPage_BlastRadiusEmptyStateIsHonest(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastRows(t, r, 25)

	// Precondition: the fixture reports damage, so a library-wide all-clear is
	// FALSE for this database no matter which page is requested.
	if base := loadBlastPane(t, r, ""); base.Counts.Total == 0 {
		t.Fatalf("precondition: fixture reports no damage, so the all-clear would be true and this test is vacuous")
	}

	const allClear = "Nothing recorded"

	if body := renderBlastPane(t, r, "?page_size=10&page=99"); strings.Contains(body, allClear) {
		t.Errorf("out-of-range page rendered the library-wide all-clear %q while the report has damage to show", allClear)
	}
	// The same sentence is CORRECT when the filtered set is genuinely empty.
	if body := renderBlastPane(t, r, "?field=no_such_field_xyz"); !strings.Contains(body, allClear) {
		t.Errorf("genuinely empty result set did not render %q; the empty state must still work", allClear)
	}
}

// loadBlastPane runs the pane loader for a query string and fails on refusal.
func loadBlastPane(t *testing.T, r *Router, query string) templates.BlastRadiusData {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/reports/blast-radius"+query, nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	got, ok := r.loadReportsBlastRadiusData(w, req)
	if !ok {
		t.Fatalf("loadReportsBlastRadiusData(%q) returned ok=false; body: %s", query, w.Body.String())
	}
	return got
}

// renderBlastPane renders the full pane and returns the response body.
func renderBlastPane(t *testing.T, r *Router, query string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/reports/blast-radius"+query, nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req.SetPathValue("name", "blast-radius")
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()
	http.HandlerFunc(r.handleReportPage).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// seedAPIBlastRows seeds n automated blast-radius rows, enough to exercise page
// boundaries above PageSizeMin. Each row is a distinct artist+field so none is
// coalesced or reverted away.
func seedAPIBlastRows(t *testing.T, r *Router, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		seedAPIBlastChange(t, r,
			fmt.Sprintf("pg-%03d", i),
			fmt.Sprintf("pg-art-%03d", i),
			fmt.Sprintf("Paging Artist %03d", i),
			"biography", "a real bio", "", "scan",
			apiBlastBase.Add(time.Duration(i)*time.Minute))
	}
}

// TestLoadReportsBlastRadiusData_QueryFailureRefuses covers the loader's
// refusal path: when the underlying query fails, it must return ok=false with a
// 500 rather than fall through and render a page it could not load. On a
// data-loss recovery surface the fall-through failure mode is the dangerous one
// -- an empty Rows slice renders the library-wide "nothing recorded" all-clear,
// which is the reassuring answer produced by an error.
//
// The failure is injected by closing the database, which is proportionate:
// HistoryRepository has eight methods, and hand-writing a fake for one error
// branch would cost more than it proves.
//
// SCOPE, stated honestly: a closed handle fails the FIRST query, so this
// exercises the loadBlastRadius error branch. It does NOT reach the clamp's
// re-fetch error branch -- that one needs the first two queries to succeed and
// only the third to fail, which a closed handle cannot express. I verified this
// by instrumenting both branches: the test reaches the first and never the
// second. The re-fetch branch is therefore still uncovered, and I have said so
// rather than let this test imply otherwise.
//
// Mutation proving teeth: replacing the `return ..., false` with a fall-through
// makes the loader return ok=true on a failed query, which this test fails.
func TestLoadReportsBlastRadiusData_QueryFailureRefuses(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastRows(t, r, 12)

	// Precondition: the loader succeeds before the failure is injected, so a
	// later ok=false is attributable to the closed handle and not to the
	// request being malformed.
	if got := loadBlastPane(t, r, "?page_size=10"); got.Counts.Total != 12 {
		t.Fatalf("precondition: loader returned Total=%d before the failure was injected, want 12", got.Counts.Total)
	}

	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db to inject the query failure: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/reports/blast-radius?page_size=10", nil)
	req = req.WithContext(middleware.WithTestUserID(req.Context(), "test-user"))
	req = withI18nCtx(t, req)
	w := httptest.NewRecorder()

	if _, ok := r.loadReportsBlastRadiusData(w, req); ok {
		t.Fatalf("loader returned ok=true after the query failed; it must refuse rather than render a page it could not load")
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}
