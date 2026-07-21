package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestUpdateArtistFanartCount_FailedPersistLeavesNoObservableCount covers the
// mutate-then-fail hole (#2635).
//
// updateArtistFanartCount set FanartExists/FanartCount on the artist BEFORE
// calling Update. The delete handlers serve a.FanartCount off that same struct,
// so when Update fails the client was told a count the database REJECTED. The
// fix computes into locals and restores the prior values on the failed persist.
func TestUpdateArtistFanartCount_FailedPersistLeavesNoObservableCount(t *testing.T) {
	t.Parallel()
	r, svc := newImageHandlerTestServer(t)
	a, dir, _ := seedPrimaryFanart(t, svc, "Failed Persist")
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(dir, "fanart1.jpg"), distinctJPEG(t, 11), 0o644); err != nil {
		t.Fatalf("seeding fanart1.jpg: %v", err)
	}

	// The values a caller would go on to serve. The fresh walk measures 2, so a
	// leaked mutation is unambiguous.
	a.FanartExists, a.FanartCount = true, 7

	// Seed the PRIOR FanartLowRes true so the restore of that field is guarded.
	// Both fixtures are 1920x1080 (distinctJPEG), well above the 960x540 fanart
	// threshold, so the COMPUTED IsLowResolution is false. The only thing that can
	// leave FanartLowRes true after the failed persist is restoring this prior
	// value -- deleting the restore line leaves it false and this test goes RED.
	a.FanartLowRes = true

	// Break the artist persist while leaving the filesystem walk working, so
	// Update is the only thing that fails.
	if _, err := r.db.Exec(`DROP TABLE artists`); err != nil {
		t.Fatalf("dropping artists: %v", err)
	}
	if err := r.artistService.Update(ctx, a); err == nil {
		t.Fatal("precondition: Update still succeeds, so the early-return path is never reached")
	}

	r.updateArtistFanartCount(ctx, a)

	if a.FanartCount != 7 {
		t.Errorf("FanartCount = %d, want the prior 7 restored: the database rejected the "+
			"write, and the caller serves this field straight into the response body, "+
			"reporting a count the DB refused", a.FanartCount)
	}
	if !a.FanartExists {
		t.Error("FanartExists was left mutated after a rejected write")
	}
	if !a.FanartLowRes {
		t.Error("FanartLowRes = false, want the prior true restored: the fixture is " +
			"high-resolution so the computed value is false, meaning the field was " +
			"left mutated after a rejected write instead of restored")
	}
}
