//go:build unix

package publish

import (
	"context"
	"errors"
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
	// THE OUTCOME MUST NAME THE CANCELLATION, not the file (#2934). Bounding
	// the read stopped the hang; it left the deadline being REPORTED as an
	// unreadable image, which sends the operator to inspect artwork that is
	// perfectly fine when what actually happened is that the request ended.
	// Asserting both halves is what makes the two arms genuinely distinct --
	// an implementation that emitted both strings would satisfy a
	// contains-check on either one alone while telling the operator nothing.
	joined := strings.Join(got.warnings, "|")
	if !strings.Contains(joined, "platform sync canceled") {
		t.Errorf("warnings = %v, want the cancellation reported as a cancellation", got.warnings)
	}
	if strings.Contains(joined, "failed to read image") {
		t.Errorf("warnings = %v carry the ordinary read-failure wording; a canceled request is being "+
			"attributed to the operator's file", got.warnings)
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
	// Same distinction as the single-image case above.
	joined := strings.Join(got.warnings, "|")
	if !strings.Contains(joined, "platform sync canceled") {
		t.Errorf("warnings = %v, want the cancellation reported as a cancellation", got.warnings)
	}
	if strings.Contains(joined, "failed to read fanart") {
		t.Errorf("warnings = %v carry the ordinary read-failure wording; a canceled request is being "+
			"attributed to the operator's artwork", got.warnings)
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
	dir := t.TempDir()
	// Slot 0 unreadable, slot 1 healthy.
	//
	// A DANGLING SYMLINK, not permission bits. Chmod 0o000 does not make a file
	// unreadable for uid 0, so the permission fixture had to SKIP the whole test
	// under root -- and this is the green sibling guarding the
	// no-over-propagation direction, so skipping it silently removes the only
	// thing stopping a fix that aborts the set on the first read error. A
	// symlink pointing at a path that does not exist fails the open with ENOENT
	// for every effective user, root included.
	//
	// IT REACHES THE READ, which is the property that makes it a valid
	// substitute rather than just a different fixture. Discovery lists the
	// directory with os.ReadDir, whose entries carry the LINK's own type and are
	// never resolved, so fanartMatches' IsDir check does not filter a symlink
	// and its .jpg extension matches like any other file. Nothing between
	// discovery and snapshotFanart stats the path, so the first thing to touch
	// the target is the open inside the bounded read -- exactly where the
	// permission fixture failed. assertOnFanartReadPath below pins the first
	// half of that (discovery really returns it) and the warning assertion pins
	// the second (the read really failed).
	bad := filepath.Join(dir, fanartPrimaryFixtureName)
	if err := os.Symlink(filepath.Join(dir, "does-not-exist.jpg"), bad); err != nil {
		t.Fatalf("planting the dangling symlink fixture: %v", err)
	}
	// PRECONDITION: the link really is dangling. A fixture that resolved (a
	// stray file at the target, a platform that refuses to create the link)
	// would make the whole test a healthy-path assertion.
	if _, err := os.Open(bad); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("precondition: opening the dangling symlink gave %v, want a not-exist error; "+
			"the fixture would not produce a read failure", err)
	}
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
// for the over-size arm on the fanart path, and #2712 MOVED which arm answers.
//
// The fixture is over img.MaxDecodeBytes (25 MB) and used to be refused by the
// read, which returns ErrImageTooLarge. The snapshot's per-file cap is 12 MiB,
// stricter and checked from a stat BEFORE the read, so this file is now refused
// without ever being opened. Both outcomes are a distinct over-size warning
// rather than the generic read failure, which is what this test has always been
// about; only the sentence changed, so the assertion is retargeted rather than
// relaxed.
//
// WHAT THIS TEST NO LONGER COVERS, AND WHO DOES. Because the fixture is now
// refused before the read, this test no longer reaches the read's own
// ErrImageTooLarge arm -- it was the fixture that used to. Stated checkably:
// collapse the two arms in snapshotFanart's read-failure branch so the generic
// "failed to read fanart %d" is emitted in BOTH cases, exactly the defect this
// test's _WarnsDistinctly name exists to catch, and the negative assertion
// below cannot see it, because neither arm ran.
//
// That arm is covered by TestSyncAllFanartToPlatforms_ReadOversizeArm_
// StillReachable, immediately below, which kills that same mutation. It reaches
// the arm the only way that remains once the stricter snapshot cap sits in
// front of it: a stat that under-reports what the read delivers, which needs a
// platform primitive (a FIFO, which stats at zero bytes) rather than an
// ordinary file. Between the two tests both arms of the branch stay pinned to
// their own wording; neither test does it alone.
func TestSyncAllFanartToPlatforms_OversizeFile_WarnsDistinctly(t *testing.T) {
	dir := t.TempDir()
	writeOversizeFile(t, filepath.Join(dir, fanartPrimaryFixtureName))
	assertOnFanartReadPath(t, dir, fanartPrimaryFixtureName)

	p := syncTestPublisher()
	warnings := p.SyncAllFanartToPlatforms(context.Background(),
		&artist.Artist{ID: "a1", Name: "Oversize Fanart", Path: dir})

	joined := strings.Join(warnings, "|")
	if !strings.Contains(joined, statRefusalPhrase) {
		t.Fatalf("warnings = %v, want the distinct over-size wording", warnings)
	}
	if strings.Contains(joined, "failed to read fanart") {
		t.Errorf("warnings = %v, want ONLY the over-size wording; the generic "+
			"read-failure string means the two arms are no longer distinct", warnings)
	}
}

// TestSyncAllFanartToPlatforms_ReadOversizeArm_StillReachable keeps the read's
// ErrImageTooLarge arm covered after #2712 took its old fixture away.
//
// snapshotFanart's comment claims that arm is now the NARROWER case rather than
// dead code -- reachable when the pre-read stat did not see the true size. A
// claim of reachability with no test that reaches it is how an arm quietly
// becomes unreachable and nobody notices, so this drives it: the FIFO stats at
// zero bytes, the pre-read cap therefore waves it past, and the read then meets
// more than img.MaxDecodeBytes and refuses on its own terms.
//
// The wording must be the READ's, not either budget message, because that is
// the whole point: a different arm answered.
func TestSyncAllFanartToPlatforms_ReadOversizeArm_StillReachable(t *testing.T) {
	dir := t.TempDir()
	liar := filepath.Join(dir, fanartPrimaryFixtureName)
	fifoDeliveringBytes(t, liar, img.MaxDecodeBytes+1)

	// PRECONDITIONS. The stat must under-report (otherwise the pre-read cap
	// answers and this test measures the wrong arm), and the payload must
	// exceed the READ bound, not merely the snapshot cap.
	info, statErr := os.Stat(liar)
	if statErr != nil {
		t.Fatalf("precondition failed: cannot stat the FIFO: %v", statErr)
	}
	if info.Size() > maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: the stat reports %d bytes, which the pre-read cap already refuses",
			info.Size())
	}
	if img.MaxDecodeBytes+1 <= maxFanartSnapshotFileBytes {
		t.Fatalf("precondition failed: the payload is inside the %d-byte snapshot cap, so the snapshot "+
			"cap could answer instead of the read bound", int64(maxFanartSnapshotFileBytes))
	}

	p := syncTestPublisher()
	warnings := p.SyncAllFanartToPlatforms(context.Background(),
		&artist.Artist{ID: "a1", Name: "Lying Stat", Path: dir})

	joined := strings.Join(warnings, "|")
	if !strings.Contains(joined, "exceeds the size limit for upload") {
		t.Fatalf("warnings = %v, want the READ bound's over-size wording; if a budget message answered "+
			"instead, the ErrImageTooLarge arm is now unreachable and should be deleted rather than "+
			"documented as live", warnings)
	}

	// The positive assertion alone is not enough. It says the read arm spoke;
	// it does not say a budget arm stayed silent, and both could speak at once
	// if a future change refused the slot as well. Since the point of this test
	// is WHICH arm answered, name the two wordings that must be absent.
	if strings.Contains(joined, statRefusalPhrase) {
		t.Errorf("warnings = %v carry the PRE-read refusal wording %q, but the FIFO stats at zero bytes, "+
			"so the pre-read check cannot legitimately have refused this slot", warnings, statRefusalPhrase)
	}
	if strings.Contains(joined, readRefusalPhrase) {
		t.Errorf("warnings = %v carry the POST-read budget wording %q; the READ refused these bytes on its "+
			"own terms, so no budget branch can have handled the slot", warnings, readRefusalPhrase)
	}
}

// TestSyncAllFanartToPlatforms_CancellationStopsTheSet is the LOAD-BEARING
// guard for #2934 on the fanart sync path, and the reason it is multi-file is
// the whole point.
//
// A SINGLE-FILE cancellation case cannot tell "stopped" from "warned and had
// nothing left to do" -- both produce one warning and zero uploads, so it
// passes against the defect.
//
// SLOT ORDER IS THE WHOLE FIXTURE, and getting it backwards makes the test
// vacuous. The healthy file must come FIRST, before the one that stalls:
//
//	healthy-first (this fixture): slot 0 is read and CAPTURED WITH BYTES, then
//	  slot 1 consumes the deadline. Under the defect that cancellation is
//	  warned about per-file, the loop continues, hasReadableFanart sees slot
//	  0's real bytes and returns TRUE, and the upload loop pushes the
//	  operator's artwork to the peer for a request they had already abandoned.
//	  That is the partial success, and it is visible at the uploader.
//	stall-first (the trap): the FIFO consumes the deadline before anything is
//	  captured, so every LATER read short-circuits on the dead context and the
//	  snapshot holds nothing but nil entries. hasReadableFanart returns false
//	  and the defect returns early on its own -- zero uploads either way, and
//	  an uploader assertion that can never fail.
//
// So the assertion is on the UPLOADER, not the warnings: what makes this a
// partial-success bug is work that reached the peer, and only the uploader can
// see that.
func TestSyncAllFanartToPlatforms_CancellationStopsTheSet(t *testing.T) {
	dir := t.TempDir()
	// Slot 0 healthy and read BEFORE the cancellation; slot 1 stalls until the
	// deadline. See the slot-order note above.
	seedJPG(t, dir, fanartPrimaryFixtureName)
	syncFifo(t, filepath.Join(dir, "fanart1.jpg"))
	assertOnFanartReadPath(t, dir, fanartPrimaryFixtureName)
	// PRECONDITION: the stalling file really is on the read path too. If
	// discovery returned only slot 0 the pass would complete normally and this
	// would assert nothing about a cancellation.
	assertOnFanartReadPath(t, dir, "fanart1.jpg")

	up := &recordingIndexedUploader{}
	orig := newIndexedImageUploader
	newIndexedImageUploader = func(_ *connection.Connection, _ *slog.Logger) connection.IndexedImageUploader {
		return up
	}
	t.Cleanup(func() { newIndexedImageUploader = orig })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	type result struct{ warnings []string }
	done := make(chan result, 1)
	p := syncTestPublisher()
	go func() {
		done <- result{warnings: p.SyncAllFanartToPlatforms(ctx,
			&artist.Artist{ID: "a1", Name: "Canceled Mid-Set", Path: dir})}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SyncAllFanartToPlatforms did not return within 5s of a 100ms deadline")
	}

	if idx := up.got(); len(idx) != 0 {
		t.Errorf("uploaded slots %v after the request was canceled, want none; the cancellation is "+
			"being absorbed as a per-file read failure and the set continues, so the bytes captured "+
			"before the abort are still pushed to the peer for a request the operator abandoned", idx)
	}
	joined := strings.Join(got.warnings, "|")
	if !strings.Contains(joined, "platform sync canceled") {
		t.Errorf("warnings = %v, want the cancellation named; stopping silently tells the caller "+
			"nothing about why the push is incomplete", got.warnings)
	}
}
