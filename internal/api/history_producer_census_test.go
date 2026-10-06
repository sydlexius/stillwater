package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// #3078: a census of every call site that can write a metadata_changes row.
// Adding one without deciding what supplies its value (its producer) fails
// TestHistoryWriteSitesAreClassified. Same tripwire shape as
// TestFanartSaveHasASingleChokepoint: it is a tripwire on the call shape, not a
// proof. It cannot see a write reached through a function value or an
// interface; the runtime WARN in artist.resolveProducerForWrite is the other
// half for those.

// historyWriteVerbs are the artist.Service methods that record history, plus
// HistoryService.Record (receiver historyService, or h), the only exported
// HistoryService method that inserts a row. Its non-test callers outside
// internal/artist are handlers_identify.go and rule/history_source.go.
var historyWriteVerbs = map[string]bool{
	"Record": true,
	"Update": true, "UpdateField": true, "ClearField": true,
	"UpdateReportingLocks": true, "UpdateAfterRuleEvaluation": true,
	"UpdateAfterRuleEvaluationReportingLocks": true,
	"UpdateNameGuarded":                       true, "RestoreLockedFieldGuarded": true,
}

// producerStampers are the calls that put a producer on a context. A site
// classified "stamped" must feed a stamped ctx to EVERY write call.
var producerStampers = map[string]bool{
	"ContextWithProducer": true, "ContextWithFieldProducers": true,
	"withRuleHistorySource": true, "refreshProducerContext": true,
}

type siteClass struct {
	kind   string // stamped | caller | none
	reason string
}

// historySiteClasses is keyed "<dir>/<file>::<func>". "stamped" = the function
// stamps its own ctx. "caller" = the producer comes from the caller's ctx.
// "none" = deliberately unstamped, reason says why.
var historySiteClasses = map[string]siteClass{
	"api/handlers_identify.go::recordIdentityHistory":              {"stamped", "direct HistoryService.Record; identityProducer(source)"},
	"rule/history_source.go::recordRuleHistory":                    {"stamped", "direct HistoryService.Record; withRuleHistorySource mirrors rule:<id>"},
	"api/handlers_refresh.go::handleRefreshLink":                   {"stamped", "operator-supplied IDs"},
	"api/handlers_refresh.go::executeRefreshCtx":                   {"stamped", "per-field provider:<name>"},
	"api/handlers_refresh.go::applyProviderName":                   {"stamped", "provider: bare prefix"},
	"api/handlers_identify.go::autoLinkAndRefresh":                 {"caller", "MBID only; inherits the caller's ctx"},
	"api/handlers_connection_library.go::populateFromEmbyCtx":      {"stamped", "platform:emby MBID backfill"},
	"api/handlers_connection_library.go::populateFromJellyfinCtx":  {"stamped", "platform:jellyfin MBID backfill"},
	"api/handlers_image.go::persistImageFlag":                      {"stamped", "filesystem-observed flag"},
	"api/handlers_image.go::updateArtistFanartCount":               {"stamped", "filesystem-observed count"},
	"api/handlers_notifications.go::handleApplyViolationCandidate": {"none", "image flags and health score only; attributes no value"},
	"api/handlers_rule.go::handleEvaluateArtist":                   {"none", "health score only; attributes no value"},
	"api/handlers_platform_state.go::handlePullMetadata":           {"stamped", "platform:<type>"},
	"api/handlers_platform_state.go::pullGenres":                   {"caller", "reqCtx carries handlePullMetadata's platform producer"},
	"api/handlers_field.go::handleFieldUpdate":                     {"stamped", "allow-listed client claim"},
	"api/handlers_field.go::handleFieldClear":                      {"stamped", "operator"},
	"api/handlers_history.go::performRevert":                       {"stamped", "restore"},
	"scanner/scanner.go::processExistingArtist":                    {"stamped", "nfo per field"},
	"maintenance/lock_damage_repair.go::attemptLockDamageRestore":  {"caller", "RestoreLockedFieldGuarded stamps restore itself"},
	"rule/bulk_executor.go::fetchMetadata":                         {"stamped", "rule:bulk_fetch_metadata"},
	"rule/bulk_executor.go::fetchImages":                           {"stamped", "rule:bulk_fetch_images"},
	"rule/bulk_executor.go::selfHealMBID":                          {"stamped", "bulk MBID self-heal source"},
	"rule/fanart_repair.go::remediateOneArtistFanart":              {"stamped", "rule:image_duplicate_exact"},
	"rule/phash_repair.go::remediateArtistPHash":                   {"stamped", "phash remediate source"},
	"rule/phash_repair.go::RestorePHashQuarantine":                 {"stamped", "phash restore source"},
	"rule/fixer.go::FixViolation":                                  {"caller", "ctx tagged by the run path before the shared persist"},
	"rule/fixer.go::persistIncompleteFix":                          {"stamped", "rule:<id> of the incomplete fix"},
	"rule/fixer.go::persistHealthAfterRun":                         {"caller", "run-path ctx tagged with the historySource it is passed"},
	"rule/fixer.go::updateHealthScore":                             {"caller", "run-path ctx tagged with the historySource it is passed"},
	"rule/helpers.go::EvaluateAndPersistHealth":                    {"none", "health score only; attributes no value"},
}

// censusSites returns "<dir>/<file>::<func>" -> verbs called, for every
// history-writing call in the non-test files of dirs. stampers maps the same
// keys to whether the function body contains a producer-stamping call.
func censusSites(t *testing.T, fset *token.FileSet, dirs map[string]string) (sites map[string][]string, stamps map[string]bool) {
	t.Helper()
	sites, stamps = map[string][]string{}, map[string]bool{}
	for name, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("census read no files in %s (err=%v): it would pass vacuously", dir, err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			collectSites(f, name+"/"+filepath.Base(path), sites, stamps)
		}
	}
	return sites, stamps
}

func collectSites(f *ast.File, file string, sites map[string][]string, stamps map[string]bool) {
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		key := file + "::" + fn.Name.Name
		// ONE source-order traversal: fed[id] says whether id currently holds a
		// stamped ctx, updated at EVERY assignment, so each write sees the most
		// recent assignment before it. stamps[key] = EVERY write's ctx argument
		// is a stamper call or such an identifier; a stamper merely present in
		// the function, or whose result is discarded, does not count.
		fed := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
				for k, rhs := range as.Rhs {
					if id, ok := as.Lhs[k].(*ast.Ident); ok {
						fed[id.Name] = isStamper(rhs) || wrapsFed(rhs, fed)
					}
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !historyWriteVerbs[sel.Sel.Name] || !receiverIsArtistService(sel.X) {
				return true
			}
			sites[key] = append(sites[key], sel.Sel.Name)
			good := isStamper(call.Args[0])
			if id, isID := call.Args[0].(*ast.Ident); isID && fed[id.Name] {
				good = true
			}
			if prev, seen := stamps[key]; seen {
				good = good && prev
			}
			stamps[key] = good
			return true
		})
	}
}

// wrapsFed reports whether e is a ctx-wrapping call (artist.ContextWithSource,
// ContextWithHistoryID...) whose first argument is an already-stamped ident, so
// stamping then wrapping keeps the ctx stamped.
func wrapsFed(e ast.Expr, fed map[string]bool) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	id, ok := call.Args[0].(*ast.Ident)
	return ok && fed[id.Name]
}

// isStamper reports whether e is a call to one of producerStampers.
func isStamper(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return producerStampers[fun.Sel.Name]
	case *ast.Ident:
		return producerStampers[fun.Name]
	}
	return false
}

// receiverIsArtistService matches x.artistService and the rule package's
// `svc` parameter, the two spellings the artist.Service takes at call sites.
func receiverIsArtistService(x ast.Expr) bool {
	switch e := x.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name == "artistService" || e.Sel.Name == "historyService"
	case *ast.Ident:
		return e.Name == "svc" || e.Name == "h"
	}
	return false
}

// censusProblems compares discovered sites to the table.
func censusProblems(sites map[string][]string, stamps map[string]bool, table map[string]siteClass) []string {
	var out []string
	for key := range sites {
		c, ok := table[key]
		switch {
		case !ok:
			out = append(out, key+": UNCLASSIFIED history write site; add it to historySiteClasses with a producer decision")
		case c.kind == "stamped" && !stamps[key]:
			out = append(out, key+": classified stamped but a write call ctx is not fed by a producer stamp")
		case c.reason == "":
			out = append(out, key+": classification has no reason")
		}
	}
	for key := range table {
		if _, ok := sites[key]; !ok {
			out = append(out, key+": STALE classification, no history write call site found")
		}
	}
	sort.Strings(out)
	return out
}

func TestHistoryWriteSitesAreClassified(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	sites, stamps := censusSites(t, fset, map[string]string{
		"api": ".", "rule": "../rule", "scanner": "../scanner", "maintenance": "../maintenance",
	})
	if len(sites) < 10 {
		t.Fatalf("census found only %d sites; the matcher is broken", len(sites))
	}
	if p := censusProblems(sites, stamps, historySiteClasses); len(p) > 0 {
		t.Errorf("history write census failed:\n  %s", strings.Join(p, "\n  "))
	}
}

// TestHistoryCensusFlagsAnUnclassifiedSite proves the census has teeth: a new
// call site absent from the table, and a "stamped" site that stamps nothing,
// are both reported.
func TestHistoryCensusFlagsAnUnclassifiedSite(t *testing.T) {
	t.Parallel()
	const src = `package api
func (r *Router) newHandler() { r.artistService.UpdateField(ctx, "a", "biography", "v") }
func (r *Router) sloppy()     { r.artistService.Update(ctx, a) }
`
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites, stamps := map[string][]string{}, map[string]bool{}
	collectSites(f, "api/fixture.go", sites, stamps)
	if len(sites) != 2 {
		t.Fatalf("precondition: fixture should yield 2 sites, got %v", sites)
	}
	table := map[string]siteClass{"api/fixture.go::sloppy": {"stamped", "claims a stamp it lacks"}}
	got := strings.Join(censusProblems(sites, stamps, table), "\n")
	for _, want := range []string{"newHandler: UNCLASSIFIED", "sloppy: classified stamped but"} {
		if !strings.Contains(got, want) {
			t.Errorf("census output missing %q; got:\n%s", want, got)
		}
	}
}

// TestHistoryCensusStampMustFeedTheWrite: a stamper present in the function is
// not enough; its result must reach every write call's ctx.
func TestHistoryCensusStampMustFeedTheWrite(t *testing.T) {
	t.Parallel()
	const src = `package api
func (r *Router) discarded() {
	_ = artist.ContextWithProducer(ctx, "x")
	r.artistService.Update(ctx, a)
}
func (r *Router) half() {
	c := artist.ContextWithProducer(ctx, "x")
	r.artistService.UpdateField(c, "a", "f", "v")
	r.artistService.Update(ctx, a)
}
func (r *Router) good() {
	c := artist.ContextWithProducer(ctx, "x")
	r.artistService.UpdateField(c, "a", "f", "v")
	r.artistService.Update(artist.ContextWithProducer(ctx, "y"), a)
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites, stamps := map[string][]string{}, map[string]bool{}
	collectSites(f, "api/fixture.go", sites, stamps)
	if len(sites) != 3 || len(sites["api/fixture.go::half"]) != 2 {
		t.Fatalf("precondition: want 3 sites, half() writing twice, got %v", sites)
	}
	table := map[string]siteClass{}
	for _, n := range []string{"discarded", "half", "good"} {
		table["api/fixture.go::"+n] = siteClass{"stamped", "r"}
	}
	got := strings.Join(censusProblems(sites, stamps, table), "\n")
	for _, want := range []string{"::discarded: classified stamped", "::half: classified stamped"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "::good") {
		t.Errorf("fully stamped function was flagged:\n%s", got)
	}
}

// TestHistoryCensusTracksReassignment: a ctx variable's stamped state is taken
// at each write in source order, not once per function. Also covers a direct
// HistoryService.Record call.
func TestHistoryCensusTracksReassignment(t *testing.T) {
	t.Parallel()
	const src = `package api
func (r *Router) before() {
	c := artist.ContextWithProducer(ctx, "x")
	r.artistService.Update(c, a)
	c = req.Context()
}
func (r *Router) after() {
	c := artist.ContextWithProducer(ctx, "x")
	c = req.Context()
	r.artistService.Update(c, a)
}
func (r *Router) direct() {
	r.historyService.Record(ctx, "a", "f", "o", "n", "manual")
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	sites, stamps := map[string][]string{}, map[string]bool{}
	collectSites(f, "api/fixture.go", sites, stamps)
	if len(sites) != 3 {
		t.Fatalf("precondition: want 3 sites (incl. the direct Record), got %v", sites)
	}
	table := map[string]siteClass{}
	for _, n := range []string{"before", "after", "direct"} {
		table["api/fixture.go::"+n] = siteClass{"stamped", "r"}
	}
	got := strings.Join(censusProblems(sites, stamps, table), "\n")
	if strings.Contains(got, "::before") {
		t.Errorf("stamped write before a reassignment was rejected:\n%s", got)
	}
	for _, want := range []string{"::after: classified stamped", "::direct: classified stamped"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
