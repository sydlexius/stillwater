package webhook

// Jellyfin webhook event types (NotificationType values from jellyfin-plugin-webhook).
// Confirmed via UAT: ItemAdded fires for MusicAlbum items (not MusicArtist).
// TODO(UAT): confirm ItemUpdated and LibraryChanged event names from real events.
const (
	JellyfinEventTest           = "Test"
	JellyfinEventItemAdded      = "ItemAdded"
	JellyfinEventItemUpdated    = "ItemUpdated"
	JellyfinEventLibraryChanged = "LibraryChanged"
)

// JellyfinPayload is the inbound webhook payload from the Jellyfin webhook plugin.
// Provider IDs are flattened as Provider_{name} fields rather than nested maps.
// Jellyfin sends MusicAlbum items (not MusicArtist) with artist info embedded.
// Note: for multi-artist albums, only the first artist's MBID is included.
type JellyfinPayload struct {
	NotificationType              string `json:"NotificationType"`
	ItemID                        string `json:"ItemId"`
	ItemType                      string `json:"ItemType"` // "MusicAlbum"
	Name                          string `json:"Name"`
	Artist                        string `json:"Artist"`
	ProviderMusicBrainzAlbumArtist string `json:"Provider_musicbrainzalbumartist"`
}

// MBID returns the MusicBrainz album artist ID from the payload.
// For multi-artist albums, only the first artist's MBID is returned.
func (p JellyfinPayload) MBID() string {
	return p.ProviderMusicBrainzAlbumArtist
}
