//go:build integration

package rule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/encryption"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/publish"
)

// #3138 S0, RULE PATH: the "No duplicate images" rule with prune_platform_copies
// on, in auto mode, run through the real Pipeline, the real
// ImageDuplicateFixer and the real *publish.Publisher against a real peer.
// This is the path a release ships (maintainer decision 4 on #3138).
//
// Real: the SQLite DB, artist/rule/connection services, the engine's checker,
// the fixer's local delete, the publisher, the HTTP client, the peer.
// Not real: the Pipeline is built with a nil publisher (as its neighbours do),
// so the post-fix metadata publish does not run; and the cache-invalidation
// hook is a counter rather than the API router's.

// rulePicture renders a smooth, structured 640x360 picture from seed: three
// plane waves per channel. Unlike a noise field it keeps its perceptual hash
// through a re-encode, which a near-duplicate fixture needs; distinct seeds
// give distinct hashes (asserted in the measurement, not assumed here).
func rulePicture(seed int) *stdimage.RGBA {
	const w, h = 640, 360
	state := uint32(seed)*2654435761 + 12345
	next := func() float64 {
		state = state*1664525 + 1013904223
		return float64(state>>8) / float64(1<<24)
	}
	type wave struct{ fx, fy, ph, amp float64 }
	var waves [3][3]wave
	for c := range waves {
		for k := range waves[c] {
			waves[c][k] = wave{fx: 0.5 + 3*next(), fy: 0.5 + 2*next(), ph: 2 * math.Pi * next(), amp: 25 + 20*next()}
		}
	}
	out := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var px [3]uint8
			for c := range waves {
				v := 128.0
				for _, wv := range waves[c] {
					v += wv.amp * math.Sin(2*math.Pi*(wv.fx*float64(x)/w+wv.fy*float64(y)/h)+wv.ph)
				}
				px[c] = uint8(math.Max(0, math.Min(255, v)))
			}
			out.Set(x, y, color.RGBA{R: px[0], G: px[1], B: px[2], A: 255})
		}
	}
	return out
}

func ruleJPEG(t *testing.T, src stdimage.Image, quality int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("encoding fixture jpeg: %v", err)
	}
	return buf.Bytes()
}

type ruleSlot struct {
	name string
	data []byte
}

// rulePeer is what the measurement needs from a peer.
type rulePeer interface {
	GetArtistDetail(ctx context.Context, platformArtistID string) (*connection.ArtistPlatformState, error)
	GetArtistBackdrop(ctx context.Context, platformArtistID string, index int) ([]byte, string, error)
	DeleteImageAtIndex(ctx context.Context, platformArtistID string, imageType string, index int) error
}

type ruleTarget struct {
	connType, url, apiKey, userID, itemID string
	client                                rulePeer
	appendBackdrop                        func(ctx context.Context, position int, data []byte) error
}

// ruleRefuseNonEmpty is the guard that runs before the first delete. The
// measurement clears the item before seeding and again in cleanup, so an item
// id that points at real artwork would lose it. An item that already holds ANY
// backdrop is refused, with no override; a read failure is refused too (the
// state is unknown, so nothing may be deleted). It issues no delete.
//
// A clean sequential run cannot trip it: every run that seeds an item
// registers a t.Cleanup that clears it (and verifies zero), so each test starts
// from an empty item, and the seeding path runs once per test.
func ruleRefuseNonEmpty(ctx context.Context, tg ruleTarget) error {
	st, err := tg.client.GetArtistDetail(ctx, tg.itemID)
	if err != nil {
		return fmt.Errorf("cannot read item %s on %s before clearing it, so refusing to delete anything: %w", tg.itemID, tg.connType, err)
	}
	if st.BackdropCount > 0 {
		return fmt.Errorf("item %s on %s already holds %d backdrop(s); this test deletes every backdrop on its item, so the item must be a disposable scratch item with zero backdrops. Nothing was deleted. To recover, clear the item's backdrops on the server (or point SW_LIVE_* at a scratch item) and re-run",
			tg.itemID, tg.connType, st.BackdropCount)
	}
	return nil
}

func ruleClear(ctx context.Context, t *testing.T, tg ruleTarget) {
	t.Helper()
	st, err := tg.client.GetArtistDetail(ctx, tg.itemID)
	if err != nil {
		t.Errorf("clearing: reading state: %v", err)
		return
	}
	for i := st.BackdropCount - 1; i >= 0; i-- {
		if err := tg.client.DeleteImageAtIndex(ctx, tg.itemID, "fanart", i); err != nil {
			t.Errorf("clearing: deleting backdrop %d: %v", i, err)
		}
	}
	if after, err := tg.client.GetArtistDetail(ctx, tg.itemID); err != nil || after.BackdropCount != 0 {
		t.Errorf("clearing: item not empty afterwards (state=%+v err=%v)", after, err)
	}
}

// ruleRead reads the peer's backdrop list and names each slot by content.
func ruleRead(ctx context.Context, t *testing.T, tg ruleTarget, known []ruleSlot, step string, want int) []string {
	t.Helper()
	// Accept want only once two consecutive reads agree on it, so a count that
	// is still converging is not taken for the settled one. At the deadline the
	// last read is used and the caller's assertion decides.
	var st *connection.ArtistPlatformState
	for deadline, prev := time.Now().Add(3*time.Second), -1; ; time.Sleep(250 * time.Millisecond) {
		var err error
		if st, err = tg.client.GetArtistDetail(ctx, tg.itemID); err != nil {
			t.Fatalf("%s: reading state: %v", step, err)
		}
		if (st.BackdropCount == want && prev == want) || time.Now().After(deadline) {
			break
		}
		prev = st.BackdropCount
	}
	byHash := map[[32]byte]string{}
	for _, s := range known {
		byHash[sha256.Sum256(s.data)] = s.name
	}
	var names []string
	for i := 0; i < st.BackdropCount; i++ {
		data, _, err := tg.client.GetArtistBackdrop(ctx, tg.itemID, i)
		if err != nil {
			t.Fatalf("%s: reading backdrop %d: %v", step, i, err)
		}
		name, ok := byHash[sha256.Sum256(data)]
		if !ok {
			t.Fatalf("%s: backdrop %d matches NO uploaded image; the peer did not store the upload byte-for-byte", step, i)
		}
		names = append(names, name)
	}
	t.Logf("COUNT %-26s = %d   slots=%v", step, st.BackdropCount, names)
	return names
}

func localFanart(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading artist folder: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "fanart") {
			out = append(out, e.Name())
		}
	}
	return out
}

// measureRulePathPlatformPrune: local folder P1, P1-q60 (one local
// near-duplicate, so the rule raises a violation); peer P1-q60, P2, P1-q75,
// P2, P1, P3. P2 and P3 exist ONLY on the peer: the prune never deletes a copy
// byte-identical to a local file, so a distinct picture that is also local
// would survive whatever the similarity judgement said. Platform-only, they
// survive only because they do not match.
//
// pruneOn=true: one auto-mode pipeline run must delete the local
// near-duplicate AND leave the peer holding exactly one copy of each picture.
//
// pruneOn=false is the CONTROL on the option: the same fixture and the same
// run with prune_platform_copies off must fix the local folder and leave all
// six backdrops on the peer. Without it, a fixer that pruned the platform
// regardless of the option would pass everything above.
func measureRulePathPlatformPrune(t *testing.T, tg ruleTarget, pruneOn bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pic1 := rulePicture(101)
	p1 := ruleSlot{"P1", ruleJPEG(t, pic1, 92)}
	p1q60 := ruleSlot{"P1-q60", ruleJPEG(t, pic1, 60)}
	p1q75 := ruleSlot{"P1-q75", ruleJPEG(t, pic1, 75)}
	p2 := ruleSlot{"P2", ruleJPEG(t, rulePicture(202), 92)}
	p3 := ruleSlot{"P3", ruleJPEG(t, rulePicture(303), 92)}
	known := []ruleSlot{p1, p1q60, p1q75, p2, p3}
	seed := []ruleSlot{p1q60, p2, p1q75, p2, p1, p3}
	seedNames := "P1-q60|P2|P1-q75|P2|P1|P3"
	const wantAfter = "P2|P1|P3"

	// Fixture property, on the generated bytes: the near-duplicates differ in
	// bytes and match perceptually; the three pictures do not match each other.
	ph := func(b []byte) uint64 {
		h, err := img.PerceptualHash(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("perceptual hash: %v", err)
		}
		return h
	}
	for _, near := range []ruleSlot{p1q60, p1q75} {
		if sim := img.Similarity(ph(p1.data), ph(near.data)); bytes.Equal(p1.data, near.data) || sim < img.DefaultDuplicateTolerance {
			t.Fatalf("fixture: %s is not a near-duplicate of P1 (similarity %.4f)", near.name, sim)
		}
	}
	for _, pair := range [][2]ruleSlot{{p1, p2}, {p1, p3}, {p2, p3}} {
		if sim := img.Similarity(ph(pair[0].data), ph(pair[1].data)); sim >= img.DefaultDuplicateTolerance {
			t.Fatalf("fixture: %s and %s are meant to be distinct (similarity %.4f)", pair[0].name, pair[1].name, sim)
		}
	}

	// Before the first delete AND before the cleanup is registered: a refused
	// item must not be cleared by the cleanup either.
	if err := ruleRefuseNonEmpty(ctx, tg); err != nil {
		t.Fatalf("%v", err)
	}
	ruleClear(ctx, t, tg)
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		ruleClear(cctx, t, tg)
	})
	for i, s := range seed {
		if err := tg.appendBackdrop(ctx, i, s.data); err != nil {
			t.Fatalf("seeding backdrop %d (%s): %v", i, s.name, err)
		}
	}
	t.Logf("UPLOADED %d backdrops: %s", len(seed), seedNames)
	if got := strings.Join(ruleRead(ctx, t, tg, known, "before (seeded)", len(seed)), "|"); got != seedNames {
		t.Fatalf("precondition: peer holds %s after seeding, want %s", got, seedNames)
	}

	// Real services on a real SQLite DB.
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
	r.Config.PrunePlatformCopies = pruneOn
	if err := ruleSvc.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	t.Logf("RULE %s: enabled=%v mode=%s prune_platform_copies=%v tolerance=%v (0 = default %.2f)",
		r.ID, r.Enabled, r.AutomationMode, r.Config.PrunePlatformCopies, r.Config.Tolerance, img.DefaultDuplicateTolerance)

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatal(err)
	}
	connSvc := connection.NewService(db, enc)
	conn := &connection.Connection{Name: "s0-peer", Type: tg.connType, URL: tg.url, APIKey: tg.apiKey, Enabled: true, Status: "ok"}
	if tg.connType == connection.TypeEmby {
		conn.Emby = &connection.EmbyConfig{PlatformUserID: tg.userID, FeatureImageWrite: true}
	} else {
		conn.Jellyfin = &connection.JellyfinConfig{PlatformUserID: tg.userID, FeatureImageWrite: true}
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}

	dir := t.TempDir()
	for name, s := range map[string]ruleSlot{"fanart.jpg": p1, "fanart2.jpg": p1q60} {
		if err := os.WriteFile(filepath.Join(dir, name), s.data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := &artist.Artist{Name: "S0 Rule Scratch", SortName: "S0 Rule Scratch", Path: dir, LibraryID: "lib-test"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		insertTestImage(t, db, a.ID, "fanart", i)
	}
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, tg.itemID); err != nil {
		t.Fatal(err)
	}

	publisher := publish.New(publish.Deps{
		Logger: testLogger(), ArtistService: artistSvc, ArtistLister: artistSvc,
		ArtistGetter: artistSvc, ArtistImages: artistSvc, ConnectionService: connSvc,
	})
	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	engine.SetImageHashRecorder(artistSvc)
	fixer := NewImageDuplicateFixer(db, nil, nonSharedFSCheck(), artistSvc, testLogger())
	invalidations := 0
	fixer.SetPlatformPruner(publisher, func() { invalidations++ })
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

	// THE RUN: one unattended pipeline pass.
	rr, err := pipeline.RunForArtist(ctx, a)
	if err != nil {
		t.Fatalf("RunForArtist: %v", err)
	}
	var dupResult *FixResult
	for i := range rr.Results {
		t.Logf("FIX RESULT: rule=%s fixed=%v irreversible=%v message=%q", rr.Results[i].RuleID, rr.Results[i].Fixed, rr.Results[i].Irreversible, rr.Results[i].Message)
		if rr.Results[i].RuleID == RuleImageDuplicate {
			dupResult = &rr.Results[i]
		}
	}
	if dupResult == nil {
		t.Fatalf("the pipeline never ran the duplicate fixer (results: %+v); the rule path was not exercised", rr.Results)
	}

	// Local phase: the near-duplicate file is gone and the survivors renumbered.
	if got := localFanart(t, dir); len(got) != 1 {
		t.Errorf("local folder holds %v after the fix, want the 1 surviving fanart file", got)
	} else {
		t.Logf("LOCAL after fix: %v", got)
	}
	if !pruneOn {
		// CONTROL: the option is off, so the peer must be exactly as seeded.
		if got := strings.Join(ruleRead(ctx, t, tg, known, "after rule fix (option OFF)", len(seed)), "|"); got != seedNames {
			t.Fatalf("OPTION-OFF CONTROL FAILED: with prune_platform_copies off the peer holds %s, want the seeded %s untouched", got, seedNames)
		}
		if !dupResult.Fixed || strings.Contains(dupResult.Message, "platform:") || dupResult.Irreversible || invalidations != 0 {
			t.Errorf("option off: fixed=%v irreversible=%v invalidations=%d message=%q; want the local fix only and no platform phase",
				dupResult.Fixed, dupResult.Irreversible, invalidations, dupResult.Message)
		}
		t.Logf("SUMMARY rule path %s item %s (option OFF control): uploaded=6 before=6 afterRuleFix=6", tg.connType, tg.itemID)
		return
	}
	// Platform phase, read back from the peer.
	if got := strings.Join(ruleRead(ctx, t, tg, known, "after rule fix (auto)", 3), "|"); got != wantAfter {
		t.Fatalf("after the rule fix the peer holds %s, want %s (one copy of each picture: the local twin P1, and the platform-only P2 and P3)", got, wantAfter)
	}
	wantNote := fmt.Sprintf("platform: connection %s removed 3 backdrop(s), kept slot(s) 1,4", conn.ID)
	if !strings.Contains(dupResult.Message, wantNote) {
		t.Errorf("fix result does not record the platform deletions: %q, want it to contain %q", dupResult.Message, wantNote)
	}
	if !dupResult.Irreversible {
		t.Errorf("a fix that deleted on the platform must be marked irreversible")
	}
	if invalidations != 1 {
		t.Errorf("cache-invalidation hook ran %d times, want 1", invalidations)
	}

	// Idempotence, two ways. A second pipeline pass finds no local violation,
	// so it must not touch the peer; and the fixer invoked directly (what
	// Fix All does for a stale violation row) runs the platform phase over the
	// already-pruned peer and must delete nothing.
	if _, err := pipeline.RunForArtist(ctx, a); err != nil {
		t.Fatalf("second RunForArtist: %v", err)
	}
	if got := strings.Join(ruleRead(ctx, t, tg, known, "after 2nd pipeline run", 3), "|"); got != wantAfter {
		t.Fatalf("second pipeline run changed the peer: %s", got)
	}
	fresh, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// "lib-test" has no library row, so the reloaded artist carries no library
	// and the fixer would stop at its shared-filesystem guard before the
	// platform phase. Restore the value the pipeline run above was given.
	fresh.LibraryID = a.LibraryID
	fr, err := fixer.Fix(ctx, fresh, &Violation{RuleID: RuleImageDuplicate, Config: RuleConfig{PrunePlatformCopies: true}})
	if err != nil {
		t.Fatalf("direct re-fix: %v", err)
	}
	t.Logf("RE-FIX RESULT: fixed=%v message=%q", fr.Fixed, fr.Message)
	if !strings.Contains(fr.Message, "platform: removed 0 backdrop(s)") || fr.Fixed {
		t.Errorf("re-fix over the pruned peer: fixed=%v message=%q, want the platform phase to run and remove 0", fr.Fixed, fr.Message)
	}
	if got := strings.Join(ruleRead(ctx, t, tg, known, "after direct re-fix", 3), "|"); got != wantAfter {
		t.Fatalf("direct re-fix changed the peer: %s", got)
	}
	t.Logf("SUMMARY rule path %s item %s: uploaded=6 before=6 afterRuleFix=3 afterSecondRun=3 afterReFix=3", tg.connType, tg.itemID)
}

// SHARED SCRATCH ITEM. These live tests read the same SW_LIVE_EMBY_ITEM_ID /
// SW_LIVE_JELLYFIN_ITEM_ID (Jellyfin: SW_LIVE_JELLYFIN_PRUNE_ITEM_ID first) as
// the internal/publish live suites, which also clear and seed it. Run them ONE
// PACKAGE AT A TIME (`-p 1`, a single package per invocation); `go test -tags
// integration ./...` runs package binaries concurrently. ruleRefuseNonEmpty
// turns a collision that leaves backdrops on the item into a loud failure, not
// silent corruption; it cannot catch every interleaving.
func liveRuleEmby(t *testing.T) ruleTarget {
	url, key := os.Getenv("SW_LIVE_EMBY_URL"), os.Getenv("SW_LIVE_EMBY_API_KEY")
	user, item := os.Getenv("SW_LIVE_EMBY_USER_ID"), os.Getenv("SW_LIVE_EMBY_ITEM_ID")
	if url == "" || key == "" || user == "" || item == "" {
		t.Skip("SW_LIVE_EMBY_URL / SW_LIVE_EMBY_API_KEY / SW_LIVE_EMBY_USER_ID / SW_LIVE_EMBY_ITEM_ID not all set; skipping live Emby rule-path prune measurement")
	}
	c := emby.New(url, key, user, testLogger())
	return ruleTarget{connection.TypeEmby, url, key, user, item, c,
		func(ctx context.Context, _ int, b []byte) error {
			return c.UploadImage(ctx, item, "fanart", b, "image/jpeg")
		}}
}

func liveRuleJellyfin(t *testing.T) ruleTarget {
	url, key := os.Getenv("SW_LIVE_JELLYFIN_URL"), os.Getenv("SW_LIVE_JELLYFIN_API_KEY")
	user, item := os.Getenv("SW_LIVE_JELLYFIN_USER_ID"), os.Getenv("SW_LIVE_JELLYFIN_PRUNE_ITEM_ID")
	if item == "" {
		item = os.Getenv("SW_LIVE_JELLYFIN_ITEM_ID")
	}
	if url == "" || key == "" || user == "" || item == "" {
		t.Skip("SW_LIVE_JELLYFIN_URL / SW_LIVE_JELLYFIN_API_KEY / SW_LIVE_JELLYFIN_USER_ID / SW_LIVE_JELLYFIN_ITEM_ID not all set; skipping live Jellyfin rule-path prune measurement")
	}
	c := jellyfin.New(url, key, user, testLogger())
	return ruleTarget{connection.TypeJellyfin, url, key, user, item, c,
		func(ctx context.Context, _ int, b []byte) error {
			return c.UploadImage(ctx, item, "fanart", b, "image/jpeg")
		}}
}

func TestLiveRulePathPlatformPrune_Emby(t *testing.T) {
	measureRulePathPlatformPrune(t, liveRuleEmby(t), true)
}

func TestLiveRulePathPlatformPrune_Jellyfin(t *testing.T) {
	measureRulePathPlatformPrune(t, liveRuleJellyfin(t), true)
}

func TestLiveRulePathOptionOffLeavesPlatformAlone_Emby(t *testing.T) {
	measureRulePathPlatformPrune(t, liveRuleEmby(t), false)
}

func TestLiveRulePathOptionOffLeavesPlatformAlone_Jellyfin(t *testing.T) {
	measureRulePathPlatformPrune(t, liveRuleJellyfin(t), false)
}

// modelledPeer is a minimal in-process stand-in for one item's backdrop list
// (GET detail, GET/POST/DELETE an indexed backdrop; a delete renumbers). It
// exists only for the self-check below, and it is strict: it answers only for
// itemID and only with the credential the real client sends (Emby:
// X-Emby-Token; Jellyfin: Authorization: MediaBrowser Token="<key>", per
// internal/connection/mediabrowser), else 404 / 401. Every rejected or
// malformed request is recorded in errs so the self-check fails loudly.
type modelledPeer struct {
	connType, itemID, apiKey string
	mu                       sync.Mutex
	data                     [][]byte
	deletes                  int
	errs                     []string
}

func (s *modelledPeer) reject(w http.ResponseWriter, code int, why string) {
	s.errs = append(s.errs, why)
	http.Error(w, why, code)
}

func (s *modelledPeer) problems() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.errs...)
}

func (s *modelledPeer) deleteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deletes
}

func (s *modelledPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	authOK := false
	if s.connType == connection.TypeEmby {
		authOK = r.Header.Get("X-Emby-Token") == s.apiKey
	} else {
		authOK = r.Header.Get("Authorization") == fmt.Sprintf(`MediaBrowser Token="%s"`, s.apiKey)
	}
	if !authOK {
		s.reject(w, http.StatusUnauthorized, "missing or wrong credential on "+r.Method+" "+r.URL.Path)
		return
	}
	const marker = "/Images/Backdrop/"
	at := strings.Index(r.URL.Path, marker)
	if at < 0 {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/Items/"+s.itemID) {
			s.reject(w, http.StatusNotFound, "unexpected request "+r.Method+" "+r.URL.Path)
			return
		}
		tags := make([]string, len(s.data))
		for i := range tags {
			tags[i] = "t" + strconv.Itoa(i)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"Name": "peer item", "BackdropImageTags": tags})
		return
	}
	if !strings.HasSuffix(r.URL.Path[:at], "/Items/"+s.itemID) {
		s.reject(w, http.StatusNotFound, "backdrop request for the wrong item: "+r.URL.Path)
		return
	}
	idx, _ := strconv.Atoi(r.URL.Path[at+len(marker):])
	switch r.Method {
	case http.MethodGet:
		if idx >= len(s.data) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(s.data[idx])
	case http.MethodPost:
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			s.reject(w, http.StatusBadRequest, "reading upload body: "+err.Error())
			return
		}
		b, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil {
			s.reject(w, http.StatusBadRequest, "bad upload body: "+err.Error())
			return
		}
		s.data = append(s.data, b)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if idx >= len(s.data) {
			http.NotFound(w, r)
			return
		}
		s.deletes++
		s.data = append(s.data[:idx], s.data[idx+1:]...)
		w.WriteHeader(http.StatusNoContent)
	}
}

// TestRulePathPlatformPruneMeasurement_HarnessSelfCheck runs the same body
// against the in-process model. It is NOT the #3138 measurement and says
// nothing about a real server; it shows the harness and its expectations
// agree with the production rule path, so a red live run is about the peer.
func TestRulePathPlatformPruneMeasurement_HarnessSelfCheck(t *testing.T) {
	for _, connType := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		modelled := func(t *testing.T) (ruleTarget, *modelledPeer) {
			peer := &modelledPeer{connType: connType, itemID: "p1", apiKey: "k"}
			srv := httptest.NewServer(peer)
			t.Cleanup(srv.Close)
			var c interface {
				rulePeer
				UploadImageAtIndex(ctx context.Context, id, imageType string, index int, data []byte, contentType string) error
			}
			if connType == connection.TypeEmby {
				c = emby.New(srv.URL, "k", "u1", testLogger())
			} else {
				c = jellyfin.New(srv.URL, "k", "u1", testLogger())
			}
			return ruleTarget{connType, srv.URL, "k", "u1", "p1", c,
				func(ctx context.Context, position int, b []byte) error {
					return c.UploadImageAtIndex(ctx, "p1", "fanart", position, b, "image/jpeg")
				}}, peer
		}
		run := func(on bool) func(t *testing.T) {
			return func(t *testing.T) {
				tg, peer := modelled(t)
				measureRulePathPlatformPrune(t, tg, on)
				if errs := peer.problems(); len(errs) != 0 {
					t.Errorf("the modelled peer rejected or could not parse %d request(s): %v", len(errs), errs)
				}
			}
		}
		t.Run(connType+"/option-on", run(true))
		t.Run(connType+"/option-off", run(false))
	}
}

// TestRulePathPlatformPruneMeasurement_RefusesNonEmptyItem proves the
// non-empty refusal: a modelled item that starts with one backdrop is refused
// and not a single delete reaches the peer. An empty item is accepted.
func TestRulePathPlatformPruneMeasurement_RefusesNonEmptyItem(t *testing.T) {
	for _, connType := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		t.Run(connType, func(t *testing.T) {
			target := func(peer *modelledPeer) ruleTarget {
				srv := httptest.NewServer(peer)
				t.Cleanup(srv.Close)
				var c rulePeer
				if connType == connection.TypeEmby {
					c = emby.New(srv.URL, "k", "u1", testLogger())
				} else {
					c = jellyfin.New(srv.URL, "k", "u1", testLogger())
				}
				return ruleTarget{connType: connType, url: srv.URL, apiKey: "k", userID: "u1", itemID: "p1", client: c}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			occupied := &modelledPeer{connType: connType, itemID: "p1", apiKey: "k", data: [][]byte{[]byte("someone's real artwork")}}
			err := ruleRefuseNonEmpty(ctx, target(occupied))
			if err == nil {
				t.Fatal("ruleRefuseNonEmpty accepted an item that already holds a backdrop")
			}
			if !strings.Contains(err.Error(), "disposable scratch item") {
				t.Errorf("refusal does not tell the operator what to do: %v", err)
			}
			if n := occupied.deleteCount(); n != 0 {
				t.Errorf("deleteCount = %d, want 0: the refusal must come before any delete", n)
			}
			occupied.mu.Lock()
			held := len(occupied.data)
			occupied.mu.Unlock()
			if held != 1 {
				t.Errorf("occupied item holds %d backdrops after the refusal, want 1", held)
			}
			empty := &modelledPeer{connType: connType, itemID: "p1", apiKey: "k"}
			if err := ruleRefuseNonEmpty(ctx, target(empty)); err != nil {
				t.Errorf("an empty item was refused: %v", err)
			}
		})
	}
}

// TestRulePathPlatformPruneMeasurement_ModelledPeerIsStrict proves the
// self-check peer rejects a wrong item id, a missing key and an unreadable
// upload body, and records each.
func TestRulePathPlatformPruneMeasurement_ModelledPeerIsStrict(t *testing.T) {
	for _, connType := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		t.Run(connType, func(t *testing.T) {
			peer := &modelledPeer{connType: connType, itemID: "p1", apiKey: "k"}
			srv := httptest.NewServer(peer)
			t.Cleanup(srv.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			mk := func(key string) rulePeer {
				if connType == connection.TypeEmby {
					return emby.New(srv.URL, key, "u1", testLogger())
				}
				return jellyfin.New(srv.URL, key, "u1", testLogger())
			}
			if _, err := mk("k").GetArtistDetail(ctx, "p1"); err != nil {
				t.Fatalf("correct item and key rejected: %v", err)
			}
			if got := len(peer.problems()); got != 0 {
				t.Fatalf("a correct request was recorded as a problem: %v", peer.problems())
			}
			if _, err := mk("k").GetArtistDetail(ctx, "other-item"); err == nil {
				t.Error("a request for the wrong item id was answered")
			}
			if _, err := mk("").GetArtistDetail(ctx, "p1"); err == nil {
				t.Error("a request with no API key was answered")
			}
			if got := len(peer.problems()); got != 2 {
				t.Errorf("recorded %d problems, want 2 (wrong item, missing key): %v", got, peer.problems())
			}

			// An upload body that is not valid base64 (stands in for a body that
			// could not be read in full) is a 400 and is recorded.
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/Items/p1/Images/Backdrop/0", strings.NewReader("%%not base64%%"))
			if connType == connection.TypeEmby {
				req.Header.Set("X-Emby-Token", "k")
			} else {
				req.Header.Set("Authorization", `MediaBrowser Token="k"`)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("bad upload body: status %d, want 400", resp.StatusCode)
			}
			if got := len(peer.problems()); got != 3 {
				t.Errorf("recorded %d problems, want 3 after the bad body: %v", got, peer.problems())
			}
		})
	}
}
