package rule

import "time"

const (
	// BulkModeYOLO auto-accepts the best match when confidence >= 0.70.
	BulkModeYOLO = "yolo"
	// BulkModePromptNoMatch auto-accepts on match but skips when no match is found.
	BulkModePromptNoMatch = "prompt_no_match"
	// BulkModeDisambiguate skips when multiple equally-scored matches exist.
	BulkModeDisambiguate = "disambiguate"
	// BulkModeManual skips all matches and marks them for manual review.
	BulkModeManual = "manual"
)

const (
	// BulkTypeFetchMetadata is a bulk job that fetches metadata from providers.
	BulkTypeFetchMetadata = "fetch_metadata"
	// BulkTypeFetchImages is a bulk job that fetches missing images from providers.
	BulkTypeFetchImages = "fetch_images"
)

const (
	// BulkStatusPending indicates the job is queued but not yet started.
	BulkStatusPending = "pending"
	// BulkStatusRunning indicates the job is currently executing.
	BulkStatusRunning = "running"
	// BulkStatusCompleted indicates the job finished successfully.
	BulkStatusCompleted = "completed"
	// BulkStatusCanceled indicates the job was canceled by the user.
	BulkStatusCanceled = "canceled"
	// BulkStatusFailed indicates the job terminated due to an error.
	BulkStatusFailed = "failed"
)

const (
	// BulkItemPending indicates the item has not been processed yet.
	BulkItemPending = "pending"
	// BulkItemFixed indicates the item was successfully resolved.
	BulkItemFixed = "fixed"
	// BulkItemSkipped indicates the item was skipped (no match or manual mode).
	BulkItemSkipped = "skipped"
	// BulkItemFailed indicates the item encountered an error during processing.
	BulkItemFailed = "failed"
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
