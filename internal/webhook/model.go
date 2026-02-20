package webhook

import "time"

// Webhook represents a configured webhook endpoint.
type Webhook struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Type      string    `json:"type"`
	Events    []string  `json:"events"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Webhook types.
const (
	TypeGeneric = "generic"
	TypeDiscord = "discord"
	TypeSlack   = "slack"
	TypeGotify  = "gotify"
)
