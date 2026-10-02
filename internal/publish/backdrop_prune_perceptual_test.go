package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"sort"
	"testing"

	img "github.com/sydlexius/stillwater/internal/image"
)

// fieldJPEG encodes bandJPEG's deterministic pixel field for seed, scaled by
// nearest-neighbor to (64*scale)x(64*scale). Same seed + different scale = the
// same picture with different bytes: one picture pushed at two resolutions,
// the population the perceptual tier exists for.
func fieldJPEG(t *testing.T, seed, scale int) []byte {
	t.Helper()
	const n = 64
	state := uint32(seed)*2654435761 + 1
	field := make([]uint8, n*n)
	for i := range field {
		state = state*1664525 + 1013904223
		field[i] = uint8(state >> 24)
	}
	w := n * scale
	m := stdimage.NewRGBA(stdimage.Rect(0, 0, w, w))
	for y := 0; y < w; y++ {
		for x := 0; x < w; x++ {
			v := field[(y/scale)*n+x/scale]
			m.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding fixture: %v", err)
	}
	return buf.Bytes()
}

// mustSimilar asserts the fixture's DEFINING property before any test trusts it:
// a perceptual pair that does not actually clear the tolerance would make every
// assertion below pass or fail for the wrong reason.
func mustSimilar(t *testing.T, a, b []byte, want bool) {
	t.Helper()
	ha, errA := img.PerceptualHash(bytes.NewReader(a))
	hb, errB := img.PerceptualHash(bytes.NewReader(b))
	if errA != nil || errB != nil {
		t.Fatalf("hashing fixture: %v / %v", errA, errB)
	}
	if got := img.Similarity(ha, hb) >= img.DefaultDuplicateTolerance; got != want {
		t.Fatalf("fixture precondition: similar=%v (sim %.3f), want %v", got, img.Similarity(ha, hb), want)
	}
}

// fp builds a synthetic fingerprint, so non-transitivity can be pinned with
// exact Hamming distances rather than whatever a JPEG happens to hash to.
func fp(index int, phash uint64, area int64, size int, twin bool) backdropFingerprint {
	return backdropFingerprint{index: index, content: sha256.Sum256([]byte{byte(index)}), phash: phash, area: area, size: size, localTwin: twin}
}

// indexSurvivor returns "index->survivor" pairs in delete order, for compact
// assertions.
func indexSurvivor(rs []redundantBackdrop) [][2]int {
	out := make([][2]int, len(rs))
	for i, r := range rs {
		out[i] = [2]int{r.Index, r.Survivor}
	}
	sort.Slice(out, func(a, b int) bool { return out[a][0] > out[b][0] })
	return out
}

func equalPairs(a, b [][2]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Base hash plus 4 and 8 flipped bits: sim(A,B)=sim(B,C)=60/64 clears 0.90,
// sim(A,C)=56/64 does not.
const (
	phA = uint64(0xF0F0_F0F0_F0F0_F0F0)
	phB = phA ^ 0x0F
	phC = phA ^ 0xFF
)

func TestPerceptualRedundant_ChainNeverCollapses(t *testing.T) {
	t.Parallel()
	if img.Similarity(phA, phC) >= img.DefaultDuplicateTolerance || img.Similarity(phA, phB) < img.DefaultDuplicateTolerance {
		t.Fatal("fixture precondition: want A~B, B~C, A!~C")
	}
	got := indexSurvivor(perceptualRedundant([]backdropFingerprint{
		fp(0, phA, 100, 10, false), fp(1, phB, 100, 10, false), fp(2, phC, 100, 10, false),
	}, nil))
	// Equal quality: the lowest index is preferred, keeps 0, deletes its direct
	// pair 1, and C (distinct from A) survives as its own representative.
	if want := [][2]int{{1, 0}}; !equalPairs(got, want) {
		t.Errorf("got %v, want %v (naive grouping would also delete 2)", got, want)
	}
}

func TestPerceptualRedundant_SurvivorIsTheBestCopyEvenAboveItsCandidates(t *testing.T) {
	t.Parallel()
	got := indexSurvivor(perceptualRedundant([]backdropFingerprint{
		fp(0, phA, 100, 10, false), // smallest
		fp(1, phA, 400, 10, false),
		fp(2, phA, 400, 20, false), // largest area, larger bytes: kept
	}, nil))
	if want := [][2]int{{1, 2}, {0, 2}}; !equalPairs(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Rule 2 (area) outranks rule 3 (bytes): a bigger, smaller-file copy is kept
// over a smaller, bigger-file one.
// perceptualRedundant's own result honors the delete-order contract; its
// internal map would otherwise hand entries back in random order.
func TestPerceptualRedundant_ReturnsDescending(t *testing.T) {
	t.Parallel()
	fps := []backdropFingerprint{fp(0, phA, 900, 10, false)}
	for i := 1; i <= 6; i++ {
		fps = append(fps, fp(i, phA, 100, 10, false))
	}
	for run := 0; run < 50; run++ {
		got := perceptualRedundant(fps, nil)
		for i := 1; i < len(got); i++ {
			if got[i].Index >= got[i-1].Index {
				t.Fatalf("run %d: not descending: %+v", run, got)
			}
		}
	}
}

func TestPerceptualRedundant_AreaBeatsByteSize(t *testing.T) {
	t.Parallel()
	got := indexSurvivor(perceptualRedundant([]backdropFingerprint{
		fp(0, phA, 400, 10, false),
		fp(1, phA, 100, 20, false),
	}, nil))
	if want := [][2]int{{1, 0}}; !equalPairs(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Two-sided threshold: 6 differing bits (similarity 58/64 = 0.906) clears the
// shared 0.90 tolerance, 7 bits (57/64 = 0.891) does not. Pins the number from
// both sides, so moving it either way is caught.
func TestPerceptualRedundant_ThresholdBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		flip  uint64
		match bool
	}{
		{"6 bits apart matches", 0x3F, true},
		{"7 bits apart does not", 0x7F, false},
	} {
		got := perceptualRedundant([]backdropFingerprint{
			fp(0, phA, 100, 10, false), fp(1, phA^tc.flip, 100, 10, false),
		}, nil)
		if (len(got) == 1) != tc.match {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}

// Many interleaved exact and perceptual deletions come back strictly
// descending on every run, whatever order the internal map yields them in.
func TestDetectBackdropRedundancy_MergedPlanIsStrictlyDescending(t *testing.T) {
	t.Parallel()
	big := fieldJPEG(t, 7, 3)
	small := fieldJPEG(t, 7, 1)
	mid := fieldJPEG(t, 7, 2)
	other := fieldJPEG(t, 99, 1)
	otherBig := fieldJPEG(t, 99, 2)
	mustSimilar(t, big, small, true)
	mustSimilar(t, other, otherBig, true)
	mustSimilar(t, big, other, false)
	// Exact deletions 9,8,7,6,4; perceptual deletions 5,3,2 (survivors 0 and
	// 1). Concatenated without the merge sort that is ...,6,4,5,... -- so the
	// tiers genuinely interleave and the order cannot be right by accident.
	backdrops := [][]byte{big, otherBig, small, other, small, mid, other, mid, small, other}
	for run := 0; run < 50; run++ {
		fake := &fakeBackdropClient{backdrops: backdrops, failAt: -1, failDeleteAt: -1}
		got, _, pErr, err := detectBackdropRedundancy(context.Background(), fake, "p1", perceptualPruneOpts{Enabled: true})
		if err != nil || pErr != nil {
			t.Fatalf("detect: %v / %v", err, pErr)
		}
		if len(got) != 8 {
			t.Fatalf("got %d entries, want 8 (all but the two biggest copies)", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i].Index >= got[i-1].Index {
				t.Fatalf("run %d: not strictly descending at %d: %+v", run, i, got)
			}
		}
	}
}

func TestPerceptualRedundant_LocalTwinIsKeptAndPreferred(t *testing.T) {
	t.Parallel()
	got := indexSurvivor(perceptualRedundant([]backdropFingerprint{
		fp(0, phA, 900, 99, false), // bigger, but not what the operator keeps locally
		fp(1, phA, 100, 10, true),
		fp(2, phA, 100, 10, true), // a second local twin of the same picture
	}, nil))
	// Slot 1 wins on the twin rule; slot 2 is also a twin, so it is never a
	// candidate even though it matches.
	if want := [][2]int{{0, 1}}; !equalPairs(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPerceptualRedundant_ExcludesClaimedAndUnhashed(t *testing.T) {
	t.Parallel()
	got := perceptualRedundant([]backdropFingerprint{
		fp(0, phA, 100, 10, false),
		fp(1, phA, 100, 10, false), // claimed by the exact tier
		fp(2, 0, 100, 10, false),   // phash 0: Similarity(0,0)=1 would match anything
		fp(3, 0, 100, 10, false),
	}, map[int]bool{1: true})
	if len(got) != 0 {
		t.Errorf("got %v, want nothing (claimed and zero-hash slots take no part)", got)
	}
}

func TestDetectBackdropRedundancy_MixedTiers(t *testing.T) {
	t.Parallel()
	hi := fieldJPEG(t, 7, 2)     // 128x128: the best copy of picture 7
	lo := fieldJPEG(t, 7, 1)     // 64x64 downscale of the same picture
	other := fieldJPEG(t, 99, 1) // a different picture
	mustSimilar(t, hi, lo, true)
	mustSimilar(t, hi, other, false)
	fake := &fakeBackdropClient{backdrops: [][]byte{lo, lo, other, hi}, failAt: -1, failDeleteAt: -1}

	got, total, pErr, err := detectBackdropRedundancy(context.Background(), fake, "p1", perceptualPruneOpts{Enabled: true})
	if err != nil || pErr != nil {
		t.Fatalf("detect: err=%v perceptualErr=%v", err, pErr)
	}
	if total != 4 {
		t.Fatalf("total = %d, want 4", total)
	}
	// Exact: 1 duplicates 0. Perceptual: 0 is the same picture as 3, and 3 is
	// larger, so 0 goes and 3 -- ABOVE it -- survives.
	want := []redundantBackdrop{
		{Index: 1, Survivor: 0, Tier: PruneTierExact},
		{Index: 0, Survivor: 3, Tier: PruneTierPerceptual},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %d entries", got, len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Index != w.Index || g.Survivor != w.Survivor || g.Tier != w.Tier {
			t.Errorf("entry %d = {%d %d %s}, want {%d %d %s}", i, g.Index, g.Survivor, g.Tier, w.Index, w.Survivor, w.Tier)
		}
	}
	if got[1].SurvivorHash != sha256.Sum256(hi) || got[1].Hash != sha256.Sum256(lo) {
		t.Error("perceptual entry must carry each side's OWN detection-time hash for re-verify")
	}
}

func TestDetectBackdropRedundancy_UndecodableBackdropFailsThePerceptualTierClosed(t *testing.T) {
	t.Parallel()
	a := fieldJPEG(t, 7, 2)
	b := fieldJPEG(t, 7, 1)
	fake := &fakeBackdropClient{backdrops: [][]byte{a, b, []byte("not an image"), []byte("not an image")}, failAt: -1, failDeleteAt: -1}

	got, _, pErr, err := detectBackdropRedundancy(context.Background(), fake, "p1", perceptualPruneOpts{Enabled: true})
	if err != nil {
		t.Fatalf("a decode failure is not a fetch failure: %v", err)
	}
	if pErr == nil {
		t.Fatal("want a perceptual-tier error naming the undecodable backdrop")
	}
	// The exact tier needs no decode and still runs: 3 duplicates 2. The
	// similar pair 0/1 is NOT pruned.
	if len(got) != 1 || got[0].Index != 3 || got[0].Tier != PruneTierExact {
		t.Errorf("got %+v, want only the exact entry for index 3", got)
	}
}

func TestDetectBackdropRedundancy_DisabledIsExactOnly(t *testing.T) {
	t.Parallel()
	fake := &fakeBackdropClient{backdrops: [][]byte{fieldJPEG(t, 7, 1), fieldJPEG(t, 7, 2)}, failAt: -1, failDeleteAt: -1}
	got, _, pErr, err := detectBackdropRedundancy(context.Background(), fake, "p1", perceptualPruneOpts{})
	if err != nil || pErr != nil || len(got) != 0 {
		t.Errorf("got %+v err=%v pErr=%v, want no entries with the tier off", got, err, pErr)
	}
}
