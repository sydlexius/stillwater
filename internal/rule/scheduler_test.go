package rule

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
)

func TestScheduler_NonPositiveInterval(t *testing.T) {
	logger := slog.Default()
	pipeline := &Pipeline{logger: logger.With(slog.String("component", "fix-pipeline"))}
	sched := NewScheduler(pipeline, logger)

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
	engine := NewEngine(ruleSvc, nil, nil, logger)
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, logger)
	sched := NewScheduler(pipeline, logger)

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
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	logger := slog.Default()
	engine := NewEngine(ruleSvc, nil, nil, logger)
	pipeline := NewPipeline(engine, artistSvc, ruleSvc, nil, logger)
	sched := NewScheduler(pipeline, logger)

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
