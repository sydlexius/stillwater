package templates

// settings_s3_golden_test.go -- regression gate for M55 issue #1809 (S3
// Integrations: the Connections + Automation tab cards -- Servers, Webhooks,
// Notification badges, API tokens -- extracted into shared Section* templ
// funcs).
//
// Two complementary gates, both fully self-contained (no temp files, never
// skipped -- so CI always exercises them):
//
//  1. TestS3_PageComposesSections renders SettingsPage and asserts each
//     extracted Section* func's output appears verbatim in the page. This is
//     the durable invariant the original extraction guaranteed: the stable
//     chrome composes the SAME section bodies the shared funcs emit, so the
//     page and the future next/ chrome cannot silently diverge. (The original
//     before/after byte-diff that proved the extraction was a one-shot,
//     temp-file check that t.Skipf'd in CI; this replaces it with an
//     always-run equivalent.)
//
//  2. The per-section golden tests (TestSection*_S3_Golden) render each Section*
//     func in isolation and compare against
//     web/templates/testdata/section_*.golden.html. Regenerate with -update.
//
// Shared test helpers (updateGolden flag, goldenPath, checkOrUpdateGolden,
// s1GoldenDir, testCtx) are defined in settings_s1_golden_test.go /
// helpers_test.go and reused here.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// fixedTokenTime is a deterministic created-at string for API-token fixtures so
// the committed per-section goldens are stable across runs.  The card formats
// this value through tf(ctx, "settings.api_tokens.created_at", ...), which
// renders the raw string verbatim.
const fixedTokenTime = "2026-01-02T03:04:05Z"

// threeConnections provides one populated connection per service type so the
// Servers card renders an Emby, Jellyfin, and Lidarr card.  The managed-files
// toggle differs across them to exercise both states.
var threeConnections = []connection.Connection{
	{
		ID:                       "conn-emby",
		Name:                     "Living Room Emby",
		Type:                     "emby",
		URL:                      "http://192.168.1.100:8096",
		Enabled:                  true,
		Status:                   "ok",
		FeatureManageServerFiles: true,
	},
	{
		ID:      "conn-jellyfin",
		Name:    "Office Jellyfin",
		Type:    "jellyfin",
		URL:     "http://192.168.1.101:8096",
		Enabled: true,
		Status:  "ok",
	},
	{
		ID:      "conn-lidarr",
		Name:    "Lidarr",
		Type:    "lidarr",
		URL:     "http://192.168.1.102:8686",
		Enabled: false,
		Status:  "error",
	},
}

// twoWebhooks provides populated Webhooks data for the Webhooks card.
var twoWebhooks = []webhook.Webhook{
	{ID: "wh-1", Name: "Discord alerts", Type: "discord", URL: "https://discord.example/hook", Enabled: true},
	{ID: "wh-2", Name: "Slack ops", Type: "slack", URL: "https://slack.example/hook", Enabled: false},
}

// activeToken and revokedToken exercise the active-list and archived-list
// branches of the API tokens card (plus the scope pills and revoked summary).
var activeToken = auth.APIToken{
	ID:        "tok-active",
	Name:      "CI read token",
	Scopes:    "read,write",
	Status:    auth.TokenStatusActive,
	CreatedAt: fixedTokenTime,
}

var revokedToken = auth.APIToken{
	ID:        "tok-revoked",
	Name:      "Old token",
	Scopes:    "read",
	Status:    auth.TokenStatusRevoked,
	CreatedAt: fixedTokenTime,
}

// twoTokens spans one active and one revoked token so the card renders both the
// active row and the collapsed archive section.
var twoTokens = []auth.APIToken{activeToken, revokedToken}

// TestS3_PageComposesSections asserts that SettingsPage embeds each extracted
// Section* func's rendered output verbatim.  If a future edit stops the page
// from composing a shared section (or forks its markup inline), the substring
// check fails -- the always-run replacement for the old temp-file byte-diff,
// which could silently t.Skipf in CI when the /tmp artifacts were absent.
func TestS3_PageComposesSections(t *testing.T) {
	ctx := testCtx(t)
	// One fixture populating BOTH the Connections and Automation clusters.
	// The Automation panel renders even though Connections is the active tab
	// (it is only visually hidden via a class), so all four section bodies
	// are present in the page output.
	data := SettingsData{
		ActiveTab:            TabConnections,
		Connections:          threeConnections,
		Webhooks:             twoWebhooks,
		BadgeEnabled:         true,
		BadgeSeverityError:   true,
		BadgeSeverityWarning: true,
		BadgeSeverityInfo:    true,
		APITokens:            twoTokens,
	}
	var page bytes.Buffer
	if err := SettingsPage(AssetPaths{}, data).Render(ctx, &page); err != nil {
		t.Fatalf("render SettingsPage: %v", err)
	}
	pageStr := page.String()

	check := func(name string, render func(context.Context, io.Writer) error) {
		t.Helper()
		var b bytes.Buffer
		if err := render(ctx, &b); err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		if !strings.Contains(pageStr, b.String()) {
			t.Errorf("SettingsPage output does not contain %s render verbatim -- the page no longer composes the shared section", name)
		}
	}

	check("SectionServers", SectionServers(data).Render)
	check("SectionWebhooks", SectionWebhooks(data).Render)
	check("SectionNotificationBadges", SectionNotificationBadges(data).Render)
	check("SectionAPITokens", SectionAPITokens(data).Render)
}

// --- Per-section golden tests ------------------------------------------------

func TestSectionServers_Populated_S3_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Connections: threeConnections}
	var buf bytes.Buffer
	if err := SectionServers(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "servers_populated", buf.String())
}

func TestSectionServers_Empty_S3_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Connections: nil}
	var buf bytes.Buffer
	if err := SectionServers(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "servers_empty", buf.String())
}

func TestSectionWebhooks_Populated_S3_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Webhooks: twoWebhooks}
	var buf bytes.Buffer
	if err := SectionWebhooks(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "webhooks_populated", buf.String())
}

func TestSectionWebhooks_Empty_S3_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{Webhooks: nil}
	var buf bytes.Buffer
	if err := SectionWebhooks(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "webhooks_empty", buf.String())
}

func TestSectionNotificationBadges_Enabled_S3_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{
		BadgeEnabled:         true,
		BadgeSeverityError:   true,
		BadgeSeverityWarning: true,
		BadgeSeverityInfo:    true,
	}
	var buf bytes.Buffer
	if err := SectionNotificationBadges(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "notif_badges_enabled", buf.String())
}

func TestSectionNotificationBadges_Disabled_S3_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{
		BadgeEnabled:         false,
		BadgeSeverityError:   false,
		BadgeSeverityWarning: false,
		BadgeSeverityInfo:    false,
	}
	var buf bytes.Buffer
	if err := SectionNotificationBadges(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "notif_badges_disabled", buf.String())
}

func TestSectionAPITokens_Populated_S3_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{APITokens: twoTokens}
	var buf bytes.Buffer
	if err := SectionAPITokens(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "api_tokens_populated", buf.String())
}

func TestSectionAPITokens_Empty_S3_Golden(t *testing.T) {
	ctx := testCtx(t)
	data := SettingsData{APITokens: nil}
	var buf bytes.Buffer
	if err := SectionAPITokens(data).Render(ctx, &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	checkOrUpdateGolden(t, "api_tokens_empty", buf.String())
}
