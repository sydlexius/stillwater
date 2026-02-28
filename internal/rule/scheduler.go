package rule

import (
	"context"
	"log/slog"
	"time"
)

// Scheduler periodically runs enabled rules via the pipeline, respecting
// each rule's automation mode.
type Scheduler struct {
	pipeline    *Pipeline
	ruleService *Service
	logger      *slog.Logger
}

// NewScheduler creates a rule scheduler.
func NewScheduler(pipeline *Pipeline, ruleService *Service, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		pipeline:    pipeline,
		ruleService: ruleService,
		logger:      logger.With(slog.String("component", "rule-scheduler")),
	}
}

// Start blocks until the context is canceled, running enabled rules on each tick.
// Each rule is evaluated via RunRule, which respects AutomationMode.
func (s *Scheduler) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		s.logger.Error("rule scheduler not started: non-positive interval", "interval", interval.String())
		return
	}
	s.logger.Info("rule scheduler started", "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("rule scheduler stopped")
			return
		case <-ticker.C:
			s.runEnabledRules(ctx)
		}
	}
}

func (s *Scheduler) runEnabledRules(ctx context.Context) {
	s.logger.Info("rule scheduler running evaluation")

	rules, err := s.ruleService.List(ctx)
	if err != nil {
		s.logger.Error("scheduled rule evaluation: listing rules", "error", err)
		return
	}

	var totalResult RunResult
	for _, r := range rules {
		if ctx.Err() != nil {
			break
		}
		if !r.Enabled {
			continue
		}

		result, err := s.pipeline.RunRule(ctx, r.ID)
		if err != nil {
			s.logger.Error("scheduled rule evaluation failed", "rule_id", r.ID, "error", err)
			continue
		}
		totalResult.ArtistsProcessed += result.ArtistsProcessed
		totalResult.ViolationsFound += result.ViolationsFound
		totalResult.FixesAttempted += result.FixesAttempted
		totalResult.FixesSucceeded += result.FixesSucceeded
	}

	s.logger.Info("scheduled rule evaluation complete",
		"artists_processed", totalResult.ArtistsProcessed,
		"violations_found", totalResult.ViolationsFound,
		"fixes_attempted", totalResult.FixesAttempted,
		"fixes_succeeded", totalResult.FixesSucceeded,
	)
}
