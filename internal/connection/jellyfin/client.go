package jellyfin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/mediabrowser"
	"github.com/sydlexius/stillwater/internal/version"
)

// ErrInvalidCredentials is returned when the media server rejects the
// credentials during the AuthenticateByName handshake.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrAuthRequired is the sentinel wrapped by write-method failures when the
// peer returns a 401 or 403. Distinct from ErrInvalidCredentials, which is
// scoped to the username/password handshake.
var ErrAuthRequired = errors.New("jellyfin: authentication required")

// wrapAuthIfStatusAuth wraps 401/403 StatusError with ErrAuthRequired; see
// emby.wrapAuthIfStatusAuth for rationale. The classification is shared
// with Emby, so this binds only the Jellyfin sentinel.
func wrapAuthIfStatusAuth(err error) error {
	return mediabrowser.ClassifyAuthError(err, ErrAuthRequired)
}

// Client communicates with a Jellyfin server. The HTTP transport, the
// MediaBrowser Authorization auth scheme, and the peer user identity live
// on the embedded shared mediabrowser.Client; everything BaseClient
// exposed before (Get, GetRaw, Post, PostJSON, PutJSON, BaseURL, APIKey,
// Logger, HTTPClient, AuthFunc) is still reachable here by promotion.
type Client struct {
	*mediabrowser.Client
}

// New creates a Jellyfin client with default HTTP settings.
//
// Uses a raw http.Client (not httpsafe.SafeClient) because Jellyfin is a
// user-configured media server that almost always runs on loopback
// (127.0.0.1) or an RFC 1918 LAN address (192.168.x.x, 10.x.x.x) for
// self-hosted deployments. The httpsafe.SafeTransport SSRF guard would
// reject those destinations, breaking the integration for legitimate
// setups. The destination URL is operator-supplied via Settings, not
// user-controlled input.
func New(baseURL, apiKey, userID string, logger *slog.Logger) *Client {
	return NewWithHTTPClient(baseURL, apiKey, userID, &http.Client{Timeout: 10 * time.Second}, logger)
}

// NewWithHTTPClient creates a Jellyfin client with a custom HTTP client (for testing).
func NewWithHTTPClient(baseURL, apiKey, userID string, httpClient *http.Client, logger *slog.Logger) *Client {
	mb := mediabrowser.NewWithProfile(mediabrowser.JellyfinProfile, baseURL, apiKey, userID, httpClient, logger)
	return &Client{Client: mb}
}

// TestConnection verifies connectivity by calling GET /System/Info.
func (c *Client) TestConnection(ctx context.Context) error {
	var info SystemInfo
	if err := c.Get(ctx, "/System/Info", &info); err != nil {
		return fmt.Errorf("testing connection: %w", err)
	}
	c.Logger.Debug("jellyfin connection ok", "server", info.ServerName, "version", info.Version)
	return nil
}

// GetMusicLibraries returns virtual folders that represent music content.
// See emby.GetMusicLibraries for the rationale behind accepting libraries
// with a blank CollectionType. Every considered folder is logged so users
// can diagnose why a given library does or does not appear in the
// conflict banner.
func (c *Client) GetMusicLibraries(ctx context.Context) ([]VirtualFolder, error) {
	var folders []VirtualFolder
	if err := mediabrowser.GetMusicLibrariesRaw2(ctx, c, &folders); err != nil {
		return nil, err
	}
	return mediabrowser.FilterMusicLibraries(folders, c.Logger, mediabrowser.PlatformJellyfin,
		func(f VirtualFolder) string { return f.CollectionType },
		func(f VirtualFolder) string { return f.Name },
	), nil
}

// CheckNFOWriterEnabled reports whether any Jellyfin music library has an NFO
// metadata saver enabled. Returns the matching library name when one is found.
// An error from the server is returned rather than swallowed so the caller
// (conflict detector) can distinguish "no conflict" from "unable to check"
// and populate ConnectionState.CheckErr for fail-closed gating; silently
// returning (false, "", nil) on error would mark the connection clean and
// reopen writes on a transient peer outage.
func (c *Client) CheckNFOWriterEnabled(ctx context.Context) (bool, string, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return false, "", fmt.Errorf("checking jellyfin nfo saver settings: %w", err)
	}

	for i := range libs {
		lib := &libs[i]
		for _, saver := range lib.LibraryOptions.MetadataSavers {
			if strings.Contains(strings.ToLower(saver), "nfo") {
				return true, lib.Name, nil
			}
		}
	}
	return false, "", nil
}

// ImageFetcherStatus describes the image fetcher configuration for a music library.
type ImageFetcherStatus struct {
	LibraryName  string
	LibraryID    string
	FetcherNames []string // e.g., ["TheAudioDb", "FanArt"]
	RiskLevel    string   // "critical" for Jellyfin (can replace existing images and strip EXIF)
}

// CheckImageFetchersEnabled returns the image fetcher status for Jellyfin music
// libraries. Returns nil if no image fetchers are enabled. Returns a non-nil
// error if the music library settings cannot be retrieved. Libraries with
// EnableInternetProviders=false are skipped.
func (c *Client) CheckImageFetchersEnabled(ctx context.Context) ([]ImageFetcherStatus, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking jellyfin image fetcher settings: %w", err)
	}

	// If internet providers are globally disabled for a library, its image
	// fetchers are inactive regardless of TypeOptions.
	entries := mediabrowser.CollectImageFetcherEntriesRaw(libs,
		func(l VirtualFolder) bool { return l.LibraryOptions.EnableInternetProviders },
		func(l VirtualFolder) string { return l.Name },
		func(l VirtualFolder) string { return l.ItemID },
		func(l VirtualFolder) []TypeOption { return l.LibraryOptions.TypeOptions },
		func(o TypeOption) string { return o.Type },
		func(o TypeOption) []string { return o.ImageFetchers },
	)
	var results []ImageFetcherStatus
	for _, e := range entries {
		// Jellyfin can replace existing images and strip EXIF provenance
		// data, so the risk level is always "critical".
		results = append(results, ImageFetcherStatus{
			LibraryName:  e.LibraryName,
			LibraryID:    e.LibraryID,
			FetcherNames: e.FetcherNames,
			RiskLevel:    "critical",
		})
	}
	return results, nil
}

// GetArtists returns album artists from a specific library with pagination.
func (c *Client) GetArtists(ctx context.Context, libraryID string, startIndex, limit int) (*ItemsResponse, error) {
	var resp ItemsResponse
	if err := mediabrowser.GetArtistsRaw(ctx, c, libraryID, startIndex, limit, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// TriggerLibraryScan triggers a full library scan.
func (c *Client) TriggerLibraryScan(ctx context.Context) error {
	return mediabrowser.TriggerLibraryScanRaw(ctx, c, wrapAuthIfStatusAuth)
}

// rescanQuery drives a non-destructive item refresh: Recursive=true so a
// folder item (an artist) picks up newly-appeared child folders (albums
// absorbed by a merge), while MetadataRefreshMode=Default and
// ReplaceAllMetadata=false mean Jellyfin only fills in missing data rather
// than forcing a re-import -- unlike reimportRefreshQuery below, this never
// overwrites existing metadata. This is also how Jellyfin is asked to notice
// a child item whose on-disk path has vanished (the merge's deleted loser
// directory) and evict it from the library.
const rescanQuery = "Recursive=true&MetadataRefreshMode=Default&ImageRefreshMode=Default&ReplaceAllMetadata=false&ReplaceAllImages=false"

// TriggerItemRescan asks Jellyfin to re-validate a single item (and, if it is
// a folder, its children) without forcing a metadata or image replace: both
// MetadataRefreshMode and ImageRefreshMode stay at Default and
// ReplaceAllMetadata/ReplaceAllImages stay false, so this only fills in
// missing data rather than overwriting what Jellyfin already has. Used by the
// post-merge refresh to scope reconciliation to the survivor and loser items
// instead of a full library scan (#2431); the caller falls back to
// TriggerLibraryScan when the survivor has no mapped item on this connection
// or this scoped call errors.
func (c *Client) TriggerItemRescan(ctx context.Context, itemID string) error {
	if strings.TrimSpace(itemID) == "" {
		return fmt.Errorf("itemID is required")
	}
	path := fmt.Sprintf("/Items/%s/Refresh?%s", url.PathEscape(itemID), rescanQuery)
	if err := c.Post(ctx, path, nil); err != nil {
		return fmt.Errorf("triggering item rescan: %w", wrapAuthIfStatusAuth(err))
	}
	return nil
}

// reimportRefreshQuery forces Jellyfin to re-read an item's on-disk NFO.
// Jellyfin's OpenAPI confirms local metadata is only re-imported under
// MetadataRefreshMode=FullRefresh, where ReplaceAllMetadata=true then replaces
// the item's metadata from the NFO. ReplaceAllImages=false leaves artwork
// untouched so a re-read does not re-scrape images (#2338). This is the only
// channel through which NFO-only fields (Disambiguation, YearsActive) reach the
// platform, underpinning the #2336 field-drop fix.
const reimportRefreshQuery = "MetadataRefreshMode=FullRefresh&ReplaceAllMetadata=true&ReplaceAllImages=false"

// TriggerArtistRefresh forces Jellyfin to re-import the artist's on-disk NFO,
// applying NFO-only fields (Disambiguation, YearsActive) the metadata API body
// cannot carry. Destructive full re-import (ReplaceAllMetadata=true), so callers
// must gate it on the operator opt-in (FeatureTriggerRefresh); the publish-layer
// dispatcher (publish.RefreshArtistOnPlatforms) is the sole caller and enforces
// that gate.
func (c *Client) TriggerArtistRefresh(ctx context.Context, artistID string) error {
	return mediabrowser.TriggerArtistRefreshRaw(ctx, c, artistID, reimportRefreshQuery, wrapAuthIfStatusAuth)
}

// GetArtistImage downloads the raw image bytes for the given artist and image type.
// imageType uses Stillwater naming (thumb, fanart, logo, banner).
func (c *Client) GetArtistImage(ctx context.Context, artistID, imageType string) ([]byte, string, error) {
	platformType := mapImageType(imageType)
	return mediabrowser.GetArtistImageRaw(ctx, c, artistID, platformType, imageType)
}

// GetArtistBackdrop downloads a backdrop image at the given 0-based index.
func (c *Client) GetArtistBackdrop(ctx context.Context, artistID string, index int) ([]byte, string, error) {
	return mediabrowser.GetArtistBackdropRaw(ctx, c, artistID, index)
}

// GetServerID fetches the Jellyfin server's identity from GET /System/Info
// and returns the "Id" field. The Jellyfin web client expects this value in
// the ?serverId=<id> query parameter when deep linking into an item.
// Used at connection-test time to resolve and persist the server ID in the
// connections table.
func (c *Client) GetServerID(ctx context.Context) (string, error) {
	var info SystemInfo
	if err := c.Get(ctx, "/System/Info", &info); err != nil {
		return "", fmt.Errorf("getting system info: %w", err)
	}
	if info.ID == "" {
		return "", fmt.Errorf("system info did not return a server id")
	}
	return info.ID, nil
}

// GetFirstUserID fetches the first user ID from GET /Users. Used at connection-test time
// to resolve and persist the user ID in the connections table.
func (c *Client) GetFirstUserID(ctx context.Context) (string, error) {
	var users []UserItem
	if err := c.Get(ctx, "/Users", &users); err != nil {
		return "", fmt.Errorf("getting users: %w", err)
	}
	for _, u := range users {
		if u.ID != "" {
			return u.ID, nil
		}
	}
	return "", fmt.Errorf("no users with a non-empty ID returned from /Users")
}

// GetArtistDetail fetches the current state of an artist from Jellyfin by platform artist ID.
func (c *Client) GetArtistDetail(ctx context.Context, platformArtistID string) (*connection.ArtistPlatformState, error) {
	var item ArtistDetailItem
	if err := mediabrowser.GetArtistDetailRaw(ctx, c, c.UserID, platformArtistID, &item); err != nil {
		return nil, err
	}
	return mediabrowser.BuildArtistPlatformState(mediabrowser.ArtistDetailFields{
		Name:              item.Name,
		SortName:          item.SortName,
		Overview:          item.Overview,
		Genres:            item.Genres,
		Tags:              item.Tags,
		PremiereDate:      item.PremiereDate,
		EndDate:           item.EndDate,
		MusicBrainzID:     item.ProviderIDs.MusicBrainzArtist,
		ImageTags:         item.ImageTags,
		BackdropImageTags: item.BackdropImageTags,
		Locked:            item.IsLocked,
		LockedFields:      item.LockedFields,
	}), nil
}

// AuthenticateByName authenticates a user against a Jellyfin server using
// username and password. This is a package-level function because it does not
// require an existing API key; authentication is the mechanism for obtaining one.
func AuthenticateByName(ctx context.Context, baseURL, username, password string, logger *slog.Logger) (*AuthResult, error) {
	cleaned, err := connection.ValidateBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	body, err := json.Marshal(map[string]string{
		"Username": username,
		"Pw":       password,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding request body: %w", err)
	}

	// The URL is built from a validated base (ValidateBaseURL enforces http/https,
	// rejects credentials, query strings, and fragments) plus a fixed API path.
	// BuildRequestURL reconstructs the URL from parsed components via a url.URL
	// struct literal, preventing path-based request target override.
	reqURL := connection.BuildRequestURL(cleaned, "/Users/AuthenticateByName")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	// DeviceId is a truncated SHA-256 hash of the validated base URL, providing
	// a stable identifier per server without exposing the actual URL. Using the
	// cleaned URL ensures equivalent inputs (e.g. with/without trailing slash)
	// produce the same device identity.
	deviceID := fmt.Sprintf("%x", sha256.Sum256([]byte(cleaned)))[:16]

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf(
		`MediaBrowser Client="Stillwater", Device="Server", DeviceId="%s", Version="%s"`,
		deviceID, version.Version,
	))

	// Auth flow targets the same operator-supplied Jellyfin server as the rest
	// of the package (see New() for the LAN/loopback rationale). httpsafe would
	// block these legitimate destinations.
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	switch resp.StatusCode {
	case http.StatusOK:
		var result AuthResult
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("decoding response: %w", err)
		}
		if result.AccessToken == "" || result.User.ID == "" {
			return nil, fmt.Errorf("incomplete authentication response: missing access token or user ID")
		}
		logger.Debug("jellyfin authentication successful", "user", result.User.Name)
		return &result, nil
	case http.StatusUnauthorized:
		// Drain the body so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, ErrInvalidCredentials
	default:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("authentication failed: HTTP %d", resp.StatusCode)
	}
}

// GetLibrarySettings reads the fetcher/saver/downloader configuration for all music libraries.
// Each library entry describes which image fetchers, metadata fetchers, and metadata savers
// are active for MusicArtist content. NeedsLockdata reports whether the library still
// has an ARMED NFO saver, in which case lockdata injection is the only protection left;
// once the saver list is cleared it is not needed (see the loop below and #2420).
func (c *Client) GetLibrarySettings(ctx context.Context) ([]LibrarySettingsStatus, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting music libraries: %w", err)
	}

	results := make([]LibrarySettingsStatus, 0, len(libs))
	for i := range libs {
		lib := &libs[i]
		imageFetchers, metadataFetchers := mediabrowser.FindMusicArtistOptionRaw(lib.LibraryOptions.TypeOptions,
			func(o TypeOption) string { return o.Type },
			func(o TypeOption) []string { return o.ImageFetchers },
			func(o TypeOption) []string { return o.MetadataFetchers },
		)
		status := LibrarySettingsStatus{
			LibraryID:               lib.ItemID,
			LibraryName:             lib.Name,
			ImageFetchers:           mediabrowser.NormalizeStrings(imageFetchers),
			MetadataFetchers:        mediabrowser.NormalizeStrings(metadataFetchers),
			MetadataSavers:          mediabrowser.NormalizeStrings(lib.LibraryOptions.MetadataSavers),
			EnableInternetProviders: lib.LibraryOptions.EnableInternetProviders,
		}
		// Lockdata is needed only while the NFO saver is still ARMED. This used to
		// be hardcoded true, on the claim that "Jellyfin MetadataSavers=[] does NOT
		// stop NFO writes". That claim is false on Jellyfin 10.11.10 and was measured
		// to be false (#2420): with the saver armed a rename let Jellyfin re-create
		// the renamed-away directory; with MetadataSavers=[] it did not. An empty
		// saver list IS the reliable lever, so a library whose savers we have
		// disarmed does not need lockdata -- and reporting that it does told
		// operators to go work around a problem that no longer exists.
		status.NeedsLockdata = len(status.MetadataSavers) > 0
		// A library has conflicts if fetchers are active (when internet providers are enabled).
		status.HasConflicts = status.EnableInternetProviders &&
			(len(status.ImageFetchers) > 0 || len(status.MetadataFetchers) > 0)
		results = append(results, status)
	}
	return results, nil
}

// DisableConflictingSettings clears image fetchers and metadata fetchers for
// MusicArtist content in the specified library via the Jellyfin API. It does not
// touch the saver list; disarming the savers is DisableFileWriteBack's job, and
// that is what the Stillwater-managed toggle drives.
//
// This comment used to claim "Jellyfin ignores MetadataSavers=[]". That is FALSE
// on Jellyfin 10.11.10 and was measured to be false: with the saver armed, a
// rename let Jellyfin re-create the renamed-away directory and resurrect a
// duplicate artist; with MetadataSavers=[] the directory stayed gone. Clearing
// the saver list also stops Jellyfin writing artwork into the library (it still
// FETCHES images for its own UI -- verified with a forced full image refresh --
// it just no longer saves them to disk). See #2420.
//
// Do not restore the old claim, and do not "fix" NFO writes here by injecting
// lockdata: the saver list is the lever, and DisableFileWriteBack pulls it.
func (c *Client) DisableConflictingSettings(ctx context.Context, libraryID string) error {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return fmt.Errorf("getting music libraries: %w", err)
	}

	var target *VirtualFolder
	for i := range libs {
		if libs[i].ItemID == libraryID {
			target = &libs[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("library not found: %s", libraryID)
	}

	// Clear MusicArtist-specific fetchers.
	for i, opt := range target.LibraryOptions.TypeOptions {
		if strings.EqualFold(opt.Type, "MusicArtist") {
			target.LibraryOptions.TypeOptions[i].ImageFetchers = []string{}
			target.LibraryOptions.TypeOptions[i].MetadataFetchers = []string{}
			break
		}
	}

	body, err := json.Marshal(target.LibraryOptions)
	if err != nil {
		return fmt.Errorf("encoding library options: %w", err)
	}

	path := fmt.Sprintf("/Library/VirtualFolders/LibraryOptions?Id=%s", libraryID)
	return wrapAuthIfStatusAuth(c.PostJSON(ctx, path, bytes.NewReader(body), nil))
}

// CheckImageSaverEnabled reports whether Jellyfin will persist artwork files
// into the shared library directory for any music library. Like Emby, this is
// governed by the library-wide SaveLocalMetadata flag (the "Save artwork into
// media folders" checkbox). True means any Stillwater write will be mirrored
// by Jellyfin under its own filename convention, duplicating the file.
func (c *Client) CheckImageSaverEnabled(ctx context.Context) (bool, string, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return false, "", fmt.Errorf("checking jellyfin image saver settings: %w", err)
	}
	found, name := mediabrowser.CheckImageSaverEnabledRaw(libs,
		func(l VirtualFolder) bool { return l.LibraryOptions.SaveLocalMetadata },
		func(l VirtualFolder) string { return l.Name },
	)
	return found, name, nil
}

// SnapshotLibraryOptions captures the current saver state for every Jellyfin
// music library. The typed GetMusicLibraries call lives in this package
// because the Emby and Jellyfin VirtualFolder shapes diverge slightly; the
// per-library snapshot envelope itself is shared via mediabrowser.BuildSnapshot.
func (c *Client) SnapshotLibraryOptions(ctx context.Context) (string, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return "", fmt.Errorf("getting music libraries for snapshot: %w", err)
	}
	return mediabrowser.SnapshotLibraryOptionsRaw(libs,
		func(l VirtualFolder) string { return l.ItemID },
		func(l VirtualFolder) string { return l.Name },
		func(l VirtualFolder) bool { return l.LibraryOptions.SaveLocalMetadata },
		func(l VirtualFolder) []string { return l.LibraryOptions.MetadataSavers },
	)
}

// DisableFileWriteBack clears SaveLocalMetadata on every Jellyfin music
// library. Delegates to mediabrowser since Emby and Jellyfin share the
// /Library/VirtualFolders REST surface byte-for-byte at the raw-JSON level;
// the platform argument only adjusts the per-call debug log prefix.
func (c *Client) DisableFileWriteBack(ctx context.Context) error {
	return mediabrowser.DisableFileWriteBack(ctx, c, c.Logger, "jellyfin")
}

// RestoreLibraryOptions applies a previously saved snapshot to the Jellyfin
// peer. Delegates to the shared mediabrowser implementation.
func (c *Client) RestoreLibraryOptions(ctx context.Context, snapshotJSON string) error {
	return mediabrowser.RestoreLibraryOptions(ctx, c, c.Logger, "jellyfin", snapshotJSON)
}

// UpdateArtistPath rewrites the Path property on the given Jellyfin artist
// item. Jellyfin's POST /Items/{id} requires the full item body (same as
// Emby's), so this fetches the current item via fetchItem, overwrites only
// Path, strips read-only fields, and POSTs the merged payload back through
// the shared postFullItem helper.
//
// Used by publish.Publisher.SyncRename after a successful directory rename to
// keep the Jellyfin item-to-path mapping consistent (#1222). A path that no
// longer matches a real directory makes Jellyfin drop the item on its next
// scan, which orphans Stillwater's platform_id row.
func (c *Client) UpdateArtistPath(ctx context.Context, platformArtistID, newPath string) error {
	if strings.TrimSpace(platformArtistID) == "" {
		return fmt.Errorf("platformArtistID is required")
	}
	if strings.TrimSpace(newPath) == "" {
		return fmt.Errorf("newPath is required")
	}
	existing, err := c.fetchItem(ctx, platformArtistID)
	if err != nil {
		return fmt.Errorf("fetching artist for path update: %w", err)
	}
	existing["Path"] = newPath
	return c.postFullItem(ctx, platformArtistID, existing, "path update")
}

// jellyfinFetchFields is the exact Fields query value fetchItem has always
// passed to GET /Items?Ids=...&Fields=... -- LockData and LockedFields are
// NOT returned by default on Jellyfin's item endpoints; they must be
// listed explicitly or a subsequent full-replacement POST (UpdateArtistLocks
// / PushMetadata / UpdateArtistPath) will silently clear server-side locks.
const jellyfinFetchFields = "Overview,ProviderIds,PremiereDate,EndDate,Genres,Tags,LockData,LockedFields"

// GetArtistPath reads back the peer's CURRENT path for an artist item. Returns
// "" when the peer exposes no path for the item.
//
// This is the read-back half of the verify-after-update contract, and on
// Jellyfin it is the ONLY thing that can detect the #2380 defect: Jellyfin's
// POST /Items/{id} accepts a Path field, returns 204, and SILENTLY DISCARDS it
// (confirmed by replay against a live Jellyfin 10.11.10). The API spec actively
// misleads here - Path is documented on BaseItemDto as "Gets or sets the path"
// and is NOT marked readOnly - so the write looks contractually sound and the
// 2xx looks like confirmation. It is not. Two prior fixes shipped broken on
// exactly that false signal. Never treat UpdateArtistPath's success return as
// evidence the path changed; read it back.
func (c *Client) GetArtistPath(ctx context.Context, platformArtistID string) (string, error) {
	if strings.TrimSpace(platformArtistID) == "" {
		return "", fmt.Errorf("platformArtistID is required")
	}
	item, err := c.fetchItem(ctx, platformArtistID)
	if err != nil {
		return "", fmt.Errorf("fetching artist for path read-back: %w", err)
	}
	path, _ := item["Path"].(string)
	return path, nil
}

// ListLibraryArtists enumerates every artist that lives in one of this peer's
// MUSIC LIBRARIES, together with the peer's own path for each.
//
// Scoped to the music libraries (ParentId) on purpose: that scoping is part of
// the correctness of the post-move relink. An unscoped artist query also returns
// metadata-only ghosts stranded outside every library root, and re-linking to one
// of those is the #2380 corruption itself.
func (c *Client) ListLibraryArtists(ctx context.Context) ([]connection.PeerArtist, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing music libraries: %w", err)
	}
	libraryIDs := make([]string, len(libs))
	for i := range libs {
		libraryIDs[i] = libs[i].ItemID
	}
	return mediabrowser.ListLibraryArtistsRaw(ctx, libraryIDs, func(ctx context.Context, libraryID string, startIndex, limit int) ([]connection.PeerArtist, int, error) {
		resp, err := c.GetArtists(ctx, libraryID, startIndex, limit)
		if err != nil {
			return nil, 0, err
		}
		items := make([]connection.PeerArtist, len(resp.Items))
		for j := range resp.Items {
			it := &resp.Items[j]
			items[j] = connection.PeerArtist{ID: it.ID, Name: it.Name, Path: it.Path}
		}
		return items, len(resp.Items), nil
	})
}
