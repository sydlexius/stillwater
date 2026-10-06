//go:build integration

package publish

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
)

// Live proof for #3200 (#3406): past an unreadable local slot, a peer that
// cannot be read must not receive the later survivor. Drives uploadFanartSet
// directly, with the REAL Emby client for every write and for the peer state;
// the only injected fault is the read failure.

// unreadablePeerReader fails every peer read.
type unreadablePeerReader struct{}

func (unreadablePeerReader) GetArtistDetail(context.Context, string) (*connection.ArtistPlatformState, error) {
	return nil, errors.New("injected peer read failure")
}

func (unreadablePeerReader) GetArtistBackdrop(context.Context, string, int) ([]byte, string, error) {
	return nil, "", errors.New("injected peer read failure")
}

// indexRecordingUploader records the index of every indexed upload it forwards.
type indexRecordingUploader struct {
	connection.IndexedImageUploader
	indices []int
}

func (r *indexRecordingUploader) UploadImageAtIndex(ctx context.Context, id, typ string, idx int, data []byte, ct string) error {
	r.indices = append(r.indices, idx)
	return r.IndexedImageUploader.UploadImageAtIndex(ctx, id, typ, idx, data, ct)
}

// Duplicated on purpose: each live test file is copied ALONE into an old-ref
// extract, so it cannot share a helper with the other file.
// resetNilSlotPeerItem empties the item and CONVERGES: Emby 4.10.1.0 answers a delete (and a
// non-indexed upload) with a 500 SQLiteException yet applies it, so a delete
// error is tolerated and only the polled state counts. It re-deletes whatever
// remains until the count reads zero (bounded 30s) and fails loudly otherwise.
// fatal is false in t.Cleanup, where Fatal would abort the teardown.
func resetNilSlotPeerItem(ctx context.Context, t *testing.T, itemID string, client *emby.Client, fatal bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for ctx.Err() == nil {
		state, err := client.GetArtistDetail(ctx, itemID)
		if err == nil && state.BackdropCount == 0 {
			return
		}
		if err == nil {
			for i := state.BackdropCount - 1; i >= 0; i-- {
				_ = client.DeleteImageAtIndex(ctx, itemID, "fanart", i)
			}
		}
		if time.Now().After(deadline) {
			msg := fmt.Sprintf("LIVE-PROOF-SETUP: item %s did not reach 0 backdrops within 30s (last read err=%v)", itemID, err)
			if fatal {
				t.Fatal(msg)
			}
			t.Error(msg)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if fatal {
		t.Fatalf("LIVE-PROOF-SETUP: context ended before item %s reached 0 backdrops: %v", itemID, ctx.Err())
	}
	t.Errorf("LIVE-PROOF-SETUP: context ended before item %s reached 0 backdrops: %v", itemID, ctx.Err())
}

func TestLiveEmby_NilSlotUnreadablePeer_LaterSurvivorWaits(t *testing.T) {
	env := loadLiveEmbyEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*liveBackdropTestTimeout)
	defer cancel()
	logger := silentLogger()
	client := emby.New(env.url, env.apiKey, env.userID, logger)
	resetNilSlotPeerItem(ctx, t, env.itemID, client, true)
	t.Cleanup(func() {
		cctx, cc := context.WithTimeout(context.Background(), liveBackdropCleanupTimeout)
		defer cc()
		resetNilSlotPeerItem(cctx, t, env.itemID, client, false)
	})

	imgs := [][]byte{bandJPEG(t, 0xB1), bandJPEG(t, 0xB2), nil, bandJPEG(t, 0xB4)}
	snapshot := make([]fanartSnapshot, len(imgs))
	for i, d := range imgs {
		snapshot[i] = fanartSnapshot{path: "fanart.jpg", index: i, data: d}
	}
	p := New(Deps{Logger: logger})
	run := func(reader connection.BackdropReader) ([]string, []int) {
		rec := &indexRecordingUploader{IndexedImageUploader: client}
		w := p.uploadFanartSet(ctx, fanartUpload{
			artist:   &artist.Artist{ID: "live-nil-slot-peer", Name: "Live UAT Artist"},
			conn:     &connection.Connection{ID: "c-emby", Name: "live-emby-uat", Type: connection.TypeEmby},
			pid:      artist.PlatformID{ConnectionID: "c-emby", PlatformArtistID: env.itemID},
			uploader: rec, snapshot: snapshot, notified: map[string]bool{}, reader: reader,
		})
		return w, rec.indices
	}
	slot2 := func() string {
		data, _, err := client.GetArtistBackdrop(ctx, env.itemID, 2)
		if err != nil {
			t.Fatalf("reading peer slot 2: %v", err)
		}
		return hashOf(data)
	}

	// Run 1, readable peer: the compacted set [A,B,D] lands.
	if w, _ := run(client); len(w) != 0 {
		t.Logf("run 1 warnings (informational): %v", w)
	}
	n := pollBackdropDetail(ctx, t, client, env.itemID, 3).BackdropCount
	t.Logf("LIVE-PROOF-COUNT scenario=unreadable-peer run=1 count=%d", n)
	if n != 3 {
		t.Fatalf("LIVE-PROOF-SETUP: precondition failed: seeded peer count = %d, want 3", n)
	}

	// Run 2, unreadable peer: D (index 3) must NOT be uploaded and must be
	// reported as not synced. A and B (before the nil slot) may be re-sent in
	// place; they are not what this proves.
	w, idxs := run(unreadablePeerReader{})
	n = pollBackdropDetail(ctx, t, client, env.itemID, 3).BackdropCount
	t.Logf("LIVE-PROOF-COUNT scenario=unreadable-peer run=2 count=%d uploads=%v", n, idxs)
	for _, i := range idxs {
		if i >= 2 {
			t.Errorf("LIVE-PROOF-DEFECT: survivor uploaded at index %d past the nil slot while the peer was unreadable", i)
		}
	}
	if n != 3 {
		t.Errorf("LIVE-PROOF-DEFECT: BackdropCount = %d, want 3 (a duplicate was appended)", n)
	}
	if !strings.Contains(strings.Join(w, "; "), "not synced") {
		t.Errorf("LIVE-PROOF-DEFECT: warnings = %v, want one containing %q", w, "not synced")
	}

	// Run 3, readable peer again: nothing owed, peer unchanged.
	// "upload failed" is informational: Emby 4.10.1.0 reports a spurious 500 for
	// a write it applied. Only the "not synced" warning is this scenario's claim.
	w3, _ := run(client)
	t.Logf("run 3 warnings (informational): %v", w3)
	if strings.Contains(strings.Join(w3, "; "), "not synced") {
		t.Errorf("LIVE-PROOF-DEFECT: run 3 still reports %q with a readable peer: %v", "not synced", w3)
	}
	n = pollBackdropDetail(ctx, t, client, env.itemID, 3).BackdropCount
	t.Logf("LIVE-PROOF-COUNT scenario=unreadable-peer run=3 count=%d", n)
	if n != 3 || slot2() != hashOf(imgs[3]) {
		t.Errorf("LIVE-PROOF-DEFECT: after the recovery run count=%d (want 3) or slot 2 is not the last survivor", n)
	}
}
