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

// TestInferPathMappings_TwoHostRootsIntoOneContainerRoot is the multi-root case a
// real split-mount deployment actually has, and which a single-mapping test would
// pass right over: TWO separate host library roots collapse into SUBFOLDERS of one
// container root on the peer. This is not one prefix swap - "/host/media -> /music"
// would be wrong in both shape and case - so inference must emit one mapping PER
// root, and the mapped paths must all land inside the peer's single root.
func TestInferPathMappings_TwoHostRootsIntoOneContainerRoot(t *testing.T) {
	t.Parallel()

	pairs := []PathPair{
		// Host root A -> /music/RootA
		{HostPath: "/host/media/roota/Alpha", PlatformPath: "/music/RootA/Alpha"},
		{HostPath: "/host/media/roota/Beta", PlatformPath: "/music/RootA/Beta"},
		// Host root B -> /music/RootB
		{HostPath: "/host/media/rootb/Gamma", PlatformPath: "/music/RootB/Gamma"},
		{HostPath: "/host/media/rootb/Delta", PlatformPath: "/music/RootB/Delta"},
	}

	got := InferPathMappings(pairs, DefaultPathInferConsensus)
	want := []PathMapping{
		{HostPrefix: "/host/media/roota", PlatformPrefix: "/music/RootA"},
		{HostPrefix: "/host/media/rootb", PlatformPrefix: "/music/RootB"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("InferPathMappings = %+v, want %+v (one mapping PER host root; stopping at the "+
			"first would leave every artist under the other root failing the root guard)", got, want)
	}

	// End to end: with BOTH mappings on the connection, an artist from either root
	// must translate and land inside the peer's single reported root. Longest-prefix
	// resolution must keep the two roots from claiming each other's paths.
	c := &Connection{PathMappings: got}
	for host, wantPlatform := range map[string]string{
		"/host/media/roota/Alpha": "/music/RootA/Alpha",
		"/host/media/rootb/Gamma": "/music/RootB/Gamma",
	} {
		mapped := c.MapArtistPath(host)
		if mapped != wantPlatform {
			t.Errorf("MapArtistPath(%q) = %q, want %q", host, mapped, wantPlatform)
		}
		if !PathWithinRoots(mapped, []string{"/music"}) {
			t.Errorf("mapped path %q is outside the peer root /music; the push would be refused", mapped)
		}
	}
}

// TestInferPathMappings_PartialEvidenceStillEmitsWhatItHas pins the partial case:
// one host root has enough matched artists, the other does not (yet). The evidenced
// root must still be emitted rather than the whole inference collapsing to nothing -
// half-mapped beats unmapped, and the caller surfaces the un-inferred root.
func TestInferPathMappings_PartialEvidenceStillEmitsWhatItHas(t *testing.T) {
	t.Parallel()

	pairs := []PathPair{
		{HostPath: "/host/media/roota/Alpha", PlatformPath: "/music/RootA/Alpha"},
		{HostPath: "/host/media/roota/Beta", PlatformPath: "/music/RootA/Beta"},
		// Only ONE pair for root B: below the consensus floor.
		{HostPath: "/host/media/rootb/Gamma", PlatformPath: "/music/RootB/Gamma"},
	}
	got := InferPathMappings(pairs, DefaultPathInferConsensus)
	want := []PathMapping{{HostPrefix: "/host/media/roota", PlatformPrefix: "/music/RootA"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("InferPathMappings = %+v, want %+v", got, want)
	}
}

// TestInferPathMappingsFromRoots covers the root-pairing signal that exists
// because artist evidence is not universally available: a live Emby server
// returns NO Path on artist items, so a pair-only inference maps Emby never.
// Roots, unlike artist paths, every peer type reports.
func TestInferPathMappingsFromRoots(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		hosts []string
		plats []string
		want  []PathMapping
	}{
		{
			// The real deployment shape: two host roots, two peer roots, matched by
			// final segment. Note the trailing slash a live Emby server actually
			// reports on one of its Locations.
			name:  "two roots matched by final segment",
			hosts: []string{"/host/media/classical", "/host/media/music"},
			plats: []string{"/classical", "/music/"},
			want: []PathMapping{
				{HostPrefix: "/host/media/classical", PlatformPrefix: "/classical"},
				{HostPrefix: "/host/media/music", PlatformPrefix: "/music"},
			},
		},
		{
			name:  "segment match is case-insensitive",
			hosts: []string{"/host/media/classical"},
			plats: []string{"/media/Classical", "/media/Music"},
			want:  []PathMapping{{HostPrefix: "/host/media/classical", PlatformPrefix: "/media/Classical"}},
		},
		{
			// One root on each side: pair them even though the segments differ -
			// there is no other candidate to confuse them with.
			name:  "single root each side pairs regardless of segment",
			hosts: []string{"/host/media/library"},
			plats: []string{"/data"},
			want:  []PathMapping{{HostPrefix: "/host/media/library", PlatformPrefix: "/data"}},
		},
		{
			// A WRONG mapping is worse than none: the guard would refuse pushes with
			// a translation the operator can see but cannot explain.
			name:  "ambiguous platform segment yields nothing",
			hosts: []string{"/host/a/music", "/host/b/other"},
			plats: []string{"/x/music", "/y/music"},
			want:  nil,
		},
		{
			name:  "ambiguous host segment yields nothing",
			hosts: []string{"/host/a/music", "/host/b/music"},
			plats: []string{"/peer/music", "/peer/other"},
			want:  nil,
		},
		{
			// Shared mount: host and platform agree already, so no mapping is needed
			// and emitting an identity mapping would just be noise.
			name:  "identical roots need no mapping",
			hosts: []string{"/music"},
			plats: []string{"/music"},
			want:  nil,
		},
		{
			name:  "no roots on one side",
			hosts: []string{"/host/media/music"},
			plats: nil,
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := InferPathMappingsFromRoots(tc.hosts, tc.plats)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("InferPathMappingsFromRoots(%v, %v) = %+v, want %+v", tc.hosts, tc.plats, got, tc.want)
			}
		})
	}
}

// TestMergePathMappings pins the precedence rule: artist EVIDENCE wins, and root
// pairing only fills host roots the evidence never reached. Two mappings for one
// host subtree would make the translation ambiguous, so a covered root is never
// added twice.
func TestMergePathMappings(t *testing.T) {
	t.Parallel()

	base := []PathMapping{{HostPrefix: "/host/media/music", PlatformPrefix: "/music"}}
	extra := []PathMapping{
		// Already covered by the evidence-derived base: must NOT be added, even
		// though the root pairing would have translated it differently.
		{HostPrefix: "/host/media/music", PlatformPrefix: "/wrong"},
		// A child of a covered root: still covered, separator-bounded.
		{HostPrefix: "/host/media/music/sub", PlatformPrefix: "/wrong"},
		// Genuinely uncovered: this is the half-mapped root the evidence missed.
		{HostPrefix: "/host/media/classical", PlatformPrefix: "/classical"},
	}

	got := MergePathMappings(base, extra)
	want := []PathMapping{
		{HostPrefix: "/host/media/classical", PlatformPrefix: "/classical"},
		{HostPrefix: "/host/media/music", PlatformPrefix: "/music"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergePathMappings = %+v, want %+v (evidence wins; root pairing only fills gaps)", got, want)
	}
}
