package templates

import (
	"bytes"
	"encoding/json"
	"html"
	"strings"
	"testing"
)

// TestArtistDuplicatesTable_MergeButtonAndMembers pins the per-group hooks
// the merge modal's JS reads at runtime: the card carries a stable
// data-duplicate-group marker, a data-group-key, a data-members JSON blob,
// and a [data-merge-open] button. Drifting any of these silently breaks the
// click handler without a Go-level compile error.
func TestArtistDuplicatesTable_MergeButtonAndMembers(t *testing.T) {
	view := ArtistDuplicatesPageView{
		Groups: []ArtistDuplicateGroupRow{{
			Key:    "the cure",
			Reason: "name_key",
			Members: []ArtistDuplicateMember{
				{ID: "a", Name: "The Cure", Path: "/music/Cure"},
				{ID: "b", Name: "The Cure", Path: "/music/The Cure", Recommended: true, RecommendedReason: "canonical_basename"},
			},
		}},
	}

	var buf bytes.Buffer
	if err := ArtistDuplicatesTable(AssetPaths{BasePath: ""}, view).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	if !strings.Contains(body, `data-duplicate-group`) {
		t.Errorf("missing data-duplicate-group marker")
	}
	if !strings.Contains(body, `data-group-key="the cure"`) {
		t.Errorf("missing data-group-key attribute")
	}
	if !strings.Contains(body, `data-merge-open`) {
		t.Errorf("missing [data-merge-open] button")
	}
	if !strings.Contains(body, `Merge...`) {
		t.Errorf("Merge button label missing")
	}

	// The data-members blob must round-trip through JSON and carry the
	// recommended flag the JS uses to pre-select the survivor radio.
	startToken := `data-members="`
	startIdx := strings.Index(body, startToken)
	if startIdx < 0 {
		t.Fatalf("missing data-members attribute")
	}
	startIdx += len(startToken)
	endIdx := strings.Index(body[startIdx:], `"`)
	if endIdx < 0 {
		t.Fatalf("data-members attribute not closed")
	}
	// templ HTML-escapes the JSON inside the attribute; html.UnescapeString
	// handles every named/numeric entity templ might emit (&#34;, &amp;,
	// &#x22;, etc.) so the test isn't brittle to escape-policy changes.
	raw := html.UnescapeString(body[startIdx : startIdx+endIdx])
	var members []map[string]any
	if err := json.Unmarshal([]byte(raw), &members); err != nil {
		t.Fatalf("data-members not valid JSON after unescape: %v\nraw: %s", err, raw)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	if members[1]["recommended"] != true {
		t.Errorf("second member should be recommended; got %#v", members[1])
	}
}

// TestArtistDuplicatesTable_RecommendedBadge pins the visible badge so a
// regression that drops it (or moves it off the recommended row) gets caught.
func TestArtistDuplicatesTable_RecommendedBadge(t *testing.T) {
	view := ArtistDuplicatesPageView{
		Groups: []ArtistDuplicateGroupRow{{
			Key:    "k",
			Reason: "name_key",
			Members: []ArtistDuplicateMember{
				{ID: "a", Name: "Foo", Path: "/x/A", Recommended: true, RecommendedReason: "most_content"},
				{ID: "b", Name: "Foo", Path: "/x/B"},
			},
		}},
	}
	var buf bytes.Buffer
	if err := ArtistDuplicatesTable(AssetPaths{BasePath: ""}, view).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `>Recommended<`) {
		t.Errorf("Recommended badge missing from rendered table")
	}
}

// TestArtistMergeModal_Renders pins the structural ids the JS depends on.
// The modal HTML is constructed by templ; tests assert on the parts the
// runtime JS queries by id (#merge-modal, #merge-backdrop,
// #merge-survivor-options, #merge-preview-body, #merge-modal-confirm,
// #merge-i18n). If any rename, the modal stops working.
func TestArtistMergeModal_Renders(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistMergeModal().Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	for _, id := range []string{
		`id="merge-i18n"`,
		`id="merge-modal"`,
		`id="merge-backdrop"`,
		`id="merge-survivor-options"`,
		`id="merge-preview-body"`,
		`id="merge-modal-confirm"`,
	} {
		if !strings.Contains(body, id) {
			t.Errorf("modal missing %s", id)
		}
	}
	if !strings.Contains(body, `data-i18n=`) {
		t.Errorf("merge-i18n div missing data-i18n attribute")
	}
}

// TestMergeI18nJSON pins the set of keys the JS reads from window-side i18n.
// Adding or removing a key requires updating both the helper and the JS;
// this test catches one-sided drift. Values must be non-empty so a missing
// translation surfaces during render rather than silently rendering "".
func TestMergeI18nJSON(t *testing.T) {
	ctx := testCtx(t)
	wantKeys := []string{
		"preview_loading",
		"preview_empty",
		"preview_network_error",
		"moves_heading",
		"warnings_heading",
		"warning_override",
		"platform_rescan_note",
		"conflicts_heading",
		"conflicts_help",
		"recommended_badge",
		"reason_canonical_basename",
		"reason_most_content",
		"reason_fallback",
		"error_merge_in_progress",
		"error_locked",
		"error_stale_group",
		"error_survivor_missing",
		"error_unknown",
		"exclude_label",
		"no_sources_selected",
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(mergeI18nJSON(ctx)), &m); err != nil {
		t.Fatalf("mergeI18nJSON did not produce valid JSON: %v", err)
	}
	for _, k := range wantKeys {
		v, ok := m[k]
		if !ok {
			t.Errorf("mergeI18nJSON missing key %q", k)
			continue
		}
		if v == "" {
			t.Errorf("mergeI18nJSON key %q has empty value", k)
		}
	}
	if len(m) != len(wantKeys) {
		t.Errorf("mergeI18nJSON has %d keys, want %d", len(m), len(wantKeys))
	}
}
