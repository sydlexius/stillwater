package static

import (
	"io/fs"
	"testing"
)

func TestFS_ContainsExpectedFiles(t *testing.T) {
	expected := []string{
		"js/htmx.min.js",
		"css/cropper.min.css",
		"css/design-tokens.css",
		"site.webmanifest",
		"img/favicon.svg",
	}

	for _, path := range expected {
		t.Run(path, func(t *testing.T) {
			f, err := FS.Open(path)
			if err != nil {
				t.Fatalf("failed to open %s: %v", path, err)
			}
			defer f.Close()

			info, err := f.Stat()
			if err != nil {
				t.Fatalf("failed to stat %s: %v", path, err)
			}
			if info.Size() == 0 {
				t.Fatalf("file %s is empty", path)
			}
		})
	}
}

func TestFS_WalkDir(t *testing.T) {
	var count int
	err := fs.WalkDir(FS, ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir failed: %v", err)
	}
	// The static directory contains JS, CSS, fonts, images, and manifest.
	// There should be a reasonable number of files embedded.
	if count < 10 {
		t.Fatalf("expected at least 10 embedded files, got %d", count)
	}
}
