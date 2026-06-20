// Package connection_test contains conformance tests for the connection sub-packages.
//
// This file enforces the invariant that every write method on *Client in the
// three platform packages (emby, jellyfin, lidarr) wraps returned errors via
// wrapAuthIfStatusAuth so the publish layer can detect auth failures and
// surface them as connection.ErrAuthRequired rather than opaque transport errors.
//
// A "write method" is any method on a *Client receiver that:
//  1. has an error in its return list, and
//  2. contains a write indicator in its body: a call to Post, PostJSON, or
//     PutJSON, or a reference to http.MethodPost or http.MethodDelete.
//
// Two tests live here:
//
//  1. TestWriteMethodsWrapAuthIfStatusAuth loads the three platform packages
//     with full type information, walks their ASTs to identify write methods,
//     and fails if any such method omits wrapAuthIfStatusAuth. Methods that
//     legitimately skip the guard are listed in allowedUnwrappedWriteMethods
//     with a rationale string.
//
//  2. TestDetectorFiresOnContrivedWriteMethod writes a synthetic Go file
//     containing a write method that omits wrapAuthIfStatusAuth, loads it, and
//     asserts the detector fires. If the AST walker ever regresses (visitor
//     change, type-info failure), this test fails alongside the production scan.
//
// Pattern: mirrors internal/httpsafe/no_raw_client_construction_test.go.
package connection_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// allowedUnwrappedWriteMethods lists *Client methods that legitimately omit
// wrapAuthIfStatusAuth. Keys are "pkgname.MethodName"; values are the rationale.
//
// Adding an entry here requires a matching rationale comment in the production
// file explaining why wrapAuthIfStatusAuth is inappropriate for that method.
var allowedUnwrappedWriteMethods = map[string]string{
	// emby.refreshItem: fire-and-forget helper; its return type is void so
	// wrapAuthIfStatusAuth is inapplicable. Errors are logged, not returned.
	// Included here as documentation; the void-return filter already excludes it.
	"emby.refreshItem": "void return: wrapAuthIfStatusAuth not applicable to fire-and-forget helper",
}

// wrapAuthTargetPkgs are the three platform packages under internal/connection/
// whose *Client write methods must use wrapAuthIfStatusAuth.
var wrapAuthTargetPkgs = []string{
	"github.com/sydlexius/stillwater/internal/connection/emby",
	"github.com/sydlexius/stillwater/internal/connection/jellyfin",
	"github.com/sydlexius/stillwater/internal/connection/lidarr",
}

// TestWriteMethodsWrapAuthIfStatusAuth walks the three platform packages and
// fails if any write method on *Client is missing a wrapAuthIfStatusAuth call.
func TestWriteMethodsWrapAuthIfStatusAuth(t *testing.T) {
	repoRoot, err := wrapAuthFindRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps,
		Dir:   repoRoot,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, wrapAuthTargetPkgs...)
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}

	if errs := wrapAuthCollectLoadErrors(pkgs); len(errs) > 0 {
		t.Fatalf("packages.Load reported %d per-package error(s); scan cannot prove the absence of violations:\n  %s",
			len(errs), strings.Join(errs, "\n  "))
	}
	if wrapAuthCountWithSyntax(pkgs) == 0 {
		t.Fatalf("packages.Load returned no packages with syntax; check repo state")
	}

	var violations []string
	for _, pkg := range pkgs {
		for i, file := range pkg.Syntax {
			if i >= len(pkg.CompiledGoFiles) {
				continue
			}
			relPath, relErr := filepath.Rel(repoRoot, pkg.CompiledGoFiles[i])
			if relErr != nil {
				relPath = pkg.CompiledGoFiles[i]
			}
			relPath = filepath.ToSlash(relPath)

			for _, finding := range scanFileForUnwrappedWriteMethods(pkg.Fset, pkg.TypesInfo, file, pkg.Name, relPath) {
				// Each finding is formatted as "pkgname.MethodName  file:line  ...".
				// Extract the allowlist key (everything before the first "  ").
				key := finding
				if idx := strings.Index(finding, "  "); idx >= 0 {
					key = finding[:idx]
				}
				if _, allowed := allowedUnwrappedWriteMethods[key]; !allowed {
					violations = append(violations, finding)
				}
			}
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("found %d write method(s) missing wrapAuthIfStatusAuth:\n  %s\n"+
			"For each: add a wrapAuthIfStatusAuth call to the method, or add an entry to "+
			"allowedUnwrappedWriteMethods in wrap_auth_required_test.go with a rationale AND "+
			"a matching rationale comment in the production file.",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// TestDetectorFiresOnContrivedWriteMethod validates that the detector correctly
// identifies a write method that omits wrapAuthIfStatusAuth, preventing a silent
// regression in the AST walker from masking real violations in the production scan.
func TestDetectorFiresOnContrivedWriteMethod(t *testing.T) {
	// BadWrite calls PostJSON (a write indicator) but omits wrapAuthIfStatusAuth.
	const src = `package emby

func (c *Client) BadWrite() error {
	return c.PostJSON()
}

type Client struct{}

func (c *Client) PostJSON() error { return nil }
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture.go: %v", err)
	}

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps,
		Dir:   dir,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if errs := wrapAuthCollectLoadErrors(pkgs); len(errs) > 0 {
		t.Fatalf("fixture packages.Load reported %d error(s):\n  %s", len(errs), strings.Join(errs, "\n  "))
	}
	if wrapAuthCountWithSyntax(pkgs) == 0 {
		t.Fatalf("fixture failed to load with syntax")
	}

	var findings []string
	for _, pkg := range pkgs {
		for i, file := range pkg.Syntax {
			if i >= len(pkg.GoFiles) {
				continue
			}
			relPath := filepath.ToSlash(filepath.Base(pkg.GoFiles[i]))
			findings = append(findings, scanFileForUnwrappedWriteMethods(pkg.Fset, pkg.TypesInfo, file, pkg.Name, relPath)...)
		}
	}

	if len(findings) == 0 {
		t.Fatalf("detector did not fire on contrived fixture; the production scan may be silently broken")
	}
	// BadWrite is on line 3 of the fixture source (package decl on 1, blank on 2, func on 3).
	if !strings.Contains(findings[0], "BadWrite") {
		t.Fatalf("expected finding to reference BadWrite; got: %v", findings)
	}
	if !strings.Contains(findings[0], "fixture.go:3") {
		t.Fatalf("expected finding to reference fixture.go:3; got: %v", findings)
	}
}

// scanFileForUnwrappedWriteMethods returns violation strings for every *Client
// write method in file that omits wrapAuthIfStatusAuth. Each finding is
// formatted as "pkgname.MethodName  file:line  <guidance>" so callers can
// extract the allowlist key (the prefix before the first "  ").
func scanFileForUnwrappedWriteMethods(fset *token.FileSet, info *types.Info, file *ast.File, pkgName, relPath string) []string {
	if info == nil {
		return nil
	}
	var out []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
			continue
		}
		if !wrapAuthIsStarClientReceiver(info, fn) {
			continue
		}
		if !wrapAuthFuncReturnsError(fn) {
			continue
		}
		if !containsWriteIndicator(fn.Body) {
			continue
		}
		if containsWrapAuthCall(fn.Body) {
			continue
		}
		pos := fset.Position(fn.Pos())
		out = append(out, fmt.Sprintf("%s.%s  %s:%d  add wrapAuthIfStatusAuth or add to allowedUnwrappedWriteMethods with rationale",
			pkgName, fn.Name.Name, relPath, pos.Line))
	}
	return out
}

// containsWriteIndicator reports whether body contains a write operation:
// a call to Post, PostJSON, or PutJSON, or a reference to http.MethodPost
// or http.MethodDelete.
func containsWriteIndicator(body *ast.BlockStmt) bool {
	writeCallNames := map[string]bool{"Post": true, "PostJSON": true, "PutJSON": true}
	writeMethodConsts := map[string]bool{"MethodPost": true, "MethodDelete": true}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch expr := n.(type) {
		case *ast.CallExpr:
			if sel, ok := expr.Fun.(*ast.SelectorExpr); ok && writeCallNames[sel.Sel.Name] {
				found = true
				return false
			}
		case *ast.SelectorExpr:
			if writeMethodConsts[expr.Sel.Name] {
				if ident, ok := expr.X.(*ast.Ident); ok && ident.Name == "http" {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// containsWrapAuthCall reports whether body contains a call to wrapAuthIfStatusAuth.
func containsWrapAuthCall(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "wrapAuthIfStatusAuth" {
			found = true
			return false
		}
		return true
	})
	return found
}

// wrapAuthIsStarClientReceiver reports whether fn has a *Client pointer receiver,
// confirmed via the type checker so aliases and embedding do not produce false
// positives.
func wrapAuthIsStarClientReceiver(info *types.Info, fn *ast.FuncDecl) bool {
	t := info.TypeOf(fn.Recv.List[0].Type)
	if t == nil {
		return false
	}
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	if !ok {
		return false
	}
	return named.Obj().Name() == "Client"
}

// wrapAuthFuncReturnsError reports whether fn's return list includes an error type.
func wrapAuthFuncReturnsError(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, field := range fn.Type.Results.List {
		if ident, ok := field.Type.(*ast.Ident); ok && ident.Name == "error" {
			return true
		}
	}
	return false
}

// wrapAuthFindRepoRoot walks up from the test's working directory until it
// finds the repo's go.mod. Stillwater is a single-module repo so the first
// go.mod found walking upward is the repo root.
func wrapAuthFindRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found walking up from %s", wd)
		}
		dir = parent
	}
}

// wrapAuthCountWithSyntax returns the number of packages that loaded with syntax.
// A zero count signals a complete load failure that would silently produce no
// findings; callers treat it as a fatal test setup error.
func wrapAuthCountWithSyntax(pkgs []*packages.Package) int {
	n := 0
	for _, p := range pkgs {
		if len(p.Syntax) > 0 {
			n++
		}
	}
	return n
}

// wrapAuthCollectLoadErrors returns per-package error strings from pkgs,
// plus an entry for any package that loaded syntax but failed to populate
// TypesInfo (which would cause scanFileForUnwrappedWriteMethods to return nil).
func wrapAuthCollectLoadErrors(pkgs []*packages.Package) []string {
	var out []string
	for _, p := range pkgs {
		for _, e := range p.Errors {
			out = append(out, p.PkgPath+": "+e.Error())
		}
		if p.TypesInfo == nil && len(p.Syntax) > 0 {
			out = append(out, p.PkgPath+": syntax loaded but TypesInfo is nil")
		}
	}
	return out
}
