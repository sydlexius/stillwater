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
	IsLocked      bool
	LockedFields  []string
}

// ArtistStateGetter fetches the current state of an artist from a platform.
type ArtistStateGetter interface {
	GetArtistDetail(ctx context.Context, platformArtistID string) (*ArtistPlatformState, error)
}
