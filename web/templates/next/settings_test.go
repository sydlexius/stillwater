package next

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/components"
	"github.com/sydlexius/stillwater/web/templates"
)

// settingsTestData is a SettingsData populated just enough that every section
// renders real content (several sections guard on non-empty data). It also has
// known slice lengths so the count-badge test can assert exact numbers:
// ProviderKeys=1, Rules=1; Libraries/Connections/Webhooks/APITokens/Users empty.
var settingsTestData = templates.SettingsData{
	ActiveProfile: &platform.Profile{Name: "Test Profile"},
	ProviderKeys: []provider.ProviderKeyStatus{
		{Name: "fanart", DisplayName: "Fanart.tv", RequiresKey: true, HasKey: true, Status: "ok", AccessTier: provider.TierFreeKey},
	},
	WebSearchProviders: []provider.WebSearchProviderStatus{
		{Name: "duckduckgo", DisplayName: "DuckDuckGo", Enabled: true},
	},
	Priorities: []provider.FieldPriority{
		{Field: "genres", Providers: []provider.ProviderName{"fanart"}},
	},
	Rules: []rule.Rule{
		{ID: "nfo-present", Name: "NFO present", Category: rule.RuleCategoryNFO, Enabled: true, AutomationMode: "manual"},
	},
}

// expectedSectionIDs is the canonical taxonomy (prototype settingsGroups, jsx
// 13-53) MINUS logs (#1338) and per-user preferences (#1774).
func expectedSectionIDs() []string {
	return []string{
		"general", "libraries", "platform",
		"providers", "languages", "rules", "schedule",
		"connections", "webhooks", "tokens",
		"users", "auth", "config-file", "maintenance", "updates",
	}
}

func settingsTestCtx(tb testing.TB) context.Context {
	tb.Helper()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		tb.Fatalf("loading i18n bundle: %v", err)
	}
	return i18n.WithTranslator(context.Background(), bundle.Translator("en"))
}

func renderSettingsPaneWith(tb testing.TB, data templates.SettingsData) *html.Node {
	tb.Helper()
	var buf bytes.Buffer
	if err := settingsPane(data, templates.AssetPaths{}).Render(settingsTestCtx(tb), &buf); err != nil {
		tb.Fatalf("rendering settingsPane: %v", err)
	}
	root, err := html.Parse(strings.NewReader(buf.String()))
	if err != nil {
		tb.Fatalf("parsing pane HTML: %v", err)
	}
	return root
}

func renderSettingsPane(tb testing.TB) *html.Node {
	tb.Helper()
	return renderSettingsPaneWith(tb, settingsTestData)
}

// renderSettingsPaneLeveled renders the pane the way the handler does -- with
// the card-title heading base level threaded through context (#1339 A1).
func renderSettingsPaneLeveled(tb testing.TB) *html.Node {
	tb.Helper()
	var buf bytes.Buffer
	ctx := components.WithHeadingLevel(settingsTestCtx(tb), 3)
	if err := settingsPane(settingsTestData, templates.AssetPaths{}).Render(ctx, &buf); err != nil {
		tb.Fatalf("rendering leveled settingsPane: %v", err)
	}
	root, err := html.Parse(strings.NewReader(buf.String()))
	if err != nil {
		tb.Fatalf("parsing pane HTML: %v", err)
	}
	return root
}

func renderRail(tb testing.TB, data templates.SettingsData) (string, *html.Node) {
	tb.Helper()
	var buf bytes.Buffer
	if err := settingsRail(data).Render(settingsTestCtx(tb), &buf); err != nil {
		tb.Fatalf("rendering settingsRail: %v", err)
	}
	root, err := html.Parse(strings.NewReader(buf.String()))
	if err != nil {
		tb.Fatalf("parsing rail HTML: %v", err)
	}
	return buf.String(), root
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, name string) bool {
	for _, a := range n.Attr {
		if a.Key == name {
			return true
		}
	}
	return false
}

// walk visits every element node in the subtree.
func walk(n *html.Node, fn func(*html.Node)) {
	if n.Type == html.ElementNode {
		fn(n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

// findSections returns every <section data-rail-section> keyed by section id.
func findSections(n *html.Node, out map[string]*html.Node) {
	walk(n, func(e *html.Node) {
		if e.Data == "section" {
			if id := attr(e, "data-rail-section"); id != "" {
				out[id] = e
			}
		}
	})
}

func firstHeadingTag(n *html.Node) string {
	res := ""
	var rec func(*html.Node) bool
	rec = func(e *html.Node) bool {
		if e.Type == html.ElementNode {
			switch e.Data {
			case "h1", "h2", "h3", "h4", "h5", "h6":
				res = e.Data
				return true
			}
		}
		for c := e.FirstChild; c != nil; c = c.NextSibling {
			if rec(c) {
				return true
			}
		}
		return false
	}
	rec(n)
	return res
}

func anyHeadingTag(n *html.Node, tag string) bool {
	found := false
	walk(n, func(e *html.Node) {
		if e.Data == tag {
			found = true
		}
	})
	return found
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSettingsTaxonomyMatchesSpec enforces the canonical taxonomy: the rail item
// set and the pane section set are EXACTLY the 15 spec sections (jsx minus logs
// minus per-user-prefs), in nothing-extra/nothing-missing form, and "logs" never
// appears (#1338 moved it out).
func TestSettingsTaxonomyMatchesSpec(t *testing.T) {
	want := map[string]bool{}
	for _, id := range expectedSectionIDs() {
		want[id] = true
	}

	_, railRoot := renderRail(t, settingsTestData)
	railIDs := map[string]bool{}
	walk(railRoot, func(e *html.Node) {
		if id := attr(e, "data-rail-link"); id != "" {
			railIDs[id] = true
		}
	})

	paneRoot := renderSettingsPane(t)
	paneNodes := map[string]*html.Node{}
	findSections(paneRoot, paneNodes)
	paneIDs := map[string]bool{}
	for id := range paneNodes {
		paneIDs[id] = true
	}

	if got := sortedKeys(railIDs); !equalStringSets(want, railIDs) {
		t.Errorf("rail item ids = %v, want %v", got, expectedSectionIDs())
	}
	if !equalStringSets(want, paneIDs) {
		t.Errorf("pane section ids = %v, want %v", sortedKeys(paneIDs), expectedSectionIDs())
	}
	if railIDs["logs"] || paneIDs["logs"] {
		t.Error("`logs` must NOT be in settings (moved to /next/logs, #1338)")
	}
	if len(railIDs) != 15 {
		t.Errorf("rail item count = %d, want 15", len(railIDs))
	}
}

func equalStringSets(want map[string]bool, got map[string]bool) bool {
	if len(want) != len(got) {
		return false
	}
	for k := range want {
		if !got[k] {
			return false
		}
	}
	return true
}

// TestSettingsKeywordIndex enforces the spec test "every section has at least 4
// keywords" -- the keyword index is what makes the filter useful.
func TestSettingsKeywordIndex(t *testing.T) {
	for _, g := range nextSettingsGroups() {
		for _, s := range g.Sections {
			if len(s.Keywords) < 4 {
				t.Errorf("section %q has %d keywords, want >= 4", s.ID, len(s.Keywords))
			}
		}
	}
}

// TestSettingsRailChrome checks the rail chrome: the filter input with the spec
// placeholder, the four groups, item keyword data, and the empty-state scaffold.
func TestSettingsRailChrome(t *testing.T) {
	out, root := renderRail(t, settingsTestData)

	if !strings.Contains(out, `id="settings-search-input"`) {
		t.Error("rail missing the keyword filter input")
	}
	if !strings.Contains(out, "Filter settings") {
		t.Errorf("rail filter placeholder not the spec copy %q; got:\n%s", "Filter settings…", out[:min(len(out), 600)])
	}
	for _, g := range []string{"essentials", "data", "integrations", "system"} {
		if !strings.Contains(out, `data-rail-group="`+g+`"`) {
			t.Errorf("rail missing group %q", g)
		}
	}
	// Every item carries a non-empty keyword index for the filter.
	walk(root, func(e *html.Node) {
		if id := attr(e, "data-rail-link"); id != "" {
			if strings.TrimSpace(attr(e, "data-keywords")) == "" {
				t.Errorf("rail item %q has no data-keywords", id)
			}
		}
	})
	if !strings.Contains(out, "data-rail-empty") || !strings.Contains(out, "data-empty-template") {
		t.Error("rail missing the empty-state scaffold (data-rail-empty / data-empty-template)")
	}
}

// TestSettingsCountBadges checks count badges reflect REAL data: with one
// enabled provider + one enabled rule in the fixture, those items show "1";
// sections whose data is empty (libraries) show no badge (never a faked number).
func TestSettingsCountBadges(t *testing.T) {
	_, root := renderRail(t, settingsTestData)

	if c, ok := countBadgeFor(root, "providers"); !ok || c != "1" {
		t.Errorf("providers badge = %q (present=%v), want \"1\"", c, ok)
	}
	if c, ok := countBadgeFor(root, "rules"); !ok || c != "1" {
		t.Errorf("rules badge = %q (present=%v), want \"1\"", c, ok)
	}
	if _, ok := countBadgeFor(root, "libraries"); ok {
		t.Error("libraries has 0 in the fixture; it must show NO count badge (never fake data)")
	}
}

func textOf(n *html.Node) string {
	var sb strings.Builder
	var rec func(*html.Node)
	rec = func(e *html.Node) {
		if e.Type == html.TextNode {
			sb.WriteString(e.Data)
		}
		for c := e.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return sb.String()
}

// TestSettingsPaneAllSectionsVisible enforces the all-scrollable model: every
// pane section renders server-side AND is visible (no single-section hide, no
// default-section attribute) so the user scrolls through them and the rail
// scroll-spies the one in view. Each section keeps its #section-<id> anchor.
func TestSettingsPaneAllSectionsVisible(t *testing.T) {
	var buf bytes.Buffer
	if err := settingsPane(settingsTestData, templates.AssetPaths{}).Render(settingsTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "data-default-section") {
		t.Error("pane still carries data-default-section; the all-scrollable model has no single default")
	}

	root, err := html.Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("parsing pane HTML: %v", err)
	}
	sections := map[string]*html.Node{}
	findSections(root, sections)
	if len(sections) == 0 {
		t.Fatal("no sections rendered")
	}
	for id, n := range sections {
		if hasAttr(n, "hidden") {
			t.Errorf("section %q is attribute-hidden; the all-scrollable pane must render every section visible", id)
		}
		if got := attr(n, "id"); got != "section-"+id {
			t.Errorf("section %q has id=%q, want %q (the scroll-spy / deep-link anchor)", id, got, "section-"+id)
		}
	}
}

// TestSettingsPaneGroupHeadings (#1339 A1): the pane opens each group with an
// <h2 class="sw-next-settings-group-heading"> divider (Essentials / Data /
// Integrations / System). This is the h2 tier between the page <h1> and the card
// <h3> titles, so the heading order never skips a level -- and the pane's first
// heading is that <h2>, not an <h3>.
func TestSettingsPaneGroupHeadings(t *testing.T) {
	root := renderSettingsPaneLeveled(t)

	gotGroups := map[string]bool{}
	walk(root, func(e *html.Node) {
		if e.Data != "h2" {
			return
		}
		for _, cl := range strings.Fields(attr(e, "class")) {
			if cl == "sw-next-settings-group-heading" {
				if g := attr(e, "data-rail-group"); g != "" {
					gotGroups[g] = true
				}
			}
		}
	})
	for _, want := range []string{"essentials", "data", "integrations", "system"} {
		if !gotGroups[want] {
			t.Errorf("pane missing the <h2> group divider for %q", want)
		}
	}
	if len(gotGroups) != 4 {
		t.Errorf("pane has %d group <h2> dividers, want 4: %v", len(gotGroups), sortedKeys(gotGroups))
	}

	// The first heading encountered in the pane must be an <h2> (a group divider),
	// establishing the h2 tier ahead of the section card <h3>s (no h1->h3 skip).
	if got := firstHeadingTag(root); got != "h2" {
		t.Errorf("pane first heading = <%s>, want <h2> (the group divider tier)", got)
	}
}

// TestSettingsSectionsAreNamedLandmarks (#1339 A2): every section is a NAMED
// region landmark via aria-label.
func TestSettingsSectionsAreNamedLandmarks(t *testing.T) {
	root := renderSettingsPane(t)
	sections := map[string]*html.Node{}
	findSections(root, sections)
	if len(sections) == 0 {
		t.Fatal("no sections rendered")
	}
	for id, node := range sections {
		if strings.TrimSpace(attr(node, "aria-label")) == "" {
			t.Errorf("section %q has no aria-label; not a named landmark (A2)", id)
		}
	}
}

// TestSettingsHeadingHierarchy (#1339 A1): on the next/ page the card titles
// nest at <h3> under the pane's <h2> group dividers (see
// TestSettingsPaneGroupHeadings), so no section contains an <h2> and each
// section's first heading is an <h3> -- the h1->h2->h3 chain never skips.
func TestSettingsHeadingHierarchy(t *testing.T) {
	root := renderSettingsPaneLeveled(t)
	sections := map[string]*html.Node{}
	findSections(root, sections)
	if len(sections) == 0 {
		t.Fatal("no sections rendered")
	}
	for id, node := range sections {
		if anyHeadingTag(node, "h2") {
			t.Errorf("section %q contains an <h2> card title; should be <h3> (A1)", id)
		}
		if got := firstHeadingTag(node); got != "" && got != "h3" {
			t.Errorf("section %q first heading = <%s>, want <h3> (A1)", id, got)
		}
	}
}

// TestSettingsHydrationTabPanels: the two lazy-hydrating modules' host sections
// carry the data-tab-panel their JS keys off (#1339): Maintenance (Image Cache)
// -> "general", Updates -> "updates"; no other section carries it.
func TestSettingsHydrationTabPanels(t *testing.T) {
	root := renderSettingsPane(t)
	sections := map[string]*html.Node{}
	findSections(root, sections)

	want := map[string]string{"maintenance": "general", "updates": "updates"}
	for id, node := range sections {
		got := attr(node, "data-tab-panel")
		if exp, ok := want[id]; ok {
			if got != exp {
				t.Errorf("section %q data-tab-panel = %q, want %q", id, got, exp)
			}
		} else if got != "" {
			t.Errorf("section %q unexpectedly carries data-tab-panel=%q", id, got)
		}
	}
	for id := range want {
		if _, ok := sections[id]; !ok {
			t.Errorf("expected section %q to render", id)
		}
	}
}

// TestSettingsBehaviorReHomesSymlink (#1339 B-KEEP): the symlink default toggle
// now lives in the Music Libraries section (moved from the removed Behavior card).
// Guards the re-home: symlink-toggle must be in "libraries", not "general" or
// "platform".
func TestSettingsBehaviorReHomesSymlink(t *testing.T) {
	data := settingsTestData
	data.ActiveProfile = &platform.Profile{ID: "custom", Name: "Custom", UseSymlinks: false}
	data.SymlinkSupported = false

	root := renderSettingsPaneWith(t, data)
	sections := map[string]*html.Node{}
	findSections(root, sections)

	librariesSec := sections["libraries"]
	generalSec := sections["general"]
	platformSec := sections["platform"]
	if librariesSec == nil || generalSec == nil || platformSec == nil {
		t.Fatal("libraries, general, and platform sections must render")
	}
	if !strings.Contains(renderNode(librariesSec), `id="symlink-toggle"`) {
		t.Error("symlink-toggle should live in the libraries section")
	}
	if strings.Contains(renderNode(generalSec), `id="symlink-toggle"`) {
		t.Error("symlink-toggle must NOT be in the general section (Behavior card removed)")
	}
	if strings.Contains(renderNode(platformSec), `id="symlink-toggle"`) {
		t.Error("symlink-toggle must NOT be in the platform section")
	}
}

// countBadgeFor returns the text of the sw-next-rail-count span for a rail item,
// and whether one was found. Extracted from TestSettingsCountBadges for reuse.
func countBadgeFor(root *html.Node, id string) (string, bool) {
	var found string
	var ok bool
	walk(root, func(e *html.Node) {
		if attr(e, "data-rail-link") != id {
			return
		}
		walk(e, func(c *html.Node) {
			for _, cl := range strings.Fields(attr(c, "class")) {
				if cl == "sw-next-rail-count" {
					found = strings.TrimSpace(textOf(c))
					ok = true
				}
			}
		})
	})
	return found, ok
}

// TestSettingsCountBadgesEnabledOnly verifies that count badges reflect only
// ENABLED items: unconfigured providers and disabled rules are excluded.
func TestSettingsCountBadgesEnabledOnly(t *testing.T) {
	data := templates.SettingsData{
		ActiveProfile: &platform.Profile{Name: "Test"},
		ProviderKeys: []provider.ProviderKeyStatus{
			{Name: "fanart", Status: "ok"},            // enabled: has valid key
			{Name: "discogs", Status: "unconfigured"}, // disabled: no key
		},
		Rules: []rule.Rule{
			{ID: "nfo-present", Name: "NFO present", Category: rule.RuleCategoryNFO, Enabled: true, AutomationMode: "manual"},
			{ID: "nfo-mbid", Name: "NFO MusicBrainz ID", Category: rule.RuleCategoryNFO, Enabled: false, AutomationMode: "manual"},
		},
	}
	_, root := renderRail(t, data)

	if c, ok := countBadgeFor(root, "providers"); !ok || c != "1" {
		t.Errorf("providers badge (1 enabled + 1 unconfigured) = %q (present=%v), want \"1\"", c, ok)
	}
	if c, ok := countBadgeFor(root, "rules"); !ok || c != "1" {
		t.Errorf("rules badge (1 enabled + 1 disabled) = %q (present=%v), want \"1\"", c, ok)
	}
}

func renderNode(n *html.Node) string {
	var sb strings.Builder
	_ = html.Render(&sb, n)
	return sb.String()
}
