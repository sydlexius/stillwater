package rule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
)

// stubActiveProfile injects an active platform profile into NFOFixer without a
// real platform.Service / DB.
type stubActiveProfile struct {
	prof *platform.Profile
	err  error
}

func (s stubActiveProfile) GetActive(context.Context) (*platform.Profile, error) {
	return s.prof, s.err
}

// #2306: NFOFixer must not create an artist.nfo when the active platform profile
// has NFO writing disabled (Plex, nfo_enabled=0), and must write for a profile
// that has it enabled.
func TestNFOFixer_ProfileGate(t *testing.T) {
	cases := []struct {
		name      string
		prof      *platform.Profile
		err       error
		wantFixed bool
		wantFile  bool
	}{
		{"plex disabled: skip", &platform.Profile{Name: "Plex", NFOEnabled: false}, nil, false, false},
		{"emby enabled: write", &platform.Profile{Name: "Emby", NFOEnabled: true}, nil, true, true},
		{"getactive error: fail-open write (no nil-deref panic)", nil, errors.New("db down"), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a := &artist.Artist{
				Name:          "Gate",
				SortName:      "Gate",
				Path:          dir,
				LibraryID:     "lib-test",
				MusicBrainzID: "mbid-gate",
			}
			f := &NFOFixer{
				fsCheck:         nonSharedFSCheck(),
				lockResolver:    stubLockResolver{value: false},
				platformService: stubActiveProfile{prof: tc.prof, err: tc.err},
			}
			res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleNFOExists})
			if err != nil {
				t.Fatalf("Fix: %v", err)
			}
			if res.Fixed != tc.wantFixed {
				t.Errorf("Fixed=%v want %v (msg=%q)", res.Fixed, tc.wantFixed, res.Message)
			}
			_, statErr := os.Stat(filepath.Join(dir, "artist.nfo"))
			gotFile := statErr == nil
			if gotFile != tc.wantFile {
				t.Errorf("artist.nfo exists=%v want %v", gotFile, tc.wantFile)
			}
		})
	}
}
