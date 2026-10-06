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
