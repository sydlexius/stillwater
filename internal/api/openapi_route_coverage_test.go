package api

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// openapi_route_coverage_test.go is #2009 #5: a bidirectional route<->spec
// coverage guard.
//
//   - spec -> router (a documented operation with no route) is already enforced
//     by TestOperationIDCoverage (its unmappedOps check), so it is not
//     duplicated here.
//
//   - router -> spec (a registered /api/v1 route with NO openapi operation) had
//     no guard: a new endpoint could ship undocumented. TestOpenAPIRouteCoverage
//     closes that direction using the same router-AST and spec parsers as the
//     operationId coverage test.
//
// It uses a ratchet baseline (testdata/openapi-route-ignore.json) seeded with
// the routes undocumented at the time of writing -- health/docs/meta endpoints
// and any pre-existing gaps -- mirroring the openapi-coverage-baseline.json and
// Bruno parity-ignore.json patterns. A NEW undocumented /api/v1 route fails;
// shrinking the baseline (documenting a route) is surfaced as a stale entry.
//
// Regenerate the baseline after an intentional change:
//
//	SW_UPDATE_ROUTE_IGNORE=1 go test ./internal/api/ -run TestOpenAPIRouteCoverage

const routeIgnorePath = "testdata/openapi-route-ignore.json"

func loadRouteIgnore(path string) (map[string]bool, error) {
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
	m := make(map[string]bool, len(list))
	for _, k := range list {
		m[k] = true
	}
	return m, nil
}

func writeRouteIgnore(path string, keys []string) error {
	sort.Strings(keys)
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func TestOpenAPIRouteCoverage(t *testing.T) {
	t.Parallel()

	ops, err := loadSpecOperations("openapi.yaml")
	if err != nil {
		t.Fatalf("loading openapi.yaml: %v", err)
	}
	routes, err := loadRouterBindings("router.go")
	if err != nil {
		t.Fatalf("loading router.go: %v", err)
	}

	// Spec keys are bare paths; router keys carry the /api/v1 prefix that the
	// spec's server base URL implies (same convention as TestOperationIDCoverage).
	specSet := make(map[string]bool, len(ops))
	for _, op := range ops {
		specSet[op.Method+" /api/v1"+op.Path] = true
	}

	// Every /api/v1 router binding that has no matching spec operation.
	var undocumented []string
	for key := range routes {
		_, route, ok := strings.Cut(key, " ")
		if !ok || !strings.HasPrefix(route, "/api/v1") {
			continue // non-API routes (/, /static/, /register) are out of scope
		}
		if !specSet[key] {
			undocumented = append(undocumented, key)
		}
	}
	sort.Strings(undocumented)

	if os.Getenv("SW_UPDATE_ROUTE_IGNORE") != "" {
		if err := writeRouteIgnore(routeIgnorePath, undocumented); err != nil {
			t.Fatalf("writing route ignore baseline: %v", err)
		}
		t.Logf("regenerated %s with %d undocumented routes", routeIgnorePath, len(undocumented))
		return
	}

	ignore, err := loadRouteIgnore(routeIgnorePath)
	if err != nil {
		t.Fatalf("loading route ignore baseline: %v", err)
	}

	// New gaps: undocumented routes not in the baseline.
	var newGaps []string
	have := make(map[string]bool, len(undocumented))
	for _, k := range undocumented {
		have[k] = true
		if !ignore[k] {
			newGaps = append(newGaps, k)
		}
	}
	if len(newGaps) > 0 {
		t.Errorf("%d /api/v1 route(s) registered in router.go have no openapi.yaml operation. Document them in the spec, or (if intentionally undocumented) regenerate the baseline with SW_UPDATE_ROUTE_IGNORE=1:\n  %s",
			len(newGaps), strings.Join(newGaps, "\n  "))
	}

	// Stale baseline entries: now documented (or removed). Keep the ratchet tight.
	var stale []string
	for k := range ignore {
		if !have[k] {
			stale = append(stale, k)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("%d entr(y/ies) in %s are now documented or gone. Regenerate with SW_UPDATE_ROUTE_IGNORE=1:\n  %s",
			len(stale), routeIgnorePath, strings.Join(stale, "\n  "))
	}
}
