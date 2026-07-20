package image

import (
	"bytes"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func resolveTestDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	im := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, im, nil); err != nil {
		t.Fatalf("encoding: %v", err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), buf.Bytes(), 0o600); err != nil {
			t.Fatalf("writing %s: %v", n, err)
		}
	}
	return dir
}

func TestResolveFanartFiles(t *testing.T) {
	superset := DefaultFileNames["fanart"]

	tests := []struct {
		name  string
		files []string
		want  []string
	}{
		{
			name:  "backdrop series resolves to dense ordinals",
			files: []string{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg"},
			want:  []string{"backdrop.jpg", "backdrop2.jpg", "backdrop3.jpg"},
		},
		{
			name:  "primary only",
			files: []string{"fanart.jpg"},
			want:  []string{"fanart.jpg"},
		},
		{
			// The superset is walked in order and fanart.jpg comes first, so it
			// wins over backdrop.jpg. This is the scanner's pass-1 order; a
			// resolver that consulted the ACTIVE PROFILE's primary instead
			// would pick backdrop.jpg under Emby and derive different ordinals
			// than the scanner, whose next pass would then delete the rows.
			name:  "superset order wins over profile primary",
			files: []string{"fanart.jpg", "backdrop.jpg", "backdrop2.jpg"},
			want:  []string{"fanart.jpg"},
		},
		{
			// Pass 2. This is the shape a slot delete that failed partway
			// leaves behind, and the state the scanner used to call "no
			// fanart at all" -- so it is precisely the artist whose rows got
			// destroyed. A pass-1-only resolver walks straight past it.
			name:  "orphan numbered variant with no primary is adopted",
			files: []string{"backdrop2.jpg"},
			want:  []string{"backdrop2.jpg"},
		},
		{
			// Pass 2 must not override pass 1: fanart.jpg has a primary, so
			// it wins even though backdrop has more files.
			name:  "pass 1 wins over a richer pass 2 candidate",
			files: []string{"fanart.jpg", "backdrop2.jpg", "backdrop3.jpg"},
			want:  []string{"fanart.jpg"},
		},
		{
			name:  "unrelated images are ignored",
			files: []string{"folder.jpg", "logo.png"},
			want:  nil,
		},
		{
			name:  "empty directory",
			files: nil,
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := resolveTestDir(t, tc.files...)
			got, err := ResolveFanartFiles(dir, superset)
			if err != nil {
				t.Fatalf("ResolveFanartFiles: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d files %v, want %d %v", len(got), base(got), len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if filepath.Base(got[i]) != w {
					t.Errorf("ordinal %d = %s, want %s", i, filepath.Base(got[i]), w)
				}
			}
		})
	}
}

// TestResolveFanartFilesUnreadableDirErrors is the invariant test: an
// unreadable directory must be an ERROR, never an empty slice. A caller that
// received (nil, nil) here would conclude "this artist has no fanart" from a
// read it never completed, which is the assertion that destroyed the registry.
func TestResolveFanartFilesUnreadableDirErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := resolveTestDir(t, "backdrop.jpg")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	got, err := ResolveFanartFiles(dir, DefaultFileNames["fanart"])
	if err == nil {
		t.Fatalf("unreadable directory returned no error (got %v); 'cannot tell' must not read as 'no files'", got)
	}
	if got != nil {
		t.Errorf("unreadable directory returned files: %v", got)
	}
}

func base(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}
