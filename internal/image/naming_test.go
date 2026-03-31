package image

import (
	"testing"
)

func TestFileNamesForType(t *testing.T) {
	naming := map[string][]string{
		"thumb":  {"folder.jpg", "artist.jpg"},
		"fanart": {"fanart.jpg"},
		"logo":   {"logo.png"},
	}

	tests := []struct {
		imageType string
		want      int
	}{
		{"thumb", 2},
		{"fanart", 1},
		{"logo", 1},
		{"banner", 0},
		{"unknown", 0},
	}

	for _, tt := range tests {
		t.Run(tt.imageType, func(t *testing.T) {
			got := FileNamesForType(naming, tt.imageType)
			if len(got) != tt.want {
				t.Errorf("FileNamesForType(%q) returned %d names, want %d", tt.imageType, len(got), tt.want)
			}
		})
	}
}

func TestPrimaryFileName(t *testing.T) {
	naming := map[string][]string{
		"thumb":  {"folder.jpg", "artist.jpg"},
		"fanart": {"fanart.jpg"},
	}

	if got := PrimaryFileName(naming, "thumb"); got != "folder.jpg" {
		t.Errorf("PrimaryFileName(thumb) = %q, want folder.jpg", got)
	}
	if got := PrimaryFileName(naming, "fanart"); got != "fanart.jpg" {
		t.Errorf("PrimaryFileName(fanart) = %q, want fanart.jpg", got)
	}
	if got := PrimaryFileName(naming, "banner"); got != "" {
		t.Errorf("PrimaryFileName(banner) = %q, want empty", got)
	}
}

func TestImageTermFor(t *testing.T) {
	tests := []struct {
		slot    string
		profile string
		want    string
	}{
		// Kodi uses filesystem-centric names
		{"thumb", "Kodi", "Folder"},
		{"fanart", "Kodi", "Fanart"},
		{"logo", "Kodi", "Logo"},
		{"banner", "Kodi", "Banner"},
		// Kodi case-insensitive
		{"thumb", "kodi", "Folder"},
		{"fanart", "KODI", "Fanart"},
		// Plex shares Kodi terminology
		{"thumb", "Plex", "Folder"},
		{"fanart", "Plex", "Fanart"},
		// Emby uses API-centric names
		{"thumb", "Emby", "Primary"},
		{"fanart", "Emby", "Backdrop"},
		{"logo", "Emby", "Logo"},
		{"banner", "Emby", "Banner"},
		// Jellyfin shares Emby terminology
		{"thumb", "Jellyfin", "Primary"},
		{"fanart", "Jellyfin", "Backdrop"},
		// Case-insensitive for Emby/Jellyfin
		{"thumb", "emby", "Primary"},
		{"fanart", "jellyfin", "Backdrop"},
		// Custom and unknown profiles use default terms
		{"thumb", "Custom", "Thumbnail"},
		{"fanart", "Custom", "Fanart"},
		{"thumb", "", "Thumbnail"},
		{"fanart", "SomeUnknown", "Fanart"},
		// Unknown slot returns empty string
		{"unknown", "Kodi", ""},
		{"unknown", "Emby", ""},
		{"unknown", "", ""},
	}

	for _, tt := range tests {
		name := tt.slot + "/" + tt.profile
		t.Run(name, func(t *testing.T) {
			got := ImageTermFor(tt.slot, tt.profile)
			if got != tt.want {
				t.Errorf("ImageTermFor(%q, %q) = %q, want %q", tt.slot, tt.profile, got, tt.want)
			}
		})
	}
}

func TestImageTermWithAttribution(t *testing.T) {
	tests := []struct {
		slot    string
		profile string
		want    string
	}{
		{"fanart", "Emby", "Backdrop (Emby)"},
		{"fanart", "Kodi", "Fanart (Kodi)"},
		{"thumb", "Jellyfin", "Primary (Jellyfin)"},
		{"thumb", "Plex", "Folder (Plex)"},
		{"logo", "Emby", "Logo (Emby)"},
		// Unknown slot returns empty
		{"unknown", "Emby", ""},
	}

	for _, tt := range tests {
		name := tt.slot + "/" + tt.profile
		t.Run(name, func(t *testing.T) {
			got := ImageTermWithAttribution(tt.slot, tt.profile)
			if got != tt.want {
				t.Errorf("ImageTermWithAttribution(%q, %q) = %q, want %q", tt.slot, tt.profile, got, tt.want)
			}
		})
	}
}

func TestAllSlots(t *testing.T) {
	if len(AllSlots) != 4 {
		t.Fatalf("AllSlots has %d entries, want 4", len(AllSlots))
	}
	// Verify order: thumb, fanart, logo, banner
	expected := []string{"thumb", "fanart", "logo", "banner"}
	for i, s := range expected {
		if AllSlots[i] != s {
			t.Errorf("AllSlots[%d] = %q, want %q", i, AllSlots[i], s)
		}
	}
}

func TestDefaultFileNames(t *testing.T) {
	if len(DefaultFileNames["thumb"]) == 0 {
		t.Error("DefaultFileNames should have thumb entries")
	}
	if len(DefaultFileNames["logo"]) == 0 {
		t.Error("DefaultFileNames should have logo entries")
	}
}
