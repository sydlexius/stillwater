package rule

import (
	"context"
	"log/slog"
	"time"
)

// Scheduler periodically runs all enabled rules via the pipeline.
type Scheduler struct {
	pipeline *Pipeline
	logger   *slog.Logger
}

// NewScheduler creates a rule scheduler.
func NewScheduler(pipeline *Pipeline, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		pipeline: pipeline,
		logger:   logger.With(slog.String("component", "rule-scheduler")),
	}
}

// Start blocks until the context is canceled, running all rules on each tick.
func (s *Scheduler) Start(ctx context.Context, interval time.Duration) {
	s.logger.Info("rule scheduler started", "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("rule scheduler stopped")
			return
		case <-ticker.C:
			s.logger.Info("rule scheduler running evaluation")
			result, err := s.pipeline.RunAll(ctx)
			if err != nil {
				s.logger.Error("scheduled rule evaluation failed", "error", err)
				continue
			}
			s.logger.Info("scheduled rule evaluation complete",
				"artists_processed", result.ArtistsProcessed,
				"violations_found", result.ViolationsFound,
				"fixes_attempted", result.FixesAttempted,
				"fixes_succeeded", result.FixesSucceeded,
			)
		}
	}
}
