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
	for _, f := range folders {
		ct := strings.TrimSpace(strings.ToLower(f.CollectionType))
		include := ct == "music" || ct == ""
		c.Logger.Debug("jellyfin virtual folder discovered", "name", f.Name, "collection_type", f.CollectionType, "included_as_music", include)
		if include {
			music = append(music, f)
		}
	}
	return music, nil
}

// CheckNFOWriterEnabled checks if any Jellyfin music library has an NFO metadata saver enabled.
// Returns true and the library name if found. On error, logs a warning and returns false.
func (c *Client) CheckNFOWriterEnabled(ctx context.Context) (bool, string, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		c.Logger.Warn("could not check jellyfin library options", "error", err)
		return false, "", nil
	}

	for _, lib := range libs {
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
	for _, lib := range libs {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body)) //nolint:gosec // G107: URL is validated by connection.ValidateBaseURL
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

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

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
	for _, lib := range libs {
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
	for i, lib := range libs {
		if lib.ItemID == libraryID {
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
	for _, lib := range libs {
		if lib.LibraryOptions.SaveLocalMetadata {
			return true, lib.Name, nil
		}
	}
	return false, "", nil
}

// LibraryWriteBackSnapshot captures the per-library SaveLocalMetadata and
// MetadataSavers values so Stillwater can restore the peer's prior config
// exactly when the user opts out. See emby.LibraryWriteBackSnapshot for the
// rationale; the shapes are intentionally identical.
type LibraryWriteBackSnapshot struct {
	Version       int                         `json:"version"`
	SnapshottedAt time.Time                   `json:"snapshotted_at"`
	Libraries     []LibrarySaverSnapshotEntry `json:"libraries"`
}

// LibrarySaverSnapshotEntry holds one library's saver state at snapshot time.
type LibrarySaverSnapshotEntry struct {
	LibraryID         string   `json:"library_id"`
	LibraryName       string   `json:"library_name"`
	SaveLocalMetadata bool     `json:"save_local_metadata"`
	MetadataSavers    []string `json:"metadata_savers"`
}

// SnapshotLibraryOptions captures the current saver state for every music
// library so RestoreLibraryOptions can replay it. See emby equivalent for
// design notes.
func (c *Client) SnapshotLibraryOptions(ctx context.Context) (string, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		return "", fmt.Errorf("getting music libraries for snapshot: %w", err)
	}
	snap := LibraryWriteBackSnapshot{
		Version:       1,
		SnapshottedAt: time.Now().UTC(),
		Libraries:     make([]LibrarySaverSnapshotEntry, 0, len(libs)),
	}
	for _, lib := range libs {
		savers := lib.LibraryOptions.MetadataSavers
		if savers == nil {
			savers = []string{}
		}
		snap.Libraries = append(snap.Libraries, LibrarySaverSnapshotEntry{
			LibraryID:         lib.ItemID,
			LibraryName:       lib.Name,
			SaveLocalMetadata: lib.LibraryOptions.SaveLocalMetadata,
			MetadataSavers:    savers,
		})
	}
	buf, err := json.Marshal(snap)
	if err != nil {
		return "", fmt.Errorf("encoding snapshot: %w", err)
	}
	return string(buf), nil
}

// DisableFileWriteBack clears SaveLocalMetadata and MetadataSavers on every
// music library via a lossless raw-JSON round-trip. See the matching Emby
// implementation for the rationale: the Jellyfin LibraryOptions response
// carries many fields our Go struct doesn't model, and PATCHing only the
// modeled subset drops the rest and makes the server error out.
func (c *Client) DisableFileWriteBack(ctx context.Context) error {
	libs, err := c.getMusicLibrariesRaw(ctx)
	if err != nil {
		return fmt.Errorf("getting music libraries: %w", err)
	}
	var firstErr error
	for _, lib := range libs {
		opts := sanitizeLibraryOptions(lib.options)
		// See emby equivalent: SaveLocalMetadata=false is the master kill
		// switch; toggling MetadataSavers alongside triggered a peer crash
		// on some library shapes, so we leave it alone.
		opts["SaveLocalMetadata"] = false
		if err := c.postLibraryOptionsRaw(ctx, lib.id, opts); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			c.Logger.Warn("disabling file write-back failed for library", "library", lib.name, "error", err)
		}
	}
	return firstErr
}

func sanitizeLibraryOptions(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if v == nil {
			continue
		}
		out[k] = v
	}
	return out
}

// RestoreLibraryOptions replays a snapshot onto the peer using the raw-JSON
// overlay approach. See emby equivalent for design notes.
func (c *Client) RestoreLibraryOptions(ctx context.Context, snapshotJSON string) error {
	var snap LibraryWriteBackSnapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snap); err != nil {
		return fmt.Errorf("decoding snapshot: %w", err)
	}
	if snap.Version != 1 {
		return fmt.Errorf("unsupported snapshot version %d", snap.Version)
	}
	libs, err := c.getMusicLibrariesRaw(ctx)
	if err != nil {
		return fmt.Errorf("getting music libraries: %w", err)
	}
	byID := make(map[string]rawMusicLibrary, len(libs))
	for _, lib := range libs {
		byID[lib.id] = lib
	}
	var firstErr error
	for _, entry := range snap.Libraries {
		lib, ok := byID[entry.LibraryID]
		if !ok {
			c.Logger.Warn("snapshot library missing on peer; skipping", "library_id", entry.LibraryID, "library_name", entry.LibraryName)
			continue
		}
		opts := lib.options
		opts["SaveLocalMetadata"] = entry.SaveLocalMetadata
		savers := entry.MetadataSavers
		if savers == nil {
			savers = []string{}
		}
		opts["MetadataSavers"] = savers
		if err := c.postLibraryOptionsRaw(ctx, lib.id, opts); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			c.Logger.Warn("restoring library options failed", "library", lib.name, "error", err)
		}
	}
	return firstErr
}

type rawMusicLibrary struct {
	id      string
	name    string
	options map[string]any
}

func (c *Client) getMusicLibrariesRaw(ctx context.Context) ([]rawMusicLibrary, error) {
	var folders []map[string]any
	if err := c.Get(ctx, "/Library/VirtualFolders", &folders); err != nil {
		return nil, fmt.Errorf("getting virtual folders: %w", err)
	}
	var out []rawMusicLibrary
	for _, f := range folders {
		collectionType, _ := f["CollectionType"].(string)
		name, _ := f["Name"].(string)
		id, _ := f["ItemId"].(string)
		locs, _ := f["Locations"].([]any)
		paths := make([]string, 0, len(locs))
		for _, v := range locs {
			if s, ok := v.(string); ok {
				paths = append(paths, s)
			}
		}
		ct := strings.TrimSpace(strings.ToLower(collectionType))
		include := ct == "music" || ct == ""
		c.Logger.Debug("jellyfin virtual folder discovered", "name", name, "collection_type", collectionType, "paths", paths, "included_as_music", include)
		if !include {
			continue
		}
		opts, _ := f["LibraryOptions"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		out = append(out, rawMusicLibrary{id: id, name: name, options: opts})
	}
	return out, nil
}

// postLibraryOptionsRaw wraps the options map in a LibraryOptionsInfo
// envelope before POSTing. Jellyfin's /Library/VirtualFolders/LibraryOptions
// endpoint inherits the same contract as Emby's (see emby.postLibraryOptionsRaw
// for the full rationale): it requires the wrapper and performs a full
// REPLACE on LibraryOptions, so the caller must pass every field from the
// original GET.
func (c *Client) postLibraryOptionsRaw(ctx context.Context, libraryID string, opts map[string]any) error {
	wrapper := map[string]any{
		"Id":             libraryID,
		"LibraryOptions": opts,
	}
	body, err := json.Marshal(wrapper)
	if err != nil {
		return fmt.Errorf("encoding library options: %w", err)
	}
	c.Logger.Debug("jellyfin library options POST", "library_id", libraryID, "body", string(body))
	path := fmt.Sprintf("/Library/VirtualFolders/LibraryOptions?Id=%s", libraryID)
	return c.PostJSON(ctx, path, bytes.NewReader(body), nil)
}

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token="%s"`, c.APIKey))
}
