package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/sydlexius/stillwater/internal/i18n"
)

func renderLayoutFor(t *testing.T, isAdmin bool) string {
	t.Helper()
	var buf bytes.Buffer
	child := templ.Raw("<p>page</p>")
	ctx := templ.WithChildren(testCtx(t), child)
	if err := Layout("t", AssetPaths{IsAdmin: isAdmin}).Render(ctx, &buf); err != nil {
		t.Fatalf("render layout: %v", err)
	}
	return buf.String()
}

// The mount is admin-only on the template side; the endpoint is admin-gated
// independently. A non-admin page must not carry the banner element or script.
func TestLayout_RegistryRepairBannerIsAdminOnly(t *testing.T) {
	if html := renderLayoutFor(t, true); !strings.Contains(html, `id="sw-registry-repair-banner"`) {
		t.Error("admin layout is missing the registry repair banner mount")
	}
	html := renderLayoutFor(t, false)
	if strings.Contains(html, "sw-registry-repair-banner") || strings.Contains(html, "registry-repair/banner") {
		t.Error("non-admin layout carries the registry repair banner")
	}
}

// The banner starts hidden, links to the docs, offers a dismissal, and (slice 2)
// carries the run control and the endpoints it drives.
func TestRegistryRepairBanner_RunControlAndHiddenByDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := RegistryRepairBanner().Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()
	for _, want := range []string{
		`class="hidden `, `id="sw-registry-repair-live" lang="en" role="status" class="sr-only"`, `how-to/repair-image-registry/#background-check`,
		`id="sw-registry-repair-dismiss"`, `data-count-other="{count} registry rows`,
		`id="sw-registry-repair-run"`, `>Repair now</button>`, `data-toast-failed="Image registry repair failed. See the server log for details."`,
		`/api/v1/reports/registry-repair/remediate`, `/api/v1/reports/registry-repair/status`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("banner markup missing %q", want)
		}
	}
	if n := strings.Count(html, "<button"); n != 2 {
		t.Errorf("banner renders %d buttons, want run + dismiss", n)
	}
}

func renderBannerFor(t *testing.T, locale string) string {
	t.Helper()
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatalf("loading i18n bundle: %v", err)
	}
	ctx := i18n.WithTranslator(context.Background(), bundle.Translator(locale))
	var buf bytes.Buffer
	if err := RegistryRepairBanner().Render(ctx, &buf); err != nil {
		t.Fatalf("render %s: %v", locale, err)
	}
	return buf.String()
}

// bannerKeys lists every banner.registry_repair.* key plus common.learn_more
// straight from en.json, so a key added later is covered without a test edit.
func bannerKeys(t *testing.T, en map[string]string) []string {
	t.Helper()
	var keys []string
	for k := range en {
		if strings.HasPrefix(k, "banner.registry_repair.") || k == "common.learn_more" {
			keys = append(keys, k)
		}
	}
	return keys
}

var placeholderRe = regexp.MustCompile(`\{\w+\}`)

// placeholders returns the sorted, de-duplicated {name} tokens of s as one string.
func placeholders(s string) string {
	seen := map[string]bool{}
	var out []string
	for _, m := range placeholderRe.FindAllString(s, -1) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// #2678: fr and ja must carry their own value for every banner key (no silent
// English fallback), the banner and live region must declare the page language,
// and the JS placeholders must survive translation. en stays English, with the
// singular form of the incomplete-write toast fixed.
func TestRegistryRepairBanner_Localized(t *testing.T) {
	raw, err := os.ReadFile("../../internal/i18n/locales/en.json")
	if err != nil {
		t.Fatal(err)
	}
	en := map[string]string{}
	if err := json.Unmarshal(raw, &en); err != nil {
		t.Fatal(err)
	}
	keys := bannerKeys(t, en)
	if len(keys) < 16 { // precondition: 15 banner keys + common.learn_more, so the loops below cannot pass vacuously
		t.Fatalf("found only %d banner keys in en.json, want >= 16", len(keys))
	}
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	enTr := bundle.Translator("en")
	for _, loc := range []string{"fr", "ja"} {
		tr := bundle.Translator(loc)
		for _, k := range keys {
			if tr.T(k) == enTr.T(k) {
				t.Errorf("%s: key %q falls back to (or equals) the English value", loc, k)
			}
		}
		html := renderBannerFor(t, loc)
		if n := strings.Count(html, `lang="`+loc+`"`); n != 2 {
			t.Errorf("%s: want lang=%q on banner and live region (2), got %d", loc, loc, n)
		}
		for _, k := range keys {
			if got, want := placeholders(tr.T(k)), placeholders(en[k]); got != want {
				t.Errorf("%s: key %q placeholders %s, want %s (as en)", loc, k, got, want)
			}
		}
		for _, bad := range []string{"may need repair", "Repair now", "Last checked", "Learn more"} {
			if strings.Contains(html, bad) {
				t.Errorf("%s: rendered banner still contains English %q", loc, bad)
			}
		}
	}
	html := renderBannerFor(t, "en")
	if strings.Count(html, `lang="en"`) != 2 {
		t.Error("en: want lang=en on banner and live region")
	}
	for _, want := range []string{`>Repair now</button>`, `1 change could not be saved`, `{failures} changes could not be saved`, `data-toast-incomplete-one=`} {
		if !strings.Contains(html, want) {
			t.Errorf("en banner missing %q", want)
		}
	}
	if strings.Contains(html, "1 changes") {
		t.Error("en banner still has the broken singular")
	}
}

// The last-checked date locale must fall back to navigator.language when
// navigator.languages is absent or empty (same order as the language picker),
// so a regional format such as en-GB survives.
func TestRegistryRepairBanner_DateLocaleFallsBackToNavigatorLanguage(t *testing.T) {
	html := renderBannerFor(t, "en")
	if !strings.Contains(html, "navigator.language ?") {
		t.Error("dateLocale does not fall back to navigator.language when navigator.languages is empty")
	}
}

// #2678 slice 4: the dialog's cached-plan text ships in every locale as
// data-confirm-plan, with each of the three placeholders the script fills
// exactly once, next to the generic data-confirm fallback.
func TestRegistryRepairBanner_ConfirmPlanAttribute(t *testing.T) {
	attrRe := regexp.MustCompile(`data-confirm-plan="([^"]*)"`)
	for _, loc := range []string{"en", "fr", "ja"} {
		html := renderBannerFor(t, loc)
		m := attrRe.FindStringSubmatch(html)
		if m == nil {
			t.Fatalf("%s: banner has no data-confirm-plan attribute", loc)
		}
		for _, ph := range []string{"{age}", "{rebuilt}", "{restored}"} {
			if n := strings.Count(m[1], ph); n != 1 {
				t.Errorf("%s: data-confirm-plan has %d of %s, want exactly 1", loc, n, ph)
			}
		}
		if !strings.Contains(html, `data-confirm="`) {
			t.Errorf("%s: the generic data-confirm fallback is gone", loc)
		}
		if loc != "en" && m[1] == attrRe.FindStringSubmatch(renderBannerFor(t, "en"))[1] {
			t.Errorf("%s: data-confirm-plan equals the English text", loc)
		}
	}
}

// The registry banner must look like the ConflictBanner warn bar (#2678): both
// carry the same gradient, blur, border and padding tokens. This proves class
// parity only, not rendering (the browser spec pins the computed style).
func TestRegistryRepairBanner_SharesConflictBannerWarnTokens(t *testing.T) {
	shared := []string{
		"bg-gradient-to-r", "from-amber-900/60", "to-yellow-700/50",
		"dark:from-amber-900/60", "dark:to-yellow-700/50", "backdrop-blur-md",
		"border-b", "border-amber-300/30", "px-6", "py-3",
	}
	classAttr := regexp.MustCompile(`class="([^"]*bg-gradient-to-r[^"]*)"`)
	check := func(name, html string) {
		t.Helper()
		m := classAttr.FindStringSubmatch(html)
		if m == nil {
			t.Fatalf("%s: no element carries bg-gradient-to-r", name)
		}
		have := map[string]bool{}
		for _, c := range strings.Fields(m[1]) {
			have[c] = true
		}
		for _, want := range shared {
			if !have[want] {
				t.Errorf("%s: gradient bar lacks shared token %q (has %q)", name, want, m[1])
			}
		}
	}
	var buf bytes.Buffer
	if err := ConflictBannerContent(ConflictBannerView{State: "image_only"}).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render conflict banner: %v", err)
	}
	check("conflict banner (warn)", buf.String())
	check("registry repair banner", renderBannerFor(t, "en"))
}

// Every banner read starts with no plan, so a failed or unusable re-read cannot
// leave stale counts in the confirmation. The behavior is proven in the browser.
func TestRegistryRepairBanner_ScriptClearsPlanAtCheckStart(t *testing.T) {
	html := renderBannerFor(t, "en")
	i := strings.Index(html, "function check()")
	if i < 0 {
		t.Fatal("banner script has no check()")
	}
	head := html[i:]
	if len(head) > 300 {
		head = head[:300]
	}
	if !strings.Contains(head, "lastPlan = null") || !strings.Contains(head, "lastChecked = null") {
		t.Errorf("check() does not clear lastPlan and lastChecked before fetching: %s", head)
	}
}

// A plan whose counts are not finite numbers must take the generic confirmation
// (never render "null"), and say so on the console. The behavior is proven in the
// browser (registry-repair-run.spec.js); this pins the guard in the shipped script.
func TestRegistryRepairBanner_ScriptGuardsNonFinitePlan(t *testing.T) {
	html := renderBannerFor(t, "en")
	for _, want := range []string{
		"Number.isFinite(d.plan.rebuild) && Number.isFinite(d.plan.restore)",
		"plan is not a pair of finite numbers",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("banner script missing %q", want)
		}
	}
}
