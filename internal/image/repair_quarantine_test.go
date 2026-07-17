package image

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// quarantineFixture writes a source file and returns its path. The quarantine
// primitive is byte-level and format-agnostic -- it copies whatever it is given
// -- so these tests deliberately use arbitrary bytes rather than JPEGs. Using a
// real image here would test the image decoder, not the copy/manifest contract,
// and would hide a regression that mangled non-JPEG artwork.
func quarantineFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture %s: %v", name, err)
	}
	return path
}

// TestQuarantineImage_CopiesBytesLeavingSourceInPlace pins the ordering the
// whole feature's crash-safety rests on: the bytes are COPIED somewhere durable
// and the original is left alone, so a crash between quarantine and removal
// leaves the artwork readable from both places rather than neither.
func TestQuarantineImage_CopiesBytesLeavingSourceInPlace(t *testing.T) {
	dir := t.TempDir()
	src := quarantineFixture(t, dir, "fanart2.jpg", "polluted-bytes")

	entry := RepairEntry{
		ArtistID: "art-1", ArtistName: "Artist One", ImageType: "fanart",
		SlotIndex: 1, FileName: "fanart2.jpg", PHash: "abc123",
		MatchedArtistID: "art-2", MatchedArtistName: "Artist Two", Similarity: 0.97,
	}
	if err := QuarantineImage(dir, "op-one", src, entry); err != nil {
		t.Fatalf("QuarantineImage: %v", err)
	}

	// The source must still be there. QuarantineImage is a copy, never a move.
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source must be left in place by quarantine: %v", err)
	}

	m, err := ReadRepairManifest(dir, "op-one")
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if m == nil || len(m.Entries) != 1 {
		t.Fatalf("expected 1 manifest entry, got %+v", m)
	}
	got := m.Entries[0]
	if got.PHash != "abc123" || got.MatchedArtistID != "art-2" || got.Similarity != 0.97 {
		t.Errorf("manifest lost the removal evidence: %+v", got)
	}
	if got.SlotIndex != 1 {
		t.Errorf("manifest must record the original slot as provenance, got %d", got.SlotIndex)
	}
	if got.QuarantinedAt.IsZero() {
		t.Error("manifest entry must carry a quarantine timestamp")
	}

	data, err := RepairEntryBytes(dir, "op-one", got)
	if err != nil {
		t.Fatalf("RepairEntryBytes: %v", err)
	}
	if string(data) != "polluted-bytes" {
		t.Errorf("quarantined bytes = %q, want the exact removed bytes", data)
	}
}

// TestQuarantineImage_RejectsTraversingOpID proves the op-id sanitizer fails
// closed. The op id is joined into a filesystem path, so an id carrying a
// separator or ".." would let a caller steer writes out of the .sw-repair
// subtree and into the artist's live artwork -- or anywhere else.
func TestQuarantineImage_RejectsTraversingOpID(t *testing.T) {
	dir := t.TempDir()
	src := quarantineFixture(t, dir, "fanart.jpg", "bytes")

	for _, opID := range []string{
		"../escape", "..", "a/b", "a\\b", ".hidden", "UPPER", "op_underscore",
		"", "-leading", "trailing-", "double--hyphen",
		strings.Repeat("a", maxRepairOpIDLen+1),
	} {
		t.Run(opID, func(t *testing.T) {
			err := QuarantineImage(dir, opID, src, RepairEntry{FileName: "fanart.jpg"})
			if err == nil {
				t.Fatalf("op id %q must be rejected, got nil error", opID)
			}
			if !strings.Contains(err.Error(), "invalid repair op id") {
				t.Errorf("expected an op-id rejection, got: %v", err)
			}
		})
	}

	// Nothing may have been created outside the temp dir's repair root.
	if _, err := os.Stat(filepath.Join(dir, RepairDirName)); !os.IsNotExist(err) {
		t.Errorf("a rejected op id must not create a repair dir; stat err = %v", err)
	}
}

// treeSnapshot returns every path under root, so a test can assert that a
// rejected call wrote NOTHING ANYWHERE rather than merely that it returned an
// error. An error-only assertion passes even when the bytes already landed --
// including outside the directory they were meant for.
func treeSnapshot(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	if err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		paths = append(paths, rel)
		return nil
	}); err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(paths)
	return paths
}

// TestQuarantineImage_RejectsAnEntryThatCannotDescribeARecoverableImage pins the
// entry validation.
//
// The manifest is the durable record a restore reads back, and these two fields
// are load-bearing: a FileName with no usable basename names no format (and,
// persisted raw, carries directory components a consumer joining it onto a path
// would resolve outside the artist dir), and a negative slot is provenance that
// lies. Neither is checked anywhere else.
//
// SCOPE: a name that merely CARRIES directory components ("../../evil.jpg") is
// NOT rejected -- it reduces to a usable basename and is normalized to it. See
// TestQuarantineImage_NormalizesFileNameToABasename. Rejected here are only the
// names that reduce to no file at all.
func TestQuarantineImage_RejectsAnEntryThatCannotDescribeARecoverableImage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entry   RepairEntry
		wantErr string
	}{
		{"dot file name", RepairEntry{SlotIndex: 0, FileName: "."}, "no usable basename"},
		{"parent file name", RepairEntry{SlotIndex: 0, FileName: ".."}, "no usable basename"},
		{"separator file name", RepairEntry{SlotIndex: 0, FileName: "/"}, "no usable basename"},
		{"empty file name", RepairEntry{SlotIndex: 0, FileName: ""}, "empty file name"},
		{"negative slot", RepairEntry{SlotIndex: -1, FileName: "fanart.jpg"}, "negative slot index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The artist dir sits under a root holding a bystander, so the
			// snapshot would catch a write that escaped upward as well as one
			// that landed where it should not.
			root := t.TempDir()
			dir := filepath.Join(root, "artist")
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatalf("creating artist dir: %v", err)
			}
			src := quarantineFixture(t, dir, "fanart.jpg", "artwork-bytes")
			quarantineFixture(t, root, "evil.jpg", "bystander-original")

			before := treeSnapshot(t, root)

			err := QuarantineImage(dir, "op-invalid", src, tc.entry)
			if err == nil {
				t.Fatalf("entry %+v must be rejected, got nil error -- the caller would now delete its original", tc.entry)
			}
			if !strings.Contains(err.Error(), "invalid repair entry") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected an entry rejection mentioning %q, got: %v", tc.wantErr, err)
			}

			// The assertion that matters: nothing was written ANYWHERE. An
			// error return alone would pass even if the bytes had escaped.
			if after := treeSnapshot(t, root); !slices.Equal(before, after) {
				t.Errorf("a rejected entry must write nothing;\n before = %v\n after  = %v", before, after)
			}
			if b, readErr := os.ReadFile(filepath.Join(root, "evil.jpg")); readErr != nil || string(b) != "bystander-original" {
				t.Errorf("a rejected entry must not touch a bystander file, got %q (err %v)", b, readErr)
			}
		})
	}
}

// TestQuarantineImage_NormalizesFileNameToABasename pins that the only FileName
// which can reach the durable manifest is one that names a file and nothing
// else.
//
// The manifest outlives the process and is read back to decide a restore. A
// consumer joining a raw FileName onto a directory -- the obvious reading of a
// field named for a file -- would resolve "../../evil.jpg" outside the artist
// dir. Normalizing at this boundary means such a FileName cannot be stored in
// the first place, rather than being a hazard every future consumer has to
// remember to defuse.
//
// The containment assertion is a REGRESSION guard on repairStoredName's
// filepath.Base, which is what actually keeps the WRITE inside the op dir --
// measured: a traversing FileName does not escape today, with or without the
// normalization above. It is pinned here so a refactor that drops that Base
// turns this red instead of silently writing artwork into the live artist dir.
func TestQuarantineImage_NormalizesFileNameToABasename(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fileName string
	}{
		{"directory components", "sub/dir/fanart3.jpg"},
		{"traversing components", "../../fanart3.jpg"},
		{"absolute path", "/etc/fanart3.jpg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "artist")
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatalf("creating artist dir: %v", err)
			}
			src := quarantineFixture(t, dir, "fanart.jpg", "artwork-bytes")

			if err := QuarantineImage(dir, "op-norm", src, RepairEntry{SlotIndex: 2, FileName: tc.fileName}); err != nil {
				t.Fatalf("a file name reducing to a usable basename must normalize, not fail: %v", err)
			}

			m, err := ReadRepairManifest(dir, "op-norm")
			if err != nil || m == nil || len(m.Entries) != 1 {
				t.Fatalf("expected 1 entry, got %+v (err %v)", m, err)
			}
			if got := m.Entries[0].FileName; got != "fanart3.jpg" {
				t.Errorf("persisted FileName = %q, want the bare basename %q", got, "fanart3.jpg")
			}

			// The bytes landed inside the op dir and nowhere else.
			opDir := filepath.Join(dir, RepairDirName, "op-norm")
			want := []string{"artist", "artist/.sw-repair", "artist/.sw-repair/op-norm",
				"artist/.sw-repair/op-norm/002-fanart3.jpg", "artist/.sw-repair/op-norm/manifest.json",
				"artist/fanart.jpg", "."}
			sort.Strings(want)
			if got := treeSnapshot(t, root); !slices.Equal(got, want) {
				t.Errorf("quarantine must write only inside %s;\n got  = %v\n want = %v", opDir, got, want)
			}

			// Normalization that broke the round trip would make the artwork
			// unrecoverable -- the failure this primitive exists to prevent.
			data, err := RepairEntryBytes(dir, "op-norm", m.Entries[0])
			if err != nil || string(data) != "artwork-bytes" {
				t.Errorf("normalized entry must still resolve to its bytes, got %q (err %v)", data, err)
			}
		})
	}
}

// TestQuarantineImage_SameBasenameAcrossSlotsDoesNotClobber pins the stored-name
// namespacing. Two slots can share an original basename (a renumber in flight, a
// platform alias), and a flat name-keyed store would silently overwrite the
// first slot's bytes with the second's -- destroying one artwork while
// reporting two quarantined.
func TestQuarantineImage_SameBasenameAcrossSlotsDoesNotClobber(t *testing.T) {
	dir := t.TempDir()
	srcA := quarantineFixture(t, dir, "a.jpg", "slot-one-bytes")
	srcB := quarantineFixture(t, dir, "b.jpg", "slot-two-bytes")

	if err := QuarantineImage(dir, "op-two", srcA, RepairEntry{SlotIndex: 1, FileName: "fanart.jpg"}); err != nil {
		t.Fatalf("quarantining slot 1: %v", err)
	}
	if err := QuarantineImage(dir, "op-two", srcB, RepairEntry{SlotIndex: 2, FileName: "fanart.jpg"}); err != nil {
		t.Fatalf("quarantining slot 2: %v", err)
	}

	m, err := ReadRepairManifest(dir, "op-two")
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m.Entries))
	}
	if m.Entries[0].StoredName == m.Entries[1].StoredName {
		t.Fatalf("entries sharing a basename must get distinct stored names, both = %q", m.Entries[0].StoredName)
	}

	first, err := RepairEntryBytes(dir, "op-two", m.Entries[0])
	if err != nil {
		t.Fatalf("reading entry 0: %v", err)
	}
	second, err := RepairEntryBytes(dir, "op-two", m.Entries[1])
	if err != nil {
		t.Fatalf("reading entry 1: %v", err)
	}
	if string(first) != "slot-one-bytes" || string(second) != "slot-two-bytes" {
		t.Errorf("bytes clobbered: entry0=%q entry1=%q", first, second)
	}
}

// TestConsumeRepairEntry_DropsEntryAndCleansUpWhenEmptied covers the restore
// side's bookkeeping, including the idempotent no-op that lets a retried restore
// reach a clean end state instead of failing on the second attempt.
func TestConsumeRepairEntry_DropsEntryAndCleansUpWhenEmptied(t *testing.T) {
	dir := t.TempDir()
	srcA := quarantineFixture(t, dir, "a.jpg", "aaa")
	srcB := quarantineFixture(t, dir, "b.jpg", "bbb")
	if err := QuarantineImage(dir, "op-three", srcA, RepairEntry{SlotIndex: 1, FileName: "a.jpg"}); err != nil {
		t.Fatalf("quarantining a: %v", err)
	}
	if err := QuarantineImage(dir, "op-three", srcB, RepairEntry{SlotIndex: 2, FileName: "b.jpg"}); err != nil {
		t.Fatalf("quarantining b: %v", err)
	}

	m, err := ReadRepairManifest(dir, "op-three")
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	first, second := m.Entries[0], m.Entries[1]

	if err := ConsumeRepairEntry(dir, "op-three", first); err != nil {
		t.Fatalf("consuming first: %v", err)
	}
	m, err = ReadRepairManifest(dir, "op-three")
	if err != nil {
		t.Fatalf("re-reading manifest: %v", err)
	}
	if len(m.Entries) != 1 || m.Entries[0].StoredName != second.StoredName {
		t.Fatalf("expected only the second entry to remain, got %+v", m.Entries)
	}
	// The consumed entry's bytes are gone; the survivor's are not.
	if _, err := RepairEntryBytes(dir, "op-three", first); err == nil {
		t.Error("consumed entry's bytes must be removed")
	}
	if _, err := RepairEntryBytes(dir, "op-three", second); err != nil {
		t.Errorf("surviving entry's bytes must remain: %v", err)
	}

	// Consuming again is a no-op, not an error: restore is idempotent.
	if err := ConsumeRepairEntry(dir, "op-three", first); err != nil {
		t.Errorf("re-consuming a gone entry must be a no-op, got: %v", err)
	}

	if err := ConsumeRepairEntry(dir, "op-three", second); err != nil {
		t.Fatalf("consuming second: %v", err)
	}
	// Last entry gone -> the op dir and the repair root are cleaned up, so a
	// fully-restored library carries no empty scaffolding.
	if _, err := os.Stat(filepath.Join(dir, RepairDirName, "op-three")); !os.IsNotExist(err) {
		t.Errorf("emptied op dir must be removed; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, RepairDirName)); !os.IsNotExist(err) {
		t.Errorf("emptied repair root must be removed; stat err = %v", err)
	}
}

// TestQuarantineImage_VanishedSourceErrorsWithoutRecordingAnEntry pins the
// crash-consistency property the manifest rests on: it must never advertise an
// entry whose bytes are not there.
//
// The source can genuinely disappear between detection and quarantine (another
// repair path, a scan, a user). If that produced a manifest entry anyway, every
// later read of the operation would fail on an entry nobody can serve, and a
// restore would report a failure for artwork that was never taken.
func TestQuarantineImage_VanishedSourceErrorsWithoutRecordingAnEntry(t *testing.T) {
	dir := t.TempDir()
	src := quarantineFixture(t, dir, "present.jpg", "bytes")
	if err := QuarantineImage(dir, "op-four", src, RepairEntry{SlotIndex: 0, FileName: "present.jpg"}); err != nil {
		t.Fatalf("seeding a real entry: %v", err)
	}

	err := QuarantineImage(dir, "op-four", filepath.Join(dir, "gone.jpg"), RepairEntry{SlotIndex: 1, FileName: "gone.jpg"})
	if err == nil {
		t.Fatal("quarantining a vanished source must error")
	}
	if !strings.Contains(err.Error(), "gone.jpg") {
		t.Errorf("the error must name the missing file, got: %v", err)
	}

	m, mErr := ReadRepairManifest(dir, "op-four")
	if mErr != nil {
		t.Fatalf("ReadRepairManifest: %v", mErr)
	}
	if len(m.Entries) != 1 || m.Entries[0].FileName != "present.jpg" {
		t.Errorf("a failed quarantine must record no entry, manifest holds %+v", m.Entries)
	}
}

// TestConsumeRepairEntry_KeepsRepairRootWhileAnotherOpHoldsEntries pins that
// emptying one operation never tears the root out from under a concurrent one.
// Losing the root would orphan the other op's quarantined artwork.
func TestConsumeRepairEntry_KeepsRepairRootWhileAnotherOpHoldsEntries(t *testing.T) {
	dir := t.TempDir()
	srcA := quarantineFixture(t, dir, "a.jpg", "aaa")
	srcB := quarantineFixture(t, dir, "b.jpg", "bbb")
	if err := QuarantineImage(dir, "op-alpha", srcA, RepairEntry{SlotIndex: 0, FileName: "a.jpg"}); err != nil {
		t.Fatalf("quarantining into op-alpha: %v", err)
	}
	if err := QuarantineImage(dir, "op-beta", srcB, RepairEntry{SlotIndex: 0, FileName: "b.jpg"}); err != nil {
		t.Fatalf("quarantining into op-beta: %v", err)
	}

	m, err := ReadRepairManifest(dir, "op-alpha")
	if err != nil {
		t.Fatalf("ReadRepairManifest: %v", err)
	}
	// Emptying op-alpha removes its own dir but must leave the root, because
	// op-beta still lives under it.
	if err := ConsumeRepairEntry(dir, "op-alpha", m.Entries[0]); err != nil {
		t.Fatalf("consuming op-alpha's only entry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, RepairDirName, "op-alpha")); !os.IsNotExist(err) {
		t.Errorf("the emptied op dir must be removed; stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, RepairDirName)); err != nil {
		t.Fatalf("the repair root must survive while another op holds entries: %v", err)
	}
	beta, err := ReadRepairManifest(dir, "op-beta")
	if err != nil || beta == nil || len(beta.Entries) != 1 {
		t.Fatalf("op-beta must be intact, got %+v (err %v)", beta, err)
	}
	data, err := RepairEntryBytes(dir, "op-beta", beta.Entries[0])
	if err != nil || string(data) != "bbb" {
		t.Errorf("op-beta's bytes must survive, got %q (err %v)", data, err)
	}
}

// TestReadRepairManifest_MalformedIsAnErrorNotAnEmptyManifest pins the
// fail-loudly contract. Returning an empty manifest over unreadable JSON would
// report "nothing to restore" while the bytes sit recoverable on disk -- a
// false-green in the one path that exists to recover data.
func TestReadRepairManifest_MalformedIsAnErrorNotAnEmptyManifest(t *testing.T) {
	dir := t.TempDir()
	opDir := filepath.Join(dir, RepairDirName, "op-bad")
	if err := os.MkdirAll(opDir, 0o755); err != nil {
		t.Fatalf("creating op dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(opDir, repairManifestName), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("writing malformed manifest: %v", err)
	}

	m, err := ReadRepairManifest(dir, "op-bad")
	if err == nil {
		t.Fatalf("a malformed manifest must be an error, got manifest %+v", m)
	}
	if !strings.Contains(err.Error(), "decoding repair manifest") {
		t.Errorf("expected a decode error naming the manifest, got: %v", err)
	}
}

// TestReadRepairManifest_MissingOpIsNotAnError distinguishes "no such operation"
// from "broken operation": the former is an ordinary empty result, the latter is
// the error above.
func TestReadRepairManifest_MissingOpIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	m, err := ReadRepairManifest(dir, "op-absent")
	if err != nil {
		t.Fatalf("a missing op must not error: %v", err)
	}
	if m != nil {
		t.Errorf("a missing op must yield a nil manifest, got %+v", m)
	}
}

// TestListRepairOps_SkipsIdsThisPackageCouldNotHaveWritten proves the listing
// never hands an unsanitized directory name back to a caller that would join it
// into a path.
func TestListRepairOps_SkipsIdsThisPackageCouldNotHaveWritten(t *testing.T) {
	dir := t.TempDir()
	src := quarantineFixture(t, dir, "fanart.jpg", "bytes")
	if err := QuarantineImage(dir, "op-legit", src, RepairEntry{SlotIndex: 0, FileName: "fanart.jpg"}); err != nil {
		t.Fatalf("QuarantineImage: %v", err)
	}
	// A directory nothing in this package could have created.
	if err := os.MkdirAll(filepath.Join(dir, RepairDirName, "Not_Valid"), 0o755); err != nil {
		t.Fatalf("creating rogue dir: %v", err)
	}

	ops, err := ListRepairOps(dir)
	if err != nil {
		t.Fatalf("ListRepairOps: %v", err)
	}
	if len(ops) != 1 || ops[0] != "op-legit" {
		t.Errorf("ListRepairOps = %v, want only [op-legit]", ops)
	}
}
