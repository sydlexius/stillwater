package api

// handlers_artist_duplicates.go -- handler for the "Possible duplicate artists"
// settings page.
//
// Route: GET {basePath}/settings/artist-duplicates
// Registered BEFORE the catch-all /settings/{section} redirect so it wins on
// a direct path match.  Admin-only (reuses requireForeignAdmin).
//
// The page is read-only: it lists detected near-duplicate groups but does not
// provide a merge button.  The filesystem-consolidating merge is tracked
// separately in #1615.  Detection runs fully in-memory (no stored column, no
// migration) via artist.DetectDuplicates.

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleArtistDuplicatesPage renders /settings/artist-duplicates.  Admin-only.
func (r *Router) handleArtistDuplicatesPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}

	// r.db is the raw *sql.DB wired in during Router construction.  Using it
	// directly avoids any intermediate layer and keeps detection off the
	// Service.List / buildWhereClause path.
	if r.db == nil {
		renderTempl(w, req, templates.ArtistDuplicatesPage(r.assetsFor(req), templates.ArtistDuplicatesPageView{}))
		return
	}

	groups, err := artist.DetectDuplicates(req.Context(), r.db)
	if err != nil {
		r.logger.Error("detecting near-duplicate artists", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	view := buildArtistDuplicatesView(groups)
	renderTempl(w, req, templates.ArtistDuplicatesPage(r.assetsFor(req), view))
}

// buildArtistDuplicatesView converts the detection result into the view model
// used by the template.  Extracted as a named function so tests can exercise
// the conversion logic independently.
func buildArtistDuplicatesView(groups []artist.NearDuplicateGroup) templates.ArtistDuplicatesPageView {
	rows := make([]templates.ArtistDuplicateGroupRow, 0, len(groups))
	for _, g := range groups {
		members := make([]templates.ArtistDuplicateMember, 0, len(g.Members))
		for _, m := range g.Members {
			members = append(members, templates.ArtistDuplicateMember{
				ID:   m.ID,
				Name: m.Name,
				Path: m.Path,
				MBID: m.MBID,
			})
		}
		rows = append(rows, templates.ArtistDuplicateGroupRow{
			Key:     g.Key,
			Reason:  g.Reason,
			Members: members,
		})
	}
	return templates.ArtistDuplicatesPageView{
		Groups: rows,
	}
}
