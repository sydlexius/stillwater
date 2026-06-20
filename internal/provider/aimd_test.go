package provider

import (
	"context"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// fakeClock is a controllable Clock implementation for AIMD tests. It avoids
// any real wall-clock dependency so tests are deterministic and instantaneous.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Sleep does not actually sleep; it is a no-op in tests. AIMD tests do not
// exercise the sleep path, so this keeps the interface satisfied.
func (f *fakeClock) Sleep(_ context.Context, _ time.Duration) error { return nil }

// advance moves the fake clock forward by d.
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// --- helpers ------------------------------------------------------------------

// newTestAIMD builds an AIMDController backed by a fresh RateLimiterMap and a
// fakeClock. Tests inspect state via the controller itself using aimdCurrentLimit.
func newTestAIMD(clk *fakeClock) *AIMDController {
	rlm := NewRateLimiterMap()
	return NewAIMDController(rlm, clk)
}

// currentLimit reads the current rate limit for name by forcing a lookup
// through SetLimit (which replaces the limiter) and then measuring the rate.
// We use the RateLimiterMap's internal state indirectly via the AIMD state.
func aimdCurrentLimit(ctrl *AIMDController, name ProviderName) rate.Limit {
	ctrl.mu.RLock()
	s, ok := ctrl.states[name]
	ctrl.mu.RUnlock()
	if !ok {
		return defaultRateLimits[name]
	}
	return s.currentLimit
}

// --- tests --------------------------------------------------------------------

// TestAIMDSuccessRamp verifies that aimdSuccessThreshold consecutive successes
// trigger exactly one additive increase and then the counter resets.
func TestAIMDSuccessRamp(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Now())
	ctrl := newTestAIMD(clk)
	name := NameMusicBrainz // floor = 1 req/s

	floor := defaultRateLimits[name]
	expected := floor + aimdIncrement

	// Record one fewer than the threshold -- no increase yet.
	for i := range aimdSuccessThreshold - 1 {
		ctrl.RecordSuccess(name)
		got := aimdCurrentLimit(ctrl, name)
		if got != floor {
			t.Fatalf("iteration %d: expected limit %v before threshold, got %v", i, floor, got)
		}
	}

	// One more success crosses the threshold.
	ctrl.RecordSuccess(name)
	got := aimdCurrentLimit(ctrl, name)
	if got != expected {
		t.Fatalf("expected limit %v after threshold, got %v", expected, got)
	}

	// Counter should have reset; the next (threshold-1) successes must not
	// produce another increase.
	for i := range aimdSuccessThreshold - 1 {
		ctrl.RecordSuccess(name)
		got = aimdCurrentLimit(ctrl, name)
		if got != expected {
			t.Fatalf("iteration %d: expected limit %v after reset, got %v", i, expected, got)
		}
	}
}

// TestAIMDSuccessCappedAtCeiling verifies that additive increases stop at the ceiling.
func TestAIMDSuccessCappedAtCeiling(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Now())
	ctrl := newTestAIMD(clk)
	name := NameMusicBrainz
	floor := defaultRateLimits[name]

	// Set ceiling just above floor so one increase hits it and subsequent
	// increases are clamped.
	ceiling := floor + aimdIncrement
	ctrl.SetCeiling(name, ceiling)

	// Trigger the first increase.
	for range aimdSuccessThreshold {
		ctrl.RecordSuccess(name)
	}
	got := aimdCurrentLimit(ctrl, name)
	if got != ceiling {
		t.Fatalf("expected limit to equal ceiling %v, got %v", ceiling, got)
	}

	// Trigger a second batch of successes; limit must stay at ceiling.
	for range aimdSuccessThreshold {
		ctrl.RecordSuccess(name)
	}
	got = aimdCurrentLimit(ctrl, name)
	if got != ceiling {
		t.Fatalf("expected limit to remain at ceiling %v after second batch, got %v", ceiling, got)
	}
}

// TestAIMDMultiplicativeDecrease verifies that RecordFailure halves the limit
// and floors it at the provider default.
func TestAIMDMultiplicativeDecrease(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Now())
	ctrl := newTestAIMD(clk)
	name := NameFanartTV // floor = 3 req/s

	floor := defaultRateLimits[name]

	// First, pump the limit up above floor by recording enough successes.
	// ceiling defaults to floor * 10.
	for range aimdSuccessThreshold {
		ctrl.RecordSuccess(name)
	}
	before := aimdCurrentLimit(ctrl, name)
	if before <= floor {
		t.Fatalf("expected limit above floor %v after successes, got %v", floor, before)
	}

	// Record a failure. Limit should halve.
	ctrl.RecordFailure(name, 0)
	afterFirst := aimdCurrentLimit(ctrl, name)
	expectedFirst := rate.Limit(float64(before) * aimdDecreaseFactor)
	if expectedFirst < floor {
		expectedFirst = floor
	}
	if afterFirst != expectedFirst {
		t.Fatalf("expected limit %v after first failure, got %v", expectedFirst, afterFirst)
	}
}

// TestAIMDDecreaseFloored verifies that repeated decreases never drop below the
// provider's configured floor, even when starting at or near the floor.
func TestAIMDDecreaseFloored(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Now())
	ctrl := newTestAIMD(clk)
	name := NameMusicBrainz // floor = 1 req/s

	floor := defaultRateLimits[name]

	// Force the clock well past the cooldown between decreases.
	for range 5 {
		clk.advance(aimdHysteresisCooldown + time.Second)
		ctrl.RecordFailure(name, 0)
		got := aimdCurrentLimit(ctrl, name)
		if got < floor {
			t.Fatalf("limit %v dropped below floor %v", got, floor)
		}
	}
	// Limit must still be at or above floor.
	got := aimdCurrentLimit(ctrl, name)
	if got < floor {
		t.Fatalf("final limit %v below floor %v", got, floor)
	}
}

// TestAIMDHysteresisWithinCooldown verifies that a second failure within the
// hysteresis cooldown window does NOT produce another decrease.
func TestAIMDHysteresisWithinCooldown(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Now())
	ctrl := newTestAIMD(clk)
	name := NameFanartTV // floor = 3 req/s

	// Drive the limit up first so there is room to decrease.
	for range aimdSuccessThreshold {
		ctrl.RecordSuccess(name)
	}
	before := aimdCurrentLimit(ctrl, name)

	// First failure: should decrease.
	ctrl.RecordFailure(name, 0)
	afterFirst := aimdCurrentLimit(ctrl, name)
	if afterFirst >= before {
		t.Fatalf("expected limit to decrease from %v; got %v", before, afterFirst)
	}

	// Second failure immediately (no clock advance): should NOT decrease.
	ctrl.RecordFailure(name, 0)
	afterSecond := aimdCurrentLimit(ctrl, name)
	if afterSecond != afterFirst {
		t.Fatalf("hysteresis: expected limit to stay at %v within cooldown, got %v", afterFirst, afterSecond)
	}

	// Advance past the cooldown; now a failure should decrease again.
	clk.advance(aimdHysteresisCooldown + time.Millisecond)
	ctrl.RecordFailure(name, 0)
	afterThird := aimdCurrentLimit(ctrl, name)
	// afterFirst may already be at floor -- if so, another decrease is clamped.
	floor := defaultRateLimits[name]
	expectedThird := rate.Limit(float64(afterFirst) * aimdDecreaseFactor)
	if expectedThird < floor {
		expectedThird = floor
	}
	if afterThird != expectedThird {
		t.Fatalf("expected limit %v after cooldown-expiry failure, got %v", expectedThird, afterThird)
	}
}

// TestAIMDCeilingSetGet verifies SetCeiling and GetCeiling round-trip.
func TestAIMDCeilingSetGet(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Now())
	ctrl := newTestAIMD(clk)
	name := NameLastFM

	want := rate.Limit(20.0)
	ctrl.SetCeiling(name, want)
	got := ctrl.GetCeiling(name)
	if got != want {
		t.Fatalf("GetCeiling: expected %v, got %v", want, got)
	}
}

// TestAIMDSetCeilingZeroReverts verifies that SetCeiling(name, 0) resets the
// ceiling to the default (aimdDefaultCeilingMultiplier * floor) rather than
// setting an unusable zero limit.
func TestAIMDSetCeilingZeroReverts(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Now())
	ctrl := newTestAIMD(clk)
	name := NameMusicBrainz
	floor := defaultRateLimits[name]
	defaultCeiling := rate.Limit(float64(floor) * aimdDefaultCeilingMultiplier)

	// Set a custom ceiling, then reset it to zero.
	ctrl.SetCeiling(name, rate.Limit(50))
	ctrl.SetCeiling(name, 0)
	got := ctrl.GetCeiling(name)
	if got != defaultCeiling {
		t.Fatalf("SetCeiling(0): expected ceiling to revert to %v, got %v", defaultCeiling, got)
	}
}

// TestAIMDConcurrencyTwoKeys is a race-detector test that initializes two
// different provider keys concurrently. The single-key concurrency test misses
// the case where stateFor's lazy-init is racing on two distinct map entries at
// the same instant. This test covers that gap.
func TestAIMDConcurrencyTwoKeys(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Now())
	ctrl := newTestAIMD(clk)

	// Use two providers with different floors to ensure distinct state slots.
	nameA := NameMusicBrainz // floor = 1 req/s
	nameB := NameFanartTV    // floor = 3 req/s

	const goroutines = 20
	const opsPerGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			for j := range opsPerGoroutine {
				// Split goroutines evenly across the two keys.
				n := nameA
				if idx%2 == 1 {
					n = nameB
				}
				if (idx+j)%3 == 0 {
					clk.advance(time.Millisecond)
					ctrl.RecordFailure(n, time.Second)
				} else {
					ctrl.RecordSuccess(n)
				}
				_ = ctrl.GetCeiling(n)
			}
		}(i)
	}
	wg.Wait()

	// Both limits must remain within their respective [floor, ceiling] ranges.
	for _, n := range []ProviderName{nameA, nameB} {
		got := aimdCurrentLimit(ctrl, n)
		floor := defaultRateLimits[n]
		ceiling := ctrl.GetCeiling(n)
		if got < floor {
			t.Errorf("provider %s: limit %v below floor %v", n, got, floor)
		}
		if got > ceiling {
			t.Errorf("provider %s: limit %v above ceiling %v", n, got, ceiling)
		}
	}
}

// TestAIMDConcurrency is a race-detector test that hammers RecordSuccess and
// RecordFailure from multiple goroutines concurrently. It does not assert
// specific limit values; the race detector catches unsafe concurrent access.
func TestAIMDConcurrency(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Now())
	ctrl := newTestAIMD(clk)
	name := NameMusicBrainz

	const goroutines = 20
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			for j := range opsPerGoroutine {
				if (idx+j)%3 == 0 {
					// Advance the fake clock from multiple goroutines to exercise
					// the hysteresis path under concurrent time reads.
					clk.advance(time.Millisecond)
					ctrl.RecordFailure(name, time.Second)
				} else {
					ctrl.RecordSuccess(name)
				}
				// GetCeiling exercises the RLock path alongside write-lock ops.
				_ = ctrl.GetCeiling(name)
			}
		}(i)
	}
	wg.Wait()

	// The limit must still be within [floor, ceiling] after all the noise.
	got := aimdCurrentLimit(ctrl, name)
	floor := defaultRateLimits[name]
	ceiling := ctrl.GetCeiling(name)
	if got < floor {
		t.Errorf("limit %v below floor %v after concurrency test", got, floor)
	}
	if got > ceiling {
		t.Errorf("limit %v above ceiling %v after concurrency test", got, ceiling)
	}
}
