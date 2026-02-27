package rule

import "time"

// Bulk operation modes control how matches are resolved.
const (
	BulkModeYOLO          = "yolo"            // Auto-accept best match (confidence >= 0.70)
	BulkModePromptNoMatch = "prompt_no_match" // Auto-accept if match, skip on no-match
	BulkModeDisambiguate  = "disambiguate"    // Skip when multiple equally-scored matches
	BulkModeManual        = "manual"          // Skip all, mark for manual review
)

// Bulk job types.
const (
	BulkTypeFetchMetadata = "fetch_metadata"
	BulkTypeFetchImages   = "fetch_images"
)

// Bulk job statuses.
const (
	BulkStatusPending   = "pending"
	BulkStatusRunning   = "running"
	BulkStatusCompleted = "completed"
	BulkStatusCanceled  = "canceled"
	BulkStatusFailed    = "failed"
)

// Bulk job item statuses.
const (
	BulkItemPending = "pending"
	BulkItemFixed   = "fixed"
	BulkItemSkipped = "skipped"
	BulkItemFailed  = "failed"
)

// BulkJob represents a background bulk operation.
type BulkJob struct {
	ID             string     `json:"id"`
	Type           string     `json:"type"`
	Mode           string     `json:"mode"`
	Status         string     `json:"status"`
	TotalItems     int        `json:"total_items"`
	ProcessedItems int        `json:"processed_items"`
	FixedItems     int        `json:"fixed_items"`
	SkippedItems   int        `json:"skipped_items"`
	FailedItems    int        `json:"failed_items"`
	Error          string     `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`

	// ArtistIDs limits the job to specific artists. Transient (not persisted).
	// When empty, the executor targets all non-excluded artists.
	ArtistIDs []string `json:"-"`
}

// BulkJobItem tracks the result for a single artist within a bulk job.
type BulkJobItem struct {
	ID         string    `json:"id"`
	JobID      string    `json:"job_id"`
	ArtistID   string    `json:"artist_id"`
	ArtistName string    `json:"artist_name"`
	Status     string    `json:"status"`
	Message    string    `json:"message,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// BulkRequest is the API request body for starting a bulk operation.
type BulkRequest struct {
	Mode      string   `json:"mode"`
	ArtistIDs []string `json:"artist_ids,omitempty"`
}
