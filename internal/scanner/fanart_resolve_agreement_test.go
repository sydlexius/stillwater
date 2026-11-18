package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	swimage "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/rule"
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
	for _, files := range fanartAgreementCases {
		t.Run(name(files), func(t *testing.T) {
			t.Parallel()
			dir := writeFanartAgreementDir(t, files)

			got, err := swimage.ResolveFanartFiles(context.Background(), dir, fanartPatterns)
			if err != nil {
				t.Fatalf("ResolveFanartFiles: %v", err)
			}
			assertSameOrdinals(t, "ResolveFanartFiles", got, discoverFanartFiles(dir, readDirListing(t, dir)))
		})
	}
}

// TestResolveScannerFanart_MatchesScanner pins rule.ResolveScannerFanart (the
// resolver duplicate detection, the hash backfill's counterpart, and the
// duplicate fixer's delete walk all share) to the scanner's discoverFanartFiles,
// over every shape the existing oracle covers PLUS the shapes where
// image.ResolveFanart disagrees with the scanner: a .jpeg is never a pass-1
// primary, and the per-ordinal extension follows the pattern that matched
// (#2632).
//
// REVERT-AND-RERUN: pointing ResolveScannerFanart back at image.ResolveFanart
// must turn the four extra shapes RED.
func TestResolveScannerFanart_MatchesScanner(t *testing.T) {
	t.Parallel()
	cases := append([][]string{
		{"fanart.jpeg", "fanart2.jpeg", "backdrop.jpg", "backdrop2.jpg"},
		{"fanart.jpeg", "backdrop.jpg"},
		{"fanart.png", "fanart2.jpg", "fanart2.png"},
		{"backdrop.jpeg", "fanart1.jpg"},
	}, fanartAgreementCases...)
	for _, files := range cases {
		t.Run(strings.Join(files, ","), func(t *testing.T) {
			t.Parallel()
			dir := writeFanartAgreementDir(t, files)
			matched, got, err := rule.ResolveScannerFanart(context.Background(), dir)
			if err != nil {
				t.Fatalf("ResolveScannerFanart: %v", err)
			}
			assertSameOrdinals(t, "ResolveScannerFanart", got, discoverFanartFiles(dir, readDirListing(t, dir)))
			assertMatchedConvention(t, matched, got)
		})
	}
}

// assertMatchedConvention pins the matched pattern the duplicate fixer renumbers
// under. RenumberFanart takes its base from that name, so it must describe the
// same files the resolver returned: an empty result has no matched name, and a
// non-empty result has a matched base equal to the base of its first file.
func assertMatchedConvention(t *testing.T, matched string, got []string) {
	t.Helper()
	if len(got) == 0 {
		if matched != "" {
			t.Errorf("matched name = %q for an empty result; want \"\"", matched)
		}
		return
	}
	if matched == "" {
		t.Fatalf("matched name is \"\" but %d files resolved (first %s)", len(got), filepath.Base(got[0]))
	}
	want := fanartConventionBase(filepath.Base(got[0]))
	if gotBase := fanartConventionBase(matched); !strings.EqualFold(gotBase, want) {
		t.Errorf("matched name %q has base %q, but the first resolved file %s has base %q; "+
			"the renumber would rename survivors under the wrong convention",
			matched, gotBase, filepath.Base(got[0]), want)
	}
}

// fanartConventionBase strips the extension and then the numeric ordinal suffix,
// leaving the convention base (backdrop2.jpg -> backdrop).
func fanartConventionBase(file string) string {
	stem := strings.TrimSuffix(file, filepath.Ext(file))
	return strings.TrimRight(stem, "0123456789")
}

func writeFanartAgreementDir(t *testing.T, files []string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("test"), 0o644); err != nil {
			t.Fatalf("writing %s fixture: %v", n, err)
		}
	}
	return dir
}

func assertSameOrdinals(t *testing.T, who string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v (%d), discoverFanartFiles = %v (%d): the resolver and the scanner "+
			"disagree on how many fanart files are on disk, and that count is the bound "+
			"deleteStaleSlots deletes by", who, got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ordinal %d: %s = %s, discoverFanartFiles = %s; slot_index is this "+
				"position, so a mismatch writes the wrong row", i, who, got[i], want[i])
		}
	}
}

// fanartAgreementCases are the directory shapes shared by the agreement oracles.
var fanartAgreementCases = [][]string{
	{"fanart.jpg"},
	{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"},
	{"fanart1.jpg", "fanart2.jpg"},
	{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg"},
	{"backdrop2.jpg"},
	// Orphan-only, multi-file, backdrop convention: pass 2 must report the
	// backdrop name, not the first candidate (fanart), or a renumber would move
	// these survivors to fanart*.jpg.
	{"backdrop2.jpg", "backdrop3.jpg", "backdrop4.jpg"},
	{"fanart.jpg", "backdrop.jpg", "backdrop2.jpg"},
	{"fanart.jpg", "fanart3.jpg"},
	{"fanart.png", "fanart1.png"},
	// ".jpeg" is NOT in fanartPatterns, so pass 1's pattern scan finds no
	// primary here and must fall through to pass 2 on both sides (both now
	// delegate to image.DiscoverFanartFrom, so this is a two-pass
	// fall-through check, not an allowlist cross-check -- the allowlist
	// itself is pinned absolutely by TestDiscoverFanart_JpegExtensionAccepted).
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
