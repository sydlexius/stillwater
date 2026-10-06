package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
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

// stubImporter resolves every import to an empty package, so the scan can
// type-check these files alone (see the Config below).
type stubImporter struct{}

func (stubImporter) Import(path string) (*types.Package, error) {
	p := types.NewPackage(path, path[strings.LastIndex(path, "/")+1:])
	p.MarkComplete()
	return p, nil
}

// paramIdent returns the i-th parameter name of fn (nil when absent).
func paramIdent(fn *ast.FuncDecl, i int) *ast.Ident {
	for _, f := range fn.Type.Params.List {
		for _, id := range f.Names {
			if i == 0 {
				return id
			}
			i--
		}
	}
	return nil
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
	// Owner struct -> its code fields. Run-result Error and the artist/file name
	// fields are free text and are not here, so they are skipped. A field named
	// like a code on an owner NOT listed (or whose type cannot be resolved) is
	// treated as a code: the scan fails closed.
	covered := map[string]map[string]string{
		"extraFanartFileResult":   {"Outcome": "outcome", "Reason": "reason"},
		"extraFanartArtistResult": {"Error": "reason"},
		"extraFanartRunResult":    {"Status": "status"},
	}
	kindOf := map[string]string{"Outcome": "outcome", "Status": "status", "Reason": "reason", "Error": "reason"}
	// passParams: function -> index of a string parameter that only carries a
	// reason* const. Allow-list, checked at every call site below.
	passParams := map[string]int{"unattempted": 1}
	// runResultVars: function -> local that holds the run result there, whose Error
	// is free text. Its type comes from a Router method this scan cannot resolve,
	// so it is allow-listed here; any other Error assignment is treated as a code.
	runResultVars := map[string]string{"handleExtraFanartMigrationRun": "res"}

	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range extraFanartCodeFiles {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	// Type-check with stub imports: types declared in these files (and so every
	// receiver of a code field) resolve; anything from another package does not,
	// and the errors that causes are ignored.
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Error: func(error) {}, Importer: stubImporter{}}
	_, _ = conf.Check("api", fset, files, info)
	ownerOf := func(e ast.Expr) string {
		tv, ok := info.Types[e]
		if !ok {
			return ""
		}
		typ := tv.Type
		if p, ok := typ.(*types.Pointer); ok {
			typ = p.Elem()
		}
		if n, ok := typ.(*types.Named); ok {
			return n.Obj().Name()
		}
		return ""
	}
	declared := map[string]bool{} // reason* consts with a string-literal value
	var fn *ast.FuncDecl
	// add records a literal; it accepts a reason* const declared here, an allow-listed
	// parameter, or a field of a covered owner (a pass-through). Else it fails.
	add := func(kind string, e ast.Expr, pos token.Pos) {
		switch x := e.(type) {
		case *ast.BasicLit:
			if s, err := strconv.Unquote(x.Value); err == nil && x.Kind == token.STRING {
				codes[kind][s] = true
				return
			}
		case *ast.Ident:
			if _, isConst := info.Uses[x].(*types.Const); isConst && declared[x.Name] {
				return
			}
			if v, ok := info.Uses[x].(*types.Var); ok && fn != nil && fn.Type.Params != nil {
				if i, ok := passParams[fn.Name.Name]; ok && info.Defs[paramIdent(fn, i)] == v {
					return
				}
			}
		case *ast.SelectorExpr:
			if _, ok := covered[ownerOf(x.X)][x.Sel.Name]; ok {
				return
			}
		}
		t.Errorf("%s: the %s value is not a literal, a declared reason* const or a pass-through of a covered field; add a label for it or extend the allow-list in this test", fset.Position(pos), kind)
	}
	check := func(owner, field string, v ast.Expr, pos token.Pos) {
		if k, ok := covered[owner][field]; ok {
			add(k, v, pos)
		} else if _, known := covered[owner]; !known && kindOf[field] != "" {
			add(kindOf[field], v, pos) // unresolved or unknown owner: fail closed
		}
	}
	for _, file := range files {
		for _, d := range file.Decls { // pass 1: declared consts
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
				for _, sp := range g.Specs {
					vs := sp.(*ast.ValueSpec)
					for i, id := range vs.Names {
						if strings.HasPrefix(id.Name, "reason") && i < len(vs.Values) {
							add("reason", vs.Values[i], id.Pos())
							declared[id.Name] = true
						}
					}
				}
			}
		}
	}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				fn = x
			case *ast.CompositeLit:
				for _, el := range x.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok {
							check(ownerOf(x), key.Name, kv.Value, kv.Pos())
						}
					}
				}
			case *ast.CallExpr:
				// Every call of an allow-listed function passes a declared reason* const.
				name := ""
				switch f := x.Fun.(type) {
				case *ast.Ident:
					name = f.Name
				case *ast.SelectorExpr:
					name = f.Sel.Name
				}
				if i, ok := passParams[name]; ok && i < len(x.Args) {
					add("reason", x.Args[i], x.Pos())
				}
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) {
					for i, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok {
							if id, isID := sel.X.(*ast.Ident); isID && sel.Sel.Name == "Error" && fn != nil && runResultVars[fn.Name.Name] == id.Name {
								continue
							}
							check(ownerOf(sel.X), sel.Sel.Name, x.Rhs[i], x.Pos())
						}
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
