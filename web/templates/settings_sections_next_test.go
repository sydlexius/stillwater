package templates

// settings_sections_next_test.go -- hook-parity + #1917 regression gate for the
// next/-only redesigned Connections section (M55 #2117, SectionServersNext).
//
// These tests encode the hook-diff self-check from
// /tmp/m55-2147/settings-hook-inventory.md as CI: every JS-hook id, data-*
// attr, feature-toggle class name, and API endpoint the connection JS depends
// on MUST survive in the redesigned next/ markup, and the #1917 dead toggles
// (library_import / nfo_write) MUST be gone while image_write stays gated to
// Emby/Jellyfin.
//
// Fixtures (threeConnections) are shared from settings_s3_golden_test.go.

import (
	"bytes"
	"strings"
	"testing"
)

// TestSectionServersNext_PreservesConnectionHooks asserts the fragile
// per-connection hook contract survives the redesign for every connection.
func TestSectionServersNext_PreservesConnectionHooks(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Connections: threeConnections}
	var buf bytes.Buffer
	if err := SectionServersNext(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// The targeted-refresh anchor must be present exactly once on the list.
	if got := strings.Count(out, `data-settings-fragment="connections"`); got != 1 {
		t.Errorf(`data-settings-fragment="connections": want 1, got %d`, got)
	}

	// Per-connection disclosure + toggle hooks (inventory GOTCHAS #1-#3).
	for _, c := range threeConnections {
		mustContain := []string{
			`id="connection-` + c.ID + `"`,
			`id="discover-` + c.ID + `"`,
			`id="features-` + c.ID + `"`,
			`id="edit-panel-` + c.ID + `"`,
			`id="edit-result-` + c.ID + `"`,
			`id="detected-` + c.ID + `"`,
			`id="stillwater-managed-` + c.ID + `"`,
			`data-conn-id="` + c.ID + `"`,
			// self-describing toggle attrs the JS repaints from
			`data-sw-btn-on=`, `data-sw-btn-off=`, `data-sw-knob-on=`, `data-sw-knob-off=`, `data-sw-error=`,
			// endpoints
			`/api/v1/connections/` + c.ID + `/test`,
			`/api/v1/connections/` + c.ID + `/stillwater-managed`,
			`/api/v1/connections/` + c.ID + `/conflict-detail`,
			`hx-put="/api/v1/connections/` + c.ID + `"`,
			// edit form fields
			`id="edit-name-` + c.ID + `"`, `id="edit-url-` + c.ID + `"`, `id="edit-api-key-` + c.ID + `"`,
		}
		for _, want := range mustContain {
			if !strings.Contains(out, want) {
				t.Errorf("connection %s (%s): missing hook %q", c.ID, c.Type, want)
			}
		}
	}

	// The feature-toggle knob/track class names the JS adds/removes BY NAME
	// (ruleToggleBtnClasses/KnobClasses) must appear verbatim, and the disclosure
	// aria-controls + hidden panels must be present.
	for _, want := range []string{"bg-blue-600", "translate-x-5", `aria-controls="features-`, `aria-controls="edit-panel-`, `class="hidden`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing JS-owned class/attr %q", want)
		}
	}

	// Add-form hooks per type (conn-form / conn-name / conn-url / conn-api-key /
	// conn-result) + the single Add-server entry point.
	for _, ct := range []string{"emby", "jellyfin", "lidarr"} {
		for _, want := range []string{
			`id="conn-form-` + ct + `"`,
			`id="conn-name-` + ct + `"`,
			`id="conn-url-` + ct + `"`,
			`id="conn-api-key-` + ct + `"`,
			`id="conn-result-` + ct + `"`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("add form %s: missing %q", ct, want)
			}
		}
	}
}

// TestSectionServersNext_Feature1917Cleanup asserts the #1917 toggle cleanup:
// the dead library_import / nfo_write toggles are dropped, image_write is
// rendered for Emby/Jellyfin but NOT Lidarr, and the Lidarr-only verify-path
// toggle is present only for Lidarr.
func TestSectionServersNext_Feature1917Cleanup(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Connections: threeConnections}
	var buf bytes.Buffer
	if err := SectionServersNext(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Dead toggles must be gone entirely (they were never read as a gate).
	for _, dead := range []string{`data-feature="library_import"`, `data-feature="nfo_write"`} {
		if strings.Contains(out, dead) {
			t.Errorf("#1917: dead toggle still rendered: %s", dead)
		}
	}

	// image_write: exactly one per non-Lidarr connection (emby + jellyfin = 2).
	if got := strings.Count(out, `data-feature="image_write"`); got != 2 {
		t.Errorf(`#1917: image_write toggle count: want 2 (emby+jellyfin, not lidarr), got %d`, got)
	}

	// verify-path toggle only for the Lidarr connection.
	if got := strings.Count(out, `id="verify-path-conn-lidarr"`); got != 1 {
		t.Errorf("verify-path (lidarr-only): want 1, got %d", got)
	}
	if strings.Contains(out, `id="verify-path-conn-emby"`) || strings.Contains(out, `id="verify-path-conn-jellyfin"`) {
		t.Errorf("verify-path leaked onto a non-Lidarr connection")
	}
}

// TestSectionServersNext_Empty asserts the empty state still renders the
// refresh anchor and the Add-server entry point.
func TestSectionServersNext_Empty(t *testing.T) {
	ctx := testCtx(t)
	var buf bytes.Buffer
	if err := SectionServersNext(SettingsData{Connections: nil}).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{`data-settings-fragment="connections"`, `id="server-add-panel"`, `id="conn-form-emby"`} {
		if !strings.Contains(out, want) {
			t.Errorf("empty state missing %q", want)
		}
	}
}
