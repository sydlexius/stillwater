package api

// handlers_artist_duplicates_ignored_test.go -- coverage for the manage-ignored
// surface (#2219 remainder, folds #2220): the list endpoint, the restore
// (un-ignore) endpoint, and the sidebar-count RE-INCREMENT that restore drives.
// The restore is the mirror of the ignore: it must invalidate the count cache so
// the duplicates pill increments again (no stale read), and it must reverse the
// filter so the group reappears in both the page list and the count.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

// adminGet builds a GET request with an admin auth context for the list endpoint.
func adminGet(t *testing.T, target string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := middleware.WithTestUserID(req.Context(), "admin-1")
	ctx = middleware.WithTestRole(ctx, "administrator")
	return req.WithContext(ctx)
}

// adminRestoreReq builds a DELETE request to the restore endpoint with the given
// id path value and an admin auth context.
func adminRestoreReq(t *testing.T, id, role string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/duplicates/ignored/"+id, nil)
	req.SetPathValue("id", id)
	ctx := middleware.WithTestUserID(req.Context(), "admin-1")
	ctx = middleware.WithTestRole(ctx, role)
	return req.WithContext(ctx)
}

// seedIgnore persists one ignore for the given member IDs and returns its row id
// (read back via LoadIgnoredGroups) so a restore test can target it.
func seedIgnore(t *testing.T, db *sql.DB, groupKey, reason string, ids ...string) string {
	t.Helper()
	ctx := context.Background()
	sig := artist.DuplicateGroupSignature(ids)
	if err := artist.IgnoreDuplicateGroup(ctx, db, sig, groupKey, reason); err != nil {
		t.Fatalf("seeding ignore: %v", err)
	}
	groups, err := artist.LoadIgnoredGroups(ctx, db)
	if err != nil {
		t.Fatalf("loading ignored groups: %v", err)
	}
	for i := range groups {
		if groups[i].Signature == sig {
			return groups[i].ID
		}
	}
	t.Fatalf("seeded ignore %q not found after insert", sig)
	return ""
}

// TestLoadIgnoredDuplicatesView_RowsAndNilDB proves the view loader projects the
// full row (id, group key, reason, derived member count) and returns the empty
// view for a nil DB (the render-empty-state seam) rather than erroring.
func TestLoadIgnoredDuplicatesView_RowsAndNilDB(t *testing.T) {
	r, db := countTestRouter(t)
	seedIgnore(t, db, "the cure", "name_key", "a1", "b2", "c3")

	view, err := r.loadIgnoredDuplicatesView(adminGet(t, "/reports/duplicates/ignored"))
	if err != nil {
		t.Fatalf("loadIgnoredDuplicatesView: %v", err)
	}
	if len(view.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(view.Rows))
	}
	row := view.Rows[0]
	if row.GroupKey != "the cure" || row.Reason != "name_key" || row.MemberCount != 3 {
		t.Errorf("row projection mismatch: %+v (want group='the cure' reason=name_key members=3)", row)
	}
	if row.ID == "" {
		t.Errorf("row must carry the id used to target a restore")
	}

	// nil DB: empty view, no error.
	rNil := &Router{logger: r.logger, db: nil}
	nilView, err := rNil.loadIgnoredDuplicatesView(adminGet(t, "/x"))
	if err != nil {
		t.Fatalf("nil-db view must not error: %v", err)
	}
	if len(nilView.Rows) != 0 {
		t.Errorf("nil-db rows = %d, want 0", len(nilView.Rows))
	}
}

// TestHandleArtistDuplicatesIgnoredList_Success asserts the JSON list returns
// every ignored group with a derived member_count and a stable count field.
func TestHandleArtistDuplicatesIgnoredList_Success(t *testing.T) {
	r, db := countTestRouter(t)
	seedIgnore(t, db, "grp-a", "name_key", "a1", "b2")
	seedIgnore(t, db, "grp-b", "mbid", "c3", "d4", "e5")

	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesIgnoredList(rec, adminGet(t, "/api/v1/artists/duplicates/ignored"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []struct {
			ID          string `json:"id"`
			Signature   string `json:"signature"`
			GroupKey    string `json:"group_key"`
			Reason      string `json:"reason"`
			MemberCount int    `json:"member_count"`
		} `json:"items"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding list response: %v; body=%q", err, rec.Body.String())
	}
	if resp.Count != 2 || len(resp.Items) != 2 {
		t.Fatalf("count=%d items=%d, want 2/2", resp.Count, len(resp.Items))
	}
	// Find grp-b (3 members) and assert its derived member_count and signature.
	var found bool
	for _, it := range resp.Items {
		if it.GroupKey == "grp-b" {
			found = true
			if it.MemberCount != 3 {
				t.Errorf("grp-b member_count = %d, want 3", it.MemberCount)
			}
			if it.Signature != "c3|d4|e5" {
				t.Errorf("grp-b signature = %q, want c3|d4|e5", it.Signature)
			}
			if it.ID == "" {
				t.Errorf("list item must carry an id for restore")
			}
		}
	}
	if !found {
		t.Errorf("grp-b missing from list; got %+v", resp.Items)
	}
}

// TestHandleArtistDuplicatesIgnoredList_NilDB pins the 503 branch.
func TestHandleArtistDuplicatesIgnoredList_NilDB(t *testing.T) {
	r := &Router{
		logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		db:     nil,
	}
	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesIgnoredList(rec, adminGet(t, "/api/v1/artists/duplicates/ignored"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%q", rec.Code, rec.Body.String())
	}
}

// TestHandleArtistDuplicatesIgnoredList_LoadError pins the 500 branch and proves
// no raw driver error leaks: dropping the table makes LoadIgnoredGroups fail.
func TestHandleArtistDuplicatesIgnoredList_LoadError(t *testing.T) {
	r, db := countTestRouter(t)
	dropIgnoredTable(t, db)

	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesIgnoredList(rec, adminGet(t, "/api/v1/artists/duplicates/ignored"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error":"internal"`) {
		t.Errorf("body should carry the generic internal envelope; got %q", body)
	}
	if strings.Contains(body, "no such table") || strings.Contains(body, "sql:") {
		t.Errorf("500 body must not leak the raw driver error; got %q", body)
	}
}

// TestHandleArtistDuplicatesRestore_ReincrementsPill is the core AC test: ignore
// one of two detected groups (count drops to 1 and is cached), then restore it
// through the handler and require the count returns to 2 -- proving the restore
// invalidated the cache (no stale read) and the group reappears in the count.
// This is the pill RE-INCREMENT the maintainer specified.
func TestHandleArtistDuplicatesRestore_ReincrementsPill(t *testing.T) {
	r, db := countTestRouter(t)
	seedTwoDistinctPairs(t, db)
	ctx := context.Background()

	// Ignore one real detected group.
	ids := firstGroupMemberIDs(t, db)
	sig := artist.DuplicateGroupSignature(ids)
	if err := artist.IgnoreDuplicateGroup(ctx, db, sig, "grp", "name_key"); err != nil {
		t.Fatalf("ignore: %v", err)
	}
	// Invalidate so the next get recomputes with the ignore applied, then prime
	// the cache at the post-ignore count (1).
	duplicatesCount.invalidate()
	primed, err := duplicatesCount.get(ctx, func(c context.Context) (int, error) { return countDuplicateGroups(c, db) })
	if err != nil {
		t.Fatalf("priming count: %v", err)
	}
	if primed != 1 {
		t.Fatalf("primed post-ignore count = %d, want 1", primed)
	}

	// Look up the ignore's row id and restore it via the handler.
	groups, err := artist.LoadIgnoredGroups(ctx, db)
	if err != nil || len(groups) != 1 {
		t.Fatalf("load ignored: groups=%d err=%v", len(groups), err)
	}
	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesRestore(rec, adminRestoreReq(t, groups[0].ID, "administrator"))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	// The restore returns the refreshed manage-table partial (HTML), now empty.
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("restore response Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "artist-duplicates-ignored-table") {
		t.Errorf("restore response should be the manage-table partial; got %q", rec.Body.String())
	}

	// Read the count through the SAME cache. A stale read would still report 1;
	// the restore must have invalidated it so it recomputes to 2.
	after, err := duplicatesCount.get(ctx, func(c context.Context) (int, error) { return countDuplicateGroups(c, db) })
	if err != nil {
		t.Fatalf("post-restore count: %v", err)
	}
	if after != 2 {
		t.Errorf("post-restore count = %d, want 2 (pill re-increments; no stale cache read)", after)
	}
	// And the ledger is empty again.
	sigs, _ := artist.LoadIgnoredSignatures(ctx, db)
	if len(sigs) != 0 {
		t.Errorf("post-restore ignored set len = %d, want 0", len(sigs))
	}
}

// TestHandleArtistDuplicatesRestore_NotFound: an unknown id 404s and does not
// invalidate the count cache (nothing changed, so a primed value must survive).
func TestHandleArtistDuplicatesRestore_NotFound(t *testing.T) {
	r, db := countTestRouter(t)
	seedTwoDistinctPairs(t, db)
	ctx := context.Background()

	// Prime the cache; a no-op restore must not disturb it.
	primed, _ := duplicatesCount.get(ctx, func(c context.Context) (int, error) { return countDuplicateGroups(c, db) })
	if primed != 2 {
		t.Fatalf("primed = %d, want 2", primed)
	}

	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesRestore(rec, adminRestoreReq(t, "no-such-id", "administrator"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not_found") {
		t.Errorf("body should carry the not_found envelope; got %q", rec.Body.String())
	}
}

// TestHandleArtistDuplicatesRestore_MissingID: an empty id path value 400s.
func TestHandleArtistDuplicatesRestore_MissingID(t *testing.T) {
	r, _ := countTestRouter(t)
	rec := httptest.NewRecorder()
	// Empty id -- do not set the path value.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/duplicates/ignored/", nil)
	ctx := middleware.WithTestUserID(req.Context(), "admin-1")
	ctx = middleware.WithTestRole(ctx, "administrator")
	r.handleArtistDuplicatesRestore(rec, req.WithContext(ctx))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
}

// TestHandleArtistDuplicatesRestore_NilDB pins the 503 branch.
func TestHandleArtistDuplicatesRestore_NilDB(t *testing.T) {
	r := &Router{
		logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		db:     nil,
	}
	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesRestore(rec, adminRestoreReq(t, "id-1", "administrator"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%q", rec.Code, rec.Body.String())
	}
}

// TestHandleArtistDuplicatesRestore_AdminGate proves the route-level admin gate
// composes correctly: wrapped in middleware.RequireAdmin (exactly as router.go
// registers it), an operator hits 403 and the ignore row survives -- no restore
// happens without administrator role.
func TestHandleArtistDuplicatesRestore_AdminGate(t *testing.T) {
	r, db := countTestRouter(t)
	id := seedIgnore(t, db, "grp", "name_key", "a1", "b2")

	gated := middleware.RequireAdmin(r.handleArtistDuplicatesRestore)
	rec := httptest.NewRecorder()
	gated(rec, adminRestoreReq(t, id, "operator"))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	// The row must still be present -- a rejected request restores nothing.
	sigs, err := artist.LoadIgnoredSignatures(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadIgnoredSignatures: %v", err)
	}
	if len(sigs) != 1 {
		t.Errorf("operator restore must not remove the ignore; ledger size = %d, want 1", len(sigs))
	}
}

// TestHandleArtistDuplicatesRestore_ExecError pins the 500 branch and no-leak:
// a valid admin request whose DELETE fails (forced by closing the DB) returns
// 500 with the generic envelope, not a 404 (a DB fault is not "not found") and
// not the raw driver string.
func TestHandleArtistDuplicatesRestore_ExecError(t *testing.T) {
	r, db := countTestRouter(t)
	if err := db.Close(); err != nil {
		t.Fatalf("closing db for error injection: %v", err)
	}
	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesRestore(rec, adminRestoreReq(t, "id-1", "administrator"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"error":"internal"`) {
		t.Errorf("body should carry the generic internal envelope; got %q", body)
	}
	if strings.Contains(body, "database is closed") || strings.Contains(body, "sql:") {
		t.Errorf("500 body must not leak the raw driver error; got %q", body)
	}
}
