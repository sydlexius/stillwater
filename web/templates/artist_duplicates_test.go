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

// membersBlob extracts and decodes the first data-members JSON blob from a
// rendered page. Shared by the tests that assert the wire contract the merge
// modal's JS depends on.
func membersBlob(t *testing.T, body string) []map[string]any {
	t.Helper()
	const startToken = `data-members="`
	startIdx := strings.Index(body, startToken)
	if startIdx < 0 {
		t.Fatalf("missing data-members attribute")
	}
	startIdx += len(startToken)
	endIdx := strings.Index(body[startIdx:], `"`)
	if endIdx < 0 {
		t.Fatalf("data-members attribute not closed")
	}
	raw := html.UnescapeString(body[startIdx : startIdx+endIdx])
	var members []map[string]any
	if err := json.Unmarshal([]byte(raw), &members); err != nil {
		t.Fatalf("data-members not valid JSON after unescape: %v\nraw: %s", err, raw)
	}
	return members
}

// TestArtistDuplicatesTable_DisambiguationConflict pins the #2527 Defect-2
// surface on the page: the group badge, the per-member amber marker, and -- the
// load-bearing part -- the two JSON fields the merge modal reads to decide
// whether to demand an override.
//
// The JSON keys are asserted by exact name because they cross a language
// boundary: DuplicateGroupMembersJSON builds its DTO via a Go struct
// type-conversion, so renaming a field compiles fine on both sides while the
// JS silently reads undefined and the gate disarms itself. No Go test would
// catch that except this one.
func TestArtistDuplicatesTable_DisambiguationConflict(t *testing.T) {
	view := ArtistDuplicatesPageView{
		Groups: []ArtistDuplicateGroupRow{{
			Key:                    "nirvana",
			Reason:                 "mbid",
			DisambiguationConflict: true,
			Members: []ArtistDuplicateMember{
				{ID: "a", Name: "Nirvana", Path: "/music/US", MBID: "m1",
					Disambiguation: "Seattle grunge band", DisambiguationConflict: true},
				{ID: "b", Name: "Nirvana", Path: "/music/UK", MBID: "m1",
					Disambiguation: "UK progressive rock band", DisambiguationConflict: true},
			},
		}},
	}

	var buf bytes.Buffer
	if err := ArtistDuplicatesTable(AssetPaths{BasePath: ""}, view).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	if !strings.Contains(body, "Disambiguation Conflict") {
		t.Errorf("conflicting group is missing the Disambiguation Conflict badge")
	}
	if !strings.Contains(body, "Seattle grunge band") || !strings.Contains(body, "UK progressive rock band") {
		t.Errorf("member disambiguation values are not rendered; the operator cannot see which rows disagree")
	}

	members := membersBlob(t, body)
	if len(members) != 2 {
		t.Fatalf("expected 2 members in blob, got %d", len(members))
	}
	for i, m := range members {
		if m["disambiguation_conflict"] != true {
			t.Errorf("member[%d]: disambiguation_conflict = %#v, want true; the modal's override gate "+
				"reads this key, so a false/absent value means an unguarded irreversible merge", i, m["disambiguation_conflict"])
		}
		if m["disambiguation"] == nil || m["disambiguation"] == "" {
			t.Errorf("member[%d]: disambiguation missing from wire blob; got %#v", i, m["disambiguation"])
		}
	}
}

// TestArtistDuplicatesTable_NoDisambiguationConflict is the negative control:
// an ordinary group must NOT badge, and its wire blob must carry
// disambiguation_conflict=false. A template that always emitted the badge, or a
// DTO that hardcoded the flag, would pass the test above and fail here.
func TestArtistDuplicatesTable_NoDisambiguationConflict(t *testing.T) {
	view := ArtistDuplicatesPageView{
		Groups: []ArtistDuplicateGroupRow{{
			Key:    "mbid-123",
			Reason: "mbid",
			Members: []ArtistDuplicateMember{
				{ID: "c3", Name: "Boards of Canada", Path: "/music/BoC", MBID: "mbid-123", Disambiguation: "Scottish duo"},
				{ID: "d4", Name: "Boards of Canada", Path: "/music/Boards", MBID: "mbid-123"},
			},
		}},
	}

	var buf bytes.Buffer
	if err := ArtistDuplicatesTable(AssetPaths{BasePath: ""}, view).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	if strings.Contains(body, "Disambiguation Conflict") {
		t.Errorf("non-conflicting group must not render the Disambiguation Conflict badge; " +
			"crying wolf trains operators to click through the override reflexively")
	}
	for i, m := range membersBlob(t, body) {
		if m["disambiguation_conflict"] != false {
			t.Errorf("member[%d]: disambiguation_conflict = %#v, want false", i, m["disambiguation_conflict"])
		}
	}
}

// TestArtistMergeModal_DisambiguationGate pins the modal-side soft gate
// (#2527): the warning block exists, ships HIDDEN (so an ordinary merge is
// unaffected), carries the override checkbox the JS gates Confirm on, and the
// script actually consults that checkbox at commit time rather than trusting
// the button's disabled state.
func TestArtistMergeModal_DisambiguationGate(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistMergeModal().Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	if !strings.Contains(body, `id="merge-disamb-warning"`) {
		t.Fatalf("merge modal is missing the #merge-disamb-warning block")
	}
	if !strings.Contains(body, `id="merge-disamb-override"`) {
		t.Fatalf("merge modal is missing the #merge-disamb-override checkbox")
	}

	// The warning must default to hidden: it is revealed per-group by openModal.
	// A block that shipped visible would warn on every merge.
	warnIdx := strings.Index(body, `id="merge-disamb-warning"`)
	blockEnd := strings.Index(body[warnIdx:], ">")
	if blockEnd < 0 {
		t.Fatalf("#merge-disamb-warning element not closed")
	}
	if !strings.Contains(body[warnIdx:warnIdx+blockEnd], "hidden") {
		t.Errorf("#merge-disamb-warning must render hidden by default; opening tag: %q",
			body[warnIdx:warnIdx+blockEnd])
	}

	// ID uniqueness: the JS resolves both by getElementById, so a duplicate
	// would make the gate bind to an arbitrary node.
	for _, id := range []string{`id="merge-disamb-warning"`, `id="merge-disamb-override"`} {
		if n := strings.Count(body, id); n != 1 {
			t.Errorf("%s appears %d times, want exactly 1", id, n)
		}
	}

	// The commit path must re-check the override, not merely rely on the
	// disabled button -- this is the assertion that would fail if someone
	// "simplified" the gate down to a visual one.
	if !strings.Contains(body, "current.disambConflict && !(disambOverride && disambOverride.checked)") {
		t.Errorf("commitMerge does not re-assert the disambiguation override before POSTing; " +
			"a disabled-button-only gate is bypassable and this merge is irreversible")
	}
	if !strings.Contains(body, "function updateConfirmState()") {
		t.Errorf("expected a single updateConfirmState() authority over the Confirm button")
	}

	// Without the change listener the soft gate becomes a HARD block: Confirm is
	// disabled, the operator ticks the override, no event fires, Confirm stays
	// disabled and the group is unmergeable.
	if !strings.Contains(body, "disambOverride.addEventListener('change', updateConfirmState)") {
		t.Errorf("the override checkbox is not wired to updateConfirmState on 'change'; ticking it " +
			"would never re-enable Confirm, turning the soft gate into a hard block")
	}
	// The gate decision must read the server's flag, not re-derive it client-side.
	if !strings.Contains(body, "m.disambiguation_conflict") {
		t.Errorf("the Confirm gate does not derive from the server's disambiguation_conflict flag; " +
			"a client-side re-derivation can silently disagree with the card badge")
	}
}

// sampleDuplicatesView returns a two-group view: one name_key group with a
// recommended survivor, one mbid group. Enough to exercise both reason badges,
// the recommended badge, the per-group action buttons, and the data-* hooks.
func sampleDuplicatesView() ArtistDuplicatesPageView {
	return ArtistDuplicatesPageView{
		Groups: []ArtistDuplicateGroupRow{
			{
				Key:    "the cure",
				Reason: "name_key",
				Members: []ArtistDuplicateMember{
					{ID: "a1", Name: "The Cure", Path: "/music/Cure"},
					{ID: "b2", Name: "The Cure", Path: "/music/The Cure", Recommended: true, RecommendedReason: "canonical_basename"},
				},
			},
			{
				Key:    "mbid-123",
				Reason: "mbid",
				Members: []ArtistDuplicateMember{
					{ID: "c3", Name: "Boards of Canada", Path: "/music/BoC", MBID: "mbid-123"},
					{ID: "d4", Name: "Boards of Canada", Path: "/music/Boards", MBID: "mbid-123", Recommended: true, RecommendedReason: "most_content"},
				},
			},
		},
	}
}

// TestArtistDuplicatesTable_IgnoreHooksAndCanonicalLinks pins the promoted
// table's per-group Ignore trigger (#1716), the hidden all-dismissed panel, and
// the canonical member links. Post-promotion (M55 #1757 PR-6b) the member link
// must target /artists/{id}, NOT the retired /next/artists/{id} lane.
func TestArtistDuplicatesTable_IgnoreHooksAndCanonicalLinks(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistDuplicatesTable(AssetPaths{BasePath: "", IsAdmin: true}, sampleDuplicatesView()).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	for _, want := range []string{
		`data-ignore-group`,               // per-group Ignore trigger (#1716)
		`data-merge-open`,                 // per-group Merge trigger
		`data-group-key="the cure"`,       // name_key group key
		`/artists/a1`,                     // member link uses the canonical route
		`id="duplicates-empty-dismissed"`, // all-dismissed panel present (hidden)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered duplicates table missing %q", want)
		}
	}
	// The member link must NOT reference the retired /next/ lane.
	if strings.Contains(body, "/next/artists/") {
		t.Error("promoted table must not link to the retired /next/artists/ lane")
	}
	// The all-dismissed panel must start hidden; the ignore script reveals it.
	if !strings.Contains(body, `id="duplicates-empty-dismissed" hidden`) {
		t.Error("duplicates-empty-dismissed panel should render with the hidden attribute")
	}
	// Both reason badges should appear (one name_key group, one mbid group).
	for _, label := range []string{"Name collision", "Shared MBID"} {
		if !strings.Contains(body, label) {
			t.Errorf("reason badge %q missing", label)
		}
	}
}

// TestArtistDuplicatesPage_EmptyState verifies the "none detected" empty state
// renders (and the all-dismissed variant does not) when there are no groups, so
// an admin with a clean library sees the right message.
func TestArtistDuplicatesPage_EmptyState(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistDuplicatesPage(AssetPaths{IsAdmin: true}, ArtistDuplicatesPageView{}).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `id="duplicates-empty-none"`) {
		t.Error("none-detected empty state missing")
	}
	if strings.Contains(body, `id="duplicates-empty-dismissed"`) {
		t.Error("all-dismissed panel should not render when there are zero groups")
	}
	if !strings.Contains(body, "No suspected duplicates detected.") {
		t.Error("empty-state message missing")
	}
}

// TestArtistDuplicatesPage_IgnoreScriptContract pins the ignore script's
// server-persistence contract (#2219): clicking Ignore POSTs the group's member
// IDs to the ignore endpoint with a CSRF header, and there is NO remaining
// localStorage read/write path (the server is the single source of truth). A
// regression to the old client-only localStorage scheme would reintroduce the
// split client/server state the AC forbids.
func TestArtistDuplicatesPage_IgnoreScriptContract(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistDuplicatesPage(AssetPaths{IsAdmin: true}, sampleDuplicatesView()).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// The ignore must POST to the server endpoint with the member_ids payload
	// and a CSRF header -- the actual persistence mechanism, not a substring
	// that a no-op script could also satisfy.
	for _, want := range []string{
		`/api/v1/artists/duplicates/ignore`, // server endpoint
		`method: 'POST'`,                    // mutation, not a read
		`member_ids`,                        // group identity sent to the server
		`X-CSRF-Token`,                      // CSRF-protected state change
		`swCsrfToken`,                       // canonical token reader
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ignore script must POST the ignore server-side; missing %q", want)
		}
	}

	// The legacy client-only ignore key scheme must be fully removed so no
	// ignore state lives only in the browser (the #2219 no-split-state AC). The
	// ui.confirm.duplicate. prefix was unique to the old ignore script; a bare
	// "localStorage" check would false-positive on the layout's other scripts.
	if strings.Contains(body, `ui.confirm.duplicate.`) {
		t.Error("ignore script must not retain the client-only localStorage key scheme (ui.confirm.duplicate.)")
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
		"keeping_badge",
		"will_be_removed",
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

// TestArtistDuplicatesIgnoredTable_RowAndRestore pins the manage-ignored table
// contract (#2219 remainder): a populated row renders the derived member count,
// the group key, a reason badge, and a Restore button whose hx-delete targets
// the id-scoped restore endpoint and whose HTMX swap replaces the whole table
// fragment. Drifting any of these silently breaks the un-ignore affordance.
func TestArtistDuplicatesIgnoredTable_RowAndRestore(t *testing.T) {
	view := ArtistDuplicatesIgnoredPageView{
		Rows: []IgnoredDuplicateGroupRow{{
			ID:          "row-42",
			GroupKey:    "the cure",
			Reason:      "name_key",
			MemberCount: 3,
			CreatedAt:   "2026-07-05 18:20:00",
		}},
	}
	var buf bytes.Buffer
	if err := ArtistDuplicatesIgnoredTable(view).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	if !strings.Contains(body, `id="artist-duplicates-ignored-table"`) {
		t.Errorf("missing stable table container id (HTMX swap target)")
	}
	if !strings.Contains(body, `hx-delete="/api/v1/artists/duplicates/ignored/row-42"`) {
		t.Errorf("Restore button must hx-delete the id-scoped restore endpoint; got %q", body)
	}
	if !strings.Contains(body, `hx-target="#artist-duplicates-ignored-table"`) || !strings.Contains(body, `hx-swap="outerHTML"`) {
		t.Errorf("Restore button must swap the whole table fragment (outerHTML)")
	}
	if !strings.Contains(body, `data-sw-roving-activate`) {
		t.Errorf("Restore button must be the roving-list Enter activation target")
	}
	if !strings.Contains(body, `the cure`) {
		t.Errorf("row must render the group key")
	}
	if !strings.Contains(body, `>3<`) {
		t.Errorf("row must render the derived member count (3)")
	}
}

// TestArtistDuplicatesIgnoredTable_EmptyState renders the empty-state copy (not a
// bare table) when nothing is ignored -- the state the user lands on after
// restoring the last group.
func TestArtistDuplicatesIgnoredTable_EmptyState(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistDuplicatesIgnoredTable(ArtistDuplicatesIgnoredPageView{}).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `id="artist-duplicates-ignored-table"`) {
		t.Errorf("empty state must still carry the swap-target container id")
	}
	if strings.Contains(body, "<table") {
		t.Errorf("empty state must not render a table; got %q", body)
	}
	// testCtx wires the real en translator, so the key resolves to its copy.
	if !strings.Contains(body, "No ignored duplicate groups") {
		t.Errorf("empty state must render the ignored_empty copy; got %q", body)
	}
}

// TestArtistDuplicatesIgnoredTable_UnknownGroupAndReason pins the two fallback
// branches TestArtistDuplicatesIgnoredTable_RowAndRestore doesn't exercise
// (it always sets GroupKey and a recognized Reason): an empty GroupKey (the
// ignore request never captured display context, or it was blank) must render
// the ignored_group_unknown placeholder rather than an empty cell, and a
// Reason outside {"mbid", "name_key"} must fall back to the em-dash rather
// than silently rendering nothing.
func TestArtistDuplicatesIgnoredTable_UnknownGroupAndReason(t *testing.T) {
	view := ArtistDuplicatesIgnoredPageView{
		Rows: []IgnoredDuplicateGroupRow{{
			ID:          "row-99",
			GroupKey:    "",
			Reason:      "",
			MemberCount: 2,
			CreatedAt:   "2026-07-06 09:00:00",
		}},
	}
	var buf bytes.Buffer
	if err := ArtistDuplicatesIgnoredTable(view).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// testCtx wires the real en translator, so the key resolves to its copy.
	if !strings.Contains(body, "Unknown group") {
		t.Errorf("empty GroupKey must render the ignored_group_unknown placeholder; got %q", body)
	}
	if !strings.Contains(body, "&mdash;") {
		t.Errorf("an unrecognized Reason must fall back to the em-dash; got %q", body)
	}
	// Neither reason badge's translated copy (mbid or name_key) should render
	// for this row -- checking the rendered text, not the i18n key, since t()
	// always resolves to copy and a raw key would never appear either way.
	if strings.Contains(body, "Shared MBID") || strings.Contains(body, "Name collision") {
		t.Errorf("unrecognized Reason must not render either badge; got %q", body)
	}
}
