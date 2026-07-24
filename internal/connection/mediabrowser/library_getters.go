// This file collects the read-only per-artist and per-library getters that
// are byte-for-byte identical between the Emby and Jellyfin REST surfaces.
// Each per-platform client.go keeps a thin method that delegates its body
// here; see the per-function comments below for the one or two spots where
// a real platform divergence exists and how the caller supplies it.
package mediabrowser

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sydlexius/stillwater/internal/connection"
)

// GetMusicLibrariesRaw2 fetches /Library/VirtualFolders into result (a
// pointer to the caller's own per-package []VirtualFolder -- the two
// VirtualFolder/LibraryOptions shapes diverge slightly, Jellyfin's carrying
// an extra EnableInternetProviders field, so that DTO stays per-package)
// and returns the decoded folders unfiltered; FilterMusicLibraries applies
// the shared "music or blank CollectionType" rule and debug-logs each
// candidate under the given platform tag.
//
// Named Raw2 to avoid colliding with the pre-existing GetMusicLibrariesRaw
// in library_options.go, which fetches into an untyped map for the
// conflict-detection snapshot/restore flow; this one decodes into the
// caller's typed slice for the ordinary GetMusicLibraries getter.
func GetMusicLibrariesRaw2(ctx context.Context, t Transport, result any) error {
	if err := t.Get(ctx, "/Library/VirtualFolders", result); err != nil {
		return fmt.Errorf("getting virtual folders: %w", err)
	}
	return nil
}

// FilterMusicLibraries applies the shared "music or blank CollectionType"
// inclusion rule to a decoded []VirtualFolder slice and debug-logs every
// candidate (name, collection type, whether it was included) under the
// given platform tag -- the only difference between the Emby and Jellyfin
// GetMusicLibraries bodies was this log line's platform prefix, which
// platform now supplies directly. Generic over T because the two
// VirtualFolder DTOs are separate types; getCollectionType/getName read the
// two fields the filter and log need out of either one.
func FilterMusicLibraries[T any](folders []T, logger *slog.Logger, platform string, getCollectionType, getName func(T) string) []T {
	var music []T
	for i := range folders {
		f := folders[i]
		ct := strings.TrimSpace(strings.ToLower(getCollectionType(f)))
		include := ct == "music" || ct == ""
		if logger != nil {
			logger.Debug(platform+" virtual folder discovered",
				"name", getName(f),
				"collection_type", getCollectionType(f),
				"included_as_music", include,
			)
		}
		if include {
			music = append(music, f)
		}
	}
	return music
}

// GetArtistBackdropRaw downloads a backdrop image at the given 0-based
// index. Identical on Emby and Jellyfin: same path shape, same GetRaw call.
func GetArtistBackdropRaw(ctx context.Context, t Transport, artistID string, index int) ([]byte, string, error) {
	path := fmt.Sprintf("/Items/%s/Images/Backdrop/%d", artistID, index)
	return t.GetRaw(ctx, path)
}

// GetArtistImageRaw downloads the raw image bytes for the given artist and
// platform-mapped image type. Callers map their own Stillwater image-type
// string (thumb, fanart, logo, banner) to the platform's image-type string
// via their per-package mapImageType before calling this -- Emby and
// Jellyfin use different lookup tables, so that mapping stays per-package.
// An empty platformType signals an unmapped Stillwater type; the caller's
// original imageType is passed through only for the error message.
func GetArtistImageRaw(ctx context.Context, t Transport, artistID, platformType, requestedImageType string) ([]byte, string, error) {
	if platformType == "" {
		return nil, "", fmt.Errorf("unsupported image type: %s", requestedImageType)
	}
	path := fmt.Sprintf("/Items/%s/Images/%s", artistID, platformType)
	return t.GetRaw(ctx, path)
}

// ArtistDetailFields is the projection BuildArtistPlatformState needs to
// build a connection.ArtistPlatformState. Both platforms decode their own
// ArtistDetailItem DTO (the shapes stay per-package -- unifying them is out
// of scope for this refactor) and populate this struct before calling
// BuildArtistPlatformState. Locked carries the one real divergence: Emby
// stores it as LockData, Jellyfin as IsLocked -- both plain bools, so the
// per-package caller resolves which field to read and passes the value in.
type ArtistDetailFields struct {
	Name              string
	SortName          string
	Overview          string
	Genres            []string
	Tags              []string
	PremiereDate      string
	EndDate           string
	MusicBrainzID     string
	ImageTags         map[string]string
	BackdropImageTags []string
	Locked            bool
	LockedFields      []string
}

// BuildArtistPlatformState assembles the shared connection.ArtistPlatformState
// from the decoded raw-item fields. Identical derivation logic on both
// platforms (HasThumb/HasFanart/etc. are all computed the same way from
// ImageTags/BackdropImageTags), so this is where GetArtistDetail's real body
// lives; each per-package GetArtistDetail method fetches its own
// ArtistDetailItem, populates ArtistDetailFields (supplying its own Locked
// source field), and calls this to build the value it returns.
func BuildArtistPlatformState(f ArtistDetailFields) *connection.ArtistPlatformState {
	return &connection.ArtistPlatformState{
		Name:          f.Name,
		SortName:      f.SortName,
		Biography:     f.Overview,
		Genres:        f.Genres,
		Tags:          f.Tags,
		PremiereDate:  f.PremiereDate,
		EndDate:       f.EndDate,
		MusicBrainzID: f.MusicBrainzID,
		HasThumb:      f.ImageTags["Primary"] != "",
		HasFanart:     len(f.BackdropImageTags) > 0,
		BackdropCount: len(f.BackdropImageTags),
		HasLogo:       f.ImageTags["Logo"] != "",
		HasBanner:     f.ImageTags["Banner"] != "",
		IsLocked:      f.Locked,
		LockedFields:  f.LockedFields,
	}
}

// GetArtistDetailRaw issues the shared /Users/{userID}/Items/{id} request
// (identical query shape and Fields list on both platforms) and decodes into
// result, which the caller passes as a pointer to its own per-package
// ArtistDetailItem DTO (the two DTOs stay separate types; unifying them is
// out of scope for this refactor).
func GetArtistDetailRaw(ctx context.Context, t Transport, userID, platformArtistID string, result any) error {
	if userID == "" {
		return fmt.Errorf("no user ID configured for this connection; re-test the connection to resolve")
	}
	path := fmt.Sprintf("/Users/%s/Items/%s?Fields=Overview,Genres,Tags,SortName,ProviderIds,ImageTags,BackdropImageTags,PremiereDate,EndDate,LockedFields", userID, platformArtistID)
	if err := t.Get(ctx, path, result); err != nil {
		return fmt.Errorf("getting artist detail: %w", err)
	}
	return nil
}

// GetArtistsRaw issues the shared AlbumArtists paginated query (identical
// query string on both platforms) and decodes into result, which the caller
// passes as a pointer to its own per-package ItemsResponse DTO (the two
// DTOs stay separate types; unifying them is out of scope for this
// refactor).
func GetArtistsRaw(ctx context.Context, t Transport, libraryID string, startIndex, limit int, result any) error {
	path := fmt.Sprintf("/Artists/AlbumArtists?ParentId=%s&StartIndex=%d&Limit=%d&Recursive=true&Fields=Path,ProviderIds,ImageTags,BackdropImageTags,Overview,Genres,Tags,SortName,PremiereDate,EndDate", libraryID, startIndex, limit)
	if err := t.Get(ctx, path, result); err != nil {
		return fmt.Errorf("getting artists: %w", err)
	}
	return nil
}

// listArtistsPageLimit bounds each page during a full library enumeration;
// matches the per-package constants it replaces on both platforms.
const listArtistsPageLimit = 500

// listArtistsPageCap bounds how many pages one enumeration will walk, so a
// peer that misreports its page count cannot spin this loop forever inside
// a rename.
const listArtistsPageCap = 200

// ArtistItemFetcher issues one page of the AlbumArtists query for the given
// library and returns each item reduced to connection.PeerArtist plus the
// page's item count (used to detect the final, short page). Implemented
// per-platform because the typed ItemsResponse/ArtistItem DTOs differ in
// fields this refactor does not unify.
type ArtistItemFetcher func(ctx context.Context, libraryID string, startIndex, limit int) (items []connection.PeerArtist, pageCount int, err error)

// ListLibraryArtistsRaw enumerates every artist in the peer's music
// libraries (given as their ItemIDs), walking pages via fetchPage until a
// short page signals the end or listArtistsPageCap is hit. Identical
// loop/paging logic on both platforms. Empty library IDs are skipped,
// matching the prior per-package behavior.
func ListLibraryArtistsRaw(ctx context.Context, libraryIDs []string, fetchPage ArtistItemFetcher) ([]connection.PeerArtist, error) {
	var out []connection.PeerArtist
	for _, libID := range libraryIDs {
		if libID == "" {
			continue
		}
		for page := 0; page < listArtistsPageCap; page++ {
			items, n, err := fetchPage(ctx, libID, page*listArtistsPageLimit, listArtistsPageLimit)
			if err != nil {
				return nil, fmt.Errorf("listing artists in library %s: %w", libID, err)
			}
			out = append(out, items...)
			if n < listArtistsPageLimit {
				break
			}
		}
	}
	return out, nil
}
