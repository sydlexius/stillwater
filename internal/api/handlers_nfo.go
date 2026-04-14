package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleNFODiff computes a field-level diff between the on-disk NFO and
// the current database metadata.
// GET /api/v1/artists/{id}/nfo/diff
func (r *Router) handleNFODiff(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeError(w, req, http.StatusNotFound, "artist not found")
			return
		}
		r.logger.Error("fetching artist for NFO diff", "artist_id", artistID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "internal error")
		return
	}

	// Compare on-disk NFO (previous state) against current database metadata.
	// For pathless artists, both sides come from the database, so no diff.
	dbNFO := nfo.FromArtist(a)
	onDiskNFO := dbNFO // default: identical, so no diff for pathless artists
	if a.Path != "" {
		nfoPath := filepath.Join(a.Path, "artist.nfo")
		parsed, parseErr := parseNFOFile(nfoPath)
		switch {
		case parseErr == nil:
			onDiskNFO = parsed
		case errors.Is(parseErr, os.ErrNotExist):
			// No file on disk: diff against nil (full added-fields diff).
			onDiskNFO = nil
		default:
			// Read/parse failure: surface via log so operators can
			// diagnose, and treat as no on-disk NFO so the diff does
			// not silently hide corruption.
			onDiskNFO = nil
			r.logger.Warn("failed to parse artist.nfo for nfo diff",
				"artist_id", artistID, "path", nfoPath, "error", parseErr)
		}
	}
	diff := nfo.Diff(onDiskNFO, dbNFO)

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.NFODiffFragment(*diff))
		return
	}
	writeJSON(w, http.StatusOK, diff)
}

// handleNFODiffPage renders the NFO diff HTML page.
// GET /artists/{id}/nfo
func (r *Router) handleNFODiffPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	artistID := req.PathValue("id")
	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			http.Error(w, "artist not found", http.StatusNotFound)
			return
		}
		r.logger.Error("fetching artist for NFO diff page", "artist_id", artistID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := templates.NFODiffPageData{
		Artist:     *a,
		Diff:       nfo.DiffResult{},
		IsPathless: a.Path == "",
	}
	renderTempl(w, req, templates.NFODiffPage(r.assetsFor(req), data))
}

// handleNFOConflictCheck checks whether an artist's NFO has been modified externally.
// GET /api/v1/artists/{id}/nfo/conflict
func (r *Router) handleNFOConflictCheck(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	// Pathless artists have no on-disk NFO, so no conflict is possible.
	if a.Path == "" {
		writeJSON(w, http.StatusOK, nfo.ConflictCheck{})
		return
	}

	nfoPath := filepath.Join(a.Path, "artist.nfo")

	// Use the latest snapshot time as reference, or fall back to 24h ago
	since := time.Now().Add(-24 * time.Hour)
	snapshots, err := r.nfoSnapshotService.List(req.Context(), artistID)
	if err == nil && len(snapshots) > 0 {
		since = snapshots[0].CreatedAt
	}

	check := nfo.CheckFileConflict(nfoPath, since)

	// Check all connection types for NFO writer config
	for _, connType := range []string{connection.TypeLidarr, connection.TypeEmby, connection.TypeJellyfin} {
		if check.ExternalWriter != "" {
			break
		}
		conns, listErr := r.connectionService.ListByType(req.Context(), connType)
		if listErr != nil {
			continue
		}
		for _, conn := range conns {
			if !conn.Enabled {
				continue
			}
			enabled, libName, _ := r.checkConnectionForNFOWriter(req.Context(), conn)
			if enabled {
				check.ExternalWriter = conn.Type + ":" + conn.Name
				if !check.HasConflict {
					check.Reason = nfoWriterWarning(conn.Type, libName)
				}
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, check)
}

// ClobberRisk describes whether a specific connection may overwrite NFO/image files.
type ClobberRisk struct {
	ConnectionID   string `json:"connection_id"`
	ConnectionName string `json:"connection_name"`
	ConnectionType string `json:"connection_type"`
	NFOWriter      bool   `json:"nfo_writer"`
	LibraryName    string `json:"library_name,omitempty"`
	Error          string `json:"error,omitempty"`
}

// ClobberCheckResponse is the response for GET /api/v1/connections/clobber-check.
type ClobberCheckResponse struct {
	HasRisk bool          `json:"has_risk"`
	Risks   []ClobberRisk `json:"risks"`
}

// handleClobberCheck checks all enabled connections for NFO/image writing configuration.
// GET /api/v1/connections/clobber-check
func (r *Router) handleClobberCheck(w http.ResponseWriter, req *http.Request) {
	// If no library has a filesystem path, Stillwater cannot write NFO files,
	// so there is no clobber risk regardless of server configuration.
	if r.libraryService != nil {
		libs, err := r.libraryService.List(req.Context())
		if err == nil {
			hasPath := false
			for _, lib := range libs {
				if lib.Path != "" {
					hasPath = true
					break
				}
			}
			if !hasPath {
				if isHTMXRequest(req) {
					w.WriteHeader(http.StatusOK)
					return
				}
				writeJSON(w, http.StatusOK, ClobberCheckResponse{Risks: []ClobberRisk{}})
				return
			}
		}
	}

	conns, err := r.connectionService.List(req.Context())
	if err != nil {
		r.logger.Error("listing connections for clobber check", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	resp := ClobberCheckResponse{Risks: []ClobberRisk{}}

	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}

		enabled, libName, checkErr := r.checkConnectionForNFOWriter(req.Context(), conn)

		risk := ClobberRisk{
			ConnectionID:   conn.ID,
			ConnectionName: conn.Name,
			ConnectionType: conn.Type,
			NFOWriter:      enabled,
			LibraryName:    libName,
		}
		if checkErr != nil {
			risk.Error = checkErr.Error()
		}

		if risk.NFOWriter || risk.Error != "" {
			resp.Risks = append(resp.Risks, risk)
			if risk.NFOWriter {
				resp.HasRisk = true
			}
		}
	}

	if isHTMXRequest(req) {
		var warnings []templates.ClobberWarning
		for _, risk := range resp.Risks {
			if risk.NFOWriter {
				warnings = append(warnings, templates.ClobberWarning{
					ConnectionName: risk.ConnectionName,
					ConnectionType: risk.ConnectionType,
					LibraryName:    risk.LibraryName,
				})
			}
		}
		renderTempl(w, req, templates.ClobberWarnings(warnings))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// checkConnectionForNFOWriter checks a single connection for NFO writer configuration.
// All three client types (Lidarr, Emby, Jellyfin) now share the same (bool, string, error) signature.
func (r *Router) checkConnectionForNFOWriter(ctx context.Context, conn connection.Connection) (bool, string, error) {
	switch conn.Type {
	case connection.TypeLidarr:
		return lidarr.New(conn.URL, conn.APIKey, r.logger).CheckNFOWriterEnabled(ctx)
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger).CheckNFOWriterEnabled(ctx)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, r.logger).CheckNFOWriterEnabled(ctx)
	default:
		return false, "", nil
	}
}

// connTypeLabel maps connection type constants to display labels.
var connTypeLabel = map[string]string{
	connection.TypeLidarr:   "Lidarr",
	connection.TypeEmby:     "Emby",
	connection.TypeJellyfin: "Jellyfin",
}

// nfoWriterWarning returns a human-readable warning for a connection with NFO writing enabled.
func nfoWriterWarning(connType, libName string) string {
	label := connTypeLabel[connType]
	if label == "" {
		label = connType
	}
	if libName != "" {
		return label + " NFO saver is enabled on library \"" + libName + "\" and may overwrite changes"
	}
	return label + " NFO writer is enabled and may overwrite changes"
}

// parseNFOFile parses an NFO file from disk. Returns (parsed, nil) on success,
// (nil, err) on failure. Callers can distinguish a missing file (os.ErrNotExist)
// from a parse error via errors.Is, so they can treat absence as an empty state
// while still surfacing corruption or IO errors.
func parseNFOFile(path string) (*nfo.ArtistNFO, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is constructed from trusted artist.Path, not user input
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	parsed, err := nfo.Parse(f)
	if err != nil {
		return nil, err
	}
	return parsed, nil
}
