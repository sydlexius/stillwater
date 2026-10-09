package publish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/connection"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/publish/publishtest"
)

// #3200: a local fanart file the push cannot read (unreadable, or degraded by a
// snapshot cap) is a slot the push can never fill. The reconciler used to count
// it as a deficit on every pass and re-push forever; these tests drive the real
// reconciler loop against a peer that models its backdrop list.

// unreadableFanart makes path unreadable and PROVES it; where chmod does not bite
// (root) the test SKIPS, like the other permission-based tests, so it never claims
// to cover an unreadable file.
func unreadableFanart(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		t.Skipf("chmod 0 does not block reads on this runner (running as root?); the unreadable-file case cannot be exercised: %s", filepath.Base(path))
	}
}

// TestReconcile_UnpushableLocalFileIsNotARepeatDeficit is the #3200 churn
// regression: local A,B,C,D with B unreadable, peer holding the compacted set
// A,C,D (what a push can reach). No pass may write, on either peer shape.
// Without the fix the peer's count (3) is below distinct+unreadable (4), so
// every pass re-pushes.
func TestReconcile_UnpushableLocalFileIsNotARepeatDeficit(t *testing.T) {
	A, B, C, D := bandJPEG(t, 51), bandJPEG(t, 52), bandJPEG(t, 53), bandJPEG(t, 54)
	for _, typ := range []string{connection.TypeEmby, connection.TypeJellyfin} {
		t.Run(typ, func(t *testing.T) {
			peer := publishtest.NewPeer(typ, [][]byte{A, C, D})
			p, a := durabilityHarness(t, typ, peer, [][]byte{A, B, C, D})
			unreadableFanart(t, filepath.Join(a.Path, "fanart2.jpg"))
			for pass := 1; pass <= 3; pass++ {
				p.ReconcileArtworkToPlatforms(context.Background())
				got, writes := peer.State()
				if writes != 0 {
					t.Fatalf("pass %d issued %d platform writes, want 0: an unreadable local file must not re-push", pass, writes)
				}
				assertPeerHolds(t, fmt.Sprintf("pass %d", pass), got, [][]byte{A, C, D})
			}
		})
	}
}

// TestReconcile_UnpushableFileDoesNotMaskARealDeficit is the two-sided half: the
// same unreadable B, but the peer is also missing the READABLE image D. Pass 1
// must push it and pass 2 must then be quiet.
func TestReconcile_UnpushableFileDoesNotMaskARealDeficit(t *testing.T) {
	A, B, C, D := bandJPEG(t, 51), bandJPEG(t, 52), bandJPEG(t, 53), bandJPEG(t, 54)
	peer := publishtest.NewPeer(connection.TypeEmby, [][]byte{A, C})
	p, a := durabilityHarness(t, connection.TypeEmby, peer, [][]byte{A, B, C, D})
	unreadableFanart(t, filepath.Join(a.Path, "fanart2.jpg"))

	p.ReconcileArtworkToPlatforms(context.Background())
	got, writes1 := peer.State()
	if writes1 == 0 {
		t.Fatal("pass 1 issued no writes, want the readable missing image pushed")
	}
	assertPeerHolds(t, "pass 1", got, [][]byte{A, C, D})

	p.ReconcileArtworkToPlatforms(context.Background())
	got, writes2 := peer.State()
	if writes2 != writes1 {
		t.Errorf("pass 2 issued %d writes, want 0", writes2-writes1)
	}
	assertPeerHolds(t, "pass 2", got, [][]byte{A, C, D})
}

// TestReconcile_JellyfinMissingImageWithUnpushableSlotRefusesWithoutDeleting:
// the deficit is real but the Jellyfin resync refuses a set with an unpushable
// slot, so the peer must come through byte-identical with no deletes.
func TestReconcile_JellyfinMissingImageWithUnpushableSlotRefusesWithoutDeleting(t *testing.T) {
	A, B, C, D := bandJPEG(t, 51), bandJPEG(t, 52), bandJPEG(t, 53), bandJPEG(t, 54)
	peer := publishtest.NewPeer(connection.TypeJellyfin, [][]byte{A, C})
	p, a := durabilityHarness(t, connection.TypeJellyfin, peer, [][]byte{A, B, C, D})
	unreadableFanart(t, filepath.Join(a.Path, "fanart2.jpg"))

	p.ReconcileArtworkToPlatforms(context.Background())
	got, writes := peer.State()
	if writes != 0 || peer.DeleteCount() != 0 {
		t.Errorf("refused resync touched the peer: %d writes, %d deletes, want 0 and 0", writes, peer.DeleteCount())
	}
	assertPeerHolds(t, "after refused resync", got, [][]byte{A, C})
}

// fixedBackdropReader serves a fixed backdrop list as a connection.BackdropReader.
type fixedBackdropReader struct{ data [][]byte }

func (f fixedBackdropReader) GetArtistDetail(context.Context, string) (*connection.ArtistPlatformState, error) {
	return &connection.ArtistPlatformState{BackdropCount: len(f.data)}, nil
}

func (f fixedBackdropReader) GetArtistBackdrop(_ context.Context, _ string, i int) ([]byte, string, error) {
	return f.data[i], "image/jpeg", nil
}

// TestFanartDeficit_AgreesWithSnapshotFanart pins that the reconciler's notion of
// "pushable" is the push's own, slot by slot, over fixtures that each differ
// along one axis: a chmod 0 file, an oversize-per-file file the push still
// reads (#3017), the file-count cap, and the cumulative-bytes cap.
func TestFanartDeficit_AgreesWithSnapshotFanart(t *testing.T) {
	const mib = 1 << 20
	sparse := func(path string, size int64, tag byte) {
		t.Helper()
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte{tag, tag, tag, tag}, 0); err != nil {
			t.Fatal(err)
		}
	}
	name := func(i int) string {
		if i == 0 {
			return "fanart.jpg"
		}
		return fmt.Sprintf("fanart%d.jpg", i+1)
	}
	cases := []struct {
		name       string
		build      func(t *testing.T, dir string)
		wantNilAt  []int
		wantUnread bool
	}{
		{"chmod 0", func(t *testing.T, dir string) {
			for i := 0; i < 3; i++ {
				writeFile(t, filepath.Join(dir, name(i)), bandJPEG(t, 60+i))
			}
			unreadableFanart(t, filepath.Join(dir, name(1)))
		}, []int{1}, true},
		{"oversize per-file still read", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, name(0)), bandJPEG(t, 70))
			sparse(filepath.Join(dir, name(1)), maxFanartSnapshotFileBytes+mib, 1)
		}, nil, false},
		{"file-count cap", func(t *testing.T, dir string) {
			for i := 0; i < maxFanartSnapshotFiles+2; i++ {
				writeFile(t, filepath.Join(dir, name(i)), []byte(fmt.Sprintf("distinct-%d", i)))
			}
		}, []int{maxFanartSnapshotFiles, maxFanartSnapshotFiles + 1}, false},
		{"total-bytes cap", func(t *testing.T, dir string) {
			for i := 0; i < 17; i++ {
				sparse(filepath.Join(dir, name(i)), 12*mib, byte(i+1))
			}
		}, []int{16}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.build(t, dir)
			p := New(Deps{Logger: silentLogger()})
			paths, err := img.DiscoverFanart(context.Background(), dir, p.getActiveFanartPrimary(context.Background()))
			if err != nil {
				t.Fatalf("discover: %v", err)
			}
			snap, _, err := p.snapshotFanart(context.Background(), paths)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}
			wantNil := map[int]bool{}
			for _, i := range tc.wantNilAt {
				wantNil[i] = true
			}
			var held [][]byte
			for i, sf := range snap {
				if (sf.data == nil) != wantNil[i] {
					t.Fatalf("precondition: slot %d nil=%v, want %v", i, sf.data == nil, wantNil[i])
				}
				if sf.data != nil {
					held = append(held, sf.data)
				}
			}
			if len(held) == len(paths) {
				// No unpushable slot: the platform count alone must decide (no
				// deficit when it already holds every file).
				if p.fanartDeficit(context.Background(), "a", dir, &connection.ArtistPlatformState{BackdropCount: len(held)}, nil, "p") {
					t.Error("deficit with every file held and none unpushable")
				}
				return
			}
			reader := fixedBackdropReader{data: held}
			state := &connection.ArtistPlatformState{BackdropCount: len(held)}
			if p.fanartDeficit(context.Background(), "a", dir, state, reader, "p") {
				t.Errorf("deficit though the platform holds all %d pushable images: classification disagrees with snapshotFanart", len(held))
			}
			state = &connection.ArtistPlatformState{BackdropCount: len(held) - 1}
			if !p.fanartDeficit(context.Background(), "a", dir, state, reader, "p") {
				t.Errorf("no deficit though the platform lacks a pushable image")
			}
		})
	}
}

// flipCtx reports no error until `after` Err calls have been made, then
// context.Canceled: discovery succeeds and the snapshot's reads then fail.
type flipCtx struct {
	context.Context
	after int
	calls int
}

func (c *flipCtx) Err() error {
	c.calls++
	if c.calls > c.after {
		return context.Canceled
	}
	return nil
}

// TestFanartDeficit_SnapshotErrorIsNoDeficit pins the fail direction: when the
// local snapshot errors (here a cancel mid-pass), the check reports no deficit
// even though the peer is genuinely short a readable image.
func TestFanartDeficit_SnapshotErrorIsNoDeficit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fanart.jpg"), bandJPEG(t, 81))
	writeFile(t, filepath.Join(dir, "fanart2.jpg"), bandJPEG(t, 82))
	p := New(Deps{Logger: silentLogger()})
	state := &connection.ArtistPlatformState{BackdropCount: 1}

	probe := &flipCtx{Context: context.Background(), after: 1 << 30}
	if _, err := img.DiscoverFanart(probe, dir, p.getActiveFanartPrimary(probe)); err != nil {
		t.Fatalf("discover: %v", err)
	}
	ctx := &flipCtx{Context: context.Background(), after: probe.calls}
	// PRECONDITION: control, with a live ctx the peer IS short.
	if !p.fanartDeficit(context.Background(), "a", dir, state, fixedBackdropReader{data: [][]byte{bandJPEG(t, 81)}}, "p") {
		t.Fatal("precondition: peer is short a readable image but no deficit reported")
	}
	// PRECONDITION: the snapshot really errors once the ctx is canceled.
	paths := []string{filepath.Join(dir, "fanart.jpg"), filepath.Join(dir, "fanart2.jpg")}
	if _, _, snapErr := p.snapshotFanart(&flipCtx{Context: context.Background()}, paths); snapErr == nil {
		t.Fatal("precondition: snapshot did not error under a canceled ctx")
	}
	if p.fanartDeficit(ctx, "a", dir, state, fixedBackdropReader{data: [][]byte{bandJPEG(t, 81)}}, "p") {
		t.Error("deficit reported on a snapshot error, want none (retry next pass)")
	}
}
