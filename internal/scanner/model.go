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
	// SuspectedDuplicates counts newly-created artists whose normalized
	// identity key collided with a key already seen in this scan's preloaded
	// artist map.  A non-zero value indicates that the library contains
	// near-duplicate directories and the operator should review
	// /settings/artist-duplicates.  No persistent flag is stored; this is a
	// per-scan informational counter only.
	SuspectedDuplicates int    `json:"suspected_duplicates,omitempty"`
	Error               string `json:"error,omitempty"`
}
