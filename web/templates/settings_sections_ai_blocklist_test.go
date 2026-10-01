package templates

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func renderAIBlocklist(t *testing.T, v AIBlocklistView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := SectionAIBlocklist(SettingsData{AIBlocklist: v}).Render(testCtx(t), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestSectionAIBlocklist_States(t *testing.T) {
	ts := time.Date(2026, 9, 30, 12, 5, 0, 0, time.UTC)
	cases := []struct {
		name       string
		view       AIBlocklistView
		want       []string
		wantAbsent []string
	}{
		{
			name:       "loaded",
			view:       AIBlocklistView{Enabled: true, Loaded: true, Rules: 1234, Source: "lists.example/l.txt", LastFetch: ts, LastChecked: ts},
			want:       []string{`data-ai-list-state="loaded"`, "lists.example/l.txt", ">1234<", "2026-09-30 12:05 UTC", `id="ai-image-list-refresh"`, "/api/v1/images/ai-blocklist/refresh"},
			wantAbsent: []string{"last-error", "turned off"},
		},
		{
			name:       "not loaded",
			view:       AIBlocklistView{Enabled: true, Source: "lists.example/l.txt"},
			want:       []string{`data-ai-list-state="not-loaded"`, "Not loaded yet", "Never", `id="ai-image-list-refresh"`},
			wantAbsent: []string{`data-ai-list="rules"`},
		},
		{
			name:       "off",
			view:       AIBlocklistView{},
			want:       []string{`data-ai-list-state="off"`, "SW_AI_BLOCKLIST_URL", "turned off"},
			wantAbsent: []string{"ai-image-list-refresh", "hx-post"},
		},
		{
			name: "error",
			view: AIBlocklistView{Enabled: true, Source: "lists.example/l.txt", LastError: "The list server answered with HTTP 500."},
			want: []string{`data-ai-list="last-error"`, "The list server answered with HTTP 500."},
		},
		{
			name: "notice",
			view: AIBlocklistView{Enabled: true, Source: "lists.example/l.txt", NoticeKind: "rate_limited", NoticeSecs: 30},
			want: []string{`role="alert"`, "Try again in 30 seconds."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderAIBlocklist(t, tc.view)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			for _, w := range tc.wantAbsent {
				if strings.Contains(out, w) {
					t.Errorf("unexpected %q in output", w)
				}
			}
		})
	}
}

func TestSectionAIBlocklist_SourceRow(t *testing.T) {
	linked := renderAIBlocklist(t, AIBlocklistView{Enabled: true, Source: "raw.githubusercontent.com/o/r/main/l.txt", SourceRepo: "o/r"})
	for _, w := range []string{`href="https://github.com/o/r"`, `target="_blank"`, `rel="noopener noreferrer"`, ">o/r<"} {
		if !strings.Contains(linked, w) {
			t.Errorf("repo render missing %q in:\n%s", w, linked)
		}
	}
	if strings.Contains(linked, "raw.githubusercontent.com") {
		t.Error("repo render must not show the raw host")
	}
	plain := renderAIBlocklist(t, AIBlocklistView{Enabled: true, Source: "example.com/lists/ai.txt"})
	if !strings.Contains(plain, "example.com/lists/ai.txt") || strings.Contains(plain, "<a ") {
		t.Errorf("custom render must be plain text with no link:\n%s", plain)
	}
}
