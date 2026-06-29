package templates

import (
	"bytes"
	"strings"
	"testing"
)

// TestSettingsUsersScript_NoOneShotLoadTriggers guards against the #1682
// regression class: using `htmx.trigger(elem, 'load')` to refresh an element
// whose hx-trigger="load" was already consumed by htmx at element-init time.
// The trigger is a no-op against the one-shot load handler, so the table
// stays stale while the upstream mutation's success toast fires. Fix uses
// htmx.ajax directly. This test pins the inline JS away from the broken
// pattern so a future contributor cannot re-introduce it for either the
// users table or the invites list (both render with hx-trigger="load").
func TestSettingsUsersScript_NoOneShotLoadTriggers(t *testing.T) {
	var buf bytes.Buffer
	if err := SettingsUsersScript().Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	js := buf.String()

	// The exact textual forms #1682 fixed. Each is paired with the element
	// id whose hx-trigger="load" makes the trigger a no-op. Re-introducing
	// any of these should fail the test, regardless of whether the new code
	// is the same regression or a near-miss.
	forbidden := []string{
		`htmx.trigger(document.getElementById('users-table-body'), 'load')`,
		`htmx.trigger(document.getElementById('invites-list'), 'load')`,
	}
	for _, pat := range forbidden {
		if strings.Contains(js, pat) {
			t.Errorf("script re-introduced #1682 anti-pattern: %s", pat)
		}
	}

	// Conversely, the refresh path must use htmx.ajax for the users-table-
	// body and invites-list URLs. These positive assertions break if a
	// refactor accidentally drops the refresh entirely (the table would
	// stop updating without any DOM-side signal).
	// Refresh URLs are root-relative on purpose: htmx's configRequest
	// listener in layout.templ prepends the meta htmx-base-path to every
	// absolute HTMX path. Concatenating usersBasePath here would double-
	// prefix on sub-path deployments. (fetch() call sites in the same
	// templ file legitimately keep usersBasePath because they are not
	// intercepted by configRequest.)
	required := []string{
		`htmx.ajax('GET', url, { target: '#users-table-body', swap: 'innerHTML' })`,
		`htmx.ajax('GET', '/api/v1/users/invites'`,
	}
	for _, pat := range required {
		if !strings.Contains(js, pat) {
			t.Errorf("script missing required refresh call: %s", pat)
		}
	}

	// refreshUsersTable() must be defined and called from the three
	// mutation handlers that previously each open-coded an htmx.trigger
	// against users-table-body. Centralizing the refresh into one helper
	// is what makes the .catch and null-guard added in the same fix apply
	// uniformly to every refresh path.
	if !strings.Contains(js, `function refreshUsersTable()`) {
		t.Errorf("script missing refreshUsersTable() helper definition")
	}
	callCount := strings.Count(js, `refreshUsersTable();`)
	if callCount < 3 {
		t.Errorf("expected refreshUsersTable() to be called at least 3 times (reload + role-change success + role-change failure + deactivate), got %d", callCount)
	}
}
