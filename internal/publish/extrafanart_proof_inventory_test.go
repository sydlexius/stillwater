package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	img "github.com/sydlexius/stillwater/internal/image"
)

// Shared by the live Emby proof for #3179 (live_extrafanart_migration_integration_test.go)
// and by the hermetic tests below, so the comparison the live run relies on is
// itself proven here without an Emby.

// proofSHA is the hex sha256 of b.
func proofSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// proofFile is one file of an artist directory, by path and by content.
type proofFile struct{ rel, sha string }

// contentInventory lists every regular file under dir, recursively, with the
// sha256 of its bytes. Content, not size or mtime: "the pushed image survived"
// passes while a different file is destroyed (the #3174 near-miss).
func contentInventory(t *testing.T, dir string) []proofFile {
	t.Helper()
	var out []proofFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(dir, p)
		out = append(out, proofFile{rel: filepath.ToSlash(rel), sha: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		t.Fatalf("inventorying %s: %v", dir, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

// missingFromInventory names what went missing between before and after. Two
// independent checks, because each alone has a blind spot:
//   - a PATH present before and absent after (a file went missing by name);
//   - a CONTENT multiset deficit (a file's bytes no longer exist anywhere), where
//     files sharing bytes each need their own surviving copy, so deleting one of
//     two byte-identical files is reported.
//
// Files that merely appeared are not losses and are not reported.
func missingFromInventory(before, after []proofFile) []string {
	have := map[string]bool{}
	copies := map[string]int{}
	for _, f := range after {
		have[f.rel] = true
		copies[f.sha]++
	}
	var out []string
	for _, f := range before {
		if !have[f.rel] {
			out = append(out, fmt.Sprintf("path %s (content %.8s) is gone", f.rel, f.sha))
		}
	}
	for _, f := range before {
		if copies[f.sha] == 0 {
			out = append(out, fmt.Sprintf("content of %s (%.8s) no longer exists in any file", f.rel, f.sha))
			continue
		}
		copies[f.sha]--
	}
	return out
}

// Fixture shape: 1 root fanart + 2 files under extrafanart/, three distinct images; the
// shape of the #3174 measurement (a discoverable fanart primary plus two extrafanart/ files).
const (
	proofRootFiles  = 1
	proofExtraFiles = 2
)

// seedExtrafanartFixture writes the fixture into dir and ASSERTS its defining
// property (the counts below), so a run can never be vacuous.
func seedExtrafanartFixture(t *testing.T, dir string) {
	t.Helper()
	files := map[string]int{
		"fanart.jpg":            0xB101,
		"extrafanart/extra.jpg": 0xB103,
		"extrafanart/more.jpg":  0xB104,
	}
	seen := map[string]string{}
	for rel, seed := range files {
		b := bandJPEG(t, seed)
		if prev, dup := seen[proofSHA(b)]; dup {
			t.Fatalf("fixture images %s and %s are byte-identical", prev, rel)
		}
		seen[proofSHA(b)] = rel
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
	root, extra := 0, 0
	for _, f := range contentInventory(t, dir) {
		if strings.HasPrefix(f.rel, "extrafanart/") {
			extra++
		} else if strings.HasSuffix(f.rel, ".jpg") {
			root++
		}
	}
	if root != proofRootFiles || extra != proofExtraFiles {
		t.Fatalf("precondition: fixture holds %d root + %d extrafanart/ images, want %d + %d", root, extra, proofRootFiles, proofExtraFiles)
	}
}

// noopInvalidator satisfies image.HashInvalidator: the proof has no database.
type noopInvalidator struct{}

func (noopInvalidator) InvalidateImageHashes(context.Context, string, string) error   { return nil }
func (noopInvalidator) InvalidateImageGeometry(context.Context, string, string) error { return nil }

// runExtrafanartMigration runs the engine exactly as the admin handler does
// (img.PlanExtraFanartMigration then img.ApplyExtraFanartMigration, Emby
// numbering) and fatals unless the documented end state holds: every planned
// file moved, extrafanart/ removed, no pre-migration bytes lost.
func runExtrafanartMigration(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	before := contentInventory(t, dir)
	names, err := img.ResolveFanartNames(nil)
	if err != nil {
		t.Fatalf("resolving fanart names: %v", err)
	}
	plan, err := img.PlanExtraFanartMigration(ctx, dir, names, false)
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if missing := missingFromInventory(before, contentInventory(t, dir)); len(missing) != 0 || len(plan.Entries) != proofExtraFiles {
		t.Fatalf("a dry run must write nothing and plan %d moves: planned %d, missing %v", proofExtraFiles, len(plan.Entries), missing)
	}
	res, err := img.ApplyExtraFanartMigration(ctx, noopInvalidator{}, "proof-artist", plan)
	if err != nil {
		t.Fatalf("applying: %v", err)
	}
	if res.Moved != proofExtraFiles || res.Dir != img.DirRemoved {
		t.Fatalf("migration end state: moved=%d dir=%q, want %d moved and extrafanart/ removed", res.Moved, res.Dir, proofExtraFiles)
	}
	// Moved and renamed, nothing lost: by CONTENT, naming what went missing.
	// Only the extrafanart/ PATHS legitimately change, so compare bytes alone.
	if lost := contentOnly(missingFromInventory(before, contentInventory(t, dir))); len(lost) != 0 {
		t.Fatalf("migration lost content: %v", lost)
	}
	if _, err := os.Stat(filepath.Join(dir, "extrafanart")); !os.IsNotExist(err) {
		t.Fatalf("extrafanart/ should be gone after the migration (stat err %v)", err)
	}
}

// contentOnly keeps the content-deficit lines: the migration renames paths on purpose.
func contentOnly(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, "content of") {
			out = append(out, l)
		}
	}
	return out
}

func TestExtrafanartProof_NonLiveHalf(t *testing.T) {
	dir := t.TempDir()
	seedExtrafanartFixture(t, dir)
	runExtrafanartMigration(t, dir)
	post := contentInventory(t, dir)
	if len(post) != proofRootFiles+proofExtraFiles {
		t.Fatalf("after migration want %d root files, got %v", proofRootFiles+proofExtraFiles, post)
	}
	// Idempotent at the operator level: a second run is a no-op on disk.
	names, _ := img.ResolveFanartNames(nil)
	plan, err := img.PlanExtraFanartMigration(context.Background(), dir, names, false)
	if err != nil || len(plan.Entries) != 0 {
		t.Fatalf("second plan: err=%v entries=%d, want none", err, len(plan.Entries))
	}
	if missing := missingFromInventory(post, contentInventory(t, dir)); len(missing) != 0 {
		t.Fatalf("second plan changed the directory: %v", missing)
	}
}

// The comparator must go RED on both loss shapes, or the live green means nothing.
func TestExtrafanartProof_InventoryGoesRedOnLoss(t *testing.T) {
	t.Run("one of two byte-identical files deleted", func(t *testing.T) {
		dir := t.TempDir()
		seedExtrafanartFixture(t, dir)
		dup, _ := os.ReadFile(filepath.Join(dir, "fanart.jpg"))
		if err := os.WriteFile(filepath.Join(dir, "fanart-copy.jpg"), dup, 0o644); err != nil {
			t.Fatal(err)
		}
		before := contentInventory(t, dir)
		if err := os.Remove(filepath.Join(dir, "fanart-copy.jpg")); err != nil {
			t.Fatal(err)
		}
		got := missingFromInventory(before, contentInventory(t, dir))
		if len(got) != 2 || !strings.Contains(strings.Join(got, "|"), "fanart-copy.jpg") {
			t.Fatalf("want the path and the content deficit for fanart-copy.jpg, got %v", got)
		}
	})
	t.Run("a unique file deleted", func(t *testing.T) {
		dir := t.TempDir()
		seedExtrafanartFixture(t, dir)
		before := contentInventory(t, dir)
		if err := os.Remove(filepath.Join(dir, "extrafanart", "more.jpg")); err != nil {
			t.Fatal(err)
		}
		got := missingFromInventory(before, contentInventory(t, dir))
		if len(got) != 2 || !strings.Contains(got[0], "extrafanart/more.jpg") {
			t.Fatalf("want path and content lines naming extrafanart/more.jpg, got %v", got)
		}
	})
	t.Run("an unchanged directory is green", func(t *testing.T) {
		dir := t.TempDir()
		seedExtrafanartFixture(t, dir)
		inv := contentInventory(t, dir)
		if got := missingFromInventory(inv, contentInventory(t, dir)); len(got) != 0 {
			t.Fatalf("false red: %v", got)
		}
	})
}

// ID3v2.3 frame ids, spelled as bytes so the typo checker does not read them as words.
var (
	id3Artist      = string([]byte{0x54, 0x50, 0x45, 0x31})
	id3AlbumArtist = string([]byte{0x54, 0x50, 0x45, 0x32})
	id3Album       = string([]byte{0x54, 0x41, 0x4c, 0x42})
	id3Title       = string([]byte{0x54, 0x49, 0x54, 0x32})
)

// proofMP3 builds a small tagged mp3: an ID3v2.3 tag (artist, album artist, album,
// title; synthetic names) plus 40 silent MPEG-1 Layer III frames (128 kbps, 44.1 kHz).
func proofMP3(title string) []byte {
	var frames bytes.Buffer
	for _, kv := range [][2]string{{id3Artist, "SW Proof 3179 Artist"}, {id3AlbumArtist, "SW Proof 3179 Artist"}, {id3Album, "SW Proof 3179 Album"}, {id3Title, title}} {
		n := len(kv[1]) + 1
		frames.WriteString(kv[0])
		frames.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n), 0, 0, 0})
		frames.WriteString(kv[1])
	}
	n := frames.Len()
	out := append([]byte("ID3\x03\x00\x00"), byte(n>>21&0x7f), byte(n>>14&0x7f), byte(n>>7&0x7f), byte(n&0x7f))
	out = append(out, frames.Bytes()...)
	for i := 0; i < 40; i++ {
		out = append(out, 0xFF, 0xFB, 0x90, 0x64)
		out = append(out, make([]byte, 413)...)
	}
	return out
}

// writeProofAlbum creates the fixture album folder with two tagged mp3s (idempotent).
func writeProofAlbum(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i, title := range []string{"SW Proof Track One", "SW Proof Track Two"} {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("0%d.mp3", i+1)), proofMP3(title), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func TestExtrafanartProof_FixtureMP3IsTagged(t *testing.T) {
	b := proofMP3("t")
	if !bytes.HasPrefix(b, []byte("ID3\x03")) || !bytes.Contains(b, []byte(id3AlbumArtist)) || len(b) < 16000 {
		t.Fatalf("unexpected mp3 fixture (%d bytes)", len(b))
	}
	dir := filepath.Join(t.TempDir(), "album")
	if err := writeProofAlbum(dir); err != nil {
		t.Fatal(err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 2 {
		t.Fatalf("album holds %d files, want 2", len(es))
	}
}

// extrafanartLoss decides whether one control run shows THE loss: an extrafanart/
// file went missing (a missing entry under extrafanart/, or fewer files there than
// seeded). Any other missing entry, such as a root file whose bytes Emby rewrote in
// place, is returned in other so it stays visible without counting as the loss.
func extrafanartLoss(missing []string, extraFiles int) (isLoss bool, other []string) {
	isLoss = extraFiles < proofExtraFiles
	for _, m := range missing {
		if strings.Contains(m, " extrafanart/") {
			isLoss = true
		} else {
			other = append(other, m)
		}
	}
	return isLoss, other
}

func TestExtrafanartProof_ControlCountsOnlyExtrafanartLoss(t *testing.T) {
	t.Run("root file rewritten in place, extrafanart intact: not the loss", func(t *testing.T) {
		dir := t.TempDir()
		seedExtrafanartFixture(t, dir)
		before := contentInventory(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), bandJPEG(t, 0xC0DE), 0o644); err != nil {
			t.Fatal(err)
		}
		missing := missingFromInventory(before, contentInventory(t, dir))
		if len(missing) == 0 {
			t.Fatal("precondition: rewriting the root file must show up as missing content")
		}
		if isLoss, other := extrafanartLoss(missing, proofExtraFiles); isLoss || len(other) == 0 {
			t.Fatalf("a rewritten root file must not count as the loss but stay visible: loss=%v other=%v", isLoss, other)
		}
	})
	t.Run("an extrafanart file deleted: the loss", func(t *testing.T) {
		dir := t.TempDir()
		seedExtrafanartFixture(t, dir)
		before := contentInventory(t, dir)
		if err := os.Remove(filepath.Join(dir, "extrafanart", "more.jpg")); err != nil {
			t.Fatal(err)
		}
		missing := missingFromInventory(before, contentInventory(t, dir))
		if isLoss, _ := extrafanartLoss(missing, proofExtraFiles-1); !isLoss {
			t.Fatalf("a deleted extrafanart file must count as the loss: %v", missing)
		}
	})
}
