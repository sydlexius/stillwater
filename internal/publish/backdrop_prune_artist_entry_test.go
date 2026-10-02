package publish

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	img "github.com/sydlexius/stillwater/internal/image"
)

// perArtistLister answers GetPlatformIDs for the asked artist only, as the
// real artist service does; the shared fake returns every mapping.
type perArtistLister struct{ *fakePlatformLister }

func (l perArtistLister) GetPlatformIDs(_ context.Context, artistID string) ([]artist.PlatformID, error) {
	var out []artist.PlatformID
	for _, id := range l.ids {
		if id.ArtistID == artistID {
			out = append(out, id)
		}
	}
	return out, nil
}

// byItemClient routes each call to the fake holding that platform artist.
type byItemClient map[string]*fakeBackdropClient

func (c byItemClient) GetArtistDetail(ctx context.Context, id string) (*connection.ArtistPlatformState, error) {
	return c[id].GetArtistDetail(ctx, id)
}
func (c byItemClient) GetArtistBackdrop(ctx context.Context, id string, i int) ([]byte, string, error) {
	return c[id].GetArtistBackdrop(ctx, id, i)
}
func (c byItemClient) DeleteImageAtIndex(ctx context.Context, id, typ string, i int) error {
	return c[id].DeleteImageAtIndex(ctx, id, typ, i)
}

// failingClient fails the test on the first platform call (always a detail read).
type failingClient struct {
	backdropPruneClient
	t *testing.T
}

func (f failingClient) GetArtistDetail(context.Context, string) (*connection.ArtistPlatformState, error) {
	f.t.Fatal("platform call before the tolerance was validated")
	return nil, nil
}

// pruneFor runs the entry point with the store holding stored, as the DB would.
func pruneFor(p *Publisher, stored *artist.Artist, opts ArtistBackdropPruneOptions) (PlatformBackdropPruneResult, error) {
	p.artistGetter = &fakeArtistGetter{artists: map[string]*artist.Artist{stored.ID: stored}}
	return p.PrunePlatformBackdropsForArtist(context.Background(), &artist.Artist{ID: stored.ID}, opts)
}

func TestPruneForArtist_TouchesOnlyThatArtist(t *testing.T) {
	f := newPerceptualFixture(t)
	mine := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big}, failAt: -1, failDeleteAt: -1}
	theirs := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, byItemClient{"p1": mine, "p2": theirs})
	l := fixtureArtist(t, p)
	l.ids = append(l.ids, artist.PlatformID{ArtistID: "a2", ConnectionID: "c-emby", PlatformArtistID: "p2"})
	l.artists = append(l.artists, artist.Artist{ID: "a2", Path: l.artists[0].Path})
	p.artistService = perArtistLister{l}

	res, err := pruneFor(p, &l.artists[0], ArtistBackdropPruneOptions{Perceptual: true, Tolerance: img.DefaultDuplicateTolerance})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	assertPlatform(t, mine, f.big)
	assertPlatform(t, theirs, f.small, f.big)
	if res.BackdropsRemoved != 1 || len(res.Failures) != 0 || len(res.Skipped) != 0 {
		t.Errorf("result %+v, want one removal and nothing failed or skipped", res)
	}
}

// Every policy cause turns the perceptual tier off, is reported in Skipped
// with its reason, and leaves the exact tier running. Failures keeps its
// pre-Skipped shape: every cause there except the locked artist.
func TestPruneForArtist_PolicySkipsAreTyped(t *testing.T) {
	f := newPerceptualFixture(t)
	for reason, setup := range map[string]func(t *testing.T, p *Publisher, a *artist.Artist){
		PruneSkipLockedArtist: func(_ *testing.T, _ *Publisher, a *artist.Artist) { a.Locked = true },
		PruneSkipProtectedFanart: func(_ *testing.T, p *Publisher, _ *artist.Artist) {
			p.artistImages = &fakeArtistImages{rows: []artist.ArtistImage{{ImageType: "fanart", Locked: true}}}
		},
		PruneSkipNoLocalFolder:    func(_ *testing.T, _ *Publisher, a *artist.Artist) { a.Path = "" },
		PruneSkipEmptyLocalFolder: func(t *testing.T, _ *Publisher, a *artist.Artist) { a.Path = t.TempDir() },
		PruneSkipUnreadableLocal: func(t *testing.T, _ *Publisher, a *artist.Artist) {
			a.Path = t.TempDir()
			if err := os.Symlink(filepath.Join(a.Path, "missing.jpg"), filepath.Join(a.Path, "fanart.jpg")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(reason, func(t *testing.T) {
			fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big, f.big}, failAt: -1, failDeleteAt: -1}
			p := perceptualPublisher(t, fake)
			a := fixtureArtist(t, p).artists[0]
			setup(t, p, &a)

			res, err := pruneFor(p, &a, ArtistBackdropPruneOptions{Perceptual: true, Tolerance: img.DefaultDuplicateTolerance})
			if err != nil {
				t.Fatalf("prune: %v", err)
			}
			assertPlatform(t, fake, f.small, f.big) // exact twin gone, perceptual pair kept
			if len(res.Skipped) != 1 || res.Skipped[0] != (PlatformBackdropPruneSkip{ArtistID: "a1", Reason: reason}) {
				t.Errorf("skipped %+v, want one %q for a1", res.Skipped, reason)
			}
			if inFail := reason != PruneSkipLockedArtist; (len(res.Failures) == 1) != inFail {
				t.Errorf("failures %+v, want reported there: %v", res.Failures, inFail)
			}
		})
	}
}

// The tolerance reaches the perceptual comparison: a pair measured inside
// [0.90, 0.95) is one picture at 0.90 and two at 0.95.
func TestPruneForArtist_ToleranceIsThreaded(t *testing.T) {
	a, b := fieldJPEG(t, 7, 1), blendJPEG(t, 7, 13, 0.20, 1)
	if s := similarity(t, a, b); s < 0.90 || s >= 0.95 {
		t.Fatalf("fixture precondition: similarity %.4f, want within [0.90, 0.95)", s)
	}
	for tol, left := range map[float64]int{0.90: 1, 0.95: 2} {
		fake := &fakeBackdropClient{backdrops: [][]byte{a, b}, failAt: -1, failDeleteAt: -1}
		p := perceptualPublisher(t, fake)
		if _, err := pruneFor(p, &fixtureArtist(t, p).artists[0], ArtistBackdropPruneOptions{Perceptual: true, Tolerance: tol}); err != nil {
			t.Fatalf("prune at %.2f: %v", tol, err)
		}
		if len(fake.backdrops) != left {
			t.Errorf("at tolerance %.2f the platform holds %d backdrops, want %d", tol, len(fake.backdrops), left)
		}
	}
}

func TestPruneForArtist_InvalidToleranceRefusedBeforePlatformIO(t *testing.T) {
	for _, tol := range []float64{math.NaN(), 0, -0.5, 1.5} {
		p := perceptualPublisher(t, failingClient{t: t})
		_, err := pruneFor(p, &fixtureArtist(t, p).artists[0], ArtistBackdropPruneOptions{Perceptual: true, Tolerance: tol})
		if !errors.Is(err, ErrPruneToleranceInvalid) {
			t.Errorf("tolerance %v: err %v, want ErrPruneToleranceInvalid", tol, err)
		}
	}
}

// The caller's snapshot says unlocked; the store says locked. The store wins,
// so a lock set after the caller read the artist still keeps it exact-only.
func TestPruneForArtist_ReloadsTheArtist(t *testing.T) {
	f := newPerceptualFixture(t)
	fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big}, failAt: -1, failDeleteAt: -1}
	p := perceptualPublisher(t, fake)
	stored := fixtureArtist(t, p).artists[0]
	stored.Locked = true
	p.artistGetter = &fakeArtistGetter{artists: map[string]*artist.Artist{"a1": &stored}}
	stale := fixtureArtist(t, p).artists[0] // Locked: false

	res, err := p.PrunePlatformBackdropsForArtist(context.Background(), &stale,
		ArtistBackdropPruneOptions{Perceptual: true, Tolerance: img.DefaultDuplicateTolerance})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	assertPlatform(t, fake, f.small, f.big)
	if len(res.Skipped) != 1 || res.Skipped[0].Reason != PruneSkipLockedArtist {
		t.Errorf("skipped %+v, want locked_artist from the stored record", res.Skipped)
	}
	p.artistGetter = &fakeArtistGetter{artists: map[string]*artist.Artist{}}
	if _, err := p.PrunePlatformBackdropsForArtist(context.Background(), &stale,
		ArtistBackdropPruneOptions{Tolerance: 0.9}); err == nil || len(fake.deleted) != 0 {
		t.Errorf("missing artist: err %v deleted %v, want an error and no delete", err, fake.deleted)
	}
}

// The options reach the run: DryRun deletes nothing, Perceptual off keeps the
// near-duplicate, and 1.0 is a valid tolerance (identical hashes only).
func TestPruneForArtist_OptionsReachTheRun(t *testing.T) {
	f := newPerceptualFixture(t)
	run := func(opts ArtistBackdropPruneOptions) (*fakeBackdropClient, PlatformBackdropPruneResult) {
		fake := &fakeBackdropClient{backdrops: [][]byte{f.small, f.big, f.big}, failAt: -1, failDeleteAt: -1}
		p := perceptualPublisher(t, fake)
		res, err := pruneFor(p, &fixtureArtist(t, p).artists[0], opts)
		if err != nil {
			t.Fatalf("%+v: %v", opts, err)
		}
		return fake, res
	}
	fake, res := run(ArtistBackdropPruneOptions{Perceptual: true, DryRun: true, Tolerance: 0.9})
	assertPlatform(t, fake, f.small, f.big, f.big)
	if !res.DryRun || res.BackdropsRemoved != 0 || len(res.Plan) != 2 {
		t.Errorf("dry run %+v, want echoed, nothing removed, 2 planned", res)
	}
	fake, _ = run(ArtistBackdropPruneOptions{Tolerance: 0.9})
	assertPlatform(t, fake, f.small, f.big)
	fake, _ = run(ArtistBackdropPruneOptions{Perceptual: true, Tolerance: 1}) // small and big hash identically
	assertPlatform(t, fake, f.big)
}
