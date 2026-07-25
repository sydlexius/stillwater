// Package api -- handlers_blast_radius.go
//
// The blast-radius report (issue #2750): "which values that I set did an
// automated writer destroy, and where". Read-only. Every handler in this file
// is a GET and nothing here writes to the database.
//
// Recovery is deliberately NOT part of this surface. Seeing the damage is
// useful on its own and is safe to ship on its own; restoring it is a
// destructive operation that earns its own review.
//
// # THE TWO HONESTY REQUIREMENTS
//
// This report is about data the product destroyed, so the ways it can mislead
// matter more than the ways it can be slow. Two of them are structural, and
// both are surfaced to the operator rather than buried:
//
//  1. ATTRIBUTION. Any row whose source does not name a recognized automated
//     writer cannot be attributed, and "unknown" is the exact complement of
//     "automated" rather than the single value "manual" -- so a source the
//     allow-list does not recognize is counted as unknown instead of vanishing
//     from the totals. Most unknown rows are "manual": scan-driven changes only
//     started recording source="scan" on 2026-07-19 (commit 5942fa7a, PR #2641,
//     issue #2636), and before that they fell through to the "manual" default
//     and are indistinguishable from an operator's own edit. Those rows are
//     always listed and always counted separately. The counts ignore the
//     operator's attribution filter for exactly this reason -- see
//     artist.CountBlastRadius.
//
//  2. COVERAGE. The report can only see fields metadata_changes records, which
//     is exactly artist.TrackableFields(). Damage to disambiguation, name,
//     sort_name, or the provider IDs leaves no row at all, so this report
//     cannot show it. The covered and uncovered field lists are computed from
//     TrackableFields() rather than written out, so they cannot drift.
//
// Reporting either of those as a clean zero would be the "unknown rendered as
// clean" defect (#2692, #2686) that this feature exists to avoid repeating.
package api

import (
	"context"
	"encoding/csv"
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

// blastRadiusExportCap bounds a CSV export. Matches the compliance export's
// cap; a library with more destroyed fields than this has a bigger problem
// than pagination, and the truncation is reported in the response rather than
// applied silently.
const blastRadiusExportCap = 10000

// blastRadiusExportPageSize is the per-query page size used while walking the
// full result set for an export. Bounded so one export cannot pull an entire
// large library into memory in a single query.
const blastRadiusExportPageSize = 200

// blastRadiusFilterFromRequest builds the narrowing half of the query filter:
// everything except the page window. Both the JSON report and the CSV export
// narrow and sort identically; only their pagination differs, so the export
// path can reuse this without paying for a per-user page-size preference read
// it would immediately discard.
//
// It still calls Validate, so the returned filter is always safe to send to
// SQL on its own; that leaves Limit at Validate's own default, which every
// caller here overwrites with its own page window.
//
// Unrecognized class/attribution/sort/order values are coerced to their
// defaults by BlastRadiusFilter.Validate rather than rejected with a 400. That
// is deliberate and differs from the compliance report: this is a read-only
// damage report, and a malformed query param should show the operator the
// unnarrowed report, never an error page that hides how much was destroyed.
func blastRadiusFilterFromRequest(req *http.Request) artist.BlastRadiusFilter {
	q := req.URL.Query()
	f := artist.BlastRadiusFilter{
		Class:       q.Get("class"),
		Attribution: q.Get("attribution"),
		Field:       q.Get("field"),
		ArtistID:    q.Get("artist_id"),
		Sort:        q.Get("sort"),
		Order:       q.Get("order"),
	}
	f.Validate()
	return f
}

// blastRadiusPagedFilterFromRequest adds the caller's page window on top of
// blastRadiusFilterFromRequest, resolving page_size against the per-user
// preference. Only the paginated JSON report calls this; the export walks the
// whole result set with its own fixed page size.
func (r *Router) blastRadiusPagedFilterFromRequest(req *http.Request) artist.BlastRadiusFilter {
	userID := middleware.UserIDFromContext(req.Context())
	pageSize := r.getUserPageSize(req.Context(), userID, intQuery(req, "page_size", 0))

	page := intQuery(req, "page", 1)
	if page < 1 {
		page = 1
	}

	f := blastRadiusFilterFromRequest(req)
	f.Limit = pageSize
	f.Offset = (page - 1) * pageSize
	f.Validate()
	return f
}

// blastRadiusCoverage reports which metadata fields this report can and cannot
// see, derived from artist.TrackableFields().
//
// Both halves are computed, never written out. The covered list IS
// TrackableFields(); the uncovered list is every other editable field. If a
// field is added to history tracking, both lists move together and the
// operator-facing caveat stays true without anyone remembering to edit it.
func blastRadiusCoverage() (covered, uncovered []string) {
	covered = artist.TrackableFields()
	isCovered := make(map[string]bool, len(covered))
	for _, f := range covered {
		isCovered[f] = true
	}
	for _, f := range artist.EditableFieldsList() {
		if !isCovered[f] {
			uncovered = append(uncovered, f)
		}
	}
	return covered, uncovered
}

// blastRadiusResponse is the JSON envelope for the report.
type blastRadiusResponse struct {
	Rows []artist.BlastRadiusRow `json:"rows"`
	// Counts always carries BOTH attribution buckets, whatever the request
	// filtered to, so a caller cannot render a headline number that silently
	// omits the unattributable rows.
	Counts   artist.BlastRadiusCounts `json:"counts"`
	Page     int                      `json:"page"`
	PageSize int                      `json:"page_size"`
	// Total mirrors Counts.Total. Duplicated at the top level because every
	// other list endpoint in this API carries a "total", and a client that
	// reaches for the familiar key should not get a zero.
	Total int `json:"total"`
	// Attribution describes the limit on who-did-it, so an API consumer gets
	// the same caveat the web UI renders rather than a bare row list.
	Attribution blastRadiusAttributionInfo `json:"attribution"`
	// Coverage describes the limit on which-fields, for the same reason.
	Coverage blastRadiusCoverageInfo `json:"coverage"`
}

// blastRadiusAttributionInfo is the machine-readable form of the attribution
// caveat.
type blastRadiusAttributionInfo struct {
	// CutoffDate is when Stillwater began recording scan-driven changes
	// distinctly. Informational only: no query filters on it.
	CutoffDate string `json:"cutoff_date"`
	// Automated counts rows whose source names an automated writer.
	Automated int `json:"automated"`
	// Unknown counts every row whose source does NOT name an automated writer --
	// the exact complement of Automated, not just source="manual". A source the
	// allow-list does not recognize is unknown rather than uncounted, so the two
	// buckets are exhaustive and their sum cannot silently omit rows. Most of
	// these are "manual", which may be an operator's own edit or may be earlier
	// scan damage. Never folded into Automated.
	Unknown int `json:"unknown"`
}

// blastRadiusCoverageInfo is the machine-readable form of the coverage caveat.
type blastRadiusCoverageInfo struct {
	// CoveredFields are the fields this report can see.
	CoveredFields []string `json:"covered_fields"`
	// UncoveredFields are editable fields with no change history on the scan
	// path. Damage to these is invisible here, and a caller must not present
	// their absence from Rows as evidence they are undamaged.
	UncoveredFields []string `json:"uncovered_fields"`
}

// loadBlastRadius runs both queries and assembles the report. Shared by the
// JSON handler and the CSV export so the two cannot disagree.
func (r *Router) loadBlastRadius(req *http.Request) (blastRadiusView, error) {
	ctx := req.Context()
	f := r.blastRadiusPagedFilterFromRequest(req)

	rows, err := r.historyService.ListBlastRadius(ctx, f)
	if err != nil {
		return blastRadiusView{}, err
	}
	counts, err := r.historyService.CountBlastRadius(ctx, f)
	if err != nil {
		return blastRadiusView{}, err
	}

	covered, uncovered := blastRadiusCoverage()
	pageSize := f.Limit

	return blastRadiusView{
		Rows:            rows,
		Counts:          counts,
		Page:            f.Offset/pageSize + 1,
		PageSize:        pageSize,
		CoveredFields:   covered,
		UncoveredFields: uncovered,
	}, nil
}

// blastRadiusView is the assembled report: the rows plus everything a caller
// needs to render the two caveats honestly. Held as one value so the JSON
// handler and the CSV export cannot disagree about what the report says.
type blastRadiusView struct {
	Rows            []artist.BlastRadiusRow
	Counts          artist.BlastRadiusCounts
	Page            int
	PageSize        int
	CoveredFields   []string
	UncoveredFields []string
}

// handleReportBlastRadius returns the report as JSON.
// GET /api/v1/reports/blast-radius
func (r *Router) handleReportBlastRadius(w http.ResponseWriter, req *http.Request) {
	if r.historyService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "history service is not available")
		return
	}

	data, err := r.loadBlastRadius(req)
	if err != nil {
		r.logger.Error("loading blast-radius report", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load blast-radius report")
		return
	}

	writeJSON(w, http.StatusOK, blastRadiusResponse{
		Rows:     data.Rows,
		Counts:   data.Counts,
		Page:     data.Page,
		PageSize: data.PageSize,
		Total:    data.Counts.Total,
		Attribution: blastRadiusAttributionInfo{
			CutoffDate: artist.AttributionCutoffDate,
			Automated:  data.Counts.Automated,
			Unknown:    data.Counts.Unknown,
		},
		Coverage: blastRadiusCoverageInfo{
			CoveredFields:   data.CoveredFields,
			UncoveredFields: data.UncoveredFields,
		},
	})
}

// collectBlastRadiusExportRows walks the whole filtered result set a page at a
// time, stopping at blastRadiusExportCap, and reports whether rows were left
// behind. listPage is the paging query, taken as a parameter so the cap
// arithmetic can be tested without seeding blastRadiusExportCap real rows.
//
// truncated is deliberately NOT "we reached the cap". blastRadiusExportPageSize
// divides blastRadiusExportCap exactly, so a result set of precisely
// blastRadiusExportCap rows would reach the cap on its final page with nothing
// behind it and claim a truncation that never happened. The flag may only be
// set when a page actually carried rows PAST the cap, which is why the surplus
// is detected before the slice rather than inferred from the total afterwards.
// This report's whole premise is not misleading the operator about what it
// captured, and an unearned "your export was cut short" misleads in the
// under-confident direction.
func collectBlastRadiusExportRows(
	ctx context.Context,
	f artist.BlastRadiusFilter,
	listPage func(context.Context, artist.BlastRadiusFilter) ([]artist.BlastRadiusRow, error),
) (rows []artist.BlastRadiusRow, truncated bool, err error) {
	for {
		page, err := listPage(ctx, f)
		if err != nil {
			return nil, false, err
		}
		if len(rows)+len(page) > blastRadiusExportCap {
			page = page[:blastRadiusExportCap-len(rows)]
			truncated = true
		}
		rows = append(rows, page...)
		if len(page) < f.Limit || truncated {
			return rows, truncated, nil
		}
		f.Offset += f.Limit
	}
}

// handleReportBlastRadiusExport streams the report as CSV.
// GET /api/v1/reports/blast-radius/export
//
// The attribution column carries the per-row unknown/automated label, and a
// trailing note row states the coverage limit. A spreadsheet detached from the
// web UI must carry the same caveats the web UI shows, or the CSV becomes the
// artifact that reads as a complete accounting when it is not.
func (r *Router) handleReportBlastRadiusExport(w http.ResponseWriter, req *http.Request) {
	if r.historyService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "history service is not available")
		return
	}
	ctx := req.Context()

	f := blastRadiusFilterFromRequest(req)
	f.Offset = 0
	f.Limit = blastRadiusExportPageSize

	all, truncated, err := collectBlastRadiusExportRows(ctx, f, r.historyService.ListBlastRadius)
	if err != nil {
		r.logger.Error("listing rows for blast-radius export", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load blast-radius report")
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="blast-radius-report.csv"`)
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"Artist", "Field", "Previous Value", "Current Value", "Change", "Source", "Attribution", "When",
	}); err != nil {
		r.logger.Error("writing blast-radius CSV header", "error", err)
		return
	}

	for i := range all {
		if ctx.Err() != nil {
			break
		}
		row := &all[i]
		if err := cw.Write([]string{
			sanitizeCSV(row.ArtistName),
			sanitizeCSV(row.Field),
			sanitizeCSV(row.OldValue),
			sanitizeCSV(row.NewValue),
			sanitizeCSV(row.Class),
			sanitizeCSV(row.Source),
			sanitizeCSV(row.Attribution),
			row.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"),
		}); err != nil {
			r.logger.Error("writing blast-radius CSV row", "change_id", row.ID, "error", err)
			return
		}
	}

	// Caveat rows. These travel with the file so a spreadsheet opened weeks
	// later still says what it does not cover.
	covered, uncovered := blastRadiusCoverage()
	notes := []string{
		"NOTE: rows marked attribution=unknown are recorded as manual changes. " +
			"Stillwater began recording scan-driven changes separately on " +
			artist.AttributionCutoffDate + "; before that a scan that changed a value " +
			"was recorded the same way an operator edit is, so these may be either.",
		"NOTE: this report covers these fields only: " + strings.Join(covered, ", ") + ".",
		"NOTE: these fields have no change history and cannot be reported here, " +
			"so their absence is not evidence they are undamaged: " + strings.Join(uncovered, ", ") + ".",
		"NOTE: change history is kept until an artist is deleted or merged into another. " +
			"Deleting or merging an artist removes its history, including rows this report would have shown.",
	}
	if truncated {
		notes = append(notes, "NOTE: this export was truncated at the row cap. "+
			"Narrow the filters and export again to see the rest.")
	}
	for _, note := range notes {
		if err := cw.Write([]string{note}); err != nil {
			r.logger.Error("writing blast-radius CSV note", "error", err)
			return
		}
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		r.logger.Error("flushing blast-radius CSV writer", "error", err)
	}
}
