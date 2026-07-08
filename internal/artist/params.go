package artist

// ListParams configures paginated, sortable, filterable artist queries.
type ListParams struct {
	Page           int
	PageSize       int
	Sort           string
	Order          string
	Search         string
	Filter         string
	LibraryID      string
	HealthScoreMin int // 0-100, only applied when > 0
	HealthScoreMax int // 0-100, only applied when > 0 and <= 100
	// Filters holds flyout-driven multi-filter state. Keys are filter names
	// and values are "include" or "exclude".
	//
	// Legacy / composite keys: missing_meta, missing_images, missing_mbid,
	// excluded, locked. Artist-type keys: type_person, type_group,
	// type_orchestra (orchestra + choir), type_other (the negation facet:
	// everything not in the named types, incl. untyped). Per-library keys:
	// library_{id}.
	//
	// Metadata-presence keys: has_biography, has_years_active, has_formed,
	// has_disbanded, has_born, has_died, has_gender, has_type, has_country,
	// has_genres, has_styles, has_moods, has_members, has_discography.
	// Image-presence keys: has_thumb, has_fanart, has_logo, has_banner.
	// Platform-membership keys: in_emby, in_jellyfin, has_lidarr.
	// Rule-violation key: has_violations.
	Filters map[string]string
	// IDs restricts the result set to a specific list of artist IDs. Used by
	// the bulk-selection "Show selected" affordance (#1227): when the user
	// toggles "show selected" the client posts the in-memory selection set
	// as a comma-separated query param so the user can paginate, view, and
	// act on the cross-page selection without losing any of it. Empty slice
	// means no restriction (the normal listing path).
	IDs []string
}

// CountParams configures filtered artist count queries. It mirrors the
// filtering subset of ListParams but omits pagination, sorting, and ordering
// since COUNT(*) does not need them.
type CountParams struct {
	Search         string
	Filter         string
	LibraryID      string
	HealthScoreMin int
	HealthScoreMax int
	Filters        map[string]string
	IDs            []string
}

// toListParams converts CountParams to ListParams so the shared
// buildWhereClause helper can be reused.
func (p CountParams) toListParams() ListParams {
	return ListParams{
		Page:           1,
		PageSize:       10, // unused but satisfies Validate
		Search:         p.Search,
		Filter:         p.Filter,
		LibraryID:      p.LibraryID,
		HealthScoreMin: p.HealthScoreMin,
		HealthScoreMax: p.HealthScoreMax,
		Filters:        p.Filters,
		IDs:            p.IDs,
	}
}

// MaxListIDs caps the number of artist IDs accepted by the IDs filter.
// This is the canonical cap: the API bulk-action cap
// (api.MaxBulkActionIDs) is sourced from this constant so the
// "Show selected" affordance and the bulk-action endpoint can never
// disagree on the in-memory selection store's hard limit.
const MaxListIDs = 1000

// Validate normalizes and validates list parameters.
func (p *ListParams) Validate() {
	if p.Page < 1 {
		p.Page = 1
	}
	if p.PageSize < 10 || p.PageSize > 500 {
		p.PageSize = 50
	}
	// Cap and de-empty the IDs filter. The handler should already have
	// dropped malformed input before this point; the cap is defense in
	// depth so a hand-crafted query string cannot exhaust SQLite's bound
	// parameter limit (default 32766) by passing an enormous list.
	if len(p.IDs) > MaxListIDs {
		p.IDs = p.IDs[:MaxListIDs]
	}
	if len(p.IDs) > 0 {
		// Drop empties AND deduplicate: ?ids=a,a,b would otherwise inflate
		// the "Showing N selected" chip count above the actual SQL IN-clause
		// match count and let the same artist consume two slots toward the
		// MaxListIDs cap. First-seen order is preserved so the chip text
		// stays stable across reloads.
		seen := make(map[string]struct{}, len(p.IDs))
		filtered := p.IDs[:0]
		for _, id := range p.IDs {
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			filtered = append(filtered, id)
		}
		p.IDs = filtered
	}
	switch p.Sort {
	case "name", "sort_name", "type", "origin", "health_score", "updated_at", "created_at",
		"nfo_exists", "thumb", "fanart", "logo", "mbid":
		// valid
	default:
		p.Sort = "name"
	}
	if p.Order != "desc" {
		p.Order = "asc"
	}
	if p.HealthScoreMin < 0 {
		p.HealthScoreMin = 0
	}
	if p.HealthScoreMin > 100 {
		p.HealthScoreMin = 100
	}
	if p.HealthScoreMax < 0 {
		p.HealthScoreMax = 0
	}
	if p.HealthScoreMax > 100 {
		p.HealthScoreMax = 100
	}
	if p.HealthScoreMin > 0 && p.HealthScoreMax > 0 && p.HealthScoreMin > p.HealthScoreMax {
		p.HealthScoreMin, p.HealthScoreMax = p.HealthScoreMax, p.HealthScoreMin
	}
}
