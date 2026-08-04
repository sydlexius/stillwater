//go:build unix

package publish

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	img "github.com/sydlexius/stillwater/internal/image"
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

// --- fanart path (#2934 round 2) ---
//
// syncAllFanartToPlatforms is a SECOND, equally request-reachable entry point in
// this file, and the first round fixed only the single-image one. Its shape was
// the same defect and strictly worse: DiscoverFanart is ctx-bound, snapshotFanart
// then read every discovered file with a bare os.ReadFile, and it reads the WHOLE
// set -- so a mount that stops answering wedges on file 1 of 42 while seven
// handler call sites sit on a 30s deadline that cannot reach it.

// fanartPrimaryFixtureName is the filename the fanart sync ACTUALLY reads under
// this package's test wiring, and planting the wedge anywhere else is how this
// bug stayed hidden: a fixture named backdrop.jpg returns instantly, because
// getActiveFanartPrimary resolves to fanart.jpg and DiscoverFanart matches on
// that base only -- backdrop.jpg is a FALLBACK in DefaultFileNames, never the
// primary. assertOnFanartReadPath below pins that rather than trusting it.
const fanartPrimaryFixtureName = "fanart.jpg"

// assertOnFanartReadPath fails unless discovery actually returns the named file.
// A stall test whose fixture is off the read path returns fast and passes for
// the wrong reason -- vacuously green, and worse than no test at all, since it
// reads as coverage of a path it never touches.
func assertOnFanartReadPath(t *testing.T, dir, name string) {
	t.Helper()
	p := syncTestPublisher()
	primary := p.getActiveFanartPrimary(context.Background())
	if primary != fanartPrimaryFixtureName {
		t.Fatalf("precondition: fanart primary resolved to %q, but the fixture is named %q; "+
			"the wedge would be planted off the read path", primary, fanartPrimaryFixtureName)
	}
	// Discovery must be given a deadline of its own: on a REGRESSION the fixture
	// is a FIFO, and while os.ReadDir does not open it, proving the precondition
	// must not itself be able to hang.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	paths, err := img.DiscoverFanart(ctx, dir, primary)
	if err != nil {
		t.Fatalf("precondition: discovering fanart in the fixture dir: %v", err)
	}
	want := filepath.Join(dir, name)
	for _, got := range paths {
		if got == want {
			return
		}
	}
	t.Fatalf("precondition: %s is NOT on the fanart read path (discovered %v); "+
		"this test would pass without ever exercising the read", want, paths)
}

// TestSyncAllFanartToPlatforms_StalledRead_ReturnsOnDeadline is the Family B
// property for the fanart path, mirroring the single-image test above.
func TestSyncAllFanartToPlatforms_StalledRead_ReturnsOnDeadline(t *testing.T) {
	dir := t.TempDir()
	syncFifo(t, filepath.Join(dir, fanartPrimaryFixtureName))
	assertOnFanartReadPath(t, dir, fanartPrimaryFixtureName)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	type result struct {
		warnings []string
		elapsed  time.Duration
	}
	done := make(chan result, 1)
	p := syncTestPublisher()
	go func() {
		start := time.Now()
		w := p.SyncAllFanartToPlatforms(ctx, &artist.Artist{ID: "a1", Name: "Stalled", Path: dir})
		done <- result{warnings: w, elapsed: time.Since(start)}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SyncAllFanartToPlatforms did not return within 5s of a 100ms deadline; " +
			"the fanart byte read is still wedging its caller")
	}
	if got.elapsed > 2*time.Second {
		t.Fatalf("SyncAllFanartToPlatforms took %v to honor a 100ms deadline; the bound is not engaging", got.elapsed)
	}
	if len(got.warnings) == 0 {
		t.Fatal("a fanart sync whose only file could not be read returned NO warnings; " +
			"silence reads to the operator as a successful push")
	}
	if !strings.Contains(strings.Join(got.warnings, "|"), "failed to read fanart") {
		t.Errorf("warnings do not report the read failure: %v", got.warnings)
	}
}

// recordingIndexedUploader accepts every fanart upload and records the slot
// index it was sent to. The green sibling below needs the INDEX, not just a
// count: the whole point of keeping a nil-data entry for a failed read is that
// the files after it keep their true slot numbers.
type recordingIndexedUploader struct {
	mu      sync.Mutex
	indices []int
}

func (r *recordingIndexedUploader) UploadImageAtIndex(_ context.Context, _, _ string, idx int, _ []byte, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.indices = append(r.indices, idx)
	return nil
}

func (r *recordingIndexedUploader) got() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.indices...)
}

// TestSyncAllFanartToPlatforms_UnreadableFileSkipsAndContinues is the REQUIRED
// green sibling, guarding the OPPOSITE failure: over-propagation.
//
// An ordinary per-file read error is not a cancellation and must NOT abort the
// set. The unreadable file is skipped with a warning, the readable ones still
// upload, and -- the part that actually has teeth -- the survivor keeps its TRUE
// slot index. A fix that bailed out of the loop, or that compacted the snapshot,
// would satisfy every cancellation assertion above and silently renumber the
// operator's whole gallery.
func TestSyncAllFanartToPlatforms_UnreadableFileSkipsAndContinues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not make a file unreadable")
	}
	dir := t.TempDir()
	// Slot 0 unreadable, slot 1 healthy. A REAL file with its permissions
	// removed, not a directory: fanartMatches skips directory entries outright,
	// so a directory fixture never reaches the read and proves nothing.
	bad := filepath.Join(dir, fanartPrimaryFixtureName)
	writeFile(t, bad, []byte{0xff, 0xd8, 0xff, 0xd9})
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatalf("removing read permission: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o600) })
	seedJPG(t, dir, "fanart1.jpg")
	assertOnFanartReadPath(t, dir, fanartPrimaryFixtureName)

	up := &recordingIndexedUploader{}
	orig := newIndexedImageUploader
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return up
	}
	t.Cleanup(func() { newIndexedImageUploader = orig })

	p := syncTestPublisher()
	warnings := p.SyncAllFanartToPlatforms(context.Background(),
		&artist.Artist{ID: "a1", Name: "Partly Unreadable", Path: dir})

	joined := strings.Join(warnings, "|")
	if !strings.Contains(joined, "failed to read fanart 0") {
		t.Errorf("warnings = %v, want the unreadable slot 0 reported", warnings)
	}
	got := up.got()
	if len(got) != 1 {
		t.Fatalf("uploads = %v, want exactly one (slot 0 unreadable, slot 1 healthy); "+
			"a read error must skip its own file, never abort the set", got)
	}
	if got[0] != 1 {
		t.Errorf("the surviving file uploaded at index %d, want its TRUE index 1; "+
			"the failed slot's index must still be spent or the gallery renumbers", got[0])
	}
}

// writeOversizeFile plants a sparse file one byte past MaxDecodeBytes. Sparse
// because the bound is 25MB and the test must not actually write 25MB.
func writeOversizeFile(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating oversize fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(img.MaxDecodeBytes + 1); err != nil {
		t.Fatalf("truncating oversize fixture: %v", err)
	}
}

// TestSyncImageToPlatforms_OversizeImage_WarnsDistinctly covers the
// ErrImageTooLarge arm, which had ZERO hits.
//
// It is NOT equivalent to the ordinary read-failure arm it sits beside: same
// control flow, different string. The failure a test catches here is a
// copy-paste leaving the SAME wording in both arms, which makes the split dead
// while every other assertion in this file still passes -- the operator is told
// to check the file for I/O errors when the real answer is "it is over 25MB".
func TestSyncImageToPlatforms_OversizeImage_WarnsDistinctly(t *testing.T) {
	dir := t.TempDir()
	writeOversizeFile(t, filepath.Join(dir, "folder.jpg"))

	p := syncTestPublisher()
	warnings := p.SyncImageToPlatforms(context.Background(),
		&artist.Artist{ID: "a1", Name: "Oversize", Path: dir}, "thumb")

	joined := strings.Join(warnings, "|")
	if !strings.Contains(joined, "exceeds the size limit") {
		t.Fatalf("warnings = %v, want the distinct over-size wording", warnings)
	}
	// And it must NOT also carry the generic wording: an arm that emitted both
	// would pass the assertion above while telling the operator nothing.
	if strings.Contains(joined, "failed to read image") {
		t.Errorf("warnings = %v, want ONLY the over-size wording; the generic "+
			"read-failure string means the two arms are no longer distinct", warnings)
	}
}

// TestSyncAllFanartToPlatforms_OversizeFile_WarnsDistinctly is the same guard
// for the over-size arm added to snapshotFanart, for the same reason.
func TestSyncAllFanartToPlatforms_OversizeFile_WarnsDistinctly(t *testing.T) {
	dir := t.TempDir()
	writeOversizeFile(t, filepath.Join(dir, fanartPrimaryFixtureName))
	assertOnFanartReadPath(t, dir, fanartPrimaryFixtureName)

	p := syncTestPublisher()
	warnings := p.SyncAllFanartToPlatforms(context.Background(),
		&artist.Artist{ID: "a1", Name: "Oversize Fanart", Path: dir})

	joined := strings.Join(warnings, "|")
	if !strings.Contains(joined, "exceeds the size limit") {
		t.Fatalf("warnings = %v, want the distinct over-size wording", warnings)
	}
	if strings.Contains(joined, "failed to read fanart") {
		t.Errorf("warnings = %v, want ONLY the over-size wording; the generic "+
			"read-failure string means the two arms are no longer distinct", warnings)
	}
}
