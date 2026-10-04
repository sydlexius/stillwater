package logging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/logging/logtest"
)

func TestWithComponent_EmitsOneComponent(t *testing.T) {
	base, buf := logtest.NewJSONLogger()
	WithComponent(base, "alpha").With("k", "v").Info("hello")

	out := buf.String()
	if d := logtest.DuplicateKeys(out); len(d) != 0 {
		t.Fatalf("duplicate keys: %v", d)
	}
	if !strings.Contains(out, `"component":"alpha"`) || !strings.Contains(out, `"k":"v"`) {
		t.Fatalf("missing component or attr: %s", out)
	}
}

// Re-tagging must never stack the key: the new name wins and an Error record
// names both so the wiring bug is visible.
func TestWithComponent_RetagReplacesAndFailsLoudly(t *testing.T) {
	base, buf := logtest.NewJSONLogger()
	outer := WithComponent(base, "outer")
	inner := WithComponent(outer, "inner")
	inner.Info("work")

	out := buf.String()
	if d := logtest.DuplicateKeys(out); len(d) != 0 {
		t.Fatalf("duplicate keys after re-tag: %v\n%s", d, out)
	}
	if !strings.Contains(out, `"msg":"work"`) || !strings.Contains(out, `"component":"inner"`) {
		t.Fatalf("the new component must win: %s", out)
	}
	for _, want := range []string{`"level":"ERROR"`, `"previous_component":"outer"`, `"new_component":"inner"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in loud re-tag record: %s", want, out)
		}
	}
	// The original logger is untouched by the re-tag.
	buf.Reset()
	outer.Info("still outer")
	if !strings.Contains(buf.String(), `"component":"outer"`) {
		t.Fatalf("re-tag mutated the original logger: %s", buf.String())
	}
}

// attrStart maps a slog call name to the index of its first key/value argument
// (the arguments before it are a context, level or message).
var attrStart = map[string]int{
	"With":  0,
	"Debug": 1, "Info": 1, "Warn": 1, "Error": 1,
	"DebugContext": 2, "InfoContext": 2, "WarnContext": 2, "ErrorContext": 2,
	"Log": 3, "LogAttrs": 3,
}

// TestNoRawComponentTagging is the class guard for #2787: the component key may
// only be set through WithComponent, because a raw logger.With("component", ...)
// stacks a second key onto an already-tagged logger and the compiler cannot
// see it. It runs rawComponentOffenders over every non-test Go file under the
// repo's internal/ and cmd/ trees.
func TestNoRawComponentTagging(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			got, perr := rawComponentOffenders(path, src)
			offenders = append(offenders, got...)
			return perr
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("raw \"component\" key found; use logging.WithComponent:\n%s", strings.Join(offenders, "\n"))
	}
}

// rawComponentOffenders parses one file and returns the position of every
// literal "component" (interpreted or raw string) that could be a KEY in a
// With/Debug/Info/Warn/Error/Log/LogAttrs call (or a Context variant), as a
// bare key, a slog.String-style constructor, or a slog.Attr{Key: ...} literal.
//
// Key/value pairing is followed exactly while every key position holds a string
// literal or a recognized slog Attr form, so With("name", "component") passes.
// A key position holding anything else (for example an Attr variable) makes the
// alignment unknowable without type information, so from there to the end of
// the call EVERY "component" literal is flagged, whichever slot it is in.
// Still out of reach of a syntactic check: a constant or computed key, and a
// spread attrs... slice. WithComponent itself uses the componentKey constant,
// which is why it does not trip the scan.
func rawComponentOffenders(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var offenders []string
	flag := func(e ast.Expr) { offenders = append(offenders, fset.Position(e.Pos()).String()) }
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		start, ok := attrStart[sel.Sel.Name]
		if !ok {
			return true
		}
		aligned := true
		for i := start; i < len(call.Args); {
			a := call.Args[i]
			if !aligned {
				// Alignment lost: any literal, or Attr key, could be a key.
				if isComponentLiteral(a) {
					flag(a)
				} else if key, isAttr := attrKey(a); isAttr && key != nil && isComponentLiteral(key) {
					flag(a)
				}
				i++
				continue
			}
			if key, isAttr := attrKey(a); isAttr {
				if key != nil && isComponentLiteral(key) {
					flag(a)
				}
				i++
			} else if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if isComponentLiteral(a) {
					flag(a)
				}
				i += 2 // bare key and its value
			} else {
				aligned = false // an Attr variable or other expression: unknowable
				i++
			}
		}
		return true
	})
	return offenders, nil
}

// TestRawComponentDetector feeds source snippets through the detector, so a
// regression in attrStart, attrKey or isComponentLiteral cannot make the repo
// walk pass vacuously.
func TestRawComponentDetector(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"bare key/value", `l.With("component", "x")`, 1},
		{"bare key after other pairs", `l.With("a", "b", "component", "x")`, 1},
		{"slog.String constructor", `l.With(slog.String("component", "x"))`, 1},
		{"slog.Attr literal", `l.With(slog.Attr{Key: "component"})`, 1},
		{"raw-string key", "l.With(`component`, \"x\")", 1},
		{"Context variant", `l.InfoContext(ctx, "m", "component", "x")`, 1},
		{"Log offset", `l.Log(ctx, lvl, "m", "component", "x")`, 1},
		{"LogAttrs offset", `l.LogAttrs(ctx, lvl, "m", slog.String("component", "x"))`, 1},
		{"Attr variable then raw pair", `l.Log(ctx, lvl, "m", attr, "component", "x")`, 1},
		{"value position is not a key", `l.With("name", "component")`, 0},
		{"message text is not a key", `l.Info("component", "k", "v")`, 0},
		{"through WithComponent", `WithComponent(l, "component")`, 0},
		{"different key", `l.With(slog.String("name", "x"))`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p\nimport \"log/slog\"\nvar _ = slog.LevelInfo\nfunc f() {\n" + tc.body + "\n}\n"
			got, err := rawComponentOffenders("snippet.go", []byte(src))
			if err != nil {
				t.Fatalf("snippet did not parse: %v\n%s", err, src)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d offenders %v, want %d\n%s", len(got), got, tc.want, src)
			}
		})
	}
}

// attrKey reports whether e is a slog.Attr expression (a slog.String-style
// constructor call or a slog.Attr composite literal) and, if so, its key
// expression (nil when it cannot be located).
func attrKey(e ast.Expr) (ast.Expr, bool) {
	switch v := e.(type) {
	case *ast.CallExpr:
		if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "slog" && len(v.Args) > 0 {
				return v.Args[0], true
			}
		}
	case *ast.CompositeLit:
		if sel, ok := v.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Attr" {
			for _, el := range v.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Key" {
						return kv.Value, true
					}
				}
			}
			return nil, true
		}
	}
	return nil, false
}

// isComponentLiteral matches the literal "component" key as an interpreted or
// raw string.
func isComponentLiteral(e ast.Expr) bool {
	v, ok := e.(*ast.BasicLit)
	return ok && v.Kind == token.STRING && (v.Value == `"component"` || v.Value == "`component`")
}

// The detector must itself see the defect it guards against: a raw double
// With stacks the key and slog emits it twice.
func TestDuplicateKeysDetectsRawStacking(t *testing.T) {
	base, buf := logtest.NewJSONLogger()
	base.With("component", "a").With("component", "b").Info("x")
	if d := logtest.DuplicateKeys(buf.String()); len(d) != 1 {
		t.Fatalf("detector missed a raw stacked key: %v / %s", d, buf.String())
	}
}

// Re-tagging with the SAME name is not a wiring bug worth an Error record, but
// it must still yield exactly one key.
func TestWithComponent_SameNameRetagIsSilent(t *testing.T) {
	base, buf := logtest.NewJSONLogger()
	WithComponent(WithComponent(base, "same"), "same").Info("work")

	out := buf.String()
	if d := logtest.DuplicateKeys(out); len(d) != 0 {
		t.Fatalf("duplicate keys: %v\n%s", d, out)
	}
	if strings.Contains(out, `"`+PreviousComponentKey+`"`) || strings.Count(out, "\n") != 1 {
		t.Fatalf("same-name re-tag must not log an Error record: %s", out)
	}
}
