package api

import (
	"context"
	"encoding/json"
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

// parseBoolLenient accepts the truthy forms HTMX and curl users are likely
// to send: "true"/"1"/"on"/"yes" map to true, everything else to false.
func parseBoolLenient(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "on", "yes", "y", "t":
		return true
	}
	return false
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
	ledger := r.conflictDetector.Current(req.Context())
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
	// are free to use JSON. Parsing is tolerant of either.
	body := struct {
		Enabled bool `json:"enabled"`
	}{}
	raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<12))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reading body"})
		return
	}
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "":
		// No body; caller relies on a query param or expects the toggle
		// default. Fall through with body.Enabled=false.
	case strings.HasPrefix(trimmed, "{"):
		if err := json.Unmarshal(raw, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
			return
		}
	default:
		// Treat as application/x-www-form-urlencoded. url.ParseQuery is
		// lenient about missing values and returns an error only on
		// malformed percent encoding.
		values, perr := url.ParseQuery(trimmed)
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form body"})
			return
		}
		body.Enabled = parseBoolLenient(values.Get("enabled"))
	}
	// Allow the caller to override via query string as a last resort, so
	// curl users without a body can still toggle via ?enabled=true.
	if q := req.URL.Query().Get("enabled"); q != "" {
		body.Enabled = parseBoolLenient(q)
	}

	conn, err := r.connectionService.GetByID(req.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}

	if body.Enabled {
		if err := r.applyStillwaterManaged(req.Context(), conn); err != nil {
			r.logger.Error("applying stillwater-managed toggle failed", "connection_id", conn.ID, "connection_type", conn.Type, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "peer rejected snapshot or disable; see server log"})
			return
		}
	} else {
		if err := r.clearStillwaterManaged(req.Context(), conn); err != nil {
			r.logger.Error("clearing stillwater-managed toggle failed", "connection_id", conn.ID, "connection_type", conn.Type, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "peer rejected restore; see server log"})
			return
		}
	}

	if r.conflictDetector != nil {
		r.conflictDetector.Invalidate()
		// Force a fresh read so the event bus emits ConflictChanged now,
		// before the HTTP response returns. The UI relies on that event to
		// re-fetch the banner without waiting for the 5-minute TTL.
		r.conflictDetector.Refresh(req.Context())
	}

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
func (r *Router) applyStillwaterManaged(ctx context.Context, conn *connection.Connection) error {
	snapshot, err := r.snapshotLibraryOptions(ctx, conn)
	if err != nil {
		return fmt.Errorf("snapshotting peer config: %w", err)
	}
	if err := r.connectionService.SetPreStillwaterConfig(ctx, conn.ID, snapshot); err != nil {
		return fmt.Errorf("persisting snapshot: %w", err)
	}
	if err := r.disableFileWriteBack(ctx, conn); err != nil {
		return fmt.Errorf("disabling peer savers: %w", err)
	}
	return r.connectionService.SetManageServerFiles(ctx, conn.ID, true)
}

// clearStillwaterManaged restores the peer from the snapshot and clears the
// toggle + snapshot column. If the snapshot is empty (toggle was flipped off
// without ever having been on) we still flip the DB bit so the UI matches.
func (r *Router) clearStillwaterManaged(ctx context.Context, conn *connection.Connection) error {
	if conn.PreStillwaterConfigJSON != "" {
		if err := r.restoreLibraryOptions(ctx, conn, conn.PreStillwaterConfigJSON); err != nil {
			return fmt.Errorf("restoring peer config: %w", err)
		}
	}
	if err := r.connectionService.SetPreStillwaterConfig(ctx, conn.ID, ""); err != nil {
		return fmt.Errorf("clearing snapshot: %w", err)
	}
	return r.connectionService.SetManageServerFiles(ctx, conn.ID, false)
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
	return view
}
