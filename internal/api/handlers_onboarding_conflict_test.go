package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/connection"
)

// onboardingConflictRequest creates a GET request that the handler treats
// as authenticated. The handler does not consult the user ID, so a bare
// request is sufficient -- we keep this helper around for future
// expansion (e.g. CSRF assertion on POST counterparts).
func onboardingConflictRequest(target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	return req
}

func TestHasQualifyingConflictConnection(t *testing.T) {
	tests := []struct {
		name  string
		conns []connection.Connection
		want  bool
	}{
		{"empty", nil, false},
		{"only disabled", []connection.Connection{{Type: connection.TypeEmby, Enabled: false}}, false},
		{"only unsupported types", []connection.Connection{{Type: "navidrome", Enabled: true}}, false},
		{"enabled emby", []connection.Connection{{Type: connection.TypeEmby, Enabled: true}}, true},
		{"enabled jellyfin", []connection.Connection{{Type: connection.TypeJellyfin, Enabled: true}}, true},
		{"enabled lidarr", []connection.Connection{{Type: connection.TypeLidarr, Enabled: true}}, true},
		{"mixed disabled+enabled", []connection.Connection{
			{Type: connection.TypeEmby, Enabled: false},
			{Type: connection.TypeLidarr, Enabled: true},
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasQualifyingConflictConnection(tt.conns); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandleGetOnboardingConflictStep_204WhenDetectorMissing(t *testing.T) {
	r := &Router{logger: testDiscardLogger()}
	req := onboardingConflictRequest("/api/v1/onboarding/conflict-step")
	w := httptest.NewRecorder()
	r.handleGetOnboardingConflictStep(w, req)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestHandleGetOnboardingConflictStep_RendersCleanBody(t *testing.T) {
	r := newConflictHarness(t, nil)
	req := onboardingConflictRequest("/api/v1/onboarding/conflict-step")
	w := httptest.NewRecorder()
	r.handleGetOnboardingConflictStep(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Clean-state copy must include the explicit "No conflicts" headline so
	// updateConflictGate observes value="" on the hidden block input.
	if !strings.Contains(body, "No conflicts detected.") {
		t.Errorf("expected clean-state copy in body; got: %s", body)
	}
	if !strings.Contains(body, `id="ob-conflict-block-state"`) && !strings.Contains(body, `id=\"ob-conflict-block-state\"`) {
		t.Errorf("expected hidden gate input in body; got: %s", body)
	}
}

func TestHandleGetOnboardingConflictStep_RefreshInvalidatesCache(t *testing.T) {
	r := newConflictHarness(t, []connection.Connection{
		{ID: "a", Name: "A", Type: connection.TypeEmby, Enabled: true},
	})
	req := onboardingConflictRequest("/api/v1/onboarding/conflict-step?refresh=1")
	w := httptest.NewRecorder()
	r.handleGetOnboardingConflictStep(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

func TestAggregateProbeError_EmptyWhenAllProbesSucceed(t *testing.T) {
	l := conflict.Ledger{
		Connections: []conflict.ConnectionState{
			{ConnectionID: "a", ConnectionName: "A", Enabled: true, CheckErr: ""},
			{ConnectionID: "b", ConnectionName: "B", Enabled: true, CheckErr: ""},
		},
	}
	if got := aggregateProbeError(l); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestAggregateProbeError_SingleFailure(t *testing.T) {
	l := conflict.Ledger{
		Connections: []conflict.ConnectionState{
			{ConnectionID: "a", ConnectionName: "EmbyOne", Enabled: true, CheckErr: "dial tcp"},
		},
	}
	got := aggregateProbeError(l)
	if !strings.Contains(got, "EmbyOne") {
		t.Errorf("expected connection name in message, got %q", got)
	}
}

func TestAggregateProbeError_DisabledIgnored(t *testing.T) {
	l := conflict.Ledger{
		Connections: []conflict.ConnectionState{
			{ConnectionID: "a", ConnectionName: "A", Enabled: false, CheckErr: "dial tcp"},
		},
	}
	if got := aggregateProbeError(l); got != "" {
		t.Errorf("disabled connection probe failures should be ignored; got %q", got)
	}
}

func TestAggregateProbeError_MultipleFailures(t *testing.T) {
	l := conflict.Ledger{
		Connections: []conflict.ConnectionState{
			{ConnectionID: "a", ConnectionName: "EmbyOne", Enabled: true, CheckErr: "dial tcp"},
			{ConnectionID: "b", ConnectionName: "JellyfinTwo", Enabled: true, CheckErr: "401"},
			{ConnectionID: "c", ConnectionName: "Healthy", Enabled: true},
		},
	}
	got := aggregateProbeError(l)
	if !strings.Contains(got, "EmbyOne") || !strings.Contains(got, "JellyfinTwo") {
		t.Errorf("expected both failed connection names, got %q", got)
	}
	if strings.Contains(got, "Healthy") {
		t.Errorf("healthy connection should not appear in error message, got %q", got)
	}
}

// TestHandleGetOnboardingConflictStep_PersistsCompletionMarker exercises
// the settings-write side-effect: each successful render upserts
// onboarding.conflict_check_completed_at so callers can detect that the
// pre-flight has been visited at least once.
func TestHandleGetOnboardingConflictStep_PersistsCompletionMarker(t *testing.T) {
	r := testRouterForOnboarding(t)
	r.conflictDetector = conflict.NewForTest(&fakeRepo{}, testDiscardLogger())

	req := onboardingConflictRequest("/api/v1/onboarding/conflict-step")
	w := httptest.NewRecorder()
	r.handleGetOnboardingConflictStep(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var got string
	err := r.db.QueryRowContext(context.Background(),
		`SELECT value FROM settings WHERE key = 'onboarding.conflict_check_completed_at'`).Scan(&got)
	if err != nil {
		t.Fatalf("expected completion marker row, got err: %v", err)
	}
	if got == "" {
		t.Errorf("expected non-empty timestamp, got empty string")
	}
}
