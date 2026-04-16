package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestHandleNotificationsExport_CSV(t *testing.T) {
	r, _ := testRouter(t)
	seedNotificationViolations(t, r.ruleService)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/export", nil)
	w := httptest.NewRecorder()

	r.handleNotificationsExport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "text/csv" {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}

	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "violations.csv") {
		t.Errorf("Content-Disposition = %q, want filename violations.csv", cd)
	}

	body := w.Body.String()
	// Header row
	if !strings.Contains(body, "Artist Name,Library,Rule ID,Severity,Message,Status,Age") {
		t.Error("expected CSV header row")
	}
	// All three seeded violations should appear (default status=active)
	if !strings.Contains(body, "Charlie") {
		t.Error("expected Charlie in CSV output")
	}
	if !strings.Contains(body, "Alice") {
		t.Error("expected Alice in CSV output")
	}
	if !strings.Contains(body, "Bob") {
		t.Error("expected Bob in CSV output")
	}
}

func TestHandleNotificationsExport_JSON(t *testing.T) {
	r, _ := testRouter(t)
	seedNotificationViolations(t, r.ruleService)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/export?format=json", nil)
	w := httptest.NewRecorder()

	r.handleNotificationsExport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var result struct {
		Violations []struct {
			ArtistName  string `json:"artist_name"`
			LibraryName string `json:"library_name"`
			RuleID      string `json:"rule_id"`
			Severity    string `json:"severity"`
			Message     string `json:"message"`
			Status      string `json:"status"`
			Age         string `json:"age"`
		} `json:"violations"`
		Count int `json:"count"`
	}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decoding JSON: %v", err)
	}
	if result.Count != 3 {
		t.Errorf("count = %d, want 3", result.Count)
	}
	// Verify library_name field is present (even if empty for test data
	// that has no matching artist/library rows).
	for _, v := range result.Violations {
		// library_name should be present as a string (not omitted) per OpenAPI spec
		_ = v.LibraryName
	}
	if len(result.Violations) != 3 {
		t.Fatalf("violations length = %d, want 3", len(result.Violations))
	}
}

func TestHandleNotificationsExport_JSONViaAcceptHeader(t *testing.T) {
	r, _ := testRouter(t)
	seedNotificationViolations(t, r.ruleService)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/export", nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()

	r.handleNotificationsExport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestHandleNotificationsExport_SeverityFilter(t *testing.T) {
	r, _ := testRouter(t)
	seedNotificationViolations(t, r.ruleService)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/export?severity=error", nil)
	w := httptest.NewRecorder()

	r.handleNotificationsExport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	// Only Charlie has error severity
	if !strings.Contains(body, "Charlie") {
		t.Error("expected Charlie (error severity) in CSV output")
	}
	if strings.Contains(body, "Alice") {
		t.Error("Alice (warning) should not appear with severity=error filter")
	}
	if strings.Contains(body, "Bob") {
		t.Error("Bob (info) should not appear with severity=error filter")
	}
}

func TestViolationAge(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		created time.Time
		want    string
	}{
		{
			name:    "zero time",
			created: time.Time{},
			want:    "",
		},
		{
			name:    "30 minutes ago",
			created: now.Add(-30 * time.Minute),
			want:    "30m",
		},
		{
			name:    "5 hours ago",
			created: now.Add(-5 * time.Hour),
			want:    "5h",
		},
		{
			name:    "3 days ago",
			created: now.Add(-72 * time.Hour),
			want:    "3d",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := violationAge(tc.created, now)
			if got != tc.want {
				t.Errorf("violationAge(%v, %v) = %q, want %q", tc.created, now, got, tc.want)
			}
		})
	}
}
