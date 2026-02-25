package artist

import (
	"encoding/json"
	"time"
)

// Artist represents a music artist or group with full metadata.
type Artist struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	SortName        string            `json:"sort_name"`
	Type            string            `json:"type"`
	Gender          string            `json:"gender"`
	Disambiguation  string            `json:"disambiguation"`
	MusicBrainzID   string            `json:"musicbrainz_id"`
	AudioDBID       string            `json:"audiodb_id"`
	DiscogsID       string            `json:"discogs_id"`
	WikidataID      string            `json:"wikidata_id"`
	Genres          []string          `json:"genres"`
	Styles          []string          `json:"styles"`
	Moods           []string          `json:"moods"`
	YearsActive     string            `json:"years_active"`
	Born            string            `json:"born"`
	Formed          string            `json:"formed"`
	Died            string            `json:"died"`
	Disbanded       string            `json:"disbanded"`
	Biography       string            `json:"biography"`
	Path            string            `json:"path"`
	NFOExists       bool              `json:"nfo_exists"`
	ThumbExists     bool              `json:"thumb_exists"`
	FanartExists    bool              `json:"fanart_exists"`
	LogoExists      bool              `json:"logo_exists"`
	BannerExists    bool              `json:"banner_exists"`
	ThumbLowRes     bool              `json:"thumb_low_res"`
	FanartLowRes    bool              `json:"fanart_low_res"`
	LogoLowRes      bool              `json:"logo_low_res"`
	BannerLowRes    bool              `json:"banner_low_res"`
	HealthScore     float64           `json:"health_score"`
	IsExcluded      bool              `json:"is_excluded"`
	ExclusionReason string            `json:"exclusion_reason,omitempty"`
	IsClassical     bool              `json:"is_classical"`
	AudioDBIDFetchedAt  *time.Time        `json:"audiodb_id_fetched_at,omitempty"`
	DiscogsIDFetchedAt  *time.Time        `json:"discogs_id_fetched_at,omitempty"`
	WikidataIDFetchedAt *time.Time        `json:"wikidata_id_fetched_at,omitempty"`
	LastFMIDFetchedAt   *time.Time        `json:"lastfm_id_fetched_at,omitempty"`
	MetadataSources     map[string]string `json:"metadata_sources,omitempty"`
	LastScannedAt       *time.Time        `json:"last_scanned_at,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
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
