package api

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/internal/publish"
	"github.com/sydlexius/stillwater/internal/rule"
)

// cachePeerPruner models the peer's backdrop list: each call deletes every
// copy after the first (a delete re-indexes the slots above it, so it walks
// high-to-low) and reports each delete in the plan.
type cachePeerPruner struct {
	backdrops []string
	calls     int
}

func (p *cachePeerPruner) PrunePlatformBackdropsForArtist(_ context.Context, a *artist.Artist, _ publish.ArtistBackdropPruneOptions) (publish.PlatformBackdropPruneResult, error) {
	p.calls++
	res := publish.PlatformBackdropPruneResult{ArtistsProcessed: 1}
	for i := len(p.backdrops) - 1; i >= 1; i-- {
		p.backdrops = append(p.backdrops[:i:i], p.backdrops[i+1:]...)
		res.BackdropsRemoved++
		res.Plan = append(res.Plan, publish.PlatformBackdropPrunePlanEntry{ArtistID: a.ID, ConnectionID: "c1", Index: i, Survivor: 0, Tier: publish.PruneTierPerceptual, Outcome: publish.PrunePlanDeleted})
	}
	return res, nil
}

type nonSharedLibs struct{}

func (nonSharedLibs) GetByID(context.Context, string) (*library.Library, error) {
	return &library.Library{SharedFSStatus: library.SharedFSNone}, nil
}

type ruleCacheHarness struct {
	db     *sql.DB
	r      *Router
	pipe   *rule.Pipeline
	prune  *cachePeerPruner
	artist *artist.Artist
	dir    string
}

// newRuleCacheHarness wires the REAL rule service, engine, pipeline and
// duplicate fixer behind a Router, so a PUT through handleUpdateRule and the
// pipeline's next read meet the same store and caches production uses.
func newRuleCacheHarness(t *testing.T) *ruleCacheHarness {
	t.Helper()
	ctx := context.Background()
	db := newTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	artistSvc := artist.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO libraries (id, name, path, type) VALUES ('lib-test', 'L', '/music', 'regular')`); err != nil {
		t.Fatalf("library: %v", err)
	}
	engine := rule.NewEngine(ruleSvc, db, nil, nil, logger)
	engine.SetImageHashRecorder(artistSvc)
	fixer := rule.NewImageDuplicateFixer(db, nil, rule.NewSharedFSCheck(nonSharedLibs{}, logger), artistSvc, logger)
	h := &ruleCacheHarness{db: db, prune: &cachePeerPruner{}, dir: t.TempDir()}
	fixer.SetPlatformPruner(h.prune, func() {})
	h.pipe = rule.NewPipeline(engine, artistSvc, ruleSvc, []rule.Fixer{fixer}, nil, logger)
	h.r = NewRouter(RouterDeps{
		SessionSecret: testSessionSecret, AuthService: nil, ArtistService: artistSvc,
		RuleService: ruleSvc, RuleEngine: engine, Pipeline: h.pipe, DB: db, Logger: logger,
		StaticFS: os.DirFS("../../web/static"),
	})
	h.artist = &artist.Artist{Name: "Cache Artist", SortName: "Cache Artist", Path: h.dir, LibraryID: "lib-test"}
	if err := artistSvc.Create(ctx, h.artist); err != nil {
		t.Fatalf("artist: %v", err)
	}
	return h
}

// reset puts a distinct slot 0 plus a NEAR-duplicate pair (one picture at two
// JPEG qualities) in slots 1 and 2, and two copies on the peer. Byte-identical
// copies would not do: the perceptual rule leaves those to the exact rule.
func (h *ruleCacheHarness) reset(t *testing.T) {
	t.Helper()
	// The rule package's createGradientJPEGQuality fixture, which its own
	// perceptual-duplicate tests rely on.
	enc := func(variant, quality int) []byte {
		const width, height = 400, 300
		im := stdimage.NewRGBA(stdimage.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				v := uint8((x*255/width + variant*37) % 256)
				im.Set(x, y, color.RGBA{R: v, G: 255 - v, B: 128, A: 255})
			}
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, im, &jpeg.Options{Quality: quality}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	for n, b := range map[string][]byte{"fanart.jpg": enc(0, 90), "fanart2.jpg": enc(1, 90), "fanart3.jpg": enc(1, 60)} {
		if err := os.WriteFile(filepath.Join(h.dir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A previous fix's reconcile retires the deleted slot's row; restore all
	// three so the checker sees the pair again.
	for i := 0; i < 3; i++ {
		if _, err := h.db.ExecContext(context.Background(), `INSERT OR IGNORE INTO artist_images (id, artist_id, image_type, slot_index, exists_flag) VALUES (?, ?, 'fanart', ?, 1)`,
			fmt.Sprintf("%s-fanart-%d", h.artist.ID, i), h.artist.ID, i); err != nil {
			t.Fatalf("image row: %v", err)
		}
	}
	h.prune.backdrops = []string{"A1", "A2"}
}

func (h *ruleCacheHarness) put(t *testing.T, body string) {
	t.Helper()
	req := httptest.NewRequestWithContext(adminContext(), http.MethodPut, "/api/v1/rules/"+rule.RuleImageDuplicate, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", rule.RuleImageDuplicate)
	w := httptest.NewRecorder()
	h.r.handleUpdateRule(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT %s: status %d body %s", body, w.Code, w.Body.String())
	}
}

func (h *ruleCacheHarness) fix(t *testing.T) *rule.FixResult {
	t.Helper()
	ctx := context.Background()
	v := &rule.RuleViolation{RuleID: rule.RuleImageDuplicate, ArtistID: h.artist.ID, ArtistName: h.artist.Name, Severity: "warning", Message: "dup", Fixable: true, Status: rule.ViolationStatusOpen}
	if err := h.r.ruleService.UpsertViolation(ctx, v); err != nil {
		t.Fatal(err)
	}
	vs, err := h.r.ruleService.ListViolationsFiltered(ctx, rule.ViolationListParams{Status: "active"})
	if err != nil || len(vs) == 0 {
		t.Fatalf("list violations: %v (%d)", err, len(vs))
	}
	fr, err := h.pipe.FixViolation(ctx, vs[0].ID)
	if err != nil {
		t.Fatalf("FixViolation: %v", err)
	}
	return fr
}

// TestUpdateRule_TurningPlatformPruneOffTakesEffect (#3138 B1): the option is
// the only consent for irreversible platform deletes, so revoking it through
// the admin PUT must stop the very next Fix, not the next restart.
func TestUpdateRule_TurningPlatformPruneOffTakesEffect(t *testing.T) {
	t.Parallel()
	h := newRuleCacheHarness(t)
	h.put(t, `{"enabled":true,"config":{"severity":"warning","tolerance":0.9,"prune_platform_copies":true}}`)
	h.reset(t)
	h.fix(t)
	if h.prune.calls != 1 {
		t.Fatalf("precondition: option on must reach the platform; calls=%d", h.prune.calls)
	}

	h.put(t, `{"config":{"severity":"warning","tolerance":0.9}}`)
	h.reset(t)
	fr := h.fix(t)
	if h.prune.calls != 1 {
		t.Fatalf("option turned OFF via PUT but the next Fix still reached the platform (calls=%d, msg=%q)", h.prune.calls, fr.Message)
	}
}

// TestUpdateRule_SwitchingToManualStopsAutoFix (#3138 B1): auto -> manual via
// the PUT must stop Run Rules from auto-fixing (and so platform-deleting).
func TestUpdateRule_SwitchingToManualStopsAutoFix(t *testing.T) {
	t.Parallel()
	h := newRuleCacheHarness(t)
	h.put(t, `{"enabled":true,"automation_mode":"auto","config":{"severity":"warning","tolerance":0.9,"prune_platform_copies":true}}`)
	h.reset(t)
	if _, err := h.pipe.RunAllScoped(context.Background(), rule.RunScopeAll); err != nil {
		t.Fatal(err)
	}
	if h.prune.calls != 1 {
		t.Fatalf("precondition: auto mode must auto-fix onto the platform; calls=%d", h.prune.calls)
	}

	h.put(t, `{"automation_mode":"manual"}`)
	h.reset(t)
	if _, err := h.pipe.RunAllScoped(context.Background(), rule.RunScopeAll); err != nil {
		t.Fatal(err)
	}
	if h.prune.calls != 1 {
		t.Fatalf("rule switched to manual via PUT but Run Rules still auto-fixed onto the platform (calls=%d)", h.prune.calls)
	}
	if !runFoundRule(t, h) {
		t.Fatal("the second pass left no open violation: manual mode must persist the detected duplicate, not fix it")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "fanart3.jpg")); err != nil {
		t.Errorf("manual mode deleted the local duplicate: %v", err)
	}
}

// runFoundRule reports whether the pass left an open image_duplicate
// violation for the harness artist (manual mode persists one, auto resolves).
func runFoundRule(t *testing.T, h *ruleCacheHarness) bool {
	t.Helper()
	vs, err := h.r.ruleService.ListViolationsFiltered(context.Background(), rule.ViolationListParams{Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.RuleID == rule.RuleImageDuplicate && v.ArtistID == h.artist.ID {
			return true
		}
	}
	return false
}
