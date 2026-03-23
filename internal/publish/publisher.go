// Package publish provides a unified abstraction for publishing artist metadata
// and images to external platforms (Emby, Jellyfin) and local NFO files.
// It replaces the previously scattered pattern of ad-hoc writeBackNFO,
// asyncPushMetadataToConnections, and syncImageToPlatforms calls.
package publish

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
)

const (
	// pushTimeout is the per-connection timeout for async metadata pushes.
	pushTimeout = 30 * time.Second
	// maxWarningRunes caps warning strings to prevent oversized JSON responses.
	maxWarningRunes = 200
)

// Deps holds all dependencies for a Publisher.
type Deps struct {
	ArtistService      artistPlatformLister
	ConnectionService  connectionGetter
	NFOSnapshotService *nfo.SnapshotService
	PlatformService    namingConfigProvider
	ExpectedWrites     expectedWritesTracker
	ImageCacheDir      string
	Logger             *slog.Logger
}

// Publisher coordinates writing artist metadata and images to local files
// (NFO) and external platform connections (Emby/Jellyfin). All operations
// are best-effort: errors are logged but never propagated to the caller,
// since the primary operation (DB update) has already succeeded.
type Publisher struct {
	artistService      artistPlatformLister
	connectionService  connectionGetter
	nfoSnapshotService *nfo.SnapshotService
	platformService    namingConfigProvider
	expectedWrites     expectedWritesTracker
	imageCacheDir      string
	logger             *slog.Logger
}

// Narrow interfaces keep the publish package decoupled from concrete types.

type artistPlatformLister interface {
	GetPlatformIDs(ctx context.Context, artistID string) ([]artist.PlatformID, error)
}

type connectionGetter interface {
	GetByID(ctx context.Context, id string) (*connection.Connection, error)
}

type namingConfigProvider interface {
	GetActive(ctx context.Context) (*platform.Profile, error)
}

type expectedWritesTracker interface {
	Add(path string)
	Remove(path string)
}

// New creates a Publisher from the given dependencies.
func New(d Deps) *Publisher {
	return &Publisher{
		artistService:      d.ArtistService,
		connectionService:  d.ConnectionService,
		nfoSnapshotService: d.NFOSnapshotService,
		platformService:    d.PlatformService,
		expectedWrites:     d.ExpectedWrites,
		imageCacheDir:      d.ImageCacheDir,
		logger:             d.Logger,
	}
}

// PublishMetadata writes the artist's NFO file and pushes metadata to all
// connected platforms. This is the primary convenience method that closes
// the gap between NFO-only writes and full platform synchronization.
func (p *Publisher) PublishMetadata(ctx context.Context, a *artist.Artist) {
	if p == nil {
		return
	}
	p.WriteBackNFO(ctx, a)
	p.PushMetadataAsync(ctx, a)
}

// WriteBackNFO writes the artist's current metadata to its artist.nfo file
// (best effort). Skips silently when the artist has no filesystem path or no
// existing NFO file on disk -- creating new NFOs from scratch is the rule
// engine's job. The on-disk check (os.Stat) guards against stale NFOExists
// flags when the file has been deleted or moved since the last scan.
func (p *Publisher) WriteBackNFO(ctx context.Context, a *artist.Artist) {
	if p == nil {
		return
	}
	if a.Path == "" {
		return
	}
	nfoPath := filepath.Join(a.Path, "artist.nfo")
	if _, err := os.Stat(nfoPath); err != nil { //nolint:gosec // G703: path constructed from DB artist record, not user input
		if os.IsNotExist(err) {
			return
		}
		p.logger.Warn("NFO write-back stat error",
			slog.String("artist_id", a.ID),
			slog.String("nfo_path", nfoPath),
			slog.String("error", err.Error()),
		)
		return
	}

	// Register expected write so the filesystem watcher does not treat
	// this write-back as an external modification.
	if p.expectedWrites != nil {
		p.expectedWrites.Add(nfoPath)
		defer p.expectedWrites.Remove(nfoPath)
	}

	if err := nfo.WriteBackArtistNFO(ctx, a, p.nfoSnapshotService, p.logger); err != nil {
		p.logger.Error("NFO write-back failed",
			slog.String("artist_id", a.ID),
			slog.String("artist_name", a.Name),
			slog.String("error", err.Error()),
		)
	}
}

// PushMetadataAsync pushes the artist's current metadata to all connected
// platforms (Emby/Jellyfin) in background goroutines. Each goroutine creates
// its own context with an explicit timeout so the push outlives the HTTP
// response without blocking it.
func (p *Publisher) PushMetadataAsync(ctx context.Context, a *artist.Artist) {
	if p == nil {
		return
	}
	platformIDs, err := p.artistService.GetPlatformIDs(ctx, a.ID)
	if err != nil {
		p.logger.Error("auto-push: listing platform IDs",
			slog.String("artist_id", a.ID),
			slog.String("error", err.Error()))
		return
	}
	if len(platformIDs) == 0 {
		return
	}

	// a is a freshly-allocated struct from GetByID with no shared mutable
	// references; reading its fields from goroutines is safe.
	data := BuildArtistPushData(a)

	for _, pid := range platformIDs {
		go func() {
			gCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
			defer cancel()
			defer func() {
				if v := recover(); v != nil {
					p.logger.Error("auto-push: panic in goroutine",
						slog.String("artist_id", a.ID),
						slog.String("connection_id", pid.ConnectionID),
						slog.Any("panic", v),
						slog.String("stack", string(debug.Stack())))
				}
			}()

			conn, connErr := p.connectionService.GetByID(gCtx, pid.ConnectionID)
			if connErr != nil {
				p.logger.Error("auto-push: fetching connection",
					slog.String("artist_id", a.ID),
					slog.String("connection_id", pid.ConnectionID),
					slog.String("error", connErr.Error()))
				return
			}
			if !conn.Enabled {
				return
			}

			pusher, ok := NewMetadataPusher(conn, p.logger)
			if !ok {
				return // connection type does not support PushMetadata (e.g. Lidarr)
			}

			if pushErr := pusher.PushMetadata(gCtx, pid.PlatformArtistID, data); pushErr != nil {
				p.logger.Error("auto-push: metadata push failed",
					slog.String("artist_id", a.ID),
					slog.String("artist_name", a.Name),
					slog.String("connection", conn.Name),
					slog.String("error", pushErr.Error()))
			} else {
				p.logger.Info("auto-push: metadata pushed",
					slog.String("artist_id", a.ID),
					slog.String("artist_name", a.Name),
					slog.String("connection", conn.Name))
			}
		}()
	}
}

// SyncImageToPlatforms uploads the specified image type to every platform
// connection that has a stored artist ID mapping. Errors are logged and
// returned as warning strings so the caller can surface them to the client.
// The local operation already succeeded, so failures here are non-fatal.
func (p *Publisher) SyncImageToPlatforms(ctx context.Context, a *artist.Artist, imageType string) []string {
	if p == nil {
		return nil
	}
	warnings := make([]string, 0)

	platformIDs, err := p.artistService.GetPlatformIDs(ctx, a.ID)
	if err != nil {
		p.logger.Error("getting platform IDs for image sync", "artist_id", a.ID, "type", imageType, "error", err)
		warnings = append(warnings, "platform sync skipped: failed to load platform mappings")
		return warnings
	}
	if len(platformIDs) == 0 {
		return warnings
	}

	dir := p.ImageDir(a)
	if dir == "" {
		p.logger.Warn("skipping platform image sync: artist has no image directory", "artist", a.Name, "type", imageType)
		warnings = append(warnings, "platform sync skipped: artist has no image directory configured")
		return warnings
	}
	patterns := p.getActiveNamingConfig(ctx, imageType)
	filePath, found := img.FindExistingImage(dir, patterns)
	if !found {
		warnings = append(warnings, "platform sync skipped: no local image found to upload")
		return warnings
	}

	data, readErr := os.ReadFile(filePath) //nolint:gosec // path from trusted naming patterns
	if readErr != nil {
		p.logger.Error("reading image for platform sync", "artist", a.Name, "type", imageType, "path", filePath, "error", readErr)
		warnings = append(warnings, "platform sync skipped: failed to read image for upload")
		return warnings
	}

	ct := "image/jpeg"
	if strings.EqualFold(filepath.Ext(filePath), ".png") {
		ct = "image/png"
	}

	for _, pid := range platformIDs {
		conn, connErr := p.connectionService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			p.logger.Error("getting connection for image sync", "connection_id", pid.ConnectionID, "error", connErr)
			warnings = append(warnings, truncateWarning(fmt.Sprintf("connection %s: failed to load", pid.ConnectionID)))
			continue
		}
		if !conn.Enabled || conn.Status != "ok" {
			p.logger.Debug("skipping connection for image sync", "connection", conn.Name, "type", imageType, "status", conn.Status)
			continue
		}

		uploader := newImageUploader(conn, p.logger)
		if uploader == nil {
			p.logger.Warn("unsupported connection type for image sync", "type", conn.Type)
			warnings = append(warnings, truncateWarning(fmt.Sprintf("%s: unsupported connection type %q", conn.Name, conn.Type)))
			continue
		}

		if uploadErr := uploader.UploadImage(ctx, pid.PlatformArtistID, imageType, data, ct); uploadErr != nil {
			p.logger.Error("syncing image to platform", "artist", a.Name, "connection", conn.Name, "type", imageType, "error", uploadErr)
			warnings = append(warnings, truncateWarning(fmt.Sprintf("%s (%s): image upload failed", conn.Name, conn.Type)))
		}
	}
	return warnings
}

// SyncAllFanartToPlatforms uploads all local fanart files to every connected
// platform at their respective indices. Unlike SyncImageToPlatforms which only
// syncs the primary image, this discovers all fanart files and uploads each one
// at the correct backdrop index. Errors are logged and returned as warnings.
func (p *Publisher) SyncAllFanartToPlatforms(ctx context.Context, a *artist.Artist) []string {
	if p == nil {
		return nil
	}
	warnings := make([]string, 0)

	platformIDs, err := p.artistService.GetPlatformIDs(ctx, a.ID)
	if err != nil {
		p.logger.Error("getting platform IDs for fanart sync",
			slog.String("artist_id", a.ID),
			slog.String("error", err.Error()))
		warnings = append(warnings, "platform sync skipped: failed to load platform mappings")
		return warnings
	}
	if len(platformIDs) == 0 {
		return warnings
	}

	dir := p.ImageDir(a)
	if dir == "" {
		p.logger.Warn("skipping platform fanart sync: artist has no image directory",
			slog.String("artist", a.Name))
		warnings = append(warnings, "platform sync skipped: artist has no image directory configured")
		return warnings
	}

	primary := p.getActiveFanartPrimary(ctx)
	fanartPaths, discoverErr := img.DiscoverFanart(dir, primary)
	if discoverErr != nil {
		p.logger.Error("discovering fanart for platform sync",
			slog.String("artist_id", a.ID),
			slog.String("error", discoverErr.Error()))
		warnings = append(warnings, "platform sync skipped: failed to read fanart directory")
		return warnings
	}
	if len(fanartPaths) == 0 {
		return warnings
	}

	for _, pid := range platformIDs {
		conn, connErr := p.connectionService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			p.logger.Error("getting connection for fanart sync",
				slog.String("connection_id", pid.ConnectionID),
				slog.String("error", connErr.Error()))
			warnings = append(warnings, truncateWarning(fmt.Sprintf("connection %s: failed to load", pid.ConnectionID)))
			continue
		}
		if !conn.Enabled || conn.Status != "ok" {
			p.logger.Debug("skipping connection for fanart sync",
				slog.String("connection", conn.Name),
				slog.String("status", conn.Status))
			continue
		}

		indexedUploader := newIndexedImageUploader(conn, p.logger)
		if indexedUploader == nil {
			p.logger.Warn("unsupported connection type for fanart sync",
				slog.String("type", conn.Type))
			warnings = append(warnings, truncateWarning(fmt.Sprintf("%s: unsupported connection type %q", conn.Name, conn.Type)))
			continue
		}

		for i, fp := range fanartPaths {
			data, readErr := os.ReadFile(fp) //nolint:gosec // path from trusted fanart discovery
			if readErr != nil {
				p.logger.Error("reading fanart for platform sync",
					slog.String("path", fp),
					slog.String("error", readErr.Error()))
				warnings = append(warnings, truncateWarning(fmt.Sprintf("%s: failed to read fanart %d", conn.Name, i)))
				continue
			}

			ct := "image/jpeg"
			if strings.EqualFold(filepath.Ext(fp), ".png") {
				ct = "image/png"
			}

			if uploadErr := indexedUploader.UploadImageAtIndex(ctx, pid.PlatformArtistID, "fanart", i, data, ct); uploadErr != nil {
				p.logger.Error("syncing fanart to platform",
					slog.String("artist", a.Name),
					slog.String("connection", conn.Name),
					slog.Int("index", i),
					slog.String("error", uploadErr.Error()))
				warnings = append(warnings, truncateWarning(fmt.Sprintf("%s (%s): fanart %d upload failed", conn.Name, conn.Type, i)))
			}
		}
	}
	return warnings
}

// SetImageCacheDir updates the fallback image cache directory. This is used
// by tests that configure the cache dir after Publisher construction.
func (p *Publisher) SetImageCacheDir(dir string) {
	if p != nil {
		p.imageCacheDir = dir
	}
}

// ImageDir returns the directory where images for this artist should be
// stored and served from. Uses the artist's filesystem path if available,
// otherwise falls back to the managed cache directory.
func (p *Publisher) ImageDir(a *artist.Artist) string {
	if a.Path != "" {
		return a.Path
	}
	if p.imageCacheDir != "" && a.ID != "" {
		return filepath.Join(p.imageCacheDir, a.ID)
	}
	return ""
}

// getActiveNamingConfig returns the filenames configured for the given image
// type in the active platform profile, falling back to defaults.
func (p *Publisher) getActiveNamingConfig(ctx context.Context, imageType string) []string {
	if p.platformService == nil {
		return img.FileNamesForType(img.DefaultFileNames, imageType)
	}
	profile, err := p.platformService.GetActive(ctx)
	if err != nil || profile == nil {
		return img.FileNamesForType(img.DefaultFileNames, imageType)
	}
	names := profile.ImageNaming.NamesForType(imageType)
	if len(names) == 0 {
		return img.FileNamesForType(img.DefaultFileNames, imageType)
	}
	return names
}

// getActiveFanartPrimary returns the primary fanart filename from the active
// platform profile, falling back to the default.
func (p *Publisher) getActiveFanartPrimary(ctx context.Context) string {
	if p.platformService == nil {
		return img.PrimaryFileName(img.DefaultFileNames, "fanart")
	}
	profile, err := p.platformService.GetActive(ctx)
	if err != nil || profile == nil {
		return img.PrimaryFileName(img.DefaultFileNames, "fanart")
	}
	name := profile.ImageNaming.PrimaryName("fanart")
	if name == "" {
		return img.PrimaryFileName(img.DefaultFileNames, "fanart")
	}
	return name
}

// newImageUploader constructs an ImageUploader for the given connection type.
// Returns nil for connection types that do not support image upload.
func newImageUploader(conn *connection.Connection, logger *slog.Logger) connection.ImageUploader {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, logger)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, logger)
	default:
		return nil
	}
}

// newIndexedImageUploader constructs an IndexedImageUploader for the given
// connection type. Returns nil for unsupported types.
func newIndexedImageUploader(conn *connection.Connection, logger *slog.Logger) connection.IndexedImageUploader {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.PlatformUserID, logger)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.PlatformUserID, logger)
	default:
		return nil
	}
}

// truncateWarning caps a warning string at maxWarningRunes runes.
func truncateWarning(msg string) string {
	if runes := []rune(msg); len(runes) > maxWarningRunes {
		return string(runes[:maxWarningRunes]) + " (truncated)"
	}
	return msg
}
