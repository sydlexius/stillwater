package next

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/web/templates"
)

// sampleDuplicatesView returns a two-group view: one name_key group with a
// recommended survivor, one mbid group. Enough to exercise both reason badges,
// the recommended badge, the per-group action buttons, and the data-* hooks.
func sampleDuplicatesView() templates.ArtistDuplicatesPageView {
	return templates.ArtistDuplicatesPageView{
		Groups: []templates.ArtistDuplicateGroupRow{
			{
				Key:    "the cure",
				Reason: "name_key",
				Members: []templates.ArtistDuplicateMember{
					{ID: "a1", Name: "The Cure", Path: "/music/Cure"},
					{ID: "b2", Name: "The Cure", Path: "/music/The Cure", Recommended: true, RecommendedReason: "canonical_basename"},
				},
			},
			{
				Key:    "mbid-123",
				Reason: "mbid",
				Members: []templates.ArtistDuplicateMember{
					{ID: "c3", Name: "Boards of Canada", Path: "/music/BoC", MBID: "mbid-123"},
					{ID: "d4", Name: "Boards of Canada", Path: "/music/Boards", MBID: "mbid-123", Recommended: true, RecommendedReason: "most_content"},
				},
			},
		},
	}
}

// TestArtistDuplicatesNextPage_ChromeAndHooks pins the next/ page shell plus the
// shared merge-modal ids and the per-group JS hooks (data-duplicate-group,
// data-group-key, data-members, data-merge-open, data-ignore-group). Drifting
// any of these silently breaks the merge or ignore handlers.
func TestArtistDuplicatesNextPage_ChromeAndHooks(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistDuplicatesNextPage(templates.AssetPaths{BasePath: "", IsAdmin: true}, sampleDuplicatesView()).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	for _, want := range []string{
		`sw-next-duplicates`,              // next/ scope marker
		`id="merge-modal"`,                // shared merge modal present
		`id="merge-i18n"`,                 // merge i18n blob present
		`data-duplicate-group`,            // group card marker (merge + ignore consume it)
		`data-group-key="the cure"`,       // name_key group key
		`data-merge-open`,                 // per-group Merge trigger
		`data-ignore-group`,               // per-group Ignore trigger (#1716)
		`data-members=`,                   // members blob for the modal + ignore key
		`/next/artists/a1`,                // member link uses the next channel
		`>Recommended<`,                   // recommended badge rendered
		`id="duplicates-empty-dismissed"`, // all-dismissed panel present (hidden)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered next duplicates page missing %q", want)
		}
	}

	// The all-dismissed panel must start hidden; the ignore script reveals it.
	if !strings.Contains(body, `id="duplicates-empty-dismissed" hidden`) {
		t.Errorf("duplicates-empty-dismissed panel should render with the hidden attribute")
	}

	// Both reason badges should appear (one name_key group, one mbid group).
	for _, label := range []string{"Name collision", "Shared MBID"} {
		if !strings.Contains(body, label) {
			t.Errorf("reason badge %q missing", label)
		}
	}
}

// TestArtistDuplicatesNextPage_EmptyState verifies the "none detected" empty
// state renders (and the all-dismissed variant does not) when there are no
// groups, so an admin with a clean library sees the right message.
func TestArtistDuplicatesNextPage_EmptyState(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistDuplicatesNextPage(templates.AssetPaths{IsAdmin: true}, templates.ArtistDuplicatesPageView{}).Render(nextTestCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `id="duplicates-empty-none"`) {
		t.Errorf("none-detected empty state missing")
	}
	if strings.Contains(body, `id="duplicates-empty-dismissed"`) {
		t.Errorf("all-dismissed panel should not render when there are zero groups")
	}
	if !strings.Contains(body, "No suspected duplicates detected.") {
		t.Errorf("empty-state message missing")
	}
}

// TestArtistDuplicatesNextPage_IgnoreScriptContract pins the ignore script's
// server-persistence contract (#2219): clicking Ignore POSTs the group's member
// IDs to the ignore endpoint with a CSRF header, and there is NO remaining
// localStorage read/write path (the server is the single source of truth). A
// regression to the old client-only localStorage scheme would reintroduce the
// split client/server state the AC forbids.
func TestArtistDuplicatesNextPage_IgnoreScriptContract(t *testing.T) {
	var buf bytes.Buffer
	if err := ArtistDuplicatesNextPage(templates.AssetPaths{IsAdmin: true}, sampleDuplicatesView()).Render(nextTestCtx(t), &buf); err != nil {
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
		t.Errorf("ignore script must not retain the client-only localStorage key scheme (ui.confirm.duplicate.)")
	}
}
