package artist

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renameTestArtist seeds a library + a single artist with a real on-disk
// directory and provider IDs / image rows so the test exercises the full
// hydrated round-trip through Service.RenameDirectory. The temp dir is
// cleaned up by t.TempDir.
func renameTestArtist(t *testing.T, libID string) (*Service, *Artist, string) {
	t.Helper()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	root := t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES (?, ?, ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		libID, libID, root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	dir := filepath.Join(root, "Original Name")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("creating artist dir: %v", err)
	}

	a := testArtist("Original Name", dir)
	a.LibraryID = libID
	a.MusicBrainzID = "5b11f4ce-a62d-471e-81fc-a69a8278c7da"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Re-fetch via the hydrated Service.GetByID so we have the row the
	// test will assert against.
	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	return svc, got, root
}

func TestRenameDirectory_Happy(t *testing.T) {
	svc, a, root := renameTestArtist(t, "lib-rename-happy")
	ctx := context.Background()

	got, err := svc.RenameDirectory(ctx, a.ID, "New Name")
	if err != nil {
		t.Fatalf("RenameDirectory: %v", err)
	}
	want := filepath.Join(root, "New Name")
	if got != want {
		t.Errorf("newPath = %q, want %q", got, want)
	}
	// On-disk: new exists, old does not.
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected new path on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Original Name")); !os.IsNotExist(err) {
		t.Errorf("expected old path gone, stat err = %v", err)
	}
	// DB: path updated.
	after, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("post-rename GetByID: %v", err)
	}
	if after.Path != want {
		t.Errorf("post-rename Path = %q, want %q", after.Path, want)
	}
}

// TestRenameDirectory_PreservesProviderIDs is the regression test for the
// CR finding on hydrated load: a non-hydrated GetByID flowed into s.update
// would silently wipe artist_provider_ids AND artist_images via
// persistNormalized re-inserting from the empty in-memory struct. The
// hydrated load must round-trip both. Tests both surfaces because the
// production-code rationale comment names them together; covering only
// one would let a regression in the other ship green.
func TestRenameDirectory_PreservesProviderIDs(t *testing.T) {
	svc, a, _ := renameTestArtist(t, "lib-rename-provider")
	ctx := context.Background()

	if a.MusicBrainzID == "" {
		t.Fatalf("seed precondition: MBID was not hydrated on the fixture (test setup bug)")
	}

	// Seed an artist_images row alongside the provider IDs so the round-trip
	// covers persistNormalized's image table as well as its provider table.
	img := &ArtistImage{
		ArtistID:  a.ID,
		ImageType: "thumb",
		SlotIndex: 0,
		Exists:    true,
	}
	if err := svc.UpsertImage(ctx, img); err != nil {
		t.Fatalf("seed UpsertImage: %v", err)
	}
	imgsBefore, err := svc.GetImagesForArtist(ctx, a.ID)
	if err != nil || len(imgsBefore) != 1 {
		t.Fatalf("seed verify GetImagesForArtist: err=%v len=%d", err, len(imgsBefore))
	}

	if _, err := svc.RenameDirectory(ctx, a.ID, "Renamed With Provider"); err != nil {
		t.Fatalf("RenameDirectory: %v", err)
	}
	after, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("post-rename GetByID: %v", err)
	}
	if after.MusicBrainzID != a.MusicBrainzID {
		t.Errorf("MBID lost across rename: was %q, now %q", a.MusicBrainzID, after.MusicBrainzID)
	}

	imgsAfter, err := svc.GetImagesForArtist(ctx, a.ID)
	if err != nil {
		t.Fatalf("post-rename GetImagesForArtist: %v", err)
	}
	if len(imgsAfter) != 1 {
		t.Errorf("artist_images lost across rename: was 1 row, now %d", len(imgsAfter))
	} else if imgsAfter[0].ImageType != "thumb" || imgsAfter[0].SlotIndex != 0 {
		t.Errorf("artist_images row mutated across rename: got %+v", imgsAfter[0])
	}
}

// TestRenameDirectory_RollbackOnDBFailure verifies the FS-rollback path
// when the on-disk rename succeeds but the DB Update fails. The test wraps
// the real artists Repository with a decorator that returns an error from
// Update; after the call returns the error, the on-disk directory must be
// back at its original location so the next scan sees a consistent state.
//
// This test is the regression guard for the FS-rollback block in
// RenameDirectory: a refactor that swaps the rollback's argument order
// (newPath/oldPath) or drops the rollback entirely would silently pass
// every other test in the suite, but would fail this one.
func TestRenameDirectory_RollbackOnDBFailure(t *testing.T) {
	db := setupTestDB(t)
	root := t.TempDir()
	ctx := context.Background()

	// Seed via the real Service so the artist is correctly persisted, then
	// rebuild a Service with a decorator on the artists repo that fails on
	// Update only. GetByID must keep working (the rename validation
	// re-fetches the artist) so a blanket "fail everything" decorator
	// would not exercise the right code path.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-rb', 'lib-rb', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}
	dir := filepath.Join(root, "Original")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("creating dir: %v", err)
	}
	seedSvc := NewService(db)
	a := testArtist("Original", dir)
	a.LibraryID = "lib-rb"
	if err := seedSvc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	artists, providers, members, aliases, images, platformIDs, completeness := NewDefaultRepos(db)
	failingArtists := &updateFailingRepo{Repository: artists}
	svc := NewServiceWithRepos(failingArtists, providers, members, aliases, images, platformIDs, completeness)

	_, err := svc.RenameDirectory(ctx, a.ID, "Should Roll Back")
	if err == nil {
		t.Fatal("RenameDirectory: expected error from forced Update failure, got nil")
	}

	// Filesystem must be back at the original location.
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Errorf("rollback failed: original directory missing after RenameDirectory error: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "Should Roll Back")); !os.IsNotExist(statErr) {
		t.Errorf("rollback failed: new path still on disk (statErr=%v)", statErr)
	}

	// DB row must still carry the original path.
	after, err := seedSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("post-rollback GetByID: %v", err)
	}
	if after.Path != dir {
		t.Errorf("DB path mutated despite rollback: was %q, now %q", dir, after.Path)
	}
}

// updateFailingRepo wraps a Repository and forces Update to fail. All other
// methods delegate. This is just enough to drive the rollback branch in
// RenameDirectory without rewriting every Repository method by hand.
type updateFailingRepo struct {
	Repository
}

func (r *updateFailingRepo) Update(ctx context.Context, a *Artist) error {
	return errors.New("simulated DB failure")
}

// updateAndDestroyRepo extends updateFailingRepo by removing the directory at
// a.Path before failing Update. Since RenameDirectory sets a.Path to newPath
// and only then calls Update, removing a.Path inside the failing Update
// guarantees the subsequent rollback's RenameDirAtomic(newPath, oldPath) has
// no source to move, so the rollback itself fails. That drives the
// "rollback also failed" slog.Error block in RenameDirectory which has no
// other natural trigger.
type updateAndDestroyRepo struct {
	Repository
}

func (r *updateAndDestroyRepo) Update(_ context.Context, a *Artist) error {
	_ = os.RemoveAll(a.Path)
	return errors.New("simulated DB failure with missing newPath")
}

// TestRenameDirectory_RenameError covers the failure of the initial on-disk
// RenameDirAtomic call (before any DB work). With the parent directory mode
// 0500, both os.Rename and the copy fallback are forbidden, so
// RenameDirAtomic returns an error that RenameDirectory wraps as
// "renaming %q to %q: %w". This exercises both the wrapped-error branch in
// the service and the handler's default 500 mapping for unsentineled errors.
func TestRenameDirectory_RenameError(t *testing.T) {
	// Root bypasses POSIX permission bits, so the chmod 0500 below would
	// not produce EACCES and the wrapped-error branch we want to exercise
	// would never fire. Same skip pattern used by maintenance_test.go.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}

	svc, a, root := renameTestArtist(t, "lib-rename-fserr")
	ctx := context.Background()

	// Strip write permission from the parent so any rename or copy under it
	// fails with EACCES. Restore in cleanup so t.TempDir's cleanup can
	// remove the tree.
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatalf("chmod parent ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	_, err := svc.RenameDirectory(ctx, a.ID, "Cannot Rename Here")
	if err == nil {
		t.Fatal("RenameDirectory: expected filesystem error, got nil")
	}
	// Wrapped under any of the named sentinels would mean a category
	// regression; the FS-rename failure must surface as a generic error.
	for _, sentinel := range []error{
		ErrRenameInvalidName, ErrRenameLocked, ErrRenameNoPath,
		ErrRenameDestExists, ErrRenameNoChange, ErrNotFound,
	} {
		if errors.Is(err, sentinel) {
			t.Fatalf("rename error matched sentinel %v; expected generic FS error", sentinel)
		}
	}
	// Original directory must still be at oldPath. (We can stat it once we
	// restore parent perms; do that here so the assertion is meaningful.)
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod parent rw for assert: %v", err)
	}
	if _, statErr := os.Stat(a.Path); statErr != nil {
		t.Errorf("original dir missing after refused rename: %v", statErr)
	}
}

// TestRenameDirectory_RollbackAlsoFails exercises the slog.Error branch
// inside RenameDirectory that fires when both the DB Update and the FS
// rollback fail. The decorator removes the newPath directory while
// returning an Update error, so the rollback's RenameDirAtomic has no
// source to move and itself fails. The function still surfaces the
// original db error to the caller; we assert that contract here so any
// future refactor that swaps which error wins is caught.
func TestRenameDirectory_RollbackAlsoFails(t *testing.T) {
	db := setupTestDB(t)
	root := t.TempDir()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-rb-double', 'lib-rb-double', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}
	dir := filepath.Join(root, "Original")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("creating dir: %v", err)
	}
	seedSvc := NewService(db)
	a := testArtist("Original", dir)
	a.LibraryID = "lib-rb-double"
	if err := seedSvc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	artists, providers, members, aliases, images, platformIDs, completeness := NewDefaultRepos(db)
	failingArtists := &updateAndDestroyRepo{Repository: artists}
	svc := NewServiceWithRepos(failingArtists, providers, members, aliases, images, platformIDs, completeness)

	_, err := svc.RenameDirectory(ctx, a.ID, "New Name Lost")
	if err == nil {
		t.Fatal("expected wrapped DB error, got nil")
	}
	// The DB error is what the caller sees; the failed rollback only logs.
	if !strings.Contains(err.Error(), "persisting renamed path") {
		t.Errorf("error wrap missing 'persisting renamed path': %v", err)
	}
	// Both the original and new paths are gone (Update destroyed newPath
	// and rollback could not restore it). The DB row, however, must still
	// carry the original path because the failing Update did not commit.
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("expected original dir gone after destructive rollback, statErr=%v", statErr)
	}
	after, err := seedSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("post-rollback GetByID: %v", err)
	}
	if after.Path != dir {
		t.Errorf("DB path mutated despite failed Update: was %q, now %q", dir, after.Path)
	}
}

func TestRenameDirectory_InvalidName(t *testing.T) {
	svc, a, _ := renameTestArtist(t, "lib-rename-invalid")
	ctx := context.Background()

	cases := []string{"", " ", ".", "..", "with/slash", "with\\back"}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := svc.RenameDirectory(ctx, a.ID, in)
			if !errors.Is(err, ErrRenameInvalidName) {
				t.Errorf("input %q: got err %v, want ErrRenameInvalidName", in, err)
			}
		})
	}
}

func TestRenameDirectory_NotFound(t *testing.T) {
	svc, _, _ := renameTestArtist(t, "lib-rename-notfound")
	ctx := context.Background()

	_, err := svc.RenameDirectory(ctx, "no-such-artist-id", "Anything")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got err %v, want ErrNotFound", err)
	}
}

func TestRenameDirectory_Locked(t *testing.T) {
	svc, a, _ := renameTestArtist(t, "lib-rename-locked")
	ctx := context.Background()

	if err := svc.Lock(ctx, a.ID, "user"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	_, err := svc.RenameDirectory(ctx, a.ID, "New Name")
	if !errors.Is(err, ErrRenameLocked) {
		t.Errorf("got err %v, want ErrRenameLocked", err)
	}
}

func TestRenameDirectory_NoPath(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-rename-nopath', 'lib-rename-nopath', '/music', 'regular', 'manual', datetime('now'), datetime('now'))`,
	); err != nil {
		t.Fatalf("seeding library: %v", err)
	}
	a := testArtist("Pathless", "")
	a.LibraryID = "lib-rename-nopath"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := svc.RenameDirectory(ctx, a.ID, "New Name")
	if !errors.Is(err, ErrRenameNoPath) {
		t.Errorf("got err %v, want ErrRenameNoPath", err)
	}
}

func TestRenameDirectory_NoChange(t *testing.T) {
	svc, a, _ := renameTestArtist(t, "lib-rename-nochange")
	ctx := context.Background()

	current := filepath.Base(a.Path)
	_, err := svc.RenameDirectory(ctx, a.ID, current)
	if !errors.Is(err, ErrRenameNoChange) {
		t.Errorf("got err %v, want ErrRenameNoChange", err)
	}
}

func TestRenameDirectory_DestExists(t *testing.T) {
	svc, a, root := renameTestArtist(t, "lib-rename-collide")
	ctx := context.Background()

	// Pre-create the conflicting target.
	if err := os.Mkdir(filepath.Join(root, "Already Here"), 0o755); err != nil {
		t.Fatalf("pre-creating collision target: %v", err)
	}

	_, err := svc.RenameDirectory(ctx, a.ID, "Already Here")
	if !errors.Is(err, ErrRenameDestExists) {
		t.Errorf("got err %v, want ErrRenameDestExists", err)
	}
	// The original directory should not have been moved.
	if _, statErr := os.Stat(a.Path); statErr != nil {
		t.Errorf("original path missing after refused rename: %v", statErr)
	}
}
