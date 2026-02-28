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

// FSWatch mode constants (bitfield).
const (
	FSModeOff   = 0 // no monitoring
	FSModeWatch = 1 // fsnotify
	FSModePoll  = 2 // directory listing diff
	FSModeBoth  = 3 // watch + poll
)

// ValidPollIntervals lists the allowed poll interval values in seconds.
var ValidPollIntervals = []int{60, 300, 900, 1800}

// Library represents a music library directory with an associated type.
type Library struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Path              string    `json:"path"`
	Type              string    `json:"type"`                          // "regular" or "classical"
	Source            string    `json:"source"`                        // "manual", "emby", "jellyfin", "lidarr"
	ConnectionID      string    `json:"connection_id"`                 // FK to connections.id (empty for manual)
	ExternalID        string    `json:"external_id"`                   // Platform-specific library ID
	FSWatch           int       `json:"fs_watch"`                      // Bitfield: 0=off, 1=watch, 2=poll, 3=both
	FSPollInterval    int       `json:"fs_poll_interval"`              // Poll interval in seconds
	FSNotifySupported bool      `json:"fs_notify_supported,omitempty"` // Runtime-only, not stored in DB
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// FSWatchEnabled reports whether fsnotify watching is enabled.
func (lib Library) FSWatchEnabled() bool { return lib.FSWatch&FSModeWatch != 0 }

// FSPollEnabled reports whether polling is enabled.
func (lib Library) FSPollEnabled() bool { return lib.FSWatch&FSModePoll != 0 }

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

// IsValidPollInterval reports whether v is one of the allowed poll intervals.
func IsValidPollInterval(v int) bool {
	for _, valid := range ValidPollIntervals {
		if v == valid {
			return true
		}
	}
	return false
}
