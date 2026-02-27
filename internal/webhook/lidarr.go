package webhook

// Lidarr webhook event types.
const (
	LidarrEventTest        = "Test"
	LidarrEventArtistAdd   = "ArtistAdded"
	LidarrEventGrab        = "Grab"
	LidarrEventDownload    = "Download"
	LidarrEventAlbumImport = "AlbumImport"
)

// LidarrPayload represents an inbound webhook payload from Lidarr.
type LidarrPayload struct {
	EventType string        `json:"eventType"`
	Artist    *LidarrArtist `json:"artist,omitempty"`
	Albums    []LidarrAlbum `json:"albums,omitempty"`
}

// LidarrArtist contains the artist data from a Lidarr webhook.
type LidarrArtist struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	Path            string `json:"path"`
	MBId            string `json:"mbId"`
	ForeignArtistID string `json:"foreignArtistId"`
}

// MBID returns the MusicBrainz artist ID, preferring MBId over ForeignArtistID.
func (a *LidarrArtist) MBID() string {
	if a.MBId != "" {
		return a.MBId
	}
	return a.ForeignArtistID
}

// LidarrAlbum contains album data from a Lidarr webhook.
type LidarrAlbum struct {
	ID             int    `json:"id"`
	Title          string `json:"title"`
	ForeignAlbumID string `json:"foreignAlbumId"`
}
