package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/web/templates"
)

// writeDiscographyJSONError writes a JSON error payload matching the
// OpenAPI Error contract for the discography tab endpoint.
func writeDiscographyJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// handleArtistDiscographyTab renders the Discography tab fragment for the
// artist detail page. Album entries are sourced from the on-disk artist.nfo
// so what the user sees matches exactly what Kodi/Emby/Jellyfin will read.
// GET /artists/{id}/discography/tab
func (r *Router) handleArtistDiscographyTab(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeDiscographyJSONError(w, http.StatusNotFound, "artist not found")
			return
		}
		r.logger.Error("loading artist for discography tab", "artist_id", artistID, "error", err)
		writeDiscographyJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Parse the on-disk NFO directly so the tab reflects persisted state.
	// The cached artist.NFOExists flag is intentionally not consulted here:
	// if the file was added or restored out-of-band, the tab should still
	// reflect reality rather than waiting for a separate scan to refresh
	// the DB flag. ErrNotExist is treated as an empty-state signal; all
	// other read/parse errors are warned so operators can diagnose.
	var albums []artist.DiscographyAlbum
	if a.Path != "" {
		nfoPath := filepath.Join(a.Path, "artist.nfo")
		parsed, err := parseNFOFile(nfoPath)
		switch {
		case err == nil:
			albums = discographyFromNFO(parsed)
		case errors.Is(err, os.ErrNotExist):
			// No file on disk: render empty state silently.
		default:
			r.logger.Warn("failed to parse artist.nfo for discography tab",
				"artist_id", artistID, "path", nfoPath, "error", err)
		}
	}

	renderTempl(w, req, templates.ArtistDiscographyTab(templates.DiscographyTabData{
		ArtistID: artistID,
		Albums:   albums,
	}))
}

// discographyFromNFO maps NFO album entries into the artist-domain type used
// by the template. Kept as a small local helper to avoid a cross-package
// dependency from templates on the nfo package.
func discographyFromNFO(n *nfo.ArtistNFO) []artist.DiscographyAlbum {
	if n == nil || len(n.Albums) == 0 {
		return nil
	}
	out := make([]artist.DiscographyAlbum, 0, len(n.Albums))
	for _, alb := range n.Albums {
		out = append(out, artist.DiscographyAlbum{
			Title:                     alb.Title,
			Year:                      alb.Year,
			MusicBrainzReleaseGroupID: alb.MusicBrainzReleaseGroupID,
		})
	}
	return out
}
