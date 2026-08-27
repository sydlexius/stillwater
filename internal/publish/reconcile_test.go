package publish

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// --- test doubles for reconciler ---

// reconcilePlatformLister satisfies artistPlatformLister with configurable
// return values for the reconciler-facing methods.
type reconcilePlatformLister struct {
	artistIDs []string
	listErr   error
	ids       map[string][]artist.PlatformID // artist_id -> platform IDs
}

func (r *reconcilePlatformLister) GetPlatformIDs(_ context.Context, artistID string) ([]artist.PlatformID, error) {
	return r.ids[artistID], nil
}

func (r *reconcilePlatformLister) ListMembersByArtistID(_ context.Context, _ string) ([]artist.BandMember, error) {
	return nil, nil
}

func (r *reconcilePlatformLister) ListArtistsWithPlatformMappings(_ context.Context) ([]string, error) {
	return r.artistIDs, r.listErr
}

func (r *reconcilePlatformLister) SetPlatformIDStable(_ context.Context, _, _, _ string) (artist.PlatformIDStableOutcome, error) {
	return artist.PlatformIDStableOutcome{}, nil
}

func (r *reconcilePlatformLister) SetPlatformID(_ context.Context, _, _, _ string) error { return nil }

func (r *reconcilePlatformLister) DeletePlatformID(_ context.Context, _, _ string) error { return nil }

// fakeArtistGetter returns artists from a map keyed by ID.
type fakeArtistGetter struct {
	artists map[string]*artist.Artist
	err     error
}

func (f *fakeArtistGetter) GetByID(_ context.Context, id string, _ ...artist.HydrateOpts) (*artist.Artist, error) {
	if f.err != nil {
		return nil, f.err
	}
	a, ok := f.artists[id]
	if !ok {
		return nil, errors.New("artist not found: " + id)
	}
	return a, nil
}

// allowGate always permits writes.
type allowGate struct{}

func (allowGate) AllowImageWrite(_ context.Context) error { return nil }

// denyGate always blocks writes.
type denyGate struct{}

func (denyGate) AllowImageWrite(_ context.Context) error {
	return errors.New("conflict: write blocked")
}

// newStateAndUploadServer returns an httptest.Server that:
//   - serves stateJSON for GET requests to paths containing "/Items/"
//   - counts POST requests to paths containing "/Images/" in hits
//   - returns 204 for everything else
func newStateAndUploadServer(stateJSON string, hits *uploadHits) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/Items/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, stateJSON)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/Images/") {
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				http.Error(w, "failed to read request body", http.StatusBadRequest)
				return
			}
			ct := r.Header.Get("Content-Type")
			hits.mu.Lock()
			hits.contentTypes = append(hits.contentTypes, ct)
			hits.bodySizes = append(hits.bodySizes, len(body))
			hits.mu.Unlock()
			hits.uploads.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// --- ReconcileArtworkToPlatforms unit tests ---

// TestReconcileArtworkToPlatforms_NilReceiver verifies the nil guard.
func TestReconcileArtworkToPlatforms_NilReceiver(t *testing.T) {
	var p *Publisher
	p.ReconcileArtworkToPlatforms(context.Background())
}

// TestReconcileArtworkToPlatforms_NoArtists verifies a no-op when there are no
// artists with platform mappings.
func TestReconcileArtworkToPlatforms_NoArtists(t *testing.T) {
	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: &reconcilePlatformLister{artistIDs: nil},
	})
	p.ReconcileArtworkToPlatforms(context.Background())
}

// TestReconcileArtworkToPlatforms_ListError verifies that a list error is
// logged and the run aborts cleanly.
func TestReconcileArtworkToPlatforms_ListError(t *testing.T) {
	p := New(Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: &reconcilePlatformLister{
			listErr: errors.New("db boom"),
		},
	})
	p.ReconcileArtworkToPlatforms(context.Background())
}

// TestReconcileArtworkToPlatforms_GateBlocked verifies that a blocked gate
// causes all artists to be skipped before any upload attempt.
func TestReconcileArtworkToPlatforms_GateBlocked(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "fanart.jpg")

	hits := &uploadHits{}
	srv := newStateAndUploadServer(`{"Id":"p1","ImageTags":{},"BackdropImageTags":[]}`, hits)
	defer srv.Close()

	lister := &reconcilePlatformLister{
		artistIDs: []string{"a1"},
		ids: map[string][]artist.PlatformID{
			"a1": {{ArtistID: "a1", ConnectionID: "c", PlatformArtistID: "p1"}},
		},
	}
	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: lister,
		ArtistGetter:  &fakeArtistGetter{artists: map[string]*artist.Artist{"a1": {ID: "a1", Path: dir, Name: "Test"}}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok",
				Emby: &connection.EmbyConfig{FeatureImageWrite: true, PlatformUserID: "u1"}},
		}},
		ImageWriteGate: denyGate{},
	})
	p.ReconcileArtworkToPlatforms(context.Background())

	time.Sleep(50 * time.Millisecond)
	if n := hits.uploads.Load(); n != 0 {
		t.Errorf("expected 0 uploads when gate is blocked; got %d", n)
	}
}

// TestReconcileArtworkToPlatforms_NoArtistGetter verifies that a missing
// ArtistGetter causes the artist to be skipped without panic.
func TestReconcileArtworkToPlatforms_NoArtistGetter(t *testing.T) {
	lister := &reconcilePlatformLister{artistIDs: []string{"a1"}}
	p := New(Deps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService:  lister,
		ImageWriteGate: allowGate{},
		// ArtistGetter deliberately nil
	})
	p.ReconcileArtworkToPlatforms(context.Background())
}

// TestReconcileArtworkToPlatforms_SkipsFeatureImageWriteOff verifies that
// connections with FeatureImageWrite=false are not targeted.
func TestReconcileArtworkToPlatforms_SkipsFeatureImageWriteOff(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "fanart.jpg")

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	lister := &reconcilePlatformLister{
		artistIDs: []string{"a1"},
		ids: map[string][]artist.PlatformID{
			"a1": {{ArtistID: "a1", ConnectionID: "c", PlatformArtistID: "p1"}},
		},
	}
	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: lister,
		ArtistGetter:  &fakeArtistGetter{artists: map[string]*artist.Artist{"a1": {ID: "a1", Path: dir}}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok",
				Emby: &connection.EmbyConfig{FeatureImageWrite: false, PlatformUserID: "u1"}},
		}},
		ImageWriteGate: allowGate{},
	})
	p.ReconcileArtworkToPlatforms(context.Background())

	time.Sleep(50 * time.Millisecond)
	if n := hits.uploads.Load(); n != 0 {
		t.Errorf("expected 0 uploads when FeatureImageWrite=false; got %d", n)
	}
}

// TestReconcileArtworkToPlatforms_UploadsMissingFanart verifies that fanart
// present locally but absent on the platform triggers an upload.
func TestReconcileArtworkToPlatforms_UploadsMissingFanart(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "fanart.jpg")
	seedJPG(t, dir, "fanart1.jpg")

	hits := &uploadHits{}
	// Platform has 0 backdrops - fanart is missing.
	stateJSON := `{"Id":"p1","ImageTags":{},"BackdropImageTags":[]}`
	srv := newStateAndUploadServer(stateJSON, hits)
	defer srv.Close()

	lister := &reconcilePlatformLister{
		artistIDs: []string{"a1"},
		ids: map[string][]artist.PlatformID{
			"a1": {{ArtistID: "a1", ConnectionID: "c", PlatformArtistID: "p1"}},
		},
	}
	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: lister,
		ArtistGetter:  &fakeArtistGetter{artists: map[string]*artist.Artist{"a1": {ID: "a1", Path: dir, Name: "Test"}}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok",
				Emby: &connection.EmbyConfig{FeatureImageWrite: true, PlatformUserID: "u1"}},
		}},
		ImageWriteGate: allowGate{},
	})
	p.ReconcileArtworkToPlatforms(context.Background())

	// Both local fanart files should be uploaded.
	waitForUploads(t, hits, 2)
}

// TestReconcileArtworkToPlatforms_SkipsWhenPlatformCurrent verifies that no
// upload occurs when the platform already has at least as many backdrops as
// the local artist directory.
func TestReconcileArtworkToPlatforms_SkipsWhenPlatformCurrent(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "fanart.jpg") // 1 local fanart

	hits := &uploadHits{}
	// Platform already has 1 backdrop (matches local) and Primary tag.
	stateJSON := `{"Id":"p1","ImageTags":{"Primary":"tag"},"BackdropImageTags":["tag"]}`
	srv := newStateAndUploadServer(stateJSON, hits)
	defer srv.Close()

	lister := &reconcilePlatformLister{
		artistIDs: []string{"a1"},
		ids: map[string][]artist.PlatformID{
			"a1": {{ArtistID: "a1", ConnectionID: "c", PlatformArtistID: "p1"}},
		},
	}
	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: lister,
		ArtistGetter:  &fakeArtistGetter{artists: map[string]*artist.Artist{"a1": {ID: "a1", Path: dir, Name: "Test"}}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok",
				Emby: &connection.EmbyConfig{FeatureImageWrite: true, PlatformUserID: "u1"}},
		}},
		ImageWriteGate: allowGate{},
	})
	p.ReconcileArtworkToPlatforms(context.Background())

	time.Sleep(150 * time.Millisecond)
	if n := hits.uploads.Load(); n != 0 {
		t.Errorf("expected 0 uploads when platform is current; got %d", n)
	}
}

// --- platformHasImageType unit tests ---

// TestPlatformHasImageType covers every branch of the helper.
func TestPlatformHasImageType(t *testing.T) {
	state := &connection.ArtistPlatformState{
		HasThumb:  true,
		HasLogo:   false,
		HasBanner: true,
	}
	cases := []struct {
		imageType string
		want      bool
	}{
		{"thumb", true},
		{"logo", false},
		{"banner", true},
		{"unknown", false},
	}
	for _, tc := range cases {
		got := platformHasImageType(state, tc.imageType)
		if got != tc.want {
			t.Errorf("platformHasImageType(%q) = %v, want %v", tc.imageType, got, tc.want)
		}
	}
}

// --- StartArtworkReconciler tests ---

// TestStartArtworkReconciler_StopsOnContextCancel verifies the ticker goroutine
// exits cleanly when the context is canceled.
func TestStartArtworkReconciler_StopsOnContextCancel(t *testing.T) {
	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: &reconcilePlatformLister{artistIDs: nil},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.StartArtworkReconciler(ctx, 50*time.Millisecond, 5*time.Millisecond)
	}()

	// Let one interval fire, then cancel.
	time.Sleep(80 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartArtworkReconciler did not stop within 2s after context cancel")
	}
}

// TestStartArtworkReconciler_CancelBeforeStartup verifies that canceling the
// context during the startup delay causes an immediate clean stop.
func TestStartArtworkReconciler_CancelBeforeStartup(t *testing.T) {
	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: &reconcilePlatformLister{artistIDs: nil},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.StartArtworkReconciler(ctx, 100*time.Millisecond, 500*time.Millisecond)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartArtworkReconciler did not stop within 2s after pre-cancel")
	}
}

// TestRunReconcileWithRecover_PanicSafety verifies that a panic inside
// ReconcileArtworkToPlatforms does not escape runReconcileWithRecover.
func TestRunReconcileWithRecover_PanicSafety(t *testing.T) {
	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: &panicPlatformLister{},
	})
	p.runReconcileWithRecover(context.Background())
}

// panicPlatformLister panics on ListArtistsWithPlatformMappings to exercise
// the panic-recovery path in runReconcileWithRecover.
type panicPlatformLister struct{}

func (panicPlatformLister) GetPlatformIDs(_ context.Context, _ string) ([]artist.PlatformID, error) {
	return nil, nil
}

func (panicPlatformLister) ListMembersByArtistID(_ context.Context, _ string) ([]artist.BandMember, error) {

	return nil, nil
}

func (panicPlatformLister) ListArtistsWithPlatformMappings(_ context.Context) ([]string, error) {
	panic("deliberate test panic in ListArtistsWithPlatformMappings")
}

func (panicPlatformLister) SetPlatformIDStable(_ context.Context, _, _, _ string) (artist.PlatformIDStableOutcome, error) {
	return artist.PlatformIDStableOutcome{}, nil
}

func (panicPlatformLister) SetPlatformID(_ context.Context, _, _, _ string) error { return nil }

func (panicPlatformLister) DeletePlatformID(_ context.Context, _, _ string) error { return nil }

// --- additional coverage for uncovered branches ---

// errPlatformListerIDs returns an error from GetPlatformIDs to exercise the
// skippedNoPIDs path in ReconcileArtworkToPlatforms.
type errPlatformListerIDs struct {
	artistIDs []string
}

func (e *errPlatformListerIDs) GetPlatformIDs(_ context.Context, _ string) ([]artist.PlatformID, error) {
	return nil, errors.New("platform IDs DB error")
}

func (e *errPlatformListerIDs) ListMembersByArtistID(_ context.Context, _ string) ([]artist.BandMember, error) {
	return nil, nil
}

func (e *errPlatformListerIDs) ListArtistsWithPlatformMappings(_ context.Context) ([]string, error) {
	return e.artistIDs, nil
}

func (e *errPlatformListerIDs) SetPlatformIDStable(_ context.Context, _, _, _ string) (artist.PlatformIDStableOutcome, error) {
	return artist.PlatformIDStableOutcome{}, nil
}

func (e *errPlatformListerIDs) SetPlatformID(_ context.Context, _, _, _ string) error { return nil }

func (e *errPlatformListerIDs) DeletePlatformID(_ context.Context, _, _ string) error { return nil }

// TestReconcileArtworkToPlatforms_NilGateWarns verifies that a nil imageWriteGate
// logs a one-time Warn at reconcile start rather than silently bypassing gating.
func TestReconcileArtworkToPlatforms_NilGateWarns(t *testing.T) {
	lister := &reconcilePlatformLister{artistIDs: []string{"a1"}}
	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: lister,
		// ImageWriteGate deliberately nil; ArtistGetter nil → skips after warn
	})
	p.ReconcileArtworkToPlatforms(context.Background()) // must not panic
}

// nilConnectionGetter always resolves GetByID to (nil, nil), simulating a
// connection that no longer exists without producing an error. It exercises
// the `conn == nil` guard in detectMissingArtwork.
type nilConnectionGetter struct{}

func (nilConnectionGetter) GetByID(_ context.Context, _ string) (*connection.Connection, error) {
	return nil, nil
}

func (nilConnectionGetter) ListByType(_ context.Context, _ string) ([]connection.Connection, error) {
	return nil, nil
}

// TestReconcileArtworkToPlatforms_NilConnection verifies that when the
// connection getter resolves a platform ID's connection to (nil, nil), the
// reconciler logs a missing-connection warning, skips that connection, and
// performs no uploads without panicking.
func TestReconcileArtworkToPlatforms_NilConnection(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "fanart.jpg")

	hits := &uploadHits{}
	srv := newStateAndUploadServer(`{"Id":"p1","ImageTags":{},"BackdropImageTags":[]}`, hits)
	defer srv.Close()

	lister := &reconcilePlatformLister{
		artistIDs: []string{"a1"},
		ids: map[string][]artist.PlatformID{
			"a1": {{ArtistID: "a1", ConnectionID: "missing", PlatformArtistID: "p1"}},
		},
	}
	p := New(Deps{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService:     lister,
		ArtistGetter:      &fakeArtistGetter{artists: map[string]*artist.Artist{"a1": {ID: "a1", Path: dir, Name: "Test"}}},
		ConnectionService: nilConnectionGetter{},
		ImageWriteGate:    allowGate{},
	})
	p.ReconcileArtworkToPlatforms(context.Background()) // must not panic

	time.Sleep(50 * time.Millisecond)
	if n := hits.uploads.Load(); n != 0 {
		t.Errorf("expected 0 uploads when connection resolves to nil; got %d", n)
	}
}

// TestStartArtworkReconciler_NonPositiveInterval verifies that a non-positive
// interval causes StartArtworkReconciler to return immediately (before any
// ticker is created) without panicking or hanging.
func TestStartArtworkReconciler_NonPositiveInterval(t *testing.T) {
	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: &reconcilePlatformLister{artistIDs: nil},
	})

	// Each call must return synchronously; if it hangs the test times out.
	p.StartArtworkReconciler(context.Background(), 0, time.Millisecond)
	p.StartArtworkReconciler(context.Background(), -time.Second, time.Millisecond)
}

// TestReconcileArtworkToPlatforms_ArtistLoadError verifies that an artist
// getter error increments skippedLoadErr and continues to the next artist.
func TestReconcileArtworkToPlatforms_ArtistLoadError(t *testing.T) {
	lister := &reconcilePlatformLister{artistIDs: []string{"a1"}}
	p := New(Deps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService:  lister,
		ArtistGetter:   &fakeArtistGetter{err: errors.New("DB gone")},
		ImageWriteGate: allowGate{},
	})
	p.ReconcileArtworkToPlatforms(context.Background()) // must not panic
}

// TestReconcileArtworkToPlatforms_PlatformIDsError verifies that a
// GetPlatformIDs error increments skippedNoPIDs and continues cleanly.
func TestReconcileArtworkToPlatforms_PlatformIDsError(t *testing.T) {
	dir := t.TempDir()
	lister := &errPlatformListerIDs{artistIDs: []string{"a1"}}
	p := New(Deps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService:  lister,
		ArtistGetter:   &fakeArtistGetter{artists: map[string]*artist.Artist{"a1": {ID: "a1", Path: dir}}},
		ImageWriteGate: allowGate{},
	})
	p.ReconcileArtworkToPlatforms(context.Background()) // must not panic
}

// TestAccumulateNeeds_DiscoverFanartError verifies that a DiscoverFanart
// failure (non-existent directory) logs a Warn and does not set fanart=true.
func TestAccumulateNeeds_DiscoverFanartError(t *testing.T) {
	dir := "/nonexistent-dir-for-test-reconcile"
	state := &connection.ArtistPlatformState{BackdropCount: 0}
	needs := &artworkNeeds{}

	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: &reconcilePlatformLister{},
	})
	p.accumulateNeeds(context.Background(), "a1", dir, state, needs)

	if needs.fanart {
		t.Error("expected fanart flag false when DiscoverFanart fails; got true")
	}
}

// TestNewArtistStateGetter_Jellyfin verifies the Jellyfin branch of the
// factory returns a non-nil getter (exercising the TypeJellyfin case).
func TestNewArtistStateGetter_Jellyfin(t *testing.T) {
	conn := &connection.Connection{
		Type:     connection.TypeJellyfin,
		URL:      "http://jf:8096",
		APIKey:   "key",
		Jellyfin: &connection.JellyfinConfig{PlatformUserID: "u1"},
	}
	got := newArtistStateGetter(conn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got == nil {
		t.Error("expected non-nil ArtistStateGetter for Jellyfin; got nil")
	}
}

// TestSetImageWriteGate_Wires verifies SetImageWriteGate stores the gate and
// that a nil receiver is safe.
func TestSetImageWriteGate_Wires(t *testing.T) {
	p := New(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: &reconcilePlatformLister{},
	})
	p.SetImageWriteGate(allowGate{})
	if p.imageWriteGate == nil {
		t.Error("expected imageWriteGate non-nil after SetImageWriteGate; got nil")
	}

	var nilP *Publisher
	nilP.SetImageWriteGate(allowGate{}) // must not panic
}

// TestSyncImageToPlatforms_IgnoresFeatureImageWrite verifies that user-initiated
// image sync pushes to ALL enabled connections regardless of FeatureImageWrite.
// The FeatureImageWrite gate applies only to the background reconciler path.
func TestSyncImageToPlatforms_IgnoresFeatureImageWrite(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "folder.jpg")

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-on", PlatformArtistID: "p1"},
			{ArtistID: "a1", ConnectionID: "c-off", PlatformArtistID: "p2"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-on":  {ID: "c-on", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
			"c-off": {ID: "c-off", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: false}},
		}},
	})
	warnings := p.SyncImageToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir, Name: "X"}, "thumb")
	if len(warnings) != 0 {
		t.Errorf("expected no warnings; got %v", warnings)
	}
	waitForUploads(t, hits, 2) // both connections receive the user-initiated push
}

// TestSyncAllFanartToPlatforms_IgnoresFeatureImageWrite mirrors the above for
// the fanart upload path: user-initiated sync pushes to all enabled connections.
func TestSyncAllFanartToPlatforms_IgnoresFeatureImageWrite(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "fanart.jpg")

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c-on", PlatformArtistID: "p1"},
			{ArtistID: "a1", ConnectionID: "c-off", PlatformArtistID: "p2"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c-on":  {ID: "c-on", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
			"c-off": {ID: "c-off", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok", Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: false}},
		}},
	})
	warnings := p.SyncAllFanartToPlatforms(context.Background(), &artist.Artist{ID: "a1", Path: dir, Name: "X"})
	if len(warnings) != 0 {
		t.Errorf("expected no warnings; got %v", warnings)
	}
	waitForUploads(t, hits, 2) // both connections receive the user-initiated push
}
