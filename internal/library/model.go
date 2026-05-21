package library

import "time"

// Library type constants.
const (
	TypeRegular   = "regular"
	TypeClassical = "classical"
)

// SunsetClassicalType is the planned removal date for the Classical library
// type, expressed as an HTTP-date value for the Sunset response header
// (RFC 8594). The date is a placeholder until v1.3.0 has a firm release date;
// update this constant once the milestone is scheduled.
//
// This constant is intentionally used only for the Deprecation/Sunset headers
// on POST /api/v1/libraries and PATCH /api/v1/libraries/{id} responses that
// create or return a Classical library. Do not use it for any other purpose.
const SunsetClassicalType = "Sun, 01 Jun 2025 00:00:00 GMT"

// Library source constants.
const (
	SourceManual   = "manual"
	SourceEmby     = "emby"
	SourceJellyfin = "jellyfin"
	SourceLidarr   = "lidarr"
)

// FSWatch mode constants (bitfield).
const (
	FSModeOff   = 0 // no monitoring
	FSModeWatch = 1 // fsnotify
	FSModePoll  = 2 // directory listing diff
	FSModeBoth  = 3 // watch + poll
)

// SharedFS status constants. An empty string means unknown (not yet evaluated).
const (
	SharedFSNone      = "none"      // no shared filesystem detected
	SharedFSSuspected = "suspected" // evidence suggests shared filesystem
	SharedFSConfirmed = "confirmed" // confirmed shared filesystem
)

// ValidPollIntervals lists the allowed poll interval values in seconds.
var ValidPollIntervals = []int{60, 300, 900, 1800}

// Library represents a music library directory with an associated type.
type Library struct {
	ID                     string    `json:"id"`
	Name                   string    `json:"name"`
	Path                   string    `json:"path"`
	Type                   string    `json:"type"`                          // "regular" or "classical"
	Source                 string    `json:"source"`                        // "manual", "emby", "jellyfin", "lidarr"
	ConnectionID           string    `json:"connection_id"`                 // FK to connections.id (empty for manual)
	ExternalID             string    `json:"external_id"`                   // Platform-specific library ID
	FSWatch                int       `json:"fs_watch"`                      // Bitfield: 0=off, 1=watch, 2=poll, 3=both
	FSPollInterval         int       `json:"fs_poll_interval"`              // Poll interval in seconds
	SharedFSStatus         string    `json:"shared_fs_status"`              // "none", "suspected", "confirmed", "" (empty = unknown)
	SharedFSEvidence       string    `json:"shared_fs_evidence"`            // JSON array of evidence strings
	SharedFSPeerLibraryIDs string    `json:"shared_fs_peer_library_ids"`    // Comma-separated library IDs
	NFOLockData            bool      `json:"nfo_lock_data"`                 // When true, NFOs written for artists in this library carry <lockdata>true</lockdata>; opt-in, default false (issue #1264)
	FSNotifySupported      bool      `json:"fs_notify_supported,omitempty"` // Runtime-only, not stored in DB
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// FSWatchEnabled reports whether fsnotify watching is enabled.
func (lib Library) FSWatchEnabled() bool { return lib.FSWatch&FSModeWatch != 0 }

// FSPollEnabled reports whether polling is enabled.
func (lib Library) FSPollEnabled() bool { return lib.FSWatch&FSModePoll != 0 }

// IsClassical reports whether the library uses the deprecated Classical type.
func (lib Library) IsClassical() bool { return lib.Type == TypeClassical }

// IsPathless reports whether the library has no filesystem path configured.
// Pathless libraries support API-only operations; filesystem operations
// (image save, NFO write) are unavailable.
func (lib Library) IsPathless() bool {
	return lib.Path == ""
}

// IsSharedFS reports whether the library has a suspected or confirmed shared filesystem.
func (lib Library) IsSharedFS() bool {
	return lib.SharedFSStatus == SharedFSSuspected || lib.SharedFSStatus == SharedFSConfirmed
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

// IsValidPollInterval reports whether v is one of the allowed poll intervals.
func IsValidPollInterval(v int) bool {
	for _, valid := range ValidPollIntervals {
		if v == valid {
			return true
		}
	}
	return false
}
