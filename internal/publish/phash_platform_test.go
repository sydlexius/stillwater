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
	"testing"

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
	if f.deleteErr != nil {
		return f.deleteErr // hard failure: nothing recorded, nothing mutated
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

	f := &fakePhashClient{backdrops: [][]byte{b0, polluted, b2}}
	want := phashOf(t, polluted)

	deleted, err := deletePollutedBackdrops(context.Background(), f, "p1", want, testTolerance)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted count: want 1, got %d", deleted)
	}
	// ARTIFACT assertions, not counters: the polluted picture is gone and BOTH
	// bystanders survive (fail-closed: a non-matching slot is never touched).
	if f.hasMatch(t, want) {
		t.Error("polluted backdrop still present after delete")
	}
	if len(f.backdrops) != 2 || !bytes.Equal(f.backdrops[0], b0) || !bytes.Equal(f.backdrops[1], b2) {
		t.Errorf("bystanders not preserved intact: got %d backdrops", len(f.backdrops))
	}
	if len(f.deletes) != 1 || f.deletes[0] != 1 {
		t.Errorf("expected exactly index 1 deleted, got %v", f.deletes)
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
	for _, tol := range []float64{0, -0.1, 1.5} {
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
