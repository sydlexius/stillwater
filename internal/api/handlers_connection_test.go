package api

import (
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/connection"
)

// TestToConnectionResponse_RFC3339Fields covers the three time.RFC3339 format
// calls in toConnectionResponse: CreatedAt, UpdatedAt, and LastCheckedAt.
func TestToConnectionResponse_RFC3339Fields(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 3, 20, 14, 30, 0, 0, time.UTC)
	checked := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

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

	if resp.CreatedAt != created.Format(time.RFC3339) {
		t.Errorf("CreatedAt = %q, want %q", resp.CreatedAt, created.Format(time.RFC3339))
	}
	if resp.UpdatedAt != updated.Format(time.RFC3339) {
		t.Errorf("UpdatedAt = %q, want %q", resp.UpdatedAt, updated.Format(time.RFC3339))
	}
	if resp.LastCheckedAt == nil {
		t.Fatal("LastCheckedAt = nil, want non-nil pointer")
	}
	if *resp.LastCheckedAt != checked.Format(time.RFC3339) {
		t.Errorf("LastCheckedAt = %q, want %q", *resp.LastCheckedAt, checked.Format(time.RFC3339))
	}
}
