package rule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// cancelingBioFixer reproduces the production race precisely: the caller's
// context is canceled DURING the fix-and-persist phase, not before the run.
// On production the trigger was a page reload aborting the in-flight
// run-rules request after evaluation had already completed.
//
// Canceling from inside Fix is what makes this faithful. Canceling before
// the entry point instead kills evaluation ("listing rules: context
// canceled"), which is a different failure and would drive a much broader fix.
type cancelingBioFixer struct {
	cancel   context.CancelFunc
	mutated  string
	fixCalls int
}

func (f *cancelingBioFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleBioExists
}

func (f *cancelingBioFixer) Fix(_ context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	f.fixCalls++
	// The client goes away mid-fix.
	f.cancel()
	a.Biography = f.mutated
	return &FixResult{
		RuleID:  v.RuleID,
		Fixed:   true,
		Message: "mutated by canceling test fixer",
	}, nil
}

// TestPipeline_CanceledContextStillPersists is the regression guard for
// issue #2724.
//
// The three walkers each threaded the caller's context -- in production the
// HTTP request context -- through the fix-and-persist phase as well as the
// evaluation phase. A client that went away mid-run (a page reload, an HTMX
// swap) canceled that context and every subsequent write failed:
//
//	WARN persisting fix result violation ... error="beginning upsert-violation
//	     transaction: context canceled"
//	WARN re-evaluating health score      ... error="listing rules: context canceled"
//
// while the handler still returned 200. The operator was told the run
// succeeded and nothing was written. Measured on production 2026-07-22.
//
// The fix mirrors the precedent in api.executeRefreshCtx: evaluation stays
// cancellable, but once evaluation has produced violations the per-artist
// fix-and-persist phase runs to completion under context.WithoutCancel.
//
// All three entry points are covered because they have three separate
// implementations of the same shape -- fixing one and not the others would
// leave the bug live on the untested paths.
func TestPipeline_CanceledContextStillPersists(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, p *Pipeline, ctx context.Context, a *artist.Artist)
	}{
		{
			name: "RunForArtist",
			run: func(t *testing.T, p *Pipeline, ctx context.Context, a *artist.Artist) {
				t.Helper()
				if _, err := p.RunForArtist(ctx, a); err != nil {
					t.Fatalf("RunForArtist: %v", err)
				}
			},
		},
		// The bulk walkers legitimately return context.Canceled: once the
		// in-flight artist finishes, the OUTER walk loop observes the
		// cancellation and stops the pass. That is the desired behavior --
		// the operator's cancel is honored at an artist boundary. What must
		// still hold is that the artist already being written completed.
		// Any other error is a real failure.
		{
			name: "RunRuleScoped",
			run: func(t *testing.T, p *Pipeline, ctx context.Context, _ *artist.Artist) {
				t.Helper()
				if _, err := p.RunRuleScoped(ctx, RuleBioExists, RunScopeAll); err != nil &&
					!errors.Is(err, context.Canceled) {
					t.Fatalf("RunRuleScoped: %v", err)
				}
			},
		},
		{
			name: "RunAllScoped",
			run: func(t *testing.T, p *Pipeline, ctx context.Context, _ *artist.Artist) {
				t.Helper()
				if _, err := p.RunAllScoped(ctx, RunScopeAll); err != nil &&
					!errors.Is(err, context.Canceled) {
					t.Fatalf("RunAllScoped: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			setupCtx := context.Background()

			// Leave only bio_exists enabled, in auto mode, so the mutating
			// fixer runs and nothing else adds noise.
			ruleSvc := NewService(db)
			if err := ruleSvc.SeedDefaults(setupCtx); err != nil {
				t.Fatalf("SeedDefaults: %v", err)
			}
			disableAllRulesExcept(t, db, RuleBioExists)
			if _, err := db.ExecContext(setupCtx,
				`UPDATE rules SET automation_mode = ? WHERE id = ?`,
				AutomationModeAuto, RuleBioExists); err != nil {
				t.Fatalf("setting automation_mode=auto: %v", err)
			}

			realArtists, providers, members, aliases, images, platformIDs, completeness := artist.NewDefaultRepos(db)
			artistSvc := artist.NewServiceWithRepos(
				realArtists, providers, members, aliases, images, platformIDs, completeness,
			)

			a := &artist.Artist{
				Name:      "Canceled Ctx " + tc.name,
				SortName:  "Canceled Ctx " + tc.name,
				Path:      t.TempDir(),
				Biography: "",
			}
			if err := artistSvc.Create(setupCtx, a); err != nil {
				t.Fatalf("creating artist: %v", err)
			}
			if err := artistSvc.MarkDirty(setupCtx, a.ID, time.Now().UTC()); err != nil {
				t.Fatalf("MarkDirty: %v", err)
			}

			// The context is live when the run starts, so evaluation
			// succeeds, and the fixer cancels it partway through the
			// fix-and-persist phase.
			runCtx, cancel := context.WithCancel(context.Background())
			defer cancel()

			engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
			fixer := &cancelingBioFixer{cancel: cancel, mutated: "mutated"}
			pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

			tc.run(t, pipeline, runCtx, a)

			// Guard against a vacuous pass: if evaluation bailed out before
			// ever reaching a fixer, the assertions below would prove nothing
			// about the persist phase.
			if fixer.fixCalls == 0 {
				t.Fatalf("fixer was never invoked; the test did not exercise the fix-and-persist path")
			}

			// The fix must have reached the database despite the canceled
			// caller context.
			reloaded, err := realArtists.GetByID(setupCtx, a.ID)
			if err != nil {
				t.Fatalf("reloading artist: %v", err)
			}
			if reloaded.Biography != "mutated" {
				t.Errorf("artist.Biography = %q, want %q -- the fix was lost to the canceled context",
					reloaded.Biography, "mutated")
			}

			// The violation bookkeeping must have landed too. A run that
			// fixes the artist but cannot record it leaves the next pass with
			// no history, which is the second half of what #2724 observed.
			var violationRows int
			if err := db.QueryRowContext(setupCtx,
				`SELECT COUNT(*) FROM rule_violations WHERE artist_id = ?`, a.ID).Scan(&violationRows); err != nil {
				t.Fatalf("counting violation rows: %v", err)
			}
			if violationRows == 0 {
				t.Error("no rule_violations row was written; the violation upsert was lost to the canceled context")
			}
		})
	}
}
