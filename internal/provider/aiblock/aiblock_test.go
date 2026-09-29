package aiblock

import (
	"testing"

	"github.com/sydlexius/stillwater/internal/provider"
)

const testList = `# comment
*://*.nightcafe.studio/*
*://*.freepik.com/premium-ai-image/*
*://*.freepik.com/ai/*
/lexica\.art\/prompt/
/\?ref=ai/
/deviant\.example\/.*-ai-art/i
*://*.MixedCase.example/*
*://*.h.example/a/*/b*
*://*.exact.example/photo.jpg
*://*.exact.example/g/*/end
*://*.q.example/store/details?id=com.x/*
*://*.q.example/store/plain?id=com.y
this line is malformed
*://*.bad*host.com/*
*://*.notld/*
/[unclosed/
/x/g
`

func TestMatchHostSubdomainAndPath(t *testing.T) {
	m := Parse(testList)
	cases := []struct {
		url  string
		want bool
	}{
		{"https://nightcafe.studio/x.jpg", true},                 // bare host
		{"https://images.nightcafe.studio/jobs/abc.jpg", true},   // subdomain
		{"https://a.b.nightcafe.studio/x", true},                 // deep subdomain
		{"https://notnightcafe.studio/x", false},                 // suffix without a dot boundary
		{"https://img.freepik.com/premium-ai-image/x.jpg", true}, // path prefix
		{"https://img.freepik.com/ai/y.jpg", true},               // second path rule
		{"https://img.freepik.com/premium-vector/z.jpg", false},  // same host, other path
		{"https://example.com/nightcafe.studio/x.jpg", false},    // list host only in path
		{"https://lexica.art/prompt/123", true},                  // regex line
		{"https://lexica.art/other", false},                      // regex non-match
		{"https://Images.NightCafe.Studio/x", true},              // URL host is case-insensitive
		{"https://nightcafe.studio./x", true},                    // trailing root dot
		{"https://images.nightcafe.studio./x", true},             // trailing dot plus subdomain
		{"https://mixedcase.example/x", true},                    // rule host is case-insensitive
		{"HTTPS://LEXICA.ART/prompt/1", true},                    // regex sees a lowercased scheme and host
		{"https://other.example/p?ref=ai", true},                 // regex matches the query
		{"https://deviant.example/x/a-AI-art", true},             // /re/i flag
		{"https://img.freepik.com/x/premium-ai-image/y", false},  // path rule is a prefix, not a substring
		{"https://h.example/a/zz/b/c.jpg", true},                 // "*" inside a path
		{"https://h.example/a/zz", false},                        // the tail after "*" must still match
		{"https://h.example/q/a/zz/b", false},                    // and the glob is anchored at the start
		{"https://exact.example/photo.jpg", true},                // exact path matches itself
		{"https://cdn.exact.example/photo.jpg", true},            // ... on a subdomain too
		{"https://exact.example/photo.jpg.backup", false},        // exact path: no surplus suffix
		{"https://exact.example/photo.jpg?v=2", false},           // exact path: no appended query
		{"https://exact.example/photo.jpg/", false},              // exact path: no appended slash
		{"https://exact.example/g/mid/end", true},                // inner glob, end-anchored
		{"https://exact.example/g/mid/end.bak", false},           // inner glob without trailing "*" is anchored
		{"https://exact.example/g/mid/end?x=1", false},           // ... and the query counts as path
		{"https://img.freepik.com/premium-ai-image/", true},      // trailing "*" keeps prefix behavior
		{"https://q.example/store/details?id=com.x/a", true},     // query-bearing rule, matching query
		{"https://q.example/store/details?id=com.z/a", false},    // same path, different query
		{"https://q.example/store/details", false},               // path without the query
		{"https://q.example/store/plain?id=com.y", true},         // exact rule that includes its query
		{"https://q.example/store/plain?id=com.y&z=1", false},    // ... does not take extra query
		{"not a url", false},
		{"", false},
	}
	for _, c := range cases {
		if got := m.MatchURL(c.url); got != c.want {
			t.Errorf("MatchURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestParseSkipsMalformedLines(t *testing.T) {
	m := Parse(testList)
	// free text, wildcard host, single-label host, bad regex, unknown flag
	if m.Skipped != 5 {
		t.Errorf("Skipped = %d, want 5", m.Skipped)
	}
	var nilM *Matcher
	if nilM.MatchURL("https://nightcafe.studio/x") {
		t.Error("nil matcher must match nothing")
	}
}

func TestFilterCountsRemoved(t *testing.T) {
	m := Parse(testList)
	in := []provider.ImageResult{
		{URL: "https://images.nightcafe.studio/jobs/a.jpg"},
		{URL: "https://img.freepik.com/premium-vector/b.jpg"},
	}
	kept, removed := m.Filter(in)
	if removed != 1 || len(kept) != 1 || kept[0].URL != in[1].URL {
		t.Errorf("Filter = kept %v removed %d", kept, removed)
	}
	// Everything removed: a non-nil empty slice, so JSON encodes [] not null.
	kept, removed = m.Filter(in[:1])
	if removed != 1 || kept == nil || len(kept) != 0 {
		t.Errorf("all removed: kept = %#v (nil=%v) removed %d, want empty non-nil and 1", kept, kept == nil, removed)
	}
}

// The vendored list must parse and behave on the two real cases seen live on
// 2026-09-29: a NightCafe image is dropped, a freepik premium-vector image is
// not (the freepik rules are path-scoped).
func TestVendoredList(t *testing.T) {
	m := Default()
	if m.Skipped != 0 {
		t.Errorf("the shipped list must parse with 0 skipped lines, got %d", m.Skipped)
	}
	if !m.MatchURL("https://images.nightcafe.studio/jobs/aGVsbG8/aGVsbG8.jpg") {
		t.Error("vendored list must block images.nightcafe.studio")
	}
	if m.MatchURL("https://img.freepik.com/premium-vector/dragon-illustration_1234.jpg") {
		t.Error("freepik premium-vector must not be blocked: the rules are path-scoped")
	}
	if !m.MatchURL("https://img.freepik.com/premium-ai-image/dragon_1234.jpg") {
		t.Error("freepik premium-ai-image must be blocked")
	}
	// The two rules that used to be silently skipped.
	if !m.MatchURL("https://www.deviantart.com/foo/art/bar-ai-art-123") {
		t.Error("the deviantart /re/i rule must be active")
	}
	if !m.MatchURL("https://www.adobe.com/de/products/firefly/x") {
		t.Error("the adobe mid-path wildcard rule must be active")
	}
	if !m.MatchURL("https://artbreeder.com/x") {
		t.Error("artbreeder.com must still be blocked after the local amendment")
	}
}

// Canary: a refresh that pulls in a list-wide rule (a bare TLD, "/./") would
// hide every result. Ordinary image hosts must never be blocked.
func TestVendoredListDoesNotBlockCleanHosts(t *testing.T) {
	m := Default()
	for _, u := range []string{
		"https://upload.wikimedia.org/wikipedia/commons/a/a1/x.jpg",
		"https://i.scdn.co/image/abc",
		"https://lastfm.freetls.fastly.net/i/u/300x300/x.jpg",
		"https://images.squarespace-cdn.com/content/v1/x.jpg",
		"https://i.pinimg.com/originals/aa/bb/x.jpg",
		"https://img.freepik.com/premium-vector/x.jpg",
		"https://commons.wikimedia.org/w/x.png",
		"https://example.com/photo.jpg",
	} {
		if m.MatchURL(u) {
			t.Errorf("clean host wrongly blocked: %s", u)
		}
	}
}

// The shipped data exercises the exact-path and query fixes: the freepik
// ".htm" rules and the voicemod rule carry no trailing "*", and the Play Store
// rules carry a query.
func TestVendoredExactAndQueryRules(t *testing.T) {
	m := Default()
	if m.Skipped != 0 {
		t.Fatalf("the shipped list must parse with 0 skipped lines, got %d", m.Skipped)
	}
	const fp = "https://img.freepik.com/premium-photo/moon-background-with-astronaut-image-ai-generated-art_39726721.htm"
	if !m.MatchURL(fp) {
		t.Error("freepik exact-path rule must match itself")
	}
	// No negative for freepik: a list-wide freepik regex also covers these
	// URLs, so a suffix stays blocked. The voicemod rule below has no overlap.
	const vm = "https://tuna.voicemod.net/sound/1fdc3b37-441c-4a34-ae88-853bbbb947bb"
	if !m.MatchURL(vm) {
		t.Error("voicemod exact-path rule must match itself")
	}
	if m.MatchURL(vm + "-x") {
		t.Error("voicemod exact-path rule must not match a surplus suffix")
	}
	// Upstream semantics: the rule requires a "/" after the id.
	const play = "https://play.google.com/store/apps/details?id=ai.art.anime/"
	if !m.MatchURL(play) {
		t.Error("play.google.com query-bearing rule must match its query")
	}
	if m.MatchURL("https://play.google.com/store/apps/details?id=org.example.clean/") {
		t.Error("play.google.com rule must not match a different id")
	}
}
