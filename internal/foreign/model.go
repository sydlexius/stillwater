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
// at the DB layer (UPSERT in repository.go). ContentHash is the lowercase
// hex sha256 of the file's bytes; allowlist matching keys on it so two
// distinct files sharing a basename like "poster.jpg" no longer collide.
// Rows recorded before migration 008 may carry an empty hash and are
// backfilled on the first post-migration scan.
//
// DuplicateCount is set by Repository.List to indicate how many ledger rows
// share this entry's content hash (always >=1; 1 means no duplicates).
// Rows with an empty content hash are never collapsed and always have
// DuplicateCount=1. Pre-008 rows therefore never over-collapse.
type Entry struct {
	ID             string    `json:"id"`
	ArtistID       string    `json:"artist_id"`
	ArtistName     string    `json:"artist_name,omitempty"`
	FilePath       string    `json:"file_path"`
	FileName       string    `json:"file_name"`
	ContentHash    string    `json:"content_hash"`
	SizeBytes      int64     `json:"size_bytes"`
	DetectedAt     time.Time `json:"detected_at"`
	DuplicateCount int       `json:"duplicate_count"`
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
// only that artist. ContentHash (lowercase hex sha256) is the dedupe key
// so two distinct files sharing a basename can each be allowlisted
// independently. FileName is preserved on the row for human readability in
// the UI but does not participate in matching.
type AllowlistEntry struct {
	ID          string         `json:"id"`
	Scope       AllowlistScope `json:"scope"`
	ArtistID    string         `json:"artist_id,omitempty"`
	ArtistName  string         `json:"artist_name,omitempty"`
	FileName    string         `json:"file_name"`
	ContentHash string         `json:"content_hash"`
	Note        string         `json:"note,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

// Summary aggregates current foreign-file ledger counts for the banner. A
// non-zero Count drives the slate/blue warning banner; a zero count is the
// normal "no foreign files" state and produces no banner of its own.
type Summary struct {
	Count       int       `json:"count"`
	GeneratedAt time.Time `json:"generated_at"`
}
