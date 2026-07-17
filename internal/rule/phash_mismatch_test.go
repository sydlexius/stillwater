package rule

import (
	"bytes"
	"context"
	"database/sql"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
)

// --------------------------------------------------------------------------
// Fixtures
//
// These fixtures are real encoded JPEGs hashed through the production
// image.PerceptualHash, not hand-picked uint64 literals, because the claim
// under test is about how real photographs behave under this hash. The
// distinctness of the fixtures is itself asserted (see
// TestPollutionJPEGFixturesAreDistinct) rather than assumed: a flat-fill
// image hashes to all zeros for EVERY variant, so a test built on flat fills
// would call any two images "perceptually identical" and pass against a
// detector that does nothing at all.
// --------------------------------------------------------------------------

// pollutionJPEG encodes a JPEG whose 8x8 block structure is mixed per variant,
// so each variant lands far from every other under a dHash. Modeled on
// backfillJPEG (internal/maintenance/fanart_hash_backfill_test.go).
func pollutionJPEG(t *testing.T, variant int) []byte {
	t.Helper()
	const (
		blocks = 8
		w      = 640
		h      = 360
	)
	m := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			bx, by := x*blocks/w, y*blocks/h
			hsh := uint32(bx)*374761393 + uint32(by)*668265263 + uint32(variant)*2246822519
			hsh ^= hsh >> 13
			hsh *= 1274126177
			hsh ^= hsh >> 16
			v := uint8(hsh >> 8)
			m.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding JPEG: %v", err)
	}
	return buf.Bytes()
}

// pollutionHash is pollutionJPEG's perceptual hash, via the production hasher.
func pollutionHash(t *testing.T, variant int) uint64 {
	t.Helper()
	h, err := image.PerceptualHash(bytes.NewReader(pollutionJPEG(t, variant)))
	if err != nil {
		t.Fatalf("hashing variant %d: %v", variant, err)
	}
	return h
}

// TestPollutionJPEGFixturesAreDistinct is the precondition every other test in
// this file leans on. It proves that the fixtures are genuinely different
// pictures under the production hash -- non-zero, pairwise unequal, and far
// outside the detector's tolerance -- so that a "not flagged" result below
// means the detector distinguished them rather than that the fixtures were
// secretly identical.
//
// Without this, the flat-fill trap applies: RGBA{128,128,128} everywhere makes
// every adjacent-pixel comparison equal, so the dHash is 0 for every image and
// all fixtures collide perfectly.
func TestPollutionJPEGFixturesAreDistinct(t *testing.T) {
	const variants = 4
	hashes := make([]uint64, variants)
	for i := range variants {
		hashes[i] = pollutionHash(t, i)
		if hashes[i] == hashUnknown {
			t.Fatalf("variant %d hashed to the all-zero sentinel: the fixture is flat, "+
				"so every comparison against it is meaningless", i)
		}
	}
	for i := range variants {
		for j := i + 1; j < variants; j++ {
			dist := image.HammingDistance(hashes[i], hashes[j])
			sim := image.Similarity(hashes[i], hashes[j])
			if sim >= defaultPHashMismatchTolerance {
				t.Fatalf("variants %d and %d are near-duplicates (hamming %d, similarity %.4f); "+
					"tests that expect them to be distinguishable are vacuous", i, j, dist, sim)
			}
			t.Logf("variants %d/%d: hamming %d, similarity %.4f", i, j, dist, sim)
		}
	}
}

// TestPHashMismatchToleranceBoundary pins the threshold's true/false-positive
// behavior exactly at its edge, on the arithmetic that defines it:
// Similarity = 1 - Hamming/64, so 0.90 admits at most 6 differing bits of 64.
// Six bits must match; seven must not. This is what makes the reused 0.90
// constant a derived choice rather than an inherited one -- if the hash width,
// the similarity formula, or the constant changes, this test says so.
func TestPHashMismatchToleranceBoundary(t *testing.T) {
	base := pollutionHash(t, 0)
	flip := func(bits int) uint64 {
		out := base
		for i := range bits {
			out ^= 1 << uint(i)
		}
		return out
	}

	sixBits := image.Similarity(base, flip(6))
	if sixBits < defaultPHashMismatchTolerance {
		t.Fatalf("6 differing bits scored %.6f, below the %.2f tolerance: a genuine "+
			"re-encode of the same picture would be missed", sixBits, defaultPHashMismatchTolerance)
	}
	sevenBits := image.Similarity(base, flip(7))
	if sevenBits >= defaultPHashMismatchTolerance {
		t.Fatalf("7 differing bits scored %.6f, at or above the %.2f tolerance: the "+
			"cutoff admits more than the near-duplicate band", sevenBits, defaultPHashMismatchTolerance)
	}
	t.Logf("boundary: 6 bits -> %.6f (match), 7 bits -> %.6f (no match)", sixBits, sevenBits)
}

// --------------------------------------------------------------------------
// Scan harness
// --------------------------------------------------------------------------

// newPHashScanPipeline builds a Pipeline over a real migrated SQLite DB. The
// detector reads artist_images and compares stored hashes, so no files on disk
// are needed -- which is itself part of the contract: the scan is read-only and
// touches neither the filesystem nor any platform.
func newPHashScanPipeline(t *testing.T) (*Pipeline, *sql.DB) {
	t.Helper()
	e, db := newDupTestEngine(t)
	svc := artist.NewService(db)
	return NewPipeline(e, svc, nil, nil, nil, testLogger()), db
}

// seedScanArtist inserts an artist that artist.Service.List can hydrate.
// insertTestArtist (checkers_test.go) leaves sort_name NULL, which the List
// path cannot scan; this detector reads names through List, so it needs the
// column populated.
func seedScanArtist(t *testing.T, db *sql.DB, id, name string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, '')`,
		id, name, name); err != nil {
		t.Fatalf("seeding artist %s: %v", id, err)
	}
}

// seedHashedImage inserts one exists_flag=1 row carrying an explicit phash hex
// string, including the empty and all-zero cases the detector must treat as
// unknown.
func seedHashedImage(t *testing.T, db *sql.DB, artistID, imageType string, slot int, phashHex string) {
	t.Helper()
	id := artistID + "-" + imageType + "-" + string(rune('0'+slot))
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag, phash, content_hash)
		 VALUES (?, ?, ?, ?, 1, ?, '')`,
		id, artistID, imageType, slot, phashHex); err != nil {
		t.Fatalf("seeding %s slot %d for %s: %v", imageType, slot, artistID, err)
	}
}

func scan(t *testing.T, p *Pipeline, scope PHashMismatchScope) PHashMismatchReport {
	t.Helper()
	report, err := p.ScanPHashMismatches(context.Background(), scope)
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}
	return report
}

// --------------------------------------------------------------------------
// The primary signal: cross-artist fanart-to-fanart collision
// --------------------------------------------------------------------------

// TestScanPHashMismatches_FlagsCrossArtistFanartCollision is the core Option-3
// proof. Artist A's slot 1 holds the same picture as artist B's slot 0 -- the
// shape of real pollution, where a fanart.tv image of B was written into A's
// folder and B's own folder holds that same source image. Both sides are
// flagged and attributed to each other, because the collision is symmetric and
// the detector does not pretend to know which artist owns the picture.
func TestScanPHashMismatches_FlagsCrossArtistFanartCollision(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")
	seedScanArtist(t, db, "art-b", "Artist B")

	own := image.HashHex(pollutionHash(t, 0))    // A's legitimate backdrop
	shared := image.HashHex(pollutionHash(t, 1)) // B's picture, in both folders
	bOther := image.HashHex(pollutionHash(t, 2)) // B's other legitimate backdrop
	seedHashedImage(t, db, "art-a", "fanart", 0, own)
	seedHashedImage(t, db, "art-a", "fanart", 1, shared)
	seedHashedImage(t, db, "art-b", "fanart", 0, shared)
	seedHashedImage(t, db, "art-b", "fanart", 1, bOther)

	report := scan(t, p, PHashMismatchScope{})

	if report.SuspectSlots != 2 {
		t.Fatalf("suspect slots = %d, want 2 (both sides of the collision); report: %+v",
			report.SuspectSlots, report)
	}
	if report.ArtistsAffected != 2 {
		t.Fatalf("artists affected = %d, want 2", report.ArtistsAffected)
	}
	if report.SlotsEvaluated != 4 {
		t.Fatalf("slots evaluated = %d, want 4", report.SlotsEvaluated)
	}

	byArtist := map[string]ArtistPHashMismatch{}
	for _, a := range report.PerArtist {
		byArtist[a.ArtistID] = a
	}
	a, ok := byArtist["art-a"]
	if !ok {
		t.Fatalf("artist A not reported; per-artist: %+v", report.PerArtist)
	}
	if len(a.Suspects) != 1 || a.Suspects[0].SlotIndex != 1 {
		t.Fatalf("artist A suspects = %+v, want exactly slot 1", a.Suspects)
	}
	s := a.Suspects[0]
	if s.MatchedArtistID != "art-b" || s.MatchedSlotIndex != 0 {
		t.Fatalf("artist A slot 1 matched %s slot %d, want art-b slot 0", s.MatchedArtistID, s.MatchedSlotIndex)
	}
	if s.MatchedArtistName != "Artist B" {
		t.Fatalf("matched artist name = %q, want %q", s.MatchedArtistName, "Artist B")
	}
	if s.Similarity != 1.0 {
		t.Fatalf("similarity = %v, want 1.0 for the same stored hash", s.Similarity)
	}
	if s.PHash != shared {
		t.Fatalf("reported phash = %q, want %q", s.PHash, shared)
	}
	if s.MatchCount != 1 {
		t.Fatalf("match count = %d, want 1", s.MatchCount)
	}

	b := byArtist["art-b"]
	if len(b.Suspects) != 1 || b.Suspects[0].SlotIndex != 0 {
		t.Fatalf("artist B suspects = %+v, want exactly slot 0", b.Suspects)
	}
}

// TestScanPHashMismatches_DistinctBackdropsAreNotFlagged is the false-positive
// side. Four different photographs across two artists collide with nothing, so
// nothing is reported -- and the fixture-distinctness test above is what makes
// this assertion mean something.
func TestScanPHashMismatches_DistinctBackdropsAreNotFlagged(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")
	seedScanArtist(t, db, "art-b", "Artist B")
	seedHashedImage(t, db, "art-a", "fanart", 0, image.HashHex(pollutionHash(t, 0)))
	seedHashedImage(t, db, "art-a", "fanart", 1, image.HashHex(pollutionHash(t, 1)))
	seedHashedImage(t, db, "art-b", "fanart", 0, image.HashHex(pollutionHash(t, 2)))
	seedHashedImage(t, db, "art-b", "fanart", 1, image.HashHex(pollutionHash(t, 3)))

	report := scan(t, p, PHashMismatchScope{})

	if report.SuspectSlots != 0 || report.ArtistsAffected != 0 {
		t.Fatalf("distinct backdrops flagged: suspects=%d affected=%d per-artist=%+v",
			report.SuspectSlots, report.ArtistsAffected, report.PerArtist)
	}
	if report.SlotsEvaluated != 4 {
		t.Fatalf("slots evaluated = %d, want 4", report.SlotsEvaluated)
	}
	if report.IndeterminateSlots != 0 {
		t.Fatalf("indeterminate = %d, want 0", report.IndeterminateSlots)
	}
}

// TestScanPHashMismatches_WithinArtistDuplicateIsNotCrossArtist guards the
// signal's scope. Two identical slots in ONE artist's folder are within-artist
// redundancy, which the exact-collapse path already owns; this detector must
// not claim them as cross-artist pollution.
func TestScanPHashMismatches_WithinArtistDuplicateIsNotCrossArtist(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")
	same := image.HashHex(pollutionHash(t, 0))
	seedHashedImage(t, db, "art-a", "fanart", 0, same)
	seedHashedImage(t, db, "art-a", "fanart", 1, same)

	report := scan(t, p, PHashMismatchScope{})

	if report.SuspectSlots != 0 {
		t.Fatalf("within-artist duplicate flagged as cross-artist pollution: %+v", report.PerArtist)
	}
}

// --------------------------------------------------------------------------
// Absence of data is not evidence of correctness
// --------------------------------------------------------------------------

// TestScanPHashMismatches_MissingPHashIsIndeterminateNotClean covers the
// false-green this detector exists to avoid. A slot that exists on disk but
// carries no hash was not evaluated; reporting it as clean would claim the
// library is fine because nothing was checked.
func TestScanPHashMismatches_MissingPHashIsIndeterminateNotClean(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")
	seedHashedImage(t, db, "art-a", "fanart", 0, image.HashHex(pollutionHash(t, 0)))
	seedHashedImage(t, db, "art-a", "fanart", 1, "")

	report := scan(t, p, PHashMismatchScope{})

	if report.IndeterminateSlots != 1 {
		t.Fatalf("indeterminate slots = %d, want 1; report: %+v", report.IndeterminateSlots, report)
	}
	if report.SlotsEvaluated != 1 {
		t.Fatalf("slots evaluated = %d, want 1 (the unhashed slot was NOT evaluated)", report.SlotsEvaluated)
	}
	got := report.Indeterminate[0]
	if got.ArtistID != "art-a" || got.SlotIndex != 1 {
		t.Fatalf("indeterminate entry = %+v, want art-a slot 1", got)
	}
	if got.Reason == "" {
		t.Fatal("indeterminate entry carries no reason; the operator cannot tell why it was skipped")
	}
	if report.FanartRegistry.SlotsSkipped != 1 {
		t.Fatalf("registry slots skipped = %d, want 1", report.FanartRegistry.SlotsSkipped)
	}
}

// TestScanPHashMismatches_UnknownNeverMatchesUnknown is the third door of the
// same poison. An all-zero phash LOOKS like data and is Hamming-distance 0 from
// every other all-zero phash, so admitting it would manufacture a perfect
// cross-artist collision between every pair of unhashed images in the library
// -- mass false positives feeding a delete path. Both artists' zero-hashed
// slots must land in the indeterminate bucket and collide with nothing.
func TestScanPHashMismatches_UnknownNeverMatchesUnknown(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")
	seedScanArtist(t, db, "art-b", "Artist B")
	const zero = "0000000000000000"
	seedHashedImage(t, db, "art-a", "fanart", 0, zero)
	seedHashedImage(t, db, "art-b", "fanart", 0, zero)

	report := scan(t, p, PHashMismatchScope{})

	if report.SuspectSlots != 0 {
		t.Fatalf("two unhashed slots were reported as colliding: %+v", report.PerArtist)
	}
	if report.IndeterminateSlots != 2 {
		t.Fatalf("indeterminate = %d, want 2", report.IndeterminateSlots)
	}
	if report.SlotsEvaluated != 0 {
		t.Fatalf("slots evaluated = %d, want 0", report.SlotsEvaluated)
	}
}

// TestScanPHashMismatches_UnparsablePHashIsIndeterminate covers the remaining
// unusable shape: a corrupt hex value must be skipped loudly, not parsed to
// some arbitrary number and compared.
func TestScanPHashMismatches_UnparsablePHashIsIndeterminate(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")
	seedHashedImage(t, db, "art-a", "fanart", 0, "not-a-hash")

	report := scan(t, p, PHashMismatchScope{})

	if report.IndeterminateSlots != 1 || report.SlotsEvaluated != 0 {
		t.Fatalf("unparsable hash: indeterminate=%d evaluated=%d, want 1/0",
			report.IndeterminateSlots, report.SlotsEvaluated)
	}
}

// TestScanPHashMismatches_RegistryCoverageIsReported covers the second
// false-green route. An artist whose fanart carries no usable hash drops out of
// the comparison registry entirely, which makes THEIR pictures unrecognizable
// as pollution in every other artist's folder -- with nothing flagged anywhere.
// The report must say how much of the library the registry actually covers.
func TestScanPHashMismatches_RegistryCoverageIsReported(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")
	seedScanArtist(t, db, "art-b", "Artist B")
	seedScanArtist(t, db, "art-c", "Artist C")
	seedHashedImage(t, db, "art-a", "fanart", 0, image.HashHex(pollutionHash(t, 0)))
	seedHashedImage(t, db, "art-b", "fanart", 0, image.HashHex(pollutionHash(t, 1)))
	// Artist C contributes nothing: both slots unhashed.
	seedHashedImage(t, db, "art-c", "fanart", 0, "")
	seedHashedImage(t, db, "art-c", "fanart", 1, "")

	report := scan(t, p, PHashMismatchScope{})

	if report.FanartRegistry.ArtistsIndexed != 2 {
		t.Fatalf("registry artists indexed = %d, want 2", report.FanartRegistry.ArtistsIndexed)
	}
	if report.FanartRegistry.ArtistsSkipped != 1 {
		t.Fatalf("registry artists skipped = %d, want 1 (artist C is invisible to every "+
			"other artist's scan and that must be reported)", report.FanartRegistry.ArtistsSkipped)
	}
	if report.FanartRegistry.SlotsIndexed != 2 || report.FanartRegistry.SlotsSkipped != 2 {
		t.Fatalf("registry slots indexed/skipped = %d/%d, want 2/2",
			report.FanartRegistry.SlotsIndexed, report.FanartRegistry.SlotsSkipped)
	}
}

// --------------------------------------------------------------------------
// Thumb attribution -- corroborating only
// --------------------------------------------------------------------------

// TestScanPHashMismatches_ThumbAttributionCorroboratesButDoesNotFlag pins the
// demotion of the thumb signal. A fanart slot resembling another artist's THUMB
// and nothing else raises no suspect: a thumb and a backdrop are different
// photographs, so that signal cannot carry detection on its own. When a real
// cross-artist fanart collision IS present, a matching thumb names the true
// subject and is reported alongside it.
func TestScanPHashMismatches_ThumbAttributionCorroboratesButDoesNotFlag(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")
	seedScanArtist(t, db, "art-b", "Artist B")

	shared := image.HashHex(pollutionHash(t, 1))
	// B's thumb happens to be that same picture: the rare case where the
	// thumb signal fires and names B as the true subject.
	seedHashedImage(t, db, "art-b", "thumb", 0, shared)
	seedHashedImage(t, db, "art-a", "fanart", 0, shared)
	seedHashedImage(t, db, "art-b", "fanart", 0, shared)

	report := scan(t, p, PHashMismatchScope{ArtistID: "art-a"})

	if len(report.PerArtist) != 1 || len(report.PerArtist[0].Suspects) != 1 {
		t.Fatalf("want one suspect for artist A; got %+v", report.PerArtist)
	}
	s := report.PerArtist[0].Suspects[0]
	if s.ThumbAttributionArtistID != "art-b" || s.ThumbAttributionArtistName != "Artist B" {
		t.Fatalf("thumb attribution = %q/%q, want art-b/Artist B",
			s.ThumbAttributionArtistID, s.ThumbAttributionArtistName)
	}
	if report.ThumbRegistry.ArtistsIndexed != 1 {
		t.Fatalf("thumb registry artists indexed = %d, want 1", report.ThumbRegistry.ArtistsIndexed)
	}
}

// TestScanPHashMismatches_ThumbAloneRaisesNoSuspect is the load-bearing half of
// the demotion: without a fanart-to-fanart collision, a thumb match alone must
// not flag anything.
func TestScanPHashMismatches_ThumbAloneRaisesNoSuspect(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")
	seedScanArtist(t, db, "art-b", "Artist B")
	shared := image.HashHex(pollutionHash(t, 1))
	seedHashedImage(t, db, "art-b", "thumb", 0, shared)
	seedHashedImage(t, db, "art-a", "fanart", 0, shared)

	report := scan(t, p, PHashMismatchScope{})

	if report.SuspectSlots != 0 {
		t.Fatalf("a thumb-only match raised a suspect: %+v", report.PerArtist)
	}
}

// --------------------------------------------------------------------------
// Scoping and tolerance
// --------------------------------------------------------------------------

// TestScanPHashMismatches_ScopedToOneArtistKeepsLibraryWideRegistry proves the
// scope narrows the PROBE, not the registry. Scoping to A must still compare
// A's slots against B's pictures -- a scope that also shrank the registry would
// find nothing, since a collision needs the other side.
func TestScanPHashMismatches_ScopedToOneArtistKeepsLibraryWideRegistry(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")
	seedScanArtist(t, db, "art-b", "Artist B")
	shared := image.HashHex(pollutionHash(t, 1))
	seedHashedImage(t, db, "art-a", "fanart", 0, shared)
	seedHashedImage(t, db, "art-b", "fanart", 0, shared)

	report := scan(t, p, PHashMismatchScope{ArtistID: "art-a"})

	if report.ArtistsScanned != 1 {
		t.Fatalf("artists scanned = %d, want 1 (scope narrows the probe)", report.ArtistsScanned)
	}
	if report.SuspectSlots != 1 {
		t.Fatalf("suspect slots = %d, want 1: the registry must stay library-wide or the "+
			"scoped scan can never see the other side of a collision", report.SuspectSlots)
	}
	if report.PerArtist[0].Suspects[0].MatchedArtistID != "art-b" {
		t.Fatalf("matched %q, want art-b", report.PerArtist[0].Suspects[0].MatchedArtistID)
	}
	if report.ScopedArtistID != "art-a" {
		t.Fatalf("scoped artist id = %q, want art-a", report.ScopedArtistID)
	}
}

// TestScanPHashMismatches_UnknownScopedArtistIsAnError refuses to answer a
// question about an artist that does not exist. Returning an empty "0 suspects"
// report for a typo'd id would be a false green.
func TestScanPHashMismatches_UnknownScopedArtistIsAnError(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")

	_, err := p.ScanPHashMismatches(context.Background(), PHashMismatchScope{ArtistID: "nope"})
	if err == nil {
		t.Fatal("scanning an unknown artist returned no error; a typo'd id must not read as clean")
	}
}

// TestScanPHashMismatches_ToleranceIsOperatorConfigurable proves the cutoff is
// a knob rather than a baked-in constant, on a pair 7 bits apart -- just
// outside the 0.90 default and just inside a lowered one.
func TestScanPHashMismatches_ToleranceIsOperatorConfigurable(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")
	seedScanArtist(t, db, "art-b", "Artist B")
	base := pollutionHash(t, 0)
	near := base
	for i := range 7 {
		near ^= 1 << uint(i)
	}
	seedHashedImage(t, db, "art-a", "fanart", 0, image.HashHex(base))
	seedHashedImage(t, db, "art-b", "fanart", 0, image.HashHex(near))

	if got := scan(t, p, PHashMismatchScope{}).SuspectSlots; got != 0 {
		t.Fatalf("at the default tolerance, a 7-bit-apart pair flagged %d slots, want 0", got)
	}
	report := scan(t, p, PHashMismatchScope{Tolerance: 0.85})
	if report.SuspectSlots != 2 {
		t.Fatalf("at tolerance 0.85, a 7-bit-apart pair flagged %d slots, want 2", report.SuspectSlots)
	}
	if report.Tolerance != 0.85 {
		t.Fatalf("reported tolerance = %v, want 0.85", report.Tolerance)
	}
}

// TestScanPHashMismatches_OutOfRangeToleranceFallsBackToDefault covers the
// guard on the scope value itself.
func TestScanPHashMismatches_OutOfRangeToleranceFallsBackToDefault(t *testing.T) {
	p, db := newPHashScanPipeline(t)
	seedScanArtist(t, db, "art-a", "Artist A")

	for _, tol := range []float64{0, -1, 1.5} {
		report := scan(t, p, PHashMismatchScope{Tolerance: tol})
		if report.Tolerance != defaultPHashMismatchTolerance {
			t.Fatalf("tolerance %v produced %v, want the %v default",
				tol, report.Tolerance, defaultPHashMismatchTolerance)
		}
	}
}

// TestScanPHashMismatches_UnwiredPipelineFailsLoudly guards the wiring: a
// half-built pipeline must error rather than return an empty clean report.
func TestScanPHashMismatches_UnwiredPipelineFailsLoudly(t *testing.T) {
	p := &Pipeline{logger: testLogger()}
	if _, err := p.ScanPHashMismatches(context.Background(), PHashMismatchScope{}); err == nil {
		t.Fatal("an unwired pipeline returned no error; a wiring bug must not read as a clean library")
	}
}
