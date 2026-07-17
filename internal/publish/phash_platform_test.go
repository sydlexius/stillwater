package publish

import (
	"bytes"
	"context"
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
