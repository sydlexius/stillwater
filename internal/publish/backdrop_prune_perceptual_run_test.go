package publish

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// perceptualFixture is one picture at three resolutions plus an unrelated
// picture, with every fixture property the tests rely on asserted up front.
type perceptualFixture struct{ small, mid, big, other []byte }

func newPerceptualFixture(t *testing.T) perceptualFixture {
	t.Helper()
	f := perceptualFixture{
		small: fieldJPEG(t, 7, 1), mid: fieldJPEG(t, 7, 2), big: fieldJPEG(t, 7, 3),
		other: fieldJPEG(t, 99, 1),
	}
	mustSimilar(t, f.big, f.small, true)
	mustSimilar(t, f.big, f.mid, true)
	mustSimilar(t, f.big, f.other, false)
	return f
}

// fakeArtistImages serves the artist_images rows the perceptual tier reads.
type fakeArtistImages struct {
	rows []artist.ArtistImage
	err  error
}

func (f *fakeArtistImages) GetImagesForArtist(_ context.Context, _ string) ([]artist.ArtistImage, error) {
	return f.rows, f.err
}

// perceptualPublisher is the one-artist publisher the perceptual tier can run
// on: nothing protected, and local fanart that no platform backdrop matches.
func perceptualPublisher(t *testing.T, fake backdropPruneClient) *Publisher {
	t.Helper()
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)
	p.artistImages = &fakeArtistImages{}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), []byte("unrelated local fanart"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureArtist(t, p).artists[0].Path = dir
	return p
}

// fixtureArtist reaches the single artist the one-artist publisher pages over,
// so a test can give it a local folder or a lock.
func fixtureArtist(t *testing.T, p *Publisher) *fakePlatformLister {
	t.Helper()
	l, ok := p.artistLister.(*fakePlatformLister)
	if !ok || len(l.artists) != 1 {
		t.Fatalf("unexpected lister fixture %T", p.artistLister)
	}
	return l
}

// assertPlatform checks the platform's FINAL state, not which calls were made:
// the fake renumbers on delete exactly like Emby and Jellyfin, so this is what
// the operator would see on the server afterwards.
func assertPlatform(t *testing.T, fake *fakeBackdropClient, want ...[]byte) {
	t.Helper()
	if len(fake.backdrops) != len(want) {
		t.Fatalf("platform holds %d backdrops, want %d (deleted %v)", len(fake.backdrops), len(want), fake.deleted)
	}
	for i := range want {
		if !bytes.Equal(fake.backdrops[i], want[i]) {
			t.Errorf("platform slot %d holds the wrong image after the prune (deleted %v)", i, fake.deleted)
		}
	}
}

// THE REGRESSION THE EARLIER BRANCH SHIPPED: the perceptual survivor sits ABOVE
// its candidates. Detection: slot 3 duplicates 1 (exact, kept 1); the big copy
// at 4 is the perceptual survivor of 2 and 0. Deleting 3 moves the survivor to
// 3, deleting 2 moves it to 2. Without the survivor shift the second and third
// re-verifies read the wrong slot and the run silently stops short.
func TestPrunePerceptual_SurvivorAboveCandidatesCompletesThePlan(t *testing.T) {
	f := newPerceptualFixture(t)
	fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.other, f.mid, f.other, f.big}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(), PlatformBackdropPruneScope{AllArtists: true, Perceptual: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	assertPlatform(t, fake, f.other, f.big)
	if res.BackdropsRemoved != 3 || res.SkippedChanged != 0 || len(res.Failures) != 0 {
		t.Errorf("removed=%d skipped=%d failures=%v, want 3/0/none", res.BackdropsRemoved, res.SkippedChanged, res.Failures)
	}
	// The plan keeps DETECTION-TIME numbering and names each entry's tier.
	want := []PlatformBackdropPrunePlanEntry{
		{Index: 3, Survivor: 1, Tier: PruneTierExact},
		{Index: 2, Survivor: 4, Tier: PruneTierPerceptual},
		{Index: 0, Survivor: 4, Tier: PruneTierPerceptual},
	}
	if len(res.Plan) != len(want) {
		t.Fatalf("plan %+v, want %d entries", res.Plan, len(want))
	}
	for i, w := range want {
		g := res.Plan[i]
		if g.Index != w.Index || g.Survivor != w.Survivor || g.Tier != w.Tier || g.Outcome != PrunePlanDeleted {
			t.Errorf("plan[%d] = %+v, want %+v deleted", i, g, w)
		}
	}

	// Idempotent: a re-run over its own output finds nothing.
	again, err := p.PrunePlatformBackdropDuplicates(context.Background(), PlatformBackdropPruneScope{AllArtists: true, Perceptual: true})
	if err != nil || len(again.Plan) != 0 {
		t.Errorf("re-run: plan %+v err %v, want nothing", again.Plan, err)
	}
}

func TestPrunePerceptual_OffByDefault(t *testing.T) {
	f := newPerceptualFixture(t)
	fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)
	if _, err := p.PrunePlatformBackdropDuplicates(context.Background(), PlatformBackdropPruneScope{AllArtists: true}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	assertPlatform(t, fake, f.small, f.big)
}

func TestPrunePerceptual_DryRunReportsTheTierAndDeletesNothing(t *testing.T) {
	f := newPerceptualFixture(t)
	fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(), PlatformBackdropPruneScope{AllArtists: true, Perceptual: true, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	assertPlatform(t, fake, f.small, f.big)
	if len(res.Plan) != 1 {
		t.Fatalf("plan %+v, want one entry", res.Plan)
	}
	if e := res.Plan[0]; e.Index != 0 || e.Survivor != 1 || e.Tier != PruneTierPerceptual || e.Outcome != PrunePlanPlanned {
		t.Errorf("plan entry %+v, want index 0 -> survivor 1, perceptual, planned", e)
	}
}

// The copy that matches the artist's local fanart is kept even when a larger
// copy exists: the local folder is authoritative, and deleting its twin would
// read to the reconciler as a missing local image and be re-pushed.
func TestPrunePerceptual_KeepsTheLocalTwin(t *testing.T) {
	f := newPerceptualFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), f.small, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeBackdropClient{backdrops: [][]byte{f.big, f.small}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)
	fixtureArtist(t, p).artists[0].Path = dir

	if _, err := p.PrunePlatformBackdropDuplicates(context.Background(), PlatformBackdropPruneScope{AllArtists: true, Perceptual: true}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	assertPlatform(t, fake, f.small)
}

func TestPrunePerceptual_LockedArtistGetsExactOnly(t *testing.T) {
	f := newPerceptualFixture(t)
	fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big, f.big}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)
	fixtureArtist(t, p).artists[0].Locked = true

	if _, err := p.PrunePlatformBackdropDuplicates(context.Background(), PlatformBackdropPruneScope{AllArtists: true, Perceptual: true}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	assertPlatform(t, fake, f.small, f.big)
}

// An unreadable local fanart file means its platform twin cannot be
// recognized, so the perceptual tier stays off for the artist and says so.
func TestPrunePerceptual_UnreadableLocalFanartFailsClosed(t *testing.T) {
	f := newPerceptualFixture(t)
	dir := t.TempDir()
	// A dangling symlink: discovered as a fanart file, unreadable even as root.
	if err := os.Symlink(filepath.Join(dir, "missing.jpg"), filepath.Join(dir, "fanart.jpg")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)
	fixtureArtist(t, p).artists[0].Path = dir

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(), PlatformBackdropPruneScope{AllArtists: true, Perceptual: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	assertPlatform(t, fake, f.small, f.big)
	if len(res.Failures) != 1 {
		t.Errorf("failures %+v, want one naming the skipped tier", res.Failures)
	}
}

func TestPrunePerceptual_UndecodableBackdropDeletesNothingPerceptual(t *testing.T) {
	f := newPerceptualFixture(t)
	junk := []byte("not an image")
	fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big, junk}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(), PlatformBackdropPruneScope{AllArtists: true, Perceptual: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	assertPlatform(t, fake, f.small, f.big, junk)
	if len(res.Failures) != 1 || res.Failures[0].ConnectionID != "c-emby" {
		t.Errorf("failures %+v, want one for the connection", res.Failures)
	}
}

// The prune holds the per-target lock other destructive backdrop writers hold:
// while it is held elsewhere, no delete happens.
func TestPrunePerceptual_WaitsForThePerTargetLock(t *testing.T) {
	fake := &fakeBackdropClient{backdrops: [][]byte{[]byte("A"), []byte("A")}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)
	unlock := p.lockPhashTarget("c-emby", "p1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.PrunePlatformBackdropDuplicates(context.Background(), PlatformBackdropPruneScope{AllArtists: true})
	}()
	select {
	case <-done:
		t.Fatal("the prune finished while another writer held the target lock")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	<-done
	if len(fake.deleted) != 1 {
		t.Errorf("deleted %v after the lock was released, want one delete", fake.deleted)
	}
}

// An artist folder with no fanart in it looks exactly like a folder on a mount
// that is down, so the local twins are unknown: the perceptual tier is skipped
// rather than run with no twin protection.
func TestPrunePerceptual_EmptyLocalFolderFailsClosed(t *testing.T) {
	f := newPerceptualFixture(t)
	dir := t.TempDir() // exists, holds no fanart
	fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)
	fixtureArtist(t, p).artists[0].Path = dir

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(), PlatformBackdropPruneScope{AllArtists: true, Perceptual: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	assertPlatform(t, fake, f.small, f.big)
	if len(res.Failures) != 1 || res.Failures[0].ArtistID != "a1" {
		t.Errorf("failures %+v, want one naming the skipped tier", res.Failures)
	}
}

// Protected fanart (#2533: locked or user-set) or an unreadable protection
// state skips the perceptual tier; the exact tier still removes the twin.
func TestPrunePerceptual_ProtectedFanartGetsExactOnly(t *testing.T) {
	f := newPerceptualFixture(t)
	for name, imgs := range map[string]*fakeArtistImages{
		"locked slot":   {rows: []artist.ArtistImage{{ImageType: "fanart", SlotIndex: 1, Locked: true}}},
		"user-set slot": {rows: []artist.ArtistImage{{ImageType: "fanart", Source: artist.ImageSourceUser}}},
		"read error":    {err: errors.New("database is locked")},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big, f.big}, failAt: -1, failDeleteAt: -1}
			p := perceptualPublisher(t, fake)
			p.artistImages = imgs
			assertSkipped(t, p, fake, "perceptual tier skipped", f.small, f.big)
		})
	}
}

// No local image folder means no twin protection, so the tier is skipped.
func TestPrunePerceptual_NoLocalFolderFailsClosed(t *testing.T) {
	f := newPerceptualFixture(t)
	fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)
	fixtureArtist(t, p).artists[0].Path = ""
	assertSkipped(t, p, fake, "no local image folder", f.small, f.big)
}

// assertSkipped runs a live perceptual prune and checks the platform's final
// state plus the one artist-level failure naming why the tier was skipped.
func assertSkipped(t *testing.T, p *Publisher, fake *fakeBackdropClient, why string, want ...[]byte) {
	t.Helper()
	res, err := p.PrunePlatformBackdropDuplicates(context.Background(), PlatformBackdropPruneScope{AllArtists: true, Perceptual: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	assertPlatform(t, fake, want...)
	if len(res.Failures) != 1 || res.Failures[0].ArtistID != "a1" || !strings.Contains(res.Failures[0].Err, why) {
		t.Errorf("failures %+v, want one containing %q", res.Failures, why)
	}
}
