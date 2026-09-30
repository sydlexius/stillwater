package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/config"
	"github.com/sydlexius/stillwater/internal/provider/aiblock"
)

// startListeners blocks on the HTTP listener, so whether it starts and drains
// the blocklist loop is checked statically, like the MBID sweep wiring.
func TestStartListenersStartsAndDrainsAIBlocklist(t *testing.T) {
	file, _ := parseMainGo(t)
	fn := findFunc(file, "startListeners")
	if fn == nil {
		t.Fatal("could not find func startListeners in main.go")
	}
	for _, call := range []string{"startAIBlocklist", "drainAIBlocklistOnShutdown"} {
		if !callsSelector(fn.Body, call) {
			t.Errorf("startListeners no longer calls %s (#2310)", call)
		}
	}
	if w := findFunc(file, "wireProviders"); w == nil || !callsSelector(w.Body, "newAIBlocklist") {
		t.Error("wireProviders no longer calls newAIBlocklist, so the web search handler has no store (#2310)")
	}
}

func aiBlocklistApp(t *testing.T, url string) (*Application, string) {
	t.Helper()
	dataDir := t.TempDir()
	cfg := config.Default()
	cfg.Database.Path = filepath.Join(dataDir, "stillwater.db")
	cfg.Image.AIBlocklistURL = url
	a := &Application{cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.Cleanup(func() { aiblock.SetDefault(nil) })
	return a, dataDir
}

func seedBlocklistCache(t *testing.T, dataDir string) {
	t.Helper()
	body, err := os.ReadFile("../../internal/provider/aiblock/testdata/upstream_sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "cache", aiblock.CacheFileName), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// An empty SW_AI_BLOCKLIST_URL never starts the loop: even a valid cached list
// next to the database is not adopted, so a harness server is fully offline
// and reports not loaded.
func TestStartAIBlocklist_EmptyURLStaysOffline(t *testing.T) {
	a, dataDir := aiBlocklistApp(t, "")
	seedBlocklistCache(t, dataDir)
	a.newAIBlocklist()
	a.startAIBlocklist(context.Background())
	if a.aiBlocklistDone != nil {
		t.Fatal("empty URL started the refresh loop")
	}
	if st := a.aiBlocklist.Status(); st.Loaded {
		t.Errorf("empty URL: Status = %+v, want not loaded", st)
	}
	if aiblock.Default().MatchURL("https://images.nightcafe.studio/x.jpg") {
		t.Error("empty URL: the default matcher must block nothing")
	}
	if err := a.drainAIBlocklist(context.Background()); err != nil {
		t.Errorf("drain of a never-started loop = %v, want nil", err)
	}
}

// With a URL set, the store is installed as the default, adopts the cached
// list from <data dir>/cache/ai_blocklist.txt, and the loop exits on cancel.
// The URL is loopback, which the SSRF-guarded client refuses without dialing,
// so the fetch fails offline and the cached list stays active.
func TestStartAIBlocklist_LoadsCacheBesideDatabaseAndDrains(t *testing.T) {
	a, dataDir := aiBlocklistApp(t, "http://127.0.0.1:1/list.txt")
	seedBlocklistCache(t, dataDir)
	a.newAIBlocklist()
	ctx, cancel := context.WithCancel(context.Background())
	a.startAIBlocklist(ctx)
	if a.aiBlocklistDone == nil {
		t.Fatal("URL set but the refresh loop was not started")
	}
	if st := a.aiBlocklist.Status(); !st.Loaded || st.Rules == 0 {
		t.Errorf("Status = %+v, want the cached list loaded", st)
	}
	if !aiblock.Default().MatchURL("https://images.nightcafe.studio/x.jpg") {
		t.Error("aiblock.Default() is not the installed store's matcher")
	}
	cancel()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if err := a.drainAIBlocklist(drainCtx); err != nil {
		t.Errorf("drain after cancel = %v, want the loop to exit", err)
	}
}
