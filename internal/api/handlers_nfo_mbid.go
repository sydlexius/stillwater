// Package api -- handlers_nfo_mbid.go
//
// The rule-written MusicBrainz ID report (issue #2809): "which artists did the
// automatic NFO rule fix assign a MusicBrainz ID to". Read-only. Both handlers
// here are GETs and nothing in this file writes to the database, contacts
// MusicBrainz, validates an ID, or reverts one. The operator gets the list; the
// judgment stays theirs.
//
// # THE HONESTY REQUIREMENTS
//
// This report exists because a rule adopted a best-guess match and some of
// those guesses were wrong. So the ways it can mislead matter more than the ways
// it can be slow, and every one of them is stated in the response rather than
// left for the reader to discover:
//
//  1. SCOPE. Only the rule-fixer path records a change. Other writers assign a
//     MusicBrainz ID and record nothing, so their artists can never appear here.
//     This is permanent, not a gap waiting to be closed.
//  2. FLOOR. The counts under-report by construction and must be read as "at
//     least this many".
//  3. RETENTION. Deleting or merging an artist away destroys its history, and
//     duplicate artists were a known consequence of these misidentifications.
//  4. NOT CONFIRMED. An artist absent from this list has not been vetted by
//     anyone; nothing records operator confirmation of an ID yet.
//
// Rendering any of those as a clean zero would repeat the "unknown rendered as
// clean" defect (#2692, #2686).
package api

import (
	"context"
	"encoding/csv"
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

// nfoMBIDExportCap bounds a CSV export, matching the blast-radius export's cap.
// Truncation is reported in a note row rather than applied silently.
const nfoMBIDExportCap = 10000

// nfoMBIDExportPageSize is the per-query page size used while walking the full
// result set for an export. Bounded so one export cannot pull an entire large
// library into memory in a single query.
const nfoMBIDExportPageSize = 200

// nfoMBIDFilterFromRequest builds the narrowing half of the query filter:
// everything except the page window. Both the JSON report and the CSV export
// call this, so the two cannot narrow or sort differently -- an export that
// quietly disagreed with the page it was downloaded from would be worse than no
// export.
//
// It calls Validate, so the returned filter is always safe to send to SQL on its
// own; that leaves Limit at Validate's default, which every caller here
// overwrites with its own page window.
func nfoMBIDFilterFromRequest(req *http.Request) artist.NFOMBIDFilter {
	q := req.URL.Query()
	f := artist.NFOMBIDFilter{
		ArtistID: q.Get("artist_id"),
		Sort:     q.Get("sort"),
		Order:    q.Get("order"),
	}
	f.Validate()
	return f
}

// nfoMBIDPagedFilterFromRequest adds the caller's page window on top of
// nfoMBIDFilterFromRequest, resolving page_size against the per-user preference.
// Only the paginated JSON report calls this; the export walks the whole result
// set with its own fixed page size.
func (r *Router) nfoMBIDPagedFilterFromRequest(req *http.Request) artist.NFOMBIDFilter {
	userID := middleware.UserIDFromContext(req.Context())
	pageSize := r.getUserPageSize(req.Context(), userID, intQuery(req, "page_size", 0))

	page := intQuery(req, "page", 1)
	if page < 1 {
		page = 1
	}

	f := nfoMBIDFilterFromRequest(req)
	f.Limit = pageSize
	f.Offset = (page - 1) * pageSize
	f.Validate()
	return f
}

// nfoMBIDCaveats is the machine-readable form of the four limits. Always
// present, never conditional on there being rows: an empty row list with no
// statement of what the report cannot see would read as a clean bill of health.
type nfoMBIDCaveats struct {
	// Scope names the one code path this report can see and says the others are
	// invisible. The most important field in the response.
	Scope string `json:"scope"`
	// Floor says the counts are a minimum.
	Floor string `json:"floor"`
	// Retention says how rows disappear.
	Retention string `json:"retention"`
	// NoPriorValue says this fix filled a blank rather than overwriting an ID.
	NoPriorValue string `json:"no_prior_value"`
	// NotConfirmed forbids reading an unlisted artist as operator-vetted.
	NotConfirmed string `json:"not_confirmed"`
	// MessageWording explains why the recorded note varies between rows.
	MessageWording string `json:"message_wording"`
}

// nfoMBIDCaveatBlock returns the caveat block. One constructor shared by the
// JSON response and the CSV notes so the two surfaces state identical limits.
func nfoMBIDCaveatBlock() nfoMBIDCaveats {
	return nfoMBIDCaveats{
		Scope:          artist.NFOMBIDCaveatScope,
		Floor:          artist.NFOMBIDCaveatFloor,
		Retention:      artist.NFOMBIDCaveatRetention,
		NoPriorValue:   artist.NFOMBIDCaveatNoPriorValue,
		NotConfirmed:   artist.NFOMBIDCaveatNotConfirmed,
		MessageWording: artist.NFOMBIDCaveatMessageWording,
	}
}

// nfoMBIDResponse is the JSON envelope for the report.
type nfoMBIDResponse struct {
	Rows     []artist.NFOMBIDWriteRow `json:"rows"`
	Counts   artist.NFOMBIDCounts     `json:"counts"`
	Page     int                      `json:"page"`
	PageSize int                      `json:"page_size"`
	// Total mirrors Counts.Total. Duplicated at the top level because every
	// other list endpoint in this API carries a "total", and a client that
	// reaches for the familiar key should not get a zero.
	Total int `json:"total"`
	// Caveats states what the report cannot see. Never omitted.
	Caveats nfoMBIDCaveats `json:"caveats"`
}

// handleReportNFOHasMBID returns the report as JSON.
// GET /api/v1/reports/nfo-has-mbid
func (r *Router) handleReportNFOHasMBID(w http.ResponseWriter, req *http.Request) {
	if r.historyService == nil {
		// 503 rather than an empty report: an unreachable history store must
		// never be mistaken for an absence of rule-written IDs.
		writeError(w, req, http.StatusServiceUnavailable, "history service is not available")
		return
	}
	ctx := req.Context()
	f := r.nfoMBIDPagedFilterFromRequest(req)

	rows, err := r.historyService.ListNFOMBIDWrites(ctx, f)
	if err != nil {
		r.logger.Error("listing rule-written MusicBrainz ID rows", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load rule-written MusicBrainz ID report")
		return
	}
	counts, err := r.historyService.CountNFOMBIDWrites(ctx, f)
	if err != nil {
		r.logger.Error("counting rule-written MusicBrainz ID rows", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load rule-written MusicBrainz ID report")
		return
	}

	writeJSON(w, http.StatusOK, nfoMBIDResponse{
		Rows:     rows,
		Counts:   counts,
		Page:     f.Offset/f.Limit + 1,
		PageSize: f.Limit,
		Total:    counts.Total,
		Caveats:  nfoMBIDCaveatBlock(),
	})
}

// collectNFOMBIDExportRows walks the whole filtered result set a page at a time,
// stopping at nfoMBIDExportCap, and reports whether rows were left behind.
// listPage is the paging query, taken as a parameter so the cap arithmetic can
// be tested without seeding nfoMBIDExportCap real rows.
//
// truncated is deliberately NOT "we reached the cap". nfoMBIDExportPageSize
// divides nfoMBIDExportCap exactly, so a result set of precisely nfoMBIDExportCap
// rows would reach the cap on its final page with nothing behind it and claim a
// truncation that never happened. The flag may only be set when a page actually
// carried rows PAST the cap, which is why the surplus is detected before the
// slice rather than inferred from the total afterwards.
func collectNFOMBIDExportRows(
	ctx context.Context,
	f artist.NFOMBIDFilter,
	listPage func(context.Context, artist.NFOMBIDFilter) ([]artist.NFOMBIDWriteRow, error),
) (rows []artist.NFOMBIDWriteRow, truncated bool, err error) {
	for {
		page, err := listPage(ctx, f)
		if err != nil {
			return nil, false, err
		}
		if len(rows)+len(page) > nfoMBIDExportCap {
			page = page[:nfoMBIDExportCap-len(rows)]
			truncated = true
		}
		rows = append(rows, page...)
		if len(page) < f.Limit || truncated {
			return rows, truncated, nil
		}
		f.Offset += f.Limit
	}
}

// nfoMBIDCurrentIDCell renders the current-MusicBrainz-ID cell. A missing row is
// spelled out rather than left blank, because a blank cell in a spreadsheet
// reads as "nothing to see" and this column is the one an operator acts on.
func nfoMBIDCurrentIDCell(row *artist.NFOMBIDWriteRow) string {
	if !row.HasCurrentMusicBrainzID {
		return "none recorded"
	}
	return row.CurrentMusicBrainzID
}

// handleReportNFOHasMBIDExport streams the report as CSV.
// GET /api/v1/reports/nfo-has-mbid/export
//
// Trailing note rows carry every caveat the JSON response carries. A
// spreadsheet outlives the page it came from, and a bare row list would become
// the artifact that reads as a complete accounting of machine-assigned IDs when
// it is nothing of the kind.
func (r *Router) handleReportNFOHasMBIDExport(w http.ResponseWriter, req *http.Request) {
	if r.historyService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "history service is not available")
		return
	}
	ctx := req.Context()

	f := nfoMBIDFilterFromRequest(req)
	f.Offset = 0
	f.Limit = nfoMBIDExportPageSize

	all, truncated, err := collectNFOMBIDExportRows(ctx, f, r.historyService.ListNFOMBIDWrites)
	if err != nil {
		r.logger.Error("listing rows for rule-written MusicBrainz ID export", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load rule-written MusicBrainz ID report")
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="nfo-has-mbid-report.csv"`)
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"Artist", "Current MusicBrainz ID", "Recorded Note", "Source", "When",
	}); err != nil {
		r.logger.Error("writing rule-written MusicBrainz ID CSV header", "error", err)
		return
	}

	for i := range all {
		if ctx.Err() != nil {
			break
		}
		row := &all[i]
		// Every cell goes through sanitizeCSV: the artist name and the recorded
		// note are both free text that reached the database from outside, and a
		// cell beginning with a spreadsheet formula trigger would otherwise
		// execute on open.
		if err := cw.Write([]string{
			sanitizeCSV(row.ArtistName),
			sanitizeCSV(nfoMBIDCurrentIDCell(row)),
			sanitizeCSV(row.Message),
			sanitizeCSV(row.Source),
			sanitizeCSV(row.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC")),
		}); err != nil {
			r.logger.Error("writing rule-written MusicBrainz ID CSV row", "change_id", row.ID, "error", err)
			return
		}
	}

	c := nfoMBIDCaveatBlock()
	notes := []string{
		"NOTE: " + c.Scope,
		"NOTE: " + c.Floor,
		"NOTE: " + c.Retention,
		"NOTE: " + c.NoPriorValue,
		"NOTE: " + c.NotConfirmed,
		"NOTE: " + c.MessageWording,
	}
	if truncated {
		notes = append(notes, "NOTE: this export was truncated at the row cap. "+
			"Narrow the filters and export again to see the rest.")
	}
	for _, note := range notes {
		if err := cw.Write([]string{sanitizeCSV(note)}); err != nil {
			r.logger.Error("writing rule-written MusicBrainz ID CSV note", "error", err)
			return
		}
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		r.logger.Error("flushing rule-written MusicBrainz ID CSV writer", "error", err)
	}
}
