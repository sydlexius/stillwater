package webhook

// Emby webhook event types (NotificationType values).
// Note: EmbyEventTest uses lowercase per Emby's notification type naming convention,
// unlike Jellyfin which uses title case for its Test event.
const (
	EmbyEventTest           = "test"
	EmbyEventLibraryChanged = "LibraryChanged"
	EmbyEventItemAdded      = "ItemAdded"
	EmbyEventItemUpdated    = "ItemUpdated"
)

// embyMBIDArtistKey is the ProviderIds key for the MusicBrainz artist ID in Emby payloads.
// Emby has historically used both "MusicBrainzArtist" and "MusicBrainzArtistId"; the
// current Emby API uses "MusicBrainzArtist". Confirm against your Emby version during UAT.
const embyMBIDArtistKey = "MusicBrainzArtist"

// EmbyItem is the item payload within an Emby webhook notification.
type EmbyItem struct {
	ID          string            `json:"Id"`
	Name        string            `json:"Name"`
	Type        string            `json:"Type"`        // "MusicArtist"
	ProviderIds map[string]string `json:"ProviderIds"`
	Path        string            `json:"Path"`
}

// MBID returns the MusicBrainz artist ID from ProviderIds.
func (i EmbyItem) MBID() string {
	if i.ProviderIds == nil {
		return ""
	}
	return i.ProviderIds[embyMBIDArtistKey]
}

// EmbyPayload is the inbound webhook payload from Emby.
type EmbyPayload struct {
	NotificationType string    `json:"NotificationType"`
	Item             *EmbyItem `json:"Item,omitempty"`
}
