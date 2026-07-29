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

// The two audit-message wordings that exist in the wild for this rule. Both are
// seeded so the handler is exercised against the old form (which the entire
// already-affected population carries) as well as the newer, longer one.
const (
	apiMBIDOldForm = "set MBID to 5b11f4ce-a62d-471e-81fc-a69a8278c7da for Alpha Artist"
	apiMBIDNewForm = `set MusicBrainz ID 83d91898-7763-47d7-b03b-b92132375c47 for Zulu Artist ` +
		`(matched "Zulu Artist" via name-search, confidence 91, runner-up scored 62)`
)

// seedAPIMBIDChange inserts one artist plus one metadata_changes row. old_value
// is always ” because the rule-fix history path never records a previous value
// and this rule only ever ran on an artist with no MusicBrainz ID.
func seedAPIMBIDChange(t *testing.T, r *Router, id, artistID, artistName, message, source string, ts time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, '')`,
		artistID, artistName, artistName); err != nil {
		t.Fatalf("seeding artist %q: %v", artistID, err)
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES (?, ?, 'rule_fix', '', ?, ?, ?)`,
		id, artistID, message, source, ts.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seeding change %q: %v", id, err)
	}
}

func seedAPIMBIDProviderID(t *testing.T, r *Router, artistID, mbid string) {
	t.Helper()
	if _, err := r.db.ExecContext(context.Background(),
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES (?, 'musicbrainz', ?)`,
		artistID, mbid); err != nil {
		t.Fatalf("seeding provider id for %q: %v", artistID, err)
	}
}

var apiMBIDBase = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// seedAPIMBIDFixture seeds both message wordings, an artist with a live
// MusicBrainz ID and one without, and one row written by a DIFFERENT rule that
// must be excluded.
//
// Artist names run opposite to the timestamps ("Alpha" is oldest, "Zulu" is
// newer) so a sort assertion cannot pass by the two axes coinciding.
func seedAPIMBIDFixture(t *testing.T, r *Router) {
	t.Helper()
	at := func(m int) time.Time { return apiMBIDBase.Add(time.Duration(m) * time.Minute) }

	// Reported, old wording, has a live ID that DIFFERS from the recorded one.
	seedAPIMBIDChange(t, r, "m-old", "mb-alpha", "Alpha Artist", apiMBIDOldForm, artist.NFOMBIDReportSource, at(1))
	seedAPIMBIDProviderID(t, r, "mb-alpha", "ffffffff-0000-0000-0000-000000000000")

	// Reported, new wording, no provider row at all: its ID has since been
	// cleared, so the report must still list it and must say "none recorded".
	seedAPIMBIDChange(t, r, "m-new", "mb-zulu", "Zulu Artist", apiMBIDNewForm, artist.NFOMBIDReportSource, at(2))

	// NOT reported: a different rule wrote it. Present so the exclusion
	// assertions below are not vacuous.
	seedAPIMBIDChange(t, r, "x-other", "mb-bystander", "Bystander", "set biography for Bystander",
		"rule:bio_exists", at(3))
	seedAPIMBIDProviderID(t, r, "mb-bystander", "11111111-2222-3333-4444-555555555555")
}

// assertUnrelatedRowExists is the PRECONDITION check. Without it, "the
// unrelated row is absent from the report" would pass on a fixture that never
// inserted one.
func assertUnrelatedRowExists(t *testing.T, r *Router) {
	t.Helper()
	var n int
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM metadata_changes WHERE source = 'rule:bio_exists'`).Scan(&n); err != nil {
		t.Fatalf("counting unrelated rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("precondition: rows with an unrelated source = %d, want 1; "+
			"every exclusion assertion in this test would be vacuous", n)
	}
}

func decodeMBIDResponse(t *testing.T, r *Router, query string) nfoMBIDResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/nfo-has-mbid"+query, nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportNFOHasMBID(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got nfoMBIDResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v; body: %s", err, w.Body.String())
	}
	return got
}

func mbidRespIDs(resp nfoMBIDResponse) []string {
	out := make([]string, len(resp.Rows))
	for i := range resp.Rows {
		out[i] = resp.Rows[i].ID
	}
	return out
}

// TestHandleReportNFOHasMBID_ReportsRuleWritesInBothWordings is the core
// behavior assertion at the API boundary: both rule-written rows, both message
// wordings verbatim, and not the row a different rule wrote.
func TestHandleReportNFOHasMBID_ReportsRuleWritesInBothWordings(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIMBIDFixture(t, r)
	assertUnrelatedRowExists(t, r)

	got := decodeMBIDResponse(t, r, "")

	if len(got.Rows) != 2 {
		t.Fatalf("rows = %v, want exactly the 2 rule-written rows", mbidRespIDs(got))
	}
	for _, id := range mbidRespIDs(got) {
		if id == "x-other" {
			t.Errorf("a row written by a different rule leaked into the report: %v", mbidRespIDs(got))
		}
	}

	byID := map[string]artist.NFOMBIDWriteRow{}
	for i := range got.Rows {
		byID[got.Rows[i].ID] = got.Rows[i]
	}
	// The recorded note is shown verbatim in BOTH wordings. Nothing parses it,
	// so neither shape may be reshaped or dropped.
	if byID["m-old"].Message != apiMBIDOldForm {
		t.Errorf("m-old Message = %q, want the recorded text verbatim", byID["m-old"].Message)
	}
	if byID["m-new"].Message != apiMBIDNewForm {
		t.Errorf("m-new Message = %q, want the recorded text verbatim", byID["m-new"].Message)
	}

	if got.Counts.Writes != 2 || got.Counts.Artists != 2 {
		t.Errorf("counts = %+v, want Writes=2 Artists=2", got.Counts)
	}
	if got.Total != got.Counts.Total {
		t.Errorf("total = %d, want it to mirror counts.total (%d)", got.Total, got.Counts.Total)
	}
}

// TestHandleReportNFOHasMBID_CaveatsArePresentEvenWhenEmpty is the
// anti-"unknown rendered as clean" guard, and the most important test in this
// file.
//
// An empty row list with no statement of what the report cannot see reads as a
// clean bill of health, and this report is precisely the one that must not be
// read that way: three other code paths assign a MusicBrainz ID without
// recording anything, so an artist's absence proves nothing at all.
//
// Asserted on an EMPTY database on purpose. That is the state in which a
// conditional caveat block would be missing and the response most misleading.
func TestHandleReportNFOHasMBID_CaveatsArePresentEvenWhenEmpty(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)

	got := decodeMBIDResponse(t, r, "")

	if len(got.Rows) != 0 {
		t.Fatalf("rows = %v, want none on an empty database", mbidRespIDs(got))
	}
	if got.Rows == nil {
		t.Error("rows marshaled as null; it must be [] so a client cannot mistake it for a missing field")
	}

	// Each caveat is checked individually. A single "caveats is non-empty" check
	// would pass with five of the six missing.
	for name, text := range map[string]string{
		"scope":           got.Caveats.Scope,
		"floor":           got.Caveats.Floor,
		"retention":       got.Caveats.Retention,
		"no_prior_value":  got.Caveats.NoPriorValue,
		"not_confirmed":   got.Caveats.NotConfirmed,
		"message_wording": got.Caveats.MessageWording,
	} {
		if text == "" {
			t.Errorf("caveat %q is empty on an empty report; the response would read as a clean "+
				"bill of health for IDs this report cannot see", name)
		}
	}

	// The scope caveat is the load-bearing one: it must name the other writers
	// as invisible, not merely describe what the report does cover.
	if !strings.Contains(got.Caveats.Scope, "Identify") || !strings.Contains(got.Caveats.Scope, "bulk rule run") {
		t.Errorf("scope caveat = %q; it must name the other paths that assign an ID without "+
			"recording anything, or a reader will take this list as complete", got.Caveats.Scope)
	}
	if !strings.Contains(got.Caveats.Floor, "minimum") {
		t.Errorf("floor caveat = %q; it must say the counts are a minimum", got.Caveats.Floor)
	}
	if !strings.Contains(got.Caveats.Retention, "merged") {
		t.Errorf("retention caveat = %q; it must say merging an artist away destroys its history",
			got.Caveats.Retention)
	}
}

// TestHandleReportNFOHasMBID_MissingCurrentIDIsNotRenderedAsClean pins the
// nullable join at the API boundary. An artist whose MusicBrainz ID has since
// been cleared has no provider row: it must still be listed, and the response
// must distinguish "no ID recorded" from "an ID that is the empty string".
func TestHandleReportNFOHasMBID_MissingCurrentIDIsNotRenderedAsClean(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIMBIDFixture(t, r)

	// PRECONDITION: mb-zulu genuinely has no provider row.
	var n int
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM artist_provider_ids WHERE artist_id = 'mb-zulu'`).Scan(&n); err != nil {
		t.Fatalf("counting provider ids: %v", err)
	}
	if n != 0 {
		t.Fatalf("precondition: mb-zulu has %d provider rows, want 0", n)
	}

	got := decodeMBIDResponse(t, r, "")
	byID := map[string]artist.NFOMBIDWriteRow{}
	for i := range got.Rows {
		byID[got.Rows[i].ID] = got.Rows[i]
	}

	cleared, ok := byID["m-new"]
	if !ok {
		t.Fatalf("the artist with no current MusicBrainz ID was dropped from the report: %v",
			mbidRespIDs(got))
	}
	if cleared.HasCurrentMusicBrainzID {
		t.Error("m-new has_current_musicbrainz_id = true, want false")
	}

	// The live value, not the one in the recorded note. m-old's note records a
	// different ID from the one the artist carries now, so a handler that read
	// the note would fail here.
	present := byID["m-old"]
	if !present.HasCurrentMusicBrainzID {
		t.Error("m-old has_current_musicbrainz_id = false, want true")
	}
	if present.CurrentMusicBrainzID != "ffffffff-0000-0000-0000-000000000000" {
		t.Errorf("m-old current_musicbrainz_id = %q, want the live value", present.CurrentMusicBrainzID)
	}
}

// TestHandleReportNFOHasMBID_HostileFilterReturnsTheUnnarrowedReport proves a
// malformed query parameter yields the full report rather than a 400 or an empty
// list. A report about possible misidentification that errors or empties on a
// bad parameter is a way to make the affected artists invisible.
func TestHandleReportNFOHasMBID_HostileFilterReturnsTheUnnarrowedReport(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIMBIDFixture(t, r)

	got := decodeMBIDResponse(t, r, "?sort=%27+OR+1%3D1+--&order=sideways&page=-4")
	if len(got.Rows) != 2 {
		t.Errorf("hostile filter returned %d rows, want the full 2-row report: %v",
			len(got.Rows), mbidRespIDs(got))
	}
	if got.Page != 1 {
		t.Errorf("page = %d, want 1; a negative page must clamp rather than compute a negative offset", got.Page)
	}
}

// TestHandleReportNFOHasMBID_SortingAndPagination walks both sort keys and one
// page window through the handler, so the query-parameter plumbing is exercised
// rather than just the repository.
func TestHandleReportNFOHasMBID_SortingAndPagination(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIMBIDFixture(t, r)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		// "Zulu Artist" is the NEWER row, so created_at desc and artist_name desc
		// happen to agree here; artist_name asc is the discriminating case below.
		{"default is newest first", "", []string{"m-new", "m-old"}},
		{"created_at asc", "?sort=created_at&order=asc", []string{"m-old", "m-new"}},
		{"artist_name asc puts Alpha first", "?sort=artist_name&order=asc", []string{"m-old", "m-new"}},
		{"artist_name desc puts Zulu first", "?sort=artist_name&order=desc", []string{"m-new", "m-old"}},
		// page_size clamps up to the API's minimum of 10, so a two-row fixture
		// cannot be split across pages here. The repository test walks the real
		// page boundaries one row at a time; this case exists to prove the page
		// parameter reaches the offset arithmetic at all.
		{"page past the end returns nothing", "?page=2&page_size=10", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decodeMBIDResponse(t, r, tc.query)
			ids := mbidRespIDs(got)
			if len(ids) != len(tc.want) {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
			for i := range tc.want {
				if ids[i] != tc.want[i] {
					t.Fatalf("ids = %v, want %v", ids, tc.want)
				}
			}
			// The counts describe the whole result set, so a page window must
			// never narrow them.
			if got.Counts.Writes != 2 {
				t.Errorf("counts.writes = %d, want 2 regardless of the page window", got.Counts.Writes)
			}
		})
	}
}

// TestHandleReportNFOHasMBID_ServiceUnavailableWithoutHistory checks the handler
// fails loudly rather than returning an empty-looking clean report when the
// history service is not wired.
func TestHandleReportNFOHasMBID_ServiceUnavailableWithoutHistory(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	r.historyService = nil

	for _, tc := range []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"json", "/api/v1/reports/nfo-has-mbid", r.handleReportNFOHasMBID},
		{"export", "/api/v1/reports/nfo-has-mbid/export", r.handleReportNFOHasMBIDExport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req = req.WithContext(adminContext())
			w := httptest.NewRecorder()
			tc.handler(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503; an unavailable history store must not read as "+
					"a report with no misidentified artists in it", w.Code)
			}
		})
	}
}

// TestHandleReportNFOHasMBID_QueryErrorSurfacesAs500 proves a repository failure
// becomes a 500 rather than an empty-but-successful report.
func TestHandleReportNFOHasMBID_QueryErrorSurfacesAs500(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	r.historyService = artist.NewHistoryServiceWithRepo(listOKCountErrHistoryRepo{
		delegate: r.historyService.Repo(),
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/nfo-has-mbid", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportNFOHasMBID(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when the count query fails; serving rows with a zeroed "+
			"count block would misreport the very numbers this report exists to provide. body: %s",
			w.Code, w.Body.String())
	}
}

// TestHandleReportNFOHasMBIDExport_CarriesRowsAndCaveats checks the CSV.
//
// The caveat rows are asserted because a spreadsheet outlives the page it came
// from: a CSV of rule-written IDs with no statement of what it cannot see is
// the artifact most likely to be mistaken for the complete set of
// machine-assigned IDs.
func TestHandleReportNFOHasMBIDExport_CarriesRowsAndCaveats(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIMBIDFixture(t, r)
	assertUnrelatedRowExists(t, r)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/nfo-has-mbid/export", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportNFOHasMBIDExport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/csv" {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="nfo-has-mbid-report.csv"`) {
		t.Errorf("Content-Disposition = %q, want a CSV attachment", cd)
	}

	// FieldsPerRecord -1: the caveat rows are single-column by design, so a
	// strict reader would reject the file the handler deliberately produces.
	cr := csv.NewReader(strings.NewReader(w.Body.String()))
	cr.FieldsPerRecord = -1
	records, err := cr.ReadAll()
	if err != nil {
		t.Fatalf("parsing CSV: %v; body: %s", err, w.Body.String())
	}
	if len(records) == 0 {
		t.Fatal("CSV is empty")
	}
	if records[0][0] != "Artist" {
		t.Errorf("header row = %v, want it to start with Artist", records[0])
	}

	body := w.Body.String()
	// Both rule-written artists are present; the bystander is not.
	for _, want := range []string{"Alpha Artist", "Zulu Artist"} {
		if !strings.Contains(body, want) {
			t.Errorf("CSV is missing artist %q", want)
		}
	}
	if strings.Contains(body, "Bystander") {
		t.Error("CSV includes an artist whose ID a different rule wrote")
	}

	// The missing current ID is spelled out. A blank cell in a spreadsheet reads
	// as nothing to see, and this is the column an operator acts on.
	if !strings.Contains(body, "none recorded") {
		t.Error(`CSV does not say "none recorded" for the artist with no current MusicBrainz ID; ` +
			"a blank cell there reads as a clean value")
	}

	// Every caveat travels with the file. Matched against the PARSED cells, not
	// the raw body: a caveat containing a double quote is escaped on the wire, so
	// a raw-substring check would report a missing note that is actually there.
	cells := map[string]bool{}
	for _, rec := range records {
		for _, cell := range rec {
			cells[cell] = true
		}
	}
	c := nfoMBIDCaveatBlock()
	for name, text := range map[string]string{
		"scope":           c.Scope,
		"floor":           c.Floor,
		"retention":       c.Retention,
		"no_prior_value":  c.NoPriorValue,
		"not_confirmed":   c.NotConfirmed,
		"message_wording": c.MessageWording,
	} {
		if !cells["NOTE: "+text] {
			t.Errorf("CSV is missing the %q caveat note; the file would read as a complete "+
				"accounting of machine-assigned IDs", name)
		}
	}
}

// TestHandleReportNFOHasMBIDExport_SanitizesFormulaCells proves a hostile artist
// name or recorded note cannot become a live formula when the CSV is opened.
func TestHandleReportNFOHasMBIDExport_SanitizesFormulaCells(t *testing.T) {
	t.Parallel()
	r, _, _ := testRouterWithHistory(t)
	seedAPIMBIDChange(t, r, "m-evil", "mb-evil", `=cmd|' /c calc'!A1`,
		`=HYPERLINK("http://example.invalid","click")`, artist.NFOMBIDReportSource, apiMBIDBase)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/reports/nfo-has-mbid/export", nil)
	req = req.WithContext(adminContext())
	w := httptest.NewRecorder()
	r.handleReportNFOHasMBIDExport(w, req)

	cr := csv.NewReader(strings.NewReader(w.Body.String()))
	cr.FieldsPerRecord = -1
	records, err := cr.ReadAll()
	if err != nil {
		t.Fatalf("parsing CSV: %v", err)
	}
	if len(records) < 2 {
		t.Fatalf("CSV has %d records, want a header plus at least one data row", len(records))
	}
	// records[1] is the seeded row: artist name in column 0, recorded note in 2.
	for _, col := range []int{0, 2} {
		cell := records[1][col]
		if strings.HasPrefix(cell, "=") {
			t.Errorf("cell %d = %q; a leading = would execute as a formula on open", col, cell)
		}
		if !strings.HasPrefix(cell, "'") {
			t.Errorf("cell %d = %q, want the sanitizer's leading apostrophe", col, cell)
		}
	}
}

// TestCollectNFOMBIDExportRows_TruncationBoundary pins the one decision the
// export makes about its own completeness: whether to tell the operator rows
// were left behind.
//
// The boundary matters because nfoMBIDExportPageSize divides nfoMBIDExportCap
// exactly. The obvious formulation, "len(collected) >= cap", cannot tell "the cap
// cut the result set short" apart from "the result set happened to end exactly at
// the cap" and claims a truncation for both. On a report whose whole premise is
// not misleading the operator about what it captured, that is a false claim of
// incompleteness.
//
// The page query is a stub rather than seeded rows: the interesting cases sit at
// 10000 and 10001 rows, which is far too slow to seed in a unit test. The stub
// serves whole pages of the requested size exactly as the real query does, so
// the arithmetic under test is the production arithmetic.
func TestCollectNFOMBIDExportRows_TruncationBoundary(t *testing.T) {
	t.Parallel()

	pagerOver := func(total int) func(context.Context, artist.NFOMBIDFilter) ([]artist.NFOMBIDWriteRow, error) {
		return func(_ context.Context, f artist.NFOMBIDFilter) ([]artist.NFOMBIDWriteRow, error) {
			if f.Offset >= total {
				return nil, nil
			}
			end := f.Offset + f.Limit
			if end > total {
				end = total
			}
			page := make([]artist.NFOMBIDWriteRow, 0, end-f.Offset)
			for i := f.Offset; i < end; i++ {
				page = append(page, artist.NFOMBIDWriteRow{ID: fmt.Sprintf("row-%d", i)})
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
		{"one row under the cap", nfoMBIDExportCap - 1, nfoMBIDExportCap - 1, false},
		// The regression case: an exact multiple of the page size landing exactly
		// on the cap. Every row was captured, so no truncation note.
		{"exactly the cap", nfoMBIDExportCap, nfoMBIDExportCap, false},
		{"one row over the cap", nfoMBIDExportCap + 1, nfoMBIDExportCap, true},
		{"far over the cap", nfoMBIDExportCap * 2, nfoMBIDExportCap, true},
		{"empty result set", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := artist.NFOMBIDFilter{Limit: nfoMBIDExportPageSize}
			rows, truncated, err := collectNFOMBIDExportRows(context.Background(), f, pagerOver(tc.total))
			if err != nil {
				t.Fatalf("collectNFOMBIDExportRows: %v", err)
			}
			if len(rows) != tc.wantRows {
				t.Errorf("collected %d rows, want %d", len(rows), tc.wantRows)
			}
			if truncated != tc.wantTruncated {
				t.Errorf("truncated = %v, want %v (result set of %d rows, cap %d)",
					truncated, tc.wantTruncated, tc.total, nfoMBIDExportCap)
			}
			if len(rows) > nfoMBIDExportCap {
				t.Errorf("collected %d rows, which exceeds the cap of %d", len(rows), nfoMBIDExportCap)
			}
		})
	}
}

// TestCollectNFOMBIDExportRows_PropagatesQueryError pins that a failing page
// query aborts the walk rather than returning a short, silently-incomplete
// export. A partial CSV here would be the worst possible outcome: it reads as a
// complete accounting.
func TestCollectNFOMBIDExportRows_PropagatesQueryError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("history store unavailable")
	calls := 0
	listPage := func(_ context.Context, f artist.NFOMBIDFilter) ([]artist.NFOMBIDWriteRow, error) {
		calls++
		if calls == 1 {
			return make([]artist.NFOMBIDWriteRow, f.Limit), nil
		}
		return nil, wantErr
	}

	f := artist.NFOMBIDFilter{Limit: nfoMBIDExportPageSize}
	rows, truncated, err := collectNFOMBIDExportRows(context.Background(), f, listPage)
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

// TestNFOMBIDFilterFromRequest_IsSharedByBothHandlers proves the JSON report and
// the CSV export narrow and sort identically. They call one helper for exactly
// this reason: an export that quietly disagreed with the page it was downloaded
// from would be worse than no export at all.
func TestNFOMBIDFilterFromRequest_IsSharedByBothHandlers(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet,
		"/x?artist_id=mb-alpha&sort=artist_name&order=asc&page=3&page_size=7", nil)
	got := nfoMBIDFilterFromRequest(req)

	if got.ArtistID != "mb-alpha" {
		t.Errorf("ArtistID = %q, want mb-alpha", got.ArtistID)
	}
	if got.Sort != artist.NFOMBIDSortArtistName {
		t.Errorf("Sort = %q, want %q", got.Sort, artist.NFOMBIDSortArtistName)
	}
	if got.Order != "asc" {
		t.Errorf("Order = %q, want asc", got.Order)
	}
	// The page window is deliberately NOT read here. The export overwrites it
	// with its own fixed page size, and reading page/page_size in the shared
	// helper would make an export silently obey a caller's pagination.
	if got.Offset != 0 {
		t.Errorf("Offset = %d, want 0; the shared helper must not apply a page window", got.Offset)
	}
}
