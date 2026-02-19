package scanner

import "time"

// ScanResult summarizes the outcome of a filesystem scan.
type ScanResult struct {
	ID               string     `json:"id"`
	Status           string     `json:"status"` // "running", "completed", "failed"
	StartedAt        time.Time  `json:"started_at"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	NewArtists       int        `json:"new_artists"`
	UpdatedArtists   int        `json:"updated_artists"`
	RemovedArtists   int        `json:"removed_artists"`
	TotalDirectories int        `json:"total_directories"`
	Error            string     `json:"error,omitempty"`
}
