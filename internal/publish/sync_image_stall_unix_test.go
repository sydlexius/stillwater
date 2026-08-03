//go:build unix

package publish

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
)

// syncImageToPlatforms locates the image under ctx (FindExistingImage) and then
// read its bytes with a bare os.ReadFile. #2934: that left the only step a dead
// mount can block forever sitting BEHIND the bound meant to reach it, so a sync
// against an unresponsive share never returned no matter what deadline the
// caller set.
//
// A FIFO with no writer reproduces that: opening it for reading blocks in the
// kernel until a writer appears, exactly as a hard-mounted export that stopped
// answering does, and it needs no network. Unix-only because mkfifo is.

// syncFifo plants a FIFO at path and opens the write end at test end so the
// abandoned reader can drain rather than outliving the test binary.
func syncFifo(t *testing.T, path string) {
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

// syncTestPublisher builds a Publisher wired to one enabled Emby connection
// pointing at an address nothing listens on. That is deliberate: these tests
// must never reach the upload, so an unreachable peer is the honest fixture --
// if the read is ever fixed to return promptly AND the sync then proceeds, the
// warning it produces names the upload, not the read, and the assertions below
// would catch that rather than silently passing.
func syncTestPublisher() *Publisher {
	return New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, URL: "http://127.0.0.1:1", Enabled: true, Status: "ok",
				Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})
}

// TestSyncImageToPlatforms_StalledRead_ReturnsOnDeadline is the Family B
// property: the call RETURNS on its deadline instead of blocking indefinitely.
//
// It asserts on the OUTCOME the caller actually receives -- the returned
// warnings -- not merely that the function came back. A sync that returned
// having silently uploaded nothing and warned about nothing would be a worse
// bug than the hang: the operator would be told the push succeeded.
func TestSyncImageToPlatforms_StalledRead_ReturnsOnDeadline(t *testing.T) {
	dir := t.TempDir()
	syncFifo(t, filepath.Join(dir, "folder.jpg"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// The call runs on its own goroutine precisely because a REGRESSION means
	// it never returns: a plain call would hang until the go test timeout and
	// surface as an unrelated panic rather than as this test failing.
	type result struct {
		warnings []string
		elapsed  time.Duration
	}
	done := make(chan result, 1)
	p := syncTestPublisher()
	go func() {
		start := time.Now()
		w := p.SyncImageToPlatforms(ctx, &artist.Artist{ID: "a1", Name: "Stalled", Path: dir}, "thumb")
		done <- result{warnings: w, elapsed: time.Since(start)}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SyncImageToPlatforms did not return within 5s of a 100ms deadline; " +
			"the image byte read is still wedging its caller")
	}
	if got.elapsed > 2*time.Second {
		t.Fatalf("SyncImageToPlatforms took %v to honor a 100ms deadline; the bound is not engaging", got.elapsed)
	}
	if len(got.warnings) == 0 {
		t.Fatal("a sync whose image could not be read returned NO warnings; the caller surfaces these " +
			"to the operator, so silence reads as a successful push")
	}
	if !strings.Contains(strings.Join(got.warnings, "|"), "failed to read image") {
		t.Errorf("warnings do not report the read failure: %v", got.warnings)
	}
}

// TestSyncImageToPlatforms_UnreadableImageStillWarnsAndContinues is the REQUIRED
// green sibling. An ordinary read failure -- not a cancellation -- must keep its
// historical handling: warn and return, never panic, never claim success. A
// fixture holding only readable files would leave this branch unexercised and
// make the guard above vacuous.
//
// The fixture has to survive the ctx-bound STAT and fail only on the byte read,
// or it would exercise the wrong branch entirely (a probe miss returns the "no
// local image found" warning, never reaching the read). A DIRECTORY named like
// the canonical image does exactly that: stat succeeds so FindExistingImage
// reports it found, and the read then returns EISDIR.
func TestSyncImageToPlatforms_UnreadableImageStillWarnsAndContinues(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "folder.jpg"), 0o755); err != nil {
		t.Fatalf("seeding an unreadable 'image': %v", err)
	}

	p := syncTestPublisher()
	warnings := p.SyncImageToPlatforms(context.Background(),
		&artist.Artist{ID: "a1", Name: "Unreadable", Path: dir}, "thumb")

	if len(warnings) == 0 {
		t.Fatal("an unreadable image produced no warning; the failure would be invisible to the operator")
	}
	joined := strings.Join(warnings, "|")
	if !strings.Contains(joined, "failed to read image") {
		t.Errorf("warnings = %v, want the ordinary read-failure warning preserved", warnings)
	}
	// And the raw OS error is not leaked into an operator-facing string.
	for _, leak := range []string{"is a directory", "no such file", "permission denied"} {
		if strings.Contains(joined, leak) {
			t.Errorf("warning leaks the raw OS error %q: %v", leak, warnings)
		}
	}
}

// TestSyncImageToPlatforms_HealthyImageStillUploads is the other half of the
// green guard: the ordinary success path must be untouched. A read that became
// stricter in any way that rejected a normal file would satisfy every
// cancellation assertion above while breaking every push in the product.
func TestSyncImageToPlatforms_HealthyImageStillUploads(t *testing.T) {
	dir := t.TempDir()
	seedJPG(t, dir, "folder.jpg")

	hits := &uploadHits{}
	srv := newImageUploadServer(hits)
	defer srv.Close()

	p := New(Deps{
		Logger: silentLogger(),
		ArtistService: &fakePlatformLister{ids: []artist.PlatformID{
			{ArtistID: "a1", ConnectionID: "c", PlatformArtistID: "p1"},
		}},
		ConnectionService: &fakeConnectionGetter{conns: map[string]*connection.Connection{
			"c": {ID: "c", Type: connection.TypeEmby, URL: srv.URL, Enabled: true, Status: "ok",
				Emby: &connection.EmbyConfig{PlatformUserID: "u1", FeatureImageWrite: true}},
		}},
	})
	warnings := p.SyncImageToPlatforms(context.Background(),
		&artist.Artist{ID: "a1", Name: "Healthy", Path: dir}, "thumb")

	if len(warnings) != 0 {
		t.Fatalf("a healthy image produced warnings: %v", warnings)
	}
	waitForUploads(t, hits, 1)
	// Assert the BODY carried bytes, not merely that a request arrived: a
	// regression that dropped the payload would still register as an upload.
	if got := hits.lastBodySize(); got == 0 {
		t.Error("the upload body was empty; the image bytes never reached the peer")
	}
}
