// Package foreign detects, records, allowlists, and removes "foreign" image
// files in artist directories. A foreign file is any file matching one of the
// media-server image-naming patterns ("backdrop*", "fanart*", "poster*",
// "logo*", "banner*", "thumb*", "clearart*", "disc*", "landscape*") that
// LACKS a Stillwater EXIF provenance tag. Such files typically come from a
// peer media server (Emby, Jellyfin, Kodi) writing artwork to disk and are
// surfaced to the user so they can review, allowlist, or delete each one.
//
// The scanner only RECORDS to the foreign_files ledger; it never deletes
// files. Per-file deletion is user-triggered through the settings UI and
// uses internal/filesystem atomic operations (RemoveFileSafe) so partial
// removes never corrupt sibling artwork.
package foreign

import "time"

// Entry is one foreign file detected in an artist directory. The record is
// keyed by (artist_id, file_path) so re-detecting the same file is a no-op
// at the DB layer (UPSERT in repository.go).
type Entry struct {
	ID         string    `json:"id"`
	ArtistID   string    `json:"artist_id"`
	ArtistName string    `json:"artist_name,omitempty"`
	FilePath   string    `json:"file_path"`
	FileName   string    `json:"file_name"`
	SizeBytes  int64     `json:"size_bytes"`
	DetectedAt time.Time `json:"detected_at"`
}

// AllowlistScope identifies whether an allowlist row matches every artist
// (global) or a single artist (artist).
type AllowlistScope string

// Allowlist scope values.
const (
	ScopeGlobal AllowlistScope = "global"
	ScopeArtist AllowlistScope = "artist"
)

// AllowlistEntry is one row of the permanent re-detection suppression list.
// For ScopeGlobal entries ArtistID is empty and the row matches every
// artist; for ScopeArtist entries ArtistID is required and the row matches
// only that artist. FileName is the basename (e.g. "backdrop.jpg").
type AllowlistEntry struct {
	ID         string         `json:"id"`
	Scope      AllowlistScope `json:"scope"`
	ArtistID   string         `json:"artist_id,omitempty"`
	ArtistName string         `json:"artist_name,omitempty"`
	FileName   string         `json:"file_name"`
	Note       string         `json:"note,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// Summary aggregates current foreign-file ledger counts for the banner. A
// non-zero Count drives the slate/blue warning banner; a zero count is the
// normal "no foreign files" state and produces no banner of its own.
type Summary struct {
	Count       int       `json:"count"`
	GeneratedAt time.Time `json:"generated_at"`
}
