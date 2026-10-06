package templates

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
)

func TestHistoryProducerLabel(t *testing.T) {
	cases := []struct{ producer, want string }{
		{"", "Value source not recorded"},
		{"operator", "Value: set by a user"},
		{"provider:lastfm", "Value: from Last.fm"},
		{"provider:", "Value: from a provider (not named)"},
		{"provider:identify_connection", "Value: identify (platform match)"},
		{"provider:identify_album", "Value: identify (album match)"},
		{"provider:identify_name", "Value: identify (name match)"},
		{"provider:spotify", "Value: from Spotify"},
		{"provider:allmusic", "Value: from AllMusic"},
		{"provider:somenewprovider", "Value: from somenewprovider"},
		{"platform:emby", "Value: from Emby"},
		{"platform:plex", "Value: from plex"},
		{"restore", "Value: restored an earlier value"},
		{"nfo", "Value: from the NFO file"},
		{"filesystem", "Value: observed on disk"},
		{"rule:nfo_exists", "Value: from rule nfo_exists"},
		// Unknown tokens are shown as-is, never "Value: from ".
		{"rule:", "rule:"},
		{"platform:", "platform:"},
		{"mystery", "mystery"},
	}
	ctx := testCtx(t)
	for _, tc := range cases {
		got := historyProducerLabel(ctx, tc.producer)
		if got != tc.want {
			t.Errorf("historyProducerLabel(%q) = %q, want %q", tc.producer, got, tc.want)
		}
		if strings.HasSuffix(got, "from ") {
			t.Errorf("historyProducerLabel(%q) = %q ends in a dangling \"from \"", tc.producer, got)
		}
	}
	for _, p := range []string{"provider:identify_connection", "provider:identify_album", "provider:identify_name"} {
		if got := historyProducerLabel(ctx, p); strings.Contains(got, "from") {
			t.Errorf("%q reads as a provider: %q", p, got)
		}
	}
}

func TestHistoryProducerShown(t *testing.T) {
	cases := []struct {
		name                    string
		field, source, producer string
		want                    bool
	}{
		{"unrecorded manual is never hidden", "biography", "manual", "", true},
		{"unrecorded scan is never hidden", "biography", "scan", "", true},
		{"unrecorded revert is never hidden", "biography", "revert", "", true},
		{"operator on manual", "biography", "manual", "operator", true},
		{"rule producer on manual source", "biography", "manual", "rule:x", true},
		{"rule mirror repeats the badge", "biography", "rule:x", "rule:x", false},
		{"identify mirror repeats the badge", "mbid", "provider:identify_album", "provider:identify_album", false},
		{"undo is restore", "biography", "revert", "restore", false},
		{"restore under another source", "biography", "manual", "restore", true},
		{"rule_fix is an audit message", "rule_fix", "rule:nfo_exists", "rule:nfo_exists", false},
		{"rule_fix unrecorded stays hidden", "rule_fix", "rule:nfo_exists", "", false},
	}
	for _, tc := range cases {
		c := artist.MetadataChange{Field: tc.field, Source: tc.source, Producer: tc.producer}
		if got := historyProducerShown(c); got != tc.want {
			t.Errorf("%s: shown = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func renderActivityRow(t *testing.T, field, source, producer string) string {
	t.Helper()
	c := artist.MetadataChangeWithArtist{
		MetadataChange: artist.MetadataChange{
			ID: "c-1", ArtistID: "a-1", Field: field, NewValue: "x",
			Source: source, Producer: producer, CreatedAt: time.Now().UTC(),
		},
		ArtistName: "Test Artist",
	}
	var buf bytes.Buffer
	if err := ActivityChangeRowFragment(c, "").Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestActivityRow_ProducerChip(t *testing.T) {
	legacy := renderActivityRow(t, "biography", "manual", "")
	if !strings.Contains(legacy, "Value source not recorded") {
		t.Error("legacy row must say the value source was not recorded")
	}
	if strings.Contains(legacy, "set by a user") {
		t.Error("legacy row must never read as set by a user")
	}
	if !strings.Contains(renderActivityRow(t, "biography", "manual", "provider:lastfm"), "Value: from Last.fm") {
		t.Error("stamped provider row must name the provider")
	}
	if strings.Contains(renderActivityRow(t, "biography", "revert", "restore"), "Value") {
		t.Error("an undo row repeats its badge and must carry no value chip")
	}
}

func TestHistoryProducerChipClass_NotRecordedIsItalic(t *testing.T) {
	if !strings.Contains(historyProducerChipClass(""), "italic") {
		t.Error("the not-recorded chip must be italic")
	}
	if strings.Contains(historyProducerChipClass("operator"), "italic") {
		t.Error("a recorded producer must not be italic")
	}
}

// The Set-to value text sits on the translucent light card. Pin the
// AA-passing classes as a cheap drift guard (#3078). A class-name pin is weak
// evidence: the rendered contrast check lives in
// tests/a11y/history-producer.spec.js, and that is what proves AA.
func TestActivityRow_ContrastSafeClasses(t *testing.T) {
	body := renderActivityRow(t, "genres", "manual", "operator")
	if strings.Contains(body, "text-green-600") {
		t.Error("value text must not use text-green-600 (3.19:1 on the light card)")
	}
	if !strings.Contains(body, `text-green-800 dark:text-green-400">Set to`) {
		t.Error("value text must use text-green-800 dark:text-green-400 " +
			"(the rendered AA check lives in tests/a11y/history-producer.spec.js)")
	}
}

// The three identify tiers are match tiers, never a provider name, and each
// maps to its own label.
func TestHistorySourceLabel_IdentifyTiers(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	for source, want := range map[string]string{
		"provider:identify_connection": "Identify: platform match",
		"provider:identify_album":      "Identify: album match",
		"provider:identify_name":       "Identify: name match",
	} {
		if got := historySourceLabel(ctx, source); got != want {
			t.Errorf("historySourceLabel(%q) = %q, want %q", source, got, want)
		}
	}
}
