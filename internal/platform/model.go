package platform

import (
	"encoding/json"
	"path/filepath"
	"strings"
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

// NamesForType returns the full filename slice for the given image type.
func (n ImageNaming) NamesForType(imageType string) []string {
	switch imageType {
	case "thumb":
		return n.Thumb
	case "fanart":
		return n.Fanart
	case "logo":
		return n.Logo
	case "banner":
		return n.Banner
	}
	return nil
}

// ValidateImageNaming checks all filenames in an ImageNaming struct and returns
// a list of human-readable error strings. Returns nil if all names are valid.
func ValidateImageNaming(n ImageNaming) []string {
	var errs []string

	validate := func(imageType string, names []string) {
		seen := make(map[string]bool, len(names))
		for _, name := range names {
			if name == "" {
				errs = append(errs, imageType+": empty filename")
				continue
			}
			if strings.ContainsAny(name, "/\\") {
				errs = append(errs, imageType+": filename contains path separator: "+name)
				continue
			}
			ext := strings.ToLower(filepath.Ext(name))
			if ext != ".jpg" && ext != ".png" {
				errs = append(errs, imageType+": extension must be .jpg or .png: "+name)
				continue
			}
			if imageType == "logo" && ext != ".png" {
				errs = append(errs, "logo: must use .png extension: "+name)
				continue
			}
			lower := strings.ToLower(name)
			if seen[lower] {
				errs = append(errs, imageType+": duplicate filename: "+name)
				continue
			}
			seen[lower] = true
		}
	}

	validate("thumb", n.Thumb)
	validate("fanart", n.Fanart)
	validate("logo", n.Logo)
	validate("banner", n.Banner)

	if len(errs) == 0 {
		return nil
	}
	return errs
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
