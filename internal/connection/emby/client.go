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
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/httpclient"
	"github.com/sydlexius/stillwater/internal/version"
)

// ErrInvalidCredentials is returned when the media server rejects the credentials.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Client communicates with an Emby server.
type Client struct {
	httpclient.BaseClient
	userID string
}

// New creates an Emby client with default HTTP settings.
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

// GetMusicLibraries returns virtual folders with CollectionType "music".
func (c *Client) GetMusicLibraries(ctx context.Context) ([]VirtualFolder, error) {
	var folders []VirtualFolder
	if err := c.Get(ctx, "/Library/VirtualFolders", &folders); err != nil {
		return nil, fmt.Errorf("getting virtual folders: %w", err)
	}

	var music []VirtualFolder
	for _, f := range folders {
		if strings.EqualFold(f.CollectionType, "music") {
			music = append(music, f)
		}
	}
	return music, nil
}

// CheckNFOWriterEnabled checks if any Emby music library has an NFO metadata saver enabled.
// Returns true and the library name if found. On error, logs a warning and returns false.
func (c *Client) CheckNFOWriterEnabled(ctx context.Context) (bool, string, error) {
	libs, err := c.GetMusicLibraries(ctx)
	if err != nil {
		c.Logger.Warn("could not check emby library options", "error", err)
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
	for _, lib := range libs {
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
		`Emby UserId="", Client="Stillwater", Device="Server", DeviceId="%s", Version="%s"`,
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
	for _, lib := range libs {
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
	// Clear global metadata savers.
	target.LibraryOptions.MetadataSavers = []string{}

	body, err := json.Marshal(target.LibraryOptions)
	if err != nil {
		return fmt.Errorf("encoding library options: %w", err)
	}

	path := fmt.Sprintf("/Library/VirtualFolders/LibraryOptions?Id=%s", libraryID)
	return c.PostJSON(ctx, path, bytes.NewReader(body), nil)
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
	item["LockData"] = lockData
	item["LockedFields"] = canonicalizeLockedFields(lockedFields)

	body, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("encoding lock update body: %w", err)
	}
	path := fmt.Sprintf("/Items/%s", platformArtistID)
	if err := c.PostJSON(ctx, path, bytes.NewReader(body), nil); err != nil {
		return fmt.Errorf("posting artist lock update: %w", err)
	}
	return nil
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
	"biography":           "Overview",
	"overview":            "Overview",
	"genres":              "Genres",
	"cast":                "Cast",
	"productionlocations": "ProductionLocations",
	"tags":                "Tags",
	"studios":             "Studios",
	"images":              "Images",
	"backdrops":           "Backdrops",
	"sortname":            "SortName",
	"officialrating":      "OfficialRating",
	"parentalrating":      "ParentalRating",
	"runtime":             "Runtime",
}

// canonicalizeLockedFields converts Stillwater's lowercase locked field names
// into Emby's canonical PascalCase MetadataFields enum values. Unknown names
// are dropped (strict allow-list). The returned slice is always non-nil so
// Emby receives an empty array rather than null when no fields are locked.
func canonicalizeLockedFields(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, f := range in {
		key := strings.ToLower(strings.TrimSpace(f))
		if key == "" {
			continue
		}
		canon, ok := embyLockedFieldCanonical[key]
		if !ok {
			continue
		}
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		out = append(out, canon)
	}
	return out
}

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("X-Emby-Token", c.APIKey)
}
