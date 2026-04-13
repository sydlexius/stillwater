package api

import (
	"errors"
	"net/http"
	"path/filepath"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/web/templates"
)

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
			http.Error(w, "artist not found", http.StatusNotFound)
			return
		}
		r.logger.Error("loading artist for discography tab", "artist_id", artistID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Parse the on-disk NFO directly so the tab reflects persisted state, not
	// the in-memory artist struct (which does not carry the Discography slice
	// through the standard repository load path today).
	var albums []artist.DiscographyAlbum
	if a.Path != "" && a.NFOExists {
		if parsed := parseNFOFile(filepath.Join(a.Path, "artist.nfo")); parsed != nil {
			albums = discographyFromNFO(parsed)
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
