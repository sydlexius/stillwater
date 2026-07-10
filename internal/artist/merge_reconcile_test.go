package artist

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingSyncer implements PlatformRenameSyncer, recording every SyncRename
// call so the reconcile tests can assert the chained canonical rename actually
// propagated to platforms. Returns one canned OK entry so
// CanonicalRenameResult.Platforms is non-empty.
type recordingSyncer struct{ renamed []string }

func (r *recordingSyncer) SyncRename(_ context.Context, artistID, _, newPath string) ([]PlatformRemapResult, error) {
	r.renamed = append(r.renamed, artistID+"->"+newPath)
	return []PlatformRemapResult{{ConnectionID: "c1", Result: PlatformRemapOK}}, nil
}

// recordingRefresher implements PlatformMergeRefresher, recording the survivor
// ID and connection set MergeAndReconcile passes to the fan-out. failConns
// marks specific connections as failed; err makes the whole call fail.
type recordingRefresher struct {
	survivorID string
	conns      []string
	calls      int
	err        error
	failConns  map[string]string // connID -> error string -> PlatformRemapFailed
}

func (r *recordingRefresher) SyncMergeRefresh(_ context.Context, survivorID string, connectionIDs []string) ([]PlatformRefreshResult, error) {
	r.calls++
	r.survivorID = survivorID
	r.conns = append(r.conns, connectionIDs...)
	if r.err != nil {
		return nil, r.err
	}
	out := make([]PlatformRefreshResult, 0, len(connectionIDs))
	for _, c := range connectionIDs {
		res := PlatformRefreshResult{ConnectionID: c, Result: PlatformRemapOK}
		if msg, bad := r.failConns[c]; bad {
			res.Result = PlatformRemapFailed
			res.Error = msg
		}
		out = append(out, res)
	}
	return out, nil
}

// seedSurvivorMBID inserts a musicbrainz provider row for the survivor so
// DetectDuplicates surfaces a non-empty MBID and MergeResult.SurvivorMBID is
// populated. This is the signal refreshAffectedPlatforms uses to reach the
// Lidarr resolve-by-MBID self-heal on a fully-unlinked merge (#2325).
func seedSurvivorMBID(t *testing.T, db *sql.DB, artistID, mbid string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES (?, 'musicbrainz', ?)`,
		artistID, mbid); err != nil {
		t.Fatalf("seeding survivor MBID: %v", err)
	}
}

// TestMergeAndReconcile_FullyUnlinkedSurvivorWithMBIDReachesRefresh is the
// journey-level regression guard for the #2325 P1 reachability defect. A
// survivor that is already at its canonical basename (no rename fires) and has
// NO platform_ids row anywhere (AffectedConnectionIDs empty) -- the exact
// "fully-unlinked merge" the Lidarr self-heal exists for -- must STILL reach
// SyncMergeRefresh, because that is where the resolve-by-MBID self-heal lives.
// Before the guard fix, refreshAffectedPlatforms returned early on the empty
// affected set and this call never happened; this test FAILS on that code and
// PASSES with the MBID-aware guard. The recordingRefresher stands in for the
// publish layer (whose self-heal internals are unit-tested in that package);
// what is proven HERE is that the real merge entry point actually invokes it.
func TestMergeAndReconcile_FullyUnlinkedSurvivorWithMBIDReachesRefresh(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t) // survivor dir "The Cure" is prefix-canonical
	ctx := context.Background()
	ref := &recordingRefresher{}
	svc.SetPlatformMergeRefresher(ref)

	// Survivor has an MBID but NO connection / SetPlatformID -> fully unlinked.
	seedSurvivorMBID(t, db, survivorID, "11111111-1111-1111-1111-111111111111")

	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile: %v", err)
	}
	// Reachability is the whole point: SyncMergeRefresh must have been called
	// (self-heal's entry point) even though there are zero affected connections.
	if ref.calls != 1 {
		t.Fatalf("SyncMergeRefresh called %d times, want 1 (self-heal must be reachable on a fully-unlinked MBID merge)", ref.calls)
	}
	if ref.survivorID != survivorID {
		t.Errorf("refresh survivorID = %q, want %q", ref.survivorID, survivorID)
	}
	// Fully unlinked: the affected set passed through is empty (self-heal
	// discovers Lidarr links itself, inside SyncMergeRefresh).
	if len(ref.conns) != 0 {
		t.Errorf("refresh connections = %v, want empty (fully-unlinked survivor)", ref.conns)
	}
	// Confirm the reachability came from the MBID gate, not a canonical rename.
	if res.CanonicalRename != nil {
		t.Errorf("CanonicalRename = %+v, want nil (survivor already canonical)", res.CanonicalRename)
	}
}

// TestMergeAndReconcile_FullyUnlinkedSurvivorNoMBIDSkipsRefresh is the negative
// guard against over-broadening the #2325 fix: a survivor with NO affected
// connection AND NO MBID has nothing to reconcile, so SyncMergeRefresh must NOT
// be called (no spurious refresh). This pins the "only reach self-heal when
// there is an MBID" half of the relaxed guard.
func TestMergeAndReconcile_FullyUnlinkedSurvivorNoMBIDSkipsRefresh(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	ref := &recordingRefresher{}
	svc.SetPlatformMergeRefresher(ref)

	// No MBID seeded, no connection linked.
	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile: %v", err)
	}
	if ref.calls != 0 {
		t.Errorf("SyncMergeRefresh called %d times, want 0 (no MBID and no affected connection -> nothing to reconcile)", ref.calls)
	}
	if res.CanonicalRename != nil {
		t.Errorf("CanonicalRename = %+v, want nil", res.CanonicalRename)
	}
}

// TestMergeAndReconcile_FullyUnlinkedSurvivorInheritsMBIDReachesRefresh is the
// "one hop removed" variant of the #2325 reachability defect (CR-1). Here the
// survivor has NO MBID of its own; a loser carries one, so commitMergeDB's
// fill-empty inheritance stamps the loser's MBID onto the survivor's DB row.
// The survivor is already at its canonical basename (no rename fires) and has
// no platform_ids row anywhere (AffectedConnectionIDs empty). Because
// MergeResult.SurvivorMBID is snapshotted from survivor.MBID ("") before the
// inheritance runs, the reachability gate would still see "" and skip the
// Lidarr self-heal UNLESS commitMergeDB backfills the inherited MBID onto the
// in-memory result. This test proves that backfill: SyncMergeRefresh must be
// reached (ref.calls == 1) via the inherited MBID. Reverting the backfill in
// commitMergeDB makes this FAIL (ref.calls == 0).
func TestMergeAndReconcile_FullyUnlinkedSurvivorInheritsMBIDReachesRefresh(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t) // survivor dir "The Cure" is prefix-canonical
	ctx := context.Background()
	ref := &recordingRefresher{}
	svc.SetPlatformMergeRefresher(ref)

	// Survivor has NO MBID; the loser carries one. commitMergeDB inherits it.
	seedSurvivorMBID(t, db, loserID, "22222222-2222-2222-2222-222222222222")

	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile: %v", err)
	}
	// Reachability via the INHERITED MBID: the gate must see the backfilled
	// SurvivorMBID and invoke the self-heal entry point.
	if ref.calls != 1 {
		t.Fatalf("SyncMergeRefresh called %d times, want 1 (inherited MBID must backfill SurvivorMBID and reach self-heal)", ref.calls)
	}
	if res.SurvivorMBID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("res.SurvivorMBID = %q, want the inherited MBID (backfilled by commitMergeDB)", res.SurvivorMBID)
	}
	// Reachability came from the inherited MBID, not a canonical rename.
	if res.CanonicalRename != nil {
		t.Errorf("CanonicalRename = %+v, want nil (survivor already canonical)", res.CanonicalRename)
	}
}

// seedEmbyConn inserts the minimal "conn-emby" connections row so
// SetPlatformID's FK is satisfied. Mirrors the inline connection-row seeding
// the Task 2 tests use; all reconcile tests map the survivor to this one.
func seedEmbyConn(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		VALUES ('conn-emby', 'conn-emby', 'emby', 'http://x:8096', 'enc-key', 1, 'ok', datetime('now'), datetime('now'))`,
	); err != nil {
		t.Fatalf("seeding connection conn-emby: %v", err)
	}
}

// nonCanonicalMergeSetup builds a survivor whose on-disk basename is NOT the
// canonical directory for its name, so MergeAndReconcile's rename step fires.
// Survivor name "The Cure" is canonical as "The Cure" in prefix mode, but the
// directory basename is "Cure, The"; the loser lives elsewhere with a
// non-colliding album so the merge commits cleanly.
func nonCanonicalMergeSetup(t *testing.T) (svc *Service, db *sql.DB, survivorID, loserID string) {
	t.Helper()
	db = newTestDB(t)
	svc = NewService(db)
	ctx := context.Background()
	root := t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-merge', 'lib-merge', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	survivorPath := filepath.Join(root, "Cure, The") // non-canonical in prefix mode
	loserPath := filepath.Join(root, "Cure Duplicate")
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

	survivor := &Artist{Name: "The Cure", SortName: "Cure, The", Path: survivorPath, LibraryID: "lib-merge"}
	loser := &Artist{Name: "The Cure", SortName: "Cure, The", Path: loserPath, LibraryID: "lib-merge"}
	if err := svc.Create(ctx, survivor); err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	if err := svc.Create(ctx, loser); err != nil {
		t.Fatalf("Create loser: %v", err)
	}
	return svc, db, survivor.ID, loser.ID
}

// TestMergeAndReconcile_DryRunSkipsRenameAndRefresh: a dry-run returns straight
// from MergeArtists with no platform side effects.
func TestMergeAndReconcile_DryRunSkipsRenameAndRefresh(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	sync := &recordingSyncer{}
	ref := &recordingRefresher{}
	svc.SetPlatformRenameSyncer(sync)
	svc.SetPlatformMergeRefresher(ref)

	res, err := svc.MergeAndReconcile(context.Background(), MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, DryRun: true, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile dry-run: %v", err)
	}
	if len(sync.renamed) != 0 || len(ref.conns) != 0 {
		t.Errorf("dry-run triggered platform work: rename=%v refresh=%v", sync.renamed, ref.conns)
	}
	if res.CanonicalRename != nil {
		t.Errorf("dry-run set CanonicalRename = %+v, want nil", res.CanonicalRename)
	}
	if res.PlatformRefresh != nil {
		t.Errorf("dry-run set PlatformRefresh = %v, want nil", res.PlatformRefresh)
	}
}

// TestMergeAndReconcile_MergeErrorSkipsReconcile: a merge that halts on a
// collision returns the error with no rename/refresh attempted.
func TestMergeAndReconcile_MergeErrorSkipsReconcile(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	sync := &recordingSyncer{}
	ref := &recordingRefresher{}
	svc.SetPlatformRenameSyncer(sync)
	svc.SetPlatformMergeRefresher(ref)

	// Inject a colliding album so MergeArtists returns ErrMergeCollisions.
	loser := mustGetArtist(t, svc, context.Background(), loserID)
	if err := os.Mkdir(filepath.Join(loser.Path, "Disintegration"), 0o755); err != nil {
		t.Fatalf("mkdir collision: %v", err)
	}

	_, err := svc.MergeAndReconcile(context.Background(), MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if !errors.Is(err, ErrMergeCollisions) {
		t.Fatalf("err = %v, want ErrMergeCollisions", err)
	}
	if len(sync.renamed) != 0 || len(ref.conns) != 0 {
		t.Errorf("failed merge triggered reconcile: rename=%v refresh=%v", sync.renamed, ref.conns)
	}
}

// TestMergeAndReconcile_CanonicalSurvivorSkipsRenameStillRefreshes: a survivor
// already at its canonical basename gets no rename, but the platform refresh
// (wire of Finding #1) still fans out over the affected connections.
func TestMergeAndReconcile_CanonicalSurvivorSkipsRenameStillRefreshes(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t) // survivor dir "The Cure" is prefix-canonical
	ctx := context.Background()
	sync := &recordingSyncer{}
	ref := &recordingRefresher{}
	svc.SetPlatformRenameSyncer(sync)
	svc.SetPlatformMergeRefresher(ref)

	seedEmbyConn(t, db)
	if err := svc.SetPlatformID(ctx, survivorID, "conn-emby", "emby-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile: %v", err)
	}
	if res.CanonicalRename != nil {
		t.Errorf("CanonicalRename = %+v, want nil (survivor already canonical)", res.CanonicalRename)
	}
	if len(sync.renamed) != 0 {
		t.Errorf("SyncRename called %v, want none (no rename)", sync.renamed)
	}
	if ref.survivorID != survivorID {
		t.Errorf("refresh survivorID = %q, want %q", ref.survivorID, survivorID)
	}
	if !equalStringSets(ref.conns, []string{"conn-emby"}) {
		t.Errorf("refresh connections = %v, want [conn-emby]", ref.conns)
	}
	if len(res.PlatformRefresh) != 1 || res.PlatformRefresh[0].ConnectionID != "conn-emby" {
		t.Errorf("PlatformRefresh = %+v, want one conn-emby entry", res.PlatformRefresh)
	}
}

// TestMergeAndReconcile_RenamesNonCanonicalSurvivorAndRefreshes: the survivor
// directory is relocated to its canonical basename (Finding #3), the rename
// propagates to platforms, and the affected connections are then refreshed.
func TestMergeAndReconcile_RenamesNonCanonicalSurvivorAndRefreshes(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := nonCanonicalMergeSetup(t)
	ctx := context.Background()
	sync := &recordingSyncer{}
	ref := &recordingRefresher{}
	svc.SetPlatformRenameSyncer(sync)
	svc.SetPlatformMergeRefresher(ref)

	seedEmbyConn(t, db)
	if err := svc.SetPlatformID(ctx, survivorID, "conn-emby", "emby-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile: %v", err)
	}
	if res.CanonicalRename == nil {
		t.Fatalf("expected a canonical rename, got none (survivor path %s)", res.SurvivorPath)
	}
	wantBase := CanonicalDirName("The Cure", "prefix") // "The Cure"
	if got := filepath.Base(res.SurvivorPath); got != wantBase {
		t.Errorf("survivor path base = %s, want %s", got, wantBase)
	}
	if filepath.Base(res.CanonicalRename.OldPath) != "Cure, The" {
		t.Errorf("CanonicalRename.OldPath base = %s, want \"Cure, The\"", filepath.Base(res.CanonicalRename.OldPath))
	}
	if len(sync.renamed) == 0 {
		t.Errorf("SyncRename was not called on canonical rename")
	}
	if len(res.CanonicalRename.Platforms) != 1 || res.CanonicalRename.Platforms[0].Result != PlatformRemapOK {
		t.Errorf("CanonicalRename.Platforms = %+v, want one OK entry", res.CanonicalRename.Platforms)
	}
	if !equalStringSets(ref.conns, []string{"conn-emby"}) {
		t.Errorf("refresh connections = %v, want [conn-emby]", ref.conns)
	}
	// On disk: survivor now at canonical basename; old dir gone.
	newDir := filepath.Join(filepath.Dir(res.CanonicalRename.OldPath), wantBase)
	if _, statErr := os.Stat(newDir); statErr != nil {
		t.Errorf("expected canonical dir %s on disk: %v", newDir, statErr)
	}
	if _, statErr := os.Stat(res.CanonicalRename.OldPath); !os.IsNotExist(statErr) {
		t.Errorf("expected old dir removed, stat err = %v", statErr)
	}
}

// TestMergeAndReconcile_NilRefresherRecordsManualWarning: with no refresher
// wired, an affected-connection set records the manual-refresh reminder instead
// of fanning out.
func TestMergeAndReconcile_NilRefresherRecordsManualWarning(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	// No SetPlatformMergeRefresher call: mergeRefresher stays nil.

	seedEmbyConn(t, db)
	if err := svc.SetPlatformID(ctx, survivorID, "conn-emby", "emby-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile: %v", err)
	}
	if res.PlatformRefresh != nil {
		t.Errorf("PlatformRefresh = %v, want nil (no refresher wired)", res.PlatformRefresh)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Connected platforms") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected manual-refresh warning, got %v", res.Warnings)
	}
}

// TestMergeAndReconcile_RefreshFailureRecordsWarning: a per-connection refresh
// failure surfaces both in PlatformRefresh and as a warning, without failing
// the merge.
func TestMergeAndReconcile_RefreshFailureRecordsWarning(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	sync := &recordingSyncer{}
	ref := &recordingRefresher{failConns: map[string]string{"conn-emby": "peer 500"}}
	svc.SetPlatformRenameSyncer(sync)
	svc.SetPlatformMergeRefresher(ref)

	seedEmbyConn(t, db)
	if err := svc.SetPlatformID(ctx, survivorID, "conn-emby", "emby-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile: %v", err)
	}
	if len(res.PlatformRefresh) != 1 || res.PlatformRefresh[0].Result != PlatformRemapFailed {
		t.Errorf("PlatformRefresh = %+v, want one failed entry", res.PlatformRefresh)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "platform refresh failed for connection conn-emby") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected per-connection refresh-failure warning, got %v", res.Warnings)
	}
}

// TestMergeAndReconcile_RefreshStartFailureRecordsWarning: when the fan-out
// itself errors (not a per-connection failure), the outer error is recorded as
// a warning and PlatformRefresh stays empty.
func TestMergeAndReconcile_RefreshStartFailureRecordsWarning(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	ref := &recordingRefresher{err: errors.New("refresher down")}
	svc.SetPlatformMergeRefresher(ref)

	seedEmbyConn(t, db)
	if err := svc.SetPlatformID(ctx, survivorID, "conn-emby", "emby-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile: %v", err)
	}
	if len(res.PlatformRefresh) != 0 {
		t.Errorf("PlatformRefresh = %+v, want empty when the fan-out could not start", res.PlatformRefresh)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "post-merge platform refresh could not start") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected refresh-start-failure warning, got %v", res.Warnings)
	}
}
