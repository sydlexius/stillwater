package publish

import (
	"bytes"
	"context"
	"fmt"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/image"
)

// bandJPEG builds a 64x64 image whose pixels are a deterministic pseudo-random
// grayscale field seeded by `seed`, then JPEG-encodes it. Distinct seeds give
// images with distinct 2D frequency content and therefore distinct perceptual
// hashes (unlike a solid fill or a single horizontal band, both of which lack
// the cross-frequency structure a phash keys on and collide). The SAME seed
// yields byte-identical output, which is what lets a test plant "the polluted
// picture" and match it back at similarity 1.0. The name is kept generic; the
// only property callers rely on is seed -> distinct-but-reproducible image.
func bandJPEG(t *testing.T, seed int) []byte {
	t.Helper()
	const w, h = 64, 64
	// A tiny LCG gives a reproducible pixel field per seed without importing a
	// randomness source that the workflow harness forbids in scripts (and to
	// keep the fixture self-contained and stable across runs).
	state := uint32(seed)*2654435761 + 1
	next := func() uint8 {
		state = state*1664525 + 1013904223
		return uint8(state >> 24)
	}
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := next()
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding fixture jpeg: %v", err)
	}
	return buf.Bytes()
}

func phashOf(t *testing.T, data []byte) uint64 {
	t.Helper()
	h, err := image.PerceptualHash(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("hashing fixture: %v", err)
	}
	return h
}

// assertDistinct fails if any two fixtures are within tolerance of each other.
// Guards against a vacuous test: if the "bystander" backdrops secretly matched
// the polluted one, a "delete only the match" assertion would pass for the
// wrong reason.
func assertDistinct(t *testing.T, fixtures ...[]byte) {
	t.Helper()
	hashes := make([]uint64, len(fixtures))
	for i, f := range fixtures {
		hashes[i] = phashOf(t, f)
	}
	for i := 0; i < len(hashes); i++ {
		for j := i + 1; j < len(hashes); j++ {
			if sim := image.Similarity(hashes[i], hashes[j]); sim >= testTolerance {
				t.Fatalf("fixtures %d and %d are not distinct at tolerance %.2f (similarity %.3f); test would be vacuous", i, j, testTolerance, sim)
			}
		}
	}
}

// fakePhashClient is an in-memory phashPlatformClient. backdrops holds the
// item's backdrop bytes by index; DeleteImageAtIndex removes one and RE-INDEXES
// (as Emby/Jellyfin do); UploadImage APPENDS. The *_ignoreWrites flags simulate
// the peers' documented silent-ignore behavior: the call returns nil but the
// artifact is left unchanged -- the exact failure the verify-by-refetch exists
// to catch.
type fakePhashClient struct {
	backdrops     [][]byte
	deletes       []int
	uploads       int
	ignoreDeletes bool // DeleteImageAtIndex returns nil but does not remove
	ignoreUploads bool // UploadImage returns nil but does not append

	// deleteErr / uploadErr, when non-nil, make the corresponding call fail
	// hard (as opposed to ignoreDeletes/ignoreUploads' silent-2xx-but-nothing-
	// happened simulation). Checked before any mutation, so a forced error
	// never leaks a partial delete/upload -- the property the orchestration
	// wrappers' non-fatal-per-connection handling depends on.
	deleteErr error
	uploadErr error

	// deleteCalls counts every DeleteImageAtIndex invocation (1-indexed against
	// deleteErrAtCall).
	deleteCalls int
	// deleteErrAtCall selects WHICH delete fails when deleteErr is set: 0 (the
	// default) fails EVERY call before it mutates -- the original
	// checked-before-any-mutation behavior. N > 0 lets the first N-1 deletes
	// succeed and MUTATE, then fails the Nth, so a test can exercise a partial
	// multi-delete where earlier slots are really gone before a later delete
	// errors.
	deleteErrAtCall int

	// indexUploads records every UploadImageAtIndex call (#3125 F3).
	indexUploads []indexUpload
}

func (f *fakePhashClient) GetArtistDetail(_ context.Context, _ string) (*connection.ArtistPlatformState, error) {
	return &connection.ArtistPlatformState{BackdropCount: len(f.backdrops)}, nil
}

func (f *fakePhashClient) GetArtistBackdrop(_ context.Context, _ string, i int) ([]byte, string, error) {
	if i < 0 || i >= len(f.backdrops) {
		return nil, "", context.Canceled // out of range: shape mismatch, surface as an error
	}
	return f.backdrops[i], "image/jpeg", nil
}

func (f *fakePhashClient) DeleteImageAtIndex(_ context.Context, _ string, _ string, i int) error {
	f.deleteCalls++
	if f.deleteErr != nil && (f.deleteErrAtCall == 0 || f.deleteCalls == f.deleteErrAtCall) {
		return f.deleteErr // hard failure on this call: nothing recorded, nothing mutated
	}
	f.deletes = append(f.deletes, i)
	if f.ignoreDeletes {
		return nil // accepted, but the artifact stays -- silent ignore
	}
	if i >= 0 && i < len(f.backdrops) {
		f.backdrops = append(f.backdrops[:i], f.backdrops[i+1:]...)
	}
	return nil
}

func (f *fakePhashClient) UploadImage(_ context.Context, _ string, _ string, data []byte, _ string) error {
	if f.uploadErr != nil {
		return f.uploadErr // hard failure: nothing recorded, nothing mutated
	}
	f.uploads++
	if f.ignoreUploads {
		return nil // accepted, but nothing stored -- silent ignore
	}
	f.backdrops = append(f.backdrops, data)
	return nil
}

// indexUploads records every UploadImageAtIndex call (#3125 F3 tests): the
// index it was asked to write, so a test can assert WHICH slot was
// targeted, not merely that some upload happened -- the whole point of F3
// is that the wrong index destroys a bystander.
type indexUpload struct {
	index int
	data  []byte
}

// UploadImageAtIndex gives fakePhashClient Emby's measured in-place-replace
// semantics (#3125): writing an in-range index REPLACES that slot's content;
// writing exactly len(backdrops) (one past the end) APPENDS a new slot. Any
// other out-of-range index is refused, matching the real client's own
// negative-index guard (mediabrowser.UploadImageAtIndexRaw) extended to the
// upper bound a real peer would also reject.
func (f *fakePhashClient) UploadImageAtIndex(_ context.Context, _ string, _ string, index int, data []byte, _ string) error {
	if f.uploadErr != nil {
		return f.uploadErr
	}
	f.indexUploads = append(f.indexUploads, indexUpload{index: index, data: data})
	if f.ignoreUploads {
		return nil
	}
	switch {
	case index >= 0 && index < len(f.backdrops):
		f.backdrops[index] = data // in-place replace
	case index == len(f.backdrops):
		f.backdrops = append(f.backdrops, data) // append at the natural next slot
	default:
		return fmt.Errorf("index %d out of range for %d backdrops", index, len(f.backdrops))
	}
	return nil
}

// hasMatch reports whether any stored backdrop is within tolerance of want.
// Asserting on THIS -- the on-disk/on-platform artifact -- not on a returned
// counter, is the point: -race and counters are blind to a write the platform
// dropped.
func (f *fakePhashClient) hasMatch(t *testing.T, want uint64) bool {
	t.Helper()
	for _, b := range f.backdrops {
		if image.Similarity(want, phashOf(t, b)) >= testTolerance {
			return true
		}
	}
	return false
}

const testTolerance = 0.85

// --- deletePollutedBackdrops ------------------------------------------------

func TestDeletePollutedBackdrops_RemovesMatchAndVerifiesGone(t *testing.T) {
	polluted := bandJPEG(t, 32)
	b0 := bandJPEG(t, 8)
	b2 := bandJPEG(t, 56)
	assertDistinct(t, polluted, b0, b2)

	// Two polluted copies (indices 1 and 3) separated by a bystander (index 2).
	// A single match cannot tell descending deletion from ascending; two
	// matches with a bystander between them can, and prove the descending order
	// keeps un-deleted ordinals from shifting under the deletes.
	f := &fakePhashClient{backdrops: [][]byte{b0, polluted, b2, polluted}}
	want := phashOf(t, polluted)

	deleted, err := deletePollutedBackdrops(context.Background(), f, "p1", want, testTolerance)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted count: want 2, got %d", deleted)
	}
	// The delete order must be exactly DESCENDING [3, 1]: deleting the higher
	// ordinal first means the lower one it has not reached yet does not shift.
	if len(f.deletes) != 2 || f.deletes[0] != 3 || f.deletes[1] != 1 {
		t.Errorf("expected descending delete order [3 1], got %v", f.deletes)
	}
	// ARTIFACT assertions, not counters: no polluted copy survives, and BOTH
	// bystanders remain intact (fail-closed: a non-matching slot is never
	// touched), including the one that sat between the two matches.
	if f.hasMatch(t, want) {
		t.Error("polluted backdrop still present after delete")
	}
	if len(f.backdrops) != 2 || !bytes.Equal(f.backdrops[0], b0) || !bytes.Equal(f.backdrops[1], b2) {
		t.Errorf("bystanders not preserved intact: got %d backdrops", len(f.backdrops))
	}
}

func TestDeletePollutedBackdrops_NoMatchIsIdempotentNoop(t *testing.T) {
	b0 := bandJPEG(t, 8)
	b2 := bandJPEG(t, 56)
	polluted := bandJPEG(t, 32)
	assertDistinct(t, polluted, b0, b2)

	f := &fakePhashClient{backdrops: [][]byte{b0, b2}}
	deleted, err := deletePollutedBackdrops(context.Background(), f, "p1", phashOf(t, polluted), testTolerance)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 0 || len(f.deletes) != 0 {
		t.Errorf("want no-op, got deleted=%d deletes=%v", deleted, f.deletes)
	}
	if len(f.backdrops) != 2 {
		t.Errorf("bystanders must be untouched, got %d", len(f.backdrops))
	}
}

// TestDeletePollutedBackdrops_VerifyCatchesSilentIgnore is the guard proof for
// verify-by-refetch. With ignoreDeletes the peer returns 2xx but keeps the
// image (the documented silent-ignore). deletePollutedBackdrops MUST return an
// error rather than report success.
//
// Revert-and-rerun proof (measured): temporarily deleting the post-delete
// re-fetch/verify block in deletePollutedBackdrops makes this test FAIL (the
// function returns deleted=1, nil despite the polluted image surviving);
// restoring the verify block makes it PASS. See the report for the measured
// RED/GREEN.
func TestDeletePollutedBackdrops_VerifyCatchesSilentIgnore(t *testing.T) {
	polluted := bandJPEG(t, 32)
	b0 := bandJPEG(t, 8)
	assertDistinct(t, polluted, b0)

	f := &fakePhashClient{backdrops: [][]byte{b0, polluted}, ignoreDeletes: true}
	want := phashOf(t, polluted)

	_, err := deletePollutedBackdrops(context.Background(), f, "p1", want, testTolerance)
	if err == nil {
		t.Fatal("want error when the platform ignored the delete, got nil")
	}
	if !f.hasMatch(t, want) {
		t.Error("precondition: the polluted backdrop should still be present (silent ignore)")
	}
}

func TestDeletePollutedBackdrops_RejectsBadTolerance(t *testing.T) {
	f := &fakePhashClient{backdrops: [][]byte{bandJPEG(t, 32)}}
	// NaN is the load-bearing case: every IEEE-754 compare against NaN is false,
	// so `t <= 0 || t > 1` ADMITS it, and a NaN cutoff makes every slot's
	// Similarity >= tolerance false -- a silent match-nothing over an
	// un-remediated library. validPHashTolerance's math.IsNaN guard must reject
	// it here before any platform IO.
	for _, tol := range []float64{0, -0.1, 1.5, math.NaN()} {
		if _, err := deletePollutedBackdrops(context.Background(), f, "p1", phashOf(t, bandJPEG(t, 32)), tol); err == nil {
			t.Errorf("tolerance %v: want error, got nil", tol)
		}
	}
	if len(f.deletes) != 0 {
		t.Errorf("no delete may happen on a rejected tolerance, got %v", f.deletes)
	}
}

// --- restoreBackdrop --------------------------------------------------------

func TestRestoreBackdrop_AppendsWhenAbsentAndVerifiesPresent(t *testing.T) {
	polluted := bandJPEG(t, 32)
	b0 := bandJPEG(t, 8)
	assertDistinct(t, polluted, b0)

	f := &fakePhashClient{backdrops: [][]byte{b0}}
	appended, err := restoreBackdrop(context.Background(), f, "p1", polluted, testTolerance)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !appended {
		t.Error("want appended=true")
	}
	// ARTIFACT: the picture is now on the platform, and the bystander is intact.
	if !f.hasMatch(t, phashOf(t, polluted)) {
		t.Error("restored backdrop not present on platform after restore")
	}
	if len(f.backdrops) != 2 || !bytes.Equal(f.backdrops[0], b0) {
		t.Errorf("append clobbered a bystander: %d backdrops", len(f.backdrops))
	}
}

func TestRestoreBackdrop_AlreadyPresentIsIdempotentNoop(t *testing.T) {
	polluted := bandJPEG(t, 32)
	f := &fakePhashClient{backdrops: [][]byte{polluted}}
	appended, err := restoreBackdrop(context.Background(), f, "p1", polluted, testTolerance)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if appended {
		t.Error("want appended=false when already present")
	}
	if f.uploads != 0 {
		t.Errorf("no upload may happen when already present, got %d", f.uploads)
	}
	if len(f.backdrops) != 1 {
		t.Errorf("already-present restore must not add a duplicate, got %d", len(f.backdrops))
	}
}

// TestRestoreBackdrop_VerifyCatchesSilentIgnore is the guard proof for the
// restore direction. With ignoreUploads the peer returns 2xx but stores
// nothing; restoreBackdrop MUST return an error, not a false success.
//
// Revert-and-rerun proof (measured): deleting the post-upload verify block
// makes this test FAIL (returns appended=true, nil while nothing was stored);
// restoring it makes it PASS.
func TestRestoreBackdrop_VerifyCatchesSilentIgnore(t *testing.T) {
	polluted := bandJPEG(t, 32)
	f := &fakePhashClient{backdrops: [][]byte{bandJPEG(t, 8)}, ignoreUploads: true}
	_, err := restoreBackdrop(context.Background(), f, "p1", polluted, testTolerance)
	if err == nil {
		t.Fatal("want error when the platform ignored the upload, got nil")
	}
	if f.hasMatch(t, phashOf(t, polluted)) {
		t.Error("precondition: nothing should have been stored (silent ignore)")
	}
}

func TestRestoreBackdrop_RefusesEmptyData(t *testing.T) {
	f := &fakePhashClient{}
	if _, err := restoreBackdrop(context.Background(), f, "p1", nil, testTolerance); err == nil {
		t.Error("want error on empty data")
	}
	if f.uploads != 0 {
		t.Errorf("empty data must not upload, got %d", f.uploads)
	}
}

// --- factory ----------------------------------------------------------------

// TestNewPhashPlatformClient_SupportedTypes is a compile-time-plus-runtime
// proof that *emby.Client and *jellyfin.Client satisfy phashPlatformClient
// (BackdropReader + IndexedImageDeleter + ImageUploader) and that unsupported
// types yield nil.
func TestNewPhashPlatformClient_SupportedTypes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, ct := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		if newPhashPlatformClient(&connection.Connection{Type: ct, URL: "http://x", APIKey: "k"}, logger) == nil {
			t.Errorf("type %s: got nil client", ct)
		}
	}
	if newPhashPlatformClient(&connection.Connection{Type: "lidarr"}, logger) != nil {
		t.Error("unsupported type: want nil")
	}
}

// --- Publisher orchestration ------------------------------------------------

// withFakePhashClient swaps the package factory to hand every construction the
// given fake, restoring the real one on cleanup.
func withFakePhashClient(t *testing.T, fake phashPlatformClient) {
	t.Helper()
	prev := phashPlatformClientFactory
	phashPlatformClientFactory = func(_ *connection.Connection, _ *slog.Logger) phashPlatformClient { return fake }
	t.Cleanup(func() { phashPlatformClientFactory = prev })
}

// withFakePhashClientByConn swaps the package factory to dispatch by
// connection ID, letting a test give two connections in one batch distinct
// (and independently failing) fakes -- the shape needed to prove a
// per-connection failure does not stop the batch from processing the others.
func withFakePhashClientByConn(t *testing.T, byConn map[string]phashPlatformClient) {
	t.Helper()
	prev := phashPlatformClientFactory
	phashPlatformClientFactory = func(conn *connection.Connection, _ *slog.Logger) phashPlatformClient {
		return byConn[conn.ID]
	}
	t.Cleanup(func() { phashPlatformClientFactory = prev })
}

// twoEmbyConnPublisher wires artist "a1" to two enabled, healthy, image-write
// connections ("c-good", "c-bad"), for batch-continuation tests: one
// connection fails, the other must still be processed.
func twoEmbyConnPublisher() *Publisher {
	artistLister := &fakePlatformLister{
		ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-good", PlatformArtistID: "p-good"},
			{ArtistID: "a1", ConnectionID: "c-bad", PlatformArtistID: "p-bad"},
		},
		artists: []artist.Artist{{ID: "a1", Name: "Test Artist"}},
	}
	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-good": {
			ID: "c-good", Name: "emby-good", Type: connection.TypeEmby, Enabled: true, Status: "ok",
			Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true},
		},
		"c-bad": {
			ID: "c-bad", Name: "emby-bad", Type: connection.TypeEmby, Enabled: true, Status: "ok",
			Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true},
		},
	}}
	return New(Deps{ArtistService: artistLister, ArtistLister: artistLister, ConnectionService: conns, Logger: silentLogger()})
}

// --- DeletePollutedBackdropOnPlatforms error branches -----------------------

// TestDeletePollutedBackdropOnPlatforms_NilWiring guards the not-fully-wired
// guard clause: a nil Publisher and a Publisher missing a required dependency
// must both error rather than panic.
func TestDeletePollutedBackdropOnPlatforms_NilWiring(t *testing.T) {
	var nilP *Publisher
	if _, err := nilP.DeletePollutedBackdropOnPlatforms(context.Background(), "a1", image.HashHex(0), testTolerance); err == nil {
		t.Fatal("nil publisher: want error, got nil")
	}
	p := New(Deps{Logger: silentLogger()})
	if _, err := p.DeletePollutedBackdropOnPlatforms(context.Background(), "a1", image.HashHex(0), testTolerance); err == nil {
		t.Fatal("unwired publisher: want error, got nil")
	}
}

// TestDeletePollutedBackdropOnPlatforms_BadHashHexReturnsError proves a
// malformed phash string is rejected before any platform is touched, rather
// than treated as a zero hash that could spuriously match.
func TestDeletePollutedBackdropOnPlatforms_BadHashHexReturnsError(t *testing.T) {
	f := &fakePhashClient{backdrops: [][]byte{bandJPEG(t, 8)}}
	withFakePhashClient(t, f)
	p := oneEmbyPublisher()

	if _, err := p.DeletePollutedBackdropOnPlatforms(context.Background(), "a1", "not-a-valid-hex-hash", testTolerance); err == nil {
		t.Fatal("want error on unparsable phash, got nil")
	}
	if len(f.deletes) != 0 {
		t.Errorf("no platform IO may happen on a hash parse failure, got deletes=%v", f.deletes)
	}
}

// TestDeletePollutedBackdropOnPlatforms_GetPlatformIDsErrorReturnsError
// proves a platform-id lookup failure is surfaced rather than treated as "no
// mappings" (which would silently skip every platform).
func TestDeletePollutedBackdropOnPlatforms_GetPlatformIDsErrorReturnsError(t *testing.T) {
	artistLister := &fakePlatformLister{idsErr: fmt.Errorf("db unavailable")}
	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{}}
	p := New(Deps{ArtistService: artistLister, ArtistLister: artistLister, ConnectionService: conns, Logger: silentLogger()})

	if _, err := p.DeletePollutedBackdropOnPlatforms(context.Background(), "a1", image.HashHex(phashOf(t, bandJPEG(t, 32))), testTolerance); err == nil {
		t.Fatal("want error when loading platform ids fails, got nil")
	}
}

// TestDeletePollutedBackdropOnPlatforms_GetByIDErrorIsFailureBatchContinues
// proves a connection lookup failure on one mapping is collected as a
// per-connection Failure and does NOT stop the other mapping from being
// processed (the whole point of the non-fatal batch contract).
func TestDeletePollutedBackdropOnPlatforms_GetByIDErrorIsFailureBatchContinues(t *testing.T) {
	polluted := bandJPEG(t, 32)
	b0 := bandJPEG(t, 8)
	assertDistinct(t, polluted, b0)

	artistLister := &fakePlatformLister{
		ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-missing", PlatformArtistID: "p-missing"},
			{ArtistID: "a1", ConnectionID: "c-good", PlatformArtistID: "p-good"},
		},
		artists: []artist.Artist{{ID: "a1", Name: "Test Artist"}},
	}
	// c-missing is intentionally absent from conns, forcing GetByID to error.
	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-good": {
			ID: "c-good", Name: "emby-good", Type: connection.TypeEmby, Enabled: true, Status: "ok",
			Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true},
		},
	}}
	p := New(Deps{ArtistService: artistLister, ArtistLister: artistLister, ConnectionService: conns, Logger: silentLogger()})

	f := &fakePhashClient{backdrops: [][]byte{b0, polluted}}
	withFakePhashClient(t, f)

	res, err := p.DeletePollutedBackdropOnPlatforms(context.Background(), "a1", image.HashHex(phashOf(t, polluted)), testTolerance)
	if err != nil {
		t.Fatalf("delete on platforms: %v", err)
	}
	if len(res.Failures) != 1 || res.Failures[0].ConnectionID != "c-missing" {
		t.Errorf("want one failure for c-missing, got %#v", res.Failures)
	}
	// c-good must still have been processed despite c-missing's lookup error.
	if res.Deleted != 1 || len(res.Targets) != 1 || res.Targets[0].ConnectionID != "c-good" {
		t.Errorf("want c-good processed despite c-missing failing, got %#v", res)
	}
}

// TestDeletePollutedBackdropOnPlatforms_UnsupportedTypeSkipsNotPanics proves
// a connection whose type has no phash client (client == nil) is skipped
// cleanly -- neither a panic nor a recorded Failure, mirroring how an
// unhealthy/disabled connection is silently skipped upstream of the client
// construction.
func TestDeletePollutedBackdropOnPlatforms_UnsupportedTypeSkipsNotPanics(t *testing.T) {
	artistLister := &fakePlatformLister{
		ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-lidarr", PlatformArtistID: "p1"},
		},
		artists: []artist.Artist{{ID: "a1", Name: "Test Artist"}},
	}
	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-lidarr": {ID: "c-lidarr", Name: "lidarr", Type: "lidarr", Enabled: true, Status: "ok"},
	}}
	p := New(Deps{ArtistService: artistLister, ArtistLister: artistLister, ConnectionService: conns, Logger: silentLogger()})

	res, err := p.DeletePollutedBackdropOnPlatforms(context.Background(), "a1", image.HashHex(phashOf(t, bandJPEG(t, 32))), testTolerance)
	if err != nil {
		t.Fatalf("delete on platforms: %v", err)
	}
	if res.Deleted != 0 || len(res.Targets) != 0 || len(res.Failures) != 0 {
		t.Errorf("unsupported connection type must be a silent skip, got %#v", res)
	}
}

// TestDeletePollutedBackdropOnPlatforms_DeleteErrorIsFailureBatchContinues is
// the orchestration-level proof that a hard delete error on one connection is
// collected as a Failure, does not abort the batch, and leaks no destructive
// action on the failing connection's own artifact (the c-bad backdrop must
// still be present since the error fired before any mutation).
func TestDeletePollutedBackdropOnPlatforms_DeleteErrorIsFailureBatchContinues(t *testing.T) {
	polluted := bandJPEG(t, 32)
	bystander := bandJPEG(t, 8)
	assertDistinct(t, polluted, bystander)

	good := &fakePhashClient{backdrops: [][]byte{bystander, polluted}}
	bad := &fakePhashClient{backdrops: [][]byte{polluted}, deleteErr: fmt.Errorf("connection reset")}
	withFakePhashClientByConn(t, map[string]phashPlatformClient{"c-good": good, "c-bad": bad})
	p := twoEmbyConnPublisher()

	want := image.HashHex(phashOf(t, polluted))
	res, err := p.DeletePollutedBackdropOnPlatforms(context.Background(), "a1", want, testTolerance)
	if err != nil {
		t.Fatalf("delete on platforms: %v", err)
	}
	if len(res.Failures) != 1 || res.Failures[0].ConnectionID != "c-bad" {
		t.Errorf("want one failure for c-bad, got %#v", res.Failures)
	}
	// c-good must still have been processed and its target recorded.
	if res.Deleted != 1 || len(res.Targets) != 1 || res.Targets[0].ConnectionID != "c-good" {
		t.Errorf("want c-good processed despite c-bad failing, got %#v", res)
	}
	// No destructive leak on the failing connection: the polluted backdrop it
	// held is still present, and the delete attempt was not silently retried
	// into a mutation.
	if !bad.hasMatch(t, phashOf(t, polluted)) {
		t.Error("c-bad's backdrop must be untouched after its delete errored")
	}
}

// TestDeletePollutedBackdropOnPlatforms_PartialDeleteRecordsTargetForRestore is
// the proof for the partial-delete recording contract. Two polluted copies (at
// indices 1 and 3) are deleted high-first; the delete of index 3 really removes
// it, then the delete of index 1 fails. The op must: surface the failure, still
// record the connection as a Target (index 3 is genuinely gone and must stay
// RESTORABLE), and count the one real deletion. The surviving index-1 copy
// proves the failure was partial, not a clean rollback.
//
// Revert-and-rerun proof (measured): reordering DeletePollutedBackdropOnPlatforms
// so `if delErr != nil { ...; continue }` runs BEFORE the deleted>0 recording
// (the pre-fix ordering) makes this test FAIL -- Targets comes back empty and
// Deleted is 0 even though slot 3 was removed. Recording successes before
// handling delErr makes it PASS.
func TestDeletePollutedBackdropOnPlatforms_PartialDeleteRecordsTargetForRestore(t *testing.T) {
	polluted := bandJPEG(t, 32)
	b0 := bandJPEG(t, 8)
	b2 := bandJPEG(t, 56)
	assertDistinct(t, polluted, b0, b2)

	// Matches at indices 1 and 3 (deleted descending: 3 then 1). deleteErrAtCall=2
	// lets the first delete (index 3) mutate, then fails the second (index 1).
	f := &fakePhashClient{
		backdrops:       [][]byte{b0, polluted, b2, polluted},
		deleteErr:       fmt.Errorf("connection reset mid-batch"),
		deleteErrAtCall: 2,
	}
	withFakePhashClient(t, f)
	p := oneEmbyPublisher()

	res, err := p.DeletePollutedBackdropOnPlatforms(context.Background(), "a1", image.HashHex(phashOf(t, polluted)), testTolerance)
	if err != nil {
		t.Fatalf("delete on platforms: %v", err)
	}
	// The later delete failed, so the connection is a Failure...
	if len(res.Failures) != 1 || res.Failures[0].ConnectionID != "c-emby" {
		t.Errorf("want one failure for c-emby, got %#v", res.Failures)
	}
	// ...but the slot that WAS removed must still be recorded so a restore can
	// put it back, and the one real deletion must be counted.
	if res.Deleted != 1 {
		t.Errorf("want Deleted=1 (the slot that really went), got %d", res.Deleted)
	}
	if len(res.Targets) != 1 || res.Targets[0].ConnectionID != "c-emby" {
		t.Errorf("partial delete must still record the target for restore, got %#v", res.Targets)
	}
	// ARTIFACT: exactly one polluted copy remains (the one whose delete failed),
	// proving the failure was partial -- one really removed, one really left.
	remaining := 0
	for _, b := range f.backdrops {
		if image.Similarity(phashOf(t, polluted), phashOf(t, b)) >= testTolerance {
			remaining++
		}
	}
	if remaining != 1 {
		t.Errorf("want exactly one polluted copy left after the partial delete, got %d", remaining)
	}
}

// TestDeletePollutedBackdropOnPlatforms_ConcurrentSameTargetDeletesOnce drives
// many concurrent deletes of the SAME target and asserts on the ARTIFACT: the
// polluted picture is removed exactly once and the bystander survives. Without
// the per-target guard the racing goroutines each resolve the same match index
// and both call delete (a double delete that would take out the shifted
// bystander), and the fake's slice mutation races under -race. With the guard
// each op sees the settled artifact, so exactly one delete lands.
func TestDeletePollutedBackdropOnPlatforms_ConcurrentSameTargetDeletesOnce(t *testing.T) {
	polluted := bandJPEG(t, 32)
	b0 := bandJPEG(t, 8)
	assertDistinct(t, polluted, b0)

	f := &fakePhashClient{backdrops: [][]byte{b0, polluted}}
	withFakePhashClient(t, f)
	p := oneEmbyPublisher()
	want := image.HashHex(phashOf(t, polluted))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.DeletePollutedBackdropOnPlatforms(context.Background(), "a1", want, testTolerance)
		}()
	}
	wg.Wait()

	if len(f.deletes) != 1 {
		t.Errorf("concurrent deletes of the same target must delete exactly once, got %v", f.deletes)
	}
	if f.hasMatch(t, phashOf(t, polluted)) {
		t.Error("polluted backdrop still present after concurrent delete")
	}
	if len(f.backdrops) != 1 || !bytes.Equal(f.backdrops[0], b0) {
		t.Errorf("bystander not intact after concurrent delete: got %d backdrops", len(f.backdrops))
	}
}

// --- RestoreBackdropToPlatforms error branches -------------------------------

// TestRestoreBackdropToPlatforms_NilWiring mirrors the delete-side nil-wiring
// guard for the restore entry point.
func TestRestoreBackdropToPlatforms_NilWiring(t *testing.T) {
	var nilP *Publisher
	targets := []image.RepairPlatformTarget{{ConnectionID: "c1", PlatformArtistID: "p1"}}
	if _, err := nilP.RestoreBackdropToPlatforms(context.Background(), targets, bandJPEG(t, 32), testTolerance); err == nil {
		t.Fatal("nil publisher: want error, got nil")
	}
	p := New(Deps{Logger: silentLogger()})
	if _, err := p.RestoreBackdropToPlatforms(context.Background(), targets, bandJPEG(t, 32), testTolerance); err == nil {
		t.Fatal("unwired publisher: want error, got nil")
	}
}

// TestRestoreBackdropToPlatforms_RefusesEmptyData mirrors the helper-level
// empty-data guard at the orchestration entry point, before any target is
// touched.
func TestRestoreBackdropToPlatforms_RefusesEmptyData(t *testing.T) {
	f := &fakePhashClient{}
	withFakePhashClient(t, f)
	p := oneEmbyPublisher()
	targets := []image.RepairPlatformTarget{{ConnectionID: "c-emby", PlatformArtistID: "p1"}}

	if _, err := p.RestoreBackdropToPlatforms(context.Background(), targets, nil, testTolerance); err == nil {
		t.Fatal("want error on empty data, got nil")
	}
	if f.uploads != 0 {
		t.Errorf("empty data must not upload to any target, got %d", f.uploads)
	}
}

// TestRestoreBackdropToPlatforms_GetByIDErrorIsFailureBatchContinues proves a
// connection lookup failure on one target is collected as a Failure and does
// not stop the other target from being restored.
func TestRestoreBackdropToPlatforms_GetByIDErrorIsFailureBatchContinues(t *testing.T) {
	polluted := bandJPEG(t, 32)
	f := &fakePhashClient{backdrops: [][]byte{bandJPEG(t, 8)}}
	withFakePhashClient(t, f)
	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-good": {
			ID: "c-good", Name: "emby-good", Type: connection.TypeEmby, Enabled: true, Status: "ok",
			Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true},
		},
	}}
	p := New(Deps{ArtistService: &fakePlatformLister{}, ArtistLister: &fakePlatformLister{}, ConnectionService: conns, Logger: silentLogger()})

	targets := []image.RepairPlatformTarget{
		{ConnectionID: "c-missing", PlatformArtistID: "p-missing"},
		{ConnectionID: "c-good", PlatformArtistID: "p-good"},
	}
	res, err := p.RestoreBackdropToPlatforms(context.Background(), targets, polluted, testTolerance)
	if err != nil {
		t.Fatalf("restore to platforms: %v", err)
	}
	if len(res.Failures) != 1 || res.Failures[0].ConnectionID != "c-missing" {
		t.Errorf("want one failure for c-missing, got %#v", res.Failures)
	}
	if res.Appended != 1 {
		t.Errorf("want c-good restored despite c-missing failing, got %#v", res)
	}
	if !f.hasMatch(t, phashOf(t, polluted)) {
		t.Error("c-good target must have received the restored backdrop")
	}
}

// TestRestoreBackdropToPlatforms_UnsupportedTypeIsFailureNotSkip proves a
// target whose connection type has no phash client is recorded as a Failure
// (unlike the delete direction's silent skip): a restore target that cannot
// be serviced must not be silently dropped, or the caller would consume the
// quarantine entry against a restore that never happened.
func TestRestoreBackdropToPlatforms_UnsupportedTypeIsFailureNotSkip(t *testing.T) {
	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-lidarr": {ID: "c-lidarr", Name: "lidarr", Type: "lidarr", Enabled: true, Status: "ok"},
	}}
	p := New(Deps{ArtistService: &fakePlatformLister{}, ArtistLister: &fakePlatformLister{}, ConnectionService: conns, Logger: silentLogger()})

	targets := []image.RepairPlatformTarget{{ConnectionID: "c-lidarr", PlatformArtistID: "p1"}}
	res, err := p.RestoreBackdropToPlatforms(context.Background(), targets, bandJPEG(t, 32), testTolerance)
	if err != nil {
		t.Fatalf("restore to platforms: %v", err)
	}
	if len(res.Failures) != 1 || res.Appended != 0 {
		t.Errorf("unsupported connection type must be a Failure, got %#v", res)
	}
}

// TestRestoreBackdropToPlatforms_UploadErrorIsFailureBatchContinuesNoLeak is
// the orchestration-level proof that a hard upload error on one target is
// collected as a Failure, does not abort the batch, and leaks no destructive
// write (the failing target's backdrop set is unchanged).
func TestRestoreBackdropToPlatforms_UploadErrorIsFailureBatchContinuesNoLeak(t *testing.T) {
	polluted := bandJPEG(t, 32)
	bystanderGood := bandJPEG(t, 8)
	bystanderBad := bandJPEG(t, 56)
	assertDistinct(t, polluted, bystanderGood, bystanderBad)

	good := &fakePhashClient{backdrops: [][]byte{bystanderGood}}
	bad := &fakePhashClient{backdrops: [][]byte{bystanderBad}, uploadErr: fmt.Errorf("connection reset")}
	withFakePhashClientByConn(t, map[string]phashPlatformClient{"c-good": good, "c-bad": bad})
	p := twoEmbyConnPublisher()

	targets := []image.RepairPlatformTarget{
		{ConnectionID: "c-good", PlatformArtistID: "p-good"},
		{ConnectionID: "c-bad", PlatformArtistID: "p-bad"},
	}
	res, err := p.RestoreBackdropToPlatforms(context.Background(), targets, polluted, testTolerance)
	if err != nil {
		t.Fatalf("restore to platforms: %v", err)
	}
	if len(res.Failures) != 1 || res.Failures[0].ConnectionID != "c-bad" {
		t.Errorf("want one failure for c-bad, got %#v", res.Failures)
	}
	if res.Appended != 1 {
		t.Errorf("want c-good restored despite c-bad failing, got %#v", res)
	}
	if !good.hasMatch(t, phashOf(t, polluted)) {
		t.Error("c-good must have received the restored backdrop")
	}
	// No destructive leak: c-bad gained nothing beyond its original bystander.
	if len(bad.backdrops) != 1 || !bytes.Equal(bad.backdrops[0], bystanderBad) {
		t.Errorf("c-bad must be unchanged after its upload errored, got %d backdrops", len(bad.backdrops))
	}
}

// TestRestoreBackdropToPlatforms_AlreadyPresentAcrossMultipleTargets is the
// orchestration-level proof for the AlreadyPresent counter: when the picture
// is already on a target, that target contributes to AlreadyPresent (not
// Appended), makes no upload, and a second target that genuinely needs the
// restore still gets it (the counters are per-target, not batch-wide).
func TestRestoreBackdropToPlatforms_AlreadyPresentAcrossMultipleTargets(t *testing.T) {
	polluted := bandJPEG(t, 32)
	bystander := bandJPEG(t, 8)
	assertDistinct(t, polluted, bystander)

	alreadyHas := &fakePhashClient{backdrops: [][]byte{polluted}}
	needsIt := &fakePhashClient{backdrops: [][]byte{bystander}}
	withFakePhashClientByConn(t, map[string]phashPlatformClient{"c-good": alreadyHas, "c-bad": needsIt})
	p := twoEmbyConnPublisher()

	targets := []image.RepairPlatformTarget{
		{ConnectionID: "c-good", PlatformArtistID: "p-good"},
		{ConnectionID: "c-bad", PlatformArtistID: "p-bad"},
	}
	res, err := p.RestoreBackdropToPlatforms(context.Background(), targets, polluted, testTolerance)
	if err != nil {
		t.Fatalf("restore to platforms: %v", err)
	}
	if res.AlreadyPresent != 1 || res.Appended != 1 || len(res.Failures) != 0 {
		t.Errorf("want AlreadyPresent=1 Appended=1, got %#v", res)
	}
	if alreadyHas.uploads != 0 {
		t.Errorf("already-present target must not receive an upload, got %d", alreadyHas.uploads)
	}
	if !needsIt.hasMatch(t, phashOf(t, polluted)) {
		t.Error("the target that needed the restore must have received it")
	}
}

// TestRestoreBackdropToPlatforms_ConcurrentSameTargetUploadsOnce drives many
// concurrent restores of the SAME target and asserts on the ARTIFACT: exactly
// one copy is uploaded and present. Without the per-target guard the racing
// goroutines each run the presence-check while the item is still empty, all see
// absent, and all upload -- a pile of duplicates (and a data race on the fake's
// slice/counter under -race). With the guard the first restore's upload settles
// before the next runs its presence-check, so the rest are idempotent no-ops.
func TestRestoreBackdropToPlatforms_ConcurrentSameTargetUploadsOnce(t *testing.T) {
	polluted := bandJPEG(t, 32)
	b0 := bandJPEG(t, 8)
	assertDistinct(t, polluted, b0)

	f := &fakePhashClient{backdrops: [][]byte{b0}}
	withFakePhashClient(t, f)
	p := oneEmbyPublisher()
	targets := []image.RepairPlatformTarget{{ConnectionID: "c-emby", PlatformArtistID: "p1"}}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.RestoreBackdropToPlatforms(context.Background(), targets, polluted, testTolerance)
		}()
	}
	wg.Wait()

	if f.uploads != 1 {
		t.Errorf("concurrent restores of the same target must upload exactly once, got %d", f.uploads)
	}
	// ARTIFACT: exactly one restored copy is present (plus the untouched
	// bystander) -- no duplicate stack.
	matches := 0
	for _, b := range f.backdrops {
		if image.Similarity(phashOf(t, polluted), phashOf(t, b)) >= testTolerance {
			matches++
		}
	}
	if matches != 1 {
		t.Errorf("want exactly one restored copy present, got %d", matches)
	}
}

func oneEmbyPublisher() *Publisher {
	artistLister := &fakePlatformLister{
		ids:     []artist.PlatformID{{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"}},
		artists: []artist.Artist{{ID: "a1", Name: "Test Artist"}},
	}
	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-emby": {
			ID: "c-emby", Name: "emby", Type: connection.TypeEmby, Enabled: true, Status: "ok",
			Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true},
		},
	}}
	return New(Deps{ArtistService: artistLister, ArtistLister: artistLister, ConnectionService: conns, Logger: silentLogger()})
}

func TestDeletePollutedBackdropOnPlatforms_RecordsTarget(t *testing.T) {
	polluted := bandJPEG(t, 32)
	b0 := bandJPEG(t, 8)
	assertDistinct(t, polluted, b0)

	f := &fakePhashClient{backdrops: [][]byte{b0, polluted}}
	withFakePhashClient(t, f)
	p := oneEmbyPublisher()

	res, err := p.DeletePollutedBackdropOnPlatforms(context.Background(), "a1", image.HashHex(phashOf(t, polluted)), testTolerance)
	if err != nil {
		t.Fatalf("delete on platforms: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("deleted: want 1, got %d", res.Deleted)
	}
	// The target is recorded from the connection an image was actually deleted
	// from -- this is what a later restore re-uploads into.
	if len(res.Targets) != 1 || res.Targets[0] != (image.RepairPlatformTarget{ConnectionID: "c-emby", PlatformArtistID: "p1"}) {
		t.Errorf("targets: want one c-emby/p1, got %#v", res.Targets)
	}
	if f.hasMatch(t, phashOf(t, polluted)) {
		t.Error("polluted backdrop still on platform after orchestrated delete")
	}
}

func TestDeletePollutedBackdropOnPlatforms_NoMatchRecordsNoTarget(t *testing.T) {
	polluted := bandJPEG(t, 32)
	b0 := bandJPEG(t, 8)
	assertDistinct(t, polluted, b0)

	f := &fakePhashClient{backdrops: [][]byte{b0}}
	withFakePhashClient(t, f)
	p := oneEmbyPublisher()

	res, err := p.DeletePollutedBackdropOnPlatforms(context.Background(), "a1", image.HashHex(phashOf(t, polluted)), testTolerance)
	if err != nil {
		t.Fatalf("delete on platforms: %v", err)
	}
	if res.Deleted != 0 || len(res.Targets) != 0 {
		t.Errorf("nothing matched, so no target may be recorded: %#v", res)
	}
}

func TestRestoreBackdropToPlatforms_AppendsToRecordedTarget(t *testing.T) {
	polluted := bandJPEG(t, 32)
	b0 := bandJPEG(t, 8)
	assertDistinct(t, polluted, b0)

	f := &fakePhashClient{backdrops: [][]byte{b0}}
	withFakePhashClient(t, f)
	p := oneEmbyPublisher()

	targets := []image.RepairPlatformTarget{{ConnectionID: "c-emby", PlatformArtistID: "p1"}}
	res, err := p.RestoreBackdropToPlatforms(context.Background(), targets, polluted, testTolerance)
	if err != nil {
		t.Fatalf("restore to platforms: %v", err)
	}
	if res.Appended != 1 || res.AlreadyPresent != 0 || len(res.Failures) != 0 {
		t.Errorf("want appended=1, got %#v", res)
	}
	if !f.hasMatch(t, phashOf(t, polluted)) {
		t.Error("restored backdrop not present on platform")
	}
}

// TestRestoreBackdropToPlatforms_UnhealthyTargetIsFailureNotSkip proves a
// target whose connection is disabled is recorded as a failure (so the caller
// keeps the quarantine entry) rather than silently counted as done.
func TestRestoreBackdropToPlatforms_UnhealthyTargetIsFailureNotSkip(t *testing.T) {
	f := &fakePhashClient{}
	withFakePhashClient(t, f)
	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-emby": {ID: "c-emby", Name: "emby", Type: connection.TypeEmby, Enabled: false, Status: "ok",
			Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
	}}
	p := New(Deps{ArtistService: &fakePlatformLister{}, ArtistLister: &fakePlatformLister{}, ConnectionService: conns, Logger: silentLogger()})

	targets := []image.RepairPlatformTarget{{ConnectionID: "c-emby", PlatformArtistID: "p1"}}
	res, err := p.RestoreBackdropToPlatforms(context.Background(), targets, bandJPEG(t, 32), testTolerance)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(res.Failures) != 1 || res.Appended != 0 {
		t.Errorf("disabled target must be a failure, got %#v", res)
	}
	if f.uploads != 0 {
		t.Errorf("no upload may happen to a disabled connection, got %d", f.uploads)
	}
}

// --- resolveFanartReplaceTarget (#3125 F3) ----------------------------------

// TestResolveFanartReplaceTarget_EmptyPlatformWritesIndexZero is the
// degenerate case: nothing to clobber, so index 0 is always safe.
func TestResolveFanartReplaceTarget_EmptyPlatformWritesIndexZero(t *testing.T) {
	f := &fakePhashClient{}
	decision, err := resolveFanartReplaceTarget(context.Background(), f, "p1", bandJPEG(t, 1), "", testTolerance)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if decision.Kind != fanartTargetIndex || decision.Index != 0 {
		t.Errorf("decision = %+v, want {Kind: fanartTargetIndex, Index: 0}", decision)
	}
}

// TestResolveFanartReplaceTarget_AlreadyPresentIsNoop proves the platform
// already holding the new bytes (a retry whose prior response was lost)
// resolves to a no-op, never a write -- assert on the DECISION, not merely
// on a later "no upload happened", so a caller that ignored the decision and
// uploaded anyway would still be caught by whoever asserts the upload count.
func TestResolveFanartReplaceTarget_AlreadyPresentIsNoop(t *testing.T) {
	newBytes := bandJPEG(t, 7)
	f := &fakePhashClient{backdrops: [][]byte{bandJPEG(t, 3), newBytes}}
	decision, err := resolveFanartReplaceTarget(context.Background(), f, "p1", newBytes, "", testTolerance)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if decision.Kind != fanartTargetNoop {
		t.Errorf("decision = %+v, want Kind fanartTargetNoop", decision)
	}
}

// TestResolveFanartReplaceTarget_IdentifiesPreviousPrimaryAmongBystanders is
// the CORE F3 proof: the previous primary has been shifted to a NON-ZERO
// index by an earlier platform-side delete (exactly the #3125 review's
// measured shape -- deleting index 0 shifts survivors down), and the
// resolver must find it there rather than defaulting to 0.
func TestResolveFanartReplaceTarget_IdentifiesPreviousPrimaryAmongBystanders(t *testing.T) {
	bystanderA := bandJPEG(t, 11)
	previousPrimary := bandJPEG(t, 22)
	bystanderB := bandJPEG(t, 33)
	assertDistinct(t, bystanderA, previousPrimary, bystanderB)

	// The previous primary sits at index 1 -- NOT 0 -- simulating the
	// post-delete-and-reindex state.
	f := &fakePhashClient{backdrops: [][]byte{bystanderA, previousPrimary, bystanderB}}
	previousHash := image.HashHex(phashOf(t, previousPrimary))

	newPrimary := bandJPEG(t, 44)
	assertDistinct(t, bystanderA, previousPrimary, bystanderB, newPrimary)

	decision, err := resolveFanartReplaceTarget(context.Background(), f, "p1", newPrimary, previousHash, testTolerance)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if decision.Kind != fanartTargetIndex || decision.Index != 1 {
		t.Errorf("decision = %+v, want {Kind: fanartTargetIndex, Index: 1} (the slot holding the previous primary)", decision)
	}
}

// TestResolveFanartReplaceTarget_CannotIdentify_FallsBackToAppend covers the
// case the #3125 review demanded be handled honestly: no stored previous
// hash (a first sync, or a never-hashed legacy row) and a non-empty
// platform. The resolver must refuse to guess an index and fall back to
// append, never picking index 0 blind.
func TestResolveFanartReplaceTarget_CannotIdentify_FallsBackToAppend(t *testing.T) {
	f := &fakePhashClient{backdrops: [][]byte{bandJPEG(t, 5), bandJPEG(t, 6)}}
	newPrimary := bandJPEG(t, 99)
	assertDistinct(t, f.backdrops[0], f.backdrops[1], newPrimary)

	decision, err := resolveFanartReplaceTarget(context.Background(), f, "p1", newPrimary, "", testTolerance)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if decision.Kind != fanartTargetAppend {
		t.Errorf("decision = %+v, want Kind fanartTargetAppend", decision)
	}
	if decision.Why == "" {
		t.Error("append decision must carry a Why, for the caller's Warn log")
	}
}

// TestResolveFanartReplaceTarget_ZeroHashHexTreatedAsUnknown guards the
// zero-hash trap: previousPHashHex parsing to the all-zero hash (an
// unhashed/legacy row, PerceptualHash's own zero value) must NOT be trusted
// as a real identity -- it would otherwise manufacture a match against any
// other genuinely-unhashed slot. Confirmed by using a platform whose only
// backdrop the all-zero hash would spuriously match if treated as usable.
func TestResolveFanartReplaceTarget_ZeroHashHexTreatedAsUnknown(t *testing.T) {
	f := &fakePhashClient{backdrops: [][]byte{bandJPEG(t, 1)}}
	newPrimary := bandJPEG(t, 2)
	assertDistinct(t, f.backdrops[0], newPrimary)

	zeroHashHex := image.HashHex(0)
	decision, err := resolveFanartReplaceTarget(context.Background(), f, "p1", newPrimary, zeroHashHex, testTolerance)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if decision.Kind != fanartTargetAppend {
		t.Errorf("decision = %+v, want Kind fanartTargetAppend (a zero previous-hash must never authorize an index write)", decision)
	}
}

// TestResolveFanartReplaceTarget_BystanderSurvivesIndexWrite is the
// MUTATION-PROOF WIRING TEST: it drives resolveFanartReplaceTarget's INDEX
// decision all the way through an actual UploadImageAtIndex call (via
// fakePhashClient's Emby-like in-place-replace semantics) and asserts the
// BYSTANDER at every OTHER index survives untouched. This is the test that
// must fail if the guard is removed and the code goes back to writing index
// 0 unconditionally: the previous primary here sits at index 1, so a
// blind-index-0 write would destroy bystanderAtZero instead.
func TestResolveFanartReplaceTarget_BystanderSurvivesIndexWrite(t *testing.T) {
	bystanderAtZero := bandJPEG(t, 111)
	previousPrimary := bandJPEG(t, 222)
	newPrimary := bandJPEG(t, 333)
	assertDistinct(t, bystanderAtZero, previousPrimary, newPrimary)

	f := &fakePhashClient{backdrops: [][]byte{bystanderAtZero, previousPrimary}}
	previousHash := image.HashHex(phashOf(t, previousPrimary))

	// PRECONDITION: the bystander occupies index 0 before anything runs.
	if !bytesEqual(f.backdrops[0], bystanderAtZero) {
		t.Fatal("precondition failed: bystander is not at index 0")
	}

	decision, err := resolveFanartReplaceTarget(context.Background(), f, "p1", newPrimary, previousHash, testTolerance)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if decision.Kind != fanartTargetIndex {
		t.Fatalf("decision = %+v, want Kind fanartTargetIndex", decision)
	}
	if decision.Index == 0 {
		t.Fatalf("resolved index = 0, which is the BYSTANDER's slot -- writing here would destroy it")
	}

	if err := f.UploadImageAtIndex(context.Background(), "p1", "fanart", decision.Index, newPrimary, "image/jpeg"); err != nil {
		t.Fatalf("upload at resolved index: %v", err)
	}

	if !bytesEqual(f.backdrops[0], bystanderAtZero) {
		t.Errorf("bystander at index 0 was overwritten; got %d bytes, want the original bystander content", len(f.backdrops[0]))
	}
	if !bytesEqual(f.backdrops[decision.Index], newPrimary) {
		t.Errorf("resolved index %d does not hold the new primary after upload", decision.Index)
	}
}

func bytesEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}

// --- resolveFanartReplaceTarget wired through SyncImageToPlatforms ---------

// noopFanartServer serves a single-backdrop artist whose stored backdrop is
// BYTE-IDENTICAL to the fixture the test will later "sync", so the resolver
// must decide fanartTargetNoop. It records every POST it receives -- the
// wire-level proof that a no-op decision issues ZERO upload requests, not
// merely that the local warnings/counters looked right.
type noopFanartServer struct {
	mu    sync.Mutex
	posts []string
	data  []byte
}

func (s *noopFanartServer) postCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posts)
}

func newNoopFanartServer(data []byte) (*httptest.Server, *noopFanartServer) {
	s := &noopFanartServer{data: data}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			s.mu.Lock()
			s.posts = append(s.posts, r.URL.Path)
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(r.URL.Path, "/Images/Backdrop/0"):
			w.Header().Set("Content-Type", "image/jpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(s.data)
		case strings.HasPrefix(r.URL.Path, "/Users/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"BackdropImageTags":["tag0"]}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	return srv, s
}

// TestSyncImageToPlatforms_FanartNoop_IssuesZeroUploadRequests is the
// wire-level twin of TestResolveFanartReplaceTarget_AlreadyPresentIsNoop: it
// drives the REAL SyncImageToPlatforms path against an httptest server whose
// stored backdrop is byte-identical to the local fanart file, and asserts
// NO POST request of any shape reaches the server -- the strongest possible
// proof that a no-op decision issues no write, stronger than counting a
// local uploaded-bool.
func TestSyncImageToPlatforms_FanartNoop_IssuesZeroUploadRequests(t *testing.T) {
	fanartBytes := bandJPEG(t, 55)

	srv, recorder := newNoopFanartServer(fanartBytes)
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), fanartBytes, 0o644); err != nil {
		t.Fatalf("seeding fanart.jpg: %v", err)
	}

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "my-emby", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})

	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, "fanart")
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings; got %v", warnings)
	}

	// A noop decision must not add this connection to uploadedTo, so the
	// post-push repair pass never runs at all for it either -- give any
	// stray goroutine a moment, then assert ZERO POSTs reached the server.
	time.Sleep(50 * time.Millisecond)
	if got := recorder.postCount(); got != 0 {
		t.Errorf("POST requests to the platform = %d, want 0 (a noop decision must never write)", got)
	}
}
