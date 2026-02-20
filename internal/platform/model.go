package platform

import (
	"encoding/json"
	"time"
)

// Profile represents a platform configuration profile.
type Profile struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	IsBuiltin   bool        `json:"is_builtin"`
	IsActive    bool        `json:"is_active"`
	NFOEnabled  bool        `json:"nfo_enabled"`
	NFOFormat   string      `json:"nfo_format"`
	ImageNaming ImageNaming `json:"image_naming"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// ImageNaming maps image types to their filename patterns.
// Each type supports multiple filenames (e.g., folder.jpg + artist.jpg for thumb).
type ImageNaming struct {
	Thumb  []string `json:"thumb"`
	Fanart []string `json:"fanart"`
	Logo   []string `json:"logo"`
	Banner []string `json:"banner"`
}

// ToMap converts ImageNaming to a map for use by the image package.
func (n ImageNaming) ToMap() map[string][]string {
	return map[string][]string{
		"thumb":  n.Thumb,
		"fanart": n.Fanart,
		"logo":   n.Logo,
		"banner": n.Banner,
	}
}

// PrimaryName returns the first filename for the given type, or empty string.
func (n ImageNaming) PrimaryName(imageType string) string {
	switch imageType {
	case "thumb":
		if len(n.Thumb) > 0 {
			return n.Thumb[0]
		}
	case "fanart":
		if len(n.Fanart) > 0 {
			return n.Fanart[0]
		}
	case "logo":
		if len(n.Logo) > 0 {
			return n.Logo[0]
		}
	case "banner":
		if len(n.Banner) > 0 {
			return n.Banner[0]
		}
	}
	return ""
}

// MarshalImageNaming serializes ImageNaming to a JSON string.
func MarshalImageNaming(n ImageNaming) string {
	data, err := json.Marshal(n)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// UnmarshalImageNaming deserializes a JSON string into ImageNaming.
// Handles both the legacy single-string format and the new array format.
func UnmarshalImageNaming(data string) ImageNaming {
	// Try new array format first
	var n ImageNaming
	if err := json.Unmarshal([]byte(data), &n); err == nil {
		return n
	}

	// Fall back to legacy single-string format
	var legacy struct {
		Thumb  string `json:"thumb"`
		Fanart string `json:"fanart"`
		Logo   string `json:"logo"`
		Banner string `json:"banner"`
	}
	if err := json.Unmarshal([]byte(data), &legacy); err != nil {
		return ImageNaming{}
	}

	result := ImageNaming{}
	if legacy.Thumb != "" {
		result.Thumb = []string{legacy.Thumb}
	}
	if legacy.Fanart != "" {
		result.Fanart = []string{legacy.Fanart}
	}
	if legacy.Logo != "" {
		result.Logo = []string{legacy.Logo}
	}
	if legacy.Banner != "" {
		result.Banner = []string{legacy.Banner}
	}
	return result
}
