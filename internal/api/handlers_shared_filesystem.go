package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/web/templates"
)

// SharedFilesystemStatus holds the current shared-filesystem detection state
// returned by the status endpoint and consumed by the notification bar template.
type SharedFilesystemStatus struct {
	HasOverlaps          bool                             `json:"has_overlaps"`
	Libraries            []SharedFilesystemEntry          `json:"libraries"`
	Dismissed            bool                             `json:"dismissed"`                        // user chose "don't show again"
	ImageFetcherWarnings []connection.ImageFetcherWarning `json:"image_fetcher_warnings,omitempty"` // platform image fetcher conflicts
}

// SharedFilesystemEntry describes one library with a shared-filesystem overlap.
type SharedFilesystemEntry struct {
	LibraryID   string `json:"library_id"`
	LibraryName string `json:"library_name"`
	Path        string `json:"path"`
	OverlapWith string `json:"overlap_with"`
}

// handleSharedFilesystemStatus returns the current shared-filesystem overlap state.
// For HTMX requests, returns the notification bar HTML partial.
// For API requests, returns JSON.
// GET /api/v1/shared-filesystem/status
func (r *Router) handleSharedFilesystemStatus(w http.ResponseWriter, req *http.Request) {
	status, sharedLibs, err := r.buildSharedFilesystemStatus(req.Context())
	if err != nil {
		r.logger.Error("checking shared filesystem status", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// For HTMX requests, render the notification bar partial.
	// Skip image fetcher API calls since the bar does not display them.
	if isHTMXRequest(req) {
		data := templates.SharedFSBarData{
			HasOverlaps: status.HasOverlaps,
			Dismissed:   status.Dismissed,
		}
		for _, lib := range status.Libraries {
			data.Libraries = append(data.Libraries, templates.SharedFSBarLib{
				Name: lib.LibraryName,
				Path: lib.Path,
			})
		}
		renderTempl(w, req, templates.SharedFilesystemBarContent(data))
		return
	}

	// Collect image fetcher warnings for JSON API consumers.
	if len(sharedLibs) > 0 {
		status.ImageFetcherWarnings = r.collectImageFetcherWarnings(req.Context(), sharedLibs)
	}

	writeJSON(w, http.StatusOK, status)
}

// handleSharedFilesystemDismiss stores a persistent "dismissed" preference
// so the notification bar is not shown again. For HTMX requests, returns
// an empty response so the bar element is removed from the DOM.
// POST /api/v1/shared-filesystem/dismiss
func (r *Router) handleSharedFilesystemDismiss(w http.ResponseWriter, req *http.Request) {
	_, err := r.db.ExecContext(req.Context(), //nolint:gosec // G701: static query, no user input
		`INSERT INTO settings (key, value, updated_at) VALUES ('shared_filesystem.dismissed', 'true', datetime('now'))
		 ON CONFLICT(key) DO UPDATE SET value = 'true', updated_at = datetime('now')`)
	if err != nil {
		r.logger.Error("dismissing shared filesystem warning", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// For HTMX requests, return empty body so hx-swap="outerHTML" removes the bar.
	if isHTMXRequest(req) {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
}

// handleSharedFilesystemRecheck returns the current count of libraries with
// a shared-filesystem status. With the evidence-based model, status is set
// by external evidence (fsnotify, mtime, provenance), not path comparison.
// POST /api/v1/shared-filesystem/recheck
func (r *Router) handleSharedFilesystemRecheck(w http.ResponseWriter, req *http.Request) {
	count, err := r.recheckSharedFilesystem(req.Context())
	if err != nil {
		r.logger.Error("rechecking shared filesystem overlaps", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"shared_libraries": count,
	})
}

// recheckSharedFilesystem returns the current count of libraries with a
// shared-filesystem status. With the evidence-based model, status is set
// externally (fsnotify, mtime checks, NFO provenance), so this is now a
// read-only query rather than a path-comparison recheck.
func (r *Router) recheckSharedFilesystem(ctx context.Context) (int, error) {
	libs, err := r.libraryService.ListSharedFS(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing shared-filesystem libraries: %w", err)
	}
	return len(libs), nil
}

// buildSharedFilesystemStatus assembles the current status by reading library
// shared_fs_status columns and the dismiss preference. Peer library names are
// resolved from SharedFSPeerLibraryIDs when available.
func (r *Router) buildSharedFilesystemStatus(ctx context.Context) (*SharedFilesystemStatus, []library.Library, error) {
	sharedLibs, err := r.libraryService.ListSharedFS(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list shared-filesystem libraries: %w", err)
	}

	status := &SharedFilesystemStatus{
		HasOverlaps: len(sharedLibs) > 0,
		Libraries:   []SharedFilesystemEntry{},
	}

	if len(sharedLibs) > 0 {
		// Build a lookup of all libraries so we can resolve peer library names.
		allLibs, allErr := r.libraryService.List(ctx)
		if allErr != nil {
			return nil, nil, fmt.Errorf("list libraries for peer resolution: %w", allErr)
		}
		libNames := make(map[string]string, len(allLibs))
		for _, lib := range allLibs {
			libNames[lib.ID] = lib.Name
		}

		for _, lib := range sharedLibs {
			// Resolve peer library IDs to a human-readable description.
			overlapWith := resolvePeerDescription(lib.SharedFSPeerLibraryIDs, libNames)

			status.Libraries = append(status.Libraries, SharedFilesystemEntry{
				LibraryID:   lib.ID,
				LibraryName: lib.Name,
				Path:        lib.Path,
				OverlapWith: overlapWith,
			})
		}
	}

	// Check the dismiss preference. sql.ErrNoRows means the setting has not
	// been stored yet (not dismissed). Any other DB error is logged and treated
	// as "not dismissed" (show the bar) as the safe default.
	var dismissed string
	err = r.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = 'shared_filesystem.dismissed'`).Scan(&dismissed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.logger.Warn("reading shared_filesystem.dismissed setting", "error", err)
	}
	if err == nil && dismissed == "true" {
		status.Dismissed = true
	}

	return status, sharedLibs, nil
}

// collectImageFetcherWarnings queries connected Emby/Jellyfin platforms for
// image fetcher settings that may conflict with Stillwater's image management.
// Each shared library with a connection is checked. Failures are logged and
// skipped so this never prevents the status endpoint from responding.
func (r *Router) collectImageFetcherWarnings(ctx context.Context, sharedLibs []library.Library) []connection.ImageFetcherWarning {
	// Deduplicate connections so we only query each platform once.
	checked := make(map[string]bool)
	var warnings []connection.ImageFetcherWarning

	for _, lib := range sharedLibs {
		if lib.ConnectionID == "" || checked[lib.ConnectionID] {
			continue
		}
		checked[lib.ConnectionID] = true

		conn, connErr := r.connectionService.GetByID(ctx, lib.ConnectionID)
		if connErr != nil {
			r.logger.Warn("could not load connection for image fetcher check",
				"connection_id", lib.ConnectionID, "error", connErr)
			continue
		}

		var w []connection.ImageFetcherWarning
		switch conn.Type {
		case connection.TypeEmby:
			w = r.checkEmbyImageFetchers(ctx, conn)
		case connection.TypeJellyfin:
			w = r.checkJellyfinImageFetchers(ctx, conn)
		default:
			r.logger.Debug("image fetcher check not implemented for connection type",
				"connection_id", conn.ID, "type", conn.Type)
			continue
		}
		warnings = append(warnings, w...)
	}

	return warnings
}

// checkEmbyImageFetchers queries an Emby connection for image fetcher settings.
func (r *Router) checkEmbyImageFetchers(ctx context.Context, conn *connection.Connection) []connection.ImageFetcherWarning {
	client := emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
	statuses, err := client.CheckImageFetchersEnabled(ctx)
	if err != nil {
		r.logger.Warn("checking emby image fetchers", "connection", conn.Name, "error", err)
		return []connection.ImageFetcherWarning{{
			Platform:  "emby",
			RiskLevel: "warn",
			Message:   fmt.Sprintf("Could not check Emby image fetcher settings for connection '%s'. Verify the connection is reachable.", conn.Name),
		}}
	}

	var warnings []connection.ImageFetcherWarning
	for _, s := range statuses {
		msg := fmt.Sprintf(
			"Emby's image fetchers (%s) are enabled for library '%s' and may download additional images to your music directory. Stillwater's NFOs are protected by lockdata, but image conflicts may occur.",
			strings.Join(s.FetcherNames, ", "), s.LibraryName,
		)
		warnings = append(warnings, connection.ImageFetcherWarning{
			Platform:     "emby",
			LibraryName:  s.LibraryName,
			FetcherNames: s.FetcherNames,
			RiskLevel:    s.RiskLevel,
			Message:      msg,
		})
	}
	return warnings
}

// checkJellyfinImageFetchers queries a Jellyfin connection for image fetcher settings.
func (r *Router) checkJellyfinImageFetchers(ctx context.Context, conn *connection.Connection) []connection.ImageFetcherWarning {
	client := jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger)
	statuses, err := client.CheckImageFetchersEnabled(ctx)
	if err != nil {
		r.logger.Warn("checking jellyfin image fetchers", "connection", conn.Name, "error", err)
		return []connection.ImageFetcherWarning{{
			Platform:  "jellyfin",
			RiskLevel: "warn",
			Message:   fmt.Sprintf("Could not check Jellyfin image fetcher settings for connection '%s'. Verify the connection is reachable.", conn.Name),
		}}
	}

	var warnings []connection.ImageFetcherWarning
	for _, s := range statuses {
		msg := fmt.Sprintf(
			"Jellyfin's image fetchers (%s) are enabled for library '%s'. Jellyfin can replace existing images and strip EXIF provenance data. Disable image fetchers in Jellyfin's library settings.",
			strings.Join(s.FetcherNames, ", "), s.LibraryName,
		)
		warnings = append(warnings, connection.ImageFetcherWarning{
			Platform:     "jellyfin",
			LibraryName:  s.LibraryName,
			FetcherNames: s.FetcherNames,
			RiskLevel:    s.RiskLevel,
			Message:      msg,
		})
	}
	return warnings
}

// resolvePeerDescription converts a comma-separated list of library IDs into
// a human-readable description like "library 'Music A', library 'Music B'".
func resolvePeerDescription(peerIDs string, libNames map[string]string) string {
	if peerIDs == "" {
		return ""
	}
	parts := strings.Split(peerIDs, ",")
	var descriptions []string
	for _, id := range parts {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if name, ok := libNames[id]; ok {
			descriptions = append(descriptions, "library '"+name+"'")
		} else {
			descriptions = append(descriptions, "unknown library (deleted?)")
		}
	}
	return strings.Join(descriptions, ", ")
}
