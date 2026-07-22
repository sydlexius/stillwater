// Package publish provides a unified abstraction for publishing artist metadata
// and images to external platforms (Emby, Jellyfin) and local NFO files.
// It replaces the previously scattered pattern of ad-hoc writeBackNFO,
// asyncPushMetadataToConnections, and syncImageToPlatforms calls.
package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/collision"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/filesystem"
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
	ArtistService artistPlatformLister
	// ArtistLister is the narrow paged-listing dependency used by
	// ScanPlatformBackdropDuplicates to walk the whole library. Kept as its
	// own explicit field rather than a runtime type assertion on
	// ArtistService so a future decorator around *artist.Service that
	// satisfies artistPlatformLister but not List fails to compile instead
	// of failing at scan time (#2540 review).
	ArtistLister       artistPageLister
	ArtistGetter       artistGetter
	ConnectionService  connectionGetter
	LibraryService     libraryResolver
	NFOSnapshotService *nfo.SnapshotService
	NFOSettingsService *nfo.NFOSettingsService
	PlatformService    activeProfileProvider
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
//
// connectionID is the connection UUID included in the SSE payload so the
// frontend can deep-link to the edit panel for that specific connection
// (e.g. /settings?tab=connections&edit=<id>&focus=api_key for auth failures).
type Notifier interface {
	NotifyConnectionPushFailed(connectionID, connectionName, errorClass, artistID, artistName, operation string, err error)
}

// Publisher coordinates writing artist metadata and images to local files
// (NFO) and external platform connections (Emby/Jellyfin). All operations
// are best-effort: errors are logged but never propagated to the caller,
// since the primary operation (DB update) has already succeeded.
type Publisher struct {
	artistService      artistPlatformLister
	artistLister       artistPageLister
	artistGetter       artistGetter
	connectionService  connectionGetter
	libraryService     libraryResolver
	nfoSnapshotService *nfo.SnapshotService
	nfoSettingsService *nfo.NFOSettingsService
	platformService    activeProfileProvider
	expectedWrites     expectedWritesTracker
	imageCacheDir      string
	logger             *slog.Logger
	notifier           Notifier
	collisionNotifier  *collision.Notifier
	fanartIdentity     FanartIdentityIndexer
	imageWriteGate     ImageWriteGate

	// phashTargetLocks serializes the complete read-modify-verify of a single
	// phash backdrop target (ConnectionID+PlatformArtistID) across concurrent
	// delete/restore calls, so two duplicate operations on the SAME platform
	// item cannot interleave their resolve->mutate->verify and race into a
	// double delete or a duplicate upload. See lockPhashTarget in
	// phash_platform.go. Keyed lazily; entries are cheap and never removed.
	phashTargetLocks sync.Map
}

// Narrow interfaces keep the publish package decoupled from concrete types.

type artistPlatformLister interface {
	GetPlatformIDs(ctx context.Context, artistID string) ([]artist.PlatformID, error)
	ListMembersByArtistID(ctx context.Context, artistID string) ([]artist.BandMember, error)
	ListArtistsWithPlatformMappings(ctx context.Context) ([]string, error)
	// SetPlatformIDStable upserts an artist<->connection platform-ID mapping via
	// the divergence-aware, deterministic stable set. Used by the Lidarr
	// merge/rename self-heal to stamp a resolved-by-MBID link without silently
	// clobbering an existing divergent id (#2344). Returns an outcome the caller
	// logs on divergence, and an error the caller inspects for logging; the
	// self-heal caller treats a non-nil error as best-effort and does not
	// propagate it further up the stack (it skips the connection and continues).
	SetPlatformIDStable(ctx context.Context, artistID, connectionID, platformArtistID string) (artist.PlatformIDStableOutcome, error)
	// SetPlatformID upserts a link AUTHORITATIVELY, overwriting whatever is
	// there. Used by the post-move relink (#2380) and ONLY by it: that caller
	// just moved the directory and then read the peer's own item back at the new
	// path, so unlike the scan-time resolvers it is not guessing and must not be
	// routed through SetPlatformIDStable -- the stable set deliberately keeps an
	// existing divergent mapping, which is precisely the stale row the relink
	// exists to replace.
	SetPlatformID(ctx context.Context, artistID, connectionID, platformArtistID string) error
	// NOTE: there is deliberately NO DeletePlatformID here, and adding one back
	// would re-open #2380's worst regression.
	//
	// The publisher's rename path cannot obtain the evidence a drop requires. A
	// peer's artist listing is a snapshot of an index the peer rebuilds
	// ASYNCHRONOUSLY, so within the rename's budget "the item is gone" and "the item
	// is mid-scan" are indistinguishable -- and the scan is the far likelier of the
	// two, since it takes minutes and the budget is seconds. Two versions of this
	// code inferred staleness here anyway (once from the timeout, once from the
	// peer's library roots) and both DELETED GOOD LINKS, unrecoverably: nothing
	// re-stamps a dropped link automatically.
	//
	// Withholding the capability is what makes that a TYPE-LEVEL guarantee instead
	// of a policy the next patch can quietly undo. Dropping belongs where the
	// evidence lives: the merge path, which holds the loser's link outright, and the
	// background reconciler (#2426), which can let the peer settle for minutes and
	// then re-resolve. Those callers declare their own capability; this one does not
	// get it.
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
	// ListByType returns all connections of the given type. Used by the Lidarr
	// merge/rename self-heal to enumerate Lidarr connections for resolve-by-MBID.
	ListByType(ctx context.Context, connType string) ([]connection.Connection, error)
}

// libraryResolver looks up the library that owns an artist's filesystem path
// so the publisher can apply per-library NFO settings (NFOLockData today,
// possibly more per-library NFO knobs later). Implemented by *library.Service.
// Returning nil + nil err is the "no owning library" case -- the publisher
// falls back to default (off) behavior.
type libraryResolver interface {
	FindForArtistPath(ctx context.Context, artistPath string) (*library.Library, error)
}

// activeProfileProvider resolves the active platform profile. The publisher uses
// it for image-naming config and to gate NFO write-back on the profile's
// NFOEnabled flag (#2306). *platform.Service satisfies it.
type activeProfileProvider interface {
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
		artistLister:       d.ArtistLister,
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

// FanartIdentityIndexer builds the cross-artist fanart phash registry that the
// #2540 collision check compares a candidate backdrop against.
// *artist.Service implements it. It is a dependency separate from the
// publisher's narrow artistService interface so wiring it does not force every
// artistPlatformLister implementation (including test fakes) to grow the method.
type FanartIdentityIndexer interface {
	BuildFanartIdentityIndex(ctx context.Context) ([]img.FanartIdentityEntry, error)
}

// SetCollisionNotifier wires the #2540 cross-artist backdrop-collision notifier
// and the registry builder it compares against, used by the outbound fanart
// sync. Set after construction because the notifier depends on the rule service,
// which is built alongside the publisher. Both are optional: a nil notifier is
// safe (Notify no-ops on a nil receiver) and a nil indexer disables the outbound
// collision check entirely (fail-open, never a blocked push).
func (p *Publisher) SetCollisionNotifier(n *collision.Notifier, idx FanartIdentityIndexer) {
	if p != nil {
		p.collisionNotifier = n
		p.fanartIdentity = idx
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
// connectionID is the raw UUID for the deep-link affordance; it may be ""
// when the connection record could not be loaded (the toast falls back to
// name-only display in that case).
func (p *Publisher) notifyPushFailure(connectionID, connectionName, errorClass, artistID, artistName, operation string, err error) {
	if p == nil || p.notifier == nil {
		return
	}
	p.notifier.NotifyConnectionPushFailed(connectionID, connectionName, errorClass, artistID, artistName, operation, err)
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
	nfoWritten := p.WriteBackNFO(ctx, a)

	// The two platform operations touch the SAME Emby/Jellyfin item and MUST be
	// ordered, not raced (#2336 review P2). Two failure modes if they run as
	// unordered goroutines:
	//   (a) Emby rejects one of two simultaneous /Items/{id}/Refresh calls
	//       ("refresh already in progress"); if the destructive re-import loses,
	//       the NFO-only fields silently never reach the platform.
	//   (b) The push path's own non-destructive refresh (emby refreshItem)
	//       persists Emby's IN-MEMORY item back to the NFO. That in-memory item
	//       lacks Disambiguation/YearsActive (the API cannot carry them), so if
	//       it lands AFTER the destructive re-import it CLOBBERS the on-disk NFO
	//       Stillwater just wrote -- dropping the two fields at the source.
	// Fix: run the API push + its non-destructive refresh FIRST, wait for it to
	// settle, THEN fire the destructive FullRefresh re-import so it always
	// re-reads the correct on-disk NFO last. This ordering is deterministic and
	// does not rely on Emby's concurrent-refresh semantics.
	//
	// P3: skip the destructive re-import entirely when no NFO was written this
	// publish (artist has no library path, or the active profile disables NFO).
	// With no fresh local NFO an opted-in Emby FullRefresh could re-scrape from
	// online fetchers and clobber the platform metadata.
	//
	// The sequence runs in a detached coordinator goroutine so the HTTP response
	// is not blocked on the push timeout; context.WithoutCancel lets the work
	// outlive the originating request.
	detached := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				p.logger.Error("publish-metadata: panic in sequencing goroutine",
					slog.String("artist_id", a.ID),
					slog.Any("panic", v),
					slog.String("stack", string(debug.Stack())))
			}
		}()
		// Blocks until every per-connection push (POST /Items/{id} + its
		// non-destructive refresh) has completed.
		p.pushMetadata(detached, a, true)
		if !nfoWritten {
			p.logger.Debug("publish-metadata: skipping destructive NFO re-import; no NFO written this publish",
				slog.String("artist_id", a.ID))
			return
		}
		p.RefreshArtistOnPlatforms(detached, a)
	}()
}

// WriteBackNFO writes the artist's current metadata to its artist.nfo file
// (best effort). When no NFO exists on disk it CREATES one from the artist's
// metadata (#2306: Stillwater's contract is to manage the NFO); when one exists
// it is rewritten in place. Both are gated by the active platform profile --
// Plex (nfo_enabled=0) does not use .nfo files, so the write is skipped (logged,
// not silent). Skips when the artist has no filesystem path. The on-disk check
// (os.Stat) guards against stale NFOExists flags when the file was deleted or
// moved since the last scan.
//
// Returns true when an artist.nfo was created or rewritten on disk this call,
// and false on every early return or write failure. PublishMetadata uses this
// to gate the destructive NFO re-import (#2336 review P3): the re-import must
// only fire when a fresh local NFO exists for the platform to re-read.
func (p *Publisher) WriteBackNFO(ctx context.Context, a *artist.Artist) bool {
	if p == nil {
		return false
	}
	if a.Path == "" {
		return false
	}
	nfoPath := filepath.Join(a.Path, "artist.nfo")

	_, statErr := os.Stat(nfoPath)
	missing := os.IsNotExist(statErr)
	if statErr != nil && !missing {
		p.logger.Warn("NFO write-back stat error",
			slog.String("artist_id", a.ID),
			slog.String("nfo_path", nfoPath),
			slog.String("error", statErr.Error()),
		)
		return false
	}

	// #2306: honor the active platform profile. Plex does not use .nfo files, so
	// neither create nor rewrite one. Fail-open when the profile can't be
	// resolved (see platform.NFOWriteAllowed).
	if p.platformService != nil {
		prof, profErr := p.platformService.GetActive(ctx)
		if !platform.NFOWriteAllowed(prof, profErr) {
			// prof is non-nil here (NFOWriteAllowed fails open on a nil profile or
			// a lookup error), but guard defensively so logging never depends on
			// that non-local invariant.
			profileName := "unknown"
			if prof != nil {
				profileName = prof.Name
			}
			p.logger.Info("NFO write-back skipped: NFO writing is disabled for the active platform profile",
				slog.String("artist_id", a.ID),
				slog.String("profile", profileName))
			return false
		}
	}

	// Register expected write so the filesystem watcher does not treat
	// this write-back as an external modification.
	if p.expectedWrites != nil {
		p.expectedWrites.Add(nfoPath)
		defer p.expectedWrites.Remove(nfoPath)
	}

	fm := p.resolveNFOFieldMap(ctx, a)
	lockNFO := p.ResolveLockNFO(ctx, a)

	if missing {
		// #2306: create a new artist.nfo from the artist's current metadata,
		// using the same field-map + lockdata shaping the rule fixer applies.
		nfoData := nfo.FromArtistWithFieldMap(a, fm)
		nfoData.LockData = lockNFO
		// Stamp provenance so an external overwrite can be detected on read,
		// matching the rewrite path (WriteBackArtistNFOWithFieldMap).
		nfoData.Stillwater = &nfo.StillwaterMeta{
			Version: nfo.StillwaterVersion,
			Written: time.Now().UTC().Format(time.RFC3339),
		}
		var buf bytes.Buffer
		if err := nfo.Write(&buf, nfoData); err != nil {
			p.logger.Error("generating NFO for create",
				slog.String("artist_id", a.ID),
				slog.String("error", err.Error()),
			)
			return false
		}
		if err := filesystem.WriteFileAtomic(nfoPath, buf.Bytes(), 0o644); err != nil {
			p.logger.Error("creating NFO",
				slog.String("artist_id", a.ID),
				slog.String("nfo_path", nfoPath),
				slog.String("error", err.Error()),
			)
			return false
		}
		// Keep the in-memory artist consistent with the file just written so a
		// caller that returns `a` directly (e.g. handleLockArtist) reports
		// nfo_exists=true.
		a.NFOExists = true
		return true
	}

	if err := nfo.WriteBackArtistNFOWithFieldMap(ctx, a, p.nfoSnapshotService, p.logger, fm, lockNFO); err != nil {
		p.logger.Error("NFO write-back failed",
			slog.String("artist_id", a.ID),
			slog.String("artist_name", a.Name),
			slog.String("error", err.Error()),
		)
		return false
	}
	return true
}

// resolveNFOFieldMap reads the configured NFO field map for platform-specific
// element mapping, falling back to the default when no settings service is
// wired or the read fails.
func (p *Publisher) resolveNFOFieldMap(ctx context.Context, a *artist.Artist) nfo.NFOFieldMap {
	if p.nfoSettingsService == nil {
		return nfo.DefaultFieldMap()
	}
	fm, err := p.nfoSettingsService.GetFieldMap(ctx)
	if err != nil {
		p.logger.Warn("reading NFO field map, using default",
			slog.String("artist_id", a.ID),
			slog.String("error", err.Error()),
		)
		return nfo.DefaultFieldMap()
	}
	return fm
}

// PushMetadataAsync pushes the artist's current metadata to all connected
// platforms (Emby/Jellyfin) in background goroutines. Each goroutine creates
// its own context with an explicit timeout so the push outlives the HTTP
// response without blocking it.
func (p *Publisher) PushMetadataAsync(ctx context.Context, a *artist.Artist) {
	if p == nil {
		return
	}
	p.pushMetadata(ctx, a, false)
}

// pushMetadata is the shared core behind PushMetadataAsync and the sequenced
// PublishMetadata path. It dispatches one push goroutine per platform mapping.
// When wait is true it blocks until every push goroutine (POST /Items/{id} plus
// its non-destructive refresh) has returned; when false it returns immediately.
// PublishMetadata uses wait=true so the destructive NFO re-import can be fired
// strictly AFTER the push and its refresh have settled (#2336 review P2).
func (p *Publisher) pushMetadata(ctx context.Context, a *artist.Artist, wait bool) {
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

	var wg sync.WaitGroup
	for _, pid := range platformIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.pushMetadataToConnection(ctx, a, pid, data)
		}()
	}
	if wait {
		wg.Wait()
	}
}

// pushMetadataToConnection performs the metadata push for a single platform
// mapping. It creates its own detached, timeout-bounded context so the push
// outlives the originating request, and recovers from panics so one bad
// connection cannot crash the fan-out. Best-effort: failures are logged and
// surfaced as toasts, never propagated.
func (p *Publisher) pushMetadataToConnection(ctx context.Context, a *artist.Artist, pid artist.PlatformID, data connection.ArtistPushData) {
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
		p.notifyPushFailure(pid.ConnectionID, shortConnLabel(pid.ConnectionID), classifyPushErr(connErr), a.ID, artistDisplayName(a), pushOpMetadataPush, connErr)
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
		p.notifyPushFailure(pid.ConnectionID, conn.Name, classifyPushErr(pushErr), a.ID, artistDisplayName(a), pushOpMetadataPush, pushErr)
	} else {
		p.logger.Info("auto-push: metadata pushed",
			slog.String("artist_id", a.ID),
			slog.String("artist_name", a.Name),
			slog.String("connection", conn.Name))
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
				p.notifyPushFailure(pid.ConnectionID, shortConnLabel(pid.ConnectionID), class, a.ID, artistDisplayName(a), pushOpLockToggle, connErr)
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
				p.notifyPushFailure(pid.ConnectionID, conn.Name, classifyPushErr(err), a.ID, artistDisplayName(a), pushOpLockToggle, err)
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
	snapMod := fileModTime(filePath)

	ct := "image/jpeg"
	if strings.EqualFold(filepath.Ext(filePath), ".png") {
		ct = "image/png"
	}

	// Peers this push HANDED THE IMAGE TO, named in the restore log below.
	//
	// Deliberately recorded BEFORE the upload result is known, not after a
	// success. A peer that deletes the file and THEN fails the request (500, a
	// context deadline, a reset connection) is the variant most likely to lose
	// data, and gating the repair on upload success skipped exactly that case.
	var uploadedTo []string

	for _, pid := range platformIDs {
		conn, connErr := p.connectionService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			p.logger.Error("getting connection for image sync", "connection_id", pid.ConnectionID, "error", connErr)
			warnings = append(warnings, truncateWarning(fmt.Sprintf("connection %s: failed to load", pid.ConnectionID)))
			p.notifyPushFailure(pid.ConnectionID, shortConnLabel(pid.ConnectionID), classifyPushErr(connErr), a.ID, artistDisplayName(a), pushOpImageUpload, connErr)
			continue
		}
		if !conn.Enabled || conn.Status != "ok" || (respectWriteGate && !conn.GetFeatureImageWrite()) {
			p.logger.Debug("skipping connection for image sync", "connection", conn.Name, "type", imageType, "status", conn.Status)
			continue
		}

		// #2698: the #2533 pre-push write-back disable used to live here and is
		// removed. Measured on Emby 4.10, the peer deletes the operator's library
		// file with its image saver either ON or OFF, so the pre-disable prevented
		// nothing and cost a round trip per push. The post-upload re-assertion
		// below covers both outcomes the peer can produce -- a missing file and
		// altered bytes -- which is why the disable is no longer needed.
		uploader := newImageUploader(conn, p.logger)
		if uploader == nil {
			p.logger.Warn("unsupported connection type for image sync", "type", conn.Type)
			warnings = append(warnings, truncateWarning(fmt.Sprintf("%s: unsupported connection type %q", conn.Name, conn.Type)))
			continue
		}

		uploadedTo = append(uploadedTo, conn.Name)
		if uploadErr := uploader.UploadImage(ctx, pid.PlatformArtistID, imageType, data, ct); uploadErr != nil {
			p.logger.Error("syncing image to platform", "artist", a.Name, "connection", conn.Name, "type", imageType, "error", uploadErr)
			warnings = append(warnings, truncateWarning(fmt.Sprintf("%s (%s): image upload failed", conn.Name, conn.Type)))
			p.notifyPushFailure(pid.ConnectionID, conn.Name, classifyPushErr(uploadErr), a.ID, artistDisplayName(a), pushOpImageUpload, uploadErr)
		}
	}

	// #2698: the local file is AUTHORITATIVE and the platform copy is DERIVED, so
	// the invariant after a push is simply that what we uploaded is still on disk,
	// byte for byte. A peer can violate it: Emby deletes the operator's library
	// file during the very upload it was handed (proven in production, twice).
	//
	// TIMING, stated as measured rather than as a guarantee: in every observed
	// case the peer's delete completed BEFORE UploadImage returned (204ms before,
	// in the production capture on the issue), so this check lands behind the
	// damage. That is an observation about two processes with no synchronization
	// between them, NOT a happens-before relation -- a peer that schedules the
	// delete during the upload but performs it after responding would defeat a
	// point-in-time check like this one. A late delete of that shape WAS observed
	// on the fanart path (~15ms after the call returned), so the window is real
	// and this repair does not close it. Anything left after the window is caught
	// by the next push or by the exists-flag reconciler, not here.
	//
	// It runs after the whole loop, not per connection, because an operator may
	// run several peers and any one of them can be the deleter -- and `data` was
	// read once before the loop, so one restoration repairs whichever peer did it.
	//
	// Content is compared, not merely existence: a peer can OVERWRITE rather than
	// delete (the original #2533 concern), and an existence check would call that
	// clean.
	if len(uploadedTo) > 0 {
		p.reassertLocalImage(a, imageType, filePath, data, snapMod, uploadedTo)
	}
	return warnings
}

// fanartSnapshot is one fanart file's path, its TRUE slot index, and the bytes
// it held before any upload ran, which is what a restoration puts back.
//
// index is carried explicitly and is NOT the position in the snapshot slice. A
// file that could not be read is kept in the set with nil data so the files
// after it keep their real slot numbers. Compacting instead -- dropping the
// unreadable entry and letting the loop's own counter supply the index --
// shifts every later backdrop down one on the platform, and because this sync
// only ever adds indices (it never deletes surplus ones), the stale image at
// the tail index then survives indefinitely. Both handler call sites already
// refuse to sync when renumbering fails, for exactly this reason.
type fanartSnapshot struct {
	path  string
	index int
	data  []byte
	// mod is the file's mtime when data was captured, reported in the restore
	// log so an operator can see how stale a restored copy is. Nothing branches
	// on it -- see reassertLocalImage on why newness cannot arbitrate here.
	mod time.Time
}

// snapshotFanart reads EVERY fanart file BEFORE the first upload, returning the
// captured set plus a warning per file that could not be read.
//
// This is not an optimization to avoid re-reading per connection -- it is the
// only arrangement that can repair the damage. A peer does NOT necessarily
// delete the file it was just handed: it deletes whatever it currently holds as
// that slot's previous image, which can be ANY file in the set. Measured on Emby
// 4.10, uploading slot 0 deleted the slot-1 file, which the caller's loop had
// not reached yet. Reading lazily per iteration meant the later read simply
// failed ("no such file or directory") and the file was gone with nothing to
// restore from, because the bytes had never been held.
//
// So the bytes must be captured while every file still exists. Cost: the whole
// fanart set is resident for the duration of the push (one production artist
// holds 42 backdrops). That is the price of being able to restore a file that is
// already deleted -- a hash-only snapshot could DETECT the loss but never repair
// it.
func (p *Publisher) snapshotFanart(fanartPaths []string) ([]fanartSnapshot, []string) {
	snapshot := make([]fanartSnapshot, 0, len(fanartPaths))
	var warnings []string
	for i, fp := range fanartPaths {
		data, readErr := os.ReadFile(fp) //nolint:gosec // path from trusted fanart discovery
		if readErr != nil {
			p.logger.Error("reading fanart for platform sync",
				slog.String("path", fp),
				slog.String("error", readErr.Error()))
			warnings = append(warnings, truncateWarning(fmt.Sprintf("failed to read fanart %d", i)))
			// Kept with nil data so the slots AFTER this one keep their real
			// index; the upload loop skips nil entries.
			snapshot = append(snapshot, fanartSnapshot{path: fp, index: i})
			continue
		}
		snapshot = append(snapshot, fanartSnapshot{path: fp, index: i, data: data, mod: fileModTime(fp)})
	}
	return snapshot, warnings
}

// hasReadableFanart reports whether any snapshot entry actually captured bytes.
// A set where every read failed has nothing to upload and nothing to restore.
func hasReadableFanart(snapshot []fanartSnapshot) bool {
	for _, sf := range snapshot {
		if sf.data != nil {
			return true
		}
	}
	return false
}

// reassertLocalImage restores the operator's local image when a platform peer
// removed or rewrote it during the push, and reports when that happened.
//
// It deliberately LOGS AT ERROR on every repair. A silent self-heal would hide a
// platform quietly eating library files -- the same "unknown rendered as clean"
// failure this codebase has already had to fix in several places. The repair
// keeps the operator's data; the log is what makes the peer's behavior visible.
//
// Best-effort: every failure path leaves the push result unchanged, because the
// upload itself already succeeded and the caller's warnings describe the sync,
// not the local filesystem.
//
// ATTRIBUTION IS THE HARD PART, and getting it wrong destroys operator data in
// the opposite direction. This function knows only "the bytes I read before the
// push"; it cannot see WHO changed the file. Nothing serializes an in-flight
// push against a concurrent operator action -- a background rule fixer can be
// pushing artist X while the operator saves or deletes that same slot. A naive
// "restore whenever the bytes differ" therefore:
//
//   - REVERTS A NEWER SAVE. Operator re-crops mid-push; the older push's repair
//     writes the previous image back over it and blames the peer.
//   - RESURRECTS A DELETED IMAGE. Operator deletes the slot mid-push; the repair
//     reads ENOENT and puts the artwork the operator just threw away back on
//     disk. That is the exact failure this repo already fixed once, where a
//     stale backup restored over a deliberately deleted slot.
//
// NEITHER DIRECTION IS ARBITRATED, because no signal available here can do it.
// The exists flag looks like the answer and is not: it is DERIVED FROM DISK
// (handleServeImage clears it on a 404, the scanner recomputes it from a walk),
// so a fresh read is poisoned by the peer's own deletion -- and the value
// captured before the push is stale for a first-ever image, where the flag is
// still false while the file is already written. Gating on it therefore refuses
// the repair either for a routine save or for the exact case #2698 describes.
// mtime cannot separate the peer from the operator either; both writes land in
// the same window.
//
// ACCEPTED RISK, stated plainly: if the operator DELETES a slot while a push for
// that same slot is in flight, the repair will put the image back. That needs
// the delete to land inside one push (30s), and the operator can delete again.
// The alternative -- refusing repairs on an unreliable signal -- silently loses
// artwork to the original bug, which is worse and far more likely. A real fix
// needs a signal that records INTENT rather than disk state (an explicit
// delete marker); that is follow-up work, not something to fake here.
//
// The OVERWRITE direction is NOT refused, and that is a deliberate, documented
// trade rather than an oversight. mtime cannot separate "the peer rewrote it"
// from "the operator saved again" -- both land between the snapshot and the
// check -- so a guard on newness would disable the crop-clobber repair (#2533),
// which is the case this fix exists to serve. RESIDUAL RISK, accepted: if the
// operator saves the same slot twice inside one push window, the older push's
// repair can write the earlier image back. That needs the two saves to overlap
// within a single 30s push; the losing outcome is a stale image, recoverable by
// saving again. Resurrecting a deleted image or leaving a peer's artwork in the
// library are both worse, and both are prevented.
//
// snapMod is the file's mtime when its bytes were captured. Retained for the
// restore log so an operator can see how stale the restored copy was.
func (p *Publisher) reassertLocalImage(a *artist.Artist, imageType, filePath string, data []byte, snapMod time.Time, uploadedTo []string) {
	current, readErr := os.ReadFile(filePath) //nolint:gosec // filePath came from FindExistingImage over trusted naming patterns
	switch {
	case readErr == nil && bytes.Equal(current, data):
		return // untouched: the common and correct case
	case readErr != nil && !errors.Is(readErr, os.ErrNotExist):
		// Cannot tell whether the file survived. Do NOT rewrite on an unknown
		// state -- an unreadable file is not a known-absent one (absent !=
		// unreadable), and blind restoration could clobber a concurrent write.
		p.logger.Warn("could not verify the local image after platform push; leaving it untouched",
			"artist", a.Name, "type", imageType, "path", filePath, "error", readErr.Error())
		return
	}

	outcome := "rewrote"
	if errors.Is(readErr, os.ErrNotExist) {
		outcome = "deleted"
	}
	p.logger.Error("a platform peer "+outcome+" the local image during push; restoring it",
		"artist", a.Name, "type", imageType, "path", filePath,
		"peers", strings.Join(uploadedTo, ","), "captured_at", snapMod.Format(time.RFC3339Nano))

	if writeErr := filesystem.WriteFileAtomic(filePath, data, 0o644); writeErr != nil {
		p.logger.Error("restoring the local image after a peer removed it FAILED; the artwork is now lost locally",
			"artist", a.Name, "type", imageType, "path", filePath, "error", writeErr.Error())
	}
}

// fileModTime returns a path's mtime, or the zero time when it cannot be
// stat'ed. It is recorded purely so the restore log can say how stale the
// bytes being put back are; nothing branches on it.
func fileModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
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

	// #2540 NOTIFY-ONLY collision check: registry built ONCE per sync, plus the
	// per-file notified set. Both are inert when the notifier is unwired.
	fanartIdentityIdx := p.fanartIdentityIndex(ctx, a)
	collisionNotified := make(map[string]bool)

	snapshot, snapWarnings := p.snapshotFanart(fanartPaths)
	warnings = append(warnings, snapWarnings...)
	if !hasReadableFanart(snapshot) {
		return warnings
	}

	// Peers this push handed a backdrop to, named in the restore log below.
	var uploadedTo []string

	// Restore any snapshot file a peer removed or rewrote during the push. Runs
	// once after ALL uploads to ALL peers, so it covers cross-file deletion and an
	// operator running several peers alike.
	//
	// Gated on uploadedTo: with no peer reached, nothing external touched these
	// files and the only thing a repair could do is revert a concurrent operator
	// write. The primary path applies the same guard.
	// No context is consulted: the repair reads the local filesystem and the
	// pre-push artist snapshot only, so it still runs correctly when the push
	// itself was canceled or timed out -- precisely when a peer may have destroyed
	// a file and nothing else will put it back.
	defer func() {
		if len(uploadedTo) == 0 {
			return
		}
		for _, sf := range snapshot {
			if sf.data == nil {
				continue // never captured; there is nothing to put back
			}
			p.reassertLocalImage(a, "fanart", sf.path, sf.data, sf.mod, uploadedTo)
		}
	}()

	for _, pid := range platformIDs {
		conn, connErr := p.connectionService.GetByID(ctx, pid.ConnectionID)
		if connErr != nil {
			p.logger.Error("getting connection for fanart sync",
				slog.String("connection_id", pid.ConnectionID),
				slog.String("error", connErr.Error()))
			warnings = append(warnings, truncateWarning(fmt.Sprintf("connection %s: failed to load", pid.ConnectionID)))
			p.notifyPushFailure(pid.ConnectionID, shortConnLabel(pid.ConnectionID), classifyPushErr(connErr), a.ID, artistDisplayName(a), pushOpImageUpload, connErr)
			continue
		}
		if !conn.Enabled || conn.Status != "ok" || (respectWriteGate && !conn.GetFeatureImageWrite()) {
			p.logger.Debug("skipping connection for fanart sync",
				slog.String("connection", conn.Name),
				slog.String("status", conn.Status))
			continue
		}

		// #2698: the #2533 pre-disable was here too and is removed for the same
		// reason as on the primary path -- it is what turns the peer's overwrite
		// into a DELETE. Measured on Emby 4.10: a fanart reorder pushes both
		// backdrops, and the peer logs
		//   Saving image to <emby-config>/metadata/musicartists/<artist>/fanart.jpg
		//   Deleting previous image <artist-dir>/backdrop.jpg
		// destroying the operator's file. The deferred re-assertion registered
		// above -- one pass over the whole snapshot after every peer -- restores
		// it. It is deliberately NOT per-file-after-its-own-upload: the peer was
		// measured deleting a file this loop had not reached yet.
		indexedUploader := newIndexedImageUploader(conn, p.logger)
		if indexedUploader == nil {
			p.logger.Warn("unsupported connection type for fanart sync",
				slog.String("type", conn.Type))
			warnings = append(warnings, truncateWarning(fmt.Sprintf("%s: unsupported connection type %q", conn.Name, conn.Type)))
			continue
		}

		// Recorded before the uploads, not after a success: a peer that destroys a
		// backdrop and then fails the request still needs the repair to run.
		uploadedTo = append(uploadedTo, conn.Name)
		warnings = append(warnings, p.uploadFanartSet(ctx, fanartUpload{
			artist:      a,
			conn:        conn,
			pid:         pid,
			uploader:    indexedUploader,
			snapshot:    snapshot,
			identityIdx: fanartIdentityIdx,
			notified:    collisionNotified,
		})...)
	}
	return warnings
}

// fanartUpload carries one peer's worth of fanart-push state. It exists to keep
// syncAllFanartToPlatforms under the cognitive-complexity budget rather than to
// model anything: the fields are exactly the loop variables the extracted body
// used to close over.
type fanartUpload struct {
	artist      *artist.Artist
	conn        *connection.Connection
	pid         artist.PlatformID
	uploader    connection.IndexedImageUploader
	snapshot    []fanartSnapshot
	identityIdx []img.FanartIdentityEntry
	notified    map[string]bool
}

// uploadFanartSet pushes every captured backdrop to ONE peer, at its TRUE slot
// index, and returns the per-file warnings.
func (p *Publisher) uploadFanartSet(ctx context.Context, u fanartUpload) []string {
	var warnings []string
	for _, sf := range u.snapshot {
		// A slot whose bytes could not be captured is skipped, but its index is
		// still spent, so the surviving files keep their true slot numbers on the
		// platform. Compacting here would shift the whole gallery down one.
		if sf.data == nil {
			continue
		}
		fp, data, idx := sf.path, sf.data, sf.index

		ct := "image/jpeg"
		if strings.EqualFold(filepath.Ext(fp), ".png") {
			ct = "image/png"
		}

		// #2540 NOTIFY-ONLY: notifies on a cross-artist collision but never
		// blocks; the upload below ALWAYS proceeds.
		p.notifyFanartCollision(ctx, u.artist, fp, data, u.identityIdx, u.notified)

		if uploadErr := u.uploader.UploadImageAtIndex(ctx, u.pid.PlatformArtistID, "fanart", idx, data, ct); uploadErr != nil {
			p.logger.Error("syncing fanart to platform",
				slog.String("artist", u.artist.Name),
				slog.String("connection", u.conn.Name),
				slog.Int("index", idx),
				slog.String("error", uploadErr.Error()))
			warnings = append(warnings, truncateWarning(fmt.Sprintf("%s (%s): fanart %d upload failed", u.conn.Name, u.conn.Type, idx)))
			p.notifyPushFailure(u.pid.ConnectionID, u.conn.Name, classifyPushErr(uploadErr), u.artist.ID, artistDisplayName(u.artist), pushOpImageUpload, uploadErr)
		}
	}
	return warnings
}

// fanartIdentityIndex builds the cross-artist fanart phash registry for one
// outbound sync. It is built ONCE per sync and reused for every file and every
// platform: the registry is a whole-library scan, so rebuilding it per image
// would re-scan the table for every backdrop of every connection.
//
// Returns nil (meaning "no collision checking this sync") when the notifier or
// indexer is unwired, or when the build fails. That is deliberate fail-open: a
// registry we cannot build must never turn into a blocked push.
func (p *Publisher) fanartIdentityIndex(ctx context.Context, a *artist.Artist) []img.FanartIdentityEntry {
	if p.collisionNotifier == nil || p.fanartIdentity == nil {
		return nil
	}
	idx, err := p.fanartIdentity.BuildFanartIdentityIndex(ctx)
	if err != nil {
		p.logger.Warn("building fanart identity index; skipping cross-artist collision check for this sync",
			slog.String("artist_id", a.ID),
			slog.String("error", err.Error()))
		return nil
	}
	return idx
}

// notifyFanartCollision raises the #2540 cross-artist backdrop-collision
// notifications for one fanart file about to be pushed. It NEVER blocks or
// alters the push -- the caller uploads regardless of what happens here.
//
// notified de-duplicates per FILE rather than per (file, platform): the same
// file is uploaded to every connected platform, but the collision is a property
// of the image, not of the destination, so without this the operator would get
// one toast per platform for a single colliding backdrop.
//
// Fail-open at every step: an empty registry, an unhashable image, or a
// Match/Indeterminate verdict all notify nothing.
func (p *Publisher) notifyFanartCollision(ctx context.Context, a *artist.Artist, path string, data []byte, reference []img.FanartIdentityEntry, notified map[string]bool) {
	if p.collisionNotifier == nil || len(reference) == 0 || notified[path] {
		return
	}
	phash, phErr := img.PerceptualHash(bytes.NewReader(data))
	if phErr != nil {
		p.logger.Debug("perceptual hash for cross-artist collision check failed; skipping this file",
			slog.String("path", path), slog.String("error", phErr.Error()))
		return
	}
	res := img.CompareIdentity(phash, a.ID, reference, collision.DefaultTolerance)
	if res.Verdict != img.IdentityMismatch {
		return
	}
	notified[path] = true
	p.collisionNotifier.Notify(ctx, a.ID, artistDisplayName(a), res)
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
//
// A package-level var (the injectable-hook pattern used throughout this repo)
// so a test can substitute an uploader that DELETES the local file mid-upload,
// which is exactly what a real peer does (#2698) and cannot otherwise be
// exercised without standing up an Emby.
var newImageUploader = func(conn *connection.Connection, logger *slog.Logger) connection.ImageUploader {
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
// connection type. Returns nil for unsupported types. Injectable for the same
// reason as newImageUploader above.
var newIndexedImageUploader = func(conn *connection.Connection, logger *slog.Logger) connection.IndexedImageUploader {
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
