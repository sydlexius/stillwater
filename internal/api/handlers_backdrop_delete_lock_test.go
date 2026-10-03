package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// #3138: the two platform delete paths take the per-target backdrop lock the
// perceptual prune and phash repair hold. These tests hold that lock from the
// test and watch whether the platform's DELETE arrives.
//
// "Not yet issued" is a short bounded wait (300ms), the same device the
// neighboring lock test TestHandlePushImages_FanartWaitsForBackdropTargetLock
// uses; everything else is channel-driven with generous timeouts.
const (
	lockHeldWait   = 300 * time.Millisecond
	lockCompleteBy = 5 * time.Second
)

// runDeleteUnderLock holds the target lock, starts `start` (a platform delete)
// in a goroutine, and checks the DELETE is held back (wantBlocked) or not,
// then releases the lock and checks the delete completes.
func runDeleteUnderLock(t *testing.T, wantBlocked bool, lock func() func(), start func(), hits <-chan struct{}) {
	t.Helper()
	unlock := lock()
	released := false
	defer func() {
		if !released {
			unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		start()
	}()
	if wantBlocked {
		select {
		case <-hits:
			t.Fatal("platform delete issued while the target lock was held")
		case <-done:
			t.Fatal("delete completed while the target lock was held")
		case <-time.After(lockHeldWait):
		}
		released = true
		unlock()
		select {
		case <-hits:
		case <-time.After(lockCompleteBy):
			t.Fatal("platform delete never issued after the lock was released")
		}
	} else {
		select {
		case <-hits:
		case <-time.After(lockCompleteBy):
			t.Fatal("non-backdrop delete blocked on the backdrop target lock")
		}
	}
	select {
	case <-done:
	case <-time.After(lockCompleteBy):
		t.Fatal("delete did not complete")
	}
}

func newDeleteHitServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	hits := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			hits <- struct{}{}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestDeleteImageFromPlatforms_BackdropWaitsForTargetLock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		imageType   string
		wantBlocked bool
	}{{"fanart", true}, {"logo", false}, {"thumb", false}, {"banner", false}} {
		t.Run(tc.imageType, func(t *testing.T) {
			t.Parallel()
			srv, hits := newDeleteHitServer(t)
			r, artistSvc := testRouterWithPlatform(t)
			a := addTestArtist(t, artistSvc, "Locked Delete "+tc.imageType)
			addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
			if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-del-1"); err != nil {
				t.Fatalf("SetPlatformID: %v", err)
			}
			runDeleteUnderLock(t, tc.wantBlocked,
				func() func() { return r.publisher.LockBackdropTarget("conn-emby", "emby-del-1") },
				func() { r.deleteImageFromPlatforms(context.Background(), a, tc.imageType) },
				hits)
		})
	}
}

func TestHandleDeletePushImage_BackdropWaitsForTargetLock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		imageType   string
		wantBlocked bool
	}{{"fanart", true}, {"logo", false}, {"thumb", false}, {"banner", false}} {
		t.Run(tc.imageType, func(t *testing.T) {
			t.Parallel()
			srv, hits := newDeleteHitServer(t)
			r, artistSvc := testRouter(t)
			a := addTestArtist(t, artistSvc, "Locked Push Delete "+tc.imageType)
			addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
			var code int
			runDeleteUnderLock(t, tc.wantBlocked,
				func() func() { return r.publisher.LockBackdropTarget("conn-emby", "emby-del-2") },
				func() {
					body := `{"connection_id":"conn-emby","platform_artist_id":"emby-del-2"}`
					req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/"+tc.imageType, strings.NewReader(body))
					req.SetPathValue("id", a.ID)
					req.SetPathValue("type", tc.imageType)
					w := httptest.NewRecorder()
					r.handleDeletePushImage(w, req)
					code = w.Code
				},
				hits)
			if code != http.StatusOK {
				t.Errorf("status = %d, want 200", code)
			}
		})
	}
}

// #3138: with no publisher wired there is no lock to take. A fanart delete is
// refused (never issued unlocked); any other type needs no lock and must still
// reach the platform.
func TestDeleteImageFromPlatforms_NilPublisher(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		imageType string
		wantHit   bool
	}{{"fanart", false}, {"logo", true}, {"thumb", true}, {"banner", true}} {
		t.Run(tc.imageType, func(t *testing.T) {
			t.Parallel()
			srv, hits := newDeleteHitServer(t)
			r, artistSvc := testRouterWithPlatform(t)
			a := addTestArtist(t, artistSvc, "Nil Pub Delete "+tc.imageType)
			addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
			if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-nil-1"); err != nil {
				t.Fatalf("SetPlatformID: %v", err)
			}
			r.publisher = nil
			warnings := r.deleteImageFromPlatforms(context.Background(), a, tc.imageType)
			gotHit := len(hits) > 0
			if gotHit != tc.wantHit {
				t.Errorf("platform delete issued = %v, want %v", gotHit, tc.wantHit)
			}
			if wantWarn := !tc.wantHit; (len(warnings) > 0) != wantWarn {
				t.Errorf("warnings = %v, want a warning = %v", warnings, wantWarn)
			}
		})
	}
}

func TestHandleDeletePushImage_NilPublisher(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		imageType string
		wantHit   bool
		wantCode  int
	}{{"fanart", false, http.StatusInternalServerError}, {"logo", true, http.StatusOK}, {"thumb", true, http.StatusOK}, {"banner", true, http.StatusOK}} {
		t.Run(tc.imageType, func(t *testing.T) {
			t.Parallel()
			srv, hits := newDeleteHitServer(t)
			r, artistSvc := testRouter(t)
			a := addTestArtist(t, artistSvc, "Nil Pub Push "+tc.imageType)
			addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
			r.publisher = nil
			body := `{"connection_id":"conn-emby","platform_artist_id":"emby-nil-2"}`
			req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/"+tc.imageType, strings.NewReader(body))
			req.SetPathValue("id", a.ID)
			req.SetPathValue("type", tc.imageType)
			w := httptest.NewRecorder()
			r.handleDeletePushImage(w, req)
			if w.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tc.wantCode)
			}
			if gotHit := len(hits) > 0; gotHit != tc.wantHit {
				t.Errorf("platform delete issued = %v, want %v", gotHit, tc.wantHit)
			}
		})
	}
}

// A failing platform delete still releases the lock and reports the failure.
func TestHandleDeletePushImage_FanartPlatformFailureReleasesLock(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Fail Push Delete")
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
	body := `{"connection_id":"conn-emby","platform_artist_id":"emby-fail-1"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/fanart", strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "fanart")
	w := httptest.NewRecorder()
	r.handleDeletePushImage(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	got := make(chan struct{})
	go func() {
		r.publisher.LockBackdropTarget("conn-emby", "emby-fail-1")()
		close(got)
	}()
	select {
	case <-got:
	case <-time.After(lockCompleteBy):
		t.Fatal("target lock still held after a failed platform delete")
	}
}

// newBlockingDeleteServer parks every DELETE inside the handler: it signals
// arrived, then waits for release (or test end), then answers status.
func newBlockingDeleteServer(t *testing.T, status int) (srv *httptest.Server, arrived <-chan struct{}, release func()) {
	t.Helper()
	arr, rel := make(chan struct{}, 4), make(chan struct{})
	var once sync.Once
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			arr <- struct{}{}
			<-rel
		}
		w.WriteHeader(status)
	}))
	releaseFn := func() { once.Do(func() { close(rel) }) }
	t.Cleanup(func() { releaseFn(); srv.Close() })
	return srv, arr, releaseFn
}

// assertLockHeldAcrossDelete runs `del` against a server that blocks inside
// DELETE. While the request is parked, the same target's lock must not be
// acquirable; once the delete returns it must be.
func assertLockHeldAcrossDelete(t *testing.T, lock func() func(), arrived <-chan struct{}, release func(), del func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		del()
	}()
	select {
	case <-arrived:
	case <-time.After(lockCompleteBy):
		t.Fatal("platform delete never arrived")
	}
	acquired := make(chan struct{})
	go func() {
		lock()()
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("target lock acquired while the platform delete was in flight")
	case <-time.After(lockHeldWait):
	}
	release()
	select {
	case <-done:
	case <-time.After(lockCompleteBy):
		t.Fatal("delete did not return after the platform was released")
	}
	select {
	case <-acquired:
	case <-time.After(lockCompleteBy):
		t.Fatal("target lock never released after the delete returned")
	}
}

func TestDeleteImageFromPlatforms_LockHeldAcrossPlatformDelete(t *testing.T) {
	t.Parallel()
	srv, arrived, release := newBlockingDeleteServer(t, http.StatusNoContent)
	r, artistSvc := testRouterWithPlatform(t)
	a := addTestArtist(t, artistSvc, "Held Across Loop")
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-held-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}
	assertLockHeldAcrossDelete(t,
		func() func() { return r.publisher.LockBackdropTarget("conn-emby", "emby-held-1") },
		arrived, release,
		func() { r.deleteImageFromPlatforms(context.Background(), a, "fanart") })
}

func TestHandleDeletePushImage_LockHeldAcrossPlatformDelete(t *testing.T) {
	t.Parallel()
	srv, arrived, release := newBlockingDeleteServer(t, http.StatusNoContent)
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Held Across Push")
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
	assertLockHeldAcrossDelete(t,
		func() func() { return r.publisher.LockBackdropTarget("conn-emby", "emby-held-2") },
		arrived, release,
		func() {
			body := `{"connection_id":"conn-emby","platform_artist_id":"emby-held-2"}`
			req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/fanart", strings.NewReader(body))
			req.SetPathValue("id", a.ID)
			req.SetPathValue("type", "fanart")
			r.handleDeletePushImage(httptest.NewRecorder(), req)
		})
}

// A failing platform delete still releases the lock on the loop path.
func TestDeleteImageFromPlatforms_FanartPlatformFailureReleasesLock(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	r, artistSvc := testRouterWithPlatform(t)
	a := addTestArtist(t, artistSvc, "Fail Loop Delete")
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-fail-2"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}
	if w := r.deleteImageFromPlatforms(context.Background(), a, "fanart"); len(w) == 0 {
		t.Fatal("expected a delete-failed warning")
	}
	got := make(chan struct{})
	go func() {
		r.publisher.LockBackdropTarget("conn-emby", "emby-fail-2")()
		close(got)
	}()
	select {
	case <-got:
	case <-time.After(lockCompleteBy):
		t.Fatal("target lock still held after a failed platform delete")
	}
}

// F3: time spent waiting for the target lock is not charged to the platform
// delete budget. Not parallel: it shrinks the package-level budget.
func TestDeleteImageFromPlatforms_LockWaitNotChargedToDeleteBudget(t *testing.T) {
	old := platformDeleteTimeout
	platformDeleteTimeout = 400 * time.Millisecond
	t.Cleanup(func() { platformDeleteTimeout = old })
	srv, hits := newDeleteHitServer(t)
	r, artistSvc := testRouterWithPlatform(t)
	a := addTestArtist(t, artistSvc, "Budget After Lock")
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-budget-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}
	unlock := r.publisher.LockBackdropTarget("conn-emby", "emby-budget-1")
	var warnings []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		warnings = r.deleteImageFromPlatforms(context.Background(), a, "fanart")
	}()
	// Wait out the whole budget while holding the lock, then release.
	time.Sleep(2 * platformDeleteTimeout)
	unlock()
	select {
	case <-done:
	case <-time.After(lockCompleteBy):
		t.Fatal("delete did not complete")
	}
	if len(hits) != 1 || len(warnings) != 0 {
		t.Errorf("platform deletes = %d, warnings = %v; want 1 delete, no warnings", len(hits), warnings)
	}
}
