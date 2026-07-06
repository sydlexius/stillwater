package artist

// duplicates_ignore_test.go -- unit + integration coverage for the server-side
// ignored-duplicate-group persistence and filter (#2219).

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestDuplicateGroupSignature_OrderInvariant proves the signature is invariant
// to member order (so an ignore keyed on {a,b} matches a later detection that
// enumerates {b,a}) and drops empty IDs. A regression here would let the same
// group be ignored twice or fail to match on re-detection.
func TestDuplicateGroupSignature_OrderInvariant(t *testing.T) {
	base := DuplicateGroupSignature([]string{"b2", "a1", "c3"})
	if base != "a1|b2|c3" {
		t.Fatalf("signature = %q, want sorted pipe-join %q", base, "a1|b2|c3")
	}
	// Reordered input must produce the identical signature.
	if got := DuplicateGroupSignature([]string{"c3", "b2", "a1"}); got != base {
		t.Errorf("reordered signature = %q, want %q (order-invariant)", got, base)
	}
	// Empty IDs are dropped, not joined as empty segments.
	if got := DuplicateGroupSignature([]string{"a1", "", "b2"}); got != "a1|b2" {
		t.Errorf("signature with empty id = %q, want %q", got, "a1|b2")
	}
	// A set with no non-empty IDs is un-ignorable and yields "".
	if got := DuplicateGroupSignature([]string{"", ""}); got != "" {
		t.Errorf("all-empty signature = %q, want empty string", got)
	}
	if got := DuplicateGroupSignature(nil); got != "" {
		t.Errorf("nil signature = %q, want empty string", got)
	}
	// Whitespace-padded IDs collapse to their trimmed form.
	if got := DuplicateGroupSignature([]string{" a1 ", "b2"}); got != "a1|b2" {
		t.Errorf("whitespace-padded signature = %q, want %q (trimmed)", got, "a1|b2")
	}
	// Duplicate IDs collapse to a single occurrence.
	if got := DuplicateGroupSignature([]string{"a1", "b2", "b2"}); got != "a1|b2" {
		t.Errorf("duplicate-id signature = %q, want %q (deduped)", got, "a1|b2")
	}
}

// mkGroup builds a NearDuplicateGroup with the given member IDs for filter tests.
func mkGroup(ids ...string) NearDuplicateGroup {
	members := make([]NearDuplicateArtist, 0, len(ids))
	for _, id := range ids {
		members = append(members, NearDuplicateArtist{ID: id, Name: id})
	}
	return NearDuplicateGroup{Key: ids[0], Members: members}
}

// TestFilterIgnoredGroups_ExactMatchAndDrift covers the core suppression
// contract: a group whose exact signature is ignored is dropped, while a
// "drifted" group (a superset/subset with a different signature) is NOT
// suppressed and resurfaces -- the Assumption-4 exact-match semantics.
func TestFilterIgnoredGroups_ExactMatchAndDrift(t *testing.T) {
	g1 := mkGroup("a1", "b2")       // signature a1|b2
	g2 := mkGroup("c3", "d4")       // signature c3|d4
	g3 := mkGroup("a1", "b2", "e5") // drifted superset of g1: signature a1|b2|e5

	ignored := map[string]struct{}{
		DuplicateGroupSignature([]string{"a1", "b2"}): {},
	}

	got := FilterIgnoredGroups([]NearDuplicateGroup{g1, g2, g3}, ignored)

	// g1 is dropped (exact match); g2 and g3 survive (g3 drifted -> new signature).
	if len(got) != 2 {
		t.Fatalf("filtered len = %d, want 2 (g1 dropped, g2+g3 kept); got %+v", len(got), got)
	}
	survived := map[string]bool{}
	for _, g := range got {
		survived[groupSignature(g)] = true
	}
	if survived["a1|b2"] {
		t.Errorf("exact-ignored group a1|b2 must be suppressed")
	}
	if !survived["c3|d4"] {
		t.Errorf("un-ignored group c3|d4 must survive")
	}
	if !survived["a1|b2|e5"] {
		t.Errorf("drifted group a1|b2|e5 must resurface (exact-match, not subset suppression)")
	}
}

// TestFilterIgnoredGroups_EmptySet returns the input unchanged when nothing is
// ignored -- the common no-ignores path must not drop any group.
func TestFilterIgnoredGroups_EmptySet(t *testing.T) {
	groups := []NearDuplicateGroup{mkGroup("a1", "b2")}
	if got := FilterIgnoredGroups(groups, nil); len(got) != 1 {
		t.Errorf("nil ignored set: len = %d, want 1 (unchanged)", len(got))
	}
	if got := FilterIgnoredGroups(groups, map[string]struct{}{}); len(got) != 1 {
		t.Errorf("empty ignored set: len = %d, want 1 (unchanged)", len(got))
	}
}

// TestIgnoreAndLoadRoundTrip persists an ignore and reads it back, then proves
// re-ignoring the same signature is idempotent (no error, no duplicate row).
func TestIgnoreAndLoadRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	sig := DuplicateGroupSignature([]string{"art-2", "art-1"})
	if err := IgnoreDuplicateGroup(ctx, db, sig, "the cure", "name_key"); err != nil {
		t.Fatalf("first ignore: %v", err)
	}
	// Idempotent: a second ignore of the same signature must not error and must
	// not create a second row (verified via the loaded-set size below).
	if err := IgnoreDuplicateGroup(ctx, db, sig, "the cure", "name_key"); err != nil {
		t.Fatalf("second (idempotent) ignore: %v", err)
	}

	got, err := LoadIgnoredSignatures(ctx, db)
	if err != nil {
		t.Fatalf("LoadIgnoredSignatures: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("loaded signature count = %d, want 1 (idempotent insert)", len(got))
	}
	if _, ok := got[sig]; !ok {
		t.Errorf("loaded set missing signature %q; got %+v", sig, got)
	}
}

// TestIgnoreDuplicateGroup_Guards pins the two programming-error guards: a nil
// db and an empty signature both error rather than silently succeeding.
func TestIgnoreDuplicateGroup_Guards(t *testing.T) {
	ctx := context.Background()
	if err := IgnoreDuplicateGroup(ctx, nil, "a|b", "", ""); err == nil {
		t.Errorf("nil db must error")
	}
	db := newTestDB(t)
	if err := IgnoreDuplicateGroup(ctx, db, "", "", ""); err == nil {
		t.Errorf("empty signature must error")
	}
}

// TestLoadIgnoredSignatures_NilDB preserves the nil-db test seam: an empty set,
// no error (mirrors DetectDuplicates' nil handling used by the count path).
func TestLoadIgnoredSignatures_NilDB(t *testing.T) {
	got, err := LoadIgnoredSignatures(context.Background(), nil)
	if err != nil {
		t.Fatalf("nil db must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nil db set len = %d, want 0", len(got))
	}
}

// TestIgnoreDuplicateGroup_ExecError forces the ExecContext failure branch: a
// closed (non-nil) DB is past the nil guard, so the INSERT hits "database is
// closed" and IgnoreDuplicateGroup must wrap and return it rather than silently
// succeeding. Distinct from the nil-db guard in TestIgnoreDuplicateGroup_Guards.
func TestIgnoreDuplicateGroup_ExecError(t *testing.T) {
	db := newTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("closing db for error injection: %v", err)
	}
	err := IgnoreDuplicateGroup(context.Background(), db, "a1|b2", "", "")
	if err == nil {
		t.Fatal("closed-db ExecContext must return an error, got nil")
	}
	// The wrap prefix proves the error came from this function's Exec branch,
	// not from an earlier guard.
	if !strings.Contains(err.Error(), "ignoring duplicate group:") {
		t.Errorf("error = %q, want the 'ignoring duplicate group:' wrap", err.Error())
	}
}

// TestLoadIgnoredSignatures_QueryError forces the QueryContext failure branch on
// a closed (non-nil) DB: past the nil-db seam, the SELECT errors and Load must
// return a nil map + wrapped error, not an empty "nothing ignored" set (which
// would silently un-ignore every group on a transient DB fault).
func TestLoadIgnoredSignatures_QueryError(t *testing.T) {
	db := newTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("closing db for error injection: %v", err)
	}
	got, err := LoadIgnoredSignatures(context.Background(), db)
	if err == nil {
		t.Fatal("closed-db QueryContext must return an error, got nil")
	}
	if got != nil {
		t.Errorf("on query error the returned map must be nil, got %+v", got)
	}
	if !strings.Contains(err.Error(), "loading ignored signatures:") {
		t.Errorf("error = %q, want the 'loading ignored signatures:' wrap", err.Error())
	}
}

// TestLoadIgnoredGroups_RowsAndOrder proves the manage-view loader returns the
// full row (id, signature, display context, timestamp) and orders newest-first.
// Ordering matters: the manage view lists the most recently ignored group at the
// top, so a regression to insertion/rowid order would surface stale entries first.
func TestLoadIgnoredGroups_RowsAndOrder(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Two ignores with explicit, distinct created_at values so the DESC order is
	// deterministic regardless of how fast the inserts run (datetime('now') has
	// 1-second resolution, so two quick default inserts could tie).
	insert := func(id, sig, key, reason, created string) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO ignored_duplicate_groups (id, signature, group_key, reason, created_at) VALUES (?,?,?,?,?)`,
			id, sig, key, reason, created); err != nil {
			t.Fatalf("seeding ignore %s: %v", id, err)
		}
	}
	insert("id-old", "a1|b2", "older", "name_key", "2026-07-01 10:00:00")
	insert("id-new", "c3|d4|e5", "newer", "mbid", "2026-07-02 10:00:00")

	got, err := LoadIgnoredGroups(ctx, db)
	if err != nil {
		t.Fatalf("LoadIgnoredGroups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded groups = %d, want 2", len(got))
	}
	// Newest first.
	if got[0].ID != "id-new" || got[1].ID != "id-old" {
		t.Fatalf("order = [%s, %s], want [id-new, id-old] (created_at DESC)", got[0].ID, got[1].ID)
	}
	// Full-row fidelity on the newest entry, including member count derived from
	// the 3-member signature.
	g := got[0]
	if g.Signature != "c3|d4|e5" || g.GroupKey != "newer" || g.Reason != "mbid" || g.CreatedAt != "2026-07-02 10:00:00" {
		t.Errorf("row fidelity mismatch: %+v", g)
	}
	if g.MemberCount() != 3 {
		t.Errorf("MemberCount() = %d, want 3 for signature %q", g.MemberCount(), g.Signature)
	}
	if got[1].MemberCount() != 2 {
		t.Errorf("MemberCount() = %d, want 2 for signature %q", got[1].MemberCount(), got[1].Signature)
	}
}

// TestLoadIgnoredGroups_NilDB preserves the nil-db seam: empty slice, no error.
func TestLoadIgnoredGroups_NilDB(t *testing.T) {
	got, err := LoadIgnoredGroups(context.Background(), nil)
	if err != nil {
		t.Fatalf("nil db must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nil db slice len = %d, want 0", len(got))
	}
}

// TestRestoreDuplicateGroup_RoundTrip is the core AC test: an ignore is
// persisted, then restored by id, and the signature set is empty afterward --
// proving the group would reappear in both the page list and the count (both
// read the same table via LoadIgnoredSignatures/FilterIgnoredGroups). This is
// the un-ignore that drives the pill re-increment.
func TestRestoreDuplicateGroup_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	sig := DuplicateGroupSignature([]string{"art-1", "art-2"})
	if err := IgnoreDuplicateGroup(ctx, db, sig, "grp", "name_key"); err != nil {
		t.Fatalf("ignore: %v", err)
	}
	groups, err := LoadIgnoredGroups(ctx, db)
	if err != nil || len(groups) != 1 {
		t.Fatalf("pre-restore load: groups=%d err=%v", len(groups), err)
	}
	id := groups[0].ID

	// Restore removes the row.
	if err := RestoreDuplicateGroup(ctx, db, id); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// The signature set the filter consumes is now empty, so FilterIgnoredGroups
	// suppresses nothing -- the group reappears in count AND list. This is the
	// invariant that makes the pill re-increment without any count-specific code.
	sigs, err := LoadIgnoredSignatures(ctx, db)
	if err != nil {
		t.Fatalf("post-restore signatures: %v", err)
	}
	if len(sigs) != 0 {
		t.Errorf("post-restore signature set len = %d, want 0 (group un-ignored)", len(sigs))
	}
	// And the resurfaced group is no longer filtered out.
	kept := FilterIgnoredGroups([]NearDuplicateGroup{mkGroup("art-1", "art-2")}, sigs)
	if len(kept) != 1 {
		t.Errorf("restored group must survive the filter; kept = %d, want 1", len(kept))
	}
}

// TestRestoreDuplicateGroup_NotFound: restoring an unknown id (or a second
// restore of an already-restored row) returns ErrIgnoredGroupNotFound so the
// handler maps it to 404, not a silent success or a 500.
func TestRestoreDuplicateGroup_NotFound(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	err := RestoreDuplicateGroup(ctx, db, "does-not-exist")
	if !errors.Is(err, ErrIgnoredGroupNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrIgnoredGroupNotFound", err)
	}
}

// TestRestoreDuplicateGroup_Guards pins the programming-error guards: a nil db
// and a blank/whitespace id both error rather than issuing a DELETE with an
// empty predicate.
func TestRestoreDuplicateGroup_Guards(t *testing.T) {
	ctx := context.Background()
	if err := RestoreDuplicateGroup(ctx, nil, "id-1"); err == nil {
		t.Errorf("nil db must error")
	}
	db := newTestDB(t)
	if err := RestoreDuplicateGroup(ctx, db, "   "); err == nil {
		t.Errorf("whitespace-only id must error (never a predicate-less DELETE)")
	}
}

// TestRestoreDuplicateGroup_ExecError forces the ExecContext failure branch on a
// closed (non-nil) DB, distinct from the not-found and guard branches: the wrap
// prefix proves it came from the Exec path.
func TestRestoreDuplicateGroup_ExecError(t *testing.T) {
	db := newTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("closing db for error injection: %v", err)
	}
	err := RestoreDuplicateGroup(context.Background(), db, "id-1")
	if err == nil {
		t.Fatal("closed-db ExecContext must return an error, got nil")
	}
	if errors.Is(err, ErrIgnoredGroupNotFound) {
		t.Errorf("a DB fault must not be reported as not-found: %v", err)
	}
	if !strings.Contains(err.Error(), "restoring duplicate group:") {
		t.Errorf("error = %q, want the 'restoring duplicate group:' wrap", err.Error())
	}
}
