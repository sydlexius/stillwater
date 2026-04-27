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
	// and values are "include" or "exclude". Single-value keys: missing_meta,
	// missing_images, missing_mbid, excluded, locked, type_person, type_group,
	// type_orchestra. Per-library keys: library_{id}.
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
// Matches the bulk-action cap (handlers_bulk_actions.MaxBulkActionIDs) so
// the "Show selected" affordance can never request more rows than the
// in-memory selection store is allowed to hold.
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
		filtered := p.IDs[:0]
		for _, id := range p.IDs {
			if id != "" {
				filtered = append(filtered, id)
			}
		}
		p.IDs = filtered
	}
	switch p.Sort {
	case "name", "sort_name", "health_score", "updated_at", "created_at":
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
