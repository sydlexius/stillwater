package httpsafe_test

// Functional regression guard for raw *http.Client construction in
// production code. Every outbound HTTP path must go through
// httpsafe.SafeClient to enforce the SafeTransport SSRF guard.
//
// Two tests live here:
//
//   1. TestNoRawHTTPClientConstruction walks every production package
//      (Tests=false, so _test.go is skipped) and asserts that no
//      *ast.CompositeLit whose type resolves to net/http.Client appears
//      outside the allowlist below. New construction sites fail the
//      test until they are migrated to httpsafe.SafeClient or
//      explicitly allowlisted; the latter requires a
//      security-review-grade rationale comment in the production code
//      naming the LAN/loopback service.
//
//   2. TestDetectorFiresOnContrivedFixture writes a synthetic Go file
//      containing a raw &http.Client{...} composite literal to a temp
//      directory, runs the same detector against it, and asserts it
//      produces a finding. If the production scan ever stops detecting
//      raw constructions (AST visitor change, types-resolution
//      regression), this test fails alongside the production scan.
//
// The detector matches *ast.CompositeLit nodes only. Alternative
// idioms that bypass the AST visitor (var c http.Client zero-value
// declarations, new(http.Client), dereferenced copies of
// http.DefaultClient) are out of scope: they are not the regression
// class observed in PR #1558 and are vanishingly rare in real Go code.
// If a future regression uses one of those idioms, extend the visitor
// to cover *ast.ValueSpec and *ast.CallExpr(new).

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// allowedRawClientSites lists production file:line pairs where a raw
// http.Client composite literal is intentional. Each entry corresponds
// to a per-site rationale comment in the production file explaining why
// httpsafe.SafeClient is the wrong tool here. The line number is the
// line that contains the `http.Client{` token, not the surrounding
// declaration.
//
// Adding an entry here is a deliberate security decision. Reviewers
// should confirm:
//   - The destination is a user-configured local service (loopback or
//     RFC1918 LAN), not an outbound-to-internet endpoint.
//   - SafeTransport's SSRF guard would reject that destination by
//     design (loopback/private addresses are exactly what it blocks).
//   - The corresponding rationale comment in production code names the
//     service and the LAN/loopback reasoning.
//
// The keys are repo-rooted slash-delimited paths (filepath.ToSlash so
// the test runs identically on Windows-style separators, even though the
// project is Linux/macOS-only today).
var allowedRawClientSites = map[string]bool{
	// Emby connection client: operator-supplied media-server URL,
	// validated via connection.ValidateBaseURL.
	"internal/connection/emby/client.go:41":  true,
	"internal/connection/emby/client.go:299": true,
	// Jellyfin connection client: same pattern as Emby.
	"internal/connection/jellyfin/client.go:41":  true,
	"internal/connection/jellyfin/client.go:303": true,
	// Lidarr connection client: operator-supplied *arr-stack URL.
	"internal/connection/lidarr/client.go:32": true,
	// Auth providers (login backends): operator-supplied media-server
	// URLs, validated via connection.ValidateBaseURL.
	"internal/auth/provider_emby.go:43":     true,
	"internal/auth/provider_jellyfin.go:43": true,
}

// rootDirs is the production-code surface this test walks. The
// production code surface is the four top-level directories that
// contain hand-written Go: cmd/, internal/, tools/, and scripts/. New
// top-level production directories must be added here so a missing
// directory cannot silently hide a regression.
var rootDirs = []string{"cmd", "internal", "tools", "scripts"}

// TestNoRawHTTPClientConstruction walks every production package and
// fails if any *ast.CompositeLit whose type is net/http.Client appears
// outside the allowlist.
func TestNoRawHTTPClientConstruction(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	// packages.Load with full type info is what lets us resolve a
	// composite-literal's type to the canonical net/http.Client even
	// when the import is aliased. Tests=false omits _test.go files
	// (which are explicitly allowed to construct raw clients for
	// fixture wiring per the issue's class-(b) classification).
	patterns := make([]string, 0, len(rootDirs))
	for _, d := range rootDirs {
		patterns = append(patterns, "./"+d+"/...")
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports,
		Dir:   repoRoot,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}
	// Per-package compile/type-check failures land in pkg.Errors, not in
	// the top-level err returned above. If TypesInfo fails to populate
	// for a package, scanFileForRawClient silently skips its files (no
	// types to resolve against) and any &http.Client{} in those files
	// goes undetected. A security guard whose failure mode is "silently
	// stop guarding" is worse than no guard; fail loud instead.
	if loadErrs := collectLoadErrors(pkgs); len(loadErrs) > 0 {
		t.Fatalf("packages.Load reported %d per-package error(s); the scan cannot prove the absence of raw constructions:\n  %s",
			len(loadErrs), strings.Join(loadErrs, "\n  "))
	}
	// A complete load failure would silently report zero findings,
	// masking a real regression. Refuse to pass in that state.
	if countWithSyntax(pkgs) == 0 {
		t.Fatalf("packages.Load returned no packages with syntax; check repo state")
	}

	var findings []string
	for _, pkg := range pkgs {
		// httpsafe defines SafeClient and must be allowed to construct
		// a raw *http.Client.
		if strings.HasSuffix(pkg.PkgPath, "/internal/httpsafe") {
			continue
		}
		for i, file := range pkg.Syntax {
			// pkg.GoFiles is parallel to pkg.Syntax. CompiledGoFiles
			// may include generated files we do not care about; GoFiles
			// is the source-of-truth source list.
			if i >= len(pkg.GoFiles) {
				continue
			}
			path := pkg.GoFiles[i]
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				rel = path
			}
			rel = filepath.ToSlash(rel)
			findings = append(findings, scanFileForRawClient(pkg.Fset, pkg.TypesInfo, file, rel)...)
		}
	}

	// Filter findings through the allowlist. A finding inside the
	// allowlist is the expected/documented state; everything else is a
	// regression. Each finding is "file:line  <message>"; the allowlist
	// keys are just "file:line", so we split on the first space.
	var regressions []string
	for _, f := range findings {
		key := f
		if idx := strings.IndexByte(f, ' '); idx > 0 {
			key = f[:idx]
		}
		if allowedRawClientSites[key] {
			continue
		}
		regressions = append(regressions, f)
	}
	if len(regressions) > 0 {
		sort.Strings(regressions)
		t.Fatalf("found %d raw http.Client construction site(s) outside the allowlist:\n  %s\n"+
			"If this site is a user-configured LAN/loopback service that SafeTransport would reject by design, "+
			"add the file:line to allowedRawClientSites in this test AND add a rationale comment in the "+
			"production code naming the service.",
			len(regressions), strings.Join(regressions, "\n  "))
	}
}

// TestDetectorFiresOnContrivedFixture writes a synthetic Go file
// containing a raw &http.Client{...} composite literal and asserts the
// detector produces a finding. If the production scan ever stops
// detecting raw constructions (AST visitor change, types-resolution
// regression), this test fails alongside the production scan.
func TestDetectorFiresOnContrivedFixture(t *testing.T) {
	const src = `package fixture

import (
	"net/http"
	"time"
)

func makeRawClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}
`

	// Write the fixture to a temp directory with its own minimal go.mod.
	// packages.Load then parses it in a real types-info context (net/http
	// resolves to the real stdlib type, not a stub). A temp module avoids
	// any cross-talk with the host repo's go.mod.
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fixture.go")
	goMod := "module fixture\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(fixturePath, []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports,
		Dir:   dir,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if loadErrs := collectLoadErrors(pkgs); len(loadErrs) > 0 {
		t.Fatalf("fixture packages.Load reported %d per-package error(s):\n  %s",
			len(loadErrs), strings.Join(loadErrs, "\n  "))
	}
	if countWithSyntax(pkgs) == 0 {
		t.Fatalf("fixture failed to load with syntax")
	}

	var findings []string
	for _, pkg := range pkgs {
		for i, file := range pkg.Syntax {
			if i >= len(pkg.GoFiles) {
				continue
			}
			rel := filepath.ToSlash(filepath.Base(pkg.GoFiles[i]))
			findings = append(findings, scanFileForRawClient(pkg.Fset, pkg.TypesInfo, file, rel)...)
		}
	}

	if len(findings) == 0 {
		t.Fatalf("detector did not fire on contrived fixture; the production scan may be broken")
	}
	// The construction is on line 9 of the fixture source above.
	// Asserting the exact line catches off-by-one regressions in
	// fset.Position(cl.Pos()).
	wantPrefix := "fixture.go:9 "
	if !strings.HasPrefix(findings[0], wantPrefix) {
		t.Fatalf("finding does not start with %q: %v", wantPrefix, findings)
	}
}

// scanFileForRawClient is the shared detector. Given a parsed AST file
// and its types info, it returns a slice of "file:line ..." findings
// for every *ast.CompositeLit whose type resolves to net/http.Client.
// Both production and contrived-fixture tests call this function, so
// the contrived fixture genuinely exercises the production detector.
//
// info should never be nil under normal operation; collectLoadErrors
// fails the test before we reach this function if a package's types
// info failed to populate. The nil guard remains defensive: if it ever
// triggers, that is a test setup bug to investigate, not a silent
// no-op.
func scanFileForRawClient(fset *token.FileSet, info *types.Info, file *ast.File, rel string) []string {
	if info == nil {
		return nil
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typ := info.TypeOf(cl)
		if typ == nil {
			return true
		}
		if !isNetHTTPClient(typ) {
			return true
		}
		pos := fset.Position(cl.Pos())
		out = append(out, rel+":"+strconv.Itoa(pos.Line)+" raw &http.Client{} construction; use httpsafe.SafeClient(timeout) instead")
		return true
	})
	return out
}

// isNetHTTPClient reports whether t is the named type net/http.Client.
// Composite literals always carry the named (non-pointer) type, so we
// match on the named type directly. A defensive Unalias keeps the check
// stable if a future Go version introduces aliases at this boundary.
func isNetHTTPClient(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == "net/http" && obj.Name() == "Client"
}

// findRepoRoot walks up from the test's working directory until it
// finds the repo's go.mod. `go test` starts us inside
// internal/httpsafe, so the walk is short.
func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		// We must not confuse a sub-module's go.mod with the repo root.
		// Stillwater is a single-module repo, so the first go.mod we
		// hit walking up IS the root; if Stillwater ever adopts
		// sub-modules, this loop should also check for a sentinel file
		// (e.g. ".git").
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", &noGoModError{start: wd}
		}
		dir = parent
	}
}

type noGoModError struct{ start string }

func (e *noGoModError) Error() string {
	return "go.mod not found walking up from " + e.start
}

// countWithSyntax returns the number of packages that loaded enough
// syntax to inspect. A complete failure to load any package would
// silently report zero findings, so we treat a zero count as a fatal
// test setup error rather than a clean run.
func countWithSyntax(pkgs []*packages.Package) int {
	count := 0
	for _, p := range pkgs {
		if len(p.Syntax) > 0 {
			count++
		}
	}
	return count
}

// collectLoadErrors returns "<pkg>: <err>" entries for every per-package
// error in pkgs, plus an entry for any package that loaded syntax but
// failed to populate TypesInfo (which would silently skip the file in
// scanFileForRawClient). Callers should fail the test loudly when this
// returns a non-empty slice.
func collectLoadErrors(pkgs []*packages.Package) []string {
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
