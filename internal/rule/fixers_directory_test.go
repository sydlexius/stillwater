package rule

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/library"
)

func TestDirectoryRenameFixer_Fix(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fixer := NewDirectoryRenameFixer(nonSharedFSCheck(), logger)

	t.Run("successful rename", func(t *testing.T) {
		tmp := t.TempDir()
		oldPath := filepath.Join(tmp, "Old Name")
		if err := os.MkdirAll(oldPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(oldPath, "artist.nfo"), []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}

		a := &artist.Artist{Name: "New Name", Path: oldPath, LibraryID: "lib-test"}
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

		a := &artist.Artist{Name: "Existing", Path: oldPath, LibraryID: "lib-test"}
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

func TestDirectoryRenameFixer_SharedFilesystem(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	libSvc := library.NewService(db)
	ctx := context.Background()

	// Create a library with shared_fs_status = suspected.
	dir := t.TempDir()
	lib := &library.Library{
		Name:   "Shared Music",
		Path:   dir,
		Type:   library.TypeRegular,
		Source: library.SourceManual,
	}
	if err := libSvc.Create(ctx, lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}
	if err := libSvc.SetSharedFSStatus(ctx, lib.ID, library.SharedFSSuspected, "", ""); err != nil {
		t.Fatalf("setting shared_fs_status: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fsCheck := NewSharedFSCheck(libSvc, logger)
	fixer := NewDirectoryRenameFixer(fsCheck, logger)

	oldPath := filepath.Join(dir, "Old Name")
	if err := os.MkdirAll(oldPath, 0o755); err != nil {
		t.Fatal(err)
	}

	a := &artist.Artist{
		Name:      "New Name",
		Path:      oldPath,
		LibraryID: lib.ID,
	}
	v := &Violation{
		RuleID: RuleDirectoryNameMismatch,
		Config: RuleConfig{ArticleMode: "prefix"},
	}

	result, err := fixer.Fix(ctx, a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if result.Fixed {
		t.Error("Fixed = true, want false for shared-filesystem library")
	}
	if !strings.Contains(result.Message, "shared-filesystem") {
		t.Errorf("expected message to mention shared-filesystem, got: %s", result.Message)
	}

	// Verify the directory was NOT renamed.
	if _, statErr := os.Stat(oldPath); os.IsNotExist(statErr) {
		t.Error("original directory was renamed; expected it to remain unchanged")
	}
}
