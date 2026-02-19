package artist

// ListParams configures paginated, sortable, filterable artist queries.
type ListParams struct {
	Page     int
	PageSize int
	Sort     string
	Order    string
	Search   string
	Filter   string
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
}
