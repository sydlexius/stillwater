package connection

import "context"

// ArtistPushData contains metadata fields to push to an external platform.
type ArtistPushData struct {
	Name           string   `json:"name"`
	SortName       string   `json:"sort_name"`
	Biography      string   `json:"biography"`
	Genres         []string `json:"genres"`
	Styles         []string `json:"styles"`
	Moods          []string `json:"moods"`
	Disambiguation string   `json:"disambiguation"`
	Born           string   `json:"born"`
	Formed         string   `json:"formed"`
	Died           string   `json:"died"`
	Disbanded      string   `json:"disbanded"`
	YearsActive    string   `json:"years_active"`
	MusicBrainzID  string   `json:"musicbrainz_id"`
}

// MetadataPusher pushes artist metadata to an external platform.
type MetadataPusher interface {
	PushMetadata(ctx context.Context, platformArtistID string, data ArtistPushData) error
}

// ImageUploader uploads images to an external platform.
type ImageUploader interface {
	UploadImage(ctx context.Context, platformArtistID string, imageType string, data []byte, contentType string) error
}
