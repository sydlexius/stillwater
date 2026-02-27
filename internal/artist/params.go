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
}

// Validate normalizes and validates list parameters.
func (p *ListParams) Validate() {
	if p.Page < 1 {
		p.Page = 1
	}
	if p.PageSize < 1 || p.PageSize > 200 {
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
