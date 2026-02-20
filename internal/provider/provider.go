package provider

import (
	"context"
	"fmt"
	"time"
)

// ProviderName uniquely identifies a metadata provider.
type ProviderName string

// Known provider names.
const (
	NameMusicBrainz ProviderName = "musicbrainz"
	NameFanartTV    ProviderName = "fanarttv"
	NameAudioDB     ProviderName = "audiodb"
	NameDiscogs     ProviderName = "discogs"
	NameLastFM      ProviderName = "lastfm"
	NameWikidata    ProviderName = "wikidata"
)

// AllProviderNames returns all known provider names in display order.
func AllProviderNames() []ProviderName {
	return []ProviderName{
		NameMusicBrainz,
		NameFanartTV,
		NameAudioDB,
		NameDiscogs,
		NameLastFM,
		NameWikidata,
	}
}

// DisplayName returns a human-readable name for the provider.
func (n ProviderName) DisplayName() string {
	switch n {
	case NameMusicBrainz:
		return "MusicBrainz"
	case NameFanartTV:
		return "Fanart.tv"
	case NameAudioDB:
		return "TheAudioDB"
	case NameDiscogs:
		return "Discogs"
	case NameLastFM:
		return "Last.fm"
	case NameWikidata:
		return "Wikidata"
	default:
		return string(n)
	}
}

// ImageType classifies the kind of artist image.
type ImageType string

// Known image types.
const (
	ImageThumb      ImageType = "thumb"
	ImageFanart     ImageType = "fanart"
	ImageLogo       ImageType = "logo"
	ImageHDLogo     ImageType = "hdlogo"
	ImageBanner     ImageType = "banner"
	ImageBackground ImageType = "background"
	ImageWideThumb  ImageType = "widethumb"
)

// ArtistSearchResult represents a single search hit from a provider.
type ArtistSearchResult struct {
	ProviderID     string `json:"provider_id"`
	Name           string `json:"name"`
	SortName       string `json:"sort_name,omitempty"`
	Type           string `json:"type,omitempty"`
	Disambiguation string `json:"disambiguation,omitempty"`
	Country        string `json:"country,omitempty"`
	Score          int    `json:"score"`
	MusicBrainzID  string `json:"musicbrainz_id,omitempty"`
	Source         string `json:"source"`
}

// ArtistMetadata is the full metadata a provider returns for a single artist.
type ArtistMetadata struct {
	ProviderID     string            `json:"provider_id"`
	MusicBrainzID  string            `json:"musicbrainz_id,omitempty"`
	AudioDBID      string            `json:"audiodb_id,omitempty"`
	DiscogsID      string            `json:"discogs_id,omitempty"`
	WikidataID     string            `json:"wikidata_id,omitempty"`
	Name           string            `json:"name"`
	SortName       string            `json:"sort_name,omitempty"`
	Type           string            `json:"type,omitempty"`
	Gender         string            `json:"gender,omitempty"`
	Disambiguation string            `json:"disambiguation,omitempty"`
	Country        string            `json:"country,omitempty"`
	Biography      string            `json:"biography,omitempty"`
	Genres         []string          `json:"genres,omitempty"`
	Styles         []string          `json:"styles,omitempty"`
	Moods          []string          `json:"moods,omitempty"`
	YearsActive    string            `json:"years_active,omitempty"`
	Born           string            `json:"born,omitempty"`
	Formed         string            `json:"formed,omitempty"`
	Died           string            `json:"died,omitempty"`
	Disbanded      string            `json:"disbanded,omitempty"`
	Members        []MemberInfo      `json:"members,omitempty"`
	SimilarArtists []string          `json:"similar_artists,omitempty"`
	Aliases        []string          `json:"aliases,omitempty"`
	URLs           map[string]string `json:"urls,omitempty"`
}

// MemberInfo is a band member as reported by a provider.
type MemberInfo struct {
	Name        string   `json:"name"`
	MBID        string   `json:"mbid,omitempty"`
	Instruments []string `json:"instruments,omitempty"`
	VocalType   string   `json:"vocal_type,omitempty"`
	DateJoined  string   `json:"date_joined,omitempty"`
	DateLeft    string   `json:"date_left,omitempty"`
	IsActive    bool     `json:"is_active"`
}

// ImageResult represents a single image available from a provider.
type ImageResult struct {
	URL      string    `json:"url"`
	Type     ImageType `json:"type"`
	Likes    int       `json:"likes,omitempty"`
	Width    int       `json:"width,omitempty"`
	Height   int       `json:"height,omitempty"`
	Language string    `json:"language,omitempty"`
	Source   string    `json:"source"`
}

// Provider is the interface all metadata source adapters must implement.
type Provider interface {
	// Name returns the unique provider identifier.
	Name() ProviderName

	// RequiresAuth returns true if this provider needs an API key to function.
	RequiresAuth() bool

	// SearchArtist searches the provider by name. Returns zero or more results.
	SearchArtist(ctx context.Context, name string) ([]ArtistSearchResult, error)

	// GetArtist fetches full metadata for an artist by the provider's own ID.
	GetArtist(ctx context.Context, id string) (*ArtistMetadata, error)

	// GetImages fetches available images for an artist by the provider's own ID.
	// Providers that do not serve images return nil, nil.
	GetImages(ctx context.Context, id string) ([]ImageResult, error)
}

// TestableProvider is an optional interface providers can implement
// for the "test connection" button in the settings UI.
type TestableProvider interface {
	Provider
	TestConnection(ctx context.Context) error
}

// ErrProviderUnavailable indicates a transient failure (rate-limited, timeout, server error).
type ErrProviderUnavailable struct {
	Provider   ProviderName
	Cause      error
	RetryAfter time.Duration
}

func (e *ErrProviderUnavailable) Error() string {
	return fmt.Sprintf("provider %s unavailable: %v", e.Provider, e.Cause)
}

func (e *ErrProviderUnavailable) Unwrap() error { return e.Cause }

// ErrNotFound indicates the provider has no data for the requested ID.
type ErrNotFound struct {
	Provider ProviderName
	ID       string
}

func (e *ErrNotFound) Error() string {
	return fmt.Sprintf("provider %s: artist %s not found", e.Provider, e.ID)
}

// ErrAuthRequired indicates the provider needs an API key but none is configured.
type ErrAuthRequired struct {
	Provider ProviderName
}

func (e *ErrAuthRequired) Error() string {
	return fmt.Sprintf("provider %s: API key not configured", e.Provider)
}
