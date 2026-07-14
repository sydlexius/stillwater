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
// syncCall is one recorded SyncRename invocation. oldPath is captured because
// the #2380 survivor reconcile calls SyncRename with oldPath == newPath (it is
// re-issuing an unchanged path to run the peer relink), and a test that ignored
// oldPath could not tell that apart from a real rename.
type syncCall struct {
	artistID string
	oldPath  string
	newPath  string
}

// recordingSyncer implements PlatformRenameSyncer. events, when non-nil, is a
// shared log both this and recordingRefresher append to, so a test can assert
// the ORDER of the two reconcile steps and not merely that both happened.
type recordingSyncer struct {
	renamed []string
	calls   []syncCall
	err     error
	results []PlatformRemapResult
	events  *[]string
}

func (r *recordingSyncer) SyncRename(_ context.Context, artistID, oldPath, newPath string) ([]PlatformRemapResult, error) {
	r.renamed = append(r.renamed, artistID+"->"+newPath)
	r.calls = append(r.calls, syncCall{artistID: artistID, oldPath: oldPath, newPath: newPath})
	if r.events != nil {
		*r.events = append(*r.events, "sync")
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.results != nil {
		return r.results, nil
	}
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
	events     *[]string         // shared with recordingSyncer; see its doc comment
}

func (r *recordingRefresher) SyncMergeRefresh(_ context.Context, survivorID string, connectionIDs []string) ([]PlatformRefreshResult, error) {
	r.calls++
	r.survivorID = survivorID
	r.conns = append(r.conns, connectionIDs...)
	if r.events != nil {
		*r.events = append(*r.events, "refresh")
	}
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

// TestMergeAndReconcile_CanonicalSurvivorStillPushesSurvivorPath: a survivor
// already at its canonical basename gets no rename -- and MUST STILL have its
// path re-issued to the peers (#2380).
//
// This test previously asserted the OPPOSITE ("SyncRename called %v, want none
// (no rename)") and so enshrined the bug: it would have failed the fix and
// certified the data loss as intended behavior. The old assertion was reasonable
// on its face -- no rename happened, so why push a path? -- which is exactly why
// it survived review.
//
// The push matters because SyncRename is the ONLY chokepoint that performs the
// peer relink. In the common dedupe (merge a duplicate INTO the correctly-named
// survivor) the basename does not change, so the old code pushed nothing and the
// peers kept pointing at the loser's now-deleted directory. Emby's and
// Jellyfin's NFO savers then recreate that directory, the scanner re-imports it
// as a duplicate row, and the merge is undone.
//
// Note oldPath == newPath == the survivor's path: nothing moved. The point is
// not the path, it is running the relink.
func TestMergeAndReconcile_CanonicalSurvivorStillPushesSurvivorPath(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t) // survivor dir "The Cure" is prefix-canonical
	ctx := context.Background()
	events := []string{}
	sync := &recordingSyncer{events: &events}
	ref := &recordingRefresher{events: &events}
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

	// Precondition: this test is only meaningful if NO rename happened. If the
	// survivor were renamed, the push would come from reconcileSurvivorCanonicalPath
	// and prove nothing about the gap being closed here.
	if res.CanonicalRename != nil {
		t.Fatalf("CanonicalRename = %+v, want nil (survivor already canonical); "+
			"this test cannot exercise the no-rename gap", res.CanonicalRename)
	}

	// The fix: the survivor's path is re-issued even though nothing moved.
	if len(sync.calls) != 1 {
		t.Fatalf("SyncRename called %d times, want exactly 1: a merge that does not rename "+
			"the survivor must STILL push its path, or the peers keep pointing at the "+
			"merged-away directory and recreate it (#2380). calls=%+v", len(sync.calls), sync.calls)
	}
	got := sync.calls[0]
	if got.artistID != survivorID {
		t.Errorf("SyncRename artistID = %q, want the survivor %q", got.artistID, survivorID)
	}
	if got.oldPath != res.SurvivorPath || got.newPath != res.SurvivorPath {
		t.Errorf("SyncRename paths = (%q -> %q), want both = the survivor's unchanged path %q",
			got.oldPath, got.newPath, res.SurvivorPath)
	}

	// The per-connection outcome is reported, not swallowed.
	if len(res.SurvivorPathSync) != 1 || res.SurvivorPathSync[0].Result != PlatformRemapOK {
		t.Errorf("SurvivorPathSync = %+v, want one ok entry", res.SurvivorPathSync)
	}

	// Ordering (maintainer's call): the survivor push runs AFTER the refresh, so
	// the library scan the refresh triggers gives the relink's peer-resolution
	// poll a head start rather than making it wait out its full budget.
	if len(events) != 2 || events[0] != "refresh" || events[1] != "sync" {
		t.Errorf("reconcile order = %v, want [refresh sync]: the survivor push must run "+
			"after the platform refresh", events)
	}

	// The pre-existing refresh behavior is unchanged.
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

// TestMergeAndReconcile_RenamedSurvivorPushesPathOnce guards the other side of
// the branch: when a rename DID happen, reconcileSurvivorCanonicalPath already
// pushed the path and ran the relink, so the new survivor reconcile must NOT
// push a second time. Without this, the fix would double-push on every
// non-canonical merge and pay the relink poll twice.
func TestMergeAndReconcile_RenamedSurvivorPushesPathOnce(t *testing.T) {
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

	// Precondition: a rename really happened, otherwise this asserts nothing.
	if res.CanonicalRename == nil {
		t.Fatalf("CanonicalRename = nil, want a rename; this test cannot exercise the "+
			"already-pushed branch. SurvivorPath=%q", res.SurvivorPath)
	}
	if len(sync.calls) != 1 {
		t.Errorf("SyncRename called %d times, want exactly 1 (the rename's own push); "+
			"the survivor reconcile must not push again. calls=%+v", len(sync.calls), sync.calls)
	}
	if res.SurvivorPathSync != nil {
		t.Errorf("SurvivorPathSync = %+v, want nil when a rename already pushed the path",
			res.SurvivorPathSync)
	}
}

// TestMergeAndReconcile_SurvivorPushRefusedIsWarnedNotSwallowed: a push refused
// by the pre-flight root guard (PlatformRemapFailed -- e.g. an unmapped
// split-mount connection) must surface as an operator-visible warning. It must
// NOT fail the merge (which has already committed) and must NOT be silently
// dropped: a refused push means a peer is still pointing at the merged-away
// directory and may recreate it.
func TestMergeAndReconcile_SurvivorPushRefusedIsWarnedNotSwallowed(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	sync := &recordingSyncer{results: []PlatformRemapResult{{
		ConnectionID: "conn-emby",
		Result:       PlatformRemapFailed,
		Error:        "/host/music/The Cure is outside that server's root folders",
	}}}
	svc.SetPlatformRenameSyncer(sync)
	svc.SetPlatformMergeRefresher(&recordingRefresher{})

	seedEmbyConn(t, db)
	if err := svc.SetPlatformID(ctx, survivorID, "conn-emby", "emby-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile returned an error; a refused push must not fail the "+
			"already-committed merge: %v", err)
	}
	if len(sync.calls) != 1 {
		t.Fatalf("SyncRename called %d times, want 1", len(sync.calls))
	}
	if len(res.SurvivorPathSync) != 1 || res.SurvivorPathSync[0].Result != PlatformRemapFailed {
		t.Errorf("SurvivorPathSync = %+v, want the failed entry reported", res.SurvivorPathSync)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "conn-emby") && strings.Contains(w, "root folders") {
			found = true
		}
	}
	if !found {
		t.Errorf("Warnings = %v, want one naming conn-emby and the refusal reason; a refused "+
			"push must be operator-visible, not swallowed", res.Warnings)
	}
}

// TestMergeAndReconcile_SurvivorPushEnumerationErrorWarns: SyncRename can fail
// outright (it enumerates the connections, so a DB or transport failure returns
// an error rather than per-connection results). The merge has already committed,
// so this must not fail it -- but it must NOT pass silently either. An
// enumeration failure means we do not know whether ANY peer was relinked, which
// is precisely the state that lets a peer recreate the merged-away directory.
func TestMergeAndReconcile_SurvivorPushEnumerationErrorWarns(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	sync := &recordingSyncer{err: errors.New("connection enumeration failed")}
	svc.SetPlatformRenameSyncer(sync)
	svc.SetPlatformMergeRefresher(&recordingRefresher{})

	seedEmbyConn(t, db)
	if err := svc.SetPlatformID(ctx, survivorID, "conn-emby", "emby-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	res, err := svc.MergeAndReconcile(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeAndReconcile returned an error; the merge has already committed and a "+
			"failed peer push must not fail it: %v", err)
	}

	// Precondition: the push really was attempted and really did fail.
	if len(sync.calls) != 1 {
		t.Fatalf("SyncRename called %d times, want 1; the error path was never exercised", len(sync.calls))
	}
	if res.SurvivorPathSync != nil {
		t.Errorf("SurvivorPathSync = %+v, want nil when the push failed outright", res.SurvivorPathSync)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "enumeration failed") && strings.Contains(w, "re-scan") {
			found = true
		}
	}
	if !found {
		t.Errorf("Warnings = %v, want one carrying the failure and telling the operator to "+
			"re-scan; a failed push must not be silent", res.Warnings)
	}
}

// TestMergeAndReconcile_NoSyncerSkipsSurvivorPush: with platform syncing
// disabled the survivor push is a no-op and records nothing. It must not panic
// and must not invent a warning -- there are no peers to be wrong about.
func TestMergeAndReconcile_NoSyncerSkipsSurvivorPush(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()
	svc.SetPlatformMergeRefresher(&recordingRefresher{})
	// Deliberately no SetPlatformRenameSyncer.

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
	if res.SurvivorPathSync != nil {
		t.Errorf("SurvivorPathSync = %+v, want nil with no syncer wired", res.SurvivorPathSync)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "re-issue the survivor's path") {
			t.Errorf("warning %q recorded with no syncer wired; there are no peers to warn about", w)
		}
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
