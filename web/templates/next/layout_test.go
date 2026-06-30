package next

import (
	"bytes"
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

// documentOrder returns the order (0-based position in a DFS walk) of the
// node, or -1 if not found.
func documentOrder(root, target *html.Node) int {
	pos := 0
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if n == target {
			return true
		}
		pos++
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	if walk(root) {
		return pos
	}
	return -1
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

	// 4. Skip link precedes the sidebar nav in document order.
	// The sidebar is the <nav> or the first <aside>/<div> with a sidebar marker.
	// We locate the first <nav> element as a proxy for the sidebar chrome.
	var navEl *html.Node
	walk(root, func(n *html.Node) {
		if navEl == nil && n.Data == "nav" {
			navEl = n
		}
	})
	if navEl == nil {
		t.Fatal("no <nav> element found in LayoutNext output -- cannot verify skip-link order")
	}
	skipPos := documentOrder(root, skipLink)
	navPos := documentOrder(root, navEl)
	if skipPos < 0 {
		t.Fatal("skip link not found in document order walk")
	}
	if navPos < 0 {
		t.Fatal("<nav> not found in document order walk")
	}
	if skipPos >= navPos {
		t.Errorf("skip link at position %d, <nav> at %d -- skip link must precede nav", skipPos, navPos)
	}

	// 5. Skip link text content is present (non-empty accessible label).
	if !strings.Contains(raw, "Skip to main content") {
		t.Error("skip link text 'Skip to main content' not found in rendered output")
	}
}
