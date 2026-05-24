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
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/httpclient"
	"github.com/sydlexius/stillwater/internal/connection/mediabrowser"
	"github.com/sydlexius/stillwater/internal/version"
)

// ErrInvalidCredentials is returned when the media server rejects the credentials.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Client communicates with a Jellyfin server.
type Client struct {
	httpclient.BaseClient
	userID string
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
	c := &Client{
		BaseClient: httpclient.NewBase(baseURL, apiKey, httpClient, logger, "jellyfin"),
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
	if err := c.Get(ctx, "/Library/VirtualFolders", &folders); err != nil {
		return nil, fmt.Errorf("getting virtual folders: %w", err)
	}

	var music []VirtualFolder
	for i := range folders {
		f := &folders[i]
		ct := strings.TrimSpace(strings.ToLower(f.CollectionType))
		include := ct == "music" || ct == ""
		c.Logger.Debug("jellyfin virtual folder discovered", "name", f.Name, "collection_type", f.CollectionType, "included_as_music", include)
		if include {
			music = append(music, *f)
		}
	}
	return music, nil
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

	var results []ImageFetcherStatus
	for i := range libs {
		lib := &libs[i]
		// If internet providers are globally disabled for this library,
		// image fetchers are inactive regardless of TypeOptions.
		if !lib.LibraryOptions.EnableInternetProviders {
			continue
		}
		for _, opt := range lib.LibraryOptions.TypeOptions {
			if !strings.EqualFold(opt.Type, "MusicArtist") {
				continue
			}
			if len(opt.ImageFetchers) > 0 {
				// Jellyfin can replace existing images and strip EXIF
				// provenance data, so the risk level is always "critical".
				results = append(results, ImageFetcherStatus{
					LibraryName:  lib.Name,
					LibraryID:    lib.ItemID,
					FetcherNames: opt.ImageFetchers,
					RiskLevel:    "critical",
				})
			}
		}
	}
	return results, nil
}

// GetArtists returns album artists from a specific library with pagination.
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
		return fmt.Errorf("triggering library scan: %w", err)
	}
	return nil
}

// TriggerArtistRefresh refreshes metadata for a specific artist.
func (c *Client) TriggerArtistRefresh(ctx context.Context, artistID string) error {
	path := fmt.Sprintf("/Items/%s/Refresh", artistID)
	if err := c.Post(ctx, path, nil); err != nil {
		return fmt.Errorf("triggering artist refresh: %w", err)
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
		IsLocked:      item.IsLocked,
		LockedFields:  item.LockedFields,
	}, nil
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
// are active for MusicArtist content. The NeedsLockdata field indicates whether NFO lockdata
// injection is required because Jellyfin ignores MetadataSavers=[] for NFO writes.
func (c *Client) GetLibrarySettings(ctx context.Context) ([]LibrarySettingsStatus, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting music libraries: %w", err)
	}

	results := make([]LibrarySettingsStatus, 0, len(libs))
	for i := range libs {
		lib := &libs[i]
		status := LibrarySettingsStatus{
			LibraryID:               lib.ItemID,
			LibraryName:             lib.Name,
			MetadataSavers:          lib.LibraryOptions.MetadataSavers,
			EnableInternetProviders: lib.LibraryOptions.EnableInternetProviders,
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
		// Jellyfin MetadataSavers=[] does NOT stop NFO writes. The only reliable
		// method is <lockdata>true</lockdata> in the NFO file itself.
		status.NeedsLockdata = true
		// A library has conflicts if fetchers are active (when internet providers are enabled).
		status.HasConflicts = status.EnableInternetProviders &&
			(len(status.ImageFetchers) > 0 || len(status.MetadataFetchers) > 0)
		results = append(results, status)
	}
	return results, nil
}

// DisableConflictingSettings clears image fetchers and metadata fetchers for
// MusicArtist content in the specified library via the Jellyfin API.
// Note: this does NOT disable NFO writes because Jellyfin ignores MetadataSavers=[];
// NFO protection requires lockdata injection (handled separately by the NFO writer).
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
	return c.PostJSON(ctx, path, bytes.NewReader(body), nil)
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
	for i := range libs {
		lib := &libs[i]
		if lib.LibraryOptions.SaveLocalMetadata {
			return true, lib.Name, nil
		}
	}
	return false, "", nil
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
	existing, err := c.fetchItem(ctx, platformArtistID)
	if err != nil {
		return fmt.Errorf("fetching artist for path update: %w", err)
	}
	existing["Path"] = newPath
	return c.postFullItem(ctx, platformArtistID, existing, "path update")
}

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token="%s"`, c.APIKey))
}
