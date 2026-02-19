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
type ImageNaming struct {
	Thumb  string `json:"thumb"`
	Fanart string `json:"fanart"`
	Logo   string `json:"logo"`
	Banner string `json:"banner"`
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
func UnmarshalImageNaming(data string) ImageNaming {
	var n ImageNaming
	if err := json.Unmarshal([]byte(data), &n); err != nil {
		return ImageNaming{}
	}
	return n
}
