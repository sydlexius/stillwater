package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/rule"
)

func seedNotificationViolations(t *testing.T, svc *rule.Service) {
	t.Helper()
	ctx := context.Background()

	violations := []*rule.RuleViolation{
		{
			RuleID: rule.RuleNFOExists, ArtistID: "a1", ArtistName: "Charlie",
			Severity: "error", Message: "missing nfo", Fixable: true,
			Status: rule.ViolationStatusOpen,
		},
		{
			RuleID: rule.RuleThumbExists, ArtistID: "a2", ArtistName: "Alice",
			Severity: "warning", Message: "missing thumb", Fixable: true,
			Status: rule.ViolationStatusOpen,
		},
		{
			RuleID: rule.RuleBioExists, ArtistID: "a3", ArtistName: "Bob",
			Severity: "info", Message: "missing bio", Fixable: false,
			Status: rule.ViolationStatusOpen,
		},
	}
	for _, v := range violations {
		if err := svc.UpsertViolation(ctx, v); err != nil {
			t.Fatalf("UpsertViolation: %v", err)
		}
	}
}

func TestHandleNotificationsTable_DefaultParams(t *testing.T) {
	r, _ := testRouter(t)
	seedNotificationViolations(t, r.ruleService)

	req := httptest.NewRequest(http.MethodGet, "/notifications/table", nil)
	w := httptest.NewRecorder()

	r.handleNotificationsTable(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	// Should contain all three artists since default status is "active"
	if !strings.Contains(body, "Charlie") {
		t.Error("expected Charlie in response")
	}
	if !strings.Contains(body, "Alice") {
		t.Error("expected Alice in response")
	}
	if !strings.Contains(body, "Bob") {
		t.Error("expected Bob in response")
	}
}

func TestHandleNotificationsTable_SeverityFilter(t *testing.T) {
	r, _ := testRouter(t)
	seedNotificationViolations(t, r.ruleService)

	req := httptest.NewRequest(http.MethodGet, "/notifications/table?severity=error", nil)
	w := httptest.NewRecorder()

	r.handleNotificationsTable(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	// Should contain only Charlie (error severity)
	if !strings.Contains(body, "Charlie") {
		t.Error("expected Charlie in response")
	}
	if strings.Contains(body, "Alice") {
		t.Error("Alice (warning) should not appear with severity=error filter")
	}
	if strings.Contains(body, "Bob") {
		t.Error("Bob (info) should not appear with severity=error filter")
	}
}

func TestHandleNotificationsTable_CategoryFilter(t *testing.T) {
	r, _ := testRouter(t)
	seedNotificationViolations(t, r.ruleService)

	req := httptest.NewRequest(http.MethodGet, "/notifications/table?category=image", nil)
	w := httptest.NewRecorder()

	r.handleNotificationsTable(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	// thumb_exists is category "image", so only Alice should appear
	if !strings.Contains(body, "Alice") {
		t.Error("expected Alice (image category) in response")
	}
	if strings.Contains(body, "Charlie") {
		t.Error("Charlie (nfo category) should not appear with category=image filter")
	}
}

func TestHandleNotificationsTable_SortByArtist(t *testing.T) {
	r, _ := testRouter(t)
	seedNotificationViolations(t, r.ruleService)

	req := httptest.NewRequest(http.MethodGet, "/notifications/table?sort=artist_name&order=asc", nil)
	w := httptest.NewRecorder()

	r.handleNotificationsTable(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	// All three should be present
	aliceIdx := strings.Index(body, "Alice")
	bobIdx := strings.Index(body, "Bob")
	charlieIdx := strings.Index(body, "Charlie")
	if aliceIdx < 0 || bobIdx < 0 || charlieIdx < 0 {
		t.Fatal("expected all three artists in response")
	}
	if aliceIdx >= bobIdx || bobIdx >= charlieIdx {
		t.Error("expected alphabetical order: Alice, Bob, Charlie")
	}
}

func TestHandleNotificationsTable_GroupBy(t *testing.T) {
	r, _ := testRouter(t)
	seedNotificationViolations(t, r.ruleService)

	req := httptest.NewRequest(http.MethodGet, "/notifications/table?group_by=severity", nil)
	w := httptest.NewRecorder()

	r.handleNotificationsTable(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	// Grouped view should show group labels and violation rows
	if !strings.Contains(body, "Charlie") {
		t.Error("expected Charlie in grouped response")
	}
	if !strings.Contains(body, "Alice") {
		t.Error("expected Alice in grouped response")
	}
}

func TestParseNotificationParams(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantSort  string
		wantOrder string
		wantSev   string
		wantCat   string
		wantGB    string
	}{
		{
			name:      "defaults",
			url:       "/notifications/table",
			wantSort:  "severity",
			wantOrder: "desc",
		},
		{
			name:      "all params",
			url:       "/notifications/table?sort=artist_name&order=desc&severity=error&category=nfo&group_by=artist&rule_id=nfo_exists",
			wantSort:  "artist_name",
			wantOrder: "desc",
			wantSev:   "error",
			wantCat:   "nfo",
			wantGB:    "artist",
		},
		{
			name:      "invalid group_by ignored",
			url:       "/notifications/table?group_by=invalid_value",
			wantSort:  "severity",
			wantOrder: "desc",
			wantGB:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			p := parseNotificationParams(req)
			if p.Sort != tc.wantSort {
				t.Errorf("Sort = %q, want %q", p.Sort, tc.wantSort)
			}
			if p.Order != tc.wantOrder {
				t.Errorf("Order = %q, want %q", p.Order, tc.wantOrder)
			}
			if p.Severity != tc.wantSev {
				t.Errorf("Severity = %q, want %q", p.Severity, tc.wantSev)
			}
			if p.Category != tc.wantCat {
				t.Errorf("Category = %q, want %q", p.Category, tc.wantCat)
			}
			if p.GroupBy != tc.wantGB {
				t.Errorf("GroupBy = %q, want %q", p.GroupBy, tc.wantGB)
			}
		})
	}
}

func TestParseNotificationParams_DefaultStatus(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/notifications/table", nil)
	p := parseNotificationParams(req)
	if p.Status != "active" {
		t.Errorf("default status = %q, want active", p.Status)
	}
}
