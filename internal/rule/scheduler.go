package rule

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

// SchedulerStatus holds scheduler state fields. The handler adds
// scheduler_enabled before writing the JSON response.
type SchedulerStatus struct {
	LastEvaluationAt *time.Time `json:"last_evaluation_at"`
	IntervalMinutes  int        `json:"interval_minutes"`
	NextEvaluationAt *time.Time `json:"next_evaluation_at"`
}

// Scheduler periodically runs enabled rules via the pipeline, respecting
// each rule's automation mode. It records a health snapshot after each run
// and supports timer reset from external triggers (e.g. manual "Run Rules").
type Scheduler struct {
	pipeline      PipelineRunner
	ruleService   *Service
	artistService *artist.Service
	logger        *slog.Logger

	interval   time.Duration
	resetCh    chan struct{}
	mu         sync.RWMutex
	lastRunAt  time.Time
	nextTickAt time.Time
}

// NewScheduler creates a rule scheduler. The artistService may be nil if
// health snapshot recording is not needed (e.g. in tests).
func NewScheduler(pipeline PipelineRunner, ruleService *Service, artistService *artist.Service, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		pipeline:      pipeline,
		ruleService:   ruleService,
		artistService: artistService,
		logger:        logger.With(slog.String("component", "rule-scheduler")),
		resetCh:       make(chan struct{}, 1),
	}
}

// Start blocks until the context is canceled, running enabled rules on each tick.
// Each rule is evaluated via RunRule, which respects AutomationMode.
func (s *Scheduler) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		s.logger.Error("rule scheduler not started: non-positive interval", "interval", interval.String())
		return
	}
	// Store interval under the mutex so Status() can read it safely.
	s.mu.Lock()
	s.interval = interval
	s.nextTickAt = time.Now().Add(interval)
	s.mu.Unlock()

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
			s.mu.Lock()
			s.nextTickAt = time.Now().Add(s.interval)
			s.mu.Unlock()
		case <-s.resetCh:
			ticker.Reset(s.interval)
			s.mu.Lock()
			s.nextTickAt = time.Now().Add(s.interval)
			s.mu.Unlock()
			s.logger.Info("rule scheduler timer reset")
		}
	}
}

// Reset restarts the scheduler timer. Call this after a manual rule run
// so the next scheduled tick starts a full interval from now.
func (s *Scheduler) Reset() {
	select {
	case s.resetCh <- struct{}{}:
	default:
		// already pending
	}
}

// Status returns the current scheduler state for the status endpoint.
func (s *Scheduler) Status() SchedulerStatus {
	s.mu.RLock()
	lastRun := s.lastRunAt
	nextTick := s.nextTickAt
	intervalMins := int(s.interval.Minutes())
	s.mu.RUnlock()

	status := SchedulerStatus{
		IntervalMinutes: intervalMins,
	}
	if !lastRun.IsZero() {
		status.LastEvaluationAt = &lastRun
	}
	if !nextTick.IsZero() {
		status.NextEvaluationAt = &nextTick
	}
	return status
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

	// Record lastRunAt before the health snapshot so Status() reflects the
	// completed evaluation even if the snapshot write is slow or fails.
	s.mu.Lock()
	s.lastRunAt = time.Now().UTC()
	s.mu.Unlock()

	// Record health snapshot after the run completes
	if s.artistService != nil && s.ruleService != nil {
		stats, err := s.artistService.GetHealthStats(ctx, "")
		if err != nil {
			s.logger.Warn("fetching health stats for snapshot", "error", err)
		} else {
			if err := s.ruleService.RecordHealthSnapshot(ctx, stats.TotalArtists, stats.CompliantArtists, stats.Score); err != nil {
				s.logger.Warn("recording health snapshot after scheduled run", "error", err)
			}
		}
	}
}
