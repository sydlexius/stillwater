package rule

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/library"
)

func TestDirectoryRenameFixer_Fix(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// The fixer now renames ONLY through the guarded port (#1221), so the tests
	// hand it the real artist service. Subtests that expect a refusal never reach
	// the renamer; the two that rename persist their artist first so the guarded
	// road can load it.
	svc, _ := newGuardedRenamer(t)
	fixer := NewDirectoryRenameFixer(nonSharedFSCheck(), svc, logger)

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
		persistArtist(t, svc, a)
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

	t.Run("destination exists with no usable sort_name", func(t *testing.T) {
		// Canonical target collides; sort_name is empty so no fallback is
		// attempted. The fixer must refuse with the canonical-collision message.
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
			t.Error("Fixed = true, want false when destination exists and no fallback")
		}
		if !strings.Contains(result.Message, "already exists") {
			t.Errorf("expected 'already exists' message, got: %s", result.Message)
		}
	})

	// #1220: when the canonical target collides AND sort_name produces a
	// distinct, free directory name, the fixer renames to the sort-name path
	// so artists with disambiguated SortName values still auto-fix.
	// #1220 idempotency: a prior run already renamed the directory to the
	// sort-name fallback. On rescan, pathIsFree(fallbackPath) sees the current
	// directory occupying the target; without a guard the fixer would report
	// "destination collides" and bounce the artist back into a fix loop.
	t.Run("directory already at sort_name fallback (idempotent rerun)", func(t *testing.T) {
		tmp := t.TempDir()
		fallbackPath := filepath.Join(tmp, "Carter Family, The (later generations)")
		canonicalCollision := filepath.Join(tmp, "The Carter Family")
		// The artist directory is already at the fallback path from a prior run.
		if err := os.MkdirAll(fallbackPath, 0o755); err != nil {
			t.Fatal(err)
		}
		// Canonical target exists too -- this is what triggered the original
		// collision-driven fallback.
		if err := os.MkdirAll(canonicalCollision, 0o755); err != nil {
			t.Fatal(err)
		}

		a := &artist.Artist{
			Name:      "The Carter Family",
			SortName:  "Carter Family, The (later generations)",
			Path:      fallbackPath,
			LibraryID: "lib-test",
		}
		v := &Violation{
			RuleID: RuleDirectoryNameMismatch,
			Config: RuleConfig{ArticleMode: "prefix"},
		}

		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if !result.Fixed {
			t.Fatalf("Fixed = false, want true (idempotent rerun); message: %s", result.Message)
		}
		// a.Path must remain at the fallback path (no actual rename happened).
		if a.Path != fallbackPath {
			t.Errorf("a.Path = %q, want %q (no-op on idempotent rerun)", a.Path, fallbackPath)
		}
		if !strings.Contains(result.Message, "already uses sort-name fallback") {
			t.Errorf("expected 'already uses sort-name fallback' message, got: %s", result.Message)
		}
	})

	t.Run("canonical collides with sort_name fallback free", func(t *testing.T) {
		tmp := t.TempDir()
		oldPath := filepath.Join(tmp, "Wrong Folder")
		canonicalCollision := filepath.Join(tmp, "The Carter Family")
		if err := os.MkdirAll(oldPath, 0o755); err != nil {
			t.Fatal(err)
		}
		// Pre-create the canonical target to force the collision branch.
		if err := os.MkdirAll(canonicalCollision, 0o755); err != nil {
			t.Fatal(err)
		}

		a := &artist.Artist{
			Name:      "The Carter Family",
			SortName:  "Carter Family, The (later generations)",
			Path:      oldPath,
			LibraryID: "lib-test",
		}
		persistArtist(t, svc, a)
		v := &Violation{
			RuleID: RuleDirectoryNameMismatch,
			Config: RuleConfig{ArticleMode: "prefix"},
		}

		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if !result.Fixed {
			t.Fatalf("Fixed = false, want true; message: %s", result.Message)
		}
		expected := filepath.Join(tmp, "Carter Family, The (later generations)")
		if a.Path != expected {
			t.Errorf("a.Path = %q, want %q", a.Path, expected)
		}
		if !strings.Contains(result.Message, "sort-name fallback") {
			t.Errorf("expected sort-name fallback message, got: %s", result.Message)
		}
	})

	// #1220: when both canonical and sort-name targets collide, refuse.
	t.Run("canonical and sort_name both collide", func(t *testing.T) {
		tmp := t.TempDir()
		oldPath := filepath.Join(tmp, "Wrong Folder")
		canonical := filepath.Join(tmp, "Hiromi")
		fallback := filepath.Join(tmp, "Uehara, Hiromi")
		for _, p := range []string{oldPath, canonical, fallback} {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}

		a := &artist.Artist{
			Name:      "Hiromi",
			SortName:  "Uehara, Hiromi",
			Path:      oldPath,
			LibraryID: "lib-test",
		}
		v := &Violation{
			RuleID: RuleDirectoryNameMismatch,
			Config: RuleConfig{ArticleMode: "prefix"},
		}

		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if result.Fixed {
			t.Error("Fixed = true, want false when both targets collide")
		}
		if !strings.Contains(result.Message, "fallback") {
			t.Errorf("expected fallback-collision message, got: %s", result.Message)
		}
	})

	// #1220: empty sort_name skips the fallback branch entirely. The fixer
	// must refuse with the canonical-collision message rather than try a
	// derived empty path.
	t.Run("canonical collides with empty sort_name", func(t *testing.T) {
		tmp := t.TempDir()
		oldPath := filepath.Join(tmp, "Wrong Folder")
		canonical := filepath.Join(tmp, "Beth Gibbons")
		for _, p := range []string{oldPath, canonical} {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}

		a := &artist.Artist{
			Name:      "Beth Gibbons",
			SortName:  "",
			Path:      oldPath,
			LibraryID: "lib-test",
		}
		v := &Violation{
			RuleID: RuleDirectoryNameMismatch,
			Config: RuleConfig{ArticleMode: "prefix"},
		}

		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if result.Fixed {
			t.Error("Fixed = true, want false when sort_name is empty")
		}
		if !strings.Contains(result.Message, "already exists") {
			t.Errorf("expected canonical-collision message, got: %s", result.Message)
		}
	})

	// #1220: when sort_name canonicalizes to the same value as canonical,
	// the fallback is not a real alternative; refuse rather than retry.
	t.Run("canonical collides with sort_name equal to canonical", func(t *testing.T) {
		tmp := t.TempDir()
		oldPath := filepath.Join(tmp, "Wrong Folder")
		canonical := filepath.Join(tmp, "Existing")
		for _, p := range []string{oldPath, canonical} {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}

		a := &artist.Artist{
			Name:      "Existing",
			SortName:  "Existing",
			Path:      oldPath,
			LibraryID: "lib-test",
		}
		v := &Violation{
			RuleID: RuleDirectoryNameMismatch,
			Config: RuleConfig{ArticleMode: "prefix"},
		}

		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if result.Fixed {
			t.Error("Fixed = true, want false when sort_name equals canonical")
		}
		if !strings.Contains(result.Message, "already exists") {
			t.Errorf("expected canonical-collision message, got: %s", result.Message)
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

	// Clears any stale violations when the only difference is Unicode
	// normalization form. The fixer must not attempt a rename (target would
	// collide with source on APFS) but must report Fixed so fix-all loops
	// stop re-queueing the same artist.
	t.Run("nfc nfd equivalent short circuits", func(t *testing.T) {
		tmp := t.TempDir()
		nfd := "Maria Joa\u0303o Pires"
		nfc := "Maria Jo\u00e3o Pires"
		oldPath := filepath.Join(tmp, nfd)
		if err := os.MkdirAll(oldPath, 0o755); err != nil {
			t.Fatal(err)
		}

		a := &artist.Artist{Name: nfc, Path: oldPath, LibraryID: "lib-test"}
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
		if !strings.Contains(result.Message, "Unicode-equivalent") {
			t.Errorf("expected Unicode-equivalent message, got: %s", result.Message)
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
	renamer, _ := newGuardedRenamer(t)
	fixer := NewDirectoryRenameFixer(fsCheck, renamer, logger)

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

// --- guarded-rename test rig (#1221 / #2380) ---------------------------------

// renameCall is one recorded SyncRename invocation: the exact (oldPath, newPath)
// the artist service handed the platform syncer after committing a rename.
type renameCall struct {
	artistID string
	oldPath  string
	newPath  string
}

// spyRenameSyncer stands in for publish.Publisher as the artist service's
// PlatformRenameSyncer. It records every call so a test can assert that a rename
// actually reached the platform-sync step -- the step that carries
// publish.guardPlatformPath. It can also inject per-platform failures to prove
// they do not fail the fix.
type spyRenameSyncer struct {
	mu      sync.Mutex
	calls   []renameCall
	results []artist.PlatformRemapResult
	err     error
}

func (s *spyRenameSyncer) SyncRename(_ context.Context, artistID, oldPath, newPath string) ([]artist.PlatformRemapResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, renameCall{artistID: artistID, oldPath: oldPath, newPath: newPath})
	return s.results, s.err
}

func (s *spyRenameSyncer) recorded() []renameCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]renameCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// newGuardedRenamer returns the REAL production DirectoryRenamer (*artist.Service)
// with a spy platform syncer attached. Using the real service (not a fake port)
// is deliberate: the point of #1221 is that the fixer must travel the same
// guarded road as the user-driven rename and the merge flow, so the test
// exercises that road rather than a stand-in for it.
func newGuardedRenamer(t *testing.T) (*artist.Service, *spyRenameSyncer) {
	t.Helper()
	svc := artist.NewService(setupTestDB(t))
	spy := &spyRenameSyncer{}
	svc.SetPlatformRenameSyncer(spy)
	return svc, spy
}

// persistArtist stores a so it has an ID the guarded renamer can load.
func persistArtist(t *testing.T, svc *artist.Service, a *artist.Artist) {
	t.Helper()
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("persisting artist: %v", err)
	}
}

// TestDirectoryRenameFixer_RoutesThroughGuardedRename is the #1221 regression
// test, and the reason it matters is #2380: the rule fixer used to rename the
// directory with filesystem.RenameDirAtomic and tell NO peer. That is a SECOND,
// UNGUARDED road to a rename -- the peers kept the old path, a peer's NFO saver
// re-created the directory that had just been renamed away, and the next scan
// re-imported it as a DUPLICATE ARTIST. The rule is enabled by default, so this
// was one "Fix" click away from reproducing the very bug the branch exists to
// kill.
//
// The assertion is on the PLATFORM SYNC step, not on the on-disk result: a
// rename that moves the directory but notifies nobody is exactly the failure.
func TestDirectoryRenameFixer_RoutesThroughGuardedRename(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, spy := newGuardedRenamer(t)
	fixer := NewDirectoryRenameFixer(nonSharedFSCheck(), svc, logger)

	tmp := t.TempDir()
	oldPath := filepath.Join(tmp, "Old Name")
	if err := os.MkdirAll(oldPath, 0o755); err != nil {
		t.Fatal(err)
	}

	a := &artist.Artist{Name: "New Name", Path: oldPath, LibraryID: "lib-test"}
	persistArtist(t, svc, a)
	v := &Violation{RuleID: RuleDirectoryNameMismatch, Config: RuleConfig{ArticleMode: "prefix"}}

	result, err := fixer.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !result.Fixed {
		t.Fatalf("Fixed = false, want true; message: %s", result.Message)
	}

	newPath := filepath.Join(tmp, "New Name")
	calls := spy.recorded()
	if len(calls) != 1 {
		t.Fatalf("platform rename syncer called %d times, want exactly 1 -- the fixer renamed the "+
			"directory without notifying any peer, which strands them on the old path (#1221/#2380)", len(calls))
	}
	got := calls[0]
	if got.artistID != a.ID || got.oldPath != oldPath || got.newPath != newPath {
		t.Errorf("SyncRename(%q, %q, %q), want (%q, %q, %q)",
			got.artistID, got.oldPath, got.newPath, a.ID, oldPath, newPath)
	}

	// The path must be updated exactly once, and by the guarded renamer: the row
	// and the in-memory artist must agree on the NEW path (not double-applied,
	// not reverted to the old one).
	if a.Path != newPath {
		t.Errorf("in-memory a.Path = %q, want %q", a.Path, newPath)
	}
	reloaded, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if reloaded.Path != newPath {
		t.Errorf("persisted artist path = %q, want %q", reloaded.Path, newPath)
	}
	if _, statErr := os.Stat(newPath); statErr != nil {
		t.Errorf("directory not renamed on disk: %v", statErr)
	}
}

// TestDirectoryRenameFixer_FallbackAlsoRoutesThroughGuardedRename covers the
// OTHER branch: when the canonical name collides, the fixer falls back to a
// sort-name-derived directory. That branch must be just as guarded -- a fix set
// where only the happy path notifies the peers is still a duplicate-artist bug
// on every collision.
func TestDirectoryRenameFixer_FallbackAlsoRoutesThroughGuardedRename(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, spy := newGuardedRenamer(t)
	fixer := NewDirectoryRenameFixer(nonSharedFSCheck(), svc, logger)

	tmp := t.TempDir()
	oldPath := filepath.Join(tmp, "Wrong Folder")
	if err := os.MkdirAll(oldPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// Occupy the canonical target so the fixer takes the sort-name fallback.
	if err := os.MkdirAll(filepath.Join(tmp, "The Session Trio"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &artist.Artist{
		Name:      "The Session Trio",
		SortName:  "Session Trio, The (studio lineup)",
		Path:      oldPath,
		LibraryID: "lib-test",
	}
	persistArtist(t, svc, a)
	v := &Violation{RuleID: RuleDirectoryNameMismatch, Config: RuleConfig{ArticleMode: "prefix"}}

	result, err := fixer.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !result.Fixed {
		t.Fatalf("Fixed = false, want true; message: %s", result.Message)
	}

	wantPath := filepath.Join(tmp, "Session Trio, The (studio lineup)")
	calls := spy.recorded()
	if len(calls) != 1 {
		t.Fatalf("sort-name fallback rename notified %d platforms, want 1 SyncRename call", len(calls))
	}
	if calls[0].newPath != wantPath || calls[0].oldPath != oldPath {
		t.Errorf("SyncRename(old=%q, new=%q), want (old=%q, new=%q)",
			calls[0].oldPath, calls[0].newPath, oldPath, wantPath)
	}
	if a.Path != wantPath {
		t.Errorf("a.Path = %q, want %q", a.Path, wantPath)
	}
}

// TestDirectoryRenameFixer_PlatformFailureDoesNotFailTheFix pins the best-effort
// contract (artist.PlatformRenameSyncer): the on-disk + DB rename has already
// committed, so a peer that refuses the push (e.g. guardPlatformPath refusing an
// out-of-root path) is reported, not rolled back -- but it is also not swallowed.
func TestDirectoryRenameFixer_PlatformFailureDoesNotFailTheFix(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc, spy := newGuardedRenamer(t)
	spy.results = []artist.PlatformRemapResult{
		{ConnectionID: "conn-1", Result: artist.PlatformRemapFailed, Error: "path is outside every platform root folder"},
	}
	fixer := NewDirectoryRenameFixer(nonSharedFSCheck(), svc, logger)

	tmp := t.TempDir()
	oldPath := filepath.Join(tmp, "Old Name")
	if err := os.MkdirAll(oldPath, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &artist.Artist{Name: "New Name", Path: oldPath, LibraryID: "lib-test"}
	persistArtist(t, svc, a)
	v := &Violation{RuleID: RuleDirectoryNameMismatch, Config: RuleConfig{ArticleMode: "prefix"}}

	result, err := fixer.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !result.Fixed {
		t.Fatalf("Fixed = false; a per-platform push failure must not fail the committed rename")
	}
	if !strings.Contains(result.Message, "platform path push") {
		t.Errorf("message must surface the failed platform push, got: %s", result.Message)
	}
}

// TestDirectoryRenameFixer_NoRenamerRefuses is the no-silent-failure guard: with
// no guarded renamer wired, the peers cannot be told anything, so the fixer must
// refuse -- never move the directory and report Fixed:true, which is the #2380
// failure mode in miniature.
func TestDirectoryRenameFixer_NoRenamerRefuses(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	fixer := NewDirectoryRenameFixer(nonSharedFSCheck(), nil, logger)

	tmp := t.TempDir()
	oldPath := filepath.Join(tmp, "Old Name")
	if err := os.MkdirAll(oldPath, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &artist.Artist{ID: "a-1", Name: "New Name", Path: oldPath, LibraryID: "lib-test"}
	v := &Violation{RuleID: RuleDirectoryNameMismatch, Config: RuleConfig{ArticleMode: "prefix"}}

	result, err := fixer.Fix(context.Background(), a, v)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if result.Fixed {
		t.Fatal("Fixed = true with no guarded renamer: the directory would move with no peer notified")
	}
	if _, statErr := os.Stat(oldPath); statErr != nil {
		t.Errorf("the directory must not have been renamed: %v", statErr)
	}
	if a.Path != oldPath {
		t.Errorf("a.Path = %q, want it left at %q", a.Path, oldPath)
	}
}

// TestDirectoryRenameFixer_GuardedRefusalsAreUnfixedNotErrors pins how the fixer
// reports the guarded renamer's sentinel refusals. Each one is an OUTCOME (the
// violation stays open with a reason the operator can act on), never a pipeline
// error and never a silent Fixed:true. The locked case matters most: the guarded
// road refuses to rename a locked artist, and before this fixer traveled that
// road it would have renamed one anyway.
func TestDirectoryRenameFixer_GuardedRefusalsAreUnfixedNotErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	t.Run("locked artist", func(t *testing.T) {
		svc, spy := newGuardedRenamer(t)
		fixer := NewDirectoryRenameFixer(nonSharedFSCheck(), svc, logger)

		tmp := t.TempDir()
		oldPath := filepath.Join(tmp, "Old Name")
		if err := os.MkdirAll(oldPath, 0o755); err != nil {
			t.Fatal(err)
		}
		// locked_at is required by the schema whenever locked = 1.
		lockedAt := time.Now().UTC()
		a := &artist.Artist{
			Name: "New Name", Path: oldPath, LibraryID: "lib-test",
			Locked: true, LockedAt: &lockedAt, LockSource: "user",
		}
		persistArtist(t, svc, a)
		v := &Violation{RuleID: RuleDirectoryNameMismatch, Config: RuleConfig{ArticleMode: "prefix"}}

		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix returned an error for a locked artist; want an unfixed result: %v", err)
		}
		if result.Fixed {
			t.Fatal("Fixed = true for a locked artist")
		}
		if !strings.Contains(result.Message, "locked") {
			t.Errorf("Message = %q, want it to name the lock", result.Message)
		}
		if _, statErr := os.Stat(oldPath); statErr != nil {
			t.Errorf("locked artist's directory was renamed anyway: %v", statErr)
		}
		if len(spy.recorded()) != 0 {
			t.Errorf("a refused rename must not have reached the platform sync step")
		}
	})

	t.Run("artist with no persisted id", func(t *testing.T) {
		svc, spy := newGuardedRenamer(t)
		fixer := NewDirectoryRenameFixer(nonSharedFSCheck(), svc, logger)

		tmp := t.TempDir()
		oldPath := filepath.Join(tmp, "Old Name")
		if err := os.MkdirAll(oldPath, 0o755); err != nil {
			t.Fatal(err)
		}
		// No ID: the guarded renamer cannot load the artist, so the peers cannot be
		// told. Renaming anyway is the failure mode; refusing is correct.
		a := &artist.Artist{Name: "New Name", Path: oldPath, LibraryID: "lib-test"}
		v := &Violation{RuleID: RuleDirectoryNameMismatch, Config: RuleConfig{ArticleMode: "prefix"}}

		result, err := fixer.Fix(context.Background(), a, v)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if result.Fixed {
			t.Fatal("Fixed = true for an artist the guarded renamer cannot load")
		}
		if _, statErr := os.Stat(oldPath); statErr != nil {
			t.Errorf("directory was renamed despite the refusal: %v", statErr)
		}
		if len(spy.recorded()) != 0 {
			t.Errorf("platform sync must not run for a refused rename")
		}
	})
}
