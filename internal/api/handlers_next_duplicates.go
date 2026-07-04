package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/templates"
	"github.com/sydlexius/stillwater/web/templates/next"
)

// handleNextArtistDuplicatesPage serves the next/ channel "Possible duplicate
// artists" page (M55 #1752) at GET {basePath}/next/reports/duplicates.
//
// It reuses the stable detection + view-model path (artist.DetectDuplicates +
// buildArtistDuplicatesView) verbatim and only swaps the rendered template for
// the next/ shell (next.ArtistDuplicatesNextPage). Admin-only, matching the
// stable handler's requireForeignAdmin gate.
//
// In stable mode (SW_UX=stable) the UX middleware 404s any /next/* request
// before this handler runs (decision 12 in architecture-decisions.md). The
// in-handler checkNextChannel guard below is therefore only reachable when the
// lane IS enabled (next/dual mode) and the resolved channel is not "next" --
// triggered by an explicit X-Stillwater-UX: stable header. In that edge case it
// returns 404 (the /next/ path does not serve stable content).
func (r *Router) handleNextArtistDuplicatesPage(w http.ResponseWriter, req *http.Request) {
	if !checkNextChannel(w, req) {
		return
	}
	if !r.requireForeignAdmin(w, req) {
		return
	}

	// r.db is the raw *sql.DB wired in during Router construction; detection
	// runs fully in-memory off it (no stored column, no migration). Mirror the
	// stable handler's nil-db guard so a partially constructed Router renders
	// an empty page rather than panicking.
	if r.db == nil {
		renderTempl(w, req, next.ArtistDuplicatesNextPage(r.assetsFor(req), templates.ArtistDuplicatesPageView{}))
		return
	}

	groups, err := artist.DetectDuplicates(req.Context(), r.db)
	if err != nil {
		r.logger.Error("detecting near-duplicate artists for next page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Drop server-side ignored groups (#2219) via the shared filter, matching
	// the stable page and the sidebar count exactly.
	ignored, err := artist.LoadIgnoredSignatures(req.Context(), r.db)
	if err != nil {
		r.logger.Error("loading ignored duplicate groups for next page", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	groups = artist.FilterIgnoredGroups(groups, ignored)

	view := buildArtistDuplicatesView(groups, r.lookupArticleMode(req))
	renderTempl(w, req, next.ArtistDuplicatesNextPage(r.assetsFor(req), view))
}
