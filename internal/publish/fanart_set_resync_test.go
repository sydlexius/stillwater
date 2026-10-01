package publish

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/collision"
	"github.com/sydlexius/stillwater/internal/connection"
	img "github.com/sydlexius/stillwater/internal/image"
)

// resyncPeer is fakeEmbyPeer (a peer that models its backdrop STATE, see its
// doc comment) plus the delete half of fanartResyncClient. DeleteImageAtIndex
// removes the slot and re-indexes the rest, which is what a real peer does and
// why the resync must delete high-index-first.
type resyncPeer struct {
	*fakeEmbyPeer
	deletes   []int
	failDelAt map[int]bool // delete indices that error WITHOUT removing
	delTries  int
}

func (r *resyncPeer) DeleteImageAtIndex(_ context.Context, _, _ string, idx int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delTries++
	if r.failDelAt[idx] {
		return errors.New("500 delete failed")
	}
	if idx < 0 || idx >= len(r.data) {
		return errors.New("404 no such backdrop")
	}
	r.deletes = append(r.deletes, idx)
	r.data = append(r.data[:idx], r.data[idx+1:]...)
	return nil
}

func (r *resyncPeer) snapshotData() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]byte, len(r.data))
	copy(out, r.data)
	return out
}

// setResyncHarness wires a Publisher whose single connection of connType is
// served by peer, and writes 3 distinct local fanart files. It returns the
// publisher, the artist, and the local files' bytes in index order.
func setResyncHarness(t *testing.T, connType string, peer *resyncPeer) (*Publisher, *artist.Artist, [][]byte) {
	t.Helper()
	dir := t.TempDir()
	names := []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg", "fanart4.jpg"}
	var local [][]byte
	for i := 0; i < 3; i++ {
		b := bandJPEG(t, 100+i)
		writeFile(t, filepath.Join(dir, names[i]), b)
		local = append(local, b)
	}

	origIdx, origResync := newIndexedImageUploader, newFanartResyncClient
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader { return peer }
	newFanartResyncClient = func(_ *connection.Connection, _ *slog.Logger) fanartResyncClient { return peer }
	t.Cleanup(func() { newIndexedImageUploader, newFanartResyncClient = origIdx, origResync })

	conn := &connection.Connection{ID: "c1", Name: "peer", Type: connType, URL: "http://127.0.0.1:1", Enabled: true, Status: "ok"}
	p := New(Deps{
		Logger:            silentLogger(),
		ArtistService:     &fakePlatformLister{ids: []artist.PlatformID{{ArtistID: "a1", ConnectionID: "c1", PlatformArtistID: "p1"}}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{"c1": conn}},
	})
	return p, &artist.Artist{ID: "a1", Name: "Test Artist", Path: dir}, local
}

func seedPeer(peer *resyncPeer, n int) {
	peer.data = nil
	for i := 0; i < n; i++ {
		peer.data = append(peer.data, []byte{0xEE, byte(i)}) // stale bytes, never equal to a local file
	}
}

// #3145: three consecutive full-set syncs against a peer whose indexed upload
// APPENDS (Jellyfin, measured 0 -> 3 -> 6 -> 9) must converge to the local
// file count, in local order, with stale platform backdrops cleared. Fails on
// the pre-fix code, where each run appends the local set again.
func TestSyncAllFanart_JellyfinRepeatedRunsConverge(t *testing.T) {
	peer := &resyncPeer{fakeEmbyPeer: &fakeEmbyPeer{appendAll: true}}
	seedPeer(peer, 2)
	p, a, local := setResyncHarness(t, connection.TypeJellyfin, peer)

	// PRECONDITIONS, so nothing passes vacuously: the peer starts with the stale
	// set, and the model really does append on an indexed upload (the premise
	// of #3145). Probe on a scratch copy so the real run starts from the seed.
	if peer.count() != 2 {
		t.Fatalf("precondition: peer holds %d backdrops, want 2 stale", peer.count())
	}
	probe := &fakeEmbyPeer{appendAll: true, data: [][]byte{{1}}}
	if err := probe.UploadImageAtIndex(context.Background(), "", "fanart", 0, []byte{2}, "image/jpeg"); err != nil || probe.count() != 2 {
		t.Fatalf("precondition: the peer model must append on an indexed upload (count=%d, err=%v)", probe.count(), err)
	}

	for run := 1; run <= 3; run++ {
		if w := p.SyncAllFanartToPlatforms(context.Background(), a); len(w) != 0 {
			t.Fatalf("run %d: unexpected warnings %v", run, w)
		}
		got := peer.snapshotData()
		if len(got) != len(local) {
			t.Fatalf("run %d: peer holds %d backdrops, want %d (inflation: #3145)", run, len(got), len(local))
		}
		for i := range local {
			if !bytes.Equal(got[i], local[i]) {
				t.Errorf("run %d: peer slot %d does not hold local fanart %d's bytes", run, i, i)
			}
		}
	}
	// High-index-first within each run: 2 stale (1,0), then 3 per later run (2,1,0).
	want := []int{1, 0, 2, 1, 0, 2, 1, 0}
	if len(peer.deletes) != len(want) {
		t.Fatalf("deletes = %v, want %v", peer.deletes, want)
	}
	for i := range want {
		if peer.deletes[i] != want[i] {
			t.Fatalf("deletes = %v, want %v", peer.deletes, want)
		}
	}
}

// Emby is deliberately UNCHANGED (#3145): its indexed upload replaces in
// place, so the per-file indexed sync stays flat across runs and never
// deletes, and the resync client is never even constructed.
func TestSyncAllFanart_EmbyKeepsPerFileIndexedUpload(t *testing.T) {
	peer := &resyncPeer{fakeEmbyPeer: &fakeEmbyPeer{}}
	seedPeer(peer, 3)
	p, a, local := setResyncHarness(t, connection.TypeEmby, peer)
	newFanartResyncClient = func(_ *connection.Connection, _ *slog.Logger) fanartResyncClient {
		t.Error("Emby must never construct the clear-then-reupload client")
		return peer
	}

	for run := 1; run <= 3; run++ {
		if w := p.SyncAllFanartToPlatforms(context.Background(), a); len(w) != 0 {
			t.Fatalf("run %d: unexpected warnings %v", run, w)
		}
		if peer.count() != 3 {
			t.Fatalf("run %d: peer holds %d backdrops, want a flat 3", run, peer.count())
		}
	}
	if len(peer.deletes) != 0 {
		t.Errorf("Emby issued deletes %v, want none", peer.deletes)
	}
	if peer.ups != 9 {
		t.Errorf("indexed uploads = %d, want 9 (3 files x 3 runs)", peer.ups)
	}
	for i, got := range peer.snapshotData() {
		if !bytes.Equal(got, local[i]) {
			t.Errorf("Emby slot %d does not hold local fanart %d's bytes", i, i)
		}
	}
}

// A failure mid-resync is reported, never success: the delete loop has already
// cleared the platform, so the operator must be told the set is incomplete.
func TestSyncAllFanart_JellyfinMidResyncUploadFailureIsReported(t *testing.T) {
	peer := &resyncPeer{fakeEmbyPeer: &fakeEmbyPeer{appendAll: true, dropCalls: map[int]bool{1: true}}}
	seedPeer(peer, 2)
	p, a, _ := setResyncHarness(t, connection.TypeJellyfin, peer)

	warnings := p.SyncAllFanartToPlatforms(context.Background(), a)

	if peer.count() != 2 {
		t.Fatalf("precondition: peer holds %d backdrops, want 2 (3 uploads, the second dropped)", peer.count())
	}
	if len(peer.deletes) != 2 {
		t.Fatalf("precondition: the stale set must have been cleared first, deletes = %v", peer.deletes)
	}
	joined := strings.Join(warnings, "|")
	if !strings.Contains(joined, "failed to upload backdrop 1") {
		t.Errorf("warnings = %v, want one naming the failed upload of backdrop 1 (a partial resync must not read as success)", warnings)
	}
}

// Restorability (#3145 d): a local file that cannot be read must stop the
// resync BEFORE anything on the platform is deleted. The snapshot is taken
// before the first platform write, so the stale-but-complete set survives.
func TestSyncAllFanart_JellyfinUnreadableLocalFileDeletesNothing(t *testing.T) {
	peer := &resyncPeer{fakeEmbyPeer: &fakeEmbyPeer{appendAll: true}}
	seedPeer(peer, 2)
	p, a, _ := setResyncHarness(t, connection.TypeJellyfin, peer)
	victim := filepath.Join(a.Path, "fanart2.jpg")
	if err := os.Chmod(victim, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(victim, 0o600) })
	if f, err := os.Open(victim); err == nil {
		_ = f.Close()
		t.Skip("file mode is not enforced here (running as root); cannot make a local read fail")
	}

	warnings := p.SyncAllFanartToPlatforms(context.Background(), a)

	if len(peer.deletes) != 0 || peer.ups != 0 || peer.count() != 2 {
		t.Errorf("platform touched despite an uncapturable slot: deletes=%v uploads=%d count=%d, want untouched (2)", peer.deletes, peer.ups, peer.count())
	}
	if !strings.Contains(strings.Join(warnings, "|"), "could not be captured") {
		t.Errorf("warnings = %v, want the refusal reason", warnings)
	}
}

// #2540: the cross-artist collision notification must still fire on a
// Jellyfin-only full-set push, which no longer goes through uploadFanartSet
// (where the notification used to be raised). Also asserts the push still
// happens: notify-only never blocks.
func TestSyncAllFanart_JellyfinCollisionNotifiesAndStillUploads(t *testing.T) {
	peer := &resyncPeer{fakeEmbyPeer: &fakeEmbyPeer{appendAll: true}}
	p, a, _ := setResyncHarness(t, connection.TypeJellyfin, peer)
	// The fixture's own hash is registered under ANOTHER artist, so the push
	// matches a foreign artist's fanart. seedDecodableJPG asserts a non-zero
	// hash, so a fail-open comparison cannot pass this vacuously.
	ownHash := seedDecodableJPG(t, a.Path, "fanart.jpg")
	pub := &recordingPublisher{}
	notifier := collision.NewNotifier(pub,
		func(context.Context, string, string, string, string) error { return nil },
		nil, nil, silentLogger())
	p.SetCollisionNotifier(notifier, &staticIdentityIndex{entries: []img.FanartIdentityEntry{{ArtistID: "other-artist", PHash: ownHash}}})

	p.SyncAllFanartToPlatforms(context.Background(), a)

	if len(pub.events) == 0 {
		t.Error("no collision notification raised on a Jellyfin-only push (#2540 regression)")
	}
	if peer.count() != 3 {
		t.Errorf("peer holds %d backdrops, want 3: the notification must never block the push", peer.count())
	}
}

// #3177: the extrafanart/ advisory is gated on a platform write having been
// attempted. A resync the restorability gate refuses writes nothing, so it
// must not warn; a real push still does.
func TestSyncAllFanart_JellyfinExtrafanartAdvisoryOnlyWhenPushed(t *testing.T) {
	t.Run("refused resync is silent", func(t *testing.T) {
		peer := &resyncPeer{fakeEmbyPeer: &fakeEmbyPeer{appendAll: true}}
		seedPeer(peer, 2)
		p, a, _ := setResyncHarness(t, connection.TypeJellyfin, peer)
		seedExtrafanart(t, a.Path, 2)
		victim := filepath.Join(a.Path, "fanart2.jpg")
		if err := os.Chmod(victim, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(victim, 0o600) })
		if f, err := os.Open(victim); err == nil {
			_ = f.Close()
			t.Skip("file mode is not enforced here (running as root)")
		}

		warnings := p.SyncAllFanartToPlatforms(context.Background(), a)

		if peer.ups != 0 || len(peer.deletes) != 0 {
			t.Fatalf("precondition: the resync must be refused, got deletes=%v uploads=%d", peer.deletes, peer.ups)
		}
		if w := findExtrafanartWarning(warnings); w != "" {
			t.Errorf("advisory raised though nothing was pushed: %q", w)
		}
	})
	t.Run("real push still warns", func(t *testing.T) {
		peer := &resyncPeer{fakeEmbyPeer: &fakeEmbyPeer{appendAll: true}}
		p, a, _ := setResyncHarness(t, connection.TypeJellyfin, peer)
		seedExtrafanart(t, a.Path, 2)

		warnings := p.SyncAllFanartToPlatforms(context.Background(), a)

		if peer.count() != 3 {
			t.Fatalf("precondition: the push must land, peer holds %d, want 3", peer.count())
		}
		if findExtrafanartWarning(warnings) == "" {
			t.Errorf("no advisory on a real push; warnings = %v", warnings)
		}
	})
}

// A failed delete on the full-set path stops before any upload (#3145 review):
// the stale backdrop would otherwise sit ahead of the re-uploaded set. Peer
// holds 3; the first (index 2) delete fails, so nothing is cleared and nothing
// uploaded, the warning says so, and the connection still counts as pushed
// (attempted) so the advisory fires.
func TestSyncAllFanart_JellyfinDeleteFailureStopsBeforeUploads(t *testing.T) {
	peer := &resyncPeer{fakeEmbyPeer: &fakeEmbyPeer{appendAll: true}, failDelAt: map[int]bool{2: true}}
	seedPeer(peer, 3)
	p, a, _ := setResyncHarness(t, connection.TypeJellyfin, peer)
	seedExtrafanart(t, a.Path, 2)

	warnings := p.SyncAllFanartToPlatforms(context.Background(), a)

	if peer.delTries != 1 {
		t.Fatalf("delete attempts = %d, want exactly 1 (stop at the first failure)", peer.delTries)
	}
	if peer.ups != 0 || peer.count() != 3 {
		t.Errorf("uploads=%d count=%d, want 0 uploads and the original 3 left in order", peer.ups, peer.count())
	}
	if !strings.Contains(strings.Join(warnings, "|"), "could not clear backdrop 2, upload skipped") {
		t.Errorf("warnings = %v, want the upload-skipped clear failure", warnings)
	}
}

// lockHooks installs the pushFanartSetToPeer lock observation seams for one
// test: waiting closes once a push is about to take the target lock (so the
// local snapshot was already captured), held closes once it holds it.
func lockHooks(t *testing.T) (waiting, held chan struct{}) {
	t.Helper()
	waiting, held = make(chan struct{}), make(chan struct{})
	var w, h sync.Once
	fanartLockWaitHook = func() { w.Do(func() { close(waiting) }) }
	fanartLockHeldHook = func() { h.Do(func() { close(held) }) }
	t.Cleanup(func() { fanartLockWaitHook, fanartLockHeldHook = nil, nil })
	return waiting, held
}

func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// The per-target lock in pushFanartSetToPeer serializes the destructive resync
// against a concurrent single-image sync. Deterministic: the waiting hook
// proves the push reached the lock point; the held hook is the positive
// observation of acquisition, and it must NOT fire while another operation
// holds the lock. With the lock removed it fires at once.
func TestSyncAllFanart_JellyfinResyncWaitsForTargetLock(t *testing.T) {
	peer := &resyncPeer{fakeEmbyPeer: &fakeEmbyPeer{appendAll: true}}
	seedPeer(peer, 2)
	p, a, local := setResyncHarness(t, connection.TypeJellyfin, peer)
	waiting, held := lockHooks(t)

	unlock := p.lockPhashTarget("c1", "p1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.SyncAllFanartToPlatforms(context.Background(), a)
	}()
	awaitClosed(t, waiting, "the push to reach the target lock")

	select {
	case <-held:
		unlock()
		<-done
		t.Fatal("the push acquired the target lock while another operation held it")
	case <-time.After(300 * time.Millisecond):
	}
	if peer.ups != 0 || len(peer.deletes) != 0 {
		t.Fatalf("peer touched while the lock was held: deletes=%v uploads=%d", peer.deletes, peer.ups)
	}
	unlock()
	awaitClosed(t, held, "the push to acquire the lock after release")
	awaitClosed(t, done, "the sync to finish")

	got := peer.snapshotData()
	if len(got) != len(local) {
		t.Fatalf("peer holds %d backdrops after release, want %d", len(got), len(local))
	}
}

// #3145 review: the local set must be re-read UNDER the lock. A full-set sync
// snapshots set A, then blocks on the lock while another operation publishes
// set B; once it wins the lock it must push B, never the stale A.
func TestSyncAllFanart_JellyfinResyncPushesSetCurrentAtLockTime(t *testing.T) {
	peer := &resyncPeer{fakeEmbyPeer: &fakeEmbyPeer{appendAll: true}}
	seedPeer(peer, 2)
	p, a, oldSet := setResyncHarness(t, connection.TypeJellyfin, peer)
	waiting, _ := lockHooks(t)

	unlock := p.lockPhashTarget("c1", "p1")
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.SyncAllFanartToPlatforms(context.Background(), a)
	}()
	awaitClosed(t, waiting, "the push to snapshot and reach the lock")

	// The other operation publishes set B (distinct bytes, one file fewer).
	newB := bandJPEG(t, 900)
	writeFile(t, filepath.Join(a.Path, "fanart.jpg"), newB)
	for _, name := range []string{"fanart2.jpg", "fanart3.jpg"} {
		if err := os.Remove(filepath.Join(a.Path, name)); err != nil {
			t.Fatal(err)
		}
	}
	unlock()
	awaitClosed(t, done, "the sync to finish")

	got := peer.snapshotData()
	if len(got) != 1 || !bytes.Equal(got[0], newB) {
		t.Fatalf("peer holds %d backdrops; want exactly the set current at lock time (1 file). stale set A had %d", len(got), len(oldSet))
	}
}
