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
	// reassertVerifyTimeout bounds the post-upload verify read in
	// reassertLocalImage. That read is deliberately DETACHED from the caller's
	// context -- see the function's doc comment -- so this is the only thing
	// standing between a dead mount and a goroutine pinned for the life of the
	// process. Short, because the file was read successfully moments earlier:
	// anything slower than this is a mount that has stopped answering, not a
	// slow disk.
	reassertVerifyTimeout = 10 * time.Second

	// reassertSettleDelay is how long the push waits after its first repair pass
	// before looking at the files ONE more time (#2712).
	//
	// WHY A SECOND LOOK AT ALL. The repair is a point-in-time check: it runs
	// after UploadImage returns and asks "is the file still what I uploaded?".
	// That question has an answer only for damage that has already landed. In
	// every case observed on the primary path the peer's delete completed BEFORE
	// the call returned (204ms before, in the production capture), so one pass
	// sufficed -- but a peer that schedules the delete during the upload and
	// performs it after responding defeats a point-in-time check entirely, and
	// exactly that shape WAS measured on the fanart path: the file vanished
	// roughly 15ms AFTER UploadImage returned. One pass reports "healthy" and
	// the operator's backdrop is gone with nothing coming to put it back.
	//
	// WHY 250ms. It is an order of magnitude over the ~15ms that was actually
	// measured, which is the only number this codebase has, and it is small
	// enough to be invisible next to what it is bolted onto: the pushes that
	// reach this code are wrapped in a 30-second context by every caller, and a
	// single peer upload is a network round trip carrying the whole image. 250ms
	// is under 1% of that budget. Going larger buys diminishing coverage (the
	// distribution has one observed sample, at 15ms) for latency the operator
	// pays on EVERY push, including the overwhelming majority where no peer
	// misbehaves at all.
	//
	// COST, stated plainly rather than hidden: every push that reached at least
	// one peer now takes 250ms longer, plus one extra stat-and-read per file.
	// The delay is paid ONCE per push, not once per peer or once per file, and
	// only when uploadedTo is non-empty -- a push that reached nobody skips the
	// repair entirely and pays nothing. On the fanart path the extra reads are
	// one per captured backdrop, the same reads the first pass already does. In
	// particular the 42-backdrop artist pays 250ms for the whole set, not 250ms
	// per backdrop, because repairAfterPush wraps the WHOLE restore loop.
	//
	// WHO PAYS IT, since "250ms" without an owner is not a cost statement. It is
	// SYNCHRONOUS in the request path: an artwork save or delete in the UI that
	// reaches a peer returns 250ms later than it used to, on top of the peer
	// round trip it already waited for. The background artwork reconciler is
	// serial, so an artist with both a primary image and fanart pays it on each
	// push -- up to about 1s per artist -- which lengthens a reconcile sweep in
	// proportion to the artists that actually have peers. That is accepted: the
	// alternative is a push that reports success over an operator's destroyed
	// backdrop, and the delay is under 1% of the 30s context each push runs in.
	//
	// It is a SLEEP and not a poll loop on purpose. Polling would turn an
	// unbounded number of reads loose on a mount that may be the reason the
	// push was slow in the first place, for no additional coverage: the damage
	// this is chasing is a single event, not a condition that clears.
	reassertSettleDelay = 250 * time.Millisecond

	// maxFanartSnapshotFiles caps how many backdrops one push holds in memory
	// (#2712).
	//
	// snapshotFanart must read EVERY backdrop before the first upload -- a peer
	// was measured deleting a slot the upload loop had not reached yet, and
	// bytes never held cannot be restored -- so the whole set is resident for
	// the duration of the push, and concurrent syncs multiply that.
	//
	// 100 is chosen from measured reality: the largest artist in the production
	// library that surfaced this issue holds 42 backdrops, so the cap is a
	// little over twice the real high-water mark. That leaves generous room for
	// growth while still refusing a directory holding thousands of files, which
	// is a broken library or a runaway import rather than an artist.
	maxFanartSnapshotFiles = 100

	// maxFanartSnapshotFileBytes caps how large ONE backdrop may be and still be
	// captured for restore (#2712).
	//
	// Note this is STRICTER than img.MaxDecodeBytes (25 MB), which is the read
	// bound and stays where it is: that limit exists to stop a single read
	// exhausting memory, while this one exists because up to
	// maxFanartSnapshotFiles of these are held AT ONCE. A 4K-wide JPEG backdrop
	// is on the order of 1 to 6 MB; 12 MiB is roughly double the top of that
	// range, so a real backdrop is never refused and a video file or a PSD that
	// wandered into the artist directory is.
	maxFanartSnapshotFileBytes = 12 << 20

	// maxFanartSnapshotTotalBytes caps the cumulative bytes ONE push holds
	// (#2712). The per-file and per-count caps multiply out to 1.2 GB, which is
	// not a bound worth having, so the total is what actually protects the
	// process.
	//
	// It is a real bound rather than a predicted one because it is enforced
	// TWICE: once from a stat before the read (so an honestly-huge file is never
	// allocated) and once from the length actually read (so a file that grew
	// between the two is discarded instead of retained). A stat-only check would
	// bound a number while leaving what is resident unbounded; see readio.go's
	// io.LimitReader argument, and fanartSnapshotBudget.refuseResult.
	//
	// 192 MiB is 201.3 MB, and the measured worst case is 42 backdrops. At the
	// top of the observed per-file range (~4.5 MB) that set is 189 MB, which
	// clears the cap by only 6 percent -- so the headroom against the EXTREME
	// case is thin, and it is stated as a number rather than called comfortable.
	// The headroom against the TYPICAL case is what makes the number safe: real
	// backdrops run well under 2 MB, so the same 42-file artist is nearer 84 MB,
	// under half the cap. Units are MB (10^6) throughout except where MiB is
	// written explicitly, because mixing the two while reasoning about a 6
	// percent margin is how a margin gets miscounted.
	maxFanartSnapshotTotalBytes = 192 << 20

	// maxFanartSnapshotWarnings caps how many per-slot warnings one snapshot
	// puts in the API response (#3018 review).
	//
	// The three caps above bound what a directory can make this push HOLD. They
	// deliberately do not stop the loop -- every remaining slot is still walked
	// so its true index survives and its loss is reported -- which left the
	// REPORT unbounded in exactly the dimension the snapshot no longer is: a
	// directory holding thousands of files produces one warning per refused
	// slot, and truncateWarning bounds each string while nothing bounded the
	// count. That is the same "the artist directory chooses the commitment"
	// defect arriving through the response body instead of the heap.
	//
	// 25 is above the count at which a per-slot list stops being actionable and
	// well under the maxFanartSnapshotFiles cap, so a legitimately large artist
	// (42 backdrops measured, 100 allowed) that fails wholesale still names its
	// first 25 slots and then says how many more there were. The full per-slot
	// detail, including the path an API response must not carry, is in the
	// Error log degradeFanartSlot writes for every slot regardless of this cap.
	maxFanartSnapshotWarnings = 25
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
	// of a policy the next patch can quietly undo. Today it belongs where the
	// evidence lives outright: the merge path, which holds the loser's link
	// directly. The background reconciler (relink_reconcile.go, #2426) is NOT
	// a second caller with this capability -- it can let the peer settle for
	// minutes and then re-resolve, but as shipped it only ever upgrades via
	// SetPlatformID and never drops; it does not implement or need
	// DeletePlatformID. Extending it to drop on stale/ambiguous evidence is
	// separate, gated follow-up work (#2442) and would need its own deliberate
	// capability grant here, not an assumption that this comment already
	// promised it one.
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
	// #2712: the wall-clock instant THIS PUSH BEGAN. Stamped as the first
	// statement of the function, before any I/O or database access, because the
	// repair's delete gate treats a marker older than this instant as an
	// unrelated earlier delete and repairs anyway. Every step between entry and
	// the byte read -- GetPlatformIDs, ImageDir, getActiveNamingConfig (a DB
	// read), FindExistingImage (a stat loop), ReadImageFileBounded (the whole
	// artwork file, off a network mount) -- is time during which the push is
	// unambiguously in flight and an operator delete is genuinely concurrent.
	// Stamping after any of them discards those deletes and resurrects the
	// artwork, which is #2712 unfixed for the early part of every push.
	//
	// FUNCTION ENTRY specifically, not merely "before the first read": there is
	// no defensible later line. Any candidate is some prologue step's end, and
	// the operator can delete during that step. Entry is the only instant that
	// is provably before everything this push does.
	//
	// Erring early is the safe direction and costs nothing: an earlier stamp
	// can only make the gate MORE willing to treat a delete as concurrent, and
	// suppressing a repair merely leaves a file the operator can re-add, while
	// resurrecting a deliberate delete is not automatically recoverable at all.
	//
	// push.dir is filled in below, once ImageDir has resolved it. It is threaded
	// rather than re-derived inside the repair; see pushScope.
	push := pushScope{at: time.Now()}

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
	// The SAME resolved directory the delete handlers key their marker on. The
	// repair must not re-derive it from the discovered file path; see pushScope.
	push.dir = dir
	patterns := p.getActiveNamingConfig(ctx, imageType)
	filePath, found := img.FindExistingImage(ctx, dir, patterns)
	if !found {
		warnings = append(warnings, "platform sync skipped: no local image found to upload")
		return warnings
	}

	// Read the bytes under ctx (#2934). FindExistingImage above is already
	// ctx-bound, so leaving the byte read on a bare os.ReadFile put the only
	// unbounded step of this function AFTER the bounded one: a mount that stops
	// answering between the probe and the read wedges the sync indefinitely and
	// nothing above can abort it. Bounded is also the size guard -- an operator
	// artwork file of arbitrary size used to be read whole into memory here.
	data, readErr := img.ReadImageFileBounded(ctx, filePath)
	if readErr != nil {
		// A cancellation gets its OWN outcome (#2934). This path already
		// returns on any read failure, so the defect here is not partial work
		// but a MISREPORT: "failed to read image for upload" tells the operator
		// their artwork is unreadable and sends them to check the file, when
		// what actually happened is that they navigated away or the request
		// timed out. Same context-not-contents rule as everywhere else here.
		// A stalled mount says the same thing a cancellation does and needs its
		// own message (#2976 review). This is a SINGLE read rather than a loop,
		// so the loop-abort question does not arise -- the path already returns
		// on any failure. What matters is the same misreport the comment above
		// describes: "failed to read image for upload" sends the operator to
		// check a file that is fine, when the mount is what stopped answering.
		if errors.Is(readErr, img.ErrTooManyStalledReads) {
			p.logger.Error("platform image sync stopped: the library mount is not responding",
				"artist", a.Name, "type", imageType, "path", filePath, "error", readErr)
			warnings = append(warnings,
				"platform sync stopped: the library mount is not responding, so the image could not be read")
			return warnings
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			p.logger.Warn("platform image sync canceled before upload",
				"artist", a.Name, "type", imageType, "path", filePath, "error", ctxErr)
			warnings = append(warnings, "platform sync canceled: the request ended before the image could be read")
			return warnings
		}
		p.logger.Error("reading image for platform sync", "artist", a.Name, "type", imageType, "path", filePath, "error", readErr)
		if errors.Is(readErr, img.ErrImageTooLarge) {
			warnings = append(warnings, "platform sync skipped: image exceeds the size limit for upload")
		} else {
			warnings = append(warnings, "platform sync skipped: failed to read image for upload")
		}
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

	// #3125 F3/C1: the bytes of the PREVIOUS primary, read ONCE before the
	// per-connection loop (it does not vary per platform) and passed to
	// uploadOneImageForSync so the fanart branch can identify which
	// platform index still holds it, rather than assuming index 0.
	//
	// SOURCED FROM THE ON-DISK BACKUP, NEVER FROM artist_images.PHash. A
	// round-1 review found the DB-hash design INERT IN PRODUCTION: the DB
	// row is stamped from the NEW file, by THIS SAME REQUEST, before this
	// function ever runs (finalizeImageSave calls recordImageProvenance at
	// handlers_image.go:225 -- itself downstream of setArtistImageFlag,
	// which reads the just-written file straight off disk -- roughly 30
	// lines before it reaches SyncImageToPlatforms at :257; the rule engine
	// does the same thing, recording provenance from the saved path before
	// syncing). So "the artist's previous primary hash" read from the DB at
	// sync time is never the previous primary -- it is the CURRENT one,
	// which can never fail to "match" the very upload being sent, and the
	// resolver's previous-primary branch was consequently unreachable.
	//
	// The one-deep on-disk backup (.sw-backup/fanart/, written by BackupSlot
	// inside SaveSlotProtected, STRICTLY BEFORE the destructive Save that
	// overwrites the canonical file -- see saveFanartSlotProtected in
	// internal/api and saveImageToDisk in internal/rule, the two chokepoints
	// every fanart-primary write funnels through) has no such race: by
	// construction it is written before this request's Save, so at the time
	// THIS FUNCTION runs it still holds the bytes the platform was last
	// given, never the new ones. See previousFanartPrimaryData's doc comment
	// for the full ordering argument and image.ReadSlotBackup for the read.
	//
	// Gated on imageType == "fanart": every other type has no index concept
	// to resolve, so the lookup would be a pure-waste disk read on the far
	// more common thumb/logo/banner sync.
	var previousFanartData []byte
	if imageType == "fanart" {
		previousFanartData = p.previousFanartPrimaryData(ctx, dir, filepath.Base(filePath))
	}

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
		//
		// #3125: fanart routes through a different call than the other three
		// image types; see uploadOneImageForSync's doc comment for why.
		uploaded, warn := p.uploadOneImageForSync(ctx, a, pid, conn, imageType, data, ct, previousFanartData)
		if uploaded {
			uploadedTo = append(uploadedTo, conn.Name)
		}
		if warn != "" {
			warnings = append(warnings, warn)
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
	// delete during the upload but performs it after responding defeats a
	// point-in-time check like this one. A late delete of that shape WAS observed
	// on the fanart path (~15ms after the call returned), so the window is real.
	//
	// #2712: that is why this is a SETTLE-AND-REVERIFY rather than a single
	// check. repairAfterPush runs the pass, waits reassertSettleDelay, and runs
	// it once more. Bounded at exactly two passes and one short wait; see the
	// constant for the cost and why it is not a poll loop.
	//
	// It runs after the whole loop, not per connection, because an operator may
	// run several peers and any one of them can be the deleter -- and `data` was
	// read once before the loop, so one restoration repairs whichever peer did it.
	//
	// Content is compared, not merely existence: a peer can OVERWRITE rather than
	// delete (the original #2533 concern), and an existence check would call that
	// clean.
	if len(uploadedTo) > 0 {
		p.repairAfterPush(func(scope repairScope) {
			p.reassertLocalImage(ctx, a, imageType, filePath, data, snapMod, push, uploadedTo, scope)
		})
	}
	return warnings
}

// uploadOneImageForSync uploads a single image to a single already-validated
// connection (enabled, status ok, write-gate satisfied -- the caller checks
// all of that before calling this) and reports whether an upload was
// attempted plus any warning string to surface to the operator. Split out of
// syncImageToPlatforms (#3125) purely to keep that function's branching
// under the repo's cognitive-complexity budget; it has no independent
// purpose and is not meant to be called from anywhere else.
//
// #3125: fanart is NOT single-slot on the platform the way thumb/logo/
// banner are. Emby and Jellyfin both keep backdrops as a numbered list
// (Backdrop/0, Backdrop/1, ...), and the two upload shapes have different
// semantics: POST .../Images/Backdrop REPLACES-BY-APPENDING (it always adds
// a new tail entry), while POST .../Images/Backdrop/{index} REPLACES IN
// PLACE at that index (measured on real Emby 4.9.5.0; see the issue for the
// wire capture). syncImageToPlatforms discovers exactly one local file --
// the PRIMARY fanart image, which is always slot 0 (see FindExistingImage /
// the primary-name convention) -- so a fanart sync here is always a replace
// of that one slot, never an add of a new one. Using the non-indexed call
// for fanart was therefore always wrong: it told the peer "append a
// backdrop" when the caller meant "replace the primary backdrop", and every
// such sync left one more permanent duplicate on the platform. Non-fanart
// types (thumb, logo, banner) have no index concept on the platform at all,
// so they keep the plain UploadImage call unchanged.
//
// EMBY-VERIFIED, JELLYFIN NOT YET FIXED BY THIS BRANCH. Measured separately
// against a real Jellyfin 10.11.10 (#3125 follow-up): its indexed endpoint
// does NOT honor the URL index for placement the way Emby's does -- POST
// .../Images/Backdrop/0 against an artist that already has a backdrop at
// index 0 still APPENDS a new entry at the tail, leaving the original
// index-0 content untouched. A delete-then-upload sequence does not recover
// single-slot replace either: deleting index 0 re-indexes every later slot
// down by one (see DeleteImageAtIndexRaw's doc comment), so the subsequent
// append lands the new content at the WRONG slot rather than the one that
// was just vacated. The only way to guarantee correct placement on Jellyfin
// is to delete every existing backdrop and re-upload the full desired set in
// order -- which is uploadFanartSet's job (the full indexed sync), not this
// single-image sync path, and reaches well past the "smallest fix" scope of
// this branch. Tracked as #3135. So: this fix stops the duplication that
// was previously guaranteed on EVERY fanart sync to EVERY platform (both Emby and Jellyfin
// were appending before this change); it does not yet make a Jellyfin
// single-image replace correct. It also does not make Jellyfin any WORSE --
// before this change Jellyfin appended via the non-indexed call, and it
// still appends now via the indexed call, so nothing regresses.
//
// F3 (#3125 review round 1): writing index 0 unconditionally assumed the
// local primary is ALWAYS platform slot 0. That is only true immediately
// after a fresh full sync -- a phash back-out prune, a remote-dedup prune,
// or an operator deleting a backdrop in the Emby/Jellyfin UI can all delete
// platform index 0 and let the peer re-index survivors down by one, so a
// bystander backdrop ends up sitting at index 0. An unconditional index-0
// write then DESTROYS that bystander rather than merely duplicating one.
// previousFanartData (the previous primary's actual on-disk bytes, from the
// pre-save backup -- see previousFanartPrimaryData) lets
// resolveFanartReplaceTarget positively identify the slot that still holds
// the OLD primary before authorizing an overwrite; see its doc comment for
// the full three-outcome (noop/index/append) decision and why "cannot
// identify" falls back to append rather than guessing.
func (p *Publisher) uploadOneImageForSync(ctx context.Context, a *artist.Artist, pid artist.PlatformID, conn *connection.Connection, imageType string, data []byte, ct string, previousFanartData []byte) (uploaded bool, warning string) {
	if imageType == "fanart" {
		return p.uploadFanartForSync(ctx, a, pid, conn, data, ct, previousFanartData)
	}

	uploader := newImageUploader(conn, p.logger)
	if uploader == nil {
		p.logger.Warn("unsupported connection type for image sync", "type", conn.Type)
		return false, truncateWarning(fmt.Sprintf("%s: unsupported connection type %q", conn.Name, conn.Type))
	}
	if uploadErr := uploader.UploadImage(ctx, pid.PlatformArtistID, imageType, data, ct); uploadErr != nil {
		p.logger.Error("syncing image to platform", "artist", a.Name, "connection", conn.Name, "type", imageType, "error", uploadErr)
		p.notifyPushFailure(pid.ConnectionID, conn.Name, classifyPushErr(uploadErr), a.ID, artistDisplayName(a), pushOpImageUpload, uploadErr)
		return true, truncateWarning(fmt.Sprintf("%s (%s): image upload failed", conn.Name, conn.Type))
	}
	return true, ""
}

// uploadFanartForSync performs the fanart-specific replace decided by
// resolveFanartReplaceTarget (#3125 F3): a no-op when the platform already
// holds the new bytes, an in-place index write when the previous primary's
// slot can be positively identified (or the platform is empty), and an
// append -- accepting one duplicate rather than risking a destroyed
// bystander -- when neither holds. See uploadOneImageForSync's doc comment
// for the full defect this guards against.
func (p *Publisher) uploadFanartForSync(ctx context.Context, a *artist.Artist, pid artist.PlatformID, conn *connection.Connection, data []byte, ct string, previousFanartData []byte) (uploaded bool, warning string) {
	client := newFanartReplaceClient(conn, p.logger)
	if client == nil {
		// Mirror the non-indexed nil-uploader handling: warn loudly and skip
		// this connection rather than silently falling through to a call
		// that would misbehave.
		p.logger.Warn("unsupported connection type for indexed fanart sync", "type", conn.Type)
		return false, truncateWarning(fmt.Sprintf("%s: unsupported connection type %q", conn.Name, conn.Type))
	}

	// #3125 F3 review: resolveFanartReplaceTarget reads the platform's
	// current backdrop set and then this function writes based on what it
	// saw -- a resolve-then-mutate sequence with exactly the TOCTOU shape
	// (time-of-check to time-of-use: the state can change between the read
	// and the write) deletePollutedBackdrops/restoreBackdrop already guard
	// with lockPhashTarget. Two concurrent syncs for the SAME artist on the
	// SAME connection (e.g. a UI replace racing a rule-engine auto-fix) could
	// otherwise both resolve against the pre-write state and then both
	// write, one of them now stale. Held across the ENTIRE resolve+write,
	// matching lockPhashTarget's own documented contract, and released via
	// defer so every return path below (including the error and noop
	// returns) releases it.
	//
	// NO DEADLOCK: this is the same key space DeletePollutedBackdropOnPlatforms
	// and RestoreBackdropToPlatforms already lock, and neither of those calls
	// into SyncImageToPlatforms (or anything that reaches this function)
	// while holding it -- deleteRemovedSlotsOnPlatforms/RestoreBackdropToPlatforms
	// are invoked from the rule-engine phash-repair path, which calls this
	// sync function only AFTER its own lock/unlock pair has already
	// completed (reconcileAfterFix / publishAfterFix run later, once the
	// phash back-out's per-target unlock() has already returned). So this
	// lock and phash_platform.go's uses of it are never nested on the same
	// goroutine's call stack for the same target.
	unlock := p.lockPhashTarget(pid.ConnectionID, pid.PlatformArtistID)
	defer unlock()

	decision, resolveErr := resolveFanartReplaceTarget(ctx, client, pid.PlatformArtistID, data, previousFanartData)
	if resolveErr != nil {
		// Cannot even READ the platform's current backdrop state -- fail
		// closed the same way the nil-client branch does, rather than
		// guessing an index blind. This is a NEW failure mode the old
		// unconditional-index-0 code never had (it never read first), so it
		// gets its own warning text rather than reusing "image upload failed".
		p.logger.Error("resolving fanart replace target", "artist", a.Name, "connection", conn.Name, "error", resolveErr)
		p.notifyPushFailure(pid.ConnectionID, conn.Name, classifyPushErr(resolveErr), a.ID, artistDisplayName(a), pushOpImageUpload, resolveErr)
		return false, truncateWarning(fmt.Sprintf("%s (%s): could not resolve fanart replace target, upload skipped", conn.Name, conn.Type))
	}

	if decision.Kind == fanartTargetNoop {
		// The platform already reflects this content (a retry that landed
		// after a lost response, or a no-op sync). Nothing to upload, and
		// this connection did not receive a fresh write, so it must NOT be
		// added to uploadedTo -- there is nothing new for the post-push
		// re-assertion to protect. Checked BEFORE the platform-capability
		// branch below on purpose: fanartTargetNoop is "nothing changed" on
		// EVERY platform, Jellyfin included, and the destructive full resync
		// must never run when there is nothing to fix (see
		// fanartTargetNoop's own doc comment).
		return false, ""
	}

	// #3135: everything past this point authorizes a write, because decision
	// is NOT a no-op. Emby's indexed endpoint replaces in place (verified
	// live, #3125), so the switch below is correct for it. Jellyfin's
	// identical-looking endpoint does not -- see
	// connection.SupportsIndexedBackdropReplace's doc comment for the
	// measured wire behavior -- so an Index or Append decision there is
	// rerouted to the only sequence Jellyfin actually honors: delete every
	// existing backdrop and re-upload the full ordered local set.
	// decision.Index/Why are Emby-shaped guidance and are deliberately
	// unused past this branch; the resync below re-derives everything it
	// needs (the full local fanart set and the platform's current count)
	// itself, because it is replacing the platform's entire backdrop list,
	// not writing one slot.
	if !connection.SupportsIndexedBackdropReplace(conn.Type) {
		return p.uploadFanartFullResyncForSync(ctx, a, pid, conn)
	}

	switch decision.Kind {
	case fanartTargetIndex:
		if uploadErr := client.UploadImageAtIndex(ctx, pid.PlatformArtistID, "fanart", decision.Index, data, ct); uploadErr != nil {
			p.logger.Error("syncing fanart to platform", "artist", a.Name, "connection", conn.Name, "type", "fanart", "index", decision.Index, "error", uploadErr)
			p.notifyPushFailure(pid.ConnectionID, conn.Name, classifyPushErr(uploadErr), a.ID, artistDisplayName(a), pushOpImageUpload, uploadErr)
			return true, truncateWarning(fmt.Sprintf("%s (%s): image upload failed", conn.Name, conn.Type))
		}
		return true, ""
	default: // fanartTargetAppend
		// LOUD, not silent: this is the one case where the fix cannot do
		// better than the pre-#3125 behavior (an accepted duplicate), and an
		// operator or future maintainer needs to be able to find WHY in the
		// logs rather than simply seeing a duplicate reappear.
		p.logger.Warn("fanart replace could not identify a safe platform index; appending instead of overwriting",
			"artist", a.Name, "connection", conn.Name, "reason", decision.Why)
		if uploadErr := client.UploadImage(ctx, pid.PlatformArtistID, "fanart", data, ct); uploadErr != nil {
			p.logger.Error("syncing fanart to platform", "artist", a.Name, "connection", conn.Name, "type", "fanart", "error", uploadErr)
			p.notifyPushFailure(pid.ConnectionID, conn.Name, classifyPushErr(uploadErr), a.ID, artistDisplayName(a), pushOpImageUpload, uploadErr)
			return true, truncateWarning(fmt.Sprintf("%s (%s): image upload failed", conn.Name, conn.Type))
		}
		return true, ""
	}
}

// uploadFanartFullResyncForSync performs the ONLY sequence Jellyfin actually
// honors for a fanart replace (#3135): delete every existing backdrop on
// THIS ONE connection, then re-upload the FULL local fanart set in index
// order. Called from uploadFanartForSync only when
// connection.SupportsIndexedBackdropReplace(conn.Type) is false and the
// resolver already ruled out fanartTargetNoop, so this always attempts at
// least one write UNLESS the restorability guard below refuses first.
//
// RE-DISCOVERS THE LOCAL SET rather than reusing the `data` bytes
// uploadFanartForSync was called with, which are only the PRIMARY fanart
// file (slot 0 -- syncImageToPlatforms discovers exactly one local file, see
// uploadOneImageForSync). A Jellyfin resync must replace the platform's
// ENTIRE backdrop list, or the clear step below would delete every
// non-primary backdrop and never restore it, so this walks the fanart
// directory the same way syncAllFanartToPlatforms does.
//
// ESTABLISH RESTORABILITY BEFORE ANY DELETE, NEVER AFTER (#3145 hostile
// review). snapshotFanart can DEGRADE a slot to nil data -- a read failure,
// or the per-file/total size caps (fanartSnapshotBudget; a 4K backdrop can
// exceed maxFanartSnapshotFileBytes on its own) -- and the upload loop below
// already correctly skips a nil-data entry. The delete loop, though, clears
// the platform's ENTIRE existing backdrop set based on the PLATFORM's own
// count, with no awareness of which local slots actually captured. Before
// this guard, that meant a single oversize or unreadable local file turned
// a size-cap DEGRADE (previously just "this platform copy stays stale") into
// a size-cap DESTRUCTION (every backdrop on the connection, deleted, with no
// byte to replace the degraded slot) -- reproduced live in review: 2 local
// files with one 1 byte over cap, platform starting with 2 backdrops, ended
// at deletes=2/uploads=1/BackdropCount=0. That is strictly worse than the
// pre-#3135 state, where a Jellyfin sync never deleted anything at all.
//
// So: refuse the WHOLE resync -- issue NEITHER a delete NOR an upload --
// the moment ANY snapshot slot failed to capture, rather than attempting a
// partial clear-and-rebuild that only deletes/restores the slots that DID
// capture. A partial resync is not simply a smaller version of the full
// one: Jellyfin's indexed endpoint ignores the index parameter entirely and
// assigns platform position purely by APPEND ORDER (see
// connection.SupportsIndexedBackdropReplace's doc comment and the #3135
// issue's live measurement), so this function's correctness depends on
// deleting the COMPLETE prior set and re-uploading the COMPLETE captured
// set in strict ascending order -- a gap left by a skipped degraded slot
// would silently shift every LATER captured slot's actual platform position
// away from its local index, a second and subtler data-integrity bug
// layered on top of the one this guard closes. Refusing outright is the
// only shape that is both correct and simple to verify; the platform's
// existing backdrops (any local counterpart's degradation notwithstanding)
// are left completely untouched, including ones that outnumber the local
// set entirely (platform holds more backdrops than there are local files at
// all) -- nothing is deleted, so nothing the operator did not touch locally
// can be stranded or destroyed by this refusal.
//
// THE DESTRUCTIVE WINDOW, when the guard above does NOT fire, is
// ACKNOWLEDGED, NOT HIDDEN: between the deletes finishing and the reuploads
// landing, this connection genuinely has ZERO backdrops -- Jellyfin's API
// has no atomic multi-image replace primitive (see the #3135 issue's "why
// delete-then-upload does not work around it"). Deletes run high-index-first
// (DeleteImageAtIndexRaw's re-indexing contract) and the reupload loop
// starts immediately after, on the assumption that a brief empty window
// during an operator-initiated replace beats the duplication #3135 exists
// to fix.
//
// A CRASH DURING THAT WINDOW IS NOT SELF-HEALED, and a future reader should
// not assume otherwise (#3145 review). If the process dies between the
// delete loop and the upload loop completing, this connection is left with
// FEWER backdrops than the local set. The background reconciler
// (reconcile.go) detects exactly that mismatch and repairs it by calling
// uploadFanartSet -- SyncAllFanartToPlatforms's per-file upload, which
// issues UploadImageAtIndex with NO preceding delete step. On Jellyfin that
// APPENDS (the same append-only behavior this whole function exists to work
// around), so the reconciler's "repair" produces DUPLICATES on top of
// whatever survived the crash, not a clean restoration. Fixing the
// reconciler's Jellyfin path is out of scope here; this comment exists so
// the gap is not silently rediscovered.
//
// A FAILURE MID-RESYNC (after the restorability guard has passed) IS
// REPORTED, NEVER SWALLOWED, AND BOTH LOOPS RUN TO COMPLETION REGARDLESS:
// stopping partway would leave the artist with SOME backdrops deleted and
// NONE restored, strictly worse than continuing (deletePollutedBackdrops
// makes the same "continue and report" choice for the same reason). Every
// failure is folded into the returned warning.
//
// NO ADDITIONAL LOCK: this runs inside uploadFanartForSync's own
// lockPhashTarget critical section (held across resolve+this call, released
// by its defer), so taking a second lock on the same key would deadlock.
// That nesting is what makes the destructive window above safe against a
// CONCURRENT sync of the same artist/connection.
func (p *Publisher) uploadFanartFullResyncForSync(ctx context.Context, a *artist.Artist, pid artist.PlatformID, conn *connection.Connection) (uploaded bool, warning string) {
	client := newFanartResyncClient(conn, p.logger)
	if client == nil {
		p.logger.Warn("unsupported connection type for fanart resync", "type", conn.Type)
		return false, truncateWarning(fmt.Sprintf("%s: unsupported connection type %q", conn.Name, conn.Type))
	}

	dir := p.ImageDir(a)
	if dir == "" {
		return false, truncateWarning(fmt.Sprintf("%s: artist has no image directory configured, fanart resync skipped", conn.Name))
	}
	primary := p.getActiveFanartPrimary(ctx)
	fanartPaths, discoverErr := img.DiscoverFanart(ctx, dir, primary)
	if discoverErr != nil {
		p.logger.Error("discovering fanart for platform resync", "artist", a.Name, "connection", conn.Name, "error", discoverErr)
		return false, truncateWarning(fmt.Sprintf("%s: failed to read fanart directory for resync", conn.Name))
	}
	if len(fanartPaths) == 0 {
		// resolveFanartReplaceTarget already ruled out noop against `data`
		// (the primary), so an empty local set here means the primary was
		// removed between that read and this one -- an operator racing this
		// sync with a delete, not the expected path. Report it rather than
		// silently doing nothing.
		return false, truncateWarning(fmt.Sprintf("%s: no local fanart found for resync", conn.Name))
	}

	snapshot, snapWarnings, snapErr := p.snapshotFanart(ctx, fanartPaths)
	if snapErr != nil {
		p.logger.Warn("fanart resync snapshot aborted", "artist", a.Name, "connection", conn.Name, "error", snapErr)
		return false, truncateWarning(fmt.Sprintf("%s: fanart resync canceled before it could read the local set", conn.Name))
	}
	if !hasReadableFanart(snapshot) {
		return false, truncateWarning(fmt.Sprintf("%s: fanart resync found no readable local fanart", conn.Name))
	}

	// #3145: RESTORABILITY GATE, before any platform read or write. A
	// snapshot slot with nil data means snapshotFanart could not capture it
	// (read failure, or a size-cap degrade -- see the doc comment above for
	// why this is refused rather than partially resynced).
	//
	// #3146 CR review: the refusal reason is built and returned FIRST in the
	// joined warning list, snapshot noise appended AFTER, never the other
	// way around. truncateWarning caps each element individually, but the
	// FINAL strings.Join(warnings, "; ") is truncated a second time -- with
	// a long connection name or several snapWarnings entries ahead of it,
	// the refusal reason (the only actionable line: it names the slot that
	// blocked the resync and why nothing was deleted) could be the part
	// truncated away, leaving the operator with snapshot noise and no
	// explanation. Ordering it first means truncation eats the LESS
	// important text when it has to eat something.
	for _, sf := range snapshot {
		if sf.data == nil {
			p.logger.Warn("fanart resync refused: a local fanart slot could not be captured, and completing the resync would delete platform backdrops with nothing to restore them",
				"artist", a.Name, "connection", conn.Name, "index", sf.index)
			refusal := truncateWarning(fmt.Sprintf("%s (%s): fanart resync skipped -- fanart %d could not be captured, so deleting and rebuilding the platform's backdrop set would destroy data", conn.Name, conn.Type, sf.index))
			warnings := append([]string{refusal}, snapWarnings...)
			return false, truncateWarning(strings.Join(warnings, "; "))
		}
	}

	var warnings []string
	warnings = append(warnings, snapWarnings...)

	detail, detailErr := client.GetArtistDetail(ctx, pid.PlatformArtistID)
	if detailErr != nil {
		p.logger.Error("reading platform backdrop state for fanart resync", "artist", a.Name, "connection", conn.Name, "error", detailErr)
		p.notifyPushFailure(pid.ConnectionID, conn.Name, classifyPushErr(detailErr), a.ID, artistDisplayName(a), pushOpImageUpload, detailErr)
		return false, truncateWarning(fmt.Sprintf("%s (%s): could not read platform backdrop state, fanart resync skipped", conn.Name, conn.Type))
	}

	// High-index-first (DeleteImageAtIndexRaw's own doc comment): the peer
	// re-indexes remaining backdrops after every delete, so an ascending
	// loop would skip every other slot.
	for i := detail.BackdropCount - 1; i >= 0; i-- {
		if delErr := client.DeleteImageAtIndex(ctx, pid.PlatformArtistID, "fanart", i); delErr != nil {
			p.logger.Error("deleting backdrop during fanart resync", "artist", a.Name, "connection", conn.Name, "index", i, "error", delErr)
			p.notifyPushFailure(pid.ConnectionID, conn.Name, classifyPushErr(delErr), a.ID, artistDisplayName(a), pushOpImageUpload, delErr)
			warnings = append(warnings, truncateWarning(fmt.Sprintf("%s (%s): fanart resync could not clear backdrop %d, continuing", conn.Name, conn.Type, i)))
		}
	}

	anyUploaded := false
	for _, sf := range snapshot {
		// The restorability gate above already proved every entry here has
		// non-nil data; no nil-skip is needed (or possible) at this point.
		ct := "image/jpeg"
		if strings.EqualFold(filepath.Ext(sf.path), ".png") {
			ct = "image/png"
		}
		if uploadErr := client.UploadImageAtIndex(ctx, pid.PlatformArtistID, "fanart", sf.index, sf.data, ct); uploadErr != nil {
			p.logger.Error("uploading backdrop during fanart resync", "artist", a.Name, "connection", conn.Name, "index", sf.index, "error", uploadErr)
			p.notifyPushFailure(pid.ConnectionID, conn.Name, classifyPushErr(uploadErr), a.ID, artistDisplayName(a), pushOpImageUpload, uploadErr)
			warnings = append(warnings, truncateWarning(fmt.Sprintf("%s (%s): fanart resync failed to upload backdrop %d", conn.Name, conn.Type, sf.index)))
			continue
		}
		anyUploaded = true
	}

	// #2698/#2712: `uploaded` follows the same contract as every other
	// branch in uploadFanartForSync -- true whenever this connection
	// actually received a write, so the caller's post-push re-assertion
	// protects the local files a peer might have clobbered while accepting
	// these uploads. The deletes above touch only the PLATFORM, never local
	// disk, so they need no such protection themselves.
	return anyUploaded, truncateWarning(strings.Join(warnings, "; "))
}

// previousFanartPrimaryData returns the bytes of the artist's PREVIOUS
// primary fanart image (#3125 C1), or nil when unavailable -- no backup on
// record (a first-ever fanart save, or a save whose backup was already
// consumed), or a read failure. Both are treated identically by the caller
// (resolveFanartReplaceTarget: "cannot identify, fall back to append").
//
// SOURCED FROM THE ONE-DEEP ON-DISK BACKUP (image.ReadSlotBackup), NEVER
// FROM artist_images.PHash. A round-1 review found the database-hash design
// INERT IN PRODUCTION: SyncImageToPlatforms (this function's caller, via
// syncImageToPlatforms) always runs AFTER the DB row for the JUST-SAVED file
// has already been stamped. Every write path takes this shape --
//
//	internal/api/handlers_image.go:171   updateArtistImageFlag -> setArtistImageFlag
//	                                      -> recordImageProvenanceSlot0(ctx, a.ID, imageType, NEW file)
//	internal/api/handlers_image.go:257   (~85 lines later) SyncImageToPlatforms
//
// -- and the rule engine's two callers match it (bulk_executor.go:536 records
// provenance from the just-saved path, :538 syncs; fixer.go calls
// recordSavedImageProvenance before publishAfterFix). So a DB read at sync
// time always returns the NEW file's own hash, which trivially "matches" the
// upload being sent every single time: the previous-primary branch was
// unreachable, and every replace silently took the append fallback --
// reproduced end to end in round 1 (backdropCount 1 -> 2, appends=1, on
// what should have been an in-place replace).
//
// The on-disk backup has no such race. BackupSlot writes it INSIDE
// SaveSlotProtected, STRICTLY BEFORE the destructive Save that overwrites
// the canonical file -- and every fanart-primary write in this codebase
// (API upload/crop/fetch, apply-candidate, the rule engine's
// downloadAndPersist and BulkExecutor.saveBestImage) reaches disk through
// SaveSlotProtected, the single chokepoint TestFanartSaveHasASingleChokepoint
// enforces. So by construction the backup this reads is what was on disk --
// and, assuming the previous sync succeeded, what the platform holds --
// at the instant just BEFORE the CURRENT save, never the new bytes.
//
// dir and primaryFileName are the SAME values syncImageToPlatforms already
// resolved for its own upload (p.ImageDir(a) and the discovered primary
// basename), so this makes no extra filesystem probe beyond the one
// directory read the backup lookup itself needs.
//
// ctx-aware (CR review round): ReadSlotBackup's directory listing and file
// read now both go through internal/image's cancellable-read primitives
// (#2689), the same idiom already used for every other on-disk image read
// in this codebase -- a stalled network-mounted library previously could
// block this call indefinitely with no way for a caller's ctx to interrupt
// it, ahead of the platform calls this same sync still needs to make.
//
// Best-effort and NEVER fatal to the sync: a read failure here (including a
// ctx-cancellation error) must not block the upload, it only narrows
// resolveFanartReplaceTarget's options down to append -- the safe
// direction.
func (p *Publisher) previousFanartPrimaryData(ctx context.Context, dir, primaryFileName string) []byte {
	data, err := img.ReadSlotBackup(ctx, dir, "fanart", primaryFileName)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.logger.Warn("reading previous-primary backup for replace-target resolution; falling back to append",
				"dir", dir, "error", err)
		}
		return nil
	}
	return data
}

// repairAfterPush runs one repair pass, waits for the peer to settle, and runs
// the SAME pass once more (#2712).
//
// WHY A SECOND PASS. The repair it wraps is a point-in-time check: it answers
// "is the file still what I uploaded?" for damage that has already landed. A
// peer that performs its delete AFTER responding to the upload is invisible to
// one pass, and that shape was measured on the fanart path (~15ms after
// UploadImage returned). The second pass is what sees it.
//
// WHY IT REUSES pass RATHER THAN REIMPLEMENTING THE CHECK. Everything that
// makes the repair safe lives inside reassertLocalImage -- the #2712 delete-
// intent gate on the ENOENT branch, the detached-but-bounded verify read, the
// "unreadable is not absent" refusal. A second implementation would be a second
// place for those to drift, and the one that drifted would be the one running
// after the operator's delete. So this function knows nothing about repair; it
// knows about TIMING only.
//
// SAFE TO RUN TWICE, which is the property that makes this legal at all.
// reassertLocalImage is idempotent by construction: its first branch returns
// immediately when the bytes on disk already equal the snapshot, so a second
// pass over a healthy file (including one the first pass just restored) writes
// nothing. It cannot resurrect an operator delete either -- the gate is inside
// the pass, so the reverify consults the same marker against the same push
// baseline and stands down for exactly the same deletes.
//
// THE TWO PASSES DO NOT REPAIR THE SAME SET OF DAMAGE, and that asymmetry is
// the whole reason repairScope exists. The first pass keeps both branches: a
// missing file and a rewritten file are equally the peer's doing at that
// instant, because the push has not yet let go of the file. The second pass
// runs AFTER a 250ms window during which the operator's UI is live, so a
// mismatch it sees can equally well be the operator's own brand-new crop --
// and restoring the pre-push bytes over that would revert a save the operator
// just made, while blaming a peer in the log. So the second pass repairs only
// the ENOENT case. See repairScope for why that is the right side to keep.
//
// BOUNDED, and the bound is the point: exactly two passes and one wait of
// reassertSettleDelay, never a loop and never a retry-until-clean. Every push
// that reached a peer pays that wait once; see the constant for the cost
// argument and for why polling would be worse rather than better.
//
// THE WAIT IS NOT CANCELLABLE, and that is deliberate rather than an oversight.
// The obvious refinement is to abort the wait when the caller's context is done,
// but the canceled push is exactly the case this exists to serve -- the repair
// itself detaches from ctx for that reason -- and cutting the wait short there
// makes the second pass run back to back with the first, which is the same
// point-in-time check twice and covers nothing. The cost of not honoring it is a
// bounded 250ms on a goroutine that does no I/O while it waits.
func (p *Publisher) repairAfterPush(pass func(repairScope)) {
	pass(repairAllDamage)
	time.Sleep(reassertSettleDelay)
	pass(repairMissingOnly)
}

// repairScope says which shapes of damage a given repair pass is allowed to
// undo (#2712). It exists because the second, post-settle pass looks at the
// files 250ms later than the first, and 250ms is long enough for the OPERATOR
// to have acted.
//
// THE DEFECT IT PREVENTS, concretely, because this reads like a nicety and is
// not one. Peer behaves perfectly. Pass 1 reads the file, finds exactly the
// bytes it pushed, and returns. The push then waits out the settle window, and
// during that window the operator saves a new crop of that same slot. Pass 2
// reads the file, sees bytes that differ from the PRE-PUSH snapshot, and -- if
// it were allowed the overwrite branch -- writes the pre-push bytes back over
// the crop the operator saved seconds ago, logging it as a peer clobber. The
// operator's work is gone and the log points at the wrong party. That is a
// window this settle pass OPENED, so this pass is where it has to be closed.
//
// WHY ENOENT IS THE SIDE WORTH KEEPING. A late DELETE is the only shape the
// issue documents and the only one this codebase has measured (~15ms after
// UploadImage returned, on the fanart path); a peer that REWRITES the file
// after it has already answered the upload has never been observed here. And
// the two shapes are not symmetric in what the gate can do about them: a late
// delete is still attributable, because img.MarkDeleteIntent lets an operator
// delete announce itself and the ENOENT branch consults it, so the second pass
// restores only deletes NOBODY claimed. A late overwrite has no such signal --
// that is the whole reason the overwrite branch is deliberately ungated -- so
// there is no version of the second pass that repairs it without also being
// able to revert an operator save.
//
// WHAT PASS 1 KEEPS. Every bit of it. Pass 1 runs the instant UploadImage
// returns, before any settle window has been waited out, and it is what serves
// the #2533 crop-clobber repair. Nothing about that branch is gated, narrowed,
// or made conditional here; only the LATER look is narrowed. A change that
// gates pass 1's overwrite branch would disable the repair this whole mechanism
// exists for.
type repairScope int

const (
	// repairAllDamage repairs both a missing file and a rewritten one. This is
	// what the first pass runs with, and it is the historical behavior.
	repairAllDamage repairScope = iota
	// repairMissingOnly repairs a file that is GONE and leaves a file whose
	// bytes merely differ alone. The second pass runs with this.
	repairMissingOnly
)

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
//
// THAT COST IS BOUNDED, in three directions (#2712), because "read everything"
// with no ceiling is a memory commitment an artist directory gets to choose and
// concurrent syncs multiply: maxFanartSnapshotFiles files,
// maxFanartSnapshotFileBytes for any one file, and maxFanartSnapshotTotalBytes
// cumulative. Each constant documents where its number comes from.
//
// EXCEEDING A BOUND DEGRADES LOUDLY, in the same three signals an unreadable
// file already produces -- an Error log with structured fields, a warning
// appended for the caller to surface to the operator, and a nil-data entry so
// every LATER slot keeps its TRUE index. Silence here would turn a memory guard
// into invisible data loss, which is why the degrade is three simultaneous
// signals rather than a debug line.
//
// The bounds do NOT abort the loop. A single over-size file says nothing about
// the rest of the set, so it is skipped like any other per-file failure. The
// two cumulative caps then behave DIFFERENTLY from each other, and the
// difference is worth stating because "the caps stop capturing" reads as a
// latch that only one of them has. Once the COUNT cap is reached nothing
// further is captured, because b.files never decreases. The TOTAL cap is
// re-evaluated per file against the REMAINING budget, so a later, smaller
// backdrop that still fits IS captured after a larger one was refused. Either
// way the loop walks the rest of the set so every slot is accounted for and
// warned about, rather than the set silently ending early.
//
// THE REPORT IS BOUNDED TOO (#3018 review), and separately from the snapshot.
// Walking every slot is what makes a large directory produce a warning per
// refused file, so the caps that stop this function holding a directory's worth
// of bytes did nothing about it returning a directory's worth of strings. See
// maxFanartSnapshotWarnings: the response carries the first N slots plus a
// count of the rest, while the Error log still names every one.
//
// THE PER-FILE CAP IS DECOUPLED FROM THE PUSH (#3017). It used to borrow the
// unreadable-file shape -- refuse before the read, keep a nil-data
// placeholder -- and that is right for an unreadable file (which could not
// have been uploaded either way) but wrong for one merely over
// maxFanartSnapshotFileBytes: a 13 MiB backdrop is under img.MaxDecodeBytes
// (25 MB), so it is perfectly legal to read and upload, and refusing it
// before the read also dropped it from uploadFanartSet, which skips every
// nil-data entry. If that file was the artist's ONLY backdrop,
// hasReadableFanart was false and the sync returned before the upload loop
// ran at all -- a push that worked before the cap existed did nothing after.
//
// So the per-file size cap no longer refuses before the read, or does
// anything else at read time: every file under img.MaxDecodeBytes is read
// and, downstream in uploadFanartSet, pushed to every peer regardless of its
// size. maxFanartSnapshotFileBytes is, as of this branch, a READ-TIME
// classification only -- a threshold that no longer gates anything in this
// function or the ones it calls. It does NOT bound how long a captured
// over-threshold file's bytes stay resident: the whole point of capturing
// them is that syncAllFanartToPlatforms's deferred repair (repairAfterPush)
// may need to restore this exact file if a peer clobbers it mid-push, and
// that need does not end until the repair's passes complete. An earlier
// version of this branch tried to null the bytes once every peer had been
// uploaded to, which (a) ran too late to matter -- the enclosing function's
// own snapshot local goes out of scope at return regardless -- and (b)
// documented a per-push memory bound that was never actually enforced at
// any instant when holding the bytes could matter (the read, every peer
// upload, and both repairAfterPush passes with its 250ms settle window all
// still need every captured byte resident). That mechanism has been
// removed; see maxFanartSnapshotTotalBytes below for what actually bounds
// this push's resident footprint. What the count and total-bytes caps below
// still do is refuse BEFORE the read, because a directory's total resident
// footprint is what those two protect, and that protection would be
// defeated by reading first.
//
// A second, smaller consequence stays OPEN and UNFIXED here: the fanart sync
// is additive -- it writes backdrop indices and never deletes surplus ones on
// its own -- so a platform index a refused (count/total-cap) slot used to
// occupy keeps whatever image it already held, indefinitely. A stale-tail
// reconciler was drafted and pulled from this branch after a hostile review
// found it introduced its own destructive-delete defects (deleting fresh
// Jellyfin appends, deleting operator-owned platform images past a
// negatively-inferred local count); it is scoped as separate follow-up work,
// not fixed by this comment. Preserving the TRUE index below is what keeps
// the stale image CONFINED to the refused slots instead of shifting every
// later backdrop onto the wrong index, which is the worse failure and the
// one this code does prevent.
//
// The reads are ctx-bound and size-bounded (#2934). DiscoverFanart above is
// already cancellable, so a bare os.ReadFile here left the same defect the
// primary path had, only worse: this loop reads the WHOLE set, so a mount that
// stops answering wedges on file 1 of 42 and no caller deadline can reach it.
// The bound is also the allocation guard -- a set of arbitrarily large operator
// files was previously read whole into memory, all of it resident at once.
//
// An ordinary per-file failure still SKIPS that file and keeps going, keeping a
// nil-data entry so the slots AFTER it retain their TRUE index. That is the
// deliberate shape: for a vanished file, a permissions error or an over-size
// file, this function's contract is "one warning per file that could not be
// read", and the caller's hasReadableFanart check then declines the push
// because nothing was captured.
//
// A CANCELLATION IS NOT ONE OF THOSE, and it is returned rather than warned
// about (#2934). Bounding the read stopped this loop hanging on a dead mount;
// it did NOT stop a canceled request becoming a partial success, because
// ReadImageFileBounded returns the context error in exactly the shape an
// ordinary read failure has. Classified as per-file, the loop kept going, the
// earlier snapshots were retained, and the caller walked on into its upload
// loop -- so a request the operator abandoned still pushed files to peers, and
// the push handler answered 200 with an errors list. Bounding the read and
// propagating the cancellation are two separate requirements and this branch
// owes both.
//
// Interrogate the CONTEXT rather than the error's contents, as everywhere else
// here: an I/O error whose text mentions a deadline is not a cancellation, and
// a cancellation wrapped in an ordinary-looking read failure still is one.
//
// The snapshot captured so far is returned ALONGSIDE the error rather than
// discarded. The bytes already in hand are exactly what the deferred
// re-assertion would need if any peer had been reached, so throwing them away
// would trade this defect for a worse one. It is the caller's job to stop
// before the upload loop.
func (p *Publisher) snapshotFanart(ctx context.Context, fanartPaths []string) ([]fanartSnapshot, []string, error) {
	snapshot := make([]fanartSnapshot, 0, len(fanartPaths))
	var warnings fanartWarningLog
	var budget fanartSnapshotBudget
	for i, fp := range fanartPaths {
		// #2712: refuse BEFORE the read, so an over-budget file is never
		// allocated at all. Checking after the read would make the cap a
		// bookkeeping exercise rather than a memory guard.
		//
		// #3017: this pre-read refuse no longer answers for the PER-FILE size
		// alone -- only for the count and total-bytes caps, which still bound
		// what this push holds AT ONCE and so still refuse before allocating.
		// A file over the per-file cap is read anyway; see this function's
		// doc comment for why and refuse's doc comment for what changed.
		if reason, refused := budget.refuse(ctx, fp, i); refused {
			entry, warning := p.degradeFanartSlot(fp, i, reason)
			snapshot = append(snapshot, entry)
			warnings.add(warning)
			continue
		}
		data, readErr := img.ReadImageFileBounded(ctx, fp)
		if readErr != nil {
			// A cancellation is not the only failure that distrusts the whole
			// loop; the process-wide stalled-read cap says the same thing by a
			// different route (#2933). Both mean the read did not happen for a
			// reason that applies to every REMAINING file too.
			//
			// This is the most consequential of the three call sites that share
			// this predicate. These bytes are the ONLY copy that can undo a
			// peer's delete: a peer does not necessarily remove the file it was
			// just handed (measured on Emby 4.10, uploading slot 0 deleted the
			// slot-1 file), and the restore loop SKIPS any entry whose data is
			// nil. So classifying a cap refusal as per-file kept nil-data
			// entries, let the push proceed, and left a peer-deleted file with
			// nothing to restore from -- unrecoverable, not merely redundant.
			if distrust := img.ReadFailureDistrustsLoop(ctx, readErr); distrust != nil {
				// Name the CAUSE, not just the effect (#2976 review). A
				// cancellation is the caller walking away; a cap refusal is
				// the mount not answering while the caller is still waiting.
				// Both abort the snapshot, but an operator reading "the
				// request ended" for a stalled mount goes looking at the
				// wrong end of the system.
				reason := "the request ended before all fanart could be read"
				if errors.Is(distrust, img.ErrTooManyStalledReads) {
					reason = "the library mount is not responding, so the remaining fanart could not be read"
				}
				p.logger.Warn("fanart snapshot aborted; no further fanart will be read",
					slog.String("path", fp),
					slog.Int("index", i),
					slog.Int("captured", len(snapshot)),
					slog.String("reason", reason),
					slog.Any("error", distrust))
				return snapshot, warnings.result(), distrust
			}
			p.logger.Error("reading fanart for platform sync",
				slog.String("path", fp),
				slog.String("error", readErr.Error()))
			if errors.Is(readErr, img.ErrImageTooLarge) {
				warnings.add(truncateWarning(fmt.Sprintf("fanart %d exceeds the size limit for upload", i)))
			} else {
				warnings.add(truncateWarning(fmt.Sprintf("failed to read fanart %d", i)))
			}
			// Kept with nil data so the slots AFTER this one keep their real
			// index; the upload loop skips nil entries.
			snapshot = append(snapshot, fanartSnapshot{path: fp, index: i})
			continue
		}
		// #2712: check the RESULT, not just the stat that preceded it.
		//
		// The pre-read refusal above is a stat, and a stat is a prediction: the
		// file can grow between the stat and the read, so on its own the cap
		// bounds a NUMBER while the read stays unbounded. internal/image's
		// readio.go makes exactly this argument for why it uses io.LimitReader
		// rather than a stat, and it applies here with the multiplier this
		// function adds -- it holds up to maxFanartSnapshotFiles results at
		// once, so believing every stat could leave 100 reads of
		// img.MaxDecodeBytes resident against a documented 192 MiB bound.
		//
		// So the stat stays (it is what keeps an honestly-huge file from being
		// allocated AT ALL, which is the cheap and common case) and this second
		// check is what makes the bound true: bytes that overshoot are DISCARDED
		// rather than retained, and the slot degrades exactly like any other
		// uncaptured one. That bounds what is resident to the total cap plus the
		// single read in flight, which img.ReadImageFileBounded already caps.
		if reason, refused := budget.refuseResult(int64(len(data)), i); refused {
			entry, warning := p.degradeFanartSlot(fp, i, reason)
			snapshot = append(snapshot, entry)
			warnings.add(warning)
			continue
		}
		budget.charge(int64(len(data)))
		snapshot = append(snapshot, fanartSnapshot{path: fp, index: i, data: data, mod: fileModTime(fp)})
	}
	return snapshot, warnings.result(), nil
}

// fanartWarningLog accumulates the per-slot warnings one snapshot produces and
// bounds how many of them reach the caller (#3018 review).
//
// It exists because snapshotFanart deliberately walks EVERY path even after a
// cap is hit -- that is what keeps each surviving slot's true index -- so the
// number of warnings scales with the artist directory, which is precisely the
// input the #2712 caps exist to stop trusting. Each string was already
// truncateWarning-bounded; nothing bounded the count, so a directory holding
// thousands of files returned thousands of strings into an API response.
//
// The overflow is COUNTED rather than dropped. A silent cut would report a
// bounded, plausible-looking list that understates the loss, which is the same
// invisible-data-loss failure the loud degrade was written to prevent. Callers
// therefore get the first maxFanartSnapshotWarnings slots plus one line saying
// how many more there were. degradeFanartSlot's Error log is unaffected and
// still names every slot with its path, so nothing is lost from the record an
// operator debugs from -- only from the response body a browser renders.
type fanartWarningLog struct {
	kept     []string
	overflow int
}

// add records one warning, keeping it only while under the cap.
func (w *fanartWarningLog) add(warning string) {
	if len(w.kept) < maxFanartSnapshotWarnings {
		w.kept = append(w.kept, warning)
		return
	}
	w.overflow++
}

// result returns the bounded warning list, with the overflow summary appended
// when any warning was withheld. It returns nil for an empty log so the
// no-warnings case is indistinguishable from the pre-cap behavior.
//
// The empty case is returned EXPLICITLY rather than by handing back a kept that
// happens to be nil (#3018 review). The nil-ness was incidental: it held only
// because the sole production caller declares the log with var and never
// preallocates, so any future caller (or test) that built kept with make(...)
// and then added nothing would return a non-nil empty slice and quietly break
// the contract this comment states. These warnings are marshaled into an API
// response, where the difference is visible as "warnings": null versus
// "warnings": [], so a stated invariant the code does not enforce is the
// defect rather than a style point.
//
// The summary is written into a slice of its OWN rather than appended onto
// w.kept, and that is a correctness requirement rather than defensive style
// (#3018 review). w.kept reaches the cap by repeated append, so it arrives here
// with spare capacity (len 25, cap 32 as the growth lands), which means
// append(w.kept, ...) writes the summary INTO w.kept's existing backing array
// and hands back a slice aliasing it. Any later append to w.kept then
// overwrites that same index, silently replacing the summary in a slice the
// caller is already holding -- measured: the returned entry became the newly
// appended warning and "and N more" was gone.
//
// That failure lands on the one line whose entire job is to stop a truncated
// list reading as a complete one, so the aliasing would quietly restore the
// invisible-data-loss the counted overflow exists to prevent.
func (w *fanartWarningLog) result() []string {
	if len(w.kept) == 0 && w.overflow == 0 {
		return nil
	}
	if w.overflow == 0 {
		return w.kept
	}
	out := make([]string, len(w.kept), len(w.kept)+1)
	copy(out, w.kept)
	return append(out, truncateWarning(fmt.Sprintf(
		"and %d more fanart slots were neither captured for restore nor synced to platforms; see the server log for each one",
		w.overflow)))
}

// fanartSnapshotBudget tracks what one snapshot has already committed, so the
// three #2712 caps can be enforced BEFORE each read rather than after it.
//
// It deliberately counts only files that were actually CAPTURED. A slot skipped
// for any reason -- over budget, unreadable, over-size -- holds no bytes, so
// charging it against the budget would let a directory full of junk starve the
// real backdrops behind it out of their capture.
type fanartSnapshotBudget struct {
	files int
	bytes int64
}

// refuse reports whether this file must be skipped to stay inside the caps, and
// the operator-facing reason if so.
//
// The per-file size is taken from a stat rather than from the read, because the
// whole point is to avoid allocating the file at all. A stat that fails is NOT
// treated as a refusal: the read that follows will produce the real error with
// the real cause, and inventing a budget refusal for what is actually a
// permissions problem would send the operator looking at the wrong thing.
// EVERY REFUSAL NAMES THE SLOT, and that is not cosmetic. These strings become
// operator-facing warnings, one per refused file. Without the index, an artist
// holding 105 backdrops produces five byte-identical sentences and the operator
// cannot tell which backdrops are now unrepairable -- which is the one fact the
// warning exists to convey.
//
// THE STAT IS CTX-BOUND, and it takes a ctx for that reason alone (#3018
// review). A raw os.Stat here put an UNBOUNDED call in front of the bounded
// read that follows, so a hard-mounted export that stopped answering wedged
// this loop one step before the #2934 bound could apply -- and since
// snapshotFanart reads the whole set before the first upload, that wedge takes
// the push with it and no caller deadline reaches it. The stat running FIRST is
// what makes the cap a memory guard rather than bookkeeping, so the ordering
// stays and the stat gets the bound instead.
func (b *fanartSnapshotBudget) refuse(ctx context.Context, path string, index int) (string, bool) {
	if b.files >= maxFanartSnapshotFiles {
		return fmt.Sprintf("fanart %d was neither captured for restore nor synced to platforms: the set exceeds the %d-file snapshot limit",
			index, maxFanartSnapshotFiles), true
	}
	// A CANCELED OR STALLED STAT IS NOT A REFUSAL, which keeps this arm's
	// existing contract intact: the read that follows returns the same failure
	// in a form the loop already classifies (img.ReadFailureDistrustsLoop
	// aborts the whole set for a cancellation or the stalled-read cap), so
	// reporting a budget refusal here would relabel an abandoned request as an
	// over-size backdrop.
	//
	// #3017: the per-file size is NO LONGER checked here. It used to refuse a
	// file over maxFanartSnapshotFileBytes before ever reading it, which also
	// meant never uploading it -- see snapshotFanart's doc comment for why
	// that was a regression. The stat is still taken and still ctx-bound
	// (unchanged reasoning below), because the TOTAL cap still needs a
	// pre-read size estimate to avoid allocating an honestly-huge file at all.
	info, statErr := img.StatBounded(ctx, path)
	if statErr != nil {
		return "", false
	}
	if size := info.Size(); b.bytes+size > maxFanartSnapshotTotalBytes {
		// "bytes on disk" is what separates this message from refuseResult's
		// ("bytes read") -- see that function's doc comment: the phrase is the
		// only operator-visible (and test-visible) signal for WHICH of the two
		// total-cap checks fired.
		return fmt.Sprintf("fanart %d was neither captured for restore nor synced to platforms: the set exceeds the %d-byte total snapshot limit (%d bytes on disk)",
			index, int64(maxFanartSnapshotTotalBytes), size), true
	}
	return "", false
}

// refuseResult re-applies the TOTAL cap to the bytes ACTUALLY read, which is
// what makes it a memory bound rather than a prediction.
//
// The pre-read stat can be wrong in exactly one direction that matters: the
// file grew between the stat and the read (TOCTOU), so a file that measured
// small can arrive large. Only the value in hand settles it. The caller
// DISCARDS bytes refused here, so a lying stat costs one over-budget read that
// img.ReadImageFileBounded already caps, rather than a retained allocation.
//
// #3017: the per-file cap is NO LONGER one of the two caps re-applied here.
// It used to refuse (and discard) any read over maxFanartSnapshotFileBytes;
// that cap is now a read-time size classification only (see snapshotFanart's
// doc comment) and no longer gates anything in this function.
func (b *fanartSnapshotBudget) refuseResult(n int64, index int) (string, bool) {
	if b.bytes+n > maxFanartSnapshotTotalBytes {
		return fmt.Sprintf("fanart %d was neither captured for restore nor synced to platforms: %d bytes read would exceed the %d-byte total snapshot limit",
			index, n, int64(maxFanartSnapshotTotalBytes)), true
	}
	return "", false
}

// charge records bytes actually captured.
func (b *fanartSnapshotBudget) charge(n int64) {
	b.files++
	b.bytes += n
}

// degradeFanartSlot is the shared degrade path for a slot whose bytes this push
// will not hold: it logs at Error, builds the operator-facing warning, and
// returns the nil-data snapshot entry that keeps every LATER slot's TRUE index.
//
// It exists so the bound refusals cannot drift away from the unreadable-file
// convention whose SHAPE they mirror. The shape is shared; the consequence is
// not (#3017), so the message names both losses.
//
// Error rather than Warn is deliberate and is asserted: this is the only signal
// that a backdrop is silently missing from the operator's peers, and a level a
// default log configuration might drop would put that loss back out of sight.
// The path is here and not in the warning because a filesystem path does not
// belong in an API response, which makes this the only record of WHICH file.
func (p *Publisher) degradeFanartSlot(path string, index int, reason string) (fanartSnapshot, string) {
	p.logger.Error("fanart slot NOT captured for restore and NOT synced to platforms; a peer delete of this file cannot be repaired either",
		slog.String("path", path),
		slog.Int("index", index),
		slog.String("reason", reason))
	return fanartSnapshot{path: path, index: index}, truncateWarning(reason)
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

// pushScope carries the facts that belong to a whole push rather than to any
// one file it touches. Both fields feed the delete gate in reassertLocalImage,
// and both were positional time.Time / string parameters before.
//
// IT IS A STRUCT TO MAKE A TRANSPOSITION IMPOSSIBLE, not for tidiness. The old
// signature took snapMod and snapAt adjacently, two time.Time values with
// opposite meanings: snapMod comes from the FILESYSTEM and describes the FILE
// (its mtime, log-only), while at comes from THIS PROCESS and describes THE
// PUSH. The compiler cannot catch a swap between two parameters of the same
// type, and the resulting failure is silent rather than loud -- an mtime is
// typically far in the past, so a transposed at would make the gate read almost
// every marker as concurrent and quietly stop repairing peer clobbers, which is
// #2698 reopened with no symptom anyone would notice. snapMod stays positional
// precisely so it can no longer sit next to a same-typed value it can be
// confused with.
//
// dir IS THE RESOLVED IMAGE DIRECTORY (Publisher.ImageDir), threaded from the
// caller rather than re-derived here as filepath.Dir(filePath). Those two are
// not the same string in general: an image-naming pattern may contain a path
// separator ("sub/folder.jpg"), because ValidateImageNaming rejects separators
// only on the platform-profile create/update handlers and the SETTINGS IMPORT
// path (settingsio -> platform.ImportCreateTx) persists ImageNaming with no
// validation at all. FindExistingImageStrict then joins dir with the pattern
// and returns "<dir>/sub/folder.jpg", so filepath.Dir of it is "<dir>/sub"
// while the delete handler marked "<dir>". The keys never meet, the gate never
// fires, and the operator's delete IS resurrected -- silently, and only on the
// configuration that bypassed validation. There is deliberately NO fallback to
// filepath.Dir: a silent fall back to the wrong key is the whole defect.
type pushScope struct {
	// dir is the resolved image directory this push is operating in -- the same
	// value the delete handlers key their marker on.
	dir string
	// at is the wall-clock instant THE PUSH BEGAN, stamped at function entry by
	// both sync entry points. It is the baseline the delete gate compares a
	// marker against, and it is NOT the instant the bytes were read: the
	// prologue between entry and the byte read is time during which the push is
	// already in flight, so a delete landing there is genuinely concurrent.
	at time.Time
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
// TWO GUARDS WERE BUILT AND WITHDRAWN before the current one, and this history
// is kept so the next reader does not rebuild either. The exists flag looks like
// the answer and is not: it is DERIVED FROM DISK (handleServeImage clears it on
// a 404, the scanner recomputes it from a walk), so a fresh read is poisoned by
// the peer's own deletion -- and the value captured before the push is stale for
// a first-ever image, where the flag is still false while the file is already
// written. Gating on it therefore refuses the repair either for a routine save
// or for the exact case #2698 describes. mtime cannot separate the peer from the
// operator either; both writes land in the same window.
//
// WHAT IS NOW GUARANTEED IN THE DELETE DIRECTION (#2712). The delete handlers
// record an explicit, short-lived intent marker (img.MarkDeleteIntent) keyed by
// (image directory, image type), written IMMEDIATELY BEFORE they touch the
// filesystem so an already-in-flight push can observe it. The ENOENT branch
// below consults it against push.at -- the wall-clock instant THE PUSH BEGAN,
// stamped at the sync function's entry rather than when the bytes were read --
// and stands down when a delete was recorded at or after that instant.
//
// PUSH START, NOT BYTE-CAPTURE TIME, and do not "correct" it back. The whole
// prologue between entry and the byte read (the platform-ID lookup, ImageDir,
// the naming-config database read, the discovery stat loop or directory walk,
// and the artwork read itself) is time during which this push is already in
// flight, so an operator delete landing there is genuinely concurrent with it.
// Comparing against the byte-capture instant instead discards every one of
// those deletes as old and unrelated, which is #2712 unfixed for the early --
// and on a network mount the slowest -- part of every push. The regression
// tests in delete_intent_prologue_test.go fail against that ordering.
//
// This is the signal neither withdrawn guard had: it is written by THE
// ACTOR WHOSE INTENT IT RECORDS, rather than inferred from disk state that both
// the operator and the peer mutate. So:
//
//   - An operator delete landing during the push window is HONORED. The artwork
//     is not resurrected, and the operator does not have to delete twice.
//   - A peer clobber with no operator delete in the window is STILL REPAIRED --
//     no marker exists, so the gate does not fire. #2698 does not regress.
//   - A first-ever save is STILL REPAIRED, which is precisely what the withdrawn
//     exists-flag guard broke: a slot nobody deleted has no marker.
//
// RESIDUAL RISK in the delete direction, accepted: the marker is TYPE-WIDE, not
// per-slot, so a genuine peer clobber of a DIFFERENT fanart slot inside the same
// window is not repaired by this push. It cannot be per-slot -- RenumberFanart
// compacts survivors to contiguous indices on every slot delete, so a slot index
// is not a stable identity for the file an in-flight push is verifying, and a
// per-slot marker would let this repair restore a deleted backdrop under a
// shifted filename. img.MarkDeleteIntent documents the worked case.
//
// Erring toward over-suppression is deliberate, and the reason is NOT that a
// missed repair heals itself -- nothing in this codebase restores a local
// artwork file from a peer. maintenance.ScanExistsFlags only clears exists_flag
// for vanished files, RestoreExistsFlags only sets it for files confirmed
// present, and the artwork reconciler in reconcile.go pushes local bytes OUT.
// The suppressed slot stays missing until the operator re-adds it, prompted by
// the exists-flag scan surfacing it as gone. That is accepted because the two
// failures are not comparable: a missing slot is VISIBLE and re-addable by hand,
// while a resurrected delete is silent, undoes what the operator deliberately
// did, and is not recoverable at all unless they notice and delete again.
//
// The OVERWRITE direction is NOT refused ON THE FIRST PASS, and that is a
// deliberate, documented trade rather than an oversight. mtime cannot separate
// "the peer rewrote it" from "the operator saved again" -- both land between the
// snapshot and the check -- so a guard on newness would disable the crop-clobber
// repair (#2533), which is the case this fix exists to serve.
//
// RESIDUAL RISK IN THE OVERWRITE DIRECTION, accepted, and stated for what it
// now actually is (#2712). The unattributable window is the interval between
// the byte capture and the FIRST repair pass, which ends the instant UploadImage
// returns for the last peer. An operator save landing inside that interval can
// still be written back by this repair: the losing outcome is a stale image,
// recoverable by saving again, and the alternative is losing the #2533 repair
// entirely.
//
// It is specifically NOT bounded by "two saves overlapping within a single 30s
// push" any more, and do not restate it that way -- one save is enough, since
// this repair puts back bytes the push captured rather than bytes the operator
// last chose. What the settle pass changed is that the window no longer EXTENDS
// through the 250ms wait: the second pass runs with repairMissingOnly, so an
// operator save landing during the settle window is left exactly as the operator
// left it. See repairScope, and the paragraph below on what the two passes each
// cover.
//
// WHEN THIS RUNS, AND WHAT THE TIMING NOW COVERS (#2712). Callers do not invoke
// this once. repairAfterPush runs it, waits reassertSettleDelay, and runs it
// again, because a single call is a POINT-IN-TIME check and answers only for
// damage that has already landed. In every case observed on the primary path
// the peer's delete completed BEFORE UploadImage returned, but that is an
// observation about two unsynchronized processes, not a happens-before
// relation, and a delete roughly 15ms AFTER the return was measured on the
// fanart path. The second pass is what catches that shape.
//
// STATE THE LIMIT HONESTLY: two passes over a bounded settle window is a wider
// net, NOT a closed window. A peer that deletes a second after responding still
// escapes both passes, and nothing here will see it -- and nothing else will
// either, because no component in this codebase restores a local artwork file
// from a peer (see the over-suppression paragraph above for why the obvious
// consolations are false). What actually happens is that the exists-flag scan
// surfaces the slot as missing and the operator re-adds it. The settle delay is
// deliberately small rather than generous, because it is latency every push
// pays including the overwhelming majority where no peer misbehaves; see
// reassertSettleDelay for that trade.
//
// The reverify inherits the delete gate rather than working around it: it is
// the same function, consulting the same marker against the same push baseline,
// so it stands down for exactly the same operator deletes the first pass does.
// A second pass that could resurrect a delete the first pass honored would be
// this bug reintroduced by its own fix.
//
// AND THE SECOND PASS IS NARROWER THAN THE FIRST: it runs with
// repairMissingOnly, so it repairs a file that is GONE and leaves a file whose
// bytes merely differ alone. The settle window is live operator time, and a
// mid-window save is indistinguishable from a late peer rewrite on this branch,
// so a second pass with the overwrite branch would revert the operator's newest
// crop back to the pre-push bytes. scope is what says which pass this is; see
// repairScope for the worked case.
//
// snapMod is the file's mtime when its bytes were captured. Retained for the
// restore log so an operator can see how stale the restored copy was. It
// describes the FILE and nothing branches on it.
//
// push carries the two facts that belong to THE PUSH rather than to any one
// file -- the resolved image directory and the instant the push began -- and
// both are load-bearing for the delete gate. See pushScope for why they are a
// struct rather than two more positional parameters.
//
// THE VERIFY READ IS DETACHED FROM ctx AND GIVEN ITS OWN DEADLINE (#2934), and
// that combination is load-bearing in BOTH directions.
//
// Detached, because this repair exists precisely FOR the canceled push. The
// fanart caller registers it in a defer specifically so it still runs when the
// push timed out -- that is when a peer is most likely to have destroyed a file
// with nothing else coming to put it back. Handing it the caller's ctx would
// make the read fail with context.Canceled, which is not os.ErrNotExist, so it
// would take the "cannot verify, leave it alone" branch and silently abandon
// the artwork. Over-propagating a cancellation here loses operator data, which
// is worse than the hang.
//
// But bounded anyway, because "not cancellable" must not mean "unbounded": a
// bare os.ReadFile on a dead mount pins this goroutine forever, and it runs in
// a defer inside a request handler. Its own short deadline is what makes the
// repair give up on its own terms rather than never.
func (p *Publisher) reassertLocalImage(ctx context.Context, a *artist.Artist, imageType, filePath string, data []byte, snapMod time.Time, push pushScope, uploadedTo []string, scope repairScope) {
	verifyCtx, cancelVerify := context.WithTimeout(context.WithoutCancel(ctx), reassertVerifyTimeout)
	defer cancelVerify()

	// readFileBounded returns the open error UNWRAPPED, so the os.ErrNotExist
	// branch below still matches a genuinely missing file -- which is the case
	// that triggers the restore.
	current, readErr := img.ReadImageFileBounded(verifyCtx, filePath)
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
	if !errors.Is(readErr, os.ErrNotExist) {
		// The file is present and its bytes differ from what this push captured.
		//
		// #2712: on a pass whose scope is repairMissingOnly -- the second,
		// post-settle pass -- stop here. By the time that pass runs, 250ms of
		// live UI time has elapsed since the push let go of the file, so those
		// differing bytes are just as likely the operator's new crop as a peer's
		// clobber, and nothing on this branch can tell the two apart (that is
		// exactly why it is ungated for the first pass). Writing here would
		// revert an operator save and blame a peer for it. See repairScope.
		//
		// This is NOT a gate on the first pass. Pass 1 still repairs an
		// overwrite unconditionally, which is the #2533 crop-clobber repair this
		// whole mechanism exists to serve.
		if scope == repairMissingOnly {
			p.logger.Debug("the local image differs after the settle window; leaving it alone",
				"artist", a.Name, "type", imageType, "path", filePath)
			return
		}
	} else {
		outcome = "deleted"

		// #2712: the file is GONE, and this is the only branch where an operator
		// delete is indistinguishable from a peer delete by content alone. Ask
		// the one party that knows -- the delete handler, which recorded its
		// intent before unlinking.
		//
		// ONLY the ENOENT branch is gated. The bytes-mismatch (overwrite) branch
		// below repairs unconditionally, deliberately: a delete marker says
		// nothing about a REWRITE, and gating that branch would disable the
		// #2533 crop-clobber repair, which is the case this whole mechanism
		// exists to serve.
		//
		// THE KEY IS push.dir, NOT filepath.Dir(filePath). See pushScope for the
		// configuration where those differ and the delete is resurrected.
		if img.DeleteIntentAfter(push.dir, imageType, push.at) {
			p.logger.Info("the local image is gone after the push, but the operator deleted it during the push; leaving it deleted",
				"artist", a.Name, "type", imageType, "path", filePath,
				"peers", strings.Join(uploadedTo, ","), "push_started_at", push.at.Format(time.RFC3339Nano))
			return
		}
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
	// #2712: ONE wall-clock instant for the WHOLE set, stamped as the first
	// statement of the function.
	//
	// ONE instant, not one per file, because the snapshot is a single
	// observation of this directory and the repair's delete gate asks "did the
	// operator delete after I looked?" -- a per-file time would make the answer
	// depend on where in the read order a given backdrop happened to fall, so a
	// delete landing mid-snapshot would be honored for the files read before it
	// and ignored for the ones read after.
	//
	// FUNCTION ENTRY specifically, not merely "before the first read". The whole
	// prologue below -- GetPlatformIDs, ImageDir, getActiveFanartPrimary,
	// DiscoverFanart (a directory walk), fanartIdentityIndex -- is time during
	// which this push is unambiguously already in flight, so a delete landing
	// there IS concurrent with it. Stamping after any of those steps makes the
	// gate read such a delete as an old unrelated one and restore the artwork,
	// which is #2712 unfixed for the early part of every push. There is no
	// defensible line later than entry: every candidate is some prologue step's
	// end, and the operator can delete during that step.
	//
	// Erring early is the only safe direction and costs nothing: an earlier
	// stamp can only make the gate MORE willing to treat a delete as
	// concurrent, and a suppressed repair leaves a file the operator can re-add,
	// while a resurrected delete is not automatically recoverable at all.
	//
	// push.dir is filled in below, once ImageDir has resolved it. It is threaded
	// rather than re-derived inside the repair; see pushScope.
	push := pushScope{at: time.Now()}

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
	// The SAME resolved directory the delete handlers key their marker on. The
	// repair must not re-derive it from a discovered backdrop path; see
	// pushScope.
	//
	// On THIS path the two happen to coincide today: DiscoverFanart lists dir's
	// own entries and skips subdirectories, so every backdrop it returns is an
	// immediate child. That is a property of the discovery function, not of the
	// repair, and the repair is shared with the single-image path where it does
	// NOT hold. Threading dir from the one place that resolved it is what makes
	// the reader's key equal to the writer's by construction rather than by
	// coincidence in each caller.
	push.dir = dir

	primary := p.getActiveFanartPrimary(ctx)
	fanartPaths, discoverErr := img.DiscoverFanart(ctx, dir, primary)
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

	snapshot, snapWarnings, snapCancelErr := p.snapshotFanart(ctx, fanartPaths)
	warnings = append(warnings, snapWarnings...)
	if snapCancelErr != nil {
		// STOP BEFORE THE UPLOAD LOOP. Continuing here is what turned an
		// abandoned request into a partial push: the files read before the
		// cancellation would still have been handed to every peer.
		//
		// Returning here does NOT cost the deferred re-assertion anything, and
		// that is worth stating because losing it would be a data-loss bug
		// strictly worse than this one. The repair is registered BELOW this
		// point and is gated on uploadedTo being non-empty -- with the upload
		// loop never entered, no peer was reached, nothing external touched
		// these files, and the only thing a repair could do is revert a
		// concurrent operator write. That is the same guard the primary path
		// applies. The detached-context repair still runs, exactly as intended,
		// for a cancellation that lands AFTER a peer has been handed a file.
		// Name the cause HERE too, not just inside snapshotFanart (#2976
		// review). Fixing the misattribution in the callee and leaving the
		// caller asserting "the request ended" reproduces the same wrong-cause
		// message one level up -- and THIS is the string that reaches the
		// operator, since it lands in the returned warnings.
		if errors.Is(snapCancelErr, img.ErrTooManyStalledReads) {
			p.logger.Error("platform fanart sync stopped before upload: the library mount is not responding",
				slog.String("artist_id", a.ID),
				slog.Any("error", snapCancelErr))
			warnings = append(warnings,
				"platform sync stopped: the library mount is not responding, so the fanart could not be read")
			return warnings
		}
		p.logger.Warn("platform fanart sync canceled before upload",
			slog.String("artist_id", a.ID),
			slog.Any("error", snapCancelErr))
		warnings = append(warnings, "platform sync canceled: the request ended before all fanart could be read")
		return warnings
	}
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
	// The caller's context is passed but deliberately NOT honored as a
	// cancellation: reassertLocalImage detaches it (context.WithoutCancel) and
	// substitutes its own short deadline, so the repair still runs when the push
	// itself was canceled or timed out -- precisely when a peer may have destroyed
	// a file and nothing else will put it back -- while a dead mount can no longer
	// pin this deferred read forever. ctx is still threaded through for its
	// VALUES; see reassertLocalImage for the full reasoning.
	//
	// #2712: the pass runs TWICE, with a short settle in between, because the
	// ~15ms-late peer delete that motivates that was measured on THIS path. See
	// repairAfterPush. The whole set is walked on each pass, not just files the
	// first pass touched: the late delete can land on any slot, including one
	// the first pass had just confirmed healthy.
	//
	// #3017 C2 (hostile review, round 1): an earlier version of this branch
	// tried to drop an over-cap slot's bytes to nil after this repair had run,
	// on the theory that the retention cap should bound how long this push
	// holds them. Two rounds of hostile review found that mechanism BOTH
	// data-lossy in one ordering (nilling before this closure ran meant a
	// peer clobber of an over-cap file had nothing to restore from) AND
	// in-effect dead code in the corrected ordering (nilling a local slice
	// microseconds before this function returns frees nothing that was not
	// already about to be garbage collected -- see snapshotFanart's doc
	// comment for the full account). It has been removed. The over-cap
	// bytes simply live as long as this function's other locals do, which is
	// exactly as long as repairAfterPush needs them.
	defer func() {
		if len(uploadedTo) == 0 {
			return
		}
		p.repairAfterPush(func(scope repairScope) {
			for _, sf := range snapshot {
				if sf.data == nil {
					continue // never captured; there is nothing to put back
				}
				p.reassertLocalImage(ctx, a, "fanart", sf.path, sf.data, sf.mod, push, uploadedTo, scope)
			}
		})
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
		// above -- a walk of the WHOLE snapshot after every peer, run twice with
		// the #2712 settle window in between -- restores it. It is deliberately
		// NOT per-file-after-its-own-upload: the peer was measured deleting a
		// file this loop had not reached yet.
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

// newFanartReplaceClient constructs a fanartReplaceClient (#3125 F3) for the
// given connection type. Returns nil for unsupported types. emby.Client and
// jellyfin.Client already implement every method the interface needs (they
// back newImageUploader, newIndexedImageUploader, and phashPlatformClient
// alike), so this is a distinct constructor only for the injectable-seam
// pattern used throughout this file -- tests substitute a fake that can
// simulate a specific backdrop layout without standing up a peer.
var newFanartReplaceClient = func(conn *connection.Connection, logger *slog.Logger) fanartReplaceClient {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)
	default:
		return nil
	}
}

// newFanartResyncClient constructs a fanartResyncClient (#3135) for the given
// connection type. Returns nil for unsupported types. Same injectable-seam
// pattern and same underlying emby.Client/jellyfin.Client as
// newFanartReplaceClient; kept as its own var (rather than reusing that one's
// return value) so a test can substitute a resync-specific fake without also
// having to satisfy fanartReplaceClient's exact method set.
var newFanartResyncClient = func(conn *connection.Connection, logger *slog.Logger) fanartResyncClient {
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
