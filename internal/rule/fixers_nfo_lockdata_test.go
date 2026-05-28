package rule

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
)

// stubLockResolver lets the truth-table tests inject the four
// (artist.Locked x library.NFOLockData) cells without standing up the
// publisher's library lookup machinery.
type stubLockResolver struct{ value bool }

func (s stubLockResolver) ResolveLockNFO(_ context.Context, _ *artist.Artist) bool {
	return s.value
}

// TestNFOFixer_LockDataTruthTable enumerates the four cells of the
// (artist.Locked, library.NFOLockData) truth table the issue #1726 fix
// must honor. The resolver returns the OR of the two knobs in
// production; here we drive it directly with the precomputed OR so the
// test pins the fixer contract independently of the resolver's
// internals.
func TestNFOFixer_LockDataTruthTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		artistLocked bool
		libraryLock  bool
		want         bool
	}{
		{"both off", false, false, false},
		{"artist on, library off", true, false, true},
		{"artist off, library on", false, true, true},
		{"both on", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			a := &artist.Artist{
				Name:          "Truth Table",
				SortName:      "Truth Table",
				Path:          dir,
				LibraryID:     "lib-test",
				MusicBrainzID: "mbid-truth",
				Locked:        tc.artistLocked,
			}
			// Resolver returns the OR; the fixer must stamp the OR.
			f := &NFOFixer{
				fsCheck:      nonSharedFSCheck(),
				lockResolver: stubLockResolver{value: tc.artistLocked || tc.libraryLock},
			}
			res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleNFOExists})
			if err != nil {
				t.Fatalf("Fix: %v", err)
			}
			if !res.Fixed {
				t.Fatalf("Fixed=false, message=%q", res.Message)
			}
			data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
			if err != nil {
				t.Fatalf("read NFO: %v", err)
			}
			parsed, err := nfo.Parse(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("parse NFO: %v", err)
			}
			if parsed.LockData != tc.want {
				t.Errorf("LockData = %v, want %v (artist=%v library=%v)", parsed.LockData, tc.want, tc.artistLocked, tc.libraryLock)
			}
		})
	}
}

// TestNFOFixer_NoResolverFallsBackToArtistLocked guards the
// constructor's nil-resolver fallback. With no resolver, the fixer must
// stamp lockdata from artist.Locked alone -- never the old hardcoded
// true.
func TestNFOFixer_NoResolverFallsBackToArtistLocked(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		locked bool
	}{
		{"unlocked", false},
		{"locked", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			a := &artist.Artist{
				Name:          "Fallback",
				SortName:      "Fallback",
				Path:          dir,
				LibraryID:     "lib-test",
				MusicBrainzID: "mbid-fb",
				Locked:        tc.locked,
			}
			f := NewNFOFixer(nil, nil, nonSharedFSCheck(), nil, nil)
			res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleNFOExists})
			if err != nil {
				t.Fatalf("Fix: %v", err)
			}
			if !res.Fixed {
				t.Fatalf("Fixed=false, message=%q", res.Message)
			}
			data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
			if err != nil {
				t.Fatalf("read NFO: %v", err)
			}
			parsed, err := nfo.Parse(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("parse NFO: %v", err)
			}
			if parsed.LockData != tc.locked {
				t.Errorf("LockData = %v, want %v", parsed.LockData, tc.locked)
			}
		})
	}
}
