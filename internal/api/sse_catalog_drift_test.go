package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/sydlexius/stillwater/internal/event"
)

// sseEventIDRE matches a dotted lower-case event identifier in a markdown
// backtick span, e.g. `artist.new`, `dashboard.action-resolved`.
var sseEventIDRE = regexp.MustCompile("`([a-z][a-z]+\\.[a-z._-]+)`")

// TestSSECatalogMatchesForwardedSet guards #2009 #12: the SSE event catalog doc
// must list exactly the events the server forwards to SSE clients
// (event.SSEForwardedTypes). The doc is hand-maintained; without this guard a
// new forwarded event (or a removed one) silently diverges from the catalog
// integrators rely on.
func TestSSECatalogMatchesForwardedSet(t *testing.T) {
	// internal/api -> ../.. is the repo root.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	docPath := filepath.Join(root, "docs", "site", "src", "contributing", "architecture", "sse-events.md")
	data, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("reading SSE catalog %s: %v", docPath, err)
	}

	docSet := map[string]bool{}
	for _, m := range sseEventIDRE.FindAllSubmatch(data, -1) {
		docSet[string(m[1])] = true
	}

	wantSet := map[string]bool{}
	for _, ty := range event.SSEForwardedTypes {
		wantSet[string(ty)] = true
	}

	var inDocNotForwarded, forwardedNotInDoc []string
	for id := range docSet {
		if !wantSet[id] {
			inDocNotForwarded = append(inDocNotForwarded, id)
		}
	}
	for id := range wantSet {
		if !docSet[id] {
			forwardedNotInDoc = append(forwardedNotInDoc, id)
		}
	}
	sort.Strings(inDocNotForwarded)
	sort.Strings(forwardedNotInDoc)

	if len(forwardedNotInDoc) > 0 {
		t.Errorf("event(s) in SSEForwardedTypes missing from the SSE catalog doc (%s) -- add them: %v", docPath, forwardedNotInDoc)
	}
	if len(inDocNotForwarded) > 0 {
		t.Errorf("event id(s) in the SSE catalog doc that are NOT in SSEForwardedTypes -- remove them or add to the forwarded set: %v", inDocNotForwarded)
	}
}

// TestSSEHubForwardsCanonicalSet ties the hub code to the canonical set: the
// events wired in sseEventMappings, plus the two that stream on the dedicated
// logs endpoint (#1338), must equal event.SSEForwardedTypes. A new hub mapping
// not added to the canonical set/catalog fails here, and vice versa.
func TestSSEHubForwardsCanonicalSet(t *testing.T) {
	covered := map[string]bool{}
	for _, m := range sseEventMappings {
		covered[string(m.eventType)] = true
	}
	// Forwarded on the dedicated logs stream, not via the hub mappings.
	covered[string(event.LogsLine)] = true
	covered[string(event.LogsThrottled)] = true

	wantSet := map[string]bool{}
	for _, ty := range event.SSEForwardedTypes {
		wantSet[string(ty)] = true
	}

	var extra, missing []string
	for id := range covered {
		if !wantSet[id] {
			extra = append(extra, id)
		}
	}
	for id := range wantSet {
		if !covered[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)

	if len(extra) > 0 {
		t.Errorf("SSE hub forwards event(s) not in event.SSEForwardedTypes -- add them to the canonical set + catalog: %v", extra)
	}
	if len(missing) > 0 {
		t.Errorf("event.SSEForwardedTypes contains event(s) neither in sseEventMappings nor the logs stream -- wire them or remove from the set: %v", missing)
	}
}
