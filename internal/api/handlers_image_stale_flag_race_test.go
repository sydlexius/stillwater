package api

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/watcher"
)

// --- #2634: a read must not destroy state on ambiguous evidence --------------
//
// The serve-image GET handler probes the artist directory once and, on a miss,
// clears the exists_flag. A file that has not become visible yet is
// indistinguishable at that instant from one that was deleted, so the GET the
// UI fires right after a successful fetch was clearing the flag for the artwork
// that fetch had just written.
//
// These tests exercise clearImageFlagAsync directly rather than through the
// handler. The production call site spawns it as a goroutine and does not wait,
// so driving it through an HTTP request would make every assertion a sleep race
// against an unobservable goroutine. The function under test is the whole of the
// destructive decision.

// shortenCorroborationDelay shrinks the re-probe delay for tests that do not
// need real-time separation between the two probes, and restores it afterwards.
// Tests using it must not run in parallel with each other; the delay is a
// package-level var.
func shortenCorroborationDelay(t *testing.T, d time.Duration) {
	t.Helper()
	orig := staleFlagCorroborationDelay
	staleFlagCorroborationDelay = d
	t.Cleanup(func() { staleFlagCorroborationDelay = orig })
}

// seedArtistWithThumbFlag creates an artist plus the artist_images row whose
// exists_flag the clear path destroys, and asserts the row really reads 1
// before the test proceeds.
//
// The row, not the artist's ThumbExists field, is what ClearImageFlag mutates
// (it routes to sqliteImageRepo.ClearExistsFlag). Asserting on the field would
// make every "flag survived" test below pass unconditionally, since nothing in
// this path writes it.
func seedArtistWithThumbFlag(t *testing.T, r *Router, svc *artist.Service, name string) *artist.Artist {
	t.Helper()
	a := &artist.Artist{Name: name, SortName: name, Path: t.TempDir(), ThumbExists: true}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Create already materializes the slot row from ThumbExists, so this forces
	// the flag rather than inserting a duplicate. RowsAffected is checked so a
	// future change to that behavior surfaces here as a seeding failure rather
	// than as five tests silently asserting against a row that does not exist.
	res, err := r.db.ExecContext(context.Background(),
		`UPDATE artist_images SET exists_flag = 1
		 WHERE artist_id = ? AND image_type = 'thumb' AND slot_index = 0`, a.ID)
	if err != nil {
		t.Fatalf("seeding artist_images row: %v", err)
	}
	if n, raErr := res.RowsAffected(); raErr != nil || n != 1 {
		t.Fatalf("seeding artist_images row: affected %d rows (err %v), want exactly 1", n, raErr)
	}
	// Precondition: the flag really is set. Without this the "flag survived"
	// assertions below would pass against a row that never had one.
	if !thumbFlag(t, r, a.ID) {
		t.Fatal("precondition: seeded artist_images row reads exists_flag=0, nothing for the clear to destroy")
	}
	return a
}

// thumbFlag reads the artist_images row's exists_flag straight from the
// database, so the assertion is on stored state rather than on a service-layer
// projection that might be cached or computed.
func thumbFlag(t *testing.T, r *Router, artistID string) bool {
	t.Helper()
	var flag int
	err := r.db.QueryRowContext(context.Background(),
		`SELECT exists_flag FROM artist_images WHERE artist_id = ? AND image_type = 'thumb' AND slot_index = 0`,
		artistID).Scan(&flag)
	if err != nil {
		t.Fatalf("reading thumb exists_flag for %s: %v", artistID, err)
	}
	return flag == 1
}

// TestClearImageFlagAsync_LateLandingWriteKeepsTheFlag is the #2634 race,
// reproduced. The first probe (the serve handler's, already done by the time
// this runs) missed; the file lands DURING the corroboration window; the second
// probe finds it. The flag must survive.
//
// The write is a real concurrent goroutine writing a real file into the real
// directory the probe reads, which is the machinery that would misbehave. A
// test that merely pre-created the file would prove only that a present file is
// not cleared, which was never in doubt.
func TestClearImageFlagAsync_LateLandingWriteKeepsTheFlag(t *testing.T) {
	shortenCorroborationDelay(t, 300*time.Millisecond)
	r, svc := newImageHandlerTestServer(t)
	a := seedArtistWithThumbFlag(t, r, svc, "LateWrite")

	patterns := []string{"thumb.jpg"}
	thumbPath := filepath.Join(a.Path, "thumb.jpg")

	// Precondition: the file is genuinely absent when the clear begins, which
	// is what makes this the ambiguous case rather than a trivially-present one.
	if _, err := os.Stat(thumbPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: thumb must be absent at the start, stat err = %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Land the write inside the corroboration window: after the first
		// probe (already past) and before the second.
		time.Sleep(50 * time.Millisecond)
		if err := os.WriteFile(thumbPath, []byte("jpeg bytes"), 0o644); err != nil {
			t.Errorf("writing thumb: %v", err)
		}
	}()

	r.clearImageFlagAsync(context.Background(), a.ID, "thumb", a.Path, patterns)
	wg.Wait()

	if !thumbFlag(t, r, a.ID) {
		t.Fatal("thumb flag was cleared for a file that landed during the corroboration window (#2634)")
	}
	// And the artifact is on disk, so the flag is telling the truth.
	if _, err := os.Stat(thumbPath); err != nil {
		t.Fatalf("thumb should exist on disk after the racing write: %v", err)
	}
}

// TestClearImageFlagAsync_GenuinelyMissingFileStillClears is the other half of
// the property, and the one that stops the fix from being "never clear
// anything". A file that is absent at both probes IS gone, and the flag that
// claims otherwise must still be corrected.
//
// Without this test, deleting the entire clear path would pass the suite.
func TestClearImageFlagAsync_GenuinelyMissingFileStillClears(t *testing.T) {
	shortenCorroborationDelay(t, 20*time.Millisecond)
	r, svc := newImageHandlerTestServer(t)
	a := seedArtistWithThumbFlag(t, r, svc, "TrulyGone")

	// Precondition asserted by the seeder: flag on, and nothing on disk.
	if _, err := os.Stat(filepath.Join(a.Path, "thumb.jpg")); !os.IsNotExist(err) {
		t.Fatalf("precondition: thumb must be absent, stat err = %v", err)
	}

	r.clearImageFlagAsync(context.Background(), a.ID, "thumb", a.Path, []string{"thumb.jpg"})

	if thumbFlag(t, r, a.ID) {
		t.Fatal("thumb flag survived for a file that was absent at BOTH probes; the corroboration guard has become an unconditional refusal")
	}
}

// TestClearImageFlagAsync_InFlightWriteSuppressesTheClear wires the
// expected-writes registry -- the exact machinery the image write path
// populates -- and asserts the destructive effect is ABSENT while a write to
// this slot is registered.
//
// This is the non-timing guard, so it is asserted with the corroboration delay
// left LONG: if the suppression did not fire, the test would block for the full
// production delay and then clear. The precondition assertion (the path really
// is in the set) is what stops this passing against an empty registry.
func TestClearImageFlagAsync_InFlightWriteSuppressesTheClear(t *testing.T) {
	r, svc := newImageHandlerTestServer(t)
	a := seedArtistWithThumbFlag(t, r, svc, "InFlight")

	patterns := []string{"thumb.jpg"}
	ew := watcher.NewExpectedWrites()
	r.expectedWrites = ew
	thumbPath := filepath.Join(a.Path, "thumb.jpg")
	ew.Add(thumbPath)

	// Precondition: the registry actually holds the path the guard will look
	// for. A guard test against an empty collaborator proves nothing.
	if !ew.IsExpected(thumbPath) {
		t.Fatalf("precondition: %s is not registered as an expected write", thumbPath)
	}

	start := time.Now()
	r.clearImageFlagAsync(context.Background(), a.ID, "thumb", a.Path, patterns)
	elapsed := time.Since(start)

	if !thumbFlag(t, r, a.ID) {
		t.Fatal("thumb flag was cleared while a write to that slot was registered as in flight")
	}
	// The suppression must be the reason, not a slow-but-eventual refusal: an
	// in-flight write is answered immediately, without waiting out the delay.
	if elapsed >= staleFlagCorroborationDelay {
		t.Errorf("clear took %v, want well under the %v corroboration delay: the in-flight guard should short-circuit before the re-probe", elapsed, staleFlagCorroborationDelay)
	}
}

// TestClearImageFlagAsync_MissingProbeInputsDeclines pins the default direction
// of the guard. Called without a directory or naming patterns there is nothing
// to corroborate against, and an uncorroborated clear is the bug. The safe
// answer is to decline, not to fall back to the old single-probe behavior.
func TestClearImageFlagAsync_MissingProbeInputsDeclines(t *testing.T) {
	r, svc := newImageHandlerTestServer(t)
	a := seedArtistWithThumbFlag(t, r, svc, "NoInputs")

	var logBuf safeBuffer
	r.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	r.clearImageFlagAsync(context.Background(), a.ID, "thumb", "", nil)

	if !thumbFlag(t, r, a.ID) {
		t.Fatal("thumb flag was cleared with no directory or patterns to corroborate against")
	}
	if got := logBuf.String(); !strings.Contains(got, "no directory or naming patterns") {
		t.Errorf("want a warning explaining the refusal, log was: %s", got)
	}
}

// TestClearImageFlagAsync_CanceledContextDeclines pins the shutdown case. The
// caller passes context.WithoutCancel, so a done context means the process is
// going away mid-corroboration. An un-cleared flag self-corrects on the next
// serve; a wrongly-cleared one does not, so the tie goes to not destroying.
func TestClearImageFlagAsync_CanceledContextDeclines(t *testing.T) {
	r, svc := newImageHandlerTestServer(t)
	a := seedArtistWithThumbFlag(t, r, svc, "Canceled")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r.clearImageFlagAsync(ctx, a.ID, "thumb", a.Path, []string{"thumb.jpg"})

	if !thumbFlag(t, r, a.ID) {
		t.Fatal("thumb flag was cleared after the context ended mid-corroboration")
	}
}
