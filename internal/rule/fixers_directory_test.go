package rule

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
)

func TestDirectoryRenameFixer_Fix(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fixer := NewDirectoryRenameFixer(nil, logger)

	t.Run("successful rename", func(t *testing.T) {
		tmp := t.TempDir()
		oldPath := filepath.Join(tmp, "Old Name")
		if err := os.MkdirAll(oldPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(oldPath, "artist.nfo"), []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}

		a := &artist.Artist{Name: "New Name", Path: oldPath}
		v := &Violation{
			RuleID: RuleDirectoryNameMismatch,
			Config: RuleConfig{ArticleMode: "prefix"},
		}

		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if !result.Fixed {
			t.Errorf("Fixed = false, want true; message: %s", result.Message)
		}
		if a.Path != filepath.Join(tmp, "New Name") {
			t.Errorf("a.Path = %q, want %q", a.Path, filepath.Join(tmp, "New Name"))
		}

		// Verify file was moved.
		data, err := os.ReadFile(filepath.Join(a.Path, "artist.nfo"))
		if err != nil {
			t.Fatalf("reading moved file: %v", err)
		}
		if string(data) != "test" {
			t.Errorf("file content = %q, want %q", data, "test")
		}
	})

	t.Run("destination exists", func(t *testing.T) {
		tmp := t.TempDir()
		oldPath := filepath.Join(tmp, "Old Name")
		newPath := filepath.Join(tmp, "Existing")
		if err := os.MkdirAll(oldPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(newPath, 0o755); err != nil {
			t.Fatal(err)
		}

		a := &artist.Artist{Name: "Existing", Path: oldPath}
		v := &Violation{
			RuleID: RuleDirectoryNameMismatch,
			Config: RuleConfig{ArticleMode: "prefix"},
		}

		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if result.Fixed {
			t.Error("Fixed = true, want false when destination exists")
		}
	})

	t.Run("empty path", func(t *testing.T) {
		a := &artist.Artist{Name: "Test", Path: ""}
		v := &Violation{
			RuleID: RuleDirectoryNameMismatch,
			Config: RuleConfig{ArticleMode: "prefix"},
		}

		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if result.Fixed {
			t.Error("Fixed = true, want false for empty path")
		}
	})
}
