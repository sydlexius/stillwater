package templates

// platform_state_rows_golden_test.go -- pins the rendered markup of the
// read-only platform-state card (all ten platformStateRow call sites) for two
// fixtures, so the positional-args -> options-struct refactor (#2214) cannot
// silently swap a field on any row. "mixed" gives rows distinct values and
// statuses; "matching" gives every editable row non-empty, case-differing but
// matching values (Match=true, BothEmpty=false, values distinguishable per
// side). The goldens were captured from the pre-refactor templates (45b8082a). Run with
// -update-platform-state-golden only after an intentional markup change.

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

var updatePlatformStateGolden = flag.Bool("update-platform-state-golden", false, "regenerate platform-state golden files")

func TestPlatformStateCardReadOnly_Golden(t *testing.T) {
	t.Parallel()
	conn := &connection.Connection{Name: "Golden Emby", Type: connection.TypeEmby}
	cases := map[string]struct {
		a     *artist.Artist
		state *connection.ArtistPlatformState
	}{
		// Mismatches (biography, genres, sort name, MusicBrainz ID: the two
		// sides differ), both-empty rows (dates), and the platform-only rows.
		"mixed": {
			a: &artist.Artist{
				ID: "art-1", Name: "Golden", SortName: "Golden",
				Biography: "Our bio", Genres: []string{"Rock", "Pop"},
				Formed: "1990", MusicBrainzID: "mbid-1",
			},
			state: &connection.ArtistPlatformState{
				SortName: "Golden, The", Biography: "Their bio", Genres: []string{"Rock"},
				MusicBrainzID: "mbid-2", Tags: []string{"a", "b"}, BackdropCount: 3,
				IsLocked: true, LockedFields: []string{"biography"}, HasThumb: true,
			},
		},
		// Every editable row matches case-insensitively with differing case.
		"matching": {
			a: &artist.Artist{
				ID: "art-1", Name: "Golden", SortName: "SORT",
				Biography: "BIO", Genres: []string{"ROCK"},
				Formed: "FORMED", Disbanded: "DISBANDED", MusicBrainzID: "MBID-1",
			},
			state: &connection.ArtistPlatformState{
				SortName: "sort", Biography: "bio", Genres: []string{"rock"},
				PremiereDate: "formed", EndDate: "disbanded", MusicBrainzID: "mbid-1",
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := PlatformStateCardReadOnly(tc.a, conn, tc.state, "").Render(testCtx(t), &buf); err != nil {
				t.Fatalf("render: %v", err)
			}
			path := filepath.Join("testdata", "platform_state_readonly_"+name+".golden.html")
			if *updatePlatformStateGolden {
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			golden, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with -update-platform-state-golden)", path, err)
			}
			if !bytes.Equal(golden, buf.Bytes()) {
				t.Errorf("platform-state markup drifted from golden %s: %s", path, describeDrift(golden, buf.Bytes()))
			}
		})
	}
}

// describeDrift locates the first differing byte between two renders and shows
// ~80 bytes of each around it. Goldens are single-line, so a bare path says
// nothing about which row drifted.
func describeDrift(want, got []byte) string {
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	ctx := func(b []byte) []byte { return b[max(i-80, 0):min(i+80, len(b))] }
	return fmt.Sprintf("first difference at byte %d (want len %d, got len %d)\n want: ...%s...\n  got: ...%s...", i, len(want), len(got), ctx(want), ctx(got))
}
