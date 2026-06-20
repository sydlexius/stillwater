package api

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// TestEnsureWizardCandidates_ProviderFailureRoutesToStepFailed pins the
// behavior added for issue #1663: with the new SearchForLinking signature
// per-provider failures arrive on a `statuses` slice instead of via the
// function-level error. The wizard's single-provider lookup must still
// translate a fully-errored result into wizardStepFailed so the existing
// Retry-banner UX keeps working; without this preservation the user would
// see an empty candidate list with no indication that MusicBrainz failed.
func TestEnsureWizardCandidates_ProviderFailureRoutesToStepFailed(t *testing.T) {
	t.Parallel()

	r, _, artistSvc := testRouterWithIdentify(t)
	// Seed a real artist row so GetByID does not short-circuit.
	a := &artist.Artist{
		Name:     "Wizard Provider Failure UAT",
		SortName: "Wizard Provider Failure UAT",
		Type:     "person",
		Path:     "/music/Wizard Provider Failure UAT",
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("seed artist: %v", err)
	}

	// Wire an orchestrator whose only registered provider errors.
	registry := provider.NewRegistry()
	registry.Register(&identifyStubProvider{
		name: provider.NameMusicBrainz,
		searchFn: func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			return nil, errors.New("musicbrainz.org: tls handshake failed")
		},
	})
	r.providerRegistry = registry
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r.orchestrator = provider.NewOrchestrator(registry, nil, logger, nil)

	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{
		{ArtistID: a.ID, ArtistName: a.Name, ArtistPath: a.Path},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Run synchronously so we can assert without sleeping.
	r.ensureWizardCandidates(context.Background(), sess, 0)

	sess.mu.Lock()
	defer sess.mu.Unlock()
	step := sess.Steps[0]
	if step.state != wizardStepFailed {
		t.Errorf("step state = %v, want wizardStepFailed; errMsg=%q", step.state, step.errMsg)
	}
	if step.errMsg == "" {
		t.Errorf("expected non-empty errMsg on failed step")
	}
}

// TestEnsureWizardCandidates_AllProvidersOKReadiesStep covers the
// happy path through the new statuses-aware branch: when every queried
// provider succeeds (even with zero results), the step must reach
// wizardStepReady, not wizardStepFailed. This is the negative case for
// the new "all queried providers errored" guard so a future change to
// the guard cannot silently break the no-match-but-not-errored flow.
func TestEnsureWizardCandidates_AllProvidersOKReadiesStep(t *testing.T) {
	t.Parallel()

	r, _, artistSvc := testRouterWithIdentify(t)
	a := &artist.Artist{
		Name:     "Wizard Provider OK UAT",
		SortName: "Wizard Provider OK UAT",
		Type:     "person",
		Path:     "/music/Wizard Provider OK UAT",
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("seed artist: %v", err)
	}

	registry := provider.NewRegistry()
	registry.Register(&identifyStubProvider{
		name: provider.NameMusicBrainz,
		searchFn: func(_ context.Context, _ string) ([]provider.ArtistSearchResult, error) {
			// Success with one result keeps the assertion specific to
			// the state transition rather than any candidate post-
			// processing.
			return []provider.ArtistSearchResult{
				{Name: "Wizard Provider OK UAT", MusicBrainzID: "mb-1", Score: 80},
			}, nil
		},
	})
	r.providerRegistry = registry
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r.orchestrator = provider.NewOrchestrator(registry, nil, logger, nil)

	sess, err := r.reIdentifyWizardStore.create([]*reIdentifyWizardStep{
		{ArtistID: a.ID, ArtistName: a.Name, ArtistPath: a.Path},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	r.ensureWizardCandidates(context.Background(), sess, 0)

	sess.mu.Lock()
	defer sess.mu.Unlock()
	step := sess.Steps[0]
	if step.state != wizardStepReady {
		t.Errorf("step state = %v, want wizardStepReady; errMsg=%q", step.state, step.errMsg)
	}
	if len(step.Candidates) != 1 {
		t.Errorf("expected 1 candidate on ready step, got %d", len(step.Candidates))
	}
}
