package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/web/templates/next"
)

// handleNextArtistsPage serves the next/ channel artists list (M55 #1335). When
// the resolved UI channel is "next" it renders the next.ArtistsPage shell; for
// an HTMX request it renders the next/ table/grid fragment. The toolbar's
// hx-get and the shared sort/selection JS resolve to /next/artists (channel-aware),
// so swaps render the next-specific table rather than the stable one (#1335).
//
// In stable mode (SW_UX=stable) the UX middleware 404s any /next/* request
// before this handler runs (decision 12 in architecture-decisions.md). The
// in-handler channel guard below is therefore only reachable when the lane IS
// enabled (next/dual mode) and the resolved channel is not "next" -- which
// happens when a user sent an explicit X-Stillwater-UX: stable header. In that
// case it delegates to the stable handleArtistsPage so the path never
// dead-ends. The data assembly is shared via buildArtistListData, so both
// channels render the identical data set.
//
// Unlike the generic /next/{path...} fallback, this screen has a dedicated
// stable handler, so the stable branch calls it directly rather than going
// through nextFallback (whose PathValue("path") lookup only resolves under the
// wildcard route, not this literal /next/artists registration).
func (r *Router) handleNextArtistsPage(w http.ResponseWriter, req *http.Request) {
	if middleware.UXChannelFromContext(req.Context()) != middleware.UXNext {
		r.handleArtistsPage(w, req)
		return
	}
	data, ok := r.buildArtistListData(w, req)
	if !ok {
		return
	}
	if isHTMXRequest(req) {
		if data.View == "grid" {
			renderTempl(w, req, next.ArtistsGrid(data))
		} else {
			renderTempl(w, req, next.ArtistsTable(data))
		}
		return
	}
	renderTempl(w, req, next.ArtistsPage(r.assetsFor(req), data))
}
