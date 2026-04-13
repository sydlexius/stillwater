package artist

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

// ErrNotFound is returned by repository methods when an artist record does not exist.
var ErrNotFound = errors.New("artist not found")

// Artist represents a music artist or group with full metadata.
type Artist struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	SortName            string            `json:"sort_name"`
	Type                string            `json:"type"`
	Gender              string            `json:"gender"`
	Disambiguation      string            `json:"disambiguation"`
	MusicBrainzID       string            `json:"musicbrainz_id"`
	AudioDBID           string            `json:"audiodb_id"`
	DiscogsID           string            `json:"discogs_id"`
	WikidataID          string            `json:"wikidata_id"`
	DeezerID            string            `json:"deezer_id"`
	SpotifyID           string            `json:"spotify_id"`
	Genres              []string          `json:"genres"`
	Styles              []string          `json:"styles"`
	Moods               []string          `json:"moods"`
	YearsActive         string            `json:"years_active"`
	Born                string            `json:"born"`
	Formed              string            `json:"formed"`
	Died                string            `json:"died"`
	Disbanded           string            `json:"disbanded"`
	Biography           string            `json:"biography"`
	Path                string            `json:"path"`
	LibraryID           string            `json:"library_id"`
	NFOExists           bool              `json:"nfo_exists"`
	ThumbExists         bool              `json:"thumb_exists"`
	FanartExists        bool              `json:"fanart_exists"`
	FanartCount         int               `json:"fanart_count"`
	LogoExists          bool              `json:"logo_exists"`
	BannerExists        bool              `json:"banner_exists"`
	ThumbLowRes         bool              `json:"thumb_low_res"`
	FanartLowRes        bool              `json:"fanart_low_res"`
	LogoLowRes          bool              `json:"logo_low_res"`
	BannerLowRes        bool              `json:"banner_low_res"`
	ThumbPlaceholder    string            `json:"thumb_placeholder,omitempty"`
	FanartPlaceholder   string            `json:"fanart_placeholder,omitempty"`
	LogoPlaceholder     string            `json:"logo_placeholder,omitempty"`
	BannerPlaceholder   string            `json:"banner_placeholder,omitempty"`
	ThumbWidth          int               `json:"thumb_width,omitempty"`
	ThumbHeight         int               `json:"thumb_height,omitempty"`
	FanartWidth         int               `json:"fanart_width,omitempty"`
	FanartHeight        int               `json:"fanart_height,omitempty"`
	LogoWidth           int               `json:"logo_width,omitempty"`
	LogoHeight          int               `json:"logo_height,omitempty"`
	BannerWidth         int               `json:"banner_width,omitempty"`
	BannerHeight        int               `json:"banner_height,omitempty"`
	HealthScore         float64           `json:"health_score"`
	HealthEvaluatedAt   *time.Time        `json:"health_evaluated_at,omitempty"`
	IsExcluded          bool              `json:"is_excluded"`
	ExclusionReason     string            `json:"exclusion_reason,omitempty"`
	IsClassical         bool              `json:"is_classical"`
	Locked              bool              `json:"locked"`
	LockSource          string            `json:"lock_source,omitempty"`
	LockedAt            *time.Time        `json:"locked_at,omitempty"`
	AudioDBIDFetchedAt  *time.Time        `json:"audiodb_id_fetched_at,omitempty"`
	DiscogsIDFetchedAt  *time.Time        `json:"discogs_id_fetched_at,omitempty"`
	WikidataIDFetchedAt *time.Time        `json:"wikidata_id_fetched_at,omitempty"`
	LastFMFetchedAt     *time.Time        `json:"lastfm_fetched_at,omitempty"`
	MetadataSources     map[string]string `json:"metadata_sources,omitempty"`
	LastScannedAt       *time.Time        `json:"last_scanned_at,omitempty"`
	// Discography captures the artist's album entries parsed from the NFO.
	// This is a transient field populated on NFO read; it is not persisted
	// to the database in this release.
	Discography []DiscographyAlbum `json:"discography,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

// DiscographyAlbum is the artist-domain representation of a single NFO
// <album> entry. It mirrors nfo.DiscographyAlbum but lives in the artist
// package so callers outside the nfo package can reference it without
// pulling the XML model in.
type DiscographyAlbum struct {
	Title                     string `json:"title"`
	Year                      string `json:"year,omitempty"`
	MusicBrainzReleaseGroupID string `json:"musicbrainz_release_group_id,omitempty"`
}

// ProviderIDMap returns the artist's provider-specific IDs as a map keyed by
// provider name, suitable for passing to orchestrator FetchMetadata/FetchImages.
//
// All four providers are always included in the map. For FetchMetadata, an
// empty value causes fallback to MBID. For FetchImages, an empty value signals
// "skip this provider" (it cannot accept MBIDs).
func (a *Artist) ProviderIDMap() map[provider.ProviderName]string {
	return map[provider.ProviderName]string{
		provider.NameAudioDB: a.AudioDBID,
		provider.NameDiscogs: a.DiscogsID,
		provider.NameDeezer:  a.DeezerID,
		provider.NameSpotify: a.SpotifyID,
	}
}

// BandMember represents a member of a band or group.
type BandMember struct {
	ID               string    `json:"id"`
	ArtistID         string    `json:"artist_id"`
	MemberName       string    `json:"member_name"`
	MemberMBID       string    `json:"member_mbid,omitempty"`
	Instruments      []string  `json:"instruments"`
	VocalType        string    `json:"vocal_type,omitempty"`
	DateJoined       string    `json:"date_joined,omitempty"`
	DateLeft         string    `json:"date_left,omitempty"`
	IsOriginalMember bool      `json:"is_original_member"`
	SortOrder        int       `json:"sort_order"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// MarshalStringSlice encodes a string slice as a JSON array string.
func MarshalStringSlice(s []string) string {
	if s == nil {
		return "[]"
	}
	data, _ := json.Marshal(s)
	return string(data)
}

// UnmarshalStringSlice decodes a JSON array string into a string slice.
func UnmarshalStringSlice(data string) []string {
	if data == "" || data == "[]" {
		return nil
	}
	var result []string
	if err := json.Unmarshal([]byte(data), &result); err != nil {
		return nil
	}
	return result
}

// MarshalStringMap encodes a string map as a JSON object string.
func MarshalStringMap(m map[string]string) string {
	if m == nil {
		return "{}"
	}
	data, _ := json.Marshal(m)
	return string(data)
}

// UnmarshalStringMap decodes a JSON object string into a string map.
func UnmarshalStringMap(data string) map[string]string {
	if data == "" || data == "{}" {
		return nil
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(data), &result); err != nil {
		return nil
	}
	return result
}
