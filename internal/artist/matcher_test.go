package artist

import (
	"context"
	"testing"
)

func TestMatchByMBID_Found(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Nirvana", "/music/Nirvana")
	a.MusicBrainzID = "5b11f4ce-a62d-471e-81fc-a69a8278c7da"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	matcher := NewMatcher(svc, DefaultMatchConfig())
	result, err := matcher.MatchByMBID(ctx, "5b11f4ce-a62d-471e-81fc-a69a8278c7da")
	if err != nil {
		t.Fatalf("MatchByMBID: %v", err)
	}
	if result == nil {
		t.Fatal("expected match result, got nil")
	}
	if result.Artist.Name != "Nirvana" {
		t.Errorf("Artist.Name = %q, want Nirvana", result.Artist.Name)
	}
	if result.Confidence != 1.0 {
		t.Errorf("Confidence = %f, want 1.0", result.Confidence)
	}
	if result.MatchType != MatchTypeMBID {
		t.Errorf("MatchType = %q, want %q", result.MatchType, MatchTypeMBID)
	}
}

func TestMatchByMBID_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	matcher := NewMatcher(svc, DefaultMatchConfig())
	result, err := matcher.MatchByMBID(context.Background(), "nonexistent-mbid")
	if err != nil {
		t.Fatalf("MatchByMBID: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
}

func TestMatchByMBID_EmptyID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	matcher := NewMatcher(svc, DefaultMatchConfig())
	result, err := matcher.MatchByMBID(context.Background(), "")
	if err != nil {
		t.Fatalf("MatchByMBID: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result for empty MBID")
	}
}

func TestMatchByID_MultipleProviders(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Tool", "/music/Tool")
	a.AudioDBID = "111222"
	a.DiscogsID = "54321"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	matcher := NewMatcher(svc, DefaultMatchConfig())

	// Match by AudioDB ID
	result, err := matcher.MatchByID(ctx, map[string]string{"audiodb": "111222"})
	if err != nil {
		t.Fatalf("MatchByID audiodb: %v", err)
	}
	if result == nil {
		t.Fatal("expected result for audiodb match")
	}
	if result.MatchType != MatchTypeAudioDB {
		t.Errorf("MatchType = %q, want %q", result.MatchType, MatchTypeAudioDB)
	}
	if result.Confidence != 0.95 {
		t.Errorf("Confidence = %f, want 0.95", result.Confidence)
	}

	// Match by Discogs ID
	result, err = matcher.MatchByID(ctx, map[string]string{"discogs": "54321"})
	if err != nil {
		t.Fatalf("MatchByID discogs: %v", err)
	}
	if result == nil {
		t.Fatal("expected result for discogs match")
	}
	if result.MatchType != MatchTypeDiscogs {
		t.Errorf("MatchType = %q, want %q", result.MatchType, MatchTypeDiscogs)
	}
}

func TestMatchByID_PriorityOrder(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create two artists with different IDs
	a1 := testArtist("Artist MBID", "/music/Artist MBID")
	a1.MusicBrainzID = "mbid-123"
	if err := svc.Create(ctx, a1); err != nil {
		t.Fatalf("Create a1: %v", err)
	}

	a2 := testArtist("Artist AudioDB", "/music/Artist AudioDB")
	a2.AudioDBID = "adb-456"
	if err := svc.Create(ctx, a2); err != nil {
		t.Fatalf("Create a2: %v", err)
	}

	matcher := NewMatcher(svc, DefaultMatchConfig())

	// When both IDs are provided, MBID takes priority
	result, err := matcher.MatchByID(ctx, map[string]string{
		"musicbrainz": "mbid-123",
		"audiodb":     "adb-456",
	})
	if err != nil {
		t.Fatalf("MatchByID: %v", err)
	}
	if result == nil {
		t.Fatal("expected match result")
	}
	if result.MatchType != MatchTypeMBID {
		t.Errorf("MatchType = %q, want %q (MBID should have priority)", result.MatchType, MatchTypeMBID)
	}
	if result.Artist.Name != "Artist MBID" {
		t.Errorf("Artist.Name = %q, want Artist MBID", result.Artist.Name)
	}
}

func TestMatchByID_NoIDs(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	matcher := NewMatcher(svc, DefaultMatchConfig())
	result, err := matcher.MatchByID(context.Background(), map[string]string{})
	if err != nil {
		t.Fatalf("MatchByID: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result for empty IDs")
	}
}

func TestMatch_ConfidenceThreshold(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Threshold Test", "/music/Threshold Test")
	a.AudioDBID = "threshold-id"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// With default config (min_confidence=0.85), AudioDB match (0.95) should pass
	matcher := NewMatcher(svc, DefaultMatchConfig())
	result, err := matcher.Match(ctx, map[string]string{"audiodb": "threshold-id"}, "Threshold Test")
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if result == nil {
		t.Fatal("expected match with default threshold")
	}

	// With very high threshold (0.99), AudioDB match (0.95) should be rejected
	highConfig := MatchConfig{
		Strategy:      MatchStrategyPreferID,
		MinConfidence: 0.99,
	}
	matcher = NewMatcher(svc, highConfig)
	result, err = matcher.Match(ctx, map[string]string{"audiodb": "threshold-id"}, "Threshold Test")
	if err != nil {
		t.Fatalf("Match high threshold: %v", err)
	}
	if result != nil {
		t.Error("expected nil result with high confidence threshold")
	}
}

func TestMatch_AlwaysPromptStrategy(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Prompt Artist", "/music/Prompt Artist")
	a.MusicBrainzID = "prompt-mbid"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	config := MatchConfig{
		Strategy:      MatchStrategyAlwaysPrompt,
		MinConfidence: 0.85,
	}
	matcher := NewMatcher(svc, config)

	// AlwaysPrompt returns the result regardless of confidence for user review
	result, err := matcher.Match(ctx, map[string]string{"musicbrainz": "prompt-mbid"}, "Prompt Artist")
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if result == nil {
		t.Fatal("expected result with always_prompt strategy")
	}
	if result.Confidence != 1.0 {
		t.Errorf("Confidence = %f, want 1.0", result.Confidence)
	}
}
