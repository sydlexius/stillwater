package templates

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/rule"
)

// TestArtistViolationsTab_DoubleSubmitGuard pins the per-row double-submit
// guard. The artist-detail violations tab uses raw fetch() rather than
// htmx, so the guard lives in the templ script function: both onclick
// handlers must disable the row's action buttons before issuing the POST
// and re-enable them in finally(). We assert the rendered tab contains
// the disable code so a future refactor cannot quietly drop it.
//
// We also assert finally() and window.showToast appear in the rendered
// script: finally guarantees re-enable on every settled outcome (success,
// HTTP error, network error) and showToast is the user-facing failure
// surface that replaces the prior native alert() dialog.
func TestArtistViolationsTab_DoubleSubmitGuard(t *testing.T) {
	data := ArtistViolationsTabData{
		ArtistID: "a-1110",
		Violations: []rule.RuleViolation{
			{
				ID:         "v-1110",
				RuleID:     rule.RuleNFOExists,
				ArtistID:   "a-1110",
				ArtistName: "Tab Test Artist",
				Severity:   "error",
				Message:    "missing nfo",
				Fixable:    true,
				Status:     rule.ViolationStatusOpen,
				CreatedAt:  time.Now().UTC(),
			},
		},
	}

	var buf bytes.Buffer
	if err := ArtistViolationsTab(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// The Fix and Dismiss buttons render their JS via templ script blocks,
	// which the templ runtime may emit either inline or in a hoisted
	// <script> tag. Either way the function bodies appear in the rendered
	// HTML for the tab, so substring assertions are stable across both
	// emission modes.

	// Disable-on-click: every settled outcome must re-enable, so the
	// finally() block is required. Without it a network error would leave
	// the row's buttons stuck in the disabled state.
	if !strings.Contains(body, "actionBtns[i].disabled = true") {
		t.Errorf("artist-tab fix/dismiss script missing the in-flight disable assignment; got:\n%s", body)
	}
	if !strings.Contains(body, "actionBtns[j].disabled = false") {
		t.Errorf("artist-tab fix/dismiss script missing the finally() re-enable assignment; got:\n%s", body)
	}
	if !strings.Contains(body, ".finally(function()") {
		t.Errorf("artist-tab fix/dismiss script missing finally() block; got:\n%s", body)
	}

	// User-facing failure surface: the script must route failures through
	// window.showToast so the artist tab matches the dashboard toast
	// styling instead of using a native browser alert dialog.
	if !strings.Contains(body, "window.showToast") {
		t.Errorf("artist-tab fix/dismiss script missing window.showToast routing; got:\n%s", body)
	}

	// Disabled-state styling so the visual feedback matches user expectation
	// while the request is in flight. Without this the buttons stay clickable-
	// looking even though the disabled property suppresses the click.
	if !strings.Contains(body, "disabled:opacity-60") {
		t.Errorf("artist-tab action buttons missing disabled:opacity-60 styling; got:\n%s", body)
	}
}

// TestArtistFindingsList_RendersList pins the next/ findings-list: it renders a
// readable <ul> (NOT the stable <table>), inside the shared #violations-content
// wrapper, with the severity, rule, message, Fix/Dismiss actions, and the OOB
// tab-badge swap so violations-sync.js refreshes it identically.
func TestArtistFindingsList_RendersList(t *testing.T) {
	data := ArtistViolationsTabData{
		ArtistID: "a-2222",
		Violations: []rule.RuleViolation{
			{
				ID:        "v-2222",
				RuleID:    rule.RuleNFOExists,
				ArtistID:  "a-2222",
				Severity:  "warning",
				Message:   "missing nfo file",
				Fixable:   true,
				Status:    rule.ViolationStatusOpen,
				CreatedAt: time.Now().UTC(),
			},
		},
	}
	var buf bytes.Buffer
	if err := ArtistFindingsList(data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		`id="violations-content-a-2222"`,  // shared wrapper for live-sync
		"<ul",                             // a list, not a table
		`id="artist-violation-v-2222"`,    // row id the fix/dismiss scripts look up
		`class="sw-next-finding-sev"`,     // severity chrome
		"warning",                         // the severity word itself
		`class="sw-next-finding-fix"`,     // fix action (Fixable + open)
		`class="sw-next-finding-dismiss"`, // dismiss action
		"missing nfo file",                // message
		`id="violations-tab-badge"`,       // OOB badge swap
	} {
		if !strings.Contains(body, want) {
			t.Errorf("findings list missing %q", want)
		}
	}
	if strings.Contains(body, "<table") {
		t.Errorf("next/ findings should be a list, not a table")
	}
}

// TestArtistFindingsList_Empty renders the empty state without a list/table.
func TestArtistFindingsList_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistFindingsList(ArtistViolationsTabData{ArtistID: "a-0"}).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if strings.Contains(body, "<ul") || strings.Contains(body, "<table") {
		t.Errorf("empty findings should render neither a list nor a table")
	}
	if !strings.Contains(body, `id="violations-content-a-0"`) {
		t.Errorf("empty findings missing the shared wrapper")
	}
}
