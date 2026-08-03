//go:build unix

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/platform"
)

// #2934, Family B: three request-reachable handlers located their image under
// the request context and then read its BYTES with a bare os.ReadFile. That put
// the only step a dead mount can block forever behind a bound that could no
// longer reach it, so the request's own cancellation was decoration -- and for
// the trim handler that also pins a singleton released by a deferred unlock.
//
// A FIFO with no writer is the reproduction: opening it for reading blocks in
// the kernel until a writer appears, the same shape a hard-mounted export that
// stopped answering produces, with no network involved. Unix-only because
// mkfifo is.

// readStallFifo plants a FIFO at path, opening the write end at test end so the
// abandoned reader drains rather than outliving the test binary and tripping a
// goroutine-leak check in some later package.
func readStallFifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}
	t.Cleanup(func() {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = f.Close()
		}
	})
}

// servedWithin runs the handler on its own goroutine and fails unless it
// returns before limit. The goroutine is not ceremony: a REGRESSION here means
// the handler never returns, and a direct call would hang the test binary until
// the go test timeout and report as an unrelated panic rather than as this
// test failing.
func servedWithin(t *testing.T, limit time.Duration, fn func()) time.Duration {
	t.Helper()
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		fn()
		done <- time.Since(start)
	}()
	select {
	case d := <-done:
		return d
	case <-time.After(limit):
		t.Fatalf("handler did not return within %v; the image byte read is still wedging the request", limit)
		return 0
	}
}

// pushFixture wires a router with one Emby connection pointed at an
// unreachable address, an artist rooted at dir, and the platform mapping the
// push handler needs. The peer is deliberately unreachable: these tests must
// never get past the read, so a live upload server would only mask a
// regression in which the read returns and the push proceeds.
func pushFixture(t *testing.T, dir string) (*Router, *artist.Artist) {
	t.Helper()
	r, artistSvc := testRouter(t)
	r.platformService = platform.NewService(r.db)
	a := &artist.Artist{Name: "Stalled Read", SortName: "Stalled Read", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", "http://127.0.0.1:1")
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-stall-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}
	return r, a
}

// pushImages drives handlePushImages for one image type under ctx and returns
// the decoded response.
func pushImages(t *testing.T, r *Router, a *artist.Artist, ctx context.Context, imageType string) (int, []string, []string) {
	t.Helper()
	body := `{"connection_id":"conn-emby","platform_artist_id":"emby-stall-1","image_types":["` + imageType + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/push/images", strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	r.handlePushImages(w, req)

	var resp struct {
		Uploaded []string `json:"uploaded"`
		Errors   []string `json:"errors"`
	}
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decoding response: %v", err)
		}
	}
	return w.Code, resp.Uploaded, resp.Errors
}

// TestHandlePushImages_StalledFanartRead_ReturnsOnDeadline covers the fanart
// branch (handlers_push.go:185), which reads one file per discovered slot.
//
// The assertion is on the OUTCOME the client receives, not merely that the
// handler returned: nothing may be reported as UPLOADED. A handler that came
// back promptly while still claiming a push it never made would be worse than
// the hang it replaced.
func TestHandlePushImages_StalledFanartRead_ReturnsOnDeadline(t *testing.T) {
	dir := t.TempDir()
	readStallFifo(t, filepath.Join(dir, "fanart.jpg"))
	r, a := pushFixture(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var uploaded, errs []string
	elapsed := servedWithin(t, 5*time.Second, func() {
		_, uploaded, errs = pushImages(t, r, a, ctx, "fanart")
	})

	if elapsed > 2*time.Second {
		t.Fatalf("handlePushImages took %v to honor a 100ms deadline; the fanart read is not ctx-bound", elapsed)
	}
	if len(uploaded) != 0 {
		t.Errorf("uploaded = %v, want empty: nothing was read, so nothing can have been pushed", uploaded)
	}
	if len(errs) == 0 {
		t.Error("no errors reported for a fanart slot that could not be read; the client is told the push was clean")
	}
}

// TestHandlePushImages_StalledSingleSlotRead_ReturnsOnDeadline covers the
// single-slot branch (handlers_push.go:218), which is a separate read reached
// through a different code path and would not be exercised by the fanart case.
func TestHandlePushImages_StalledSingleSlotRead_ReturnsOnDeadline(t *testing.T) {
	dir := t.TempDir()
	readStallFifo(t, filepath.Join(dir, "folder.jpg"))
	r, a := pushFixture(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var uploaded, errs []string
	elapsed := servedWithin(t, 5*time.Second, func() {
		_, uploaded, errs = pushImages(t, r, a, ctx, "thumb")
	})

	if elapsed > 2*time.Second {
		t.Fatalf("handlePushImages took %v to honor a 100ms deadline; the single-slot read is not ctx-bound", elapsed)
	}
	if len(uploaded) != 0 {
		t.Errorf("uploaded = %v, want empty: nothing was read, so nothing can have been pushed", uploaded)
	}
	if len(errs) == 0 {
		t.Error("no errors reported for a thumb that could not be read; the client is told the push was clean")
	}
}

// TestHandlePushImages_OrdinaryReadFailureStillContinues is the REQUIRED green
// sibling covering BOTH push read sites in one request. An ordinary read
// failure affects only its own file, so it must still be reported per-slot and
// the remaining slots must still upload -- a fix that aborted the request on
// any read error would turn one odd file into a wholly failed push.
//
// The fixture must differ along the axis under test: slot 0 is UNREADABLE and
// slots 1-2 are fine. A fixture where every file failed, or every file
// succeeded, would leave the continue-vs-abort distinction unexercised and make
// this assertion vacuous.
//
// A DANGLING SYMLINK gives the failure deterministically, and it is the right
// tool rather than an arbitrary one: DiscoverFanart's listing skips
// DIRECTORIES, so a directory standing in for the image would never be
// discovered as a slot at all and the read branch would go untouched. A symlink
// passes that check and then fails on the read itself, which is the branch
// under test.
func TestHandlePushImages_OrdinaryReadFailureStillContinues(t *testing.T) {
	// A live upload server IS wanted here: the point is that the healthy
	// siblings still reach the peer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "fanart.jpg")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	for _, name := range []string{"fanart2.jpg", "fanart3.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake-"+name), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	r, artistSvc := testRouter(t)
	r.platformService = platform.NewService(r.db)
	a := &artist.Artist{Name: "Partial Read", SortName: "Partial Read", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-stall-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	code, uploaded, errs := pushImages(t, r, a, context.Background(), "fanart")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: one unreadable slot must not fail the request", code)
	}
	if len(errs) == 0 {
		t.Error("the unreadable slot produced no error; the failure would be invisible to the client")
	}
	// The teeth: the healthy siblings STILL uploaded. Without this the test
	// would pass against a fix that aborted the whole loop on the first error.
	if len(uploaded) != 2 {
		t.Errorf("uploaded = %v, want the 2 healthy slots: one unreadable file aborted the rest", uploaded)
	}
}

// TestHandleLogoTrim_StalledRead_ReturnsOnDeadline covers handlers_image.go's
// trim read. This one is the singleton case: the handler claims a slot released
// by a deferred unlock, so a read that never returns 409s the endpoint for the
// life of the process rather than merely hanging one request.
func TestHandleLogoTrim_StalledRead_ReturnsOnDeadline(t *testing.T) {
	dir := t.TempDir()
	readStallFifo(t, filepath.Join(dir, "logo.png"))

	r, artistSvc := testRouter(t)
	r.platformService = platform.NewService(r.db)
	a := &artist.Artist{Name: "Trim Stall", SortName: "Trim Stall", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	w := httptest.NewRecorder()
	elapsed := servedWithin(t, 5*time.Second, func() {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/images/logo/trim", nil)
		req.SetPathValue("id", a.ID)
		r.handleLogoTrim(w, req.WithContext(ctx))
	})

	if elapsed > 2*time.Second {
		t.Fatalf("handleLogoTrim took %v to honor a 100ms deadline; the logo read is not ctx-bound", elapsed)
	}
	// The trim must NOT have reported success: a 2xx would tell the client the
	// logo was trimmed and saved when nothing was ever read.
	if w.Code >= 200 && w.Code < 300 {
		t.Errorf("status = %d: a trim whose source could not be read must not report success; body: %s",
			w.Code, w.Body.String())
	}
	// And the original is untouched -- the FIFO is still the only thing there.
	if _, err := os.Lstat(filepath.Join(dir, "logo.png")); err != nil {
		t.Errorf("the original logo path was disturbed by a trim that never read it: %v", err)
	}
}
