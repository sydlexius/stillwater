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

//go:embed _doc-anchors.txt
var docAnchorsRaw string

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

// loadAnchorSet parses an embedded anchors file into a set. Empty lines and
// comment lines (#) are ignored to permit codegen headers.
func loadAnchorSet(t *testing.T, raw string) map[string]struct{} {
	t.Helper()
	set := make(map[string]struct{})
	scanner := bufio.NewScanner(strings.NewReader(raw))
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

// TestDocAnchorsInSync asserts the embedded copy at
// web/components/_doc-anchors.txt matches the canonical copy under
// docs/site/src/reference/. The gen-doc-anchors tool writes both; this test
// catches drift if a contributor regenerates one without the other.
func TestDocAnchorsInSync(t *testing.T) {
	root := repoRoot(t)
	canonical, err := os.ReadFile(filepath.Join(root, "docs", "site", "src", "reference", "_doc-anchors.txt"))
	if err != nil {
		// In a docs-stripped checkout the canonical file may be absent;
		// skip rather than fail so trimmed-tree builds still pass tests.
		t.Skipf("canonical doc-anchors file not present: %v", err)
		return
	}
	if !bytes.Equal(canonical, []byte(docAnchorsRaw)) {
		t.Errorf("web/components/_doc-anchors.txt is out of sync with docs/site/src/reference/_doc-anchors.txt; run `make generate-docs`")
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

// nonLiteralAnchorAllowlist enumerates the (templ source path, enclosing
// templ function name) pairs whose ContextHelp call passes its docAnchor
// through from its own parameter list rather than as a string literal.
// Each such helper is itself called only from sites whose docAnchor IS a
// literal (and those upstream call sites get validated by this test), so
// the helper itself is safe to skip. Scoping by symbol -- not by file --
// means a future non-literal ContextHelp added elsewhere in the same
// templ still fails fast.
var nonLiteralAnchorAllowlist = map[string]map[string]struct{}{
	"web/templates/settings.templ": {
		// connectionFeatureToggleTT(connID, feature, label, enabled, tooltip,
		// docAnchor) forwards docAnchor to ContextHelp. Its three caller
		// sites in the same file pass literal anchors that this test
		// validates upstream.
		"connectionFeatureToggleTT": {},
	},
}

// TestContextHelpAnchors asserts that every components.ContextHelp(...)
// call site passes a docAnchor that is either empty, present in the
// embedded anchor set, or routed through a templ helper whose own callers
// are validated (see nonLiteralAnchorAllowlist). The scan uses Go's AST
// over the templ-generated *_templ.go files so nested string literals
// inside the first three args (e.g. t(ctx, "settings.X.label")) don't
// trip the matcher. Drift here surfaces as a broken "Read more" link in
// the rendered popover; failing in CI is preferable to shipping a 404
// to the user.
//
// Routing: if the anchor contains a slash it is looked up in the doc-anchors
// set (cross-section docs pages); otherwise in the settings-anchors set
// (legacy settings-by-tab behavior).
func TestContextHelpAnchors(t *testing.T) {
	settingsAnchors := loadAnchorSet(t, settingsAnchorsRaw)
	docAnchors := loadAnchorSet(t, docAnchorsRaw)
	root := repoRoot(t)

	var unknown []string
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
			// A *_templ.go file that won't parse is itself a CI-blocking
			// defect (regenerate templ?). Skipping silently would leave the
			// rest of this file's call sites unvalidated, so the anchor
			// contract test could pass green while a real broken Read more
			// link ships. Hard fail.
			t.Errorf("%s: parse failed (regenerate templ?): %v", path, parseErr)
			return nil
		}
		// Resolve the source templ path once per file -- templ generates
		// *_templ.go alongside its *.templ. The allowlist keys on the
		// .templ path so a future contributor opens the source, not the
		// generated mirror.
		srcPath := strings.TrimSuffix(path, "_templ.go") + ".templ"
		rel, relErr := filepath.Rel(root, srcPath)
		if relErr != nil {
			rel = srcPath
		}
		rel = filepath.ToSlash(rel)
		// Walk top-level decls so we know which templ helper an inner
		// ContextHelp call belongs to. Templ compiles each `templ Foo(...)`
		// into a top-level `func Foo(...) templ.Component`, so the
		// enclosing FuncDecl name is the symbol the allowlist scopes by.
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			symbolName := fn.Name.Name
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isContextHelpCall(call) {
					return true
				}
				// ContextHelp(id, label, text, docAnchor). Anything other
				// than the 4-arg signature is a legacy caller the build
				// hasn't yet surfaced; fail here with the call site.
				if len(call.Args) != 4 {
					pos := fset.Position(call.Pos())
					t.Errorf("%s: ContextHelp called with %d args, want 4", pos, len(call.Args))
					return true
				}
				lit, ok := call.Args[3].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					// Allowlist match requires BOTH the source templ path
					// AND the enclosing helper symbol. File-scope alone
					// would silently absorb a future non-literal call site
					// added elsewhere in the same templ.
					if symbols, ok := nonLiteralAnchorAllowlist[rel]; ok {
						if _, ok := symbols[symbolName]; ok {
							return true
						}
					}
					pos := fset.Position(call.Args[3].Pos())
					t.Errorf("%s: ContextHelp in %s called with non-literal docAnchor; pass a string literal or allowlist the (file, symbol) pair", pos, symbolName)
					return true
				}
				anchor, err := strconv.Unquote(lit.Value)
				if err != nil {
					pos := fset.Position(lit.Pos())
					t.Errorf("%s: cannot unquote anchor literal %q: %v", pos, lit.Value, err)
					return true
				}
				if anchor == "" {
					return true
				}
				// Route to the appropriate anchor set based on whether the
				// anchor contains a slash (cross-section doc path) or not
				// (legacy settings-by-tab slug).
				var anchors map[string]struct{}
				var setName string
				if strings.Contains(anchor, "/") {
					anchors = docAnchors
					setName = "web/components/_doc-anchors.txt"
				} else {
					anchors = settingsAnchors
					setName = "web/components/_settings-anchors.txt"
				}
				if _, ok := anchors[anchor]; !ok {
					pos := fset.Position(lit.Pos())
					unknown = append(unknown, pos.String()+": "+anchor+" (not in "+setName+")")
				}
				return true
			})
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk web/: %v", walkErr)
	}
	if len(unknown) > 0 {
		t.Fatalf("ContextHelp call sites reference unknown anchors:\n  %s\n\nFix: pick an existing slug from the appropriate anchors file or run `make generate-docs`.", strings.Join(unknown, "\n  "))
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

// TestContextHelpRender_WithSettingsAnchor verifies the popover renders the
// "Read more" link pointing at the settings reference page when the docAnchor
// is a plain slug (no slash) -- the legacy Settings call-site behavior.
func TestContextHelpRender_WithSettingsAnchor(t *testing.T) {
	var buf bytes.Buffer
	if err := ContextHelp("help-test", "Test", "Body text", "settings-general-base-path").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sw-context-help-link") {
		t.Errorf("expected sw-context-help-link class; got: %s", out)
	}
	if !strings.Contains(out, "https://sydlexius.github.io/stillwater/reference/settings-by-tab/#settings-general-base-path") {
		t.Errorf("expected docs deep link; got: %s", out)
	}
}

// TestContextHelpRender_WithDocPathAnchor verifies the popover renders the
// "Read more" link pointing at a cross-section docs page when the docAnchor
// contains a slash (the new non-settings path). The URL must NOT contain the
// "reference/settings-by-tab/" infix.
func TestContextHelpRender_WithDocPathAnchor(t *testing.T) {
	var buf bytes.Buffer
	if err := ContextHelp("help-test", "Test", "Body text", "core-concepts/field-locks#layer-1-artist-lock-the-big-switch").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sw-context-help-link") {
		t.Errorf("expected sw-context-help-link class; got: %s", out)
	}
	wantURL := "https://sydlexius.github.io/stillwater/core-concepts/field-locks#layer-1-artist-lock-the-big-switch"
	if !strings.Contains(out, wantURL) {
		t.Errorf("expected docs deep link %q; got: %s", wantURL, out)
	}
	if strings.Contains(out, "reference/settings-by-tab/") {
		t.Errorf("cross-section docAnchor must not route through settings-by-tab; got: %s", out)
	}
}
