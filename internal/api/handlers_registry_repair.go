// Package api -- handlers_registry_repair.go
//
// Operator entry point for image registry repair (#2669). The two repair
// passes themselves already shipped and are NOT implemented here:
//
//   - REBUILD: maintenance.Service.RepairImageRegistry (#2670) inserts
//     artist_images rows for files that are on disk but have no registry row.
//     Insert-only; it never modifies an existing row.
//   - RESTORE: maintenance.Service.RestoreExistsFlags (#2668) flips
//     exists_flag back to 1 for existing rows whose file is positively
//     confirmed present. Monotone 0 -> 1 only.
//
// This file composes them into one admin-gated request and one operator
// report.
//
// PASS ORDER. Rebuild runs first, then restore. The two passes operate on
// DISJOINT row sets and the result does not depend on the order: rebuild only
// INSERTS rows whose key is absent from the registry, and it inserts them with
// exists_flag already set to 1, while restore only reads rows that exist with
// exists_flag = 0. A row rebuild creates is therefore never a row restore can
// select, in either order. The order is fixed rather than significant -- the
// rollup's op_id and every artist-level counter come from the rebuild result,
// so running it first means those facts are in hand before the second pass,
// and "create the missing rows, then correct the flags" is the order the
// operation reads in.
//
// THE FIVE COUNTERS, AND WHY absent != unreadable. The report the issue asks
// for is scanned/rebuilt/restored/absent/unreadable, and the last two must
// never collapse into one bucket:
//
//   - absent    -- the directory is definitively gone (ENOENT). For an
//     additive repair that is a clean, expected no-op.
//   - unreadable -- the directory could not be examined at all (EACCES,
//     ESTALE, an unmounted share). We cannot tell what is on disk, so nothing
//     is touched and the operator is told.
//
// A third condition sits beside them: skipped, an artist with no resolvable
// image directory at all. No filesystem location was ever derived, so it is a
// registry data defect rather than a filesystem fact, and it belongs in
// neither of the two above. Together with scanned the four account for every
// artist in scope. Two further counters are reported in ROW units and are
// never summed with the artist ones: unverifiable_rows (rows the restore pass
// could not verify) and write_failures (writes attempted that did not land).
//
// Reporting "cannot tell" as "nothing there" is the exact mistake that caused
// the incident this whole feature repairs, so the endpoint keeps the two facts
// in separate counters and the nested per-pass results preserve each pass's own
// detail underneath.
//
// Route (admin-only via requireForeignAdmin; snake_case JSON):
//
//	POST {basePath}/api/v1/reports/registry-repair/remediate
//	     {commit?, artist_id?}
//
// COMMIT IS AFFIRMATIVE, not dry_run. A Go bool zero-values to false, so a
// `dry_run` field would mean a request that omitted it WRITES. `commit` means
// an empty body, a malformed field, or a dropped key PREVIEWS. This matches
// maintenance.ImageRepairOpts / maintenance.ExistsFlagRestoreOpts, which are
// shaped the same way for the same reason; introducing an inverted convention
// next to them is how one of the two eventually gets read the wrong way round.
//
// SINGLETON. This endpoint claims its OWN r.registryRepairRunning under a
// dedicated r.registryRepairMu, deliberately NOT the destructive-fanart slot
// (r.backdropRepairRunning / r.bulkActionMu) that the phash back-out and
// fanart-duplicate remediation share. Precedent: platformPruneMu is likewise
// separate. The reasoning is the same one -- that singleton exists because
// every pass under it tombs, renumbers, or deletes fanart BYTES on disk, so two
// of them would race the same directory. Registry repair writes no files at
// all: it only reads the filesystem and inserts/updates artist_images rows, so
// it shares no TOCTOU surface with them. It is still a singleton against
// ITSELF, because two concurrent runs would each scan and then write the same
// rows.
package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/maintenance"
)

// registryRepairRequest is the POST body for the repair endpoint. Both fields
// are optional: the zero value is a library-wide PREVIEW.
type registryRepairRequest struct {
	// Commit must be true for anything to be written. False (the default) runs
	// the identical discovery and verification in both passes and reports the
	// plan without issuing a single write.
	Commit bool `json:"commit"`
	// ArtistID scopes both passes to one artist; empty means library-wide.
	ArtistID string `json:"artist_id"`
}

// registryRepairReport is the operator-facing rollup. The five top-level
// counters are the report the issue specifies; the nested rebuild/restore
// objects preserve each pass's own detail (op id, per-slot outcomes, per-pass
// counters) so nothing already computed is discarded.
type registryRepairReport struct {
	// OpID is the rebuild pass's operation id, surfaced at the top level
	// because it is the only identifier a later investigation can correlate
	// this run's log lines by.
	OpID string `json:"op_id"`
	// Commit echoes the request's mode back so a report cannot be mistaken for
	// the other mode once it is detached from the request that produced it.
	Commit bool `json:"commit"`
	// DryRun is the negation of Commit, carried explicitly to match the shape
	// both nested results already use.
	DryRun bool `json:"dry_run"`
	// Scanned counts artists whose image directory the rebuild pass read
	// successfully. It deliberately does NOT include absent or unreadable
	// artists -- it answers "how much did we actually look at".
	Scanned int `json:"scanned"`
	// Rebuilt counts registry rows inserted for files found on disk. On a
	// preview it is 0 by construction; rebuild.rows_planned is the preview
	// count.
	Rebuilt int `json:"rebuilt"`
	// Restored counts exists_flag values flipped 0 -> 1. On a preview it is
	// what WOULD be flipped (the restore pass computes the same set either
	// way).
	Restored int `json:"restored"`
	// Absent counts artists whose image directory is definitively gone
	// (ENOENT). A clean, expected no-op -- never an error to investigate.
	Absent int `json:"absent"`
	// Skipped counts ARTISTS with no resolvable image directory at all: the
	// artists row has no path and there is no image-cache fallback, so no
	// filesystem location was ever derived to examine. This is a registry
	// data defect, not a filesystem condition -- it is neither Absent (we
	// observed no absence) nor Unreadable (we attempted no read), and folding
	// it into either would repeat the category error this endpoint exists to
	// prevent. Reported so that
	// scanned + absent + unreadable + skipped accounts for every artist in
	// scope; before it existed such an artist was invisible in every counter.
	Skipped int `json:"skipped"`
	// Unreadable counts ARTISTS whose image directory the rebuild pass could
	// not examine at all -- EACCES, ESTALE, an unmounted share, or a registry
	// read failure. We cannot tell what is on disk for these artists, so
	// nothing was touched. Held apart from Absent because "cannot tell" and
	// "not there" are different facts and only one of them is safe to act on.
	// This is an ARTIST count. The corresponding ROW count is
	// UnverifiableRows; the two are never summed.
	Unreadable int `json:"unreadable"`
	// UnverifiableRows counts individual artist_images rows the restore pass
	// could not verify either way (unresolvable directory, permission denied,
	// I/O error on the probe). It is a ROW count, deliberately held apart from
	// Unreadable, which is an ARTIST count: one unreadable directory can
	// account for many unverifiable rows, and summing the two produces a
	// number in no unit at all.
	UnverifiableRows int `json:"unverifiable_rows"`
	// WriteFailures counts writes this run attempted and could not complete:
	// restore UPDATE statements that errored (leaving a confirmed-present slot still
	// flagged missing) plus rebuild inserts that were planned but did not land.
	// Always 0 on a preview, by construction -- no write is attempted. Non-zero
	// means the repair is INCOMPLETE and should be re-run; the repair is
	// idempotent, so re-running is safe. Surfaced at the top level because a
	// half-completed repair reported as a clean 200 is what stops an operator
	// from re-running it.
	WriteFailures int `json:"write_failures"`
	// Rebuild is the rebuild pass's full result, including per-slot outcomes.
	Rebuild *maintenance.ImageRepairResult `json:"rebuild"`
	// Restore is the restore pass's full result.
	Restore *maintenance.ExistsFlagRestoreResult `json:"restore"`
}

// claimRegistryRepairSlot atomically claims the registry-repair singleton or
// writes a 409 and returns ok=false. On success it returns a release func the
// caller MUST defer. See this file's package comment for why the flag is
// dedicated rather than shared with the destructive-fanart singleton.
func (r *Router) claimRegistryRepairSlot(w http.ResponseWriter) (release func(), ok bool) {
	r.registryRepairMu.Lock()
	if r.registryRepairRunning {
		r.registryRepairMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"status":  "running",
			"message": "an image registry repair is already in progress",
		})
		return nil, false
	}
	r.registryRepairRunning = true
	r.registryRepairMu.Unlock()
	return func() {
		r.registryRepairMu.Lock()
		r.registryRepairRunning = false
		r.registryRepairMu.Unlock()
	}, true
}

// handleRegistryRepairRemediate runs the rebuild and restore passes and
// reports the combined result. POST
// {basePath}/api/v1/reports/registry-repair/remediate. Admin-gated; singleton;
// previews unless commit is true.
//
// Synchronous by design, matching the phash/backdrop remediation precedent:
// the singleton flag, not a progress feed, is what keeps two runs apart. Both
// passes are bounded per-artist-directory work.
func (r *Router) handleRegistryRepairRemediate(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	if r.maintenanceService == nil {
		// Fail loud: the production router always wires this. A miss is a
		// wiring bug, never a silent no-op that reports a clean zero-count
		// repair -- this repo forbids silent-failure capability guards, and a
		// repair endpoint reporting success while doing nothing is precisely
		// the failure class the feature exists to eliminate.
		r.logger.Error("maintenance service not wired; registry repair unavailable")
		http.Error(w, "registry repair unavailable", http.StatusInternalServerError)
		return
	}

	var body registryRepairRequest
	// decodePHashBody is the package's strict single-object JSON decoder
	// (DisallowUnknownFields, trailing-token rejection, 1 MiB cap, empty body
	// allowed so the zero value applies). Reused rather than duplicated; it is
	// not phash-specific beyond its name.
	if !decodePHashBody(w, req, &body) {
		return
	}

	release, ok := r.claimRegistryRepairSlot(w)
	if !ok {
		return
	}
	defer release()

	ctx := req.Context()

	// Pass 1: REBUILD. Insert rows for files on disk that the registry has
	// forgotten. Runs first for the reasons in the package comment; the two
	// passes are disjoint, so the result does not depend on it.
	rebuild, err := r.maintenanceService.RepairImageRegistry(ctx, maintenance.ImageRepairOpts{
		Commit:   body.Commit,
		ArtistID: body.ArtistID,
	})
	if err != nil {
		if errors.Is(err, maintenance.ErrLibraryUnreachable) {
			// The mount-down guard fired: not one artist directory was
			// readable across the whole library. Nothing was written. This is
			// an environment fault the operator can fix, not a server bug, so
			// it gets its own status and a message that says what to check.
			r.logger.Error("registry repair: library not visible", slog.String("error", err.Error()))
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"status":  "library_unreachable",
				"message": "no artist directory was readable across the whole library; check that the media mount is up. Nothing was changed.",
			})
			return
		}
		r.logger.Error("registry repair: rebuild pass failed", slog.String("error", err.Error()))
		http.Error(w, "registry repair failed", http.StatusInternalServerError)
		return
	}

	// Pass 2: RESTORE. Flip exists_flag back for rows that already existed with
	// the flag cleared and whose file is confirmed present. Not the rows pass 1
	// inserted -- those are inserted already flagged present.
	restore, err := r.maintenanceService.RestoreExistsFlags(ctx, maintenance.ExistsFlagRestoreOpts{
		Commit:   body.Commit,
		ArtistID: body.ArtistID,
	})
	if err != nil {
		// The rebuild pass may already have written rows. Say the repair
		// failed rather than reporting a partial result as a success: a
		// half-run repair reported as complete is what stops an operator from
		// re-running it. The repair is idempotent, so a re-run is safe.
		r.logger.Error("registry repair: restore pass failed", slog.String("error", err.Error()))
		http.Error(w, "registry repair failed", http.StatusInternalServerError)
		return
	}

	failures := writeFailures(body.Commit, rebuild, restore)
	if failures > 0 {
		// Error, not Warn: the run completed but the repair did NOT. The
		// response is still 200 (the report body is the actionable artifact
		// and a non-2xx invites clients and proxies to discard it), so this
		// log line is what keeps the partial failure from being silent.
		r.logger.Error("registry repair completed with write failures; the repair is INCOMPLETE and should be re-run",
			slog.String("op_id", rebuild.OpID),
			slog.Int("write_failures", failures))
	}

	writeJSON(w, http.StatusOK, registryRepairReport{
		OpID:     rebuild.OpID,
		Commit:   body.Commit,
		DryRun:   !body.Commit,
		Scanned:  rebuild.ArtistsScanned,
		Rebuilt:  rebuild.RowsInserted,
		Restored: restore.Restored,
		Absent:   rebuild.ArtistsAbsent,
		// Every artist the rebuild pass saw lands in exactly one of scanned,
		// absent, unreadable and skipped. Before skipped was surfaced, an
		// artist with no resolvable image directory reached no counter at all
		// and was invisible in the rollup.
		Skipped:    rebuild.ArtistsSkipped,
		Unreadable: rebuild.ArtistsFailed,
		// A ROW count, kept out of Unreadable (an ARTIST count) on purpose:
		// one unreadable directory can account for many unverifiable rows, so
		// summing the two produces a number in no unit at all and inflates a
		// single bad folder into several faults.
		UnverifiableRows: restore.Skipped,
		WriteFailures:    failures,
		Rebuild:          rebuild,
		Restore:          restore,
	})
}

// writeFailures totals the writes this run attempted and could not complete.
// On a preview it is 0 by construction: neither pass issues a write, and
// rebuild's RowsPlanned on a preview is the PLAN, not a shortfall, so
// subtracting RowsInserted from it there would report the whole plan as
// failures.
//
// Both terms are ROW counts, so unlike the artists-plus-rows sum this report
// used to publish as unreadable, summing them is legitimate.
func writeFailures(commit bool, rebuild *maintenance.ImageRepairResult, restore *maintenance.ExistsFlagRestoreResult) int {
	if !commit {
		return 0
	}
	// Clamped: a concurrent writer inserting a planned row between the plan
	// and the post-write re-read would otherwise make the shortfall negative
	// and silently offset a genuine restore failure.
	shortfall := rebuild.RowsPlanned - rebuild.RowsInserted
	if shortfall < 0 {
		shortfall = 0
	}
	return shortfall + restore.Failed
}
