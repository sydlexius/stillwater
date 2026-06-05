package templates

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/rule"
)

// TestDashboardActionCard_DoubleSubmitGuard pins the htmx-native double-click
// guard on the dashboard violation cards. Both the Fix and Dismiss buttons
// must render hx-disabled-elt="this" so htmx disables the element while
// the request is in flight and re-enables it on settled (success or
// error). Without this pin a future refactor that splits the button
// rendering could silently drop the guard and reintroduce the duplicate-
// POST regression. We do NOT exercise the click suppression itself --
// htmx owns that semantics.
func TestDashboardActionCard_DoubleSubmitGuard(t *testing.T) {
	v := rule.RuleViolation{
		ID:         "v-test-1110",
		RuleID:     rule.RuleNFOExists,
		ArtistID:   "a-1",
		ArtistName: "Test Artist",
		Severity:   "error",
		Message:    "missing nfo",
		Fixable:    true,
		Status:     rule.ViolationStatusOpen,
		CreatedAt:  time.Now().UTC(),
	}

	var buf bytes.Buffer
	if err := DashboardActionCard(v, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	// The Fix button is the htmx POST against /fix. It must carry the
	// disable attribute so a rapid second click cannot queue a second
	// fix attempt against an already-resolving violation.
	if !strings.Contains(body, `hx-post="/api/v1/notifications/v-test-1110/fix"`) {
		t.Fatalf("rendered card missing fix hx-post attribute; got:\n%s", body)
	}
	fixIdx := strings.Index(body, `hx-post="/api/v1/notifications/v-test-1110/fix"`)
	// Find the closing > of the Fix button starting from the hx-post.
	fixCloseIdx := strings.Index(body[fixIdx:], ">")
	if fixCloseIdx < 0 {
		t.Fatalf("could not find end of Fix button tag")
	}
	fixTag := body[fixIdx : fixIdx+fixCloseIdx]
	if !strings.Contains(fixTag, `hx-disabled-elt="this"`) {
		t.Errorf("Fix button missing hx-disabled-elt=\"this\"; tag was:\n%s", fixTag)
	}

	// Same guard required on the Dismiss button: even though dismiss is
	// idempotent server-side, the UX requirement is no double-submit
	// during in-flight, and dropping the attribute would leave a visible
	// "second click goes through" regression.
	if !strings.Contains(body, `hx-post="/api/v1/notifications/v-test-1110/dismiss"`) {
		t.Fatalf("rendered card missing dismiss hx-post attribute; got:\n%s", body)
	}
	dismissIdx := strings.Index(body, `hx-post="/api/v1/notifications/v-test-1110/dismiss"`)
	dismissCloseIdx := strings.Index(body[dismissIdx:], ">")
	if dismissCloseIdx < 0 {
		t.Fatalf("could not find end of Dismiss button tag")
	}
	dismissTag := body[dismissIdx : dismissIdx+dismissCloseIdx]
	if !strings.Contains(dismissTag, `hx-disabled-elt="this"`) {
		t.Errorf("Dismiss button missing hx-disabled-elt=\"this\"; tag was:\n%s", dismissTag)
	}
}

// TestDashboardActionCard_ChannelAwareArtistLink pins #1852 end-to-end through
// the SHARED card render: the stable channel (no ctx base) links to
// /artists/<id>, and the next channel (WithArtistDetailBase, as the next
// action-queue handler injects) links to /next/artists/<id> with no bare
// /artists/<id> leak. A regression dropping the ctx injection, or rendering the
// card with the wrong context, would slip past the helper-level test.
func TestDashboardActionCard_ChannelAwareArtistLink(t *testing.T) {
	v := rule.RuleViolation{
		ID:         "v-1852",
		RuleID:     rule.RuleNFOExists,
		ArtistID:   "a-42",
		ArtistName: "Channel Artist",
		Severity:   "error",
		Message:    "missing nfo",
		Fixable:    true,
		Status:     rule.ViolationStatusOpen,
		CreatedAt:  time.Now().UTC(),
	}

	var stable bytes.Buffer
	if err := DashboardActionCard(v, "").Render(testCtx(t), &stable); err != nil {
		t.Fatalf("render stable: %v", err)
	}
	if sb := stable.String(); !strings.Contains(sb, `href="/artists/a-42"`) || strings.Contains(sb, "/next/artists/") {
		t.Errorf("stable card: want href=\"/artists/a-42\" and no /next/artists/; got:\n%s", sb)
	}

	var next bytes.Buffer
	nextCtx := WithArtistDetailBase(testCtx(t), "/next/artists")
	if err := DashboardActionCard(v, "").Render(nextCtx, &next); err != nil {
		t.Fatalf("render next: %v", err)
	}
	nb := next.String()
	if !strings.Contains(nb, `href="/next/artists/a-42"`) {
		t.Errorf("next card missing href=\"/next/artists/a-42\"; got:\n%s", nb)
	}
	if strings.Contains(nb, `href="/artists/a-42"`) {
		t.Errorf("next card leaked a bare href=\"/artists/a-42\"; got:\n%s", nb)
	}
}
