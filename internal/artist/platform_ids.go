package artist

import (
	"errors"
	"time"
)

// ErrPlatformIDNotFound is returned when a platform ID mapping does not exist.
var ErrPlatformIDNotFound = errors.New("platform id not found")

// ErrPlatformIDClaimedByAnotherArtist is returned by SetPlatformID when the
// requested (connection_id, platform_artist_id) pair is already held by a
// different artist. Issue #1076 added a UNIQUE index on that pair so a
// platform item can only ever be claimed by a single Stillwater artist.
// Callers that prefer to no-op rather than fail (for example, the manual-
// library backfill helper) should match on this sentinel via errors.Is.
var ErrPlatformIDClaimedByAnotherArtist = errors.New("platform id already claimed by another artist")

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
	// HasFilesystem records membership in at least one filesystem-source
	// library (a libraries row whose connection_id is NULL). Replaces the
	// pre-#1004 path-presence heuristic in the artist list row, which
	// gave false positives for Emby/Jellyfin-only artists that happen to
	// carry an on-disk path written by the connection populate.
	HasFilesystem bool `json:"has_filesystem"`
}
