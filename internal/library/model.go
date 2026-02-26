package library

import "time"

// Library type constants.
const (
	TypeRegular   = "regular"
	TypeClassical = "classical"
)

// Library represents a music library directory with an associated type.
type Library struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Type      string    `json:"type"` // "regular" or "classical"
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IsDegraded reports whether the library has no filesystem path configured.
// Degraded libraries support API-only operations; filesystem operations
// (image save, NFO restore) are unavailable.
func (lib Library) IsDegraded() bool {
	return lib.Path == ""
}
