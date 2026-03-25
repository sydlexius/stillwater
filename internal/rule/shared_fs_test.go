package rule

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/library"
)

// stubLibQuerier implements LibraryQuerier for testing.
type stubLibQuerier struct {
	lib *library.Library
	err error
}

func (s *stubLibQuerier) GetByID(_ context.Context, _ string) (*library.Library, error) {
	return s.lib, s.err
}

func TestSharedFSCheck_IsShared(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	t.Run("shared library returns true", func(t *testing.T) {
		check := NewSharedFSCheck(&stubLibQuerier{
			lib: &library.Library{SharedFSStatus: library.SharedFSSuspected},
		}, logger)
		a := &artist.Artist{LibraryID: "lib-1"}
		if !check.IsShared(context.Background(), a) {
			t.Error("IsShared = false, want true for shared library")
		}
	})

	t.Run("confirmed library returns true", func(t *testing.T) {
		check := NewSharedFSCheck(&stubLibQuerier{
			lib: &library.Library{SharedFSStatus: library.SharedFSConfirmed},
		}, logger)
		a := &artist.Artist{LibraryID: "lib-1"}
		if !check.IsShared(context.Background(), a) {
			t.Error("IsShared = false, want true for confirmed library")
		}
	})

	t.Run("non-shared library returns false", func(t *testing.T) {
		check := NewSharedFSCheck(&stubLibQuerier{
			lib: &library.Library{SharedFSStatus: library.SharedFSNone},
		}, logger)
		a := &artist.Artist{LibraryID: "lib-1"}
		if check.IsShared(context.Background(), a) {
			t.Error("IsShared = true, want false for non-shared library")
		}
	})

	t.Run("empty library ID returns true (fail-closed)", func(t *testing.T) {
		check := NewSharedFSCheck(&stubLibQuerier{
			lib: &library.Library{SharedFSStatus: library.SharedFSNone},
		}, logger)
		a := &artist.Artist{LibraryID: ""}
		if !check.IsShared(context.Background(), a) {
			t.Error("IsShared = false, want true for empty library ID")
		}
	})

	t.Run("nil querier returns true (fail-closed)", func(t *testing.T) {
		check := NewSharedFSCheck(nil, logger)
		a := &artist.Artist{LibraryID: "lib-1"}
		if !check.IsShared(context.Background(), a) {
			t.Error("IsShared = false, want true for nil querier")
		}
	})

	t.Run("DB error returns true (fail-closed)", func(t *testing.T) {
		check := NewSharedFSCheck(&stubLibQuerier{
			err: errors.New("db connection lost"),
		}, logger)
		a := &artist.Artist{LibraryID: "lib-1"}
		if !check.IsShared(context.Background(), a) {
			t.Error("IsShared = false, want true on DB error")
		}
	})

	t.Run("nil receiver returns true (fail-closed)", func(t *testing.T) {
		var check *SharedFSCheck
		a := &artist.Artist{LibraryID: "lib-1"}
		if !check.IsShared(context.Background(), a) {
			t.Error("IsShared = false, want true for nil receiver")
		}
	})
}

func TestNFOFixer_SharedFS_Skips(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	check := NewSharedFSCheck(&stubLibQuerier{
		lib: &library.Library{SharedFSStatus: library.SharedFSSuspected},
	}, logger)
	fixer := NewNFOFixer(nil, nil, check, nil)
	a := &artist.Artist{Name: "Test", Path: t.TempDir(), LibraryID: "lib-1"}
	v := &Violation{RuleID: RuleNFOExists}
	result, err := fixer.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if result.Fixed {
		t.Error("Fixed = true, want false")
	}
	if !strings.Contains(result.Message, "skipped") {
		t.Errorf("Message = %q", result.Message)
	}
}

func TestImageFixer_SharedFS_Skips(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	check := NewSharedFSCheck(&stubLibQuerier{
		lib: &library.Library{SharedFSStatus: library.SharedFSSuspected},
	}, logger)
	fixer := NewImageFixer(nil, nil, check, logger)
	a := &artist.Artist{Name: "Test", Path: t.TempDir(), LibraryID: "lib-1", MusicBrainzID: "mbid-1"}
	v := &Violation{RuleID: RuleThumbExists}
	result, err := fixer.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if result.Fixed {
		t.Error("Fixed = true, want false")
	}
	if !strings.Contains(result.Message, "skipped") {
		t.Errorf("Message = %q", result.Message)
	}
}

func TestLogoTrimFixer_SharedFS_Skips(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	check := NewSharedFSCheck(&stubLibQuerier{
		lib: &library.Library{SharedFSStatus: library.SharedFSSuspected},
	}, logger)
	fixer := NewLogoTrimFixer(nil, check, logger)
	a := &artist.Artist{Name: "Test", Path: t.TempDir(), LibraryID: "lib-1"}
	v := &Violation{RuleID: RuleLogoTrimmable}
	result, err := fixer.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if result.Fixed {
		t.Error("Fixed = true, want false")
	}
	if !strings.Contains(result.Message, "skipped") {
		t.Errorf("Message = %q", result.Message)
	}
}

func TestLogoPaddingFixer_SharedFS_Skips(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	check := NewSharedFSCheck(&stubLibQuerier{
		lib: &library.Library{SharedFSStatus: library.SharedFSSuspected},
	}, logger)
	fixer := NewLogoPaddingFixer(nil, check, logger)
	a := &artist.Artist{Name: "Test", Path: t.TempDir(), LibraryID: "lib-1"}
	v := &Violation{RuleID: RuleLogoPadding}
	result, err := fixer.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if result.Fixed {
		t.Error("Fixed = true, want false")
	}
	if !strings.Contains(result.Message, "skipped") {
		t.Errorf("Message = %q", result.Message)
	}
}

func TestBackdropSequencingFixer_SharedFS_Skips(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	check := NewSharedFSCheck(&stubLibQuerier{
		lib: &library.Library{SharedFSStatus: library.SharedFSSuspected},
	}, logger)
	fixer := NewBackdropSequencingFixer(nil, check, logger)
	a := &artist.Artist{Name: "Test", Path: t.TempDir(), LibraryID: "lib-1"}
	v := &Violation{RuleID: RuleBackdropSequencing}
	result, err := fixer.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if result.Fixed {
		t.Error("Fixed = true, want false")
	}
	if !strings.Contains(result.Message, "skipped") {
		t.Errorf("Message = %q", result.Message)
	}
}

func TestExtraneousImagesFixer_SharedFS_WidensExpected(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	check := NewSharedFSCheck(&stubLibQuerier{
		lib: &library.Library{SharedFSStatus: library.SharedFSSuspected},
	}, logger)

	t.Run("nil platformService blocks on shared filesystem", func(t *testing.T) {
		// When platformService is nil on a shared filesystem, the fixer
		// cannot determine the safe deletion set (union of all profiles).
		// It must block rather than fall back to a narrow default set
		// that could delete files owned by another platform.
		fixer := NewExtraneousImagesFixer(nil, check, logger)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "random.jpg"), []byte{0xFF, 0xD8, 0xFF, 0xD9}, 0o644); err != nil {
			t.Fatal(err)
		}
		a := &artist.Artist{Path: dir, LibraryID: "lib-1"}
		v := &Violation{RuleID: RuleExtraneousImages}
		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if result.Fixed {
			t.Error("Fixed = true, want false on shared FS without platform service")
		}
		if !strings.Contains(result.Message, "skipped") {
			t.Errorf("Message = %q, want skip message", result.Message)
		}
		// File should NOT be deleted
		if _, statErr := os.Stat(filepath.Join(dir, "random.jpg")); os.IsNotExist(statErr) {
			t.Error("random.jpg was deleted; should be preserved when fixer is blocked")
		}
	})

	t.Run("non-shared library does not widen expected set", func(t *testing.T) {
		// When the library is NOT shared, the fixer should use single-profile
		// defaults regardless of platformService availability.
		nonSharedCheck := NewSharedFSCheck(&stubLibQuerier{
			lib: &library.Library{SharedFSStatus: library.SharedFSNone},
		}, logger)
		fixer := NewExtraneousImagesFixer(nil, nonSharedCheck, logger)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "random.jpg"), []byte{0xFF, 0xD8, 0xFF, 0xD9}, 0o644); err != nil {
			t.Fatal(err)
		}
		a := &artist.Artist{Path: dir, LibraryID: "lib-1"}
		v := &Violation{RuleID: RuleExtraneousImages}
		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if !result.Fixed {
			t.Error("Fixed = false, want true (extraneous file should be deleted)")
		}
	})
}

func TestDirectoryRenameFixer_SharedFS_Blocks(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	check := NewSharedFSCheck(&stubLibQuerier{
		lib: &library.Library{SharedFSStatus: library.SharedFSSuspected},
	}, logger)
	fixer := NewDirectoryRenameFixer(check, logger)
	a := &artist.Artist{Name: "New Name", Path: t.TempDir(), LibraryID: "lib-1"}
	v := &Violation{RuleID: RuleDirectoryNameMismatch, Config: RuleConfig{ArticleMode: "prefix"}}
	result, err := fixer.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if result.Fixed {
		t.Error("Fixed = true, want false")
	}
	if !strings.Contains(result.Message, "skipped") {
		t.Errorf("Message = %q", result.Message)
	}
}

func TestDirectoryRenameFixer_NonSharedFS_Proceeds(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	check := NewSharedFSCheck(&stubLibQuerier{
		lib: &library.Library{SharedFSStatus: library.SharedFSNone},
	}, logger)
	fixer := NewDirectoryRenameFixer(check, logger)

	tmp := t.TempDir()
	oldPath := filepath.Join(tmp, "Old Name")
	if err := os.MkdirAll(oldPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldPath, "artist.nfo"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &artist.Artist{Name: "New Name", Path: oldPath, LibraryID: "lib-1"}
	v := &Violation{RuleID: RuleDirectoryNameMismatch, Config: RuleConfig{ArticleMode: "prefix"}}

	result, err := fixer.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !result.Fixed {
		t.Errorf("Fixed = false on non-shared library, want true; message: %s", result.Message)
	}
}
