package rule

import (
	"bytes"
	"context"
	"database/sql"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
)

// --------------------------------------------------------------------------
// Harness
// --------------------------------------------------------------------------

// newPHashRepairPipeline builds a Pipeline over a real migrated SQLite DB with a
// real hash recorder, which img.RenumberFanart requires (it refuses to renumber
// without one).
func newPHashRepairPipeline(t *testing.T) (*Pipeline, *sql.DB) {
	t.Helper()
	e, db := newDupTestEngine(t)
	svc := artist.NewService(db)
	return NewPipeline(e, svc, nil, nil, nil, testLogger()), db
}

// seedRepairArtist inserts an artist with a real on-disk path.
func seedRepairArtist(t *testing.T, db *sql.DB, id, name, path string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, ?)`,
		id, name, name, path); err != nil {
		t.Fatalf("seeding artist %s: %v", id, err)
	}
}

// writePollutionFanart writes fixture variant to dir/name and returns the hex
// perceptual hash of the bytes actually written, computed with the production
// hasher.
//
// Returning the MEASURED hash rather than a literal is what keeps these tests
// honest. The remediation path re-hashes the file from disk and refuses to act
// unless it matches the stored hash, so a hand-written hex string would make
// every test exercise the skip path while appearing to test removal.
func writePollutionFanart(t *testing.T, dir, name string, variant int) string {
	t.Helper()
	data := pollutionJPEG(t, variant)
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	h, err := image.PerceptualHash(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("hashing %s: %v", name, err)
	}
	return image.HashHex(h)
}

// fanartVariantAt reports which pollutionJPEG variant the file at dir/name
// holds, by perceptual match against each candidate. Returns -1 when it matches
// none.
//
// Identifying restored artwork by CONTENT is deliberate: these tests assert that
// the right picture ended up in the right place, and comparing filenames or
// sizes would pass just as happily if the bytes were swapped -- which is exactly
// the failure mode under test.
func fanartVariantAt(t *testing.T, dir, name string, variants []int) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	got, err := image.PerceptualHash(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("hashing %s: %v", name, err)
	}
	for _, v := range variants {
		if image.Similarity(got, pollutionHash(t, v)) >= defaultPHashMismatchTolerance {
			return v
		}
	}
	return -1
}

// seedPollutedLibrary builds the canonical scenario these tests share:
//
//	artist A holds v0, v1, v2 at slots 0, 1, 2 -- slot 1 is POLLUTION (it is
//	actually artist B's picture);
//	artist B holds that same picture (v1) at its own slot 0, which is what
//	makes the collision detectable at all.
//
// Returns A's directory.
func seedPollutedLibrary(t *testing.T, db *sql.DB) string {
	t.Helper()
	dirA, dirB := t.TempDir(), t.TempDir()
	seedRepairArtist(t, db, "art-a", "Artist A", dirA)
	seedRepairArtist(t, db, "art-b", "Artist B", dirB)

	h0 := writePollutionFanart(t, dirA, "fanart.jpg", 0)
	h1 := writePollutionFanart(t, dirA, "fanart2.jpg", 1) // the pollution
	h2 := writePollutionFanart(t, dirA, "fanart3.jpg", 2)
	hB := writePollutionFanart(t, dirB, "fanart.jpg", 1) // B's own copy

	seedHashedImage(t, db, "art-a", "fanart", 0, h0)
	seedHashedImage(t, db, "art-a", "fanart", 1, h1)
	seedHashedImage(t, db, "art-a", "fanart", 2, h2)
	seedHashedImage(t, db, "art-b", "fanart", 0, hB)

	// Precondition: the fixtures must be genuinely distinct pictures, and the
	// pollution must genuinely collide with B's copy. Without this, a
	// "removed the right slot" result could come from fixtures that all hash
	// alike -- the flat-fill trap that made an earlier draft of this feature
	// pass against buggy code.
	if h1 != hB {
		t.Fatalf("precondition: the pollution and B's copy must be the same picture, got %s vs %s", h1, hB)
	}
	if h0 == h1 || h1 == h2 || h0 == h2 {
		t.Fatalf("precondition: A's three slots must be distinct pictures, got %s/%s/%s", h0, h1, h2)
	}
	return dirA
}

// --------------------------------------------------------------------------
// Scope and input guards
// --------------------------------------------------------------------------

// TestRemediatePHashMismatches_RequiresArtistScope pins the per-artist default.
// An unscoped run at a badly chosen tolerance is a library-wide artwork loss, so
// it must not be reachable by omitting a parameter.
func TestRemediatePHashMismatches_RequiresArtistScope(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	seedPollutedLibrary(t, db)

	_, err := p.RemediatePHashMismatches(context.Background(), PHashMismatchScope{}, PHashRemediateOpts{})
	if err == nil {
		t.Fatal("an unscoped remediate must be rejected without AllArtists")
	}
	if !strings.Contains(err.Error(), "artist scope is required") {
		t.Errorf("expected a scope-required error, got: %v", err)
	}
}

// TestRemediatePHashMismatches_RejectsUnusableTolerance pins the NaN guard on
// the DESTRUCTIVE path specifically.
//
// ScanPHashMismatches silently falls back to the default for an unusable
// tolerance, which is right for a read-only report. It is wrong here: an
// operator who asked for 0.98 and silently got 0.90 would be confirming a
// suspect set that is not the one they configured. NaN is the sharpest case --
// every IEEE-754 comparison against it is false, so a naive range check ADMITS
// it and it then defeats the similarity filter, making every slot a suspect.
func TestRemediatePHashMismatches_RejectsUnusableTolerance(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	for name, tol := range map[string]float64{
		"NaN": math.NaN(), "negative": -0.5, "above one": 1.5,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.RemediatePHashMismatches(context.Background(),
				PHashMismatchScope{ArtistID: "art-a", Tolerance: tol},
				PHashRemediateOpts{})
			if err == nil {
				t.Fatalf("tolerance %v must be rejected on the destructive path", tol)
			}
			if !strings.Contains(err.Error(), "tolerance must be within") {
				t.Errorf("expected a tolerance rejection, got: %v", err)
			}
		})
	}

	// Nothing was touched by any of the rejected runs.
	for _, name := range []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg"} {
		if _, err := os.Stat(filepath.Join(dirA, name)); err != nil {
			t.Errorf("%s must survive a rejected remediate: %v", name, err)
		}
	}
}

// --------------------------------------------------------------------------
// Removal
// --------------------------------------------------------------------------

// TestRemediatePHashMismatches_QuarantinesThenRemovesAndRenumbers is the happy
// path, asserted on CONTENT rather than on counts: the polluted picture is gone,
// its bytes are recoverable from quarantine, and the survivors are renumbered
// into a contiguous run.
func TestRemediatePHashMismatches_QuarantinesThenRemovesAndRenumbers(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("RemediatePHashMismatches: %v", err)
	}
	if res.SlotsRemoved != 1 || res.Quarantined != 1 || res.Failures != 0 {
		t.Fatalf("want 1 removed / 1 quarantined / 0 failures, got %+v", res)
	}

	// Renumbered: two contiguous slots holding v0 and v2. v1 is gone.
	if got := fanartVariantAt(t, dirA, "fanart.jpg", []int{0, 1, 2}); got != 0 {
		t.Errorf("slot 0 must still hold v0, holds v%d", got)
	}
	if got := fanartVariantAt(t, dirA, "fanart2.jpg", []int{0, 1, 2}); got != 2 {
		t.Errorf("slot 1 must hold v2 after renumbering, holds v%d", got)
	}
	if _, err := os.Stat(filepath.Join(dirA, "fanart3.jpg")); !os.IsNotExist(err) {
		t.Errorf("the gap must be closed, fanart3.jpg still present; stat err = %v", err)
	}
	// No tomb survives a committed run.
	entries, _ := os.ReadDir(dirA)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), phashTombSuffix) {
			t.Errorf("a committed run must leave no tomb, found %s", e.Name())
		}
	}

	// The removed bytes are recoverable, and the manifest records WHY.
	m, err := image.ReadRepairManifest(dirA, res.OpID)
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m == nil || len(m.Entries) != 1 {
		t.Fatalf("expected 1 quarantined entry, got %+v", m)
	}
	if m.Entries[0].MatchedArtistID != "art-b" {
		t.Errorf("manifest must attribute the collision to art-b, got %q", m.Entries[0].MatchedArtistID)
	}
	data, err := image.RepairEntryBytes(dirA, res.OpID, m.Entries[0])
	if err != nil {
		t.Fatalf("RepairEntryBytes: %v", err)
	}
	if !bytes.Equal(data, pollutionJPEG(t, 1)) {
		t.Error("quarantined bytes must be exactly the removed picture (v1)")
	}
}

// TestRemediatePHashMismatches_DryRunMutatesNothing pins the preview contract.
// The dry run is what an operator approves before confirming a deletion, so if
// it mutated anything the confirmation step would be theater.
func TestRemediatePHashMismatches_DryRunMutatesNothing(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.SlotsRemoved != 0 || res.Quarantined != 0 {
		t.Errorf("a dry run must remove and quarantine nothing, got %+v", res)
	}
	if len(res.Outcomes) != 1 || res.Outcomes[0].Action != "would-remove" {
		t.Errorf("a dry run must preview the slot it would remove, got %+v", res.Outcomes)
	}
	if res.Outcomes[0].SlotIndex != 1 {
		t.Errorf("the previewed slot must be the polluted one (1), got %d", res.Outcomes[0].SlotIndex)
	}

	for _, name := range []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg"} {
		if _, err := os.Stat(filepath.Join(dirA, name)); err != nil {
			t.Errorf("%s must survive a dry run: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dirA, image.RepairDirName)); !os.IsNotExist(err) {
		t.Errorf("a dry run must not create a quarantine; stat err = %v", err)
	}
}

// TestRemediatePHashMismatches_SkipsSlotWhoseBytesNoLongerMatch pins the
// re-verification safeguard.
//
// The detector reads artist_images.phash, which is a CACHE. If the file was
// replaced since that hash was written, the flagged ordinal now holds a picture
// nobody flagged -- and removing it would destroy artwork on the strength of a
// hash that no longer describes it. The skip is the safeguard working, so it is
// counted apart from a failure.
func TestRemediatePHashMismatches_SkipsSlotWhoseBytesNoLongerMatch(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	// The DB still says slot 1 is v1; the disk now holds v3. This is the
	// stale-cache state, reached without touching the DB.
	writePollutionFanart(t, dirA, "fanart2.jpg", 3)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("RemediatePHashMismatches: %v", err)
	}
	if res.SlotsRemoved != 0 || res.Quarantined != 0 {
		t.Errorf("a slot failing re-verification must not be removed, got %+v", res)
	}
	if res.SlotsSkipped != 1 {
		t.Fatalf("want 1 skipped slot, got %d (%+v)", res.SlotsSkipped, res.Outcomes)
	}
	if res.Failures != 0 {
		t.Errorf("a skip is the safeguard working, not a failure; got %d failures", res.Failures)
	}
	if len(res.Outcomes) != 1 || res.Outcomes[0].Action != "skipped" {
		t.Fatalf("expected a skipped outcome, got %+v", res.Outcomes)
	}
	if !strings.Contains(res.Outcomes[0].Reason, "no longer matches") {
		t.Errorf("the skip must say the slot changed since detection, got %q", res.Outcomes[0].Reason)
	}
	// The bystander that moved in is untouched.
	if got := fanartVariantAt(t, dirA, "fanart2.jpg", []int{0, 1, 2, 3}); got != 3 {
		t.Errorf("the unflagged picture must survive, slot 1 holds v%d", got)
	}
}

// TestRemediatePHashMismatches_RestoresStagedTombsAndKeepsQuarantineOnRenumberFailure
// pins the crash-safe deletion contract, mirroring
// TestImageDuplicateFixer_Fix_RestoresStagedTombsOnRenumberFailure.
//
// The failure is forced deterministically and host-independently: RenumberFanart
// clears any stale temp file named fanart_renumber_0.jpg.tmp before staging its
// first survivor, so a NON-EMPTY directory at that path makes its os.Remove
// return ENOTEMPTY -- aborting the renumber only AFTER the slot has already been
// staged, which is precisely the rollback window.
//
// It also pins the deliberate NON-cleanup of the quarantine on the failure path:
// if a tomb restore is ever refused, the quarantine is the only remaining copy of
// that artwork, so tidying it away to keep the manifest neat would destroy the
// thing the quarantine exists to preserve.
func TestRemediatePHashMismatches_RestoresStagedTombsAndKeepsQuarantineOnRenumberFailure(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	blockDir := filepath.Join(dirA, "fanart_renumber_0.jpg.tmp")
	if err := os.Mkdir(blockDir, 0o755); err != nil {
		t.Fatalf("creating block dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blockDir, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatalf("populating block dir: %v", err)
	}

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("the run itself must not error; per-artist failures are counted: %v", err)
	}
	if res.Failures != 1 {
		t.Fatalf("want 1 artist failure from the forced renumber failure, got %+v", res)
	}
	if res.SlotsRemoved != 0 {
		t.Errorf("nothing may be reported removed when the commit failed, got %d", res.SlotsRemoved)
	}

	// Rollback proof: the staged slot is back at its original path, and no
	// tomb is left behind.
	if got := fanartVariantAt(t, dirA, "fanart2.jpg", []int{0, 1, 2}); got != 1 {
		t.Errorf("the staged slot must be RESTORED to fanart2.jpg holding v1, holds v%d", got)
	}
	if _, err := os.Stat(filepath.Join(dirA, "fanart2.jpg"+phashTombSuffix)); !os.IsNotExist(err) {
		t.Errorf("no tomb may remain after a clean rollback; stat err = %v", err)
	}
	// The untouched slots are untouched.
	if got := fanartVariantAt(t, dirA, "fanart.jpg", []int{0, 1, 2}); got != 0 {
		t.Errorf("slot 0 must be untouched, holds v%d", got)
	}
	if got := fanartVariantAt(t, dirA, "fanart3.jpg", []int{0, 1, 2}); got != 2 {
		t.Errorf("slot 2 must be untouched, holds v%d", got)
	}

	// The quarantine is RETAINED, not cleaned up.
	m, err := image.ReadRepairManifest(dirA, res.OpID)
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m == nil || len(m.Entries) != 1 {
		t.Fatalf("the quarantined copy must be retained on the failure path, got %+v", m)
	}
}

// TestRemediatePHashMismatches_SkipsSlotThatNoLongerExistsOnDisk covers the
// DB/disk divergence that is this repo's dominant bug shape: artist_images says
// there is a slot 5, the directory holds three files. Indexing the discovery
// slice with the stale ordinal would panic; treating it as removable would act
// on a row describing nothing. It must skip, loudly and counted.
func TestRemediatePHashMismatches_SkipsSlotThatNoLongerExistsOnDisk(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	seedRepairArtist(t, db, "art-a", "Artist A", dirA)
	seedRepairArtist(t, db, "art-b", "Artist B", dirB)

	h0 := writePollutionFanart(t, dirA, "fanart.jpg", 0)
	hB := writePollutionFanart(t, dirB, "fanart.jpg", 1)
	seedHashedImage(t, db, "art-a", "fanart", 0, h0)
	// A row for a slot the directory does not have, colliding with B.
	seedHashedImage(t, db, "art-a", "fanart", 5, hB)
	seedHashedImage(t, db, "art-b", "fanart", 0, hB)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("RemediatePHashMismatches: %v", err)
	}
	if res.SlotsRemoved != 0 || res.Quarantined != 0 {
		t.Errorf("a slot absent from disk must not be removed, got %+v", res)
	}
	if res.SlotsSkipped != 1 {
		t.Fatalf("want 1 skipped slot, got %d (%+v)", res.SlotsSkipped, res.Outcomes)
	}
	if !strings.Contains(res.Outcomes[0].Reason, "no longer exists") {
		t.Errorf("the skip must name the vanished slot, got %q", res.Outcomes[0].Reason)
	}
	if got := fanartVariantAt(t, dirA, "fanart.jpg", []int{0, 1}); got != 0 {
		t.Errorf("the real slot must be untouched, holds v%d", got)
	}
}

// TestRemediatePHashMismatches_SkipsArtistWithNoPath pins that an artist with no
// resolved directory is skipped rather than driving a removal against "".
func TestRemediatePHashMismatches_SkipsArtistWithNoPath(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirB := t.TempDir()
	seedRepairArtist(t, db, "art-a", "Artist A", "") // no path
	seedRepairArtist(t, db, "art-b", "Artist B", dirB)

	hB := writePollutionFanart(t, dirB, "fanart.jpg", 1)
	seedHashedImage(t, db, "art-a", "fanart", 0, hB)
	seedHashedImage(t, db, "art-b", "fanart", 0, hB)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("RemediatePHashMismatches: %v", err)
	}
	if res.ArtistsProcessed != 0 || res.SlotsRemoved != 0 || res.Failures != 0 {
		t.Errorf("a path-less artist must be skipped without failing, got %+v", res)
	}
}

// TestRemediatePHashMismatches_AllArtistsRunsUnscoped pins that the explicit
// escape hatch actually works -- the guard is on FORGETTING the scope, not on
// library-wide runs as such.
func TestRemediatePHashMismatches_AllArtistsRunsUnscoped(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{}, PHashRemediateOpts{AllArtists: true, DryRun: true})
	if err != nil {
		t.Fatalf("an explicit AllArtists run must be permitted: %v", err)
	}
	// The collision is symmetric, so an unscoped pass sees BOTH sides of it --
	// A's polluted slot and B's own legitimate copy. That is the ambiguity the
	// human confirmation exists to absorb, and it is exactly why unscoped is
	// not the default.
	if len(res.Outcomes) != 2 {
		t.Errorf("an unscoped scan must surface both sides of the symmetric collision, got %+v", res.Outcomes)
	}
	for _, o := range res.Outcomes {
		if o.Action != "would-remove" {
			t.Errorf("dry run must only preview, got %q", o.Action)
		}
	}
	if _, err := os.Stat(filepath.Join(dirA, image.RepairDirName)); !os.IsNotExist(err) {
		t.Errorf("an unscoped dry run must still mutate nothing; stat err = %v", err)
	}
}

// TestRemediateAndRestore_UnwiredPipelineFailLoudly mirrors
// TestScanPHashMismatches_UnwiredPipelineFailsLoudly: a half-built pipeline must
// refuse rather than no-op its way to a "successful" run that removed nothing.
func TestRemediateAndRestore_UnwiredPipelineFailLoudly(t *testing.T) {
	p := NewPipeline(nil, nil, nil, nil, nil, testLogger())

	if _, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{}); err == nil {
		t.Error("an unwired remediate must fail loudly")
	}
	if _, err := p.RestorePHashQuarantine(context.Background(), "art-a", "op-x"); err == nil {
		t.Error("an unwired restore must fail loudly")
	}
}

// --------------------------------------------------------------------------
// Restore -- the index-shift contract
// --------------------------------------------------------------------------

// TestRestorePHashQuarantine_AppendsWithoutClobberingTheShiftedSurvivor is THE
// test for this PR. It proves the restore path is correct under the index shift
// that removal itself causes.
//
// The setup makes the stale index actively dangerous rather than merely wrong:
//
//	before: slot0=v0  slot1=v1(polluted)  slot2=v2
//	remove slot1, renumber ->
//	after:  slot0=v0  slot1=v2            (v2 MOVED DOWN into ordinal 1)
//
// The manifest recorded SlotIndex=1. Ordinal 1 now holds v2 -- an innocent
// bystander. A restore that trusted the recorded integer would write v1 over v2,
// destroying real artwork and re-creating the exact cross-artist corruption this
// feature exists to back out, while reporting success.
//
// Correct behavior: recognize v1 as absent, APPEND it at the next free ordinal,
// and leave v2 exactly where it is.
//
// Revert-and-rerun proof, MEASURED (both variants reported in the PR):
//
//	A. Point the append target at the recorded entry.SlotIndex, leaving
//	   restoreOneQuarantined's occupancy check in place -> RED, but via the
//	   check: "refusing to restore onto occupied path fanart2.jpg", surfacing
//	   as a restore failure rather than a clobber. The occupancy check is a
//	   real second line of defense, not decoration.
//	B. Point the append target at entry.SlotIndex AND drop the occupancy
//	   check -> RED on the assertion this test is named for: "restore
//	   CLOBBERED the shifted survivor: ordinal 1 holds v1, want v2", with
//	   fanart3.jpg absent. That is the artwork actually being destroyed.
//
// Restoring both makes it GREEN. Variant B is the one that shows what the bug
// costs; variant A is why it takes two mistakes, not one, to get there.
func TestRestorePHashQuarantine_AppendsWithoutClobberingTheShiftedSurvivor(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if res.SlotsRemoved != 1 {
		t.Fatalf("setup: expected the polluted slot to be removed, got %+v", res)
	}

	// Precondition for the whole point of this test: the recorded index now
	// denotes a DIFFERENT picture. If this ever stops holding, the test below
	// proves nothing and must be redesigned rather than deleted.
	m, err := image.ReadRepairManifest(dirA, res.OpID)
	if err != nil || m == nil || len(m.Entries) != 1 {
		t.Fatalf("setup: expected 1 manifest entry, got %+v (err %v)", m, err)
	}
	if m.Entries[0].SlotIndex != 1 {
		t.Fatalf("setup: expected the manifest to record slot 1, got %d", m.Entries[0].SlotIndex)
	}
	if got := fanartVariantAt(t, dirA, "fanart2.jpg", []int{0, 1, 2}); got != 2 {
		t.Fatalf("setup: ordinal 1 must now hold the SHIFTED survivor v2, holds v%d -- "+
			"without the shift this test cannot detect an index-trusting restore", got)
	}

	rres, err := p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID)
	if err != nil {
		t.Fatalf("RestorePHashQuarantine: %v", err)
	}
	if rres.Restored != 1 || rres.AlreadyPresent != 0 || len(rres.Failures) != 0 {
		t.Fatalf("want 1 restored / 0 already-present / 0 failures, got %+v", rres)
	}

	// THE ASSERTION: the bystander survived.
	if got := fanartVariantAt(t, dirA, "fanart2.jpg", []int{0, 1, 2}); got != 2 {
		t.Errorf("restore CLOBBERED the shifted survivor: ordinal 1 holds v%d, want v2. "+
			"This is the recorded-index-as-address bug", got)
	}
	// The restored picture is back, appended.
	if got := fanartVariantAt(t, dirA, "fanart3.jpg", []int{0, 1, 2}); got != 1 {
		t.Errorf("the restored picture must be appended at the next free ordinal, ordinal 2 holds v%d, want v1", got)
	}
	if got := fanartVariantAt(t, dirA, "fanart.jpg", []int{0, 1, 2}); got != 0 {
		t.Errorf("slot 0 must be untouched, holds v%d", got)
	}

	// The entry is consumed, so the quarantine does not advertise artwork it
	// has already returned.
	m, err = image.ReadRepairManifest(dirA, res.OpID)
	if err != nil {
		t.Fatalf("re-reading manifest: %v", err)
	}
	if m != nil && len(m.Entries) != 0 {
		t.Errorf("the restored entry must be consumed, manifest still holds %+v", m.Entries)
	}
}

// TestRestorePHashQuarantine_AlreadyPresentIsAnIdempotentNoOp pins retry safety.
// A restore interrupted after writing the bytes but before consuming the entry
// must, on re-run, recognize the picture as already back and consume the entry
// rather than append a second copy.
func TestRestorePHashQuarantine_AlreadyPresentIsAnIdempotentNoOp(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}

	if _, err := p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	countAfterFirst := len(discoverForTest(t, dirA))

	// Re-quarantine the same picture WITHOUT removing it, to reconstruct the
	// interrupted state: bytes on disk AND an unconsumed manifest entry.
	h1 := image.HashHex(pollutionHash(t, 1))
	if err := image.QuarantineImage(dirA, res.OpID, filepath.Join(dirA, "fanart3.jpg"), image.RepairEntry{
		ArtistID: "art-a", ArtistName: "Artist A", ImageType: "fanart",
		SlotIndex: 1, FileName: "fanart2.jpg", PHash: h1,
	}); err != nil {
		t.Fatalf("re-quarantining: %v", err)
	}

	rres, err := p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID)
	if err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if rres.AlreadyPresent != 1 || rres.Restored != 0 {
		t.Errorf("a picture already back must be a no-op, got %+v", rres)
	}
	if got := len(discoverForTest(t, dirA)); got != countAfterFirst {
		t.Errorf("an idempotent restore must not append a duplicate: %d slots before, %d after",
			countAfterFirst, got)
	}
	m, err := image.ReadRepairManifest(dirA, res.OpID)
	if err != nil {
		t.Fatalf("re-reading manifest: %v", err)
	}
	if m != nil && len(m.Entries) != 0 {
		t.Errorf("the no-op entry must still be consumed, manifest holds %+v", m.Entries)
	}
}

// TestRestorePHashQuarantine_RefusesToRestoreOntoAnOccupiedPath pins restore's
// last line of defense.
//
// It is not decoration: when the append target was reverted to the recorded
// index during this PR's revert-and-rerun, THIS check is what turned the clobber
// into a reported failure. Discovery only counts recognized artwork, so a stray
// entry can occupy the computed target without being a slot -- here a directory,
// which discovery skips. Overwriting it would destroy a file this feature never
// took, so restore refuses and says why.
func TestRestorePHashQuarantine_RefusesToRestoreOntoAnOccupiedPath(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}

	// Two slots survive, so restore will compute ordinal 2 -> fanart3.jpg. A
	// directory there is occupied but NOT discoverable as a slot.
	blocker := filepath.Join(dirA, "fanart3.jpg")
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatalf("creating blocker: %v", err)
	}

	rres, err := p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID)
	if err != nil {
		t.Fatalf("the run must not error; per-entry failures are collected: %v", err)
	}
	if rres.Restored != 0 || len(rres.Failures) != 1 {
		t.Fatalf("want 0 restored / 1 failure, got %+v", rres)
	}
	if !strings.Contains(rres.Failures[0], "occupied path") {
		t.Errorf("the failure must name the occupancy refusal, got %q", rres.Failures[0])
	}
	// The blocker is untouched, and the entry is NOT consumed -- the artwork
	// is still recoverable once the operator clears the obstruction.
	if fi, statErr := os.Stat(blocker); statErr != nil || !fi.IsDir() {
		t.Errorf("the blocking entry must be untouched; stat err = %v", statErr)
	}
	m, err := image.ReadRepairManifest(dirA, res.OpID)
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m == nil || len(m.Entries) != 1 {
		t.Errorf("a refused restore must NOT consume the entry, got %+v", m)
	}
}

// TestRestorePHashQuarantine_RecognizesAReEncodedCopyAsAlreadyPresent exercises
// the PERCEPTUAL arm of the already-present check, which the byte-equal arm
// otherwise shadows.
//
// It is the arm that matters in practice: a picture that came back through a
// different path (a re-fetch, a platform round-trip, a format conversion) is the
// same photograph but not the same bytes. Appending it again would hand the
// operator a duplicate backdrop and re-arm the duplicate detector against them.
func TestRestorePHashQuarantine_RecognizesAReEncodedCopyAsAlreadyPresent(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}

	// Put the removed picture back by hand as a RE-ENCODE: same photograph,
	// different bytes. Two survivors, so the next free ordinal is fanart3.jpg.
	reencoded := reEncodeJPEG(t, pollutionJPEG(t, 1), 60)
	if bytes.Equal(reencoded, pollutionJPEG(t, 1)) {
		t.Fatal("precondition: the re-encode must differ byte-wise, or this test shadows the byte-equal arm")
	}
	if err := os.WriteFile(filepath.Join(dirA, "fanart3.jpg"), reencoded, 0o644); err != nil {
		t.Fatalf("writing re-encode: %v", err)
	}
	before := len(discoverForTest(t, dirA))

	rres, err := p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID)
	if err != nil {
		t.Fatalf("RestorePHashQuarantine: %v", err)
	}
	if rres.AlreadyPresent != 1 || rres.Restored != 0 {
		t.Errorf("a re-encoded copy must count as already present, got %+v", rres)
	}
	if got := len(discoverForTest(t, dirA)); got != before {
		t.Errorf("no duplicate may be appended: %d slots before, %d after", before, got)
	}
}

// TestReverifySlotPHash_RefusesAnEmptyFlaggedHash pins "unknown never matches
// unknown" on the DELETION path.
//
// An empty or zero phash is UNKNOWN, not a value -- it is Hamming-distance-0
// from every other unknown. The detector already buckets empty-hash slots as
// indeterminate and never raises them as suspects, so this is defense in depth
// against a caller that hands the removal path a slot the detector would not
// have. On a path that deletes artwork, "we do not know what this is" must be a
// refusal, not a wildcard.
func TestReverifySlotPHash_RefusesAnEmptyFlaggedHash(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	ok, reason := p.reverifySlotPHash(filepath.Join(dirA, "fanart.jpg"), "")
	if ok {
		t.Fatal("an empty flagged hash must never authorize a removal")
	}
	if !strings.Contains(reason, "unknown hash") {
		t.Errorf("the refusal must name the unknown hash, got %q", reason)
	}
}

// TestRestorePHashQuarantine_UnknownOpIsAnError pins that a restore against an
// operation that does not exist fails loudly rather than reporting a successful
// zero-entry restore.
func TestRestorePHashQuarantine_UnknownOpIsAnError(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	seedPollutedLibrary(t, db)

	_, err := p.RestorePHashQuarantine(context.Background(), "art-a", "op-does-not-exist")
	if err == nil {
		t.Fatal("restoring an unknown op must be an error, not a silent zero-entry success")
	}
	if !strings.Contains(err.Error(), "no repair operation") {
		t.Errorf("expected an unknown-op error, got: %v", err)
	}
}

// --------------------------------------------------------------------------
// #2564 AC: the rule package's provenance slot index
// --------------------------------------------------------------------------

// TestExistingImageFileNames_NeverResolvesANonPrimaryFanartSlot pins the
// structural invariant that recordSavedImageProvenanceSlot0 depends on.
//
// #2564's AC names internal/rule/fixers.go alongside internal/api's call site
// because both passed a hard-coded slot 0. The API's was a real defect and took
// a slotIndex parameter (#2574). This package's 0 is correct -- but only because
// no path here can write a numbered fanart slot, and THAT is the fact worth
// guarding. Asserting the literal would prove nothing; asserting reachability
// catches the change that would make the literal wrong.
//
// So: even with numbered variants sitting on disk, name resolution must return
// only primary aliases. If a future caller teaches this package to append a
// backdrop, this test fails and points at the provenance recorder that must then
// become slot-aware -- rather than the phash of an appended file silently
// landing on slot 0's row, which is exactly the starved/wrong-hash state that
// makes the #2564 detector unreliable.
func TestExistingImageFileNames_NeverResolvesANonPrimaryFanartSlot(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg", "backdrop.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	// Precondition: the numbered variants really are discoverable slots, so a
	// "no numbered names" result below means resolution excluded them rather
	// than that they were never there.
	if got := len(discoverForTest(t, dir)); got != 3 {
		t.Fatalf("precondition: expected 3 discoverable fanart slots, got %d", got)
	}

	// platformService is nil, exercising the img.DefaultFileNames fallback --
	// the same path both production callers take when no profile is active.
	names := existingImageFileNames(context.Background(), dir, "fanart", nil)
	if len(names) == 0 {
		t.Fatal("expected at least the primary fanart name")
	}
	for _, name := range names {
		if name != "fanart.jpg" && name != "fanart.png" && name != "backdrop.jpg" && name != "backdrop.png" {
			t.Errorf("name resolution returned %q, a NON-primary fanart slot. The hard-coded "+
				"slot 0 in recordSavedImageProvenanceSlot0 is now wrong: that write must "+
				"record against the slot it actually wrote", name)
		}
	}

	// And the names it did return all sort to ordinal 0 under DiscoverFanart,
	// which is the other half of the argument.
	for _, name := range names {
		paths, err := image.DiscoverFanart(dir, name)
		if err != nil {
			t.Fatalf("discovering with primary %q: %v", name, err)
		}
		if len(paths) == 0 || filepath.Base(paths[0]) != name {
			t.Errorf("primary %q must sort to ordinal 0, discovery gave %v", name, paths)
		}
	}
}

// reEncodeJPEG decodes and re-encodes data at the given quality, yielding the
// same photograph as different bytes.
func reEncodeJPEG(t *testing.T, data []byte, quality int) []byte {
	t.Helper()
	m, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decoding for re-encode: %v", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("re-encoding: %v", err)
	}
	return buf.Bytes()
}

// discoverForTest returns the artist's fanart paths under the default naming.
func discoverForTest(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := image.DiscoverFanart(dir, "fanart.jpg")
	if err != nil {
		t.Fatalf("discovering fanart: %v", err)
	}
	return paths
}
