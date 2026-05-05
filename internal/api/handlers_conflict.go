package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/web/templates"
)

// Sentinel errors classify failures from applyStillwaterManaged /
// clearStillwaterManaged so the HTTP handler can map them to the right
// status code. ErrConflictPeerRejected => 502 (peer-side: snapshot read,
// disable, restore). ErrConflictLocalPersist => 500 (Stillwater-side:
// SetPreStillwaterConfig, SetManageServerFiles). Local persistence
// failures returned a 502 in the original implementation, which sent
// callers toward the wrong remediation path.
var (
	ErrConflictPeerRejected = errors.New("peer rejected stillwater-managed change")
	ErrConflictLocalPersist = errors.New("persisting stillwater-managed state failed")
)

// parseBoolStrict accepts the truthy / falsy forms HTMX and curl users are
// likely to send and signals via the second return whether the input was
// recognized at all. The strict variant lets the handler distinguish a
// missing/garbled value from an explicit false: a "missing" signal must
// produce a 400, otherwise an empty body or a typo silently flips the toggle
// off and triggers a destructive state change instead of a clean rejection
// at the API boundary.
func parseBoolStrict(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "on", "yes", "y", "t":
		return true, true
	case "0", "false", "off", "no", "n", "f":
		return false, true
	}
	return false, false
}

// handleGetConflicts returns the current conflict ledger as JSON. Consumed by
// the UI banner (for the "Recheck" button), external monitoring, and tests.
//
// GET /api/v1/conflicts
func (r *Router) handleGetConflicts(w http.ResponseWriter, req *http.Request) {
	if r.conflictDetector == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "conflict detector not configured"})
		return
	}
	// ?refresh=1 forces a synchronous re-query of every peer. Used after
	// remediation when the user clicks "Recheck now" in the banner.
	if req.URL.Query().Get("refresh") == "1" {
		r.conflictDetector.Invalidate()
	}
	ledger := r.conflictDetector.Current(req.Context())
	// Mirror the banner enrichment so JSON consumers see the same ledger.
	count := r.foreignSummaryForBanner(req.Context())
	ledger.ForeignFiles = conflict.ForeignFileSummary{Count: count}
	writeJSON(w, http.StatusOK, ledger)
}

// handleGetConflictBanner renders the banner partial for HTMX consumption.
// Returns the rendered HTML so the banner div can be swapped in place with
// a single hx-get; no JSON plumbing on the client side.
//
// GET /api/v1/config/conflict-banner
func (r *Router) handleGetConflictBanner(w http.ResponseWriter, req *http.Request) {
	if r.conflictDetector == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// ?refresh=1 forces a synchronous re-query of every peer before render,
	// matching GET /conflicts. The banner template's "Check again" buttons
	// (conflict_banner.templ:172, 316) rely on this to clear stale state
	// after a user remediates on the peer side.
	if req.URL.Query().Get("refresh") == "1" {
		r.conflictDetector.Invalidate()
	}
	ledger := r.conflictDetector.Current(req.Context())
	// Populate the foreign-file count on the ledger so BannerState can
	// promote the slate/blue warning state when no real conflict is active.
	count := r.foreignSummaryForBanner(req.Context())
	ledger.ForeignFiles = conflict.ForeignFileSummary{Count: count}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ConflictBannerContent(conflictBannerView(ledger)).Render(req.Context(), w); err != nil {
		r.logger.Warn("rendering conflict banner failed", "error", err)
	}
}

// handleGetConnectionConflictDetail renders the per-connection "Detected on
// this server" panel shown inside the settings card. The settings page
// lazy-loads this via HTMX so the synchronous peer check doesn't block the
// whole settings render.
//
// GET /api/v1/connections/{id}/conflict-detail
func (r *Router) handleGetConnectionConflictDetail(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" || r.conflictDetector == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ledger := r.conflictDetector.Current(req.Context())
	view := templates.ConnectionConflictDetailView{}
	for _, c := range ledger.Connections {
		if c.ConnectionID != id {
			continue
		}
		view.Known = true
		view.NFOWriteback = c.NFOWriteback
		view.ImageWriteback = c.ImageWriteback
		view.LibraryName = c.LibraryName
		view.ManageServerFiles = c.ManageServerFiles
		view.CheckErr = c.CheckErr
		if len(c.Paths) > 0 {
			limit := 3
			if len(c.Paths) < limit {
				limit = len(c.Paths)
			}
			summary := ""
			for i := 0; i < limit; i++ {
				if i > 0 {
					summary += ", "
				}
				summary += c.Paths[i]
			}
			if len(c.Paths) > limit {
				summary += fmt.Sprintf(" (+%d more)", len(c.Paths)-limit)
			}
			view.PathsSummary = summary
		}
		break
	}
	// HTMX lazy-loads this fragment per-row; an unknown id (deleted
	// connection, or a stale id from a cached page) has no detection state
	// to display. Returning 204 keeps the per-row container empty rather
	// than rendering a misleading "Not yet checked" message that the
	// template's !v.Known branch would emit.
	if !view.Known {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ConnectionConflictDetail(view).Render(req.Context(), w); err != nil {
		r.logger.Warn("rendering conflict detail failed", "error", err)
	}
}

// handleSetStillwaterManaged flips the "Let Stillwater manage artwork and NFO
// files on this server" toggle. ON snapshots the peer's saver config then
// PATCHes the peer to disable its savers; OFF restores the snapshot and
// clears it. After either path the detector is invalidated so the banner
// reflects the new state within one refresh.
//
// POST /api/v1/connections/{id}/stillwater-managed
// Body: {"enabled": true|false}
func (r *Router) handleSetStillwaterManaged(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing connection id"})
		return
	}

	// Accept either a JSON body ({"enabled":true}) or a form-encoded body
	// (enabled=true). HTMX buttons in the banner use form encoding because
	// the project does not bundle htmx's json-enc extension; API callers
	// are free to use JSON. The query-string fallback covers curl one-liners.
	//
	// Strict-validation rationale: empty body + missing query param + missing
	// or unparsable "enabled" key all return 400 instead of being coerced
	// to enabled=false. The off path mutates DB and peer state; treating bad
	// input as "disable" turns user typos and dropped HTMX bodies into
	// destructive state changes. Validate at the boundary, not after.
	body := struct {
		Enabled bool `json:"enabled"`
	}{}
	raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<12))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading body"})
		return
	}
	trimmed := strings.TrimSpace(string(raw))
	var seenEnabled bool
	switch {
	case trimmed == "":
		// No body; the query-param branch below will need to supply
		// enabled or we reject.
	case strings.HasPrefix(trimmed, "{"):
		// Parse into a raw map first so a missing "enabled" key is
		// distinguishable from an explicit false. Unmarshalling directly
		// into the struct silently zero-values the field.
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(raw, &payload); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
			return
		}
		if v, ok := payload["enabled"]; ok {
			if err := json.Unmarshal(v, &body.Enabled); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid enabled value"})
				return
			}
			seenEnabled = true
		}
	default:
		// Treat as application/x-www-form-urlencoded. url.ParseQuery is
		// lenient about missing values and only errors on malformed
		// percent encoding.
		values, perr := url.ParseQuery(trimmed)
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form body"})
			return
		}
		if v := values.Get("enabled"); v != "" {
			parsed, ok := parseBoolStrict(v)
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid enabled value"})
				return
			}
			body.Enabled, seenEnabled = parsed, true
		}
	}
	// Query-string fallback for curl users without a body, AND an override
	// for callers that want to be explicit on top of a body. Always wins
	// when present so the precedence is "URL > body" -- HTMX never sends
	// both, but this keeps API behavior predictable.
	if q := req.URL.Query().Get("enabled"); q != "" {
		parsed, ok := parseBoolStrict(q)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid enabled query param"})
			return
		}
		body.Enabled, seenEnabled = parsed, true
	}
	if !seenEnabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing enabled"})
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	// Idempotency guard. If the connection is already in the requested state
	// the side effects (snapshotLibraryOptions / SetPreStillwaterConfig on the
	// enable side; restoreLibraryOptions / SetPreStillwaterConfig clear on the
	// disable side) are not just wasted work, they are destructive: a second
	// enable=true would re-snapshot the peer's current (already-disabled)
	// LibraryOptions and overwrite pre_stillwater_config_json with that
	// post-managed state, so a future disable would replay Stillwater's own
	// settings instead of the user's original config. The matching disable
	// side would re-clear an already-cleared snapshot column. Returning the
	// current state in the same shape as a real toggle keeps the response
	// contract stable for clients that don't branch on no-op vs. apply.
	// See issue #1190 for the data-loss reproduction.
	if body.Enabled == conn.FeatureManageServerFiles {
		writeJSON(w, http.StatusOK, map[string]any{
			"connection_id":               conn.ID,
			"feature_manage_server_files": conn.FeatureManageServerFiles,
		})
		return
	}

	// refreshConflictState rebuilds the cached ledger and emits a
	// ConflictChanged event so the UI banner and write gate pick up the
	// new connection state immediately. It MUST run on every error path
	// too: clearStillwaterManaged flips SetManageServerFiles(false) before
	// attempting the peer restore, so a failure mid-flight leaves the DB
	// flag off while the cached ledger still treats the connection as
	// managed. Without this refresh the banner and gate would stay stale
	// until the 5-minute TTL expires. context.WithoutCancel keeps the
	// refresh going even after writeJSON sends the response and the HTTP
	// framework cancels the request context.
	refreshConflictState := func() {
		if r.conflictDetector == nil {
			return
		}
		r.conflictDetector.Invalidate()
		r.conflictDetector.Refresh(context.WithoutCancel(req.Context()))
	}

	if body.Enabled {
		if err := r.applyStillwaterManaged(req.Context(), conn); err != nil {
			refreshConflictState()
			r.logger.Error("applying stillwater-managed toggle failed", "connection_id", conn.ID, "connection_type", conn.Type, "error", err)
			status, msg := stillwaterManagedErrorResponse(err, "peer rejected snapshot or disable; see server log")
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	} else {
		if err := r.clearStillwaterManaged(req.Context(), conn); err != nil {
			refreshConflictState()
			r.logger.Error("clearing stillwater-managed toggle failed", "connection_id", conn.ID, "connection_type", conn.Type, "error", err)
			status, msg := stillwaterManagedErrorResponse(err, "peer rejected restore; see server log")
			writeJSON(w, status, map[string]string{"error": msg})
			return
		}
	}

	refreshConflictState()

	writeJSON(w, http.StatusOK, map[string]any{
		"connection_id":               conn.ID,
		"feature_manage_server_files": body.Enabled,
	})
}

// applyStillwaterManaged snapshots the peer then PATCHes it to disable all
// file write-back savers. Atomicity caveat: if the PATCH partially succeeds
// (one library out of three accepts, two fail), we keep the snapshot so
// restore can still roll back the accepted library. Better a partial
// rollback than an orphaned mutation.
//
// Once SetPreStillwaterConfig has persisted the snapshot, any subsequent
// failure must roll back: a second enable attempt would resnap the
// already-mutated peer state and overwrite the real pre-Stillwater config,
// so opt-out could no longer restore the original saver settings.
func (r *Router) applyStillwaterManaged(ctx context.Context, conn *connection.Connection) error {
	snapshot, err := r.snapshotLibraryOptions(ctx, conn)
	if err != nil {
		return fmt.Errorf("%w: snapshotting peer config: %w", ErrConflictPeerRejected, err)
	}
	if err := r.connectionService.SetPreStillwaterConfig(ctx, conn.ID, snapshot); err != nil {
		return fmt.Errorf("%w: persisting snapshot: %w", ErrConflictLocalPersist, err)
	}
	if err := r.disableFileWriteBack(ctx, conn); err != nil {
		r.rollbackStillwaterManaged(ctx, conn, snapshot, "disable peer savers")
		return fmt.Errorf("%w: disabling peer savers: %w", ErrConflictPeerRejected, err)
	}
	if err := r.connectionService.SetManageServerFiles(ctx, conn.ID, true); err != nil {
		r.rollbackStillwaterManaged(ctx, conn, snapshot, "set managed flag")
		return fmt.Errorf("%w: setting managed flag: %w", ErrConflictLocalPersist, err)
	}
	return nil
}

// rollbackStillwaterManaged best-effort restores the peer to the snapshotted
// state and clears the pre-Stillwater config row when applyStillwaterManaged
// fails after persisting the snapshot. Rollback failures are logged but not
// returned: the caller surfaces the original failure so the user sees the
// proximate cause rather than a derived rollback error.
func (r *Router) rollbackStillwaterManaged(ctx context.Context, conn *connection.Connection, snapshot, stage string) {
	if err := r.restoreLibraryOptions(ctx, conn, snapshot); err != nil {
		r.logger.Error("rollback restoreLibraryOptions failed", "connection_id", conn.ID, "stage", stage, "error", err)
	}
	if err := r.connectionService.SetPreStillwaterConfig(ctx, conn.ID, ""); err != nil {
		r.logger.Error("rollback SetPreStillwaterConfig clear failed", "connection_id", conn.ID, "stage", stage, "error", err)
	}
}

// clearStillwaterManaged flips the DB managed flag off FIRST, then restores
// the peer from snapshot and clears the snapshot column. The ordering is
// load-bearing: if SetManageServerFiles(false) fails, the peer is still in
// "Stillwater-managed" state with its savers off, so the conflict gate
// stays closed (the safe default). If we restored peer write-back first
// and then SetManageServerFiles failed, Stillwater would still consider
// the connection managed -- and AnyImageConflict / AnyNFOConflict skip
// managed rows -- so the gate would silently reopen even though peer
// write-back is back on. Snapshot clearing is last because failing there
// only leaves a stale snapshot (cosmetic; restore is idempotent).
func (r *Router) clearStillwaterManaged(ctx context.Context, conn *connection.Connection) error {
	snapshot := conn.PreStillwaterConfigJSON
	if err := r.connectionService.SetManageServerFiles(ctx, conn.ID, false); err != nil {
		return fmt.Errorf("%w: disabling managed mode: %w", ErrConflictLocalPersist, err)
	}
	if snapshot != "" {
		if err := r.restoreLibraryOptions(ctx, conn, snapshot); err != nil {
			return fmt.Errorf("%w: restoring peer config: %w", ErrConflictPeerRejected, err)
		}
	}
	if err := r.connectionService.SetPreStillwaterConfig(ctx, conn.ID, ""); err != nil {
		return fmt.Errorf("%w: clearing snapshot: %w", ErrConflictLocalPersist, err)
	}
	return nil
}

func (r *Router) snapshotLibraryOptions(ctx context.Context, conn *connection.Connection) (string, error) {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger).SnapshotLibraryOptions(ctx)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger).SnapshotLibraryOptions(ctx)
	case connection.TypeLidarr:
		return lidarr.New(conn.URL, conn.APIKey, r.logger).SnapshotLibraryOptions(ctx)
	default:
		return "", fmt.Errorf("unsupported connection type: %s", conn.Type)
	}
}

func (r *Router) disableFileWriteBack(ctx context.Context, conn *connection.Connection) error {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger).DisableFileWriteBack(ctx)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger).DisableFileWriteBack(ctx)
	case connection.TypeLidarr:
		return lidarr.New(conn.URL, conn.APIKey, r.logger).DisableFileWriteBack(ctx)
	default:
		return fmt.Errorf("unsupported connection type: %s", conn.Type)
	}
}

func (r *Router) restoreLibraryOptions(ctx context.Context, conn *connection.Connection, snapshotJSON string) error {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger).RestoreLibraryOptions(ctx, snapshotJSON)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger).RestoreLibraryOptions(ctx, snapshotJSON)
	case connection.TypeLidarr:
		return lidarr.New(conn.URL, conn.APIKey, r.logger).RestoreLibraryOptions(ctx, snapshotJSON)
	default:
		return fmt.Errorf("unsupported connection type: %s", conn.Type)
	}
}

// gateImageWrite returns true if the handler should proceed. Returns false
// (and has already written a 409 body) if the gate blocked the write. Safe
// to call when r.conflictGate is nil (no detector configured) -- callers
// fall through as if there were no conflict.
func (r *Router) gateImageWrite(w http.ResponseWriter, req *http.Request) bool {
	if r.conflictGate == nil {
		return true
	}
	if err := r.conflictGate.AllowImageWrite(req.Context()); err != nil {
		if be, ok := conflict.AsBlocked(err); ok {
			writeConflictError(w, be)
			return false
		}
		// Non-blocked error means the ledger failed to compute; fail open
		// rather than blocking every write on a transient detector problem.
		r.logger.Warn("image write gate check failed; falling through", "error", err)
	}
	return true
}

// gateNFOWrite is the NFO-axis equivalent of gateImageWrite.
func (r *Router) gateNFOWrite(w http.ResponseWriter, req *http.Request) bool {
	if r.conflictGate == nil {
		return true
	}
	if err := r.conflictGate.AllowNFOWrite(req.Context()); err != nil {
		if be, ok := conflict.AsBlocked(err); ok {
			writeConflictError(w, be)
			return false
		}
		r.logger.Warn("nfo write gate check failed; falling through", "error", err)
	}
	return true
}

// stillwaterManagedErrorResponse maps a sentinel-wrapped error from
// applyStillwaterManaged / clearStillwaterManaged to an HTTP status and
// user-facing message. ErrConflictLocalPersist => 500 (Stillwater-side DB
// failure); ErrConflictPeerRejected => 502 with the supplied peerMsg
// (which differs between apply and clear). Anything unwrapped falls
// through to 502 to preserve the historical shape.
func stillwaterManagedErrorResponse(err error, peerMsg string) (int, string) {
	switch {
	case errors.Is(err, ErrConflictLocalPersist):
		return http.StatusInternalServerError, "stillwater failed to persist managed-mode change; see server log"
	case errors.Is(err, ErrConflictPeerRejected):
		return http.StatusBadGateway, peerMsg
	default:
		return http.StatusBadGateway, peerMsg
	}
}

// writeConflictError emits a 409 JSON body with the structured conflict payload
// from a gate BlockedError. Handlers use this uniformly so the UI receives the
// same shape from every blocked endpoint.
func writeConflictError(w http.ResponseWriter, be *conflict.BlockedError) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":  fmt.Sprintf("%s_write_blocked", be.Axis),
		"axis":   string(be.Axis),
		"reason": be.Reason,
		"ledger": be.Ledger,
	})
}

// conflictBannerView converts the internal ledger to the view struct the
// templ component expects. Keeping the conversion local avoids leaking
// conflict package types into web/templates. PrimaryConnectionID is set to
// the first contributing connection when there is exactly one, so the
// "Let Stillwater manage it" CTA in the amber banner has a target; for
// multi-connection conflicts the CTA is suppressed and the user reviews
// each connection from the settings page.
func conflictBannerView(l conflict.Ledger) templates.ConflictBannerView {
	view := templates.ConflictBannerView{
		State: l.BannerState(),
	}
	for _, c := range l.Connections {
		if !c.Enabled || c.ManageServerFiles {
			continue
		}
		if c.ImageWriteback || c.NFOWriteback {
			view.Connections = append(view.Connections, templates.ConflictBannerConn{
				ID:             c.ConnectionID,
				Name:           c.ConnectionName,
				Type:           c.ConnectionType,
				LibraryName:    c.LibraryName,
				NFOWriteback:   c.NFOWriteback,
				ImageWriteback: c.ImageWriteback,
			})
		}
	}
	for _, rt := range l.RoundTrips {
		view.RoundTrips = append(view.RoundTrips, templates.ConflictBannerRoundTrip{
			AName: rt.ConnectionAName,
			BName: rt.ConnectionBName,
			Path:  rt.OverlappingPath,
		})
	}
	if len(view.Connections) == 1 {
		view.PrimaryConnectionID = view.Connections[0].ID
	}
	view.ForeignFileCount = l.ForeignFiles.Count
	return view
}
