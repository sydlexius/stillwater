//go:build integration

package api

// Live (#3145) test: each of the four Backdrops-tab handlers is driven through
// its HTTP route against a REAL Jellyfin and shown not to inflate the platform's
// backdrop count. Every one of them ends in SyncAllFanartToPlatforms. Before
// #3317 that call appended the whole local set on Jellyfin each time (0 -> 3 ->
// 6 -> 9); it now clears then re-uploads, so the platform count must equal the
// local fanart file count after every run.
//
// Skips unless SW_LIVE_JELLYFIN_URL / _API_KEY / _USER_ID / _ITEM_ID are all
// set. The item is a SCRATCH MusicArtist: every backdrop on it is cleared up
// front and again at cleanup.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/encryption"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/internal/rule"
)

// liveJPEG returns a real, distinct 64x64 JPEG per seed (a reproducible noise
// field), so slot content can be compared byte-for-byte across the peer.
func liveJPEG(t *testing.T, seed int) []byte {
	t.Helper()
	state := uint32(seed)*2654435761 + 1
	im := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			state = state*1664525 + 1013904223
			im.Set(x, y, color.RGBA{R: uint8(state >> 24), G: uint8(state >> 16), B: uint8(state >> 8), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, im, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding jpeg: %v", err)
	}
	return buf.Bytes()
}

// liveHandlerHarness is a real Router (real SQLite, real Publisher, real
// Jellyfin connection) reached through its mux with a Bearer token, plus the
// direct Jellyfin client used to seed and read back the platform.
type liveHandlerHarness struct {
	t        *testing.T
	ctx      context.Context
	mux      http.Handler
	token    string
	router   *Router
	artist   *artist.Artist
	client   *jellyfin.Client
	itemID   string
	preCount int // platform backdrop count just before the handler call
}

func newLiveHandlerHarness(t *testing.T) *liveHandlerHarness {
	t.Helper()
	url, key := os.Getenv("SW_LIVE_JELLYFIN_URL"), os.Getenv("SW_LIVE_JELLYFIN_API_KEY")
	userID, itemID := os.Getenv("SW_LIVE_JELLYFIN_USER_ID"), os.Getenv("SW_LIVE_JELLYFIN_ITEM_ID")
	if url == "" || key == "" || userID == "" || itemID == "" {
		t.Skip("SW_LIVE_JELLYFIN_URL / SW_LIVE_JELLYFIN_API_KEY / SW_LIVE_JELLYFIN_USER_ID / SW_LIVE_JELLYFIN_ITEM_ID not all set; skipping live Jellyfin handler test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	db := newTestDB(t)
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	authSvc := auth.NewService(db)
	if _, err := authSvc.Setup(ctx, "admin", "password"); err != nil {
		t.Fatalf("setting up admin: %v", err)
	}
	var uid string
	if err := db.QueryRow("SELECT id FROM users WHERE username = 'admin'").Scan(&uid); err != nil {
		t.Fatalf("looking up admin: %v", err)
	}
	token, _, err := authSvc.CreateAPIToken(ctx, uid, "live-3145", string(auth.ScopeAdmin))
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	pub := publish.New(publish.Deps{ArtistService: artistSvc, ArtistLister: artistSvc, ConnectionService: connSvc, Logger: logger})
	r := NewRouter(RouterDeps{
		SessionSecret: testSessionSecret, AuthService: authSvc, ArtistService: artistSvc,
		ConnectionService: connSvc, PlatformService: platform.NewService(db), RuleService: ruleSvc, DB: db, Logger: logger,
		StaticFS: os.DirFS("../../web/static"), Publisher: pub,
	})
	// Same override the other handler tests use: the auto-wired detector would
	// probe the real peer for conflicts and is not what is under test.
	r.conflictDetector = conflict.NewForTest(connSvc, logger)
	r.conflictGate = conflict.NewGate(r.conflictDetector)

	conn := &connection.Connection{
		ID: "conn-jf", Name: "live-jellyfin", Type: connection.TypeJellyfin, URL: url, APIKey: key,
		Enabled: true, Status: "ok",
		Jellyfin: &connection.JellyfinConfig{PlatformUserID: userID, FeatureImageWrite: true},
	}
	if err := connSvc.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}
	a := &artist.Artist{Name: "Live 3145 Artist", SortName: "Live 3145 Artist", Type: "group", Path: t.TempDir()}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, itemID); err != nil {
		t.Fatalf("setting platform id: %v", err)
	}
	hctx, hcancel := context.WithCancel(context.Background())
	t.Cleanup(hcancel)

	h := &liveHandlerHarness{t: t, ctx: ctx, mux: r.Handler(hctx), token: token, router: r, artist: a,
		client: jellyfin.New(url, key, userID, logger), itemID: itemID}
	h.clearPlatform()
	t.Cleanup(h.clearPlatform) // leave the scratch item with no backdrops
	return h
}

// clearPlatform deletes every backdrop on the item, high-index-first (the peer
// re-indexes after each delete). t.Errorf, not Fatalf: it runs in Cleanup.
func (h *liveHandlerHarness) clearPlatform() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := h.client.GetArtistDetail(ctx, h.itemID)
	if err != nil {
		h.t.Errorf("clearPlatform: reading state: %v", err)
		return
	}
	for i := st.BackdropCount - 1; i >= 0; i-- {
		if err := h.client.DeleteImageAtIndex(ctx, h.itemID, "fanart", i); err != nil {
			h.t.Errorf("clearPlatform: deleting backdrop %d: %v", i, err)
		}
	}
	if st, err = h.client.GetArtistDetail(ctx, h.itemID); err != nil || st.BackdropCount != 0 {
		h.t.Errorf("clearPlatform: item not empty afterwards (err=%v)", err)
	}
}

func (h *liveHandlerHarness) platformCount() int {
	h.t.Helper()
	st, err := h.client.GetArtistDetail(h.ctx, h.itemID)
	if err != nil {
		h.t.Fatalf("reading platform state: %v", err)
	}
	return st.BackdropCount
}

// seedStale appends n backdrops whose bytes are not in the local set and
// asserts they landed (the precondition that the platform starts dirty).
func (h *liveHandlerHarness) seedStale(n, seedBase int) {
	h.t.Helper()
	before := h.platformCount()
	for i := 0; i < n; i++ {
		if err := h.client.UploadImage(h.ctx, h.itemID, "fanart", liveJPEG(h.t, seedBase+i), "image/jpeg"); err != nil {
			h.t.Fatalf("seeding stale backdrop: %v", err)
		}
	}
	time.Sleep(500 * time.Millisecond)
	if got := h.platformCount(); got != before+n {
		h.t.Fatalf("precondition failed: platform count = %d after seeding %d onto %d, want %d", got, n, before, before+n)
	}
	h.preCount = before + n
}

// localFiles returns the bytes of the artist's fanart files in slot order.
func (h *liveHandlerHarness) localFiles() [][]byte {
	h.t.Helper()
	paths, err := img.DiscoverFanart(h.ctx, h.artist.Path, h.router.getActiveFanartPrimary(h.ctx))
	if err != nil {
		h.t.Fatalf("discovering local fanart: %v", err)
	}
	out := make([][]byte, len(paths))
	for i, p := range paths {
		if out[i], err = os.ReadFile(p); err != nil {
			h.t.Fatalf("reading %s: %v", p, err)
		}
	}
	return out
}

// topUpLocal writes fresh local files until the set has n files.
func (h *liveHandlerHarness) topUpLocal(n, seedBase int) {
	h.t.Helper()
	primary, kodi := h.router.getActiveFanartPrimary(h.ctx), h.router.isKodiNumbering(h.ctx)
	for i := len(h.localFiles()); i < n; i++ {
		p := filepath.Join(h.artist.Path, img.FanartFilename(primary, i, kodi))
		if err := os.WriteFile(p, liveJPEG(h.t, seedBase+i), 0o600); err != nil {
			h.t.Fatalf("writing local fanart %d: %v", i, err)
		}
	}
	if got := len(h.localFiles()); got != n {
		h.t.Fatalf("precondition failed: %d local fanart files, want %d", got, n)
	}
}

// do sends one request through the real mux and requires 200 and no sync warnings.
func (h *liveHandlerHarness) do(method, path, body string) {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		h.t.Fatalf("%s %s = %d, want 200; body: %s", method, path, w.Code, w.Body.String())
	}
	var resp struct {
		SyncWarnings []string `json:"sync_warnings"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.SyncWarnings) != 0 {
		h.t.Fatalf("%s %s returned sync warnings: %v", method, path, resp.SyncWarnings)
	}
}

// runThrice drives one handler three times in a row (two runs cannot tell
// "adds once" from "adds every time"). Before each run, prep fixes the local
// set; the platform is then made dirty (2 stale backdrops on run 1, one fresh
// stale one on later runs, so every run starts with something to clear). After
// each run the platform count must equal the LOCAL file count, read from disk
// then (delete and batch-delete shrink it, assign grows it), and slot i must
// hold local file i's bytes.
func runThrice(t *testing.T, startLocal int, prep func(h *liveHandlerHarness, run int), call func(h *liveHandlerHarness, run int)) {
	h := newLiveHandlerHarness(t)
	h.topUpLocal(startLocal, 0xA00)
	for run := 1; run <= 3; run++ {
		prep(h, run)
		if run == 1 {
			h.seedStale(2, 0xE00)
		} else {
			h.seedStale(1, 0xE00+run)
		}
		call(h, run)
		time.Sleep(500 * time.Millisecond)

		local := h.localFiles()
		got := h.platformCount()
		t.Logf("run %d: platform before=%d, local files=%d, platform after=%d", run, h.preCount, len(local), got)
		if got != len(local) {
			t.Fatalf("run %d: platform BackdropCount = %d, want %d (the local file count); a larger value means the #3145 append-inflation is back", run, got, len(local))
		}
		for i, want := range local {
			data, _, err := h.client.GetArtistBackdrop(h.ctx, h.itemID, i)
			if err != nil {
				t.Fatalf("run %d: reading platform backdrop %d: %v", run, i, err)
			}
			if !bytes.Equal(data, want) {
				t.Errorf("run %d: platform backdrop %d does not hold local fanart %d's bytes", run, i, i)
			}
		}
	}
}

// Slot delete: re-add a local file before each run so there are 4 to delete
// from; deleting slot 1 leaves 3, and the platform must read 3 each time.
func TestLiveJellyfin_HandleFanartSlotDelete_DoesNotInflate(t *testing.T) {
	runThrice(t, 4,
		func(h *liveHandlerHarness, run int) { h.topUpLocal(4, 0xB00+run*16) },
		func(h *liveHandlerHarness, _ int) {
			h.do(http.MethodDelete, "/api/v1/artists/"+h.artist.ID+"/images/fanart/1", "")
		})
}

// Batch delete: same top-up to 4, then delete slots 0 and 2, leaving 2.
func TestLiveJellyfin_HandleFanartBatchDelete_DoesNotInflate(t *testing.T) {
	runThrice(t, 4,
		func(h *liveHandlerHarness, run int) { h.topUpLocal(4, 0xC00+run*16) },
		func(h *liveHandlerHarness, _ int) {
			h.do(http.MethodDelete, "/api/v1/artists/"+h.artist.ID+"/images/fanart/batch", `{"indices":[0,2]}`)
		})
}

// Reorder: the local count stays 3; rotating [2,0,1] changes the order every
// run, so the byte-order check proves the platform follows the new order.
func TestLiveJellyfin_HandleFanartReorder_DoesNotInflate(t *testing.T) {
	runThrice(t, 3,
		func(*liveHandlerHarness, int) {},
		func(h *liveHandlerHarness, _ int) {
			h.do(http.MethodPost, "/api/v1/artists/"+h.artist.ID+"/images/fanart/reorder", `{"order":[2,0,1]}`)
		})
}

// Slot assign: each run pulls the freshly seeded (last, unique) platform
// backdrop into the next free local slot, so the local set grows 2 -> 3 -> 4 -> 5
// and the platform must follow it exactly, never run-count times the set.
func TestLiveJellyfin_HandleFanartSlotAssign_DoesNotInflate(t *testing.T) {
	runThrice(t, 2,
		func(*liveHandlerHarness, int) {},
		func(h *liveHandlerHarness, _ int) {
			slot := len(h.localFiles())
			body := fmt.Sprintf(`{"connection_id":"conn-jf","platform_index":%d}`, h.preCount-1)
			h.do(http.MethodPost, fmt.Sprintf("/api/v1/artists/%s/images/fanart/%d/assign", h.artist.ID, slot), body)
		})
}
