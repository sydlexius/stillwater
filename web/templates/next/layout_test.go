package next

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/sydlexius/stillwater/web/templates"
)

// renderLayoutNext renders LayoutNext with empty children and returns the HTML
// string and parsed document root.
func renderLayoutNext(tb testing.TB) (string, *html.Node) {
	tb.Helper()
	var buf bytes.Buffer
	ctx := nextTestCtx(tb)
	if err := LayoutNext("Test", templates.AssetPaths{}).Render(ctx, &buf); err != nil {
		tb.Fatalf("rendering LayoutNext: %v", err)
	}
	root, err := html.Parse(strings.NewReader(buf.String()))
	if err != nil {
		tb.Fatalf("parsing LayoutNext HTML: %v", err)
	}
	return buf.String(), root
}

// findByID returns the first element with the given id attribute, or nil.
func findByID(n *html.Node, id string) *html.Node {
	if n.Type == html.ElementNode {
		for _, a := range n.Attr {
			if a.Key == "id" && a.Val == id {
				return n
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findByID(c, id); found != nil {
			return found
		}
	}
	return nil
}

// findFirst returns the first element matching tag + class predicate (DFS).
func findFirst(n *html.Node, tag string, hasClass func(string) bool) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		for _, a := range n.Attr {
			if a.Key == "class" && hasClass(a.Val) {
				return n
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findFirst(c, tag, hasClass); found != nil {
			return found
		}
	}
	return nil
}

// isKeyboardFocusable reports whether n participates in the keyboard tab order:
// a natively focusable control (a/area with href, button, select, textarea,
// non-hidden input) or any element with tabindex >= 0. Elements with
// tabindex="-1" (e.g. the main landmark) are programmatically focusable but NOT
// tabbable, and disabled controls are skipped, so both are excluded.
func isKeyboardFocusable(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	var href, tabindex, inputType string
	hasTabindex, disabled := false, false
	for _, a := range n.Attr {
		switch a.Key {
		case "href":
			href = a.Val
		case "tabindex":
			tabindex, hasTabindex = a.Val, true
		case "type":
			inputType = a.Val
		case "disabled":
			disabled = true
		}
	}
	if disabled {
		return false
	}
	if hasTabindex {
		return tabindex != "" && !strings.HasPrefix(tabindex, "-")
	}
	switch n.Data {
	case "a", "area":
		return href != ""
	case "button", "select", "textarea":
		return true
	case "input":
		return inputType != "hidden"
	}
	return false
}

// textContent returns the concatenated text-node content under n.
func textContent(n *html.Node) string {
	var sb strings.Builder
	var rec func(*html.Node)
	rec = func(m *html.Node) {
		if m.Type == html.TextNode {
			sb.WriteString(m.Data)
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return sb.String()
}

// firstFocusable returns the first keyboard-focusable element in document order,
// or nil if none exists.
func firstFocusable(root *html.Node) *html.Node {
	var found *html.Node
	var walkNode func(*html.Node)
	walkNode = func(n *html.Node) {
		if found != nil {
			return
		}
		if isKeyboardFocusable(n) {
			found = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walkNode(c)
		}
	}
	walkNode(root)
	return found
}

// TestLayoutNext_SkipLink verifies WCAG 2.4.1 Bypass Blocks: the skip-to-main
// link must be the first focusable element, precede the sidebar nav, carry the
// correct class, and target the main landmark id.
func TestLayoutNext_SkipLink(t *testing.T) {
	t.Parallel()
	raw, root := renderLayoutNext(t)

	// 1. Skip link is present.
	skipLink := findFirst(root, "a", func(cls string) bool {
		return strings.Contains(cls, "sw-skip-link")
	})
	if skipLink == nil {
		t.Fatal("sw-skip-link <a> not found in LayoutNext output")
	}

	// 2. Skip link targets the main landmark id.
	href := ""
	for _, a := range skipLink.Attr {
		if a.Key == "href" {
			href = a.Val
			break
		}
	}
	const wantHref = "#sw-main"
	if href != wantHref {
		t.Errorf("skip link href = %q, want %q", href, wantHref)
	}

	// 3. Main landmark has the matching id and tabindex="-1".
	mainEl := findByID(root, "sw-main")
	if mainEl == nil {
		t.Fatal(`<main id="sw-main"> not found in LayoutNext output`)
	}
	if mainEl.Data != "main" {
		t.Errorf("<element id=sw-main> is <%s>, want <main>", mainEl.Data)
	}
	tabindex := ""
	for _, a := range mainEl.Attr {
		if a.Key == "tabindex" {
			tabindex = a.Val
			break
		}
	}
	if tabindex != "-1" {
		t.Errorf("<main id=sw-main> tabindex = %q, want \"-1\"", tabindex)
	}

	// 4. Skip link is the FIRST keyboard-focusable element in the whole
	// document -- the real WCAG 2.4.1 contract, not the earlier <nav>-precedence
	// proxy. Any tabbable control inserted before it (a button, link, input, or
	// positive-tabindex element, anywhere including the head) now fails this
	// test, whereas the nav proxy would still pass.
	first := firstFocusable(root)
	if first == nil {
		t.Fatal("no keyboard-focusable element found in LayoutNext output")
	}
	if first != skipLink {
		desc := "<" + first.Data
		for _, a := range first.Attr {
			if a.Key == "id" || a.Key == "class" {
				desc += " " + a.Key + "=" + strconv.Quote(a.Val)
			}
		}
		desc += ">"
		t.Errorf("first keyboard-focusable element is %s; the sw-skip-link must be first", desc)
	}

	// 5. Skip link carries a non-empty accessible name (localized via t(ctx,...)).
	// The test context uses the en translator, so the label resolves to English.
	if text := strings.TrimSpace(textContent(skipLink)); text == "" {
		t.Error("skip link has empty text content -- accessible name missing")
	} else if !strings.Contains(raw, "Skip to main content") {
		t.Errorf("skip link label = %q; want the localized %q", text, "Skip to main content")
	}
}

// TestLayoutNext_MountsCommandPalette verifies the Cmd-K command palette shell
// is mounted in LayoutNext, hidden by default (#1775).
func TestLayoutNext_MountsCommandPalette(t *testing.T) {
	html, root := renderLayoutNext(t)
	if !strings.Contains(html, `id="sw-cmdk"`) {
		t.Fatal("command palette root #sw-cmdk not found in LayoutNext output")
	}
	node := findFirst(root, "div", func(cls string) bool { return strings.Contains(cls, "sw-cmdk-overlay") })
	if node == nil {
		t.Fatal("command palette overlay not rendered")
	}
	// Hidden by default.
	if !strings.Contains(html, `id="sw-cmdk" class="fixed inset-0 z-50 hidden`) {
		t.Fatal("command palette must be hidden by default")
	}
}
