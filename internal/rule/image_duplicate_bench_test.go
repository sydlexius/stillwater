package rule

// Wall-clock evidence for #2349, alongside the decode counts asserted in
// image_duplicate_exact_test.go.
//
// The cold evaluation is what EVERY evaluation looked like before the fix:
// nothing persisted the computed hash, so every fanart file was re-read and
// re-decoded on every pass. The warm evaluation is what every pass after the
// first looks like now. The ratio between them is the recurring cost the
// recomputation bug was paying forever.

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestImageDuplicate_ColdVsWarmEvaluationCost reports the measured cost of a
// hashing evaluation versus a fully-cached one, and fails if the cached pass is
// not decisively cheaper -- which would mean the hashes are not actually being
// reused.
func TestImageDuplicate_ColdVsWarmEvaluationCost(t *testing.T) {
	const slots = 8

	e, db := newDupTestEngine(t)
	insertTestArtist(t, db, "art-cost", "Cost Artist")

	dir := t.TempDir()
	for i := 0; i < slots; i++ {
		name := "fanart.jpg"
		if i > 0 {
			name = fanartSlotFileName(i)
		}
		createGradientJPEG(t, filepath.Join(dir, name), i)
		insertTestImage(t, db, "art-cost", "fanart", i)
	}

	a := &artist.Artist{ID: "art-cost", Name: "Cost Artist", Path: dir}
	checker := e.makeImageDuplicateChecker()

	// Cold: no stored hashes, so all `slots` files are read and decoded.
	coldCount := installHashCounter(t)
	coldStart := time.Now()
	checker(t.Context(), a, RuleConfig{})
	cold := time.Since(coldStart)

	// Warm: hashes persisted by the cold pass; no file should be touched.
	warmCount := installHashCounter(t)
	warmStart := time.Now()
	checker(t.Context(), a, RuleConfig{})
	warm := time.Since(warmStart)

	t.Logf("cold evaluation: %v (%d reads, %d decodes over %d fanart slots)",
		cold, coldCount.reads, coldCount.decodes, slots)
	t.Logf("warm evaluation: %v (%d reads, %d decodes)",
		warm, warmCount.reads, warmCount.decodes)
	if warm > 0 {
		t.Logf("warm is %.1fx cheaper than cold", float64(cold)/float64(warm))
	}

	if coldCount.decodes != slots {
		t.Errorf("cold pass decoded %d files, want %d", coldCount.decodes, slots)
	}
	if warmCount.decodes != 0 || warmCount.reads != 0 {
		t.Fatalf("warm pass did %d reads and %d decodes, want 0 of each -- the hashes "+
			"are not being reused across evaluations (#2349)", warmCount.reads, warmCount.decodes)
	}
	// No wall-clock assertion here: a zero-read/zero-decode warm pass can
	// still occasionally clock slower than the cold one on a loaded runner
	// (GC pauses, scheduler noise), and the deterministic decode/read-count
	// assertions above already prove the hash cache is doing its job. A
	// timing comparison would only add flake risk, not coverage. The t.Logf
	// speedup line above stays for visibility.
}

// fanartSlotFileName maps a slot index to the numbered fanart filename that
// DiscoverFanart expects (slot 1 -> fanart2.jpg, slot 2 -> fanart3.jpg, ...).
func fanartSlotFileName(slot int) string {
	return fmt.Sprintf("fanart%d.jpg", slot+1)
}
