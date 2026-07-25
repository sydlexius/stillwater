package artist

// merge_platform_only_test.go -- #2730. A platform-only artist row (path = '')
// must be detectable as a duplicate and mergeable WITHOUT destroying the
// identity it carries.
//
// Three distinct defects are covered here, and they are separable:
//
//  1. Detection dropped platform-only rows entirely (the WHERE a.path <> ''
//     filter), so the pair never appeared in the report and the merge endpoint
//     answered HTTP 422 "merge target is stale" for a group that structurally
//     could not exist.
//  2. The merge deleted the loser row outright, and ON DELETE CASCADE took its
//     artist_provider_ids / artist_platform_ids / artist_libraries /
//     metadata_changes with it. The Emby item mapping in particular is what
//     stops the next populate from recreating the duplicate, so destroying it
//     made the merge silently self-undoing.
//  3. A survivor with no path made every filepath.Join(survivorPath, child)
//     RELATIVE, so the commit phase moved the losers' album directories into
//     the server process working directory, outside every library.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedMixedPair builds the shape reported on #2730: one LOCAL row with a
// filesystem path and one PLATFORM-ONLY row with an empty path, byte-identical
// names, and the same MusicBrainz ID on both. Returns the service, the raw DB
// (for direct row assertions), and the two artist IDs.
func seedMixedPair(t *testing.T) (svc *Service, db *sql.DB, localID, platformOnlyID string) {
	t.Helper()
	db = newTestDB(t)
	svc = NewService(db)
	ctx := context.Background()
	root := t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-2730', 'lib-2730', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	localPath := filepath.Join(root, "Northfield Chorale")
	if err := os.MkdirAll(filepath.Join(localPath, "Album One"), 0o755); err != nil {
		t.Fatalf("mkdir local artist dir: %v", err)
	}

	local := &Artist{Name: "Northfield Chorale", SortName: "Northfield Chorale", Path: localPath, LibraryID: "lib-2730"}
	if err := svc.Create(ctx, local); err != nil {
		t.Fatalf("Create local: %v", err)
	}
	// Platform-only row: no path at all. This is what an Emby or Jellyfin
	// populate creates for an artist with no directory in a filesystem library.
	platformOnly := &Artist{Name: "Northfield Chorale", SortName: "Northfield Chorale", Path: ""}
	if err := svc.Create(ctx, platformOnly); err != nil {
		t.Fatalf("Create platform-only: %v", err)
	}

	for _, id := range []string{local.ID, platformOnly.ID} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES (?, 'musicbrainz', ?)`,
			id, testMBID2730); err != nil {
			t.Fatalf("seeding MBID for %s: %v", id, err)
		}
	}
	return svc, db, local.ID, platformOnly.ID
}

const testMBID2730 = "11111111-2222-3333-4444-555555555555"

// seedConnection inserts a connection row so artist_platform_ids (which has an
// FK to connections) can be populated.
func seedConnection(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, created_at, updated_at)
		 VALUES (?, ?, 'emby', 'http://localhost', '', 1, datetime('now'), datetime('now'))`,
		id, id); err != nil {
		t.Fatalf("seeding connection %s: %v", id, err)
	}
}

// --- Detection ---------------------------------------------------------------

// TestDetectDuplicates_MixedLocalAndPlatformOnly is the #2730 detection
// regression guard: a local row and a platform-only row that share a name key
// and an MBID must form ONE group, and the platform-only member must be marked
// as such so the UI and the survivor guard can route on it.
func TestDetectDuplicates_MixedLocalAndPlatformOnly(t *testing.T) {
	_, db, localID, platformOnlyID := seedMixedPair(t)
	ctx := context.Background()

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1 (the local + platform-only pair)", len(groups))
	}
	g := groups[0]
	if len(g.Members) != 2 {
		t.Fatalf("len(Members) = %d, want 2", len(g.Members))
	}

	byID := make(map[string]NearDuplicateArtist, 2)
	for _, m := range g.Members {
		byID[m.ID] = m
	}
	local, ok := byID[localID]
	if !ok {
		t.Fatalf("local artist %s missing from the group", localID)
	}
	platformOnly, ok := byID[platformOnlyID]
	if !ok {
		t.Fatalf("platform-only artist %s missing from the group", platformOnlyID)
	}
	if local.PlatformOnly {
		t.Errorf("local member PlatformOnly = true, want false (it has path %q)", local.Path)
	}
	if !platformOnly.PlatformOnly {
		t.Errorf("platform-only member PlatformOnly = false, want true (its path is empty)")
	}
	// Both carry the same MBID, so the group is MBID-confirmed.
	if g.Reason != "mbid" {
		t.Errorf("group Reason = %q, want %q", g.Reason, "mbid")
	}
}

// TestDetectDuplicates_TwoPlatformOnlyRowsGroup covers the all-path-less group:
// two platform-only rows (an Emby row and a Jellyfin row for the same artist)
// must still be detected. Nothing about detection depends on a filesystem path.
func TestDetectDuplicates_TwoPlatformOnlyRowsGroup(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := newSQLiteArtistRepo(db)

	var ids []string
	for range 2 {
		a := &Artist{Name: "Harbor Line Quartet", Path: ""}
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("seeding platform-only artist: %v", err)
		}
		ids = append(ids, a.ID)
	}

	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1 (two platform-only rows sharing a name key)", len(groups))
	}
	for _, m := range groups[0].Members {
		if !m.PlatformOnly {
			t.Errorf("member %s PlatformOnly = false, want true", m.ID)
		}
	}
	_ = ids
}

// --- The path-less survivor refusal ------------------------------------------

// TestMergeArtists_RefusesPathlessSurvivor is the guard for the album-escape
// defect. With a path-less survivor, filepath.Join("", child) yields a RELATIVE
// path and the commit phase relocates the loser's albums into the server's
// working directory. The merge must refuse before reaching that code.
//
// The assertion is deliberately two-part: the sentinel AND the absence of any
// filesystem effect. A guard that returned the right error after already moving
// a directory would pass a sentinel-only check.
func TestMergeArtists_RefusesPathlessSurvivor(t *testing.T) {
	svc, _, localID, platformOnlyID := seedMixedPair(t)
	ctx := context.Background()

	// Run from an isolated working directory so a regression (a stray relative
	// rename) lands somewhere observable rather than in the repo.
	sandbox := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(sandbox); err != nil {
		t.Fatalf("chdir sandbox: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	_, err = svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  platformOnlyID, // the path-less row
		LoserIDs:    []string{localID},
		ArticleMode: "prefix",
	})
	if !errors.Is(err, ErrMergeSurvivorPathless) {
		t.Fatalf("MergeArtists error = %v, want ErrMergeSurvivorPathless", err)
	}

	entries, readErr := os.ReadDir(sandbox)
	if readErr != nil {
		t.Fatalf("reading sandbox: %v", readErr)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("working directory contains %v after the refused merge, want empty: "+
			"album content escaped the library", names)
	}

	// The loser row must still exist: a refused merge deletes nothing.
	var count int
	if err := svc.artists.(*sqliteArtistRepo).db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artists WHERE id = ?`, localID).Scan(&count); err != nil {
		t.Fatalf("counting local row: %v", err)
	}
	if count != 1 {
		t.Errorf("local artist row count = %d, want 1 (a refused merge must not delete)", count)
	}
}

// TestMergeArtists_AllPathlessSurvivorAllowed pins the OTHER side of the guard.
// The refusal is conditional on some member having a path; when NO member does
// (two platform-only rows), there is no filesystem phase to get wrong and the
// merge must proceed as a pure database carry.
//
// Without this test the guard could be written as the simpler, wrong
// "survivor.Path == ” -> refuse", which would make every platform-only pair
// permanently unmergeable -- reintroducing #2730 by a different route.
func TestMergeArtists_AllPathlessSurvivorAllowed(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteArtistRepo(db)

	var ids []string
	for range 2 {
		a := &Artist{Name: "Harbor Line Quartet", Path: ""}
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("seeding platform-only artist: %v", err)
		}
		ids = append(ids, a.ID)
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  ids[0],
		LoserIDs:    []string{ids[1]},
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists on an all-path-less group: %v, want success", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists WHERE id = ?`, ids[1]).Scan(&count); err != nil {
		t.Fatalf("counting loser: %v", err)
	}
	if count != 0 {
		t.Errorf("loser row count = %d, want 0 (the merge should have deleted it)", count)
	}
}

// --- Identity carry ----------------------------------------------------------

// TestMergeArtists_CarriesPlatformIDs proves the loser's platform mapping is
// re-pointed at the survivor rather than cascade-deleted. This is the mapping
// that stops the next Emby populate from recreating the duplicate, so losing it
// makes the merge self-undoing.
func TestMergeArtists_CarriesPlatformIDs(t *testing.T) {
	svc, db, localID, platformOnlyID := seedMixedPair(t)
	ctx := context.Background()
	seedConnection(t, db, "conn-emby")

	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		 VALUES (?, 'conn-emby', 'emby-item-9001')`, platformOnlyID); err != nil {
		t.Fatalf("seeding loser platform id: %v", err)
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  localID,
		LoserIDs:    []string{platformOnlyID},
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	var owner string
	if err := db.QueryRowContext(ctx,
		`SELECT artist_id FROM artist_platform_ids WHERE platform_artist_id = 'emby-item-9001'`,
	).Scan(&owner); err != nil {
		t.Fatalf("the Emby mapping no longer exists after the merge (cascade-deleted): %v", err)
	}
	if owner != localID {
		t.Errorf("emby-item-9001 belongs to artist %s, want the survivor %s", owner, localID)
	}
}

// TestMergeArtists_CarriesPlatformIDsSkipsPrimaryKeyConflict covers the way
// artist_platform_ids CAN refuse a carried row: PRIMARY KEY
// (artist_id, connection_id), when the survivor is already mapped on the same
// connection the loser is mapped on.
//
// This is NOT a benign skip, and the assertions below are deliberately about
// the PEER ITEM rather than merely about row hygiene. Because
// (connection_id, platform_artist_id) is UNIQUE, a survivor and a loser mapped
// on the SAME connection are necessarily mapped to two DIFFERENT platform
// items, and BOTH items exist on the peer. That is the reported shape of
// #2730: one Emby item resolved to the local artist and the other became the
// platform-only row. So the refused row is not dead identity -- it is a live
// peer item that ends up mapped to nothing.
//
// The schema makes retention impossible (the PK allows exactly one row per
// artist+connection), and the post-merge refresh cannot recover it either: it
// evicts loser items whose on-disk directory disappeared, and a platform-only
// loser never had one. So the contract is that the operator MUST be told,
// by name, which mapping was dropped. Silence here means the next populate
// finds no mapping, creates a fresh row, and the merge undoes itself.
//
// The table's OTHER unique constraint, UNIQUE (connection_id,
// platform_artist_id) from #1076, is deliberately NOT tested as a refusal: see
// TestMergeArtists_CarryCannotViolateItemUniqueIndex for why it is structurally
// unreachable under this UPDATE.
func TestMergeArtists_CarriesPlatformIDsSkipsPrimaryKeyConflict(t *testing.T) {
	svc, db, localID, platformOnlyID := seedMixedPair(t)
	ctx := context.Background()
	seedConnection(t, db, "conn-emby")

	// Both rows mapped on the SAME connection: the carry's UPDATE would move
	// the loser onto (survivorID, conn-emby), which the survivor already holds.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		 VALUES (?, 'conn-emby', 'emby-survivor')`, localID); err != nil {
		t.Fatalf("seeding survivor platform id: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		 VALUES (?, 'conn-emby', 'emby-loser')`, platformOnlyID); err != nil {
		t.Fatalf("seeding loser platform id: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  localID,
		LoserIDs:    []string{platformOnlyID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v, want success (a refused carry row must not fail the merge)", err)
	}

	// THE POINT OF THIS TEST: the operator must be told the loser's Emby item
	// is now mapped to nothing. Without this warning the merge reports clean
	// success while leaving 'emby-loser' unmapped, and the next populate
	// recreates the duplicate.
	var warned bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "emby-loser") && strings.Contains(w, "conn-emby") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning names the dropped mapping (connection conn-emby, item emby-loser); "+
			"got Warnings = %v. A silent drop leaves the peer item unmapped and the next "+
			"populate recreates the duplicate, undoing the merge", res.Warnings)
	}

	// The survivor keeps its OWN mapping on the contested connection.
	var item string
	if err := db.QueryRowContext(ctx,
		`SELECT platform_artist_id FROM artist_platform_ids WHERE artist_id = ? AND connection_id = 'conn-emby'`,
		localID).Scan(&item); err != nil {
		t.Fatalf("survivor's own mapping vanished: %v", err)
	}
	if item != "emby-survivor" {
		t.Errorf("survivor's conn-emby mapping = %q, want %q (the survivor's own value must win)",
			item, "emby-survivor")
	}

	// No row may remain attributed to the deleted loser.
	var orphans int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = ?`, platformOnlyID,
	).Scan(&orphans); err != nil {
		t.Fatalf("counting orphan platform ids: %v", err)
	}
	if orphans != 0 {
		t.Errorf("loser %s still owns %d platform id row(s) after the merge, want 0",
			platformOnlyID, orphans)
	}
}

// TestMergeArtists_CarryCannotViolateItemUniqueIndex documents WHY the #1076
// unique index -- UNIQUE (connection_id, platform_artist_id) -- is not a
// refusal path the carry has to handle.
//
// The carry UPDATE changes only artist_id. That column is not part of the
// index, so no row's (connection_id, platform_artist_id) tuple can change, so
// the index holds after the update exactly when it held before. A state that
// would violate it post-carry is already unrepresentable pre-carry, because
// the index rejects it at insert time.
//
// This test pins the consequence: a survivor and a loser holding the SAME
// platform item id on DIFFERENT connections is legal, and BOTH mappings carry
// through intact. If someone later rewrites the carry to re-key rows (an
// INSERT ... SELECT, or touching connection_id), this test is what notices.
func TestMergeArtists_CarryCannotViolateItemUniqueIndex(t *testing.T) {
	svc, db, localID, platformOnlyID := seedMixedPair(t)
	ctx := context.Background()
	seedConnection(t, db, "conn-emby")
	seedConnection(t, db, "conn-jellyfin")

	// Same platform item id, different connections. Legal before the carry
	// (the index keys on connection too) and legal after it.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		 VALUES (?, 'conn-emby', 'item-42')`, localID); err != nil {
		t.Fatalf("seeding survivor platform id: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		 VALUES (?, 'conn-jellyfin', 'item-42')`, platformOnlyID); err != nil {
		t.Fatalf("seeding loser platform id: %v", err)
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  localID,
		LoserIDs:    []string{platformOnlyID},
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = ? AND platform_artist_id = 'item-42'`,
		localID).Scan(&count); err != nil {
		t.Fatalf("counting survivor mappings: %v", err)
	}
	if count != 2 {
		t.Errorf("survivor holds item-42 on %d connection(s), want 2 (both mappings must carry; "+
			"the unique index keys on connection too, so neither refuses)", count)
	}
}

// TestMergeArtists_CarriesPlatformIDsAcrossMultipleLosers pins the carry when
// two losers collide with each OTHER rather than with the survivor. The first
// loser's mapping moves onto the survivor, and the second loser's mapping on
// that same connection then hits the PK against the row the first loser just
// installed. First-carried wins; the merge still succeeds and leaves no orphan.
func TestMergeArtists_CarriesPlatformIDsAcrossMultipleLosers(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteArtistRepo(db)
	root := t.TempDir()
	seedConnection(t, db, "conn-emby")

	survivorPath := filepath.Join(root, "Harbor Line Quartet")
	if err := os.MkdirAll(survivorPath, 0o755); err != nil {
		t.Fatalf("mkdir survivor: %v", err)
	}
	survivor := &Artist{Name: "Harbor Line Quartet", Path: survivorPath}
	if err := repo.Create(ctx, survivor); err != nil {
		t.Fatalf("seeding survivor: %v", err)
	}
	var loserIDs []string
	for i := range 2 {
		a := &Artist{Name: "Harbor Line Quartet", Path: ""}
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("seeding loser: %v", err)
		}
		loserIDs = append(loserIDs, a.ID)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
			 VALUES (?, 'conn-emby', ?)`, a.ID, fmt.Sprintf("emby-loser-%d", i)); err != nil {
			t.Fatalf("seeding loser platform id: %v", err)
		}
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivor.ID,
		LoserIDs:    loserIDs,
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v, want success", err)
	}

	// Exactly one mapping on that connection: the PK allows no more.
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = ? AND connection_id = 'conn-emby'`,
		survivor.ID).Scan(&count); err != nil {
		t.Fatalf("counting survivor mappings: %v", err)
	}
	if count != 1 {
		t.Errorf("survivor holds %d conn-emby mapping(s), want 1", count)
	}
	// And nothing left dangling on either loser.
	for _, id := range loserIDs {
		var orphans int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM artist_platform_ids WHERE artist_id = ?`, id).Scan(&orphans); err != nil {
			t.Fatalf("counting orphans for %s: %v", id, err)
		}
		if orphans != 0 {
			t.Errorf("loser %s still owns %d platform id row(s), want 0", id, orphans)
		}
	}
}

// TestMergeArtists_CarriesProviderIDsFillEmpty proves provider IDs are carried
// with fill-empty semantics: the loser supplies providers the survivor lacks,
// and never overwrites a value the survivor already holds.
func TestMergeArtists_CarriesProviderIDsFillEmpty(t *testing.T) {
	svc, db, localID, platformOnlyID := seedMixedPair(t)
	ctx := context.Background()

	// Survivor already has a discogs id; the loser has a DIFFERENT one plus a
	// spotify id the survivor lacks entirely.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
		 VALUES (?, 'discogs', 'survivor-discogs')`, localID); err != nil {
		t.Fatalf("seeding survivor discogs: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
		 VALUES (?, 'discogs', 'loser-discogs'), (?, 'spotify', 'loser-spotify')`,
		platformOnlyID, platformOnlyID); err != nil {
		t.Fatalf("seeding loser providers: %v", err)
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  localID,
		LoserIDs:    []string{platformOnlyID},
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	got := make(map[string]string)
	rows, err := db.QueryContext(ctx,
		`SELECT provider, provider_id FROM artist_provider_ids WHERE artist_id = ?`, localID)
	if err != nil {
		t.Fatalf("querying survivor providers: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var provider, id string
		if err := rows.Scan(&provider, &id); err != nil {
			t.Fatalf("scanning provider row: %v", err)
		}
		got[provider] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating provider rows: %v", err)
	}

	if got["discogs"] != "survivor-discogs" {
		t.Errorf("discogs = %q, want %q (the survivor's own value must win)", got["discogs"], "survivor-discogs")
	}
	if got["spotify"] != "loser-spotify" {
		t.Errorf("spotify = %q, want %q (the loser fills a gap the survivor had)", got["spotify"], "loser-spotify")
	}
	if got["musicbrainz"] != testMBID2730 {
		t.Errorf("musicbrainz = %q, want %q", got["musicbrainz"], testMBID2730)
	}
}

// TestMergeArtists_CarriesLibraryMembership proves the survivor inherits the
// loser's library membership. This is what makes the merged artist visible in
// the Emby library view afterwards, which is the operator-visible point of
// merging a platform-only row at all.
func TestMergeArtists_CarriesLibraryMembership(t *testing.T) {
	svc, db, localID, platformOnlyID := seedMixedPair(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-emby', 'Music (Emby)', '', 'regular', 'emby', datetime('now'), datetime('now'))`,
	); err != nil {
		t.Fatalf("seeding emby library: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_libraries (artist_id, library_id, source) VALUES (?, 'lib-emby', 'emby')`,
		platformOnlyID); err != nil {
		t.Fatalf("seeding loser library membership: %v", err)
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  localID,
		LoserIDs:    []string{platformOnlyID},
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_libraries WHERE artist_id = ? AND library_id = 'lib-emby'`,
		localID).Scan(&count); err != nil {
		t.Fatalf("counting survivor library membership: %v", err)
	}
	if count != 1 {
		t.Errorf("survivor membership in lib-emby = %d row(s), want 1", count)
	}
}

// TestMergeArtists_CarriesMetadataChanges proves the loser's change history
// moves to the survivor. On the reported incident the loser's history holds the
// wrong-MBID rule fix that caused the fork, so destroying it destroys the audit
// trail for the very defect being repaired.
func TestMergeArtists_CarriesMetadataChanges(t *testing.T) {
	svc, db, localID, platformOnlyID := seedMixedPair(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source)
		 VALUES ('mc-2730', ?, 'musicbrainz_id', '', 'wrong-mbid', 'rule_fix')`,
		platformOnlyID); err != nil {
		t.Fatalf("seeding loser metadata change: %v", err)
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  localID,
		LoserIDs:    []string{platformOnlyID},
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	var owner string
	if err := db.QueryRowContext(ctx,
		`SELECT artist_id FROM metadata_changes WHERE id = 'mc-2730'`).Scan(&owner); err != nil {
		t.Fatalf("the loser's change history was destroyed by the merge: %v", err)
	}
	if owner != localID {
		t.Errorf("metadata change mc-2730 belongs to %s, want the survivor %s", owner, localID)
	}
}

// TestMergeArtists_CarryAppliesToPathBearingLoser pins that the carry is NOT
// conditional on the loser being platform-only. Before #2730 a path-bearing
// loser also lost its Emby mapping to the cascade; that was a latent defect in
// the ordinary merge and it is fixed by the same code.
//
// Without this test the carry could be written inside a `if loser.Path == ""`
// branch and the ordinary-merge defect would silently remain.
func TestMergeArtists_CarryAppliesToPathBearingLoser(t *testing.T) {
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	seedConnection(t, db, "conn-emby")

	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		 VALUES (?, 'conn-emby', 'emby-path-bearing-loser')`, loserID); err != nil {
		t.Fatalf("seeding loser platform id: %v", err)
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	var owner string
	if err := db.QueryRowContext(ctx,
		`SELECT artist_id FROM artist_platform_ids WHERE platform_artist_id = 'emby-path-bearing-loser'`,
	).Scan(&owner); err != nil {
		t.Fatalf("a PATH-BEARING loser's Emby mapping was cascade-deleted: %v", err)
	}
	if owner != survivorID {
		t.Errorf("mapping belongs to %s, want the survivor %s", owner, survivorID)
	}
}

// TestMergeArtists_NoCarryWhenLoserRowSurvives pins the carry to the SAME gate
// the row deletion uses. commitMergeDB only deletes losers recorded in
// result.Removed; a loser left on disk keeps its row so the scanner reconciles
// to it (#2010). Carrying that loser's identity away while its row survives
// would strip a live artist of its platform mapping.
//
// The setup blocks removal the documented way: the survivor has a DIRECTORY
// where the loser has a loose FILE of the same name, so executeLoserMerge
// leaves the file in place, the loser dir is non-empty, and removed=false.
func TestMergeArtists_NoCarryWhenLoserRowSurvives(t *testing.T) {
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	seedConnection(t, db, "conn-emby")

	survivor, err := svc.GetByID(ctx, survivorID)
	if err != nil {
		t.Fatalf("loading survivor: %v", err)
	}
	loser, err := svc.GetByID(ctx, loserID)
	if err != nil {
		t.Fatalf("loading loser: %v", err)
	}
	// mergeSetup puts artist.nfo (a loose file) in the loser. Make the
	// survivor's same-named entry a DIRECTORY: executeLoserMerge then refuses
	// to delete the loser's copy, so the loser dir stays non-empty.
	if err := os.Mkdir(filepath.Join(survivor.Path, "artist.nfo"), 0o755); err != nil {
		t.Fatalf("creating blocking directory: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		 VALUES (?, 'conn-emby', 'emby-surviving-loser')`, loserID); err != nil {
		t.Fatalf("seeding loser platform id: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}
	// Precondition: the scenario really did leave the loser row in place. If
	// this stops holding the assertion below becomes vacuous.
	if len(res.LosersDeleted) != 0 {
		t.Fatalf("precondition failed: loser was deleted (%v); the test no longer exercises "+
			"the row-survives path", res.LosersDeleted)
	}
	_ = loser

	var owner string
	if err := db.QueryRowContext(ctx,
		`SELECT artist_id FROM artist_platform_ids WHERE platform_artist_id = 'emby-surviving-loser'`,
	).Scan(&owner); err != nil {
		t.Fatalf("querying platform id: %v", err)
	}
	if owner != loserID {
		t.Errorf("mapping belongs to %s, want the still-live loser %s: identity was carried away "+
			"from a row that was not deleted", owner, loserID)
	}
}

// --- Platform refresh reachability -------------------------------------------

// TestMergeAndReconcile_PlatformOnlyLoserReachesRefresh proves the post-merge
// platform refresh actually runs for a platform-only loser.
//
// This is the end-to-end anti-resurrection guarantee. Carrying the mapping in
// the database is only half the job: SyncMergeRefresh is what makes the peer
// stop pointing at the deleted loser item. refreshAffectedPlatforms early-
// returns when there is neither an affected connection nor a survivor MBID, so
// reachability here is a real question and is asserted rather than assumed.
func TestMergeAndReconcile_PlatformOnlyLoserReachesRefresh(t *testing.T) {
	svc, db, localID, platformOnlyID := seedMixedPair(t)
	ctx := context.Background()
	seedConnection(t, db, "conn-emby")

	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_platform_ids (artist_id, connection_id, platform_artist_id)
		 VALUES (?, 'conn-emby', 'emby-item-9001')`, platformOnlyID); err != nil {
		t.Fatalf("seeding loser platform id: %v", err)
	}

	ref := &recordingRefresher{}
	svc.SetPlatformMergeRefresher(ref)

	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID:  localID,
		LoserIDs:    []string{platformOnlyID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile: %v", err)
	}

	if ref.calls != 1 {
		t.Fatalf("SyncMergeRefresh called %d time(s), want 1: the platform-only merge never "+
			"reached the peer reconciliation, so Emby keeps its stale item and the next "+
			"populate recreates the duplicate", ref.calls)
	}
	if ref.survivorID != localID {
		t.Errorf("SyncMergeRefresh survivorID = %s, want %s", ref.survivorID, localID)
	}
	// The loser's platform item must be passed through so the refresh can scope
	// its eviction to that specific item rather than a full library scan.
	got := ref.loserPlatformIDs["conn-emby"]
	if len(got) != 1 || got[0] != "emby-item-9001" {
		t.Errorf("loserPlatformIDs[conn-emby] = %v, want [emby-item-9001]", got)
	}
	if len(res.AffectedConnectionIDs) != 1 || res.AffectedConnectionIDs[0] != "conn-emby" {
		t.Errorf("AffectedConnectionIDs = %v, want [conn-emby]", res.AffectedConnectionIDs)
	}
}

// --- Scoping: a bystander member must not veto the request -------------------
//
// Detection stopped excluding platform-only rows, so a near-duplicate group can
// now contain members the operator did not ask to merge. Both checks below used
// to scan every group member, which turned an unrelated row into a veto.

// seedTrioOneLocalTwoPlatformOnly builds the group shape that makes bystander
// bugs reachable: one local row with a folder plus two platform-only rows, all
// sharing a name key so detection groups them.
func seedTrioOneLocalTwoPlatformOnly(t *testing.T) (svc *Service, db *sql.DB, localID string, platformOnlyIDs []string) {
	t.Helper()
	db = newTestDB(t)
	svc = NewService(db)
	ctx := context.Background()
	root := t.TempDir()

	localPath := filepath.Join(root, "Harbor Line Quartet")
	if err := os.MkdirAll(filepath.Join(localPath, "Album One"), 0o755); err != nil {
		t.Fatalf("mkdir local artist dir: %v", err)
	}
	repo := newSQLiteArtistRepo(db)
	local := &Artist{Name: "Harbor Line Quartet", SortName: "Harbor Line Quartet", Path: localPath}
	if err := repo.Create(ctx, local); err != nil {
		t.Fatalf("seeding local: %v", err)
	}
	for range 2 {
		a := &Artist{Name: "Harbor Line Quartet", SortName: "Harbor Line Quartet", Path: ""}
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("seeding platform-only: %v", err)
		}
		platformOnlyIDs = append(platformOnlyIDs, a.ID)
	}
	return svc, db, local.ID, platformOnlyIDs
}

// TestMergeArtists_PathlessGuardIgnoresBystander pins the OVER-refusal half of
// the guard's scope. Merging two platform-only rows into each other has no
// filesystem phase, so it is safe -- even though a folder-bearing bystander sits
// in the same group. Judging the request by that bystander refused a safe merge
// with a 422 telling the operator to keep an entry they never asked to touch.
func TestMergeArtists_PathlessGuardIgnoresBystander(t *testing.T) {
	svc, db, localID, pOnly := seedTrioOneLocalTwoPlatformOnly(t)
	ctx := context.Background()

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  pOnly[0],
		LoserIDs:    []string{pOnly[1]}, // the local bystander is NOT requested
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v, want success (nothing being merged has a folder; "+
			"the local bystander is not part of this request)", err)
	}

	// The bystander is untouched.
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists WHERE id = ?`, localID).Scan(&count); err != nil {
		t.Fatalf("counting bystander: %v", err)
	}
	if count != 1 {
		t.Errorf("bystander row count = %d, want 1", count)
	}
}

// TestMergeArtists_PathlessGuardStillRefusesRequestedLoser pins the UNDER-refusal
// half, which the scoping fix must not weaken. When a REQUESTED loser has a
// folder and the survivor does not, the refusal is absolute: this is the
// album-escape data loss, where filepath.Join("", child) is relative and the
// commit phase moves albums into the server's working directory.
func TestMergeArtists_PathlessGuardStillRefusesRequestedLoser(t *testing.T) {
	svc, _, localID, pOnly := seedTrioOneLocalTwoPlatformOnly(t)
	ctx := context.Background()

	sandbox := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(sandbox); err != nil {
		t.Fatalf("chdir sandbox: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// Same group as the test above, but the folder-bearing row IS requested.
	_, err = svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  pOnly[0],
		LoserIDs:    []string{localID, pOnly[1]},
		ArticleMode: "prefix",
	})
	if !errors.Is(err, ErrMergeSurvivorPathless) {
		t.Fatalf("MergeArtists error = %v, want ErrMergeSurvivorPathless", err)
	}
	entries, readErr := os.ReadDir(sandbox)
	if readErr != nil {
		t.Fatalf("reading sandbox: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("working directory is not empty after the refused merge: album content escaped")
	}
}

// TestMergeArtists_LockedBystanderDoesNotBlockMerge is the regression guard for
// the lock check's scope. LockSync locks artists from a peer's IsLocked state by
// walking artist_platform_ids, so platform-only rows are exactly the population
// that gets locked from Emby. Once detection admitted those rows into groups, an
// Emby-locked ghost row started returning 423 for a local-to-local merge that
// has nothing to do with it -- a merge that worked before.
func TestMergeArtists_LockedBystanderDoesNotBlockMerge(t *testing.T) {
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	// A platform-only row in the same group, locked from the platform side.
	repo := newSQLiteArtistRepo(db)
	ghost := &Artist{Name: "The Cure", SortName: "The Cure", Path: ""}
	if err := repo.Create(ctx, ghost); err != nil {
		t.Fatalf("seeding platform-only ghost: %v", err)
	}
	if err := repo.SetLock(ctx, ghost.ID, true, "platform"); err != nil {
		t.Fatalf("locking ghost: %v", err)
	}
	// Precondition: the ghost really is in the same group, otherwise this test
	// passes vacuously without ever exercising the bystander path.
	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("DetectDuplicates: %v", err)
	}
	var grouped bool
	for _, g := range groups {
		var hasGhost, hasSurvivor bool
		for _, m := range g.Members {
			if m.ID == ghost.ID {
				hasGhost = true
			}
			if m.ID == survivorID {
				hasSurvivor = true
			}
		}
		if hasGhost && hasSurvivor {
			grouped = true
		}
	}
	if !grouped {
		t.Fatalf("precondition failed: the locked platform-only row is not grouped with the survivor, "+
			"so this test would not exercise the locked-bystander path (groups=%d)", len(groups))
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID}, // the locked ghost is NOT requested
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v, want success (a locked bystander must not block a merge "+
			"of two other artists)", err)
	}
}

// TestMergeArtists_LockedRequestedMemberStillRefuses pins the other direction:
// narrowing the lock check to the requested members must not stop a lock on the
// survivor or on a requested loser from refusing. A locked artist opts out of
// destructive operations and merging deletes the loser row.
func TestMergeArtists_LockedRequestedMemberStillRefuses(t *testing.T) {
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	repo := newSQLiteArtistRepo(db)

	// "user" is one of the lock_source values the schema CHECK permits.
	if err := repo.SetLock(ctx, loserID, true, "user"); err != nil {
		t.Fatalf("locking loser: %v", err)
	}
	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	}); !errors.Is(err, ErrMergeLocked) {
		t.Fatalf("MergeArtists error = %v, want ErrMergeLocked (a locked REQUESTED loser must refuse)", err)
	}
}

// --- Reconcile on a path-less survivor ---------------------------------------

// TestMergeAndReconcile_AllPathlessEmitsNoRenameWarning pins that a successful
// all-platform-only merge reports cleanly. The canonical-path reconciliation
// used to run against a survivor with no directory, get ErrRenameNoPath back,
// and surface "could not move survivor to canonical directory" in the modal --
// a failure warning for an operation that was never applicable.
func TestMergeAndReconcile_AllPathlessEmitsNoRenameWarning(t *testing.T) {
	db := newTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	repo := newSQLiteArtistRepo(db)

	var ids []string
	for range 2 {
		a := &Artist{Name: "Harbor Line Quartet", SortName: "Harbor Line Quartet", Path: ""}
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("seeding platform-only artist: %v", err)
		}
		ids = append(ids, a.ID)
	}

	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID:  ids[0],
		LoserIDs:    []string{ids[1]},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile: %v", err)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "canonical directory") {
			t.Errorf("merge succeeded but reported a canonical-rename failure: %q. "+
				"A path-less survivor has no directory to make canonical", w)
		}
	}
}

// --- The explicit path-less filesystem skips ---------------------------------

// TestPreflightOneLoser_PathlessSkipsFilesystem and its executeLoserMerge
// counterpart pin the EXPLICIT `loser.Path == ""` early returns. Both are
// semantically equivalent today to falling through to os.Lstat(""), which
// returns ENOENT and lands on the crash-recovery branch -- so without these
// tests the explicit guards could be deleted with the whole suite still green,
// which is exactly the "one refactor away from breaking" risk their comments
// cite. These call the helpers directly because that equivalence is invisible
// from the orchestrator level.
func TestPreflightOneLoser_PathlessSkipsFilesystem(t *testing.T) {
	t.Parallel()
	// A survivor path that does NOT exist. If the path-less loser were walked
	// rather than skipped, enumerateChildren would run against "" and the
	// collision walk would report an error instead of a clean no-op.
	result := &MergeResult{}
	if err := preflightOneLoser(
		NearDuplicateArtist{ID: "loser", Path: "", PlatformOnly: true},
		filepath.Join(t.TempDir(), "survivor"),
		result,
	); err != nil {
		t.Fatalf("preflightOneLoser on a path-less loser: %v, want nil", err)
	}
	if len(result.Conflicts) != 0 || len(result.Moved) != 0 || len(result.Deleted) != 0 {
		t.Errorf("path-less loser produced filesystem plan entries: conflicts=%d moved=%d deleted=%d, want all 0",
			len(result.Conflicts), len(result.Moved), len(result.Deleted))
	}
}

func TestExecuteLoserMerge_PathlessReportsRemoved(t *testing.T) {
	t.Parallel()
	result := &MergeResult{}
	removed, err := executeLoserMerge(
		NearDuplicateArtist{ID: "loser", Path: "", PlatformOnly: true},
		filepath.Join(t.TempDir(), "survivor"),
		result,
	)
	if err != nil {
		t.Fatalf("executeLoserMerge on a path-less loser: %v, want nil", err)
	}
	// removed=true is what lets commitMergeDB delete the row and carry its
	// identity. Returning false would leave the platform-only duplicate in
	// place and silently defeat the whole feature.
	if !removed {
		t.Error("removed = false, want true: the DB phase is gated on this, so a path-less " +
			"loser would never be deleted or have its identity carried")
	}
	if len(result.Moved) != 0 || len(result.Warnings) != 0 {
		t.Errorf("path-less loser produced moved=%d warnings=%v, want none", len(result.Moved), result.Warnings)
	}
}
