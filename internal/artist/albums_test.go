package artist

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListLocalAlbums(t *testing.T) {
	dir := t.TempDir()

	// Create some album directories.
	for _, name := range []string{"OK Computer", "The Bends", "Kid A", ".hidden"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Create a regular file (should be skipped).
	if err := os.WriteFile(filepath.Join(dir, "cover.jpg"), []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}

	albums := ListLocalAlbums(dir)
	if len(albums) != 3 {
		t.Fatalf("expected 3 albums, got %d: %v", len(albums), albums)
	}
	// Should be sorted.
	expected := []string{"Kid A", "OK Computer", "The Bends"}
	for i, want := range expected {
		if albums[i] != want {
			t.Errorf("albums[%d] = %q, want %q", i, albums[i], want)
		}
	}
}

func TestListLocalAlbums_NonexistentPath(t *testing.T) {
	albums := ListLocalAlbums("/nonexistent/path/12345")
	if albums != nil {
		t.Errorf("expected nil for nonexistent path, got %v", albums)
	}
}

func TestNormalizeAlbumName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"OK Computer", "ok computer"},
		{"ok computer", "ok computer"},
		{"Kid A (Deluxe Edition)", "kid a"},
		{"In Rainbows (Disc 2)", "in rainbows"},
		{"Hail to the Thief", "hail to the thief"},
		{"...Like Clockwork", "like clockwork"},
		{"The King Is Dead!", "the king is dead"},
		{"  extra  spaces  ", "extra spaces"},
		{"", ""},
	}
	for _, c := range cases {
		got := normalizeAlbumName(c.input)
		if got != c.want {
			t.Errorf("normalizeAlbumName(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestCompareAlbums(t *testing.T) {
	local := []string{"OK Computer", "The Bends", "Kid A", "Live at Glastonbury"}
	remote := []string{"ok computer", "The Bends", "Kid A (Deluxe Edition)", "Pablo Honey", "Amnesiac"}

	comp := CompareAlbums(local, remote)

	if comp.MatchCount != 3 {
		t.Errorf("expected 3 matches, got %d", comp.MatchCount)
	}
	if comp.LocalCount != 4 {
		t.Errorf("expected LocalCount 4, got %d", comp.LocalCount)
	}
	if comp.RemoteCount != 5 {
		t.Errorf("expected RemoteCount 5, got %d", comp.RemoteCount)
	}
	if comp.MatchPercent != 75 {
		t.Errorf("expected MatchPercent 75, got %d", comp.MatchPercent)
	}
	if len(comp.LocalOnly) != 1 {
		t.Errorf("expected 1 local-only, got %d: %v", len(comp.LocalOnly), comp.LocalOnly)
	}
	if len(comp.RemoteOnly) != 2 {
		t.Errorf("expected 2 remote-only, got %d: %v", len(comp.RemoteOnly), comp.RemoteOnly)
	}
}

func TestCompareAlbums_Empty(t *testing.T) {
	comp := CompareAlbums(nil, nil)
	if comp.MatchCount != 0 || comp.MatchPercent != 0 {
		t.Errorf("expected zero values for empty inputs, got match=%d pct=%d",
			comp.MatchCount, comp.MatchPercent)
	}
}

func TestCompareAlbums_AllMatch(t *testing.T) {
	local := []string{"Album One", "Album Two"}
	remote := []string{"album one", "Album Two"}

	comp := CompareAlbums(local, remote)
	if comp.MatchPercent != 100 {
		t.Errorf("expected 100%% match, got %d%%", comp.MatchPercent)
	}
	if len(comp.LocalOnly) != 0 {
		t.Errorf("expected no local-only, got %v", comp.LocalOnly)
	}
}

func TestCompareAlbums_NoMatch(t *testing.T) {
	local := []string{"Album X", "Album Y"}
	remote := []string{"Album A", "Album B"}

	comp := CompareAlbums(local, remote)
	if comp.MatchPercent != 0 {
		t.Errorf("expected 0%% match, got %d%%", comp.MatchPercent)
	}
	if comp.MatchCount != 0 {
		t.Errorf("expected 0 matches, got %d", comp.MatchCount)
	}
}
