package dbutil

// ValidateSortKey checks a user-supplied sort key against a per-endpoint
// allowlist of canonical column names. It returns the canonical SQL column
// (the map value) when the key is allowed, false otherwise.
//
// An empty key returns the empty string and ok=true so handlers can preserve
// their existing default-when-empty behavior (the caller substitutes its own
// default before passing the result into a query). The allowlist values are
// always literal column expressions controlled by the caller, never user
// input, so the returned string is safe to interpolate into ORDER BY.
//
// Typical use at an HTTP boundary:
//
//	var allowedArtistSort = map[string]string{
//	    "name":         "name",
//	    "sort_name":    "sort_name",
//	    "health_score": "health_score",
//	    "updated_at":   "updated_at",
//	    "created_at":   "created_at",
//	}
//
//	col, ok := dbutil.ValidateSortKey(req.URL.Query().Get("sort"), allowedArtistSort)
//	if !ok {
//	    // 400 Bad Request with structured error JSON
//	}
//
// Empty-string allowlist values are permitted (e.g. for in-memory sorters
// that distinguish "no sort" from a named sort).
func ValidateSortKey(key string, allowed map[string]string) (column string, ok bool) {
	if key == "" {
		return "", true
	}
	col, found := allowed[key]
	if !found {
		return "", false
	}
	return col, true
}

// ValidateSortOrder checks a user-supplied order string against the fixed
// {"", "asc", "desc"} set. Empty input returns ok=true with the empty string
// so the caller can substitute its own default. Any other value returns ok
// false so the handler can emit a 400.
func ValidateSortOrder(order string) (normalized string, ok bool) {
	switch order {
	case "":
		return "", true
	case "asc", "desc":
		return order, true
	default:
		return "", false
	}
}
