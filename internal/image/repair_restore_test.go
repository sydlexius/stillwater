package image

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

// TestQuarantineImage_FailsWhenTheStoredNameCannotBeWritten covers a real
// overflow, not a synthetic one.
//
// StoredName prefixes the original basename with "%03d-" to namespace it by
// slot. A filename that is legal on disk can therefore become illegal once
// stored: at the usual 255-byte component limit, a 252-byte basename that the
// scanner happily discovered overflows when the 4-byte prefix is added. The
// quarantine write then fails -- and it must SAY so rather than return nil and
// let the caller delete the original.
func TestQuarantineImage_FailsWhenTheStoredNameCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	longName := strings.Repeat("a", 300) + ".jpg"
	src := quarantineFixture(t, dir, "short.jpg", "artwork-bytes")

	err := QuarantineImage(dir, "op-long", src, RepairEntry{SlotIndex: 1, FileName: longName})
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

// TestQuarantineImage_RefusesWhenTheExistingManifestIsUnreadable pins that a
// broken manifest stops the operation instead of being papered over.
//
// The tempting behavior is to shrug and start a fresh manifest. That would
// ORPHAN every entry already recorded: their bytes stay on disk but nothing
// references them, so the artwork they hold becomes unrecoverable through any
// supported path -- while the operation reports success and goes on to delete
// more. Refusing keeps the damage at zero and leaves the evidence in place.
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

// TestRepairEntry_StoredNameIsDerivedWhenAbsent pins the fallback that keeps a
// manifest entry usable when it carries no StoredName.
//
// This is not hypothetical: an entry hand-written by an operator recovering a
// damaged manifest, or one produced before StoredName existed, has the field
// empty. Deriving it from (SlotIndex, FileName) -- the same rule that produced
// it in the first place -- means those entries still restore rather than
// silently resolving to the op directory itself.
func TestRepairEntry_StoredNameIsDerivedWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	src := quarantineFixture(t, dir, "fanart2.jpg", "artwork-bytes")
	if err := QuarantineImage(dir, "op-derive", src, RepairEntry{SlotIndex: 1, FileName: "fanart2.jpg"}); err != nil {
		t.Fatalf("QuarantineImage: %v", err)
	}

	// An entry as a caller might reconstruct it: identity fields only.
	bare := RepairEntry{SlotIndex: 1, FileName: "fanart2.jpg"}

	data, err := RepairEntryBytes(dir, "op-derive", bare)
	if err != nil {
		t.Fatalf("RepairEntryBytes must derive the stored name: %v", err)
	}
	if string(data) != "artwork-bytes" {
		t.Errorf("derived lookup returned %q, want the quarantined bytes", data)
	}

	if err := ConsumeRepairEntry(dir, "op-derive", bare); err != nil {
		t.Fatalf("ConsumeRepairEntry must derive the stored name: %v", err)
	}
	m, err := ReadRepairManifest(dir, "op-derive")
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m != nil && len(m.Entries) != 0 {
		t.Errorf("the derived entry must have been consumed, manifest holds %+v", m.Entries)
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
