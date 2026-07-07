package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
)

// #2306: WriteBackNFO must CREATE a missing artist.nfo (from the artist's
// current metadata), gated by the active platform profile -- Plex (NFOEnabled
// false) is skipped, every other profile writes, and an unresolved profile
// fails open (writes).
func TestWriteBackNFO_MissingNFO_CreateGatedByProfile(t *testing.T) {
	cases := []struct {
		name     string
		provider activeProfileProvider // nil = no service = fail-open
		wantFile bool
	}{
		{"no profile service: fail-open create", nil, true},
		{"emby enabled: create", &fakePlatformProvider{profile: &platform.Profile{Name: "Emby", NFOEnabled: true}}, true},
		{"plex disabled: skip", &fakePlatformProvider{profile: &platform.Profile{Name: "Plex", NFOEnabled: false}}, false},
		{"getactive error: fail-open create (no nil-deref panic)", &fakePlatformProvider{err: errors.New("db down")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeArtistDir(t, "") // no artist.nfo present
			ew := newRecordingExpectedWrites()
			deps := Deps{Logger: silentLogger(), ExpectedWrites: ew}
			deps.PlatformService = tc.provider
			p := New(deps)

			p.WriteBackNFO(context.Background(), &artist.Artist{ID: "a", Path: dir, Name: "Schumann"})

			_, statErr := os.Stat(filepath.Join(dir, "artist.nfo"))
			gotFile := statErr == nil
			if gotFile != tc.wantFile {
				t.Fatalf("artist.nfo exists=%v want %v (stat err=%v)", gotFile, tc.wantFile, statErr)
			}
			if tc.wantFile {
				data, err := os.ReadFile(filepath.Join(dir, "artist.nfo"))
				if err != nil {
					t.Fatalf("read created NFO: %v", err)
				}
				if !strings.Contains(string(data), "Schumann") {
					t.Errorf("created NFO missing artist name; got:\n%s", data)
				}
			}
		})
	}
}
