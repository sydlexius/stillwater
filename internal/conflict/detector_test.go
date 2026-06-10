package conflict

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/event"
)

// fakeRepo lets us drive the detector with a fixed connection list without
// standing up a real DB.
type fakeRepo struct {
	conns []connection.Connection
}

func (f *fakeRepo) List(_ context.Context) ([]connection.Connection, error) {
	return f.conns, nil
}

// fakeClient implements peerClient with hard-coded booleans, so each test
// can describe exactly which conflicts exist on each peer.
type fakeClient struct {
	nfo            bool
	image          bool
	libName        string
	nfoErr         error
	imageErr       error
	disableErr     error
	clearOnDisable bool
	callCount      int
	disableCalls   int
	mu             sync.Mutex
}

func (f *fakeClient) CheckNFOWriterEnabled(_ context.Context) (bool, string, error) {
	f.mu.Lock()
	f.callCount++
	f.mu.Unlock()
	return f.nfo, f.libName, f.nfoErr
}

func (f *fakeClient) CheckImageSaverEnabled(_ context.Context) (bool, string, error) {
	f.mu.Lock()
	f.callCount++
	f.mu.Unlock()
	return f.image, f.libName, f.imageErr
}

// DisableFileWriteBack counts invocations so tests can assert the auto
// re-disable path fired. Individual tests that want the "peer kept its
// savers off after disable" behavior can flip `clearOnDisable=true`;
// tests that want to verify the detector keeps re-calling on every
// refresh (because the peer keeps re-enabling the saver) can leave it
// false.
func (f *fakeClient) DisableFileWriteBack(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disableCalls++
	if f.clearOnDisable {
		f.nfo = false
		f.image = false
	}
	return f.disableErr
}

type fakePaths struct{ paths []string }

func (f *fakePaths) MusicLibraryPaths(_ context.Context) ([]string, error) {
	return f.paths, nil
}

func newLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// buildDetector wires a detector whose per-connection client is looked up
// from the supplied map by connection ID.
func buildDetector(t *testing.T, conns []connection.Connection, clients map[string]peerClient, paths map[string]pathProvider) (*Detector, *event.Bus) {
	t.Helper()
	bus := event.NewBus(newLogger(), 16)
	factory := func(c connection.Connection) (peerClient, pathProvider) {
		return clients[c.ID], paths[c.ID]
	}
	return newDetectorWithClients(&fakeRepo{conns: conns}, bus, newLogger(), factory), bus
}

func TestLedgerBannerStates(t *testing.T) {
	cases := []struct {
		name  string
		state ConnectionState
		want  string
	}{
		{"clean", ConnectionState{Enabled: true}, "clean"},
		{"image only", ConnectionState{Enabled: true, ImageWriteback: true}, "image_only"},
		{"nfo only", ConnectionState{Enabled: true, NFOWriteback: true}, "nfo_only"},
		{"both", ConnectionState{Enabled: true, ImageWriteback: true, NFOWriteback: true}, "both"},
		{"managed suppresses", ConnectionState{Enabled: true, ImageWriteback: true, NFOWriteback: true, ManageServerFiles: true}, "clean"},
		{"disabled suppresses", ConnectionState{Enabled: false, ImageWriteback: true}, "clean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := Ledger{Connections: []ConnectionState{tc.state}}
			if got := l.BannerState(); got != tc.want {
				t.Errorf("BannerState = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLedgerForeignFilesState(t *testing.T) {
	// foreign_files state appears only when no real conflict is active.
	clean := Ledger{ForeignFiles: ForeignFileSummary{Count: 3}}
	if got := clean.BannerState(); got != "foreign_files" {
		t.Errorf("clean ledger with foreign files: got %q, want %q", got, "foreign_files")
	}
	// A real conflict suppresses the foreign-files state so the user sees
	// the higher-severity banner instead.
	withConflict := Ledger{
		Connections:  []ConnectionState{{Enabled: true, ImageWriteback: true}},
		ForeignFiles: ForeignFileSummary{Count: 3},
	}
	if got := withConflict.BannerState(); got != "image_only" {
		t.Errorf("conflict + foreign files: got %q, want %q", got, "image_only")
	}
	// Zero count keeps the clean state.
	emptyClean := Ledger{ForeignFiles: ForeignFileSummary{Count: 0}}
	if got := emptyClean.BannerState(); got != "clean" {
		t.Errorf("clean ledger w/o foreign files: got %q, want %q", got, "clean")
	}
}

func TestLedgerRoundTripPromotesToC(t *testing.T) {
	l := Ledger{
		Connections: []ConnectionState{{Enabled: true}},
		RoundTrips:  []RoundTrip{{OverlappingPath: "/music"}},
	}
	if got := l.BannerState(); got != "round_trip" {
		t.Errorf("want round_trip, got %s", got)
	}
	if !l.AnyImageConflict() || !l.AnyNFOConflict() {
		t.Error("round-trip should force both image and NFO gates on")
	}
}

func TestDetectorAggregatesPerPlatform(t *testing.T) {
	conns := []connection.Connection{
		{ID: "emby", Name: "Emby", Type: connection.TypeEmby, Enabled: true},
		{ID: "jfin", Name: "Jellyfin", Type: connection.TypeJellyfin, Enabled: true},
		{ID: "lid", Name: "Lidarr", Type: connection.TypeLidarr, Enabled: true},
	}
	clients := map[string]peerClient{
		"emby": &fakeClient{image: true, libName: "Music"},
		"jfin": &fakeClient{nfo: true, libName: "Jellyfin Music"},
		"lid":  &fakeClient{nfo: true, image: true},
	}
	d, _ := buildDetector(t, conns, clients, nil)
	l := d.Refresh(context.Background())

	if len(l.Connections) != 3 {
		t.Fatalf("want 3 connections, got %d", len(l.Connections))
	}
	if !l.AnyImageConflict() {
		t.Error("image gate should engage")
	}
	if !l.AnyNFOConflict() {
		t.Error("NFO gate should engage")
	}
	if l.BannerState() != "both" {
		t.Errorf("want banner=both, got %s", l.BannerState())
	}
}

func TestDetectorManagedConnectionContributesNothing(t *testing.T) {
	conns := []connection.Connection{
		{ID: "emby", Name: "Emby", Type: connection.TypeEmby, Enabled: true, FeatureManageServerFiles: true},
	}
	clients := map[string]peerClient{
		"emby": &fakeClient{image: true, nfo: true, libName: "Music"},
	}
	d, _ := buildDetector(t, conns, clients, nil)
	l := d.Refresh(context.Background())

	if l.AnyImageConflict() || l.AnyNFOConflict() {
		t.Error("managed connection should not contribute to gate")
	}
	if l.BannerState() != "clean" {
		t.Errorf("want clean, got %s", l.BannerState())
	}
}

func TestDetectorPublishesEventOnTransition(t *testing.T) {
	conns := []connection.Connection{{ID: "a", Type: connection.TypeEmby, Enabled: true}}
	client := &fakeClient{}
	clients := map[string]peerClient{"a": client}
	d, bus := buildDetector(t, conns, clients, nil)

	var (
		mu   sync.Mutex
		seen []event.Event
		// got buffers one signal per delivered event so the test can wait
		// on a synchronization point instead of a fixed sleep. Using a
		// time-based drain made this test flaky on slow runners; the
		// channel approach pins exact event-arrival ordering.
		got = make(chan event.Event, 4)
	)
	bus.Subscribe(event.ConflictChanged, func(e event.Event) {
		mu.Lock()
		seen = append(seen, e)
		mu.Unlock()
		// Non-blocking send so the bus goroutine never stalls if the
		// test exits early.
		select {
		case got <- e:
		default:
		}
	})
	go bus.Start()
	defer bus.Stop()

	// First refresh: clean -> clean (no event should fire).
	d.Refresh(context.Background())

	// Simulate peer flipping its image saver on.
	client.image = true
	d.Refresh(context.Background())

	// Wait for the transition event with a generous timeout. A failure here
	// means the bus did not deliver the expected event within a second,
	// which is many orders of magnitude beyond the in-process dispatch
	// latency under load.
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ConflictChanged event")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("want 1 event after transition, got %d", len(seen))
	}
	if seen[0].Data["banner_state"] != "image_only" {
		t.Errorf("want banner_state=image_only, got %v", seen[0].Data["banner_state"])
	}
}

func TestDetectorRoundTripOverPathOverlap(t *testing.T) {
	conns := []connection.Connection{
		{ID: "a", Name: "A", Type: connection.TypeEmby, Enabled: true},
		{ID: "b", Name: "B", Type: connection.TypeJellyfin, Enabled: true},
	}
	clients := map[string]peerClient{
		"a": &fakeClient{image: true},
		"b": &fakeClient{},
	}
	paths := map[string]pathProvider{
		"a": &fakePaths{paths: []string{"/mnt/music"}},
		"b": &fakePaths{paths: []string{"/mnt/music/rock"}},
	}
	d, _ := buildDetector(t, conns, clients, paths)
	l := d.Refresh(context.Background())

	if len(l.RoundTrips) != 1 {
		t.Fatalf("want 1 round-trip, got %d", len(l.RoundTrips))
	}
	if l.RoundTrips[0].OverlappingPath != "/mnt/music" {
		t.Errorf("overlap path = %q, want /mnt/music", l.RoundTrips[0].OverlappingPath)
	}
}

func TestDetectorCacheTTL(t *testing.T) {
	conns := []connection.Connection{{ID: "a", Type: connection.TypeEmby, Enabled: true}}
	client := &fakeClient{}
	d, _ := buildDetector(t, conns, map[string]peerClient{"a": client}, nil)
	d.ttl = 50 * time.Millisecond

	d.Current(context.Background())
	calls := client.callCount
	d.Current(context.Background())
	if client.callCount != calls {
		t.Error("within TTL the detector should serve from cache")
	}

	d.Invalidate()
	d.Current(context.Background())
	if client.callCount == calls {
		t.Error("after TTL expires Current should trigger a refresh")
	}
}

func TestDetectorInvalidateForcesRefresh(t *testing.T) {
	conns := []connection.Connection{{ID: "a", Type: connection.TypeEmby, Enabled: true}}
	client := &fakeClient{}
	d, _ := buildDetector(t, conns, map[string]peerClient{"a": client}, nil)

	d.Current(context.Background())
	calls := client.callCount
	d.Invalidate()
	d.Current(context.Background())
	if client.callCount == calls {
		t.Error("Invalidate should have forced a re-query")
	}
}

func TestDetectorDisabledConnectionSkipsRemoteCall(t *testing.T) {
	conns := []connection.Connection{{ID: "a", Type: connection.TypeEmby, Enabled: false}}
	client := &fakeClient{}
	d, _ := buildDetector(t, conns, map[string]peerClient{"a": client}, nil)

	l := d.Refresh(context.Background())
	if client.callCount != 0 {
		t.Errorf("disabled connection should not trigger remote calls, got %d", client.callCount)
	}
	if len(l.Connections) != 1 || l.Connections[0].Enabled {
		t.Error("disabled connection should still appear in ledger with Enabled=false")
	}
}

func TestDetectorUnsupportedTypeReturnsCheckErr(t *testing.T) {
	conns := []connection.Connection{{ID: "a", Type: "unknown", Enabled: true}}
	// Factory returns nil for unsupported types.
	factory := func(c connection.Connection) (peerClient, pathProvider) { return nil, nil }
	bus := event.NewBus(newLogger(), 16)
	d := newDetectorWithClients(&fakeRepo{conns: conns}, bus, newLogger(), factory)

	l := d.Refresh(context.Background())
	if l.Connections[0].CheckErr == "" {
		t.Error("unsupported type should populate CheckErr")
	}
}

func TestNewForTestProducesCleanLedger(t *testing.T) {
	d := NewForTest(&fakeRepo{conns: []connection.Connection{
		{ID: "a", Type: connection.TypeEmby, Enabled: true},
	}}, newLogger())
	l := d.Refresh(context.Background())
	if l.BannerState() != "clean" {
		t.Errorf("NewForTest should yield clean ledger, got %s", l.BannerState())
	}
}

func TestNormalizePathsCleansAndDedupes(t *testing.T) {
	out := normalizePaths([]string{"/mnt/music/", "/mnt/music", "/mnt//music", "", "  ", "/other"})
	if len(out) != 2 {
		t.Fatalf("want 2 unique paths, got %v", out)
	}
	if out[0] != "/mnt/music" {
		t.Errorf("want first=/mnt/music, got %q", out[0])
	}
}

func TestIsAncestorRespectsSeparators(t *testing.T) {
	cases := []struct {
		parent, child string
		want          bool
	}{
		{"/mnt/music", "/mnt/music/rock", true},
		{"/mnt/music", "/mnt/music", false},
		{"/mnt/music", "/mnt/musicals", false},
		{"", "/anything", false},
	}
	for _, tc := range cases {
		if got := isAncestor(tc.parent, tc.child); got != tc.want {
			t.Errorf("isAncestor(%q,%q) = %v, want %v", tc.parent, tc.child, got, tc.want)
		}
	}
}

func TestLedgerHasAnyConflictRespectsManaged(t *testing.T) {
	managed := ConnectionState{Enabled: true, ImageWriteback: true, ManageServerFiles: true}
	if managed.HasAnyConflict() {
		t.Error("managed connection should report no conflict")
	}
	unmanaged := ConnectionState{Enabled: true, ImageWriteback: true}
	if !unmanaged.HasAnyConflict() {
		t.Error("unmanaged image writeback should conflict")
	}
}

// TestLedgerHasAnyConflictIgnoresDisabled guards the contract that disabled
// connections never contribute to gating even if stale saver flags remain
// populated from a prior probe.
func TestLedgerHasAnyConflictIgnoresDisabled(t *testing.T) {
	disabled := ConnectionState{Enabled: false, ImageWriteback: true, NFOWriteback: true}
	if disabled.HasAnyConflict() {
		t.Error("disabled connection should report no conflict even with stale writeback flags")
	}
}

// TestLedgerAnyConflictFailsClosedOnCheckErr guards the contract that an
// enabled unmanaged connection with an unknown saver state (CheckErr set)
// must gate writes rather than silently passing. Symmetric across both
// image and NFO axes.
func TestLedgerAnyConflictFailsClosedOnCheckErr(t *testing.T) {
	l := Ledger{Connections: []ConnectionState{{
		ConnectionID: "a",
		Enabled:      true,
		CheckErr:     "nfo check: dial tcp: connection refused",
	}}}
	if !l.AnyImageConflict() {
		t.Error("AnyImageConflict must return true when CheckErr is set on an enabled unmanaged connection")
	}
	if !l.AnyNFOConflict() {
		t.Error("AnyNFOConflict must return true when CheckErr is set on an enabled unmanaged connection")
	}

	// Managed connection with CheckErr does not contribute; Stillwater has
	// disabled the peer's savers so a failed probe does not matter.
	lManaged := Ledger{Connections: []ConnectionState{{
		ConnectionID:      "a",
		Enabled:           true,
		ManageServerFiles: true,
		CheckErr:          "probe failed",
	}}}
	if lManaged.AnyImageConflict() || lManaged.AnyNFOConflict() {
		t.Error("managed connection should not gate on CheckErr")
	}
}

func TestLedgerMarshalRoundTrip(t *testing.T) {
	l := Ledger{Connections: []ConnectionState{{ConnectionID: "x"}}}
	buf, err := l.Marshal()
	if err != nil {
		t.Fatalf("marshal err: %v", err)
	}
	if len(buf) == 0 {
		t.Error("empty body")
	}
}

func TestDetectorAutoReDisablesManagedDrift(t *testing.T) {
	// Managed connection whose peer is reporting savers back on. Detector
	// should call DisableFileWriteBack and the post-disable re-check (which
	// the fakeClient satisfies via clearOnDisable) should scrub the state.
	conns := []connection.Connection{
		{ID: "a", Name: "Emby", Type: connection.TypeEmby, Enabled: true, FeatureManageServerFiles: true},
	}
	client := &fakeClient{nfo: true, image: true, libName: "Music", clearOnDisable: true}
	d, _ := buildDetector(t, conns, map[string]peerClient{"a": client}, nil)
	l := d.Refresh(context.Background())

	if client.disableCalls != 1 {
		t.Errorf("DisableFileWriteBack should have fired once, got %d calls", client.disableCalls)
	}
	if l.Connections[0].NFOWriteback || l.Connections[0].ImageWriteback {
		t.Errorf("ledger should reflect the post-disable state, got %+v", l.Connections[0])
	}
}

func TestDetectorAutoReDisableNoopForUnmanaged(t *testing.T) {
	// Unmanaged connection with savers on -- must NOT call DisableFileWriteBack.
	conns := []connection.Connection{
		{ID: "a", Name: "Emby", Type: connection.TypeEmby, Enabled: true},
	}
	client := &fakeClient{nfo: true, image: true}
	d, _ := buildDetector(t, conns, map[string]peerClient{"a": client}, nil)
	d.Refresh(context.Background())
	if client.disableCalls != 0 {
		t.Errorf("unmanaged connection must not trigger disable, got %d calls", client.disableCalls)
	}
}

func TestDetectorSurfacesCheckErrors(t *testing.T) {
	conns := []connection.Connection{{ID: "a", Type: connection.TypeEmby, Enabled: true}}
	client := &fakeClient{nfoErr: errors.New("boom")}
	d, _ := buildDetector(t, conns, map[string]peerClient{"a": client}, nil)
	l := d.Refresh(context.Background())

	if l.Connections[0].CheckErr == "" {
		t.Error("check error should propagate to ConnectionState")
	}
}

// errRepo returns a fixed error from List so we can exercise the
// cold-start Refresh fail-closed path.
type errRepo struct{ err error }

func (e *errRepo) List(context.Context) ([]connection.Connection, error) {
	return nil, e.err
}

func TestDetectorColdStartRepoErrorFailsClosed(t *testing.T) {
	// If the very first refresh cannot list connections (e.g. DB locked
	// at boot), Refresh must synthesize a fail-closed sentinel so every
	// write gate reports conflict. A silent empty ledger here would
	// reopen every write on startup before any peer check has ever run.
	bus := event.NewBus(newLogger(), 16)
	factory := func(c connection.Connection) (peerClient, pathProvider) { return &fakeClient{}, nil }
	d := newDetectorWithClients(&errRepo{err: errors.New("db locked")}, bus, newLogger(), factory)

	l := d.Refresh(context.Background())

	if len(l.Connections) != 1 {
		t.Fatalf("want 1 sentinel connection, got %d", len(l.Connections))
	}
	if l.Connections[0].CheckErr == "" {
		t.Error("sentinel must populate CheckErr so AnyImageConflict / AnyNFOConflict fail closed")
	}
	if !l.AnyImageConflict() || !l.AnyNFOConflict() {
		t.Error("sentinel must force both axes conflicted")
	}
}

// errPaths returns a fixed error from MusicLibraryPaths. The detector
// must surface that via CheckErr, otherwise round-trip detection is
// silently disabled for that connection.
type errPaths struct{ err error }

func (e *errPaths) MusicLibraryPaths(context.Context) ([]string, error) {
	return nil, e.err
}

func TestDetectorSurfacesPathProviderErrorAsCheckErr(t *testing.T) {
	conns := []connection.Connection{
		{ID: "e", Name: "Emby", Type: connection.TypeEmby, Enabled: true},
	}
	client := &fakeClient{}
	paths := map[string]pathProvider{"e": &errPaths{err: errors.New("peer 502")}}
	d, _ := buildDetector(t, conns, map[string]peerClient{"e": client}, paths)

	l := d.Refresh(context.Background())

	if len(l.Connections) != 1 {
		t.Fatalf("want 1 connection, got %d", len(l.Connections))
	}
	if l.Connections[0].CheckErr == "" {
		t.Error("paths error must surface via CheckErr, not be silently swallowed")
	}
}

// postDisableErrClient drives the managed-drift auto-disable path into
// the post-disable recheck, where we want to assert that a recheck error
// gets surfaced via CheckErr rather than retaining the pre-disable flag.
type postDisableErrClient struct {
	calls int
	mu    sync.Mutex
	// first two Check* calls (the pre-disable probes) report writeback=true
	// so managed-drift logic engages; subsequent calls (the post-disable
	// recheck) return errors so the CheckErr surfacing branch runs.
}

func (p *postDisableErrClient) CheckNFOWriterEnabled(context.Context) (bool, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
		return true, "lib", nil
	}
	return false, "", errors.New("peer down after disable (nfo)")
}

func (p *postDisableErrClient) CheckImageSaverEnabled(context.Context) (bool, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 2 {
		return true, "lib", nil
	}
	return false, "", errors.New("peer down after disable (image)")
}

func (p *postDisableErrClient) DisableFileWriteBack(context.Context) error { return nil }

func TestDetectorSurfacesPostDisableRecheckErrors(t *testing.T) {
	// Managed connection reporting savers back on. DisableFileWriteBack
	// succeeds, but the post-disable recheck can't reach the peer. The
	// detector must capture that via CheckErr so the banner does not
	// keep reporting stale (pre-disable) writeback state.
	conns := []connection.Connection{
		{ID: "e", Name: "Emby", Type: connection.TypeEmby, Enabled: true, FeatureManageServerFiles: true},
	}
	client := &postDisableErrClient{}
	d, _ := buildDetector(t, conns, map[string]peerClient{"e": client}, nil)

	l := d.Refresh(context.Background())

	if len(l.Connections) != 1 {
		t.Fatalf("want 1 connection, got %d", len(l.Connections))
	}
	if l.Connections[0].CheckErr == "" {
		t.Error("post-disable recheck error must surface via CheckErr")
	}
}
