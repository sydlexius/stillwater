package artist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// merge_extrafanart_report_test.go covers #3180: a merge that leaves the
// survivor holding a non-empty extrafanart/ REPORTS it (artist name and image
// count) and changes nothing on disk. The merge still combines both sides'
// extrafanart/ contents exactly as before; the tests below pin both halves.

// writeFiles writes name->content under dir (creating parents).
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
}

// invEntry is one regular file in an inventory: path relative to its artist
// directory, tagged with whose directory it came from, plus its content hash.
type invEntry struct {
	Rel string
	Sum string
}

// inventory walks root and returns every regular file as (rel, sha256),
// prefixed with tag so the before-merge inventory of two artists stays apart.
func inventory(t *testing.T, tag, root string) []invEntry {
	t.Helper()
	var out []invEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
		rel, _ := filepath.Rel(root, p)
		out = append(out, invEntry{Rel: tag + ":" + rel, Sum: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		t.Fatalf("inventory %s: %v", root, err)
	}
	return out
}

// unaccounted returns the before-entries that have no counterpart (by content,
// as a MULTISET: two byte-identical files need two files afterwards) in after,
// minus the entries the merge deletes on purpose. Files move and de-duplicate
// names during the merge, so identity is the content hash, not the path.
func unaccounted(before, after []invEntry, deliberate map[string]bool) []string {
	pool := map[string]int{}
	for _, a := range after {
		pool[a.Sum]++
	}
	var missing []string
	for _, b := range before {
		if deliberate[b.Rel] {
			continue
		}
		if pool[b.Sum] > 0 {
			pool[b.Sum]--
			continue
		}
		missing = append(missing, b.Rel)
	}
	sort.Strings(missing)
	return missing
}

// reportFixture seeds the survivor and loser extrafanart/ directories and
// returns the paths. Survivor holds fanart1.jpg, same-a.jpg and a dotfile image; the loser holds
// fanart1.jpg (a NAME COLLISION, different bytes), same-b.jpg (BYTE-IDENTICAL to
// the survivor's same-a.jpg), fanart2.jpg, plus the things the merge drops on
// purpose or does not count: a dotfile image, an OS junk file, and a nested dir.
// Expected report: 2 survivor + 3 loser images = 5.
const wantReportCount = 5

func reportFixture(t *testing.T) (svc *Service, survivorID, loserID, survivorPath, loserPath string) {
	t.Helper()
	svc, _, survivorID, loserID = mergeSetup(t)
	ctx := context.Background()
	survivorPath = mustGetArtist(t, svc, ctx, survivorID).Path
	loserPath = mustGetArtist(t, svc, ctx, loserID).Path
	writeFiles(t, filepath.Join(survivorPath, "extrafanart"), map[string]string{
		"fanart1.jpg": "survivor-1",
		"same-a.jpg":  "identical-bytes",
		".keep.jpg":   "survivor-dotfile", // stays put; never counted
	})
	writeFiles(t, filepath.Join(loserPath, "extrafanart"), map[string]string{
		"fanart1.jpg":      "loser-1",
		"same-b.jpg":       "identical-bytes",
		"fanart2.jpg":      "loser-2",
		".hidden.jpg":      "dotfile",
		"Thumbs.db":        "junk",
		"nested/inner.jpg": "inner",
	})
	return svc, survivorID, loserID, survivorPath, loserPath
}

// assertReportPreconditions proves the fixture holds what the test claims
// BEFORE the exercise (a vacuous fixture would pass for the wrong reason).
func assertReportPreconditions(t *testing.T, survivorPath, loserPath string) {
	t.Helper()
	for dir, want := range map[string]int{
		filepath.Join(survivorPath, "extrafanart"): 3, // 2 images + dotfile
		filepath.Join(loserPath, "extrafanart"):    6, // 3 images + dotfile + junk + nested dir
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("precondition: read %s: %v", dir, err)
		}
		if len(entries) != want {
			t.Fatalf("precondition: %s holds %d entries, want %d", dir, len(entries), want)
		}
	}
	a, _ := os.ReadFile(filepath.Join(survivorPath, "extrafanart", "same-a.jpg"))
	b, _ := os.ReadFile(filepath.Join(loserPath, "extrafanart", "same-b.jpg"))
	if len(a) == 0 || string(a) != string(b) {
		t.Fatalf("precondition: same-a.jpg and same-b.jpg must be byte-identical and non-empty")
	}
}

func TestMergeArtists_ReportsSurvivorExtraFanart(t *testing.T) {
	t.Parallel()
	svc, survivorID, loserID, survivorPath, loserPath := reportFixture(t)
	ctx := context.Background()
	assertReportPreconditions(t, survivorPath, loserPath)

	res, err := svc.MergeArtists(ctx, MergeRequest{SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix"})
	if err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}
	if res.ExtraFanart == nil {
		t.Fatal("ExtraFanart = nil, want a report for a survivor left with extrafanart images")
	}
	if res.ExtraFanart.FileCount != wantReportCount {
		t.Errorf("FileCount = %d, want %d (survivor 2 + loser 3 images; dotfile, junk and nested dir excluded)", res.ExtraFanart.FileCount, wantReportCount)
	}
	if res.ExtraFanart.ArtistName != "The Cure" {
		t.Errorf("ArtistName = %q, want %q", res.ExtraFanart.ArtistName, "The Cure")
	}

	// The report matches the folder on disk: the same count the migration sees.
	entries, err := os.ReadDir(filepath.Join(survivorPath, "extrafanart"))
	if err != nil {
		t.Fatalf("read survivor extrafanart: %v", err)
	}
	images := 0
	for _, e := range entries {
		if !e.IsDir() && e.Name()[0] != '.' && filepath.Ext(e.Name()) == ".jpg" {
			images++
		}
	}
	if images != res.ExtraFanart.FileCount {
		t.Errorf("survivor extrafanart holds %d images, report says %d", images, res.ExtraFanart.FileCount)
	}
}

// The merge's filesystem behavior for extrafanart/ is UNCHANGED (#3180): both
// sides' contents are combined, a name clash keeps both, and NO file is lost.
// Every pre-merge file of BOTH artists is accounted for by content (multiset,
// so the byte-identical pair must survive as two files); only the dotfile and
// the OS junk file are deleted, deliberately and by name.
func TestMergeArtists_ExtraFanartNoFileLostAndBehaviorUnchanged(t *testing.T) {
	t.Parallel()
	svc, survivorID, loserID, survivorPath, loserPath := reportFixture(t)
	ctx := context.Background()
	assertReportPreconditions(t, survivorPath, loserPath)

	before := append(inventory(t, "S", survivorPath), inventory(t, "L", loserPath)...)
	deliberate := map[string]bool{
		"L:extrafanart/.hidden.jpg": true, // dotfile: never carried over
		"L:extrafanart/Thumbs.db":   true, // OS junk: swept with the loser dir
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix"}); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}
	after := inventory(t, "S", survivorPath)
	if missing := unaccounted(before, after, deliberate); len(missing) != 0 {
		t.Errorf("files lost by the merge: %v", missing)
	}
	if len(after) != len(before)-len(deliberate) {
		t.Errorf("survivor holds %d files, want %d (every pre-merge file minus the %d deliberate deletes)", len(after), len(before)-len(deliberate), len(deliberate))
	}

	// Exact layout, as before this change.
	extra := filepath.Join(survivorPath, "extrafanart")
	assertFileContent(t, filepath.Join(extra, "fanart1.jpg"), "survivor-1")
	assertFileContent(t, filepath.Join(extra, "fanart1-1.jpg"), "loser-1")
	assertFileContent(t, filepath.Join(extra, "same-a.jpg"), "identical-bytes")
	assertFileContent(t, filepath.Join(extra, "same-b.jpg"), "identical-bytes")
	assertFileContent(t, filepath.Join(extra, "fanart2.jpg"), "loser-2")
	assertFileContent(t, filepath.Join(extra, "nested", "inner.jpg"), "inner")
	if _, err := os.Stat(loserPath); !os.IsNotExist(err) {
		t.Errorf("loser dir should be removed, stat err = %v", err)
	}
}

// The unaccounted helper must itself catch a loss, including a lost duplicate
// that a set-based check would hide.
func TestUnaccountedCatchesLostDuplicate(t *testing.T) {
	t.Parallel()
	before := []invEntry{{"S:a.jpg", "x"}, {"L:b.jpg", "x"}, {"L:c.jpg", "y"}}
	after := []invEntry{{"S:a.jpg", "x"}, {"S:c.jpg", "y"}}
	got := unaccounted(before, after, nil)
	if len(got) != 1 || got[0] != "L:b.jpg" {
		t.Errorf("unaccounted = %v, want [L:b.jpg] (one of two identical files lost)", got)
	}
}

// The count in a dry run equals the count the real merge then reports for the
// same fixture, and the dry run changes nothing on disk.
func TestMergeArtists_DryRunExtraFanartCountMatchesCommit(t *testing.T) {
	t.Parallel()

	type tc struct {
		name  string
		setup func(t *testing.T, survivorPath, loserPath string)
		want  int
	}
	cases := []tc{
		{"both sides have extrafanart", func(t *testing.T, s, l string) {
			writeFiles(t, filepath.Join(s, "extrafanart"), map[string]string{"fanart1.jpg": "s1", "dup.png": "same"})
			writeFiles(t, filepath.Join(l, "extrafanart"), map[string]string{"fanart1.jpg": "l1", "dup.png": "same", ".hidden.jpg": "dot", "notes.txt": "text"})
		}, 4},
		{"survivor lacks it: whole folder moves, symlink comes along", func(t *testing.T, s, l string) {
			writeFiles(t, filepath.Join(l, "extrafanart"), map[string]string{"a.jpg": "a", "b.jpg": "b"})
			if err := os.Symlink(filepath.Join(l, "extrafanart", "a.jpg"), filepath.Join(l, "extrafanart", "link.jpg")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
		}, 3},
		{"merged symlink is skipped", func(t *testing.T, s, l string) {
			writeFiles(t, filepath.Join(s, "extrafanart"), map[string]string{"a.jpg": "a"})
			writeFiles(t, filepath.Join(l, "extrafanart"), map[string]string{"b.jpg": "b"})
			if err := os.Symlink(filepath.Join(l, "extrafanart", "b.jpg"), filepath.Join(l, "extrafanart", "link.jpg")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
		}, 2},
		{"loser extrafanart is a symlink to a directory: not merged, not counted", func(t *testing.T, s, l string) {
			writeFiles(t, filepath.Join(s, "extrafanart"), map[string]string{"a.jpg": "a"})
			target := filepath.Join(t.TempDir(), "elsewhere")
			writeFiles(t, target, map[string]string{"x.jpg": "x", "y.jpg": "y"})
			if err := os.Symlink(target, filepath.Join(l, "extrafanart")); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			if fi, err := os.Lstat(filepath.Join(l, "extrafanart")); err != nil || fi.IsDir() {
				t.Fatalf("precondition: loser extrafanart must be a symlink, not a directory (%v)", err)
			}
		}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			svc, _, survivorID, loserID := mergeSetup(t)
			ctx := context.Background()
			survivorPath := mustGetArtist(t, svc, ctx, survivorID).Path
			loserPath := mustGetArtist(t, svc, ctx, loserID).Path
			c.setup(t, survivorPath, loserPath)
			before := append(inventory(t, "S", survivorPath), inventory(t, "L", loserPath)...)

			req := MergeRequest{SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix"}
			req.DryRun = true
			dry, err := svc.MergeArtists(ctx, req)
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if dry.ExtraFanart == nil {
				t.Fatal("dry run: ExtraFanart = nil, want a report")
			}
			if dry.ExtraFanart.FileCount != c.want {
				t.Errorf("dry run count = %d, want %d", dry.ExtraFanart.FileCount, c.want)
			}
			afterDry := append(inventory(t, "S", survivorPath), inventory(t, "L", loserPath)...)
			if len(afterDry) != len(before) || len(unaccounted(before, afterDry, nil)) != 0 {
				t.Errorf("dry run changed the filesystem")
			}

			req.DryRun = false
			done, err := svc.MergeArtists(ctx, req)
			if err != nil {
				t.Fatalf("real merge: %v", err)
			}
			if done.ExtraFanart == nil {
				t.Fatal("real merge: ExtraFanart = nil, want a report")
			}
			if done.ExtraFanart.FileCount != dry.ExtraFanart.FileCount {
				t.Errorf("real count = %d, dry-run count = %d: they must match", done.ExtraFanart.FileCount, dry.ExtraFanart.FileCount)
			}
			if done.ExtraFanart.ArtistName != dry.ExtraFanart.ArtistName {
				t.Errorf("artist name differs: dry %q real %q", dry.ExtraFanart.ArtistName, done.ExtraFanart.ArtistName)
			}
		})
	}
}

// The report is silent when the survivor ends with no extrafanart images, and
// extrathumbs/ is never reported even though it is merged additively.
func TestMergeArtists_ExtraFanartReportSilentWhenNothingToMigrate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, s, l string)
	}{
		{"no extrafanart anywhere", func(t *testing.T, s, l string) {}},
		{"empty survivor dir, loser holds only junk and a nested dir", func(t *testing.T, s, l string) {
			if err := os.MkdirAll(filepath.Join(s, "extrafanart"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFiles(t, filepath.Join(l, "extrafanart"), map[string]string{"Thumbs.db": "junk", ".hidden.jpg": "dot", "nested/inner.jpg": "x"})
		}},
		{"extrathumbs only is merged but not reported", func(t *testing.T, s, l string) {
			writeFiles(t, filepath.Join(s, "extrathumbs"), map[string]string{"thumb1.jpg": "s"})
			writeFiles(t, filepath.Join(l, "extrathumbs"), map[string]string{"thumb2.jpg": "l"})
		}},
	}
	for _, c := range cases {
		for _, dry := range []bool{true, false} {
			name := c.name + "/real"
			if dry {
				name = c.name + "/dry"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				svc, _, survivorID, loserID := mergeSetup(t)
				ctx := context.Background()
				survivorPath := mustGetArtist(t, svc, ctx, survivorID).Path
				loserPath := mustGetArtist(t, svc, ctx, loserID).Path
				c.setup(t, survivorPath, loserPath)

				res, err := svc.MergeArtists(ctx, MergeRequest{SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix", DryRun: dry})
				if err != nil {
					t.Fatalf("MergeArtists: %v", err)
				}
				if res.ExtraFanart != nil {
					t.Errorf("ExtraFanart = %+v, want nil (nothing for the migration to move)", *res.ExtraFanart)
				}
				if c.name == "extrathumbs only is merged but not reported" && !dry {
					// extrathumbs/ behavior unchanged: both sides still combined.
					assertFileContent(t, filepath.Join(survivorPath, "extrathumbs", "thumb1.jpg"), "s")
					assertFileContent(t, filepath.Join(survivorPath, "extrathumbs", "thumb2.jpg"), "l")
				}
			})
		}
	}
}

// A merge that fails part-way must not claim a count it did not verify.
func TestMergeArtists_ExtraFanartNoReportOnFailedMerge(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	survivorPath := mustGetArtist(t, svc, ctx, survivorID).Path
	loserPath := mustGetArtist(t, svc, ctx, loserID).Path
	survExtra := filepath.Join(survivorPath, "extrafanart")
	writeFiles(t, survExtra, map[string]string{"keep.jpg": "survivor"})
	writeFiles(t, filepath.Join(loserPath, "extrafanart"), map[string]string{"new.jpg": "loser"})
	if err := os.Chmod(survExtra, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(survExtra, 0o755) })

	res, err := svc.MergeArtists(ctx, MergeRequest{SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix"})
	if err == nil {
		t.Fatal("MergeArtists: expected a filesystem error, got nil")
	}
	if res == nil {
		t.Fatal("expected a partial result alongside the error")
	}
	if res.ExtraFanart != nil {
		t.Errorf("ExtraFanart = %+v on a failed merge, want nil (the count was not verified)", *res.ExtraFanart)
	}
}

// A failure to COUNT must never abort a merge that has already moved files, and
// must not be silent: the merge succeeds, no count is claimed, and the result
// carries a warning.
func TestMergeArtists_ExtraFanartCountFailureIsAWarningNotAnAbort(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot make the folder uncountable")
	}
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	survivorPath := mustGetArtist(t, svc, ctx, survivorID).Path
	survExtra := filepath.Join(survivorPath, "extrafanart")
	writeFiles(t, survExtra, map[string]string{"keep.jpg": "survivor"})
	if err := os.Chmod(survExtra, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(survExtra, 0o755) })
	if _, err := os.ReadDir(survExtra); err == nil {
		t.Fatal("precondition: the survivor's extrafanart must be unreadable")
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix"})
	if err != nil {
		t.Fatalf("MergeArtists: %v (a count failure must not fail the merge)", err)
	}
	if _, gerr := svc.GetByID(ctx, loserID); !errors.Is(gerr, ErrNotFound) {
		t.Errorf("loser row err = %v, want ErrNotFound (the DB phase must have run)", gerr)
	}
	if res.ExtraFanart != nil {
		t.Errorf("ExtraFanart = %+v, want nil (the count was not verified)", *res.ExtraFanart)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "could not count the extrafanart images") {
			found = true
		}
	}
	if !found {
		t.Errorf("Warnings = %v, want one saying the extrafanart images could not be counted", res.Warnings)
	}
}

// The report names the SURVIVOR, not a loser, when the two names differ.
func TestMergeArtists_ExtraFanartReportNamesTheSurvivor(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `UPDATE artists SET name = ? WHERE id = ?`, "Cure, The", loserID); err != nil {
		t.Fatalf("rename loser: %v", err)
	}
	survivorPath := mustGetArtist(t, svc, ctx, survivorID).Path
	loserPath := mustGetArtist(t, svc, ctx, loserID).Path
	if a, b := mustGetArtist(t, svc, ctx, survivorID).Name, mustGetArtist(t, svc, ctx, loserID).Name; a == b {
		t.Fatalf("precondition: names must differ, both %q", a)
	}
	writeFiles(t, filepath.Join(survivorPath, "extrafanart"), map[string]string{"a.jpg": "a"})
	writeFiles(t, filepath.Join(loserPath, "extrafanart"), map[string]string{"b.jpg": "b"})
	for _, dry := range []bool{true, false} {
		res, err := svc.MergeArtists(ctx, MergeRequest{SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix", DryRun: dry})
		if err != nil {
			t.Fatalf("MergeArtists(dry=%v): %v", dry, err)
		}
		if res.ExtraFanart == nil || res.ExtraFanart.ArtistName != "The Cure" {
			t.Errorf("dry=%v report = %+v, want the survivor name %q", dry, res.ExtraFanart, "The Cure")
		}
	}
}
