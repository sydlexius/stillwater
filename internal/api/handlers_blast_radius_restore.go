// Package api -- handlers_blast_radius_restore.go
//
// The recovery half of the blast-radius report (issue #2750). The report
// (handlers_blast_radius.go) is read-only and says what an automated writer
// destroyed. This file puts a listed value back.
//
// One endpoint handles both the single-row and the bulk case, because there is
// no meaningful difference between them: a single restore is a bulk restore of
// one row, and giving them separate code paths is how the two eventually
// diverge on exactly the checks that matter.
//
//	POST {basePath}/api/v1/reports/blast-radius/restore
//	     {"change_ids": ["..."], "commit": true}
//
// # COMMIT IS AFFIRMATIVE, NOT dry_run
//
// A Go bool zero-values to false, so a `dry_run` field would mean a request
// that omitted it WRITES. `commit` means an empty body, a malformed field, or
// a key dropped by a proxy PREVIEWS. This matches the registry-repair endpoint
// (handlers_registry_repair.go) and maintenance.ImageRepairOpts, which are
// shaped the same way for the same reason.
//
// A preview runs the IDENTICAL eligibility pass the commit runs, then stops
// before the write. It shares one function with the commit path
// (planBlastRestore) so the plan an operator approves cannot be computed
// differently from the plan that executes.
//
// # ELIGIBILITY IS A POSITIVE ALLOW-LIST
//
// A row is restorable only when every one of these is affirmatively true:
//
//  1. The change id resolves to a real metadata_changes row. Nothing is
//     matched fuzzily -- an id that does not resolve exactly is refused, never
//     guessed at.
//  2. validateRevertable passes: the field is history-tracked, and the row is
//     not itself a revert. Both checks are the single-revert endpoint's, reused
//     rather than reimplemented.
//  3. The row is STILL the row the blast-radius report would list for its
//     (artist, field) pair -- verified by re-running the report's own query,
//     narrowed to that artist and field, and requiring the top row's id to
//     equal the requested id.
//
// Check 3 is the load-bearing one and it is deliberately a positive match
// rather than a "not known to be stale" test. The report ranks one row per
// (artist_id, field) and keeps it only if it is still damage, so a pair that
// has since been recovered, overwritten again, or edited by the operator no
// longer yields that id at the top. Requiring the id to come BACK from the
// report means a stale browser tab, a replayed request, or a bulk selection
// assembled ten minutes ago cannot write an old value over a newer one. It
// also makes duplicate ids for one (artist, field) impossible to smuggle in:
// the report yields at most one row per pair, so at most one can match.
//
// Inverting this into a deny-list ("refuse if we can prove it is stale") would
// mean every unanticipated state defaults to WRITING. On a recovery path that
// is the wrong default direction.
//
// # A RESTORE DOES NOT RE-TRIGGER THE WRITER THAT CAUSED THE DAMAGE
//
// This is the correctness core, and it is inherited from the single-revert
// endpoint rather than invented here.
//
// The ordinary field-edit path (handleFieldUpdate) does three things after
// mutating: it calls publisher.PublishMetadata (NFO write-back plus a platform
// push), and it publishes event.ArtistUpdated, which the rule pipeline's dirty
// and health subscribers consume to re-evaluate and potentially re-fix the
// artist. That re-evaluation is the automated writer. A restore that emitted
// ArtistUpdated would hand the recovered field straight back to the rule or
// provider refresh that blanked it, and the operator would watch their value
// disappear a second time.
//
// So this handler does what handleRevertHistory does: it calls performRevert
// and NOTHING else that could cascade. No PublishMetadata, no ArtistUpdated.
// The only event emitted is activity.recent, which is a UI notification for
// the dashboard rail and has no subscriber that writes anything.
//
// The write itself still records history (performRevert injects source
// "revert"), so the restore is auditable and shows up in the artist's history
// exactly like a single undo does. That same "revert" source is what makes the
// restored pair drop out of the blast-radius report: the report's ranking CTE
// sees the newer revert row as the latest change for that (artist, field), and
// its outer select excludes source='revert' from damage.
//
// Locked by TestBlastRestore_DoesNotRepublishOrRetriggerWriter, which wires a
// real event bus, asserts the restore emits no ArtistUpdated, and asserts as a
// precondition that an ordinary field update through the same bus DOES.
//
// # SINGLETON
//
// Two concurrent restores could target the same (artist, field) pair from two
// different stale selections and race each other's write. The endpoint claims
// its own r.blastRestoreRunning under a dedicated r.blastRestoreMu and returns
// 409 to the loser, matching the fix-all / bulk-action / registry-repair
// precedent. The mutex is dedicated rather than shared with the fanart
// singletons for the same reason registryRepairMu is: this path touches no
// image bytes on disk, so it shares no TOCTOU surface with them.
package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
)

// blastRestoreMaxIDs bounds one request. It matches BlastRadiusFilter's own
// maximum page size, so an operator can always restore everything on one page
// of the report in one call and cannot submit a selection larger than any page
// the report ever produced. Each id costs one report query plus one write, and
// the whole request is synchronous under the singleton, so an unbounded list
// would hold the slot for an unbounded time.
const blastRestoreMaxIDs = 500

// Restore item statuses. Every planned item carries exactly one.
const (
	// blastRestorePlanned is a preview item that WOULD be written.
	blastRestorePlanned = "planned"
	// blastRestoreRestored is a committed item whose write landed.
	blastRestoreRestored = "restored"
	// blastRestoreUnchanged is an eligible item whose field already holds the
	// value being restored, so no write was needed. Held apart from "restored"
	// so a count of restores never includes rows nothing happened to.
	blastRestoreUnchanged = "unchanged"
	// blastRestoreRefused is an item that failed the allow-list. Reason says
	// which check.
	blastRestoreRefused = "refused"
)

// Refusal reasons. These are stable machine-readable tokens; the UI turns them
// into prose.
const (
	// blastRefuseNotFound: the change id resolves to no metadata_changes row.
	blastRefuseNotFound = "change_not_found"
	// blastRefuseNotRevertible: the field is not tracked by the history
	// system, so there is no recorded old value to trust.
	blastRefuseNotRevertible = "not_revertible"
	// blastRefuseRevertOfRevert: the row is itself a revert. Restoring one
	// would chain undo onto undo.
	blastRefuseRevertOfRevert = "revert_of_revert"
	// blastRefuseNotCurrent: the row is no longer what the blast-radius report
	// lists for its (artist, field) pair -- it was superseded, already
	// recovered, or edited since. Writing it would overwrite a newer value
	// with an older one.
	blastRefuseNotCurrent = "no_longer_current"
	// blastRefuseWriteFailed: the row was eligible and the write was attempted
	// but errored. The item is reported so a partial bulk restore cannot look
	// like a clean one.
	blastRefuseWriteFailed = "restore_failed"
)

// blastRestoreRequest is the POST body. The zero value is a preview of nothing.
type blastRestoreRequest struct {
	// ChangeIDs are metadata_changes ids taken from the blast-radius report.
	// One id is the single-row case; many is the bulk case.
	ChangeIDs []string `json:"change_ids"`
	// Commit must be true for anything to be written. False (the default)
	// runs the identical eligibility pass and reports the plan.
	Commit bool `json:"commit"`
}

// blastRestoreItem is the per-row outcome. It carries both values so an
// operator reviewing a preview sees exactly what would replace what, rather
// than a count they have to trust.
type blastRestoreItem struct {
	// ChangeID is the requested metadata_changes id, echoed back so a client
	// can correlate items to its selection even when a row was refused before
	// anything else about it could be resolved.
	ChangeID string `json:"change_id"`
	// ArtistID and ArtistName identify the target. Empty when the id did not
	// resolve.
	ArtistID   string `json:"artist_id"`
	ArtistName string `json:"artist_name"`
	// Field is the metadata field to be written.
	Field string `json:"field"`
	// CurrentValue is what the field holds right now: the value the automated
	// writer left behind, read live rather than taken from the change row, so
	// a preview reflects the database as it is at preview time.
	CurrentValue string `json:"current_value"`
	// RestoreValue is the operator's value that would be put back. An empty
	// string is a legitimate restore target only in the sense that
	// validateRevertable already rejects it as damage -- the report never
	// lists a row whose old_value is empty.
	RestoreValue string `json:"restore_value"`
	// Status is one of the blastRestore* status constants.
	Status string `json:"restore_status"`
	// Reason is set only when Status is "refused".
	Reason string `json:"refuse_reason,omitempty"`
	// RestoreChangeID is the id of the metadata_changes row this restore
	// wrote, so the restore itself can be looked up in history. Set only on a
	// committed write that landed.
	RestoreChangeID string `json:"restore_change_id,omitempty"`
}

// blastRestoreResponse is the operator-facing rollup.
type blastRestoreResponse struct {
	// Commit echoes the request's mode back so a report cannot be mistaken for
	// the other mode once detached from the request that produced it.
	Commit bool `json:"commit"`
	// DryRun is the negation of Commit, carried explicitly to match the shape
	// the registry-repair report already uses.
	DryRun bool `json:"dry_run"`
	// Requested is how many ids the caller sent, after de-duplication.
	Requested int `json:"requested"`
	// Eligible counts ids that passed every allow-list check. On a preview it
	// is the size of the plan.
	Eligible int `json:"eligible"`
	// Restored counts writes that landed. Always 0 on a preview, by
	// construction: the preview path never reaches the write.
	Restored int `json:"restored"`
	// Unchanged counts eligible items whose field already held the restore
	// value, so nothing was written.
	Unchanged int `json:"unchanged"`
	// Refused counts items that failed the allow-list or whose write errored.
	// Non-zero after a commit means the restore is INCOMPLETE; the per-item
	// reason says why, and the operation is safe to re-run.
	Refused int `json:"refused"`
	// Items carries every requested id in request order.
	Items []blastRestoreItem `json:"items"`
}

// claimBlastRestoreSlot atomically claims the restore singleton or writes a 409
// and returns ok=false. On success it returns a release func the caller MUST
// defer. Mirrors claimRegistryRepairSlot.
func (r *Router) claimBlastRestoreSlot(w http.ResponseWriter) (release func(), ok bool) {
	r.blastRestoreMu.Lock()
	if r.blastRestoreRunning {
		r.blastRestoreMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"status":  "running",
			"message": "a blast-radius restore is already in progress",
		})
		return nil, false
	}
	r.blastRestoreRunning = true
	r.blastRestoreMu.Unlock()
	return func() {
		r.blastRestoreMu.Lock()
		r.blastRestoreRunning = false
		r.blastRestoreMu.Unlock()
	}, true
}

// dedupeChangeIDs removes blank and repeated ids while preserving request
// order. Repeats are dropped rather than refused: a client that sent the same
// row twice meant it once, and processing it twice would make the second pass
// see a state the first pass created.
func dedupeChangeIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// isCurrentBlastRow reports whether change is STILL the row the blast-radius
// report would list for its (artist, field) pair.
//
// It re-runs the report's own query narrowed to that one pair and requires the
// top row's id to equal the change's id. Reusing ListBlastRadius rather than
// re-deriving the predicate is the point: the definition of "currently
// destroyed" lives in exactly one place (internal/artist/sqlite_history.go),
// so the restore path cannot come to disagree with the report about which rows
// are restorable.
//
// A query error returns false, not an error the caller might treat as "assume
// current". On a recovery path an unreadable eligibility check must refuse.
// The error is returned alongside so the caller can log it.
func (r *Router) isCurrentBlastRow(ctx context.Context, change *artist.MetadataChange) (bool, error) {
	f := artist.BlastRadiusFilter{
		ArtistID: change.ArtistID,
		Field:    change.Field,
		// Class and Attribution left empty so Validate coerces both to
		// BlastScopeAll: eligibility must not depend on which slice of the
		// report the operator happened to be looking at.
		Limit: 1,
	}
	f.Validate()

	rows, err := r.historyService.ListBlastRadius(ctx, f)
	if err != nil {
		return false, err
	}
	// The query returns at most one row per (artist_id, field), and the filter
	// pinned both, so a match means this exact change is the current damage.
	return len(rows) == 1 && rows[0].ID == change.ID, nil
}

// planBlastRestore resolves every requested id into an item with a decided
// status, WITHOUT writing anything. Both the preview and the commit path call
// it, so the plan an operator approves is computed by the same code that later
// executes it.
//
// Eligible items come back with status "planned"; the commit path then
// upgrades each to restored/unchanged/refused as it writes. Items that fail
// the allow-list come back already refused and the commit path skips them.
func (r *Router) planBlastRestore(ctx context.Context, ids []string) []blastRestoreItem {
	items := make([]blastRestoreItem, 0, len(ids))
	for _, id := range ids {
		items = append(items, r.planOneBlastRestore(ctx, id))
	}
	return items
}

// planOneBlastRestore runs the allow-list for a single change id. Every exit
// except the final one is a refusal: the function can only reach "planned" by
// passing all three checks in order.
func (r *Router) planOneBlastRestore(ctx context.Context, id string) blastRestoreItem {
	item := blastRestoreItem{ChangeID: id, Status: blastRestoreRefused}

	// Check 1: the id resolves exactly. No fuzzy matching, no nearest row.
	change, err := r.historyService.GetByID(ctx, id)
	if err != nil {
		if !errors.Is(err, artist.ErrChangeNotFound) {
			r.logger.Error("blast restore: fetching change", "change_id", id, "error", err)
		}
		item.Reason = blastRefuseNotFound
		return item
	}
	item.ArtistID = change.ArtistID
	item.Field = change.Field
	item.RestoreValue = change.OldValue

	// Check 2: the single-revert endpoint's own eligibility rules, reused.
	if verr := validateRevertable(change); verr != nil {
		switch {
		case errors.Is(verr, errRevertOfRevert):
			item.Reason = blastRefuseRevertOfRevert
		default:
			item.Reason = blastRefuseNotRevertible
		}
		return item
	}

	// Check 3: it is still what the report lists for this (artist, field).
	current, err := r.isCurrentBlastRow(ctx, change)
	if err != nil {
		r.logger.Error("blast restore: checking row currency",
			"change_id", id, "artist_id", change.ArtistID, "field", change.Field, "error", err)
		item.Reason = blastRefuseNotCurrent
		return item
	}
	if !current {
		item.Reason = blastRefuseNotCurrent
		return item
	}

	// Read the live artist so the preview shows what is actually in the
	// database now, not what the change row recorded at the time. A missing
	// artist is a refusal, not a write against an id that no longer exists.
	a, err := r.artistService.GetByID(ctx, change.ArtistID)
	if err != nil {
		if !errors.Is(err, artist.ErrNotFound) {
			r.logger.Error("blast restore: fetching artist",
				"change_id", id, "artist_id", change.ArtistID, "error", err)
		}
		item.Reason = blastRefuseNotCurrent
		return item
	}
	item.ArtistName = a.Name
	item.CurrentValue = artist.FieldValueFromArtist(a, change.Field)

	item.Status = blastRestorePlanned
	item.Reason = ""
	return item
}

// commitBlastRestore writes every planned item and updates its status in
// place. Items that arrived refused are left untouched.
//
// Each item gets its OWN performRevert call, and therefore its own
// pre-assigned history id: artist.ContextWithHistoryID must only ever cover a
// code path that writes at most one history row, or the second insert collides
// on the primary key. Threading one context through the whole batch would do
// exactly that.
//
// A write failure demotes that one item to refused and the loop continues. A
// bulk restore that abandoned the remaining rows on the first error would
// leave the operator with a partial recovery and no record of which rows were
// never attempted.
func (r *Router) commitBlastRestore(ctx context.Context, items []blastRestoreItem) {
	for i := range items {
		it := &items[i]
		if it.Status != blastRestorePlanned {
			continue
		}

		// Re-fetch rather than carrying the planning copy: performRevert needs
		// the change row, and re-reading it keeps the write bound to the row
		// as it exists at write time.
		change, err := r.historyService.GetByID(ctx, it.ChangeID)
		if err != nil {
			r.logger.Error("blast restore: re-fetching change before write",
				"change_id", it.ChangeID, "error", err)
			it.Status = blastRestoreRefused
			it.Reason = blastRefuseNotFound
			continue
		}

		restoreChangeID, changed, err := r.performRevert(ctx, change)
		if err != nil {
			r.logger.Error("blast restore: writing restore",
				"change_id", it.ChangeID, "artist_id", change.ArtistID,
				"field", change.Field, "error", err)
			it.Status = blastRestoreRefused
			it.Reason = blastRefuseWriteFailed
			continue
		}

		if !changed {
			// The field already equalled the value being restored, so
			// UpdateField/ClearField skipped the write and recorded no
			// history. Reported as its own status rather than folded into
			// "restored", which would inflate the count with rows nothing
			// happened to.
			it.Status = blastRestoreUnchanged
			continue
		}

		it.Status = blastRestoreRestored
		it.RestoreChangeID = restoreChangeID
		it.CurrentValue = it.RestoreValue

		// activity.recent only. Deliberately NOT event.ArtistUpdated and NOT
		// publisher.PublishMetadata: both would hand the recovered field back
		// to the rule pipeline / platform push that destroyed it. See this
		// file's package comment.
		r.publishActivityRecent("reverted", change.Field+" restored", change.ArtistID)
	}
}

// summarizeBlastRestore tallies the per-item statuses into the response
// counters. Kept separate from the loops that set the statuses so the counts
// are derived from the items the caller receives, never accumulated alongside
// them where the two could disagree.
func summarizeBlastRestore(items []blastRestoreItem) (eligible, restored, unchanged, refused int) {
	for i := range items {
		switch items[i].Status {
		case blastRestorePlanned:
			eligible++
		case blastRestoreRestored:
			eligible++
			restored++
		case blastRestoreUnchanged:
			eligible++
			unchanged++
		case blastRestoreRefused:
			refused++
		}
	}
	return eligible, restored, unchanged, refused
}

// handleBlastRadiusRestore restores operator values that an automated writer
// destroyed. POST {basePath}/api/v1/reports/blast-radius/restore.
//
// Admin-gated, singleton, and a PREVIEW unless the body sets commit:true.
// Synchronous by design, matching the registry-repair and phash precedents:
// the singleton flag, not a progress feed, is what keeps two runs apart, and
// the request is bounded at blastRestoreMaxIDs rows.
func (r *Router) handleBlastRadiusRestore(w http.ResponseWriter, req *http.Request) {
	// Restoring writes to artist metadata, so it is admin-gated even though
	// the read-only report it works from is not.
	if !r.requireForeignAdmin(w, req) {
		return
	}
	if r.historyService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "history service is not available")
		return
	}

	// Parse and validate BEFORE claiming the singleton so a malformed request
	// cannot hold the slot, matching handleFixAll.
	var body blastRestoreRequest
	// decodePHashBody is the package's strict single-object JSON decoder
	// (DisallowUnknownFields, trailing-token rejection, 1 MiB cap). Reused
	// rather than duplicated; it is not phash-specific beyond its name.
	if !decodePHashBody(w, req, &body) {
		return
	}

	ids := dedupeChangeIDs(body.ChangeIDs)
	if len(ids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "change_ids must contain at least one id",
		})
		return
	}
	if len(ids) > blastRestoreMaxIDs {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "too many change_ids in one request",
		})
		return
	}

	release, ok := r.claimBlastRestoreSlot(w)
	if !ok {
		return
	}
	defer release()

	ctx := req.Context()
	items := r.planBlastRestore(ctx, ids)
	if body.Commit {
		r.commitBlastRestore(ctx, items)
	}

	eligible, restored, unchanged, refused := summarizeBlastRestore(items)
	writeJSON(w, http.StatusOK, blastRestoreResponse{
		Commit:    body.Commit,
		DryRun:    !body.Commit,
		Requested: len(ids),
		Eligible:  eligible,
		Restored:  restored,
		Unchanged: unchanged,
		Refused:   refused,
		Items:     items,
	})
}
