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

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
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
	} {
		w := postExtraFanart(r, adminContext(), tc.body, tc.ct)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d, body %s", tc.name, w.Code, w.Body.String())
		}
		res := decodeRun(t, w)
		if !res.DryRun || res.Status != "planned" || res.Planned != 5 {
			t.Errorf("%s: want dry_run planned with 5 planned; got %+v", tc.name, res)
		}
		if d := diffInventory(before, inventory(t, a, b)); len(d) > 0 {
			t.Errorf("%s: a dry run changed the library: %v", tc.name, d)
		}
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

// A canceled dry run stops. The cancel comes MID-RUN, from the Warn logged when
// the first (missing-folder) artist fails to plan; one made up front would fail
// the profile lookup and never reach the check in the artist loop.
func TestExtraFanartMigration_CanceledDryRunStops(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	broken := &artist.Artist{Name: "Aaa Broken", SortName: "Aaa Broken", Path: filepath.Join(t.TempDir(), "gone")}
	if err := svc.Create(context.Background(), broken); err != nil {
		t.Fatal(err)
	}
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

// A live request is refused before any work: a status the client cannot mistake
// for success, dry_run echoed as false, a fixed message, and the library untouched.
func TestExtraFanartMigration_LiveRequestIsRefused(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	before := inventory(t, a)
	// Hold the singleton: a refusal that tried to take it would answer 409.
	r.extraFanartMu.Lock()
	r.extraFanartRunning = true
	r.extraFanartMu.Unlock()
	for _, tc := range []struct{ body, ct string }{
		{`{"dry_run": false}`, "application/json"},
		{`dry_run=false`, "application/x-www-form-urlencoded"},
	} {
		w := postExtraFanart(r, adminContext(), tc.body, tc.ct)
		res := decodeRun(t, w)
		if w.Code != http.StatusServiceUnavailable || res.DryRun || res.Status != "failed" || res.Error == "" {
			t.Errorf("%q: want 503 failed with dry_run=false and a message, got %d %+v", tc.body, w.Code, res)
		}
		if strings.Contains(w.Body.String(), a.dir) {
			t.Errorf("%q: the refusal leaked a path", tc.body)
		}
	}
	if d := diffInventory(before, inventory(t, a)); len(d) > 0 {
		t.Errorf("a refused live request changed the library: %v", d)
	}
}
