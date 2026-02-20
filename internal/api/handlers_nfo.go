package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleNFODiff computes a field-level diff between the current NFO and a snapshot.
// GET /api/v1/artists/{id}/nfo/diff
func (r *Router) handleNFODiff(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	// Parse the current on-disk NFO
	nfoPath := filepath.Join(a.Path, "artist.nfo")
	currentNFO := parseNFOFile(nfoPath)

	// Determine what to compare against
	var snapshotNFO *nfo.ArtistNFO

	compareToID := req.URL.Query().Get("compare_to")
	if compareToID != "" {
		snap, err := r.nfoSnapshotService.GetByID(req.Context(), compareToID)
		if err == nil {
			snapshotNFO, _ = nfo.Parse(strings.NewReader(snap.Content))
		}
	} else {
		// Default: compare against the latest snapshot
		snapshots, err := r.nfoSnapshotService.List(req.Context(), artistID)
		if err == nil && len(snapshots) > 0 {
			snapshotNFO, _ = nfo.Parse(strings.NewReader(snapshots[0].Content))
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
	artistID := req.PathValue("id")
	if artistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing artist id"})
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
	artistID := req.PathValue("id")
	snapshotID := req.PathValue("snapshotId")

	if artistID == "" || snapshotID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing required path parameters"})
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

	// Safety: save current NFO as a new snapshot before overwriting
	currentPath := filepath.Join(a.Path, "artist.nfo")
	if currentData, readErr := os.ReadFile(currentPath); readErr == nil { //nolint:gosec // G304: path is constructed from trusted artist.Path, not user input
		if _, saveErr := r.nfoSnapshotService.Save(req.Context(), artistID, string(currentData)); saveErr != nil {
			r.logger.Warn("saving safety snapshot before restore", "artist_id", artistID, "error", saveErr)
		}
	}

	// Write the snapshot content to disk
	if err := filesystem.WriteFileAtomic(currentPath, []byte(snap.Content), 0o644); err != nil {
		r.logger.Error("restoring nfo snapshot", "artist_id", artistID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to restore NFO")
		return
	}

	// Update artist flags
	a.NFOExists = true
	if err := r.artistService.Update(req.Context(), a); err != nil {
		r.logger.Warn("updating artist after nfo restore", "artist_id", artistID, "error", err)
	}

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

	snapshots, _ := r.nfoSnapshotService.List(req.Context(), artistID)
	if snapshots == nil {
		snapshots = []nfo.Snapshot{}
	}

	data := templates.NFODiffPageData{
		Artist:    *a,
		Diff:      nfo.DiffResult{},
		Snapshots: snapshots,
	}
	renderTempl(w, req, templates.NFODiffPage(r.assets(), data))
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
