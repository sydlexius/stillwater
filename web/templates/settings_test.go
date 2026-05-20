package templates

import (
	"bytes"
	"strings"
	"testing"
)

// TestMetadataLanguagesJSON_PreservesEmpty locks in the invariant the
// Providers tab JS depends on: when the user has no stored preference (or
// has explicitly cleared it via the Clear UI), the template renders
// `data-languages="[]"` rather than coercing back to the default `["en"]`.
// Coercion would silently re-render an English pill after a reset and
// contradict what the user just did. See issue #1138.
func TestMetadataLanguagesJSON_PreservesEmpty(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  string
	}{
		{"nil slice preserves empty", nil, `[]`},
		{"empty slice preserves empty", []string{}, `[]`},
		{"single language serializes normally", []string{"en"}, `["en"]`},
		{"multiple languages preserve order", []string{"en-US", "en-GB", "en"}, `["en-US","en-GB","en"]`},
		{"non-Latin tag marshals unchanged", []string{"ja"}, `["ja"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := SettingsData{MetadataLanguages: tt.input}
			got := d.metadataLanguagesJSON()
			if got != tt.want {
				t.Errorf("metadataLanguagesJSON() with %v = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestSettingsUpdatesTab_RestartRequiredVisible asserts that when the
// server has marked the binary apply complete, the Updates tab renders
// the persistent "restart to finish" banner and the Apply button is
// disabled. This is the post-Apply UI contract for issue #1169: without
// these markers the user has no signal that Apply succeeded and that a
// restart is the only remaining step.
func TestSettingsUpdatesTab_RestartRequiredVisible(t *testing.T) {
	data := UpdatesTabData{
		CurrentVersion:  "v0.9.0",
		Channel:         "stable",
		LatestVersion:   "v0.9.6",
		UpdateAvailable: true,
		RestartRequired: true,
		PendingVersion:  "v0.9.5",
	}

	var buf bytes.Buffer
	if err := settingsUpdatesTab(data, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	// The restart-required banner DOM is always present (so the JS can
	// later flip its visibility), but when RestartRequired is true the
	// `hidden` class must NOT be applied. Asserting the absence of
	// `class="...hidden..."` on the banner element is the most direct
	// way to lock the visibility behavior.
	bannerTag := findOpeningTagByID(t, html, "updates-restart-required-row")
	if strings.Contains(bannerTag, "hidden") {
		t.Errorf("banner is hidden when RestartRequired is true; tag = %q", bannerTag)
	}

	// The pending version tag must appear so the user knows which release
	// will load on restart.
	if !strings.Contains(html, "v0.9.5") {
		t.Error("pending version v0.9.5 missing from rendered banner")
	}

	// The Apply button must be disabled even though UpdateAvailable=true,
	// because applying again would overwrite the staged binary.
	applyIdx := strings.Index(html, `id="updates-apply-btn"`)
	if applyIdx == -1 {
		t.Fatal("missing Apply button in rendered HTML")
	}
	applyTagEnd := strings.Index(html[applyIdx:], ">") + applyIdx
	applyTag := html[applyIdx : applyTagEnd+1]
	if !strings.Contains(applyTag, "disabled") {
		t.Errorf("Apply button not disabled when RestartRequired is true; tag = %q", applyTag)
	}
}

// TestSettingsUpdatesTab_RestartRequiredHidden asserts the banner stays
// hidden in the normal pre-Apply state. This is the negative complement
// to the visible test above: a regression that always-renders the banner
// would silently confuse users into restarting before any apply ran.
func TestSettingsUpdatesTab_RestartRequiredHidden(t *testing.T) {
	data := UpdatesTabData{
		CurrentVersion: "v0.9.0",
		Channel:        "stable",
	}

	var buf bytes.Buffer
	if err := settingsUpdatesTab(data, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	bannerTag := findOpeningTagByID(t, html, "updates-restart-required-row")
	if !strings.Contains(bannerTag, "hidden") {
		t.Errorf("banner not hidden in default state; tag = %q", bannerTag)
	}
}

// TestSettingsUpdatesTab_RestartRequiredDocker asserts the post-Apply
// banner renders the Docker-flavored restart instruction (recreate the
// container) instead of the native one (re-run the binary). Without this
// branch the banner left Docker users without an actionable next step.
func TestSettingsUpdatesTab_RestartRequiredDocker(t *testing.T) {
	data := UpdatesTabData{
		CurrentVersion:  "v0.9.0",
		Channel:         "stable",
		LatestVersion:   "v0.9.5",
		UpdateAvailable: true,
		RestartRequired: true,
		PendingVersion:  "v0.9.5",
		IsDocker:        true,
	}

	var buf bytes.Buffer
	if err := settingsUpdatesTab(data, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	// Pin both branches by inspecting the instruction div directly.
	// The hidden `updates-i18n` element at the top of the tab carries the
	// native string as a `data-*` attribute regardless of IsDocker, so a
	// global "native text absent" assertion would be over-strict; check
	// the instruction div's body instead.
	instructionStart := strings.Index(html, `id="updates-restart-required-instruction"`)
	if instructionStart == -1 {
		t.Fatal("missing restart-required-instruction element in rendered HTML")
	}
	tagEnd := strings.Index(html[instructionStart:], ">")
	if tagEnd == -1 {
		t.Fatal("malformed instruction tag")
	}
	closeIdx := strings.Index(html[instructionStart+tagEnd:], "</div>")
	if closeIdx == -1 {
		t.Fatal("missing closing tag for instruction div")
	}
	body := html[instructionStart+tagEnd+1 : instructionStart+tagEnd+closeIdx]
	if !strings.Contains(body, "Recreate the container") {
		t.Errorf("Docker restart instruction body missing 'Recreate the container'; got %q", body)
	}
	if strings.Contains(body, "Stop and re-run the Stillwater binary") {
		t.Errorf("native restart instruction leaked into Docker instruction body; got %q", body)
	}
}

// TestSettingsPage_TLSStatusCard pins the read-only TLS Status card
// rendered on the Settings General tab. The card is the only place an
// operator can confirm without log-tailing whether direct TLS took effect,
// so the rendered branches must stay covered by tests as the M47 work
// adds ACME and HTTP/3 wiring downstream.
func TestSettingsPage_TLSStatusCard(t *testing.T) {
	cases := []struct {
		name     string
		tls      TLSStatusData
		wantText []string
		denyText []string
		wantMode string
	}{
		{
			name: "off shows plain HTTP listener",
			tls: TLSStatusData{
				Mode:     "off",
				HTTPPort: 1973,
			},
			wantText: []string{"Inactive", "HTTP on :1973"},
			denyText: []string{"Active (BYO certificate)", "HTTPS on :"},
			wantMode: "off",
		},
		{
			name: "byo shows HTTPS listener on TLS port",
			tls: TLSStatusData{
				Mode:      "byo",
				HTTPSPort: 443,
			},
			wantText: []string{"Active (BYO certificate)", "HTTPS on :443"},
			// Anchor "ACME" to the status pill marker (data-tls-mode="acme")
			// rather than the bare substring; the latter started matching
			// the help-popover prose, which mentions ACME as one of the
			// configurable modes regardless of which mode is active.
			denyText: []string{"HTTP on :", "HTTP redirect on :", `data-tls-mode="acme"`, "Active (ACME"},
			wantMode: "byo",
		},
		{
			name: "byo collapse mode shows Server.Port as HTTPS",
			tls: TLSStatusData{
				Mode:      "byo",
				HTTPSPort: 1973,
			},
			wantText: []string{"Active (BYO certificate)", "HTTPS on :1973"},
			denyText: []string{"HTTP on :", "HTTP redirect on :"},
			wantMode: "byo",
		},
		{
			name: "redirect row absent when HTTPRedirectPort is zero",
			tls: TLSStatusData{
				Mode:             "byo",
				HTTPSPort:        443,
				HTTPRedirectPort: 0,
			},
			wantText: []string{"Active (BYO certificate)", "HTTPS on :443"},
			denyText: []string{"HTTP redirect on :", `data-tls-listener="redirect"`},
			wantMode: "byo",
		},
		{
			name: "acme with domain shows the issued name",
			tls: TLSStatusData{
				Mode:       "acme",
				AcmeDomain: "stillwater.example.com",
				HTTPSPort:  443,
			},
			wantText: []string{"Active (ACME, stillwater.example.com)", "HTTPS on :443"},
			denyText: []string{"BYO certificate", "Inactive"},
			wantMode: "acme",
		},
		{
			name: "redirect listener row renders only when configured",
			tls: TLSStatusData{
				Mode:             "byo",
				HTTPSPort:        443,
				HTTPRedirectPort: 80,
			},
			wantText: []string{"Active (BYO certificate)", "HTTPS on :443", "HTTP redirect on :80"},
			wantMode: "byo",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := SettingsData{
				ActiveTab: "general",
				TLS:       tc.tls,
				// Defaults that the rest of the General tab depends on so
				// the render path does not panic on nil data.
				BasePath: "/",
			}
			var buf bytes.Buffer
			if err := SettingsPage(AssetPaths{}, data).Render(testCtx(t), &buf); err != nil {
				t.Fatalf("render: %v", err)
			}
			html := buf.String()

			if !strings.Contains(html, `id="tls-status-card"`) {
				t.Fatal("rendered HTML missing tls-status-card element")
			}
			if !strings.Contains(html, `data-tls-mode="`+tc.wantMode+`"`) {
				t.Errorf("missing data-tls-mode=%q marker", tc.wantMode)
			}
			for _, want := range tc.wantText {
				if !strings.Contains(html, want) {
					t.Errorf("rendered HTML missing %q", want)
				}
			}
			for _, deny := range tc.denyText {
				if strings.Contains(html, deny) {
					t.Errorf("rendered HTML unexpectedly contains %q", deny)
				}
			}
		})
	}
}

// findOpeningTagByID returns the rendered opening tag (everything between
// `<` and `>` inclusive) of the element whose `id` attribute matches `id`.
// Used by banner visibility tests to verify class attributes without coupling
// to the full element body.
func findOpeningTagByID(t *testing.T, html, id string) string {
	t.Helper()
	idx := strings.Index(html, `id="`+id+`"`)
	if idx == -1 {
		t.Fatalf("missing element id=%q in rendered HTML", id)
	}
	openTagStart := strings.LastIndex(html[:idx], "<")
	openTagEnd := strings.Index(html[idx:], ">") + idx
	if openTagStart < 0 || openTagEnd <= openTagStart {
		t.Fatalf("malformed markup around id=%q", id)
	}
	return html[openTagStart : openTagEnd+1]
}

// TestSettingsLibrariesTab_JSDataAttributesResolved guards against the
// failure mode of #1302: i18n calls leaking inside <script> blocks as
// literal `{ t(ctx, "...") }` text. The Libraries tab's JS reads four
// data-* attributes on #settings-library-list, and the script must see
// resolved strings rather than template syntax. The substring check on the
// full rendered HTML catches the same class of bug anywhere in SettingsPage,
// not just the four keys patched here.
func TestSettingsLibrariesTab_JSDataAttributesResolved(t *testing.T) {
	data := SettingsData{
		ActiveTab: "libraries",
		BasePath:  "/",
	}
	var buf bytes.Buffer
	if err := SettingsPage(AssetPaths{}, data).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	// Class-level regression guard for #1302: scan for literal `{ t(ctx,`
	// anywhere in the rendered HTML. JS-comment lines (//-prefixed) are
	// skipped because they legitimately discuss the bug pattern in inline
	// developer documentation. Block-comment /* ... */ false positives are
	// not handled; this codebase uses //-comments inside <script>.
	for _, raw := range strings.Split(html, "\n") {
		if strings.HasPrefix(strings.TrimSpace(raw), "//") {
			continue
		}
		if strings.Contains(raw, `{ t(ctx,`) {
			t.Errorf("rendered HTML contains literal `{ t(ctx,` outside a JS comment: %q (regression of the class of bug fixed in #1302)", strings.TrimSpace(raw))
		}
	}

	tag := findOpeningTagByID(t, html, "settings-library-list")
	for _, attr := range []string{
		"data-fs-mode-title",
		"data-poll-interval-title",
		"data-resync",
		"data-scan",
	} {
		needle := attr + `="`
		idx := strings.Index(tag, needle)
		if idx == -1 {
			t.Errorf("missing %s on #settings-library-list opening tag", attr)
			continue
		}
		val := tag[idx+len(needle):]
		end := strings.IndexByte(val, '"')
		if end <= 0 {
			t.Errorf("%s rendered with empty value", attr)
		}
	}
}
