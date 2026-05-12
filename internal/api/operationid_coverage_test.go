package api

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sydlexius/stillwater/internal/filesystem"
)

// TestOperationIDCoverage asserts that every operationId in openapi.yaml has at
// least one corresponding test invocation in the running test suite.
//
// Approach (static analysis, no runtime instrumentation):
//
//  1. Parse openapi.yaml and collect (method, path) -> operationId for all
//     226-ish operations.
//
//  2. Parse router.go AST. For each mux.HandleFunc / mux.Handle call, extract
//     the route literal ("METHOD path", possibly built from "METHOD " + bp +
//     "/api/v1/..."), and find the first r.handleXxx selector reference in
//     the remaining arguments (peeling middleware wrappers like wrapAuth,
//     requireMultiUser, RequireAdmin, loginRL.Middleware). The result is a
//     (method, path) -> handlerFuncName map.
//
//  3. Parse every *_test.go file in this package and collect the set of
//     handler function names referenced (any selector .handleXxx).
//
//  4. An operationId is "covered" iff its (method, path) maps to a handler
//     name in the test-reference set.
//
// Rationale for static analysis over runtime instrumentation:
//   - Existing tests call handlers directly (e.g. r.handleLogout(rec, req))
//     rather than going through the registered mux. A runtime counter would
//     require modifying every test file to route through a shared wrapper,
//     which is out of scope for this PR and would not survive parallel work
//     by sibling M49 W5 agents.
//   - Static analysis is deterministic, fast, and stable across test
//     ordering and parallel test execution.
//   - The signal is binary (referenced / not referenced) rather than
//     hit-count, which is acceptable for a coverage gate.
//
// Baseline gating:
//
// Because sibling M49 W5 agents are landing tests for handler files in
// parallel, this PR establishes a BASELINE allow-list of currently-uncovered
// operationIds at testdata/openapi-coverage-baseline.json. The test fails
// only when a NEW gap appears (an operationId is uncovered AND not in the
// baseline). When tests are added that close a baseline gap, the test also
// fails, prompting the contributor to shrink the baseline (one-way ratchet).
//
// The summary artifact at testdata/openapi-coverage.json is regenerated on
// every test run (per AC) and lists every operationId with its (method,
// path), handler, and covered-bool flag.
func TestOperationIDCoverage(t *testing.T) {
	t.Parallel()

	ops, err := loadSpecOperations("openapi.yaml")
	if err != nil {
		t.Fatalf("loading openapi.yaml: %v", err)
	}

	routes, err := loadRouterBindings("router.go")
	if err != nil {
		t.Fatalf("loading router.go: %v", err)
	}

	testRefs, err := loadTestHandlerRefs(".")
	if err != nil {
		t.Fatalf("loading test handler refs: %v", err)
	}

	// Compute coverage per operationId.
	type covEntry struct {
		OperationID string `json:"operationId"`
		Method      string `json:"method"`
		Path        string `json:"path"`
		Handler     string `json:"handler"` // empty if no route binding found
		Covered     bool   `json:"covered"`
	}

	entries := make([]covEntry, 0, len(ops))
	for _, op := range ops {
		// Spec paths are bare ("/artists/{id}"); router paths include the
		// "/api/v1" prefix that matches the spec's server base URL.
		routeKey := op.Method + " " + "/api/v1" + op.Path
		handler := routes[routeKey]
		covered := handler != "" && testRefs[handler]
		entries = append(entries, covEntry{
			OperationID: op.OperationID,
			Method:      op.Method,
			Path:        op.Path,
			Handler:     handler,
			Covered:     covered,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].OperationID < entries[j].OperationID
	})

	// Write the per-operationId summary artifact.
	if err := writeCoverageSummary(entries); err != nil {
		t.Errorf("writing coverage summary: %v", err)
	}

	// Build the current uncovered set and compare against the baseline.
	currentUncovered := make(map[string]bool)
	var unmappedOps []string
	for _, e := range entries {
		if !e.Covered {
			currentUncovered[e.OperationID] = true
		}
		if e.Handler == "" {
			unmappedOps = append(unmappedOps, fmt.Sprintf("%s %s (%s)", e.Method, e.Path, e.OperationID))
		}
	}

	// If any operationId has no router binding at all, that is a spec/router
	// mismatch and is always an error (regardless of baseline).
	if len(unmappedOps) > 0 {
		sort.Strings(unmappedOps)
		t.Errorf("operationIds without a matching route in router.go (%d):\n  %s",
			len(unmappedOps), strings.Join(unmappedOps, "\n  "))
	}

	baseline, err := loadBaseline("testdata/openapi-coverage-baseline.json")
	if err != nil {
		t.Fatalf("loading baseline: %v", err)
	}

	// New gaps: uncovered ops NOT in the baseline.
	var newGaps []string
	for opID := range currentUncovered {
		if !baseline[opID] {
			newGaps = append(newGaps, opID)
		}
	}
	// Stale baseline entries: ops in baseline but now covered. Surface these
	// so contributors shrink the baseline as coverage improves.
	var stale []string
	for opID := range baseline {
		if !currentUncovered[opID] {
			stale = append(stale, opID)
		}
	}

	if len(newGaps) > 0 {
		sort.Strings(newGaps)
		t.Errorf("new operationIds without test invocation (%d). Add a handler test or, if intentional, append to testdata/openapi-coverage-baseline.json:\n  %s",
			len(newGaps), strings.Join(newGaps, "\n  "))
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("baseline contains %d operationIds that are NOW covered. Remove them from testdata/openapi-coverage-baseline.json to keep the ratchet tight:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// specOperation is a single (method, path, operationId) triple from the spec.
type specOperation struct {
	Method      string
	Path        string
	OperationID string
}

// loadSpecOperations parses openapi.yaml and returns every operation.
func loadSpecOperations(path string) ([]specOperation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}

	// Standard HTTP methods that appear in OpenAPI path items. Anything else
	// (parameters, summary, etc.) is filtered by checking against this set.
	httpMethods := map[string]bool{
		"get": true, "post": true, "put": true, "patch": true,
		"delete": true, "head": true, "options": true, "trace": true,
	}

	var ops []specOperation
	for p, methods := range doc.Paths {
		for m, op := range methods {
			if !httpMethods[strings.ToLower(m)] {
				continue
			}
			if op.OperationID == "" {
				continue
			}
			ops = append(ops, specOperation{
				Method:      strings.ToUpper(m),
				Path:        p,
				OperationID: op.OperationID,
			})
		}
	}
	return ops, nil
}

// loadRouterBindings parses router.go and extracts (method+" "+path) ->
// handler-func-name for every mux.HandleFunc / mux.Handle call whose first
// argument is a string-literal route pattern (possibly built with
// "METHOD " + bp + "/path" concatenation).
func loadRouterBindings(path string) (map[string]string, error) {
	fset := token.NewFileSet()
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}

	bindings := make(map[string]string)
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "mux" {
			return true
		}
		if sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle" {
			return true
		}
		if len(call.Args) < 2 {
			return true
		}

		// First arg: route pattern. Accept either a single string literal or
		// a "METHOD " + bp + "/path" concatenation chain.
		pattern, ok := evalRoutePattern(call.Args[0])
		if !ok {
			return true
		}
		// Pattern is e.g. "GET /api/v1/health" or "GET /". Strip blank-prefix
		// (catch-all) cases by requiring a space-separated method token.
		method, route, ok := splitRoutePattern(pattern)
		if !ok {
			return true
		}

		// Remaining args: search recursively for any selector with name
		// matching ^handle[A-Z]. The first such selector is the bound
		// handler. This unpeels wrapAuth(), requireMultiUser(),
		// middleware.RequireAdmin(), loginRL.Middleware(http.HandlerFunc(...)),
		// etc., without enumerating every wrapper.
		handler := findHandlerSelector(call.Args[1:])
		if handler == "" {
			return true
		}

		bindings[method+" "+route] = handler
		return true
	})

	return bindings, nil
}

// evalRoutePattern evaluates a route-pattern expression that is either a
// single STRING literal or a left-associative + concatenation chain of
// string literals and the bp identifier. Returns the literal string with bp
// replaced by "" (representing the empty default base path used in tests).
//
// Supported forms:
//   - "GET /api/v1/health"
//   - "GET " + bp + "/api/v1/health"
//   - bp + "/static/"
//   - bp + "/{path...}"
func evalRoutePattern(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.Ident:
		// bare bp identifier means empty base path.
		if e.Name == "bp" {
			return "", true
		}
		return "", false
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := evalRoutePattern(e.X)
		if !ok {
			return "", false
		}
		right, ok := evalRoutePattern(e.Y)
		if !ok {
			return "", false
		}
		return left + right, true
	}
	return "", false
}

// splitRoutePattern splits "METHOD /path" into method and path. Returns
// false for non-method-prefixed patterns (e.g. catch-all "/" without a
// method, used by handle404 and similar).
func splitRoutePattern(s string) (string, string, bool) {
	idx := strings.IndexByte(s, ' ')
	if idx < 0 {
		return "", "", false
	}
	method := s[:idx]
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE":
		return method, s[idx+1:], true
	}
	return "", "", false
}

// findHandlerSelector walks a slice of expressions and returns the .Sel.Name
// of the first selector expression whose name starts with "handle" followed
// by an uppercase letter. Recurses into call expressions to peel wrappers.
func findHandlerSelector(exprs []ast.Expr) string {
	var found string
	for _, e := range exprs {
		ast.Inspect(e, func(n ast.Node) bool {
			if found != "" {
				return false
			}
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := sel.Sel.Name
			if strings.HasPrefix(name, "handle") && len(name) > len("handle") {
				c := name[len("handle")]
				if c >= 'A' && c <= 'Z' {
					found = name
					return false
				}
			}
			return true
		})
		if found != "" {
			return found
		}
	}
	return ""
}

// loadTestHandlerRefs scans every *_test.go file in the given dir and
// returns the set of handler-function names referenced (via selector
// expressions or bare identifiers). A reference is any name starting with
// "handle" + uppercase letter.
func loadTestHandlerRefs(dir string) (map[string]bool, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, err
	}

	refs := make(map[string]bool)
	fset := token.NewFileSet()
	for _, path := range matches {
		// Skip this file: its references to handler names in string-literal
		// comments or test scaffolding should not count as "tests."
		if filepath.Base(path) == "operationid_coverage_test.go" {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return nil, err
		}
		// Count only handler INVOCATIONS, not bare identifier references.
		// A bare ast.Ident match would also flag the handler's own func
		// declaration site (and any test helper that takes handleXxx as a
		// value), falsely marking it covered. Walking only *ast.CallExpr
		// limits the signal to "a test actually calls this handler."
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if isHandlerName(fn.Sel.Name) {
					refs[fn.Sel.Name] = true
				}
			case *ast.Ident:
				if isHandlerName(fn.Name) {
					refs[fn.Name] = true
				}
			}
			return true
		})
	}
	return refs, nil
}

// isHandlerName returns true for identifiers matching ^handle[A-Z].
func isHandlerName(name string) bool {
	if !strings.HasPrefix(name, "handle") || len(name) <= len("handle") {
		return false
	}
	c := name[len("handle")]
	return c >= 'A' && c <= 'Z'
}

// writeCoverageSummary writes the per-operationId coverage artifact to
// testdata/openapi-coverage.json. The file is sorted by operationId and is
// regenerated on every test run.
func writeCoverageSummary(entries any) error {
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// Use the repo's atomic-write helper (tmp/bak/rename) so an
	// interrupted run never leaves a partial JSON artifact on disk.
	// Matches the convention enforced everywhere else in the codebase.
	return filesystem.WriteFileAtomic("testdata/openapi-coverage.json", data, 0o644)
}

// loadBaseline reads the baseline allow-list of currently-uncovered
// operationIds. The file format is a JSON array of strings. Missing file
// is treated as empty baseline (strict mode).
func loadBaseline(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(list))
	for _, s := range list {
		set[s] = true
	}
	return set, nil
}
