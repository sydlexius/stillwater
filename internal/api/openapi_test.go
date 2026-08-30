package api

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"gopkg.in/yaml.v3"
)

// TestOpenAPIConsistency loads the internal/api package with full type
// information, finds every call to writeJSON, and extracts the JSON field
// names its response-body argument can produce, then verifies each of those
// fields exists somewhere in the OpenAPI spec.
//
// Two argument shapes are understood:
//
//  1. A map[string]T{...} literal written at the call site: field names are
//     the literal string keys.
//  2. A named-struct literal written at the call site, e.g.
//     writeJSON(w, http.StatusOK, blastRadiusResponse{...}). Its STATIC TYPE
//     is resolved via go/types (golang.org/x/tools/go/packages, so a field
//     type declared in another package -- e.g. artist.BlastRadiusCounts
//     embedded in blastRadiusResponse -- is followed too), walking through
//     structs, embedded fields, pointers, slices, arrays and map values,
//     recursively, to collect every exported/promoted JSON field name the
//     struct can put in the response.
//
// This catches the most common form of spec drift: a developer adds a
// response field -- whether directly in a handler's map literal, or on a
// domain struct that a handler's response envelope embeds -- and forgets to
// update openapi.yaml.
//
// Limitations, deliberately scoped to keep the false-positive rate at zero:
//   - Only literal string keys in map composites are seen; dynamic keys
//     (variables, concatenation) are invisible to AST/type analysis.
//   - Only a response argument that is ITSELF a composite literal written at
//     the writeJSON call site is resolved. A bare variable, a function call
//     result, or a pointer expression (writeJSON(w, status, someVar)) is
//     NOT followed through its type -- that would require proving the
//     variable's dynamic value at the call site is exhaustively covered by
//     its static type's fields, which false-positives immediately on the
//     ~100 existing pass-through call sites in this codebase (e.g.
//     `writeJSON(w, http.StatusCreated, invite)`) that were never audited
//     against the spec and are out of scope for this fix. A handler that
//     wants its response type checked should construct the envelope inline
//     at the call site, which is already the prevailing style for the
//     handlers with a named response struct.
//   - Does not map handlers to specific endpoints. A field is considered
//     present if it appears in ANY schema in the spec. This avoids false
//     positives from route-mapping complexity but may miss cases where a
//     field exists in the wrong schema.
//   - Does not evaluate custom MarshalJSON methods, so a type whose JSON
//     shape diverges from its Go field layout (a hand-written MarshalJSON)
//     is not modeled correctly -- such a type would need a comment here if
//     it starts producing false positives.
func TestOpenAPIConsistency(t *testing.T) {
	t.Parallel()
	specFields, err := collectSpecFields("openapi.yaml")
	if err != nil {
		t.Fatalf("parsing openapi.yaml: %v", err)
	}

	handlerFields, err := collectHandlerFields(".")
	if err != nil {
		t.Fatalf("parsing handler files: %v", err)
	}

	// Common fields used in many handlers that are excluded from the
	// consistency check: "error" and "status" are covered by the Error
	// and Status schemas; "message" appears in domain schemas
	// (LibraryOpResult, Rule) and ad-hoc success responses.
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

// collectHandlerFields loads dir as a Go package with full type information
// (via golang.org/x/tools/go/packages, so declarations in OTHER packages --
// e.g. internal/artist -- are resolved too), finds every call to writeJSON
// in its production (non-test) source, and extracts every JSON field name
// that can reach the response body: literal string keys for map arguments,
// and every exported/promoted struct field (recursively) for anything with
// a static Go type.
func collectHandlerFields(dir string) (map[string][]string, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps,
		Dir:   dir,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		return nil, err
	}

	var loadErrs []string
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	}
	if len(loadErrs) > 0 {
		return nil, fmt.Errorf("packages.Load reported %d error(s):\n  %s", len(loadErrs), strings.Join(loadErrs, "\n  "))
	}

	fields := make(map[string][]string) // field name -> list of "file:line" locations
	seen := make(map[types.Type]bool)   // dedup/cycle guard for the type walk

	for _, pkg := range pkgs {
		if pkg.Fset == nil || pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			collectWriteJSONFields(pkg.Fset, pkg.TypesInfo, file, fields, seen)
		}
	}

	return fields, nil
}

// collectWriteJSONFields walks file looking for calls to writeJSON and
// records every JSON field name reachable from the response-body argument
// (the 3rd argument) into fields.
func collectWriteJSONFields(fset *token.FileSet, info *types.Info, file *ast.File, fields map[string][]string, seen map[types.Type]bool) {
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

		arg := call.Args[2]

		comp, ok := arg.(*ast.CompositeLit)
		if !ok {
			// Not a literal constructed at the call site -- a bare variable,
			// a function call result, a pointer expression. Out of scope:
			// see the doc comment on collectHandlerFields.
			return true
		}

		if isMapStringKeyed(comp) {
			// A map[string]T{...} literal: field names are the literal
			// string keys, which are DATA, not part of the static type, so
			// the AST is the only place to find them.
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
		}

		// A named-struct composite literal constructed directly at the
		// call site, e.g. writeJSON(w, http.StatusOK, blastRadiusResponse{
		// ...}). This is the case the old scanner silently skipped ("could
		// be a struct variable or pointer -- skip"): the response envelope
		// IS visible as source text in internal/api, but its field names
		// were never extracted, and following its type resolves embedded
		// fields from other packages (e.g. artist.BlastRadiusCounts) too.
		walkFieldType(info.TypeOf(arg), fset, seen, fields)

		return true
	})
}

// walkFieldType recursively resolves t, recording every exported/promoted
// JSON field name it can reach into fields. It follows pointers, slices,
// arrays and map values, and for structs, every exported field -- embedded
// fields are followed without emitting their own name (matching
// encoding/json's promotion rules), and every other field is both recorded
// and followed (so a field like Counts artist.BlastRadiusCounts contributes
// "counts" AND every field nested inside BlastRadiusCounts).
//
// seen guards against revisiting the same type twice (efficiency) and
// against infinite recursion on a self-referential type.
func walkFieldType(t types.Type, fset *token.FileSet, seen map[types.Type]bool, fields map[string][]string) {
	if t == nil || seen[t] {
		return
	}
	seen[t] = true

	switch u := t.Underlying().(type) {
	case *types.Pointer:
		walkFieldType(u.Elem(), fset, seen, fields)
	case *types.Slice:
		walkFieldType(u.Elem(), fset, seen, fields)
	case *types.Array:
		walkFieldType(u.Elem(), fset, seen, fields)
	case *types.Map:
		walkFieldType(u.Elem(), fset, seen, fields)
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			f := u.Field(i)
			if !f.Exported() {
				continue
			}

			tag := reflect.StructTag(u.Tag(i))
			jsonTag := tag.Get("json")
			name := f.Name()
			omitted := false
			renamed := false
			if jsonTag != "" {
				parts := strings.Split(jsonTag, ",")
				if parts[0] == "-" && len(parts) == 1 {
					omitted = true
				} else if parts[0] != "" {
					name = parts[0]
					renamed = true
				}
			}

			if omitted {
				continue
			}

			// An anonymous (embedded) field with no explicit JSON name is
			// PROMOTED: encoding/json splices its exported fields into the
			// parent, so it never appears under its own key.
			if f.Embedded() && !renamed {
				walkFieldType(f.Type(), fset, seen, fields)
				continue
			}

			pos := fset.Position(f.Pos())
			location := filepath.Base(pos.Filename) + ":" + strconv.Itoa(pos.Line)
			fields[name] = append(fields[name], location)

			// Keep walking: this field's own type may itself be a struct
			// (or a slice/pointer/map to one) with fields that also reach
			// the response body.
			walkFieldType(f.Type(), fset, seen, fields)
		}
	default:
		// Basic types, interfaces, channels, funcs: nothing further to walk.
	}
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

// TestCollectHandlerFieldsSeesFieldDeclaredOutsidePackage is the mutation
// proof for #3110: it builds a synthetic two-package module -- a "domain"
// package holding a response struct (mirroring internal/artist), and an
// "api" package whose handler embeds that domain struct in a writeJSON call
// (mirroring internal/api/handlers_blast_radius.go embedding
// artist.BlastRadiusCounts) -- and asserts that collectHandlerFields, run
// against the api package, reports the field the DOMAIN package declared.
//
// Before the #3110 fix, collectHandlerFields only regex-globbed
// handlers*.go in the target directory and only understood map[string]T{}
// literals; a struct-literal writeJSON argument was skipped entirely
// ("could be a struct variable or pointer -- skip"), so a field declared on
// an embedded domain struct was invisible no matter which package declared
// it. This test fails on that old behavior and passes on the new one --
// see PR history for the red run.
func TestCollectHandlerFieldsSeesFieldDeclaredOutsidePackage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	domainDir := filepath.Join(dir, "domain")
	if err := os.MkdirAll(domainDir, 0o750); err != nil {
		t.Fatalf("mkdir domain: %v", err)
	}
	const domainSrc = `package domain

// Counts is declared OUTSIDE the api package, mirroring
// artist.BlastRadiusCounts in the real codebase.
type Counts struct {
	// FieldFromAnotherPackage is the field under test: it must be seen even
	// though it never appears as source text in the api package.
	FieldFromAnotherPackage int ` + "`json:\"field_from_another_package\"`" + `
}
`
	if err := os.WriteFile(filepath.Join(domainDir, "domain.go"), []byte(domainSrc), 0o600); err != nil {
		t.Fatalf("write domain.go: %v", err)
	}

	apiDir := filepath.Join(dir, "api")
	if err := os.MkdirAll(apiDir, 0o750); err != nil {
		t.Fatalf("mkdir api: %v", err)
	}
	const apiSrc = `package api

import "fixture/domain"

func writeJSON(w int, status int, v any) {}

type response struct {
	Counts domain.Counts ` + "`json:\"counts\"`" + `
}

func handle() {
	writeJSON(0, 200, response{})
}
`
	if err := os.WriteFile(filepath.Join(apiDir, "handlers.go"), []byte(apiSrc), 0o600); err != nil {
		t.Fatalf("write handlers.go: %v", err)
	}

	fields, err := collectHandlerFields(apiDir)
	if err != nil {
		t.Fatalf("collectHandlerFields: %v", err)
	}

	// Precondition: the promoted local field is seen at all, proving the
	// walk reached the response struct in the first place.
	if _, ok := fields["counts"]; !ok {
		t.Fatalf("expected to see local field %q; got %v", "counts", fields)
	}

	// The actual assertion: the field declared in the SEPARATE domain
	// package, nested inside Counts, is seen too.
	if _, ok := fields["field_from_another_package"]; !ok {
		t.Fatalf("collectHandlerFields did not see field %q declared outside the api package; got fields: %v",
			"field_from_another_package", fields)
	}
}
