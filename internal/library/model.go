package library

import "time"

// Library type constants.
const (
	TypeRegular   = "regular"
	TypeClassical = "classical"
)

// Library source constants.
const (
	SourceManual   = "manual"
	SourceEmby     = "emby"
	SourceJellyfin = "jellyfin"
	SourceLidarr   = "lidarr"
)

// Library represents a music library directory with an associated type.
type Library struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	Type         string    `json:"type"`          // "regular" or "classical"
	Source       string    `json:"source"`        // "manual", "emby", "jellyfin", "lidarr"
	ConnectionID string    `json:"connection_id"` // FK to connections.id (empty for manual)
	ExternalID   string    `json:"external_id"`   // Platform-specific library ID
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// IsDegraded reports whether the library has no filesystem path configured.
// Degraded libraries support API-only operations; filesystem operations
// (image save, NFO restore) are unavailable.
func (lib Library) IsDegraded() bool {
	return lib.Path == ""
}

// SourceDisplayName returns a human-readable name for the library source.
// Returns an empty string for manual libraries.
func (lib Library) SourceDisplayName() string {
	switch lib.Source {
	case SourceEmby:
		return "Emby"
	case SourceJellyfin:
		return "Jellyfin"
	case SourceLidarr:
		return "Lidarr"
	default:
		return ""
	}
}
