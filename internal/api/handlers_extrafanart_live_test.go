package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rootHashes returns the content hashes of the regular files directly in dir
// and of those in dir/extrafanart, so a test can show a file arrived at the
// root byte-identical and left extrafanart/.
func rootHashes(t *testing.T, dir string) (root, extra map[string]bool) {
	t.Helper()
	read := func(d string) map[string]bool {
		out := map[string]bool{}
		entries, err := os.ReadDir(d)
		if err != nil {
			return out
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(d, e.Name()))
			if err != nil {
				t.Fatalf("reading %s: %v", e.Name(), err)
			}
			sum := sha256.Sum256(b)
			out[hex.EncodeToString(sum[:])] = true
		}
		return out
	}
	return read(dir), read(filepath.Join(dir, "extrafanart"))
}

// A live run moves every file with identical content, empties and removes
// extrafanart/, reports exact counts, and a second run finds nothing to do.
func TestExtraFanartMigration_LiveRunMovesFiles(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 3)
	b := seedExtraFanartArtist(t, svc, "Bravo", 2)
	_, wantA := rootHashes(t, a.dir)
	_, wantB := rootHashes(t, b.dir)

	// Precondition: the plan is non-empty and the files sit at the old paths.
	pre := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json"))
	if pre.Planned != 5 || len(wantA) != 3 || len(wantB) != 2 {
		t.Fatalf("precondition: want 5 planned and 3+2 files in extrafanart/, got planned=%d a=%d b=%d", pre.Planned, len(wantA), len(wantB))
	}

	w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
	res := decodeRun(t, w)
	if w.Code != http.StatusOK || res.DryRun || res.Status != "migrated" || res.Moved != 5 || res.Failed != 0 || res.Problems != 0 {
		t.Fatalf("want 200 migrated with 5 moved, 0 failed; got %d %+v", w.Code, res)
	}
	for _, tc := range []struct {
		art  efArtist
		want map[string]bool
	}{{a, wantA}, {b, wantB}} {
		root, extra := rootHashes(t, tc.art.dir)
		for h := range tc.want {
			if !root[h] {
				t.Errorf("%s: a file's content is missing from the artist folder after the move", tc.art.name)
			}
		}
		if len(extra) != 0 {
			t.Errorf("%s: extrafanart/ still holds files", tc.art.name)
		}
		if _, err := os.Stat(filepath.Join(tc.art.dir, "extrafanart")); !os.IsNotExist(err) {
			t.Errorf("%s: the emptied extrafanart/ directory should be removed (err %v)", tc.art.name, err)
		}
		if !root[hashOf("root-"+tc.art.name)] {
			t.Errorf("%s: the existing root backdrop was lost", tc.art.name)
		}
	}
	if strings.Contains(w.Body.String(), a.dir) {
		t.Error("the response leaked an artist path")
	}
	again := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json"))
	if again.Status != "nothing_to_do" || again.Moved != 0 {
		t.Errorf("second run: want nothing_to_do, got %+v", again)
	}
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A concurrent live run gets a 409 and moves nothing; the slot is free again
// after a run that failed (guard failure), so one failure cannot lock the
// operator out until a restart.
func TestExtraFanartMigration_LiveLockHeldAndReleased(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	before := inventory(t, a)

	r.extraFanartMu.Lock()
	r.extraFanartRunning = true
	r.extraFanartMu.Unlock()
	w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
	if res := decodeRun(t, w); w.Code != http.StatusConflict || res.Status != "running" || res.DryRun {
		t.Fatalf("want 409 running with dry_run=false, got %d %+v", w.Code, res)
	}
	if d := diffInventory(before, inventory(t, a)); len(d) > 0 {
		t.Fatalf("a refused live run changed the library: %v", d)
	}

	r.extraFanartMu.Lock()
	r.extraFanartRunning = false
	r.extraFanartMu.Unlock()
	if err := r.db.Close(); err != nil {
		t.Fatal(err)
	}
	if w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json"); w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 from the broken guard, got %d", w.Code)
	}
	r.extraFanartMu.Lock()
	held := r.extraFanartRunning
	r.extraFanartMu.Unlock()
	if held {
		t.Error("the singleton slot was not released after a failed live run")
	}
}

// A request context that is already canceled (a client that disconnected) does
// not stop or half-finish a live run: the run uses a detached context.
func TestExtraFanartMigration_LiveRunSurvivesRequestCancel(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 3)
	b := seedExtraFanartArtist(t, svc, "Bravo", 2)

	ctx, cancel := context.WithCancel(adminContext())
	cancel()
	res := decodeRun(t, postExtraFanart(r, ctx, `{"dry_run": false}`, "application/json"))
	if res.Status != "migrated" || res.Moved != 5 {
		t.Fatalf("a canceled request must not stop a live run; got %+v", res)
	}
	for _, art := range []efArtist{a, b} {
		if _, extra := rootHashes(t, art.dir); len(extra) != 0 {
			t.Errorf("%s: files left in extrafanart/ after a detached run", art.name)
		}
	}
}

// Server shutdown still stops a live run, MID-RUN: the shutdown fires after the
// first artist finished and before the second one's apply. The finished artist
// stays moved, the stranded files are run_stopped and counted as failed, the
// slot is freed, and a re-run (after the context is replaced, as a restart
// would) moves the rest.
func TestExtraFanartMigration_LiveRunStopsOnShutdown(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	b := seedExtraFanartArtist(t, svc, "Bravo", 3)
	r.extraFanartBeforeApply = func(id string) {
		if id == b.id {
			r.webhookShutdownCancel()
		}
	}

	w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
	res := decodeRun(t, w)
	if w.Code != http.StatusInternalServerError || res.Status != "partial" || res.Moved != 2 || res.Failed != 3 {
		t.Fatalf("want 500 partial with 2 moved, 3 failed; got %d %+v", w.Code, res)
	}
	var stopped int
	for _, ar := range res.Artists {
		for _, f := range ar.Files {
			if f.Outcome == "failed" && f.Reason == reasonRunStopped {
				stopped++
			}
		}
	}
	if stopped != 3 {
		t.Errorf("want 3 run_stopped files, got %d", stopped)
	}
	if _, extra := rootHashes(t, a.dir); len(extra) != 0 {
		t.Error("the artist finished before the shutdown should stay moved")
	}
	if _, extra := rootHashes(t, b.dir); len(extra) != 3 {
		t.Errorf("the stranded artist's files should still be in extrafanart/, got %d", len(extra))
	}
	r.extraFanartMu.Lock()
	held := r.extraFanartRunning
	r.extraFanartMu.Unlock()
	if held {
		t.Error("slot not released after shutdown")
	}

	r.extraFanartBeforeApply = nil
	r.webhookShutdownCtx, r.webhookShutdownCancel = context.WithCancel(context.Background())
	again := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json"))
	if again.Status != "migrated" || again.Moved != 3 {
		t.Errorf("re-run: want migrated with 3 moved, got %+v", again)
	}
}

// A source removed by someone else (folder still present) is reported as
// source_gone: not moved, not failed.
func TestExtraFanartMigration_LiveSourceGoneIsNeitherMovedNorFailed(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	r.extraFanartBeforeApply = func(string) {
		if err := os.Remove(filepath.Join(a.dir, "extrafanart", "img0.jpg")); err != nil {
			t.Errorf("removing source: %v", err)
		}
	}
	res := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json"))
	if res.Moved != 1 || res.Failed != 0 || res.Problems != 0 {
		t.Fatalf("want 1 moved, 0 failed, 0 problems; got %+v", res)
	}
	var gone int
	for _, f := range res.Artists[0].Files {
		if f.Outcome == "source_gone" {
			gone++
		}
	}
	if gone != 1 {
		t.Errorf("want exactly 1 source_gone file, got %d (%+v)", gone, res.Artists[0].Files)
	}
}

// A move the filesystem refuses (the artist folder turned read-only after
// planning) is an OutcomeFailed: counted as failed and a problem, never moved.
func TestExtraFanartMigration_LiveMoveFailureIsCounted(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 1)
	t.Cleanup(func() { _ = os.Chmod(a.dir, 0o755) })
	r.extraFanartBeforeApply = func(string) {
		if err := os.Chmod(a.dir, 0o555); err != nil {
			t.Errorf("chmod: %v", err)
		}
	}
	res := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json"))
	if res.Status != "failed" || res.Moved != 0 || res.Failed != 1 || res.Problems != 1 {
		t.Fatalf("want failed with 0 moved, 1 failed, 1 problem; got %+v", res)
	}
	if f := res.Artists[0].Files[0]; f.Outcome != "failed" || f.Reason != reasonMoveFailed {
		t.Errorf("want failed/%s, got %+v", reasonMoveFailed, f)
	}
}

// hookInvalidator models the hash store: fn runs where the engine invalidates,
// which is AFTER the files moved and BEFORE the emptied folder is removed, so a
// test can break the filesystem or block at exactly that point.
type hookInvalidator struct {
	fn func(ctx context.Context) error
}

func (h hookInvalidator) InvalidateImageHashes(ctx context.Context, _, _ string) error {
	return h.fn(ctx)
}
func (hookInvalidator) InvalidateImageGeometry(context.Context, string, string) error { return nil }

// Files that moved but whose stored hashes could not be cleared are reported:
// the artist carries index_refresh_failed and the run is partial, not migrated.
func TestExtraFanartMigration_LiveIndexRefreshFailureIsReported(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	r.fanartInvalidator = hookInvalidator{fn: func(context.Context) error { return errors.New("hash store unavailable") }}
	w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
	res := decodeRun(t, w)
	if w.Code != http.StatusMultiStatus || res.Status != "partial" || res.Moved != 2 || res.Problems != 1 || res.Artists[0].Error != reasonIndexRefresh {
		t.Fatalf("want 207 partial, 2 moved, 1 problem, %s; got %d %+v", reasonIndexRefresh, w.Code, res)
	}
}

// A folder that is present at plan time and gone at apply time (a mount that
// dropped mid-run) is that artist FAILING: counted, named with a fixed code,
// never skipped as missing and never counted as success. Other artists proceed.
func TestExtraFanartMigration_LiveMidPlanFolderLossIsReportedPerArtist(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	b := seedExtraFanartArtist(t, svc, "Bravo", 3)
	r.extraFanartBeforeApply = func(id string) {
		if id == b.id {
			if err := os.RemoveAll(b.dir); err != nil {
				t.Errorf("removing fixture folder: %v", err)
			}
		}
	}

	w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
	res := decodeRun(t, w)
	if w.Code != http.StatusMultiStatus || res.Status != "partial" || res.Moved != 2 || res.Failed != 3 || res.ArtistsSkippedMissing != 0 || res.Problems != 1 {
		t.Fatalf("want 207 partial: 2 moved, 3 failed, none skipped-as-missing, 1 problem; got %d %+v", w.Code, res)
	}
	var sawB bool
	for _, ar := range res.Artists {
		switch ar.ArtistID {
		case b.id:
			sawB = true
			if ar.Error != reasonFolderGone {
				t.Errorf("Bravo: want error %q, got %q", reasonFolderGone, ar.Error)
			}
			for _, f := range ar.Files {
				if f.Outcome != "failed" || f.Reason != reasonFolderGone {
					t.Errorf("Bravo file: want failed/%s, got %+v", reasonFolderGone, f)
				}
			}
		case a.id:
			if ar.Error != "" {
				t.Errorf("Alpha should be clean, got error %q", ar.Error)
			}
		}
	}
	if !sawB {
		t.Error("the artist whose folder vanished is not named in the response")
	}
	if _, extra := rootHashes(t, a.dir); len(extra) != 0 {
		t.Error("Alpha was not migrated")
	}
}

// A file whose destination fills up between plan and apply is refused, never
// overwritten, and counted as a failure while the rest of the artist moves.
func TestExtraFanartMigration_LiveOccupiedDestinationIsFailureNotOverwrite(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	plan := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json"))
	if plan.Planned != 2 {
		t.Fatalf("precondition: want 2 planned, got %+v", plan)
	}
	// Occupy the first planned destination (fanart1-style name) just before apply.
	var occupied string
	r.extraFanartBeforeApply = func(string) {
		occupied = filepath.Join(a.dir, "backdrop1.jpg")
		if err := os.WriteFile(occupied, []byte("operator-file"), 0o644); err != nil {
			t.Errorf("seeding occupant: %v", err)
		}
	}
	w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
	res := decodeRun(t, w)
	b, err := os.ReadFile(occupied)
	if err != nil || string(b) != "operator-file" {
		t.Fatalf("the operator's file was overwritten or removed: %q %v", b, err)
	}
	if w.Code != http.StatusMultiStatus || res.Failed != 1 || res.Moved != 1 || res.Status != "partial" {
		t.Errorf("want 207 with 1 moved and 1 failed (occupied destination), partial; got %d %+v", w.Code, res)
	}
}

func TestExtraFanartRunResult_FinishLive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   extraFanartRunResult
		want string
	}{
		{"moved", extraFanartRunResult{Moved: 2}, "migrated"},
		{"moved with problems", extraFanartRunResult{Moved: 2, Problems: 1}, "partial"},
		{"stopped after moving", extraFanartRunResult{Moved: 1, aborted: true}, "partial"},
		{"stopped before moving", extraFanartRunResult{aborted: true}, "failed"},
		{"only problems", extraFanartRunResult{Problems: 2}, "failed"},
		{"all skipped missing", extraFanartRunResult{ArtistsSkippedMissing: 1}, "nothing_checked"},
		{"nothing", extraFanartRunResult{}, "nothing_to_do"},
	} {
		res := tc.in
		res.finish()
		if res.Status != tc.want {
			t.Errorf("%s: want %s, got %s", tc.name, tc.want, res.Status)
		}
	}
}

// lockParent makes the artist's parent folder unsearchable (so a stat of the artist
// folder fails with permission denied) or read-only, restoring it at cleanup.
func lockParent(t *testing.T, a efArtist, mode os.FileMode) {
	t.Helper()
	parent := filepath.Dir(a.dir)
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	if err := os.Chmod(parent, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
}

// A stat that fails for a reason other than "not found" is NOT proof the folder
// vanished: the artist is reported folder_unreadable, not folder_unavailable.
func TestExtraFanartMigration_LiveUnreadableFolderIsNotReportedAsGone(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	// img1 is removed after planning (source_gone); after img0 moves, the parent
	// becomes unsearchable, so the folder check gets EACCES, not ENOENT.
	r.extraFanartBeforeApply = func(string) {
		if err := os.Remove(filepath.Join(a.dir, "extrafanart", "img1.jpg")); err != nil {
			t.Errorf("removing source: %v", err)
		}
	}
	r.fanartInvalidator = hookInvalidator{fn: func(context.Context) error { lockParent(t, a, 0o000); return nil }}
	res := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json"))
	if res.Moved != 1 || len(res.Artists) != 1 || res.Artists[0].Error != reasonFolderUnreadable {
		t.Fatalf("want 1 moved and error %s; got %+v", reasonFolderUnreadable, res)
	}
	if res.Problems != 1 || res.Status != "partial" {
		t.Errorf("want 1 problem and partial, got %+v", res)
	}
}

// Files moved but the emptied extrafanart/ folder could not be removed: the artist
// carries directory_not_removed and the run is partial, never migrated.
func TestExtraFanartMigration_LiveDirNotRemovedIsReported(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	t.Cleanup(func() { _ = os.Chmod(a.dir, 0o755) })
	r.fanartInvalidator = hookInvalidator{fn: func(context.Context) error {
		return os.Chmod(a.dir, 0o555) // files have moved; removing the emptied dir now fails
	}}
	res := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json"))
	if res.Moved != 2 || res.Status != "partial" || res.Problems != 1 || res.Artists[0].Error != reasonDirNotRemoved {
		t.Fatalf("want partial, 2 moved, 1 problem, %s; got %+v", reasonDirNotRemoved, res)
	}
}

// A hung invalidation cannot hold the run (and so the shutdown drain) open: it is
// bounded, and surfaces as index_refresh_failed.
func TestExtraFanartMigration_LiveHungInvalidationIsBounded(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 1)
	r.extraFanartInvalidateTimeout = 50 * time.Millisecond
	r.fanartInvalidator = hookInvalidator{fn: func(ctx context.Context) error {
		<-ctx.Done() // blocks until the bound fires; a missing bound hangs the test
		return ctx.Err()
	}}
	start := time.Now()
	done := make(chan extraFanartRunResult, 1)
	go func() {
		done <- decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json"))
	}()
	select {
	case res := <-done:
		if res.Status != "partial" || res.Artists[0].Error != reasonIndexRefresh {
			t.Errorf("want partial with %s, got %+v", reasonIndexRefresh, res)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the run did not return; the invalidation is unbounded (waited %s)", time.Since(start))
	}
}

// A stop keeps each unattempted entry's disposition: identical stays skipped,
// blocked stays blocked, and only the MOVE becomes run_stopped.
func TestExtraFanartMigration_LiveStopKeepsEntryDispositions(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 1) // img0.jpg: a move
	// Identical to the root backdrop: skipped. A symlink: blocked.
	if err := os.WriteFile(filepath.Join(a.dir, "extrafanart", "img1.jpg"), []byte("root-Alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(a.dir, "backdrop.jpg"), filepath.Join(a.dir, "extrafanart", "img2.jpg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	pre := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": true}`, "application/json"))
	if pre.Planned != 1 || pre.SkippedIdentical != 1 || pre.Problems != 1 {
		t.Fatalf("precondition: want 1 move, 1 identical, 1 blocked; got %+v", pre)
	}
	r.extraFanartBeforeApply = func(string) { r.webhookShutdownCancel() } // stop before any entry is applied

	res := decodeRun(t, postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json"))
	got := map[string]string{}
	for _, f := range res.Artists[0].Files {
		got[f.File] = f.Outcome + "/" + f.Reason
	}
	want := map[string]string{"img0.jpg": "failed/" + reasonRunStopped, "img1.jpg": "skipped/" + reasonIdenticalCopy, "img2.jpg": "blocked/" + reasonNotMovedSafe}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: want %s, got %s", k, v, got[k])
		}
	}
	if res.Moved != 0 || res.Failed != 2 || res.SkippedIdentical != 1 || res.Problems != 1 {
		t.Errorf("want 0 moved, 2 failed, 1 identical, 1 problem; got %+v", res)
	}
}

// The status code is derived from the outcome, never from recomputed counts.
func TestExtraFanartHTTPStatus(t *testing.T) {
	t.Parallel()
	boom := errors.New("stopped")
	cases := []struct {
		name   string
		dryRun bool
		status string
		err    error
		want   int
	}{
		{"preview planned", true, "planned", nil, http.StatusOK},
		{"preview blocked is not a failure", true, "blocked", nil, http.StatusOK},
		{"preview nothing_checked", true, "nothing_checked", nil, http.StatusOK},
		{"preview nothing_to_do", true, "nothing_to_do", nil, http.StatusOK},
		{"live migrated", false, "migrated", nil, http.StatusOK},
		{"live nothing_to_do", false, "nothing_to_do", nil, http.StatusOK},
		{"live nothing_checked", false, "nothing_checked", nil, http.StatusOK},
		{"live partial", false, "partial", nil, http.StatusMultiStatus},
		{"live failed", false, "failed", nil, http.StatusMultiStatus},
		{"live stopped early", false, "partial", boom, http.StatusInternalServerError},
		{"preview stopped early", true, "failed", boom, http.StatusInternalServerError},
		{"busy live", false, "running", errExtraFanartRunning, http.StatusConflict},
		{"busy preview", true, "running", errExtraFanartRunning, http.StatusConflict},
	}
	for _, tc := range cases {
		res := &extraFanartRunResult{DryRun: tc.dryRun, Status: tc.status}
		if got := extraFanartHTTPStatus(res, tc.err); got != tc.want {
			t.Errorf("%s: want %d, got %d", tc.name, tc.want, got)
		}
	}
}

// hashesOf returns every file's content hash in an inventory, so a test can show
// no file vanished wherever it moved to.
func hashesOf(inv map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, h := range inv {
		if h != "dir" && !strings.HasPrefix(h, "symlink->") {
			out[h] = true
		}
	}
	return out
}

// lockDir makes the artist folder refuse writes and PROVES it did, so the test
// cannot pass against a lock that silently bound nothing.
func lockDir(t *testing.T, a efArtist) {
	t.Helper()
	t.Cleanup(func() { _ = os.Chmod(a.dir, 0o755) })
	if err := os.Chmod(a.dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	probe := filepath.Join(a.dir, "probe.tmp")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err == nil {
		_ = os.Remove(probe)
		t.Fatalf("precondition: the locked folder %s still accepts writes", a.name)
	}
}

// A finished live run where one artist's folder refuses the move is 207
// partial: the other artist moved, the locked one is named, nothing vanished.
func TestExtraFanartMigration_PartialRunAnswers207(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	b := seedExtraFanartArtist(t, svc, "Bravo", 3)
	before := inventory(t, a, b)
	lockDir(t, b)

	w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
	res := decodeRun(t, w)
	if w.Code != http.StatusMultiStatus || res.Status != "partial" || res.Moved != 2 || res.Failed != 3 {
		t.Fatalf("want 207 partial with 2 moved, 3 failed; got %d %+v", w.Code, res)
	}
	var named int
	for _, ar := range res.Artists {
		for _, f := range ar.Files {
			if ar.ArtistID == b.id && f.Outcome == "failed" && f.File != "" {
				named++
			}
		}
	}
	if named != 3 {
		t.Errorf("want the 3 locked files named under the failed artist, got %d", named)
	}
	if _, extra := rootHashes(t, a.dir); len(extra) != 0 {
		t.Error("Alpha should have moved")
	}
	afterHashes := hashesOf(inventory(t, a, b))
	for h := range hashesOf(before) {
		if !afterHashes[h] {
			t.Errorf("a file vanished (hash %s)", h)
		}
	}
}

// Every due file failing is 207 failed (the run finished), not 200.
func TestExtraFanartMigration_AllFailedAnswers207(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	lockDir(t, a)
	w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
	if res := decodeRun(t, w); w.Code != http.StatusMultiStatus || res.Status != "failed" || res.Moved != 0 {
		t.Fatalf("want 207 failed with 0 moved; got %d %+v", w.Code, res)
	}
}

// A clean run is 200 migrated; repeat runs are 200 nothing_to_do and change nothing.
func TestExtraFanartMigration_CleanRunAnswers200ThenIdempotent(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	a := seedExtraFanartArtist(t, svc, "Alpha", 2)
	w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
	if res := decodeRun(t, w); w.Code != http.StatusOK || res.Status != "migrated" || res.Moved != 2 {
		t.Fatalf("want 200 migrated with 2 moved; got %d %+v", w.Code, res)
	}
	settled := inventory(t, a)
	for i := 0; i < 3; i++ {
		w := postExtraFanart(r, adminContext(), `{"dry_run": false}`, "application/json")
		if res := decodeRun(t, w); w.Code != http.StatusOK || res.Status != "nothing_to_do" {
			t.Fatalf("repeat %d: want 200 nothing_to_do; got %d %+v", i, w.Code, res)
		}
		if d := diffInventory(settled, inventory(t, a)); len(d) != 0 {
			t.Fatalf("repeat %d changed the library: %v", i, d)
		}
	}
}

// A live run that outlives the server's WriteTimeout still delivers its receipt,
// because the handler extends the write deadline.
func TestExtraFanartMigration_ReceiptSurvivesWriteTimeout(t *testing.T) {
	t.Parallel()
	r, svc := testRouterForBackdrops(t)
	seedExtraFanartArtist(t, svc, "Alpha", 2)
	r.extraFanartBeforeApply = func(string) { time.Sleep(600 * time.Millisecond) }
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.handleExtraFanartMigrationRun(w, req.WithContext(adminContext()))
	}))
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(`{"dry_run": false}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("the receipt was lost to the write timeout: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(body), `"status":"migrated"`) {
		t.Fatalf("want the full migrated body, got %q (err %v)", body, err)
	}
}

// The write deadline must outlast the run limit plus the run's bounded tail, yet
// stay finite (a zero time would mean no deadline at all).
func TestExtraFanartWriteDeadline(t *testing.T) {
	t.Parallel()
	now := time.Now()
	d := extraFanartWriteDeadline(now)
	if d.IsZero() {
		t.Fatal("deadline must be set")
	}
	if min := now.Add(extraFanartRunTimeout + 2*extraFanartInvalidateTimeout); d.Before(min) {
		t.Errorf("deadline %v is shorter than the run limit plus two invalidations (%v)", d.Sub(now), min.Sub(now))
	}
	if max := now.Add(extraFanartRunTimeout + 10*time.Minute); d.After(max) {
		t.Errorf("deadline %v exceeds the sane cap %v", d.Sub(now), max.Sub(now))
	}
}
