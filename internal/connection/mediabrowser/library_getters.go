// This file collects the read-only per-artist and per-library getters that
// are byte-for-byte identical between the Emby and Jellyfin REST surfaces.
// Each per-platform client.go keeps a thin method that delegates its body
// here; see the per-function comments below for the one or two spots where
// a real platform divergence exists and how the caller supplies it.
package mediabrowser

import (
	"context"
	"errors"
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

// FetcherEntry is the shared per-library result CollectImageFetcherEntriesRaw
// produces. Callers wrap each entry in their own platform ImageFetcherStatus
// type (adding the platform's RiskLevel constant) since the two
// ImageFetcherStatus DTOs stay separate types.
//
// Defaulted distinguishes the two ways a library can be fetching images:
// explicitly configured fetchers (Defaulted=false, FetcherNames populated)
// versus no MusicArtist configuration at all, which leaves the peer's own
// defaults in force (Defaulted=true, FetcherNames empty). They need
// different remediation copy -- one says "turn these off", the other says
// "nothing is configured, so the server's defaults are running" -- so the
// distinction is carried rather than flattened into the name list.
type FetcherEntry struct {
	LibraryName  string
	LibraryID    string
	FetcherNames []string
	Defaulted    bool
}

// CollectImageFetcherEntriesRaw shares the library/TypeOption iteration
// common to CheckImageFetchersEnabled on both platforms: walk every library
// that passes includeLibrary and report each one that would fetch artist
// images. The RiskLevel label stays with the caller since it is not part of
// this shared shape. Generic over T (library) and O (type option) because
// the two platforms' VirtualFolder/TypeOption DTOs are separate types.
//
// declaredMusic gates the DEFAULTED case only, and it is separate from
// includeLibrary for a reason worth stating. The library filter upstream
// (FilterMusicLibraries) admits a library whose CollectionType is "music"
// OR BLANK, because some installs leave it blank on mixed or legacy folders
// that really do hold music. That leniency is free as long as absence is
// silent -- but this function makes ABSENCE THE TRIGGER, and a
// non-music library (home videos, a mixed folder) has no MusicArtist
// TypeOption precisely because it is not a music library. Emitting the
// defaulted warning for those tells an operator to go switch off artist
// image fetchers in their home-video library, where no such setting exists.
//
// So the two cases need different evidence:
//
//   - EXPLICIT fetchers (a MusicArtist entry with a non-empty list) are
//     reported for blank CollectionType too. The entry's existence is
//     affirmative proof the peer treats this library as holding artists,
//     whatever its label says, and the fetchers really are armed.
//   - DEFAULTED (no MusicArtist entry) requires declaredMusic. Claiming
//     "your artist defaults are running" on the strength of an ABSENCE
//     needs the peer to have affirmatively said this is a music library;
//     blank is not a statement, it is the lack of one.
//
// Getting this wrong does not merely add noise: a banner full of
// unactionable warnings about home-video libraries trains operators to
// ignore the one warning that is real.
//
// THREE INPUT STATES, and conflating any two of them misreports the peer:
//
//   - MusicArtist option PRESENT with a non-empty fetcher list -> reported,
//     Defaulted=false. The operator turned these on.
//   - MusicArtist option PRESENT with an EMPTY fetcher list -> NOT reported,
//     treated as clean. Precisely: the peer sent a MusicArtist entry and the
//     decoded fetcher list is empty. That is USUALLY an operator who
//     configured the library and chose no fetchers -- but the DTO decodes a
//     missing "ImageFetchers" KEY and an explicit "ImageFetchers": [] to the
//     same empty slice, so this cannot claim more than "the entry exists and
//     its list came back empty". If a peer is ever observed omitting the key
//     on an entry it otherwise populates, that shape belongs with the
//     absent-entry case (defaults apply, hence dirty) and telling the two
//     apart needs a *[]string or json.RawMessage on both platforms' DTOs.
//     Not done speculatively: it widens two DTOs and every accessor for an
//     input shape nobody has seen, and the current classification is the
//     conservative one for the shape that IS observed.
//   - MusicArtist option ABSENT entirely -> reported, Defaulted=true. This
//     is the case that used to be silently clean and is the #2719 defect.
//     An absent option does not mean "off", it means the peer has no stored
//     configuration for this type and therefore applies its OWN DEFAULTS --
//     and those have image fetchers ON. Reporting nothing here told the
//     operator they were safe while the peer was actively fetching.
//
// The per-platform divergence is expressed entirely through includeLibrary
// and is load-bearing for the absent case specifically: Jellyfin passes its
// EnableInternetProviders flag, Emby passes a constant true because it has
// no library-level equivalent. A library that fails includeLibrary is
// skipped WHOLE, which is what keeps "absent option means defaults apply"
// from firing on a Jellyfin library whose internet providers are switched
// off -- there the defaults cannot run, so silence is a TRUE clean rather
// than a false one. Do not hoist the absent-entry check above the
// includeLibrary gate; that inverts a false negative into a false positive.
func CollectImageFetcherEntriesRaw[T any, O any](
	libs []T,
	includeLibrary func(T) bool,
	declaredMusic func(T) bool,
	getName, getItemID func(T) string,
	getTypeOptions func(T) []O,
	getOptType func(O) string,
	getImageFetchers func(O) []string,
) []FetcherEntry {
	var out []FetcherEntry
	for i := range libs {
		lib := libs[i]
		if !includeLibrary(lib) {
			continue
		}
		var sawMusicArtist bool
		for _, opt := range getTypeOptions(lib) {
			if !strings.EqualFold(getOptType(opt), TypeMusicArtist) {
				continue
			}
			sawMusicArtist = true
			if fetchers := getImageFetchers(opt); len(fetchers) > 0 {
				out = append(out, FetcherEntry{
					LibraryName:  getName(lib),
					LibraryID:    getItemID(lib),
					FetcherNames: fetchers,
				})
			}
		}
		// See the declaredMusic paragraph above: an absent entry only means
		// "artist defaults are running" if the peer says this is a music
		// library. On a blank-CollectionType folder it means "this is not a
		// music library", and warning about it is a false positive.
		if !sawMusicArtist && declaredMusic(lib) {
			out = append(out, FetcherEntry{
				LibraryName: getName(lib),
				LibraryID:   getItemID(lib),
				Defaulted:   true,
			})
		}
	}
	return out
}

// DeclaredMusicCollectionType reports whether a library's CollectionType is
// affirmatively "music". Shared so both platforms apply the identical rule
// to the declaredMusic gate, and kept distinct from FilterMusicLibraries'
// looser "music or blank" test on purpose -- see CollectImageFetcherEntriesRaw
// for why an absence-triggered warning needs the stricter evidence.
func DeclaredMusicCollectionType(collectionType string) bool {
	return strings.EqualFold(strings.TrimSpace(collectionType), "music")
}

// FindMusicArtistOptionRaw returns the ImageFetchers/MetadataFetchers of the
// first TypeOption whose Type is "MusicArtist" (case-insensitive), matching
// the break-on-first-match semantics both platforms' GetLibrarySettings use.
// Returns (nil, nil) when no MusicArtist option exists. Generic over O
// because the two platforms' TypeOption DTOs are separate types.
func FindMusicArtistOptionRaw[O any](opts []O, getOptType func(O) string, getImageFetchers, getMetadataFetchers func(O) []string) (imageFetchers, metadataFetchers []string) {
	for _, opt := range opts {
		if strings.EqualFold(getOptType(opt), "MusicArtist") {
			return getImageFetchers(opt), getMetadataFetchers(opt)
		}
	}
	return nil, nil
}

// NormalizeStrings converts a nil slice to an empty, non-nil slice so JSON
// serializes as [] rather than null. Both platforms' GetLibrarySettings
// apply this to every string-slice field on the result.
func NormalizeStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
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
	items, _, err := ListLibraryArtistsComplete(ctx, libraryIDs, fetchPage)
	return items, err
}

// ErrListingTruncated marks an enumeration that stopped at listArtistsPageCap
// with the peer still reporting full pages. The items returned alongside it are
// VALID but INCOMPLETE.
//
// It exists because the dangerous shape of a truncated listing is that it looks
// exactly like a finished one: a caller that asked "is X in the library?" and
// got back a list without X cannot tell "X is absent" from "I stopped reading
// before X". Absence-from-a-listing is only meaningful if the listing was
// complete, so completeness has to be part of the return value rather than an
// assumption.
//
// Deliberately a WARNING rather than a hard error, and the reason is the same
// asymmetry: truncation does not make the items wrong, only the ABSENCES
// unreliable. A caller enumerating what exists (an import, a presence check)
// is fine; a caller reasoning from what is missing is not. So the items and the
// flag are returned together and the caller decides. Mirrors ErrPartialScan
// (internal/dupimages/cache.go), whose contract is the same one: only a scan
// that saw everything may be used to clear state.
var ErrListingTruncated = errors.New("mediabrowser: artist listing truncated at the page cap; absences are not meaningful")

// ListLibraryArtistsComplete is ListLibraryArtistsRaw plus the answer to "did I
// see the whole library?".
//
// Returns (items, complete, err). complete is true only when every library was
// walked to a short page -- the peer's own signal that it had nothing more to
// give. It is false when any library hit listArtistsPageCap while still
// returning full pages, and false on error.
//
// A caller that will DESTROY state on absence (drop a stored platform link,
// #2426) must refuse to act unless complete is true. A caller merely
// enumerating what is present can ignore it.
//
// Today the cap is 200 pages of 500, so truncation needs a 100,000-artist
// library and is not a live condition on any real deployment. It is
// nonetheless reported rather than assumed away: "cannot happen yet" is a
// property of current constants, not of the contract, and the whole point of
// this signal is that its absence is invisible at the call site. A caller that
// checks it is correct at any cap; one that assumes completeness silently
// becomes wrong if the cap or the limit ever moves.
func ListLibraryArtistsComplete(
	ctx context.Context, libraryIDs []string, fetchPage ArtistItemFetcher,
) (items []connection.PeerArtist, complete bool, err error) {
	var out []connection.PeerArtist
	sawEverything := true
	for _, libID := range libraryIDs {
		if libID == "" {
			continue
		}
		// reachedShortPage records the peer's own end-of-data signal. Starting
		// it false and setting it only on the short-page break is what makes
		// exhausting the cap detectable: the loop finishing by exhaustion and
		// the loop finishing by a short page are otherwise the same exit.
		reachedShortPage := false
		for page := 0; page < listArtistsPageCap; page++ {
			pageItems, n, fetchErr := fetchPage(ctx, libID, page*listArtistsPageLimit, listArtistsPageLimit)
			if fetchErr != nil {
				return nil, false, fmt.Errorf("listing artists in library %s: %w", libID, fetchErr)
			}
			out = append(out, pageItems...)
			if n < listArtistsPageLimit {
				reachedShortPage = true
				break
			}
		}
		if !reachedShortPage {
			sawEverything = false
		}
	}
	return out, sawEverything, nil
}
