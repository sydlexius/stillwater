package connection

import "context"

// ArtistPushData contains metadata fields to push to an external platform.
type ArtistPushData struct {
	Name      string   `json:"name"`
	SortName  string   `json:"sort_name"`
	Biography string   `json:"biography"`
	Genres    []string `json:"genres"`
}

// MetadataPusher pushes artist metadata to an external platform.
type MetadataPusher interface {
	PushMetadata(ctx context.Context, platformArtistID string, data ArtistPushData) error
}
