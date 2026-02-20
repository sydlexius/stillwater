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

func TestDefaultFileNames(t *testing.T) {
	if len(DefaultFileNames["thumb"]) == 0 {
		t.Error("DefaultFileNames should have thumb entries")
	}
	if len(DefaultFileNames["logo"]) == 0 {
		t.Error("DefaultFileNames should have logo entries")
	}
}
