package templates

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// TestFieldProviderModalContent_MembersApply_Present pins the apply path on
// the Members manual-fetch modal. After a successful provider fetch returns
// at least one member, the modal must render an Apply button (data-apply-members)
// that carries the serialized members payload so the saveMembers script can
// POST it to the upsert endpoint. Without this guard a future refactor that
// reorders the if/else chain in FieldProviderModalContent could silently drop
// the only control that lets the user persist fetched members -- the
// regression that issue #1034 reported.
func TestFieldProviderModalContent_MembersApply_Present(t *testing.T) {
	a := &artist.Artist{ID: "a-1034", Name: "Test Band"}
	results := []provider.FieldProviderResult{
		{
			Provider: provider.NameMusicBrainz,
			HasData:  true,
			Members: []provider.MemberInfo{
				{Name: "Adam Levine", Instruments: []string{"vocals"}},
				{Name: "Jesse Carmichael", Instruments: []string{"keyboards"}},
			},
		},
	}

	var buf bytes.Buffer
	if err := FieldProviderModalContent(a, "members", results, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// The apply button is keyed by the data-apply-members attribute so the
	// saveMembers script can find it deterministically and so this test can
	// distinguish it from the slice-field "Use this" buttons that share the
	// same modal.
	if !strings.Contains(body, "data-apply-members") {
		t.Fatalf("members apply button (data-apply-members) missing from rendered modal; got:\n%s", body)
	}

	// The button must carry the serialized members payload as a data
	// attribute so the saveMembers script can POST it without re-querying
	// the server. Without this, the click would have nothing to send.
	if !strings.Contains(body, `data-members="`) {
		t.Errorf("members apply button missing data-members payload; got:\n%s", body)
	}
	if !strings.Contains(body, "Adam Levine") {
		t.Errorf("members payload missing Adam Levine; got:\n%s", body)
	}

	// W4.B (#1232) double-submit safety: the Apply button must carry the
	// disabled:opacity-60 styling so the visual feedback matches the
	// violation-action surfaces while the request is in flight.
	if !strings.Contains(body, "disabled:opacity-60") {
		t.Errorf("members apply button missing disabled:opacity-60 styling; got:\n%s", body)
	}

	// The onclick handler must invoke saveMembers against the
	// /members/from-provider endpoint -- without this URL the apply
	// path would POST to the wrong handler or be a silent no-op.
	if !strings.Contains(body, "/api/v1/artists/a-1034/members/from-provider") {
		t.Errorf("apply button missing from-provider endpoint URL; got:\n%s", body)
	}
}

// TestFieldProviderModalContent_MembersApply_AbsentBeforeFetch verifies
// that the Apply button does NOT render when a provider returned no members
// (HasData=false). The fetched-but-empty branch shows the "no data" message
// instead, so showing an Apply button with an empty payload would let the
// user clobber existing members with nothing -- the gating contract this
// test pins.
func TestFieldProviderModalContent_MembersApply_AbsentBeforeFetch(t *testing.T) {
	a := &artist.Artist{ID: "a-1034", Name: "Test Band"}
	results := []provider.FieldProviderResult{
		{
			Provider: provider.NameMusicBrainz,
			HasData:  false, // simulates a provider that returned no members
		},
	}

	var buf bytes.Buffer
	if err := FieldProviderModalContent(a, "members", results, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	if strings.Contains(body, "data-apply-members") {
		t.Errorf("members apply button should not render when no provider returned data; got:\n%s", body)
	}
}

// TestFieldProviderModalContent_MembersApply_W4BFailurePattern pins the
// W4.B (#1232) failure-handling pattern in the saveMembers script body.
// The script must:
//   - disable the button on click (in-flight guard against double-submit),
//   - re-enable in finally() so a transient failure cannot leave the
//     button permanently inert,
//   - route failures through window.showToast (with alert() fallback) so
//     the user sees the same visual treatment as every other action error.
//
// Mirrors TestArtistViolationsTab_DoubleSubmitGuard for the violations tab
// surface; without this pin a future refactor could quietly drop one of
// the safety hooks and reintroduce the silent-failure regression that
// issue #1034 reported.
func TestFieldProviderModalContent_MembersApply_W4BFailurePattern(t *testing.T) {
	a := &artist.Artist{ID: "a-1034", Name: "Test Band"}
	results := []provider.FieldProviderResult{
		{
			Provider: provider.NameMusicBrainz,
			HasData:  true,
			Members: []provider.MemberInfo{
				{Name: "Solo Member"},
			},
		},
	}

	var buf bytes.Buffer
	if err := FieldProviderModalContent(a, "members", results, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// Disable on click -- the very first thing the script does after
	// capturing `this` is to disable the button so a rapid second click
	// cannot queue a duplicate POST.
	if !strings.Contains(body, "btn.disabled = true") {
		t.Errorf("saveMembers script missing in-flight disable assignment; got:\n%s", body)
	}
	// Re-enable in finally -- without this a single failure leaves the
	// Apply button permanently disabled.
	if !strings.Contains(body, "btn.disabled = false") {
		t.Errorf("saveMembers script missing finally() re-enable assignment; got:\n%s", body)
	}
	if !strings.Contains(body, ".finally(function()") {
		t.Errorf("saveMembers script missing finally() block; got:\n%s", body)
	}

	// Toast routing with alert() fallback so the user is never left
	// wondering why Apply did nothing -- mirrors the violation-action
	// pattern from #1232.
	if !strings.Contains(body, "window.showToast") {
		t.Errorf("saveMembers script missing window.showToast routing; got:\n%s", body)
	}
	if !strings.Contains(body, "alert(msg)") {
		t.Errorf("saveMembers script missing alert() fallback; got:\n%s", body)
	}
}
