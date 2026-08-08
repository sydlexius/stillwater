//go:build unix

package publish

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// #2976 review (Copilot, suppressed). Both sync entry points aborted correctly
// on a stalled mount but told the operator the WRONG THING: "platform sync
// canceled: the request ended before ... could be read". A cancellation is the
// client walking away; a stalled read cap refusal is the server unable to read
// its own library with the request still very much alive. The first sends an
// operator to look at their browser or their own patience, the second at their
// mount.
//
// These assert on the WARNING STRINGS because those are what the operator
// actually sees -- the warnings are returned to the caller and surfaced. A test
// that only checked "the call returned an abort" would pass with either
// message, which is exactly how the wrong one survived a round of review.
//
// The wedge is a FIFO with no writer: the read blocks until the ctx deadline
// fires. That produces the CANCELLATION arm, so these tests pin the message on
// the path that already worked and would silently regress if the two branches
// were ever collapsed back together. The cap arm's message is pinned by the
// api-package sibling (handlers_push_stall_test.go), where the sentinel can be
// injected directly through writeCanceledPush.

// TestSyncAllFanart_StalledMount_WarningNamesTheRead is the fanart-set path.
func TestSyncAllFanart_StalledMount_WarningNamesTheRead(t *testing.T) {
	dir := t.TempDir()
	syncFifo(t, filepath.Join(dir, fanartPrimaryFixtureName))
	// AFTER planting it: discovery reads the directory, so the fixture has to
	// exist for the precondition to mean anything. Asserting first reported
	// "not on the read path" for a file that simply was not there yet.
	assertOnFanartReadPath(t, dir, fanartPrimaryFixtureName)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	p := syncTestPublisher()
	done := make(chan []string, 1)
	go func() {
		done <- p.SyncAllFanartToPlatforms(ctx, &artist.Artist{ID: "a1", Path: dir, Name: "Dest"})
	}()

	var warnings []string
	select {
	case warnings = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SyncAllFanartToPlatforms did not return within 5s of a 150ms deadline; the read is not bounded")
	}

	if len(warnings) == 0 {
		t.Fatal("a sync that read nothing must warn; silence would tell the operator the push succeeded")
	}
	joined := strings.Join(warnings, " | ")
	// The abort must be reported as a READ failure, not as a successful push
	// and not as an upload problem -- nothing was ever uploaded.
	if !strings.Contains(joined, "could not be read") && !strings.Contains(joined, "before all fanart could be read") {
		t.Errorf("warnings %q do not tell the operator the fanart could not be READ", joined)
	}
	if strings.Contains(joined, "uploaded") {
		t.Errorf("warnings %q mention an upload, but the peer was never reached", joined)
	}
}

// TestSyncImageToPlatforms_StalledMount_WarningNamesTheRead is the
// single-image path. It carried the ORIGINAL defect: a bare ctx.Err() after a
// bounded read, so a cap refusal fell through to "failed to read image for
// upload" -- blaming the operator's file for an unresponsive mount. That branch
// now has its own arm; this pins the cancellation message it sits beside.
func TestSyncImageToPlatforms_StalledMount_WarningNamesTheRead(t *testing.T) {
	dir := t.TempDir()
	syncFifo(t, filepath.Join(dir, "folder.jpg"))

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	p := syncTestPublisher()
	done := make(chan []string, 1)
	go func() {
		done <- p.SyncImageToPlatforms(ctx, &artist.Artist{ID: "a1", Path: dir, Name: "Dest"}, "thumb")
	}()

	var warnings []string
	select {
	case warnings = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SyncImageToPlatforms did not return within 5s of a 150ms deadline; the read is not bounded")
	}

	joined := strings.Join(warnings, " | ")
	if !strings.Contains(joined, "could not be read") && !strings.Contains(joined, "before the image could be read") {
		t.Errorf("warnings %q do not tell the operator the image could not be READ", joined)
	}
	// The specific misreport this family exists to prevent: telling the
	// operator their FILE is bad when the mount is what stopped answering.
	if strings.Contains(joined, "failed to read image for upload") {
		t.Errorf("warnings %q blame the file; the read never completed, so nothing is known about it", joined)
	}
}
