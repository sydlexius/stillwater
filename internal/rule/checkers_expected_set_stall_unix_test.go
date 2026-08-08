//go:build unix

package rule

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/library"
)

// #2976 hostile review, site #7 -- the most destructive instance of this PR's
// defect, and the one every earlier round missed.
//
// expectedImageFiles builds the WHITELIST that ExtraneousImagesFixer deletes
// AGAINST: every image file in the artist directory that is not on the list is
// unlinked. Its own doc comment already stated the rule -- "Handing a short set
// to the deletion loop would destroy operator artwork... Callers must treat a
// non-nil error as 'no safe answer'" -- and Fix has a HARD STOP that returns
// before the deletion loop on a non-nil error.
//
// The bug was that a stalled-read cap refusal produced a NIL one. Every
// DiscoverFanart call failed, every numbered fanart dropped out of the
// whitelist, the hard stop never fired, and the fixer deleted the operator's
// fanart1/2/3 while reporting success. The guard consulted only ctx.Err(), and
// the cap refuses with the request context still perfectly alive.
//
// Both tests below drive a LIVE context. That is the whole point: a canceled
// context already worked, and using one would prove nothing about the cap.

// saturateCapForExpectedSet wedges reads against a FIFO until the process-wide
// abandoned-read cap refuses a new one.
//
// Genuine saturation rather than an injected sentinel: the cap is package
// state in internal/image, and driving it through the real abandonment path
// also proves the production mechanism engages.
func saturateCapForExpectedSet(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "wedge")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable on this platform: %v", err)
	}
	t.Cleanup(func() {
		// Release the abandoned readers so the process-wide gauge self-heals
		// instead of leaking into later tests in this package.
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
	for i := 0; i < 40; i++ {
		if image.StalledReadCount() >= 16 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, _ = image.ReadImageFileBounded(ctx, fifo)
		cancel()
	}
	t.Skipf("could not saturate the stalled-read cap (count=%d); another test may be draining it",
		image.StalledReadCount())
}

// TestExpectedImageFiles_CapRefusalIsNoSafeAnswer is the unit-level property:
// a cap refusal must produce an ERROR, because the caller's hard stop keys on
// exactly that.
func TestExpectedImageFiles_CapRefusalIsNoSafeAnswer(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg", "fanart3.jpg", "thumb.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", n, err)
		}
	}

	// PRECONDITION: with a healthy mount the whitelist DOES cover the numbered
	// fanart. Without this, "the whitelist is short under a cap" could be true
	// of a fixture that never had them, and the test would prove nothing.
	healthy, err := expectedImageFiles(context.Background(), nil, dir)
	if err != nil {
		t.Fatalf("precondition: healthy expectedImageFiles returned %v", err)
	}
	for _, n := range []string{"fanart1.jpg", "fanart2.jpg", "fanart3.jpg"} {
		if !healthy[n] {
			t.Fatalf("precondition: healthy whitelist is missing %s, so a short set under a "+
				"cap would not be attributable to the cap", n)
		}
	}

	saturateCapForExpectedSet(t)

	ctx := context.Background()
	if ctx.Err() != nil {
		t.Fatal("the context must be LIVE; a canceled one exercises the branch that already worked")
	}
	_, capErr := expectedImageFiles(ctx, nil, dir)
	if capErr == nil {
		t.Fatal("a stalled-read cap refusal returned a NIL error. The caller's hard stop keys on " +
			"a non-nil error, so this hands a SHORT WHITELIST to a deletion loop that unlinks " +
			"every file not on it.")
	}
	if !errors.Is(capErr, image.ErrTooManyStalledReads) {
		t.Errorf("error %v does not wrap ErrTooManyStalledReads; the abort must name its cause", capErr)
	}
}

type extraneousStallLibs struct{}

func (extraneousStallLibs) GetByID(_ context.Context, id string) (*library.Library, error) {
	return &library.Library{ID: id, SharedFSStatus: library.SharedFSNone}, nil
}

// TestExtraneousImagesFixer_CapRefusalDoesNotDeleteArtwork is the one that
// matters: it asserts on FILES ON DISK after driving the real fixer, not on a
// counter or a returned message.
//
// The pre-fix behavior was `fixed=true, "deleted 4 extraneous file(s)"` with
// fanart1/2/3 gone from the directory.
func TestExtraneousImagesFixer_CapRefusalDoesNotDeleteArtwork(t *testing.T) {
	dir := t.TempDir()
	names := []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg", "fanart3.jpg", "thumb.jpg"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", n, err)
		}
	}

	f := NewExtraneousImagesFixer(nil, NewSharedFSCheck(extraneousStallLibs{}, slog.Default()), slog.Default())
	a := &artist.Artist{ID: "a1", Name: "Stalled", Path: dir, LibraryID: "lib1"}

	saturateCapForExpectedSet(t)

	res, err := f.Fix(context.Background(), a, &Violation{RuleID: RuleExtraneousImages})
	if err == nil {
		t.Errorf("Fix returned nil error under a saturated cap (result %+v); it must refuse "+
			"rather than act on a whitelist it could not build", res)
	}
	if res != nil && res.Fixed {
		t.Errorf("Fix reported fixed=true (%q) while unable to read the directory", res.Message)
	}

	// THE ASSERTION THAT MATTERS. Counters and messages can be wrong in either
	// direction; the operator's artwork either survived or it did not.
	for _, n := range names {
		if _, statErr := os.Stat(filepath.Join(dir, n)); os.IsNotExist(statErr) {
			t.Errorf("DATA LOSS: %s was deleted during a stalled mount. The whitelist was short "+
				"because nothing could be read, not because the file was extraneous.", n)
		}
	}
}
