package artist

import "time"

// Alias represents an alternative name for an artist.
type Alias struct {
	ID        string    `json:"id"`
	ArtistID  string    `json:"artist_id"`
	Alias     string    `json:"alias"`
	Source    string    `json:"source,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// DuplicateGroup represents a set of artists that may be duplicates.
type DuplicateGroup struct {
	Artists []Artist `json:"artists"`
	Reason  string   `json:"reason"`
}
