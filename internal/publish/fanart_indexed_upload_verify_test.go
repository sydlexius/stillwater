package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	stdimage "image"
	"image/jpeg"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	img "github.com/sydlexius/stillwater/internal/image"
)

// TestMain shrinks the #3126 verify timing to unit-test scale: a fake peer
// settles at once (or never), so there is no real lag to wait out. The budget
// stays generous (300ms against a 2ms poll) so a loaded machine cannot starve
// a poll that would converge; a test that needs the budget to EXPIRE or to be
// larger sets it with setBudget.
func TestMain(m *testing.M) {
	indexedUploadLagTolerance = 300 * time.Millisecond
	indexedUploadPollInterval = 2 * time.Millisecond
	os.Exit(m.Run())
}

func setBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := indexedUploadLagTolerance
	indexedUploadLagTolerance = d
	t.Cleanup(func() { indexedUploadLagTolerance = old })
}

var errEmby500 = errors.New("500 Object reference not set to an instance of an object")

// fakeEmbyPeer models the peer's STATE, not just the calls (#3126). data is the
// authoritative index-addressed backdrop list. With the Emby write rules
// (appendAll=false): idx < len replaces and returns nil; idx == len appends and
// returns nil (a clean 204, measured); idx > len APPENDS and returns the 500.
// appendAll models Jellyfin: every upload appends and returns nil.
type fakeEmbyPeer struct {
	mu        sync.Mutex
	data      [][]byte
	appendAll bool

	dropCalls     map[int]bool // upload call ordinals that error WITHOUT writing
	staleN        int          // GetArtistDetail serves the pre-write count this many times after a landed write
	staleLeft     int
	staleVal      int
	detailErr     func(call int) error // injected GetArtistDetail failure, by call ordinal
	seedStale     int                  // when >0, the FIRST GetArtistDetail reports this count instead
	getCalls      int
	ups           int
	fetches       int   // GetArtistBackdrop calls
	readsAtUpload []int // getCalls when each upload arrived
	lastErr       error // the raw error the last UploadImageAtIndex returned
}

// newFakeEmbyPeer seeds three backdrops, the issue's own repro (count=3).
func newFakeEmbyPeer() *fakeEmbyPeer {
	f := &fakeEmbyPeer{}
	for i := 0; i < 3; i++ {
		f.data = append(f.data, []byte{byte(i)})
	}
	return f
}

func (f *fakeEmbyPeer) UploadImageAtIndex(_ context.Context, _, _ string, idx int, b []byte, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := f.ups
	f.ups++
	f.readsAtUpload = append(f.readsAtUpload, f.getCalls)
	f.lastErr = nil
	if f.dropCalls[call] {
		f.lastErr = errEmby500
		return f.lastErr
	}
	n := len(f.data)
	switch {
	case f.appendAll:
		f.data = append(f.data, append([]byte(nil), b...))
	case idx < n:
		f.data[idx] = append([]byte(nil), b...)
	default:
		f.data = append(f.data, append([]byte(nil), b...))
		if idx > n {
			f.lastErr = errEmby500
		}
		if f.staleN > 0 {
			f.staleLeft, f.staleVal = f.staleN, n
		}
	}
	return f.lastErr
}

func (f *fakeEmbyPeer) GetArtistDetail(_ context.Context, _ string) (*connection.ArtistPlatformState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := f.getCalls
	f.getCalls++
	if f.detailErr != nil {
		if err := f.detailErr(call); err != nil {
			return nil, err
		}
	}
	if call == 0 && f.seedStale > 0 {
		return &connection.ArtistPlatformState{BackdropCount: f.seedStale}, nil
	}
	if f.staleLeft > 0 {
		f.staleLeft--
		return &connection.ArtistPlatformState{BackdropCount: f.staleVal}, nil
	}
	return &connection.ArtistPlatformState{BackdropCount: len(f.data)}, nil
}

func (f *fakeEmbyPeer) GetArtistBackdrop(_ context.Context, _ string, i int) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	if i < 0 || i >= len(f.data) {
		return nil, "", errors.New("404")
	}
	return f.data[i], "image/jpeg", nil
}

func (f *fakeEmbyPeer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.data)
}

func (f *fakeEmbyPeer) reads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls
}

// slot is one snapshot entry: index i, content c (0 = nil data, a slot whose
// local read failed: it spends the index without writing).
type slot struct{ i, c int }

func harness(peer *fakeEmbyPeer, typ string, slots ...slot) (*Publisher, fanartUpload) {
	var snap []fanartSnapshot
	for _, s := range slots {
		sf := fanartSnapshot{path: "fanart.jpg", index: s.i}
		if s.c != 0 {
			sf.data = []byte{byte(0x40 + s.c)}
		}
		snap = append(snap, sf)
	}
	return New(Deps{Logger: silentLogger()}), fanartUpload{
		artist:      &artist.Artist{ID: "a1", Name: "Test Artist"},
		conn:        &connection.Connection{ID: "c1", Name: "Peer", Type: typ},
		pid:         artist.PlatformID{ConnectionID: "c1", PlatformArtistID: "p1"},
		uploader:    peer,
		reader:      peer,
		snapshot:    snap,
		identityIdx: []img.FanartIdentityEntry{},
		notified:    map[string]bool{},
	}
}

// nilSlot3 is the live test's shape: a 3-backdrop peer, slot 3 unreadable
// locally (spends index 3, no write), slot 4 uploaded at idx 4 > len 3.
var nilSlot3 = []slot{{3, 0}, {4, 1}}

// The #3126 AC: an out-of-range upload that 500s but landed is not a failure,
// and the peer holds the bytes exactly once. The count read after the write is
// stale for 3 reads (the issue measured seconds of lag), so a want one too low
// would accept the stale count and read the landed write as absent.
func TestUploadFanartSet_OOBUploadRecoversFromFalseFailure(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.staleN = 3
	p, u := harness(peer, connection.TypeEmby, nilSlot3...)
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 0 {
		t.Fatalf("warnings = %v, want none", w)
	}
	if peer.lastErr == nil {
		t.Fatal("precondition: the peer must have returned its 500 (idx 4 > len 3)")
	}
	if peer.count() != 4 || !bytes.Equal(peer.data[3], []byte{0x41}) {
		t.Fatalf("peer = %v, want 4 backdrops with the upload at index 3", peer.data)
	}
}

func TestUploadFanartSet_RetryDoesNotDuplicate(t *testing.T) {
	retryStable(t, nilSlot3...)
}

// A genuine failure (the write never landed) is still reported.
func TestUploadFanartSet_GenuineFailureStillReported(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.dropCalls = map[int]bool{0: true}
	p, u := harness(peer, connection.TypeEmby, nilSlot3...)
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 1 {
		t.Fatalf("warnings = %v, want exactly 1", w)
	}
	if peer.count() != 3 {
		t.Fatalf("count = %d, want 3", peer.count())
	}
}

// C2: the count alone must not decide. The seed read is stale-low (2, peer
// really has 3), so a genuine IN-RANGE failure at idx 2 looks out of range, and
// the peer's count (3) clears the cheap filter. Only the content check can say
// the bytes are absent.
func TestUploadFanartSet_StaleSeedDoesNotMaskGenuineFailure(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.seedStale = 2
	peer.dropCalls = map[int]bool{0: true}
	p, u := harness(peer, connection.TypeEmby, slot{2, 1})
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 1 {
		t.Fatalf("warnings = %v, want 1: the count cleared the filter but the bytes are absent", w)
	}
}

// C1: Jellyfin appends at every index, so recovery must not be armed at all:
// a genuine failure at idx 4 is reported and the peer is never polled.
func TestUploadFanartSet_JellyfinNeverArmed(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.appendAll = true
	peer.dropCalls = map[int]bool{1: true}
	p, u := harness(peer, connection.TypeJellyfin, slot{3, 1}, slot{4, 2})
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 1 {
		t.Fatalf("warnings = %v, want 1 for the genuinely failed idx 4", w)
	}
	if peer.reads() != 0 {
		t.Fatalf("GetArtistDetail called %d times, want 0: recovery must not be armed on Jellyfin", peer.reads())
	}
}

// M10: an IN-range genuine failure gets exactly one warning and no polling.
func TestUploadFanartSet_InRangeFailureIsNotPolled(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.dropCalls = map[int]bool{0: true}
	p, u := harness(peer, connection.TypeEmby, slot{1, 1})
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 1 {
		t.Fatalf("warnings = %v, want exactly 1", w)
	}
	if peer.reads() != 1 {
		t.Fatalf("reads = %d, want 1 (the seed only): an in-range error must not poll", peer.reads())
	}
}

// M8/M11b: a verify read that errors cannot confirm anything, so the original
// failure is reported even though the write did land, and recovery is then
// disarmed (no second verify read for the next slot): reads = seed + cache load
// (2) + the one failed verify read.
func TestUploadFanartSet_VerifyReadErrorReportsFailureAndDisarms(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.detailErr = func(call int) error {
		if call >= 3 {
			return errors.New("peer unreachable")
		}
		return nil
	}
	p, u := harness(peer, connection.TypeEmby, slot{4, 1}, slot{6, 2})
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 2 {
		t.Fatalf("warnings = %v, want 2", w)
	}
	if peer.reads() != 4 {
		t.Fatalf("reads = %d, want 4 (seed + cache load + the one failed verify read)", peer.reads())
	}
}

// M6: after a RECOVERED 500 (idx 5 lands at 3, count 4) the tracked count
// re-baselines to 4, so the following genuine failure at idx 4 is NOT past the
// count and must not poll. Reads: seed 1 + cache load 2 + verify poll 2.
func TestUploadFanartSet_TrackedCountRebaselinesAfterRecovery(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.dropCalls = map[int]bool{1: true}
	p, u := harness(peer, connection.TypeEmby, slot{5, 1}, slot{4, 2})
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 1 {
		t.Fatalf("warnings = %v, want exactly 1 (idx 4)", w)
	}
	if peer.reads() != 5 {
		t.Fatalf("reads = %d, want 5 (seed + cache load + one verify poll)", peer.reads())
	}
}

// M4: a clean append (idx 3 == count 3) advances the tracked count, so the
// in-range genuine failure that follows (idx 3 again) must not poll.
func TestUploadFanartSet_TrackedCountAdvancesOnCleanAppend(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.dropCalls = map[int]bool{1: true}
	p, u := harness(peer, connection.TypeEmby, slot{3, 1}, slot{3, 2})
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 1 {
		t.Fatalf("warnings = %v, want exactly 1", w)
	}
	if peer.reads() != 1 {
		t.Fatalf("reads = %d, want 1 (the seed only)", peer.reads())
	}
}

// M1 (latency): after the first confirmed not-landed result recovery is off, so
// the second genuine failure must not poll. Judged within ONE run: the read
// count at the second upload equals the read count at the end.
func TestUploadFanartSet_DisarmsAfterFirstNotLanded(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.dropCalls = map[int]bool{0: true, 1: true}
	p, u := harness(peer, connection.TypeEmby, slot{4, 1}, slot{5, 2})
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 2 {
		t.Fatalf("warnings = %v, want 2", w)
	}
	if got, end := peer.readsAtUpload[1], peer.reads(); got != end {
		t.Fatalf("reads at the second upload = %d, at the end = %d: the second failure polled", got, end)
	}
}

// retryStable runs uploadFanartSet twice over the same nil-slot snapshot and
// asserts the second run changes nothing: no duplicate appended, no slot
// overwritten with another slot's bytes.
func retryStable(t *testing.T, slots ...slot) {
	t.Helper()
	peer := newFakeEmbyPeer()
	p, u := harness(peer, connection.TypeEmby, slots...)
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 0 {
		t.Fatalf("run 1 warnings = %v, want none", w)
	}
	after1, fetched := fmt.Sprint(peer.data), peer.fetches
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 0 {
		t.Fatalf("run 2 warnings = %v, want none", w)
	}
	if after2 := fmt.Sprint(peer.data); after2 != after1 {
		t.Fatalf("retry changed the peer: %s -> %s", after1, after2)
	}
	// The peer's backdrops are fetched ONCE per call, not once per slot.
	if got := peer.fetches - fetched; got != len(peer.data) {
		t.Fatalf("run 2 fetched %d backdrops, want %d (one pass)", got, len(peer.data))
	}
}

// One nil gap, two trailing slots: without the guard run 2 overwrote slot 4 with A, then appended B.
func TestUploadFanartSet_OneGapTwoTrailingRetry(t *testing.T) {
	retryStable(t, slot{3, 0}, slot{4, 1}, slot{5, 2})
}

func TestUploadFanartSet_TwoGapRetryNoDuplicate(t *testing.T) {
	retryStable(t, slot{3, 0}, slot{4, 1}, slot{5, 0}, slot{6, 2})
}

// The guard costs backdrop downloads, so it must stay off until a nil slot
// makes the indices misaligned: a contiguous first sync fetches nothing.
func TestUploadFanartSet_SkipGuardCostsNothingOnCleanAppends(t *testing.T) {
	peer := &fakeEmbyPeer{}
	var slots []slot
	for i := 0; i < 10; i++ {
		slots = append(slots, slot{i, i + 1})
	}
	p, u := harness(peer, connection.TypeEmby, slots...)
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 0 || peer.count() != 10 {
		t.Fatalf("warnings = %v, count = %d, want none and 10", w, peer.count())
	}
	if peer.fetches != 0 {
		t.Fatalf("fetched %d backdrops, want 0: the guard ran with no nil slot", peer.fetches)
	}
}

// No nil gap: two slots with the same bytes are both uploaded.
func TestUploadFanartSet_SameBytesTwoSlotsBothUploaded(t *testing.T) {
	peer := newFakeEmbyPeer()
	p, u := harness(peer, connection.TypeEmby, slot{3, 1}, slot{4, 1}, slot{5, 2})
	p.uploadFanartSet(context.Background(), u)
	if peer.count() != 6 {
		t.Fatalf("count = %d, want 6", peer.count())
	}
}

// No nil gap: an in-range replace whose bytes sit elsewhere is still uploaded.
func TestUploadFanartSet_InRangeReplaceNotSkipped(t *testing.T) {
	peer := newFakeEmbyPeer() // [0 1 2]
	p, u := harness(peer, connection.TypeEmby, slot{1, 1})
	u.snapshot[0].data = []byte{2}
	p.uploadFanartSet(context.Background(), u)
	if peer.ups != 1 {
		t.Fatalf("uploads = %d, want 1: the replace was skipped", peer.ups)
	}
}

// I1: the verify check is EXACT. A re-encoded copy of the bytes (perceptually
// near-identical) already on the peer, a stale seed and a genuine failure must
// still produce the warning.
func TestUploadFanartSet_ReencodedCopyDoesNotMaskGenuineFailure(t *testing.T) {
	x := bandJPEG(t, 0x52)
	m, _, err := stdimage.Decode(bytes.NewReader(x))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, &jpeg.Options{Quality: 70}); err != nil {
		t.Fatal(err)
	}
	peer := &fakeEmbyPeer{data: [][]byte{bandJPEG(t, 0x10), bandJPEG(t, 0x11), buf.Bytes()}}
	peer.seedStale = 2
	peer.dropCalls = map[int]bool{0: true}
	p, u := harness(peer, connection.TypeEmby, slot{2, 1})
	u.snapshot[0].data = x
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 1 {
		t.Fatalf("warnings = %v, want 1: a re-encoded copy must not count as the upload", w)
	}
}

// R8: when the seed read fails recovery is disarmed, so an out-of-range error
// is reported at once with no polling (reads = the failed seed only).
func TestUploadFanartSet_SeedReadErrorDisarmsRecovery(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.detailErr = func(int) error { return errors.New("peer unreachable") }
	p, u := harness(peer, connection.TypeEmby, nilSlot3...)
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 1 {
		t.Fatalf("warnings = %v, want 1", w)
	}
	if peer.reads() != 1 {
		t.Fatalf("reads = %d, want 1 (the failed seed only)", peer.reads())
	}
}

// R2/R3: every backdrop is hashed, the one at index 0 included.
func TestPeerContentHashes_CoversEveryIndex(t *testing.T) {
	peer := newFakeEmbyPeer()
	hashes, err := peerContentHashes(context.Background(), peer, "p1", 0, 3)
	if err != nil || len(hashes) != 3 {
		t.Fatalf("hashes = %v, err = %v, want 3 hashes", hashes, err)
	}
	if hashes[0] != img.ContentHash(peer.data[0]) {
		t.Fatal("hash 0 does not match backdrop 0")
	}
}

// M1/M12: the count must hold across two consecutive reads at or above want;
// a count still climbing resets the stability counter.
func TestPollStableCount_RequiresTwoConsecutiveEqualReads(t *testing.T) {
	setBudget(t, 5*time.Second)
	r := &scriptedCounts{seq: []int{3, 3, 4, 5, 5}}
	count, ok, err := pollStableCount(context.Background(), r, "p1", 5)
	if err != nil || !ok || count != 5 {
		t.Fatalf("got (%d, %v, %v), want (5, true, nil)", count, ok, err)
	}
	if r.calls != 5 {
		t.Fatalf("reads = %d, want 5: trusting a single read, or a stability counter that never resets, stops earlier", r.calls)
	}
}

type scriptedCounts struct {
	seq   []int
	calls int
}

func (s *scriptedCounts) GetArtistDetail(context.Context, string) (*connection.ArtistPlatformState, error) {
	i := s.calls
	if i >= len(s.seq) {
		i = len(s.seq) - 1
	}
	s.calls++
	return &connection.ArtistPlatformState{BackdropCount: s.seq[i]}, nil
}

// M7: a count that never holds still must come back ok=false, not landed.
func TestPollStableCount_NeverStableIsNotOK(t *testing.T) {
	seq := make([]int, 200)
	for i := range seq {
		seq[i] = 3 + i
	}
	if _, ok, err := pollStableCount(context.Background(), &scriptedCounts{seq: seq}, "p1", 3); ok || err != nil {
		t.Fatalf("ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

// slowReader delays each backdrop fetch, honoring ctx.
type slowReader struct {
	*fakeEmbyPeer
	d time.Duration
}

func (s slowReader) GetArtistBackdrop(ctx context.Context, id string, i int) ([]byte, string, error) {
	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case <-time.After(s.d):
	}
	return s.fakeEmbyPeer.GetArtistBackdrop(ctx, id, i)
}

// M3: a hanging fetch is cut at its own budget, not left to the whole-scan cap
// (ten budgets).
func TestPeerContentHashes_HangingFetchCutAtPerFetchBudget(t *testing.T) {
	setBudget(t, 50*time.Millisecond)
	start := time.Now()
	_, err := peerContentHashes(context.Background(), slowReader{newFakeEmbyPeer(), time.Hour}, "p1", 0, 3)
	if err == nil || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("err = %v after %v, want an error within ~one 50ms budget", err, time.Since(start))
	}
}

// F1: the peer already holds X (beyond a stale-low seed); an upload of X at an
// out-of-range index fails GENUINELY. The bytes exist, but THIS upload added
// nothing, so the failure must be reported.
func TestUploadFanartSet_ExistingBytesDoNotConfirmGenuineFailure(t *testing.T) {
	peer := newFakeEmbyPeer() // [0 1 2]
	peer.data = append(peer.data, []byte{0x41})
	peer.seedStale = 3
	peer.dropCalls = map[int]bool{0: true}
	p, u := harness(peer, connection.TypeEmby, slot{4, 1})
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 1 {
		t.Fatalf("warnings = %v, want 1: the bytes were already on the peer, nothing new landed", w)
	}
}

// F2: the peer holds 4 (X at index 3) but the seed reads 2; a genuine failure at
// idx 2 disarms recovery, a nil slot at 3 enables the guard, and idx 4 carries
// X. The guard must see X at index 3 (fresh count), not a 2-entry prefix.
func TestUploadFanartSet_GuardHashesTheSettledCountNotTheStaleSeed(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.data = append(peer.data, []byte{0x41})
	peer.seedStale = 2
	peer.dropCalls = map[int]bool{0: true}
	p, u := harness(peer, connection.TypeEmby, slot{2, 2}, slot{3, 0}, slot{4, 1})
	p.uploadFanartSet(context.Background(), u)
	if peer.count() != 4 {
		t.Fatalf("count = %d, want 4: the guard missed X at index 3 and appended a duplicate", peer.count())
	}
}

// F3: N trailing slots after a nil slot each land with a 500. The peer is read
// once, not once per upload: total backdrop fetches <= the final count.
func TestUploadFanartSet_PeerReadOncePerPush(t *testing.T) {
	peer := newFakeEmbyPeer()
	p, u := harness(peer, connection.TypeEmby, slot{3, 0}, slot{4, 1}, slot{5, 2}, slot{6, 3}, slot{7, 4})
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 0 {
		t.Fatalf("warnings = %v, want none", w)
	}
	if peer.count() != 7 {
		t.Fatalf("count = %d, want 7", peer.count())
	}
	t.Logf("fetches = %d for a final count of %d", peer.fetches, peer.count())
	if peer.fetches > peer.count() {
		t.Fatalf("fetches = %d, want <= final count %d (one read of the peer, not one per upload)", peer.fetches, peer.count())
	}
}

// F3b: a clean append is added to the cache, so a later genuine failure of the
// SAME bytes is not mistaken for "newly landed" (the earlier copy sits below the
// cached length). Y lands via a 500 and loads the cache; X appends cleanly at
// idx 4; X at idx 6 then fails for real.
func TestUploadFanartSet_CleanAppendIsCached(t *testing.T) {
	peer := newFakeEmbyPeer()
	peer.dropCalls = map[int]bool{2: true}
	p, u := harness(peer, connection.TypeEmby, slot{4, 2}, slot{4, 1}, slot{6, 1})
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 1 {
		t.Fatalf("warnings = %v, want 1 for the genuinely failed idx 6", w)
	}
}

// An in-range replace is recorded in the cache: a later slot with the replaced
// bytes is skipped by the guard instead of appended.
func TestUploadFanartSet_ReplaceIsCached(t *testing.T) {
	peer := newFakeEmbyPeer() // [0 1 2]
	p, u := harness(peer, connection.TypeEmby, slot{3, 0}, slot{1, 1}, slot{4, 1})
	p.uploadFanartSet(context.Background(), u)
	if peer.count() != 3 {
		t.Fatalf("count = %d, want 3: the replaced bytes were not in the cache, so slot 4 appended a copy", peer.count())
	}
}

// C2: the count grew past the baseline but with DIFFERENT bytes (something else
// appended while our upload genuinely failed), so the failure is reported.
type appendsOther struct{ *fakeEmbyPeer }

func (a appendsOther) UploadImageAtIndex(_ context.Context, _, _ string, _ int, _ []byte, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.data = append(a.data, []byte{0xEE})
	return errEmby500
}

func TestUploadFanartSet_DifferentBytesAppendedDoNotConfirm(t *testing.T) {
	peer := newFakeEmbyPeer()
	p, u := harness(peer, connection.TypeEmby, slot{4, 1})
	u.uploader = appendsOther{peer}
	if w := p.uploadFanartSet(context.Background(), u); len(w) != 1 {
		t.Fatalf("warnings = %v, want 1: the new entry is not our bytes", w)
	}
}
