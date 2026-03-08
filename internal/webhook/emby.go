package webhook

import "strings"

// Emby webhook event types (Event field values confirmed via UAT against Emby 4.9).
// Emby uses dot-separated lowercase event names.
// TODO(UAT): confirm ItemUpdated and LibraryChanged event names from real events.
const (
	EmbyEventTest           = "system.notificationtest"
	EmbyEventLibraryChanged = "library.changed"
	EmbyEventItemAdded      = "library.new"
	EmbyEventItemUpdated    = "item.updated"
)

// embyMBIDAlbumArtistKey is the ProviderIds key for the MusicBrainz album artist ID.
// For multi-artist albums, multiple MBIDs are slash-separated.
// Confirmed via UAT against Emby 4.9.
const embyMBIDAlbumArtistKey = "MusicBrainzAlbumArtist"

// EmbyArtistItem is an artist reference within an Emby item payload.
type EmbyArtistItem struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

// EmbyItem is the item payload within an Emby webhook notification.
// Emby 4.9 sends MusicAlbum items (not MusicArtist) with artist info embedded.
type EmbyItem struct {
	ID          string            `json:"Id"`
	Name        string            `json:"Name"`
	Type        string            `json:"Type"` // "MusicAlbum"
	ProviderIds map[string]string `json:"ProviderIds"`
	Path        string            `json:"Path"`
	ArtistItems []EmbyArtistItem  `json:"ArtistItems"`
}

// ArtistMBIDs returns the MusicBrainz artist IDs from ProviderIds.
// Emby encodes multiple artists as slash-separated values.
func (i EmbyItem) ArtistMBIDs() []string {
	if i.ProviderIds == nil {
		return nil
	}
	raw := i.ProviderIds[embyMBIDAlbumArtistKey]
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "/")
}

// EmbyPayload is the inbound webhook payload from Emby 4.9.
// The event type is carried in the Event field (not NotificationType as in older docs).
type EmbyPayload struct {
	Event string    `json:"Event"`
	Item  *EmbyItem `json:"Item,omitempty"`
}
