package webhook

// Jellyfin webhook event types (NotificationType values from jellyfin-plugin-webhook).
const (
	JellyfinEventTest           = "Test"
	JellyfinEventItemAdded      = "ItemAdded"
	JellyfinEventItemUpdated    = "ItemUpdated"
	JellyfinEventLibraryChanged = "LibraryChanged"
)

// JellyfinPayload is the inbound webhook payload from the Jellyfin webhook plugin.
// Provider IDs are flattened as Provider_{name} fields rather than nested maps.
type JellyfinPayload struct {
	NotificationType          string `json:"NotificationType"`
	ItemId                    string `json:"ItemId"`
	ItemType                  string `json:"ItemType"` // "MusicArtist"
	Name                      string `json:"Name"`
	ProviderMusicBrainzArtist string `json:"Provider_musicbrainzartist"`
}

// MBID returns the MusicBrainz artist ID from the payload.
func (p *JellyfinPayload) MBID() string {
	return p.ProviderMusicBrainzArtist
}
