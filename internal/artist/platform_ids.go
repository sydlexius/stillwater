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

// PlatformPresence indicates which platform types an artist has mappings for.
// Each field is true when at least one connection of that type has a platform ID
// for the artist.
type PlatformPresence struct {
	HasEmby     bool `json:"has_emby"`
	HasJellyfin bool `json:"has_jellyfin"`
	HasLidarr   bool `json:"has_lidarr"`
}
