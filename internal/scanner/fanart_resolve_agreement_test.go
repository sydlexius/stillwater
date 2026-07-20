package scanner

import (
	"os"
	"path/filepath"
	"testing"

	swimage "github.com/sydlexius/stillwater/internal/image"
)

// TestResolveFanartFiles_MatchesScanner is the agreement oracle between the
// scanner's discovery and the registry repair's.
//
// The repair (internal/maintenance) rebuilds artist_images rows from files on
// disk, and its slot_index values are image.ResolveFanartFiles ordinals. The
// scanner's discoverFanartFiles produces the count that BOUNDS deleteStaleSlots:
// a row survives reconcile only while slot_index < found. So if the two
// discoveries ever disagree about how many fanart files a directory holds, the
// repair writes rows at ordinals the next scan considers out of range and
// deletes them -- a repair whose output the scanner destroys, which is worse
// than no repair at all.
//
// Pinning them against a shared fixture set makes that divergence a build
// failure rather than a silent data loss. The fixture list deliberately
// includes the orphan-numbered-variant shape (no primary on disk), because
// that is the case pass 2 exists for and the one a pass-1-only repair would
// walk straight past.
func TestResolveFanartFiles_MatchesScanner(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"fanart.jpg"},
		{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"},
		{"fanart1.jpg", "fanart2.jpg"},
		{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg"},
		{"backdrop2.jpg"},
		{"fanart.jpg", "backdrop.jpg", "backdrop2.jpg"},
		{"fanart.jpg", "fanart3.jpg"},
		{"fanart.png", "fanart1.png"},
		// Extension allowlist. ".jpeg" is NOT in fanartPatterns, so a
		// pattern-driven matcher silently drops it; DiscoverFanart accepts it.
		{"fanart.jpeg"},
		{"fanart.jpeg", "fanart2.jpeg"},
		// Two extensions both claiming ordinal 0 -- the extension-preference
		// sort is what stops this double-counting.
		{"fanart.jpg", "fanart.png"},
		{"backdrop.jpg", "backdrop.png", "backdrop2.jpg"},
		// Excluded extensions must not be counted.
		{"fanart.gif", "fanart.webp", "fanart.bmp"},
		// Numbered parse: Atoi must succeed AND n > 0.
		{"fanart.jpg", "fanartx.jpg", "fanart-alt.jpg"},
		{"fanart.jpg", "fanart0.jpg"},
		{"folder.jpg", "logo.png", "banner.jpg"},
		{"Fanart.JPG", "FANART1.jpg"},
		{},
	}

	for _, files := range cases {
		t.Run(name(files), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for _, n := range files {
				if err := os.WriteFile(filepath.Join(dir, n), []byte("test"), 0o644); err != nil {
					t.Fatalf("writing %s fixture: %v", n, err)
				}
			}

			got, err := swimage.ResolveFanartFiles(dir, fanartPatterns)
			if err != nil {
				t.Fatalf("ResolveFanartFiles: %v", err)
			}
			want := discoverFanartFiles(dir, readDirListing(t, dir))

			if len(got) != len(want) {
				t.Fatalf("ResolveFanartFiles = %v (%d), discoverFanartFiles = %v (%d): the "+
					"repair and the scanner disagree on how many fanart files are on disk, "+
					"and that count is the bound deleteStaleSlots deletes by",
					got, len(got), want, len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("ordinal %d: ResolveFanartFiles = %s, discoverFanartFiles = %s; "+
						"slot_index is this position, so a mismatch writes the wrong row",
						i, got[i], want[i])
				}
			}
		})
	}
}

func name(files []string) string {
	if len(files) == 0 {
		return "empty dir"
	}
	out := files[0]
	if len(files) > 1 {
		out += "+more"
	}
	return out
}
