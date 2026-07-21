package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
)

// activateFanartProfile creates a platform profile whose fanart naming lists
// only the given names and makes it active, so a test controls exactly which
// convention the handler believes the library uses.
func activateFanartProfile(t *testing.T, svc *platform.Service, names ...string) {
	t.Helper()
	ctx := context.Background()
	p := &platform.Profile{
		Name:       "Convention Under Test",
		NFOEnabled: true,
		NFOFormat:  "kodi",
		ImageNaming: platform.ImageNaming{
			Thumb:  []string{"folder.jpg"},
			Fanart: names,
			Logo:   []string{"logo.png"},
			Banner: []string{"banner.jpg"},
		},
	}
	if err := svc.Create(ctx, p); err != nil {
		t.Fatalf("creating platform profile: %v", err)
	}
	if err := svc.SetActive(ctx, p.ID); err != nil {
		t.Fatalf("activating platform profile: %v", err)
	}

	// PRECONDITION: the profile really is active and really reports the names
	// under test. Without it the assertions below could pass because the
	// handler read some other profile entirely.
	active, err := svc.GetActive(ctx)
	if err != nil || active == nil {
		t.Fatalf("GetActive after SetActive: profile=%v err=%v", active, err)
	}
	if got := active.ImageNaming.NamesForType("fanart"); len(got) != len(names) || got[0] != names[0] {
		t.Fatalf("active profile fanart naming = %v, want %v", got, names)
	}
}

// TestUpdateArtistFanartCount_ProfileConventionMismatchStillCounts is the
// convention-agnostic enumeration guard (#2635).
//
// The artist's library holds fanart.jpg / fanart1.jpg. The active platform
// profile says the fanart convention is backdrop.*. Resolving the enumeration
// from that profile's primary alone finds NOTHING -- and finds it without an
// error, because the directory read succeeds and simply contains no
// backdrop*.jpg.
//
// The profile states what Stillwater WRITES; it is not evidence about what the
// library already HOLDS. An enumeration built from it is not a measurement of
// the directory, and a count of zero derived that way is a positive claim that
// no artwork exists.
//
// This asserts the COUNT rather than surviving registry rows, deliberately.
// updateArtistFanartCount does not yet reconcile the registry on this branch,
// so a row-survival assertion would pass whatever the count said and guard
// nothing. The count IS the enumeration, and it is what a reconcile will later
// consume as its delete bound -- see the sibling branch that adds it.
func TestUpdateArtistFanartCount_ProfileConventionMismatchStillCounts(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a, dir, _ := seedPrimaryFanart(t, svc, "Convention Mismatch")
	ctx := context.Background()

	// The library uses the fanart.* convention. seedPrimaryFanart wrote
	// fanart.jpg; add a second so an undercount is distinguishable from a
	// legitimately single-slot artist.
	if err := os.WriteFile(filepath.Join(dir, "fanart1.jpg"), distinctJPEG(t, 11), 0o644); err != nil {
		t.Fatalf("seeding fanart1.jpg: %v", err)
	}

	// The install's profile expects the OTHER convention entirely.
	activateFanartProfile(t, r.platformService, "backdrop.jpg", "backdrop.png")

	// PRECONDITION: no file matching the profile's convention exists, so a
	// profile-only resolution genuinely finds nothing to count.
	if _, err := os.Stat(filepath.Join(dir, "backdrop.jpg")); !os.IsNotExist(err) {
		t.Fatalf("precondition: no backdrop.jpg may exist, stat err = %v", err)
	}

	r.updateArtistFanartCount(ctx, a)

	if a.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want 2: the profile expects backdrop.jpg but the "+
			"library holds fanart.jpg/fanart1.jpg, and both are on disk. Enumeration "+
			"must resolve the convention the library HOLDS, not the one the profile "+
			"writes -- a zero here is a positive claim that the artwork is gone",
			a.FanartCount)
	}
	if !a.FanartExists {
		t.Error("FanartExists = false against a directory holding two fanart files")
	}
}

// TestUpdateArtistFanartCount_UnresolvableProfileDoesNotGuess covers the other
// half of the same guard: a profile lookup that FAILS.
//
// getActiveFanartPrimary substitutes the built-in defaults whenever GetActive
// returns an error. That is fine for its read-only callers and wrong for
// enumeration: a guessed convention is not evidence, and if the guess misses
// the library's convention the walk comes up empty. "We could not determine the
// naming convention" and "there is no artwork" are different answers and must
// not share a representation.
//
// The library here uses backdrop.*, which is what makes the guess wrong: the
// fallback guesses DefaultFileNames["fanart"][0] == "fanart.jpg". A fanart.*
// fixture would be rescued by the guess and this test would guard nothing.
func TestUpdateArtistFanartCount_UnresolvableProfileDoesNotGuess(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	ctx := context.Background()

	dir := t.TempDir()
	a := &artist.Artist{Name: "Unresolvable Profile", SortName: "Unresolvable Profile", Path: dir}
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	for _, name := range []string{"backdrop.jpg", "backdrop2.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), distinctJPEG(t, 11), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}
	activateFanartProfile(t, r.platformService, "backdrop.jpg", "backdrop.png")

	// Establish the true count first, so the assertion below distinguishes
	// "refused and left it alone" from "never set it".
	r.updateArtistFanartCount(ctx, a)
	if a.FanartCount != 2 {
		t.Fatalf("precondition: want FanartCount 2 before the lookup breaks, got %d", a.FanartCount)
	}

	// Break the profile lookup while leaving the artist and image tables
	// intact, so only the convention resolution fails.
	if _, err := r.db.Exec(`DROP TABLE platform_profiles`); err != nil {
		t.Fatalf("dropping platform_profiles: %v", err)
	}
	if _, err := r.platformService.GetActive(ctx); err == nil {
		t.Fatal("precondition: GetActive still succeeds, so the refusal path is never reached")
	}

	r.updateArtistFanartCount(ctx, a)

	if a.FanartCount != 2 {
		t.Errorf("FanartCount = %d, want the measured 2 left untouched: the naming "+
			"convention could not be resolved, so no honest enumeration exists. "+
			"Guessing a convention and reporting what it happens to find is a "+
			"fabricated measurement", a.FanartCount)
	}
}
