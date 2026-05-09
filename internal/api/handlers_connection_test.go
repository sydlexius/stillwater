package api

import (
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
)

// TestToConnectionResponse_RFC3339Fields covers the three time.RFC3339 format
// calls in toConnectionResponse: CreatedAt, UpdatedAt, and LastCheckedAt.
// Inputs are constructed in a non-UTC location to verify the handler normalizes
// to UTC before formatting -- the OpenAPI contract requires UTC RFC3339.
func TestToConnectionResponse_RFC3339Fields(t *testing.T) {
	t.Parallel()
	tz, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	created := time.Date(2026, 1, 15, 10, 0, 0, 0, tz)
	updated := time.Date(2026, 3, 20, 14, 30, 0, 0, tz)
	checked := time.Date(2026, 5, 1, 9, 0, 0, 0, tz)

	c := connection.Connection{
		ID:            "conn-1",
		Name:          "Test Emby",
		Type:          connection.TypeEmby,
		URL:           "http://emby.local:8096",
		APIKey:        "secret",
		Enabled:       true,
		Status:        "ok",
		CreatedAt:     created,
		UpdatedAt:     updated,
		LastCheckedAt: &checked,
	}

	resp := toConnectionResponse(c)

	wantCreated := created.UTC().Format(time.RFC3339)
	wantUpdated := updated.UTC().Format(time.RFC3339)
	wantChecked := checked.UTC().Format(time.RFC3339)

	if resp.CreatedAt != wantCreated {
		t.Errorf("CreatedAt = %q, want %q", resp.CreatedAt, wantCreated)
	}
	if !strings.HasSuffix(resp.CreatedAt, "Z") {
		t.Errorf("CreatedAt = %q, want UTC zone (Z suffix)", resp.CreatedAt)
	}
	if resp.UpdatedAt != wantUpdated {
		t.Errorf("UpdatedAt = %q, want %q", resp.UpdatedAt, wantUpdated)
	}
	if resp.LastCheckedAt == nil {
		t.Fatal("LastCheckedAt = nil, want non-nil pointer")
	}
	if *resp.LastCheckedAt != wantChecked {
		t.Errorf("LastCheckedAt = %q, want %q", *resp.LastCheckedAt, wantChecked)
	}
	if !strings.HasSuffix(*resp.LastCheckedAt, "Z") {
		t.Errorf("LastCheckedAt = %q, want UTC zone (Z suffix)", *resp.LastCheckedAt)
	}
}
