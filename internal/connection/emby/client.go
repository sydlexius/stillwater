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
	for _, f := range folders {
		ct := strings.TrimSpace(strings.ToLower(f.CollectionType))
		include := ct == "music" || ct == ""
		c.Logger.Debug("emby virtual folder discovered", "name", f.Name, "collection_type", f.CollectionType, "included_as_music", include)
		if include {
			music = append(music, f)
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
	for _, lib := range libs {
		if lib.LibraryOptions.SaveLocalMetadata {
			return true, lib.Name, nil
		}
	}
	return false, "", nil
}

// LibraryWriteBackSnapshot captures just the fields Stillwater mutates when
// DisableFileWriteBack runs: SaveLocalMetadata + MetadataSavers per music
// library. On restore, Stillwater GETs the current library options from the
// peer and overlays these fields only, so unrelated options that changed
// after the snapshot are preserved. Version bumps if the snapshot shape
// ever needs to evolve.
type LibraryWriteBackSnapshot struct {
	Version       int                         `json:"version"`
	SnapshottedAt time.Time                   `json:"snapshotted_at"`
	Libraries     []LibrarySaverSnapshotEntry `json:"libraries"`
}

// LibrarySaverSnapshotEntry holds the per-library saver state captured for
// restore. LibraryName is informational only (for UI) and is not used during
// restore; LibraryID is the authoritative key.
type LibrarySaverSnapshotEntry struct {
	LibraryID         string   `json:"library_id"`
	LibraryName       string   `json:"library_name"`
	SaveLocalMetadata bool     `json:"save_local_metadata"`
	MetadataSavers    []string `json:"metadata_savers"`
}

// SnapshotLibraryOptions captures the current SaveLocalMetadata + MetadataSavers
// for every music library. The returned JSON is stored on the connection row and
// replayed verbatim by RestoreLibraryOptions on opt-out.
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
// music library on the peer. Uses a lossless raw-JSON round-trip:
// Emby's LibraryOptions response contains dozens of fields our Go struct
// does not model, and PATCHing back with only the three fields we care
// about makes Emby crash ("Object reference not set to an instance of an
// object", i.e. a .NET NullReferenceException because required options
// are now nil). Instead we GET each library's full options as a raw JSON
// map, mutate only the two keys we intentionally change, and POST the
// merged map back.
func (c *Client) DisableFileWriteBack(ctx context.Context) error {
	libs, err := c.getMusicLibrariesRaw(ctx)
	if err != nil {
		return fmt.Errorf("getting music libraries: %w", err)
	}
	var firstErr error
	for _, lib := range libs {
		opts := sanitizeLibraryOptions(lib.options)
		// SaveLocalMetadata=false is the master switch: when off, Emby
		// will neither save artwork nor invoke any MetadataSaver, so we
		// deliberately leave MetadataSavers untouched. Mutating it
		// alongside the flag crashed Emby with a NullReferenceException
		// on some library shapes -- the peer API appears to expect the
		// saver list to stay consistent with SaveLocalMetadata.
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

// sanitizeLibraryOptions drops keys whose values are null in the raw map.
// Emby's POST handler treats some fields as non-nullable and throws when
// they arrive as explicit nulls (the GET response happily returns them
// that way). Dropping null keys before serialization lets Emby fill in
// defaults for those fields instead of NullReferenceException-ing on them.
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

// RestoreLibraryOptions applies a previously saved snapshot to the peer. For
// each library in the snapshot it GETs the current options as raw JSON,
// overlays only SaveLocalMetadata + MetadataSavers, and POSTs back. See
// DisableFileWriteBack for why the raw-map approach is mandatory.
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

// rawMusicLibrary is the lossless shape used by DisableFileWriteBack and
// RestoreLibraryOptions. Options is the library's full LibraryOptions JSON
// object from the peer, preserving every field our Go struct does not
// model. Mutating only the keys we intentionally change keeps Emby happy.
type rawMusicLibrary struct {
	id      string
	name    string
	options map[string]any
}

// getMusicLibrariesRaw fetches /Library/VirtualFolders as an array of
// arbitrary JSON objects and returns each music library's ItemId + Name +
// full LibraryOptions map. A library counts as "music" for conflict
// detection if its CollectionType is explicitly "music" OR is empty (some
// Emby/Jellyfin installations leave the type blank for mixed or legacy
// libraries). Every candidate is logged at debug so users can verify which
// libraries the conflict detector is considering.
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
		include := strings.EqualFold(collectionType, "music") || strings.TrimSpace(collectionType) == ""
		c.Logger.Debug("emby virtual folder discovered", "name", name, "collection_type", collectionType, "paths", paths, "included_as_music", include)
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

// postLibraryOptionsRaw serializes a LibraryOptionsInfo wrapper around the
// given options map and POSTs it to Emby.
//
// Emby's /Library/VirtualFolders/LibraryOptions endpoint refuses a bare
// LibraryOptions body with an opaque 500 "Object reference not set to an
// instance of an object." Empirically (verified against Emby 4.x via a
// throwaway diagnostic) the endpoint requires the LibraryOptionsInfo
// envelope {"Id": <libraryID>, "LibraryOptions": {...}}. The inner map
// must include every field the peer originally returned; omitted fields
// are silently dropped from the peer's config because the endpoint
// performs a full REPLACE on LibraryOptions rather than a merge.
//
// Callers are therefore expected to pass the full options map from a
// GET, mutate only the fields they mean to change, and let this helper
// wrap and POST. The logged body preserves diagnostic value when future
// peer-version drift breaks the endpoint again.
func (c *Client) postLibraryOptionsRaw(ctx context.Context, libraryID string, opts map[string]any) error {
	wrapper := map[string]any{
		"Id":             libraryID,
		"LibraryOptions": opts,
	}
	body, err := json.Marshal(wrapper)
	if err != nil {
		return fmt.Errorf("encoding library options: %w", err)
	}
	c.Logger.Debug("emby library options POST", "library_id", libraryID, "body", string(body))
	path := fmt.Sprintf("/Library/VirtualFolders/LibraryOptions?Id=%s", libraryID)
	return c.PostJSON(ctx, path, bytes.NewReader(body), nil)
}

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("X-Emby-Token", c.APIKey)
}
