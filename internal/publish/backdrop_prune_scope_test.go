package publish

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// --- Scope (#3139) --------------------------------------------------------

// TestPruneScope_RefusesAnUnscopedRun. The publisher-side half of the
// either/or requirement: the guarantee must not depend on which caller
// reached it, so it is asserted here and not only at the handler.
//
// Each case ATTEMPTS the run against byte-identical backdrops a proceeding run
// WOULD delete, and demands both an error AND an untouched platform. The error
// alone would pass even if the run deleted first and complained after; "nothing
// was deleted" against a fixture with nothing to delete passes vacuously.
func TestPruneScope_RefusesAnUnscopedRun(t *testing.T) {
	dup := []byte("AAA")

	for _, tc := range []struct {
		name  string
		scope PlatformBackdropPruneScope
		want  error
	}{
		{"no scope at all", PlatformBackdropPruneScope{}, ErrPruneScopeMissing},
		{"both scopes", PlatformBackdropPruneScope{ArtistID: "a1", AllArtists: true}, ErrPruneScopeAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeBackdropClient{backdrops: [][]byte{dup, append([]byte(nil), dup...)}, failAt: -1, failDeleteAt: -1}
			p := newTestPublisherWithOneArtistOnePlatform(t, fake)

			res, err := p.PrunePlatformBackdropDuplicates(context.Background(), tc.scope)
			if err == nil {
				t.Fatal("want an error; an unusable scope on a destructive path must be a refusal, not a normalization")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is match against %v", err, tc.want)
			}
			// PRECONDITION: the fixture really was deletable.
			if len(fake.backdrops) != 2 {
				t.Fatalf("fixture no longer holds its two byte-identical backdrops: %d", len(fake.backdrops))
			}
			if len(fake.deleted) != 0 {
				t.Errorf("refused the run but deleted %v on the platform first", fake.deleted)
			}
			if res.BackdropsRemoved != 0 {
				t.Errorf("BackdropsRemoved = %d on a refused run, want 0", res.BackdropsRemoved)
			}
		})
	}
}

// TestPruneScope_AcceptsAValidScope is the counterweight to the refusal table
// above. Without it, a Validate that rejected EVERYTHING would pass every
// refusal case and the whole feature would be dead.
func TestPruneScope_AcceptsAValidScope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope PlatformBackdropPruneScope
	}{
		{"library-wide", PlatformBackdropPruneScope{AllArtists: true}},
		{"one artist", PlatformBackdropPruneScope{ArtistID: "a1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.scope.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

// scopedArtistLister maps DIFFERENT artists to DIFFERENT platform IDs, which
// the package's shared fakePlatformLister cannot do (its GetPlatformIDs
// ignores the artist ID entirely). Distinguishing the pruned artist from an
// untouched bystander is #3139's central acceptance criterion, and it is
// unassertable without per-artist mappings. Everything else is inherited, so
// this file adds only the two methods whose behavior the scope tests need.
type scopedArtistLister struct {
	*fakePlatformLister
	byArtist map[string][]artist.PlatformID
}

func (s *scopedArtistLister) GetPlatformIDs(_ context.Context, artistID string) ([]artist.PlatformID, error) {
	return s.byArtist[artistID], nil
}

func (s *scopedArtistLister) GetByID(_ context.Context, id string, _ ...artist.HydrateOpts) (*artist.Artist, error) {
	for i := range s.artists {
		if s.artists[i].ID == id {
			return &s.artists[i], nil
		}
	}
	return nil, errors.New("artist not found")
}

// routingBackdropClient dispatches on the platform artist ID, so a scoped
// run's effect on the TARGET and on a BYSTANDER can be measured separately in
// one run. The client factory is keyed by CONNECTION, so the routing has to
// happen inside the client.
type routingBackdropClient struct {
	fakes map[string]*fakeBackdropClient
}

func (r *routingBackdropClient) GetArtistDetail(ctx context.Context, pid string) (*connection.ArtistPlatformState, error) {
	return r.fakes[pid].GetArtistDetail(ctx, pid)
}

func (r *routingBackdropClient) GetArtistBackdrop(ctx context.Context, pid string, i int) ([]byte, string, error) {
	return r.fakes[pid].GetArtistBackdrop(ctx, pid, i)
}

func (r *routingBackdropClient) DeleteImageAtIndex(ctx context.Context, pid, kind string, i int) error {
	return r.fakes[pid].DeleteImageAtIndex(ctx, pid, kind, i)
}

// newScopedPruneTestPublisher wires two artists ("a1" the target, "a2" the
// bystander) onto one image-write-enabled Emby connection, each with its own
// fake platform state.
func newScopedPruneTestPublisher(t *testing.T, fakes map[string]*fakeBackdropClient) *Publisher {
	t.Helper()
	prev := backdropPruneClientFactory
	t.Cleanup(func() { backdropPruneClientFactory = prev })

	svc := &scopedArtistLister{
		fakePlatformLister: &fakePlatformLister{
			artists: []artist.Artist{{ID: "a1", Name: "Target"}, {ID: "a2", Name: "Bystander"}},
		},
		byArtist: map[string][]artist.PlatformID{
			"a1": {{ArtistID: "a1", ConnectionID: "c-emby", PlatformArtistID: "p1"}},
			"a2": {{ArtistID: "a2", ConnectionID: "c-emby", PlatformArtistID: "p2"}},
		},
	}
	router := &routingBackdropClient{fakes: fakes}
	backdropPruneClientFactory = func(*connection.Connection, *slog.Logger) backdropPruneClient { return router }

	conns := &fakeConnectionGetter{conns: map[string]*connection.Connection{
		"c-emby": {
			ID: "c-emby", Name: "emby", Type: connection.TypeEmby, Enabled: true, Status: "ok",
			Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true},
		},
	}}
	return New(Deps{
		ArtistService:     svc,
		ArtistLister:      svc,
		ArtistGetter:      svc,
		ConnectionService: conns,
		Logger:            silentLogger(),
	})
}

// dupPair returns two byte-identical backdrops -- a fixture that a proceeding
// prune will certainly reduce to one.
func dupPair() [][]byte {
	a := []byte("AAA")
	return [][]byte{a, append([]byte(nil), a...)}
}

// TestPruneScope_SingleArtistLeavesTheBystanderUntouched is #3139's central
// acceptance criterion, and it is asserted on the BYSTANDER as well as the
// target. Checking only that the target was pruned would pass just as happily
// if the run had swept the entire library.
func TestPruneScope_SingleArtistLeavesTheBystanderUntouched(t *testing.T) {
	target := &fakeBackdropClient{backdrops: dupPair(), failAt: -1, failDeleteAt: -1}
	// The bystander carries the SAME redundant shape, so it would certainly be
	// pruned by a library-wide run. A bystander with nothing to delete would
	// pass this test under either scoping.
	bystander := &fakeBackdropClient{backdrops: dupPair(), failAt: -1, failDeleteAt: -1}

	p := newScopedPruneTestPublisher(t, map[string]*fakeBackdropClient{"p1": target, "p2": bystander})

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(),
		PlatformBackdropPruneScope{ArtistID: "a1"})
	if err != nil {
		t.Fatalf("scoped prune: %v", err)
	}
	if res.BackdropsRemoved != 1 || len(target.deleted) != 1 {
		t.Errorf("target: removed %d, deleted %v; want 1 and one delete", res.BackdropsRemoved, target.deleted)
	}
	if len(bystander.deleted) != 0 {
		t.Errorf("BYSTANDER was pruned (%v) by a run scoped to a1; the scope did not narrow anything", bystander.deleted)
	}
	if len(bystander.backdrops) != 2 {
		t.Errorf("bystander has %d backdrops left, want its original 2", len(bystander.backdrops))
	}
}

// TestPruneScope_AllArtistsStillSweepsEveryone is the counterweight: a scope
// implementation that pruned only the first artist regardless would pass the
// bystander test above. Same fixtures, opposite scope, opposite expectation.
func TestPruneScope_AllArtistsStillSweepsEveryone(t *testing.T) {
	one := &fakeBackdropClient{backdrops: dupPair(), failAt: -1, failDeleteAt: -1}
	two := &fakeBackdropClient{backdrops: dupPair(), failAt: -1, failDeleteAt: -1}
	p := newScopedPruneTestPublisher(t, map[string]*fakeBackdropClient{"p1": one, "p2": two})

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(),
		PlatformBackdropPruneScope{AllArtists: true})
	if err != nil {
		t.Fatalf("library prune: %v", err)
	}
	if res.BackdropsRemoved != 2 {
		t.Errorf("BackdropsRemoved = %d, want 2 (both artists)", res.BackdropsRemoved)
	}
	if len(one.deleted) != 1 || len(two.deleted) != 1 {
		t.Errorf("deleted %v / %v, want one each", one.deleted, two.deleted)
	}
}

// TestPruneScope_UnknownArtistIsRefusedWithoutTouchingAnything. A scoped run
// naming an artist that does not exist must fail rather than fall through to
// a library-wide sweep -- the same fail-closed direction as a missing scope.
func TestPruneScope_UnknownArtistIsRefusedWithoutTouchingAnything(t *testing.T) {
	one := &fakeBackdropClient{backdrops: dupPair(), failAt: -1, failDeleteAt: -1}
	two := &fakeBackdropClient{backdrops: dupPair(), failAt: -1, failDeleteAt: -1}
	p := newScopedPruneTestPublisher(t, map[string]*fakeBackdropClient{"p1": one, "p2": two})

	if _, err := p.PrunePlatformBackdropDuplicates(context.Background(),
		PlatformBackdropPruneScope{ArtistID: "nope"}); err == nil {
		t.Fatal("want an error for an unknown artist")
	}
	if len(one.deleted) != 0 || len(two.deleted) != 0 {
		t.Errorf("an unresolvable scope still deleted: %v / %v", one.deleted, two.deleted)
	}
}

// TestPruneScope_ArtistScopeWithoutAGetterIsRefused. The artist-scoped branch
// needs artistGetter, which the library-wide branch does not, so an
// incompletely wired publisher must refuse the scoped run rather than fall
// through to the library sweep it CAN perform.
func TestPruneScope_ArtistScopeWithoutAGetterIsRefused(t *testing.T) {
	fake := &fakeBackdropClient{backdrops: dupPair(), failAt: -1, failDeleteAt: -1}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake) // wires no ArtistGetter

	if _, err := p.PrunePlatformBackdropDuplicates(context.Background(),
		PlatformBackdropPruneScope{ArtistID: "a1"}); err == nil {
		t.Fatal("want an error when the artist lookup is unwired")
	}
	if len(fake.deleted) != 0 {
		t.Errorf("an unwired scoped run still deleted %v", fake.deleted)
	}
}

// --- The exact tier's survivor is always the lowest index -----------------

// TestDedupBackdropIndices_SurvivorIsAlwaysBelowItsCandidate states executably
// the property that makes descending-index deletion sufficient on this tier.
//
// Both peers renumber every slot ABOVE a deleted one down by one. Deleting
// high-index-first protects the remaining CANDIDATES, but says nothing about
// the SURVIVOR, whose slot the pre-delete re-verify also reads. What closes
// that gap is that dedupBackdropIndices always keeps the FIRST index of each
// byte-identical group: the survivor sits below every candidate, so no delete
// can move it. A tier choosing its survivor any other way (largest, newest)
// would break this and would need slot numbers rewritten mid-loop.
func TestDedupBackdropIndices_SurvivorIsAlwaysBelowItsCandidate(t *testing.T) {
	h := func(b byte) [32]byte {
		var out [32]byte
		out[0] = b
		return out
	}
	for _, tc := range []struct {
		name   string
		hashes [][32]byte
	}{
		{"one group", [][32]byte{h('a'), h('a'), h('a')}},
		{"survivor first, distinct between", [][32]byte{h('a'), h('b'), h('a'), h('c'), h('a')}},
		{"two interleaved groups", [][32]byte{h('a'), h('b'), h('b'), h('a'), h('b')}},
		{"group starting late", [][32]byte{h('a'), h('b'), h('c'), h('c'), h('c')}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redundant := dedupBackdropIndices(tc.hashes)
			if len(redundant) == 0 {
				t.Fatal("fixture produced no redundant entries; the property would hold vacuously")
			}
			for _, rb := range redundant {
				if rb.Survivor >= rb.Index {
					t.Errorf("entry %+v: survivor is NOT below its candidate, so a delete below it would renumber it and the loop would need a shift", rb)
				}
				if tc.hashes[rb.Survivor] != rb.Hash {
					t.Errorf("entry %+v: survivor's hash differs from the candidate's on the EXACT tier", rb)
				}
				// The survivor must also be the LOWEST index carrying that
				// hash: merely "below" would still allow an intermediate copy
				// to be kept while a lower one was deleted.
				for i := 0; i < rb.Survivor; i++ {
					if tc.hashes[i] == rb.Hash {
						t.Errorf("entry %+v: index %d carries the same hash and is lower than the recorded survivor", rb, i)
					}
				}
			}
		})
	}
}

// TestPrune_SurvivingBytesAreTheKeptCopy closes the loop end to end: after a
// real run against a re-indexing platform, what REMAINS is one copy of the
// duplicated image plus every distinct image. A count alone cannot tell
// "kept the right copy" from "kept a copy".
func TestPrune_SurvivingBytesAreTheKeptCopy(t *testing.T) {
	dup, other, third := []byte("AAA"), []byte("BBB"), []byte("CCC")
	fake := &fakeBackdropClient{backdrops: [][]byte{dup, other, dup, third, dup}, failAt: -1, failDeleteAt: -1}
	p := newTestPublisherWithOneArtistOnePlatform(t, fake)

	res, err := p.PrunePlatformBackdropDuplicates(context.Background(),
		PlatformBackdropPruneScope{AllArtists: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.BackdropsRemoved != 2 {
		t.Fatalf("BackdropsRemoved = %d, want 2", res.BackdropsRemoved)
	}
	want := []string{"AAA", "BBB", "CCC"}
	if len(fake.backdrops) != len(want) {
		t.Fatalf("%d backdrops remain, want %d", len(fake.backdrops), len(want))
	}
	for i, w := range want {
		if string(fake.backdrops[i]) != w {
			t.Errorf("slot %d holds %q, want %q (full: %q)", i, fake.backdrops[i], w, fake.backdrops)
		}
	}
}
