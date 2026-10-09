package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	img "github.com/sydlexius/stillwater/internal/image"
)

// efArtist is one fixture artist: its directory, plus the content it was built
// with so a test can assert what is still on disk afterwards.
type efArtist struct {
	name, id, dir string
}

// seedExtraFanartArtist creates an artist whose directory holds one root
// backdrop plus n distinct files under extrafanart/. It ASSERTS its own
// precondition (the directory exists with exactly n files) before returning, so
// no test below can pass against a fixture that silently built nothing.
func seedExtraFanartArtist(t *testing.T, svc *artist.Service, name string, n int) efArtist {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, "extrafanart"), 0o755); err != nil {
		t.Fatalf("creating fixture dir: %v", err)
	}
	// Distinct bytes per file: identity is the engine's sha256, so two files with
	// the same bytes would be (correctly) treated as one image.
	if err := os.WriteFile(filepath.Join(dir, "backdrop.jpg"), []byte("root-"+name), 0o644); err != nil {
		t.Fatalf("writing root backdrop: %v", err)
	}
	for i := 0; i < n; i++ {
		body := []byte(fmt.Sprintf("extra-%s-%d", name, i))
		if err := os.WriteFile(filepath.Join(dir, "extrafanart", fmt.Sprintf("img%d.jpg", i)), body, 0o644); err != nil {
			t.Fatalf("writing extrafanart file: %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "extrafanart"))
	if err != nil || len(entries) != n {
		t.Fatalf("precondition: extrafanart/ must hold %d files, got %d (err %v)", n, len(entries), err)
	}
	a := &artist.Artist{Name: name, SortName: name, Path: dir}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	return efArtist{name: name, id: a.ID, dir: dir}
}

// inventory is a full, comparable picture of the given artist directories:
// every file and directory (relative path), with a content hash for files. A
// move, rename, deletion or empty-directory removal all change it.
func inventory(t *testing.T, arts ...efArtist) map[string]string {
	t.Helper()
	inv := map[string]string{}
	for _, a := range arts {
		err := filepath.WalkDir(a.dir, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			rel := a.name + "/" + strings.TrimPrefix(strings.TrimPrefix(p, a.dir), "/")
			if d.IsDir() {
				inv[rel+"/"] = "dir"
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				target, _ := os.Readlink(p) // a retarget changes the inventory
				inv[rel] = "symlink->" + target
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			sum := sha256.Sum256(b)
			inv[rel] = hex.EncodeToString(sum[:])
			return nil
		})
		if err != nil {
			t.Fatalf("inventorying %s: %v", a.dir, err)
		}
	}
	return inv
}

// diffInventory names every entry that differs, so a failure says which file
// went missing rather than only that something did.
func diffInventory(before, after map[string]string) []string {
	var out []string
	for k, v := range before {
		if av, ok := after[k]; !ok {
			out = append(out, "missing: "+k)
		} else if av != v {
			out = append(out, "changed: "+k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			out = append(out, "new: "+k)
		}
	}
	sort.Strings(out)
	return out
}

func postExtraFanart(r *Router, ctx context.Context, body, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/reports/extrafanart-migration", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.handleExtraFanartMigrationRun(w, req)
	return w
}

func decodeRun(t *testing.T, w *httptest.ResponseRecorder) extraFanartRunResult {
	t.Helper()
	var res extraFanartRunResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decoding body %q: %v", w.Body.String(), err)
	}
	return res
}

// A dry run, in every spelling, writes nothing -- proven by inventory, never by
// the response's own dry_run claim -- and is repeatable.
func TestExtraFanartMigration_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 3)
	b := seedExtraFanartArtist(t, svc, "Bravo", 2)
	before := inventory(t, a, b)

	for _, tc := range []struct{ name, body, ct string }{
		{"json explicit", `{"dry_run": true}`, "application/json"},
		{"empty body defaults to dry", ``, "application/json"},
		{"form", `dry_run=true`, "application/x-www-form-urlencoded"},
		{"empty object defaults to dry", `{}`, "application/json"},
	} {
		// Sequential subtests (no t.Parallel): a Fatalf in one input no longer hides
		// the rest, and each case only reads the shared fixture and `before`.
		t.Run(tc.name, func(t *testing.T) {
			w := postExtraFanart(r, adminContext(), tc.body, tc.ct)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, body %s", w.Code, w.Body.String())
			}
			res := decodeRun(t, w)
			if !res.DryRun || res.Status != "planned" || res.Planned != 5 {
				t.Errorf("want dry_run planned with 5 planned; got %+v", res)
			}
			if d := diffInventory(before, inventory(t, a, b)); len(d) > 0 {
				t.Errorf("a dry run changed the library: %v", d)
			}
		})
	}
}

// While one preview holds the singleton slot, another request gets a 409 that
// still says it was a dry run.
func TestExtraFanartMigration_SingletonConflict(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	before := inventory(t, a)

	r.extraFanartMu.Lock()
	r.extraFanartRunning = true
	r.extraFanartMu.Unlock()

	const dry = true
	w := postExtraFanart(r, adminContext(), fmt.Sprintf(`{"dry_run": %t}`, dry), "application/json")
	res := decodeRun(t, w)
	if w.Code != http.StatusConflict || res.Status != "running" || res.DryRun != dry {
		t.Errorf("dry_run=%t: want 409 running echoing dry_run, got %d %+v", dry, w.Code, res)
	}
	if d := diffInventory(before, inventory(t, a)); len(d) > 0 {
		t.Errorf("a refused run changed the library: %v", d)
	}
}

// Every response that follows a parsed request echoes dry_run truthfully,
// including the guard failure ahead of the plan. Closing the database makes
// the platform-profile lookup fail before any plan exists.
func TestExtraFanartMigration_GuardFailureEchoesDryRun(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	before := inventory(t, a)
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing the database to break the guard: %v", err)
	}

	const dry = true
	w := postExtraFanart(r, adminContext(), fmt.Sprintf(`{"dry_run": %t}`, dry), "application/json")
	res := decodeRun(t, w)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("dry_run=%t: want 500 from the failed guard, got %d (%s)", dry, w.Code, w.Body.String())
	}
	if res.DryRun != dry {
		t.Errorf("dry_run=%t: the failed-guard body says dry_run=%t", dry, res.DryRun)
	}
	if res.Status != "failed" {
		t.Errorf("dry_run=%t: want status failed, got %q (a run that stopped before the plan is never partial)", dry, res.Status)
	}
	if d := diffInventory(before, inventory(t, a)); len(d) > 0 {
		t.Errorf("a failed guard changed the library: %v", d)
	}
}

// cancelOnWarn is a logger handler that cancels a context the first time anything
// is logged at Warn, then discards the record. It lets a test cancel a request
// from INSIDE the run, at a moment the test chooses, instead of before it starts.
type cancelOnWarn struct {
	slog.Handler
	cancel context.CancelFunc
}

func (h cancelOnWarn) Handle(_ context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelWarn {
		h.cancel()
	}
	return nil
}

// seedPlanFailArtist creates an artist whose extrafanart/ is a symlink, which
// the engine refuses to plan: a deterministic plan failure that logs a Warn.
func seedPlanFailArtist(t *testing.T, svc *artist.Service, name string) efArtist {
	t.Helper()
	a := seedExtraFanartArtist(t, svc, name, 0)
	if err := os.Remove(filepath.Join(a.dir, "extrafanart")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(a.dir, "extrafanart")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return a
}

// A canceled dry run stops. The cancel comes MID-RUN, from the Warn logged when
// the first (unplannable) artist fails to plan; one made up front would fail
// the profile lookup and never reach the check in the artist loop.
func TestExtraFanartMigration_CanceledDryRunStops(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedPlanFailArtist(t, svc, "Aaa Broken")
	a := seedExtraFanartArtist(t, svc, "Zeta", 2)
	before := inventory(t, a)
	ctx, cancel := context.WithCancel(adminContext())
	defer cancel()
	r.logger = slog.New(cancelOnWarn{Handler: slog.NewTextHandler(io.Discard, nil), cancel: cancel})
	w := postExtraFanart(r, ctx, `{"dry_run": true}`, "application/json")
	res := decodeRun(t, w)
	if w.Code != http.StatusInternalServerError || !res.DryRun || res.Status != "failed" {
		t.Errorf("a canceled dry run must stop with failed: got %d %+v", w.Code, res)
	}
	if res.Planned != 0 {
		t.Errorf("the run kept planning after the cancel: %+v", res)
	}
	if d := diffInventory(before, inventory(t, a)); len(d) > 0 {
		t.Errorf("a canceled dry run changed the library: %v", d)
	}
	// The singleton slot must be free again: a following preview is not a 409.
	next := postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json")
	if next.Code != http.StatusOK {
		t.Errorf("after a canceled run the next preview must succeed, got %d %s", next.Code, next.Body.String())
	}
}

// A cancel that lands while the LAST artist is being planned must not read as a
// finished preview: the check after each artist catches it before the loop ends.
func TestExtraFanartMigration_CancelDuringLastArtistStops(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedPlanFailArtist(t, svc, "Only")
	ctx, cancel := context.WithCancel(adminContext())
	defer cancel()
	r.logger = slog.New(cancelOnWarn{Handler: slog.NewTextHandler(io.Discard, nil), cancel: cancel})
	w := postExtraFanart(r, ctx, `{"dry_run": true}`, "application/json")
	if res := decodeRun(t, w); w.Code != http.StatusInternalServerError || res.Status != "failed" {
		t.Errorf("a cancel during the last artist must answer 500 failed, got %d %+v", w.Code, res)
	}
}

// Only administrators can run or even preview it, and a refused request moves
// nothing. A malformed dry_run is a 400, never a silent live run.
func TestExtraFanartMigration_AdminGateAndStrictFlag(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	before := inventory(t, a)

	operator := middleware.WithTestRole(middleware.WithTestUserID(context.Background(), "u1"), "operator")
	if w := postExtraFanart(r, operator, `{"dry_run": false}`, "application/json"); w.Code != http.StatusForbidden {
		t.Errorf("non-admin POST: want 403, got %d", w.Code)
	}

	for _, tc := range []struct{ body, ct string }{
		{`dry_run=maybe`, "application/x-www-form-urlencoded"},
		{`dry_run=`, "application/x-www-form-urlencoded"},
		{`{"dry_run": "no"}`, "application/json"},
		{`{"dry_run": null}`, "application/json"},
		{`null`, "application/json"},
		{`{"dry_run": false, "all_artists": true}`, "application/json"},
	} {
		if w := postExtraFanart(r, adminContext(), tc.body, tc.ct); w.Code != http.StatusBadRequest {
			t.Errorf("body %q: want 400, got %d", tc.body, w.Code)
		}
	}
	if d := diffInventory(before, inventory(t, a)); len(d) > 0 {
		t.Errorf("a refused or malformed request changed the library: %v", d)
	}
}

// F2: an artist whose own folder does not exist (unmounted share, stale row)
// has nothing to migrate and must not count as a problem on every run.
func TestExtraFanartMigration_MissingArtistFolderIsSkippedNotAProblem(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	ghost := &artist.Artist{Name: "Ghost", SortName: "Ghost", Path: filepath.Join(t.TempDir(), "unmounted", "Ghost")}
	if err := svc.Create(context.Background(), ghost); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ghost.Path); !os.IsNotExist(err) {
		t.Fatalf("precondition: the ghost artist's folder must not exist, stat err = %v", err)
	}

	dry := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json"))
	if dry.ArtistsSkippedMissing != 1 {
		t.Errorf("the skipped artist must be counted, got artists_skipped_missing=%d", dry.ArtistsSkippedMissing)
	}
	if dry.Problems != 0 || dry.Status != "planned" {
		t.Errorf("dry run: a missing artist folder must not be a problem, got %+v", dry)
	}
}

// A run in which no artist could be examined must not read as a verified-empty
// library: status nothing_checked, whether every artist is missing or the rest
// are genuinely empty.
func TestExtraFanartMigration_AllSkippedIsNothingChecked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		ghosts, real int
	}{{"only missing artists", 2, 0}, {"missing plus a genuinely empty artist", 1, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, svc := testRouterForBackdrops(t)
			for i := 0; i < tc.ghosts; i++ {
				name := fmt.Sprintf("Ghost%d", i)
				g := &artist.Artist{Name: name, SortName: name, Path: filepath.Join(t.TempDir(), "unmounted", name)}
				if err := svc.Create(context.Background(), g); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < tc.real; i++ {
				seedExtraFanartArtist(t, svc, fmt.Sprintf("Empty%d", i), 0)
			}
			res := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json"))
			if res.Status != "nothing_checked" || res.ArtistsSkippedMissing != tc.ghosts ||
				res.ArtistsScanned != tc.ghosts+tc.real || res.Planned != 0 {
				t.Errorf("want nothing_checked with %d skipped of %d scanned, got %+v", tc.ghosts, tc.ghosts+tc.real, res)
			}
		})
	}
}

// The folder check is bounded by the run context. With a canceled context and a
// plan error that says "not found", a folder that really is missing must NOT be
// counted as skipped: a context error is not "missing". (This proves the check
// honors ctx; it cannot model a mount that hangs, since a stat that blocks until
// cancel needs a stubbed filesystem the repo's helper does not offer.)
func TestExtraFanartMigration_CanceledFolderCheckIsNotMissing(t *testing.T) {
	t.Parallel()
	ghost := filepath.Join(t.TempDir(), "unmounted")
	if !artistFolderMissing(context.Background(), fs.ErrNotExist, ghost) {
		t.Fatal("precondition: with a live context a missing folder counts as missing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if artistFolderMissing(ctx, fs.ErrNotExist, ghost) {
		t.Error("a canceled folder check must not count the artist as skipped")
	}
}

// An artist whose Path is a symlink to a target that does not exist counts as
// missing: os.Stat follows the link, so the folder it names is not there.
func TestExtraFanartMigration_DanglingSymlinkArtistFolderIsSkipped(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	link := filepath.Join(t.TempDir(), "Linked")
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("precondition: the link itself must exist: %v", err)
	}
	if _, err := os.Stat(link); !os.IsNotExist(err) {
		t.Fatalf("precondition: the link must dangle, stat err = %v", err)
	}
	a := &artist.Artist{Name: "Linked", SortName: "Linked", Path: link}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	res := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json"))
	if res.ArtistsSkippedMissing != 1 || res.Problems != 0 {
		t.Errorf("a dangling-symlink artist folder must be skipped, not a problem; got %+v", res)
	}
}

// An artist folder that exists but whose extrafanart/ cannot be read stays a
// real problem: only a missing ARTIST folder is skipped.
func TestExtraFanartMigration_UnreadableExtrafanartIsStillAProblem(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the fixture cannot make extrafanart/ unreadable")
	}
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 1)
	sub := filepath.Join(a.dir, "extrafanart")
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if _, err := os.ReadDir(sub); err == nil {
		t.Fatal("precondition: extrafanart/ must be unreadable")
	}
	res := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json"))
	if res.Problems != 1 {
		t.Errorf("an unreadable extrafanart/ must be reported as a problem, got %+v", res)
	}
}

// R4: an artist folder that EXISTS but whose plan fails with a not-exist error
// from inside it (a dangling root symlink) is a real problem, never "skipped".
func TestExtraFanartMigration_NotExistInsideAnExistingFolderIsAProblem(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 1)
	root := filepath.Join(a.dir, "backdrop.jpg")
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(a.dir, "nowhere.jpg"), root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := os.Stat(a.dir); err != nil {
		t.Fatalf("precondition: the artist folder must exist: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("precondition: the root backdrop must be a dangling link, stat err = %v", err)
	}
	res := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json"))
	if res.Problems != 1 || res.ArtistsSkippedMissing != 0 {
		t.Errorf("a not-exist error inside an existing folder must be a problem, got %+v", res)
	}
}

// A failure's underlying error can carry paths and OS text. The JSON body must
// show none of it and must still name the artist and the file. A symlinked file
// makes the engine produce a per-file blocked reason, and an artist whose
// extrafanart/ is itself a symlink makes planning fail with a raw engine error.
func TestExtraFanartMigration_FailureTextIsGenericAndPathFree(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 1)
	bad := seedExtraFanartArtist(t, svc, "Bravo", 0)
	target := filepath.Join(t.TempDir(), "elsewhere.jpg")
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bad.dir, "extrafanart", "linked.jpg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Artist whose extrafanart is itself a symlink: planning refuses with an
	// error whose raw text is a path-bearing message.
	evil := seedExtraFanartArtist(t, svc, "Charlie", 0)
	if err := os.Remove(filepath.Join(evil.dir, "extrafanart")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(evil.dir, "extrafanart")); err != nil {
		t.Fatal(err)
	}
	// Precondition: the engine really does produce raw error text here, so the
	// "no raw text" assertions below are not vacuous.
	if _, perr := img.PlanExtraFanartMigration(context.Background(), evil.dir, []string{"backdrop.jpg"}, false); perr == nil ||
		!strings.Contains(perr.Error(), "not a real directory") {
		t.Fatalf("precondition: the engine must refuse the symlinked extrafanart/ with raw text, got %v", perr)
	}

	tmpRoots := []string{bad.dir, evil.dir, target, os.TempDir()}
	assertClean := func(label, body string) {
		for _, p := range tmpRoots {
			if strings.Contains(body, p) {
				t.Errorf("%s leaks path %q", label, p)
			}
		}
		for _, raw := range []string{"symlink?", "refusing", "no such file", "not a real directory", "operation not permitted"} {
			if strings.Contains(body, raw) {
				t.Errorf("%s leaks raw error text %q", label, raw)
			}
		}
		for _, want := range []string{"Bravo", "linked.jpg", "Charlie"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s no longer names %q", label, want)
			}
		}
	}
	w := postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json")
	assertClean("JSON preview", w.Body.String())
}

// The status derivation for a preview. A preview is never reported partial,
// however many problems it found.
func TestExtraFanartRunResult_Status(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		res  extraFanartRunResult
		want string
	}{
		{"aborted after planning", extraFanartRunResult{DryRun: true, Planned: 3, Problems: 1, aborted: true}, "failed"},
		{"only a blocked file", extraFanartRunResult{DryRun: true, Problems: 1}, "blocked"},
		{"moves and a blocked file", extraFanartRunResult{DryRun: true, Planned: 2, Problems: 1}, "planned"},
		{"nothing", extraFanartRunResult{DryRun: true}, "nothing_to_do"},
		{"all skipped", extraFanartRunResult{DryRun: true, ArtistsSkippedMissing: 2}, "nothing_checked"},
		{"skipped and a planned move", extraFanartRunResult{DryRun: true, Planned: 1, ArtistsSkippedMissing: 1}, "planned"},
		{"skipped and a problem", extraFanartRunResult{DryRun: true, Problems: 1, ArtistsSkippedMissing: 1}, "blocked"},
	} {
		res := tc.res
		res.finish()
		if res.Status != tc.want {
			t.Errorf("%s: status %q, want %q", tc.name, res.Status, tc.want)
		}
	}
}

// The three per-file/per-artist outcomes a preview reports, with their fixed
// reason codes: a byte-identical copy is skipped (and left in place), a symlinked
// file is blocked, and an artist whose extrafanart/ is itself a link fails to plan.
func TestExtraFanartMigration_OutcomesUseFixedReasonCodes(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	dup := seedExtraFanartArtist(t, svc, "Alpha", 0)
	if err := os.WriteFile(filepath.Join(dup.dir, "extrafanart", "dupe.jpg"), []byte("root-Alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := seedExtraFanartArtist(t, svc, "Bravo", 0)
	if err := os.Symlink(filepath.Join(dup.dir, "backdrop.jpg"), filepath.Join(linked.dir, "extrafanart", "linked.jpg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	broken := seedPlanFailArtist(t, svc, "Charlie")
	before := inventory(t, dup, linked, broken)

	res := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json"))
	got := map[string]string{}
	for _, a := range res.Artists {
		for _, f := range a.Files {
			got[a.Name+"/"+f.File] = f.Outcome + ":" + f.Reason
		}
		if a.Error != "" {
			got[a.Name] = a.Error
		}
	}
	want := map[string]string{
		"Alpha/dupe.jpg":   "skipped:" + reasonIdenticalCopy,
		"Bravo/linked.jpg": "blocked:" + reasonNotMovedSafe,
		"Charlie":          reasonPlanFailed,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if res.SkippedIdentical != 1 || res.Problems != 2 || res.Status != "blocked" {
		t.Errorf("want 1 identical, 2 problems, status blocked; got %+v", res)
	}
	if d := diffInventory(before, inventory(t, dup, linked, broken)); len(d) > 0 {
		t.Errorf("a preview changed the library (the identical copy must stay in place): %v", d)
	}
}

// cancelOnLog cancels a context on the first record of any level, so a test can
// cancel from the Info line logged when an artist is skipped.
type cancelOnLog struct {
	slog.Handler
	cancel context.CancelFunc
}

func (h cancelOnLog) Handle(context.Context, slog.Record) error {
	h.cancel()
	return nil
}

// A cancel that lands while a missing-folder artist is skipped must still end
// as a 500 failed, never a 200 with that artist counted as skipped: the context
// check runs after the artist, whichever branch the artist took.
func TestExtraFanartMigration_CancelWhileSkippingStillFails(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	ghost := &artist.Artist{Name: "Ghost", SortName: "Ghost", Path: filepath.Join(t.TempDir(), "unmounted")}
	if err := svc.Create(context.Background(), ghost); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(adminContext())
	defer cancel()
	r.logger = slog.New(cancelOnLog{Handler: slog.NewTextHandler(io.Discard, nil), cancel: cancel})
	w := postExtraFanart(r, ctx, `{"dry_run": true}`, "application/json")
	if res := decodeRun(t, w); w.Code != http.StatusInternalServerError || res.Status != "failed" {
		t.Errorf("a cancel during a skip must answer 500 failed, got %d %+v", w.Code, res)
	}
}

// Measures the preview at the issue's worst case, ~650 artists with several
// extrafanart files each, synchronously. The bound is loose: it only catches a
// pathological regression, and the logged time is the measurement.
func TestExtraFanartMigration_DryRunWallClock(t *testing.T) {
	r, svc := testRouterForBackdrops(t)
	const artists, perArtist = 650, 5
	for i := 0; i < artists; i++ {
		seedExtraFanartArtist(t, svc, fmt.Sprintf("Artist%03d", i), perArtist)
	}
	start := time.Now()
	res, err := r.previewExtraFanartMigration(context.Background(), false)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.ArtistsWithFiles != artists || res.Planned != artists*perArtist {
		t.Fatalf("precondition: want %d artists / %d planned moves, got %d / %d", artists, artists*perArtist, res.ArtistsWithFiles, res.Planned)
	}
	t.Logf("preview over %d artists x %d files (%d plan rows): %s", artists, perArtist, res.Planned, elapsed)
	if elapsed > 5*time.Second {
		t.Errorf("preview took %s; that is in the range where it should move off the request path", elapsed)
	}
}
