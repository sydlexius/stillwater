package image

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// makeFifoHook is set on unix by extrafanart_fifo_unix_test.go; nil elsewhere.
var makeFifoHook func(path string) error

// seedTree writes rel-path -> content under a fresh dir.
func seedTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// inventory is the FULL tree: every file (content), symlink (target) and
// directory, relative.
func inventory(t *testing.T, dir string) map[string]string {
	t.Helper()
	inv := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if info.IsDir() {
			inv[rel] = "<dir>"
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 { // record the link itself, never its target
			tgt, lerr := os.Readlink(p)
			inv[rel] = "<link:" + tgt + ">"
			return lerr
		}
		b, rerr := os.ReadFile(p)
		inv[rel] = string(b)
		return rerr
	})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// treeDiff names every path that is missing, unexpected, or changed between
// two inventories; "" means identical.
func treeDiff(got, want map[string]string) string {
	var out []string
	for p, w := range want {
		if g, ok := got[p]; !ok {
			out = append(out, "missing: "+p)
		} else if g != w {
			out = append(out, "changed: "+p+" ("+g+" != "+w+")")
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			out = append(out, "unexpected: "+p)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "; ")
}

// wantInv asserts the WHOLE artist directory equals want, naming differences.
func wantInv(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	if d := treeDiff(inventory(t, dir), want); d != "" {
		t.Errorf("artist dir differs from expected: %s", d)
	}
}

// requireExtraFanartCount fails the test unless extrafanart/ holds exactly n
// entries now, so a fixture that silently seeded nothing cannot pass.
func requireExtraFanartCount(t *testing.T, dir string, n int) {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dir, extraFanartDir))
	if err != nil || len(ents) != n {
		t.Fatalf("precondition: extrafanart/ has %d entries (err=%v), want %d", len(ents), err, n)
	}
}

func destNames(p *ExtraFanartPlan) []string {
	var out []string
	for _, e := range p.Entries {
		if e.Disposition == DispositionMove {
			out = append(out, filepath.Base(e.Dest))
		}
	}
	return out
}

func TestPlanExtraFanart_IdenticalPairIsReportedAndKept(t *testing.T) {
	ctx := context.Background()
	dir := seedTree(t, map[string]string{"fanart.jpg": "same-bytes", "extrafanart/a.jpg": "same-bytes"})
	root, _ := HashFile(ctx, filepath.Join(dir, "fanart.jpg"), false)
	extra, _ := HashFile(ctx, filepath.Join(dir, "extrafanart/a.jpg"), false)
	if root.Content == "" || root.Content != extra.Content {
		t.Fatal("precondition: the pair must have equal sha256")
	}
	before := inventory(t, dir)

	plan, err := PlanExtraFanartMigration(ctx, dir, []string{"fanart.jpg"}, false)
	if err != nil {
		t.Fatal(err)
	}
	wantInv(t, dir, before) // planning writes nothing
	if len(plan.Entries) != 1 || plan.Entries[0].Disposition != DispositionSkipIdentical {
		t.Fatalf("plan must name the skip, got %+v", plan.Entries)
	}
	res, err := ApplyExtraFanartMigration(ctx, &fakeHashInvalidator{}, "a1", plan)
	if err != nil {
		t.Fatal(err)
	}
	wantInv(t, dir, before) // both files, and the directory, still present
	if res.Moved != 0 || res.Dir != DirUntouched {
		t.Errorf("moved=%d dir=%q, want 0 and untouched", res.Moved, res.Dir)
	}
}

func TestPlanExtraFanart_ConventionFollowsDirectoryNotProfile(t *testing.T) {
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart/a.JPG": "a"}) // upper-case ext is lowered
	// The "profile" prefers backdrop.jpg; the directory holds fanart.jpg.
	plan, err := PlanExtraFanartMigration(context.Background(), dir, []string{"backdrop.jpg", "fanart.jpg"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := destNames(plan); !reflect.DeepEqual(got, []string{"fanart2.jpg"}) {
		t.Errorf("dest = %v, want [fanart2.jpg]", got)
	}
}

func TestPlanExtraFanart_SameBatchFilesGetDistinctDestinations(t *testing.T) {
	files := map[string]string{"backdrop.jpg": "r", "backdrop2.jpg": "r2", "extrafanart/a.jpg": "a", "extrafanart/b.png": "b", "extrafanart/c.jpg": "a"}
	plan, _ := PlanExtraFanartMigration(context.Background(), seedTree(t, files), []string{"backdrop.jpg"}, false)
	// c is byte-identical to a (also moving), so it is skipped, not duplicated at the root.
	if len(plan.Entries) != 3 || plan.Entries[2].Disposition != DispositionSkipIdentical {
		t.Fatalf("plan = %+v", plan.Entries)
	}
	// backdrop2 is index 1, so the next suffixes are 3 and 4; the source's own
	// extension is kept.
	if got := destNames(plan); !reflect.DeepEqual(got, []string{"backdrop3.jpg", "backdrop4.png"}) {
		t.Errorf("dest = %v", got)
	}
}

func TestPlanExtraFanart_KodiAndEmbyOffsetsProduceDifferentNames(t *testing.T) {
	ctx := context.Background()
	// Only the primary exists: index 1 is base2 for Emby, base1 for Kodi.
	files := map[string]string{"fanart.jpg": "r", "extrafanart/a.jpg": "a"}
	emby, _ := PlanExtraFanartMigration(ctx, seedTree(t, files), []string{"fanart.jpg"}, false)
	kodi, _ := PlanExtraFanartMigration(ctx, seedTree(t, files), []string{"fanart.jpg"}, true)
	if e, k := destNames(emby), destNames(kodi); !reflect.DeepEqual(e, []string{"fanart2.jpg"}) || !reflect.DeepEqual(k, []string{"fanart1.jpg"}) {
		t.Errorf("emby=%v kodi=%v, want [fanart2.jpg] / [fanart1.jpg]", e, k)
	}
}

func TestPlanExtraFanart_AllocatesFromDiskNotCount(t *testing.T) {
	// Two files on disk (a count says next suffix 3) but the highest is 4.
	dir := seedTree(t, map[string]string{"backdrop.jpg": "r", "backdrop4.jpg": "r4", "extrafanart/a.jpg": "a"})
	plan, _ := PlanExtraFanartMigration(context.Background(), dir, []string{"backdrop.jpg"}, false)
	if got := destNames(plan); !reflect.DeepEqual(got, []string{"backdrop5.jpg"}) {
		t.Errorf("dest = %v, want [backdrop5.jpg]", got)
	}
}

func TestApplyExtraFanart_RefusesOccupiedDestinationAndReportsPartialTruthfully(t *testing.T) {
	ctx := context.Background()
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart/a.jpg": "a", "extrafanart/b.jpg": "b"})
	plan, _ := PlanExtraFanartMigration(ctx, dir, []string{"fanart.jpg"}, false)
	if got := destNames(plan); !reflect.DeepEqual(got, []string{"fanart2.jpg", "fanart3.jpg"}) {
		t.Fatalf("precondition: dest = %v", got)
	}
	// An operator file appears at the first destination between plan and apply.
	if err := os.WriteFile(filepath.Join(dir, "fanart2.jpg"), []byte("operator"), 0o644); err != nil {
		t.Fatal(err)
	}
	inv := &fakeHashInvalidator{}
	res, err := ApplyExtraFanartMigration(ctx, inv, "a1", plan)
	if err != nil {
		t.Fatal(err)
	}
	wantInv(t, dir, map[string]string{
		"fanart.jpg": "r", "fanart2.jpg": "operator", "fanart3.jpg": "b",
		"extrafanart": "<dir>", "extrafanart/a.jpg": "a",
	})
	if res.Results[0].Outcome != OutcomeBlocked || res.Results[0].Err == nil || res.Results[1].Outcome != OutcomeMoved {
		t.Errorf("results = %+v", res.Results)
	}
	if res.Moved != 1 || res.Dir != DirUntouched || len(inv.calls) != 1 {
		t.Errorf("moved=%d dir=%q invalidations=%d", res.Moved, res.Dir, len(inv.calls))
	}
}

func TestApplyExtraFanart_FullMigrationRemovesEmptyDirIsIdempotentAndInvalidates(t *testing.T) {
	ctx := context.Background()
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart/a.jpg": "a", "extrafanart/b.jpg": "b"})
	requireExtraFanartCount(t, dir, 2)
	plan, _ := PlanExtraFanartMigration(ctx, dir, []string{"fanart.jpg"}, false)
	inv := &fakeHashInvalidator{}
	res, err := ApplyExtraFanartMigration(ctx, inv, "a1", plan)
	if err != nil {
		t.Fatal(err)
	}
	after := map[string]string{"fanart.jpg": "r", "fanart2.jpg": "a", "fanart3.jpg": "b"}
	wantInv(t, dir, after)
	if res.Moved != 2 || res.Dir != DirRemoved {
		t.Errorf("moved=%d dir=%q", res.Moved, res.Dir)
	}
	if !reflect.DeepEqual(inv.calls, []string{"a1/fanart"}) || !reflect.DeepEqual(inv.geomCall, []string{"a1/fanart"}) {
		t.Errorf("invalidation hashes=%v geometry=%v", inv.calls, inv.geomCall)
	}
	// Runs 2 and 3 replay the same plan: each must be a no-op and leave the
	// FULL tree identical to the state after run 1.
	snapshot := inventory(t, dir)
	for run := 2; run <= 3; run++ {
		resN, err := ApplyExtraFanartMigration(ctx, inv, "a1", plan)
		if err != nil {
			t.Fatal(err)
		}
		if d := treeDiff(inventory(t, dir), snapshot); d != "" {
			t.Errorf("run %d changed the tree: %s", run, d)
		}
		if resN.Moved != 0 || len(inv.calls) != 1 {
			t.Errorf("run %d moved=%d invalidations=%d, want no-op", run, resN.Moved, len(inv.calls))
		}
	}
}

func TestApplyExtraFanart_KeepsDirWhenAnythingRemains(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		files   map[string]string
		want    map[string]string
		wantDir DirOutcome
	}{
		"skipped identical copy is never examined for removal": {
			map[string]string{"fanart.jpg": "r", "extrafanart/dup.jpg": "r", "extrafanart/b.jpg": "b"},
			map[string]string{"fanart.jpg": "r", "fanart2.jpg": "b", "extrafanart": "<dir>", "extrafanart/dup.jpg": "r"},
			DirUntouched,
		},
		"dotfile": {
			map[string]string{"fanart.jpg": "r", "extrafanart/b.jpg": "b", "extrafanart/.keep": "k"},
			map[string]string{"fanart.jpg": "r", "fanart2.jpg": "b", "extrafanart": "<dir>", "extrafanart/.keep": "k"},
			DirKeptNotEmpty,
		},
		"non-image file": {
			map[string]string{"fanart.jpg": "r", "extrafanart/b.jpg": "b", "extrafanart/notes.txt": "n"},
			map[string]string{"fanart.jpg": "r", "fanart2.jpg": "b", "extrafanart": "<dir>", "extrafanart/notes.txt": "n"},
			DirKeptNotEmpty,
		},
		"nested folder is kept whole": {
			map[string]string{"fanart.jpg": "r", "extrafanart/b.jpg": "b", "extrafanart/sub/c.jpg": "c"},
			map[string]string{"fanart.jpg": "r", "fanart2.jpg": "b", "extrafanart": "<dir>", "extrafanart/sub": "<dir>", "extrafanart/sub/c.jpg": "c"},
			DirKeptNotEmpty,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := seedTree(t, tc.files)
			requireExtraFanartCount(t, dir, 2) // every case seeds two top-level entries
			plan, _ := PlanExtraFanartMigration(ctx, dir, []string{"fanart.jpg"}, false)
			res, err := ApplyExtraFanartMigration(ctx, &fakeHashInvalidator{}, "a1", plan)
			if err != nil {
				t.Fatal(err)
			}
			wantInv(t, dir, tc.want)
			if res.Moved != 1 || res.Dir != tc.wantDir {
				t.Errorf("moved=%d dir=%q, want 1 and %q", res.Moved, res.Dir, tc.wantDir)
			}
		})
	}
}

func TestExtraFanart_UnhashableAndFailedFilesAreReportedWhileOthersMove(t *testing.T) {
	ctx := context.Background()
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart/b.jpg": "b"})
	// A dangling symlink cannot be hashed: planned as blocked, not dropped.
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "extrafanart", "a.jpg")); err != nil {
		t.Fatal(err)
	}
	requireExtraFanartCount(t, dir, 2)
	plan, err := PlanExtraFanartMigration(ctx, dir, []string{"fanart.jpg"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 2 || plan.Entries[0].Disposition != DispositionBlocked || plan.Entries[1].Disposition != DispositionMove {
		t.Fatalf("plan = %+v", plan.Entries)
	}
	// A stale entry whose source no longer exists is reported as gone.
	plan.Entries = append(plan.Entries, MigrationEntry{Source: filepath.Join(dir, "extrafanart", "z.jpg"),
		Dest: filepath.Join(dir, "fanart9.jpg"), Disposition: DispositionMove})
	res, err := ApplyExtraFanartMigration(ctx, &fakeHashInvalidator{}, "a1", plan)
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 1 || res.Dir != DirUntouched || res.Results[0].Outcome != OutcomeBlocked || res.Results[2].Outcome != OutcomeGone {
		t.Errorf("moved=%d dir=%q results=%+v", res.Moved, res.Dir, res.Results)
	}
	if _, serr := os.Lstat(filepath.Join(dir, "fanart2.jpg")); serr != nil {
		t.Errorf("the file that could move must have moved: %v", serr)
	}
	// The blocked symlink stays; nothing else in the artist dir changed.
	wantInv(t, dir, map[string]string{"fanart.jpg": "r", "fanart2.jpg": "b", "extrafanart": "<dir>", "extrafanart/a.jpg": "<link:" + filepath.Join(dir, "gone") + ">"})
}

func TestPlanExtraFanart_IdenticalToNonChosenConventionIsSkipped(t *testing.T) {
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "backdrop.jpg": "x", "extrafanart/dup.jpg": "x"})
	plan, err := PlanExtraFanartMigration(context.Background(), dir, []string{"fanart.jpg", "backdrop.jpg"}, false)
	if err != nil || len(plan.Entries) != 1 || plan.Entries[0].Disposition != DispositionSkipIdentical {
		t.Fatalf("err=%v plan=%+v", err, plan)
	}
}

func TestExtraFanart_SymlinkedSourceIsBlockedAndNothingMoves(t *testing.T) {
	ctx := context.Background()
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "elsewhere.jpg": "e", "extrafanart/b.jpg": "b"})
	if err := os.Symlink("../elsewhere.jpg", filepath.Join(dir, "extrafanart", "a.jpg")); err != nil {
		t.Fatal(err)
	}
	wantEntries := 2
	if makeFifoHook != nil {
		wantEntries = 3
	}
	// A FIFO would block HashFile's open; it must be blocked before any read.
	if makeFifoHook != nil {
		if err := makeFifoHook(filepath.Join(dir, "extrafanart", "c.jpg")); err != nil {
			t.Fatal(err)
		}
	}
	requireExtraFanartCount(t, dir, wantEntries)
	pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
	defer pcancel()
	plan, err := PlanExtraFanartMigration(pctx, dir, []string{"fanart.jpg"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if pctx.Err() != nil {
		t.Fatal("planning waited on a FIFO until the timeout instead of blocking it unread")
	}
	if plan.Entries[0].Disposition != DispositionBlocked || plan.Entries[1].Disposition != DispositionMove {
		t.Fatalf("plan = %+v", plan.Entries)
	}
	if makeFifoHook != nil && plan.Entries[2].Disposition != DispositionBlocked {
		t.Fatalf("fifo must be blocked: %+v", plan.Entries)
	}
	if _, err := os.Lstat(filepath.Join(dir, "extrafanart", "a.jpg")); err != nil {
		t.Fatal("link must not be consumed: ", err)
	}
	res, _ := ApplyExtraFanartMigration(ctx, &fakeHashInvalidator{}, "a1", plan)
	if res.Moved != 1 {
		t.Errorf("moved=%d, want only the regular file", res.Moved)
	}
	for _, r := range res.Results {
		if r.Outcome == OutcomeBlocked && r.Err == nil {
			t.Errorf("blocked result without Err: %+v", r)
		}
	}
	if fi, err := os.Lstat(filepath.Join(dir, "extrafanart", "a.jpg")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink must stay where it was: %v", err)
	}
}

func TestExtraFanart_SymlinkedDirIsRefusedAndNeverUnlinked(t *testing.T) {
	ctx := context.Background()
	dir := seedTree(t, map[string]string{"fanart.jpg": "r"})
	outside := seedTree(t, map[string]string{"a.jpg": "a"})
	link := filepath.Join(dir, "extrafanart")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanExtraFanartMigration(ctx, dir, []string{"fanart.jpg"}, false); err == nil {
		t.Error("planning through a symlinked extrafanart/ must be refused")
	}
	// A hand-built plan (stale or forged) must still not unlink the symlink.
	plan := &ExtraFanartPlan{ArtistDir: dir, Entries: []MigrationEntry{{
		Source: filepath.Join(link, "a.jpg"), Dest: filepath.Join(dir, "fanart2.jpg"), Disposition: DispositionMove}}}
	res, _ := ApplyExtraFanartMigration(ctx, &fakeHashInvalidator{}, "a1", plan)
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 || res.Moved != 0 || res.Results[0].Outcome != OutcomeFailed {
		t.Errorf("symlink must survive and nothing move: moved=%d results=%+v err=%v", res.Moved, res.Results, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "a.jpg")); err != nil {
		t.Errorf("outside/a.jpg must stay in place: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "fanart2.jpg")); err == nil {
		t.Error("nothing may land at the artist root through the symlink")
	}
}

func TestApplyExtraFanart_ForgedEntriesAreBlockedWithErr(t *testing.T) {
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart/a.jpg": "a", "elsewhere/x.jpg": "x"})
	before := inventory(t, dir)
	mk := func(src, dst string) MigrationEntry {
		return MigrationEntry{Source: src, Dest: dst, Disposition: DispositionMove}
	}
	plan := &ExtraFanartPlan{ArtistDir: dir, Entries: []MigrationEntry{
		mk(filepath.Join(dir, "elsewhere", "x.jpg"), filepath.Join(dir, "fanart2.jpg")),          // source outside extrafanart/
		mk(filepath.Join(dir, "extrafanart", "a.jpg"), filepath.Join(dir, "elsewhere", "y.jpg")), // dest outside the root
		mk(filepath.Join(dir, "extrafanart", ".."), filepath.Join(dir, "fanart3.jpg")),           // dot-dot base
	}}
	res, err := ApplyExtraFanartMigration(context.Background(), &fakeHashInvalidator{}, "a1", plan)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res.Results {
		if r.Outcome != OutcomeBlocked || r.Err == nil {
			t.Errorf("entry %d: %+v, want blocked with Err", i, r)
		}
	}
	wantInv(t, dir, before)
}

func TestApplyExtraFanart_NilInvalidatorMovesNothing(t *testing.T) {
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart/a.jpg": "a"})
	plan, _ := PlanExtraFanartMigration(context.Background(), dir, []string{"fanart.jpg"}, false)
	before := inventory(t, dir)
	if _, err := ApplyExtraFanartMigration(context.Background(), nil, "a1", plan); err == nil {
		t.Error("a nil invalidator must be rejected")
	}
	wantInv(t, dir, before)
}

func TestApplyExtraFanart_BothInvalidationsRunAndErrorsJoin(t *testing.T) {
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart/a.jpg": "a"})
	plan, _ := PlanExtraFanartMigration(context.Background(), dir, []string{"fanart.jpg"}, false)
	inv := &fakeHashInvalidator{err: errors.New("hash boom"), geomErr: errors.New("geom boom")}
	res, _ := ApplyExtraFanartMigration(context.Background(), inv, "a1", plan)
	if len(inv.geomCall) != 1 || res.InvalidErr == nil || !strings.Contains(res.InvalidErr.Error(), "hash boom") || !strings.Contains(res.InvalidErr.Error(), "geom boom") {
		t.Errorf("geometry calls=%d invalidErr=%v", len(inv.geomCall), res.InvalidErr)
	}
}

// cancelOnSecondErr wraps a REAL cancellable context and cancels it on the
// second Err call (after the first file moved), so Done and Err are truthful.
type cancelOnSecondErr struct {
	context.Context
	cancel context.CancelFunc
	n      int
}

func (c *cancelOnSecondErr) Err() error {
	if c.n++; c.n == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

// ctxHonoringInvalidator models the production invalidator (db.ExecContext):
// a canceled context clears nothing and returns the error.
type ctxHonoringInvalidator struct{ cleared int }

func (f *ctxHonoringInvalidator) InvalidateImageHashes(ctx context.Context, _, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.cleared++
	return nil
}

func (f *ctxHonoringInvalidator) InvalidateImageGeometry(ctx context.Context, _, _ string) error {
	return ctx.Err()
}

func TestApplyExtraFanart_CancelMidApplyStillInvalidatesWhatMoved(t *testing.T) {
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart/a.jpg": "a", "extrafanart/b.jpg": "b"})
	plan, _ := PlanExtraFanartMigration(context.Background(), dir, []string{"fanart.jpg"}, false)
	inv := &ctxHonoringInvalidator{}
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := ApplyExtraFanartMigration(&cancelOnSecondErr{Context: cctx, cancel: cancel}, inv, "a1", plan)
	if !errors.Is(err, context.Canceled) || res.Moved != 1 || inv.cleared != 1 || res.InvalidErr != nil || res.Dir != DirUntouched {
		t.Errorf("err=%v moved=%d cleared=%d invalidErr=%v dir=%q", err, res.Moved, inv.cleared, res.InvalidErr, res.Dir)
	}
}

func TestApplyExtraFanart_SourceGoneAloneDoesNotKeepTheDir(t *testing.T) {
	ctx := context.Background()
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart/b.jpg": "b"})
	requireExtraFanartCount(t, dir, 1)
	plan, _ := PlanExtraFanartMigration(ctx, dir, []string{"fanart.jpg"}, false)
	plan.Entries = append(plan.Entries, MigrationEntry{Source: filepath.Join(dir, "extrafanart", "z.jpg"),
		Dest: filepath.Join(dir, "fanart9.jpg"), Disposition: DispositionMove})
	res, _ := ApplyExtraFanartMigration(ctx, &fakeHashInvalidator{}, "a1", plan)
	if res.Moved != 1 || res.Dir != DirRemoved {
		t.Errorf("moved=%d dir=%q, want 1 and removed", res.Moved, res.Dir)
	}
	wantInv(t, dir, map[string]string{"fanart.jpg": "r", "fanart2.jpg": "b"})
}

func TestApplyExtraFanart_SourceSwappedForDirAfterPlanIsBlocked(t *testing.T) {
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart/a.jpg": "a"})
	requireExtraFanartCount(t, dir, 1)
	plan, _ := PlanExtraFanartMigration(context.Background(), dir, []string{"fanart.jpg"}, false)
	a := filepath.Join(dir, "extrafanart", "a.jpg")
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	res, _ := ApplyExtraFanartMigration(context.Background(), &fakeHashInvalidator{}, "a1", plan)
	if res.Moved != 0 || res.Results[0].Outcome != OutcomeBlocked || res.Results[0].Err == nil {
		t.Errorf("moved=%d results=%+v", res.Moved, res.Results)
	}
	if _, err := os.Lstat(filepath.Join(dir, "fanart2.jpg")); err == nil {
		t.Error("a directory must not be moved into the artist root")
	}
	wantInv(t, dir, map[string]string{"fanart.jpg": "r", "extrafanart": "<dir>", "extrafanart/a.jpg": "<dir>"})
}

func TestApplyExtraFanart_NilPlanIsRejected(t *testing.T) {
	if res, err := ApplyExtraFanartMigration(context.Background(), &fakeHashInvalidator{}, "a1", nil); err == nil || res != nil {
		t.Errorf("res=%v err=%v, want a nil result and an error", res, err)
	}
}

// Proves removeDirOnly's directory-only semantics: a regular file at the path
// survives (os.Remove would unlink it, rmdir refuses it). It does NOT guard
// the call site in ApplyExtraFanartMigration: the window between the
// emptiness check and the removal cannot be forced from a test, so reverting
// that call to os.Remove would not fail anything.
func TestRemoveDirOnly_NeverDeletesAFile(t *testing.T) {
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart": "operator data"})
	if err := removeDirOnly(filepath.Join(dir, extraFanartDir)); err == nil {
		t.Error("removing a regular file as a directory must fail")
	}
	wantInv(t, dir, map[string]string{"fanart.jpg": "r", "extrafanart": "operator data"})

	// The intended case still works: an empty real directory is removed.
	empty := seedTree(t, map[string]string{"fanart.jpg": "r"})
	if err := os.Mkdir(filepath.Join(empty, extraFanartDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeDirOnly(filepath.Join(empty, extraFanartDir)); err != nil {
		t.Fatal(err)
	}
	wantInv(t, empty, map[string]string{"fanart.jpg": "r"})
}

// lockDirInvalidator makes the artist dir read-only from inside the
// invalidation step, which runs after every move and before the emptied
// folder is removed: the moves succeed, the rmdir then fails.
type lockDirInvalidator struct {
	fakeHashInvalidator
	dir string
	t   *testing.T
}

func (l *lockDirInvalidator) InvalidateImageHashes(ctx context.Context, artistID, imageType string) error {
	if err := os.Chmod(l.dir, 0o555); err != nil {
		l.t.Fatal(err)
	}
	return l.fakeHashInvalidator.InvalidateImageHashes(ctx, artistID, imageType)
}

func TestApplyExtraFanart_EmptiedDirRemovalFailureIsReportedWithItsPath(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs directory permissions to be enforced (not root, not Windows)")
	}
	ctx := context.Background()
	dir := seedTree(t, map[string]string{"fanart.jpg": "r", "extrafanart/b.jpg": "b"})
	requireExtraFanartCount(t, dir, 1)
	plan, _ := PlanExtraFanartMigration(ctx, dir, []string{"fanart.jpg"}, false)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	res, err := ApplyExtraFanartMigration(ctx, &lockDirInvalidator{dir: dir, t: t}, "a1", plan)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, extraFanartDir)
	if res.Moved != 1 || res.Dir != DirKeptError || res.DirErr == nil {
		t.Fatalf("moved=%d dir=%q err=%v, want 1, kept-error and an error", res.Moved, res.Dir, res.DirErr)
	}
	if !strings.Contains(res.DirErr.Error(), want) {
		t.Errorf("DirErr = %v, want it to name %s", res.DirErr, want)
	}
	if errors.Unwrap(res.DirErr) == nil {
		t.Errorf("DirErr = %v, want the underlying cause wrapped", res.DirErr)
	}
	wantInv(t, dir, map[string]string{"fanart.jpg": "r", "fanart2.jpg": "b", "extrafanart": "<dir>"})
}
