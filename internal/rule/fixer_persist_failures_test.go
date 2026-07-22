package rule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestRunResult_PersistFailures pins the SEMANTICS of RunResult.PersistFailures:
// it counts artists whose run could not be fully WRITTEN, and nothing else.
//
// The distinction is not academic. The bulk walkers hand mergeContribution an
// `ok` bool that is false for TWO different reasons: a write failed, or
// evaluation bailed before any write was attempted. An earlier revision of
// #2724 incremented PersistFailures from that bool, which reported a
// persist failure for a run that never wrote anything -- telling the operator
// data was lost when none had been. These tests exist so that cannot come back.
func TestRunResult_PersistFailures(t *testing.T) {
	t.Run("evaluation failure is not a persist failure", func(t *testing.T) {
		db := setupTestDB(t)
		ctx := context.Background()

		ruleSvc := NewService(db)
		if err := ruleSvc.SeedDefaults(ctx); err != nil {
			t.Fatalf("SeedDefaults: %v", err)
		}

		realArtists, providers, members, aliases, images, platformIDs, completeness := artist.NewDefaultRepos(db)
		artistSvc := artist.NewServiceWithRepos(
			realArtists, providers, members, aliases, images, platformIDs, completeness,
		)

		a := &artist.Artist{Name: "Eval Fail", SortName: "Eval Fail", Path: t.TempDir()}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}
		if err := artistSvc.MarkDirty(ctx, a.ID, time.Now().UTC()); err != nil {
			t.Fatalf("MarkDirty: %v", err)
		}

		engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
		pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, testLogger())

		// Break evaluation itself: with the rules table gone, the engine
		// errors out before any write is attempted.
		if _, err := db.ExecContext(ctx, `DROP TABLE rules`); err != nil {
			t.Fatalf("dropping rules table: %v", err)
		}

		result, err := pipeline.RunAllScoped(ctx, RunScopeAll)
		if err != nil && !errors.Is(err, context.Canceled) {
			// A walker-level error is acceptable here; what matters is the
			// counter, which is only meaningful when a result came back.
			t.Logf("RunAllScoped returned: %v", err)
		}
		if result == nil {
			t.Skip("no result returned; nothing to assert about the counter")
		}

		// Nothing was ever written, so nothing can have failed to persist.
		if result.PersistFailures != 0 {
			t.Errorf("PersistFailures = %d, want 0 -- evaluation failed before any write was "+
				"attempted, so reporting a persist failure tells the operator data was lost "+
				"when none was", result.PersistFailures)
		}
	})

	t.Run("a failed write is a persist failure", func(t *testing.T) {
		db := setupTestDB(t)
		ctx := context.Background()

		ruleSvc := NewService(db)
		if err := ruleSvc.SeedDefaults(ctx); err != nil {
			t.Fatalf("SeedDefaults: %v", err)
		}
		disableAllRulesExcept(t, db, RuleBioExists)
		if _, err := db.ExecContext(ctx,
			`UPDATE rules SET automation_mode = ? WHERE id = ?`,
			AutomationModeAuto, RuleBioExists); err != nil {
			t.Fatalf("setting automation_mode=auto: %v", err)
		}

		// failingArtistRepo (fixer_persist_order_test.go) fails every Update,
		// which is what the health-score persist goes through.
		realArtists, providers, members, aliases, images, platformIDs, completeness := artist.NewDefaultRepos(db)
		artistSvc := artist.NewServiceWithRepos(
			&failingArtistRepo{Repository: realArtists},
			providers, members, aliases, images, platformIDs, completeness,
		)

		a := &artist.Artist{Name: "Write Fail", SortName: "Write Fail", Path: t.TempDir(), Biography: ""}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}
		if err := artistSvc.MarkDirty(ctx, a.ID, time.Now().UTC()); err != nil {
			t.Fatalf("MarkDirty: %v", err)
		}

		engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
		fixer := &mutatingBioFixer{mutated: "mutated"}
		pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

		result, err := pipeline.RunForArtist(ctx, a)
		if err != nil {
			t.Fatalf("RunForArtist: %v", err)
		}

		// Guard against a vacuous pass: if the fixer never ran, no write was
		// attempted and the assertion below would prove nothing.
		if fixer.fixCalls == 0 {
			t.Fatalf("fixer was never invoked; no write was attempted, so this test " +
				"cannot distinguish a persist failure from a no-op")
		}
		if result.PersistFailures == 0 {
			t.Error("PersistFailures = 0, want > 0 -- the artist Update failed, so the run " +
				"did not fully persist and must not report as clean")
		}
	})
}
