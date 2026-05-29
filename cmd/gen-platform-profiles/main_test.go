package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalSQL is a small inline fixture that exercises the parser without
// depending on the real migration file.
const minimalSQL = `
-- Built-in platform profiles.
INSERT OR IGNORE INTO platform_profiles (id, name, is_builtin, is_active, nfo_enabled, nfo_format, image_naming) VALUES
    ('emby',     'Emby',     1, 0, 1, 'kodi', '{"thumb":["folder.jpg"],"fanart":["backdrop.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}'),
    ('plex',     'Plex',     1, 0, 0, 'kodi', '{"thumb":["artist.jpg"],"fanart":["fanart.jpg"],"logo":["logo.png"],"banner":["banner.jpg"]}');
`

// TestParseProfiles_Fixture verifies that the parser extracts the expected
// rows from a small inline SQL fixture.
func TestParseProfiles_Fixture(t *testing.T) {
	profiles, err := parseProfiles([]byte(minimalSQL))
	if err != nil {
		t.Fatalf("parseProfiles: %v", err)
	}

	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profiles))
	}

	// Emby row
	emby := profiles[0]
	if emby.ID != "emby" {
		t.Errorf("profiles[0].ID = %q, want %q", emby.ID, "emby")
	}
	if emby.Name != "Emby" {
		t.Errorf("profiles[0].Name = %q, want %q", emby.Name, "Emby")
	}
	if !emby.NFOEnabled {
		t.Error("profiles[0].NFOEnabled: want true")
	}
	if got := emby.ImageNaming["fanart"]; len(got) != 1 || got[0] != "backdrop.jpg" {
		t.Errorf("profiles[0].ImageNaming[fanart] = %v, want [backdrop.jpg]", got)
	}
	if got := emby.ImageNaming["thumb"]; len(got) != 1 || got[0] != "folder.jpg" {
		t.Errorf("profiles[0].ImageNaming[thumb] = %v, want [folder.jpg]", got)
	}

	// Plex row (nfo_enabled == 0)
	plex := profiles[1]
	if plex.ID != "plex" {
		t.Errorf("profiles[1].ID = %q, want %q", plex.ID, "plex")
	}
	if plex.NFOEnabled {
		t.Error("profiles[1].NFOEnabled: want false (Plex has nfo_enabled=0)")
	}
	if got := plex.ImageNaming["thumb"]; len(got) != 1 || got[0] != "artist.jpg" {
		t.Errorf("profiles[1].ImageNaming[thumb] = %v, want [artist.jpg]", got)
	}
}

// TestParseProfiles_RealSourceFile verifies that the parser can extract all
// profiles from the actual SQL migration file it targets in production.
func TestParseProfiles_RealSourceFile(t *testing.T) {
	// The test runs from the package directory; walk up to the repo root.
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read source file %s: %v", srcPath, err)
	}

	profiles, err := parseProfiles(raw)
	if err != nil {
		t.Fatalf("parseProfiles: %v", err)
	}

	if len(profiles) == 0 {
		t.Fatal("expected at least one profile, got none")
	}

	// Build an id -> profile map for easy lookup.
	byID := make(map[string]profileRow, len(profiles))
	for _, p := range profiles {
		byID[p.ID] = p
	}

	// Known profiles that must be present.
	for _, id := range []string{"emby", "jellyfin", "kodi", "plex", "custom"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("expected profile %q to be present", id)
		}
	}

	// Emby and Jellyfin must have fanart=backdrop.jpg.
	for _, id := range []string{"emby", "jellyfin"} {
		p := byID[id]
		if got := p.ImageNaming["fanart"]; len(got) == 0 || got[0] != "backdrop.jpg" {
			t.Errorf("%s fanart: got %v, want [backdrop.jpg]", id, got)
		}
	}

	// Kodi, Plex, Custom must have fanart=fanart.jpg.
	for _, id := range []string{"kodi", "plex", "custom"} {
		p := byID[id]
		if got := p.ImageNaming["fanart"]; len(got) == 0 || got[0] != "fanart.jpg" {
			t.Errorf("%s fanart: got %v, want [fanart.jpg]", id, got)
		}
	}

	// Plex must have thumb=artist.jpg and NFO disabled.
	plex := byID["plex"]
	if got := plex.ImageNaming["thumb"]; len(got) == 0 || got[0] != "artist.jpg" {
		t.Errorf("plex thumb: got %v, want [artist.jpg]", got)
	}
	if plex.NFOEnabled {
		t.Error("plex: NFOEnabled should be false")
	}

	// All profiles except Plex must have NFO enabled.
	for _, id := range []string{"emby", "jellyfin", "kodi", "custom"} {
		p := byID[id]
		if !p.NFOEnabled {
			t.Errorf("%s: NFOEnabled should be true", id)
		}
	}
}

// TestParseProfiles_NoBlock confirms that a SQL source without the
// platform_profiles INSERT returns an error rather than silently emitting
// an empty table.
func TestParseProfiles_NoBlock(t *testing.T) {
	src := []byte("SELECT 1;\n")
	_, err := parseProfiles(src)
	if err == nil {
		t.Fatal("expected an error for a SQL source with no platform_profiles INSERT, got nil")
	}
}

// TestRenderTable_Fixture verifies that renderTable produces the expected
// Markdown table for a small set of profiles.
func TestRenderTable_Fixture(t *testing.T) {
	profiles := []profileRow{
		{
			Name:        "Emby",
			NFOEnabled:  true,
			ImageNaming: map[string][]string{"thumb": {"folder.jpg"}, "fanart": {"backdrop.jpg"}, "logo": {"logo.png"}, "banner": {"banner.jpg"}},
		},
		{
			Name:        "Plex",
			NFOEnabled:  false,
			ImageNaming: map[string][]string{"thumb": {"artist.jpg"}, "fanart": {"fanart.jpg"}, "logo": {"logo.png"}, "banner": {"banner.jpg"}},
		},
	}

	got := renderTable(profiles)

	// Must contain the header row.
	if !strings.Contains(got, "| Profile | Thumbnail | Fanart | Logo | Banner | NFO files |") {
		t.Errorf("missing header row in:\n%s", got)
	}

	// Emby row checks.
	if !strings.Contains(got, "| Emby |") {
		t.Error("missing Emby row")
	}
	if !strings.Contains(got, "`backdrop.jpg`") {
		t.Error("missing backdrop.jpg in Emby row")
	}
	if !strings.Contains(got, "| Yes |") {
		t.Error("missing 'Yes' NFO column for Emby")
	}

	// Plex row checks.
	if !strings.Contains(got, "| Plex |") {
		t.Error("missing Plex row")
	}
	if !strings.Contains(got, "`artist.jpg`") {
		t.Error("missing artist.jpg in Plex row")
	}
	if !strings.Contains(got, "| No |") {
		t.Error("missing 'No' NFO column for Plex")
	}
}

// TestImageCell covers the imageCell helper for zero, one, and multiple
// filenames.
func TestImageCell(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{nil, "-"},
		{[]string{}, "-"},
		{[]string{"folder.jpg"}, "`folder.jpg`"},
		{[]string{"folder.jpg", "artist.jpg"}, "`folder.jpg`, `artist.jpg`"},
	}
	for _, tc := range cases {
		if got := imageCell(tc.names); got != tc.want {
			t.Errorf("imageCell(%v) = %q, want %q", tc.names, got, tc.want)
		}
	}
}

// TestRun_Idempotent verifies that calling run twice produces no diff:
// the second call must detect that the file is already up to date and
// skip the write, leaving the content unchanged.
func TestRun_Idempotent(t *testing.T) {
	srcPath := filepath.Join("..", "..", defaultSourcePath)

	tmpDir := t.TempDir()
	outPath := filepath.Join(tmpDir, "platform-profiles.md")

	// First run: should write the file.
	if err := run(srcPath, outPath, false); err != nil {
		t.Fatalf("first run: %v", err)
	}

	content1, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read after first run: %v", err)
	}

	// Second run: should be a no-op.
	if err := run(srcPath, outPath, false); err != nil {
		t.Fatalf("second run: %v", err)
	}

	content2, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read after second run: %v", err)
	}

	if string(content1) != string(content2) {
		t.Errorf("content changed between runs (not idempotent):\nfirst:  %q\nsecond: %q", content1, content2)
	}
}

// TestRun_CheckMode verifies that -check mode returns nil when the file is
// already up to date, and returns an error when the file is stale.
func TestRun_CheckMode(t *testing.T) {
	srcPath := filepath.Join("..", "..", defaultSourcePath)
	tmpDir := t.TempDir()
	outPath := filepath.Join(tmpDir, "platform-profiles.md")

	// Write the file first.
	if err := run(srcPath, outPath, false); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	// Check mode should return nil (file is fresh).
	if err := run(srcPath, outPath, true); err != nil {
		t.Errorf("check on fresh file: expected nil, got %v", err)
	}

	// Corrupt the file and re-check; should return an error.
	if err := os.WriteFile(outPath, []byte("stale content"), 0o644); err != nil {
		t.Fatalf("corrupt file: %v", err)
	}
	if err := run(srcPath, outPath, true); err == nil {
		t.Error("check on stale file: expected error, got nil")
	}
}

// TestMain_HappyPath drives main() end-to-end through the flag parser so the
// CLI entry point is exercised.
func TestMain_HappyPath(t *testing.T) {
	dir := t.TempDir()

	// Write a minimal SQL fixture to a temp file.
	srcPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(srcPath, []byte(minimalSQL), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	outPath := filepath.Join(dir, "platform-profiles.md")

	// main() registers flags on and parses the global flag.CommandLine.
	// Save and restore both, and swap in a fresh FlagSet so the flags are not
	// redefined against the default CommandLine.
	oldArgs := os.Args
	oldFlags := flag.CommandLine
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldFlags
	})
	flag.CommandLine = flag.NewFlagSet("gen-platform-profiles", flag.ContinueOnError)
	os.Args = []string{"gen-platform-profiles", "-source", srcPath, "-output", outPath}

	main()

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("expected main() to write %s: %v", outPath, err)
	}
	if !strings.Contains(string(got), "`backdrop.jpg`") {
		t.Errorf("generated output missing backdrop.jpg:\n%s", got)
	}
	if !strings.Contains(string(got), "`artist.jpg`") {
		t.Errorf("generated output missing artist.jpg:\n%s", got)
	}
}

// TestRun_SourceNotFound covers the source-read error branch in run().
func TestRun_SourceNotFound(t *testing.T) {
	dir := t.TempDir()
	if err := run(filepath.Join(dir, "does-not-exist.sql"), filepath.Join(dir, "out.md"), false); err == nil {
		t.Fatal("expected an error for a missing source file, got nil")
	}
}
