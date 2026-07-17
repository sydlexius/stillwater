package image

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// This file covers the restore primitive and the quarantine's IO-failure
// branches -- the paths that decide what happens when the filesystem says no on
// a path that is holding the only copy of a user's artwork.
//
// Every failure here is forced by a TYPE error (a file where a directory must
// be, a name that cannot exist), never by chmod. A permission-based test
// silently PASSES as root, so on a CI runner that happens to run as root it
// would stop exercising the branch it claims to guard while still reporting
// green -- a coverage number that measures nothing. Type errors fail for root
// too.

// --------------------------------------------------------------------------
// WriteFanartBytes -- the restore primitive
// --------------------------------------------------------------------------

// TestWriteFanartBytes_WritesBytesVerbatim pins the contract that makes restore
// safe to retry.
//
// The bytes handed to this function are the exact bytes that were removed from
// the artist. It must not re-encode them and must not stamp fresh provenance
// over them -- which is why it does NOT route through Save, the acquisition
// pipeline that does both. A re-encode would shift the image's perceptual hash
// away from the one recorded in the manifest, silently breaking the
// content-addressed already-present check that keeps restore idempotent; a
// fresh EXIF stamp would erase the very history a recovery exists to reinstate.
//
// Asserting byte equality is what catches that: if this function ever grows a
// re-encode or an EXIF injection, the bytes differ and this fails. Reading the
// provenance back is not redundant decoration -- it names WHICH breakage
// occurred when the byte assertion fires, and it fails loudly if a future
// version preserves length while rewriting the metadata in place.
func TestWriteFanartBytes_WritesBytesVerbatim(t *testing.T) {
	dir := t.TempDir()
	original := makeImageBytes(t, "jpeg")

	// Give the source real provenance, as a genuine quarantined backdrop would
	// carry: it was fetched from somewhere, once, and that fact must survive.
	stamped, err := InjectMeta(original, &ExifMeta{Source: "fanart.tv", URL: "https://example.invalid/a.jpg"})
	if err != nil {
		t.Fatalf("injecting source provenance: %v", err)
	}

	target := filepath.Join(dir, "fanart2.jpg")
	if err := WriteFanartBytes(target, stamped); err != nil {
		t.Fatalf("WriteFanartBytes: %v", err)
	}

	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if !bytes.Equal(onDisk, stamped) {
		t.Errorf("restored bytes differ from the bytes handed in (%d in, %d out): a restore "+
			"must not re-encode or re-stamp -- that shifts the phash away from the manifest's "+
			"and breaks the content-addressed idempotency check", len(stamped), len(onDisk))
	}

	meta, err := ReadProvenance(target)
	if err != nil {
		t.Fatalf("reading provenance back: %v", err)
	}
	if meta == nil || meta.Source != "fanart.tv" {
		t.Errorf("the ORIGINAL provenance must survive a restore, got %+v -- a restore "+
			"reinstates history, it does not acquire an image", meta)
	}
}

// TestWriteFanartBytes_ReplacesAnExistingFileLeavingNoResidue pins the atomic
// replace and the absence of temp litter.
//
// A leftover "<target>.tmp" or ".bak" in an artist folder is not inert: it is a
// file the operator did not put there, sitting next to their artwork, and the
// tmp/bak/rename pattern exists precisely so that an interrupted write leaves
// neither a half-file nor a stray one.
func TestWriteFanartBytes_ReplacesAnExistingFileLeavingNoResidue(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "fanart.jpg")
	if err := os.WriteFile(target, []byte("stale-content"), 0o644); err != nil {
		t.Fatalf("seeding an existing file: %v", err)
	}

	restored := makeImageBytes(t, "jpeg")
	if err := WriteFanartBytes(target, restored); err != nil {
		t.Fatalf("WriteFanartBytes: %v", err)
	}

	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if !bytes.Equal(onDisk, restored) {
		t.Error("an existing file must be replaced by the restored bytes")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "fanart.jpg" {
			continue
		}
		t.Errorf("restore left residue in the artist folder: %s", e.Name())
	}
}

// TestWriteFanartBytes_ReportsAWriteItCannotPerform pins that a failed restore
// is LOUD.
//
// This is the one that matters on a data-loss path. The caller consumes the
// manifest entry -- dropping the quarantined bytes -- only after this returns
// nil. If a failed write were swallowed, the caller would treat the artwork as
// returned and discard the only remaining copy. The error must also name the
// file, because the operator's next question is "which one".
func TestWriteFanartBytes_ReportsAWriteItCannotPerform(t *testing.T) {
	dir := t.TempDir()
	// A regular file where the parent directory would have to be. MkdirAll
	// cannot descend through it, so the write fails for structural reasons
	// that apply to every user including root.
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding blocker: %v", err)
	}
	target := filepath.Join(blocker, "fanart2.jpg")

	err := WriteFanartBytes(target, makeImageBytes(t, "jpeg"))
	if err == nil {
		t.Fatal("a write that cannot be performed must return an error, never nil")
	}
	if !strings.Contains(err.Error(), "fanart2.jpg") {
		t.Errorf("the error must name the file it failed to restore, got: %v", err)
	}
}

// TestWriteFanartBytes_RefusesToWriteNoBytesOverLiveArtwork pins the guard on
// the destructive edge of the restore path.
//
// WriteFileAtomic has no opinion about length: handed no bytes it atomically
// installs a ZERO-BYTE FILE over the target and returns nil. On this path the
// target is the artist's live artwork, so a restore invoked to reinstate a
// picture would instead destroy it and report success -- this repo's dominant
// bug class, on the one code path that exists to undo a destructive change. The
// chain is reachable, not theoretical: RepairEntryBytes returns (empty, nil) for
// a zero-byte quarantined file and a restore hands that straight here.
//
// The assertion that carries the contract is that the TARGET IS UNCHANGED. An
// error-only assertion would pass against a version that truncated the file and
// then returned an error, which is the outcome this exists to prevent.
func TestWriteFanartBytes_RefusesToWriteNoBytesOverLiveArtwork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    []byte
		wantErr string
	}{
		// Same refusal, different upstream bug: nil is a caller that never
		// obtained bytes, empty is a read that succeeded against an empty file.
		{"nil", nil, "nil image data"},
		{"empty", []byte{}, "empty image data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "fanart2.jpg")
			live := makeImageBytes(t, "jpeg")
			if err := os.WriteFile(target, live, 0o644); err != nil {
				t.Fatalf("seeding live artwork: %v", err)
			}

			err := WriteFanartBytes(target, tc.data)
			if err == nil {
				t.Fatal("writing no bytes must be refused, never a nil that reports the artwork restored")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected an error mentioning %q, got: %v", tc.wantErr, err)
			}
			if !strings.Contains(err.Error(), "fanart2.jpg") {
				t.Errorf("the error must name the file, got: %v", err)
			}

			// The real contract: the live artwork survived untouched.
			got, readErr := os.ReadFile(target)
			if readErr != nil {
				t.Fatalf("the live artwork must still be readable: %v", readErr)
			}
			if !bytes.Equal(got, live) {
				t.Errorf("live artwork must be left byte-identical; len %d -> %d", len(live), len(got))
			}
		})
	}
}

// --------------------------------------------------------------------------
// QuarantineImage -- the safety hinge's failure branches
// --------------------------------------------------------------------------

// TestQuarantineImage_FailsWhenTheQuarantineDirCannotBeCreated pins the hinge.
//
// The caller's contract is: quarantine the bytes, THEN stage and remove the
// original. If this returned nil without having stored anything, the caller
// would proceed to delete artwork whose only copy was never written. A
// quarantine that cannot be created must stop the operation, not start it.
func TestQuarantineImage_FailsWhenTheQuarantineDirCannotBeCreated(t *testing.T) {
	dir := t.TempDir()
	src := quarantineFixture(t, dir, "fanart.jpg", "artwork-bytes")

	// A regular file occupying the .sw-repair path: MkdirAll cannot descend.
	if err := os.WriteFile(filepath.Join(dir, RepairDirName), []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding blocker: %v", err)
	}

	err := QuarantineImage(dir, "op-blocked", src, RepairEntry{SlotIndex: 0, FileName: "fanart.jpg"})
	if err == nil {
		t.Fatal("quarantine must fail when its directory cannot be created")
	}
	if !strings.Contains(err.Error(), "creating quarantine dir") {
		t.Errorf("expected a quarantine-dir error, got: %v", err)
	}
	// The source is untouched -- quarantine never moves, only copies.
	if _, statErr := os.Stat(src); statErr != nil {
		t.Errorf("the source must be untouched by a failed quarantine: %v", statErr)
	}
}

// TestQuarantineImage_ReportsAByteWriteItCannotPerform pins the fail-loud half
// of the safety hinge: when the bytes cannot be stored, the caller must learn
// about it, because on nil it goes on to DELETE the original.
//
// The trigger is an unwritable name; the contract under test is the error and
// the absence of a manifest entry, not the name. An earlier version of this test
// claimed to pin a subtler thing -- that StoredName's "%03d-" prefix pushes an
// otherwise-legal filename over the component limit -- and did not: at 304 bytes
// the name is unwritable with or without the prefix, so deleting the prefix left
// the test green. That claim is retracted rather than propped up. Measured, the
// prefix is load-bearing only for basenames in (232, 236], because
// WriteFileAtomic's own CreateTemp suffix already caps the full name near 240
// rather than NAME_MAX's 255 -- and that suffix is 15 or 16 bytes depending on
// the random's digit count, so a fixture sized into that band would encode a
// stdlib implementation detail and wobble with it.
//
// The prefix does not need this test anyway: it is already pinned robustly by
// TestQuarantineImage_SameBasenameAcrossSlotsDoesNotClobber, which goes RED the
// moment the prefix is removed (measured).
func TestQuarantineImage_ReportsAByteWriteItCannotPerform(t *testing.T) {
	dir := t.TempDir()
	unwritableName := strings.Repeat("a", 300) + ".jpg"
	src := quarantineFixture(t, dir, "short.jpg", "artwork-bytes")

	err := QuarantineImage(dir, "op-long", src, RepairEntry{SlotIndex: 1, FileName: unwritableName})
	if err == nil {
		t.Fatal("quarantine must fail when the stored bytes cannot be written")
	}
	if !strings.Contains(err.Error(), "writing quarantined bytes") {
		t.Errorf("expected a bytes-write error, got: %v", err)
	}

	// Nothing may be advertised in the manifest for bytes that were never
	// stored: a later restore would fail on an entry nobody can serve.
	m, mErr := ReadRepairManifest(dir, "op-long")
	if mErr != nil {
		t.Fatalf("ReadRepairManifest: %v", mErr)
	}
	if m != nil && len(m.Entries) != 0 {
		t.Errorf("a failed byte write must record no manifest entry, got %+v", m.Entries)
	}
}

// TestQuarantineImage_ConcurrentSlotsOfOneOpAllSurvive pins the serialization
// that keeps the manifest honest.
//
// Unguarded, this is silent artwork loss: two goroutines quarantining different
// slots of one operation both write their bytes, both read the manifest, both
// append to their own copy, and the last write wins. The loser's entry is gone
// while its bytes sit on disk referenced by nothing -- unreachable through
// ListRepairOps, ReadRepairManifest and RepairEntryBytes alike. Both calls
// return nil, so the caller deletes both originals and one artwork is
// unrecoverable through every supported path.
//
// PR-3b consumes this primitive and may fan an artist's slots across goroutines,
// so the contract has to hold rather than be a convention nobody was told about.
//
// Run under -race, which catches the concurrent map access; the assertions catch
// the lost-update itself, which is NOT a data race the detector would flag (the
// writes are to separate files and separate structs -- it is a lost read-modify-
// write across processes' worth of state, invisible to -race). That is why this
// asserts entry survival rather than trusting a clean race report.
func TestQuarantineImage_ConcurrentSlotsOfOneOpAllSurvive(t *testing.T) {
	const slots = 8
	dir := t.TempDir()

	srcs := make([]string, slots)
	for i := range slots {
		srcs[i] = quarantineFixture(t, dir, fmt.Sprintf("fanart%d.jpg", i), fmt.Sprintf("bytes-%d", i))
	}

	var wg sync.WaitGroup
	errs := make([]error, slots)
	start := make(chan struct{})
	for i := range slots {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // maximize overlap on the manifest read-modify-write
			errs[i] = QuarantineImage(dir, "op-concurrent", srcs[i], RepairEntry{
				SlotIndex: i, FileName: fmt.Sprintf("fanart%d.jpg", i),
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("slot %d: QuarantineImage: %v", i, err)
		}
	}

	// EVERY entry survived. Each call returned nil, so the caller is entitled
	// to delete all eight originals; the manifest must account for all eight.
	m, err := ReadRepairManifest(dir, "op-concurrent")
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m == nil || len(m.Entries) != slots {
		got := 0
		if m != nil {
			got = len(m.Entries)
		}
		t.Fatalf("manifest holds %d of %d entries -- every call returned nil, so the "+
			"missing ones name artwork the caller has already deleted and nothing "+
			"references", got, slots)
	}

	// And every entry actually resolves to its own bytes.
	seen := make(map[string]bool, slots)
	for i := range m.Entries {
		data, err := RepairEntryBytes(dir, "op-concurrent", m.Entries[i])
		if err != nil {
			t.Errorf("entry %+v: RepairEntryBytes: %v", m.Entries[i], err)
			continue
		}
		seen[string(data)] = true
	}
	for i := range slots {
		if !seen[fmt.Sprintf("bytes-%d", i)] {
			t.Errorf("slot %d's bytes are not retrievable through the manifest", i)
		}
	}
}

// TestConsumeRepairEntry_ConcurrentWithQuarantineDoesNotDropTheAppend pins the
// other half: consume is a read-modify-write on the same shared manifest and
// races quarantine identically. Unguarded, a consume that read the manifest
// before a concurrent quarantine's append will rewrite it without that entry,
// orphaning the bytes quarantine just stored and reported success for.
func TestConsumeRepairEntry_ConcurrentWithQuarantineDoesNotDropTheAppend(t *testing.T) {
	dir := t.TempDir()
	victim := quarantineFixture(t, dir, "fanart.jpg", "victim-bytes")
	keeper := quarantineFixture(t, dir, "fanart2.jpg", "keeper-bytes")

	// Seed the entry that will be consumed.
	if err := QuarantineImage(dir, "op-race", victim, RepairEntry{SlotIndex: 0, FileName: "fanart.jpg"}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	var consumeErr, quarantineErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		consumeErr = ConsumeRepairEntry(dir, "op-race", RepairEntry{SlotIndex: 0, FileName: "fanart.jpg"})
	}()
	go func() {
		defer wg.Done()
		<-start
		quarantineErr = QuarantineImage(dir, "op-race", keeper, RepairEntry{SlotIndex: 1, FileName: "fanart2.jpg"})
	}()
	close(start)
	wg.Wait()

	if consumeErr != nil {
		t.Fatalf("consume: %v", consumeErr)
	}
	if quarantineErr != nil {
		t.Fatalf("quarantine: %v", quarantineErr)
	}

	// Whichever order they landed in, the keeper's entry must exist: its
	// QuarantineImage returned nil, so its original is gone.
	m, err := ReadRepairManifest(dir, "op-race")
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m == nil || len(m.Entries) != 1 {
		t.Fatalf("expected exactly the keeper's entry to remain, got %+v", m)
	}
	data, err := RepairEntryBytes(dir, "op-race", m.Entries[0])
	if err != nil {
		t.Fatalf("RepairEntryBytes: %v", err)
	}
	if string(data) != "keeper-bytes" {
		t.Errorf("the surviving entry resolves to %q, want keeper-bytes", data)
	}
}

// TestQuarantineImage_RefusesWhenTheExistingManifestIsUnreadable pins that a
// broken manifest stops the operation instead of being papered over.
//
// The tempting behavior is to shrug and start a fresh manifest. That would
// ORPHAN every entry already recorded: their bytes stay on disk but nothing
// references them, so the artwork they hold becomes unrecoverable through any
// supported path -- while the operation reports success and goes on to delete
// more.
//
// Refusing does NOT leave nothing behind: this call's own bytes were already
// written before the manifest was read, so the refusal leaves them orphaned too.
// That is the point. The bytes are inert litter, whereas the caller -- seeing an
// error -- keeps the original and deletes nothing, and a retry rewrites the same
// bytes and appends the entry. Zero artwork lost, one stray file; the alternative
// trades the stray file for unrecoverable artwork.
func TestQuarantineImage_RefusesWhenTheExistingManifestIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	src := quarantineFixture(t, dir, "fanart.jpg", "artwork-bytes")
	opDir := filepath.Join(dir, RepairDirName, "op-corrupt")
	if err := os.MkdirAll(opDir, 0o750); err != nil {
		t.Fatalf("creating op dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(opDir, repairManifestName), []byte("{truncated"), 0o644); err != nil {
		t.Fatalf("writing corrupt manifest: %v", err)
	}

	err := QuarantineImage(dir, "op-corrupt", src, RepairEntry{SlotIndex: 0, FileName: "fanart.jpg"})
	if err == nil {
		t.Fatal("quarantine must refuse to append to an unreadable manifest")
	}
	if !strings.Contains(err.Error(), "decoding repair manifest") {
		t.Errorf("expected a manifest decode error, got: %v", err)
	}
}

// --------------------------------------------------------------------------
// Read paths -- fail closed on a tainted or unreadable operation
// --------------------------------------------------------------------------

// TestRepairReadPaths_RejectAnInvalidOpID pins that every entry point joining an
// op id into a path validates it, not just the write path. A read that accepted
// "../.." would hand a caller bytes from outside the quarantine, and
// ConsumeRepairEntry would DELETE outside it.
func TestRepairReadPaths_RejectAnInvalidOpID(t *testing.T) {
	dir := t.TempDir()
	entry := RepairEntry{SlotIndex: 0, FileName: "fanart.jpg", StoredName: "000-fanart.jpg"}

	if _, err := ReadRepairManifest(dir, "../escape"); err == nil {
		t.Error("ReadRepairManifest must reject a traversing op id")
	}
	if _, err := RepairEntryBytes(dir, "../escape", entry); err == nil {
		t.Error("RepairEntryBytes must reject a traversing op id")
	}
	if err := ConsumeRepairEntry(dir, "../escape", entry); err == nil {
		t.Error("ConsumeRepairEntry must reject a traversing op id")
	}
}

// TestReadRepairManifest_ErrorsWhenTheManifestCannotBeRead separates "no such
// operation" (nil, nil) from "this operation is broken" (error). A read failure
// that is not IsNotExist means the entry exists but is unusable, and reporting
// that as an absent operation would hide recoverable artwork behind a green
// light.
func TestReadRepairManifest_ErrorsWhenTheManifestCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	// A directory where the manifest file should be: os.ReadFile returns
	// EISDIR, which is emphatically not IsNotExist.
	if err := os.MkdirAll(filepath.Join(dir, RepairDirName, "op-dir", repairManifestName), 0o750); err != nil {
		t.Fatalf("creating dir-as-manifest: %v", err)
	}

	m, err := ReadRepairManifest(dir, "op-dir")
	if err == nil {
		t.Fatalf("an unreadable manifest must error, got manifest %+v", m)
	}
	if !strings.Contains(err.Error(), "reading repair manifest") {
		t.Errorf("expected a manifest read error, got: %v", err)
	}
}

// TestListRepairOps_ErrorsWhenTheRepairRootIsNotADirectory pins the same
// distinction for the listing: an absent root legitimately means "no
// quarantines" (nil, nil), but a root that exists and cannot be listed means
// the quarantine is compromised and must not read as empty.
func TestListRepairOps_ErrorsWhenTheRepairRootIsNotADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RepairDirName), []byte("x"), 0o644); err != nil {
		t.Fatalf("seeding file-as-root: %v", err)
	}

	ops, err := ListRepairOps(dir)
	if err == nil {
		t.Fatalf("an unlistable repair root must error, got ops %v", ops)
	}
	if !strings.Contains(err.Error(), "reading repair dir") {
		t.Errorf("expected a repair-dir read error, got: %v", err)
	}
}

// TestListRepairOps_AbsentRootIsNotAnError is the other side of that line: a
// library that has never been repaired reports no operations, quietly.
func TestListRepairOps_AbsentRootIsNotAnError(t *testing.T) {
	ops, err := ListRepairOps(t.TempDir())
	if err != nil {
		t.Fatalf("an absent repair root must not error: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("an absent repair root must report no ops, got %v", ops)
	}
}

// --------------------------------------------------------------------------
// StoredName back-compatibility
// --------------------------------------------------------------------------

// seedBareManifestOp writes a quarantine operation whose MANIFEST ENTRY carries
// no StoredName, with its bytes present under the derived name.
//
// It builds the manifest by hand on purpose. Going through QuarantineImage
// cannot produce this shape -- that function always populates StoredName -- so a
// test that seeds via QuarantineImage and then passes a bare entry exercises
// only the mirror case (manifest populated, caller bare), which never had a bug.
// The shape that breaks is the STORED side being empty: a manifest hand-repaired
// by an operator, or one written before the field existed.
func seedBareManifestOp(t *testing.T, dir, opID string, slot int, fileName, content string) {
	t.Helper()
	opDir := filepath.Join(dir, RepairDirName, opID)
	if err := os.MkdirAll(opDir, 0o750); err != nil {
		t.Fatalf("creating op dir: %v", err)
	}
	// Bytes under the name QuarantineImage would have chosen.
	stored := repairStoredName(slot, fileName)
	if err := os.WriteFile(filepath.Join(opDir, stored), []byte(content), 0o644); err != nil {
		t.Fatalf("writing quarantined bytes: %v", err)
	}
	// Manifest entry with NO stored_name field at all.
	manifest := fmt.Sprintf(
		`{"op_id":%q,"created_at":"2026-07-16T00:00:00Z","entries":[{"artist_id":"art-a","image_type":"fanart","slot_index":%d,"file_name":%q,"similarity":0.97,"quarantined_at":"2026-07-16T00:00:00Z"}]}`,
		opID, slot, fileName)
	if err := os.WriteFile(filepath.Join(opDir, repairManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("writing bare manifest: %v", err)
	}

	// Precondition: the stored side really is empty. If a future change makes
	// the decoder populate it, this test silently stops covering the bug.
	m, err := ReadRepairManifest(dir, opID)
	if err != nil || m == nil || len(m.Entries) != 1 {
		t.Fatalf("seeding: expected 1 entry, got %+v (err %v)", m, err)
	}
	if m.Entries[0].StoredName != "" {
		t.Fatalf("seeding: the manifest's StoredName must be EMPTY for this test to "+
			"pin anything, got %q", m.Entries[0].StoredName)
	}
}

// TestConsumeRepairEntry_RemovesAnEntryWhoseStoredManifestNameIsEmpty pins the
// bug that ConsumeRepairEntry derived only the CALLER's stored name and compared
// it against the manifest's RAW field.
//
// An entry with an empty StoredName therefore matched nothing: the filter kept
// every entry, the length check saw no change, and the function RETURNED NIL.
// Success reported, nothing removed, bytes never unlinked, entry never dropped
// -- this repo's dominant bug class exactly, on the path that bookkeeps the
// operator's only remaining copy of their artwork. Reachable today via a
// hand-repaired manifest or one predating the field.
//
// Revert-and-rerun proof (reported in the PR): restoring the raw-field
// comparison (`m.Entries[i].StoredName != stored`) makes this RED on the
// "manifest still holds" assertion; deriving both sides makes it GREEN.
func TestConsumeRepairEntry_RemovesAnEntryWhoseStoredManifestNameIsEmpty(t *testing.T) {
	dir := t.TempDir()
	seedBareManifestOp(t, dir, "op-bare", 1, "fanart2.jpg", "artwork-bytes")

	bare := RepairEntry{SlotIndex: 1, FileName: "fanart2.jpg"}
	if err := ConsumeRepairEntry(dir, "op-bare", bare); err != nil {
		t.Fatalf("ConsumeRepairEntry: %v", err)
	}

	// The entry is really gone -- not "returned nil while doing nothing".
	m, err := ReadRepairManifest(dir, "op-bare")
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m != nil && len(m.Entries) != 0 {
		t.Fatalf("consume returned nil but the manifest STILL HOLDS %+v -- "+
			"reported success while doing nothing", m.Entries)
	}
	// And the bytes were actually unlinked with it.
	if _, err := os.Stat(filepath.Join(dir, RepairDirName, "op-bare")); !os.IsNotExist(err) {
		t.Errorf("the emptied op dir must be removed; stat err = %v", err)
	}
}

// TestRepairEntryBytes_DerivesTheStoredNameWhenAbsent covers the read side of
// the same fallback: an entry carrying only identity fields still resolves to
// its bytes rather than to the op directory itself.
func TestRepairEntryBytes_DerivesTheStoredNameWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	seedBareManifestOp(t, dir, "op-read", 1, "fanart2.jpg", "artwork-bytes")

	data, err := RepairEntryBytes(dir, "op-read", RepairEntry{SlotIndex: 1, FileName: "fanart2.jpg"})
	if err != nil {
		t.Fatalf("RepairEntryBytes must derive the stored name: %v", err)
	}
	if string(data) != "artwork-bytes" {
		t.Errorf("derived lookup returned %q, want the quarantined bytes", data)
	}
}

// TestConsumeRepairEntry_PropagatesAnUnreadableManifest pins that consuming does
// not quietly succeed against a broken operation. ConsumeRepairEntry drops
// bytes; doing that on a manifest it could not parse would be deleting on the
// strength of a file it never read.
func TestConsumeRepairEntry_PropagatesAnUnreadableManifest(t *testing.T) {
	dir := t.TempDir()
	opDir := filepath.Join(dir, RepairDirName, "op-broken")
	if err := os.MkdirAll(opDir, 0o750); err != nil {
		t.Fatalf("creating op dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(opDir, repairManifestName), []byte("}{"), 0o644); err != nil {
		t.Fatalf("writing corrupt manifest: %v", err)
	}

	err := ConsumeRepairEntry(dir, "op-broken", RepairEntry{SlotIndex: 0, FileName: "fanart.jpg"})
	if err == nil {
		t.Fatal("consuming against an unreadable manifest must error")
	}
	if !strings.Contains(err.Error(), "decoding repair manifest") {
		t.Errorf("expected a manifest decode error, got: %v", err)
	}
}
