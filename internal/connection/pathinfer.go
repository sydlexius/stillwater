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
