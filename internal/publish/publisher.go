// Package publish provides a unified abstraction for publishing artist metadata
// and images to external platforms (Emby, Jellyfin) and local NFO files.
// It replaces the previously scattered pattern of ad-hoc writeBackNFO,
// asyncPushMetadataToConnections, and syncImageToPlatforms calls.
package publish

import (
	"context"
	"errors"
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
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
)

// pushOpLockToggle is the operation slug emitted on connection.push_failed
// events from PushLocks.
const pushOpLockToggle = "lock_toggle"

// pushOpMetadataPush is the operation slug emitted on connection.push_failed
// events from PushMetadataAsync. Toast subscribers can filter by this slug
// to distinguish a metadata-write failure from a lock-toggle failure.
const pushOpMetadataPush = "metadata_push"

// pushOpImageUpload is the operation slug emitted on connection.push_failed
// events from SyncImageToPlatforms and SyncAllFanartToPlatforms. Toast
// subscribers can filter by this slug to distinguish an image-upload failure
// from a metadata-write or lock-toggle failure.
const pushOpImageUpload = "image_upload"

const (
	// pushTimeout is the per-connection timeout for async metadata pushes.
	pushTimeout = 30 * time.Second
	// maxWarningRunes caps warning strings to prevent oversized JSON responses.
	maxWarningRunes = 200
)

// Deps holds all dependencies for a Publisher.
type Deps struct {
	ArtistService      artistPlatformLister
	ArtistGetter       artistGetter
	ConnectionService  connectionGetter
	LibraryService     libraryResolver
	NFOSnapshotService *nfo.SnapshotService
	NFOSettingsService *nfo.NFOSettingsService
	PlatformService    namingConfigProvider
	ExpectedWrites     expectedWritesTracker
	ImageCacheDir      string
	Logger             *slog.Logger
	// Notifier surfaces fire-and-forget goroutine errors (per-connection
	// push failures) so the operator can see them as SSE-driven toasts.
	// Optional; nil leaves notification disabled and the goroutine logs
	// the error as before.
	Notifier Notifier
	// ImageWriteGate gates artwork writes via the conflict ledger. Optional;
	// when nil the background reconciler proceeds without conflict checking and
	// logs a one-time warning. Set via SetImageWriteGate after construction.
	ImageWriteGate ImageWriteGate
}

// Notifier reports per-connection push failures from detached goroutines.
// The publish package depends on this narrow interface rather than the
// concrete event.Bus so tests can use a slice-backed fake without pulling
// in the bus's start/stop lifecycle.
//
// artistID / artistName / operation give the toast enough context to
// disambiguate a single failure from a fan-out (one PushLocks call can
// produce N failures for the same artist if N platforms reject the
// write); operation is a stable slug ("lock_toggle" today) so future push
// surfaces can be filtered without churning the interface.
type Notifier interface {
	NotifyConnectionPushFailed(connectionName, errorClass, artistID, artistName, operation string, err error)
}

// Publisher coordinates writing artist metadata and images to local files
// (NFO) and external platform connections (Emby/Jellyfin). All operations
// are best-effort: errors are logged but never propagated to the caller,
// since the primary operation (DB update) has already succeeded.
type Publisher struct {
	artistService      artistPlatformLister
	artistGetter       artistGetter
	connectionService  connectionGetter
	libraryService     libraryResolver
	nfoSnapshotService *nfo.SnapshotService
	nfoSettingsService *nfo.NFOSettingsService
	platformService    namingConfigProvider
	expectedWrites     expectedWritesTracker
	imageCacheDir      string
	logger             *slog.Logger
	notifier           Notifier
	imageWriteGate     ImageWriteGate
}

// Narrow interfaces keep the publish package decoupled from concrete types.

type artistPlatformLister interface {
	GetPlatformIDs(ctx context.Context, artistID string) ([]artist.PlatformID, error)
	ListMembersByArtistID(ctx context.Context, artistID string) ([]artist.BandMember, error)
	ListArtistsWithPlatformMappings(ctx context.Context) ([]string, error)
}

// artistGetter loads a full artist record by ID for the background reconciler.
type artistGetter interface {
	GetByID(ctx context.Context, id string, opts ...artist.HydrateOpts) (*artist.Artist, error)
}

// ImageWriteGate gates image writes via the conflict ledger. Implemented by
// *conflict.Gate; kept as a narrow interface so the publish package does not
// import internal/conflict.
type ImageWriteGate interface {
	AllowImageWrite(ctx context.Context) error
}

type connectionGetter interface {
	GetByID(ctx context.Context, id string) (*connection.Connection, error)
}

// libraryResolver looks up the library that owns an artist's filesystem path
// so the publisher can apply per-library NFO settings (NFOLockData today,
// possibly more per-library NFO knobs later). Implemented by *library.Service.
// Returning nil + nil err is the "no owning library" case -- the publisher
// falls back to default (off) behavior.
type libraryResolver interface {
	FindForArtistPath(ctx context.Context, artistPath string) (*library.Library, error)
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
		artistGetter:       d.ArtistGetter,
		connectionService:  d.ConnectionService,
		libraryService:     d.LibraryService,
		nfoSnapshotService: d.NFOSnapshotService,
		nfoSettingsService: d.NFOSettingsService,
		platformService:    d.PlatformService,
		expectedWrites:     d.ExpectedWrites,
		imageCacheDir:      d.ImageCacheDir,
		logger:             d.Logger,
		notifier:           d.Notifier,
		imageWriteGate:     d.ImageWriteGate,
	}
}

// SetImageWriteGate wires the conflict gate used by the background artwork
// reconciler. Call this after construction once the gate is available (the
// gate is created inside api.NewRouter which runs after publish.New).
func (p *Publisher) SetImageWriteGate(gate ImageWriteGate) {
	if p != nil {
		p.imageWriteGate = gate
	}
}

// notifyPushFailure forwards a per-connection goroutine error to the
// configured Notifier when one is wired up. Safe to call with a nil
// publisher field; tests that omit Notifier still exercise the error
// path through the logger without panicking on the nil check.
func (p *Publisher) notifyPushFailure(connectionName, errorClass, artistID, artistName, operation string, err error) {
	if p == nil || p.notifier == nil {
		return
	}
	p.notifier.NotifyConnectionPushFailed(connectionName, errorClass, artistID, artistName, operation, err)
}

// ResolveLockNFO returns the effective <lockdata> value for the artist: the
// OR of the per-artist lock flag and the per-library NFOLockData setting
// (issue #1726). Either knob being on stamps lockdata=true; both off leaves
// it absent. The library lookup is best-effort and defaults to false on
// error so a transient DB hiccup never silently flips the lock bit on.
//
// Exported so the rule package (NFOExistsFixer) can call into the same
// resolver the publisher uses; keeping the logic in one place is the entire
// point of the refactor.
func (p *Publisher) ResolveLockNFO(ctx context.Context, a *artist.Artist) bool {
	if a == nil {
		return false
	}
	if a.Locked {
		return true
	}
	if p == nil || p.libraryService == nil || a.Path == "" {
		return false
	}
	lib, err := p.libraryService.FindForArtistPath(ctx, a.Path)
	if err != nil {
		p.logger.Warn("resolving owning library for NFO lock setting; defaulting to off",
			slog.String("artist_id", a.ID),
			slog.String("error", err.Error()),
		)
		return false
	}
	if lib == nil {
		return false
	}
	return lib.NFOLockData
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
	if _, err := os.Stat(nfoPath); err != nil {
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

	// Read the current NFO field map to apply platform-specific element mapping.
	fm := nfo.DefaultFieldMap()
	if p.nfoSettingsService != nil {
		var fmErr error
		fm, fmErr = p.nfoSettingsService.GetFieldMap(ctx)
		if fmErr != nil {
			p.logger.Warn("reading NFO field map, using default",
				slog.String("artist_id", a.ID),
				slog.String("error", fmErr.Error()),
			)
			fm = nfo.DefaultFieldMap()
		}
	}

	lockNFO := p.ResolveLockNFO(ctx, a)
	if err := nfo.WriteBackArtistNFOWithFieldMap(ctx, a, p.nfoSnapshotService, p.logger, fm, lockNFO); err != nil {
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

	// Best-effort fetch of the artist's band members so the platform push
	// can map them to Jellyfin's People array. A failure here is logged but
	// does not abort the push -- the member list is enrichment, not
	// authoritative metadata.
	members, memberErr := p.artistService.ListMembersByArtistID(ctx, a.ID)
	if memberErr != nil {
		p.logger.Warn("auto-push: listing band members",
			slog.String("artist_id", a.ID),
			slog.String("error", memberErr.Error()))
		members = nil
	}

	// a is a freshly-allocated struct from GetByID with no shared mutable
	// references; reading its fields from goroutines is safe.
	data := BuildArtistPushData(a, members)

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
				// Mirror the PushLocks lookup-failure notify path so the
				// metadata push surface emits the same toast taxonomy.
				// shortConnLabel falls back to an 8-char id prefix the
				// operator can correlate against the settings page
				// connection list -- it matches the connection_id prefix
				// shown in the "auto-push: fetching connection" error log
				// above, so a toast can be cross-referenced to the log
				// entry without exposing the full UUID. classifyPushErr
				// translates the lookup failure into a stable category;
				// connErr is always non-nil in this branch so the empty
				// return from classifyPushErr is unreachable here.
				p.notifyPushFailure(shortConnLabel(pid.ConnectionID), classifyPushErr(connErr), a.ID, artistDisplayName(a), pushOpMetadataPush, connErr)
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
				// Same notify path as PushLocks: classifyPushErr translates
				// the raw transport / status error into the stable taxonomy
				// (auth_failed, timeout, server_error, ...) so the toast
				// tells the operator what kind of intervention is needed.
				p.notifyPushFailure(conn.Name, classifyPushErr(pushErr), a.ID, artistDisplayName(a), pushOpMetadataPush, pushErr)
			} else {
				p.logger.Info("auto-push: metadata pushed",
					slog.String("artist_id", a.ID),
					slog.String("artist_name", a.Name),
					slog.String("connection", conn.Name))
			}
		}()
	}
}

// PushLocks synchronizes only the artist's lock state (whole-item flag and
// per-field list) to every connected platform. This is called from the lock
// toggle handlers so Emby/Jellyfin reflect the pin immediately without
// requiring a manual push. Critically, it does NOT go through PushMetadata:
// sending LockData on every metadata write would cause the platforms to
// re-scrape unlocked items and can replace existing images with provider
// results.
func (p *Publisher) PushLocks(ctx context.Context, a *artist.Artist) {
	if p == nil {
		return
	}
	platformIDs, err := p.artistService.GetPlatformIDs(ctx, a.ID)
	if err != nil {
		p.logger.Error("lock-push: listing platform IDs",
			slog.String("artist_id", a.ID),
			slog.String("error", err.Error()))
		return
	}
	if len(platformIDs) == 0 {
		return
	}

	locked := a.Locked
	fields := append([]string(nil), a.LockedFields...)

	for _, pid := range platformIDs {
		go func() {
			gCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
			defer cancel()
			defer func() {
				if v := recover(); v != nil {
					p.logger.Error("lock-push: panic in goroutine",
						slog.String("artist_id", a.ID),
						slog.String("connection_id", pid.ConnectionID),
						slog.Any("panic", v),
						slog.String("stack", string(debug.Stack())))
				}
			}()

			conn, connErr := p.connectionService.GetByID(gCtx, pid.ConnectionID)
			if connErr != nil {
				p.logger.Error("lock-push: fetching connection",
					slog.String("artist_id", a.ID),
					slog.String("connection_id", pid.ConnectionID),
					slog.String("error", connErr.Error()))
				// No connection name available here. Publisher has no
				// access to api.connectionIndex (it lives in package api
				// and would create an import cycle), so fall back to a
				// short UUID prefix the operator can recognize against
				// the settings page connection list.
				// Route the lookup failure through classifyPushErr so the
				// toast surface uses the same stable taxonomy as the push
				// itself; a free-form "connection lookup failed" string
				// bypasses any client-side mapping/localization. The "" ->
				// "rejected" fallback only triggers if classifyPushErr
				// returns empty (nil err or unmatched), which is a defense
				// in depth rather than an expected branch on this path.
				class := classifyPushErr(connErr)
				if class == "" {
					class = "rejected"
				}
				p.notifyPushFailure(shortConnLabel(pid.ConnectionID), class, a.ID, artistDisplayName(a), pushOpLockToggle, connErr)
				return
			}
			if !conn.Enabled {
				p.logger.Debug("lock-push: skipping disabled connection",
					slog.String("artist_id", a.ID),
					slog.String("connection", conn.Name))
				return
			}

			syncer := newLockSyncer(conn, p.logger)
			if syncer == nil {
				p.logger.Debug("lock-push: connection type does not support lock sync",
					slog.String("artist_id", a.ID),
					slog.String("connection", conn.Name),
					slog.String("type", conn.Type))
				return
			}
			if err := syncer.UpdateArtistLocks(gCtx, pid.PlatformArtistID, locked, fields); err != nil {
				p.logger.Error("lock-push: update failed",
					slog.String("artist_id", a.ID),
					slog.String("connection", conn.Name),
					slog.String("error", err.Error()))
				// classifyPushErr converts the raw transport / status
				// error into a stable taxonomy (auth_failed, timeout,
				// unreachable, ...) so the toast tells the operator what
				// kind of intervention is needed instead of collapsing
				// every failure to "lock sync failed".
				p.notifyPushFailure(conn.Name, classifyPushErr(err), a.ID, artistDisplayName(a), pushOpLockToggle, err)
			} else {
				p.logger.Info("lock-push: locks synchronized",
					slog.String("artist_id", a.ID),
					slog.String("connection", conn.Name),
					slog.Bool("locked", locked),
					slog.Int("field_count", len(fields)))
			}
		}()
	}
}

// newLockSyncer constructs a LockSyncer for the given connection type.
// Returns nil for connection types that do not support lock updates.
func newLockSyncer(conn *connection.Connection, logger *slog.Logger) connection.LockSyncer {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	default:
		return nil
	}
}

// LockSyncClientFactory returns the production factory used by the
// connection.LockSync platform-pull scheduler (issue #1726 Part C). Lives
// here so cmd/stillwater/main.go does not need to import the emby /
// jellyfin sub-packages directly, and the connection package does not
// need to import them (which would form an import cycle).
func LockSyncClientFactory() connection.LockSyncClientFactory {
	return func(conn *connection.Connection, logger *slog.Logger) connection.ArtistStateGetter {
		if conn == nil {
			return nil
		}
		switch conn.Type {
		case connection.TypeEmby:
			return emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
		case connection.TypeJellyfin:
			return jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
		default:
			return nil
		}
	}
}

// SyncImageToPlatforms uploads the specified image type to every platform
// connection that has a stored artist ID mapping. Errors are logged and
// returned as warning strings so the caller can surface them to the client.
// The local operation already succeeded, so failures here are non-fatal.
func (p *Publisher) SyncImageToPlatforms(ctx context.Context, a *artist.Artist, imageType string) []string {
	return p.syncImageToPlatforms(ctx, a, imageType, false)
}

// syncImageToPlatforms is the internal implementation of SyncImageToPlatforms.
// When respectWriteGate is true, connections without FeatureImageWrite are skipped;
// this is used by the background reconciler only. User-initiated callers pass false
// so all enabled connections receive the push regardless of the per-connection toggle.
func (p *Publisher) syncImageToPlatforms(ctx context.Context, a *artist.Artist, imageType string, respectWriteGate bool) []string {
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
			p.notifyPushFailure(shortConnLabel(pid.ConnectionID), classifyPushErr(connErr), a.ID, artistDisplayName(a), pushOpImageUpload, connErr)
			continue
		}
		if !conn.Enabled || conn.Status != "ok" || (respectWriteGate && !conn.GetFeatureImageWrite()) {
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
			p.notifyPushFailure(conn.Name, classifyPushErr(uploadErr), a.ID, artistDisplayName(a), pushOpImageUpload, uploadErr)
		}
	}
	return warnings
}

// SyncAllFanartToPlatforms uploads all local fanart files to every connected
// platform at their respective indices. Unlike SyncImageToPlatforms which only
// syncs the primary image, this discovers all fanart files and uploads each one
// at the correct backdrop index. Errors are logged and returned as warnings.
func (p *Publisher) SyncAllFanartToPlatforms(ctx context.Context, a *artist.Artist) []string {
	return p.syncAllFanartToPlatforms(ctx, a, false)
}

// syncAllFanartToPlatforms is the internal implementation of SyncAllFanartToPlatforms.
// When respectWriteGate is true, connections without FeatureImageWrite are skipped;
// this is used by the background reconciler only.
func (p *Publisher) syncAllFanartToPlatforms(ctx context.Context, a *artist.Artist, respectWriteGate bool) []string {
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
			p.notifyPushFailure(shortConnLabel(pid.ConnectionID), classifyPushErr(connErr), a.ID, artistDisplayName(a), pushOpImageUpload, connErr)
			continue
		}
		if !conn.Enabled || conn.Status != "ok" || (respectWriteGate && !conn.GetFeatureImageWrite()) {
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
				p.notifyPushFailure(conn.Name, classifyPushErr(uploadErr), a.ID, artistDisplayName(a), pushOpImageUpload, uploadErr)
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
		return emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	default:
		return nil
	}
}

// newIndexedImageUploader constructs an IndexedImageUploader for the given
// connection type. Returns nil for unsupported types.
func newIndexedImageUploader(conn *connection.Connection, logger *slog.Logger) connection.IndexedImageUploader {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
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

// classifyPushErr maps a push error to a stable user-facing class so the
// toast surface can tell auth from network from timeout. The taxonomy is
// intentionally small: each class is something the operator can act on
// (re-auth, check network, retry later) rather than a one-to-one mirror
// of every Go error type. The fallback "rejected" covers anything that
// doesn't pattern-match so a future provider error path can't surface as
// an empty string.
//
// String matching is necessary because the platform clients (emby,
// jellyfin, lidarr) currently wrap raw "HTTP %d" errors rather than
// exposing sentinel types; the test suite locks in the substring contract
// so a client refactor cannot silently break the taxonomy.
func classifyPushErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "dial tcp"):
		return "unreachable"
	case strings.Contains(msg, "status 401"),
		strings.Contains(msg, "status 403"),
		strings.Contains(msg, "HTTP 401"),
		strings.Contains(msg, "HTTP 403"):
		return "auth_failed"
	case strings.Contains(msg, "status 404"),
		strings.Contains(msg, "HTTP 404"):
		return "not_found"
	case strings.Contains(msg, "status 5"),
		strings.Contains(msg, "HTTP 5"):
		return "server_error"
	default:
		return "rejected"
	}
}

// shortConnLabel formats an unknown-connection-id fallback used when the
// publisher cannot resolve a connection name (the GetByID hop itself
// failed). Eight hex chars are enough for the operator to correlate with
// the settings page connection list without dumping the full UUID into a
// toast.
func shortConnLabel(connectionID string) string {
	if connectionID == "" {
		return "unknown connection"
	}
	short := connectionID
	if len(short) > 8 {
		short = short[:8]
	}
	return "unknown connection (id=" + short + ")"
}

// artistDisplayName returns the artist's user-facing label for toast
// context, falling back to the ID when the name is empty so the operator
// always has something to correlate.
func artistDisplayName(a *artist.Artist) string {
	if a == nil {
		return ""
	}
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}
