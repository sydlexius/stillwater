package emby

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
	"github.com/sydlexius/stillwater/internal/connection/httpclient"
	"github.com/sydlexius/stillwater/internal/connection/mediabrowser"
	"github.com/sydlexius/stillwater/internal/version"
)

// ErrInvalidCredentials is returned when the media server rejects the
// credentials during the AuthenticateByName handshake. Scoped to the initial
// username/password exchange; write methods report auth-class failures via
// ErrAuthRequired (see below).
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrAuthRequired is the sentinel wrapped by write-method failures when the
// peer returns a 401 or 403. Callers in the publish layer use
// errors.Is(err, emby.ErrAuthRequired) so publish.classifyPushErr maps the
// failure to the auth_failed class consumed by the per-connection
// push-failure toast. Distinct from ErrInvalidCredentials (which is scoped
// to the username/password handshake) because the API-key write path has
// its own re-auth UI signal.
var ErrAuthRequired = errors.New("emby: authentication required")

// wrapAuthIfStatusAuth detects an httpclient.StatusError whose code is 401 or
// 403 and wraps the original error with ErrAuthRequired. Used by every write
// method in this package so the publish layer can route auth-class failures
// to a re-auth UI signal without parsing the formatted error string. The
// original error is preserved (still %w-wrapped) so the existing
// classifyPushErr substring contract on err.Error() continues to match the
// "HTTP 401" / "status 401" surface.
func wrapAuthIfStatusAuth(err error) error {
	if err == nil {
		return nil
	}
	var se *httpclient.StatusError
	if errors.As(err, &se) && se.IsAuth() {
		return fmt.Errorf("%w: %w", ErrAuthRequired, err)
	}
	return err
}

// Client communicates with an Emby server.
type Client struct {
	httpclient.BaseClient
	userID string
}

// New creates an Emby client with default HTTP settings.
//
// Uses a raw http.Client (not httpsafe.SafeClient) because Emby is a
// user-configured media server that almost always lives on loopback
// (127.0.0.1) or an RFC 1918 LAN address (192.168.x.x, 10.x.x.x) for
// self-hosted deployments. The httpsafe.SafeTransport SSRF guard would
// reject precisely those destinations, breaking the integration for
// legitimate setups. The destination URL is supplied by the operator
// via Settings, not by user-controlled provider input.
func New(baseURL, apiKey, userID string, logger *slog.Logger) *Client {
	return NewWithHTTPClient(baseURL, apiKey, userID, &http.Client{Timeout: 10 * time.Second}, logger)
}

// NewWithHTTPClient creates an Emby client with a custom HTTP client (for testing).
func NewWithHTTPClient(baseURL, apiKey, userID string, httpClient *http.Client, logger *slog.Logger) *Client {
	c := &Client{
		BaseClient: httpclient.NewBase(baseURL, apiKey, httpClient, logger, "emby"),
		userID:     userID,
	}
	c.AuthFunc = c.setAuth
	return c
}

// TestConnection verifies connectivity by calling GET /System/Info.
func (c *Client) TestConnection(ctx context.Context) error {
	var info SystemInfo
	if err := c.Get(ctx, "/System/Info", &info); err != nil {
		return fmt.Errorf("testing connection: %w", err)
	}
	c.Logger.Debug("emby connection ok", "server", info.ServerName, "version", info.Version)
	return nil
}

// GetMusicLibraries returns virtual folders that represent music content.
// A folder qualifies when its CollectionType is explicitly "music" OR is
// left blank. Emby lets administrators create libraries without an
// explicit type (typically when the library mixes categories or was
// created by an older client), and those libraries still receive NFO and
// artwork writes, so they must be considered by conflict detection.
// Collection types that positively identify another category -- "movies",
// "tvshows", "homevideos", "boxsets", etc. -- are excluded so we do not
// mistakenly offer to disable savers on a user's movies library.
func (c *Client) GetMusicLibraries(ctx context.Context) ([]VirtualFolder, error) {
	var folders []VirtualFolder
	if err := c.Get(ctx, "/Library/VirtualFolders", &folders); err != nil {
		return nil, fmt.Errorf("getting virtual folders: %w", err)
	}

	var music []VirtualFolder
	for i := range folders {
		f := &folders[i]
		ct := strings.TrimSpace(strings.ToLower(f.CollectionType))
		include := ct == "music" || ct == ""
		c.Logger.Debug("emby virtual folder discovered", "name", f.Name, "collection_type", f.CollectionType, "included_as_music", include)
		if include {
			music = append(music, *f)
		}
	}
	return music, nil
}

// CheckNFOWriterEnabled reports whether any Emby music library has an NFO
// metadata saver enabled. Returns the matching library name when one is found.
// An error from the server is returned rather than swallowed so the caller
// (conflict detector) can distinguish "no conflict" from "unable to check"
// and populate ConnectionState.CheckErr for fail-closed gating; silently
// returning (false, "", nil) on error would mark the connection clean and
// reopen writes on a transient peer outage.
func (c *Client) CheckNFOWriterEnabled(ctx context.Context) (bool, string, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return false, "", fmt.Errorf("checking emby nfo saver settings: %w", err)
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
	RiskLevel    string   // "warn" for Emby (adds missing images only)
}

// CheckImageFetchersEnabled returns the image fetcher status for music libraries.
// Returns nil if no image fetchers are enabled. Returns a non-nil error if the
// music library settings cannot be retrieved.
func (c *Client) CheckImageFetchersEnabled(ctx context.Context) ([]ImageFetcherStatus, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking emby image fetcher settings: %w", err)
	}

	var results []ImageFetcherStatus
	for i := range libs {
		lib := &libs[i]
		for _, opt := range lib.LibraryOptions.TypeOptions {
			if !strings.EqualFold(opt.Type, "MusicArtist") {
				continue
			}
			if len(opt.ImageFetchers) > 0 {
				results = append(results, ImageFetcherStatus{
					LibraryName:  lib.Name,
					LibraryID:    lib.ItemID,
					FetcherNames: opt.ImageFetchers,
					RiskLevel:    "warn",
				})
			}
		}
	}
	return results, nil
}

// GetArtists returns album artists from a specific library (by parent ID) with pagination.
func (c *Client) GetArtists(ctx context.Context, libraryID string, startIndex, limit int) (*ItemsResponse, error) {
	path := fmt.Sprintf("/Artists/AlbumArtists?ParentId=%s&StartIndex=%d&Limit=%d&Recursive=true&Fields=Path,ProviderIds,ImageTags,BackdropImageTags,Overview,Genres,Tags,SortName,PremiereDate,EndDate", libraryID, startIndex, limit)
	var resp ItemsResponse
	if err := c.Get(ctx, path, &resp); err != nil {
		return nil, fmt.Errorf("getting artists: %w", err)
	}
	return &resp, nil
}

// TriggerLibraryScan triggers a full library scan.
func (c *Client) TriggerLibraryScan(ctx context.Context) error {
	if err := c.Post(ctx, "/Library/Refresh", nil); err != nil {
		return fmt.Errorf("triggering library scan: %w", wrapAuthIfStatusAuth(err))
	}
	return nil
}

// reimportRefreshQuery forces Emby to re-read an item's on-disk NFO. Emby only
// re-imports local metadata under MetadataRefreshMode=FullRefresh, and only then
// does ReplaceAllMetadata=true take effect (a destructive replace of the item's
// metadata from the NFO). ImageRefreshMode=Default + ReplaceAllImages=false leave
// artwork untouched so a re-read does not re-scrape images (a separately-tracked
// concern, #2338). This is the ONLY channel through which NFO-only fields --
// Disambiguation and YearsActive, which have no Emby BaseItemDto field -- reach
// the platform, so it underpins the #2336 field-drop fix.
const reimportRefreshQuery = "MetadataRefreshMode=FullRefresh&ReplaceAllMetadata=true&ImageRefreshMode=Default&ReplaceAllImages=false"

// TriggerArtistRefresh forces Emby to re-import the artist's on-disk NFO,
// applying NFO-only fields (Disambiguation, YearsActive) that the metadata API
// body cannot carry. This is a destructive full re-import (ReplaceAllMetadata=true),
// so callers must gate it on the operator's opt-in (FeatureTriggerRefresh); the
// publish-layer dispatcher (publish.RefreshArtistOnPlatforms) is the sole caller
// and enforces that gate.
func (c *Client) TriggerArtistRefresh(ctx context.Context, artistID string) error {
	if strings.TrimSpace(artistID) == "" {
		return fmt.Errorf("artistID is required")
	}
	// PathEscape the ID so a value containing reserved characters cannot break
	// out of the URL segment; the query string carries the re-import mode.
	path := fmt.Sprintf("/Items/%s/Refresh?%s", url.PathEscape(artistID), reimportRefreshQuery)
	if err := c.Post(ctx, path, nil); err != nil {
		return fmt.Errorf("triggering artist refresh: %w", wrapAuthIfStatusAuth(err))
	}
	return nil
}

// GetArtistImage downloads the raw image bytes for the given artist and image type.
// imageType uses Stillwater naming (thumb, fanart, logo, banner).
func (c *Client) GetArtistImage(ctx context.Context, artistID, imageType string) ([]byte, string, error) {
	platformType := mapImageType(imageType)
	if platformType == "" {
		return nil, "", fmt.Errorf("unsupported image type: %s", imageType)
	}
	path := fmt.Sprintf("/Items/%s/Images/%s", artistID, platformType)
	return c.GetRaw(ctx, path)
}

// GetArtistBackdrop downloads a backdrop image at the given 0-based index.
func (c *Client) GetArtistBackdrop(ctx context.Context, artistID string, index int) ([]byte, string, error) {
	path := fmt.Sprintf("/Items/%s/Images/Backdrop/%d", artistID, index)
	return c.GetRaw(ctx, path)
}

// GetServerID fetches the Emby server's identity from GET /System/Info and
// returns the "Id" field. This value is what the Emby web client expects in
// the ?serverId=<id> query parameter when deep linking into an item. Used at
// connection-test time to resolve and persist the server ID in the
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

// GetArtistDetail fetches the current state of an artist from Emby by platform artist ID.
func (c *Client) GetArtistDetail(ctx context.Context, platformArtistID string) (*connection.ArtistPlatformState, error) {
	if c.userID == "" {
		return nil, fmt.Errorf("no user ID configured for this connection; re-test the connection to resolve")
	}
	path := fmt.Sprintf("/Users/%s/Items/%s?Fields=Overview,Genres,Tags,SortName,ProviderIds,ImageTags,BackdropImageTags,PremiereDate,EndDate,LockedFields", c.userID, platformArtistID)
	var item ArtistDetailItem
	if err := c.Get(ctx, path, &item); err != nil {
		return nil, fmt.Errorf("getting artist detail: %w", err)
	}
	return &connection.ArtistPlatformState{
		Name:          item.Name,
		SortName:      item.SortName,
		Biography:     item.Overview,
		Genres:        item.Genres,
		Tags:          item.Tags,
		PremiereDate:  item.PremiereDate,
		EndDate:       item.EndDate,
		MusicBrainzID: item.ProviderIDs.MusicBrainzArtist,
		HasThumb:      item.ImageTags["Primary"] != "",
		HasFanart:     len(item.BackdropImageTags) > 0,
		BackdropCount: len(item.BackdropImageTags),
		HasLogo:       item.ImageTags["Logo"] != "",
		HasBanner:     item.ImageTags["Banner"] != "",
		IsLocked:      item.LockData,
		LockedFields:  item.LockedFields,
	}, nil
}

// AuthenticateByName authenticates a user against an Emby server using
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
		`Emby UserId="", Client="Stillwater", Device="Server", DeviceId="%s", Version="%s"`,
		deviceID, version.Version,
	))

	// Auth flow targets the same operator-supplied Emby server as the rest of
	// the package (see New() for the LAN/loopback rationale). httpsafe would
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
		logger.Debug("emby authentication successful", "user", result.User.Name)
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
// are active for MusicArtist content.
func (c *Client) GetLibrarySettings(ctx context.Context) ([]LibrarySettingsStatus, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting music libraries: %w", err)
	}

	results := make([]LibrarySettingsStatus, 0, len(libs))
	for i := range libs {
		lib := &libs[i]
		status := LibrarySettingsStatus{
			LibraryID:      lib.ItemID,
			LibraryName:    lib.Name,
			MetadataSavers: lib.LibraryOptions.MetadataSavers,
		}
		for _, opt := range lib.LibraryOptions.TypeOptions {
			if strings.EqualFold(opt.Type, "MusicArtist") {
				status.ImageFetchers = opt.ImageFetchers
				status.MetadataFetchers = opt.MetadataFetchers
				break
			}
		}
		// Normalize nil slices to empty so JSON serializes as [] not null.
		if status.ImageFetchers == nil {
			status.ImageFetchers = []string{}
		}
		if status.MetadataFetchers == nil {
			status.MetadataFetchers = []string{}
		}
		if status.MetadataSavers == nil {
			status.MetadataSavers = []string{}
		}
		// A library has conflicts if any fetcher/saver is active.
		status.HasConflicts = len(status.ImageFetchers) > 0 ||
			len(status.MetadataFetchers) > 0 ||
			len(status.MetadataSavers) > 0
		results = append(results, status)
	}
	return results, nil
}

// DisableConflictingSettings clears image fetchers, metadata fetchers, and metadata savers
// for MusicArtist content in the specified library via the Emby API.
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
	// Clear global metadata savers.
	target.LibraryOptions.MetadataSavers = []string{}

	body, err := json.Marshal(target.LibraryOptions)
	if err != nil {
		return fmt.Errorf("encoding library options: %w", err)
	}

	path := fmt.Sprintf("/Library/VirtualFolders/LibraryOptions?Id=%s", libraryID)
	return wrapAuthIfStatusAuth(c.PostJSON(ctx, path, bytes.NewReader(body), nil))
}

// UpdateArtistLocks persists the given field-level lock list and whole-item lock
// flag to Emby for the named artist. Emby requires a full item payload on
// POST /Items/{id}, so this method first fetches the current item (via the
// user-scoped endpoint) and then POSTs the mutation back with only LockData
// and LockedFields overwritten. Other properties are preserved verbatim to
// avoid clobbering fields Stillwater does not manage.
func (c *Client) UpdateArtistLocks(ctx context.Context, platformArtistID string, lockData bool, lockedFields []string) error {
	if c.userID == "" {
		return fmt.Errorf("no user ID configured for this connection; re-test the connection to resolve")
	}
	// Fetch the full item payload as Emby returns it. Emby's POST /Items/{id}
	// treats the body as a full replacement, so we must preserve unrelated
	// fields to avoid dropping them.
	getPath := fmt.Sprintf("/Users/%s/Items/%s", c.userID, platformArtistID)
	var item map[string]any
	if err := c.Get(ctx, getPath, &item); err != nil {
		return fmt.Errorf("fetching artist for lock update: %w", err)
	}
	// Guard against a JSON null / empty body that would leave item as a
	// nil map; assigning to a nil map panics.
	if item == nil {
		item = make(map[string]any)
	}
	item["LockData"] = lockData
	canonLocks, dropped := canonicalizeLockedFieldsDrops(lockedFields)
	item["LockedFields"] = canonLocks
	for _, d := range dropped {
		c.Logger.Warn("emby: dropping unmappable lock field",
			"artist_id", platformArtistID, "field", d)
	}

	body, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("encoding lock update body: %w", err)
	}
	path := fmt.Sprintf("/Items/%s", platformArtistID)
	if err := c.PostJSON(ctx, path, bytes.NewReader(body), nil); err != nil {
		return fmt.Errorf("posting artist lock update: %w", wrapAuthIfStatusAuth(err))
	}
	return nil
}

// UpdateArtistPath rewrites the Path property on the given Emby artist item.
// Emby's POST /Items/{id} treats the body as a full replacement, so this
// fetches the current item first (via the user-scoped endpoint), mutates only
// the Path field, and POSTs the merged payload back. Same round-trip shape as
// UpdateArtistLocks so unrelated properties survive untouched.
//
// Used by publish.Publisher.SyncRename after a successful directory rename to
// keep the Emby item-to-path mapping consistent (#1222). A path that no
// longer matches a real directory makes Emby drop the item on its next scan,
// which orphans Stillwater's platform_id row; this call avoids that
// reconciliation drift.
func (c *Client) UpdateArtistPath(ctx context.Context, platformArtistID, newPath string) error {
	if strings.TrimSpace(platformArtistID) == "" {
		return fmt.Errorf("platformArtistID is required")
	}
	if strings.TrimSpace(newPath) == "" {
		return fmt.Errorf("newPath is required")
	}
	if c.userID == "" {
		return fmt.Errorf("no user ID configured for this connection; re-test the connection to resolve")
	}
	// PathEscape the platform ID so an ID containing reserved characters
	// (slashes, percent signs, etc.) cannot break out of the URL segment.
	// Jellyfin's push.go already does this; bringing Emby into parity
	// closes the same class of bug here.
	escapedID := url.PathEscape(platformArtistID)
	getPath := fmt.Sprintf("/Users/%s/Items/%s", url.PathEscape(c.userID), escapedID)
	var item map[string]any
	if err := c.Get(ctx, getPath, &item); err != nil {
		return fmt.Errorf("fetching artist for path update: %w", err)
	}
	if item == nil {
		item = make(map[string]any)
	}
	item["Path"] = newPath

	body, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("encoding path update body: %w", err)
	}
	postPath := fmt.Sprintf("/Items/%s", escapedID)
	if err := c.PostJSON(ctx, postPath, bytes.NewReader(body), nil); err != nil {
		return fmt.Errorf("posting artist path update: %w", wrapAuthIfStatusAuth(err))
	}
	return nil
}

// GetArtistPath reads back the peer's CURRENT path for an artist item. Returns
// "" when the peer exposes no path for the item.
//
// On Emby that is the ONLY answer it ever gives for an artist. Two facts were
// established by direct replay against a live Emby 4.9.5.0:
//
//  1. POST /Items/{id} with a Path field returns 204 and SILENTLY DISCARDS the
//     path - the read-back shows the value unchanged. Emby has the identical
//     defect to Jellyfin (both descend from the same codebase), so
//     UpdateArtistPath's success return is NOT evidence the path changed.
//  2. Emby artist items carry no path at all: every MusicArtist in the probed
//     library reported Path: null. Emby artists are VIRTUAL, keyed by name and
//     derived from track tags, rather than folder-backed the way Jellyfin's are.
//
// Consequence for callers: an empty return here is the normal, expected Emby
// answer and means "this peer does not track a path for artists", NOT "the item
// is broken". The relink logic keys Emby off the artist NAME for exactly that
// reason - a path-keyed re-resolve cannot work on a peer that reports no paths.
func (c *Client) GetArtistPath(ctx context.Context, platformArtistID string) (string, error) {
	if strings.TrimSpace(platformArtistID) == "" {
		return "", fmt.Errorf("platformArtistID is required")
	}
	if c.userID == "" {
		return "", fmt.Errorf("no user ID configured for this connection; re-test the connection to resolve")
	}
	getPath := fmt.Sprintf("/Users/%s/Items/%s",
		url.PathEscape(c.userID), url.PathEscape(platformArtistID))
	var item map[string]any
	if err := c.Get(ctx, getPath, &item); err != nil {
		return "", fmt.Errorf("fetching artist for path read-back: %w", wrapAuthIfStatusAuth(err))
	}
	path, _ := item["Path"].(string)
	return path, nil
}

// listArtistsPageLimit bounds each GetArtists page during a full library
// enumeration. See the Jellyfin twin for the rationale; the two peers share the
// AlbumArtists paging surface.
const listArtistsPageLimit = 500

// listArtistsPageCap bounds how many pages one enumeration will walk, so a peer
// that misreports its page count cannot spin this loop forever inside a rename.
const listArtistsPageCap = 200

// ListLibraryArtists enumerates every artist in this peer's MUSIC LIBRARIES,
// with the peer's own path for each (empty on Emby - see GetArtistPath).
//
// Scoped to the music libraries (ParentId) on purpose: the scoping is load-bearing
// for the post-move relink, since an unscoped query also surfaces metadata-only
// ghosts stranded outside every library root, and re-linking to one of those is
// the #2380 corruption itself.
func (c *Client) ListLibraryArtists(ctx context.Context) ([]connection.PeerArtist, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing music libraries: %w", err)
	}
	var out []connection.PeerArtist
	for i := range libs {
		libID := libs[i].ItemID
		if libID == "" {
			continue
		}
		for page := 0; page < listArtistsPageCap; page++ {
			resp, err := c.GetArtists(ctx, libID, page*listArtistsPageLimit, listArtistsPageLimit)
			if err != nil {
				return nil, fmt.Errorf("listing artists in library %s: %w", libID, err)
			}
			for j := range resp.Items {
				it := &resp.Items[j]
				out = append(out, connection.PeerArtist{ID: it.ID, Name: it.Name, Path: it.Path})
			}
			if len(resp.Items) < listArtistsPageLimit {
				break
			}
		}
	}
	return out, nil
}

// embyLockedFieldCanonical maps the lowercase field names Stillwater stores in
// the database to the PascalCase values Emby's MetadataFields enum expects on
// the LockedFields property. Any value not present here is dropped from the
// payload so we never send enum values Emby does not recognize.
var embyLockedFieldCanonical = map[string]string{
	"name": "Name",
	// Stillwater exposes "biography" as the user-facing lock key, but Emby's
	// MetadataFields enum names the underlying storage "Overview" (see the
	// Get path at line 190 which also fetches Fields=Overview). Both aliases
	// map to the same PascalCase value so lock UI using either term syncs.
	"biography": "Overview",
	"overview":  "Overview",
	"genres":    "Genres",
	// Styles and Moods are stored as Tags on Emby's item (see PushMetadata
	// tagItems mapping); lock them as "Tags" on the platform side. Multiple
	// Stillwater keys canonicalize to the same platform value; dedup in
	// canonicalizeLockedFields ensures "Tags" appears only once.
	"styles":              "Tags",
	"moods":               "Tags",
	"cast":                "Cast",
	"productionlocations": "ProductionLocations",
	"tags":                "Tags",
	"studios":             "Studios",
	"images":              "Images",
	"backdrops":           "Backdrops",
	"sortname":            "SortName",
	"sort_name":           "SortName",
	"officialrating":      "OfficialRating",
	"parentalrating":      "ParentalRating",
	"runtime":             "Runtime",
}

// canonicalizeLockedFields converts Stillwater's lowercase locked field names
// into Emby's canonical PascalCase MetadataFields enum values. Unknown names
// are dropped (strict allow-list). The returned slice is always non-nil so
// Emby receives an empty array rather than null when no fields are locked.
func canonicalizeLockedFields(in []string) []string {
	out, _ := canonicalizeLockedFieldsDrops(in)
	return out
}

// canonicalizeLockedFieldsDrops is the diagnostic variant that also returns
// the list of input tokens that had no mapping. Used by the publish path to
// surface mapping gaps via a warn log rather than failing silently.
func canonicalizeLockedFieldsDrops(in []string) (canon []string, dropped []string) {
	canon = make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, f := range in {
		key := strings.ToLower(strings.TrimSpace(f))
		if key == "" {
			continue
		}
		c, ok := embyLockedFieldCanonical[key]
		if !ok {
			dropped = append(dropped, key)
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		canon = append(canon, c)
	}
	return canon, dropped
}

// CheckImageSaverEnabled reports whether Emby will persist artwork files into
// the shared library directory for any music library. Emby controls this with
// the library-wide SaveLocalMetadata flag: when true, artwork is written to
// disk alongside the media (using Emby's own naming convention, which
// duplicates Stillwater's writes under different filenames). Returns true and
// the first matching library's name; no saver found returns (false, "", nil).
// An error from the server is returned rather than swallowed so the caller
// can distinguish "no conflict" from "unable to check."
func (c *Client) CheckImageSaverEnabled(ctx context.Context) (bool, string, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return false, "", fmt.Errorf("checking emby image saver settings: %w", err)
	}
	for i := range libs {
		lib := &libs[i]
		if lib.LibraryOptions.SaveLocalMetadata {
			return true, lib.Name, nil
		}
	}
	return false, "", nil
}

// SnapshotLibraryOptions captures the current SaveLocalMetadata + MetadataSavers
// for every Emby music library. The returned JSON is stored on the connection
// row and replayed verbatim by RestoreLibraryOptions on opt-out. The typed
// GetMusicLibraries call lives in this package because the Emby and Jellyfin
// VirtualFolder shapes diverge slightly; the per-library snapshot envelope
// itself is shared via mediabrowser.BuildSnapshot.
func (c *Client) SnapshotLibraryOptions(ctx context.Context) (string, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return "", fmt.Errorf("getting music libraries for snapshot: %w", err)
	}
	entries := make([]mediabrowser.LibrarySaverSnapshotEntry, 0, len(libs))
	for i := range libs {
		lib := &libs[i]
		savers := lib.LibraryOptions.MetadataSavers
		if savers == nil {
			savers = []string{}
		}
		entries = append(entries, mediabrowser.LibrarySaverSnapshotEntry{
			LibraryID:         lib.ItemID,
			LibraryName:       lib.Name,
			SaveLocalMetadata: lib.LibraryOptions.SaveLocalMetadata,
			MetadataSavers:    savers,
		})
	}
	return mediabrowser.BuildSnapshot(entries)
}

// DisableFileWriteBack clears SaveLocalMetadata on every Emby music library.
// Delegates to mediabrowser since the Emby and Jellyfin /Library/VirtualFolders
// surfaces are identical at the raw-JSON level; the platform argument only
// adjusts the per-call debug log prefix.
func (c *Client) DisableFileWriteBack(ctx context.Context) error {
	return mediabrowser.DisableFileWriteBack(ctx, c, c.Logger, "emby")
}

// RestoreLibraryOptions applies a previously saved snapshot to the Emby peer.
// Delegates to the shared mediabrowser implementation; see that package for the
// raw-JSON round-trip rationale and the null-key sanitization the POST needs.
func (c *Client) RestoreLibraryOptions(ctx context.Context, snapshotJSON string) error {
	return mediabrowser.RestoreLibraryOptions(ctx, c, c.Logger, "emby", snapshotJSON)
}

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("X-Emby-Token", c.APIKey)
}
