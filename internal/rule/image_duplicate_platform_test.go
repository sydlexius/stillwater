package rule

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/publish"
)

// fakePlatformPruner models the PEER'S STATE, not just the call: each
// connection holds an ordered backdrop list whose labels share a first letter
// when they are "the same picture". A delete RE-INDEXES every higher slot
// down by one and a read of an out-of-range slot ERRORS, as Emby does, so a
// caller that trusted stale indices would see failures rather than a pass.
type fakePlatformPruner struct {
	conns map[string][]string
	// failAfter, when >= 0, makes the run return err after that many deletes.
	failAfter int
	err       error
	// badRead makes every pre-delete read fail, as a peer that drops the
	// connection does: a Failure is recorded and nothing is deleted.
	badRead bool

	calls       int
	gotOpts     []publish.ArtistBackdropPruneOptions
	localAtCall []string // fanart file names on disk when the prune ran
}

func newFakePlatform(conns map[string][]string) *fakePlatformPruner {
	return &fakePlatformPruner{conns: conns, failAfter: -1}
}

func (f *fakePlatformPruner) read(conn string, i int) (string, error) {
	l := f.conns[conn]
	if f.badRead {
		return "", fmt.Errorf("read slot %d: connection reset", i)
	}
	if i < 0 || i >= len(l) {
		return "", fmt.Errorf("index %d out of range (have %d)", i, len(l))
	}
	return l[i], nil
}

func (f *fakePlatformPruner) PrunePlatformBackdropsForArtist(_ context.Context, a *artist.Artist, opts publish.ArtistBackdropPruneOptions) (publish.PlatformBackdropPruneResult, error) {
	f.calls++
	f.gotOpts = append(f.gotOpts, opts)
	entries, _ := os.ReadDir(a.Path)
	f.localAtCall = nil
	for _, e := range entries {
		f.localAtCall = append(f.localAtCall, e.Name())
	}
	res := publish.PlatformBackdropPruneResult{ArtistsProcessed: 1}
	ids := make([]string, 0, len(f.conns))
	for id := range f.conns {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		// Detect: survivor is the lowest slot of each picture group.
		first := map[byte]int{}
		var doomed []int
		for i, label := range f.conns[id] {
			if s, ok := first[label[0]]; ok {
				doomed = append(doomed, i)
				res.Plan = append(res.Plan, publish.PlatformBackdropPrunePlanEntry{ConnectionID: id, Index: i, Survivor: s, Tier: publish.PruneTierPerceptual, Outcome: publish.PrunePlanPlanned})
			} else {
				first[label[0]] = i
			}
		}
		// Delete high-index-first, re-verifying each slot before it goes.
		for k := len(doomed) - 1; k >= 0; k-- {
			if f.failAfter >= 0 && res.BackdropsRemoved >= f.failAfter {
				return res, f.err
			}
			i := doomed[k]
			if _, err := f.read(id, i); err != nil {
				res.Failures = append(res.Failures, publish.PlatformBackdropPruneFailure{ArtistID: a.ID, ConnectionID: id, Err: err.Error()})
				break
			}
			l := f.conns[id]
			f.conns[id] = append(l[:i:i], l[i+1:]...)
			res.BackdropsRemoved++
			for p := range res.Plan {
				if res.Plan[p].ConnectionID == id && res.Plan[p].Index == i {
					res.Plan[p].Outcome = publish.PrunePlanDeleted
				}
			}
		}
	}
	return res, nil
}

// dupFixture builds a real artist folder with three fanart slots, two of them
// the same picture, so the LOCAL phase has exactly one file to delete.
func dupFixture(t *testing.T, id string) (*ImageDuplicateFixer, *artist.Artist) {
	t.Helper()
	f, a, _ := dupFixtureDB(t, id, testLogger())
	return f, a
}

func dupFixtureDB(t *testing.T, id string, logger *slog.Logger) (*ImageDuplicateFixer, *artist.Artist, *sql.DB) {
	t.Helper()
	db := setupTestDB(t)
	insertTestArtist(t, db, id, "Platform Dup Artist")
	for i := 1; i <= 2; i++ {
		insertTestImage(t, db, id, "fanart", i)
	}
	dir := t.TempDir()
	createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
	createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1)
	createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1)
	f := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), &fakeHashRecorder{}, logger)
	a := &artist.Artist{ID: id, Name: "Platform Dup Artist", Path: dir, LibraryID: "lib-test", FanartExists: true, FanartCount: 3}
	return f, a, db
}

func wire(f *ImageDuplicateFixer, p *fakePlatformPruner) *int {
	n := 0
	f.SetPlatformPruner(p, func() { n++ })
	return &n
}

func dupViolation(prune bool, tol float64) *Violation {
	return &Violation{RuleID: RuleImageDuplicate, Config: RuleConfig{Tolerance: tol, PrunePlatformCopies: prune}}
}

func TestDupPlatform_OptionOff_NoPlatformCall(t *testing.T) {
	f, a := dupFixture(t, "art-off")
	p := newFakePlatform(map[string][]string{"c1": {"A1", "A2"}})
	invalidations := wire(f, p)

	res, err := f.Fix(t.Context(), a, dupViolation(false, 0.90))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed || res.SlotsRemoved != 1 {
		t.Fatalf("local phase changed: Fixed=%v SlotsRemoved=%d msg=%q", res.Fixed, res.SlotsRemoved, res.Message)
	}
	if p.calls != 0 || *invalidations != 0 {
		t.Fatalf("option off: platform calls = %d, invalidations = %d, want 0/0", p.calls, *invalidations)
	}
	if strings.Contains(res.Message, "platform") || res.Irreversible {
		t.Errorf("option off must be today's result exactly; got msg=%q irreversible=%v", res.Message, res.Irreversible)
	}
	if len(p.conns["c1"]) != 2 {
		t.Errorf("platform state changed with the option off: %v", p.conns["c1"])
	}
}

// The option belongs to the perceptual rule only: the exact rule (and the
// fanart repair that drives it) never reaches the platform.
func TestDupPlatform_ExactRuleNeverPrunesPlatform(t *testing.T) {
	f, a := dupFixture(t, "art-exact")
	p := newFakePlatform(map[string][]string{"c1": {"A1", "A2"}})
	wire(f, p)
	v := dupViolation(true, 0.90)
	v.RuleID = RuleImageDuplicateExact
	if _, err := f.Fix(t.Context(), a, v); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if p.calls != 0 {
		t.Fatalf("exact rule reached the platform: calls = %d", p.calls)
	}
}

func TestDupPlatform_DiscoveryOnlyNeverPrunes(t *testing.T) {
	f, a := dupFixture(t, "art-disc")
	p := newFakePlatform(map[string][]string{"c1": {"A1", "A2"}})
	wire(f, p)
	v := dupViolation(true, 0.90)
	v.Config.DiscoveryOnly = true
	if _, err := f.Fix(t.Context(), a, v); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if p.calls != 0 {
		t.Fatalf("discovery-only pass reached the platform: calls = %d", p.calls)
	}
}

func TestDupPlatform_OptionOn_LocalThenPlatform(t *testing.T) {
	f, a := dupFixture(t, "art-on")
	// Survivor A1 at slot 0; A2 and A3 are near-duplicates of it, B1 distinct.
	p := newFakePlatform(map[string][]string{"c1": {"A1", "A2", "B1", "A3"}, "c2": {"C1"}})
	invalidations := wire(f, p)

	res, err := f.Fix(t.Context(), a, dupViolation(true, 0.90))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	// Local phase first: the platform saw the folder AFTER the local delete.
	if res.SlotsRemoved != 1 || !res.RemovedFiles || !res.Fixed {
		t.Fatalf("local result lost: %+v", res)
	}
	if got := strings.Join(p.localAtCall, ","); got != "fanart.jpg,fanart2.jpg" {
		t.Fatalf("platform phase saw local folder %q, want post-local fanart.jpg,fanart2.jpg", got)
	}
	if p.calls != 1 || !p.gotOpts[0].Perceptual || p.gotOpts[0].DryRun || p.gotOpts[0].Tolerance != 0.90 {
		t.Fatalf("pruner calls=%d opts=%+v", p.calls, p.gotOpts)
	}
	if got := strings.Join(p.conns["c1"], ","); got != "A1,B1" {
		t.Errorf("platform c1 = %s, want A1,B1 (survivor kept, distinct kept)", got)
	}
	if !strings.Contains(res.Message, "connection c1 removed 2 backdrop(s), kept slot(s) 0") {
		t.Errorf("message lacks the per-connection audit: %q", res.Message)
	}
	if strings.Contains(res.Message, "connection c2") {
		t.Errorf("message claims deletes on an untouched connection: %q", res.Message)
	}
	if !res.Irreversible {
		t.Error("Irreversible = false after a platform delete; undo would be offered")
	}
	if *invalidations != 1 {
		t.Errorf("cache invalidations = %d, want 1 (#3092)", *invalidations)
	}
}

// A clean local folder plus a platform delete is a completed fix, and a
// platform with nothing to remove leaves the result untouched (no undo
// suppression, no invalidation).
func TestDupPlatform_LocalCleanPlatformOutcome(t *testing.T) {
	f, a := dupFixture(t, "art-clean")
	if err := os.Remove(filepath.Join(a.Path, "fanart3.jpg")); err != nil {
		t.Fatal(err)
	}
	p := newFakePlatform(map[string][]string{"c1": {"A1", "A2"}})
	invalidations := wire(f, p)
	res, err := f.Fix(t.Context(), a, dupViolation(true, 0.90))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !res.Fixed || !res.Irreversible || res.RemovedFiles || *invalidations != 1 {
		t.Fatalf("clean local + platform delete: %+v invalidations=%d", res, *invalidations)
	}

	p2 := newFakePlatform(map[string][]string{"c1": {"A1", "B1"}})
	inv2 := wire(f, p2)
	res, err = f.Fix(t.Context(), a, dupViolation(true, 0.90))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if res.Fixed || res.Irreversible || *inv2 != 0 || !strings.Contains(res.Message, "removed 0 backdrop(s)") {
		t.Fatalf("nothing to prune: %+v invalidations=%d", res, *inv2)
	}
}

func TestDupPlatform_ErrorFoldedIntoMessage(t *testing.T) {
	f, a := dupFixture(t, "art-err")
	p := newFakePlatform(map[string][]string{"c1": {"A1", "A2", "A3"}})
	p.failAfter = 1
	p.err = errors.New("emby went away")
	invalidations := wire(f, p)

	res, err := f.Fix(t.Context(), a, dupViolation(true, 0.90))
	if err != nil {
		t.Fatalf("platform error escaped as a Go error (would skip Update/reconcile): %v", err)
	}
	if res == nil || !res.Fixed || res.SlotsRemoved != 1 || !res.RemovedFiles {
		t.Fatalf("local result not intact: %+v", res)
	}
	if !strings.Contains(res.Message, "removed 1 duplicate fanart file(s)") {
		t.Errorf("local part of message lost: %q", res.Message)
	}
	if !strings.Contains(res.Message, "error: emby went away") {
		t.Errorf("message does not carry the platform error: %q", res.Message)
	}
	if !strings.Contains(res.Message, "connection c1 removed 1 backdrop(s)") || !res.Irreversible {
		t.Errorf("partial platform delete not recorded: %q irreversible=%v", res.Message, res.Irreversible)
	}
	if *invalidations != 1 {
		t.Errorf("partial delete must still invalidate caches; got %d", *invalidations)
	}
}

func TestDupPlatform_ToleranceZeroNormalized(t *testing.T) {
	f, a := dupFixture(t, "art-tol0")
	p := newFakePlatform(map[string][]string{"c1": {"A1", "A2"}})
	wire(f, p)
	if _, err := f.Fix(t.Context(), a, dupViolation(true, 0)); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if p.calls != 1 || p.gotOpts[0].Tolerance != img.DefaultDuplicateTolerance {
		t.Fatalf("tolerance 0 not normalized: calls=%d opts=%+v", p.calls, p.gotOpts)
	}
}

func TestDupPlatform_ToleranceFloor(t *testing.T) {
	for _, tc := range []struct {
		tol     float64
		allowed bool
	}{
		{0.5, false},
		{0.8499, false},
		{platformPruneToleranceFloor, true},
		{1.0, true},
		{1.5, false},
	} {
		t.Run(fmt.Sprint(tc.tol), func(t *testing.T) {
			f, a := dupFixture(t, "art-floor")
			p := newFakePlatform(map[string][]string{"c1": {"A1", "A2"}})
			wire(f, p)
			res, err := f.Fix(t.Context(), a, dupViolation(true, tc.tol))
			if err != nil {
				t.Fatalf("Fix: %v", err)
			}
			if tc.allowed {
				if p.calls != 1 || p.gotOpts[0].Tolerance != tc.tol {
					t.Fatalf("tolerance %v refused: calls=%d msg=%q", tc.tol, p.calls, res.Message)
				}
				return
			}
			if p.calls != 0 {
				t.Fatalf("tolerance %v reached the platform", tc.tol)
			}
			if !strings.Contains(res.Message, "platform: refused") {
				t.Errorf("refusal not reported: %q", res.Message)
			}
			if len(p.conns["c1"]) != 2 {
				t.Errorf("platform changed: %v", p.conns["c1"])
			}
		})
	}
	if platformPruneToleranceFloor != 0.85 {
		t.Fatalf("floor = %v, want 0.85", platformPruneToleranceFloor)
	}
}

func TestDupPlatform_ExcludedArtistFixerStaysLocal(t *testing.T) {
	f, a := dupFixture(t, "art-excl")
	a.IsExcluded = true
	p := newFakePlatform(map[string][]string{"c1": {"A1", "A2"}})
	wire(f, p)
	res, err := f.Fix(t.Context(), a, dupViolation(true, 0.90))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if p.calls != 0 {
		t.Fatalf("excluded artist reached the platform")
	}
	if !strings.Contains(res.Message, "artist is excluded") {
		t.Errorf("message = %q", res.Message)
	}
}

func TestDupPlatform_UnwiredRefuses(t *testing.T) {
	f, a := dupFixture(t, "art-unwired")
	res, err := f.Fix(t.Context(), a, dupViolation(true, 0.90))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !strings.Contains(res.Message, "not available") || res.Irreversible {
		t.Fatalf("unwired: %+v", res)
	}
	f.SetPlatformPruner(newFakePlatform(nil), nil)
	res, _ = f.Fix(t.Context(), a, dupViolation(true, 0.90))
	if !strings.Contains(res.Message, "not available") {
		t.Fatalf("pruner without invalidation hook must refuse: %q", res.Message)
	}
}

// TestDupPlatform_PipelineAuto drives the real pipeline in AUTO mode (the
// maintainer allowed unattended platform deletes once the option is on) and
// proves an excluded artist is skipped by the pipeline before any fixer runs.
func TestDupPlatform_PipelineAuto(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		t.Run(fmt.Sprintf("excluded=%v", excluded), func(t *testing.T) {
			ctx := context.Background()
			db := setupTestDB(t)
			artistSvc := artist.NewService(db)
			ruleSvc := NewService(db)
			if err := ruleSvc.SeedDefaults(ctx); err != nil {
				t.Fatal(err)
			}
			r, err := ruleSvc.GetByID(ctx, RuleImageDuplicate)
			if err != nil {
				t.Fatal(err)
			}
			r.Enabled = true
			r.AutomationMode = AutomationModeAuto
			r.Config.PrunePlatformCopies = true
			if err := ruleSvc.Update(ctx, r); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			createGradientJPEG(t, filepath.Join(dir, "fanart.jpg"), 0)
			createGradientJPEG(t, filepath.Join(dir, "fanart2.jpg"), 1)
			createGradientJPEG(t, filepath.Join(dir, "fanart3.jpg"), 1)
			a := &artist.Artist{Name: "Pipeline Dup", SortName: "Pipeline Dup", Path: dir, LibraryID: "lib-test", IsExcluded: excluded}
			if excluded {
				a.ExclusionReason = "test"
			}
			if err := artistSvc.Create(ctx, a); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				insertTestImage(t, db, a.ID, "fanart", i)
			}
			engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
			engine.SetImageHashRecorder(artistSvc)
			fixer := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), artistSvc, testLogger())
			p := newFakePlatform(map[string][]string{"c1": {"A1", "A2"}})
			wire(fixer, p)
			pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

			rr, err := pipeline.RunForArtist(ctx, a)
			if err != nil {
				t.Fatalf("RunForArtist: %v", err)
			}
			for _, x := range rr.Results {
				t.Logf("result: %+v", x)
			}
			_, statErr := os.Stat(filepath.Join(dir, "fanart3.jpg"))
			if excluded {
				if p.calls != 0 || statErr != nil {
					t.Fatalf("excluded artist was fixed: platform calls=%d, fanart3 stat=%v", p.calls, statErr)
				}
				return
			}
			// Positive control: the same harness DOES reach both phases.
			if p.calls != 1 || !os.IsNotExist(statErr) {
				t.Fatalf("auto run: platform calls=%d, fanart3 stat=%v", p.calls, statErr)
			}
		})
	}
}

// N1: when the local phase never reached detection there is no local
// judgment, so the platform must not be touched (the publisher would still
// run its exact tier on an artist the rule never looked at).
func TestDupPlatform_LocalNotRunNeverReachesPlatform(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *ImageDuplicateFixer, a *artist.Artist)
		want  string
	}{
		{"shared filesystem", func(f *ImageDuplicateFixer, _ *artist.Artist) {
			f.fsCheck = NewSharedFSCheck(&stubLibQuerier{lib: &library.Library{SharedFSStatus: library.SharedFSConfirmed}}, testLogger())
		}, "shared-filesystem"},
		{"no path", func(_ *ImageDuplicateFixer, a *artist.Artist) { a.Path = "" }, "artist has no path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, a := dupFixture(t, "art-notrun")
			tc.setup(f, a)
			p := newFakePlatform(map[string][]string{"c1": {"A1", "A2"}})
			wire(f, p)
			res, err := f.Fix(t.Context(), a, dupViolation(true, 0.90))
			if err != nil {
				t.Fatalf("Fix: %v", err)
			}
			if !strings.Contains(res.Message, tc.want) {
				t.Fatalf("precondition: local phase should bail with %q; got %q", tc.want, res.Message)
			}
			if p.calls != 0 {
				t.Fatalf("local phase never ran detection, yet the platform was pruned (calls=%d, msg=%q)", p.calls, res.Message)
			}
		})
	}
}

// N2: a run that failed without deleting still invalidates the caches, as
// the prune handler's partial-failure branch does.
func TestDupPlatform_FailureOnlyRunInvalidates(t *testing.T) {
	f, a := dupFixture(t, "art-failonly")
	p := newFakePlatform(map[string][]string{"c1": {"A1", "A2"}})
	p.badRead = true
	invalidations := wire(f, p)
	res, err := f.Fix(t.Context(), a, dupViolation(true, 0.90))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if p.calls != 1 || !strings.Contains(res.Message, "connection c1 failed") || res.Irreversible {
		t.Fatalf("precondition: one failed, delete-free run; calls=%d msg=%q irreversible=%v", p.calls, res.Message, res.Irreversible)
	}
	if *invalidations != 1 {
		t.Fatalf("failure-only platform run did not invalidate caches (got %d)", *invalidations)
	}
}

// N3: a local phase blocked by protected slots leaves its duplicates on disk,
// so a platform delete must not mark the fix Fixed (that would resolve a
// violation that is still true).
func TestDupPlatform_BlockedLocalNeverFixed(t *testing.T) {
	f, a, db := dupFixtureDB(t, "art-blocked", testLogger())
	if _, err := db.ExecContext(t.Context(),
		`UPDATE artist_images SET locked = 1 WHERE artist_id = ? AND image_type = 'fanart' AND slot_index = 2`, a.ID); err != nil {
		t.Fatal(err)
	}
	p := newFakePlatform(map[string][]string{"c1": {"A1", "A2"}})
	wire(f, p)
	res, err := f.Fix(t.Context(), a, dupViolation(true, 0.90))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !strings.Contains(res.Message, "locked or user-set") || p.calls != 1 || !res.Irreversible {
		t.Fatalf("precondition: blocked local + platform delete; calls=%d msg=%q irreversible=%v", p.calls, res.Message, res.Irreversible)
	}
	if res.Fixed {
		t.Fatalf("blocked local phase marked Fixed after a platform delete; its duplicates are still on disk: %q", res.Message)
	}
	if _, statErr := os.Stat(filepath.Join(a.Path, "fanart3.jpg")); statErr != nil {
		t.Errorf("protected local slot was deleted: %v", statErr)
	}
}

// N4: each platform delete leaves one structured audit log line.
func TestDupPlatform_PerDeleteAuditLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f, a, _ := dupFixtureDB(t, "art-audit", logger)
	p := newFakePlatform(map[string][]string{"c1": {"A1", "B1", "A2"}, "c2": {"C1", "C2"}})
	wire(f, p)
	if _, err := f.Fix(t.Context(), a, dupViolation(true, 0.90)); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	var got []string
	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil || m["msg"] != "duplicate-images rule deleted a platform backdrop" {
			continue
		}
		got = append(got, fmt.Sprintf("%v %v %v %v %v %v", m["artist_id"], m["connection_id"], m["index"], m["survivor"], m["tier"], m["tolerance"]))
	}
	want := []string{
		"art-audit c1 2 0 perceptual 0.9",
		"art-audit c2 1 0 perceptual 0.9",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("audit log lines = %q, want %q", got, want)
	}
}

// N6: a hook without a pruner refuses too.
func TestDupPlatform_NilPrunerRefuses(t *testing.T) {
	f, a := dupFixture(t, "art-nilpruner")
	hooked := 0
	f.SetPlatformPruner(nil, func() { hooked++ })
	res, err := f.Fix(t.Context(), a, dupViolation(true, 0.90))
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !strings.Contains(res.Message, "not available") || res.Irreversible || hooked != 0 {
		t.Fatalf("nil pruner: msg=%q irreversible=%v hook=%d", res.Message, res.Irreversible, hooked)
	}
}
