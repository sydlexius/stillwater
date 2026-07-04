package api

// handlers_artist_duplicates_ignore_test.go -- coverage for the server-side
// ignore endpoint (#2219) and the sidebar-count decrement it drives. The
// endpoint is admin-only, idempotent, and invalidates the count cache so the
// pill drops on the next poll (the stale-count fix).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
)

// ignoreReq builds a POST request to the ignore endpoint with an admin auth
// context. body is JSON-encoded; pass a raw string via rawIgnoreReq for the
// malformed-body case.
func ignoreReq(t *testing.T, body any) *http.Request {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/duplicates/ignore", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	ctx := middleware.WithTestUserID(req.Context(), "admin-1")
	ctx = middleware.WithTestRole(ctx, "administrator")
	return req.WithContext(ctx)
}

// seedTwoDistinctPairs inserts two independent near-duplicate pairs so
// DetectDuplicates surfaces exactly two groups. Returns nothing; tests that
// need concrete member IDs read them back via artist.DetectDuplicates.
func seedTwoDistinctPairs(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	mustInsert := func(name, path string) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
			 VALUES (lower(hex(randomblob(16))), ?, ?, ?, datetime('now'), datetime('now'))`,
			name, name, path,
		); err != nil {
			t.Fatalf("seeding artist %q: %v", name, err)
		}
	}
	curly := string([]rune{0x2019})
	// Pair 1: apostrophe variant.
	mustInsert("Caedmon's Call", "/music/Caedmon's Call")
	mustInsert("Caedmon"+curly+"s Call", "/music/Caedmon2")
	// Pair 2: article variant ("The Cure" vs "Cure, The" both normalize equal).
	mustInsert("The Cure", "/music/The Cure")
	mustInsert("Cure, The", "/music/Cure, The")
}

// firstGroupMemberIDs runs the detector and returns the member IDs of the first
// detected group, so a test can ignore a real group by its exact signature.
func firstGroupMemberIDs(t *testing.T, db *sql.DB) []string {
	t.Helper()
	groups, err := artist.DetectDuplicates(context.Background(), db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}
	if len(groups) == 0 {
		t.Fatalf("expected at least one duplicate group to ignore")
	}
	ids := make([]string, 0, len(groups[0].Members))
	for _, m := range groups[0].Members {
		ids = append(ids, m.ID)
	}
	return ids
}

// TestHandleArtistDuplicatesIgnore_NonAdmin asserts the admin gate: an operator
// hits 403 and never persists anything.
func TestHandleArtistDuplicatesIgnore_NonAdmin(t *testing.T) {
	r, db := countTestRouter(t)
	seedTwoDistinctPairs(t, db)

	buf, _ := json.Marshal(ignoreDuplicateRequest{MemberIDs: []string{"a", "b"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/duplicates/ignore", bytes.NewReader(buf))
	ctx := middleware.WithTestUserID(req.Context(), "user-1")
	ctx = middleware.WithTestRole(ctx, "operator")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesIgnore(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	got, err := artist.LoadIgnoredSignatures(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadIgnoredSignatures: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("non-admin request must not persist an ignore; found %d rows", len(got))
	}
}

// TestHandleArtistDuplicatesIgnore_MalformedBody asserts a non-JSON body 400s.
func TestHandleArtistDuplicatesIgnore_MalformedBody(t *testing.T) {
	r, _ := countTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/duplicates/ignore",
		strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	ctx := middleware.WithTestUserID(req.Context(), "admin-1")
	ctx = middleware.WithTestRole(ctx, "administrator")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesIgnore(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Errorf("body should carry the invalid_request envelope; got %q", rec.Body.String())
	}
}

// TestHandleArtistDuplicatesIgnore_EmptyMemberIDs asserts that a body with no
// usable member IDs (empty signature) 400s rather than persisting an empty key.
func TestHandleArtistDuplicatesIgnore_EmptyMemberIDs(t *testing.T) {
	r, db := countTestRouter(t)

	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesIgnore(rec, ignoreReq(t, ignoreDuplicateRequest{MemberIDs: []string{"", ""}}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	got, _ := artist.LoadIgnoredSignatures(context.Background(), db)
	if len(got) != 0 {
		t.Errorf("empty member_ids must not persist a row; found %d", len(got))
	}
}

// TestHandleArtistDuplicatesIgnore_SuccessAndIdempotent asserts a successful
// ignore persists exactly one row, returns 200 with the signature, and that
// re-issuing the identical ignore is idempotent (200, still one row).
func TestHandleArtistDuplicatesIgnore_SuccessAndIdempotent(t *testing.T) {
	r, db := countTestRouter(t)

	body := ignoreDuplicateRequest{MemberIDs: []string{"b2", "a1"}, GroupKey: "the cure", Reason: "name_key"}

	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesIgnore(rec, ignoreReq(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("first ignore status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	// The canonical signature (sorted, pipe-joined) must be echoed back.
	if !strings.Contains(rec.Body.String(), `"signature":"a1|b2"`) {
		t.Errorf("response should echo the canonical signature a1|b2; got %q", rec.Body.String())
	}

	// Idempotent replay: same members, still 200, still a single row.
	rec2 := httptest.NewRecorder()
	r.handleArtistDuplicatesIgnore(rec2, ignoreReq(t, body))
	if rec2.Code != http.StatusOK {
		t.Fatalf("idempotent ignore status = %d, want 200; body=%q", rec2.Code, rec2.Body.String())
	}

	got, err := artist.LoadIgnoredSignatures(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadIgnoredSignatures: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("persisted signature count = %d, want 1 (idempotent)", len(got))
	}
	if _, ok := got["a1|b2"]; !ok {
		t.Errorf("persisted set missing a1|b2; got %+v", got)
	}
}

// TestCountDecrementsOnIgnore is the stale-count fix's core assertion: prime the
// sidebar count so the module cache holds it, ignore one real group, then read
// the count again and require it dropped by one. A stale cache read would still
// report the pre-ignore count, so this proves the ignore handler invalidated the
// cache.
func TestCountDecrementsOnIgnore(t *testing.T) {
	r, db := countTestRouter(t)
	seedTwoDistinctPairs(t, db)

	// Prime: populate the module cache with the pre-ignore count (2 groups).
	before, err := duplicatesCount.get(context.Background(), func(ctx context.Context) (int, error) {
		return countDuplicateGroups(ctx, db)
	})
	if err != nil {
		t.Fatalf("priming count: %v", err)
	}
	if before != 2 {
		t.Fatalf("primed count = %d, want 2 (two seeded pairs)", before)
	}

	// Ignore one real group via the handler (which invalidates the cache).
	ids := firstGroupMemberIDs(t, db)
	rec := httptest.NewRecorder()
	r.handleArtistDuplicatesIgnore(rec, ignoreReq(t, ignoreDuplicateRequest{MemberIDs: ids}))
	if rec.Code != http.StatusOK {
		t.Fatalf("ignore status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}

	// Read the count again through the SAME cache. If the handler had not
	// invalidated, this would return the stale 2; it must recompute to 1.
	after, err := duplicatesCount.get(context.Background(), func(ctx context.Context) (int, error) {
		return countDuplicateGroups(ctx, db)
	})
	if err != nil {
		t.Fatalf("post-ignore count: %v", err)
	}
	if after != 1 {
		t.Errorf("post-ignore count = %d, want 1 (decrement after ignore; no stale cache read)", after)
	}
}

// TestCountDuplicateGroupsExcludesIgnored pins the filter at the count layer
// directly (no HTTP): a persisted ignore drops the matching group from the count.
func TestCountDuplicateGroupsExcludesIgnored(t *testing.T) {
	_, db := countTestRouter(t)
	seedTwoDistinctPairs(t, db)
	ctx := context.Background()

	n, err := countDuplicateGroups(ctx, db)
	if err != nil {
		t.Fatalf("countDuplicateGroups: %v", err)
	}
	if n != 2 {
		t.Fatalf("baseline count = %d, want 2", n)
	}

	// Ignore one real group by its exact signature.
	ids := firstGroupMemberIDs(t, db)
	if err := artist.IgnoreDuplicateGroup(ctx, db, artist.DuplicateGroupSignature(ids), "", ""); err != nil {
		t.Fatalf("IgnoreDuplicateGroup: %v", err)
	}

	n2, err := countDuplicateGroups(ctx, db)
	if err != nil {
		t.Fatalf("countDuplicateGroups after ignore: %v", err)
	}
	if n2 != 1 {
		t.Errorf("count after ignore = %d, want 1 (ignored group excluded)", n2)
	}
}
