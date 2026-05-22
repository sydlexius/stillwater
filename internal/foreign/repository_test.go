package foreign

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedRow inserts a foreign_files row using raw SQL into the same *sql.DB
// that backs the repository. contentHash may be empty to simulate pre-008
// rows.
func seedRow(t *testing.T, repo *Repository, id, artistID, filePath, fileName, contentHash string) {
	t.Helper()
	var hashArg interface{}
	if contentHash != "" {
		hashArg = contentHash
	}
	_, err := repo.db.Exec(
		`INSERT INTO foreign_files (id, artist_id, file_path, file_name, content_hash, size_bytes, detected_at)
		 VALUES (?, ?, ?, ?, ?, 100, ?)`,
		id, artistID, filePath, fileName, hashArg, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("seedRow(%q, %q): %v", artistID, filePath, err)
	}
}

// seedArtist inserts an artists row needed by the LEFT JOIN in List/ListRaw.
func seedArtist(t *testing.T, repo *Repository, id, name string) {
	t.Helper()
	_, err := repo.db.Exec(`INSERT INTO artists (id, name, path) VALUES (?, ?, '')`, id, name)
	if err != nil {
		t.Fatalf("seedArtist(%q): %v", id, err)
	}
}

// TestList_CollapsesByContentHash verifies that two ledger rows sharing a
// non-empty content_hash are collapsed into one entry by Repository.List,
// and that DuplicateCount reflects the number of raw rows.
func TestList_CollapsesByContentHash(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	seedArtist(t, repo, "a1", "Artist One")
	seedArtist(t, repo, "a2", "Artist Two")

	// Both rows carry the same content_hash -- same physical file, two artists.
	const hash = "aabbccdd00112233aabbccdd00112233aabbccdd00112233aabbccdd00112233"
	// Row with the smaller id ("id-1") becomes the representative (MIN(id)).
	seedRow(t, repo, "id-1", "a1", "/music/foo/fanart.jpg", "fanart.jpg", hash)
	seedRow(t, repo, "id-2", "a2", "/symlink/foo/fanart.jpg", "fanart.jpg", hash)

	entries, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 collapsed entry, got %d: %+v", len(entries), entries)
	}
	got := entries[0]
	if got.ID != "id-1" {
		t.Errorf("representative ID: got %q, want %q", got.ID, "id-1")
	}
	if got.DuplicateCount != 2 {
		t.Errorf("DuplicateCount: got %d, want 2", got.DuplicateCount)
	}
	if got.ContentHash != hash {
		t.Errorf("ContentHash: got %q, want %q", got.ContentHash, hash)
	}
}

// TestList_ThreeWayCollapse verifies a file linked from three artist records
// still produces one entry with DuplicateCount=3.
func TestList_ThreeWayCollapse(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("a%d", i)
		name := fmt.Sprintf("Artist %d", i)
		seedArtist(t, repo, id, name)
	}

	const hash = "deadbeef00000000deadbeef00000000deadbeef00000000deadbeef00000000"
	for i := 1; i <= 3; i++ {
		rowID := fmt.Sprintf("row-%d", i)
		artistID := fmt.Sprintf("a%d", i)
		filePath := fmt.Sprintf("/path/%d/backdrop.jpg", i)
		seedRow(t, repo, rowID, artistID, filePath, "backdrop.jpg", hash)
	}

	entries, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 collapsed entry, got %d", len(entries))
	}
	if entries[0].DuplicateCount != 3 {
		t.Errorf("DuplicateCount: got %d, want 3", entries[0].DuplicateCount)
	}
}

// TestList_EmptyHashNoCollapse verifies that rows with an empty content_hash
// (pre-008 rows) are never collapsed together, even when they share the same
// empty hash. Each empty-hash row must produce its own distinct entry.
func TestList_EmptyHashNoCollapse(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	seedArtist(t, repo, "a1", "Legacy Artist One")
	seedArtist(t, repo, "a2", "Legacy Artist Two")

	// Both rows have empty content_hash -- simulating pre-008 ledger rows.
	// They must not be collapsed into a single entry.
	seedRow(t, repo, "leg-1", "a1", "/music/a1/backdrop.jpg", "backdrop.jpg", "")
	seedRow(t, repo, "leg-2", "a2", "/music/a2/backdrop.jpg", "backdrop.jpg", "")

	entries, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries for 2 empty-hash rows, got %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.DuplicateCount != 1 {
			t.Errorf("empty-hash row %q: DuplicateCount should be 1, got %d", e.ID, e.DuplicateCount)
		}
	}
}

// TestList_SingleRowDuplicateCount1 verifies that a single row with a
// non-empty content_hash produces DuplicateCount=1 (no duplicates).
func TestList_SingleRowDuplicateCount1(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	seedArtist(t, repo, "a1", "Solo Artist")
	seedRow(t, repo, "r1", "a1", "/music/a1/poster.jpg", "poster.jpg",
		"cafebabe00000000cafebabe00000000cafebabe00000000cafebabe00000000")

	entries, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].DuplicateCount != 1 {
		t.Errorf("DuplicateCount: got %d, want 1", entries[0].DuplicateCount)
	}
}

// TestCount_MatchesList verifies that Count returns the same number of entries
// as List -- both use the same grouping logic so the banner count and the
// table row count are always in sync.
func TestCount_MatchesList(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	seedArtist(t, repo, "a1", "Artist One")
	seedArtist(t, repo, "a2", "Artist Two")
	seedArtist(t, repo, "a3", "Artist Three")

	const hashA = "aaaa000000000000aaaa000000000000aaaa000000000000aaaa000000000000"
	const hashB = "bbbb000000000000bbbb000000000000bbbb000000000000bbbb000000000000"

	// Two rows sharing hashA -> collapses to 1.
	seedRow(t, repo, "r1", "a1", "/music/a1/fanart.jpg", "fanart.jpg", hashA)
	seedRow(t, repo, "r2", "a2", "/music/a2/fanart.jpg", "fanart.jpg", hashA)
	// One row with hashB -> 1.
	seedRow(t, repo, "r3", "a3", "/music/a3/poster.jpg", "poster.jpg", hashB)
	// One row with empty hash -> 1 (own key).
	seedRow(t, repo, "r4", "a1", "/music/a1/backdrop.jpg", "backdrop.jpg", "")

	// Expected: 3 distinct entries (hashA group + hashB + empty-hash).
	entries, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	listLen := len(entries)

	count, err := repo.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != listLen {
		t.Errorf("Count()=%d does not match len(List())=%d", count, listLen)
	}
	if count != 3 {
		t.Errorf("expected 3 distinct entries, got %d", count)
	}
}

// TestListRaw_ReturnsAllRows verifies that ListRaw returns every ledger row
// without any content-hash deduplication, with DuplicateCount=1 on each row.
func TestListRaw_ReturnsAllRows(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	seedArtist(t, repo, "a1", "Artist One")
	seedArtist(t, repo, "a2", "Artist Two")

	const hash = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	seedRow(t, repo, "raw-1", "a1", "/music/a1/fanart.jpg", "fanart.jpg", hash)
	seedRow(t, repo, "raw-2", "a2", "/music/a2/fanart.jpg", "fanart.jpg", hash)

	raw, err := repo.ListRaw(ctx)
	if err != nil {
		t.Fatalf("ListRaw: %v", err)
	}
	// ListRaw must return both rows even though they share a content_hash.
	if len(raw) != 2 {
		t.Fatalf("ListRaw: expected 2 rows, got %d", len(raw))
	}
	for _, e := range raw {
		if e.DuplicateCount != 1 {
			t.Errorf("ListRaw row %q: DuplicateCount should be 1, got %d", e.ID, e.DuplicateCount)
		}
	}
}

// TestDismiss_DeletesAllSiblingRows verifies that iterating over ListRaw and
// calling DeleteByPath for each row fully clears the ledger when rows share
// a content_hash. This mirrors the dismiss handler's loop and confirms that
// a List-based dismiss would have left rows behind.
func TestDismiss_DeletesAllSiblingRows(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	seedArtist(t, repo, "a1", "Artist One")
	seedArtist(t, repo, "a2", "Artist Two")

	const hash = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	seedRow(t, repo, "d-1", "a1", "/music/a1/fanart.jpg", "fanart.jpg", hash)
	seedRow(t, repo, "d-2", "a2", "/music/a2/fanart.jpg", "fanart.jpg", hash)

	// Simulate the dismiss loop: list raw, allowlist once per hash, delete
	// every path.
	raw, err := repo.ListRaw(ctx)
	if err != nil {
		t.Fatalf("ListRaw: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range raw {
		if !seen[e.ContentHash] {
			if err := repo.AddAllowlist(ctx, AllowlistEntry{
				Scope:       ScopeGlobal,
				FileName:    e.FileName,
				ContentHash: e.ContentHash,
				Note:        "test dismiss",
			}); err != nil {
				t.Fatalf("AddAllowlist: %v", err)
			}
			seen[e.ContentHash] = true
		}
		if err := repo.DeleteByPath(ctx, e.ArtistID, e.FilePath); err != nil {
			t.Fatalf("DeleteByPath(%q, %q): %v", e.ArtistID, e.FilePath, err)
		}
	}

	// After dismiss, the ledger should be empty.
	count, err := repo.Count(ctx)
	if err != nil {
		t.Fatalf("Count after dismiss: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows after dismiss, got %d", count)
	}
}

// TestList_MixedHashAndEmpty verifies that a mix of hashed rows and
// empty-hash rows are all returned, with correct collapsing: hashed rows
// collapse by hash, empty rows are each their own entry.
func TestList_MixedHashAndEmpty(t *testing.T) {
	db := newTestDB(t)
	repo := NewRepository(db)
	ctx := context.Background()

	seedArtist(t, repo, "a1", "Artist A")
	seedArtist(t, repo, "a2", "Artist B")

	const hash = "0011223344556677001122334455667700112233445566770011223344556677"
	// Two hashed rows -> 1 entry.
	seedRow(t, repo, "h1", "a1", "/music/a1/fanart.jpg", "fanart.jpg", hash)
	seedRow(t, repo, "h2", "a2", "/music/a2/fanart.jpg", "fanart.jpg", hash)
	// One empty-hash row -> 1 entry.
	seedRow(t, repo, "e1", "a1", "/music/a1/backdrop.jpg", "backdrop.jpg", "")

	entries, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Expect 2 entries: the collapsed hashed group + the empty-hash row.
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}

	// Verify the hashed group has DuplicateCount=2 and the empty-hash row has 1.
	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	hashed, ok := byID["h1"]
	if !ok {
		t.Fatalf("representative row h1 not in List results; IDs present: %v",
			func() []string {
				ids := make([]string, 0, len(byID))
				for id := range byID {
					ids = append(ids, id)
				}
				return ids
			}())
	}
	if hashed.DuplicateCount != 2 {
		t.Errorf("hashed group DuplicateCount: got %d, want 2", hashed.DuplicateCount)
	}
	emptyRow, ok := byID["e1"]
	if !ok {
		t.Fatalf("empty-hash row e1 not in List results")
	}
	if emptyRow.DuplicateCount != 1 {
		t.Errorf("empty-hash row DuplicateCount: got %d, want 1", emptyRow.DuplicateCount)
	}
}
