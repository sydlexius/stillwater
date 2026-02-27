package image

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFanartFilename(t *testing.T) {
	tests := []struct {
		name          string
		primaryName   string
		index         int
		kodiNumbering bool
		want          string
	}{
		{"emby primary", "backdrop.jpg", 0, false, "backdrop.jpg"},
		{"emby second", "backdrop.jpg", 1, false, "backdrop2.jpg"},
		{"emby third", "backdrop.jpg", 2, false, "backdrop3.jpg"},
		{"kodi primary", "fanart.jpg", 0, true, "fanart.jpg"},
		{"kodi second", "fanart.jpg", 1, true, "fanart1.jpg"},
		{"kodi third", "fanart.jpg", 2, true, "fanart2.jpg"},
		{"plex primary", "fanart.jpg", 0, false, "fanart.jpg"},
		{"plex second", "fanart.jpg", 1, false, "fanart2.jpg"},
		{"png primary", "backdrop.png", 0, false, "backdrop.png"},
		{"png second", "backdrop.png", 1, false, "backdrop2.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FanartFilename(tt.primaryName, tt.index, tt.kodiNumbering)
			if got != tt.want {
				t.Errorf("FanartFilename(%q, %d, %v) = %q, want %q",
					tt.primaryName, tt.index, tt.kodiNumbering, got, tt.want)
			}
		})
	}
}

func TestDiscoverFanart(t *testing.T) {
	dir := t.TempDir()

	// Create test files
	for _, name := range []string{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg", "unrelated.jpg", "logo.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths := DiscoverFanart(dir, "backdrop.jpg")
	if len(paths) != 3 {
		t.Fatalf("expected 3 fanart files, got %d: %v", len(paths), paths)
	}

	wantBases := []string{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg"}
	for i, want := range wantBases {
		got := filepath.Base(paths[i])
		if got != want {
			t.Errorf("paths[%d] = %q, want %q", i, got, want)
		}
	}
}

func TestDiscoverFanart_KodiNaming(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths := DiscoverFanart(dir, "fanart.jpg")
	if len(paths) != 3 {
		t.Fatalf("expected 3 fanart files, got %d: %v", len(paths), paths)
	}

	wantBases := []string{"fanart.jpg", "fanart1.jpg", "fanart2.jpg"}
	for i, want := range wantBases {
		got := filepath.Base(paths[i])
		if got != want {
			t.Errorf("paths[%d] = %q, want %q", i, got, want)
		}
	}
}

func TestDiscoverFanart_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	paths := DiscoverFanart(dir, "backdrop.jpg")
	if len(paths) != 0 {
		t.Errorf("expected 0 fanart files, got %d", len(paths))
	}
}

func TestDiscoverFanart_MixedCase(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"backdrop.jpg", "Backdrop2.jpg", "BACKDROP3.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths := DiscoverFanart(dir, "backdrop.jpg")
	if len(paths) != 3 {
		t.Fatalf("expected 3 fanart files (mixed case), got %d: %v", len(paths), paths)
	}
}

func TestMaxFanartIndex(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		primary string
		want    int
	}{
		{"empty dir", nil, "backdrop.jpg", -1},
		{"primary only", []string{"backdrop.jpg"}, "backdrop.jpg", 0},
		{"primary plus numbered", []string{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg"}, "backdrop.jpg", 3},
		{"gap in numbering", []string{"backdrop.jpg", "backdrop5.jpg"}, "backdrop.jpg", 5},
		{"only high numbered", []string{"fanart3.jpg"}, "fanart.jpg", 3},
		{"unrelated files only", []string{"logo.png", "folder.jpg"}, "backdrop.jpg", -1},
		{"mixed case", []string{"Backdrop.jpg", "BACKDROP2.png"}, "backdrop.jpg", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := MaxFanartIndex(dir, tt.primary)
			if got != tt.want {
				t.Errorf("MaxFanartIndex() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDiscoverFanart_DuplicateExtension(t *testing.T) {
	dir := t.TempDir()

	// Both backdrop.jpg and backdrop.png exist; should only return one (prefer .jpg match)
	for _, name := range []string{"backdrop.jpg", "backdrop.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths := DiscoverFanart(dir, "backdrop.jpg")
	if len(paths) != 1 {
		t.Fatalf("expected 1 fanart file (dedup), got %d: %v", len(paths), paths)
	}
	if filepath.Base(paths[0]) != "backdrop.jpg" {
		t.Errorf("expected backdrop.jpg (preferred ext), got %q", filepath.Base(paths[0]))
	}
}

func TestDiscoverFanart_DuplicateNumbered(t *testing.T) {
	dir := t.TempDir()

	// backdrop2.jpg and backdrop2.png both exist at numeric index 2
	for _, name := range []string{"backdrop.jpg", "backdrop2.jpg", "backdrop2.png"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths := DiscoverFanart(dir, "backdrop.jpg")
	if len(paths) != 2 {
		t.Fatalf("expected 2 fanart files (primary + one numbered), got %d: %v", len(paths), paths)
	}
	if filepath.Base(paths[1]) != "backdrop2.jpg" {
		t.Errorf("expected backdrop2.jpg (preferred ext), got %q", filepath.Base(paths[1]))
	}
}

func TestDiscoverFanart_AlternateExtension(t *testing.T) {
	dir := t.TempDir()

	// Primary is backdrop.jpg but actual file is backdrop.png
	if err := os.WriteFile(filepath.Join(dir, "backdrop.png"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths := DiscoverFanart(dir, "backdrop.jpg")
	if len(paths) != 1 {
		t.Fatalf("expected 1 fanart file (alternate ext), got %d: %v", len(paths), paths)
	}
	if filepath.Base(paths[0]) != "backdrop.png" {
		t.Errorf("expected backdrop.png, got %q", filepath.Base(paths[0]))
	}
}
