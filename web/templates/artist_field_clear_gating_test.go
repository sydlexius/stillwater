package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// TestFieldEdit_ClearGating pins the edit-mode Clear-control gating
// (M55 #1336, 4A). The Clear (hx-delete) affordance renders only when the field
// has a value to clear AND is not "name". Two reasons it is gated:
//   - "name": the server rejects a name clear with HTTP 400 (handleFieldClear).
//   - empty field: an empty-field clear is not a no-op (it republishes metadata
//     and emits a spurious "cleared" activity), so Clear is hidden when empty.
//
// This test pins all three outcomes at the template layer so a regression in the
// if-gate is caught: name -> absent; empty "died" -> absent; non-empty "died" ->
// present.
func TestFieldEdit_ClearGating(t *testing.T) {
	// The Clear control is the hx-delete on the field's clear endpoint. It is
	// distinct from the edit form's hx-patch to the same /fields/<field> path,
	// so the assertions key on the hx-delete attribute specifically.
	clearAttr := func(field string) string {
		return `hx-delete="/api/v1/artists/ar-clear-1/fields/` + field + `"`
	}
	render := func(t *testing.T, a *artist.Artist, field string) string {
		t.Helper()
		var buf bytes.Buffer
		if err := FieldEdit(a, field, nil, nil).Render(testCtx(t), &buf); err != nil {
			t.Fatalf("render %s: %v", field, err)
		}
		return buf.String()
	}

	// name: Clear must always be omitted (non-clearable server-side), even with a
	// value present.
	withName := &artist.Artist{ID: "ar-clear-1", Name: "Test Artist", Type: "person"}
	if got := render(t, withName, "name"); strings.Contains(got, clearAttr("name")) {
		t.Errorf("FieldEdit(name) rendered a Clear control %q, want it omitted; got:\n%s", clearAttr("name"), got)
	}

	// died, empty: Clear must be omitted (nothing to clear; avoids a no-op write).
	emptyDied := &artist.Artist{ID: "ar-clear-1", Name: "Test Artist", Type: "person"}
	if got := render(t, emptyDied, "died"); strings.Contains(got, clearAttr("died")) {
		t.Errorf("FieldEdit(died) on an empty field rendered a Clear control %q, want it omitted; got:\n%s", clearAttr("died"), got)
	}

	// died, non-empty: a clearable detail field with a value must keep Clear.
	setDied := &artist.Artist{ID: "ar-clear-1", Name: "Test Artist", Type: "person", Died: "1990-01-01"}
	if got := render(t, setDied, "died"); !strings.Contains(got, clearAttr("died")) {
		t.Errorf("FieldEdit(died) with a value missing the Clear control %q; got:\n%s", clearAttr("died"), got)
	}
}
