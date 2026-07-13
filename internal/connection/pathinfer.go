package connection

import (
	"sort"
	"strings"
)

// DefaultPathInferConsensus is the minimum number of matched artist pairs that
// must agree on the same host->platform prefix pair (per platform root) before
// InferPathMappings emits a mapping for it. Two is deliberately low but not one:
// a single agreeing pair could be a coincidence (e.g. one oddly-nested artist),
// while two independent artists sharing the same prefix translation is strong
// evidence of a real split-mount layout. Operators can always override via the
// manual editor (#2303 / #2329).
const DefaultPathInferConsensus = 2

// PathPair is one observed (Stillwater host path, Lidarr platform path) pair for
// the SAME artist, matched by MusicBrainz ID upstream. InferPathMappings derives
// candidate prefix translations from a batch of these. Both sides name the same
// artist directory, so their trailing segment is expected to be identical and
// only the leading mount prefix differs on a split-mount deployment.
type PathPair struct {
	// HostPath is the artist directory as Stillwater sees it on disk.
	HostPath string
	// PlatformPath is the same artist directory as Lidarr addresses it.
	PlatformPath string
}

// InferPathMappings derives host->platform prefix mappings from a batch of
// matched artist path pairs, with zero I/O. For each pair it strips the
// identical trailing artist-folder segment; the differing leading directories
// are a candidate (HostPrefix, PlatformPrefix) pair. Candidates are clustered by
// platform prefix so a multi-root Lidarr yields one mapping per root, and a
// mapping is emitted only when at least minConsensus pairs agree on the same
// host prefix for that root (majority per root, with a deterministic
// lexicographic tie-break on the host string so a split vote never depends on
// map-iteration order).
//
// A pair is SKIPPED (contributes no candidate) when:
//   - its normalized trailing basenames differ (not the same artist directory,
//     so the prefixes are not comparable);
//   - either side's leading directory is empty (a root-level path carries no
//     prefix to translate);
//   - the two leading directories are identical (a shared mount needs no
//     mapping - sending the path verbatim already resolves).
//
// Both sides are normalized first: backslashes become forward slashes (platform
// paths cross the wire in POSIX form regardless of host OS) and trailing slashes
// are trimmed. The returned slice is sorted by platform prefix then host prefix
// for a stable result, and is nil when no mapping meets the consensus floor.
func InferPathMappings(pairs []PathPair, minConsensus int) []PathMapping {
	if minConsensus < 1 {
		minConsensus = 1
	}

	// platformPrefix -> hostPrefix -> vote count. Clustering by platform prefix
	// (the Lidarr root) is what lets a multi-root instance produce one mapping
	// per root instead of a single blended guess.
	votes := make(map[string]map[string]int)

	for _, p := range pairs {
		hostDir, hostBase, ok := splitDirBase(p.HostPath)
		if !ok {
			continue
		}
		platDir, platBase, ok := splitDirBase(p.PlatformPath)
		if !ok {
			continue
		}
		// Different artist folders can't calibrate a prefix translation.
		if hostBase != platBase {
			continue
		}
		// Identical leading directories = shared mount; verbatim already works.
		if hostDir == platDir {
			continue
		}
		hostVotes := votes[platDir]
		if hostVotes == nil {
			hostVotes = make(map[string]int)
			votes[platDir] = hostVotes
		}
		hostVotes[hostDir]++
	}

	var out []PathMapping
	for platPrefix, hostVotes := range votes {
		bestHost := ""
		bestCount := 0
		for host, count := range hostVotes {
			// Majority wins; ties break on the lexicographically smaller host so
			// the result is deterministic regardless of map-iteration order.
			//
			// Intentional limitation: when artists under one platform root sit at
			// mixed nesting depths (e.g. most at /music/<artist> but a few at
			// /music/sub/<artist>, all landing under /data), only the majority
			// host prefix is emitted for that root. The flat PathMapping table
			// cannot express a per-artist exception, so the minority layout is
			// left unmapped and MapArtistPath would mistranslate it. This is the
			// spec-accepted majority-wins tradeoff: inference is best-effort and
			// operator-overridable via the manual editor, not a guarantee that
			// every observed artist round-trips. Do not "fix" this by emitting the
			// minority pair too -- two mappings sharing a platform root would make
			// the reverse translation ambiguous.
			if count > bestCount || (count == bestCount && (bestHost == "" || host < bestHost)) {
				bestHost = host
				bestCount = count
			}
		}
		if bestCount >= minConsensus {
			out = append(out, PathMapping{HostPrefix: bestHost, PlatformPrefix: platPrefix})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].PlatformPrefix != out[j].PlatformPrefix {
			return out[i].PlatformPrefix < out[j].PlatformPrefix
		}
		return out[i].HostPrefix < out[j].HostPrefix
	})
	return out
}

// splitDirBase normalizes a path (backslashes to forward slashes, trailing
// slashes trimmed) then splits it into its leading directory and trailing
// segment on the final forward slash. ok is false when the path has no leading
// directory (empty, or a bare/root-level segment with no separator), because
// such a path carries no prefix to translate.
func splitDirBase(p string) (dir, base string, ok bool) {
	norm := strings.TrimRight(strings.ReplaceAll(p, `\`, "/"), "/")
	i := strings.LastIndex(norm, "/")
	if i <= 0 {
		// i < 0: no separator (bare segment). i == 0: root-level ("/Artist"),
		// whose leading directory is empty. Neither yields a usable prefix.
		return "", "", false
	}
	dir = norm[:i]
	base = norm[i+1:]
	if base == "" {
		return "", "", false
	}
	return dir, base, true
}

// InferPathMappingsFromRoots derives host->platform prefix mappings by pairing
// Stillwater's LIBRARY ROOTS against the peer's OWN ROOTS (Lidarr root folders,
// Emby/Jellyfin library Locations), matching on the final path segment
// case-insensitively. Zero I/O, zero artist evidence required.
//
// This exists because per-artist evidence is NOT universally available (#2380
// functional test): Emby returns NO Path on artist items - not from
// /Artists/AlbumArtists, not from /Items, not from the single-item detail
// endpoint, with or without Fields=Path - so an inference that can only learn
// from (host path, platform path) artist pairs infers NOTHING for Emby, forever,
// no matter how many artists are linked. Emby DOES report its library Locations,
// and Lidarr reports its root folders, so the roots are the one signal every peer
// type actually gives us. Root pairing also covers a library root that simply has
// too few matched artists to clear the pair-consensus floor (a one-artist
// classical root), which is how a connection ended up half-mapped.
//
// Pairing rules, deliberately conservative - a WRONG mapping is worse than none,
// because the root guard would then refuse pushes with a translation the operator
// can see but not explain:
//   - A host root maps to a platform root when their final segments match
//     case-insensitively ("/host/media/classical" <-> "/classical") and that match
//     is UNIQUE on both sides. Ambiguity (two roots sharing a segment) yields no
//     mapping for the roots involved.
//   - When each side has exactly ONE root, they are paired regardless of segment,
//     since there is no other candidate to confuse them with.
//   - Identical host and platform paths are skipped: a shared mount needs no
//     mapping and sending the path verbatim already resolves.
//
// Results are sorted (platform prefix, then host prefix) for determinism.
func InferPathMappingsFromRoots(hostRoots, platformRoots []string) []PathMapping {
	hosts := normalizeRootList(hostRoots)
	plats := normalizeRootList(platformRoots)
	if len(hosts) == 0 || len(plats) == 0 {
		return nil
	}

	var out []PathMapping
	add := func(host, plat string) {
		if host == "" || plat == "" || host == plat {
			return
		}
		out = append(out, PathMapping{HostPrefix: host, PlatformPrefix: plat})
	}

	// Single root on each side: unambiguous by construction.
	if len(hosts) == 1 && len(plats) == 1 {
		add(hosts[0], plats[0])
		return out
	}

	// Segment matching. Count both sides first so a duplicated segment can be
	// rejected rather than resolved arbitrarily.
	hostSeg := map[string]int{}
	for _, h := range hosts {
		hostSeg[lastSegment(h)]++
	}
	platBySeg := map[string][]string{}
	for _, p := range plats {
		seg := lastSegment(p)
		platBySeg[seg] = append(platBySeg[seg], p)
	}

	for _, h := range hosts {
		seg := lastSegment(h)
		if seg == "" || hostSeg[seg] != 1 {
			continue // ambiguous on the host side
		}
		cands := platBySeg[seg]
		if len(cands) != 1 {
			continue // absent, or ambiguous on the platform side
		}
		add(h, cands[0])
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].PlatformPrefix != out[j].PlatformPrefix {
			return out[i].PlatformPrefix < out[j].PlatformPrefix
		}
		return out[i].HostPrefix < out[j].HostPrefix
	})
	return out
}

// MergePathMappings returns base plus every mapping from extra whose HostPrefix
// is not already covered by base. Evidence-based mappings (the artist-pair kind)
// are the base and always win; root-paired mappings only FILL the roots the
// evidence could not reach. Coverage is separator-bounded, so a base mapping at
// "/host/media" covers an extra at "/host/media/classical" and the two cannot
// both be emitted for the same subtree.
func MergePathMappings(base, extra []PathMapping) []PathMapping {
	out := append([]PathMapping(nil), base...)
	for _, e := range extra {
		host := normalizeRootPath(e.HostPrefix)
		if host == "" {
			continue
		}
		covered := false
		for _, b := range out {
			bh := normalizeRootPath(b.HostPrefix)
			if bh == "" {
				continue
			}
			if _, ok := pathRemainder(host, bh); ok {
				covered = true
				break
			}
			if _, ok := pathRemainder(bh, host); ok {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PlatformPrefix != out[j].PlatformPrefix {
			return out[i].PlatformPrefix < out[j].PlatformPrefix
		}
		return out[i].HostPrefix < out[j].HostPrefix
	})
	return out
}

// normalizeRootList normalizes each root (POSIX fold, Clean, trailing-slash trim)
// and drops empties and duplicates, preserving first-seen order.
func normalizeRootList(roots []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range roots {
		n := normalizeRootPath(r)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// lastSegment returns the final path segment of an already-normalized root.
func lastSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return strings.ToLower(p[i+1:])
	}
	return strings.ToLower(p)
}
