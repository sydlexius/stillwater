package artist

import "time"

// ProviderID represents a metadata provider ID associated with an artist.
// Provider IDs are stored in a normalized table (artist_provider_ids) rather
// than as columns on the artists table.
type ProviderID struct {
	Provider   string     `json:"provider"`
	ProviderID string     `json:"provider_id"`
	FetchedAt  *time.Time `json:"fetched_at,omitempty"`
}
