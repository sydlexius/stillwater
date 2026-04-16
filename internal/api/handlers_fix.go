package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// FixAllProgress tracks the state of an async fix-all operation.
type FixAllProgress struct {
	mu        sync.RWMutex
	Status    string `json:"status"`
	Total     int    `json:"total"`
	Processed int    `json:"processed"`
	Fixed     int    `json:"fixed"`
	Skipped   int    `json:"skipped"`
	Failed    int    `json:"failed"`
}

// handleFixViolation applies the recommended fix for a single violation.
// On success, registers an undo entry in the in-memory UndoStore and returns
// its ID so the caller can present an Undo button to the user.
// POST /api/v1/notifications/{id}/fix
func (r *Router) handleFixViolation(w http.ResponseWriter, req *http.Request) {
	id, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	if r.pipeline == nil {
		r.logger.Error("fix-violation: pipeline not configured", "id", id)
		writeError(w, req, http.StatusServiceUnavailable, "rule pipeline not configured")
		return
	}

	// Capture pre-fix state so the fix can be reverted within the undo window.
	// Short-circuit when undo is disabled (undoStore is nil).
	var pfs *preFixState
	if r.undoStore != nil {
		pfs = r.capturePreFixState(req.Context(), id)
	}

	// Inject metadata language preferences so language-aware fixers
	// (e.g. name_language_pref) can promote localized aliases.
	fixCtx := r.injectMetadataLanguages(req.Context())
	fr, err := r.pipeline.FixViolation(fixCtx, id)
	if err != nil {
		r.logger.Error("fix violation failed", "id", id, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to apply fix")
		return
	}

	var status string
	switch {
	case fr.Fixed:
		status = "fixed"
	case fr.Dismissed:
		status = "dismissed"
	default:
		status = "failed"
	}

	resp := map[string]any{
		"status":  status,
		"message": fr.Message,
	}

	// Register an undo entry when the fix succeeded and we have pre-fix state.
	if fr.Fixed && r.undoStore != nil && pfs != nil {
		revert := pfs.revert
		// Directory renames: freeze the post-fix path NOW rather than lazily
		// at revert time, so a second rename during the undo window cannot
		// corrupt the revert target.
		if pfs.dirRenameOldPath != "" {
			current, aErr := r.artistService.GetByID(req.Context(), pfs.artistID)
			switch {
			case aErr != nil:
				r.logger.Warn("undo: could not freeze post-fix path for directory rename; undo unavailable",
					"artist_id", pfs.artistID, "error", aErr)
			case current.Path == pfs.dirRenameOldPath:
				r.logger.Debug("undo: directory path unchanged after fix; no revert needed",
					"artist_id", pfs.artistID)
			default:
				revert = rule.DirectoryRenameRevert(pfs.dirRenameOldPath, current.Path)
			}
		}
		if revert != nil {
			undoID := r.undoStore.Register(id, revert)
			resp["undo_id"] = undoID
			resp["undo_expires_in"] = int(rule.UndoWindowDuration.Seconds())
		}
	}

	// Fixing a violation changes the artist's health score.
	r.InvalidateHealthCache()

	// When called via HTMX (dashboard action cards), return HTML so
	// hx-swap="outerHTML" removes the card. When an undo entry was
	// registered, render a brief undo toast so the user can revert.
	if isHTMXRequest(req) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if fr.Fixed || fr.Dismissed {
			// When an undo toast is present, skip the HX-Trigger so the
			// queue does not reload and destroy the toast before the user
			// can click Undo. The toast's auto-dismiss script dispatches
			// the event after it expires or is dismissed.
			if undoID, ok := resp["undo_id"].(string); ok && undoID != "" {
				expiresIn, _ := resp["undo_expires_in"].(int)
				renderTempl(w, req, templates.UndoToast(undoID, expiresIn))
			} else {
				// No undo toast -- trigger queue refresh immediately.
				w.Header().Set("HX-Trigger", "dashboard:action-resolved")
				w.WriteHeader(http.StatusOK)
			}
		} else {
			// Fix did not resolve -- return 422 with message so the card
			// stays in place and HTMX does not swap it out.
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = fmt.Fprint(w, html.EscapeString(fr.Message)) //nolint:gosec // G705: fr.Message is rule-engine text, escaped for safety
		}
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// preFixState holds the pre-fix snapshot context needed to register an undo.
// For most rules, revert contains the full revert function. For directory
// renames, dirRenameOldPath is set instead and revert is nil; the caller
// MUST populate revert by freezing both old and new paths after the fix
// succeeds (see handleFixViolation). This avoids lazy path resolution that
// could be corrupted by a second rename during the undo window.
type preFixState struct {
	revert           rule.RevertFunc
	dirRenameOldPath string
	artistID         string
}

// capturePreFixState loads the violation and the associated artist, then
// builds a preFixState that can restore pre-fix state. Returns nil when no
// relevant files are found (e.g. for pure metadata fixes that do not touch
// the filesystem).
//
// Errors during snapshot capture are logged and silently ignored so that a
// snapshot failure does not block the fix itself.
func (r *Router) capturePreFixState(ctx context.Context, violationID string) *preFixState {
	if r.ruleService == nil || r.artistService == nil {
		return nil
	}

	rv, err := r.ruleService.GetViolationByID(ctx, violationID)
	if err != nil {
		r.logger.Debug("undo: could not load violation for pre-fix snapshot", "id", violationID, "error", err)
		return nil
	}

	a, err := r.artistService.GetByID(ctx, rv.ArtistID)
	if err != nil {
		r.logger.Debug("undo: could not load artist for pre-fix snapshot",
			"artist_id", rv.ArtistID, "error", err)
		return nil
	}
	if a.Path == "" {
		// Pathless artists have no on-disk files to snapshot.
		return nil
	}

	// Directory rename: capture the old path now. The post-fix path is
	// frozen in handleFixViolation after FixViolation succeeds, so that
	// a second rename during the undo window cannot corrupt the target.
	if rv.RuleID == rule.RuleDirectoryNameMismatch {
		return &preFixState{
			dirRenameOldPath: a.Path,
			artistID:         a.ID,
		}
	}

	snaps := captureFilesForRule(ctx, a, rv.RuleID, r)
	if len(snaps) == 0 {
		return nil
	}
	return &preFixState{revert: rule.MultiFileRevert(snaps)}
}

// captureFilesForRule resolves the file paths that a given rule's fixer is
// expected to modify and captures their current on-disk content.
func captureFilesForRule(ctx context.Context, a *artist.Artist, ruleID string, r *Router) []rule.FileSnapshot {
	switch ruleID {
	case rule.RuleNFOExists, rule.RuleNFOHasMBID, rule.RuleBioExists, rule.RuleMetadataQuality:
		// NFO fixes write to artist.nfo.
		snap, err := rule.CaptureFile(filepath.Join(a.Path, "artist.nfo"))
		if err != nil {
			r.logger.Debug("undo: could not snapshot artist.nfo", "artist", a.Name, "error", err)
			return nil
		}
		return []rule.FileSnapshot{snap}

	case rule.RuleThumbExists, rule.RuleThumbSquare, rule.RuleThumbMinRes:
		return captureImageFiles(ctx, a.Path, "thumb", r)

	case rule.RuleFanartExists, rule.RuleFanartMinRes, rule.RuleFanartAspect:
		return captureImageFiles(ctx, a.Path, "fanart", r)

	case rule.RuleLogoExists, rule.RuleLogoMinRes, rule.RuleLogoPadding:
		return captureImageFiles(ctx, a.Path, "logo", r)

	case rule.RuleBannerExists, rule.RuleBannerMinRes:
		return captureImageFiles(ctx, a.Path, "banner", r)

	default:
		// Other rules (extraneous images, etc.) either do not have a
		// straightforward single-file undo, or undo is not supported.
		// Directory rename is handled directly in capturePreFixState.
		return nil
	}
}

// captureImageFiles captures snapshots for all canonical filenames of imageType
// in dir. Files that do not exist are captured with Exists=false so that undo
// can remove them if they were created by the fix.
func captureImageFiles(ctx context.Context, dir, imageType string, r *Router) []rule.FileSnapshot {
	// Resolve canonical filenames, preferring the active platform profile.
	var names []string
	if r.platformService != nil {
		if profile, err := r.platformService.GetActive(ctx); err == nil && profile != nil {
			names = profile.ImageNaming.NamesForType(imageType)
		}
	}
	if len(names) == 0 {
		names = img.FileNamesForType(img.DefaultFileNames, imageType)
	}

	snaps := make([]rule.FileSnapshot, 0, len(names))
	for _, name := range names {
		snap, err := rule.CaptureFile(filepath.Join(dir, name))
		if err != nil {
			r.logger.Debug("undo: could not snapshot image file", "file", name, "error", err)
			continue
		}
		snaps = append(snaps, snap)
	}
	return snaps
}

// handleUndoFix reverts a recently applied fix using the registered undo entry.
// POST /api/v1/fix-undo/{undoId}
func (r *Router) handleUndoFix(w http.ResponseWriter, req *http.Request) {
	undoID, ok := RequirePathParam(w, req, "undoId")
	if !ok {
		return
	}

	if r.undoStore == nil {
		writeError(w, req, http.StatusServiceUnavailable, "undo not available")
		return
	}

	entry, ok := r.undoStore.Pop(undoID)
	if !ok {
		// Either the undo ID was never registered, was already used, or expired.
		writeError(w, req, http.StatusGone, "undo window expired or undo already applied")
		return
	}

	// Use a context that survives client disconnect but has a bounded
	// deadline. Pop() already consumed the single-use token, so if the
	// client disconnects mid-revert there is no retry path. WithoutCancel
	// prevents premature cancellation; the 30s timeout prevents hangs on
	// stuck filesystems or DB connections during server shutdown.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 30*time.Second)
	defer cancel()

	if err := entry.Revert(ctx); err != nil {
		r.logger.Error("undo fix failed", "violation_id", entry.ViolationID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to revert fix")
		return
	}

	// Reopen the violation so it appears as fixable again.
	var reopenFailed bool
	if r.ruleService != nil {
		if reopenErr := r.ruleService.ReopenViolation(ctx, entry.ViolationID); reopenErr != nil {
			r.logger.Error("undo: failed to reopen violation after successful revert",
				"id", entry.ViolationID, "error", reopenErr)
			reopenFailed = true
		}
	}

	r.logger.Info("fix reverted", "violation_id", entry.ViolationID)

	// Undoing a fix restores pre-fix state, changing health scores.
	r.InvalidateHealthCache()

	// When called via HTMX (the undo button in the toast), return an empty
	// HTML fragment so hx-swap="outerHTML" removes the toast from the DOM.
	// Also trigger a dashboard refresh so the reopened violation reappears.
	if isHTMXRequest(req) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("HX-Trigger", "dashboard:action-resolved")
		w.WriteHeader(http.StatusOK)
		// Empty response removes the toast element via outerHTML swap.
		return
	}

	if reopenFailed {
		// Files were restored but the violation status is inconsistent.
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "reverted",
			"message": "fix reverted but violation could not be reopened; run rules to refresh",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "reverted",
		"message": "fix reverted successfully",
	})
}

// handleFixAll starts an async bulk fix for all open fixable violations.
// Rejects concurrent starts with 409 Conflict.
// POST /api/v1/notifications/fix-all
func (r *Router) handleFixAll(w http.ResponseWriter, req *http.Request) {
	if r.pipeline == nil {
		r.logger.Error("fix-all: pipeline not configured")
		writeError(w, req, http.StatusServiceUnavailable, "rule pipeline not configured")
		return
	}

	// Parse optional ID filter before claiming the slot so a slow/stalled
	// client cannot hold the singleton open. Return 400 for malformed JSON
	// (but allow empty body / EOF to mean "fix all").
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	// Atomic check-and-set: reject if already running, otherwise claim the slot
	// immediately so concurrent requests cannot both pass the check.
	progress := &FixAllProgress{Status: "running"}
	r.fixAllMu.Lock()
	if r.fixAllProgress != nil {
		r.fixAllProgress.mu.RLock()
		running := r.fixAllProgress.Status == "running"
		r.fixAllProgress.mu.RUnlock()
		if running {
			r.fixAllMu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{
				"status":  "running",
				"message": "fix-all already in progress",
			})
			return
		}
	}
	r.fixAllProgress = progress
	r.fixAllMu.Unlock()

	// releaseProgress clears the slot if this request still owns it, allowing
	// a subsequent request to start a new fix-all.
	releaseProgress := func() {
		r.fixAllMu.Lock()
		if r.fixAllProgress == progress {
			r.fixAllProgress = nil
		}
		r.fixAllMu.Unlock()
	}

	violations, err := r.ruleService.ListViolationsFiltered(req.Context(), rule.ViolationListParams{
		Status: "active",
	})
	if err != nil {
		releaseProgress()
		r.logger.Error("listing violations for fix-all", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list violations")
		return
	}

	idSet := make(map[string]bool, len(body.IDs))
	for _, id := range body.IDs {
		idSet[id] = true
	}

	// Only include open fixable violations; skip pending_choice (requires
	// user candidate selection) and non-fixable violations.
	var fixable []rule.RuleViolation
	for _, v := range violations {
		if !v.Fixable || v.Status != rule.ViolationStatusOpen {
			continue
		}
		if len(idSet) > 0 && !idSet[v.ID] {
			continue
		}
		fixable = append(fixable, v)
	}

	if len(fixable) == 0 {
		releaseProgress()
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "completed",
			"message": "no fixable violations",
			"total":   0,
		})
		return
	}

	// Set the total now that we know the count.
	scoped := len(idSet) > 0
	progress.mu.Lock()
	progress.Total = len(fixable)
	progress.mu.Unlock()

	fixAllCtx := r.injectMetadataLanguages(req.Context())
	r.runFixAll(fixAllCtx, fixable, scoped, progress)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "running",
		"total":  len(fixable),
	})
}

// handleFixAllStatus returns the progress of the current fix-all operation.
// GET /api/v1/notifications/fix-all/status
func (r *Router) handleFixAllStatus(w http.ResponseWriter, _ *http.Request) {
	r.fixAllMu.RLock()
	progress := r.fixAllProgress
	r.fixAllMu.RUnlock()

	if progress == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "idle"})
		return
	}

	progress.mu.RLock()
	resp := map[string]any{
		"status":    progress.Status,
		"total":     progress.Total,
		"processed": progress.Processed,
		"fixed":     progress.Fixed,
		"skipped":   progress.Skipped,
		"failed":    progress.Failed,
	}
	progress.mu.RUnlock()

	writeJSON(w, http.StatusOK, resp)
}

// runFixAll begins fixing violations in a background goroutine.
// The caller must set r.fixAllProgress before calling this method.
//
// Optimizations over a naive per-violation loop:
//  1. Orphan cleanup: dismiss violations for deleted artists in one SQL query
//     (skipped when the request is scoped to specific IDs)
//  2. Artist grouping: check each artist once, skip orphaned groups
//  3. Rule caching: Pipeline caches rule lookups across the entire run
//  4. Yield: sleep between artist groups to release the SQLite write lock
func (r *Router) runFixAll(reqCtx context.Context, violations []rule.RuleViolation, scoped bool, progress *FixAllProgress) {
	go func() {
		// Detach from request lifecycle but preserve request-scoped values.
		ctx := context.WithoutCancel(reqCtx)

		// Phase 1: dismiss orphaned violations in bulk (one SQL query).
		// Skip when the request targets specific IDs to avoid side effects
		// on unrelated violations.
		if !scoped {
			dismissed, err := r.ruleService.DismissOrphanedViolations(ctx)
			if err != nil {
				r.logger.Warn("fix-all: orphan cleanup failed", "error", err)
			} else if dismissed > 0 {
				r.logger.Info("fix-all: dismissed orphaned violations", "count", dismissed)
			}
		}

		// Phase 2: group violations by artist.
		type artistGroup struct {
			artistID   string
			violations []rule.RuleViolation
		}
		groupOrder := []string{}
		byArtist := map[string]*artistGroup{}
		for _, rv := range violations {
			g, ok := byArtist[rv.ArtistID]
			if !ok {
				g = &artistGroup{artistID: rv.ArtistID}
				byArtist[rv.ArtistID] = g
				groupOrder = append(groupOrder, rv.ArtistID)
			}
			g.violations = append(g.violations, rv)
		}

		// Phase 3: process artist groups with caching and yield.
		for _, artistID := range groupOrder {
			g := byArtist[artistID]

			// Check artist existence once per group.
			_, aErr := r.artistService.GetByID(ctx, artistID)
			if aErr != nil && errors.Is(aErr, artist.ErrNotFound) {
				// Explicitly dismiss each violation for this deleted artist.
				for _, rv := range g.violations {
					if dErr := r.ruleService.DismissViolation(ctx, rv.ID); dErr != nil {
						r.logger.Warn("fix-all: failed to dismiss orphan violation", "id", rv.ID, "error", dErr)
					}
					progress.mu.Lock()
					progress.Processed++
					progress.Skipped++
					progress.mu.Unlock()
				}
				continue
			}

			// Fix each violation for this artist.
			for _, rv := range g.violations {
				fr, fixErr := r.pipeline.FixViolation(ctx, rv.ID)

				progress.mu.Lock()
				progress.Processed++
				if fixErr != nil {
					progress.Failed++
					r.logger.Warn("fix-all: violation fix failed", "id", rv.ID, "error", fixErr)
				} else if fr.Fixed {
					progress.Fixed++
				} else {
					progress.Skipped++
				}
				progress.mu.Unlock()
			}

			// Yield between artist groups to let HTTP handlers acquire the
			// SQLite write lock, keeping the UI responsive.
			time.Sleep(10 * time.Millisecond)
		}

		progress.mu.Lock()
		progress.Status = "completed"
		progress.mu.Unlock()

		// Invalidate the health cache after bulk fixes complete so the next
		// dashboard poll reflects the updated health scores.
		r.InvalidateHealthCache()
	}()
}
