package provider

import (
	"context"
	"fmt"
	"time"
)

// AccessTier classifies a provider's access model.
type AccessTier string

// Access tier constants for classifying a provider's access model.
const (
	TierFree     AccessTier = "free"     // No key, no limit known
	TierFreeKey  AccessTier = "free_key" // Free account/sign-up required
	TierFreemium AccessTier = "freemium" // Free tier with quota, paid for more
	TierPaid     AccessTier = "paid"     // Paid access only
)

// RateLimitInfo documents the known rate limits for a provider.
type RateLimitInfo struct {
	RequestsPerSecond float64    `json:"requests_per_second,omitempty"`
	RequestsPerDay    int        `json:"requests_per_day,omitempty"`   // 0 = unknown/unlimited
	RequestsPerMonth  int        `json:"requests_per_month,omitempty"` // 0 = unknown/unlimited
	ResetAt           *time.Time `json:"reset_at,omitempty"`
}

// ProviderCapability describes a provider's access model and documented rate limits.
type ProviderCapability struct {
	Tier      AccessTier     `json:"tier"`
	HelpURL   string         `json:"help_url,omitempty"`
	RateLimit *RateLimitInfo `json:"rate_limit,omitempty"`
}

// ProviderCapabilities returns the known capability metadata for each provider.
func ProviderCapabilities() map[ProviderName]ProviderCapability {
	return map[ProviderName]ProviderCapability{
		NameMusicBrainz: {
			Tier:      TierFree,
			RateLimit: &RateLimitInfo{RequestsPerSecond: 1},
		},
		NameFanartTV: {
			Tier:      TierFreeKey,
			HelpURL:   "https://fanart.tv/get-an-api-key/",
			RateLimit: &RateLimitInfo{RequestsPerSecond: 3},
		},
		NameAudioDB: {
			Tier:      TierFreemium,
			HelpURL:   "https://www.patreon.com/thedatadb",
			RateLimit: &RateLimitInfo{RequestsPerSecond: 2},
		},
		NameDiscogs: {
			Tier:      TierFreeKey,
			HelpURL:   "https://www.discogs.com/settings/developers",
			RateLimit: &RateLimitInfo{RequestsPerSecond: 1, RequestsPerDay: 1000},
		},
		NameLastFM: {
			Tier:      TierFreeKey,
			HelpURL:   "https://www.last.fm/api/account/create",
			RateLimit: &RateLimitInfo{RequestsPerSecond: 5},
		},
		NameWikidata: {
			Tier:      TierFree,
			RateLimit: &RateLimitInfo{RequestsPerSecond: 5},
		},
		NameDuckDuckGo: {
			Tier:      TierFree,
			RateLimit: &RateLimitInfo{RequestsPerSecond: 1},
		},
		NameDeezer: {
			Tier:      TierFree,
			RateLimit: &RateLimitInfo{RequestsPerSecond: 5},
		},
	}
}

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
	NameDuckDuckGo  ProviderName = "duckduckgo"
	NameDeezer      ProviderName = "deezer"
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
		NameDeezer,
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
	case NameDuckDuckGo:
		return "DuckDuckGo"
	case NameDeezer:
		return "Deezer"
	default:
		return string(n)
	}
}

// AllWebSearchProviderNames returns all known web search provider names in display order.
func AllWebSearchProviderNames() []ProviderName {
	return []ProviderName{NameDuckDuckGo}
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

// WebImageProvider is the interface for web-based image search adapters.
// Unlike Provider (which uses provider-specific IDs like MBIDs), web image
// providers search by artist name and image type.
type WebImageProvider interface {
	Name() ProviderName
	RequiresAuth() bool
	SearchImages(ctx context.Context, artistName string, imageType ImageType) ([]ImageResult, error)
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
