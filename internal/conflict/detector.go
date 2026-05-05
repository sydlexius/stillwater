package conflict

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/internal/event"
)

// defaultTTL is how long a refreshed ledger remains authoritative before the
// detector re-queries every connection. 5 minutes balances "user sees their
// remediation reflected quickly" against "we don't hammer every peer on every
// write." Callers that need an up-to-the-second answer should call Refresh
// explicitly (e.g. after toggling the "Let Stillwater manage" switch).
const defaultTTL = 5 * time.Minute

// peerClient is the subset of each platform client that the detector needs.
// Captured as an interface so tests can substitute a fake without standing
// up an HTTP server.
type peerClient interface {
	CheckNFOWriterEnabled(ctx context.Context) (enabled bool, libName string, err error)
	CheckImageSaverEnabled(ctx context.Context) (enabled bool, libName string, err error)
	// DisableFileWriteBack is invoked by the auto re-disable logic: when a
	// managed connection reports a peer-side writeback flag going from
	// off to on (e.g. an admin re-enabled SaveLocalMetadata in Emby's
	// UI), the detector calls this to turn it back off, honoring the
	// "Stillwater watches this server and turns off its artwork and NFO
	// savers whenever they get re-enabled" promise in the settings copy.
	DisableFileWriteBack(ctx context.Context) error
}

// pathProvider exposes the music-library filesystem paths for a connection.
// Jellyfin and Emby implement this; Lidarr returns an empty slice because
// its per-library path model differs (artists carry their own path, not
// libraries).
type pathProvider interface {
	MusicLibraryPaths(ctx context.Context) ([]string, error)
}

// ConnectionRepo is the minimum read surface the detector needs from the
// connection service. Extracted to an interface so the detector can be unit
// tested with an in-memory list.
type ConnectionRepo interface {
	List(ctx context.Context) ([]connection.Connection, error)
}

// Detector aggregates per-connection conflict state and caches the result.
// It is safe for concurrent callers: all state goes through a mutex.
type Detector struct {
	repo     ConnectionRepo
	bus      *event.Bus
	logger   *slog.Logger
	ttl      time.Duration
	clientFn func(connection.Connection) (peerClient, pathProvider)

	mu           sync.RWMutex
	cachedLedger Ledger
	cachedAt     time.Time
	// lastSignature is a content hash of the most recently published
	// ledger. We debounce ConflictChanged events on signature equality
	// rather than banner-state equality so the UI still refreshes when
	// the list of offenders changes but the banner classification stays
	// the same (e.g. Emby flipped to managed while Jellyfin remains a
	// conflict: state stays "image_only" but the row for Emby must
	// disappear).
	lastSignature string
	// refreshMu serializes the expensive peer fan-out in Refresh so a
	// burst of Current() callers that all observe a stale cache at the
	// same instant only trigger one network sweep. The first holder does
	// the work; subsequent holders see a fresh cache on re-check and
	// return without re-querying peers. See Current() for the pattern.
	refreshMu sync.Mutex

	// onBeforeRefreshLock, if loaded non-nil, is invoked from Current() on
	// the slow path immediately before refreshMu.Lock(). Test-only hook
	// used by the cache-stampede coalescing tests to deterministically
	// wait until every concurrent caller has reached the contention
	// point; production callers leave it nil. Stored as atomic.Pointer so
	// hot-path readers in Current() incur no mutex contention and tests
	// can install or clear the hook without racing concurrent callers.
	onBeforeRefreshLock atomic.Pointer[func()]
}

// NewDetector returns a detector wired to the live connection service and
// the real platform clients.
func NewDetector(repo ConnectionRepo, bus *event.Bus, logger *slog.Logger) *Detector {
	return &Detector{
		repo:     repo,
		bus:      bus,
		logger:   logger,
		ttl:      defaultTTL,
		clientFn: realClientFactory(logger),
	}
}

// newDetectorWithClients is used by tests to inject a custom client factory.
// Kept package-private so the only public construction path is NewDetector.
// TTL is seeded long by default; tests that exercise the cache boundary
// reach in and overwrite d.ttl directly (see TestDetectorCacheTTL) rather
// than plumbing a parameter every call site would set the same way.
func newDetectorWithClients(repo ConnectionRepo, bus *event.Bus, logger *slog.Logger, factory func(connection.Connection) (peerClient, pathProvider)) *Detector {
	return &Detector{
		repo:     repo,
		bus:      bus,
		logger:   logger,
		ttl:      time.Hour,
		clientFn: factory,
	}
}

// NewBlockingForTest constructs a detector that reports a fixed blocking
// ledger on every call, so tests can verify 409 short-circuit behavior in
// write handlers without a live peer. The returned detector's cache is
// pre-populated with a single Enabled connection whose image and NFO
// writeback flags are both on, forcing both Gate axes to block.
func NewBlockingForTest(logger *slog.Logger) *Detector {
	d := newDetectorWithClients(
		&staticRepo{conns: []connection.Connection{{ID: "blocking", Name: "blocking", Type: connection.TypeEmby, Enabled: true}}},
		nil, logger,
		func(c connection.Connection) (peerClient, pathProvider) { return &blockingClient{}, nil },
	)
	d.Refresh(context.Background())
	return d
}

type staticRepo struct{ conns []connection.Connection }

func (s *staticRepo) List(context.Context) ([]connection.Connection, error) {
	return s.conns, nil
}

type blockingClient struct{}

func (blockingClient) CheckNFOWriterEnabled(context.Context) (bool, string, error) {
	return true, "blocking-lib", nil
}
func (blockingClient) CheckImageSaverEnabled(context.Context) (bool, string, error) {
	return true, "blocking-lib", nil
}
func (blockingClient) DisableFileWriteBack(context.Context) error { return nil }

// NewForTest constructs a detector that never contacts any peer client,
// always returning clean state. Exported so other-package tests can build a
// Detector without standing up an HTTP fake for every connection type.
// The repo parameter drives which connections appear in the ledger; pass
// a stub implementation that returns the fixture connections you want.
func NewForTest(repo ConnectionRepo, logger *slog.Logger) *Detector {
	return newDetectorWithClients(repo, nil, logger, func(c connection.Connection) (peerClient, pathProvider) {
		return &noopClient{}, nil
	})
}

// noopClient implements peerClient with always-false answers. Used by
// NewForTest so the ledger reports clean state without needing a network
// fake per connection type.
type noopClient struct{}

func (noopClient) CheckNFOWriterEnabled(context.Context) (bool, string, error) {
	return false, "", nil
}
func (noopClient) CheckImageSaverEnabled(context.Context) (bool, string, error) {
	return false, "", nil
}
func (noopClient) DisableFileWriteBack(context.Context) error { return nil }

// Current returns the most recent ledger. If the cache is stale (or empty)
// it triggers a synchronous Refresh. Callers in hot paths (write-handler
// gates) should use this; callers doing bulk background work can call
// Refresh directly to force a fetch.
//
// Cache-stampede guard: when several goroutines see the cache stale at the
// same moment, only one holds refreshMu and runs Refresh. The rest wait on
// the mutex, then re-check freshness and return the just-populated cache
// without re-querying peers. Without this, N concurrent write handlers
// after a TTL boundary would each fan out HTTP calls to every peer.
func (d *Detector) Current(ctx context.Context) Ledger {
	d.mu.RLock()
	ledger := d.cachedLedger
	fresh := !d.cachedAt.IsZero() && time.Since(d.cachedAt) < d.ttl
	d.mu.RUnlock()
	if fresh {
		return ledger
	}
	if hook := d.onBeforeRefreshLock.Load(); hook != nil {
		(*hook)()
	}
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	// Recheck under the refresh lock: a goroutine that raced with us may
	// have already completed the refresh while we were waiting.
	d.mu.RLock()
	ledger = d.cachedLedger
	fresh = !d.cachedAt.IsZero() && time.Since(d.cachedAt) < d.ttl
	d.mu.RUnlock()
	if fresh {
		return ledger
	}
	return d.Refresh(ctx)
}

// Refresh rebuilds the ledger from scratch by querying every enabled
// connection. Returns the new ledger. Emits event.ConflictChanged on the
// bus if the banner-state transitioned since the previous refresh.
func (d *Detector) Refresh(ctx context.Context) Ledger {
	conns, err := d.repo.List(ctx)
	if err != nil {
		d.logger.Warn("listing connections for conflict refresh failed", "error", err)
		d.mu.RLock()
		prior := d.cachedLedger
		firstRun := d.cachedAt.IsZero()
		d.mu.RUnlock()
		if !firstRun {
			// Stale ledger is preferable to an all-clean ledger that
			// would silently drop the gate.
			return prior
		}
		// First-ever refresh failed: we have no prior knowledge, so
		// synthesize a fail-closed sentinel. AnyImageConflict and
		// AnyNFOConflict treat a non-empty CheckErr as a conflict
		// (gate closed), which is the conservative default until a
		// later refresh succeeds. CheckErr is client-visible (rendered
		// in the conflict detail panel), so the message stays generic;
		// the full error text was already logged above.
		sentinel := Ledger{
			GeneratedAt: time.Now().UTC(),
			Connections: []ConnectionState{{
				ConnectionID:   "__unavailable__",
				ConnectionName: "connection list",
				Enabled:        true,
				CheckErr:       "connection list unavailable",
				CheckedAt:      time.Now().UTC(),
			}},
		}
		return sentinel
	}

	states := make([]ConnectionState, 0, len(conns))
	for _, c := range conns {
		states = append(states, d.checkOne(ctx, c))
	}
	ledger := Ledger{
		GeneratedAt: time.Now().UTC(),
		Connections: states,
		RoundTrips:  detectRoundTrips(states),
	}

	signature := ledgerSignature(ledger)
	d.mu.Lock()
	prevSignature := d.lastSignature
	d.cachedLedger = ledger
	d.cachedAt = ledger.GeneratedAt
	d.lastSignature = signature
	d.mu.Unlock()

	// Only emit a transition event after the first refresh completes --
	// the empty -> something transition on startup is noise -- AND when
	// the ledger content actually differs. Debouncing on banner-state
	// alone was too coarse (flipping one of several conflicting peers to
	// managed keeps the banner at e.g. "image_only" even though the row
	// list changed). Signature includes state + per-connection flags +
	// round-trips so any UI-visible delta triggers a refresh.
	if d.bus != nil && prevSignature != "" && prevSignature != signature {
		d.bus.Publish(event.Event{
			Type: event.ConflictChanged,
			Data: map[string]any{
				"banner_state": ledger.BannerState(),
				"image_gated":  ledger.AnyImageConflict(),
				"nfo_gated":    ledger.AnyNFOConflict(),
				"round_trip":   len(ledger.RoundTrips) > 0,
			},
		})
	}
	return ledger
}

// ledgerSignature returns a stable string that changes whenever the ledger
// content meaningfully changes for UI purposes. It deliberately ignores
// GeneratedAt (which changes every refresh) and CheckedAt (per-state
// timestamp). Order within slices matters, which is fine because the
// detector iterates connections in deterministic repo order.
func ledgerSignature(l Ledger) string {
	var b strings.Builder
	b.WriteString(l.BannerState())
	for _, c := range l.Connections {
		b.WriteByte('|')
		b.WriteString(c.ConnectionID)
		b.WriteByte(':')
		if c.Enabled {
			b.WriteByte('E')
		}
		if c.ManageServerFiles {
			b.WriteByte('M')
		}
		if c.NFOWriteback {
			b.WriteByte('N')
		}
		if c.ImageWriteback {
			b.WriteByte('I')
		}
		b.WriteByte(':')
		b.WriteString(c.LibraryName)
		b.WriteByte(':')
		if c.CheckErr != "" {
			b.WriteByte('X')
		}
	}
	for _, rt := range l.RoundTrips {
		b.WriteByte('|')
		b.WriteString("rt:")
		b.WriteString(rt.ConnectionAID)
		b.WriteByte('-')
		b.WriteString(rt.ConnectionBID)
		b.WriteByte('@')
		b.WriteString(rt.OverlappingPath)
	}
	return b.String()
}

// Invalidate clears the cache so the next Current call forces a refresh.
// Used after mutations that could change conflict state (connection test,
// save, delete; toggle flips).
func (d *Detector) Invalidate() {
	d.mu.Lock()
	d.cachedAt = time.Time{}
	d.mu.Unlock()
}

func (d *Detector) checkOne(ctx context.Context, c connection.Connection) ConnectionState {
	state := ConnectionState{
		ConnectionID:      c.ID,
		ConnectionName:    c.Name,
		ConnectionType:    c.Type,
		Enabled:           c.Enabled,
		ManageServerFiles: c.FeatureManageServerFiles,
		CheckedAt:         time.Now().UTC(),
	}
	if !c.Enabled {
		// Skip the remote call for disabled connections but still surface the
		// row so the UI can render a grey "not checked" pill.
		return state
	}

	client, paths := d.clientFn(c)
	if client == nil {
		state.CheckErr = "unsupported connection type for conflict detection"
		return state
	}

	// CheckErr flows to GET /conflicts JSON and the per-connection detail
	// panel HTML, so it must stay free of internal/peer error text (URLs,
	// HTTP snippets, file paths). Log the full error server-side and store
	// only a generic, user-facing reason in state.CheckErr. AnyImageConflict
	// and AnyNFOConflict treat any non-empty CheckErr as a conflict, so the
	// gate fails closed regardless of the message text.
	if nfo, lib, err := client.CheckNFOWriterEnabled(ctx); err != nil {
		d.logger.Warn("nfo writer check failed", "connection", c.Name, "error", err)
		state.CheckErr = "nfo check unavailable"
	} else {
		state.NFOWriteback = nfo
		if nfo && state.LibraryName == "" {
			state.LibraryName = lib
		}
	}

	if img, lib, err := client.CheckImageSaverEnabled(ctx); err != nil {
		d.logger.Warn("image saver check failed", "connection", c.Name, "error", err)
		if state.CheckErr == "" {
			state.CheckErr = "image check unavailable"
		} else {
			state.CheckErr += "; image check unavailable"
		}
	} else {
		state.ImageWriteback = img
		if img && state.LibraryName == "" {
			state.LibraryName = lib
		}
	}

	if paths != nil {
		libPaths, err := paths.MusicLibraryPaths(ctx)
		if err != nil {
			// A path fetch failure means we cannot detect round-trip
			// overlap for this connection, which would otherwise hide a
			// real shared-directory conflict. Surface via CheckErr so
			// AnyImageConflict/AnyNFOConflict fail closed on this row
			// rather than silently treating it as non-overlapping. Log
			// the full error server-side; CheckErr is client-visible so
			// it stays generic.
			d.logger.Warn("fetching library paths failed", "connection", c.Name, "error", err)
			if state.CheckErr == "" {
				state.CheckErr = "paths check unavailable"
			} else {
				state.CheckErr += "; paths check unavailable"
			}
		} else {
			state.Paths = normalizePaths(libPaths)
		}
	}

	// Auto re-disable: when a connection is in managed mode but the peer
	// is still reporting a saver as on, push DisableFileWriteBack again.
	// This realizes the promise in the settings copy ("Stillwater watches
	// this server and turns off its artwork and NFO savers whenever they
	// get re-enabled") -- someone enabling SaveLocalMetadata in Emby's
	// admin UI while the toggle is on gets quietly rolled back on the
	// next conflict refresh. We then re-query the peer so the ledger and
	// banner reflect the corrected state rather than the pre-disable
	// read, and the user never sees a spurious amber conflict chip.
	if state.ManageServerFiles && (state.ImageWriteback || state.NFOWriteback) {
		d.logger.Info("re-disabling peer savers after managed connection drift",
			"connection", c.Name,
			"image_writeback", state.ImageWriteback,
			"nfo_writeback", state.NFOWriteback,
		)
		if err := client.DisableFileWriteBack(ctx); err != nil {
			// Critical: a failed auto re-disable must NOT leave the row
			// in a "managed and clean" state. AnyImageConflict and
			// AnyNFOConflict skip rows where ManageServerFiles=true,
			// so if we kept the managed flag here the gate would
			// silently reopen even though the peer saver is still on.
			// Flip ManageServerFiles=false in the in-memory state (the
			// DB column stays unchanged so the user's intent persists)
			// and surface CheckErr so the gate stays closed. The DB
			// flag will be re-honored on the next refresh once the
			// peer is reachable again and the disable succeeds.
			d.logger.Warn("auto re-disable failed", "connection", c.Name, "error", err)
			state.ManageServerFiles = false
			if state.CheckErr == "" {
				state.CheckErr = "managed re-disable unavailable"
			} else {
				state.CheckErr += "; managed re-disable unavailable"
			}
		} else {
			// Re-query so the ledger reflects the corrected state. If
			// either recheck errors we surface it via CheckErr rather
			// than keeping the pre-disable flag -- silently retaining
			// the stale value would make the banner report "still
			// conflicted" when the peer actually just went offline.
			// CheckErr is client-visible, so log full error server-side
			// and store a generic phrase here.
			if nfo, _, nfoErr := client.CheckNFOWriterEnabled(ctx); nfoErr == nil {
				state.NFOWriteback = nfo
			} else {
				d.logger.Warn("post-disable nfo recheck failed", "connection", c.Name, "error", nfoErr)
				if state.CheckErr == "" {
					state.CheckErr = "post-disable nfo recheck unavailable"
				} else {
					state.CheckErr += "; post-disable nfo recheck unavailable"
				}
			}
			if img, _, imgErr := client.CheckImageSaverEnabled(ctx); imgErr == nil {
				state.ImageWriteback = img
			} else {
				d.logger.Warn("post-disable image recheck failed", "connection", c.Name, "error", imgErr)
				if state.CheckErr == "" {
					state.CheckErr = "post-disable image recheck unavailable"
				} else {
					state.CheckErr += "; post-disable image recheck unavailable"
				}
			}
		}
	}

	d.logger.Debug("conflict detector state",
		"connection", c.Name,
		"type", c.Type,
		"managed", state.ManageServerFiles,
		"nfo_writeback", state.NFOWriteback,
		"image_writeback", state.ImageWriteback,
		"library", state.LibraryName,
		"paths", state.Paths,
		"check_err", state.CheckErr,
	)
	return state
}

// detectRoundTrips finds pairs of enabled connections whose music library
// filesystem paths overlap (one path is an ancestor of the other, or they
// are equal). Overlapping paths mean a Stillwater write reaches both peers
// via the shared filesystem, so even one enabled saver contaminates all of
// them. Managed connections are excluded because their savers are off.
// Every overlapping path is surfaced -- users with multi-path libraries
// (e.g. an Emby VirtualFolder that covers both /music and /classical)
// want to see both paths in the banner, not just the first one found.
func detectRoundTrips(states []ConnectionState) []RoundTrip {
	var out []RoundTrip
	for i := range states {
		a := states[i]
		if !a.Enabled || a.ManageServerFiles || len(a.Paths) == 0 {
			continue
		}
		for j := i + 1; j < len(states); j++ {
			b := states[j]
			if !b.Enabled || b.ManageServerFiles || len(b.Paths) == 0 {
				continue
			}
			for _, overlap := range allOverlaps(a.Paths, b.Paths) {
				out = append(out, RoundTrip{
					ConnectionAID:   a.ConnectionID,
					ConnectionAName: a.ConnectionName,
					ConnectionBID:   b.ConnectionID,
					ConnectionBName: b.ConnectionName,
					OverlappingPath: overlap,
				})
			}
		}
	}
	return out
}

// allOverlaps returns every distinct shared (equal or ancestor) path
// between the two path lists. Deduplicated so a three-way ancestor does
// not emit the same bullet twice.
func allOverlaps(a, b []string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, pa := range a {
		for _, pb := range b {
			switch {
			case pa == pb:
				add(pa)
			case isAncestor(pa, pb):
				add(pa)
			case isAncestor(pb, pa):
				add(pb)
			}
		}
	}
	return out
}

// isAncestor returns true if parent is a strict ancestor of child on the
// filesystem, e.g. "/music" is an ancestor of "/music/rock". Equal paths
// return false so callers handle the equality case explicitly (semantically
// "they share this exact path" vs "parent contains child").
func isAncestor(parent, child string) bool {
	if parent == "" || child == "" || parent == child {
		return false
	}
	// Normalize trailing slashes so "/music" ancestors "/music/rock" but not
	// "/musicals".
	p := strings.TrimRight(parent, string(filepath.Separator))
	c := strings.TrimRight(child, string(filepath.Separator))
	return strings.HasPrefix(c, p+string(filepath.Separator))
}

// normalizePaths cleans each path (collapses ".." and duplicate separators),
// drops empties, and deduplicates. We intentionally do not EvalSymlinks here
// because peer-reported paths may reference mounts that only exist on the
// peer; a best-effort string comparison is the right signal.
func normalizePaths(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		clean := filepath.Clean(p)
		if _, dup := seen[clean]; dup {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

// realClientFactory returns a function that constructs the appropriate peer
// client + path provider pair for a given connection. Lidarr returns nil for
// the path provider because its library path model does not map cleanly.
func realClientFactory(logger *slog.Logger) func(connection.Connection) (peerClient, pathProvider) {
	return func(c connection.Connection) (peerClient, pathProvider) {
		switch c.Type {
		case connection.TypeEmby:
			client := emby.New(c.URL, c.APIKey, c.PlatformUserID, logger)
			return client, embyPaths{client: client}
		case connection.TypeJellyfin:
			client := jellyfin.New(c.URL, c.APIKey, c.PlatformUserID, logger)
			return client, jellyfinPaths{client: client}
		case connection.TypeLidarr:
			client := lidarr.New(c.URL, c.APIKey, logger)
			return client, nil
		default:
			return nil, nil
		}
	}
}

type embyPaths struct{ client *emby.Client }

func (e embyPaths) MusicLibraryPaths(ctx context.Context) ([]string, error) {
	libs, err := e.client.GetMusicLibraries(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, lib := range libs {
		out = append(out, lib.Locations...)
	}
	return out, nil
}

type jellyfinPaths struct{ client *jellyfin.Client }

func (j jellyfinPaths) MusicLibraryPaths(ctx context.Context) ([]string, error) {
	libs, err := j.client.GetMusicLibraries(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, lib := range libs {
		out = append(out, lib.Locations...)
	}
	return out, nil
}
