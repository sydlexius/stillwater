package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

// fakeMergeRefresher implements artist.PlatformMergeRefresher, recording the
// survivor + connection set the handler-driven MergeAndReconcile passes and
// returning an OK outcome per connection so the payload mapping is exercised.
type fakeMergeRefresher struct {
	gotSurvivor string
	gotConns    []string
}

func (f *fakeMergeRefresher) SyncMergeRefresh(_ context.Context, survivorID string, connectionIDs []string) ([]artist.PlatformRefreshResult, error) {
	f.gotSurvivor = survivorID
	f.gotConns = connectionIDs
	out := make([]artist.PlatformRefreshResult, 0, len(connectionIDs))
	for _, c := range connectionIDs {
		out = append(out, artist.PlatformRefreshResult{ConnectionID: c, Result: artist.PlatformRemapOK})
	}
	return out, nil
}

// seedNonCanonicalMergeFixture is like seedMergeFixture but the survivor's
// directory basename ("Cure, The") is NOT canonical for its name ("The Cure")
// in prefix mode, so a committed MergeAndReconcile relocates it.
func seedNonCanonicalMergeFixture(t *testing.T, svc *artist.Service, db *sql.DB) (survivorID, loserID string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at) VALUES ('lib-merge-nc', 'lib-merge-nc', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seed library: %v", err)
	}
	survivorPath := filepath.Join(root, "Cure, The") // non-canonical in prefix mode
	loserPath := filepath.Join(root, "Cure Dup")
	for _, p := range []string{survivorPath, loserPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	if err := os.Mkdir(filepath.Join(survivorPath, "Album A"), 0o755); err != nil {
		t.Fatalf("mkdir survivor album: %v", err)
	}
	if err := os.Mkdir(filepath.Join(loserPath, "Album B"), 0o755); err != nil {
		t.Fatalf("mkdir loser album: %v", err)
	}
	survivor := &artist.Artist{Name: "The Cure", SortName: "Cure, The", Path: survivorPath, LibraryID: "lib-merge-nc"}
	loser := &artist.Artist{Name: "The Cure", SortName: "Cure, The", Path: loserPath, LibraryID: "lib-merge-nc"}
	if err := svc.Create(ctx, survivor); err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	if err := svc.Create(ctx, loser); err != nil {
		t.Fatalf("Create loser: %v", err)
	}
	return survivor.ID, loser.ID
}

// TestHandleArtistsMerge_PlatformRefresh proves the handler routes through
// MergeAndReconcile and surfaces the fan-out outcomes: with a refresher wired
// and the survivor mapped to a connection, the response carries platform_refresh
// keyed by that connection.
func TestHandleArtistsMerge_PlatformRefresh(t *testing.T) {
	t.Parallel()
	r, svc, db := mergeTestRouter(t)
	ref := &fakeMergeRefresher{}
	svc.SetPlatformMergeRefresher(ref)
	survivorID, loserID, _ := seedMergeFixture(t, svc, db)

	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at) VALUES ('conn-emby', 'conn-emby', 'emby', 'http://x:8096', 'enc-key', 1, 'ok', datetime('now'), datetime('now'))`,
	); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	if err := svc.SetPlatformID(ctx, survivorID, "conn-emby", "emby-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	rec := httptest.NewRecorder()
	r.handleArtistsMerge(rec, adminReq(t, map[string]any{
		"survivor_id": survivorID, "loser_ids": []string{loserID}, "dry_run": false,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got mergeResultPayload
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got.PlatformRefresh) != 1 || got.PlatformRefresh[0].ConnectionID != "conn-emby" || got.PlatformRefresh[0].Result != "ok" {
		t.Errorf("platform_refresh = %+v, want one conn-emby ok entry", got.PlatformRefresh)
	}
	if ref.gotSurvivor != survivorID {
		t.Errorf("refresher survivor = %q, want %q", ref.gotSurvivor, survivorID)
	}
	// Survivor "The Cure" is already prefix-canonical: no rename in the payload.
	if got.CanonicalRename != nil {
		t.Errorf("canonical_rename = %+v, want nil (survivor already canonical)", got.CanonicalRename)
	}
}

// TestHandleArtistsMerge_CanonicalRename proves the canonical_rename payload is
// surfaced when a committed merge relocates a non-canonical survivor directory.
func TestHandleArtistsMerge_CanonicalRename(t *testing.T) {
	t.Parallel()
	r, svc, db := mergeTestRouter(t)
	survivorID, loserID := seedNonCanonicalMergeFixture(t, svc, db)

	rec := httptest.NewRecorder()
	r.handleArtistsMerge(rec, adminReq(t, map[string]any{
		"survivor_id": survivorID, "loser_ids": []string{loserID}, "dry_run": false,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got mergeResultPayload
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.CanonicalRename == nil {
		t.Fatalf("canonical_rename = nil, want non-nil (survivor path %s)", got.SurvivorPath)
	}
	if filepath.Base(got.CanonicalRename.NewPath) != "The Cure" {
		t.Errorf("canonical_rename.new_path base = %s, want \"The Cure\"", filepath.Base(got.CanonicalRename.NewPath))
	}
	if filepath.Base(got.CanonicalRename.OldPath) != "Cure, The" {
		t.Errorf("canonical_rename.old_path base = %s, want \"Cure, The\"", filepath.Base(got.CanonicalRename.OldPath))
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

// TestRespondMergeError_SurvivorMissing exercises the 422 split: the
// handler distinguishes ErrMergeSurvivorMissing from ErrMergeStaleGroup
// so the UI can show a specific message ("pick a different survivor")
// instead of conflating both as "stale group". The orchestrator never
// reaches ErrMergeSurvivorMissing through the public path because
// resolveGroupMembers already verifies every requested ID (including
// survivor) is in the group, so we test the handler branch directly.
//
// TG7 in the PR #1654 triage doc.
func TestRespondMergeError_SurvivorMissing(t *testing.T) {
	t.Parallel()
	r, _, _ := mergeTestRouter(t)

	rec := httptest.NewRecorder()
	r.respondMergeError(rec, artist.ErrMergeSurvivorMissing, nil)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	var got map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got["error"] != "survivor_missing" {
		t.Errorf("error = %q, want survivor_missing (must NOT conflate with stale_group)", got["error"])
	}
}

// TestRespondMergeError_StaleGroupDistinct mirrors TestRespondMergeError_SurvivorMissing:
// the original stale_group code stays untouched after the survivor_missing
// split, so we pin both branches to guard the dispatcher against future
// regressions that re-merge the cases.
func TestRespondMergeError_StaleGroupDistinct(t *testing.T) {
	t.Parallel()
	r, _, _ := mergeTestRouter(t)

	rec := httptest.NewRecorder()
	r.respondMergeError(rec, artist.ErrMergeStaleGroup, nil)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	var got map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got["error"] != "stale_group" {
		t.Errorf("error = %q, want stale_group", got["error"])
	}
}

// TestRespondMergeError_CollisionsAlwaysHasConflicts pins the 409
// collisions contract: the `conflicts` array MUST be present even when
// the orchestrator returns a nil result. The schema's oneOf requires
// `conflicts` on the collisions variant.
func TestRespondMergeError_CollisionsAlwaysHasConflicts(t *testing.T) {
	t.Parallel()
	r, _, _ := mergeTestRouter(t)

	// Case 1: nil result.
	rec := httptest.NewRecorder()
	r.respondMergeError(rec, artist.ErrMergeCollisions, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var got map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got["conflicts"]; !ok {
		t.Errorf("conflicts key missing when result is nil; got %v", got)
	}
	conflicts, ok := got["conflicts"].([]any)
	if !ok {
		t.Errorf("conflicts must be an array, got %T", got["conflicts"])
	}
	if len(conflicts) != 0 {
		t.Errorf("conflicts should be empty array when result is nil, got %v", conflicts)
	}

	// Case 2: result with conflicts.
	rec = httptest.NewRecorder()
	r.respondMergeError(rec, artist.ErrMergeCollisions, &artist.MergeResult{
		SurvivorID:   "sid",
		SurvivorPath: "/x/y",
		Conflicts: []artist.ConflictItem{
			{Name: "Disintegration", SurvivorPath: "/x/y/Disintegration", LoserPath: "/x/z/Disintegration"},
		},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (with result)", rec.Code)
	}
	got = nil
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode (with result): %v", err)
	}
	conflicts, ok = got["conflicts"].([]any)
	if !ok || len(conflicts) != 1 {
		t.Errorf("expected 1 conflict, got %v", got["conflicts"])
	}
	if got["survivor_id"] != "sid" {
		t.Errorf("survivor_id missing or wrong: %v", got["survivor_id"])
	}
}

// TestRespondMergeError_SanitizedMessages verifies the handler does not
// leak raw err.Error() to the client for the four sentinel branches that
// previously did. Each branch must respond with a fixed human message
// and the documented stable error code.
func TestRespondMergeError_SanitizedMessages(t *testing.T) {
	t.Parallel()
	r, _, _ := mergeTestRouter(t)

	// Wrap each sentinel with a noisy internal-looking suffix the
	// handler must NOT propagate to the client.
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantError   string
		mustNotLeak string
	}{
		{"invalid_request", fmt.Errorf("%w: secret/internal/path", artist.ErrMergeInvalidRequest),
			http.StatusBadRequest, "invalid_request", "secret/internal/path"},
		{"stale_group", fmt.Errorf("%w: internal-detail", artist.ErrMergeStaleGroup),
			http.StatusUnprocessableEntity, "stale_group", "internal-detail"},
		{"survivor_missing", fmt.Errorf("%w: internal-detail", artist.ErrMergeSurvivorMissing),
			http.StatusUnprocessableEntity, "survivor_missing", "internal-detail"},
		{"locked", fmt.Errorf("%w: id-a,id-b", artist.ErrMergeLocked),
			http.StatusLocked, "locked", "id-a,id-b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.respondMergeError(rec, tc.err, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"error":"`+tc.wantError+`"`) {
				t.Errorf("body missing error=%s: %s", tc.wantError, body)
			}
			if strings.Contains(body, tc.mustNotLeak) {
				t.Errorf("body leaked internal detail %q: %s", tc.mustNotLeak, body)
			}
		})
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
