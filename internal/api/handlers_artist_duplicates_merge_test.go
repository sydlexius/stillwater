package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/rule"
)

// mergeTestRouter wires the minimum Router surface the merge handler needs.
// Auth is bypassed at the middleware layer; the handler's requireForeignAdmin
// gate is exercised by tests that omit or downgrade the admin context.
func mergeTestRouter(t *testing.T) (*Router, *artist.Service, *sql.DB) {
	t.Helper()

	db := newTestDB(t)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	r := &Router{
		logger:        logger,
		artistService: artistSvc,
		ruleService:   ruleSvc,
		authService:   authSvc,
		db:            db,
	}
	return r, artistSvc, db
}

// seedMergeFixture creates a library plus two near-duplicate artists with
// non-overlapping album subdirs and a loose file on the loser. Returns the
// IDs and the on-disk root so individual tests can mutate the layout (e.g.
// to inject a collision).
func seedMergeFixture(t *testing.T, svc *artist.Service, db *sql.DB) (survivorID, loserID, root string) {
	t.Helper()
	ctx := context.Background()
	root = t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at) VALUES ('lib-merge-api', 'lib-merge-api', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seed library: %v", err)
	}

	survivorPath := filepath.Join(root, "The Cure")
	loserPath := filepath.Join(root, "Cure, The")
	for _, p := range []string{survivorPath, loserPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	if err := os.Mkdir(filepath.Join(survivorPath, "Disintegration"), 0o755); err != nil {
		t.Fatalf("mkdir survivor album: %v", err)
	}
	for _, album := range []string{"Pornography", "Bloodflowers"} {
		if err := os.Mkdir(filepath.Join(loserPath, album), 0o755); err != nil {
			t.Fatalf("mkdir loser album %s: %v", album, err)
		}
	}
	if err := os.WriteFile(filepath.Join(loserPath, "artist.nfo"), []byte("loser-nfo"), 0o600); err != nil {
		t.Fatalf("write loser nfo: %v", err)
	}

	survivor := &artist.Artist{Name: "The Cure", SortName: "Cure, The", Path: survivorPath, LibraryID: "lib-merge-api"}
	loser := &artist.Artist{Name: "The Cure", SortName: "Cure, The", Path: loserPath, LibraryID: "lib-merge-api"}
	if err := svc.Create(ctx, survivor); err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	if err := svc.Create(ctx, loser); err != nil {
		t.Fatalf("Create loser: %v", err)
	}
	return survivor.ID, loser.ID, root
}

// adminReq builds a POST /api/v1/artists/merge request with an admin auth
// context attached, ready for direct handler invocation.
func adminReq(t *testing.T, body any) *http.Request {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/merge", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	ctx := middleware.WithTestUserID(req.Context(), "admin-1")
	ctx = middleware.WithTestRole(ctx, "administrator")
	return req.WithContext(ctx)
}

func TestHandleArtistsMerge_Happy(t *testing.T) {
	t.Parallel()
	r, svc, db := mergeTestRouter(t)
	survivorID, loserID, _ := seedMergeFixture(t, svc, db)

	req := adminReq(t, map[string]any{
		"survivor_id": survivorID,
		"loser_ids":   []string{loserID},
		"dry_run":     false,
	})
	rec := httptest.NewRecorder()
	r.handleArtistsMerge(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got mergeResultPayload
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.SurvivorID != survivorID {
		t.Errorf("survivor_id = %q, want %q", got.SurvivorID, survivorID)
	}
	if len(got.Moved) != 3 {
		t.Errorf("len(moved) = %d, want 3", len(got.Moved))
	}
	if len(got.LosersDeleted) != 1 {
		t.Errorf("len(losers_deleted) = %d, want 1", len(got.LosersDeleted))
	}
}

func TestHandleArtistsMerge_DryRun(t *testing.T) {
	t.Parallel()
	r, svc, db := mergeTestRouter(t)
	survivorID, loserID, _ := seedMergeFixture(t, svc, db)

	req := adminReq(t, map[string]any{
		"survivor_id": survivorID,
		"loser_ids":   []string{loserID},
		"dry_run":     true,
	})
	rec := httptest.NewRecorder()
	r.handleArtistsMerge(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got mergeResultPayload
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !got.DryRun {
		t.Errorf("dry_run = false, want true")
	}
	if len(got.LosersDeleted) != 0 {
		t.Errorf("dry-run deleted %d losers, want 0", len(got.LosersDeleted))
	}
}

func TestHandleArtistsMerge_Collisions(t *testing.T) {
	t.Parallel()
	r, svc, db := mergeTestRouter(t)
	survivorID, loserID, root := seedMergeFixture(t, svc, db)
	// Inject a collision so the pre-flight walk halts.
	if err := os.Mkdir(filepath.Join(root, "Cure, The", "Disintegration"), 0o755); err != nil {
		t.Fatalf("mkdir collision: %v", err)
	}

	req := adminReq(t, map[string]any{
		"survivor_id": survivorID,
		"loser_ids":   []string{loserID},
	})
	rec := httptest.NewRecorder()
	r.handleArtistsMerge(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got["error"] != "collisions" {
		t.Errorf("error = %v, want collisions", got["error"])
	}
	conflicts, ok := got["conflicts"].([]any)
	if !ok || len(conflicts) == 0 {
		t.Errorf("expected conflicts in response, got %v", got)
	}
}

// NOTE: merge_in_progress (409) is exercised at the orchestrator layer in
// internal/artist/merge_artists_test.go (TestMergeArtists_InProgress and
// TestMergeArtists_RaceWithSelf). The handler-side mapping is one
// errors.Is branch in respondMergeError, so duplicating the singleton
// setup here would add risk (cross-package access to mergeMu) without
// adding coverage.

func TestHandleArtistsMerge_Locked(t *testing.T) {
	t.Parallel()
	r, svc, db := mergeTestRouter(t)
	survivorID, loserID, _ := seedMergeFixture(t, svc, db)
	if err := svc.Lock(context.Background(), loserID, "user"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	req := adminReq(t, map[string]any{
		"survivor_id": survivorID,
		"loser_ids":   []string{loserID},
	})
	rec := httptest.NewRecorder()
	r.handleArtistsMerge(rec, req)
	if rec.Code != http.StatusLocked {
		t.Fatalf("status = %d, want 423; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got["error"] != "locked" {
		t.Errorf("error = %q, want locked", got["error"])
	}
}

func TestHandleArtistsMerge_StaleGroup(t *testing.T) {
	t.Parallel()
	r, svc, db := mergeTestRouter(t)
	survivorID, _, _ := seedMergeFixture(t, svc, db)

	// Pair the survivor with an unrelated artist that was never in its
	// group. Re-validation should refuse with 422 stale_group.
	other := &artist.Artist{
		Name: "Radiohead", SortName: "Radiohead",
		Path: filepath.Join(t.TempDir(), "Radiohead"), LibraryID: "lib-merge-api",
	}
	if err := os.MkdirAll(other.Path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := svc.Create(context.Background(), other); err != nil {
		t.Fatalf("Create other: %v", err)
	}

	req := adminReq(t, map[string]any{
		"survivor_id": survivorID,
		"loser_ids":   []string{other.ID},
	})
	rec := httptest.NewRecorder()
	r.handleArtistsMerge(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleArtistsMerge_Malformed(t *testing.T) {
	t.Parallel()
	r, _, _ := mergeTestRouter(t)

	// Bad JSON.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/merge", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	ctx := middleware.WithTestUserID(req.Context(), "admin-1")
	ctx = middleware.WithTestRole(ctx, "administrator")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	r.handleArtistsMerge(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleArtistsMerge_InvalidRequest(t *testing.T) {
	t.Parallel()
	r, svc, db := mergeTestRouter(t)
	survivorID, _, _ := seedMergeFixture(t, svc, db)

	// Survivor in loser_ids should be rejected.
	req := adminReq(t, map[string]any{
		"survivor_id": survivorID,
		"loser_ids":   []string{survivorID},
	})
	rec := httptest.NewRecorder()
	r.handleArtistsMerge(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleArtistsMerge_NonAdmin(t *testing.T) {
	t.Parallel()
	r, svc, db := mergeTestRouter(t)
	survivorID, loserID, _ := seedMergeFixture(t, svc, db)

	buf, err := json.Marshal(map[string]any{
		"survivor_id": survivorID,
		"loser_ids":   []string{loserID},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/merge", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	// User present but role is operator -> 403.
	ctx := middleware.WithTestUserID(req.Context(), "user-1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	r.handleArtistsMerge(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
