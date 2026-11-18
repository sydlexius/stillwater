package image

import (
	"bytes"
	"context"
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
			got, err := ResolveFanartFiles(context.Background(), dir, superset)
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

	got, err := ResolveFanartFiles(context.Background(), dir, DefaultFileNames["fanart"])
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

// TestResolveFanartFilesNamed_UsesOneDirectorySnapshot pins that the chosen
// candidate and the ordinal list both come from the single directory read. The
// seam mutates the directory right after the first read; a resolver that reads
// again (pass 1's DiscoverFanart, or pass 2 once per candidate) would see the
// mutation and mix two snapshots.
func TestResolveFanartFilesNamed_UsesOneDirectorySnapshot(t *testing.T) {
	// Not parallel: it swaps a package variable.
	run := func(t *testing.T, initial []string, mutate func(dir string) error) (string, []string, int) {
		t.Helper()
		dir := resolveTestDir(t, initial...)
		reads := 0
		orig := osReadDir
		osReadDir = func(d string) ([]os.DirEntry, error) {
			entries, err := orig(d)
			reads++
			if reads == 1 {
				// Runs in the resolver's worker goroutine, so it must not
				// call t.Fatal; hand the failure back as the read error and
				// let the test goroutine report it after the resolver returns.
				if mErr := mutate(dir); mErr != nil {
					return nil, mErr
				}
			}
			return entries, err
		}
		t.Cleanup(func() { osReadDir = orig })
		name, paths, err := ResolveFanartFilesNamed(context.Background(), dir, []string{"backdrop.jpg", "fanart.jpg"})
		if err != nil {
			t.Fatalf("ResolveFanartFilesNamed: %v", err)
		}
		return name, paths, reads
	}

	t.Run("pass 1: a primary removed after the read is still reported", func(t *testing.T) {
		name, paths, reads := run(t, []string{"fanart.jpg"}, func(dir string) error {
			return os.Remove(filepath.Join(dir, "fanart.jpg"))
		})
		if name != "fanart.jpg" || len(paths) != 1 || filepath.Base(paths[0]) != "fanart.jpg" {
			t.Errorf("got (%q, %v), want the first snapshot's fanart.jpg", name, paths)
		}
		if reads != 1 {
			t.Errorf("directory read %d times, want 1", reads)
		}
	})

	t.Run("pass 2: a file added after the read is not picked up", func(t *testing.T) {
		// Prepare the bytes up front, on the test goroutine.
		src := resolveTestDir(t, "x.jpg")
		b, err := os.ReadFile(filepath.Join(src, "x.jpg"))
		if err != nil {
			t.Fatal(err)
		}
		name, paths, reads := run(t, []string{"fanart2.jpg"}, func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "fanart3.jpg"), b, 0o600)
		})
		if name != "fanart.jpg" || len(paths) != 1 || filepath.Base(paths[0]) != "fanart2.jpg" {
			t.Errorf("got (%q, %v), want only the first snapshot's fanart2.jpg", name, paths)
		}
		if reads != 1 {
			t.Errorf("directory read %d times, want 1", reads)
		}
	})
}
