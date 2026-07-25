package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// seedAPIBlastChange inserts one artist plus one metadata_changes row.
func seedAPIBlastChange(t *testing.T, r *Router, id, artistID, artistName, field, oldValue, newValue, source string, ts time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, '')`,
		artistID, artistName, artistName); err != nil {
		t.Fatalf("seeding artist %q: %v", artistID, err)
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, artistID, field, oldValue, newValue, source, ts.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seeding change %q: %v", id, err)
	}
}

var apiBlastBase = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// seedAPIBlastFixture seeds both attribution states and both damage classes,
// plus a recovered field that must not appear.
func seedAPIBlastFixture(t *testing.T, r *Router) {
	t.Helper()
	at := func(m int) time.Time { return apiBlastBase.Add(time.Duration(m) * time.Minute) }

	// Attributed automated: a scan cleared a biography, a provider replaced genres.
	seedAPIBlastChange(t, r, "a-scan", "art-1", "First Artist", "biography", "a real bio", "", "scan", at(1))
	seedAPIBlastChange(t, r, "a-prov", "art-1", "First Artist", "genres", "Rock", "Pop", "provider:musicbrainz", at(2))
	// Unattributable: recorded manual, may be an operator edit or earlier scan damage.
	seedAPIBlastChange(t, r, "u-man", "art-2", "Second Artist", "styles", "Grunge", "", "manual", at(3))
	// Recovered: must not be reported.
	seedAPIBlastChange(t, r, "x-dmg", "art-3", "Third Artist", "moods", "Energetic", "", "scan", at(4))
	seedAPIBlastChange(t, r, "x-rev", "art-3", "Third Artist", "moods", "", "Energetic", "revert", at(5))
}

// decodeBlastResponse runs the JSON handler and decodes the envelope.
func decodeBlastResponse(t *testing.T, r *Router, query string) blastRadiusResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius"+query, nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportBlastRadius(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got blastRadiusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v; body: %s", err, w.Body.String())
	}
	return got
}

func blastRespIDs(resp blastRadiusResponse) []string {
	out := make([]string, len(resp.Rows))
	for i := range resp.Rows {
		out[i] = resp.Rows[i].ID
	}
	return out
}

// TestHandleReportBlastRadius_UnknownBucketIsCountedAndReturned is the
// anti-"unknown rendered as clean" guard at the API boundary, and the most
// important test in this file.
//
// A row recorded as "manual" cannot be attributed: scan-driven changes only
// began recording source="scan" on 2026-07-19, so an earlier scan that cleared
// a value is indistinguishable from the operator's own edit. An API response
// that omits those rows, folds their count into the automated one, or labels
// them as attributed would under-report real data loss while looking complete.
//
// All three are asserted: the row is PRESENT, it is LABELED unknown, and it is
// COUNTED in its own bucket.
func TestHandleReportBlastRadius_UnknownBucketIsCountedAndReturned(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastFixture(t, r)

	got := decodeBlastResponse(t, r, "")

	var found *artist.BlastRadiusRow
	for i := range got.Rows {
		if got.Rows[i].ID == "u-man" {
			found = &got.Rows[i]
		}
	}
	if found == nil {
		t.Fatalf("the unattributable row was DROPPED from the API response; "+
			"pre-cutoff damage would be invisible to any API consumer. got %v",
			blastRespIDs(got))
	}
	if found.Attribution != artist.BlastAttributionUnknown {
		t.Errorf("row attribution = %q, want %q; a manual-sourced row must never "+
			"be returned as proven-clean", found.Attribution, artist.BlastAttributionUnknown)
	}

	if got.Counts.Unknown != 1 {
		t.Errorf("counts.unknown = %d, want 1", got.Counts.Unknown)
	}
	if got.Counts.Automated != 2 {
		t.Errorf("counts.automated = %d, want 2", got.Counts.Automated)
	}
	// The dedicated attribution block must carry the same numbers, so a client
	// rendering only that block cannot miss the unknown bucket.
	if got.Attribution.Unknown != 1 {
		t.Errorf("attribution.unknown = %d, want 1", got.Attribution.Unknown)
	}
	if got.Attribution.Automated != 2 {
		t.Errorf("attribution.automated = %d, want 2", got.Attribution.Automated)
	}
	if got.Attribution.CutoffDate != artist.AttributionCutoffDate {
		t.Errorf("attribution.cutoff_date = %q, want %q",
			got.Attribution.CutoffDate, artist.AttributionCutoffDate)
	}
}

// TestHandleReportBlastRadius_UnknownCountSurvivesAnAutomatedOnlyFilter proves
// a filter cannot be used to make the unattributable rows disappear from the
// counts. The rows leave the list, as asked; the count stays.
func TestHandleReportBlastRadius_UnknownCountSurvivesAnAutomatedOnlyFilter(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastFixture(t, r)

	got := decodeBlastResponse(t, r, "?attribution=automated")

	for i := range got.Rows {
		if got.Rows[i].Attribution != artist.BlastAttributionAutomated {
			t.Errorf("automated-only filter returned a %q row", got.Rows[i].Attribution)
		}
	}
	if got.Counts.Unknown != 1 {
		t.Errorf("counts.unknown = %d under an automated-only filter, want 1; "+
			"narrowing the list must not zero the other bucket's count", got.Counts.Unknown)
	}
	if got.Attribution.Unknown != 1 {
		t.Errorf("attribution.unknown = %d under an automated-only filter, want 1",
			got.Attribution.Unknown)
	}
	// Total follows the filter because pagination needs it to.
	if got.Total != 2 {
		t.Errorf("total = %d under an automated-only filter, want 2", got.Total)
	}
}

// TestHandleReportBlastRadius_CoverageIsDerivedFromTrackableFields asserts the
// response states which fields the report can and cannot see, and that both
// lists are derived rather than written out.
//
// The uncovered assertion is the load-bearing half: disambiguation has no
// change history on the scan path, so its absence from the row list is not
// evidence it is undamaged, and the response has to say so.
func TestHandleReportBlastRadius_CoverageIsDerivedFromTrackableFields(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	got := decodeBlastResponse(t, r, "")

	covered := map[string]bool{}
	for _, f := range got.Coverage.CoveredFields {
		covered[f] = true
	}
	// The covered list must BE TrackableFields(), not a copy that can drift.
	want := artist.TrackableFields()
	if len(got.Coverage.CoveredFields) != len(want) {
		t.Errorf("covered_fields has %d entries, want %d (TrackableFields())",
			len(got.Coverage.CoveredFields), len(want))
	}
	for _, f := range want {
		if !covered[f] {
			t.Errorf("covered_fields is missing %q, which IS tracked", f)
		}
	}

	uncovered := map[string]bool{}
	for _, f := range got.Coverage.UncoveredFields {
		uncovered[f] = true
	}
	for _, f := range []string{"disambiguation", "name", "sort_name"} {
		if !uncovered[f] {
			t.Errorf("uncovered_fields is missing %q; damage to it leaves no "+
				"change history, so the response must say it cannot be reported", f)
		}
		if covered[f] {
			t.Errorf("%q appears in BOTH covered and uncovered", f)
		}
	}
}

// TestHandleReportBlastRadius_RecoveredFieldsAreNotReported proves a recovered
// field drops out at the API boundary, with a precondition so it cannot pass
// on a handler that returns nothing at all.
func TestHandleReportBlastRadius_RecoveredFieldsAreNotReported(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	at := func(m int) time.Time { return apiBlastBase.Add(time.Duration(m) * time.Minute) }

	seedAPIBlastChange(t, r, "rec-dmg", "rec-1", "Recovered Artist", "born", "1967-02-20", "", "scan", at(1))

	// PRECONDITION: reported while still destroyed.
	got := decodeBlastResponse(t, r, "")
	if len(got.Rows) != 1 {
		t.Fatalf("precondition failed: damage not reported; got %v", blastRespIDs(got))
	}

	seedAPIBlastChange(t, r, "rec-rev", "rec-1", "Recovered Artist", "born", "", "1967-02-20", "revert", at(2))

	got = decodeBlastResponse(t, r, "")
	if len(got.Rows) != 0 {
		t.Errorf("recovered field still reported: %v", blastRespIDs(got))
	}
	if got.Counts.Total != 0 {
		t.Errorf("counts.total = %d after recovery, want 0", got.Counts.Total)
	}
}

// TestHandleReportBlastRadius_EmptyReportEncodesAnArray guards the JSON shape:
// an empty report must encode "rows": [] and not "rows": null, so a client
// iterating the field does not have to special-case null.
func TestHandleReportBlastRadius_EmptyReportEncodesAnArray(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportBlastRadius(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"rows":[]`) {
		t.Errorf("empty report did not encode rows as []; body: %s", w.Body.String())
	}
}

// TestHandleReportBlastRadius_HostileFilterReturnsTheUnnarrowedReport proves a
// malformed query param yields the full report rather than a 400 or an empty
// list. A damage report that errors or silently empties on a bad param is a way
// to make data loss invisible.
func TestHandleReportBlastRadius_HostileFilterReturnsTheUnnarrowedReport(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastFixture(t, r)

	got := decodeBlastResponse(t, r, "?class=%27+OR+1%3D1+--&attribution=bogus&sort=nonsense")
	if len(got.Rows) != 3 {
		t.Errorf("hostile filter returned %d rows, want the full 3-row report: %v",
			len(got.Rows), blastRespIDs(got))
	}
}

// TestHandleReportBlastRadius_ServiceUnavailableWithoutHistory checks the
// handler fails loudly rather than returning an empty-looking clean report when
// the history service is not wired.
func TestHandleReportBlastRadius_ServiceUnavailableWithoutHistory(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	r.historyService = nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportBlastRadius(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; an unavailable history store must not "+
			"read as a report with no damage in it", w.Code)
	}
}

// TestHandleReportBlastRadiusExport_CarriesRowsAndCaveats checks the CSV.
//
// The caveat rows are asserted because a spreadsheet outlives the page it came
// from: a CSV of destroyed values with no statement of what it cannot see is
// the artifact most likely to be mistaken for a complete accounting.
func TestHandleReportBlastRadiusExport_CarriesRowsAndCaveats(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastFixture(t, r)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius/export", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportBlastRadiusExport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/csv" {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "blast-radius-report.csv") {
		t.Errorf("Content-Disposition = %q, want an attachment filename", cd)
	}

	// FieldsPerRecord -1: the caveat rows are single-column by design, so a
	// strict reader would reject the file it is supposed to be validating.
	cr := csv.NewReader(strings.NewReader(w.Body.String()))
	cr.FieldsPerRecord = -1
	records, err := cr.ReadAll()
	if err != nil {
		t.Fatalf("parsing CSV: %v; body: %s", err, w.Body.String())
	}
	if len(records) < 5 {
		t.Fatalf("CSV has %d records, want a header, 3 rows, and caveat notes", len(records))
	}
	if records[0][0] != "Artist" {
		t.Errorf("first header cell = %q, want %q", records[0][0], "Artist")
	}

	body := w.Body.String()
	// The unattributable row must be labeled in the file, not just present.
	if !strings.Contains(body, artist.BlastAttributionUnknown) {
		t.Error("CSV does not label any row unknown; the attribution limit is " +
			"invisible in the exported file")
	}
	for _, want := range []string{
		artist.AttributionCutoffDate, // the attribution caveat
		"disambiguation",             // named in the uncovered-fields caveat
		"deleted or merged",          // the retention caveat
	} {
		if !strings.Contains(body, want) {
			t.Errorf("CSV is missing the caveat text %q; a detached spreadsheet "+
				"would read as a complete accounting", want)
		}
	}
}

// TestHandleReportBlastRadiusExport_SanitizesFormulaInjection checks a value
// beginning with a formula trigger is neutralized. Artist names and metadata
// values are attacker-influenced in the sense that they come from scraped
// provider data and on-disk NFO files, not only from the operator.
func TestHandleReportBlastRadiusExport_SanitizesFormulaInjection(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIBlastChange(t, r, "inj", "inj-1", "Injection Artist", "biography",
		"=cmd|'/c calc'!A1", "", "scan", apiBlastBase)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius/export", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportBlastRadiusExport(w, req)

	if !strings.Contains(w.Body.String(), "'=cmd") {
		t.Errorf("formula-triggering value was not prefixed; body: %s", w.Body.String())
	}
}
