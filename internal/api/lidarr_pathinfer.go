package api

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/sydlexius/stillwater/internal/connection"
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

// inferLidarrPathMappings derives host->platform path mappings for a Lidarr
// connection by matching Stillwater artists to the connection's Lidarr artists
// by MusicBrainz ID and feeding the resulting (host path, platform path) pairs
// to connection.InferPathMappings. It returns the derived mappings, the number
// of artists that matched by MBID (matched), and an error only when the inputs
// could not be gathered.
//
// BEST-EFFORT semantics: callers treat a non-nil error as "no inference
// available" and never fail the create/enable that triggered it. matched is the
// pair count fed to inference, useful for the "M matched artists" surface even
// when the consensus floor emits zero mappings.
func (r *Router) inferLidarrPathMappings(ctx context.Context, conn *connection.Connection) ([]connection.PathMapping, int, error) {
	if conn == nil || conn.Type != connection.TypeLidarr {
		return nil, 0, nil
	}

	// Stillwater side: every artist with both an MBID and a host path.
	mbidPaths, err := r.artistService.ListMBIDPaths(ctx)
	if err != nil {
		return nil, 0, err
	}
	if len(mbidPaths) == 0 {
		return nil, 0, nil
	}
	// Index host paths by normalized MBID for an O(1) join against Lidarr. A
	// duplicate MBID (data anomaly) keeps the first host path seen.
	hostByMBID := make(map[string]string, len(mbidPaths))
	for _, mp := range mbidPaths {
		key := strings.ToLower(strings.TrimSpace(mp.MBID))
		if key == "" || mp.Path == "" {
			continue
		}
		if _, ok := hostByMBID[key]; !ok {
			hostByMBID[key] = mp.Path
		}
	}

	// Lidarr side: enumerate the connection's artists via the swappable seam.
	lister := lidarrArtistListerFactory(conn, r.logger)
	if lister == nil {
		return nil, 0, nil
	}
	artists, err := lister.GetArtists(ctx)
	if err != nil {
		return nil, 0, err
	}

	var pairs []connection.PathPair
	for _, la := range artists {
		key := strings.ToLower(strings.TrimSpace(la.ForeignArtistID))
		if key == "" || la.Path == "" {
			continue
		}
		hostPath, ok := hostByMBID[key]
		if !ok {
			continue
		}
		pairs = append(pairs, connection.PathPair{HostPath: hostPath, PlatformPath: la.Path})
	}

	mappings := connection.InferPathMappings(pairs, connection.DefaultPathInferConsensus)
	return mappings, len(pairs), nil
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
	if conn == nil || conn.Type != connection.TypeLidarr || !conn.Enabled {
		return
	}
	// Fast pre-check on the in-memory snapshot: skip the enumeration entirely
	// when the connection already carries mappings. The authoritative empty
	// check is re-done under the lock below against the canonical row.
	if len(conn.GetPathMappings()) != 0 {
		return
	}

	mappings, matched, err := r.inferLidarrPathMappings(ctx, conn)
	if err != nil {
		r.logger.Info("path-mapping inference skipped", "connection_id", conn.ID, "error", err)
		return
	}
	if len(mappings) == 0 {
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
	r.logger.Info("applied inferred Lidarr path mappings",
		"connection_id", conn.ID,
		"mappings", len(mappings),
		"matched_artists", matched)
}
