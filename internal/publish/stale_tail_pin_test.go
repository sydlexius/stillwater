package publish

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/publish/publishtest"
)

// These tests pin what a full-set sync does TODAY when a local backdrop
// disappears (#3175). They drive the real Publisher and the real emby/jellyfin
// clients over HTTP against publishtest.Peer, which models the peer's list.
//
// One of them (the Emby last-slot case) pins a KNOWN GAP, not desired
// behavior. See its comment.

// holdsExactly asserts the peer's list equals want, comparing BYTES (content),
// not a returned count.
func holdsExactly(t *testing.T, step string, peer *publishtest.Peer, want [][]byte) {
	t.Helper()
	got, _ := peer.State()
	if len(got) != len(want) {
		t.Fatalf("%s: platform holds %d backdrops, want %d", step, len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("%s: platform slot %d does not hold the expected image", step, i)
		}
	}
}

// TestStaleTail_EmbyLastLocalFileRemoved_PlatformKeepsIt PINS A KNOWN GAP in
// #3175; it is NOT desired behavior. A later slice of #3175 will flip the final
// assertion (the removed image should leave the platform).
//
// Setup: five local files are pushed (Emby ends up with the same five). The
// LAST local file is then removed and the set is synced three times. Emby's
// full-set push only uploads: it replaces slots 0-3 with the same bytes and
// never deletes slot 4, whose content is no longer local. (A removed MIDDLE
// file would not leave a tail like this: the leftover would be byte-identical
// to a kept slot and the duplicate prune would remove it.)
func TestStaleTail_EmbyLastLocalFileRemoved_PlatformKeepsIt(t *testing.T) {
	ctx := context.Background()
	local := [][]byte{bandJPEG(t, 51), bandJPEG(t, 52), bandJPEG(t, 53), bandJPEG(t, 54), bandJPEG(t, 55)}
	peer := publishtest.NewPeer(connection.TypeEmby, nil)
	p, a := durabilityHarness(t, connection.TypeEmby, peer, local)

	p.SyncAllFanartToPlatforms(ctx, a)
	// PRECONDITION: the first push really put all five on the platform.
	holdsExactly(t, "after the initial push", peer, local)

	if err := os.Remove(filepath.Join(a.Path, "fanart5.jpg")); err != nil {
		t.Fatalf("removing the last local file: %v", err)
	}
	// PRECONDITION: the local set really shrank to four.
	if _, err := os.Stat(filepath.Join(a.Path, "fanart5.jpg")); !os.IsNotExist(err) {
		t.Fatalf("precondition: fanart5.jpg still exists (err %v)", err)
	}

	postsBefore := peer.RequestCount("POST")
	for i := 0; i < 3; i++ {
		p.SyncAllFanartToPlatforms(ctx, a)
	}

	// The syncs really reached the peer: one upload per remaining local file,
	// per sync. (Without this the pin would pass with zero syncs.)
	if got, want := peer.RequestCount("POST")-postsBefore, 3*(len(local)-1); got != want {
		t.Fatalf("precondition: the 3 syncs issued %d uploads, want %d", got, want)
	}
	// CURRENT BEHAVIOR (the gap): the removed image is still the last slot,
	// and no DELETE was even ATTEMPTED (the log counts failed and unindexed
	// ones too, unlike DeleteCount).
	holdsExactly(t, "after three syncs (current gap: the stale tail survives)", peer, local)
	if n := peer.RequestCount("DELETE"); n != 0 {
		t.Errorf("the Emby full-set push issued %d DELETE requests, want 0 today", n)
	}
}

// TestStaleTail_ZeroLocalFiles_PlatformSetKept: with no local backdrops the
// sync returns before reaching any peer, on both kinds, so the platform's set
// is left exactly as it was and no request is made.
func TestStaleTail_ZeroLocalFiles_PlatformSetKept(t *testing.T) {
	ctx := context.Background()
	seed := [][]byte{bandJPEG(t, 61), bandJPEG(t, 62), bandJPEG(t, 63)}
	for _, typ := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		t.Run(typ, func(t *testing.T) {
			peer := publishtest.NewPeer(typ, seed)
			p, a := durabilityHarness(t, typ, peer, nil)
			// PRECONDITION: the artist really has no local backdrops, and the
			// platform really holds three.
			if entries, err := os.ReadDir(a.Path); err != nil || len(entries) != 0 {
				t.Fatalf("precondition: local dir has %d entries (err %v), want 0", len(entries), err)
			}
			holdsExactly(t, "before the sync", peer, seed)

			syncs := 0
			for i := 0; i < 3; i++ {
				p.SyncAllFanartToPlatforms(ctx, a)
				syncs++
			}
			if syncs != 3 {
				t.Fatalf("precondition: ran %d syncs, want 3", syncs)
			}

			holdsExactly(t, "after three syncs", peer, seed)
			// No request of ANY kind reached the peer (the log covers every
			// request, matched or not).
			if reqs := peer.Requests(); len(reqs) != 0 {
				t.Errorf("a zero-local-file sync made %d requests, want 0: %+v", len(reqs), reqs)
			}

			// POSITIVE CONTROL: with one local file the same harness DOES
			// reach the peer, so the silence above is the early return and
			// not a wrongly wired harness.
			if err := os.WriteFile(filepath.Join(a.Path, "fanart.jpg"), bandJPEG(t, 64), 0o600); err != nil {
				t.Fatalf("writing the control file: %v", err)
			}
			p.SyncAllFanartToPlatforms(ctx, a)
			if len(peer.Requests()) == 0 {
				t.Fatal("control: a sync with one local file made no request; the harness is not wired to the peer")
			}
		})
	}
}

// TestStaleTail_JellyfinLastLocalFileRemoved_ConvergesToLocal pins the
// behavior that already works: Jellyfin's full-set push clears the list and
// re-uploads the local files, so the tail disappears.
func TestStaleTail_JellyfinLastLocalFileRemoved_ConvergesToLocal(t *testing.T) {
	ctx := context.Background()
	local := [][]byte{bandJPEG(t, 71), bandJPEG(t, 72), bandJPEG(t, 73), bandJPEG(t, 74), bandJPEG(t, 75)}
	peer := publishtest.NewPeer(connection.TypeJellyfin, nil)
	p, a := durabilityHarness(t, connection.TypeJellyfin, peer, local)

	p.SyncAllFanartToPlatforms(ctx, a)
	holdsExactly(t, "after the initial push", peer, local) // PRECONDITION

	if err := os.Remove(filepath.Join(a.Path, "fanart5.jpg")); err != nil {
		t.Fatalf("removing the last local file: %v", err)
	}

	for i := 0; i < 3; i++ {
		p.SyncAllFanartToPlatforms(ctx, a)
		holdsExactly(t, "after a sync", peer, local[:4])
	}
}
