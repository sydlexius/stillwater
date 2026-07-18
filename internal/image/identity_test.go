package image

import (
	"math"
	"testing"
)

// withFlippedBits flips exactly the lowest n bits of candidateBase, so the
// returned hash is exactly Hamming distance n from candidateBase (XOR flips
// exactly the masked bits; a mask built from n distinct single-bit values
// has popcount n).
func withFlippedBits(n int) uint64 {
	var mask uint64
	for i := 0; i < n; i++ {
		mask |= 1 << uint(i)
	}
	return candidateBase ^ mask
}

// candidateBase is an arbitrary nonzero 64-bit pattern used as the reference
// point for constructing hashes at known Hamming distances via
// withFlippedBits. Its exact value does not matter -- only distances from it
// do -- as long as it is nonzero (candidate == 0 has its own dedicated
// unusable-input test).
const candidateBase = uint64(0x0F0F0F0F0F0F0F0F)

func TestCompareIdentity(t *testing.T) {
	t.Parallel()

	// Sanity-check the distance construction itself before trusting it in
	// every case below: distance 6 -> similarity 0.90625 (>= 0.90 default
	// tolerance, collides); distance 7 -> similarity 0.890625 (< 0.90,
	// does not collide). This is the exact boundary the tolerance draws.
	if sim := Similarity(candidateBase, withFlippedBits(6)); math.Abs(sim-0.90625) > 1e-9 {
		t.Fatalf("distance-6 similarity = %v, want 0.90625 (test construction is broken)", sim)
	}
	if sim := Similarity(candidateBase, withFlippedBits(7)); math.Abs(sim-0.890625) > 1e-9 {
		t.Fatalf("distance-7 similarity = %v, want 0.890625 (test construction is broken)", sim)
	}

	const tolerance = 0.90 // mirrors defaultPHashMismatchTolerance / defaultImageDupTolerance

	dist2 := withFlippedBits(2) // similarity 0.96875, well inside tolerance
	dist6 := withFlippedBits(6) // similarity 0.90625, just inside tolerance
	dist7 := withFlippedBits(7) // similarity 0.890625, just outside tolerance

	tests := []struct {
		name       string
		candidate  uint64
		destArtist string
		reference  []FanartIdentityEntry
		tolerance  float64

		wantVerdict    IdentityVerdict
		wantColliding  string
		wantSimilarity float64
		wantMatchCount int
	}{
		{
			name:       "match: no cross-artist collision within tolerance",
			candidate:  candidateBase,
			destArtist: "artist-A",
			reference: []FanartIdentityEntry{
				{ArtistID: "artist-B", PHash: dist7},
			},
			tolerance:   tolerance,
			wantVerdict: IdentityMatch,
		},
		{
			name:       "single-artist mismatch",
			candidate:  candidateBase,
			destArtist: "artist-A",
			reference: []FanartIdentityEntry{
				{ArtistID: "artist-B", PHash: dist6},
			},
			tolerance:      tolerance,
			wantVerdict:    IdentityMismatch,
			wantColliding:  "artist-B",
			wantSimilarity: 0.90625,
			wantMatchCount: 1,
		},
		{
			name:       "many-artist mismatch picks the best (highest-similarity) colliding artist",
			candidate:  candidateBase,
			destArtist: "artist-A",
			reference: []FanartIdentityEntry{
				{ArtistID: "artist-B", PHash: dist6},
				{ArtistID: "artist-C", PHash: dist2}, // higher similarity, should win
				{ArtistID: "artist-D", PHash: dist7}, // outside tolerance, must not count
			},
			tolerance:      tolerance,
			wantVerdict:    IdentityMismatch,
			wantColliding:  "artist-C",
			wantSimilarity: 0.96875,
			wantMatchCount: 2, // B and C, not D
		},
		{
			name:       "dest artist's own entries are excluded from comparison",
			candidate:  candidateBase,
			destArtist: "artist-A",
			reference: []FanartIdentityEntry{
				{ArtistID: "artist-A", PHash: candidateBase}, // identical hash, but it's the dest artist itself
				{ArtistID: "artist-B", PHash: dist7},         // outside tolerance
			},
			tolerance:   tolerance,
			wantVerdict: IdentityMatch,
		},
		{
			name:       "zero/unusable candidate is indeterminate",
			candidate:  0,
			destArtist: "artist-A",
			reference: []FanartIdentityEntry{
				{ArtistID: "artist-B", PHash: dist2},
			},
			tolerance:   tolerance,
			wantVerdict: IdentityIndeterminate,
		},
		{
			name:        "empty registry is indeterminate",
			candidate:   candidateBase,
			destArtist:  "artist-A",
			reference:   nil,
			tolerance:   tolerance,
			wantVerdict: IdentityIndeterminate,
		},
		{
			name:       "NaN tolerance is indeterminate, never a false collision",
			candidate:  candidateBase,
			destArtist: "artist-A",
			reference: []FanartIdentityEntry{
				{ArtistID: "artist-B", PHash: candidateBase}, // identical hash: would be a guaranteed match at any valid tolerance
			},
			tolerance:   math.NaN(),
			wantVerdict: IdentityIndeterminate,
		},
		{
			name:       "zero tolerance is indeterminate",
			candidate:  candidateBase,
			destArtist: "artist-A",
			reference: []FanartIdentityEntry{
				{ArtistID: "artist-B", PHash: candidateBase},
			},
			tolerance:   0,
			wantVerdict: IdentityIndeterminate,
		},
		{
			name:       "above-1 tolerance is indeterminate",
			candidate:  candidateBase,
			destArtist: "artist-A",
			reference: []FanartIdentityEntry{
				{ArtistID: "artist-B", PHash: candidateBase},
			},
			tolerance:   1.5,
			wantVerdict: IdentityIndeterminate,
		},
		{
			name:       "just inside tolerance (distance 6) collides",
			candidate:  candidateBase,
			destArtist: "artist-A",
			reference: []FanartIdentityEntry{
				{ArtistID: "artist-B", PHash: dist6},
			},
			tolerance:      tolerance,
			wantVerdict:    IdentityMismatch,
			wantColliding:  "artist-B",
			wantSimilarity: 0.90625,
			wantMatchCount: 1,
		},
		{
			name:       "just outside tolerance (distance 7) does not collide",
			candidate:  candidateBase,
			destArtist: "artist-A",
			reference: []FanartIdentityEntry{
				{ArtistID: "artist-B", PHash: dist7},
			},
			tolerance:   tolerance,
			wantVerdict: IdentityMatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := CompareIdentity(tt.candidate, tt.destArtist, tt.reference, tt.tolerance)

			if got.Verdict != tt.wantVerdict {
				t.Fatalf("Verdict = %v, want %v", got.Verdict, tt.wantVerdict)
			}
			if got.Verdict != IdentityMismatch {
				// Non-mismatch verdicts must never carry mismatch fields --
				// a consumer that forgets to switch on Verdict must not
				// accidentally see a stale colliding artist.
				if got.CollidingArtistID != "" || got.Similarity != 0 || got.MatchCount != 0 {
					t.Errorf("non-mismatch verdict carried mismatch fields: %+v", got)
				}
				return
			}
			if got.CollidingArtistID != tt.wantColliding {
				t.Errorf("CollidingArtistID = %q, want %q", got.CollidingArtistID, tt.wantColliding)
			}
			if math.Abs(got.Similarity-tt.wantSimilarity) > 1e-9 {
				t.Errorf("Similarity = %v, want %v", got.Similarity, tt.wantSimilarity)
			}
			if got.MatchCount != tt.wantMatchCount {
				t.Errorf("MatchCount = %d, want %d", got.MatchCount, tt.wantMatchCount)
			}
		})
	}
}

// TestCompareIdentity_ZeroReferenceHashIgnored guards the same zero-hash
// trap usablePHash exists to close off (internal/rule/phash_mismatch.go:
// 259-279): a reference entry that itself carries an unusable zero hash must
// never be treated as a collision, even though bit-for-bit XOR-based
// similarity between two zero-ish inputs can look deceptively perfect. The
// registry loader is expected to filter these out before they ever reach
// CompareIdentity, but the primitive defends the invariant itself rather
// than trusting every future caller to get that filtering right.
func TestCompareIdentity_ZeroReferenceHashIgnored(t *testing.T) {
	t.Parallel()
	got := CompareIdentity(candidateBase, "artist-A", []FanartIdentityEntry{
		{ArtistID: "artist-B", PHash: 0},
	}, 0.90)
	if got.Verdict != IdentityMatch {
		t.Fatalf("Verdict = %v, want IdentityMatch (a zero reference hash must never collide)", got.Verdict)
	}
}

func TestIdentityVerdict_String(t *testing.T) {
	t.Parallel()
	cases := map[IdentityVerdict]string{
		IdentityIndeterminate: "indeterminate",
		IdentityMatch:         "match",
		IdentityMismatch:      "mismatch",
		IdentityVerdict(99):   "unknown",
	}
	for v, want := range cases {
		if got := v.String(); got != want {
			t.Errorf("IdentityVerdict(%d).String() = %q, want %q", v, got, want)
		}
	}
}
