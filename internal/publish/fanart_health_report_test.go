package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// #3200: after a full-set fanart snapshot the publisher tells a health reporter
// which backdrops could not be read. These tests drive the real snapshot and the
// real sync paths against a reporter that models the peer's STATE (open or
// closed per artist, plus the last slot list), not just a list of calls.

// fakeFanartHealth models the rule service's finding for each artist: Raise
// opens it and replaces the slot list, Resolve closes it.
type fakeFanartHealth struct {
	mu       sync.Mutex
	open     map[string]bool
	slots    map[string][]int
	reasons  map[string]string
	raises   int
	resolves int
	err      error                 // returned from both methods when set
	block    bool                  // calls wait for their context to end
	inResolv func(artistID string) // test seam: runs inside Resolve, before it takes effect
}

func newFakeFanartHealth() *fakeFanartHealth {
	return &fakeFanartHealth{open: map[string]bool{}, slots: map[string][]int{}, reasons: map[string]string{}}
}

// callErr models a database call: blocked until the context ends, or failing
// when the context is already done.
func (f *fakeFanartHealth) callErr(ctx context.Context) error {
	if f.block {
		<-ctx.Done()
	}
	return ctx.Err()
}

func (f *fakeFanartHealth) RaiseFanartUnreadable(ctx context.Context, artistID string, slots []int, reason string) error {
	if err := f.callErr(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raises++
	if f.err != nil {
		return f.err
	}
	f.open[artistID] = true
	f.slots[artistID] = append([]int(nil), slots...)
	f.reasons[artistID] = reason
	return nil
}

func (f *fakeFanartHealth) ResolveFanartUnreadable(ctx context.Context, artistID string) error {
	if err := f.callErr(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	hook := f.inResolv
	f.mu.Unlock()
	if hook != nil {
		hook(artistID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.err != nil {
		return f.err
	}
	f.open[artistID] = false
	delete(f.slots, artistID)
	return nil
}

// state returns a consistent view: is a finding open, its slots, and the call counts.
func (f *fakeFanartHealth) state(artistID string) (open bool, slots []int, raises, resolves int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[artistID], append([]int(nil), f.slots[artistID]...), f.raises, f.resolves
}

// mixedFanartPaths returns three paths: slots 0 and 2 are real images and slot 1
// does not exist, so the snapshot keeps it with nil data. A missing file needs no
// chmod, so this holds when the tests run as root.
func mixedFanartPaths(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	good0, good2 := filepath.Join(dir, "fanart.jpg"), filepath.Join(dir, "fanart3.jpg")
	writeFile(t, good0, bandJPEG(t, 11))
	writeFile(t, good2, bandJPEG(t, 12))
	return []string{good0, filepath.Join(dir, "fanart2.jpg"), good2}
}

func cleanFanartPaths(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	p0, p1 := filepath.Join(dir, "fanart.jpg"), filepath.Join(dir, "fanart2.jpg")
	writeFile(t, p0, bandJPEG(t, 21))
	writeFile(t, p1, bandJPEG(t, 22))
	return []string{p0, p1}
}

func TestFanartHealth_NilSlotRaisesWithTheZeroBasedSlotList(t *testing.T) {
	rep := newFakeFanartHealth()
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)

	snap, _, err := p.snapshotFanartAndReport(context.Background(), "a1", mixedFanartPaths(t))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap) != 3 || snap[1].data != nil || snap[0].data == nil || snap[2].data == nil {
		t.Fatalf("precondition: want exactly slot 1 uncaptured, got %d slots", len(snap))
	}
	open, slots, raises, _ := rep.state("a1")
	if !open || raises != 1 {
		t.Fatalf("finding open=%v after %d raises, want open after exactly 1", open, raises)
	}
	// 0-based local backdrop indexes, as the publish path reports them (the finding names files, not positions).
	if !reflect.DeepEqual(slots, []int{1}) {
		t.Errorf("slots = %v, want [1]", slots)
	}
	if rep.reasons["a1"] == "" {
		t.Error("reason is empty, want a short plain cause")
	}
}

func TestFanartHealth_CleanSnapshotResolvesAnOpenFinding(t *testing.T) {
	rep := newFakeFanartHealth()
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)
	ctx := context.Background()

	if _, _, err := p.snapshotFanartAndReport(ctx, "a1", mixedFanartPaths(t)); err != nil {
		t.Fatal(err)
	}
	if open, _, _, _ := rep.state("a1"); !open {
		t.Fatal("precondition: finding should be open before the clean pass")
	}
	if _, _, err := p.snapshotFanartAndReport(ctx, "a1", cleanFanartPaths(t)); err != nil {
		t.Fatal(err)
	}
	open, slots, _, resolves := rep.state("a1")
	if open || len(slots) != 0 || resolves != 1 {
		t.Errorf("after a clean pass open=%v slots=%v resolves=%d, want closed, no slots, 1 resolve", open, slots, resolves)
	}
}

func TestFanartHealth_AbortedSnapshotNeitherRaisesNorResolves(t *testing.T) {
	rep := newFakeFanartHealth()
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)
	paths := mixedFanartPaths(t)
	if _, _, err := p.snapshotFanartAndReport(context.Background(), "a1", paths); err != nil {
		t.Fatal(err)
	}
	_, _, raises0, resolves0 := rep.state("a1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A canceled snapshot over a CLEAN set: if an error were treated as evidence
	// it would resolve; over a failing set it would re-raise. Check both.
	for name, set := range map[string][]string{"clean set": cleanFanartPaths(t), "failing set": paths} {
		if _, _, err := p.snapshotFanartAndReport(ctx, "a1", set); err == nil {
			t.Fatalf("%s: precondition: the snapshot did not error under a canceled context", name)
		}
	}
	open, slots, raises, resolves := rep.state("a1")
	if !open || !reflect.DeepEqual(slots, []int{1}) {
		t.Errorf("an aborted snapshot changed the finding: open=%v slots=%v, want it left open with [1]", open, slots)
	}
	if raises != raises0 || resolves != resolves0 {
		t.Errorf("an aborted snapshot reported: raises %d->%d resolves %d->%d", raises0, raises, resolves0, resolves)
	}
}

// unreadableMiddleHarness is an Emby Publisher on real SQLite with one artist whose
// backdrop B (fanart2.jpg, slot 1) is unreadable, and a peer that models its
// backdrop list. It SKIPS where chmod does not bite (root).
func unreadableMiddleHarness(t *testing.T, peer *statefulBackdropPeer, rep FanartHealthReporter) (*Publisher, *artist.Artist, string) {
	t.Helper()
	A, B, C, D := bandJPEG(t, 51), bandJPEG(t, 52), bandJPEG(t, 53), bandJPEG(t, 54)
	p, a := durabilityHarness(t, connection.TypeEmby, peer, [][]byte{A, B, C, D})
	p.SetFanartHealthReporter(rep)
	bad := filepath.Join(a.Path, "fanart2.jpg")
	unreadableFanart(t, bad)
	return p, a, bad
}

func TestFanartHealth_ManualSyncRaisesThenResolves(t *testing.T) {
	rep := newFakeFanartHealth()
	peer := &statefulBackdropPeer{}
	p, a, bad := unreadableMiddleHarness(t, peer, rep)

	p.SyncAllFanartToPlatforms(context.Background(), a)
	open, slots, _, _ := rep.state(a.ID)
	if !open || !reflect.DeepEqual(slots, []int{1}) {
		t.Fatalf("manual sync: open=%v slots=%v, want open with [1]", open, slots)
	}

	if err := chmodReadable(bad); err != nil {
		t.Fatal(err)
	}
	p.SyncAllFanartToPlatforms(context.Background(), a)
	if open, _, _, _ := rep.state(a.ID); open {
		t.Error("manual sync after the file became readable left the finding open")
	}
}

func TestFanartHealth_BackgroundReconcilerRaises(t *testing.T) {
	// The peer already holds the three pushable images, so the reconciler finds no
	// deficit and never runs a push. The only thing that can raise is the
	// reconciler's own full-set read.
	A, C, D := bandJPEG(t, 51), bandJPEG(t, 53), bandJPEG(t, 54)
	rep := newFakeFanartHealth()
	peer := &statefulBackdropPeer{data: [][]byte{A, C, D}}
	p, a, _ := unreadableMiddleHarness(t, peer, rep)

	p.ReconcileArtworkToPlatforms(context.Background())
	if _, writes := peer.state(); writes != 0 {
		t.Fatalf("precondition: the reconciler pushed (%d writes), so a raise could come from the push path", writes)
	}
	open, slots, _, _ := rep.state(a.ID)
	if !open || !reflect.DeepEqual(slots, []int{1}) {
		t.Errorf("reconciler: open=%v slots=%v, want open with [1]", open, slots)
	}
}

func TestFanartHealth_ReconcilerPushPathRaises(t *testing.T) {
	// Here the peer is short a readable image, so the reconciler pushes through
	// syncAllFanartToPlatforms(respectWriteGate=true).
	A, C := bandJPEG(t, 51), bandJPEG(t, 53)
	rep := newFakeFanartHealth()
	peer := &statefulBackdropPeer{data: [][]byte{A, C}}
	p, a, _ := unreadableMiddleHarness(t, peer, rep)

	p.ReconcileArtworkToPlatforms(context.Background())
	if _, writes := peer.state(); writes == 0 {
		t.Fatal("precondition: the reconciler did not push")
	}
	if open, slots, _, _ := rep.state(a.ID); !open || !reflect.DeepEqual(slots, []int{1}) {
		t.Errorf("reconciler push: open=%v slots=%v, want open with [1]", open, slots)
	}
}

func TestFanartHealth_JellyfinRefusedResyncRaises(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "fanart.jpg"), bandJPEG(t, 61))
	// Over the read bound, so the snapshot degrades slot 1 and the resync refuses.
	sparseFile(t, filepath.Join(dir, "fanart2.jpg"), 26<<20)

	fake := &fakeResyncClient{backdropCount: 2}
	withFakeFanartResyncClient(t, fake)
	rep := newFakeFanartHealth()
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)

	uploaded, warning := p.uploadFanartFullResyncForSync(context.Background(), &artist.Artist{ID: "a1", Path: dir},
		artist.PlatformID{ConnectionID: "c-jf", PlatformArtistID: "p1"},
		&connection.Connection{ID: "c-jf", Name: "my-jellyfin", Type: connection.TypeJellyfin})
	if uploaded || warning == "" {
		t.Fatalf("precondition: the resync should have been refused (uploaded=%v warning=%q)", uploaded, warning)
	}
	if open, slots, _, _ := rep.state("a1"); !open || !reflect.DeepEqual(slots, []int{1}) {
		t.Errorf("refused Jellyfin resync: open=%v slots=%v, want open with [1]", open, slots)
	}
}

func TestFanartHealth_ReporterErrorDoesNotFailOrShortenTheSync(t *testing.T) {
	rep := newFakeFanartHealth()
	rep.err = errors.New("database is locked")
	peer := &statefulBackdropPeer{}
	p, a, _ := unreadableMiddleHarness(t, peer, rep)

	p.SyncAllFanartToPlatforms(context.Background(), a)
	if _, _, raises, _ := rep.state(a.ID); raises == 0 {
		t.Fatal("precondition: the reporter was never called")
	}
	got, writes := peer.state()
	if writes == 0 {
		t.Fatal("the push did not happen after the reporter failed")
	}
	assertPeerHolds(t, "after a failing reporter", got, [][]byte{bandJPEG(t, 51), bandJPEG(t, 53), bandJPEG(t, 54)})
}

// TestFanartHealth_OverlappingSnapshotsCannotResolveANewerFailure is the
// ordering seam: an older CLEAN snapshot that reports AFTER a newer failing one
// must not close the finding the newer one opened. The clean pass is held inside
// Resolve while a failing pass for the same artist starts.
func TestFanartHealth_OverlappingSnapshotsCannotResolveANewerFailure(t *testing.T) {
	rep := newFakeFanartHealth()
	inResolve, release := make(chan struct{}), make(chan struct{})
	rep.inResolv = func(string) {
		close(inResolve)
		<-release
	}
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)
	clean, failing := cleanFanartPaths(t), mixedFanartPaths(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, _ = p.snapshotFanartAndReport(ctx, "a1", clean)
	}()
	select {
	case <-inResolve:
	case <-time.After(10 * time.Second):
		t.Fatal("the clean pass never reached Resolve")
	}
	// The clean snapshot is already taken. The failing pass now snapshots and
	// reports; with the per-artist guard it must wait for the clean pass to finish.
	go func() {
		defer wg.Done()
		_, _, _ = p.snapshotFanartAndReport(ctx, "a1", failing)
	}()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, _, raises, _ := rep.state("a1"); raises > 0 {
			break // unguarded: the newer failure reported first
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	wg.Wait()

	if open, slots, _, _ := rep.state("a1"); !open || !reflect.DeepEqual(slots, []int{1}) {
		t.Errorf("an older clean snapshot closed a newer failure: open=%v slots=%v, want open with [1]", open, slots)
	}
}

// chmodReadable restores a file made unreadable by unreadableFanart.
func chmodReadable(path string) error { return os.Chmod(path, 0o600) }

// missingFanart returns paths for the given file names in a fresh directory,
// writing a real image for each name in readable and leaving the rest absent
// (a nil slot).
func missingFanart(t *testing.T, readable map[string]bool, names ...string) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = filepath.Join(dir, n)
		if readable[n] {
			writeFile(t, paths[i], bandJPEG(t, 30+i))
		}
	}
	return paths
}

func reportedReason(t *testing.T, paths []string) (slots []int, reason string) {
	t.Helper()
	rep := newFakeFanartHealth()
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)
	if _, _, err := p.snapshotFanartAndReport(context.Background(), "a1", paths); err != nil {
		t.Fatal(err)
	}
	return rep.slots["a1"], rep.reasons["a1"]
}

// The slot number does not name the file under Kodi naming or after a gap, so
// the reason names base names, and every unreadable slot is reported.
func TestFanartHealth_ReasonNamesTheFilesAndEverySlot(t *testing.T) {
	ok := map[string]bool{"fanart.jpg": true, "fanart2.jpg": true}
	slots, reason := reportedReason(t, missingFanart(t, ok, "fanart.jpg", "fanart1.jpg", "fanart2.jpg", "fanart4.jpg"))
	if !reflect.DeepEqual(slots, []int{1, 3}) {
		t.Errorf("slots = %v, want [1 3]", slots)
	}
	want := "2 backdrop files could not be read, so Stillwater is not sending them to your media servers: fanart1.jpg, fanart4.jpg. Check that each file exists, can be read and is not too large."
	if reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}

	var many []string
	for i := 1; i <= 12; i++ {
		many = append(many, fmt.Sprintf("b%02d.jpg", i))
	}
	_, reason = reportedReason(t, missingFanart(t, nil, many...))
	if !strings.Contains(reason, "b10.jpg and 2 more.") || strings.Contains(reason, "b11") {
		t.Errorf("reason = %q, want the list capped at 10 names then \"and 2 more\"", reason)
	}
}

// The name cap is a boundary: exactly maxReportedFanartNames names are listed in
// full with no "and N more" tail, and one more name triggers the tail.
func TestFanartNameList_CapBoundary(t *testing.T) {
	names := make([]string, 0, maxReportedFanartNames+1)
	for i := 0; i < maxReportedFanartNames+1; i++ {
		names = append(names, fmt.Sprintf("b%02d.jpg", i))
	}
	if got := fanartNameList(names[:maxReportedFanartNames]); strings.Contains(got, "more") || !strings.HasSuffix(got, "b09.jpg") {
		t.Errorf("exactly %d names = %q, want all listed and no tail", maxReportedFanartNames, got)
	}
	if got := fanartNameList(names); !strings.HasSuffix(got, "and 1 more") || strings.Contains(got, "b10") {
		t.Errorf("%d names = %q, want the first %d then \"and 1 more\"", len(names), got, maxReportedFanartNames)
	}
}

// A readable file left out by the snapshot budget is reported, but not as unreadable.
func TestFanartHealth_BudgetSkippedFileIsNotCalledUnreadable(t *testing.T) {
	dir := t.TempDir()
	img := bandJPEG(t, 40)
	var paths []string
	for i := 1; i <= maxFanartSnapshotFiles+1; i++ {
		paths = append(paths, filepath.Join(dir, fmt.Sprintf("fanart%d.jpg", i)))
		writeFile(t, paths[i-1], img)
	}
	slots, reason := reportedReason(t, paths)
	if !reflect.DeepEqual(slots, []int{maxFanartSnapshotFiles}) {
		t.Fatalf("slots = %v, want only the over-budget slot", slots)
	}
	if !strings.Contains(reason, "fanart101.jpg") || !strings.Contains(reason, "over the size or count limit") || strings.Contains(reason, "could not be read") {
		t.Errorf("reason = %q, want the file named with the limit as the cause, not a read failure", reason)
	}
}

// The three message shapes (#3469), each singular and plural, produced by the
// real snapshot. Files are named by base name only: the position of fanart2.jpg
// in a Kodi-style set is 3, and that digit must never appear.
func TestFanartHealth_MessageShapes(t *testing.T) {
	// numbered builds a set of n backdrops fanart.jpg, fanart1.jpg, ... in which
	// the names listed in missing are absent. Over the snapshot budget the tail
	// is left out as readable-but-skipped.
	numbered := func(t *testing.T, n int, missing ...string) []string {
		names := []string{"fanart.jpg"}
		for i := 1; i < n; i++ {
			names = append(names, fmt.Sprintf("fanart%d.jpg", i))
		}
		ok := map[string]bool{}
		for _, nm := range names {
			ok[nm] = !slices.Contains(missing, nm)
		}
		return missingFanart(t, ok, names...)
	}
	const (
		unread1 = "1 backdrop file could not be read, so Stillwater is not sending it to your media servers: fanart2.jpg. Check that the file exists, can be read and is not too large."
		skip1   = "1 backdrop file was left out of this push because the set is over the size or count limit for one push: fanart100.jpg. The file itself may be fine."
		skip2   = "2 backdrop files were left out of this push because the set is over the size or count limit for one push: fanart100.jpg, fanart101.jpg. The files themselves may be fine."
		// An unreadable file does not use up the read budget, so with one missing
		// file in a 102-file set only the last file is left out.
		skipLast = "1 backdrop file was left out of this push because the set is over the size or count limit for one push: fanart101.jpg. The file itself may be fine."
		unread2  = "2 backdrop files could not be read, so Stillwater is not sending them to your media servers: fanart2.jpg, fanart3.jpg. Check that each file exists, can be read and is not too large."
	)
	cases := []struct {
		name string
		n    int
		miss []string
		want string
	}{
		{"one unreadable, position differs from name", 4, []string{"fanart2.jpg"}, unread1},
		{"two unreadable", 5, []string{"fanart2.jpg", "fanart3.jpg"}, unread2},
		{"one over the limit", maxFanartSnapshotFiles + 1, nil, skip1},
		{"two over the limit", maxFanartSnapshotFiles + 2, nil, skip2},
		{"both kinds, each under its own clause", maxFanartSnapshotFiles + 2, []string{"fanart2.jpg"}, unread1 + " " + skipLast},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, reason := reportedReason(t, numbered(t, tc.n, tc.miss...))
			if reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
			if strings.Contains(tc.want, "left out") && !strings.Contains(tc.want, "could not be read") && strings.Contains(reason, "could not be read") {
				t.Errorf("an over-limit-only message says the file could not be read: %q", reason)
			}
		})
	}
	// Position 3 (fanart2.jpg) must not leak as a number beside the name.
	if _, reason := reportedReason(t, numbered(t, 4, "fanart2.jpg")); strings.Contains(reason, "3") {
		t.Errorf("reason shows a position number: %q", reason)
	}
}

// With no backdrop left there is nothing to be unreadable: a stale finding clears.
func TestFanartHealth_NoBackdropsLeftResolvesAStaleFinding(t *testing.T) {
	t.Run("manual sync", func(t *testing.T) {
		rep := newFakeFanartHealth()
		p, a := durabilityHarness(t, connection.TypeEmby, &statefulBackdropPeer{}, nil)
		p.SetFanartHealthReporter(rep)
		_ = rep.RaiseFanartUnreadable(context.Background(), a.ID, []int{1}, "x")
		p.SyncAllFanartToPlatforms(context.Background(), a)
		if open, _, _, _ := rep.state(a.ID); open {
			t.Error("finding stayed open after the artist's last backdrop was gone")
		}
	})
	t.Run("jellyfin resync", func(t *testing.T) {
		withFakeFanartResyncClient(t, &fakeResyncClient{})
		rep := newFakeFanartHealth()
		p := New(Deps{Logger: silentLogger()})
		p.SetFanartHealthReporter(rep)
		_ = rep.RaiseFanartUnreadable(context.Background(), "a1", []int{1}, "x")
		p.uploadFanartFullResyncForSync(context.Background(), &artist.Artist{ID: "a1", Path: t.TempDir()},
			artist.PlatformID{ConnectionID: "c", PlatformArtistID: "p"}, &connection.Connection{ID: "c", Type: connection.TypeJellyfin})
		if open, _, _, _ := rep.state("a1"); open {
			t.Error("finding stayed open after the artist's last backdrop was gone")
		}
	})
}

// Parks one pass inside the snapshot step and runs another against it.
func overlapPasses(t *testing.T, rep *fakeFanartHealth, parked, failing []string) {
	t.Helper()
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)
	in, release := make(chan struct{}), make(chan struct{})
	var parkedOnce atomic.Bool
	fanartSnapshotTakenHook = func(string) {
		// Only the first pass parks. (sync.Once would block the second pass too.)
		if parkedOnce.CompareAndSwap(false, true) {
			close(in)
			<-release
		}
	}
	t.Cleanup(func() { fanartSnapshotTakenHook = nil })
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _, _ = p.snapshotFanartAndReport(context.Background(), "a1", parked) }()
	<-in
	go func() { defer wg.Done(); _, _, _ = p.snapshotFanartAndReport(context.Background(), "a1", failing) }()
	time.Sleep(300 * time.Millisecond)
	close(release)
	wg.Wait()
}

// The snapshot itself must be inside the lock: a clean pass parked right after
// its snapshot must hold off a failing pass that starts meanwhile.
func TestFanartHealth_SnapshotIsTakenInsideTheLock(t *testing.T) {
	rep := newFakeFanartHealth()
	overlapPasses(t, rep, cleanFanartPaths(t), mixedFanartPaths(t))
	if open, slots, _, _ := rep.state("a1"); !open || !reflect.DeepEqual(slots, []int{1}) {
		t.Errorf("open=%v slots=%v, want the later failing pass to leave the finding open with [1]", open, slots)
	}
}

// A snapshot that errors midway holds partial data, and is still no evidence.
func TestFanartHealth_PartialSnapshotThatErroredReportsNothing(t *testing.T) {
	rep := newFakeFanartHealth()
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)
	_ = rep.RaiseFanartUnreadable(context.Background(), "a1", []int{1}, "x")
	paths := cleanFanartPaths(t)
	after := -1
	for n := 0; n < 50 && after < 0; n++ {
		if snap, _, err := p.snapshotFanart(&flipCtx{Context: context.Background(), after: n}, paths); err != nil && len(snap) > 0 {
			after = n
		}
	}
	if after < 0 {
		t.Fatal("precondition: no cancel point gives an error together with partial data")
	}
	_, _, raises0, _ := rep.state("a1")
	if _, _, err := p.snapshotFanartAndReport(&flipCtx{Context: context.Background(), after: after}, "a1", paths); err == nil {
		t.Fatal("precondition: the snapshot did not error")
	}
	if open, _, raises, resolves := rep.state("a1"); !open || resolves != 0 || raises != raises0 {
		t.Errorf("a partial errored snapshot reported: open=%v raises=%d resolves=%d", open, raises, resolves)
	}
}

// The request ending right after the snapshot must not lose the report.
func TestFanartHealth_ReportSurvivesTheRequestEnding(t *testing.T) {
	rep := newFakeFanartHealth()
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)
	ctx, cancel := context.WithCancel(context.Background())
	fanartSnapshotTakenHook = func(string) { cancel() }
	t.Cleanup(func() { fanartSnapshotTakenHook = nil })
	if _, _, err := p.snapshotFanartAndReport(ctx, "a1", mixedFanartPaths(t)); err != nil {
		t.Fatal(err)
	}
	if open, _, _, _ := rep.state("a1"); !open {
		t.Error("the report was lost when the request ended after the snapshot")
	}
}

// A reporter that never answers cannot hold the push for longer than the timeout.
func TestFanartHealth_ReportIsBoundedByATimeout(t *testing.T) {
	rep := newFakeFanartHealth()
	rep.block = true
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)
	prev := fanartReportTimeout
	fanartReportTimeout = 50 * time.Millisecond
	t.Cleanup(func() { fanartReportTimeout = prev })
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = p.snapshotFanartAndReport(context.Background(), "a1", mixedFanartPaths(t))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the report was not bounded by its timeout")
	}
}

// parkHolder starts a clean pass for "a1" and parks it right after its snapshot,
// so it holds the artist's gate. The returned func releases it and waits.
func parkHolder(t *testing.T, p *Publisher) (release func()) {
	t.Helper()
	in, rel := make(chan struct{}), make(chan struct{})
	var parkedOnce atomic.Bool
	fanartSnapshotTakenHook = func(string) {
		if parkedOnce.CompareAndSwap(false, true) {
			close(in)
			<-rel
		}
	}
	t.Cleanup(func() { fanartSnapshotTakenHook = nil })
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = p.snapshotFanartAndReport(context.Background(), "a1", cleanFanartPaths(t))
	}()
	<-in
	var once sync.Once
	release = func() { once.Do(func() { close(rel); <-done }) }
	t.Cleanup(release)
	return release
}

// runBehindHolder runs a failing pass for "a1" and returns what it handed back
// to its caller, or fails if it does not return within a second.
func runBehindHolder(t *testing.T, p *Publisher, ctx context.Context) ([]fanartSnapshot, error) {
	t.Helper()
	type result struct {
		snap []fanartSnapshot
		err  error
	}
	got := make(chan result, 1)
	go func() {
		snap, _, err := p.snapshotFanartAndReport(ctx, "a1", mixedFanartPaths(t))
		got <- result{snap, err}
	}()
	select {
	case r := <-got:
		return r.snap, r.err
	case <-time.After(time.Second):
		t.Fatal("the pass behind a stalled holder did not return: its gate wait is not cancelable or bounded")
		return nil, nil
	}
}

// A push waiting behind a stalled earlier pass returns promptly when its request
// ends and does not report. The request is over, so its own snapshot stops with
// the cancel error, exactly as a canceled push always has; the abandoned wait
// itself adds no failure.
func TestFanartHealth_WaitBehindAStalledPassEndsWithTheRequest(t *testing.T) {
	rep := newFakeFanartHealth()
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)
	parkHolder(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := runBehindHolder(t, p, ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the request's own cancel", err)
	}
	if _, _, raises, resolves := rep.state("a1"); raises != 0 || resolves != 0 {
		t.Errorf("an unordered pass reported: raises=%d resolves=%d, want none", raises, resolves)
	}
}

// A wait that times out has the same outcome with no cancel at all.
func TestFanartHealth_GateWaitTimeoutStillPushesWithoutReporting(t *testing.T) {
	rep := newFakeFanartHealth()
	p := New(Deps{Logger: silentLogger()})
	p.SetFanartHealthReporter(rep)
	prev := fanartGateWait
	fanartGateWait = 50 * time.Millisecond
	t.Cleanup(func() { fanartGateWait = prev })
	release := parkHolder(t, p)

	snap, err := runBehindHolder(t, p, context.Background())
	if err != nil || len(snap) != 3 {
		t.Errorf("the timed-out pass returned %d slots, err %v; want its full snapshot of 3 and no error so the push goes on", len(snap), err)
	}
	if _, _, raises, _ := rep.state("a1"); raises != 0 {
		t.Errorf("the timed-out pass raised %d time(s), want 0", raises)
	}
	release()
	if open, _, raises, resolves := rep.state("a1"); open || raises != 0 || resolves != 1 {
		t.Errorf("after the holder finished: open=%v raises=%d resolves=%d, want only the holder's own resolve", open, raises, resolves)
	}
}
