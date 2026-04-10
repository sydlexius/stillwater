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
	}
}

// Validate normalizes and validates list parameters.
func (p *ListParams) Validate() {
	if p.Page < 1 {
		p.Page = 1
	}
	if p.PageSize < 10 || p.PageSize > 500 {
		p.PageSize = 50
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
