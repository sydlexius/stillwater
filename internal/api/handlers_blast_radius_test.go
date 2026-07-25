package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
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

// TestHandleReportBlastRadiusExport_SanitizesEveryColumn is the mutation-proof
// sanitization test: EVERY emitted cell that can carry attacker-influenced
// text (artist name, field, old value, new value, source) must be sanitized,
// not just one of them. Each of the four trigger characters (= + - @) is
// planted in a different column so a sanitizer that only guards one column
// cannot pass by accident.
//
// This test is mutation-proved: with sanitizeCSV's call sites removed (or the
// function turned into a no-op), this test goes RED. See the companion
// verification note in the report to the lead for the actual mutation run.
func TestHandleReportBlastRadiusExport_SanitizesEveryColumn(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	// Plant a distinct trigger character in each attacker-reachable column:
	// artist name (=), field is fixed by the schema so we use old/new value and
	// source instead to cover the remaining three triggers.
	seedAPIBlastChange(t, r, "inj-name", "inj-name-1", "=SUM(A1:A9)", "biography",
		"old val", "new val", "scan", apiBlastBase)
	seedAPIBlastChange(t, r, "inj-old", "inj-old-1", "Old Value Artist", "biography",
		"+cmd|'/c calc'!A1", "harmless new", "scan", apiBlastBase.Add(time.Minute))
	seedAPIBlastChange(t, r, "inj-new", "inj-new-1", "New Value Artist", "biography",
		"harmless old", "-cmd|'/c calc'!A1", "scan", apiBlastBase.Add(2*time.Minute))
	// Source is normally one of a small allow-listed set (manual/scan/provider:*
	// /rule:*/import/revert), but the column is still written through sanitizeCSV
	// like every other cell, so prove that too using a value the DB layer does
	// not validate.
	seedAPIBlastChange(t, r, "inj-src", "inj-src-1", "Source Artist", "biography",
		"old", "new", "@cmd|'/c calc'!A1", apiBlastBase.Add(3*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius/export", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportBlastRadiusExport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	cr := csv.NewReader(strings.NewReader(w.Body.String()))
	cr.FieldsPerRecord = -1
	records, err := cr.ReadAll()
	if err != nil {
		t.Fatalf("parsing CSV: %v; body: %s", err, w.Body.String())
	}

	// Column order: Artist, Field, Previous Value, Current Value, Change,
	// Source, Attribution, When.
	const (
		colArtist = 0
		colOld    = 2
		colNew    = 3
		colSource = 5
	)

	byArtist := map[string][]string{}
	for _, rec := range records {
		if len(rec) < 8 {
			continue // caveat note rows are single-column
		}
		byArtist[rec[colArtist]] = rec
	}

	nameRow, ok := byArtist["'=SUM(A1:A9)"]
	if !ok {
		t.Fatalf("artist-name column was not sanitized; want a row keyed on %q, got artists: %v",
			"'=SUM(A1:A9)", blastRadiusTestArtistKeys(byArtist))
	}
	_ = nameRow

	oldRow, ok := byArtist["Old Value Artist"]
	if !ok {
		t.Fatalf("row for Old Value Artist missing entirely")
	}
	if !strings.HasPrefix(oldRow[colOld], "'+cmd") {
		t.Errorf("previous-value column was not sanitized: got %q", oldRow[colOld])
	}

	newRow, ok := byArtist["New Value Artist"]
	if !ok {
		t.Fatalf("row for New Value Artist missing entirely")
	}
	if !strings.HasPrefix(newRow[colNew], "'-cmd") {
		t.Errorf("current-value column was not sanitized: got %q", newRow[colNew])
	}

	srcRow, ok := byArtist["Source Artist"]
	if !ok {
		t.Fatalf("row for Source Artist missing entirely")
	}
	if !strings.HasPrefix(srcRow[colSource], "'@cmd") {
		t.Errorf("source column was not sanitized: got %q", srcRow[colSource])
	}
}

func blastRadiusTestArtistKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBlastRadiusCoverage_DerivesFromTrackableFields is a unit-level (no HTTP)
// check on blastRadiusCoverage itself: the covered list must literally BE
// artist.TrackableFields() (same length, same members), and the uncovered
// list must be every artist.EditableFieldsList() member that TrackableFields
// does not name. A hard-coded list that happens to match today would pass a
// looser check but silently drift the day a field is added to either side; this
// asserts the derivation, not just today's snapshot.
func TestBlastRadiusCoverage_DerivesFromTrackableFields(t *testing.T) {
	t.Parallel()

	covered, uncovered := blastRadiusCoverage()

	want := artist.TrackableFields()
	if len(covered) != len(want) {
		t.Fatalf("covered has %d entries, want %d matching TrackableFields()", len(covered), len(want))
	}
	wantSet := make(map[string]bool, len(want))
	for _, f := range want {
		wantSet[f] = true
	}
	for _, f := range covered {
		if !wantSet[f] {
			t.Errorf("covered contains %q, which is not in TrackableFields()", f)
		}
	}

	editable := artist.EditableFieldsList()
	coveredSet := make(map[string]bool, len(covered))
	for _, f := range covered {
		coveredSet[f] = true
	}
	var wantUncovered []string
	for _, f := range editable {
		if !coveredSet[f] {
			wantUncovered = append(wantUncovered, f)
		}
	}
	if len(uncovered) != len(wantUncovered) {
		t.Fatalf("uncovered has %d entries, want %d (editable minus covered)", len(uncovered), len(wantUncovered))
	}
	uncoveredSet := make(map[string]bool, len(uncovered))
	for _, f := range uncovered {
		uncoveredSet[f] = true
	}
	for _, f := range wantUncovered {
		if !uncoveredSet[f] {
			t.Errorf("uncovered is missing %q", f)
		}
	}
	// No field may appear in both lists.
	for _, f := range covered {
		if uncoveredSet[f] {
			t.Errorf("%q appears in both covered and uncovered", f)
		}
	}
}

// TestBlastRadiusFilterFromRequest_CoercesUnrecognizedValues drives
// blastRadiusFilterFromRequest directly (rather than through the JSON
// handler) so the coercion behavior of every query param is pinned
// independent of what the downstream query does with the filter. Empty
// string and outright nonsense are both exercised because Validate treats
// them identically (fall through to the default), and a coercion bug that
// only shows up for one of the two would otherwise hide.
func TestBlastRadiusFilterFromRequest_CoercesUnrecognizedValues(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	cases := []struct {
		name  string
		query string
	}{
		{"empty query", ""},
		{"nonsense values", "?class=bogus&attribution=bogus&sort=bogus&order=bogus"},
		{"sql-ish injection attempt", "?class=%27+OR+1%3D1+--&attribution=%27&sort=%27&order=%27"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius"+tc.query, nil)
			req = req.WithContext(adminContext())

			f := r.blastRadiusPagedFilterFromRequest(req)

			if f.Class != artist.BlastScopeAll {
				t.Errorf("Class = %q, want the coerced default %q", f.Class, artist.BlastScopeAll)
			}
			if f.Attribution != artist.BlastScopeAll {
				t.Errorf("Attribution = %q, want the coerced default %q", f.Attribution, artist.BlastScopeAll)
			}
			if f.Sort != artist.BlastSortCreatedAt {
				t.Errorf("Sort = %q, want the coerced default %q", f.Sort, artist.BlastSortCreatedAt)
			}
			if f.Order != "desc" {
				t.Errorf("Order = %q, want the coerced default %q", f.Order, "desc")
			}
			if f.Limit <= 0 || f.Limit > 500 {
				t.Errorf("Limit = %d, want a positive value clamped to <=500", f.Limit)
			}
		})
	}
}

// TestBlastRadiusFilterFromRequest_PageAndPageSize pins the page/offset
// arithmetic and the page_size query-param path (both go through intQuery,
// which handlers_blast_radius.go calls directly and which had no coverage
// under this file).
func TestBlastRadiusFilterFromRequest_PageAndPageSize(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	cases := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{"default page and page_size", "", 50, 0},
		{"page 2 with explicit page_size", "?page=2&page_size=20", 20, 20},
		{"page 0 clamps to page 1", "?page=0&page_size=10", 10, 0},
		{"negative page clamps to page 1", "?page=-5&page_size=10", 10, 0},
		{"non-numeric page_size falls back to default", "?page_size=notanumber", 50, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius"+tc.query, nil)
			req = req.WithContext(adminContext())

			f := r.blastRadiusPagedFilterFromRequest(req)

			if f.Limit != tc.wantLimit {
				t.Errorf("Limit = %d, want %d", f.Limit, tc.wantLimit)
			}
			if f.Offset != tc.wantOffset {
				t.Errorf("Offset = %d, want %d", f.Offset, tc.wantOffset)
			}
		})
	}
}

// TestHandleReportBlastRadius_RepositoryErrorSurfacesAs500 exercises the
// ListBlastRadius error path in handleReportBlastRadius: a repository failure
// must surface as a 500, never as a 200 with an empty-looking report (which
// would read as "nothing was destroyed" instead of "we could not check").
func TestHandleReportBlastRadius_RepositoryErrorSurfacesAs500(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithErrHistoryService(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportBlastRadius(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on a repository error; body: %s", w.Code, w.Body.String())
	}
}

// TestHandleReportBlastRadius_CountErrorSurfacesAs500 exercises the second
// query in loadBlastRadius: even when ListBlastRadius would succeed, a
// failure from CountBlastRadius must still 500 rather than serve rows with a
// zeroed-out counts block (which would silently misreport the honesty
// numbers this feature exists to guarantee).
func TestHandleReportBlastRadius_CountErrorSurfacesAs500(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	r.historyService = artist.NewHistoryServiceWithRepo(listOKCountErrHistoryRepo{
		delegate: r.historyService.Repo(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportBlastRadius(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when CountBlastRadius fails; body: %s", w.Code, w.Body.String())
	}
}

// listOKCountErrHistoryRepo delegates ListBlastRadius (and everything else)
// to a real repo but always fails CountBlastRadius, isolating the second
// query's error path in loadBlastRadius from the first's.
type listOKCountErrHistoryRepo struct {
	delegate artist.HistoryRepository
}

func (l listOKCountErrHistoryRepo) Record(ctx context.Context, c *artist.MetadataChange) error {
	return l.delegate.Record(ctx, c)
}

func (l listOKCountErrHistoryRepo) GetByID(ctx context.Context, id string) (*artist.MetadataChange, error) {
	return l.delegate.GetByID(ctx, id)
}

func (l listOKCountErrHistoryRepo) List(ctx context.Context, artistID string, limit, offset int) ([]artist.MetadataChange, int, error) {
	return l.delegate.List(ctx, artistID, limit, offset)
}

func (l listOKCountErrHistoryRepo) ListGlobal(ctx context.Context, filter artist.GlobalHistoryFilter) ([]artist.MetadataChangeWithArtist, int, error) {
	return l.delegate.ListGlobal(ctx, filter)
}

func (l listOKCountErrHistoryRepo) ListBlastRadius(ctx context.Context, f artist.BlastRadiusFilter) ([]artist.BlastRadiusRow, error) {
	return l.delegate.ListBlastRadius(ctx, f)
}

func (l listOKCountErrHistoryRepo) CountBlastRadius(ctx context.Context, f artist.BlastRadiusFilter) (artist.BlastRadiusCounts, error) {
	return artist.BlastRadiusCounts{}, errors.New("simulated count failure")
}

// TestHandleReportBlastRadiusExport_RepositoryErrorAbortsCleanly exercises the
// export handler's error path when the FIRST page load fails, before any
// bytes have been written. Unlike the JSON handler, the export path collects
// all pages into memory before writing anything, so an error on the first
// (only, here) page still gets to choose its status code: 500, not a
// half-written CSV.
func TestHandleReportBlastRadiusExport_RepositoryErrorAbortsCleanly(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithErrHistoryService(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius/export", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()

	// Must not panic even though ListBlastRadius fails immediately on the
	// first page.
	r.handleReportBlastRadiusExport(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; a repository failure before any CSV bytes are "+
			"written must not read as a successful, if empty, export: body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "NOTE:") || strings.Contains(body, "Artist,Field") {
		t.Errorf("CSV content was written after a repository error that occurred before "+
			"any page succeeded; body: %s", body)
	}
}

// TestHandleReportBlastRadiusExport_ServiceUnavailableWithoutHistory mirrors
// the JSON handler's guard for the export path, which had no direct coverage.
func TestHandleReportBlastRadiusExport_ServiceUnavailableWithoutHistory(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	r.historyService = nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius/export", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportBlastRadiusExport(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when history service is unavailable", w.Code)
	}
}

// TestHandleReportBlastRadiusExport_MultiPageWalkCollectsAllRows seeds more
// rows than blastRadiusExportPageSize (200) so the pagination loop inside
// handleReportBlastRadiusExport actually iterates more than once and every
// page ends up in the CSV. The cap itself is not exercised here: seeding
// blastRadiusExportCap (10000) rows is too slow for a unit test, so the
// truncation flag is pinned by TestBlastRadiusExportTruncation below, which
// drives the same decision against the cap arithmetic directly.
func TestHandleReportBlastRadiusExport_MultiPageWalkCollectsAllRows(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	at := func(m int) time.Time { return apiBlastBase.Add(time.Duration(m) * time.Minute) }

	// blastRadiusExportPageSize is 200; seed 210 rows so the loop's "less than a
	// full page means we're done" exit condition is exercised on a real second
	// page rather than trivially on the first. ListBlastRadius keeps only the
	// MOST RECENT row per (artist_id, field), so each row needs a distinct
	// field to avoid collapsing into one; the field name itself is not
	// validated against an enum by this query.
	const rowCount = 210
	for i := 0; i < rowCount; i++ {
		id := fmt.Sprintf("multi-row-%03d", i)
		field := fmt.Sprintf("field-%03d", i)
		seedAPIBlastChange(t, r, id, "multi-artist", "Multi Artist", field,
			"old", "new", "scan", at(i))
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/blast-radius/export", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportBlastRadiusExport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body excerpt: %.300s", w.Code, w.Body.String())
	}
	cr := csv.NewReader(strings.NewReader(w.Body.String()))
	cr.FieldsPerRecord = -1
	records, err := cr.ReadAll()
	if err != nil {
		t.Fatalf("parsing CSV: %v", err)
	}
	// header + rowCount data rows + at least 4 caveat notes, no truncation note
	// since rowCount < blastRadiusExportCap.
	dataRows := 0
	for _, rec := range records[1:] {
		if len(rec) >= 8 {
			dataRows++
		}
	}
	if dataRows != rowCount {
		t.Errorf("CSV has %d data rows, want all %d seeded rows collected across the "+
			"multi-page walk", dataRows, rowCount)
	}
	if strings.Contains(w.Body.String(), "truncated at the row cap") {
		t.Errorf("truncation note present at %d rows, well under the %d cap", rowCount, blastRadiusExportCap)
	}
}

// TestCollectBlastRadiusExportRows_TruncationBoundary pins the one decision the
// export makes about its own completeness: whether to tell the operator rows
// were left behind.
//
// The boundary matters because blastRadiusExportPageSize divides
// blastRadiusExportCap exactly. The obvious formulation of the flag,
// "len(collected) >= cap", cannot tell "the cap cut the result set short" apart
// from "the result set happened to end exactly at the cap", and reports a
// truncation for both. On a report whose entire premise is not misleading the
// operator about what it captured, that is a false claim of incompleteness.
//
// The page query is a stub rather than seeded rows: the interesting cases sit
// at 10000 and 10001 rows, and seeding those is far too slow for a unit test.
// The stub serves whole pages of the requested size, exactly as the real query
// does, so the arithmetic under test is the production arithmetic.
func TestCollectBlastRadiusExportRows_TruncationBoundary(t *testing.T) {
	t.Parallel()

	// pagerOver returns a listPage stub backed by a result set of `total` rows.
	pagerOver := func(total int) func(context.Context, artist.BlastRadiusFilter) ([]artist.BlastRadiusRow, error) {
		return func(_ context.Context, f artist.BlastRadiusFilter) ([]artist.BlastRadiusRow, error) {
			if f.Offset >= total {
				return nil, nil
			}
			end := f.Offset + f.Limit
			if end > total {
				end = total
			}
			page := make([]artist.BlastRadiusRow, 0, end-f.Offset)
			for i := f.Offset; i < end; i++ {
				var row artist.BlastRadiusRow
				row.ID = fmt.Sprintf("row-%d", i)
				page = append(page, row)
			}
			return page, nil
		}
	}

	cases := []struct {
		name          string
		total         int
		wantRows      int
		wantTruncated bool
	}{
		{"well under the cap", 450, 450, false},
		{"one row under the cap", blastRadiusExportCap - 1, blastRadiusExportCap - 1, false},
		// The regression case: an exact multiple of the page size that lands
		// exactly on the cap. Every row was captured, so no truncation note.
		{"exactly the cap", blastRadiusExportCap, blastRadiusExportCap, false},
		{"one row over the cap", blastRadiusExportCap + 1, blastRadiusExportCap, true},
		{"far over the cap", blastRadiusExportCap * 2, blastRadiusExportCap, true},
		{"empty result set", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := artist.BlastRadiusFilter{Limit: blastRadiusExportPageSize}
			rows, truncated, err := collectBlastRadiusExportRows(context.Background(), f, pagerOver(tc.total))
			if err != nil {
				t.Fatalf("collectBlastRadiusExportRows: %v", err)
			}
			if len(rows) != tc.wantRows {
				t.Errorf("collected %d rows, want %d", len(rows), tc.wantRows)
			}
			if truncated != tc.wantTruncated {
				t.Errorf("truncated = %v, want %v (result set of %d rows, cap %d)",
					truncated, tc.wantTruncated, tc.total, blastRadiusExportCap)
			}
			if len(rows) > blastRadiusExportCap {
				t.Errorf("collected %d rows, which exceeds the cap of %d", len(rows), blastRadiusExportCap)
			}
		})
	}
}

// TestCollectBlastRadiusExportRows_PropagatesQueryError pins that a failing
// page query aborts the walk rather than returning a short, silently-incomplete
// export. A partial CSV here would be the worst possible outcome: it reads as a
// complete accounting of the damage.
func TestCollectBlastRadiusExportRows_PropagatesQueryError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("history store unavailable")
	calls := 0
	listPage := func(_ context.Context, f artist.BlastRadiusFilter) ([]artist.BlastRadiusRow, error) {
		calls++
		if calls == 1 {
			page := make([]artist.BlastRadiusRow, f.Limit)
			return page, nil
		}
		return nil, wantErr
	}

	f := artist.BlastRadiusFilter{Limit: blastRadiusExportPageSize}
	rows, truncated, err := collectBlastRadiusExportRows(context.Background(), f, listPage)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if rows != nil {
		t.Errorf("rows = %d rows, want nil so a failed walk cannot be written as a complete export", len(rows))
	}
	if truncated {
		t.Error("truncated = true on an error return, want false")
	}
}
