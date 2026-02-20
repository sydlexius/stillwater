package nfo

import (
	"os"
	"time"
)

// ConflictCheck describes the result of checking for NFO write conflicts.
type ConflictCheck struct {
	HasConflict    bool       `json:"has_conflict"`
	Reason         string     `json:"reason,omitempty"`
	LastModified   *time.Time `json:"last_modified,omitempty"`
	ExternalWriter string     `json:"external_writer,omitempty"`
}

// CheckFileConflict checks if an NFO file has been modified externally
// since the given reference time. If the file does not exist, there is no conflict.
func CheckFileConflict(nfoPath string, since time.Time) *ConflictCheck {
	info, err := os.Stat(nfoPath)
	if err != nil {
		return &ConflictCheck{HasConflict: false}
	}

	modTime := info.ModTime()
	if modTime.After(since) {
		return &ConflictCheck{
			HasConflict:  true,
			Reason:       "file modified externally since last write",
			LastModified: &modTime,
		}
	}

	return &ConflictCheck{
		HasConflict:  false,
		LastModified: &modTime,
	}
}
