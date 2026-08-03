package rule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// The guards in this file all cover ONE defect class: a function that reads the
// filesystem under a context now fails on CANCELLATION as well as on a genuine
// I/O problem, through an error that looks identical. A caller written when
// only the I/O case existed logs-and-continues, which silently converts "the
// whole operation was abandoned" into "one file had a problem" -- and the
// derived result (a deletion whitelist, a fanart count) is then acted on as if
// it were complete.
//
// Every guard here cancels the context BEFORE the call, which is the cheapest
// faithful reproduction: runCancellable short-circuits on ctx.Err() before it
// touches the filesystem, so the helper returns exactly the error shape a
// mid-flight cancellation produces.

// TestExpectedImageFiles_CanceledCtxDoesNotYieldPartialWhitelist proves the
// expected-file set is never returned SHORT. The set is a whitelist and
// ExtraneousImagesFixer deletes every image file absent from it, so a truncated
// set is a license to delete artwork that was simply never discovered.
func TestExpectedImageFiles_CanceledCtxDoesNotYieldPartialWhitelist(t *testing.T) {
	dir := t.TempDir()
	// A numbered fanart variant that ONLY DiscoverFanart puts on the
	// whitelist. It is not in any static name list, so if discovery is
	// skipped the file is not expected -- which is the deletion trigger.
	if err := os.WriteFile(filepath.Join(dir, "fanart3.jpg"), []byte("artwork"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	expected, err := expectedImageFiles(ctx, nil, dir)
	if err == nil {
		t.Fatalf("expectedImageFiles returned no error on a canceled ctx; got set of %d entries", len(expected))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want it to wrap context.Canceled", err)
	}
	if expected != nil {
		t.Errorf("a partial whitelist was returned alongside the error: %v", expected)
	}
}

// TestExtraneousImagesFixer_CanceledCtxDeletesNothing is the data-loss guard.
// A cancellation must stop the fixer BEFORE the deletion loop; the loop must
// never run against an incomplete expected set.
func TestExtraneousImagesFixer_CanceledCtxDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	// Numbered fanart variants: legitimate artwork that is expected ONLY
	// because DiscoverFanart finds it. Under a truncated whitelist these are
	// exactly the files the deletion loop unlinks.
	artwork := []string{"fanart1.jpg", "fanart2.jpg", "fanart3.jpg"}
	for _, name := range artwork {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("artwork"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	a := &artist.Artist{Name: "Cancel Guard", Path: dir, LibraryID: "lib-test"}
	f := NewExtraneousImagesFixer(nil, nonSharedFSCheck(), testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := f.Fix(ctx, a, &Violation{RuleID: RuleExtraneousImages})
	if err == nil {
		t.Fatalf("Fix returned no error on a canceled ctx; result = %+v", result)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want it to wrap context.Canceled", err)
	}

	// The assertion that matters: the artwork is still on disk.
	for _, name := range artwork {
		if _, statErr := os.Stat(filepath.Join(dir, name)); statErr != nil {
			t.Errorf("%s was deleted under a canceled ctx: %v", name, statErr)
		}
	}
}

// TestResyncFanartFields_CanceledCtxDoesNotClearFanart proves a cancellation
// never records "this artist has no fanart" for an artist that has plenty.
// These paths run in AUTO mode across the whole library with nobody watching,
// so a wrong derived field is written and persisted unattended.
func TestResyncFanartFields_CanceledCtxDoesNotClearFanart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), []byte("artwork"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &artist.Artist{
		Name:         "Cancel Guard",
		Path:         dir,
		FanartExists: true,
		FanartCount:  1,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := resyncFanartFields(ctx, a, []string{"fanart.jpg"})
	if err == nil {
		t.Fatal("resyncFanartFields returned no error on a canceled ctx")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want it to wrap context.Canceled", err)
	}
	if !a.FanartExists {
		t.Error("FanartExists was cleared for an artist that HAS fanart, from a canceled scan")
	}
	if a.FanartCount != 1 {
		t.Errorf("FanartCount = %d; want it left at 1 rather than rewritten from a truncated scan", a.FanartCount)
	}
}

// TestCountBackdrops_CanceledCtxReportsRatherThanUndercounts proves an
// abandoned scan is not reported as a low count. The count feeds the
// backdrop_min_count rule, so an undercount raises a "missing backdrops"
// violation against an artist that is fine.
func TestCountBackdrops_CanceledCtxReportsRatherThanUndercounts(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"fanart.jpg", "fanart1.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("artwork"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	e := &Engine{platformService: nil}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	count, err := e.countBackdrops(ctx, dir)
	if err == nil {
		t.Fatalf("countBackdrops returned no error on a canceled ctx; count = %d", count)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want it to wrap context.Canceled", err)
	}
}
