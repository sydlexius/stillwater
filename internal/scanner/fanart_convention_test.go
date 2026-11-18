package scanner

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	swimage "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/rule"
)

// conventionJPEG encodes a gradient. seed shifts the colors so two seeds are
// visually distinct; quality varies the bytes without changing the picture, so
// the same seed at two qualities is a perceptual (not exact) duplicate.
func conventionJPEG(t *testing.T, seed, quality int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 640, 360))
	for y := range 360 {
		for x := range 640 {
			v := uint8((x*255/640 + seed) % 256)
			im.Set(x, y, color.RGBA{R: v, G: 255 - v, B: uint8((y * 255 / 360) ^ seed), A: 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("encoding jpeg: %v", err)
	}
	return b.Bytes()
}

func writeConv(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// scanThenDupScan runs a REAL scan over libDir (so artist_images rows come from
// the scanner, not hand-inserted), optionally activates the Emby profile (whose
// fanart primary is backdrop.jpg; nil platform service means the fanart.jpg
// default), and runs ScanFanartDuplicates. It returns the report and the
// artist's fanart row names-by-slot count for the precondition check.
func scanThenDupScan(t *testing.T, libDir, artistDir string, emby bool) (rule.FanartDupReport, int) {
	t.Helper()
	rep, rows, _, _ := scanThenDupScanDB(t, libDir, artistDir, emby)
	return rep, rows
}

// scanThenDupScanDB is scanThenDupScan that also returns the database and the
// artist id, for tests that inspect the rows the scan persisted.
func scanThenDupScanDB(t *testing.T, libDir, artistDir string, emby bool) (rule.FanartDupReport, int, *sql.DB, string) {
	t.Helper()
	svc, artistSvc, db := setupScannerWithDB(t, libDir)
	ctx := context.Background()
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	waitForScan(t, svc, 10*time.Second)

	var psvc *platform.Service
	if emby {
		psvc = platform.NewService(db)
		prof, err := psvc.GetByName(ctx, "Emby")
		if err != nil {
			t.Fatalf("loading Emby profile: %v", err)
		}
		if err := psvc.SetActive(ctx, prof.ID); err != nil {
			t.Fatalf("activating Emby profile: %v", err)
		}
	}
	a, err := artistSvc.GetByPath(ctx, artistDir)
	if err != nil || a == nil {
		t.Fatalf("artist not found after scan: %v", err)
	}
	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_images WHERE artist_id = ? AND image_type = 'fanart' AND exists_flag = 1`, a.ID).Scan(&rows); err != nil {
		t.Fatalf("counting fanart rows: %v", err)
	}

	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	e := rule.NewEngine(nil, db, psvc, nil, lg)
	e.SetImageHashRecorder(artistSvc)
	rep, err := rule.NewPipeline(e, artistSvc, nil, nil, nil, lg).ScanFanartDuplicates(ctx)
	if err != nil {
		t.Fatalf("ScanFanartDuplicates: %v", err)
	}
	return rep, rows, db, a.ID
}

// TestScanFanartDuplicates_OtherConvention is the #2632 guard. The scanner
// registers fanart under every convention, so rows exist for files named by the
// convention the ACTIVE profile does not use; duplicate detection must find
// them rather than silently excluding them.
//
// REVERT-AND-RERUN: resolving with DiscoverFanart(profile primary) in
// discoverFanartForDup must turn every subtest RED.
func TestScanFanartDuplicates_OtherConvention(t *testing.T) {
	cases := []struct {
		name   string
		base   string // convention of the files on disk
		emby   bool   // active profile (Emby primary = backdrop.jpg, default = fanart.jpg)
		second func(t *testing.T) []byte
		exact  int
		perc   int
	}{
		{"exact backdrop files under fanart profile", "backdrop", false, func(t *testing.T) []byte { return conventionJPEG(t, 0, 90) }, 1, 0},
		{"exact fanart files under backdrop profile", "fanart", true, func(t *testing.T) []byte { return conventionJPEG(t, 0, 90) }, 1, 0},
		{"perceptual backdrop files under fanart profile", "backdrop", false, func(t *testing.T) []byte { return conventionJPEG(t, 0, 55) }, 0, 1},
		{"perceptual fanart files under backdrop profile", "fanart", true, func(t *testing.T) []byte { return conventionJPEG(t, 0, 55) }, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			libDir := t.TempDir()
			createArtistDir(t, libDir, "Artist")
			dir := filepath.Join(libDir, "Artist")
			writeConv(t, dir, tc.base+".jpg", conventionJPEG(t, 0, 90))
			writeConv(t, dir, tc.base+"2.jpg", tc.second(t))

			rep, rows := scanThenDupScan(t, libDir, dir, tc.emby)
			// PRECONDITION: the scanner registered both slots for the files.
			if rows != 2 {
				t.Fatalf("precondition: fanart rows = %d, want 2", rows)
			}
			if rep.ScanErrors != 0 {
				t.Errorf("ScanErrors = %d, want 0 (rows resolvable)", rep.ScanErrors)
			}
			if rep.ExactRedundantSlots != tc.exact || rep.PerceptualRedundantSlots != tc.perc {
				t.Errorf("report exact=%d perceptual=%d, want exact=%d perceptual=%d (%+v)",
					rep.ExactRedundantSlots, rep.PerceptualRedundantSlots, tc.exact, tc.perc, rep)
			}
		})
	}
}

// TestScanFanartDuplicates_MixedDirectoryMatchesScannerOrdinals pins the
// hard constraint: in a directory holding BOTH conventions the scanner resolves
// fanart* (its pattern order), so detection must too, even under an Emby
// profile whose primary is backdrop.jpg. The fanart* pair is distinct and the
// backdrop* pair is a byte-identical duplicate, so a profile-first resolution
// (backdrop*) would report a duplicate that is not in the slots the scanner
// stored, aiming a later delete at the wrong file.
//
// REVERT-AND-RERUN: resolving profile-first (ResolveFanartNames([]string{primary}))
// must turn this RED.
func TestScanFanartDuplicates_MixedDirectoryMatchesScannerOrdinals(t *testing.T) {
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Artist")
	dir := filepath.Join(libDir, "Artist")
	writeConv(t, dir, "fanart.jpg", conventionJPEG(t, 0, 90))
	writeConv(t, dir, "fanart2.jpg", conventionJPEG(t, 120, 90))
	writeConv(t, dir, "backdrop.jpg", conventionJPEG(t, 60, 90))
	writeConv(t, dir, "backdrop2.jpg", conventionJPEG(t, 60, 90))

	// PRECONDITION: the scanner itself picks the fanart* pair.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := discoverFanartFiles(dir, entries)
	if len(got) != 2 || filepath.Base(got[0]) != "fanart.jpg" || filepath.Base(got[1]) != "fanart2.jpg" {
		t.Fatalf("precondition: scanner resolved %v, want [fanart.jpg fanart2.jpg]", got)
	}

	rep, rows := scanThenDupScan(t, libDir, dir, true)
	if rows != 2 {
		t.Fatalf("precondition: fanart rows = %d, want 2", rows)
	}
	if rep.ExactRedundantSlots != 0 || rep.PerceptualRedundantSlots != 0 || rep.ArtistsAffected != 0 {
		t.Errorf("report = %+v, want no duplicates (the scanner's fanart* slots are distinct)", rep)
	}
}

// TestScanFanartDuplicates_UnresolvableRowIsReported pins the no-silent-skip
// rule: a fanart row the scan cannot back with a file (here the files vanished
// after the scan registered them) must be counted in ScanErrors, never reported
// as a clean artist.
//
// REVERT-AND-RERUN: dropping the unresolvedFanart accounting in
// findImageDuplicates / ScanFanartDuplicates must turn this RED.
func TestScanFanartDuplicates_UnresolvableRowIsReported(t *testing.T) {
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Artist")
	dir := filepath.Join(libDir, "Artist")
	writeConv(t, dir, "backdrop.jpg", conventionJPEG(t, 0, 90))
	writeConv(t, dir, "backdrop2.jpg", conventionJPEG(t, 0, 90))

	svc, artistSvc, db := setupScannerWithDB(t, libDir)
	ctx := context.Background()
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	waitForScan(t, svc, 10*time.Second)
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artist_images WHERE image_type = 'fanart' AND exists_flag = 1`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("precondition: fanart rows = %d (err %v), want 2", rows, err)
	}
	for _, n := range []string{"backdrop.jpg", "backdrop2.jpg"} {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}

	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	e := rule.NewEngine(nil, db, nil, nil, lg)
	e.SetImageHashRecorder(artistSvc)
	rep, err := rule.NewPipeline(e, artistSvc, nil, nil, nil, lg).ScanFanartDuplicates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ScanErrors != 1 {
		t.Errorf("ScanErrors = %d, want 1 (the unresolvable artist must be reported, not passed clean): %+v", rep.ScanErrors, rep)
	}
}

// notSharedLibs reports every library as exclusively managed, which the
// fixer's shared-filesystem guard requires before it will delete.
type notSharedLibs struct{}

func (notSharedLibs) GetByID(_ context.Context, id string) (*library.Library, error) {
	return &library.Library{ID: id, SharedFSStatus: "none"}, nil
}

// TestImageDuplicateFixer_OtherConventionDeletesRightSlot drives the DELETE
// side: detection and the fixer must resolve the same ordinals, or the fixer
// removes the wrong file. backdrop.jpg (A), backdrop2.jpg (A duplicate) and
// backdrop3.jpg (B, distinct) sit under the default fanart.jpg profile; exactly
// the duplicate must go and B must survive, renumbered within the backdrop
// convention.
//
// REVERT-AND-RERUN: resolving with DiscoverFanart(primary) in
// deleteDuplicateFanartWithRollback while detection resolves across
// conventions must turn this RED.
func TestImageDuplicateFixer_OtherConventionDeletesRightSlot(t *testing.T) {
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Artist")
	dir := filepath.Join(libDir, "Artist")
	a, b := conventionJPEG(t, 0, 90), conventionJPEG(t, 120, 90)
	writeConv(t, dir, "backdrop.jpg", a)
	writeConv(t, dir, "backdrop2.jpg", a)
	writeConv(t, dir, "backdrop3.jpg", b)

	svc, artistSvc, db := setupScannerWithDB(t, libDir)
	ctx := context.Background()
	if _, err := svc.Run(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	waitForScan(t, svc, 10*time.Second)
	art, err := artistSvc.GetByPath(ctx, dir)
	if err != nil || art == nil {
		t.Fatalf("artist not found: %v", err)
	}

	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	art.LibraryID = "lib-1"
	fixer := rule.NewImageDuplicateFixer(db, nil, rule.NewSharedFSCheck(notSharedLibs{}, lg), artistSvc, lg)
	res, err := fixer.Fix(ctx, art, &rule.Violation{RuleID: rule.RuleImageDuplicateExact})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed || res.SlotsRemoved != 1 {
		t.Fatalf("Fix result = %+v, want Fixed with 1 slot removed", res)
	}
	for name, want := range map[string][]byte{"backdrop.jpg": a, "backdrop2.jpg": b} {
		got, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: err=%v, content mismatch=%v; the distinct image must survive, renumbered", name, rerr, !bytes.Equal(got, want))
		}
	}
	if _, serr := os.Stat(filepath.Join(dir, "backdrop3.jpg")); !os.IsNotExist(serr) {
		t.Errorf("backdrop3.jpg should have been renumbered away (stat err %v)", serr)
	}
}

// TestScanFanartDuplicates_JpegOrphanShapeEmby is the #2632 review regression.
// fanart.jpeg / fanart2.jpeg are NOT scanner primaries (pass 1 needs an exact
// pattern name), so the scanner registers the backdrop* files as the three
// slots. A resolver that accepts .jpeg as a primary resolves fanart*.jpeg
// instead: the real backdrop2==backdrop duplicate goes unreported, a healthy
// artist is reported as a scan error, and A/B hashes are stored into slots the
// scanner maps to C/C.
//
// REVERT-AND-RERUN: pointing ResolveScannerFanart back at image.ResolveFanart
// must turn this RED.
func TestScanFanartDuplicates_JpegOrphanShapeEmby(t *testing.T) {
	libDir := t.TempDir()
	createArtistDir(t, libDir, "Artist")
	dir := filepath.Join(libDir, "Artist")
	c := conventionJPEG(t, 60, 90)
	writeConv(t, dir, "fanart.jpeg", conventionJPEG(t, 0, 90))
	writeConv(t, dir, "fanart2.jpeg", conventionJPEG(t, 120, 90))
	writeConv(t, dir, "backdrop.jpg", c)
	writeConv(t, dir, "backdrop2.jpg", c)
	writeConv(t, dir, "backdrop3.jpg", conventionJPEG(t, 200, 90))

	// PRECONDITION: the scanner itself maps the slots to the backdrop* files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := discoverFanartFiles(dir, entries)
	if len(want) != 3 || filepath.Base(want[0]) != "backdrop.jpg" {
		t.Fatalf("precondition: scanner resolved %v, want the 3 backdrop files", want)
	}

	rep, rows, db, artistID := scanThenDupScanDB(t, libDir, dir, true)
	if rows != 3 {
		t.Fatalf("precondition: fanart rows = %d, want 3", rows)
	}
	if rep.ScanErrors != 0 || rep.ExactRedundantSlots != 1 {
		t.Errorf("healthy dir: want ScanErrors=0 and 1 exact duplicate; got %+v", rep)
	}
	// The stored hashes must describe the scanner's file for each slot.
	for slot, path := range want {
		var ph, content string
		if err := db.QueryRowContext(context.Background(),
			`SELECT COALESCE(phash,''), COALESCE(content_hash,'') FROM artist_images
			 WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = ?`, artistID, slot).Scan(&ph, &content); err != nil {
			t.Fatalf("reading slot %d hashes: %v", slot, err)
		}
		fh, err := swimage.HashFile(context.Background(), path, true)
		if err != nil {
			t.Fatalf("hashing %s: %v", path, err)
		}
		if ph != swimage.HashHex(fh.Perceptual) || content != fh.Content {
			t.Errorf("slot %d stored hashes (%q, %q) do not match the scanner's file %s (%q, %q)",
				slot, ph, content, filepath.Base(path), swimage.HashHex(fh.Perceptual), fh.Content)
		}
	}
}
