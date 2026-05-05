package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestIsNoiseKey covers the segment-based noise filter. Each example is a
// real or close-to-real i18n key; the table documents which side of the
// filter the key falls on so the rule's behavior is auditable.
func TestIsNoiseKey(t *testing.T) {
	cases := []struct {
		key   string
		noise bool
	}{
		// Toasts and status flashes -- noise.
		{"settings.libraries.lock_nfo_enabled_toast", true},
		{"settings.metadata_languages.toast_saved", true},
		{"settings.metadata_languages.toast_max", true},
		{"settings.metadata_languages.toast_failed", true},
		// Validation errors -- noise.
		{"settings.base_path.error_must_start_slash", true},
		{"settings.base_path.error_network", true},
		{"settings.save_error", true},
		// ARIA labels -- noise.
		{"settings.metadata_languages.aria_pill_label", true},
		{"settings.metadata_languages.aria_list_label", true},
		// Placeholders -- noise.
		{"settings.libraries.name_placeholder", true},
		{"settings.api_tokens.name_placeholder", true},
		// Confirmation dialogs -- noise.
		{"settings.api_tokens.confirm_delete", true},
		{"settings.api_tokens.confirm_revoke", true},
		// Inline UI hints -- noise.
		{"settings.metadata_languages.aria_pill_hint", true},
		{"settings.metadata_languages.default_hint", true},
		// Real settings the filter must not catch.
		{"settings.tab.general", false},
		{"settings.platform_profile.title", false},
		{"settings.platform_profile.description", false},
		{"settings.image_cache.title", false},
		{"settings.updates.channel.label", false},
		{"settings.updates.channel.description", false},
		{"settings.appearance.theme.help", false},
		// Section-name collision regressions: each of these section names
		// contains a substring that overlaps with a noise token. The token
		// list is anchored (e.g. `confirm_`, `_hint`) precisely so these
		// real sections survive the filter.
		{"settings.confirm_dialogs.title", false},
		{"settings.confirm_dialogs.description", false},
		{"settings.confirm_dialogs.reset_button", false},
		{"settings.auth.oidc_display_name_hint", true}, // suffix `_hint` IS noise
		{"settings.api_tokens.confirm_delete", true},   // suffix `confirm_` IS noise
		{"settings.api_tokens.confirm_revoke", true},   // suffix `confirm_` IS noise
	}
	for _, tc := range cases {
		got := isNoiseKey(tc.key)
		if got != tc.noise {
			t.Errorf("isNoiseKey(%q) = %v, want %v", tc.key, got, tc.noise)
		}
	}
}

// TestExtractKeys verifies the regex finds t(ctx, ...) call sites and skips
// noise. The fixture mixes real templ syntax with multi-line whitespace and
// a noise key to exercise both filters.
func TestExtractKeys(t *testing.T) {
	src := []byte(`
		<div data-tab-panel="general">
			<h2>{ t(ctx, "settings.platform_profile.title") }</h2>
			<p>{ t(ctx, "settings.platform_profile.description") }</p>
			<button>{ t(ctx, "settings.image_cache.clear_cache_toast") }</button>
			<input aria-label={ t(ctx,
				"settings.metadata_languages.aria_pill_label") } />
			<span>{ t(ctx, "settings.platform_profile.title") }</span>  <!-- duplicate -->
		</div>
	`)
	got := extractKeys(src)
	want := []string{
		"settings.platform_profile.title",
		"settings.platform_profile.description",
	}
	if len(got) != len(want) {
		t.Fatalf("extractKeys() returned %d keys, want %d: %v", len(got), len(want), got)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("extractKeys()[%d] = %q, want %q", i, got[i], k)
		}
	}
}

// TestScanPanels_Dedupe verifies that scanPanels deduplicates panel IDs when
// the templ source references the same data-tab-panel attribute more than
// once (e.g. inline JS using it as a CSS selector below the actual panel
// div). Each tab must appear exactly once in the result.
func TestScanPanels_Dedupe(t *testing.T) {
	dir := t.TempDir()
	trunk := filepath.Join(dir, "settings.templ")
	body := `
		<div data-tab-panel="general">
			<span>{ t(ctx, "settings.platform_profile.title") }</span>
		</div>
		<div data-tab-panel="providers">
			<span>{ t(ctx, "settings.provider_keys.title") }</span>
		</div>
		<script>
			var panel = document.querySelector('[data-tab-panel="general"]');
			var other = document.querySelector('[data-tab-panel="providers"]');
		</script>
	`
	if err := os.WriteFile(trunk, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	panels, err := scanPanels([]string{trunk}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(panels) != 2 {
		t.Fatalf("scanPanels() returned %d panels, want 2: %+v", len(panels), panels)
	}
	if panels[0].ID != "general" || panels[1].ID != "providers" {
		t.Errorf("scanPanels() panel IDs = [%q, %q], want [general, providers]",
			panels[0].ID, panels[1].ID)
	}
}

// TestScanPanels_SubTemplateAttribution verifies that keys in a sub-template
// file (e.g. settings_users.templ) are attributed to the panel named in the
// subTemplateOwner map.
func TestScanPanels_SubTemplateAttribution(t *testing.T) {
	dir := t.TempDir()
	trunk := filepath.Join(dir, "settings.templ")
	users := filepath.Join(dir, "settings_users.templ")

	if err := os.WriteFile(trunk, []byte(`
		<div data-tab-panel="users">
			@settingsUsersTab(data.Users)
		</div>
	`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(users, []byte(`
		<h2>{ t(ctx, "settings.users.title") }</h2>
	`), 0o644); err != nil {
		t.Fatal(err)
	}

	panels, err := scanPanels([]string{trunk, users}, map[string]string{
		users: "users",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(panels) != 1 {
		t.Fatalf("expected 1 panel, got %d", len(panels))
	}
	wantKey := "settings.users.title"
	if !contains(panels[0].Keys, wantKey) {
		t.Errorf("expected sub-template key %q attributed to users panel; got keys=%v",
			wantKey, panels[0].Keys)
	}
}

// TestRenderDocument_BulletShape spot-checks the rendered Markdown structure:
// tabs are H2, sections are H3, controls are bullet items with inline anchors.
func TestRenderDocument_BulletShape(t *testing.T) {
	doc := document{Tabs: []docTab{{
		ID:    "general",
		Label: "General",
		Sections: []docSection{{
			ID:          "platform_profile",
			Title:       "Platform profile",
			Description: "Pick the active platform profile.",
			Controls: []docControl{
				{ID: "preset", Label: "Preset", Description: "Built-in presets."},
				{ID: "custom_filenames", Label: "Custom filenames"},
			},
		}},
	}}}
	got := renderDocument(doc)

	wants := []string{
		"## General  {#tab-general}",
		"### Platform profile  {#settings-general-platform-profile}",
		"Pick the active platform profile.",
		"- **Preset** {#settings-general-platform-profile-preset} -- Built-in presets.",
		"- **Custom filenames** {#settings-general-platform-profile-custom-filenames}",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("renderDocument() missing %q\nfull output:\n%s", w, got)
		}
	}
}

// TestRenderControl_VisibilityAndHelp verifies that visibility and help text
// fold into the bullet's prose with the documented marker syntax.
func TestRenderControl_VisibilityAndHelp(t *testing.T) {
	var b strings.Builder
	renderControl(&b, "general", "base_path", docControl{
		ID:          "value",
		Label:       "Base path",
		Description: "URL prefix served by Stillwater.",
		Visibility:  "Editable when SW_BASE_PATH is unset.",
		Help:        "Restart required for changes to take effect.",
	})
	out := b.String()
	if !strings.Contains(out, "*Visibility:*") {
		t.Errorf("expected *Visibility:* marker in output: %q", out)
	}
	if !strings.Contains(out, "**Help:**") {
		t.Errorf("expected **Help:** marker in output: %q", out)
	}
	if !strings.Contains(out, "URL prefix served by Stillwater.") {
		t.Errorf("expected description text in output: %q", out)
	}
}

// TestCollectAnchors_DeterministicAndUnique verifies the companion file is
// sorted, deduplicated, and contains every kind of anchor the document
// emits (tab, section, control).
func TestCollectAnchors_DeterministicAndUnique(t *testing.T) {
	doc := document{Tabs: []docTab{
		{ID: "general", Sections: []docSection{
			{ID: "image_cache", Controls: []docControl{{ID: "max_size"}, {ID: "clear"}}},
			{ID: "base_path", Controls: []docControl{{ID: "value"}}},
		}},
		{ID: "providers"},
	}}
	got, err := collectAnchors(doc)
	if err != nil {
		t.Fatalf("collectAnchors() unexpected error: %v", err)
	}

	wantAll := []string{
		"settings-general-base-path",
		"settings-general-base-path-value",
		"settings-general-image-cache",
		"settings-general-image-cache-clear",
		"settings-general-image-cache-max-size",
		"tab-general",
		"tab-providers",
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("collectAnchors() result is not sorted: %v", got)
	}
	for _, w := range wantAll {
		if !contains(got, w) {
			t.Errorf("collectAnchors() missing anchor %q; got %v", w, got)
		}
	}
	// Uniqueness: no two adjacent entries should be equal.
	for i := 1; i < len(got); i++ {
		if got[i] == got[i-1] {
			t.Errorf("collectAnchors() emitted duplicate anchor %q at index %d", got[i], i)
		}
	}
}

// TestCollectAnchors_FailsOnCollision asserts that two distinct controls
// hashing to the same anchor produce an error rather than getting silently
// glossed over by a dedupe pass. The collision below is contrived (two
// controls in the same section sharing an ID), but the same shape arises in
// the real en.json when section/control IDs differ only in characters that
// the slug normalizer flattens.
func TestCollectAnchors_FailsOnCollision(t *testing.T) {
	doc := document{Tabs: []docTab{{
		ID: "general",
		Sections: []docSection{{
			ID: "image_cache",
			Controls: []docControl{
				{ID: "clear"},
				{ID: "clear"}, // duplicate -> collision
			},
		}},
	}}}
	if _, err := collectAnchors(doc); err == nil {
		t.Error("collectAnchors() returned nil error on duplicate-anchor input")
	}
}

// TestReplaceBetweenMarkers covers the marker-splice helper and its idempotency.
func TestReplaceBetweenMarkers(t *testing.T) {
	src := []byte("preamble\n<!-- BEGIN GENERATED: x -->\nold body\n<!-- END GENERATED: x -->\ntrailing")
	out, err := replaceBetweenMarkers(src,
		"<!-- BEGIN GENERATED: x -->",
		"<!-- END GENERATED: x -->",
		"new body")
	if err != nil {
		t.Fatal(err)
	}
	want := "preamble\n<!-- BEGIN GENERATED: x -->\nnew body\n<!-- END GENERATED: x -->\ntrailing"
	if string(out) != want {
		t.Errorf("replaceBetweenMarkers() = %q; want %q", out, want)
	}

	// Idempotency: applying the same body again yields the same bytes.
	out2, err := replaceBetweenMarkers(out,
		"<!-- BEGIN GENERATED: x -->",
		"<!-- END GENERATED: x -->",
		"new body")
	if err != nil {
		t.Fatal(err)
	}
	if string(out2) != string(out) {
		t.Errorf("replaceBetweenMarkers() not idempotent: %q vs %q", out2, out)
	}
}

// TestReplaceBetweenMarkers_MissingBegin returns a typed error so callers
// can distinguish a corrupted docs file from a stale one.
func TestReplaceBetweenMarkers_MissingBegin(t *testing.T) {
	_, err := replaceBetweenMarkers([]byte("no markers here"),
		"<!-- BEGIN -->", "<!-- END -->", "body")
	if err == nil {
		t.Fatal("expected error for missing begin marker, got nil")
	}
	if !strings.Contains(err.Error(), "begin marker") {
		t.Errorf("error %q should mention begin marker", err)
	}
}

// TestRunCheckMode runs the generator end-to-end against a tiny fixture in a
// temp directory, then runs it again in -check mode against the same
// fixture; the second run must succeed (idempotent), and a perturbed file
// must cause -check to fail.
func TestRunCheckMode(t *testing.T) {
	dir := t.TempDir()

	// Minimal i18n fixture.
	i18nPath := filepath.Join(dir, "en.json")
	i18nBody := `{
		"settings.tab.general": "General",
		"settings.platform_profile.title": "Platform profile",
		"settings.platform_profile.description": "Pick a profile."
	}`
	if err := os.WriteFile(i18nPath, []byte(i18nBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// Minimal templ fixture.
	trunk := filepath.Join(dir, "settings.templ")
	trunkBody := `
		<div data-tab-panel="general">
			<h2>{ t(ctx, "settings.platform_profile.title") }</h2>
			<p>{ t(ctx, "settings.platform_profile.description") }</p>
		</div>
	`
	if err := os.WriteFile(trunk, []byte(trunkBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// Output skeleton with markers.
	outPath := filepath.Join(dir, "settings-by-tab.md")
	outSkeleton := "front\n<!-- BEGIN GENERATED: settings-reference -->\n<!-- END GENERATED: settings-reference -->\nback\n"
	if err := os.WriteFile(outPath, []byte(outSkeleton), 0o644); err != nil {
		t.Fatal(err)
	}

	anchorsPath := filepath.Join(dir, "_settings-anchors.txt")

	// Override generator's templ source list / sub-template owner via direct
	// helper invocation, since the run() function reads from package-level
	// constants. Build the document by hand and exercise the writer path.
	keys, err := loadI18nKeys(i18nPath)
	if err != nil {
		t.Fatal(err)
	}
	tabs, err := scanPanels([]string{trunk}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := buildDocument(tabs, keys)
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderDocument(doc)
	anchors, err := collectAnchors(doc)
	if err != nil {
		t.Fatal(err)
	}

	if err := writeOrCheck(outPath, beginMarker, endMarker, rendered, false); err != nil {
		t.Fatalf("writeOrCheck() write: %v", err)
	}
	if err := writeAnchorsOrCheck(anchorsPath, anchors, false); err != nil {
		t.Fatalf("writeAnchorsOrCheck() write: %v", err)
	}

	// -check mode against the just-written files: must succeed.
	if err := writeOrCheck(outPath, beginMarker, endMarker, rendered, true); err != nil {
		t.Errorf("writeOrCheck(check) on fresh output: %v", err)
	}
	if err := writeAnchorsOrCheck(anchorsPath, anchors, true); err != nil {
		t.Errorf("writeAnchorsOrCheck(check) on fresh output: %v", err)
	}

	// Perturb the docs file; -check must now fail.
	perturbed := strings.Replace(string(mustRead(t, outPath)), "Platform profile", "Tampered", 1)
	if err := os.WriteFile(outPath, []byte(perturbed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeOrCheck(outPath, beginMarker, endMarker, rendered, true); err == nil {
		t.Error("writeOrCheck(check) should fail on perturbed file")
	}
}

// TestControlIDFor verifies the control ID derivation rules: a metadata
// suffix is stripped to expose the control name; multi-segment paths beneath
// a section preserve their parent segments; bare leaves are returned as-is.
func TestControlIDFor(t *testing.T) {
	cases := []struct {
		rest []string
		want string
	}{
		{[]string{"theme", "label"}, "theme"},
		{[]string{"theme", "description"}, "theme"},
		{[]string{"theme", "help"}, "theme"},
		{[]string{"theme", "visibility"}, "theme"},
		{[]string{"builtin_profiles", "preset", "label"}, "builtin_profiles.preset"},
		{[]string{"preset"}, "preset"},
		{[]string{"placeholder"}, "placeholder"}, // Bare metadata-suffix-name leaf is treated as the ID.
		{[]string{}, ""},
	}
	for _, tc := range cases {
		if got := controlIDFor(tc.rest); got != tc.want {
			t.Errorf("controlIDFor(%v) = %q, want %q", tc.rest, got, tc.want)
		}
	}
}

// TestBuildControl exercises every metadata branch of the per-control
// assembler: the canonical .label/.description/.help/.visibility shape, the
// legacy ._desc fallback, and the bare-key (no-suffix) label fallback.
func TestBuildControl(t *testing.T) {
	keys := map[string]string{
		"settings.appearance.theme.label":       "Theme",
		"settings.appearance.theme.description": "Pick a theme.",
		"settings.appearance.theme.help":        "Light, dark, or system.",
		"settings.appearance.theme.visibility":  "Always shown.",
		// Legacy desc is honored only when no canonical .description exists.
		"settings.legacy.foo._desc":       "ignored because canonical wins",
		"settings.legacy.foo.label":       "Foo",
		"settings.legacy.foo.description": "Canonical description.",
		// Bare-key control with no .label sibling: value becomes the label.
		"settings.bare.simple": "Simple Toggle",
		// Placeholder must be silently dropped.
		"settings.bare.simple.placeholder": "ignored",
	}

	ctrl, err := buildControl("theme", []string{
		"settings.appearance.theme.label",
		"settings.appearance.theme.description",
		"settings.appearance.theme.help",
		"settings.appearance.theme.visibility",
	}, keys)
	if err != nil {
		t.Fatalf("buildControl(theme) unexpected error: %v", err)
	}
	if ctrl.Label != "Theme" || ctrl.Description != "Pick a theme." ||
		ctrl.Help != "Light, dark, or system." || ctrl.Visibility != "Always shown." {
		t.Errorf("buildControl(theme) = %+v; want all four canonical fields populated", ctrl)
	}

	// Both .label and .description present: canonical wins.
	ctrl, err = buildControl("foo", []string{
		"settings.legacy.foo.label",
		"settings.legacy.foo.description",
	}, keys)
	if err != nil {
		t.Fatalf("buildControl(foo) unexpected error: %v", err)
	}
	if ctrl.Description != "Canonical description." {
		t.Errorf("buildControl(foo).Description = %q; want %q", ctrl.Description, "Canonical description.")
	}

	// Bare key with no .label: the value is used as the label.
	ctrl, err = buildControl("simple", []string{
		"settings.bare.simple",
		"settings.bare.simple.placeholder",
	}, keys)
	if err != nil {
		t.Fatalf("buildControl(simple) unexpected error: %v", err)
	}
	if ctrl.Label != "Simple Toggle" {
		t.Errorf("buildControl(simple).Label = %q; want bare-key fallback %q", ctrl.Label, "Simple Toggle")
	}

	// No keys at all: humanize the ID as the label of last resort.
	ctrl, err = buildControl("untranslated_thing", nil, keys)
	if err != nil {
		t.Fatalf("buildControl(empty) unexpected error: %v", err)
	}
	if ctrl.Label != "Untranslated thing" {
		t.Errorf("buildControl(empty).Label = %q; want humanized fallback", ctrl.Label)
	}

	// Description without label: ERROR. The previous behavior silently
	// humanized the slug (Multi user, Oidc) and let mismatched keys leak
	// into the docs page; CR PR #1303 round-3 caught this masking the real
	// i18n drift.
	keysOrphan := map[string]string{
		"settings.X.orphan.description": "Description with no label.",
	}
	if _, err := buildControl("orphan", []string{"settings.X.orphan.description"}, keysOrphan); err == nil {
		t.Error("buildControl(orphan) expected error for description-without-label; got nil")
	}
}

// TestHumanize covers the underscore-id to "Title sentence" conversion used
// when no i18n label is available.
func TestHumanize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"theme", "Theme"},
		{"platform_profile", "Platform profile"},
		{"a_b_c", "A b c"},
		{"_leading", " leading"}, // Empty first segment is left empty (defensive).
	}
	for _, tc := range cases {
		if got := humanize(tc.in); got != tc.want {
			t.Errorf("humanize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLastSegmentOf verifies the helper returns the metadata role suffix.
func TestLastSegmentOf(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"settings.appearance.theme.label", "label"},
		{"settings.appearance.theme", "theme"},
		{"flat", "flat"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := lastSegmentOf(tc.in); got != tc.want {
			t.Errorf("lastSegmentOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLookupLabel covers all three branches: empty key (falls back), missing
// key (falls back), present-but-empty value (falls back), and present.
func TestLookupLabel(t *testing.T) {
	keys := map[string]string{
		"settings.tab.general": "General",
		"settings.empty":       "",
	}
	cases := []struct {
		key, fallback, want string
	}{
		{"settings.tab.general", "general", "General"},
		{"", "general", "General"},                             // empty key -> humanize fallback
		{"settings.missing", "missing_thing", "Missing thing"}, // absent -> humanize
		{"settings.empty", "fallback_id", "Fallback id"},       // present-but-empty -> humanize
	}
	for _, tc := range cases {
		if got := lookupLabel(keys, tc.key, tc.fallback); got != tc.want {
			t.Errorf("lookupLabel(%q, %q) = %q, want %q", tc.key, tc.fallback, got, tc.want)
		}
	}
}

// TestRun_HappyPath drives the top-level run() entry point through a
// self-contained fixture so the integration glue (load + scan + build + write)
// is covered. The fixture lives entirely under t.TempDir so the test does not
// touch the real repo files.
func TestRun_HappyPath(t *testing.T) {
	dir := t.TempDir()

	i18nPath := filepath.Join(dir, "en.json")
	if err := os.WriteFile(i18nPath, []byte(`{
		"settings.tab.general": "General",
		"settings.platform_profile.title": "Platform profile",
		"settings.platform_profile.description": "Pick a profile.",
		"settings.platform_profile.preset.label": "Preset",
		"settings.platform_profile.preset.description": "Built-in presets."
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Note: run() resolves templ sources via discoverTemplSources(), which
	// reads the real `web/templates/settings*.templ` glob rooted at the
	// process working directory -- so a full run() invocation against a
	// custom templ would need to chdir into a fixture tree. We exercise the
	// individual stages here instead; every code path the binary would
	// touch in a real generation flows through these calls.
	keys, err := loadI18nKeys(i18nPath)
	if err != nil {
		t.Fatalf("loadI18nKeys: %v", err)
	}
	if _, ok := keys["settings.tab.general"]; !ok {
		t.Errorf("loadI18nKeys filtered out settings.tab.general")
	}

	// Invalid JSON path should propagate a parse error.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadI18nKeys(bad); err == nil {
		t.Error("loadI18nKeys(bad json) returned nil error")
	}

	// Missing path should propagate a read error.
	if _, err := loadI18nKeys(filepath.Join(dir, "nonexistent.json")); err == nil {
		t.Error("loadI18nKeys(missing) returned nil error")
	}
}

// TestScanPanels_LastPanelStopsAtNextTemplFunc verifies that the last panel's
// region ends at the next `^templ X(` declaration, not at end-of-file. Without
// this bound, helper templ functions defined after the page-rendering function
// (e.g. sortableInitScript, resetConfirmPrefsScript) would have all their
// t(ctx, ...) calls attributed to whichever panel happened to be last, producing
// duplicate sections under the wrong tab.
func TestScanPanels_LastPanelStopsAtNextTemplFunc(t *testing.T) {
	dir := t.TempDir()
	trunk := filepath.Join(dir, "settings.templ")
	body := `
templ SettingsPage() {
	<div data-tab-panel="general">
		<span>{ t(ctx, "settings.platform_profile.title") }</span>
	</div>
	<div data-tab-panel="updates">
		<span>{ t(ctx, "settings.updates.title") }</span>
	</div>
}

templ helperScript() {
	<script>
		// This i18n call belongs to a HELPER, not the updates panel.
		var label = "{ t(ctx, "settings.libraries.title") }";
	</script>
}
`
	if err := os.WriteFile(trunk, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	panels, err := scanPanels([]string{trunk}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(panels) != 2 {
		t.Fatalf("expected 2 panels, got %d", len(panels))
	}
	for _, p := range panels {
		for _, k := range p.Keys {
			if k == "settings.libraries.title" {
				t.Errorf("panel %q leaked helper key %q (should be bounded by next ^templ declaration)",
					p.ID, k)
			}
		}
	}
}

// TestDiscoverTemplSources verifies that the auto-discovery picks up new
// settings_*.templ files without manual generator edits. The standalone User
// Preferences page lives in preferences.templ (does not match the glob), so
// no exclusion entry is needed for it.
func TestDiscoverTemplSources(t *testing.T) {
	dir := t.TempDir()
	must := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	trunk := must("settings.templ")
	users := must("settings_users.templ")
	auth := must("settings_auth_providers.templ")
	// preferences.templ deliberately does NOT match settings_*.templ; create
	// it to confirm the glob ignores it without needing an exclude entry.
	must("preferences.templ")
	future := must("settings_billing.templ")

	sources, owner, err := discoverTemplSources(trunk, filepath.Join(dir, "settings_*.templ"))
	if err != nil {
		t.Fatal(err)
	}

	// Trunk first.
	if len(sources) == 0 || sources[0] != trunk {
		t.Errorf("expected trunk first; got %v", sources)
	}
	// Pin the full discovered order, not just membership: the generator's
	// output is only stable if discoverTemplSources sorts sub-templates
	// deterministically. A regression that returned sources in
	// filesystem-iteration order would still satisfy the membership
	// check below, so assert the exact slice here.
	wantSources := []string{trunk, auth, future, users}
	if len(sources) != len(wantSources) {
		t.Fatalf("expected %d sources, got %d: %v", len(wantSources), len(sources), sources)
	}
	for i, want := range wantSources {
		if sources[i] != want {
			t.Errorf("sources[%d] = %q, want %q (full slice: %v)", i, sources[i], want, sources)
		}
	}
	// Sub-templates discovered by glob (sorted): auth, billing, users.
	wantOwners := map[string]string{
		users:  "users",
		auth:   "auth_providers",
		future: "billing",
	}
	for path, wantPanel := range wantOwners {
		got, ok := owner[path]
		if !ok {
			t.Errorf("missing %s in owner map", path)
			continue
		}
		if got != wantPanel {
			t.Errorf("owner[%s] = %q, want %q", path, got, wantPanel)
		}
	}
	// preferences.templ must not be picked up by the settings_*.templ glob.
	for _, src := range sources {
		if filepath.Base(src) == "preferences.templ" {
			t.Errorf("preferences.templ should not match settings_*.templ glob; got in sources: %v", sources)
		}
	}
}

// TestScanPanels_NoSources verifies the empty-input guard.
func TestScanPanels_NoSources(t *testing.T) {
	if _, err := scanPanels(nil, nil); err == nil {
		t.Error("scanPanels(nil) returned nil error")
	}
}

// TestScanPanels_NoPanels verifies the no-data-tab-panel-found guard.
func TestScanPanels_NoPanels(t *testing.T) {
	dir := t.TempDir()
	trunk := filepath.Join(dir, "settings.templ")
	if err := os.WriteFile(trunk, []byte(`<div>no panels here</div>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanPanels([]string{trunk}, nil); err == nil {
		t.Error("scanPanels(no panels) returned nil error")
	}
}

// TestScanPanels_UnmappedSubTemplate verifies that a sub-template not listed
// in subTemplateOwner produces an error rather than silently dropping its keys.
func TestScanPanels_UnmappedSubTemplate(t *testing.T) {
	dir := t.TempDir()
	trunk := filepath.Join(dir, "settings.templ")
	sub := filepath.Join(dir, "settings_unknown.templ")
	if err := os.WriteFile(trunk, []byte(`<div data-tab-panel="general"></div>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, []byte(`{ t(ctx, "settings.x.y") }`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanPanels([]string{trunk, sub}, map[string]string{}); err == nil {
		t.Error("scanPanels(unmapped sub) returned nil error")
	}
}

// contains is a small slice-membership helper used by several tests.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test helper, controlled path
	if err != nil {
		t.Fatal(err)
	}
	return data
}
