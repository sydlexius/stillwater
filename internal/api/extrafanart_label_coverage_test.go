package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/i18n"
)

// extraFanartCodeFiles are the handler sources scanned for every code the
// server can emit. Adding a file that produces codes is a one-line change here.
var extraFanartCodeFiles = []string{
	"handlers_extrafanart_migration.go",
}

// Every code the handlers can emit has a label in en.json. drift_test.go cannot
// see these keys (they are built from the code), which is how an earlier
// branch's copy drifted. The codes are read from source: every reason* const,
// plus every string assigned to, or set in a composite literal as, an Outcome,
// Status or Reason field (and an artist-level Error, which holds a reason code).
// A right-hand side the scan cannot read as a string fails the test rather than
// being skipped, so a code built at runtime cannot escape it.
func TestExtraFanartLabelKeysCoverEveryCode(t *testing.T) {
	t.Parallel()
	codes := map[string]map[string]bool{"reason": {}, "outcome": {}, "status": {}}
	kindOf := map[string]string{"Outcome": "outcome", "Status": "status", "Reason": "reason", "Error": "reason"}

	fset := token.NewFileSet()
	for _, name := range extraFanartCodeFiles {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		// add records a literal, or accepts a pass-through of a field or const this
		// scan already covers; anything else cannot be checked and fails.
		add := func(kind string, e ast.Expr, pos token.Pos) {
			switch x := e.(type) {
			case *ast.BasicLit:
				if s, err := strconv.Unquote(x.Value); err == nil && x.Kind == token.STRING {
					codes[kind][s] = true
					return
				}
			case *ast.Ident:
				if strings.HasPrefix(x.Name, "reason") {
					return // a reason* const, collected from its declaration
				}
			case *ast.SelectorExpr:
				if _, ok := kindOf[x.Sel.Name]; ok {
					return // copies a covered field (for example Reason: a.Error)
				}
			}
			t.Errorf("%s: %s is not a string literal, a reason* const or a covered field; its codes cannot be checked", fset.Position(pos), kind)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for i, id := range x.Names {
					if strings.HasPrefix(id.Name, "reason") && i < len(x.Values) {
						add("reason", x.Values[i], id.Pos())
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := x.Key.(*ast.Ident); ok && kindOf[key.Name] != "" {
					add(kindOf[key.Name], x.Value, x.Pos())
				}
			case *ast.AssignStmt:
				if len(x.Lhs) != len(x.Rhs) {
					return true
				}
				for i, lhs := range x.Lhs {
					// Only Outcome, Status and Reason: a bare "Error =" is also the run-level
					// free-text message, which is never rendered as a label.
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name != "Error" && kindOf[sel.Sel.Name] != "" {
						add(kindOf[sel.Sel.Name], x.Rhs[i], x.Pos())
					}
				}
			}
			return true
		})
	}
	// Guard against a vacuous pass: the scan must find the known vocabulary.
	for kind, must := range map[string]string{"reason": "folder_unreadable", "outcome": "source_gone", "status": "nothing_checked"} {
		if !codes[kind][must] {
			t.Fatalf("scan found no %s code %q; the AST walk is broken (found %v)", kind, must, codes[kind])
		}
	}
	bundle, err := i18n.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	tr := bundle.Translator("en")
	for kind, set := range codes {
		for code := range set {
			if key := "extrafanart_migration." + kind + "_" + code; tr.T(key) == key {
				t.Errorf("no en.json label for %s", key)
			}
		}
	}
}
