package connection

import (
	"fmt"
	"net/url"
	"time"
)

// Supported connection types.
const (
	TypeEmby     = "emby"
	TypeJellyfin = "jellyfin"
	TypeLidarr   = "lidarr"
)

// Connection represents an external service connection.
type Connection struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Type          string     `json:"type"`
	URL           string     `json:"url"`
	APIKey        string     `json:"api_key,omitempty"`
	Enabled       bool       `json:"enabled"`
	Status        string     `json:"status"`
	StatusMessage string     `json:"status_message,omitempty"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Validate checks required fields and constraints.
func (c *Connection) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !isValidType(c.Type) {
		return fmt.Errorf("type must be one of: emby, jellyfin, lidarr")
	}
	if c.URL == "" {
		return fmt.Errorf("url is required")
	}
	if _, err := url.ParseRequestURI(c.URL); err != nil {
		return fmt.Errorf("url is not valid: %w", err)
	}
	if c.APIKey == "" {
		return fmt.Errorf("api_key is required")
	}
	return nil
}

func isValidType(t string) bool {
	return t == TypeEmby || t == TypeJellyfin || t == TypeLidarr
}
