package artist

import (
	"context"
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
	id1a := insert("Caedmon's Call", "/music/Caedmon's Call", "")             // U+0027
	id1b := insert("Caedmon"+curlyApostrophe+"s Call", "/music/Caedmon2", "") // U+2019

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

	// --- Assert pair 1 (Caedmon) found ---
	g1 := findGroup(byMembers(id1a, id1b))
	if g1 == nil {
		t.Errorf("Caedmon apostrophe pair not found in groups (ids %s / %s)", id1a, id1b)
	} else if g1.reason != "name_key" {
		t.Errorf("Caedmon pair reason = %q, want name_key", g1.reason)
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
