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

	// External provider IDs to publish into the platform's ProviderIds
	// dictionary. The platform push code maps these to its own canonical
	// dictionary keys (e.g. "MusicBrainzArtist", "TheAudioDb", "Discogs",
	// "Spotify"). Empty strings are not published.
	MusicBrainzID string `json:"musicbrainz_id"`
	AudioDBID     string `json:"audiodb_id"`
	DiscogsID     string `json:"discogs_id"`
	SpotifyID     string `json:"spotify_id"`

	// BandMembers carries the artist's member list in a platform-agnostic
	// shape so the push layer can map it into Jellyfin's People array. Empty
	// when the artist has no members or when the caller did not fetch them.
	BandMembers []ArtistPersonRef `json:"band_members,omitempty"`

	// LockSortName signals that Stillwater itself derived the SortName
	// value (currently: zero-padded numeric prefix for artists like
	// "12 Pebbles" whose canonical SortName from MusicBrainz was empty).
	// The flag is NOT set when the SortName came verbatim from upstream
	// metadata; locking those would override a user's manual unlock on
	// the platform side.
	//
	// **Honored by the Emby push only.** Emby's MetadataField enum
	// contains "SortName" and the platform clears ForcedSortName on the
	// next refresh unless that lock is set, so the Emby push fetches
	// the current LockedFields and appends "SortName" when this flag
	// is true (see emby/push.go fetchAndMergeLockedFields). Jellyfin's
	// MetadataField enum has no "SortName" member -- sending it returns
	// HTTP 400 and fails the entire push -- so the Jellyfin push path
	// intentionally ignores this flag. ForcedSortName persists across
	// Jellyfin's metadata refresh on its own without any lock. Future
	// platforms must explicitly opt in by reading this flag; the default
	// for any platform that does not consult it is "ignore", matching
	// today's Jellyfin behavior.
	LockSortName bool `json:"-"`
}

// ArtistPersonRef is a platform-agnostic representation of a band member
// suitable for translating into Jellyfin's People entries. Defined in the
// connection package (rather than embedding artist.BandMember directly) so
// the connection layer does not depend on the artist domain package.
type ArtistPersonRef struct {
	// Name is the member's display name.
	Name string `json:"name"`
	// Role is a short human-readable role string (e.g. "Guitarist",
	// "Vocals (Lead)"). Producers compose this from instruments + vocal
	// type. Optional.
	Role string `json:"role,omitempty"`
}

// MetadataPusher pushes artist metadata to an external platform.
type MetadataPusher interface {
	PushMetadata(ctx context.Context, platformArtistID string, data ArtistPushData) error
}

// LockSyncer updates the whole-item and per-field lock state for an artist on
// a platform without touching content metadata. Kept separate from
// MetadataPusher because lock changes are sensitive (toggling LockData via the
// generic metadata push can trigger Emby/Jellyfin to re-scrape and replace
// images) and must run on their own HTTP cycle.
type LockSyncer interface {
	UpdateArtistLocks(ctx context.Context, platformArtistID string, lockData bool, lockedFields []string) error
}

// ImageUploader uploads images to an external platform.
type ImageUploader interface {
	UploadImage(ctx context.Context, platformArtistID string, imageType string, data []byte, contentType string) error
}

// IndexedImageUploader uploads images at a specific index to an external platform.
// This is used for backdrop/fanart images where platforms support multiple images
// at numbered indices (e.g., Emby/Jellyfin Backdrop/0, Backdrop/1, etc.).
type IndexedImageUploader interface {
	UploadImageAtIndex(ctx context.Context, platformArtistID string, imageType string, index int, data []byte, contentType string) error
}

// ImageDeleter deletes images from an external platform.
type ImageDeleter interface {
	DeleteImage(ctx context.Context, platformArtistID string, imageType string) error
}
