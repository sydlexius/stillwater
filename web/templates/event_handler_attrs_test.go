package templates

// event_handler_attrs_test.go -- render-level coverage for hx-on* handlers
// built from Go values. templ v0.3.1100 classifies every on* / hx-on* /
// data-hx-on* attribute as an event handler (GHSA-94c2-xg24-pwhv, templ ADR
// 0004): a string value no longer compiles in a literal attribute, and in a
// spread templ.Attributes it silently renders a "failed sanitization" comment
// instead of the handler. These tests pin the rendered handlers so a future
// templ bump, or a helper going back to a plain string, fails here instead of
// shipping a dead button.

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/sydlexius/stillwater/internal/auth"
)

const failedSanitizationMarker = "templ: failed sanitization"

// attrValues returns the parsed (HTML-unescaped) values of every attribute
// named key in doc, in document order.
func attrValues(t *testing.T, doc string, key string) []string {
	t.Helper()
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parsing HTML: %v", err)
	}
	var out []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == key {
					out = append(out, a.Val)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

func TestSectionAPITokens_AfterRequestHandlers(t *testing.T) {
	// The revoked token's ID carries a double quote, a single quote and a '<'
	// so the test proves the ID cannot break out of the JS string literal.
	hostileID := `a"b'c<d`
	data := SettingsData{APITokens: []auth.APIToken{
		{ID: "abc123", Name: "active", Scopes: "read", Status: auth.TokenStatusActive, CreatedAt: "2026-01-01"},
		{ID: hostileID, Name: "revoked", Scopes: "read", Status: auth.TokenStatusRevoked, CreatedAt: "2026-01-01"},
	}}
	var buf bytes.Buffer
	if err := SectionAPITokens(data).Render(settingsTestCtx(t), &buf); err != nil {
		t.Fatalf("rendering SectionAPITokens: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, failedSanitizationMarker) {
		t.Fatalf("token section rendered a failed-sanitization handler:\n%s", out)
	}
	if strings.Contains(out, "<script") {
		t.Errorf("token handlers must not emit a <script> element")
	}

	handlers := attrValues(t, out, "hx-on--after-request")
	if len(handlers) != 2 {
		t.Fatalf("got %d hx-on--after-request handlers, want 2 (revoke + delete): %q", len(handlers), handlers)
	}

	const fail = "{ try { var r=JSON.parse(event.detail.xhr.responseText); " +
		"showToast(r.error||btn.dataset.toastFail); } catch(e) { showToast(btn.dataset.toastFail); } }"
	wantRevoke := "var btn=this; if(event.detail.successful) { " +
		"showSuccessToast(btn.dataset.toastSuccess); " +
		"setTimeout(function(){ swRefreshSettingsSection('tokens'); }, 500); " +
		"} else " + fail
	if handlers[0] != wantRevoke {
		t.Errorf("revoke handler:\n got %q\nwant %q", handlers[0], wantRevoke)
	}

	wantDelete := "var btn=this; if(event.detail.successful) { " +
		// json.Marshal emits '<' as the six-character JS escape backslash-u003c.
		`var row = document.getElementById("token-row-a\"b'c` + `\` + `u003cd"); ` +
		"if(row) { row.style.transition='opacity 0.3s'; row.style.opacity='0'; " +
		"setTimeout(function(){ row.remove(); }, 300); } " +
		"showSuccessToast(btn.dataset.toastSuccess); " +
		"} else " + fail
	if handlers[1] != wantDelete {
		t.Errorf("delete handler:\n got %q\nwant %q", handlers[1], wantDelete)
	}
}

func TestBottomTabs_LogoutClearsPreferencesBeforeRequest(t *testing.T) {
	out := renderBottomTabs(t, false)
	if strings.Contains(out, failedSanitizationMarker) {
		t.Fatalf("BottomTabs rendered a failed-sanitization handler")
	}
	got := attrValues(t, out, "hx-on::before-request")
	want := "if(window.swPreferences)swPreferences.clearCache()"
	if len(got) != 1 || got[0] != want {
		t.Errorf("hx-on::before-request handlers = %q, want exactly [%q]", got, want)
	}
}
