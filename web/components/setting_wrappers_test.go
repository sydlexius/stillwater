package components

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// textChild returns a simple templ.Component that writes the given literal
// string. Used in tests to supply children to wrapper components without
// pulling in full domain types.
func textChild(s string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	})
}

// TestSettingRow_NoInputID verifies that when inputID is empty the label is
// rendered as a plain <div>, making no false label-for-control association.
func TestSettingRow_NoInputID(t *testing.T) {
	var buf bytes.Buffer
	child := textChild(`<select id="my-select"></select>`)
	ctx := templ.WithChildren(context.Background(), child)
	if err := SettingRow("My Label", "", "").Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Label rendered as a <div>, not a <label>.
	if !strings.Contains(out, `<div class="block text-sm font-medium text-gray-700 dark:text-gray-300">My Label</div>`) {
		t.Errorf("expected plain div label; got:\n%s", out)
	}
	// No <label for=...> emitted.
	if strings.Contains(out, `<label`) {
		t.Errorf("unexpected <label> element when inputID is empty; got:\n%s", out)
	}
	// Child control present.
	if !strings.Contains(out, `<select id="my-select">`) {
		t.Errorf("child control missing from output; got:\n%s", out)
	}
	// No desc paragraph when desc is empty.
	if strings.Contains(out, `<p class="mt-1 text-xs`) {
		t.Errorf("unexpected desc paragraph when desc is empty; got:\n%s", out)
	}
}

// TestSettingRow_WithInputID verifies that a non-empty inputID causes the
// label to be rendered as <label for="{inputID}"> so the browser associates
// the label with its control for accessibility.
func TestSettingRow_WithInputID(t *testing.T) {
	var buf bytes.Buffer
	child := textChild(`<input type="text" id="base-path-field">`)
	ctx := templ.WithChildren(context.Background(), child)
	if err := SettingRow("Base Path", "", "base-path-field").Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Label rendered as <label for=...>.
	if !strings.Contains(out, `<label for="base-path-field"`) {
		t.Errorf("expected <label for=\"base-path-field\">; got:\n%s", out)
	}
	if !strings.Contains(out, `class="block text-sm font-medium text-gray-700 dark:text-gray-300"`) {
		t.Errorf("expected label class; got:\n%s", out)
	}
	if !strings.Contains(out, `Base Path`) {
		t.Errorf("label text missing; got:\n%s", out)
	}
	// No stray <div class="block text-sm..."> when inputID is set.
	if strings.Contains(out, `<div class="block text-sm font-medium`) {
		t.Errorf("unexpected plain-div label when inputID is set; got:\n%s", out)
	}
}

// TestSettingRow_WithDesc verifies that a non-empty desc value causes a helper
// <p> to appear below the child control.
func TestSettingRow_WithDesc(t *testing.T) {
	var buf bytes.Buffer
	child := textChild(`<input type="text" id="field">`)
	ctx := templ.WithChildren(context.Background(), child)
	if err := SettingRow("Label", "Helper text shown below the control.", "field").Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, `<p class="mt-1 text-xs text-gray-500 dark:text-gray-400">Helper text shown below the control.</p>`) {
		t.Errorf("expected desc paragraph; got:\n%s", out)
	}
}

// TestSettingSection_Structure verifies the card+header shell emitted by
// SettingSection matches the pattern every settings card hand-rolled before
// the M55 shared layer was introduced: .sw-card > border-b header (h2 +
// ContextHelp + desc) > px-6 py-4 space-y-4 body.
func TestSettingSection_Structure(t *testing.T) {
	var buf bytes.Buffer
	child := textChild(`<p id="section-body">body content</p>`)
	ctx := templ.WithChildren(context.Background(), child)
	if err := SettingSection(
		"help-test-section",
		"Section Title",
		"Help text for this section.",
		"settings-general-base-path",
		"One-line section description.",
	).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Outer card div.
	if !strings.Contains(out, `class="sw-card bg-white dark:bg-gray-800 shadow rounded-lg"`) {
		t.Errorf("sw-card class missing; got:\n%s", out)
	}
	// Header border div.
	if !strings.Contains(out, `class="px-6 py-4 border-b border-gray-200 dark:border-gray-700"`) {
		t.Errorf("header border class missing; got:\n%s", out)
	}
	// Title h2.
	if !strings.Contains(out, `<h2 class="text-lg font-semibold">Section Title</h2>`) {
		t.Errorf("h2 title missing; got:\n%s", out)
	}
	// Description paragraph.
	if !strings.Contains(out, `class="mt-1 text-sm text-gray-500 dark:text-gray-400"`) {
		t.Errorf("description class missing; got:\n%s", out)
	}
	if !strings.Contains(out, `One-line section description.`) {
		t.Errorf("description text missing; got:\n%s", out)
	}
	// Body div with correct spacing class.
	if !strings.Contains(out, `class="px-6 py-4 space-y-4"`) {
		t.Errorf("body space-y-4 class missing; got:\n%s", out)
	}
	// Children rendered inside the body.
	if !strings.Contains(out, `id="section-body"`) {
		t.Errorf("child content missing from body; got:\n%s", out)
	}
}

// TestSettingSection_WithSettingRow exercises both wrappers composed together:
// a SettingSection containing a SettingRow with an inputID, mirroring the
// usage pattern expected once stable settings sections are extracted to the
// shared layer.
func TestSettingSection_WithSettingRow(t *testing.T) {
	var buf bytes.Buffer
	// Inner control wrapped in a SettingRow with an associated inputID.
	rowChild := textChild(`<input type="text" id="cache-max-size" value="256">`)
	rowCtx := templ.WithChildren(context.Background(), rowChild)
	rowHTML := new(bytes.Buffer)
	if err := SettingRow("Cache Size", "Maximum size of the image cache.", "cache-max-size").Render(rowCtx, rowHTML); err != nil {
		t.Fatalf("render row: %v", err)
	}
	// Wrap the rendered row as a child of SettingSection.
	sectionChild := textChild(rowHTML.String())
	sectionCtx := templ.WithChildren(context.Background(), sectionChild)
	if err := SettingSection(
		"help-image-cache",
		"Image Cache",
		"Controls the on-disk image cache.",
		"settings-general-image-cache",
		"Configure cache size and eviction.",
	).Render(sectionCtx, &buf); err != nil {
		t.Fatalf("render section: %v", err)
	}
	out := buf.String()

	// Section card present.
	if !strings.Contains(out, `sw-card`) {
		t.Errorf("sw-card missing; got:\n%s", out)
	}
	// SettingRow label rendered as <label for=...> inside the section.
	if !strings.Contains(out, `<label for="cache-max-size"`) {
		t.Errorf("SettingRow label missing inside SettingSection; got:\n%s", out)
	}
	// SettingRow desc present.
	if !strings.Contains(out, `Maximum size of the image cache.`) {
		t.Errorf("SettingRow desc missing; got:\n%s", out)
	}
	// Input control present.
	if !strings.Contains(out, `id="cache-max-size"`) {
		t.Errorf("control id missing; got:\n%s", out)
	}
}
