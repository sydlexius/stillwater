package rule

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
)

func mixedConventionJPEG(t *testing.T, w, h, seed int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := uint8((x*255/w + seed) % 256)
			im.Set(x, y, color.RGBA{R: v, G: 255 - v, B: uint8((y * 255 / h) ^ seed), A: 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding jpeg: %v", err)
	}
	return b.Bytes()
}

// TestImageDuplicateFixer_MixedDirectoryUnderEmby is the #2632 delete-side
// guard on a directory holding BOTH conventions under an Emby profile (primary
// backdrop.jpg). The scanner resolves fanart* (its pattern order), so the
// duplicate is fanart2.jpg; a profile-first delete walk would aim the stored
// slot at the distinct backdrop2.jpg. After the fix the artist row must also
// describe the scanner's view (one slot, fanart.jpg's 1920x1080), not the
// backdrop files' 400x400.
//
// REVERT-AND-RERUN: a profile-first delete walk (ResolveFanartNames with the
// profile primary first) or a profile-first resync/reconcile must turn this RED.
func TestImageDuplicateFixer_MixedDirectoryUnderEmby(t *testing.T) {
	ctx := context.Background()
	e, db := newDupTestEngine(t)
	psvc := platform.NewService(db)
	prof, err := psvc.GetByName(ctx, "Emby")
	if err != nil {
		t.Fatal(err)
	}
	if err := psvc.SetActive(ctx, prof.ID); err != nil {
		t.Fatal(err)
	}
	e.platformService = psvc

	const artistID = "art-mixed"
	insertTestArtist(t, db, artistID, "Mixed")
	for slot := range 2 {
		insertTestImage(t, db, artistID, "fanart", slot)
	}
	if _, err := db.Exec(`INSERT INTO libraries (id, name, type, source, created_at, updated_at) VALUES ('lib-test','T','regular','manual',datetime('now'),datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	svc := artist.NewService(db)
	if err := svc.AddLibraryMembership(ctx, artistID, "lib-test", "manual"); err != nil {
		t.Fatal(err)
	}
	a := &artist.Artist{ID: artistID, Name: "Mixed", Path: dir, LibraryID: "lib-test", FanartExists: true, FanartCount: 2}
	if err := svc.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	dup := mixedConventionJPEG(t, 1920, 1080, 0)
	back1 := mixedConventionJPEG(t, 400, 400, 60)
	back2 := mixedConventionJPEG(t, 400, 400, 120)
	writeFanartFiles(t, dir, map[string][]byte{
		"fanart.jpg": dup, "fanart2.jpg": dup, "backdrop.jpg": back1, "backdrop2.jpg": back2,
	})

	fixer := NewImageDuplicateFixer(db, psvc, nonSharedFSCheck(), svc, testLogger())
	p := NewPipeline(e, svc, nil, []Fixer{fixer}, nil, testLogger())
	if _, err := p.RemediateFanartDuplicates(ctx); err != nil {
		t.Fatalf("RemediateFanartDuplicates: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "fanart2.jpg")); !os.IsNotExist(err) {
		t.Errorf("fanart2.jpg (the scanner's duplicate slot) should be deleted; stat err %v", err)
	}
	for name, want := range map[string][]byte{"fanart.jpg": dup, "backdrop.jpg": back1, "backdrop2.jpg": back2} {
		got, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil || !bytes.Equal(got, want) {
			t.Errorf("%s must be byte-unchanged (err %v, content differs %v)", name, rerr, !bytes.Equal(got, want))
		}
	}

	got, err := svc.GetByID(ctx, artistID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FanartCount != 1 || got.FanartWidth != 1920 || got.FanartHeight != 1080 || got.FanartLowRes {
		t.Errorf("artist row = count %d, %dx%d, lowRes %v; scanner view is 1 slot, 1920x1080, not low-res",
			got.FanartCount, got.FanartWidth, got.FanartHeight, got.FanartLowRes)
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artist_images WHERE artist_id = ? AND image_type = 'fanart' AND exists_flag = 1`, artistID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("fanart rows after fix = %d, want 1 (the scanner's view)", rows)
	}
}
