package api

import (
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
)

// TestOpenAPIConsistency parses handler source files to extract response field
// names from writeJSON calls using map literal arguments, then verifies those
// fields exist somewhere in the OpenAPI spec. This catches the most common form
// of spec drift: a developer adds a response field to a handler but forgets to
// update openapi.yaml.
//
// Limitations:
//   - Only detects literal string keys in map composites passed to writeJSON.
//     Dynamic keys (variables, concatenation) are invisible to AST analysis.
//   - Does not map handlers to specific endpoints. A field is considered
//     present if it appears in ANY schema in the spec. This avoids false
//     positives from route-mapping complexity but may miss cases where a
//     field exists in the wrong schema.
func TestOpenAPIConsistency(t *testing.T) {
	specFields, err := collectSpecFields("openapi.yaml")
	if err != nil {
		t.Fatalf("parsing openapi.yaml: %v", err)
	}

	handlerFields, err := collectHandlerFields(".")
	if err != nil {
		t.Fatalf("parsing handler files: %v", err)
	}

	// Common fields used in many handlers that are implicitly covered by
	// generic schemas (Error, Status) or are wrapper keys for lists.
	wellKnown := map[string]bool{
		"error":   true,
		"status":  true,
		"message": true,
	}

	var missing []string
	for field, locations := range handlerFields {
		if wellKnown[field] {
			continue
		}
		if !specFields[field] {
			missing = append(missing, field+" ("+strings.Join(locations, ", ")+")")
		}
	}

	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("handler response fields not found in openapi.yaml (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// collectSpecFields parses openapi.yaml and returns a set of all property
// names found under any "properties" mapping in the document. This includes
// both response and request schemas. The broader traversal avoids false
// positives from shared schemas referenced by both request and response
// definitions. The trade-off is that a field existing only in a request
// schema could mask a missing response schema entry, but this is rare in
// practice and acceptable given the test's primary goal of catching
// obviously missing fields.
func collectSpecFields(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}

	fields := make(map[string]bool)
	extractProperties(doc, fields)
	return fields, nil
}

// extractProperties recursively walks a YAML document collecting all keys
// found under any "properties" mapping.
func extractProperties(node any, fields map[string]bool) {
	switch v := node.(type) {
	case map[string]any:
		if props, ok := v["properties"]; ok {
			if propMap, ok := props.(map[string]any); ok {
				for key := range propMap {
					fields[key] = true
				}
			}
		}
		for _, child := range v {
			extractProperties(child, fields)
		}
	case []any:
		for _, item := range v {
			extractProperties(item, fields)
		}
	}
}

// collectHandlerFields parses all handler source files (excluding tests) and
// extracts string literal keys from map composite literals that appear as
// arguments to writeJSON calls. Scans both handlers.go and handlers_*.go.
func collectHandlerFields(dir string) (map[string][]string, error) {
	patterns := []string{
		filepath.Join(dir, "handlers.go"),
		filepath.Join(dir, "handlers_*.go"),
	}

	var matches []string
	for _, p := range patterns {
		m, err := filepath.Glob(p)
		if err != nil {
			return nil, err
		}
		matches = append(matches, m...)
	}

	fields := make(map[string][]string) // field name -> list of "file:line" locations

	fset := token.NewFileSet()
	for _, path := range matches {
		// Skip test files.
		if strings.HasSuffix(path, "_test.go") {
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

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			if !isWriteJSONCall(call) {
				return true
			}

			// The response body is the 3rd argument (index 2).
			if len(call.Args) < 3 {
				return true
			}

			comp, ok := call.Args[2].(*ast.CompositeLit)
			if !ok {
				// Could be a struct variable or pointer -- skip, we only
				// catch literal map responses here.
				return true
			}

			if !isMapStringKeyed(comp) {
				return true
			}

			for _, elt := range comp.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				lit, ok := kv.Key.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				key, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				pos := fset.Position(lit.Pos())
				location := filepath.Base(pos.Filename) + ":" + strconv.Itoa(pos.Line)
				fields[key] = append(fields[key], location)
			}

			return true
		})
	}

	return fields, nil
}

// isWriteJSONCall checks if a call expression is a call to writeJSON.
func isWriteJSONCall(call *ast.CallExpr) bool {
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == "writeJSON"
}

// isMapStringKeyed checks if a composite literal's type is a map with string
// keys (map[string]any, map[string]string, map[string]int, etc.).
func isMapStringKeyed(comp *ast.CompositeLit) bool {
	mt, ok := comp.Type.(*ast.MapType)
	if !ok {
		return false
	}

	keyIdent, ok := mt.Key.(*ast.Ident)
	return ok && keyIdent.Name == "string"
}
