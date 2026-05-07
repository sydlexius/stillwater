package components

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

//go:embed _settings-anchors.txt
var settingsAnchorsRaw string

// repoRoot walks upward from this package until it finds go.mod, then returns
// that directory. Tests use it to read repo-relative artifacts (the canonical
// anchors file under docs/, the templ tree).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod walking up from %s", dir)
		}
		dir = parent
	}
}

// loadAnchorSet parses the embedded anchors file into a set keyed by anchor
// slug. Empty lines and comment lines (#) are ignored to permit the codegen
// tool to add a header in future revisions without breaking this loader.
func loadAnchorSet(t *testing.T) map[string]struct{} {
	t.Helper()
	set := make(map[string]struct{})
	scanner := bufio.NewScanner(strings.NewReader(settingsAnchorsRaw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		set[line] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan anchors: %v", err)
	}
	if len(set) == 0 {
		t.Fatalf("embedded anchors set is empty")
	}
	return set
}

// TestSettingsAnchorsInSync asserts the embedded copy at
// web/components/_settings-anchors.txt matches the canonical copy under
// docs/site/src/reference/. The settings-reference codegen writes the docs
// copy; this test catches drift if a contributor regenerates one without the
// other. The Makefile generate-docs target syncs both paths.
func TestSettingsAnchorsInSync(t *testing.T) {
	root := repoRoot(t)
	canonical, err := os.ReadFile(filepath.Join(root, "docs", "site", "src", "reference", "_settings-anchors.txt"))
	if err != nil {
		// In a docs-stripped checkout the canonical file may be absent;
		// skip rather than fail so trimmed-tree builds still pass tests.
		t.Skipf("canonical anchors file not present: %v", err)
		return
	}
	if !bytes.Equal(canonical, []byte(settingsAnchorsRaw)) {
		t.Errorf("web/components/_settings-anchors.txt is out of sync with docs/site/src/reference/_settings-anchors.txt; run `make generate-docs`")
	}
}

// isContextHelpCall reports whether the call expression targets the
// ContextHelp templ component. Templ rewrites `@components.ContextHelp(...)`
// into a `components.ContextHelp(...).Render(ctx, w)` chain in the generated
// Go, and in-package callers reference it as `ContextHelp(...)`.
func isContextHelpCall(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		ident, ok := fun.X.(*ast.Ident)
		return ok && ident.Name == "components" && fun.Sel.Name == "ContextHelp"
	case *ast.Ident:
		return fun.Name == "ContextHelp"
	}
	return false
}

// TestContextHelpAnchors asserts that every components.ContextHelp(...)
// call site passes a docAnchor that is either empty or present in the
// embedded anchor set. The scan uses Go's AST over the templ-generated
// *_templ.go files so nested string literals inside the first three args
// (e.g. t(ctx, "settings.X.label")) don't trip the matcher. Drift here
// surfaces as a broken "Read more" link in the rendered popover; failing
// in CI is preferable to shipping a 404 to the user.
func TestContextHelpAnchors(t *testing.T) {
	anchors := loadAnchorSet(t)
	root := repoRoot(t)

	var (
		unknown    []string
		nonLiteral []string
	)
	walkErr := filepath.Walk(filepath.Join(root, "web"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, "_templ.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Logf("%s: parse skipped: %v", path, parseErr)
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isContextHelpCall(call) {
				return true
			}
			// ContextHelp(id, label, text, docAnchor). Index 3 is the
			// docAnchor; if the call has fewer args (e.g. an old caller
			// that the build hasn't surfaced yet) skip silently.
			if len(call.Args) < 4 {
				return true
			}
			lit, ok := call.Args[3].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				pos := fset.Position(call.Args[3].Pos())
				nonLiteral = append(nonLiteral, pos.String())
				return true
			}
			anchor, err := strconv.Unquote(lit.Value)
			if err != nil {
				pos := fset.Position(lit.Pos())
				t.Logf("%s: cannot unquote anchor literal %q: %v", pos, lit.Value, err)
				return true
			}
			if anchor == "" {
				return true
			}
			if _, ok := anchors[anchor]; !ok {
				pos := fset.Position(lit.Pos())
				unknown = append(unknown, pos.String()+": "+anchor)
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk web/: %v", walkErr)
	}
	if len(nonLiteral) > 0 {
		// Dynamic anchors cannot be validated statically. Treat as a
		// reviewer-visible warning rather than a hard failure so a future
		// caller can opt out by allowlisting; today there are none.
		t.Logf("ContextHelp call sites with non-literal docAnchor (skipped):\n  %s", strings.Join(nonLiteral, "\n  "))
	}
	if len(unknown) > 0 {
		t.Fatalf("ContextHelp call sites reference unknown settings anchors:\n  %s\n\nFix: pick an existing slug from web/components/_settings-anchors.txt or add a settings panel/i18n entry that the codegen will emit.", strings.Join(unknown, "\n  "))
	}
}

// TestContextHelpRender_NoDocAnchor verifies the popover renders without a
// "Read more" link when docAnchor is empty (the legacy 3-arg behavior).
func TestContextHelpRender_NoDocAnchor(t *testing.T) {
	var buf bytes.Buffer
	if err := ContextHelp("help-test", "Test", "Body text", "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Body text") {
		t.Errorf("popover missing body text: %s", out)
	}
	if strings.Contains(out, "sw-context-help-link") {
		t.Errorf("docAnchor empty but Read more link present: %s", out)
	}
}

// TestContextHelpRender_WithDocAnchor verifies the popover renders the
// "Read more" link pointing at the docs site when docAnchor is set.
func TestContextHelpRender_WithDocAnchor(t *testing.T) {
	var buf bytes.Buffer
	if err := ContextHelp("help-test", "Test", "Body text", "settings-general-base-path").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sw-context-help-link") {
		t.Errorf("expected sw-context-help-link class; got: %s", out)
	}
	if !strings.Contains(out, "/docs/reference/settings-by-tab/#settings-general-base-path") {
		t.Errorf("expected docs deep link; got: %s", out)
	}
}
