//go:build unix

package rule

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/image"
)

// TestListPHashRepairRuns_StalledManifestReadIsAnErrorNotAShorterList: a manifest
// read that never answers (a FIFO stands in for a hung mount) ends in a context
// timeout. That says the MOUNT is unresponsive, so the list must return an error
// rather than skip the run and report fewer back-outs.
func TestListPHashRepairRuns_StalledManifestReadIsAnErrorNotAShorterList(t *testing.T) {
	p, db := newPHashRepairPipeline(t)
	dir := t.TempDir()
	seedRepairArtist(t, db, "art-a", "Artist A", dir)
	writeManifestForTest(t, dir, "good-run", `{"created_at":"2026-10-02T12:00:00Z","entries":[{"file_name":"f.jpg","stored_name":"001-f.jpg"}]}`)
	writeManifestForTest(t, dir, "wedged-run", `{}`)
	wedged := filepath.Join(dir, image.RepairDirName, "wedged-run", "manifest.json")
	if err := removeForTest(wedged); err != nil {
		t.Fatal(err)
	}
	wedgeRestoreFifo(t, wedged)

	// The good run alone would list fine; only the wedged manifest can fail it.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	runs, err := p.ListPHashRepairRuns(ctx, "art-a")
	if err == nil {
		t.Fatalf("a stalled manifest read must be an error, got a list of %d runs", len(runs))
	}
}
