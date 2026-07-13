package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
)

// lidarrArtistLister is the narrow read surface path-mapping inference needs
// from a Lidarr connection: enumerate its artists so each can be matched to a
// Stillwater artist by MusicBrainz ID. Implemented by *lidarr.Client; declared
// locally so the factory can be swapped in tests without a real HTTP fixture.
// This mirrors the self-heal seam of the same name in internal/publish; that
// one is unexported and cannot cross the package boundary, so inference keeps
// its own copy of the pattern rather than importing publish for a lister.
type lidarrArtistLister interface {
	GetArtists(ctx context.Context) ([]lidarr.Artist, error)
}

// lidarrArtistListerFactory builds a lidarrArtistLister for a Lidarr connection.
// The production factory returns a real *lidarr.Client; tests override it
// (t.Cleanup-restored) to inject a fake. Declared as a package-level var (not
// const) purely so tests can swap it.
var lidarrArtistListerFactory = func(conn *connection.Connection, logger *slog.Logger) lidarrArtistLister {
	return lidarr.New(conn.URL, conn.APIKey, logger)
}

// platformArtistPath is one artist as a PEER sees it: its MusicBrainz ID (the
// join key against Stillwater's own artists) and its path in the peer's
// filesystem namespace. The per-platform listers below normalize Lidarr's
// ArtistResource and Emby/Jellyfin's ArtistItem into this one shape so
// inference is written once rather than three times.
type platformArtistPath struct {
	MBID string
	Path string
}

// mediaArtistLister is the read surface inference needs from an Emby or
// Jellyfin connection: enumerate the music libraries, then page their artists.
// Both clients satisfy it structurally; declared locally so tests can inject a
// fake without an HTTP fixture (same seam pattern as lidarrArtistLister).
type mediaArtistLister interface {
	ListArtistPaths(ctx context.Context) ([]platformArtistPath, error)
}

// mediaArtistPageSize bounds each Emby/Jellyfin artist page. Matches the page
// size the library scanners already use against the same endpoint.
const mediaArtistPageSize = 200

// mediaArtistPageCap bounds the number of pages a single ListArtistPaths
// enumeration will walk. Same concept as jellyfin.listArtistsPageCap: a peer
// that misreports TotalRecordCount (or returns a full page forever) would
// otherwise spin here indefinitely. 200 pages x 200 = 40k artists, far past
// any real library, so the cap can only be hit by a misbehaving peer.
const mediaArtistPageCap = 200

// mediaArtistListerFactory builds a mediaArtistLister for an Emby or Jellyfin
// connection. Overridable by tests (t.Cleanup-restored). Returns nil for a type
// with no media-server artist surface.
var mediaArtistListerFactory = func(conn *connection.Connection, logger *slog.Logger) mediaArtistLister {
	switch conn.Type {
	case connection.TypeEmby:
		return embyArtistLister{c: emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger), logger: logger}
	case connection.TypeJellyfin:
		return jellyfinArtistLister{c: jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger), logger: logger}
	default:
		return nil
	}
}

type embyArtistLister struct {
	c      *emby.Client
	logger *slog.Logger
}

func (l embyArtistLister) ListArtistPaths(ctx context.Context) ([]platformArtistPath, error) {
	libs, err := l.c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing emby music libraries: %w", err)
	}
	var out []platformArtistPath
	// Index-based iteration: VirtualFolder and ArtistItem are large enough that a
	// per-iteration value copy trips gocritic's rangeValCopy.
	for i := range libs {
		page := 0
		for start := 0; ; start += mediaArtistPageSize {
			if page >= mediaArtistPageCap {
				l.logger.Error("emby artist enumeration truncated: page cap reached",
					"library_id", libs[i].ItemID, "page_cap", mediaArtistPageCap)
				break
			}
			page++
			resp, err := l.c.GetArtists(ctx, libs[i].ItemID, start, mediaArtistPageSize)
			if err != nil {
				return nil, fmt.Errorf("listing emby artists: %w", err)
			}
			if resp == nil || len(resp.Items) == 0 {
				break
			}
			for j := range resp.Items {
				it := &resp.Items[j]
				out = append(out, platformArtistPath{MBID: it.ProviderIDs.MusicBrainzArtist, Path: it.Path})
			}
			if start+len(resp.Items) >= resp.TotalRecordCount {
				break
			}
		}
	}
	return out, nil
}

type jellyfinArtistLister struct {
	c      *jellyfin.Client
	logger *slog.Logger
}

func (l jellyfinArtistLister) ListArtistPaths(ctx context.Context) ([]platformArtistPath, error) {
	libs, err := l.c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing jellyfin music libraries: %w", err)
	}
	var out []platformArtistPath
	// Index-based iteration: VirtualFolder and ArtistItem are large enough that a
	// per-iteration value copy trips gocritic's rangeValCopy.
	for i := range libs {
		page := 0
		for start := 0; ; start += mediaArtistPageSize {
			if page >= mediaArtistPageCap {
				l.logger.Error("jellyfin artist enumeration truncated: page cap reached",
					"library_id", libs[i].ItemID, "page_cap", mediaArtistPageCap)
				break
			}
			page++
			resp, err := l.c.GetArtists(ctx, libs[i].ItemID, start, mediaArtistPageSize)
			if err != nil {
				return nil, fmt.Errorf("listing jellyfin artists: %w", err)
			}
			if resp == nil || len(resp.Items) == 0 {
				break
			}
			for j := range resp.Items {
				it := &resp.Items[j]
				out = append(out, platformArtistPath{MBID: it.ProviderIDs.MusicBrainzArtist, Path: it.Path})
			}
			if start+len(resp.Items) >= resp.TotalRecordCount {
				break
			}
		}
	}
	return out, nil
}

// listPlatformArtistPaths enumerates the peer's artists as (MBID, platform path)
// records, dispatching on connection type. Lidarr goes through the pre-existing
// lidarrArtistLister seam (kept so #2329's tests keep working); Emby and
// Jellyfin go through mediaArtistLister. Returns nil for an unknown type.
func (r *Router) listPlatformArtistPaths(ctx context.Context, conn *connection.Connection) ([]platformArtistPath, error) {
	switch conn.Type {
	case connection.TypeLidarr:
		lister := lidarrArtistListerFactory(conn, r.logger)
		if lister == nil {
			return nil, nil
		}
		artists, err := lister.GetArtists(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]platformArtistPath, 0, len(artists))
		for _, a := range artists {
			out = append(out, platformArtistPath{MBID: a.ForeignArtistID, Path: a.Path})
		}
		return out, nil
	case connection.TypeEmby, connection.TypeJellyfin:
		lister := mediaArtistListerFactory(conn, r.logger)
		if lister == nil {
			return nil, nil
		}
		return lister.ListArtistPaths(ctx)
	default:
		return nil, nil
	}
}

// inferPathMappings derives host->platform path mappings for ANY connection type
// by matching Stillwater artists to the peer's artists by MusicBrainz ID and
// feeding the resulting (host path, platform path) pairs to
// connection.InferPathMappings. It returns the derived mappings, the number of
// artists that matched by MBID (matched), and an error only when the inputs
// could not be gathered.
//
// Generalized off Lidarr in #2380: an Emby or Jellyfin container mounts the
// library under its own prefix exactly the way Lidarr does, so restricting
// inference (and therefore the whole mapped state) to Lidarr left the two media
// servers receiving raw host paths.
//
// BEST-EFFORT semantics: callers treat a non-nil error as "no inference
// available" and never fail the create/enable that triggered it. matched is the
// pair count fed to inference, useful for the "M matched artists" surface even
// when the consensus floor emits zero mappings.
func (r *Router) inferPathMappings(ctx context.Context, conn *connection.Connection) ([]connection.PathMapping, int, error) {
	if conn == nil {
		return nil, 0, nil
	}

	// Stillwater side: every artist with both an MBID and a host path.
	mbidPaths, err := r.artistService.ListMBIDPaths(ctx)
	if err != nil {
		return nil, 0, err
	}
	// NOTE: an empty mbidPaths is NOT an early return. Artist evidence is only ONE
	// of the two signals; root pairing below needs none of it, and a peer like Emby
	// (which reports no artist paths at all) would otherwise never be mapped.
	var pairs []connection.PathPair
	if len(mbidPaths) > 0 {
		// Peer side: enumerate the connection's artists via the per-type seam.
		artists, aerr := r.listPlatformArtistPaths(ctx, conn)
		if aerr != nil {
			return nil, 0, aerr
		}
		pairs = buildInferencePairs(mbidPaths, artists)
	}
	mappings := connection.InferPathMappings(pairs, connection.DefaultPathInferConsensus)

	// ROOT PAIRING fills what the artist evidence cannot reach. Two real cases,
	// both observed against live peers:
	//   - EMBY returns NO Path on artist items (verified against a live server:
	//     /Artists/AlbumArtists, /Items, and the single-item detail endpoint all
	//     return an empty Path, with and without Fields=Path). Emby therefore
	//     yields ZERO usable pairs no matter how many artists are linked, so
	//     pair-only inference left every Emby connection unmapped forever.
	//   - A library root with only one MBID-matched artist sits below the
	//     pair-consensus floor, so it stayed unmapped while its sibling root
	//     mapped fine - a half-mapped connection.
	// Every peer type DOES report its own roots (Lidarr root folders, Emby /
	// Jellyfin library Locations), and Stillwater knows its own library roots, so
	// the roots are the one signal that is always available. Evidence still WINS:
	// MergePathMappings only adds a root pairing for a host root the pair-derived
	// set does not already cover.
	roots, rerr := r.listPlatformRoots(ctx, conn)
	if rerr != nil {
		r.logger.Info("path-mapping inference: could not read platform roots",
			"connection_id", conn.ID, "type", conn.Type, "error", rerr)
	} else if rootMappings := connection.InferPathMappingsFromRoots(r.hostLibraryRoots(ctx), roots); len(rootMappings) > 0 {
		mappings = connection.MergePathMappings(mappings, rootMappings)
	}
	return mappings, len(pairs), nil
}

// platformRootLister is the read surface inference needs to ask a peer for its
// OWN roots: Lidarr's root folders, or an Emby/Jellyfin library's Locations. It
// mirrors publish's renameRootLister (whose unexported seam cannot cross the
// package boundary) so the guard and inference agree on what "the peer's roots"
// means.
type platformRootLister interface {
	ListRoots(ctx context.Context) ([]string, error)
}

// platformRootListerFactory builds a platformRootLister per connection type.
// Overridable by tests (t.Cleanup-restored), like the artist-lister factories.
// Returns nil for a type with no root surface.
var platformRootListerFactory = func(conn *connection.Connection, logger *slog.Logger) platformRootLister {
	switch conn.Type {
	case connection.TypeLidarr:
		return lidarrRoots{lidarr.New(conn.URL, conn.APIKey, logger)}
	case connection.TypeEmby:
		return embyRoots{emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)}
	case connection.TypeJellyfin:
		return jellyfinRoots{jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), logger)}
	default:
		return nil
	}
}

type lidarrRoots struct{ c *lidarr.Client }

func (l lidarrRoots) ListRoots(ctx context.Context) ([]string, error) {
	folders, err := l.c.GetRootFolders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(folders))
	for i := range folders {
		out = append(out, folders[i].Path)
	}
	return out, nil
}

type embyRoots struct{ c *emby.Client }

func (l embyRoots) ListRoots(ctx context.Context) ([]string, error) {
	libs, err := l.c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	// Index-based: VirtualFolder is large enough to trip gocritic's rangeValCopy.
	for i := range libs {
		out = append(out, libs[i].Locations...)
	}
	return out, nil
}

type jellyfinRoots struct{ c *jellyfin.Client }

func (l jellyfinRoots) ListRoots(ctx context.Context) ([]string, error) {
	libs, err := l.c.GetMusicLibraries(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range libs {
		out = append(out, libs[i].Locations...)
	}
	return out, nil
}

// listPlatformRoots asks the peer for its own roots. Returns (nil, nil) for a
// connection type with no root surface.
func (r *Router) listPlatformRoots(ctx context.Context, conn *connection.Connection) ([]string, error) {
	lister := platformRootListerFactory(conn, r.logger)
	if lister == nil {
		return nil, nil
	}
	return lister.ListRoots(ctx)
}

// hostLibraryRoots returns Stillwater's own library roots (the host side of a
// mapping). Empty when the library service is unavailable or errors - root
// pairing then simply contributes nothing.
func (r *Router) hostLibraryRoots(ctx context.Context) []string {
	if r.libraryService == nil {
		return nil
	}
	libs, err := r.libraryService.List(ctx)
	if err != nil {
		r.logger.Info("path-mapping inference: could not list library roots", "error", err)
		return nil
	}
	out := make([]string, 0, len(libs))
	for i := range libs {
		if p := strings.TrimSpace(libs[i].Path); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// buildInferencePairs joins Stillwater (MBID, host path) records against a
// Lidarr artist list by MusicBrainz ID and returns the observed (host path,
// platform path) pairs for inference. Pure and order-independent (its output
// does not depend on the order of either input slice).
//
// Duplicate-MBID handling is the key invariant (#2335): ListMBIDPaths can
// return the same MBID more than once (a data anomaly - MBIDs should be
// unique). If those rows carry DISTINCT host paths the MBID is AMBIGUOUS and is
// excluded from the join entirely, so no arbitrary "first seen" winner can make
// the result depend on row order. Repeated rows with the IDENTICAL host path
// are a benign true-duplicate and keep that single path. MBIDs are matched
// case-insensitively (trimmed + lowercased) on both sides.
func buildInferencePairs(mbidPaths []artist.MBIDPath, artists []platformArtistPath) []connection.PathPair {
	hostByMBID := make(map[string]string, len(mbidPaths))
	ambiguousMBID := make(map[string]bool)
	for _, mp := range mbidPaths {
		key := strings.ToLower(strings.TrimSpace(mp.MBID))
		if key == "" || mp.Path == "" {
			continue
		}
		if ambiguousMBID[key] {
			continue
		}
		if prev, ok := hostByMBID[key]; ok {
			if prev != mp.Path {
				// Conflicting host paths for one MBID: drop it so no pair forms,
				// deterministically, regardless of which row arrived first.
				delete(hostByMBID, key)
				ambiguousMBID[key] = true
			}
			continue
		}
		hostByMBID[key] = mp.Path
	}

	var pairs []connection.PathPair
	for _, la := range artists {
		key := strings.ToLower(strings.TrimSpace(la.MBID))
		if key == "" || la.Path == "" {
			continue
		}
		hostPath, ok := hostByMBID[key]
		if !ok {
			continue
		}
		pairs = append(pairs, connection.PathPair{HostPath: hostPath, PlatformPath: la.Path})
	}
	return pairs
}

// applyInferredPathMappingsIfEmpty runs inference for a Lidarr connection and,
// only when the connection currently has NO path mappings, persists the derived
// set. A non-empty mapping list (operator override or prior inference) is never
// overwritten, so the operation is monotonic and clobber-free (#2329 B3). It is
// best-effort: every failure is logged and swallowed so the caller's
// create/enable path is never broken. Called for its side effect (the persisted
// mappings); the connection's in-memory PathMappings are updated in place when a
// set is applied.
//
// CONCURRENCY: inference issues an up-to-10s Lidarr GetArtists round-trip. If
// the empty check and the write straddled that window unlocked, a manual
// POST /path-mappings landing mid-enumeration could save the operator's list
// and then be clobbered by this write -- defeating the "never overwrite an
// operator override" invariant. So the check-and-write is serialized on the
// SAME per-id pathMappingsMu the manual handlers use, and the canonical
// connection is RE-FETCHED and RE-CHECKED for emptiness inside the lock: a save
// that landed during the enumeration is seen and this method backs off. The
// expensive enumeration runs BEFORE the lock so a manual save is never blocked
// for the full 10s; only the fast re-check-and-write is serialized.
func (r *Router) applyInferredPathMappingsIfEmpty(ctx context.Context, conn *connection.Connection) {
	// Every connection type since #2380: Emby and Jellyfin need the same
	// host->container translation Lidarr does, and gating inference on Lidarr
	// was one half of why they never reached a mapped state at all.
	if conn == nil || !conn.Enabled {
		return
	}
	// Fast pre-check on the in-memory snapshot: skip the enumeration entirely
	// when the connection already carries mappings. The authoritative empty
	// check is re-done under the lock below against the canonical row.
	if len(conn.GetPathMappings()) != 0 {
		return
	}

	mappings, matched, err := r.inferPathMappings(ctx, conn)
	if err != nil {
		r.logger.Warn("path-mapping inference FAILED",
			"connection_id", conn.ID, "name", conn.Name, "type", conn.Type, "error", err)
		return
	}
	// A connection that infers NOTHING must SAY it inferred nothing. Returning
	// quietly here is the same silent-failure archetype as the bug this whole
	// change exists to kill: the operator sees path_mappings=null, no log line,
	// and no way to tell "inference ran and found nothing" from "inference never
	// ran" - which is exactly how a peer that infers zero went unnoticed.
	if len(mappings) == 0 {
		r.logger.Warn("path-mapping inference produced NO mappings: this connection stays unmapped, "+
			"so any rename/merge push for a split-mount library will be refused by the root guard. "+
			"Enter a path mapping manually (Settings > Connections > Path Mappings) if this peer "+
			"addresses the library under a different prefix",
			"connection_id", conn.ID,
			"name", conn.Name,
			"type", conn.Type,
			"matched_artists", matched)
		return
	}

	// Serialize the read-check-write with the manual handlers so a save that
	// committed during the enumeration is observed here and not clobbered.
	muIface, _ := r.pathMappingsMu.LoadOrStore(conn.ID, &sync.Mutex{})
	connMu := muIface.(*sync.Mutex)
	connMu.Lock()
	defer connMu.Unlock()

	fresh, ferr := r.connectionService.GetByID(ctx, conn.ID)
	if ferr != nil {
		r.logger.Info("path-mapping inference skipped: reloading connection", "connection_id", conn.ID, "error", ferr)
		return
	}
	// A mapping list that appeared during the enumeration (manual save, or a
	// concurrent auto-apply) wins: back off rather than overwrite it.
	if len(fresh.GetPathMappings()) != 0 {
		r.logger.Info("path-mapping inference backed off: mappings set during enumeration",
			"connection_id", conn.ID)
		return
	}

	setPathMappings(fresh, mappings)
	if updErr := r.connectionService.Update(ctx, fresh); updErr != nil {
		r.logger.Error("persisting inferred path mappings", "connection_id", conn.ID, "error", updErr)
		return
	}
	// Reflect the applied set on the caller's in-memory connection so a JSON
	// create/update response shows the mappings that were just persisted.
	setPathMappings(conn, mappings)
	// Name the ACTUAL peer type. This line used to say "Lidarr" for every
	// connection, so an operator debugging Emby read "applied inferred Lidarr path
	// mappings" against an Emby connection id.
	r.logger.Info("applied inferred path mappings",
		"connection_id", conn.ID,
		"name", conn.Name,
		"type", conn.Type,
		"mappings", len(mappings),
		"matched_artists", matched)

	// HALF-MAPPED IS NOT MAPPED. A real deployment has SEVERAL library roots, and
	// inference emits one mapping per platform root it has evidence for - so a
	// connection can come out with some roots translated and others not. The
	// un-inferred roots then fail guardPlatformPath on every push while the mapped
	// ones sail through, which looks GREEN on any single-library check. Say so
	// loudly rather than let a partial set masquerade as a complete one.
	if unmapped := r.unmappedLibraryRoots(ctx, mappings); len(unmapped) > 0 {
		r.logger.Warn("path-mapping inference is PARTIAL: some library roots have no mapping; "+
			"pushes for artists under them will be refused until one is inferred or entered manually",
			"connection_id", conn.ID,
			"mapped", len(mappings),
			"unmapped_roots", strings.Join(unmapped, ", "))
	}
}

// unmappedLibraryRoots returns the library roots that NO mapping in mappings
// covers - i.e. the roots whose artists would still be pushed with a raw host
// path (and therefore refused by the root guard). A root counts as covered when
// some mapping's HostPrefix is a separator-bounded prefix of it, or vice versa
// (a mapping may be inferred at the root itself or at a parent of it).
//
// Returns nil when the library service is unavailable or every root is covered.
// It is diagnostic only: it never blocks or alters what gets persisted.
func (r *Router) unmappedLibraryRoots(ctx context.Context, mappings []connection.PathMapping) []string {
	if r.libraryService == nil || len(mappings) == 0 {
		return nil
	}
	libs, err := r.libraryService.List(ctx)
	if err != nil {
		r.logger.Info("path-mapping inference: could not list libraries to check root coverage", "error", err)
		return nil
	}
	var unmapped []string
	for i := range libs {
		root := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(libs[i].Path), `\`, "/"), "/")
		if root == "" {
			continue
		}
		covered := false
		for _, m := range mappings {
			host := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(m.HostPrefix), `\`, "/"), "/")
			if host == "" {
				continue
			}
			if root == host || strings.HasPrefix(root, host+"/") || strings.HasPrefix(host, root+"/") {
				covered = true
				break
			}
		}
		if !covered {
			unmapped = append(unmapped, root)
		}
	}
	return unmapped
}

// applyInferredPathMappingsAllConnections re-runs inference for every enabled,
// still-unmapped connection. It is the scanner's post-scan hook (wired in
// NewRouter) and the fix for the OOBE ordering hole: inference joins Stillwater
// artists to a peer's artists by MusicBrainz ID, so it can only produce anything
// once the library has been scanned. The operator, however, adds the connection
// FIRST - at which point ListMBIDPaths returns zero rows, inference yields
// nothing, and the create/update handlers are the only things that ever call it.
// Re-running here, where the inputs first exist, is what keeps the feature from
// being inert in the normal first-run order (and keeps the fail-closed root
// guard from refusing every push on a split mount forever).
//
// Best-effort and idempotent by construction: applyInferredPathMappingsIfEmpty
// skips disabled connections, skips any connection that already carries mappings
// (so an operator override is never clobbered), and swallows per-connection
// failures. A later scan simply tries again.
func (r *Router) applyInferredPathMappingsAllConnections(ctx context.Context) {
	if r.connectionService == nil {
		return
	}
	conns, err := r.connectionService.List(ctx)
	if err != nil {
		r.logger.Error("post-scan path-mapping inference: listing connections", "error", err)
		return
	}
	for i := range conns {
		// Index-based: Connection is large enough that a per-iteration value copy
		// trips gocritic's rangeValCopy.
		r.applyInferredPathMappingsIfEmpty(ctx, &conns[i])
	}
}
