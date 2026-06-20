package provider

import (
	"log/slog"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// AIMD tuning constants. These are locked values, not suggestions.
const (
	// aimdIncrement is the additive-increase step applied to the current rate
	// limit (in requests per second) after aimdSuccessThreshold consecutive
	// successful provider calls.
	aimdIncrement = rate.Limit(0.5)

	// aimdDecreaseFactor is the multiplicative-decrease factor applied to the
	// current rate limit when a provider signals rate-limiting (429 / 503 with
	// a RetryAfter). The limit is halved on each decrease, floored at the
	// provider's configured default.
	aimdDecreaseFactor = 0.5

	// aimdSuccessThreshold is the number of consecutive successful provider
	// calls required before one additive increase is applied.
	aimdSuccessThreshold = 10

	// aimdHysteresisCooldown is the minimum interval between two consecutive
	// multiplicative decreases for the same provider. Decreases that arrive
	// within this window are ignored to prevent thrashing on a burst of 429s.
	aimdHysteresisCooldown = 30 * time.Second

	// aimdDefaultCeilingMultiplier sets the default per-provider ceiling as a
	// multiple of the provider's default rate limit. An explicit ceiling set via
	// SetCeiling overrides this.
	aimdDefaultCeilingMultiplier = 10
)

// aimdState tracks the adaptive rate-limit state for a single provider.
type aimdState struct {
	// currentLimit is the active rate limit (req/s) managed by this controller.
	// It starts at the provider's default and is adjusted by AIMD logic.
	currentLimit rate.Limit
	// ceiling is the maximum rate limit this provider is allowed to reach.
	// Additive increases are capped here.
	ceiling rate.Limit
	// lastDecrease records when the most recent multiplicative decrease was
	// applied. It is used to enforce the hysteresis cooldown.
	lastDecrease time.Time
	// successCount is the number of consecutive successes since the last
	// increase or decrease. When it reaches aimdSuccessThreshold, one additive
	// increase is applied and successCount resets to zero.
	successCount int
}

// AIMDController implements additive-increase / multiplicative-decrease
// adaptive rate limiting for the provider layer. It wraps a RateLimiterMap and
// adjusts each provider's rate limit in response to success and failure signals
// from the orchestrator.
//
// All methods are safe for concurrent use.
type AIMDController struct {
	mu     sync.RWMutex
	rlm    *RateLimiterMap
	clock  Clock
	states map[ProviderName]*aimdState
	logger *slog.Logger
}

// NewAIMDController creates a new AIMDController backed by rlm. The clock
// parameter is used for hysteresis checks; pass SystemClock() in production
// and a controllable fake in tests.
func NewAIMDController(rlm *RateLimiterMap, clock Clock) *AIMDController {
	return &AIMDController{
		rlm:    rlm,
		clock:  clock,
		states: make(map[ProviderName]*aimdState),
		logger: slog.Default().With(slog.String("component", "aimd")),
	}
}

// stateFor returns the aimdState for name, initializing it lazily on first
// access. The caller must hold at least a write lock.
func (c *AIMDController) stateFor(name ProviderName) *aimdState {
	if s, ok := c.states[name]; ok {
		return s
	}
	floor := defaultRateLimits[name]
	ceiling := rate.Limit(float64(floor) * aimdDefaultCeilingMultiplier)
	if ceiling <= 0 {
		// Unknown provider or zero-floor: use a safe default so the controller
		// doesn't divide by zero or produce a nonsensical ceiling.
		ceiling = aimdIncrement * aimdDefaultCeilingMultiplier
	}
	s := &aimdState{
		currentLimit: floor,
		ceiling:      ceiling,
	}
	c.states[name] = s
	return s
}

// RecordSuccess signals that a provider call succeeded. After
// aimdSuccessThreshold consecutive successes, the current rate limit is
// increased by aimdIncrement (capped at ceiling) and pushed into the
// underlying RateLimiterMap.
func (c *AIMDController) RecordSuccess(name ProviderName) {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.stateFor(name)
	s.successCount++
	if s.successCount < aimdSuccessThreshold {
		return
	}

	// Threshold reached: apply one additive increase.
	s.successCount = 0
	newLimit := s.currentLimit + aimdIncrement
	if newLimit > s.ceiling {
		newLimit = s.ceiling
	}
	if newLimit == s.currentLimit {
		// Already at ceiling; nothing to do.
		return
	}
	s.currentLimit = newLimit
	c.rlm.SetLimit(name, newLimit)
	c.logger.Info("aimd: rate limit increased",
		slog.String("provider", string(name)),
		slog.Float64("new_limit", float64(newLimit)),
	)
}

// RecordFailure signals that a provider call failed with a rate-limit or
// server-unavailable error. It applies a multiplicative decrease to the
// current rate limit, floored at the provider's default, and resets the
// success counter. A hysteresis guard prevents more than one decrease per
// aimdHysteresisCooldown window to avoid thrashing on a burst of 429s.
//
// retryAfter is the server-advised backoff duration from the error (may be 0
// if the server provided no hint); it is available for future observability
// but the core decrease logic uses the multiplicative factor and floor only.
func (c *AIMDController) RecordFailure(name ProviderName, retryAfter time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.stateFor(name)
	now := c.clock.Now()

	// Hysteresis: ignore a decrease if one was applied within the cooldown
	// window. This prevents a burst of 429s from repeatedly halving the limit
	// in quick succession.
	if !s.lastDecrease.IsZero() && now.Sub(s.lastDecrease) < aimdHysteresisCooldown {
		return
	}

	floor := defaultRateLimits[name]
	newLimit := rate.Limit(float64(s.currentLimit) * aimdDecreaseFactor)
	if newLimit < floor {
		newLimit = floor
	}
	s.currentLimit = newLimit
	s.successCount = 0
	s.lastDecrease = now
	c.rlm.SetLimit(name, newLimit)
	c.logger.Info("aimd: rate limit decreased",
		slog.String("provider", string(name)),
		slog.Float64("new_limit", float64(newLimit)),
		slog.Duration("retry_after", retryAfter),
	)
}

// SetCeiling overrides the rate limit ceiling for a provider. The ceiling caps
// how high additive increases can push the limit. A ceiling of zero or less
// reverts to the default (aimdDefaultCeilingMultiplier * floor).
func (c *AIMDController) SetCeiling(name ProviderName, ceiling rate.Limit) {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.stateFor(name)
	if ceiling <= 0 {
		floor := defaultRateLimits[name]
		ceiling = rate.Limit(float64(floor) * aimdDefaultCeilingMultiplier)
	}
	s.ceiling = ceiling
}

// GetCeiling returns the current ceiling for a provider, initializing state
// lazily if needed.
func (c *AIMDController) GetCeiling(name ProviderName) rate.Limit {
	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.stateFor(name)
	return s.ceiling
}
