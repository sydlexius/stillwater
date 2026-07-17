package rule

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/publish"
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
	// The audit trail must not lie: a slot staged for removal whose commit
	// failed and was rolled back must be reported "failed", never "removed".
	// The outcome is stamped only AFTER the commit confirms; before this fix it
	// was set to "removed" inside the suspect loop and left standing on the
	// failure path, claiming a removal that never happened.
	if len(res.Outcomes) != 1 {
		t.Fatalf("want exactly 1 outcome for the staged-then-failed slot, got %+v", res.Outcomes)
	}
	if res.Outcomes[0].Action != "failed" {
		t.Errorf("a rolled-back slot must be reported %q, not %q (%+v)",
			"failed", res.Outcomes[0].Action, res.Outcomes[0])
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

// TestRestorePHashQuarantine_AReEncodedCopyIsRetainedForReviewNotConsumed
// exercises the PERCEPTUAL arm, which the byte-equal arm otherwise shadows.
//
// It was called ...AsAlreadyPresent, which is now the name of a DIFFERENT and
// incompatible outcome ("an identical copy is on disk, nothing is needed"). A
// re-encode is not that: the quarantine still holds the only copy of the
// original bytes and a human has to settle it. The old name asserted the
// opposite of what this pins.
//
// It is the arm that matters in practice: a picture that came back through a
// different path (a re-fetch, a platform round-trip, a format conversion) is the
// same photograph but not the same bytes. Appending it again would hand the
// operator a duplicate backdrop and re-arm the duplicate detector against them.
func TestRestorePHashQuarantine_AReEncodedCopyIsRetainedForReviewNotConsumed(t *testing.T) {
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
	// A re-encode is a PERCEPTUAL match, so it is the RETAINED arm -- not
	// AlreadyPresent, which means "an identical copy is on disk, nothing is
	// needed". Asserting AlreadyPresent here used to pass only because the two
	// facts shared one counter; now that they are distinct, so is this.
	if rres.NeedsReview != 1 || rres.AlreadyPresent != 0 || rres.Restored != 0 {
		t.Errorf("a re-encoded copy must be RETAINED for review, not reported as already present: %+v", rres)
	}
	if got := len(discoverForTest(t, dirA)); got != before {
		t.Errorf("no duplicate may be appended: %d slots before, %d after", before, got)
	}

	// The entry is RETAINED, and this assertion is the point. A re-encode is a
	// PERCEPTUAL match, not a byte match: the quarantine still holds the only
	// copy of the ORIGINAL bytes, and the thing on disk is a lossier version of
	// them. Consuming here would destroy the better copy in favor of the worse
	// one and call it success.
	//
	// This test previously asserted only the counters, so it passed just as
	// happily against the version that consumed on a perceptual match -- it was
	// blessing the blocker rather than pinning a contract. Asked "what broken
	// behavior still passes this?", the answer was "the one that deletes the
	// artwork". It is asked and answered now.
	m, err := image.ReadRepairManifest(dirA, res.OpID)
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m == nil || len(m.Entries) != 1 {
		t.Fatalf("a perceptual-only match must RETAIN the entry -- the quarantine holds "+
			"the only copy of the original bytes. manifest = %+v", m)
	}
	data, err := image.RepairEntryBytes(dirA, res.OpID, m.Entries[0])
	if err != nil {
		t.Fatalf("the quarantined bytes must still be readable: %v", err)
	}
	if !bytes.Equal(data, pollutionJPEG(t, 1)) {
		t.Error("the retained bytes must be exactly the removed picture")
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

// --------------------------------------------------------------------------
// #2564 AC: the provenance recorder itself
// --------------------------------------------------------------------------

// captureProvenanceRecorder records the slot index it was asked to write.
type captureProvenanceRecorder struct {
	calls []int // slotIndex per call
	err   error
}

func (c *captureProvenanceRecorder) UpdateImageProvenance(_ context.Context, _, _ string, slotIndex int, _, _, _, _, _ string) error {
	c.calls = append(c.calls, slotIndex)
	return c.err
}

// TestRecordSavedImageProvenance_WritesThePrimarySlot pins #2564's AC for this
// package: the rule engine's post-save provenance write lands on slot 0.
//
// The companion test TestExistingImageFileNames_NeverResolvesANonPrimaryFanartSlot
// proves WHY 0 is the right answer (nothing here can write a non-primary slot).
// This proves the recorder actually writes it -- the two together are what turn
// "correct only by accident" into a checked property. Neither alone is enough:
// the invariant test never calls the recorder, and this test would still pass if
// the invariant broke.
func TestRecordSavedImageProvenance_WritesThePrimarySlot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fanart.jpg")
	if err := os.WriteFile(path, pollutionJPEG(t, 0), 0o644); err != nil {
		t.Fatalf("writing image: %v", err)
	}

	// Precondition: provenance is actually collectable from this file. With an
	// empty ProvenanceData the recorder returns early and never writes, so the
	// slot assertion below would pass vacuously against a broken recorder.
	if image.CollectProvenance(path, testLogger()).IsEmpty() {
		t.Fatal("precondition: the fixture must yield collectable provenance, " +
			"or the slot assertion never runs")
	}

	rec := &captureProvenanceRecorder{}
	recordSavedImageProvenance(context.Background(), rec, "art-a", "fanart", path, testLogger())

	if len(rec.calls) != 1 {
		t.Fatalf("expected exactly one provenance write, got %d", len(rec.calls))
	}
	if rec.calls[0] != 0 {
		t.Errorf("the rule engine's post-save provenance write must land on slot 0, got %d", rec.calls[0])
	}
}

// TestRecordSavedImageProvenance_SkipsWhenNothingCollectable pins that a file
// yielding no provenance is skipped rather than written as a row of empties,
// which would stamp slot 0 with blanks and look to a per-slot phash reader like
// a slot that was measured and found to have no hash.
func TestRecordSavedImageProvenance_SkipsWhenNothingCollectable(t *testing.T) {
	rec := &captureProvenanceRecorder{}
	recordSavedImageProvenance(context.Background(), rec,
		"art-a", "fanart", filepath.Join(t.TempDir(), "absent.jpg"), testLogger())

	if len(rec.calls) != 0 {
		t.Errorf("a file with no collectable provenance must not be written, got %d call(s)", len(rec.calls))
	}
}

// --------------------------------------------------------------------------
// Remediation failure branches -- the hinge, at the caller's level
// --------------------------------------------------------------------------

// TestRemediatePHashMismatches_QuarantineFailureLeavesTheOriginalInPlace is the
// most important of these: it proves the safety hinge end-to-end from 3b's side.
//
// If the quarantine cannot store the bytes, the removal MUST NOT proceed. The
// primitive returns an error for exactly this reason; this asserts that the
// caller honors it -- the artist's file is still on disk, unstaged, and the run
// reports the artist as failed rather than repaired.
func TestRemediatePHashMismatches_QuarantineFailureLeavesTheOriginalInPlace(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	// A regular file where the quarantine root must be: MkdirAll cannot
	// descend through it, for root as much as anyone.
	if err := os.WriteFile(filepath.Join(dirA, image.RepairDirName), []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding blocker: %v", err)
	}

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("the run must not error; per-artist failures are counted: %v", err)
	}
	if res.Failures != 1 {
		t.Fatalf("a quarantine failure must fail the artist, got %+v", res)
	}
	if res.SlotsRemoved != 0 {
		t.Errorf("nothing may be removed when the quarantine failed, got %d", res.SlotsRemoved)
	}

	// THE ASSERTION: all three originals survive, unstaged.
	for i, name := range []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg"} {
		if got := fanartVariantAt(t, dirA, name, []int{0, 1, 2}); got != i {
			t.Errorf("%s must be untouched holding v%d, holds v%d", name, i, got)
		}
	}
	entries, _ := os.ReadDir(dirA)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), phashTombSuffix) {
			t.Errorf("nothing may be staged when the quarantine failed, found %s", e.Name())
		}
	}
}

// TestRemediatePHashMismatches_StaleTombBlocksStagingAndFailsLoudly pins the
// leftover-tomb path: a previous run crashed between staging and commit, leaving
// something at the tomb path that cannot be cleared. Staging must abort rather
// than proceed on an ambiguous filesystem, and the artist's slots must survive.
//
// A non-empty directory at the tomb path makes the clearing os.Remove return
// ENOTEMPTY -- deterministic and host-independent, unlike a permission denial.
func TestRemediatePHashMismatches_StaleTombBlocksStagingAndFailsLoudly(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	tomb := filepath.Join(dirA, "fanart2.jpg"+phashTombSuffix)
	if err := os.Mkdir(tomb, 0o755); err != nil {
		t.Fatalf("creating stale tomb dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tomb, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatalf("populating stale tomb dir: %v", err)
	}

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("the run must not error; per-artist failures are counted: %v", err)
	}
	if res.Failures != 1 {
		t.Fatalf("an unclearable stale tomb must fail the artist, got %+v", res)
	}
	if res.SlotsRemoved != 0 {
		t.Errorf("nothing may be reported removed, got %d", res.SlotsRemoved)
	}
	// The polluted slot is still there: staging never happened.
	if got := fanartVariantAt(t, dirA, "fanart2.jpg", []int{0, 1, 2}); got != 1 {
		t.Errorf("the slot must survive a failed staging, holds v%d", got)
	}
}

// TestRemediatePHashMismatches_SkipsASlotThatCannotBeHashed pins re-verification
// against a file that is no longer a decodable image -- a truncated or corrupted
// write. It cannot be confirmed as the flagged picture, so it must be skipped,
// not deleted on the strength of a stale DB hash.
func TestRemediatePHashMismatches_SkipsASlotThatCannotBeHashed(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	// Still discoverable (.jpg), no longer decodable. The DB still claims v1.
	if err := os.WriteFile(filepath.Join(dirA, "fanart2.jpg"), []byte("not a jpeg at all"), 0o644); err != nil {
		t.Fatalf("corrupting slot: %v", err)
	}

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("RemediatePHashMismatches: %v", err)
	}
	if res.SlotsRemoved != 0 || res.SlotsSkipped != 1 {
		t.Fatalf("an unhashable slot must be skipped, not removed: %+v", res)
	}
	if !strings.Contains(res.Outcomes[0].Reason, "re-hashing slot") {
		t.Errorf("the skip must name the hash failure, got %q", res.Outcomes[0].Reason)
	}
	if _, err := os.Stat(filepath.Join(dirA, "fanart2.jpg")); err != nil {
		t.Errorf("the unhashable slot must survive: %v", err)
	}
}

// TestRemediatePHashMismatches_ArtistPathThatIsNotADirectoryFails pins that a
// library path which is no longer a directory fails the artist loudly rather
// than being treated as an artist with nothing to repair.
func TestRemediatePHashMismatches_ArtistPathThatIsNotADirectoryFails(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	// Point the artist at a regular file. DiscoverFanart's ReadDir then fails
	// with ENOTDIR -- a type error, not a permission one.
	notADir := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding file: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`UPDATE artists SET path = ? WHERE id = 'art-a'`, notADir); err != nil {
		t.Fatalf("repointing artist: %v", err)
	}

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("the run must not error; per-artist failures are counted: %v", err)
	}
	if res.Failures != 1 || res.SlotsRemoved != 0 {
		t.Errorf("an unreadable artist dir must fail the artist and remove nothing, got %+v", res)
	}
	_ = dirA
}

// --------------------------------------------------------------------------
// Restore failure branches
// --------------------------------------------------------------------------

// TestRestorePHashQuarantine_UnknownArtistIsAnError pins that restore refuses an
// artist it cannot load rather than reporting a successful zero-entry restore.
func TestRestorePHashQuarantine_UnknownArtistIsAnError(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	seedPollutedLibrary(t, db)

	if _, err := p.RestorePHashQuarantine(context.Background(), "art-does-not-exist", "op-x"); err == nil {
		t.Fatal("restoring for an unknown artist must error")
	}
}

// TestRestorePHashQuarantine_ArtistWithNoPathIsAnError pins the same for an
// artist whose directory was never resolved: there is nowhere to restore TO, and
// saying so beats writing into "".
func TestRestorePHashQuarantine_ArtistWithNoPathIsAnError(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	seedRepairArtist(t, db, "art-nopath", "No Path", "")

	_, err := p.RestorePHashQuarantine(context.Background(), "art-nopath", "op-x")
	if err == nil {
		t.Fatal("restoring for a path-less artist must error")
	}
	if !strings.Contains(err.Error(), "no path") {
		t.Errorf("the error must name the missing path, got: %v", err)
	}
}

// --------------------------------------------------------------------------
// Metadata sync after irreversible work must not report success as failure
// --------------------------------------------------------------------------

// captureWarnPipeline builds a repair pipeline whose logger writes WARN+ lines
// to the returned buffer, so a test can assert a soft-failure path actually
// surfaced a warning rather than merely swallowing an error and returning nil.
func captureWarnPipeline(t *testing.T, e *Engine, db *sql.DB) (*Pipeline, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return NewPipeline(e, artist.NewService(db), nil, nil, nil, logger), buf
}

// blockArtistUpdates installs a trigger that aborts every UPDATE on the artists
// table while leaving reads and inserts working.
//
// It is the deterministic way to make artistService.Update fail AFTER the scan
// and after the on-disk work: closing the DB would fail the scan first and never
// reach the metadata sync, and the concrete *artist.Service field cannot be
// stubbed. The trigger touches only the artists table, so the image-hash
// recorder's writes to artist_images (which the renumber depends on) still work.
func blockArtistUpdates(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`CREATE TRIGGER block_artist_update BEFORE UPDATE ON artists
		 BEGIN SELECT RAISE(ABORT, 'artist update blocked for test'); END`); err != nil {
		t.Fatalf("installing artist-update block trigger: %v", err)
	}
}

// TestRemediatePHashMismatches_MetadataSyncFailureStillReportsRemovalSuccess
// pins that a failed artist metadata sync AFTER the irreversible on-disk removal
// is reported as a warning, not as the operation failing.
//
// The removal (quarantine, stage, renumber, unlink tombs) has already committed
// by the time Update runs; it cannot be undone. Reporting the metadata miss as a
// failure would invite an operator to re-run a destructive path that already
// succeeded. The result must stay successful and a warning must be surfaced.
func TestRemediatePHashMismatches_MetadataSyncFailureStillReportsRemovalSuccess(t *testing.T) {
	e, db := newDupTestEngine(t)
	p, logs := captureWarnPipeline(t, e, db)
	dirA := seedPollutedLibrary(t, db)

	// Fail the metadata sync only; the scan and the on-disk work must still run.
	blockArtistUpdates(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("a metadata sync miss after an irreversible removal must not fail the run: %v", err)
	}
	// (1) The destructive result is still reported successful.
	if res.SlotsRemoved != 1 || res.Quarantined != 1 || res.Failures != 0 {
		t.Fatalf("want 1 removed / 1 quarantined / 0 failures despite the sync miss, got %+v", res)
	}
	if len(res.Outcomes) != 1 || res.Outcomes[0].Action != "removed" {
		t.Fatalf("the committed slot must be reported removed, got %+v", res.Outcomes)
	}
	// The removal really happened on disk: the polluted picture is gone and the
	// gap is closed. Assert the artifact, not just the counters.
	if got := fanartVariantAt(t, dirA, "fanart.jpg", []int{0, 1, 2}); got != 0 {
		t.Errorf("slot 0 must still hold v0, holds v%d", got)
	}
	if got := fanartVariantAt(t, dirA, "fanart2.jpg", []int{0, 1, 2}); got != 2 {
		t.Errorf("slot 1 must hold v2 after renumbering, holds v%d", got)
	}
	if _, err := os.Stat(filepath.Join(dirA, "fanart3.jpg")); !os.IsNotExist(err) {
		t.Errorf("the gap must be closed, fanart3.jpg still present; stat err = %v", err)
	}
	// (2) A warning was surfaced.
	if !strings.Contains(logs.String(), "metadata sync failed") {
		t.Errorf("a metadata sync miss must surface a warning; log was: %q", logs.String())
	}
}

// TestRestorePHashQuarantine_MetadataSyncFailureStillReportsRestoreSuccess pins
// the same contract on the restore path: by the time Update runs, the artwork is
// already back on disk. A metadata sync miss must warn and preserve the
// successful restore, not report the recovered artwork as a failed restore.
func TestRestorePHashQuarantine_MetadataSyncFailureStillReportsRestoreSuccess(t *testing.T) {
	e, db := newDupTestEngine(t)
	p, logs := captureWarnPipeline(t, e, db)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("remediate setup: %v", err)
	}
	if res.SlotsRemoved != 1 {
		t.Fatalf("setup: expected the polluted slot removed, got %+v", res)
	}

	// Arm the failure only now: the removal is committed, the restore is next.
	blockArtistUpdates(t, db)
	logs.Reset()

	rres, err := p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID)
	if err != nil {
		t.Fatalf("a metadata sync miss after the artwork is back must not fail the restore: %v", err)
	}
	// (1) The restore is still reported successful.
	if rres.Restored != 1 || len(rres.Failures) != 0 {
		t.Fatalf("want 1 restored / 0 failures despite the sync miss, got %+v", rres)
	}
	// The artwork really is back on disk, appended.
	if got := fanartVariantAt(t, dirA, "fanart3.jpg", []int{0, 1, 2}); got != 1 {
		t.Errorf("the restored picture must be on disk at the next free ordinal, ordinal 2 holds v%d", got)
	}
	// The entry is consumed -- the bytes are back, so the quarantine has no claim.
	m, err := image.ReadRepairManifest(dirA, res.OpID)
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m != nil && len(m.Entries) != 0 {
		t.Errorf("a successful restore must consume its entry, manifest holds %+v", m.Entries)
	}
	// (2) A warning was surfaced.
	if !strings.Contains(logs.String(), "metadata sync failed") {
		t.Errorf("a metadata sync miss must surface a warning; log was: %q", logs.String())
	}
}

// --------------------------------------------------------------------------
// Re-verify the matched counterpart before authorizing a delete
// --------------------------------------------------------------------------

// TestRemediatePHashMismatches_SkipsWhenMatchedCounterpartChangedOnDisk pins the
// second half of the re-verification safeguard.
//
// A removal is authorized by a PAIR: the suspect looks like some OTHER artist's
// fanart. reverifySlotPHash re-hashes only the suspect's own bytes; the matched
// counterpart's hash comes from the scan's cache and is otherwise never re-read.
// So a counterpart that changed on disk after the scan but before the commit
// would leave the collision no longer holding, yet the stale cache would still
// authorize a destructive removal. The counterpart must be re-read and the match
// re-confirmed; a stale counterpart is a SKIP, not a delete.
func TestRemediatePHashMismatches_SkipsWhenMatchedCounterpartChangedOnDisk(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	seedRepairArtist(t, db, "art-a", "Artist A", dirA)
	seedRepairArtist(t, db, "art-b", "Artist B", dirB)

	h0 := writePollutionFanart(t, dirA, "fanart.jpg", 0)
	h1 := writePollutionFanart(t, dirA, "fanart2.jpg", 1) // the pollution
	h2 := writePollutionFanart(t, dirA, "fanart3.jpg", 2)
	hB := writePollutionFanart(t, dirB, "fanart.jpg", 1) // B's copy: the counterpart

	seedHashedImage(t, db, "art-a", "fanart", 0, h0)
	seedHashedImage(t, db, "art-a", "fanart", 1, h1)
	seedHashedImage(t, db, "art-a", "fanart", 2, h2)
	seedHashedImage(t, db, "art-b", "fanart", 0, hB)

	if h1 != hB {
		t.Fatalf("precondition: the pollution and B's copy must be the same picture, got %s vs %s", h1, hB)
	}

	// The counterpart changes on disk AFTER the scan's cache was written: B's
	// slot 0 now holds a DIFFERENT picture (v2), while the DB still says hB (v1).
	// The scan still finds the collision from the stale cache; the live
	// counterpart no longer matches.
	writePollutionFanart(t, dirB, "fanart.jpg", 2)

	// Precondition: the new counterpart must genuinely fall outside the
	// tolerance, or the skip below would prove nothing.
	if sim := image.Similarity(pollutionHash(t, 1), pollutionHash(t, 2)); sim >= defaultPHashMismatchTolerance {
		t.Fatalf("precondition: v1 and v2 must be distinguishable, similarity %.4f >= tolerance", sim)
	}

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("RemediatePHashMismatches: %v", err)
	}
	// The suspect's OWN bytes still match its flagged hash, so the suspect-only
	// safeguard would have authorized the delete. The counterpart re-check is
	// what must stop it.
	if res.SlotsRemoved != 0 || res.Quarantined != 0 {
		t.Errorf("a suspect whose counterpart no longer matches must not be removed, got %+v", res)
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
	if !strings.Contains(res.Outcomes[0].Reason, "counterpart") {
		t.Errorf("the skip must name the counterpart re-verification, got %q", res.Outcomes[0].Reason)
	}
	// A's suspect slot survived: nothing was deleted on an unreproducible signal.
	if got := fanartVariantAt(t, dirA, "fanart2.jpg", []int{0, 1, 2}); got != 1 {
		t.Errorf("the suspect slot must survive, slot 1 holds v%d", got)
	}
}

// TestRemediatePHashMismatches_SkipsWhenMatchedCounterpartRemovedFromDisk pins
// the other half of the counterpart re-verification: the counterpart file is
// GONE, not merely changed.
//
// The scan's cache still records the counterpart's hash, so the collision is
// still reported; but the file that gave the removal its only corroboration is
// no longer on disk. An absent counterpart must NOT authorize a delete -- it is
// strictly a skip, same as a counterpart that no longer matches.
func TestRemediatePHashMismatches_SkipsWhenMatchedCounterpartRemovedFromDisk(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	seedRepairArtist(t, db, "art-a", "Artist A", dirA)
	seedRepairArtist(t, db, "art-b", "Artist B", dirB)

	h0 := writePollutionFanart(t, dirA, "fanart.jpg", 0)
	h1 := writePollutionFanart(t, dirA, "fanart2.jpg", 1) // the pollution
	h2 := writePollutionFanart(t, dirA, "fanart3.jpg", 2)
	hB := writePollutionFanart(t, dirB, "fanart.jpg", 1) // B's copy: the counterpart

	seedHashedImage(t, db, "art-a", "fanart", 0, h0)
	seedHashedImage(t, db, "art-a", "fanart", 1, h1)
	seedHashedImage(t, db, "art-a", "fanart", 2, h2)
	seedHashedImage(t, db, "art-b", "fanart", 0, hB)

	if h1 != hB {
		t.Fatalf("precondition: the pollution and B's copy must be the same picture, got %s vs %s", h1, hB)
	}

	// The counterpart file vanishes after the scan's cache was written: the DB
	// still says B slot 0 = hB, but the disk no longer holds it.
	if err := os.Remove(filepath.Join(dirB, "fanart.jpg")); err != nil {
		t.Fatalf("removing counterpart: %v", err)
	}

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("RemediatePHashMismatches: %v", err)
	}
	if res.SlotsRemoved != 0 || res.Quarantined != 0 {
		t.Errorf("a suspect whose counterpart is gone must not be removed, got %+v", res)
	}
	if res.SlotsSkipped != 1 {
		t.Fatalf("want 1 skipped slot, got %d (%+v)", res.SlotsSkipped, res.Outcomes)
	}
	if len(res.Outcomes) != 1 || res.Outcomes[0].Action != "skipped" {
		t.Fatalf("expected a skipped outcome, got %+v", res.Outcomes)
	}
	if !strings.Contains(res.Outcomes[0].Reason, "counterpart slot no longer exists") {
		t.Errorf("the skip must name the vanished counterpart, got %q", res.Outcomes[0].Reason)
	}
	if got := fanartVariantAt(t, dirA, "fanart2.jpg", []int{0, 1, 2}); got != 1 {
		t.Errorf("the suspect slot must survive, slot 1 holds v%d", got)
	}
}

// TestRestorePHashQuarantine_UnreadableManifestPropagates pins that a corrupted
// manifest stops the restore. Continuing would silently return artwork the
// operator cannot account for, on the one path that exists to recover it.
func TestRestorePHashQuarantine_UnreadableManifestPropagates(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	opDir := filepath.Join(dirA, image.RepairDirName, "op-corrupt")
	if err := os.MkdirAll(opDir, 0o750); err != nil {
		t.Fatalf("creating op dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(opDir, "manifest.json"), []byte("{nope"), 0o644); err != nil {
		t.Fatalf("writing corrupt manifest: %v", err)
	}

	_, err := p.RestorePHashQuarantine(context.Background(), "art-a", "op-corrupt")
	if err == nil {
		t.Fatal("an unreadable manifest must stop the restore")
	}
	if !strings.Contains(err.Error(), "decoding repair manifest") {
		t.Errorf("expected a manifest decode error, got: %v", err)
	}
}

// TestRestorePHashQuarantine_MissingBytesFailThatEntryNotTheRun pins per-entry
// isolation: an entry whose bytes are gone is reported as that entry's failure,
// and its manifest record is KEPT (nothing was restored, so nothing may be
// consumed) rather than aborting the whole restore.
func TestRestorePHashQuarantine_MissingBytesFailThatEntryNotTheRun(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	m, err := image.ReadRepairManifest(dirA, res.OpID)
	if err != nil || m == nil || len(m.Entries) != 1 {
		t.Fatalf("setup: expected 1 entry, got %+v (err %v)", m, err)
	}

	// Delete the quarantined bytes out from under the manifest.
	stored := filepath.Join(dirA, image.RepairDirName, res.OpID, m.Entries[0].StoredName)
	if err := os.Remove(stored); err != nil {
		t.Fatalf("removing quarantined bytes: %v", err)
	}

	rres, err := p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID)
	if err != nil {
		t.Fatalf("the run must not error; per-entry failures are collected: %v", err)
	}
	if rres.Restored != 0 || len(rres.Failures) != 1 {
		t.Fatalf("want 0 restored / 1 failure, got %+v", rres)
	}
	// The entry is NOT consumed: nothing came back, so the record must stand.
	m, err = image.ReadRepairManifest(dirA, res.OpID)
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m == nil || len(m.Entries) != 1 {
		t.Errorf("a failed restore must not consume its entry, got %+v", m)
	}
}

// TestRestorePHashQuarantine_HashInvalidationFailureDoesNotFailTheRestore pins a
// contract that is easy to get backwards: by the time the hashes are
// invalidated, the artwork is ALREADY BACK ON DISK. The restore succeeded.
//
// A failure to drop the artist's stale fanart hashes is a cache problem -- the
// next scan re-derives them -- so it must be logged and the entry still counted
// restored. Propagating it would report a successful recovery as a failure, and
// an operator retrying "the failed restore" would find the picture already
// present and be told nothing happened. The entry must also still be consumed:
// the bytes are back, so the quarantine has no further claim on them.
//
// REMEDIATION IS DELIBERATELY THE OPPOSITE, and the asymmetry is the point.
// img.RenumberFanart invalidates BEFORE its destructive rename and propagates
// the error, so an invalidation failure there ABORTS the removal -- correct,
// because nothing has been destroyed yet and proceeding would leave a stale hash
// pointing at a reshuffled slot. Here the write has already happened, so the
// same failure must not be fatal. Fail-closed before the mutation, fail-open
// after it. (Measured: a recorder that fails from the start never reaches this
// test's restore -- it fails the remediate setup instead, via RenumberFanart.
// Hence the mid-test arming below rather than a recorder that fails throughout.)
func TestRestorePHashQuarantine_HashInvalidationFailureDoesNotFailTheRestore(t *testing.T) {
	e, db := newDupTestEngine(t)
	failing := &fakeHashRecorder{}
	e.SetImageHashRecorder(failing)
	p := NewPipeline(e, artist.NewService(db), nil, nil, nil, testLogger())
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if res.SlotsRemoved != 1 {
		t.Fatalf("setup: expected the polluted slot removed, got %+v", res)
	}

	// Arm the failure only now: the removal is committed, the restore is next.
	failing.invalidateErr = errors.New("hash store offline")
	failing.invalidated = nil

	rres, err := p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID)
	if err != nil {
		t.Fatalf("a hash-invalidation failure must not fail the restore: %v", err)
	}
	if rres.Restored != 1 || len(rres.Failures) != 0 {
		t.Fatalf("the artwork is back, so the entry must count restored: %+v", rres)
	}

	// Precondition for this test meaning anything: the failing invalidator was
	// actually reached. Without this the assertions above pass against a
	// pipeline that never called it.
	if len(failing.invalidated) == 0 {
		t.Fatal("precondition: the invalidator was never called, so its failure was never exercised")
	}

	// The picture really is back on disk, appended.
	if got := fanartVariantAt(t, dirA, "fanart3.jpg", []int{0, 1, 2}); got != 1 {
		t.Errorf("the restored picture must be on disk at the next free ordinal, ordinal 2 holds v%d", got)
	}
	// And the entry is consumed -- the quarantine has no claim on returned bytes.
	m, err := image.ReadRepairManifest(dirA, res.OpID)
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m != nil && len(m.Entries) != 0 {
		t.Errorf("a successful restore must consume its entry, manifest holds %+v", m.Entries)
	}
}

// nearMissJPEG returns a picture built from pollutionJPEG(variant)'s block
// structure with its first four blocks brightened -- a DIFFERENT photograph
// whose dHash still lands inside the restore's tolerance. It models the
// everyday case the restore path has to survive: a same-shoot frame, a crop
// variant, a re-encode at another quality. This repo ships a whole duplicate
// fixer precisely because such near-duplicates are common.
func nearMissJPEG(t *testing.T, variant int) []byte {
	t.Helper()
	const blocks, w, h = 8, 640, 360
	m := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			bx, by := x*blocks/w, y*blocks/h
			hsh := uint32(bx)*374761393 + uint32(by)*668265263 + uint32(variant)*2246822519
			hsh ^= hsh >> 13
			hsh *= 1274126177
			hsh ^= hsh >> 16
			v := int(uint8(hsh >> 8))
			if by*blocks+bx < 4 {
				if v += 90; v > 255 {
					v = 255
				}
			}
			m.Set(x, y, color.RGBA{R: uint8(v), G: uint8(v), B: uint8(v), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, nil); err != nil {
		t.Fatalf("encoding near-miss JPEG: %v", err)
	}
	return buf.Bytes()
}

// TestRestorePHashQuarantine_APerceptualNearMissMustNotDestroyTheQuarantinedBytes
// is the regression test for the blocker that inverted this PR's own thesis.
//
// The design argues a false positive is survivable BECAUSE the quarantine holds
// the original. This path was what destroyed it: quarantinedImagePresent
// returned true when ANY surviving slot merely RESEMBLED the removed picture
// (>= 0.90, about 6 differing bits of 64 -- a loose bar), and restore treated
// "present" as "no work to do" and called ConsumeRepairEntry, which UNLINKS the
// stored bytes. A PERCEPTUAL match authorized an EXACT, IRREVERSIBLE deletion.
// The original was already gone (remediation removed it), so the artwork was
// destroyed outright -- and the run reported success.
//
// The fix decouples the two decisions: a resemblance may suppress an APPEND (do
// not add a near-duplicate), but only BYTE EQUALITY may authorize a CONSUME,
// because only byte equality proves the exact bytes are recoverable from disk.
//
// Measured before the fix: Restored=0 AlreadyPresent=1 Failures=[], quarantined
// bytes GONE, manifest entries 0 -- the artwork unrecoverable through every
// supported path. Measured after: entry retained, bytes intact.
func TestRestorePHashQuarantine_APerceptualNearMissMustNotDestroyTheQuarantinedBytes(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("remediate: %v", err)
	}
	if res.SlotsRemoved != 1 {
		t.Fatalf("setup: expected the polluted slot removed, got %+v", res)
	}

	// Plant an unrelated survivor that merely RESEMBLES the removed picture.
	// Two survivors remain, so the next free ordinal is fanart3.jpg.
	bystander := nearMissJPEG(t, 1)
	if err := os.WriteFile(filepath.Join(dirA, "fanart3.jpg"), bystander, 0o644); err != nil {
		t.Fatalf("planting bystander: %v", err)
	}

	// Preconditions. Without both of these the test proves nothing: it must be
	// a PERCEPTUAL match (inside tolerance) and NOT a byte match, which is
	// exactly the band where a resemblance was authorizing a deletion.
	removed := pollutionJPEG(t, 1)
	if bytes.Equal(bystander, removed) {
		t.Fatal("precondition: the bystander must NOT be byte-identical, or this tests the exact-match path")
	}
	bh, err := image.PerceptualHash(bytes.NewReader(bystander))
	if err != nil {
		t.Fatalf("hashing bystander: %v", err)
	}
	sim := image.Similarity(pollutionHash(t, 1), bh)
	if sim < defaultPHashMismatchTolerance || sim >= 1.0 {
		t.Fatalf("precondition: the bystander must sit inside the tolerance but not be identical, got %.4f", sim)
	}
	t.Logf("bystander resembles the removed picture at similarity %.4f (tolerance %.2f)", sim, defaultPHashMismatchTolerance)

	rres, err := p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID)
	if err != nil {
		t.Fatalf("RestorePHashQuarantine: %v", err)
	}

	// THE ASSERTIONS: the quarantined bytes must SURVIVE. Asserting the
	// outcome counters alone would pass against the broken version, which
	// reported AlreadyPresent=1 while deleting the artwork.
	m, err := image.ReadRepairManifest(dirA, res.OpID)
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m == nil || len(m.Entries) != 1 {
		t.Fatalf("a resemblance must NOT consume the entry -- the removed artwork's only "+
			"copy is the quarantine, and the original is already deleted. manifest = %+v", m)
	}
	data, err := image.RepairEntryBytes(dirA, res.OpID, m.Entries[0])
	if err != nil {
		t.Fatalf("the quarantined bytes must still be readable: %v", err)
	}
	if !bytes.Equal(data, removed) {
		t.Error("the quarantined bytes must be exactly the removed picture")
	}
	// The REPORTED fact must say a human is needed. The artwork is not on
	// disk and the op will never empty on its own, so anything that reads as
	// "nothing to do" -- which AlreadyPresent means -- is a lie of exactly the
	// kind this repo keeps shipping: success reported, work not done.
	if rres.NeedsReview != 1 {
		t.Errorf("a resemblance must report NeedsReview so someone looks; got %+v", rres)
	}
	if rres.AlreadyPresent != 0 || rres.Restored != 0 || len(rres.Failures) != 0 {
		t.Errorf("a resemblance is neither success nor failure, got %+v", rres)
	}
	// The bystander is untouched either way.
	if onDisk, readErr := os.ReadFile(filepath.Join(dirA, "fanart3.jpg")); readErr != nil || !bytes.Equal(onDisk, bystander) {
		t.Errorf("the bystander must be untouched (err %v)", readErr)
	}
}

// --------------------------------------------------------------------------
// Per-artist serialization (#2564 PR-4b)
// --------------------------------------------------------------------------

// TestPhashArtistMutex_KeyedByArtist pins the keying: the same artist id always
// yields the SAME mutex (so two operations on one artist contend), and a
// different id yields a different one (so unrelated artists still run in
// parallel).
func TestPhashArtistMutex_KeyedByArtist(t *testing.T) {
	p, _ := newPHashRepairPipeline(t)
	a1 := p.phashArtistMutex("art-a")
	a2 := p.phashArtistMutex("art-a")
	b := p.phashArtistMutex("art-b")
	if a1 != a2 {
		t.Error("the same artist id must return the same mutex")
	}
	if a1 == b {
		t.Error("different artist ids must return different mutexes")
	}
}

// TestRestorePHashQuarantine_SerializesOnThePerArtistLock proves the restore's
// whole critical section runs under the per-artist lock: with the lock held, a
// restore for that artist must BLOCK and only proceed once it is released.
//
// This is the deterministic proof the -race detector cannot give: the hazard it
// guards (a restore's append racing a concurrent remediation's renumber over the
// same files and manifest) is a file-level lost update, invisible to -race. So
// the guard is verified by observing that the operation actually contends on the
// lock. Remove the mu.Lock() in RestorePHashQuarantine and this test fails: the
// restore completes while the lock is held.
func TestRestorePHashQuarantine_SerializesOnThePerArtistLock(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	seedPollutedLibrary(t, db)
	// First remediate creates a quarantine op holding the removed pollution.
	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("seed remediate: %v", err)
	}

	mu := p.phashArtistMutex("art-a")
	mu.Lock()

	done := make(chan error, 1)
	go func() {
		_, rErr := p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID)
		done <- rErr
	}()

	select {
	case <-done:
		mu.Unlock()
		t.Fatal("restore completed while the per-artist lock was held; it does not serialize")
	case <-time.After(200 * time.Millisecond):
		// Still blocked on the lock, as required.
	}
	mu.Unlock()

	select {
	case rErr := <-done:
		if rErr != nil {
			t.Fatalf("restore after unlock: %v", rErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restore did not complete after the lock was released")
	}
}

// TestRemediatePHashMismatches_SerializesOnThePerArtistLock is the back-out
// counterpart: with the artist's lock held, a remediation of that artist must
// block until it is released. Remove the lock acquisition in the
// RemediatePHashMismatches per-artist loop and this fails.
func TestRemediatePHashMismatches_SerializesOnThePerArtistLock(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	seedPollutedLibrary(t, db)

	mu := p.phashArtistMutex("art-a")
	mu.Lock()

	done := make(chan error, 1)
	go func() {
		_, rErr := p.RemediatePHashMismatches(context.Background(),
			PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
		done <- rErr
	}()

	select {
	case <-done:
		mu.Unlock()
		t.Fatal("remediate reached its per-artist work while the lock was held; it does not serialize")
	case <-time.After(200 * time.Millisecond):
		// Blocked, as required.
	}
	mu.Unlock()

	select {
	case rErr := <-done:
		if rErr != nil {
			t.Fatalf("remediate after unlock: %v", rErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remediate did not complete after the lock was released")
	}
}

// TestRemediateAndRestore_ConcurrentSameArtistIsRaceFree runs a remediate and a
// restore of the SAME artist concurrently under -race and asserts the disk and
// manifest are never left inconsistent: no staged tomb survives, every fanart
// file on disk is readable, and any manifest entry still present has readable
// bytes. The per-artist lock is what makes these invariants hold under any
// interleaving.
func TestRemediateAndRestore_ConcurrentSameArtistIsRaceFree(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dirA := seedPollutedLibrary(t, db)
	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("seed remediate: %v", err)
	}

	var (
		wg         sync.WaitGroup
		restoreRes PHashRestoreResult
		restoreErr error
		rem2Res    PHashRemediateResult
		rem2Err    error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		restoreRes, restoreErr = p.RestorePHashQuarantine(context.Background(), "art-a", res.OpID)
	}()
	go func() {
		defer wg.Done()
		rem2Res, rem2Err = p.RemediatePHashMismatches(context.Background(),
			PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	}()
	wg.Wait()

	// Capture and ASSERT each op's result, not just err==nil. Because the
	// per-artist lock serializes the two, each must land in a clean, fully
	// accounted terminal state; discarding the results would let a lock that
	// silently dropped or double-counted work still pass.
	if restoreErr != nil {
		t.Fatalf("concurrent restore: %v", restoreErr)
	}
	if rem2Err != nil {
		t.Fatalf("concurrent second remediate: %v", rem2Err)
	}
	// The restore acts on THIS op's manifest, which the second remediate never
	// touches (it mints its own op id), so the restore processes exactly the one
	// entry the seed back-out quarantined and resolves it to exactly one terminal
	// outcome (restored, already-present, or needs-review) with no failure --
	// whatever the interleaving.
	if restoreRes.OpID != res.OpID {
		t.Errorf("restore result op id = %q, want %q", restoreRes.OpID, res.OpID)
	}
	if got := restoreRes.Restored + restoreRes.AlreadyPresent + restoreRes.NeedsReview; got != 1 {
		t.Errorf("restore must account for exactly the 1 quarantined entry; got restored=%d already_present=%d needs_review=%d (sum %d)",
			restoreRes.Restored, restoreRes.AlreadyPresent, restoreRes.NeedsReview, got)
	}
	if len(restoreRes.Failures) != 0 {
		t.Errorf("restore must have no failures; got %v", restoreRes.Failures)
	}
	// The second back-out must also complete cleanly: it mints its own op id and
	// records zero per-artist failures (a race that corrupted disk or the
	// manifest would surface here as a failure).
	if rem2Res.OpID == "" {
		t.Error("second remediate must mint an op id")
	}
	if rem2Res.Failures != 0 {
		t.Errorf("second remediate must have no per-artist failures; got %d", rem2Res.Failures)
	}

	// No tomb may survive either committed operation.
	entries, err := os.ReadDir(dirA)
	if err != nil {
		t.Fatalf("reading artist dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), phashTombSuffix) {
			t.Errorf("a staged tomb survived concurrent ops: %s", e.Name())
		}
	}

	// Every discovered fanart file must be readable (no half-written slot).
	paths, err := image.DiscoverFanart(dirA, "fanart")
	if err != nil {
		t.Fatalf("DiscoverFanart: %v", err)
	}
	for _, path := range paths {
		if _, err := os.ReadFile(path); err != nil {
			t.Errorf("a fanart slot is unreadable after concurrent ops: %v", err)
		}
	}

	// Any manifest entry still present must reference bytes that exist: the
	// manifest never advertises an entry whose bytes are gone.
	m, err := image.ReadRepairManifest(dirA, res.OpID)
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m != nil {
		for _, e := range m.Entries {
			if _, err := image.RepairEntryBytes(dirA, res.OpID, e); err != nil {
				t.Errorf("manifest references bytes that are gone: %v", err)
			}
		}
	}
}

// newPHashRepairPipelineWithPublisherLogged builds the same harness as
// newPHashRepairPipeline but wires a real *publish.Publisher over the same DB --
// so the platform delete/restore WIRING (deleteRemovedSlotsOnPlatforms,
// restorePHashToPlatforms) is exercised rather than short-circuited by a nil
// publisher -- with the pipeline's (and publisher's) logger swapped for one that
// records to the returned buffer. The platform wiring surfaces its per-connection
// outcome ONLY through this log -- the returned PHashRemediateResult/
// PHashRestoreResult carry no platform field -- so a test that wants to prove the
// wiring was actually reached must read the log. Reading the buffer after the op
// returns is race-free: the platform pass runs synchronously on the calling
// goroutine, no goroutine writes it concurrently.
func newPHashRepairPipelineWithPublisherLogged(t *testing.T) (*Pipeline, *sql.DB, *artist.Service, *bytes.Buffer) {
	t.Helper()
	e, db := newDupTestEngine(t)
	svc := artist.NewService(db)
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("building encryptor: %v", err)
	}
	connSvc := connection.NewService(db, enc)
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pub := publish.New(publish.Deps{
		ArtistService:     svc,
		ConnectionService: connSvc,
		Logger:            logger,
	})
	return NewPipeline(e, svc, nil, nil, pub, logger), db, svc, &logBuf
}

// seedUndecryptableConnection inserts a connection whose api key cannot be
// decrypted, so GetByID errors when the publisher tries to load it. That makes
// the platform delete/restore record a per-connection FAILURE without any
// network I/O -- exactly the wiring branch under test, deterministically and
// offline.
func seedUndecryptableConnection(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO connections
		 (id, name, type, url, encrypted_api_key, enabled, status,
		  feature_image_write, created_at, updated_at)
		 VALUES (?, ?, 'emby', 'http://127.0.0.1:0', 'not-decryptable', 1, 'ok', 1,
		         '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z')`,
		id, id); err != nil {
		t.Fatalf("seeding connection %s: %v", id, err)
	}
}

// TestRemediatePHashMismatches_WiresPlatformDelete exercises the platform-delete
// wiring: after the on-disk removal commits, the back-out asks the publisher to
// remove the same picture from the artist's mapped platforms. The mapped
// connection cannot be loaded (undecryptable key), so the platform delete is a
// per-connection failure -- non-fatal, as designed: the local removal still
// stands and the quarantine still holds the bytes.
func TestRemediatePHashMismatches_WiresPlatformDelete(t *testing.T) {
	p, db, svc, logBuf := newPHashRepairPipelineWithPublisherLogged(t)
	dirA := seedPollutedLibrary(t, db)
	seedUndecryptableConnection(t, db, "conn-x")
	if err := svc.SetPlatformID(context.Background(), "art-a", "conn-x", "plat-a"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	res, err := p.RemediatePHashMismatches(context.Background(),
		PHashMismatchScope{ArtistID: "art-a"}, PHashRemediateOpts{})
	if err != nil {
		t.Fatalf("RemediatePHashMismatches: %v", err)
	}
	// The local removal is unaffected by the platform-side failure.
	if res.SlotsRemoved != 1 || res.Quarantined != 1 {
		t.Fatalf("local removal must stand despite platform failure: %+v", res)
	}
	// The polluted slot is gone on disk.
	if got := fanartVariantAt(t, dirA, "fanart.jpg", []int{0, 1, 2}); got != 0 {
		t.Errorf("slot 0 must still hold v0, holds v%d", got)
	}
	// The platform delete was ATTEMPTED for the mapped target and its
	// per-connection failure was SURFACED. This is what stops the test being
	// vacuous: the local assertions above pass even if the platform-delete call
	// is deleted from the product code, because the local removal never depends
	// on it. This log line is emitted ONLY by deleteRemovedSlotsOnPlatforms
	// iterating the returned failures, so removing (or no-oping) that call turns
	// this test RED.
	logs := logBuf.String()
	if !strings.Contains(logs, "phash back-out: platform delete reported a per-connection failure") {
		t.Errorf("platform delete wiring was not reached (no per-connection failure logged); logs:\n%s", logs)
	}
	if !strings.Contains(logs, "conn-x") {
		t.Errorf("platform delete failure was not surfaced for the mapped connection conn-x; logs:\n%s", logs)
	}
}

// TestRestorePHashQuarantine_WiresPlatformRestore exercises the platform-restore
// wiring: a restore re-uploads the bytes to each recorded platform target. The
// target's connection cannot be loaded, so the platform restore is a
// per-connection failure -- non-fatal: the on-disk restore (here, recognizing
// the byte-identical copy already present) still completes and the entry is
// consumed.
func TestRestorePHashQuarantine_WiresPlatformRestore(t *testing.T) {
	p, db, _, logBuf := newPHashRepairPipelineWithPublisherLogged(t)
	dirA, _ := t.TempDir(), t.TempDir()
	seedRepairArtist(t, db, "art-a", "Artist A", dirA)
	seedUndecryptableConnection(t, db, "conn-x")

	// The artist holds v1 on disk; quarantine a copy of it under an op and
	// record a platform target for it. A restore then finds the byte-identical
	// copy already present (exact) and reaches the platform-restore wiring.
	h := writePollutionFanart(t, dirA, "fanart.jpg", 1)
	entry := image.RepairEntry{
		ArtistID: "art-a", ArtistName: "Artist A", ImageType: "fanart",
		SlotIndex: 0, FileName: "fanart.jpg", PHash: h,
	}
	if err := image.QuarantineImage(dirA, "op-restore", filepath.Join(dirA, "fanart.jpg"), entry); err != nil {
		t.Fatalf("QuarantineImage: %v", err)
	}
	if err := image.SetRepairEntryPlatformTargets(dirA, "op-restore",
		image.RepairEntry{SlotIndex: 0, FileName: "fanart.jpg"},
		[]image.RepairPlatformTarget{{ConnectionID: "conn-x", PlatformArtistID: "plat-a"}}); err != nil {
		t.Fatalf("SetRepairEntryPlatformTargets: %v", err)
	}

	res, err := p.RestorePHashQuarantine(context.Background(), "art-a", "op-restore")
	if err != nil {
		t.Fatalf("RestorePHashQuarantine: %v", err)
	}
	// The byte-identical copy was already on disk, so the entry is consumed as
	// already-present; the platform failure does not change that outcome.
	if res.AlreadyPresent != 1 || len(res.Failures) != 0 {
		t.Fatalf("expected 1 already-present / 0 local failures, got %+v", res)
	}
	// The entry was consumed: the op is now empty.
	m, err := image.ReadRepairManifest(dirA, "op-restore")
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m != nil && len(m.Entries) != 0 {
		t.Errorf("entry must be consumed after an already-present restore, got %+v", m)
	}
	// RestoreBackdropToPlatforms was ATTEMPTED for the recorded target, even
	// though the local copy was already present. Without this the test is
	// vacuous: the already-present / consumed assertions above pass even if the
	// platform-restore call is deleted, because the on-disk outcome does not
	// depend on it. This log line comes ONLY from restorePHashToPlatforms
	// iterating the returned failures, so removing (or no-oping) that call turns
	// this test RED.
	logs := logBuf.String()
	if !strings.Contains(logs, "phash restore: platform restore reported a per-connection failure") {
		t.Errorf("platform restore wiring was not reached (no per-connection failure logged); logs:\n%s", logs)
	}
	if !strings.Contains(logs, "conn-x") {
		t.Errorf("platform restore was not attempted for the recorded target conn-x; logs:\n%s", logs)
	}
}
