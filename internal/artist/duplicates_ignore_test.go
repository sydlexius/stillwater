package artist

// duplicates_ignore_test.go -- unit + integration coverage for the server-side
// ignored-duplicate-group persistence and filter (#2219).

import (
	"context"
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
