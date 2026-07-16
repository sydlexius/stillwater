package publish

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// TestNewBackdropPruneClient_SupportedTypes is primarily a compile-time proof
// that *emby.Client and *jellyfin.Client satisfy backdropPruneClient (i.e.
// that both clients implement DeleteImageAtIndex, GetArtistDetail, and
// GetArtistBackdrop). If Tasks 1/2 are incomplete, this file fails to build.
func TestNewBackdropPruneClient_SupportedTypes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, ct := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		conn := &connection.Connection{Type: ct, URL: "http://x", APIKey: "k"}
		if newBackdropPruneClient(conn, logger) == nil {
			t.Errorf("type %s: got nil client", ct)
		}
	}
	if newBackdropPruneClient(&connection.Connection{Type: "lidarr"}, logger) != nil {
		t.Error("unsupported type: want nil")
	}
}

// fakeBackdropClient is a fake backdropPruneClient: backdrops holds the
// artist's backdrop bytes by index, deleted records DeleteImageAtIndex calls
// in order, failAt (when >= 0) makes GetArtistBackdrop error at that index
// (exercising the fetch-failure-skips-connection path), and failDeleteAt
// (when >= 0) makes DeleteImageAtIndex error at that index without recording
// it in deleted (exercising the delete-failure-stops-connection path).
// mutateAtVerify, when non-nil, maps an index to bytes that GetArtistBackdrop
// returns starting on that index's SECOND fetch onward -- i.e. detection
// (the first fetch, during backdropRedundantIndices) still sees the
// original backdrops bytes, but the prune loop's re-verify fetch
// (immediately before delete) sees the mutated bytes instead, simulating a
// concurrent platform write between detection and delete (the TOCTOU
// window this guard closes).
// failAtVerify, when non-nil, maps an index to true to make GetArtistBackdrop
// error starting on that index's SECOND fetch onward -- i.e. detection (the
// first fetch) still succeeds, but the prune loop's immediate-pre-delete
// re-verify fetch errors instead, simulating a platform read failure (rather
// than a content change) discovered only at re-verify time. This is distinct
// from failAt, which fails EVERY fetch of an index including detection (so it
// cannot reach the re-verify branch at all).
type fakeBackdropClient struct {
	backdrops      [][]byte
	deleted        []int
	failAt         int
	failDeleteAt   int
	mutateAtVerify map[int][]byte
	failAtVerify   map[int]bool
	fetchCounts    map[int]int
}

func (f *fakeBackdropClient) GetArtistDetail(_ context.Context, _ string) (*connection.ArtistPlatformState, error) {
	return &connection.ArtistPlatformState{BackdropCount: len(f.backdrops)}, nil
}

func (f *fakeBackdropClient) GetArtistBackdrop(_ context.Context, _ string, i int) ([]byte, string, error) {
	if f.failAt >= 0 && i == f.failAt {
		return nil, "", fmt.Errorf("boom at %d", i)
	}
	if f.fetchCounts == nil {
		f.fetchCounts = make(map[int]int)
	}
	f.fetchCounts[i]++
	if f.fetchCounts[i] > 1 {
		if f.failAtVerify[i] {
			return nil, "", fmt.Errorf("re-verify boom at %d", i)
		}
		if mutated, ok := f.mutateAtVerify[i]; ok {
			return mutated, "image/jpeg", nil
		}
	}
	return f.backdrops[i], "image/jpeg", nil
}

func (f *fakeBackdropClient) DeleteImageAtIndex(_ context.Context, _ string, _ string, i int) error {
	if f.failDeleteAt >= 0 && i == f.failDeleteAt {
		return fmt.Errorf("delete boom at %d", i)
	}
	f.deleted = append(f.deleted, i)
	return nil
}

// newTestPublisherWithOneArtistOnePlatform builds a *Publisher wired with one
// artist ("a1") mapped to one enabled, healthy Emby connection ("c-emby"),
// and swaps backdropPruneClientFactory to hand every client-construction call
// the given fake (t.Cleanup restores the real factory).
func newTestPublisherWithOneArtistOnePlatform(t *testing.T, fake backdropPruneClient) *Publisher {
	t.Helper()

	prevFactory := backdropPruneClientFactory
	backdropPruneClientFactory = func(_ *connection.Connection, _ *slog.Logger) backdropPruneClient {
		return fake
	}
	t.Cleanup(func() { backdropPruneClientFactory = prevFactory })

	artistLister := &fakePlatformLister{
		ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		},
		artists: []artist.Artist{
			{ID: "a1", Name: "Test Artist"},
		},
	}
	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-emby": {
			ID:      "c-emby",
			Name:    "emby",
			Type:    connection.TypeEmby,
			Enabled: true,
			Status:  "ok",
			Emby:    &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true},
		},
	}}

	return New(Deps{
		ArtistService:     artistLister,
		ArtistLister:      artistLister,
		ConnectionService: conns,
		Logger:            silentLogger(),
	})
}

// newTestPublisherWithOneArtistOnePlatform_NoImageWrite mirrors
// newTestPublisherWithOneArtistOnePlatform but the connection's
// FeatureImageWrite is false, so PrunePlatformBackdropDuplicates must skip it
// (the read-only scan has no such gate; prune does).
func newTestPublisherWithOneArtistOnePlatform_NoImageWrite(t *testing.T, fake backdropPruneClient) *Publisher {
	t.Helper()

	prevFactory := backdropPruneClientFactory
	backdropPruneClientFactory = func(_ *connection.Connection, _ *slog.Logger) backdropPruneClient {
		return fake
	}
	t.Cleanup(func() { backdropPruneClientFactory = prevFactory })

	artistLister := &fakePlatformLister{
		ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		},
		artists: []artist.Artist{
			{ID: "a1", Name: "Test Artist"},
		},
	}
	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-emby": {
			ID:      "c-emby",
			Name:    "emby",
			Type:    connection.TypeEmby,
			Enabled: true,
			Status:  "ok",
			Emby:    &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: false},
		},
	}}

	return New(Deps{
		ArtistService:     artistLister,
		ArtistLister:      artistLister,
		ConnectionService: conns,
		Logger:            silentLogger(),
	})
}

// multiArtistPlatformLister wraps fakePlatformLister but filters
// GetPlatformIDs by the requested artistID. fakePlatformLister.GetPlatformIDs
// ignores its artistID parameter and always returns the whole f.ids list,
// which is harmless for every existing single-artist fixture (there is only
// one artist to attribute ids to) but wrong for a multi-artist fixture where
// different artists carry different platform mappings: without this filter,
// processing artist "a1" would also pick up artist "a2"'s connection (and
// vice versa), corrupting both the per-artist failure attribution and the
// delete counts this test exists to verify. Every other method delegates to
// the embedded fake unchanged.
type multiArtistPlatformLister struct {
	*fakePlatformLister
}

func (f *multiArtistPlatformLister) GetPlatformIDs(_ context.Context, artistID string) ([]artist.PlatformID, error) {
	var out []artist.PlatformID
	for _, pid := range f.ids {
		if pid.ArtistID == artistID {
			out = append(out, pid)
		}
	}
	return out, nil
}

// TestPrunePlatformBackdropDuplicates_ContinuesAfterPerArtistFailure guards
// against a regression that turns the per-artist/per-connection `continue` in
// the prune loop into a batch-aborting `return`: every existing prune fixture
// wires exactly one artist to one connection, so a `return` in place of that
// `continue` would still pass all of them (there is nothing after the first
// artist for it to skip). Here artist a1's only connection fails backdrop
// detection (a fetch error) while artist a2, on a separate connection, has
// real redundant backdrops. Both must be processed independently: a1's
// failure is recorded AND a2's redundant backdrops are still deleted -- if a
// `return` replaced the `continue`, a2 would never run and BackdropsRemoved
// would be 0.
func TestPrunePlatformBackdropDuplicates_ContinuesAfterPerArtistFailure(t *testing.T) {
	prevFactory := backdropPruneClientFactory
	t.Cleanup(func() { backdropPruneClientFactory = prevFactory })

	dup, distinct := []byte("AAA"), []byte("BBB")
	// a1's client: 2 backdrops, but the very first fetch errors -> detection
	// fails for the whole connection.
	failingClient := &fakeBackdropClient{backdrops: [][]byte{dup, dup}, failAt: 0, failDeleteAt: -1}
	// a2's client: 3 backdrops, indices 0 and 1 byte-identical -> redundant=[1],
	// deletable cleanly.
	okClient := &fakeBackdropClient{backdrops: [][]byte{dup, dup, distinct}, failAt: -1, failDeleteAt: -1}

	clients := map[string]backdropPruneClient{
		"c-fail": failingClient,
		"c-ok":   okClient,
	}
	backdropPruneClientFactory = func(conn *connection.Connection, _ *slog.Logger) backdropPruneClient {
		return clients[conn.ID]
	}

	inner := &fakePlatformLister{
		ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-fail", PlatformArtistID: "p1"},
			{ArtistID: "a2", ConnectionID: "c-ok", PlatformArtistID: "p2"},
		},
		artists: []artist.Artist{
			{ID: "a1", Name: "Artist One"},
			{ID: "a2", Name: "Artist Two"},
		},
	}
	artistLister := &multiArtistPlatformLister{fakePlatformLister: inner}
	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-fail": {
			ID: "c-fail", Name: "emby-fail", Type: connection.TypeEmby, Enabled: true, Status: "ok",
			Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true},
		},
		"c-ok": {
			ID: "c-ok", Name: "emby-ok", Type: connection.TypeEmby, Enabled: true, Status: "ok",
			Emby: &connection.EmbyConfig{PlatformUserID: "u2", FeatureImageWrite: true},
		},
	}}

	p := New(Deps{
		ArtistService:     artistLister,
		ArtistLister:      inner,
		ConnectionService: conns,
		Logger:            silentLogger(),
	})

	res, err := p.PrunePlatformBackdropDuplicates(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("Failures = %d, want 1 (a1's detection failure); got %+v", len(res.Failures), res.Failures)
	}
	if res.Failures[0].ArtistID != "a1" {
		t.Fatalf("Failures[0].ArtistID = %q, want a1", res.Failures[0].ArtistID)
	}
	if res.BackdropsRemoved != 1 {
		t.Fatalf("BackdropsRemoved = %d, want 1 (a2's redundant backdrop, proving the batch did not abort on a1's failure)", res.BackdropsRemoved)
	}
	if want := []int{1}; !reflect.DeepEqual(okClient.deleted, want) {
		t.Fatalf("okClient.deleted = %v, want %v", okClient.deleted, want)
	}
	if len(failingClient.deleted) != 0 {
		t.Fatalf("failingClient.deleted = %v, want none (detection failed before any delete)", failingClient.deleted)
	}
}

// TestScanPlatformBackdropDuplicates_CountsRedundant: 4 backdrops where 0, 1,
// 2 are byte-identical and 3 is distinct -> Redundant = 2 (keep index 0 and
// index 3, drop the two other copies).
func TestScanPlatformBackdropDuplicates_CountsRedundant(t *testing.T) {
	dup, distinct := []byte("AAA"), []byte("BBB")
	fake := &fakeBackdropClient{backdrops: [][]byte{dup, dup, dup, distinct}, failAt: -1, failDeleteAt: -1}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)

	report, err := p.ScanPlatformBackdropDuplicates(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.RedundantBackdrops != 2 {
		t.Fatalf("RedundantBackdrops = %d, want 2", report.RedundantBackdrops)
	}
	if report.ArtistsAffected != 1 || report.ConnectionsAffected != 1 {
		t.Fatalf("affected = artists %d conns %d, want 1/1", report.ArtistsAffected, report.ConnectionsAffected)
	}
	if len(report.PerArtist) != 1 || report.PerArtist[0].Redundant != 2 || report.PerArtist[0].Backdrops != 4 {
		t.Fatalf("PerArtist = %+v", report.PerArtist)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("scan issued %d deletes, want 0: a scan must never delete", len(fake.deleted))
	}
}

// TestScanPlatformBackdropDuplicates_ExactOnly: two visually-similar but
// byte-different backdrops are NOT grouped as redundant.
func TestScanPlatformBackdropDuplicates_ExactOnly(t *testing.T) {
	fake := &fakeBackdropClient{backdrops: [][]byte{[]byte("AAAA"), []byte("AAAB")}, failAt: -1, failDeleteAt: -1}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)

	report, err := p.ScanPlatformBackdropDuplicates(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.RedundantBackdrops != 0 {
		t.Fatalf("RedundantBackdrops = %d, want 0 (byte-different)", report.RedundantBackdrops)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("scan issued %d deletes, want 0: a scan must never delete", len(fake.deleted))
	}
}

// TestScanPlatformBackdropDuplicates_FetchFailureSkipsConnection: a backdrop
// fetch error partway through an artist/connection's reads aborts that whole
// connection rather than reporting a partial/blind redundant count. Three
// byte-identical backdrops with failAt=1 (the second read fails) must yield
// ScanErrors=1, RedundantBackdrops=0, and no PerArtist entry -- never a
// partial count from the one backdrop that was read before the failure.
func TestScanPlatformBackdropDuplicates_FetchFailureSkipsConnection(t *testing.T) {
	dup := []byte("AAA")
	fake := &fakeBackdropClient{backdrops: [][]byte{dup, dup, dup}, failAt: 1, failDeleteAt: -1}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)

	report, err := p.ScanPlatformBackdropDuplicates(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.ScanErrors != 1 {
		t.Fatalf("ScanErrors = %d, want 1", report.ScanErrors)
	}
	if report.RedundantBackdrops != 0 {
		t.Fatalf("RedundantBackdrops = %d, want 0 (whole connection skipped, not partially counted)", report.RedundantBackdrops)
	}
	if len(report.PerArtist) != 0 {
		t.Fatalf("PerArtist = %+v, want empty (skipped connection must not appear)", report.PerArtist)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("scan issued %d deletes, want 0: a scan must never delete", len(fake.deleted))
	}
}

// TestPrunePlatformBackdropDuplicates_DeletesHighIndexFirst: 0,1,2 identical +
// 3 distinct -> delete indices [2,1] in that order (descending), keep 0 and 3.
func TestPrunePlatformBackdropDuplicates_DeletesHighIndexFirst(t *testing.T) {
	dup, distinct := []byte("AAA"), []byte("BBB")
	fake := &fakeBackdropClient{backdrops: [][]byte{dup, dup, dup, distinct}, failAt: -1, failDeleteAt: -1}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake) // connection has ImageWrite enabled
	res, err := p.PrunePlatformBackdropDuplicates(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.BackdropsRemoved != 2 {
		t.Fatalf("BackdropsRemoved = %d, want 2", res.BackdropsRemoved)
	}
	if want := []int{2, 1}; !reflect.DeepEqual(fake.deleted, want) {
		t.Fatalf("delete order = %v, want %v (high-index-first)", fake.deleted, want)
	}
}

// TestPrunePlatformBackdropDuplicates_SkipsWhenContentChangedSinceDetection
// guards the TOCTOU re-verify gate: detection sees 0,1,2 byte-identical + 3
// distinct (redundant = [2,1] descending). Before the delete of index 1, a
// concurrent platform write is simulated by mutating what index 1 returns on
// its second fetch (the prune loop's immediate-pre-delete re-verify). Index
// 2's content is unchanged, so it re-verifies clean and is deleted; index 1's
// content changed, so it must be SKIPPED (never deleted) and counted in
// SkippedChanged, while the connection continues (a skip performs no delete,
// so lower indices are unaffected -- this is why the guard uses `continue`
// rather than `break` on a skip, unlike the delete-error path).
func TestPrunePlatformBackdropDuplicates_SkipsWhenContentChangedSinceDetection(t *testing.T) {
	dup, distinct, mutated := []byte("AAA"), []byte("BBB"), []byte("CCC")
	fake := &fakeBackdropClient{
		backdrops:      [][]byte{dup, dup, dup, distinct},
		failAt:         -1,
		failDeleteAt:   -1,
		mutateAtVerify: map[int][]byte{1: mutated},
	}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)

	res, err := p.PrunePlatformBackdropDuplicates(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := []int{2}; !reflect.DeepEqual(fake.deleted, want) {
		t.Fatalf("deleted = %v, want %v (index 2 re-verifies clean; index 1 changed and must be skipped)", fake.deleted, want)
	}
	if res.SkippedChanged != 1 {
		t.Fatalf("SkippedChanged = %d, want 1", res.SkippedChanged)
	}
	if res.BackdropsRemoved != 1 {
		t.Fatalf("BackdropsRemoved = %d, want 1", res.BackdropsRemoved)
	}
}

// TestPrunePlatformBackdropDuplicates_SkipsWhenReVerifyFetchFails guards the
// OTHER re-verify skip branch: the re-fetch itself erroring (as opposed to
// succeeding but returning changed content, covered above). Fixture: indices
// 0,1,2,3 byte-identical + 4 distinct -> detection sees redundant=[3,2,1]
// descending. failAtVerify[2] fires only on index 2's SECOND fetch, so
// detection (index 2's first fetch) still succeeds and index 2 is still
// counted redundant; only the prune loop's immediate-pre-delete re-verify of
// index 2 errors. This shape (a THIRD redundant index, 1, still pending
// after the failure) is required to distinguish the fetch-error skip's
// `continue` from a `break`: with only one index left after the failure (as
// in the two-redundant-index shape used elsewhere), continue and break both
// leave nothing further to delete and are indistinguishable. Here, deleting
// 3, skipping 2, and continuing to delete 1 (deleted == [3, 1]) proves
// `continue`; a `break` in that branch would stop after skipping 2 and leave
// deleted == [3].
func TestPrunePlatformBackdropDuplicates_SkipsWhenReVerifyFetchFails(t *testing.T) {
	dup, distinct := []byte("AAA"), []byte("BBB")
	fake := &fakeBackdropClient{
		backdrops:    [][]byte{dup, dup, dup, dup, distinct},
		failAt:       -1,
		failDeleteAt: -1,
		failAtVerify: map[int]bool{2: true},
	}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)

	res, err := p.PrunePlatformBackdropDuplicates(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := []int{3, 1}; !reflect.DeepEqual(fake.deleted, want) {
		t.Fatalf("deleted = %v, want %v (index 3 deletes, index 2's re-verify fetch fails and is skipped via continue, index 1 still deletes)", fake.deleted, want)
	}
	if res.SkippedChanged != 1 {
		t.Fatalf("SkippedChanged = %d, want 1", res.SkippedChanged)
	}
	if res.BackdropsRemoved != 2 {
		t.Fatalf("BackdropsRemoved = %d, want 2", res.BackdropsRemoved)
	}
}

// TestPrunePlatformBackdropDuplicates_SkipsWhenNoImageWrite: connection without
// FeatureImageWrite is not pruned.
func TestPrunePlatformBackdropDuplicates_SkipsWhenNoImageWrite(t *testing.T) {
	dup := []byte("AAA")
	fake := &fakeBackdropClient{backdrops: [][]byte{dup, dup}, failAt: -1, failDeleteAt: -1}
	p := newTestPublisherWithOneArtistOnePlatform_NoImageWrite(t, fake)
	res, err := p.PrunePlatformBackdropDuplicates(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.BackdropsRemoved != 0 || len(fake.deleted) != 0 {
		t.Fatalf("pruned without image-write: removed=%d deleted=%v", res.BackdropsRemoved, fake.deleted)
	}
}

// TestPrunePlatformBackdropDuplicates_FetchFailureSkipsConnection: a mid-fetch
// error skips the connection (no deletes) and is recorded as a failure.
func TestPrunePlatformBackdropDuplicates_FetchFailureSkipsConnection(t *testing.T) {
	dup := []byte("AAA")
	fake := &fakeBackdropClient{backdrops: [][]byte{dup, dup, dup}, failAt: 1, failDeleteAt: -1} // fetch of index 1 errors
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)
	res, err := p.PrunePlatformBackdropDuplicates(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted despite fetch failure: %v", fake.deleted)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("Failures = %d, want 1", len(res.Failures))
	}
}

// TestPrunePlatformBackdropDuplicates_DeleteFailureStopsConnection: 4
// byte-identical backdrops (0-3) plus 1 distinct (4) -> redundant indices
// [3, 2, 1] descending, keeping 0 and 4. failDeleteAt=2 fails the SECOND
// delete in that sequence (index 2), leaving index 1 still pending in the
// loop. The delete of index 3 must succeed, the delete of index 2 must fail
// and the loop must then break -- never attempting index 1, whose backing
// slot may have shifted after the failed delete. This distinguishes the
// break from a bare continue: with only two redundant indices left after a
// failure (as in the fetch-failure fixture above) there is nothing further
// to iterate to, so that shape can't tell break and continue apart; this
// fixture has a THIRD index still pending after the failure, so it does.
func TestPrunePlatformBackdropDuplicates_DeleteFailureStopsConnection(t *testing.T) {
	dup, distinct := []byte("AAA"), []byte("BBB")
	fake := &fakeBackdropClient{backdrops: [][]byte{dup, dup, dup, dup, distinct}, failAt: -1, failDeleteAt: 2} // second delete (index 2) fails; index 1 still pending
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)
	res, err := p.PrunePlatformBackdropDuplicates(context.Background())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := []int{3}; !reflect.DeepEqual(fake.deleted, want) {
		t.Fatalf("deleted = %v, want %v (delete failure must stop the connection, not skip ahead to index 1)", fake.deleted, want)
	}
	if res.BackdropsRemoved != 1 {
		t.Fatalf("BackdropsRemoved = %d, want 1", res.BackdropsRemoved)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("Failures = %d, want 1", len(res.Failures))
	}
}
