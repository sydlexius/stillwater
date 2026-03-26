package rule

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/event"
)

// pollHealthScore polls the database every 100ms until the condition is met,
// or the timeout (5s) is reached.
func pollHealthScore(t *testing.T, svc *artist.Service, artistID string, condition func(float64) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for health score condition on artist %s", artistID)
		case <-ticker.C:
			a, err := svc.GetByID(context.Background(), artistID)
			if err != nil {
				continue // DB might not be ready yet
			}
			if condition(a.HealthScore) {
				return
			}
		}
	}
}

// pollPendingQueue polls the health subscriber's pending queue every 100ms until it's empty,
// or the timeout (5s) is reached.
func pollPendingQueue(t *testing.T, sub *HealthSubscriber) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for pending queue to drain")
		case <-ticker.C:
			sub.mu.Lock()
			n := len(sub.pending)
			sub.mu.Unlock()
			if n == 0 {
				return
			}
		}
	}
}

func setupSubscriberTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestHealthSubscriber_SingleEvent(t *testing.T) {
	db := setupSubscriberTestDB(t)
	logger := slog.Default()
	svc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rule defaults: %v", err)
	}
	engine := NewEngine(ruleSvc, db, nil, nil, logger)

	// Create an artist with zero health score
	a := &artist.Artist{
		Name:        "Test Artist",
		SortName:    "Test Artist",
		Path:        "/music/test-artist",
		HealthScore: 0.0,
	}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	sub := NewHealthSubscriber(engine, svc, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go sub.Start(ctx)
	defer sub.Stop()

	// Publish an event
	sub.HandleEvent(event.Event{
		Type: event.ArtistUpdated,
		Data: map[string]any{"artist_id": a.ID},
	})

	// Wait for the debounce window + processing time using poll instead of sleep
	pollHealthScore(t, svc, a.ID, func(score float64) bool { return score != 0.0 })

	// Verify the artist's health score was updated. The artist has no NFO,
	// no thumb, no fanart, etc., so it fails several rules but passes others
	// (e.g. rules with nil checkers for missing filesystem paths count as
	// passed). The resulting score should be non-zero (some rules pass).
	updated, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("getting artist after evaluation: %v", err)
	}

	if updated.HealthScore == 0.0 {
		t.Errorf("HealthScore = 0.0 after subscriber evaluation, want non-zero (subscriber should have persisted a score)")
	}
}

func TestHealthSubscriber_DebounceCoalesces(t *testing.T) {
	db := setupSubscriberTestDB(t)
	logger := slog.Default()
	svc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rule defaults: %v", err)
	}
	engine := NewEngine(ruleSvc, db, nil, nil, logger)

	a := &artist.Artist{
		Name:        "Debounce Artist",
		SortName:    "Debounce Artist",
		Path:        "/music/debounce",
		HealthScore: 0.0,
	}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	sub := NewHealthSubscriber(engine, svc, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go sub.Start(ctx)
	defer sub.Stop()

	// Fire multiple events rapidly for the same artist
	for range 5 {
		sub.HandleEvent(event.Event{
			Type: event.ArtistUpdated,
			Data: map[string]any{"artist_id": a.ID},
		})
	}

	// Check that only one entry is pending (debounced)
	sub.mu.Lock()
	pendingCount := len(sub.pending)
	sub.mu.Unlock()

	if pendingCount != 1 {
		t.Errorf("pending count = %d, want 1 (events should coalesce)", pendingCount)
	}

	// Wait for processing using poll instead of sleep
	pollPendingQueue(t, sub)

	// Verify it was processed
	sub.mu.Lock()
	remaining := len(sub.pending)
	sub.mu.Unlock()

	if remaining != 0 {
		t.Errorf("remaining pending = %d, want 0 after processing", remaining)
	}
}

func TestHealthSubscriber_NilEngine(t *testing.T) {
	logger := slog.Default()
	sub := NewHealthSubscriber(nil, nil, logger)

	// Should not panic
	sub.HandleEvent(event.Event{
		Type: event.ArtistUpdated,
		Data: map[string]any{"artist_id": "some-id"},
	})

	// Start with nil engine should return immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sub.Start(ctx) // Should not block

	// Bootstrap with nil engine should be a no-op
	sub.Bootstrap(context.Background())
}

func TestHealthSubscriber_ContextCancellation(t *testing.T) {
	db := setupSubscriberTestDB(t)
	logger := slog.Default()
	svc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rule defaults: %v", err)
	}
	engine := NewEngine(ruleSvc, db, nil, nil, logger)

	sub := NewHealthSubscriber(engine, svc, logger)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		sub.Start(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Good -- Start returned after context cancellation
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return within 3 seconds after context cancellation")
	}
}

func TestHealthSubscriber_Bootstrap(t *testing.T) {
	db := setupSubscriberTestDB(t)
	logger := slog.Default()
	svc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rule defaults: %v", err)
	}
	engine := NewEngine(ruleSvc, db, nil, nil, logger)

	// Create two artists with zero health score
	for _, name := range []string{"Bootstrap A", "Bootstrap B"} {
		a := &artist.Artist{
			Name:        name,
			SortName:    name,
			Path:        "/music/" + name,
			HealthScore: 0.0,
		}
		if err := svc.Create(context.Background(), a); err != nil {
			t.Fatalf("creating artist %q: %v", name, err)
		}
	}

	// Create one with non-zero score (should be skipped)
	skip := &artist.Artist{
		Name:        "Already Scored",
		SortName:    "Already Scored",
		Path:        "/music/already-scored",
		HealthScore: 85.0,
	}
	if err := svc.Create(context.Background(), skip); err != nil {
		t.Fatalf("creating scored artist: %v", err)
	}

	sub := NewHealthSubscriber(engine, svc, logger)
	sub.Bootstrap(context.Background())

	// Verify that the zero-score artists were evaluated
	ids, err := svc.ListZeroHealthIDs(context.Background())
	if err != nil {
		t.Fatalf("ListZeroHealthIDs: %v", err)
	}

	// After bootstrap, zero-score artists should have been re-evaluated.
	// The rule engine will assign non-zero scores since some rules pass.
	if len(ids) != 0 {
		t.Errorf("remaining zero-score artists after bootstrap = %d, want 0", len(ids))
	}

	// Verify the non-zero artist was not modified
	scored, err := svc.GetByID(context.Background(), skip.ID)
	if err != nil {
		t.Fatalf("getting scored artist: %v", err)
	}
	if scored.HealthScore != 85.0 {
		t.Errorf("pre-scored artist HealthScore = %.1f, want 85.0 (should not be modified)", scored.HealthScore)
	}
}

func TestTopViolationSummaries_Empty(t *testing.T) {
	db := setupSubscriberTestDB(t)
	svc := NewService(db)

	results, err := svc.TopViolationSummaries(context.Background(), 10)
	if err != nil {
		t.Fatalf("TopViolationSummaries: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0 for empty DB", len(results))
	}
}

func TestTopViolationSummaries_WithViolations(t *testing.T) {
	db := setupSubscriberTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	// Create two non-excluded artists
	a1 := &artist.Artist{Name: "Artist A", SortName: "Artist A", Path: "/music/a"}
	a2 := &artist.Artist{Name: "Artist B", SortName: "Artist B", Path: "/music/b"}
	if err := artistSvc.Create(ctx, a1); err != nil {
		t.Fatalf("creating artist A: %v", err)
	}
	if err := artistSvc.Create(ctx, a2); err != nil {
		t.Fatalf("creating artist B: %v", err)
	}

	// Create an excluded artist (should not appear in results)
	excluded := &artist.Artist{
		Name: "Excluded", SortName: "Excluded", Path: "/music/excluded",
		IsExcluded: true, ExclusionReason: "test",
	}
	if err := artistSvc.Create(ctx, excluded); err != nil {
		t.Fatalf("creating excluded artist: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Insert open violations: 2 for nfo_exists, 1 for thumb_exists
	for _, v := range []RuleViolation{
		{ID: "v1", RuleID: RuleNFOExists, ArtistID: a1.ID, ArtistName: a1.Name, Severity: "error", Status: "open", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{ID: "v2", RuleID: RuleNFOExists, ArtistID: a2.ID, ArtistName: a2.Name, Severity: "error", Status: "open", CreatedAt: time.Now(), UpdatedAt: time.Now()},
		{ID: "v3", RuleID: RuleThumbExists, ArtistID: a1.ID, ArtistName: a1.Name, Severity: "error", Status: "open", CreatedAt: time.Now(), UpdatedAt: time.Now()},
	} {
		if err := ruleSvc.UpsertViolation(ctx, &v); err != nil {
			t.Fatalf("inserting violation %s: %v", v.ID, err)
		}
	}

	// Insert a resolved violation (should NOT appear)
	_, err := db.ExecContext(ctx, `
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, severity, message, fixable, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"v4", RuleFanartExists, a1.ID, a1.Name, "warning", "", false, "resolved", now, now)
	if err != nil {
		t.Fatalf("inserting resolved violation: %v", err)
	}

	// Insert an open violation for the excluded artist (should NOT appear)
	_, err = db.ExecContext(ctx, `
		INSERT INTO rule_violations (id, rule_id, artist_id, artist_name, severity, message, fixable, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"v5", RuleNFOExists, excluded.ID, excluded.Name, "error", "", false, "open", now, now)
	if err != nil {
		t.Fatalf("inserting excluded violation: %v", err)
	}

	results, err := ruleSvc.TopViolationSummaries(ctx, 10)
	if err != nil {
		t.Fatalf("TopViolationSummaries: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2 (nfo_exists and thumb_exists)", len(results))
	}

	// First result should be nfo_exists with count 2 (highest)
	if results[0].RuleID != RuleNFOExists {
		t.Errorf("results[0].RuleID = %q, want %q", results[0].RuleID, RuleNFOExists)
	}
	if results[0].Count != 2 {
		t.Errorf("results[0].Count = %d, want 2", results[0].Count)
	}
	if results[0].RuleName == "" {
		t.Error("results[0].RuleName is empty, want non-empty")
	}

	// Second result should be thumb_exists with count 1
	if results[1].RuleID != RuleThumbExists {
		t.Errorf("results[1].RuleID = %q, want %q", results[1].RuleID, RuleThumbExists)
	}
	if results[1].Count != 1 {
		t.Errorf("results[1].Count = %d, want 1", results[1].Count)
	}
}

func TestTopViolationSummaries_LimitEnforced(t *testing.T) {
	db := setupSubscriberTestDB(t)
	ruleSvc := NewService(db)
	artistSvc := artist.NewService(db)
	ctx := context.Background()

	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	a := &artist.Artist{Name: "Limit Test", SortName: "Limit Test", Path: "/music/limit"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Create violations for multiple rules
	for i, ruleID := range []string{RuleNFOExists, RuleThumbExists, RuleFanartExists} {
		v := RuleViolation{
			ID: fmt.Sprintf("lv%d", i), RuleID: ruleID,
			ArtistID: a.ID, ArtistName: a.Name,
			Severity: "error", Status: "open",
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		if err := ruleSvc.UpsertViolation(ctx, &v); err != nil {
			t.Fatalf("inserting violation: %v", err)
		}
	}

	// Request limit of 2
	results, err := ruleSvc.TopViolationSummaries(ctx, 2)
	if err != nil {
		t.Fatalf("TopViolationSummaries: %v", err)
	}

	if len(results) != 2 {
		t.Errorf("len(results) = %d, want 2 (limit enforced)", len(results))
	}
}
