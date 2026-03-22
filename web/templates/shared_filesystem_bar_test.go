package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestSharedFilesystemBarContent_WithWarnings(t *testing.T) {
	data := SharedFSBarData{
		HasOverlaps: true,
		Libraries:   []SharedFSBarLib{{Name: "Music", Path: "/music"}},
		ImageFetcherWarnings: []SharedFSBarWarning{
			{Platform: "emby", RiskLevel: "warn", Message: "Emby fetchers enabled"},
			{Platform: "jellyfin", RiskLevel: "critical", Message: "Jellyfin fetchers active"},
		},
	}

	var buf bytes.Buffer
	err := SharedFilesystemBarContent(data).Render(context.Background(), &buf)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	html := buf.String()

	// The bar should render when HasOverlaps is true and not dismissed.
	if !strings.Contains(html, "Shared filesystem detected") {
		t.Error("expected 'Shared filesystem detected' in rendered HTML")
	}

	// Check library name and path appear.
	if !strings.Contains(html, "Music") {
		t.Error("expected library name 'Music' in rendered HTML")
	}
	if !strings.Contains(html, "/music") {
		t.Error("expected library path '/music' in rendered HTML")
	}

	// Check warn-level warning renders with "Note:" prefix.
	if !strings.Contains(html, "Emby fetchers enabled") {
		t.Error("expected warn-level warning message in HTML")
	}
	if !strings.Contains(html, "Note:") {
		t.Error("expected 'Note:' prefix for warn-level warning")
	}

	// Check critical-level warning renders with "Action needed:" prefix.
	if !strings.Contains(html, "Jellyfin fetchers active") {
		t.Error("expected critical-level warning message in HTML")
	}
	if !strings.Contains(html, "Action needed:") {
		t.Error("expected 'Action needed:' prefix for critical-level warning")
	}

	// Check ARIA role for the warnings list.
	if !strings.Contains(html, `role="list"`) {
		t.Error("expected role='list' on warnings container")
	}
	if !strings.Contains(html, `aria-label="Image fetcher warnings"`) {
		t.Error("expected aria-label on warnings container")
	}
}

func TestSharedFilesystemBarContent_NoWarnings(t *testing.T) {
	data := SharedFSBarData{
		HasOverlaps: true,
		Libraries:   []SharedFSBarLib{{Name: "Music", Path: "/music"}},
	}

	var buf bytes.Buffer
	err := SharedFilesystemBarContent(data).Render(context.Background(), &buf)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	html := buf.String()

	// The bar renders but the warnings list should not appear.
	if !strings.Contains(html, "Shared filesystem detected") {
		t.Error("expected bar to render when HasOverlaps is true")
	}
	if strings.Contains(html, "Image fetcher warnings") {
		t.Error("expected no warnings list when ImageFetcherWarnings is empty")
	}
}

func TestSharedFilesystemBarContent_Dismissed(t *testing.T) {
	data := SharedFSBarData{
		HasOverlaps: true,
		Dismissed:   true,
		Libraries:   []SharedFSBarLib{{Name: "Music", Path: "/music"}},
		ImageFetcherWarnings: []SharedFSBarWarning{
			{Platform: "emby", RiskLevel: "warn", Message: "should not appear"},
		},
	}

	var buf bytes.Buffer
	err := SharedFilesystemBarContent(data).Render(context.Background(), &buf)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	html := buf.String()

	// Dismissed bar should render nothing -- no content, no warnings.
	if strings.Contains(html, "Shared filesystem detected") {
		t.Error("dismissed bar should not render content")
	}
	if strings.Contains(html, "should not appear") {
		t.Error("dismissed bar should not render warnings")
	}
}

func TestSharedFilesystemBarContent_NoOverlaps(t *testing.T) {
	data := SharedFSBarData{
		HasOverlaps: false,
	}

	var buf bytes.Buffer
	err := SharedFilesystemBarContent(data).Render(context.Background(), &buf)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if strings.Contains(buf.String(), "Shared filesystem detected") {
		t.Error("bar should not render when HasOverlaps is false")
	}
}
