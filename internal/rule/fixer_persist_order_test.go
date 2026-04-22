package rule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// failingArtistRepo wraps a real artist.Repository and forces Update to return
// an error. Every other method delegates to the inner repo so normal test
// setup (Create, GetByID, MarkDirty, etc.) still works. This lets the test
// simulate a transient Update failure mid-pipeline without disturbing the rest
// of the persistence surface.
type failingArtistRepo struct {
	artist.Repository
}

// errForcedUpdate is the sentinel returned from Update. The pipeline logs the
// error and falls back to its log-and-continue contract, so tests assert on
// observable state (violation status, artist row, rule_results) rather than on
// the returned error.
var errForcedUpdate = errors.New("forced update failure")

// Update always fails. rules.Pipeline.updateHealthScore calls
// UpdateAfterRuleEvaluation -> artist.Service.update -> Repository.Update, so
// shadowing Update here is enough to simulate the persist failure that #983
// describes.
func (r *failingArtistRepo) Update(_ context.Context, _ *artist.Artist) error {
	return errForcedUpdate
}

// mutatingBioFixer is a test-only Fixer that handles bio_exists violations by
// setting the artist's Biography field in-memory and reporting Fixed=true. The
// real MetadataFixer relies on a provider chain; using a stub keeps the test
// hermetic and focused on the persistence-order invariant.
type mutatingBioFixer struct {
	mutated string // value written into a.Biography on Fix
}

func (f *mutatingBioFixer) CanFix(v *Violation) bool {
	return v.RuleID == RuleBioExists
}

func (f *mutatingBioFixer) Fix(_ context.Context, a *artist.Artist, v *Violation) (*FixResult, error) {
	a.Biography = f.mutated
	return &FixResult{
		RuleID:  v.RuleID,
		Fixed:   true,
		Message: "mutated by test fixer",
	}, nil
}

// TestPipeline_PersistFailureDoesNotResolveViolation is the regression guard
// for issue #983. All three walker entry points (RunForArtist, RunRule,
// RunAll) used to mark the rule_violations row as ViolationStatusResolved
// before calling updateHealthScore -- which is the one code path that
// persists the mutated artist. When the artist Update failed, the violation
// was already resolved and the artist row was unchanged: the fix silently
// disappeared and the next pass had no record that anything was wrong.
//
// The table covers each entry point. For each one, the pipeline is wired with
// a real SQLite DB, the rule pipeline and service running against a normal
// rule.Service, and an artist.Service whose Repository is wrapped by
// failingArtistRepo so every Update returns errForcedUpdate.
//
// Assertions:
//  1. The artist row reloaded from the DB must NOT have the mutated Biography
//     (the Update never reached the DB).
//  2. The violation row must NOT be ViolationStatusResolved. Any pre-fix
//     status (open) is acceptable -- the point is that the pipeline must not
//     claim the violation is resolved when the underlying fix was not
//     persisted.
//  3. rules_evaluated_at must not be stamped. That invariant is already held
//     by #698's persistOK flag; the assertion guards against regression.
//
// The entry-point API follows the existing log-and-continue contract (failures
// are warn-logged, not returned), so we do not assert on returned errors.
func TestPipeline_PersistFailureDoesNotResolveViolation(t *testing.T) {
	cases := []struct {
		name string
		// run invokes the pipeline entry point under test. Some take an
		// explicit artist pointer; others scan the whole catalog. The
		// caller supplies whichever arguments the entry point needs.
		run func(t *testing.T, p *Pipeline, a *artist.Artist)
	}{
		{
			name: "RunForArtist",
			run: func(t *testing.T, p *Pipeline, a *artist.Artist) {
				t.Helper()
				if _, err := p.RunForArtist(context.Background(), a); err != nil {
					t.Fatalf("RunForArtist: %v", err)
				}
			},
		},
		{
			name: "RunRule",
			run: func(t *testing.T, p *Pipeline, _ *artist.Artist) {
				t.Helper()
				// Scope=all so the walker touches the seeded artist
				// regardless of the dirty-tracker state (the forced
				// Update failure prevents the normal "mark clean"
				// pathway, but we also want the initial run to hit the
				// artist even when it happens to be clean).
				if _, err := p.RunRuleScoped(context.Background(), RuleBioExists, RunScopeAll); err != nil {
					t.Fatalf("RunRuleScoped: %v", err)
				}
			},
		},
		{
			name: "RunAll",
			run: func(t *testing.T, p *Pipeline, _ *artist.Artist) {
				t.Helper()
				if _, err := p.RunAllScoped(context.Background(), RunScopeAll); err != nil {
					t.Fatalf("RunAllScoped: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			ctx := context.Background()

			// Seed the rule service and leave only bio_exists enabled
			// in auto mode, so the mutating fixer runs and nothing
			// else introduces noise.
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

			// Build an artist.Service whose Repository wraps the real
			// sqlite repo and fails every Update. The other six repos
			// come straight from NewDefaultRepos so Create, provider
			// persistence, etc. all work as normal during setup.
			realArtists, providers, members, aliases, images, platformIDs, completeness := artist.NewDefaultRepos(db)
			artistSvc := artist.NewServiceWithRepos(
				&failingArtistRepo{Repository: realArtists},
				providers, members, aliases, images, platformIDs, completeness,
			)

			// Seed a single artist with an empty biography so
			// bio_exists reports a violation. Create goes straight
			// through to the underlying repo's Create (the decorator
			// only overrides Update), so the row is inserted as
			// expected.
			a := &artist.Artist{
				Name:      "Persist Order " + tc.name,
				SortName:  "Persist Order " + tc.name,
				Path:      t.TempDir(),
				Biography: "",
			}
			if err := artistSvc.Create(ctx, a); err != nil {
				t.Fatalf("creating artist: %v", err)
			}

			// Force the artist into the dirty set so the incremental
			// walker would pick it up too. The explicit mark is
			// harmless for RunScopeAll callers.
			if err := artistSvc.MarkDirty(ctx, a.ID, time.Now().UTC()); err != nil {
				t.Fatalf("MarkDirty: %v", err)
			}

			engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
			fixer := &mutatingBioFixer{mutated: "mutated"}
			pipeline := NewPipeline(engine, artistSvc, ruleSvc, []Fixer{fixer}, nil, testLogger())

			tc.run(t, pipeline, a)

			// Assertion 1: the mutated Biography must never reach the
			// DB. The failingArtistRepo refuses every Update, so the
			// row we read back must still carry the empty biography.
			reloaded, err := realArtists.GetByID(ctx, a.ID)
			if err != nil {
				t.Fatalf("reloading artist: %v", err)
			}
			if reloaded.Biography != "" {
				t.Errorf("artist.Biography = %q, want empty (Update should have failed and mutation should not have landed)", reloaded.Biography)
			}

			// Assertion 2: no violation row for this artist may be
			// ViolationStatusResolved. The fixer reported Fixed=true,
			// but the persist failed, so the pipeline MUST leave the
			// violation unresolved so the next pass retries.
			violations, err := ruleSvc.ListViolationsFiltered(ctx, ViolationListParams{ArtistID: a.ID})
			if err != nil {
				t.Fatalf("ListViolationsFiltered: %v", err)
			}
			for _, v := range violations {
				if v.RuleID != RuleBioExists {
					continue
				}
				if v.Status == ViolationStatusResolved {
					t.Errorf("violation for %s has Status=%q, want anything but %q -- resolve MUST NOT run before the artist Update succeeds",
						v.RuleID, v.Status, ViolationStatusResolved)
				}
			}

			// Assertion 3: rules_evaluated_at must still be nil. This
			// is already protected by #698's persistOK flag; the
			// check guards against a regression that would let the
			// walker stamp the artist clean despite a transient DB
			// failure on the main row.
			if reloaded.RulesEvaluatedAt != nil {
				t.Errorf("rules_evaluated_at = %v, want nil (walker must not stamp when persist failed)", reloaded.RulesEvaluatedAt)
			}
		})
	}
}
