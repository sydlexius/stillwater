package artist

import (
	"errors"
	"time"
)

// ErrPlatformIDNotFound is returned when a platform ID mapping does not exist.
var ErrPlatformIDNotFound = errors.New("platform id not found")

// PlatformID maps a Stillwater artist to their ID on a specific platform connection.
type PlatformID struct {
	ArtistID         string    `json:"artist_id"`
	ConnectionID     string    `json:"connection_id"`
	PlatformArtistID string    `json:"platform_artist_id"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}
