package api

import (
	"net/http"

	"github.com/sydlexius/stillwater/internal/dbutil"
)

// allowedArtistSort is the allowlist of public sort keys for artist list
// and search endpoints (handleListArtists, handleArtistsPage,
// handleListLockedArtists, compliance report). The map value is the
// canonical SQL column name passed downstream to artist.ListParams.Sort.
// The empty key is allowed and resolves to the per-handler default ("name").
var allowedArtistSort = map[string]string{
	"name":         "name",
	"sort_name":    "sort_name",
	"type":         "type",
	"origin":       "origin",
	"health_score": "health_score",
	"updated_at":   "updated_at",
	"created_at":   "created_at",
}

// allowedViolationSort is the allowlist for rule violation list/export
// endpoints. The empty key is allowed and resolves to the parser default
// ("severity"). Keys mirror the documented ViolationListParams.Sort values.
var allowedViolationSort = map[string]string{
	"artist_name": "artist_name",
	"severity":    "severity",
	"rule_id":     "rule_id",
	"created_at":  "created_at",
}

// allowedImageSearchSort is the allowlist for the in-memory sorter used by
// the artist image search endpoints. Empty key means "no sort"; the
// sortImageResults helper applies its built-in default in that case.
var allowedImageSearchSort = map[string]string{
	"likes":      "likes",
	"resolution": "resolution",
}

// validateSortParam reads the "sort" query parameter, validates it against
// the supplied allowlist, and on rejection writes a 400 response and
// returns ok=false. The canonical column is returned; on the empty-key
// path (column == "") the caller substitutes its own default before
// forwarding to the data layer.
func validateSortParam(w http.ResponseWriter, req *http.Request, allowed map[string]string) (column string, ok bool) {
	raw := req.URL.Query().Get("sort")
	col, valid := dbutil.ValidateSortKey(raw, allowed)
	if !valid {
		writeError(w, req, http.StatusBadRequest, "invalid sort key: "+raw)
		return "", false
	}
	return col, true
}

// validateOrderParam reads the "order" query parameter, accepts only
// "", "asc", "desc", and writes a 400 response on anything else.
func validateOrderParam(w http.ResponseWriter, req *http.Request) (order string, ok bool) {
	raw := req.URL.Query().Get("order")
	normalized, valid := dbutil.ValidateSortOrder(raw)
	if !valid {
		writeError(w, req, http.StatusBadRequest, "invalid order: "+raw)
		return "", false
	}
	return normalized, true
}
