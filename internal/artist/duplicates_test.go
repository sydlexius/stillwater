package artist

import (
	"context"
	"database/sql"
	"testing"
)

// TestDetectDuplicates exercises the full detection pipeline against a real
// in-memory SQLite database seeded with near-duplicate artists.
func TestDetectDuplicates(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Helper: insert an artist row with a given path (non-empty = filesystem
	// artist) and optional MBID.
	insert := func(name, path, mbid string) string {
		t.Helper()
		repo := newSQLiteArtistRepo(db)
		a := &Artist{Name: name, SortName: name, Path: path}
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("seeding artist %q: %v", name, err)
		}
		if mbid != "" {
			if _, err := db.ExecContext(ctx,
				`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES (?, 'musicbrainz', ?)`,
				a.ID, mbid,
			); err != nil {
				t.Fatalf("seeding MBID for %q: %v", name, err)
			}
		}
		return a.ID
	}

	// --- Pair 1: apostrophe U+0027 vs U+2019 (the observed live case) ---
	curlyApostrophe := string([]rune{0x2019})
	id1a := insert("Larkfield's Reach", "/music/Larkfield's Reach", "")            // U+0027
	id1b := insert("Larkfield"+curlyApostrophe+"s Reach", "/music/Larkfield2", "") // U+2019

	// --- Pair 2: "The Cure" vs "Cure, The" (article variants) ---
	id2a := insert("The Cure", "/music/The Cure", "")
	id2b := insert("Cure, The", "/music/Cure, The", "")

	// --- Pair 3: MBID match with different names (AC/DC substitution) ---
	// AC/DC on disk becomes "AC_DC" or "ACDC" in different tools.
	// The name keys for "ACDC" and "AC_DC" differ (separator-only fold), but
	// they share an MBID so the MBID edge merges them.
	sharedMBID := "5b11f4ce-a62d-471e-81fc-a69a8278c7da"
	id3a := insert("ACDC", "/music/ACDC", sharedMBID)
	id3b := insert("AC_DC", "/music/AC_DC", sharedMBID)

	// --- Solo: no duplicate (Radiohead is unique) ---
	_ = insert("Radiohead", "/music/Radiohead", "")

	// --- Platform-only artist (path='') must be excluded ---
	repo := newSQLiteArtistRepo(db)
	platformOnly := &Artist{Name: "The Cure", Path: ""} // same name as pair 2
	if err := repo.Create(ctx, platformOnly); err != nil {
		t.Fatalf("seeding platform-only artist: %v", err)
	}

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}

	// Build a helper map: member ID set -> group for easy assertions.
	type groupInfo struct {
		reason  string
		members map[string]bool
	}
	byMembers := func(ids ...string) map[string]bool {
		m := make(map[string]bool, len(ids))
		for _, id := range ids {
			m[id] = true
		}
		return m
	}

	findGroup := func(expected map[string]bool) *groupInfo {
		for _, g := range groups {
			got := make(map[string]bool, len(g.Members))
			for _, m := range g.Members {
				got[m.ID] = true
			}
			match := true
			for id := range expected {
				if !got[id] {
					match = false
					break
				}
			}
			if match && len(got) == len(expected) {
				return &groupInfo{reason: g.Reason, members: got}
			}
		}
		return nil
	}

	// --- Assert pair 1 (apostrophe variants) found ---
	g1 := findGroup(byMembers(id1a, id1b))
	if g1 == nil {
		t.Errorf("apostrophe pair not found in groups (ids %s / %s)", id1a, id1b)
	} else if g1.reason != "name_key" {
		t.Errorf("apostrophe pair reason = %q, want name_key", g1.reason)
	}

	// --- Assert pair 2 (The Cure / Cure, The) found ---
	g2 := findGroup(byMembers(id2a, id2b))
	if g2 == nil {
		t.Errorf("The Cure / Cure, The pair not found in groups")
	} else if g2.reason != "name_key" {
		t.Errorf("Cure article pair reason = %q, want name_key", g2.reason)
	}

	// --- Assert pair 3 (ACDC / AC_DC via MBID) found ---
	g3 := findGroup(byMembers(id3a, id3b))
	if g3 == nil {
		t.Errorf("ACDC / AC_DC MBID pair not found in groups")
	} else if g3.reason != "mbid" {
		t.Errorf("ACDC pair reason = %q, want mbid", g3.reason)
	}

	// --- Assert Radiohead is NOT in any group ---
	for _, g := range groups {
		for _, m := range g.Members {
			if m.Name == "Radiohead" {
				t.Errorf("Radiohead unexpectedly appears in a duplicate group (group key=%q)", g.Key)
			}
		}
	}

	// --- Assert platform-only "The Cure" is NOT in any group ---
	for _, g := range groups {
		for _, m := range g.Members {
			if m.ID == platformOnly.ID {
				t.Errorf("platform-only artist (id=%s) unexpectedly appears in a duplicate group", platformOnly.ID)
			}
		}
	}

	// --- Total group count ---
	if len(groups) != 3 {
		t.Errorf("expected 3 duplicate groups, got %d", len(groups))
		for _, g := range groups {
			t.Logf("  group key=%q reason=%q members=%v", g.Key, g.Reason, func() []string {
				names := make([]string, len(g.Members))
				for i, m := range g.Members {
					names[i] = m.Name
				}
				return names
			}())
		}
	}
}

// seedArtistWithMBID inserts a path-bearing artist and, when mbid is non-empty,
// its MusicBrainz provider row.  It returns the new artist ID.  Shared by the
// conflicting-MBID tests below.
func seedArtistWithMBID(ctx context.Context, t *testing.T, db *sql.DB, name, path, mbid string) string {
	t.Helper()
	repo := newSQLiteArtistRepo(db)
	a := &Artist{Name: name, SortName: name, Path: path}
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("seeding artist %q: %v", name, err)
	}
	if mbid != "" {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES (?, 'musicbrainz', ?)`,
			a.ID, mbid,
		); err != nil {
			t.Fatalf("seeding MBID for %q: %v", name, err)
		}
	}
	return a.ID
}

// seedArtistWithDisamb seeds an artist carrying both an MBID and a
// disambiguation value.  Disambiguation is persisted through repo.Create (the
// artists.disambiguation column is NOT NULL, defaulting to the empty string),
// so the detector reads it back through its own SELECT exactly as it would in
// production.
func seedArtistWithDisamb(ctx context.Context, t *testing.T, db *sql.DB, name, path, mbid, disamb string) string {
	t.Helper()
	repo := newSQLiteArtistRepo(db)
	a := &Artist{Name: name, SortName: name, Path: path, Disambiguation: disamb}
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("seeding artist %q: %v", name, err)
	}
	if mbid != "" {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES (?, 'musicbrainz', ?)`,
			a.ID, mbid,
		); err != nil {
			t.Fatalf("seeding MBID for %q: %v", name, err)
		}
	}
	return a.ID
}

// assertSeededDisamb reads artists.disambiguation back out of the DB and fails
// when it does not match want.  This is the anti-vacuity guard for the
// Defect-2 tests: a "no conflict detected" pass must not come from a row whose
// disambiguation silently failed to persist, which would mean the conflict path
// was never exercised at all.
func assertSeededDisamb(ctx context.Context, t *testing.T, db *sql.DB, id, want string) {
	t.Helper()
	var got string
	if err := db.QueryRowContext(ctx,
		`SELECT disambiguation FROM artists WHERE id = ?`, id,
	).Scan(&got); err != nil {
		t.Fatalf("reading back disambiguation for %s: %v", id, err)
	}
	if got != want {
		t.Fatalf("seeded disambiguation for %s = %q, want %q", id, got, want)
	}
}

// memberByID returns the group member with the given ID, or nil.
func memberByID(g *NearDuplicateGroup, id string) *NearDuplicateArtist {
	for i := range g.Members {
		if g.Members[i].ID == id {
			return &g.Members[i]
		}
	}
	return nil
}

// TestDetectDuplicates_DisambiguationConflictIsSoft is the core Defect-2 guard
// (#2527).  Two artists share an MBID but carry DIFFERENT non-empty
// disambiguation values.
//
// The maintainer's locked decision is that this is a SOFT gate, so the
// assertion has two halves and BOTH matter:
//
//  1. They are STILL GROUPED.  A hard-exclude implementation (the shape the
//     conflicting-MBID guard uses) would drop the group and pass a naive
//     "conflicts are not offered" test -- this half fails it.
//  2. The conflict is FLAGGED on the group and on both members.  An
//     implementation that merely threads disambiguation through as display data
//     without computing the flag passes half 1 -- this half fails it.
//
// What still passes if broken: a stubbed `return false` conflict flag passes
// half 1 and fails half 2 (that is the revert-and-rerun mutant).
func TestDetectDuplicates_DisambiguationConflictIsSoft(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const mbid = "33333333-3333-3333-3333-333333333333"
	idA := seedArtistWithDisamb(ctx, t, db, "Nirvana", "/music/NirvanaUS", mbid, "Seattle grunge band")
	idB := seedArtistWithDisamb(ctx, t, db, "NIRVANA", "/music/NirvanaUK", mbid, "UK progressive rock band")

	// Anti-vacuity: prove the distinct disambiguation values actually persisted,
	// so a missing flag cannot be excused by a row that never carried a value.
	assertSeededDisamb(ctx, t, db, idA, "Seattle grunge band")
	assertSeededDisamb(ctx, t, db, idB, "UK progressive rock band")

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}

	// Half 1: still grouped. Differing disambiguation must NOT exclude -- that
	// is the difference between Defect-2 (soft) and Defect-1 (hard).
	g := groupContainingBoth(groups, idA, idB)
	if g == nil {
		t.Fatalf("artists with conflicting disambiguation were NOT grouped; Defect-2 is a SOFT gate "+
			"(warn + explicit override), not an exclusion -- the operator must still be offered the "+
			"merge. groups=%+v", groups)
	}

	// Half 2: the conflict is flagged, at group level and per member.
	if !g.DisambiguationConflict {
		t.Errorf("group %q: DisambiguationConflict = false, want true; the merge modal gates Confirm "+
			"on this flag, so an unflagged conflict means the operator is offered an irreversible "+
			"merge of two different artists with no warning", g.Key)
	}
	for _, id := range []string{idA, idB} {
		m := memberByID(g, id)
		if m == nil {
			t.Fatalf("member %s missing from group %q", id, g.Key)
		}
		if !m.DisambiguationConflict {
			t.Errorf("member %s (disambiguation %q): DisambiguationConflict = false, want true",
				id, m.Disambiguation)
		}
	}
}

// TestDetectDuplicates_DisambiguationNoConflict pins the NEGATIVE side of the
// flag: an empty disambiguation is "unknown", not a competing opinion, so it
// must never raise a conflict against a populated one.
//
// What still passes if broken: an over-broad "any difference conflicts"
// implementation (comparing values without the non-empty filter) groups these
// two -- passing the soft-gate test above -- but flags them here, which this
// test rejects.  Without this test, crying wolf on every untagged artist would
// ship green and train operators to click through the override reflexively,
// destroying the warning's value.
func TestDetectDuplicates_DisambiguationNoConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const mbid = "44444444-4444-4444-4444-444444444444"
	idA := seedArtistWithDisamb(ctx, t, db, "Portishead", "/music/Portishead", mbid, "Bristol trip-hop")
	idB := seedArtistWithDisamb(ctx, t, db, "PORTISHEAD", "/music/Portishead2", mbid, "")

	assertSeededDisamb(ctx, t, db, idA, "Bristol trip-hop")
	assertSeededDisamb(ctx, t, db, idB, "")

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}
	g := groupContainingBoth(groups, idA, idB)
	if g == nil {
		t.Fatalf("same-MBID artists were not grouped at all; groups=%+v", groups)
	}
	if g.DisambiguationConflict {
		t.Errorf("group %q: DisambiguationConflict = true, want false; an EMPTY disambiguation is "+
			"unknown, not a contradicting value, so it must not trigger the override gate", g.Key)
	}
	for _, id := range []string{idA, idB} {
		if m := memberByID(g, id); m != nil && m.DisambiguationConflict {
			t.Errorf("member %s: DisambiguationConflict = true, want false", id)
		}
	}
	// Reason must still read "mbid": the Defect-2 annotation is computed after
	// grouping and must not perturb the existing label (#2527 Divergence 2).
	if g.Reason != "mbid" {
		t.Errorf("group %q: Reason = %q, want %q", g.Key, g.Reason, "mbid")
	}
}

// TestDetectDuplicates_DisambiguationPartialConflict covers the mixed group:
// values {X, Y, ""}.  The group conflicts, but the UNTAGGED member is not the
// one making a contradictory claim and must stay unflagged.
//
// What still passes if broken: a blanket "flag every member of a conflicting
// group" passes the soft-gate test above (both members there are tagged) and
// fails only here.  The distinction is real UI: the per-member flag drives which
// rows get the amber marker, so blanket-flagging would point the operator at the
// innocent row.
func TestDetectDuplicates_DisambiguationPartialConflict(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const mbid = "55555555-5555-5555-5555-555555555555"
	idX := seedArtistWithDisamb(ctx, t, db, "Bandname", "/music/BandX", mbid, "Norwegian black metal")
	idY := seedArtistWithDisamb(ctx, t, db, "BANDNAME", "/music/BandY", mbid, "Canadian indie pop")
	idNone := seedArtistWithDisamb(ctx, t, db, "BandName", "/music/BandZ", mbid, "")

	assertSeededDisamb(ctx, t, db, idX, "Norwegian black metal")
	assertSeededDisamb(ctx, t, db, idY, "Canadian indie pop")
	assertSeededDisamb(ctx, t, db, idNone, "")

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}
	g := groupContainingBoth(groups, idX, idY)
	if g == nil {
		t.Fatalf("same-MBID artists were not grouped; groups=%+v", groups)
	}
	if !g.DisambiguationConflict {
		t.Fatalf("group %q: DisambiguationConflict = false, want true (X and Y contradict)", g.Key)
	}
	if m := memberByID(g, idX); m == nil || !m.DisambiguationConflict {
		t.Errorf("member X: want DisambiguationConflict true, got %+v", m)
	}
	if m := memberByID(g, idY); m == nil || !m.DisambiguationConflict {
		t.Errorf("member Y: want DisambiguationConflict true, got %+v", m)
	}
	if m := memberByID(g, idNone); m == nil {
		t.Errorf("untagged member %s missing from group", idNone)
	} else if m.DisambiguationConflict {
		t.Errorf("untagged member %s: DisambiguationConflict = true, want false; it asserts no value, "+
			"so it contradicts nothing and must not carry the amber marker", idNone)
	}
}

// TestDetectDuplicates_DisambiguationDoesNotAffectGrouping is the regression
// lock on the Defect-1/Defect-2 boundary: the disambiguation annotation must
// never feed back into the union-find.  Conflicting MBIDs stay HARD-excluded
// even when the disambiguation values agree perfectly -- an implementation that
// softened the MBID guard into a warning would pass the Defect-2 tests above
// while reopening the exact data-loss vector #2531 closed.
func TestDetectDuplicates_DisambiguationDoesNotAffectGrouping(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const (
		mbid1 = "66666666-6666-6666-6666-666666666666"
		mbid2 = "77777777-7777-7777-7777-777777777777"
	)
	// Same name key, IDENTICAL disambiguation, but different MBIDs.
	idA := seedArtistWithDisamb(ctx, t, db, "Split", "/music/SplitA", mbid1, "the band")
	idB := seedArtistWithDisamb(ctx, t, db, "SPLIT", "/music/SplitB", mbid2, "the band")

	assertSeededMBID(ctx, t, db, idA, mbid1)
	assertSeededMBID(ctx, t, db, idB, mbid2)
	assertSeededDisamb(ctx, t, db, idA, "the band")
	assertSeededDisamb(ctx, t, db, idB, "the band")

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}
	if g := groupContainingBoth(groups, idA, idB); g != nil {
		t.Fatalf("conflicting-MBID artists were grouped (reason=%q); matching disambiguation must NOT "+
			"soften the Defect-1 hard exclusion -- MusicBrainz asserting two distinct entities "+
			"outranks the operator's own tag", g.Reason)
	}
}

// assertSeededMBID reads the MusicBrainz provider_id back out of the DB and
// fails when it does not match want.  This is the anti-vacuity guard for the
// conflicting-MBID tests: a "rows are not grouped" pass must not come from a
// row that silently failed to seed its MBID and so never entered the
// conflicting-MBID code path at all.
func assertSeededMBID(ctx context.Context, t *testing.T, db *sql.DB, id, want string) {
	t.Helper()
	var got string
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(provider_id, '') FROM artist_provider_ids WHERE artist_id = ? AND provider = 'musicbrainz'`,
		id,
	).Scan(&got)
	if err != nil {
		t.Fatalf("reading back MBID for %s: %v", id, err)
	}
	if got != want {
		t.Fatalf("seeded MBID for %s = %q, want %q", id, got, want)
	}
}

// groupContainingBoth returns the group (if any) whose member ID set includes
// both a and b.
func groupContainingBoth(groups []NearDuplicateGroup, a, b string) *NearDuplicateGroup {
	for i := range groups {
		ids := make(map[string]bool, len(groups[i].Members))
		for _, m := range groups[i].Members {
			ids[m.ID] = true
		}
		if ids[a] && ids[b] {
			return &groups[i]
		}
	}
	return nil
}

// TestDetectDuplicates_ConflictingMBID is the #2527 Defect 1 acceptance
// criterion: two artists with the SAME normalized name key but TWO DIFFERENT
// non-empty MusicBrainz IDs are distinct artists that merely collide on name.
// A merge is irreversible and physically relocates files, so they must NEVER be
// offered as a merge candidate -- i.e. they must never share a group.
//
// Both fall out as singletons here (each is the only member of its would-be
// group), so no group is emitted at all.
//
// MUTANT NOTE: if the MBID guard were removed, the name-key union would join
// these two rows and this test would go RED (a group with both ids appears).
// A weaker assertion -- e.g. only checking len(groups)==0 without asserting the
// distinct MBIDs were actually seeded -- could pass vacuously if a row failed to
// insert its MBID; the assertSeededMBID calls below close that hole.
func TestDetectDuplicates_ConflictingMBID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const (
		mbid1 = "11111111-1111-1111-1111-111111111111"
		mbid2 = "22222222-2222-2222-2222-222222222222"
	)
	// Same normalized name key ("duplicity"), different real artists.
	idA := seedArtistWithMBID(ctx, t, db, "Duplicity", "/music/DuplicityA", mbid1)
	idB := seedArtistWithMBID(ctx, t, db, "DUPLICITY", "/music/DuplicityB", mbid2)

	// Anti-vacuity: prove the distinct MBIDs actually persisted so the
	// conflicting-MBID path is genuinely exercised.
	assertSeededMBID(ctx, t, db, idA, mbid1)
	assertSeededMBID(ctx, t, db, idB, mbid2)

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}
	if g := groupContainingBoth(groups, idA, idB); g != nil {
		t.Fatalf("conflicting-MBID artists %s / %s were grouped together (reason=%q, key=%q); "+
			"a merge would irreversibly relocate files for two distinct artists", idA, idB, g.Reason, g.Key)
	}
	// Neither should appear in ANY group (each is a singleton once the other is
	// excluded).
	for _, g := range groups {
		for _, m := range g.Members {
			if m.ID == idA || m.ID == idB {
				t.Errorf("conflicting-MBID artist %s unexpectedly appears in group key=%q", m.ID, g.Key)
			}
		}
	}
}

// TestDetectDuplicates_ConflictingMBIDTransitivity covers the transitivity and
// bucket-order corners of the guarded union.
//
// Sub-case "bridge": three rows share the same name key -- A(mbid=M1),
// B(mbid=""), C(mbid=M2).  The empty-MBID bridge row B must NOT let M1 and M2
// end up in one component.  A pairwise-only guard (no per-component
// representative propagation) would union A-B then B-C and smuggle M1 and M2
// into a single group; the per-component repMBID tracking prevents that.
//
// Sub-case "bucket order": three rows share the same name key -- A(mbid=M1),
// B(mbid=M1), C(mbid=M2) -- and C is named so it sorts FIRST (queries ORDER BY
// name, so C becomes the bucket pivot).  A and B are genuine duplicates (same
// M1) and MUST still group together.  A pivot-on-first union loop would only
// try union(C,A) and union(C,B), both refused by the guard, and would DROP the
// A-B pairing entirely (silent false negative).  The all-pairs loop adds
// union(A,B) and keeps them together.
func TestDetectDuplicates_ConflictingMBIDTransitivity(t *testing.T) {
	const (
		m1 = "aaaaaaaa-0000-0000-0000-000000000001"
		m2 = "bbbbbbbb-0000-0000-0000-000000000002"
	)

	t.Run("bridge", func(t *testing.T) {
		db := newTestDB(t)
		ctx := context.Background()

		// Three case-variant names that all normalize to the same key
		// ("aaa bridge"); the guard must keep M1 and M2 apart for ANY bucket
		// order, so the exact ordering here is not load-bearing.
		idA := seedArtistWithMBID(ctx, t, db, "Aaa Bridge", "/music/bridgeA", m1)
		idB := seedArtistWithMBID(ctx, t, db, "AAA BRIDGE", "/music/bridgeB", "") // empty bridge
		idC := seedArtistWithMBID(ctx, t, db, "aaa bridge", "/music/bridgeC", m2)

		// Premise guard: all three must share one normalized name key, else the
		// name-key bucket never forms and the test proves nothing.
		if NormalizeIdentityKey("Aaa Bridge") != NormalizeIdentityKey("AAA BRIDGE") ||
			NormalizeIdentityKey("AAA BRIDGE") != NormalizeIdentityKey("aaa bridge") {
			t.Fatalf("bridge names do not share a name key; test premise invalid")
		}
		_ = idB
		assertSeededMBID(ctx, t, db, idA, m1)
		assertSeededMBID(ctx, t, db, idC, m2)

		groups, err := DetectDuplicates(ctx, db)
		if err != nil {
			t.Fatalf("DetectDuplicates: %v", err)
		}
		// The M1 row and the M2 row must never share a group.
		if g := groupContainingBoth(groups, idA, idC); g != nil {
			t.Fatalf("M1 row %s and M2 row %s were bridged into one group (key=%q) via the empty-MBID row; "+
				"transitivity guard failed", idA, idC, g.Key)
		}
	})

	t.Run("bucket_order_pivot_conflicts", func(t *testing.T) {
		db := newTestDB(t)
		ctx := context.Background()

		// Three case-variant names that all normalize to the same key
		// ("orderly") but sort differently by raw ASCII: uppercase bytes sort
		// before lowercase, so "ORDERLY" < "Orderly" < "orderly".  Give the
		// CONFLICTING M2 row the all-caps name so it is the ORDER BY name pivot.
		idPivotConflict := seedArtistWithMBID(ctx, t, db, "ORDERLY", "/music/orderPivot", m2)
		idA := seedArtistWithMBID(ctx, t, db, "Orderly", "/music/orderA", m1)
		idB := seedArtistWithMBID(ctx, t, db, "orderly", "/music/orderB", m1)

		assertSeededMBID(ctx, t, db, idPivotConflict, m2)
		assertSeededMBID(ctx, t, db, idA, m1)
		assertSeededMBID(ctx, t, db, idB, m1)

		// Premise guards: identical name keys, and the conflicting row sorts
		// first (so it is the pivot a pivot-on-first loop would use).
		if NormalizeIdentityKey("ORDERLY") != NormalizeIdentityKey("Orderly") ||
			NormalizeIdentityKey("Orderly") != NormalizeIdentityKey("orderly") {
			t.Fatalf("order names do not share a name key; test premise invalid")
		}
		if "ORDERLY" >= "Orderly" || "Orderly" >= "orderly" {
			t.Fatalf("order names do not sort as expected; test premise invalid")
		}

		groups, err := DetectDuplicates(ctx, db)
		if err != nil {
			t.Fatalf("DetectDuplicates: %v", err)
		}
		// The two genuine M1 duplicates must still be grouped together despite
		// the conflicting pivot sorting first.
		if g := groupContainingBoth(groups, idA, idB); g == nil {
			t.Fatalf("genuine M1 duplicates %s / %s were dropped when the conflicting pivot sorted first "+
				"(all-pairs union missing)", idA, idB)
		}
		// And the conflicting M2 row must not be dragged in with them.
		if g := groupContainingBoth(groups, idA, idPivotConflict); g != nil {
			t.Fatalf("conflicting M2 pivot %s was grouped with M1 row %s (key=%q)", idPivotConflict, idA, g.Key)
		}
	})
}

// TestDetectDuplicates_EmptyDB verifies that DetectDuplicates returns an empty
// slice (not an error) when the database contains no path-bearing artists.
func TestDetectDuplicates_EmptyDB(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates on empty DB: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("expected 0 groups, got %d", len(groups))
	}
}

// TestDetectDuplicates_PathEmpty ensures platform-only rows (path=”) are
// never included in a duplicate group even when they share a name with a
// filesystem-backed artist.
func TestDetectDuplicates_PathEmpty(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := newSQLiteArtistRepo(db)

	// Platform-only (no path)
	pOnly := &Artist{Name: "Ghost", Path: ""}
	if err := repo.Create(ctx, pOnly); err != nil {
		t.Fatalf("seeding platform-only: %v", err)
	}

	// Filesystem artist with the same name
	fsArtist := &Artist{Name: "Ghost", Path: "/music/Ghost"}
	if err := repo.Create(ctx, fsArtist); err != nil {
		t.Fatalf("seeding fs artist: %v", err)
	}

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}
	// "Ghost" appears only once as a filesystem artist so there should be no group.
	if len(groups) != 0 {
		t.Errorf("expected 0 groups (platform-only excluded), got %d", len(groups))
	}
}

// TestDetectDuplicates_MixedGroup verifies that a group formed by a name-key
// collision where only a subset of members share an MBID is classified as
// "name_key", not "mbid".  This is the bug scenario: three artists share the
// same normalized name key; two of them also share an MBID.  Because not ALL
// members carry the same MBID, findSharedMBID returns "" and the group must
// stay "name_key".  The Key must be the normalized name key, not the MBID.
func TestDetectDuplicates_MixedGroup(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	insert := func(name, path, mbid string) string {
		t.Helper()
		repo := newSQLiteArtistRepo(db)
		a := &Artist{Name: name, SortName: name, Path: path}
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("seeding artist %q: %v", name, err)
		}
		if mbid != "" {
			if _, err := db.ExecContext(ctx,
				`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES (?, 'musicbrainz', ?)`,
				a.ID, mbid,
			); err != nil {
				t.Fatalf("seeding MBID for %q: %v", name, err)
			}
		}
		return a.ID
	}

	// Three artists with the same normalized name key "hiromi".
	// Two carry the same MBID; one has no MBID at all.
	sharedMBID := "aabbccdd-0000-0000-0000-000000000001"
	idA := insert("Hiromi", "/music/Hiromi", sharedMBID)
	idB := insert("HIROMI", "/music/HIROMI", sharedMBID)
	idC := insert("hiromi", "/music/hiromi_solo", "") // no MBID

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	g := groups[0]

	// All three must be in the group.
	inGroup := make(map[string]bool, len(g.Members))
	for _, m := range g.Members {
		inGroup[m.ID] = true
	}
	for _, id := range []string{idA, idB, idC} {
		if !inGroup[id] {
			t.Errorf("expected artist %s in group, but it was absent", id)
		}
	}

	// Mixed group must be classified as name_key, not mbid.
	if g.Reason != "name_key" {
		t.Errorf("mixed group reason = %q, want name_key", g.Reason)
	}

	// Key must be the normalized name key, not the shared MBID.
	wantKey := NormalizeIdentityKey("Hiromi")
	if g.Key == sharedMBID {
		t.Errorf("mixed group key = %q (the MBID), want normalized name key %q", g.Key, wantKey)
	}
	if g.Key != wantKey {
		t.Errorf("mixed group key = %q, want %q", g.Key, wantKey)
	}
}

// TestDetectDuplicates_AbsentAfterMerge is the "result survives the next
// scan" acceptance criterion checked at the detection layer: after a clean
// MergeArtists call, the next DetectDuplicates call must not surface the
// merged group. Detection is the source of truth the UI reads, so this
// closes the loop that motivated the merge endpoint (#1615): a DB-only
// merge would still see the loser path on disk via a re-scan and re-promote
// it back into a fresh artist row.
func TestDetectDuplicates_AbsentAfterMerge(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	// Sanity: detection reports the group before merge.
	before, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("pre-merge DetectDuplicates: %v", err)
	}
	foundBefore := false
	for _, g := range before {
		ids := make(map[string]bool, len(g.Members))
		for _, m := range g.Members {
			ids[m.ID] = true
		}
		if ids[survivorID] && ids[loserID] {
			foundBefore = true
			break
		}
	}
	if !foundBefore {
		t.Fatalf("pre-merge group not detected; setup bug")
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	after, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("post-merge DetectDuplicates: %v", err)
	}
	for _, g := range after {
		for _, m := range g.Members {
			if m.ID == loserID {
				t.Errorf("post-merge group still references loser %s: %+v", loserID, g)
			}
		}
	}
}

// TestDetectDuplicates_NFCvsNFD checks that an NFC-named and NFD-named artist
// (both with non-empty paths) produce the same key and end up in one group.
func TestDetectDuplicates_NFCvsNFD(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := newSQLiteArtistRepo(db)

	// NFC form: e + combining acute -> precomposed U+00E9
	// NFD form: e + combining acute U+0301 (two runes)
	nfc := "Café"  // precomposed
	nfd := "Café" // decomposed (e + combining acute)

	aA := &Artist{Name: nfc, Path: "/music/NFC"}
	aB := &Artist{Name: nfd, Path: "/music/NFD"}
	if err := repo.Create(ctx, aA); err != nil {
		t.Fatalf("seeding NFC artist: %v", err)
	}
	if err := repo.Create(ctx, aB); err != nil {
		t.Fatalf("seeding NFD artist: %v", err)
	}

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}
	if len(groups) != 1 {
		t.Errorf("expected 1 NFC/NFD duplicate group, got %d", len(groups))
		return
	}
	if groups[0].Reason != "name_key" {
		t.Errorf("NFC/NFD group reason = %q, want name_key", groups[0].Reason)
	}
}
