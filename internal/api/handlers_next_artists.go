package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/web/templates/next"
)

// handleNextArtistsPage serves the next/ channel artists list (M55 #1335). When
// the resolved UI channel is "next" it renders the next.ArtistsPage shell; for
// an HTMX request it renders the next/ table/grid fragment. The toolbar's
// hx-get and the shared sort/selection JS resolve to /next/artists
// (channel-aware), so swaps render the next-specific table rather than the
// stable one (#1335).
//
// In stable mode (SW_UX=stable) the UX middleware 404s any /next/* request
// before this handler runs (decision 12 in architecture-decisions.md). The
// in-handler channel guard below is therefore only reachable when the lane IS
// enabled (next/dual mode) and the resolved channel is not "next" -- which
// happens when a user sends an explicit X-Stillwater-UX: stable header. In that
// case it returns 404 (decision 12: all handleNext* handlers return 404 on an
// explicit /next/ path with the stable opt-out; the path does not serve stable
// content). The data assembly is shared via buildArtistListData, so the next/
// table/grid uses the same data set as the stable list.
func (r *Router) handleNextArtistsPage(w http.ResponseWriter, req *http.Request) {
	if !checkNextChannel(w, req) {
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
