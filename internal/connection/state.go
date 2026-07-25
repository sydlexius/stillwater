package connection

import "context"

// ArtistPlatformState holds the current state of an artist on a platform.
type ArtistPlatformState struct {
	Name          string
	SortName      string
	Biography     string
	Genres        []string
	Tags          []string
	PremiereDate  string
	EndDate       string
	MusicBrainzID string
	HasThumb      bool
	HasFanart     bool
	HasLogo       bool
	HasBanner     bool
	BackdropCount int
	IsLocked      bool
	LockedFields  []string
}

// ArtistStateGetter fetches the current state of an artist from a platform.
type ArtistStateGetter interface {
	GetArtistDetail(ctx context.Context, platformArtistID string) (*ArtistPlatformState, error)
}

// ImageFetcherWarning describes a platform image fetcher configuration that
// may conflict with Stillwater's image management.
type ImageFetcherWarning struct {
	Platform     string   `json:"platform"` // "emby" or "jellyfin"
	LibraryName  string   `json:"library_name"`
	FetcherNames []string `json:"fetcher_names"`
	RiskLevel    string   `json:"risk_level"` // "warn" or "critical"
	Message      string   `json:"message"`
	// Defaulted marks a library that has no artist metadata configuration
	// on the platform, so the platform's own defaults are in force and
	// those fetch images. FetcherNames is empty for these because the
	// platform never reports which fetchers its defaults use. The
	// remediation differs from an explicit fetcher list -- there is
	// nothing to switch off yet, the operator has to configure the
	// library -- so the two are rendered with different copy.
	Defaulted bool `json:"defaulted,omitempty"`
}
