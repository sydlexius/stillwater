package rule

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

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
