package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleNFODiff computes a field-level diff between the current NFO and a snapshot.
// GET /api/v1/artists/{id}/nfo/diff
func (r *Router) handleNFODiff(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	// For pathless artists, generate a virtual NFO from database fields.
	// For path-based artists, read the on-disk NFO file.
	var currentNFO *nfo.ArtistNFO
	if a.Path != "" {
		nfoPath := filepath.Join(a.Path, "artist.nfo")
		currentNFO = parseNFOFile(nfoPath)
	} else {
		currentNFO = nfo.FromArtist(a)
	}

	// Determine what to compare against
	var snapshotNFO *nfo.ArtistNFO

	compareToID := req.URL.Query().Get("compare_to")
	if compareToID != "" {
		snap, err := r.nfoSnapshotService.GetByID(req.Context(), compareToID)
		if err != nil {
			writeError(w, req, http.StatusNotFound, "snapshot not found")
			return
		}
		if snap.ArtistID != artistID {
			writeError(w, req, http.StatusNotFound, "snapshot not found")
			return
		}
		snapshotNFO, err = nfo.Parse(strings.NewReader(snap.Content))
		if err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid snapshot content")
			return
		}
	} else {
		// Default: compare against the latest snapshot
		snapshots, err := r.nfoSnapshotService.List(req.Context(), artistID)
		if err != nil {
			r.logger.Warn("listing snapshots for nfo diff", "artist_id", artistID, "error", err)
		} else if len(snapshots) > 0 {
			parsed, parseErr := nfo.Parse(strings.NewReader(snapshots[0].Content))
			if parseErr != nil {
				r.logger.Warn("parsing snapshot for nfo diff", "artist_id", artistID, "snapshot_id", snapshots[0].ID, "error", parseErr)
			} else {
				snapshotNFO = parsed
			}
		}
	}

	diff := nfo.Diff(snapshotNFO, currentNFO)

	if isHTMXRequest(req) {
		renderTempl(w, req, templates.NFODiffFragment(*diff))
		return
	}
	writeJSON(w, http.StatusOK, diff)
}

// handleNFOSnapshotList returns all NFO snapshots for an artist.
// GET /api/v1/artists/{id}/nfo/snapshots
func (r *Router) handleNFOSnapshotList(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	snapshots, err := r.nfoSnapshotService.List(req.Context(), artistID)
	if err != nil {
		r.logger.Error("listing nfo snapshots", "artist_id", artistID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to list snapshots")
		return
	}

	if snapshots == nil {
		snapshots = []nfo.Snapshot{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snapshots})
}

// handleNFOSnapshotRestore restores an NFO from a snapshot.
// POST /api/v1/artists/{id}/nfo/snapshots/{snapshotId}/restore
func (r *Router) handleNFOSnapshotRestore(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	snapshotID, ok := RequirePathParam(w, req, "snapshotId")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	snap, err := r.nfoSnapshotService.GetByID(req.Context(), snapshotID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "snapshot not found")
		return
	}
	if snap.ArtistID != artistID {
		writeError(w, req, http.StatusNotFound, "snapshot not found")
		return
	}

	if a.Path == "" {
		// Pathless artist: apply snapshot fields to the database and push to platforms.
		snapNFO, parseErr := nfo.Parse(strings.NewReader(snap.Content))
		if parseErr != nil {
			writeError(w, req, http.StatusBadRequest, "invalid snapshot content")
			return
		}

		// Save current state as a safety snapshot before restoring.
		currentNFO := nfo.FromArtist(a)
		var buf strings.Builder
		if writeErr := nfo.Write(&buf, currentNFO); writeErr != nil {
			r.logger.Warn("generating safety snapshot for virtual restore", "artist_id", artistID, "error", writeErr)
		} else if buf.Len() > 0 {
			if _, saveErr := r.nfoSnapshotService.Save(req.Context(), artistID, buf.String()); saveErr != nil {
				r.logger.Warn("saving safety snapshot before virtual restore", "artist_id", artistID, "error", saveErr)
			}
		}

		nfo.ApplyNFOToArtist(snapNFO, a)
		if err := r.artistService.Update(req.Context(), a); err != nil {
			r.logger.Error("updating artist from virtual nfo restore", "artist_id", artistID, "error", err)
			writeError(w, req, http.StatusInternalServerError, "failed to restore NFO")
			return
		}
		r.publisher.PushMetadataAsync(req.Context(), a)
		writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
		return
	}

	// Path-based artist: restore NFO file to disk.
	// Safety: save current NFO as a new snapshot before overwriting
	currentPath := filepath.Join(a.Path, "artist.nfo")
	if currentData, readErr := os.ReadFile(currentPath); readErr == nil { //nolint:gosec // G304: path is constructed from trusted artist.Path, not user input
		if _, saveErr := r.nfoSnapshotService.Save(req.Context(), artistID, string(currentData)); saveErr != nil {
			r.logger.Warn("saving safety snapshot before restore", "artist_id", artistID, "error", saveErr)
		}
	}

	// Register expected write so the filesystem watcher does not treat
	// this restore as an external modification.
	if r.expectedWrites != nil {
		r.expectedWrites.Add(currentPath)
		defer r.expectedWrites.Remove(currentPath)
	}

	// Write the snapshot content to disk
	if err := filesystem.WriteFileAtomic(currentPath, []byte(snap.Content), 0o644); err != nil {
		r.logger.Error("restoring nfo snapshot", "artist_id", artistID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to restore NFO")
		return
	}

	// Parse the restored snapshot and apply its fields to the artist struct
	// so that PushMetadataAsync sends the restored values, not the pre-restore
	// state. This mirrors the pathless branch logic above.
	if snapNFO, parseErr := nfo.Parse(strings.NewReader(snap.Content)); parseErr == nil {
		nfo.ApplyNFOToArtist(snapNFO, a)
	}
	a.NFOExists = true
	if err := r.artistService.Update(req.Context(), a); err != nil {
		r.logger.Warn("updating artist after nfo restore", "artist_id", artistID, "error", err)
	}
	// Push to platforms only -- the NFO was already restored to disk above,
	// so WriteBackNFO is not needed.
	r.publisher.PushMetadataAsync(req.Context(), a)

	writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
}

// handleNFODiffPage renders the NFO diff HTML page.
// GET /artists/{id}/nfo
func (r *Router) handleNFODiffPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		renderTempl(w, req, templates.LoginPage(r.assets()))
		return
	}

	artistID := req.PathValue("id")
	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		http.Error(w, "artist not found", http.StatusNotFound)
		return
	}

	snapshots, snapErr := r.nfoSnapshotService.List(req.Context(), artistID)
	if snapErr != nil {
		r.logger.Warn("listing snapshots for nfo diff page", "artist_id", artistID, "error", snapErr)
	}
	if snapshots == nil {
		snapshots = []nfo.Snapshot{}
	}

	data := templates.NFODiffPageData{
		Artist:     *a,
		Diff:       nfo.DiffResult{},
		Snapshots:  snapshots,
		IsPathless: a.Path == "",
	}
	renderTempl(w, req, templates.NFODiffPage(r.assets(), data))
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

// parseNFOFile parses an NFO file from disk, returning nil if it cannot be read.
func parseNFOFile(path string) *nfo.ArtistNFO {
	f, err := os.Open(path) //nolint:gosec // G304: path is constructed from trusted artist.Path, not user input
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck

	parsed, err := nfo.Parse(f)
	if err != nil {
		return nil
	}
	return parsed
}
