package connection

import (
	"reflect"
	"testing"
)

// TestInferPathMappings exercises the pure prefix-derivation logic: strip the
// identical trailing artist folder, cluster the differing leading directories by
// platform root, and emit the majority host prefix per root once the consensus
// floor is met. Every case pins the exact emitted slice (order included, since
// InferPathMappings sorts deterministically) so a silent behavior change fails.
func TestInferPathMappings(t *testing.T) {
	tests := []struct {
		name         string
		pairs        []PathPair
		minConsensus int
		want         []PathMapping
	}{
		{
			name: "single root, consensus met",
			pairs: []PathPair{
				{HostPath: "/music/Alpha", PlatformPath: "/data/media/Alpha"},
				{HostPath: "/music/Beta", PlatformPath: "/data/media/Beta"},
			},
			minConsensus: 2,
			want:         []PathMapping{{HostPrefix: "/music", PlatformPrefix: "/data/media"}},
		},
		{
			name: "multi root, one mapping per root",
			pairs: []PathPair{
				{HostPath: "/music/Alpha", PlatformPath: "/data/media/Alpha"},
				{HostPath: "/music/Beta", PlatformPath: "/data/media/Beta"},
				{HostPath: "/music2/Gamma", PlatformPath: "/mnt/lib2/Gamma"},
				{HostPath: "/music2/Delta", PlatformPath: "/mnt/lib2/Delta"},
			},
			minConsensus: 2,
			want: []PathMapping{
				{HostPrefix: "/music", PlatformPrefix: "/data/media"},
				{HostPrefix: "/music2", PlatformPrefix: "/mnt/lib2"},
			},
		},
		{
			name: "mismatched trailing basename is skipped",
			pairs: []PathPair{
				// Different artist folders: not comparable, must not vote.
				{HostPath: "/music/Alpha", PlatformPath: "/data/media/Zeta"},
				{HostPath: "/music/Beta", PlatformPath: "/data/media/Beta"},
			},
			minConsensus: 2,
			want:         nil, // only one valid vote for /data/media, below floor of 2
		},
		{
			name: "consensus floor met exactly",
			pairs: []PathPair{
				{HostPath: "/music/Alpha", PlatformPath: "/data/Alpha"},
				{HostPath: "/music/Beta", PlatformPath: "/data/Beta"},
			},
			minConsensus: 2,
			want:         []PathMapping{{HostPrefix: "/music", PlatformPrefix: "/data"}},
		},
		{
			name: "below consensus floor emits nothing",
			pairs: []PathPair{
				{HostPath: "/music/Alpha", PlatformPath: "/data/Alpha"},
			},
			minConsensus: 2,
			want:         nil,
		},
		{
			name: "conflicting hosts, majority wins per root",
			pairs: []PathPair{
				{HostPath: "/music/Alpha", PlatformPath: "/data/Alpha"},
				{HostPath: "/music/Beta", PlatformPath: "/data/Beta"},
				{HostPath: "/music/Gamma", PlatformPath: "/data/Gamma"},
				// Minority host prefix for the same platform root: outvoted 3-1.
				{HostPath: "/other/Delta", PlatformPath: "/data/Delta"},
			},
			minConsensus: 2,
			want:         []PathMapping{{HostPrefix: "/music", PlatformPrefix: "/data"}},
		},
		{
			name: "tie between hosts breaks on lexicographic host order",
			pairs: []PathPair{
				{HostPath: "/zeta/Alpha", PlatformPath: "/data/Alpha"},
				{HostPath: "/zeta/Beta", PlatformPath: "/data/Beta"},
				{HostPath: "/alpha/Gamma", PlatformPath: "/data/Gamma"},
				{HostPath: "/alpha/Delta", PlatformPath: "/data/Delta"},
			},
			minConsensus: 2,
			// 2-2 tie; "/alpha" < "/zeta" so the lexicographically smaller wins.
			want: []PathMapping{{HostPrefix: "/alpha", PlatformPrefix: "/data"}},
		},
		{
			name:         "empty input",
			pairs:        nil,
			minConsensus: 2,
			want:         nil,
		},
		{
			name: "identity prefix (shared mount) is skipped",
			pairs: []PathPair{
				{HostPath: "/music/Alpha", PlatformPath: "/music/Alpha"},
				{HostPath: "/music/Beta", PlatformPath: "/music/Beta"},
			},
			minConsensus: 2,
			want:         nil,
		},
		{
			name: "root-level and bare paths carry no prefix and are skipped",
			pairs: []PathPair{
				{HostPath: "/Alpha", PlatformPath: "/data/Alpha"},  // host dir empty
				{HostPath: "Beta", PlatformPath: "/data/Beta"},     // host bare
				{HostPath: "/music/Gamma", PlatformPath: "/Gamma"}, // platform dir empty
			},
			minConsensus: 1,
			want:         nil,
		},
		{
			name: "normalization: backslashes and trailing slashes",
			pairs: []PathPair{
				// Windows-style host paths + a trailing slash on the platform side.
				{HostPath: `C:\music\Alpha`, PlatformPath: "/data/Alpha/"},
				{HostPath: `C:\music\Beta`, PlatformPath: "/data/Beta"},
			},
			minConsensus: 2,
			want:         []PathMapping{{HostPrefix: "C:/music", PlatformPrefix: "/data"}},
		},
		{
			name: "minConsensus below one is clamped to one",
			pairs: []PathPair{
				{HostPath: "/music/Alpha", PlatformPath: "/data/Alpha"},
			},
			minConsensus: 0,
			want:         []PathMapping{{HostPrefix: "/music", PlatformPrefix: "/data"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := InferPathMappings(tc.pairs, tc.minConsensus)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("InferPathMappings() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestInferPathMappings_Deterministic confirms repeated runs over the same input
// yield an identical slice, guarding the map-iteration tie-break from
// nondeterminism (Go randomizes map order per run).
func TestInferPathMappings_Deterministic(t *testing.T) {
	pairs := []PathPair{
		{HostPath: "/a/One", PlatformPath: "/x/One"},
		{HostPath: "/a/Two", PlatformPath: "/x/Two"},
		{HostPath: "/b/Three", PlatformPath: "/y/Three"},
		{HostPath: "/b/Four", PlatformPath: "/y/Four"},
		{HostPath: "/c/Five", PlatformPath: "/x/Five"},
	}
	first := InferPathMappings(pairs, 2)
	for i := 0; i < 50; i++ {
		if got := InferPathMappings(pairs, 2); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differed: %+v vs %+v", i, got, first)
		}
	}
}
