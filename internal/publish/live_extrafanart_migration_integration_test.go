//go:build integration

package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
)

// Live proof for #3179: after the extrafanart/ migration, THREE consecutive real
// fanart pushes to a real Emby leave the artist's whole file inventory intact.
//
// OWN FIXTURE ARTIST. Emby keeps a folder-bound music artist only for a folder
// holding TAGGED audio in an album subfolder, so the sandbox is the sentinel
// plus exactly one album folder this test creates itself (proofAlbumDir, two
// generated tagged mp3s, nothing committed; left in place on exit so reruns are
// fast). The test scans the library, DISCOVERS the artist item by its synthetic
// name, and verifies its backdrop paths are in this folder before anything
// destructive (requireBound; the item Path is only logged). SW_LIVE_EMBY_ITEM_ID
// is not read at all; the item id is discovered.
//
// WHY A HOST DIRECTORY INSIDE EMBY'S LIBRARY. The #3174 loss is Emby deleting
// files it believes are backdrops. A t.TempDir() is invisible to Emby (see
// live_extrafanart_advisory_integration_test.go), so a green there proves
// nothing. SW_LIVE_EMBY_ARTIST_DIR must be the host path of the folder of the
// discovered music artist (in a disposable library). SAFETY: that directory
// must hold ONLY the sentinel and the proof's own album folder, else the test
// fatals before touching anything; on exit it is emptied back to those two.
//
// THE PUSH PATH is Publisher.SyncAllFanartToPlatforms: snapshot, upload set,
// repairAfterPush, the path #3174 measured as 2 -> 1 -> 0. Migration runs the
// engine functions the admin handler calls (see runExtrafanartMigration).
//
// Two tests, because a green alone is vacuous: ThreePushesLoseNothing (migrated)
// and ControlUnmigratedLoses (same sequence, no migration) which PASSES only when
// the loss is observed, so the script can require both.

const (
	proofPushes      = 3
	proofLiveTimeout = 5 * time.Minute
)

type proofRun struct {
	run, extra, emby int
	missing          []string
}

type extraProofRig struct {
	env    liveEmbyEnv
	client *emby.Client
	p      *Publisher
	art    *artist.Artist
	dir    string
}

func newExtraProofRig(ctx context.Context, t *testing.T) *extraProofRig {
	t.Helper()
	env := liveEmbyEnv{url: os.Getenv("SW_LIVE_EMBY_URL"), apiKey: os.Getenv("SW_LIVE_EMBY_API_KEY"), userID: os.Getenv("SW_LIVE_EMBY_USER_ID")}
	if env.url == "" || env.apiKey == "" || env.userID == "" {
		t.Skip("SW_LIVE_EMBY_URL / _API_KEY / _USER_ID not all set; skipping live extrafanart proof")
	}
	dir := os.Getenv("SW_LIVE_EMBY_ARTIST_DIR")
	if err := checkSandboxDir(dir); err != nil {
		t.Fatalf("LIVE-PROOF-SETUP: SW_LIVE_EMBY_ARTIST_DIR: %v (this test deletes what it finds on exit)", err)
	}
	if err := writeProofAlbum(filepath.Join(dir, proofAlbumDir)); err != nil {
		t.Fatalf("LIVE-PROOF-SETUP: writing the fixture album: %v", err)
	}
	logger := silentLogger()
	client := emby.New(env.url, env.apiKey, env.userID, logger)
	r := &extraProofRig{env: env, client: client, dir: dir}
	r.env.itemID = r.discoverItem(ctx, t)
	r.p = New(Deps{
		Logger: logger,
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "live-3179", ConnectionID: "c-emby", PlatformArtistID: r.env.itemID},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-emby": {ID: "c-emby", Name: "live-emby-uat", Type: connection.TypeEmby, URL: r.env.url, APIKey: r.env.apiKey, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: r.env.userID, FeatureImageWrite: true}},
		}},
	})
	r.art = &artist.Artist{ID: "live-3179", Name: "Live UAT Artist", Path: dir}
	// Leave the item as found: empty the sandbox first, THEN let Emby rescan and
	// drop its backdrops, so clearing them deletes no file of ours.
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		r.wipe(t)
		r.rescan(cctx, t, 0)
		r.clearIfProven(cctx, t, false)
	})
	// ORDER: the item was discovered by name only, so nothing destructive happens
	// until clearIfProven has shown it belongs to the sandbox (or has nothing to clear).
	r.clearIfProven(ctx, t, true)
	return r
}

// itemBound reports positive evidence that the discovered item is the sandbox's
// artist: it is credited on the proof's own album tracks (folder-independent, so it
// works with zero backdrops), or it has backdrops and every one lies in the sandbox.
func (r *extraProofRig) itemBound(ctx context.Context, t *testing.T) bool {
	t.Helper()
	var res struct{ Items []struct{ Path string } }
	q := "/Items?Recursive=true&IncludeItemTypes=Audio&Fields=Path&ArtistIds=" + url.QueryEscape(r.env.itemID)
	if err := r.embyGET(ctx, q, &res); err != nil {
		t.Logf("binding proof by tracks unavailable: %v", err)
	}
	for _, it := range res.Items {
		if strings.Contains(it.Path, "/"+proofArtistName+"/"+proofAlbumDir+"/") {
			return true
		}
	}
	_, _, bds := r.snapshot(ctx, t, "binding-proof")
	ok := len(bds) > 0
	for _, b := range bds {
		ok = ok && underSandbox(b.Path)
	}
	return ok
}

// clearIfProven empties the item's backdrops only when that cannot touch a foreign
// item: zero backdrops (a no-op) or itemBound. Setup (fatal) fails; cleanup reports.
func (r *extraProofRig) clearIfProven(ctx context.Context, t *testing.T, fatal bool) {
	t.Helper()
	if st, err := r.client.GetArtistDetail(ctx, r.env.itemID); err == nil && st.BackdropCount == 0 {
		return
	}
	if !r.itemBound(ctx, t) {
		msg := fmt.Sprintf("LIVE-PROOF-SETUP: refusing to clear backdrops of item %s: nothing proves it belongs to the sandbox", r.env.itemID)
		if fatal {
			t.Fatal(msg)
		}
		t.Error(msg)
		return
	}
	clearAllBackdrops(ctx, t, r.env.itemID, r.client)
}

// wipe empties the sandbox back to the sentinel and the album (wipeSandbox refuses a symlink).
func (r *extraProofRig) wipe(t *testing.T) {
	t.Helper()
	if err := wipeSandbox(r.dir); err != nil {
		t.Errorf("cleanup: %v", err)
	}
}

// rescan asks Emby to re-read the item's folder (no metadata or image replace),
// then polls up to 30s for want backdrops (want < 0: any settled count). It
// returns the last count read. Auth is the X-Emby-Token header, never logged.
func (r *extraProofRig) rescan(ctx context.Context, t *testing.T, want int) int {
	t.Helper()
	return r.refresh(ctx, t, want, false)
}

// refresh is rescan, optionally as a full image refresh that REPLACES the item's
// images from the folder (replace=true; ReplaceAllMetadata stays false). That
// re-binds backdrops to the sandbox files after Emby swapped them to its own copies.
func (r *extraProofRig) refresh(ctx context.Context, t *testing.T, want int, replace bool) int {
	t.Helper()
	mode := "Default"
	if replace {
		mode = "FullRefresh"
	}
	u := fmt.Sprintf("%s/Items/%s/Refresh?Recursive=true&ImageRefreshMode=%s&MetadataRefreshMode=%s&ReplaceAllImages=%t&ReplaceAllMetadata=false", strings.TrimRight(r.env.url, "/"), url.PathEscape(r.env.itemID), mode, mode, replace)
	deadline := time.Now().Add(30 * time.Second)
	last := -1
	for attempt := 0; ctx.Err() == nil; attempt++ {
		if attempt%5 == 0 {
			r.post(ctx, t, u)
		}
		time.Sleep(time.Second)
		if st, err := r.client.GetArtistDetail(ctx, r.env.itemID); err == nil {
			last = st.BackdropCount
			if want < 0 || last == want {
				return last
			}
		}
		if time.Now().After(deadline) {
			break
		}
	}
	return last
}

// execute is the whole sequence. migrate=false is the CONTROL: the same seed and
// the same three pushes, minus the migration. It returns one proofRun per push.
func (r *extraProofRig) execute(ctx context.Context, t *testing.T, migrate bool) []proofRun {
	t.Helper()
	// Seed ORDER matters for reach: build the fixture (asserting its counts) in a scratch
	// dir, put the extrafanart/ files in the sandbox first and let Emby index them as the
	// first backdrops, THEN add the root fanart. Emby may still sort the root file first;
	// LIVE-PROOF-REACH records what it actually did.
	scratch := t.TempDir()
	seedExtrafanartFixture(t, scratch)
	if err := os.CopyFS(filepath.Join(r.dir, "extrafanart"), os.DirFS(filepath.Join(scratch, "extrafanart"))); err != nil {
		t.Fatalf("LIVE-PROOF-SETUP: copying extrafanart/: %v", err)
	}
	r.requireBound(ctx, t, "seeded-extrafanart-only")
	rootBytes, err := os.ReadFile(filepath.Join(scratch, "fanart.jpg"))
	if err != nil {
		t.Fatalf("LIVE-PROOF-SETUP: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "fanart.jpg"), rootBytes, 0o644); err != nil {
		t.Fatalf("LIVE-PROOF-SETUP: %v", err)
	}
	seeded := r.requireBound(ctx, t, "seeded")
	if migrate {
		runExtrafanartMigration(t, r.dir)
		if n := r.refresh(ctx, t, proofRootFiles+proofExtraFiles, true); n != proofRootFiles+proofExtraFiles {
			t.Fatalf("LIVE-PROOF-SETUP: Emby sees %d backdrops after the migration, want %d", n, proofRootFiles+proofExtraFiles)
		}
		r.requireBound(ctx, t, "migrated")
	}
	if !migrate {
		// REACH: a push replaces Emby backdrop indexes 0..k-1 (k = local root fanart files).
		// The loss needs an extrafanart/ file at one of them; say whether it does.
		reach := false
		for i, b := range seeded {
			if i < proofRootFiles && strings.Contains(b.Path, "/extrafanart/") {
				reach = true
			}
		}
		t.Logf("LIVE-PROOF-REACH push_touches_indexes=0..%d extrafanart_within_reach=%v seeded_order=%v savelocalmetadata=%q", proofRootFiles-1, reach, redactImages(seeded), os.Getenv("SW_PROOF_SAVE_LOCAL_METADATA"))
	}
	baseline := contentInventory(t, r.dir)
	t.Logf("LIVE-PROOF-COUNT migrate=%v baseline_files=%d", migrate, len(baseline))
	var runs []proofRun
	for run := 1; run <= proofPushes; run++ {
		t.Logf("LIVE-PROOF-PUSH migrate=%v run=%d at=%s", migrate, run, time.Now().Format(time.RFC3339))
		warnings := r.p.SyncAllFanartToPlatforms(ctx, r.art)
		t.Logf("run %d push warnings (informational; Emby 4.10.x can 500 a write it applied): %v", run, warnings)
		time.Sleep(2 * time.Second) // let Emby finish any file removal it queued
		now := contentInventory(t, r.dir)
		extra := 0
		for _, f := range now {
			if strings.HasPrefix(f.rel, "extrafanart/") {
				extra++
			}
		}
		r.snapshot(ctx, t, fmt.Sprintf("after-push-%d-migrate-%v", run, migrate))
		emby := pollBackdropDetail(ctx, t, r.client, r.env.itemID, -1).BackdropCount
		runs = append(runs, proofRun{run: run, extra: extra, emby: emby, missing: missingFromInventory(baseline, now)})
		names := make([]string, 0, len(now))
		for _, f := range now {
			names = append(names, f.rel)
		}
		t.Logf("LIVE-PROOF-COUNT migrate=%v run=%d files=%d extrafanart_files=%d emby_backdrops=%d missing=%d names=%v", migrate, run, len(now), extra, emby, len(runs[run-1].missing), names)
	}
	return runs
}

// NOTE: this green does not itself assert that a push uploaded anything; it is only
// meaningful paired with the control below (which proves the pushes reach and delete
// files on this Emby), and scripts/live-extrafanart-proof.sh requires both.
func TestLiveEmby_ExtrafanartMigration_ThreePushesLoseNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), proofLiveTimeout)
	defer cancel()
	r := newExtraProofRig(ctx, t)
	for _, run := range r.execute(ctx, t, true) {
		if len(run.missing) > 0 {
			t.Errorf("LIVE-PROOF-DEFECT: push %d of %d lost files: %s", run.run, proofPushes, strings.Join(run.missing, "; "))
		}
		if run.emby != proofRootFiles+proofExtraFiles {
			t.Errorf("LIVE-PROOF-DEFECT: push %d left Emby with %d backdrops, want a stable %d", run.run, run.emby, proofRootFiles+proofExtraFiles)
		}
	}
}

// The red side: no migration first. Passes only when the loss is observed.
func TestLiveEmby_ExtrafanartMigration_ControlUnmigratedLoses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), proofLiveTimeout)
	defer cancel()
	r := newExtraProofRig(ctx, t)
	lost := 0
	for _, run := range r.execute(ctx, t, false) {
		isLoss, other := extrafanartLoss(run.missing, run.extra)
		if len(other) > 0 {
			t.Logf("LIVE-PROOF-OTHER push %d: not counted as the loss (no extrafanart/ file is gone): %s", run.run, strings.Join(other, "; "))
		}
		if isLoss {
			lost++
			t.Logf("LIVE-PROOF-LOSS push %d: extrafanart/ holds %d file(s); missing: %s", run.run, run.extra, strings.Join(run.missing, "; "))
		}
	}
	if lost == 0 {
		t.Fatalf("LIVE-PROOF-SETUP: the unmigrated control lost nothing in %d pushes, so the loss did not reproduce here and the migrated green proves nothing. The 2->1->0 in #3174 was measured on Emby 4.10.0.30 with the library option SaveLocalMetadata=true (this run: %q); see LIVE-PROOF-REACH and the Emby log lines the wrapper collects", proofPushes, os.Getenv("SW_PROOF_SAVE_LOCAL_METADATA"))
	}
}

// post sends an authenticated empty POST and logs a failure or a non-2xx answer.
func (r *extraProofRig) post(ctx context.Context, t *testing.T, u string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, http.NoBody)
	if err != nil {
		t.Logf("building POST request: %v", err)
		return
	}
	req.Header.Set("X-Emby-Token", r.env.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("POST failed: %v", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.Logf("POST answered HTTP %d", resp.StatusCode)
	}
}

// embyGET reads a JSON answer from Emby (token in a header, never in a URL or log).
func (r *extraProofRig) embyGET(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.env.url, "/")+path, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("X-Emby-Token", r.env.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// discoverItem scans the library, polls (bounded, 3 min) for the fixture artist by
// its synthetic name. Binding to the sandbox folder is proven separately (requireBound).
func (r *extraProofRig) discoverItem(ctx context.Context, t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for attempt := 0; time.Now().Before(deadline) && ctx.Err() == nil; attempt++ {
		if attempt%12 == 0 { // (re)start a scan every minute
			r.post(ctx, t, strings.TrimRight(r.env.url, "/")+"/Library/Refresh")
		}
		var res struct {
			Items []struct{ Id, Name, Path string }
		}
		q := "/Items?Recursive=true&IncludeItemTypes=MusicArtist&Fields=Path&SearchTerm=" + url.QueryEscape(proofArtistName)
		if err := r.embyGET(ctx, q, &res); err != nil {
			t.Logf("discovery poll: %v", err)
		}
		var found []string
		for _, it := range res.Items {
			if it.Name == proofArtistName {
				found = append(found, it.Id)
			}
		}
		if len(found) > 1 {
			// Cleanup clears backdrops on the chosen item, so never guess between two.
			t.Fatalf("LIVE-PROOF-SETUP: %d MusicArtist items are named %q (ids %v); remove the duplicates so the proof cannot act on the wrong one", len(found), proofArtistName, found)
		}
		if len(found) == 1 {
			t.Logf("discovered artist item %s", found[0])
			return found[0]
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("LIVE-PROOF-SETUP: artist %q did not appear in Emby within 3 minutes of a scan; is the library a music library covering the sandbox folder?", proofArtistName)
	return ""
}

// embyImage is one entry of GET /Items/{id}/Images.
type embyImage struct {
	ImageType  string
	ImageIndex int
	Path       string
}

// snapshot reads the item (per-item endpoint, Path requested) and its image
// listing, writes both to the diagnostics file, and returns them. Only synthetic
// names and paths are recorded; no header or key ever is.
func (r *extraProofRig) snapshot(ctx context.Context, t *testing.T, stage string) (itemPath, itemType string, backdrops []embyImage) {
	t.Helper()
	var item struct{ Id, Name, Type, Path string }
	var imgs []embyImage
	e1 := r.embyGET(ctx, fmt.Sprintf("/Users/%s/Items/%s?Fields=Path", url.PathEscape(r.env.userID), url.PathEscape(r.env.itemID)), &item)
	e2 := r.embyGET(ctx, fmt.Sprintf("/Items/%s/Images", url.PathEscape(r.env.itemID)), &imgs)
	for _, im := range imgs {
		if im.ImageType == "Backdrop" {
			backdrops = append(backdrops, im)
		}
	}
	red := make([]embyImage, len(imgs))
	for i, im := range imgs {
		red[i] = embyImage{im.ImageType, im.ImageIndex, redactPath(im.Path)}
	}
	shown := item
	shown.Path = redactPath(item.Path)
	rec, _ := json.Marshal(map[string]any{"stage": stage, "item": shown, "images": red, "itemErr": fmt.Sprint(e1), "imagesErr": fmt.Sprint(e2)})
	t.Logf("LIVE-PROOF-DIAG %s", rec)
	if dir := os.Getenv("SW_PROOF_DIAG_DIR"); dir != "" {
		if f, err := os.OpenFile(filepath.Join(dir, "live-3179-diag.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.Write(append(rec, '\n'))
			_ = f.Close()
		}
	}
	return item.Path, item.Type, backdrops
}

// requireBound makes Emby re-bind the item's backdrops to the sandbox files (a
// full image refresh; this also undoes a previous run having consumed the
// precondition), then fails with LIVE-PROOF-SETUP unless there is at least one
// backdrop and EVERY backdrop path is under the sandbox. The image paths are the
// positive evidence the loss needs (files Emby can delete); the item Path is only
// printed, because Emby does not reliably return one for a music artist.
func (r *extraProofRig) requireBound(ctx context.Context, t *testing.T, stage string) []embyImage {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		r.refresh(ctx, t, -1, true)
		itemPath, itemType, bds := r.snapshot(ctx, t, stage)
		bound := len(bds) > 0
		for _, b := range bds {
			bound = bound && underSandbox(b.Path)
		}
		if bound {
			return bds
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			var ps []string
			for _, b := range bds {
				ps = append(ps, fmt.Sprintf("%d:%q", b.ImageIndex, redactPath(b.Path)))
			}
			t.Fatalf("LIVE-PROOF-SETUP (%s): item %s type=%q path=%q (suffix matches folder: %v) has %d backdrops with paths [%s]; need >=1 and each under /%s/. See live-3179-diag.jsonl",
				stage, r.env.itemID, itemType, redactPath(itemPath), path.Base(itemPath) == proofArtistName, len(bds), strings.Join(ps, " "), proofArtistName)
		}
		time.Sleep(3 * time.Second)
	}
}

// redactImages is images with every path made library-root free, for logging.
func redactImages(in []embyImage) []embyImage {
	out := make([]embyImage, len(in))
	for i, im := range in {
		out[i] = embyImage{im.ImageType, im.ImageIndex, redactPath(im.Path)}
	}
	return out
}
