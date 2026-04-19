package rule

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// fakePipelineRunner captures the context passed to RunRule so tests can
// assert that language preferences were injected before rule evaluation.
// Other PipelineRunner methods are no-op stubs -- the scheduler only calls
// RunRule.
type fakePipelineRunner struct {
	mu     sync.Mutex
	ctxSeq []context.Context
}

func (f *fakePipelineRunner) RunRule(ctx context.Context, _ string) (*RunResult, error) {
	f.mu.Lock()
	f.ctxSeq = append(f.ctxSeq, ctx)
	f.mu.Unlock()
	return &RunResult{}, nil
}

func (f *fakePipelineRunner) RunForArtist(_ context.Context, _ *artist.Artist) (*RunResult, error) {
	return &RunResult{}, nil
}

func (f *fakePipelineRunner) RunImageRulesForArtist(_ context.Context, _ *artist.Artist) (*RunResult, error) {
	return &RunResult{}, nil
}

func (f *fakePipelineRunner) RunAll(_ context.Context) (*RunResult, error) {
	return &RunResult{}, nil
}

func (f *fakePipelineRunner) FixViolation(_ context.Context, _ string) (*FixResult, error) {
	return &FixResult{}, nil
}

func (f *fakePipelineRunner) capturedContexts() []context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]context.Context, len(f.ctxSeq))
	copy(out, f.ctxSeq)
	return out
}

func TestScheduler_Reset(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	logger := slog.Default()
	engine := NewEngine(ruleSvc, nil, nil, nil, logger)
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, logger)
	sched := NewScheduler(pipeline, ruleSvc, artistSvc, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go sched.Start(ctx, 10*time.Second) // Long interval

	// Reset should not panic or block
	sched.Reset()

	// Verify scheduler is still alive
	time.Sleep(50 * time.Millisecond)
	cancel()
}

func TestScheduler_Status_BeforeRun(t *testing.T) {
	logger := slog.Default()
	pipeline := &Pipeline{logger: logger.With(slog.String("component", "fix-pipeline"))}
	sched := NewScheduler(pipeline, nil, nil, logger)

	status := sched.Status()
	if status.LastEvaluationAt != nil {
		t.Error("LastEvaluationAt should be nil before any run")
	}
}

func TestScheduler_Status_AfterRun(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	logger := slog.Default()
	engine := NewEngine(ruleSvc, nil, nil, nil, logger)
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, logger)
	sched := NewScheduler(pipeline, ruleSvc, artistSvc, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go sched.Start(ctx, 50*time.Millisecond)

	// Wait for at least one tick
	time.Sleep(200 * time.Millisecond)

	status := sched.Status()
	if status.LastEvaluationAt == nil {
		t.Fatal("LastEvaluationAt should be set after a tick")
	}
	if status.NextEvaluationAt == nil {
		t.Fatal("NextEvaluationAt should be set after a tick")
	}
	// IntervalMinutes will be 0 at sub-minute scale; just check LastEvaluationAt was set.
}

func TestScheduler_RecordsHealthSnapshot(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	logger := slog.Default()
	engine := NewEngine(ruleSvc, nil, nil, nil, logger)
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, logger)
	sched := NewScheduler(pipeline, ruleSvc, artistSvc, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go sched.Start(ctx, 50*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	cancel()

	var count int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM health_history").Scan(&count); err != nil {
		t.Fatalf("counting health_history rows: %v", err)
	}
	if count == 0 {
		t.Error("expected at least one health_history row after scheduled run")
	}
}

func TestScheduler_NonPositiveInterval(t *testing.T) {
	logger := slog.Default()
	pipeline := &Pipeline{logger: logger.With(slog.String("component", "fix-pipeline"))}
	sched := NewScheduler(pipeline, nil, nil, logger)

	// Start with zero interval should return immediately without panicking.
	done := make(chan struct{})
	go func() {
		sched.Start(context.Background(), 0)
		close(done)
	}()
	select {
	case <-done:
		// ok -- returned immediately
	case <-time.After(2 * time.Second):
		t.Fatal("Start(0) did not return promptly")
	}
}

func TestScheduler_ContextCancellation(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)

	logger := slog.Default()
	engine := NewEngine(ruleSvc, nil, nil, nil, logger)
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, logger)
	sched := NewScheduler(pipeline, ruleSvc, nil, logger)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		sched.Start(ctx, 1*time.Hour)
		close(done)
	}()

	// Cancel and verify scheduler stops promptly.
	cancel()
	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop promptly on context cancellation")
	}
}

func TestScheduler_SetLangPrefProvider_InjectsIntoRunRuleCtx(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	logger := slog.Default()

	// Seed a single enabled rule so runEnabledRules has something to
	// iterate. The PipelineRunner is a fake that only records the ctx it
	// receives, so the rule's config never executes.
	ctxInsert := context.Background()
	// The template DB ships with seeded system rules. Clear them so the
	// only enabled rule this tick sees is the one we insert, which keeps
	// assertions (single RunRule invocation) tight.
	if _, err := db.ExecContext(ctxInsert, `DELETE FROM rules`); err != nil {
		t.Fatalf("clearing seeded rules: %v", err)
	}
	if _, err := db.ExecContext(ctxInsert, `
		INSERT INTO rules (id, name, description, category, enabled)
		VALUES (?, ?, ?, ?, 1)
	`, "test-rule", "test", "", "metadata"); err != nil {
		t.Fatalf("seeding rule: %v", err)
	}

	fake := &fakePipelineRunner{}
	sched := NewScheduler(fake, ruleSvc, nil, logger)

	want := []string{"en-US", "en-GB", "en"}
	var providerCalls int
	sched.SetLangPrefProvider(func(context.Context) []string {
		providerCalls++
		return want
	})

	sched.runEnabledRules(context.Background())

	if providerCalls != 1 {
		t.Errorf("LangPrefProvider calls = %d, want 1 per tick", providerCalls)
	}

	captured := fake.capturedContexts()
	if len(captured) != 1 {
		t.Fatalf("pipeline RunRule invocations = %d, want 1", len(captured))
	}
	got := provider.MetadataLanguages(captured[0])
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ctx lang prefs = %v, want %v", got, want)
	}
}

func TestScheduler_NoLangPrefProvider_CtxLeftUnchanged(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	logger := slog.Default()

	ctxInsert := context.Background()
	// The template DB ships with seeded system rules. Clear them so the
	// only enabled rule this tick sees is the one we insert, which keeps
	// assertions (single RunRule invocation) tight.
	if _, err := db.ExecContext(ctxInsert, `DELETE FROM rules`); err != nil {
		t.Fatalf("clearing seeded rules: %v", err)
	}
	if _, err := db.ExecContext(ctxInsert, `
		INSERT INTO rules (id, name, description, category, enabled)
		VALUES (?, ?, ?, ?, 1)
	`, "test-rule", "test", "", "metadata"); err != nil {
		t.Fatalf("seeding rule: %v", err)
	}

	fake := &fakePipelineRunner{}
	sched := NewScheduler(fake, ruleSvc, nil, logger)
	// Do not call SetLangPrefProvider: the scheduler must behave as
	// pre-#1136 (no injection) so existing deployments keep working.

	sched.runEnabledRules(context.Background())

	captured := fake.capturedContexts()
	if len(captured) != 1 {
		t.Fatalf("pipeline RunRule invocations = %d, want 1", len(captured))
	}
	if got := provider.MetadataLanguages(captured[0]); got != nil {
		t.Errorf("ctx lang prefs = %v, want nil (no provider set)", got)
	}
}

func TestScheduler_LangPrefProvider_EmptySliceDoesNotInject(t *testing.T) {
	db := setupTestDB(t)
	ruleSvc := NewService(db)
	logger := slog.Default()

	ctxInsert := context.Background()
	// The template DB ships with seeded system rules. Clear them so the
	// only enabled rule this tick sees is the one we insert, which keeps
	// assertions (single RunRule invocation) tight.
	if _, err := db.ExecContext(ctxInsert, `DELETE FROM rules`); err != nil {
		t.Fatalf("clearing seeded rules: %v", err)
	}
	if _, err := db.ExecContext(ctxInsert, `
		INSERT INTO rules (id, name, description, category, enabled)
		VALUES (?, ?, ?, ?, 1)
	`, "test-rule", "test", "", "metadata"); err != nil {
		t.Fatalf("seeding rule: %v", err)
	}

	fake := &fakePipelineRunner{}
	sched := NewScheduler(fake, ruleSvc, nil, logger)
	sched.SetLangPrefProvider(func(context.Context) []string { return nil })

	sched.runEnabledRules(context.Background())

	captured := fake.capturedContexts()
	if len(captured) != 1 {
		t.Fatalf("pipeline RunRule invocations = %d, want 1", len(captured))
	}
	if got := provider.MetadataLanguages(captured[0]); got != nil {
		t.Errorf("ctx lang prefs = %v, want nil for empty provider result", got)
	}
}

func TestScheduler_TickTriggersRun(t *testing.T) {
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	logger := slog.Default()
	engine := NewEngine(ruleSvc, nil, nil, nil, logger)
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, nil, logger)
	sched := NewScheduler(pipeline, ruleSvc, nil, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		// Use a very short interval so the first tick fires quickly.
		sched.Start(ctx, 50*time.Millisecond)
		close(done)
	}()

	// Wait for at least one tick to complete, then cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
		// ok -- scheduler ran at least once and stopped
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not complete after tick")
	}
}
