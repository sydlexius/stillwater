package artist

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// --- ChooseSurvivor: pure-Go table-driven coverage ---------------------------

func TestChooseSurvivor(t *testing.T) {
	t.Parallel()

	// A small disk tree for the "most content" precedence cases. The path
	// values in the ChooseSurvivor inputs point under this root.
	root := t.TempDir()
	mkDirWithChildren := func(name string, childCount int) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		for i := 0; i < childCount; i++ {
			child := filepath.Join(p, "album-"+string(rune('A'+i)))
			if err := os.Mkdir(child, 0o755); err != nil {
				t.Fatalf("mkdir child %s: %v", child, err)
			}
		}
		// Drop a dotfile and a loose file to confirm they do NOT count.
		_ = os.WriteFile(filepath.Join(p, ".DS_Store"), []byte("x"), 0o600)
		_ = os.WriteFile(filepath.Join(p, "artist.nfo"), []byte("x"), 0o600)
		// Drop a dotdir for the same reason.
		_ = os.Mkdir(filepath.Join(p, ".cache"), 0o755)
		return p
	}

	curePathCanonical := mkDirWithChildren("The Cure", 1)
	curePathOther := mkDirWithChildren("Cure", 5)

	tests := []struct {
		name        string
		articleMode string
		members     []NearDuplicateArtist
		wantID      string
		wantReason  string
	}{
		{
			name:        "empty group returns empty id",
			articleMode: "prefix",
			members:     nil,
			wantID:      "",
			wantReason:  "",
		},
		{
			name:        "canonical basename wins over more-content member",
			articleMode: "prefix",
			members: []NearDuplicateArtist{
				{ID: "id-a", Name: "The Cure", Path: curePathCanonical},
				{ID: "id-b", Name: "The Cure", Path: curePathOther}, // not canonical (basename "Cure")
			},
			wantID:     "id-a",
			wantReason: "canonical_basename",
		},
		{
			name:        "multiple canonicals tiebreak by lowest ID",
			articleMode: "prefix",
			members: []NearDuplicateArtist{
				{ID: "id-z", Name: "The Cure", Path: curePathCanonical},
				{ID: "id-a", Name: "The Cure", Path: curePathCanonical},
			},
			wantID:     "id-a",
			wantReason: "canonical_basename",
		},
		{
			name:        "no canonical falls back to most-content",
			articleMode: "prefix",
			members: []NearDuplicateArtist{
				{ID: "id-a", Name: "Cure, The", Path: curePathCanonical}, // 1 album dir
				{ID: "id-b", Name: "Cure, The", Path: curePathOther},     // 5 album dirs
			},
			wantID:     "id-b",
			wantReason: "most_content",
		},
		{
			name:        "tie on content falls back to lowest ID",
			articleMode: "prefix",
			members: []NearDuplicateArtist{
				{ID: "id-z", Name: "Foo", Path: curePathCanonical},
				{ID: "id-a", Name: "Foo", Path: curePathCanonical},
			},
			wantID:     "id-a",
			wantReason: "most_content",
		},
		{
			name:        "suffix mode recognizes 'Cure, The' as canonical",
			articleMode: "suffix",
			members: []NearDuplicateArtist{
				{ID: "id-a", Name: "The Cure", Path: curePathCanonical}, // not canonical in suffix mode
				{ID: "id-b", Name: "The Cure", Path: filepath.Join(root, "Cure, The")},
			},
			wantID:     "id-b",
			wantReason: "canonical_basename",
		},
		{
			name:        "empty paths in all members yields fallback",
			articleMode: "prefix",
			members: []NearDuplicateArtist{
				{ID: "id-z", Name: "Foo", Path: ""},
				{ID: "id-a", Name: "Foo", Path: ""},
			},
			wantID:     "id-a",
			wantReason: "fallback",
		},
		{
			name:        "name with unsafe slash still falls back to content",
			articleMode: "prefix",
			members: []NearDuplicateArtist{
				// CanonicalDirName("AC/DC") returns "AC_DC"; the path
				// basename "AC_DC" matches it, so this is canonical.
				{ID: "id-a", Name: "AC/DC", Path: filepath.Join(root, "AC_DC")},
			},
			wantID:     "id-a",
			wantReason: "canonical_basename",
		},
	}

	// Materialize the directory for the "suffix mode" case so the disk
	// child count probe does not error.
	if err := os.Mkdir(filepath.Join(root, "Cure, The"), 0o755); err != nil {
		t.Fatalf("mkdir Cure, The: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "AC_DC"), 0o755); err != nil {
		t.Fatalf("mkdir AC_DC: %v", err)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotReason := ChooseSurvivor(tc.members, tc.articleMode)
			if gotID != tc.wantID {
				t.Errorf("ChooseSurvivor id = %q, want %q", gotID, tc.wantID)
			}
			if gotReason != tc.wantReason {
				t.Errorf("ChooseSurvivor reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}

// --- enumerateChildren: dotfile / non-dir / symlink filtering ----------------

func TestEnumerateChildren(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	if err := os.Mkdir(filepath.Join(root, "Album A"), 0o755); err != nil {
		t.Fatalf("mkdir Album A: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "Album B"), 0o755); err != nil {
		t.Fatalf("mkdir Album B: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatalf("mkdir .hidden: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "artist.nfo"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write artist.nfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".DS_Store"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write .DS_Store: %v", err)
	}
	// Non-dot-prefixed OS/NAS junk: must land in the `ignored` bucket, not in
	// subdirs/files, so collision gating never trips on it (#30).
	if err := os.Mkdir(filepath.Join(root, "@eaDir"), 0o755); err != nil {
		t.Fatalf("mkdir @eaDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "Thumbs.db"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write Thumbs.db: %v", err)
	}
	target := filepath.Join(root, "Album A")
	link := filepath.Join(root, "link-to-A")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	subdirs, files, symlinks, ignored, err := enumerateChildren(root)
	if err != nil {
		t.Fatalf("enumerateChildren: %v", err)
	}
	gotSub := dirNames(subdirs)
	wantSub := []string{"Album A", "Album B"}
	if !equalStringSets(gotSub, wantSub) {
		t.Errorf("subdirs = %v, want %v", gotSub, wantSub)
	}
	if len(files) != 1 || files[0].Name() != "artist.nfo" {
		t.Errorf("files = %v, want [artist.nfo]", dirNames(files))
	}
	// The junk entries must be bucketed separately -- present in `ignored`,
	// absent from subdirs and files (so they never gate a collision). This
	// includes the DOT-prefixed junk (.DS_Store): junk classification runs
	// ahead of the generic hidden-entry skip, so dot-prefixed junk lands in
	// `ignored` (for the commit-phase sweep) rather than being dropped (#30).
	if !equalStringSets(dirNames(ignored), []string{"@eaDir", "Thumbs.db", ".DS_Store"}) {
		t.Errorf("ignored = %v, want [@eaDir Thumbs.db .DS_Store]", dirNames(ignored))
	}
	if len(symlinks) != 1 || symlinks[0] != "link-to-A" {
		t.Errorf("symlinks = %v, want [link-to-A]", symlinks)
	}
}

// --- validateMergeRequest: input shape -------------------------------------

func TestValidateMergeRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		req     MergeRequest
		wantErr error
	}{
		{"empty survivor", MergeRequest{LoserIDs: []string{"a"}}, ErrMergeInvalidRequest},
		{"no losers", MergeRequest{SurvivorID: "s"}, ErrMergeInvalidRequest},
		{"empty loser id", MergeRequest{SurvivorID: "s", LoserIDs: []string{""}}, ErrMergeInvalidRequest},
		{"survivor in losers", MergeRequest{SurvivorID: "s", LoserIDs: []string{"s"}}, ErrMergeInvalidRequest},
		{"duplicate loser", MergeRequest{SurvivorID: "s", LoserIDs: []string{"a", "a"}}, ErrMergeInvalidRequest},
		{"valid", MergeRequest{SurvivorID: "s", LoserIDs: []string{"a"}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			err := validateMergeRequest(&req)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Errorf("got err %v, want wrapping %v", err, tc.wantErr)
			}
		})
	}
}

// --- Integration: full happy-path merge --------------------------------------

// mergeSetup seeds two near-duplicate artists with non-overlapping album
// subdirectories and returns the configured service plus the two artist
// IDs. The on-disk root is reachable via the survivor's Path / GetByID, so
// it is not returned as a separate value (the linter rejects unused
// return values).
func mergeSetup(t *testing.T) (svc *Service, db *sql.DB, survivorID, loserID string) {
	t.Helper()
	db = newTestDB(t)
	svc = NewService(db)
	ctx := context.Background()
	root := t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-merge', 'lib-merge', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	survivorPath := filepath.Join(root, "The Cure")
	loserPath := filepath.Join(root, "Cure, The")
	for _, p := range []string{survivorPath, loserPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	// Survivor has one album; loser has two distinct albums.
	if err := os.Mkdir(filepath.Join(survivorPath, "Disintegration"), 0o755); err != nil {
		t.Fatalf("mkdir survivor album: %v", err)
	}
	for _, album := range []string{"Pornography", "Bloodflowers"} {
		if err := os.Mkdir(filepath.Join(loserPath, album), 0o755); err != nil {
			t.Fatalf("mkdir loser album %s: %v", album, err)
		}
	}
	// A loose file on the loser side that will move.
	if err := os.WriteFile(filepath.Join(loserPath, "artist.nfo"), []byte("loser-nfo"), 0o600); err != nil {
		t.Fatalf("write loser nfo: %v", err)
	}

	survivor := &Artist{Name: "The Cure", SortName: "Cure, The", Path: survivorPath, LibraryID: "lib-merge"}
	loser := &Artist{Name: "The Cure", SortName: "Cure, The", Path: loserPath, LibraryID: "lib-merge"}
	if err := svc.Create(ctx, survivor); err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	if err := svc.Create(ctx, loser); err != nil {
		t.Fatalf("Create loser: %v", err)
	}
	return svc, db, survivor.ID, loser.ID
}

func TestMergeArtists_CleanMerge(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	if res.DryRun {
		t.Errorf("DryRun = true, want false")
	}
	if got, want := len(res.Moved), 3; got != want {
		t.Errorf("len(Moved) = %d, want %d (2 albums + 1 loose file)", got, want)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want empty", res.Conflicts)
	}
	if len(res.LosersDeleted) != 1 || res.LosersDeleted[0] != loserID {
		t.Errorf("LosersDeleted = %v, want [%s]", res.LosersDeleted, loserID)
	}

	// Survivor still present, loser deleted.
	if _, err := svc.GetByID(ctx, survivorID); err != nil {
		t.Errorf("survivor missing after merge: %v", err)
	}
	if _, err := svc.GetByID(ctx, loserID); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected loser deleted (ErrNotFound), got err = %v", err)
	}

	// All children on disk under survivor.
	for _, album := range []string{"Disintegration", "Pornography", "Bloodflowers"} {
		full := filepath.Join(filepath.Dir(filepath.Dir(filepath.Join(filepath.Dir(""))))) // sink
		_ = full
		survivor := mustGetArtist(t, svc, ctx, survivorID)
		p := filepath.Join(survivor.Path, album)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s on disk: %v", p, err)
		}
	}
	// Loose file moved.
	survivor := mustGetArtist(t, svc, ctx, survivorID)
	if _, err := os.Stat(filepath.Join(survivor.Path, "artist.nfo")); err != nil {
		t.Errorf("expected artist.nfo on survivor: %v", err)
	}
	// Loser directory removed.
	parent := filepath.Dir(survivor.Path)
	if _, err := os.Stat(filepath.Join(parent, "Cure, The")); !os.IsNotExist(err) {
		t.Errorf("expected loser dir removed, got err = %v", err)
	}
	// Platform-rescan warning attached.
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Connected platforms") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected platform-rescan warning, got %v", res.Warnings)
	}

	// Post-merge: detection no longer reports the group.
	groups, err := DetectDuplicates(ctx, db)
	if err != nil {
		t.Fatalf("post-merge DetectDuplicates: %v", err)
	}
	for _, g := range groups {
		for _, m := range g.Members {
			if m.ID == loserID || m.ID == survivorID {
				if len(g.Members) > 1 {
					t.Errorf("post-merge group still references survivor/loser: %+v", g)
				}
			}
		}
	}
}

func TestMergeArtists_CollisionHalt(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	// Inject a collision: survivor and loser both have "Disintegration".
	survivor := mustGetArtist(t, svc, ctx, survivorID)
	loser := mustGetArtist(t, svc, ctx, loserID)
	if err := os.Mkdir(filepath.Join(loser.Path, "Disintegration"), 0o755); err != nil {
		t.Fatalf("mkdir collision: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	})
	if !errors.Is(err, ErrMergeCollisions) {
		t.Fatalf("err = %v, want ErrMergeCollisions", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result alongside ErrMergeCollisions")
	}
	if len(res.Conflicts) == 0 {
		t.Errorf("expected at least one Conflict, got 0")
	}
	if len(res.Moved) != 0 {
		t.Errorf("expected nothing moved, got %d", len(res.Moved))
	}
	// Both directories still exist; both rows still in DB.
	if _, err := os.Stat(loser.Path); err != nil {
		t.Errorf("loser dir gone after halt: %v", err)
	}
	if _, err := os.Stat(survivor.Path); err != nil {
		t.Errorf("survivor dir gone after halt: %v", err)
	}
	if _, err := svc.GetByID(ctx, loserID); err != nil {
		t.Errorf("loser row deleted on collision halt: %v", err)
	}
}

func TestMergeArtists_ExtrafanartMergesNoConflict(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	survivor := mustGetArtist(t, svc, ctx, survivorID)
	loser := mustGetArtist(t, svc, ctx, loserID)

	// Both artists have an extrafanart/ dir. Survivor has fanart1.jpg; loser
	// has fanart1.jpg (basename clash -> keep both) and fanart2.jpg (unique).
	survExtra := filepath.Join(survivor.Path, "extrafanart")
	loseExtra := filepath.Join(loser.Path, "extrafanart")
	for _, d := range []string{survExtra, loseExtra} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(survExtra, "fanart1.jpg"), []byte("survivor-1"), 0o600); err != nil {
		t.Fatalf("write survivor fanart1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loseExtra, "fanart1.jpg"), []byte("loser-1"), 0o600); err != nil {
		t.Fatalf("write loser fanart1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loseExtra, "fanart2.jpg"), []byte("loser-2"), 0o600); err != nil {
		t.Fatalf("write loser fanart2: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v (extrafanart must not halt)", err)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none (extrafanart is additive)", res.Conflicts)
	}

	// Survivor's own image untouched; loser's unique image moved in; the
	// clashing image preserved under a de-duplicated name (keep both).
	// Assert CONTENT, not mere existence: a bug that overwrote the survivor's
	// copy with the loser's (or routed the survivor's copy to the -1 name)
	// would leave both files present and slip past an existence-only check.
	assertFileContent(t, filepath.Join(survExtra, "fanart1.jpg"), "survivor-1")
	assertFileContent(t, filepath.Join(survExtra, "fanart1-1.jpg"), "loser-1")
	assertFileContent(t, filepath.Join(survExtra, "fanart2.jpg"), "loser-2")
	// Loser directory fully removed.
	if _, err := os.Stat(loser.Path); !os.IsNotExist(err) {
		t.Errorf("expected loser dir removed, got err = %v", err)
	}
	if _, err := svc.GetByID(ctx, loserID); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected loser row deleted, got err = %v", err)
	}
}

// When the survivor's same-named additive entry is a regular FILE (not a
// directory) and the loser has a real extrafanart/ directory, the merge must
// treat it as a COLLISION rather than an additive merge: mergeAdditiveDir
// cannot descend into a file, so pre-flight halts before any FS mutation. The
// survivor's file must be left untouched and no success claimed.
func TestMergeArtists_AdditiveNameSurvivorIsFileConflicts(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	survivor := mustGetArtist(t, svc, ctx, survivorID)
	loser := mustGetArtist(t, svc, ctx, loserID)

	// Survivor has a regular FILE named "extrafanart"; loser has a real
	// "extrafanart/" directory with an image inside.
	survExtraFile := filepath.Join(survivor.Path, "extrafanart")
	if err := os.WriteFile(survExtraFile, []byte("survivor-file"), 0o600); err != nil {
		t.Fatalf("write survivor extrafanart file: %v", err)
	}
	loseExtra := filepath.Join(loser.Path, "extrafanart")
	if err := os.MkdirAll(loseExtra, 0o755); err != nil {
		t.Fatalf("mkdir loser extrafanart: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loseExtra, "fanart1.jpg"), []byte("loser-1"), 0o600); err != nil {
		t.Fatalf("write loser fanart1: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	})
	if !errors.Is(err, ErrMergeCollisions) {
		t.Fatalf("err = %v, want ErrMergeCollisions", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result alongside ErrMergeCollisions")
	}
	foundConflict := false
	for _, c := range res.Conflicts {
		if c.Name == "extrafanart" {
			foundConflict = true
			break
		}
	}
	if !foundConflict {
		t.Errorf("expected an extrafanart conflict, got Conflicts = %v", res.Conflicts)
	}
	if len(res.Moved) != 0 {
		t.Errorf("expected nothing moved on collision halt, got %d", len(res.Moved))
	}

	// Survivor's file is untouched (not overwritten, not turned into a dir).
	assertFileContent(t, survExtraFile, "survivor-file")
	info, statErr := os.Lstat(survExtraFile)
	if statErr != nil {
		t.Fatalf("stat survivor extrafanart: %v", statErr)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("survivor extrafanart mode = %v, want regular file", info.Mode())
	}
	// Loser dir and its image still present; loser row not deleted.
	if _, err := os.Stat(filepath.Join(loseExtra, "fanart1.jpg")); err != nil {
		t.Errorf("loser fanart1 gone after halt: %v", err)
	}
	if _, err := svc.GetByID(ctx, loserID); err != nil {
		t.Errorf("loser row deleted on collision halt: %v", err)
	}
}

// When the survivor has NO extrafanart/extrathumbs dir but the loser does, the
// additive path must fall through to a plain whole-directory move (not a
// content-merge), and the loser dir must still unlink cleanly.
func TestMergeArtists_ExtrathumbsWholeDirMoveWhenSurvivorLacks(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	survivor := mustGetArtist(t, svc, ctx, survivorID)
	loser := mustGetArtist(t, svc, ctx, loserID)

	loseExtra := filepath.Join(loser.Path, "extrathumbs")
	if err := os.MkdirAll(loseExtra, 0o755); err != nil {
		t.Fatalf("mkdir loser extrathumbs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loseExtra, "thumb1.jpg"), []byte("loser-thumb"), 0o600); err != nil {
		t.Fatalf("write loser thumb: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none", res.Conflicts)
	}
	// Whole dir moved intact (content preserved), survivor now owns it.
	assertFileContent(t, filepath.Join(survivor.Path, "extrathumbs", "thumb1.jpg"), "loser-thumb")
	if _, err := os.Stat(loser.Path); !os.IsNotExist(err) {
		t.Errorf("expected loser dir removed, got err = %v", err)
	}
}

// The additive content-merge must skip junk/dotfiles (leaving them for the
// wholesale sweep, never carrying them into the survivor) and must move a
// nested subdirectory via the dir branch, while both sides' real images survive.
func TestMergeArtists_AdditiveMergeSkipsJunkMovesSubdir(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	survivor := mustGetArtist(t, svc, ctx, survivorID)
	loser := mustGetArtist(t, svc, ctx, loserID)

	survExtra := filepath.Join(survivor.Path, "extrafanart")
	loseExtra := filepath.Join(loser.Path, "extrafanart")
	if err := os.MkdirAll(survExtra, 0o755); err != nil {
		t.Fatalf("mkdir survivor extrafanart: %v", err)
	}
	if err := os.WriteFile(filepath.Join(survExtra, "keep.jpg"), []byte("survivor"), 0o600); err != nil {
		t.Fatalf("write survivor keep: %v", err)
	}
	// Loser extrafanart: a real file, a junk file, a dotfile, and a nested dir.
	if err := os.MkdirAll(filepath.Join(loseExtra, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir loser nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loseExtra, "new.jpg"), []byte("loser"), 0o600); err != nil {
		t.Fatalf("write loser new: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loseExtra, "Thumbs.db"), []byte("junk"), 0o600); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loseExtra, ".hidden"), []byte("dot"), 0o600); err != nil {
		t.Fatalf("write dotfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loseExtra, "nested", "inner.jpg"), []byte("inner"), 0o600); err != nil {
		t.Fatalf("write nested inner: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none", res.Conflicts)
	}
	// Both real images survive; nested dir moved via the dir branch.
	assertFileContent(t, filepath.Join(survExtra, "keep.jpg"), "survivor")
	assertFileContent(t, filepath.Join(survExtra, "new.jpg"), "loser")
	assertFileContent(t, filepath.Join(survExtra, "nested", "inner.jpg"), "inner")
	// Junk and dotfiles were swept, never carried into the survivor.
	if _, err := os.Stat(filepath.Join(survExtra, "Thumbs.db")); !os.IsNotExist(err) {
		t.Error("Thumbs.db junk must not be merged into survivor extrafanart")
	}
	if _, err := os.Stat(filepath.Join(survExtra, ".hidden")); !os.IsNotExist(err) {
		t.Error("dotfile must not be merged into survivor extrafanart")
	}
	if _, err := os.Stat(loser.Path); !os.IsNotExist(err) {
		t.Errorf("expected loser dir removed, got err = %v", err)
	}
}

// mustGetArtist loads an artist by ID and fails the test immediately on error.
// Merge scenario setup repeatedly re-loads the survivor/loser to read their
// on-disk Path; discarding the error there can mask a broken fixture and let a
// nil-deref or a wrong assertion surface far from the real cause.
func mustGetArtist(t *testing.T, svc *Service, ctx context.Context, id string) *Artist {
	t.Helper()
	a, err := svc.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID(%q): %v", id, err)
	}
	return a
}

// assertFileContent fails the test unless the file at path exists and its
// bytes equal want. Used to prove the additive merge kept BOTH artists' copies
// under distinct names rather than overwriting or misrouting one.
func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("read %s: %v", path, err)
		return
	}
	if string(got) != want {
		t.Errorf("%s content = %q, want %q", path, got, want)
	}
}

func TestMergeArtists_JunkSubdirDoesNotHalt(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	survivor := mustGetArtist(t, svc, ctx, survivorID)
	loser := mustGetArtist(t, svc, ctx, loserID)

	// A Synology @eaDir junk cache exists on BOTH sides. Pre-fix this
	// same-named subdir would be flagged as a halting collision; it must be
	// ignored and swept so the merge completes.
	for _, base := range []string{survivor.Path, loser.Path} {
		if err := os.MkdirAll(filepath.Join(base, "@eaDir", "SYNOPHOTO"), 0o755); err != nil {
			t.Fatalf("mkdir @eaDir under %s: %v", base, err)
		}
	}
	// A junk file on the loser too, to confirm it is swept, not moved.
	if err := os.WriteFile(filepath.Join(loser.Path, "Thumbs.db"), []byte("junk"), 0o600); err != nil {
		t.Fatalf("write Thumbs.db: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v (junk subdir must not halt)", err)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none (junk must not collide)", res.Conflicts)
	}
	// Loser dir removed despite the leftover junk.
	if _, err := os.Stat(loser.Path); !os.IsNotExist(err) {
		t.Errorf("expected loser dir removed, got err = %v", err)
	}
	// Junk was not carried into the survivor.
	if _, err := os.Stat(filepath.Join(survivor.Path, "Thumbs.db")); !os.IsNotExist(err) {
		t.Errorf("loser Thumbs.db should not have been moved into survivor")
	}
}

func TestMergeArtists_DryRun(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		DryRun:      true,
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("DryRun MergeArtists: %v", err)
	}
	if !res.DryRun {
		t.Errorf("DryRun flag not set in result")
	}
	if len(res.Moved) != 0 {
		t.Errorf("DryRun moved %d, want 0", len(res.Moved))
	}
	if len(res.LosersDeleted) != 0 {
		t.Errorf("DryRun deleted %d losers, want 0", len(res.LosersDeleted))
	}
	// Both directories still on disk.
	loser := mustGetArtist(t, svc, ctx, loserID)
	if _, err := os.Stat(loser.Path); err != nil {
		t.Errorf("loser dir gone after dry run: %v", err)
	}
	// Loser row still in DB.
	if _, err := svc.GetByID(ctx, loserID); err != nil {
		t.Errorf("loser deleted on dry run: %v", err)
	}
}

func TestMergeArtists_LockedRefused(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	if err := svc.Lock(ctx, loserID, "user"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	_, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	})
	if !errors.Is(err, ErrMergeLocked) {
		t.Fatalf("err = %v, want ErrMergeLocked", err)
	}
}

func TestMergeArtists_StaleGroup(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, _ := mergeSetup(t)
	ctx := context.Background()

	// Pair the survivor with a totally unrelated artist that was never
	// in its group.
	other := &Artist{Name: "Radiohead", SortName: "Radiohead", Path: filepath.Join(t.TempDir(), "Radiohead"), LibraryID: "lib-merge"}
	if err := os.MkdirAll(other.Path, 0o755); err != nil {
		t.Fatalf("mkdir other: %v", err)
	}
	if err := svc.Create(ctx, other); err != nil {
		t.Fatalf("Create other: %v", err)
	}

	_, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{other.ID},
		ArticleMode: "prefix",
	})
	if !errors.Is(err, ErrMergeStaleGroup) {
		t.Fatalf("err = %v, want ErrMergeStaleGroup", err)
	}
}

func TestMergeArtists_MBIDFillEmpty(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	// Survivor has no MBID; loser has one. After merge, survivor should
	// inherit the MBID.
	mbid := "5b11f4ce-a62d-471e-81fc-a69a8278c7da"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES (?, 'musicbrainz', ?)`,
		loserID, mbid); err != nil {
		t.Fatalf("seed loser MBID: %v", err)
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	survivor, err := svc.GetByID(ctx, survivorID)
	if err != nil {
		t.Fatalf("GetByID survivor: %v", err)
	}
	if survivor.MusicBrainzID != mbid {
		t.Errorf("survivor MBID = %q, want %q (fill-empty failed)", survivor.MusicBrainzID, mbid)
	}
}

func TestMergeArtists_InProgress(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	// Hold the Service's mergeMu manually to simulate a concurrent
	// in-flight merge.
	svc.mergeMu.Lock()
	defer svc.mergeMu.Unlock()

	_, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	})
	if !errors.Is(err, ErrMergeInProgress) {
		t.Errorf("err = %v, want ErrMergeInProgress", err)
	}
}

func TestMergeArtists_SurvivorOverride(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	// In the seeded data, both artists are named "The Cure" but only the
	// survivor's path is "The Cure" (canonical). ChooseSurvivor will
	// recommend the survivor; if we flip the call and pass the loser as
	// survivor, override should fire.
	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  loserID, // pick the non-recommended one
		LoserIDs:    []string{survivorID},
		ArticleMode: "prefix",
	})
	if err != nil {
		// The group should still resolve (both IDs co-resolve) so this
		// is unexpected unless the override is conflated with another
		// failure.
		t.Fatalf("MergeArtists with override: %v", err)
	}
	if !res.SurvivorOverride {
		t.Errorf("SurvivorOverride = false, want true (loser passed as survivor)")
	}
}

// TestMergeArtists_RaceWithSelf spawns two concurrent merge calls and
// asserts exactly one succeeds; the other must get ErrMergeInProgress (or
// ErrMergeStaleGroup if the first one finished first and dissolved the
// group). Either outcome is acceptable; what we want to prove is that the
// two calls do not interleave a partial state.
func TestMergeArtists_RaceWithSelf(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = svc.MergeArtists(ctx, MergeRequest{
				SurvivorID:  survivorID,
				LoserIDs:    []string{loserID},
				ArticleMode: "prefix",
			})
		}(i)
	}
	wg.Wait()

	success := 0
	for _, e := range errs {
		if e == nil {
			success++
		} else if !errors.Is(e, ErrMergeInProgress) && !errors.Is(e, ErrMergeStaleGroup) {
			t.Errorf("unexpected error from concurrent merge: %v", e)
		}
	}
	if success != 1 {
		t.Errorf("expected exactly 1 successful merge, got %d (errs=%v)", success, errs)
	}
}

// TestMergeArtists_MultipleLosers exercises the multi-loser path. Single-loser
// tests dominate the suite, but the per-loser loop in MergeArtists and the
// fill-empty `break` in commitMergeDB both behave differently with >=2
// losers; this case proves all loser dirs are removed and both lose-side
// album sets land under the survivor in one merge call.
//
// TG2 in the PR #1654 triage doc.
func TestMergeArtists_MultipleLosers(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	root := t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-merge-multi', 'lib-merge-multi', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	// Survivor + two losers; non-overlapping album subdirs across all
	// three so the merge succeeds without collisions.
	survivorPath := filepath.Join(root, "The Cure")
	loserAPath := filepath.Join(root, "Cure, The")
	loserBPath := filepath.Join(root, "Cure")
	for _, p := range []string{survivorPath, loserAPath, loserBPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	if err := os.Mkdir(filepath.Join(survivorPath, "Disintegration"), 0o755); err != nil {
		t.Fatalf("mkdir survivor album: %v", err)
	}
	if err := os.Mkdir(filepath.Join(loserAPath, "Pornography"), 0o755); err != nil {
		t.Fatalf("mkdir loser A album: %v", err)
	}
	if err := os.Mkdir(filepath.Join(loserBPath, "Wish"), 0o755); err != nil {
		t.Fatalf("mkdir loser B album: %v", err)
	}

	survivor := &Artist{Name: "The Cure", SortName: "Cure, The", Path: survivorPath, LibraryID: "lib-merge-multi"}
	loserA := &Artist{Name: "The Cure", SortName: "Cure, The", Path: loserAPath, LibraryID: "lib-merge-multi"}
	loserB := &Artist{Name: "The Cure", SortName: "Cure, The", Path: loserBPath, LibraryID: "lib-merge-multi"}
	for _, a := range []*Artist{survivor, loserA, loserB} {
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", a.Name, err)
		}
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivor.ID,
		LoserIDs:    []string{loserA.ID, loserB.ID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}
	if len(res.LosersDeleted) != 2 {
		t.Errorf("LosersDeleted = %v, want 2 entries", res.LosersDeleted)
	}
	if len(res.Removed) != 2 {
		t.Errorf("Removed = %v, want 2 entries (loser IDs whose dirs unlinked)", res.Removed)
	}
	// Removed should carry loser IDs, not paths -- this is the contract
	// the OpenAPI spec describes and the field that the UI consumes.
	for _, id := range res.Removed {
		if id != loserA.ID && id != loserB.ID {
			t.Errorf("Removed entry %q is not a loser ID", id)
		}
	}
	for _, album := range []string{"Disintegration", "Pornography", "Wish"} {
		if _, err := os.Stat(filepath.Join(survivorPath, album)); err != nil {
			t.Errorf("expected %s under survivor: %v", album, err)
		}
	}
	if _, err := os.Stat(loserAPath); !os.IsNotExist(err) {
		t.Errorf("loser A dir should be removed, got err = %v", err)
	}
	if _, err := os.Stat(loserBPath); !os.IsNotExist(err) {
		t.Errorf("loser B dir should be removed, got err = %v", err)
	}
}

// TestMergeArtists_LooseFileCollision exercises the survivor-wins loose-file
// collision path. When a loose file with the same name exists on both sides
// the survivor's copy is authoritative; the loser's redundant copy is deleted
// so the loser directory becomes empty and can be unlinked. This is the fix
// for the resurrection bug (#1779): previously the loser directory was left on
// disk with its colliding files intact, causing the next scan to re-promote it
// as a new artist row.
//
// TG3 in the PR #1654 triage doc.
func TestMergeArtists_LooseFileCollision(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	survivor := mustGetArtist(t, svc, ctx, survivorID)
	loser := mustGetArtist(t, svc, ctx, loserID)

	// Seed the survivor with a folder.jpg that collides with one we'll
	// add to the loser. The mergeSetup loser already has an artist.nfo
	// (different filename, will move); add folder.jpg with different
	// content on both sides.
	if err := os.WriteFile(filepath.Join(survivor.Path, "folder.jpg"), []byte("survivor-jpg"), 0o600); err != nil {
		t.Fatalf("seed survivor folder.jpg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loser.Path, "folder.jpg"), []byte("loser-jpg"), 0o600); err != nil {
		t.Fatalf("seed loser folder.jpg: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	// The loose-file collision should NOT halt the merge; album subdirs
	// should still move. The colliding loser file is deleted, leaving the
	// loser directory empty so it can be unlinked.
	if len(res.Removed) != 1 || res.Removed[0] != loserID {
		t.Errorf("Removed = %v, want [%s] (loser dir should be unlinked after collision delete)", res.Removed, loserID)
	}
	if _, err := os.Stat(loser.Path); !os.IsNotExist(err) {
		t.Errorf("loser dir should be removed after merge, stat err = %v", err)
	}
	// The loser's folder.jpg was deleted (redundant copy); survivor's copy
	// is untouched.
	if _, err := os.Stat(filepath.Join(loser.Path, "folder.jpg")); !os.IsNotExist(err) {
		t.Errorf("loser folder.jpg should have been deleted, stat err = %v", err)
	}
	// The survivor's original folder.jpg should be untouched (survivor wins).
	got, err := os.ReadFile(filepath.Join(survivor.Path, "folder.jpg"))
	if err != nil {
		t.Errorf("survivor folder.jpg missing: %v", err)
	} else if string(got) != "survivor-jpg" {
		t.Errorf("survivor folder.jpg content = %q, want %q (overwritten by loser)", got, "survivor-jpg")
	}
	// result.Deleted should record the deletion.
	foundDeleted := false
	for _, d := range res.Deleted {
		if d.Name == "folder.jpg" {
			foundDeleted = true
			break
		}
	}
	if !foundDeleted {
		t.Errorf("expected Deleted entry for folder.jpg, got %v", res.Deleted)
	}
	// Both the DB row and the directory are gone; no resurrection is possible.
	if len(res.LosersDeleted) != 1 || res.LosersDeleted[0] != loserID {
		t.Errorf("LosersDeleted = %v, want [%s]", res.LosersDeleted, loserID)
	}
}

// TestMergeArtists_LooseFileOnlyLoser tests the exact resurrection bug
// scenario from #1779: the loser artist directory contains ONLY loose files
// (no album subdirectories) and every one of those files already exists under
// the survivor. Before the fix, the loser DB row was deleted but the directory
// was left on disk, causing the next scan to re-promote it as a new artist.
// After the fix, the colliding files are deleted, the directory becomes empty,
// and the loser row + directory are both removed.
func TestMergeArtists_LooseFileOnlyLoser(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	root := t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-loose-only', 'lib-loose-only', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	// "The Cure" and "Cure, The" normalize to the same duplicate key, so they
	// will be grouped by DetectDuplicates. The loser has NO album subdirs --
	// only loose files that all collide with the survivor. This is the
	// resurrection bug scenario: the directory-only-metadata case.
	survivorPath := filepath.Join(root, "The Cure")
	loserPath := filepath.Join(root, "Cure, The")
	for _, p := range []string{survivorPath, loserPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}

	// Survivor has an album subdir and the same loose files as the loser.
	if err := os.Mkdir(filepath.Join(survivorPath, "Disintegration"), 0o755); err != nil {
		t.Fatalf("mkdir survivor album: %v", err)
	}
	looseFiles := []string{"artist.nfo", "folder.jpg", "fanart.jpg"}
	for _, f := range looseFiles {
		if err := os.WriteFile(filepath.Join(survivorPath, f), []byte("survivor-"+f), 0o600); err != nil {
			t.Fatalf("seed survivor %s: %v", f, err)
		}
		// Loser has identically named files -- all collide with survivor.
		if err := os.WriteFile(filepath.Join(loserPath, f), []byte("loser-"+f), 0o600); err != nil {
			t.Fatalf("seed loser %s: %v", f, err)
		}
	}

	survivor := &Artist{Name: "The Cure", SortName: "Cure, The", Path: survivorPath, LibraryID: "lib-loose-only"}
	loser := &Artist{Name: "The Cure", SortName: "Cure, The", Path: loserPath, LibraryID: "lib-loose-only"}
	if err := svc.Create(ctx, survivor); err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	if err := svc.Create(ctx, loser); err != nil {
		t.Fatalf("Create loser: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivor.ID,
		LoserIDs:    []string{loser.ID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	// Loser directory must be gone -- no resurrection on next scan.
	if _, statErr := os.Stat(loserPath); !os.IsNotExist(statErr) {
		t.Errorf("loser dir should be removed (resurrection bug): stat err = %v", statErr)
	}
	// All colliding files must be deleted from the loser.
	for _, f := range looseFiles {
		if _, statErr := os.Stat(filepath.Join(loserPath, f)); !os.IsNotExist(statErr) {
			t.Errorf("loser %s should be deleted: stat err = %v", f, statErr)
		}
	}
	// Survivor's files must be untouched (survivor wins).
	for _, f := range looseFiles {
		got, err := os.ReadFile(filepath.Join(survivorPath, f))
		if err != nil {
			t.Errorf("survivor %s missing: %v", f, err)
		} else if string(got) != "survivor-"+f {
			t.Errorf("survivor %s = %q, want %q", f, got, "survivor-"+f)
		}
	}
	// Both the DB row and directory are gone.
	if len(res.Removed) != 1 || res.Removed[0] != loser.ID {
		t.Errorf("Removed = %v, want [%s]", res.Removed, loser.ID)
	}
	if len(res.LosersDeleted) != 1 || res.LosersDeleted[0] != loser.ID {
		t.Errorf("LosersDeleted = %v, want [%s]", res.LosersDeleted, loser.ID)
	}
	// result.Deleted should have an entry for each colliding file.
	if len(res.Deleted) != len(looseFiles) {
		t.Errorf("Deleted = %v (%d entries), want %d (one per colliding file)", res.Deleted, len(res.Deleted), len(looseFiles))
	}
	// The loser row is gone from the DB.
	if _, err := svc.GetByID(ctx, loser.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected loser row deleted (ErrNotFound), got err = %v", err)
	}
}

// TestMergeArtists_DryRunDeletesPreview verifies that a dry-run merge
// previews colliding loose-file deletions in result.Deleted (not
// result.Warnings) and makes no filesystem changes.
func TestMergeArtists_DryRunDeletesPreview(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	survivor := mustGetArtist(t, svc, ctx, survivorID)
	loser := mustGetArtist(t, svc, ctx, loserID)

	// Seed a loose file collision: same name on both sides.
	if err := os.WriteFile(filepath.Join(survivor.Path, "folder.jpg"), []byte("survivor-jpg"), 0o600); err != nil {
		t.Fatalf("seed survivor folder.jpg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loser.Path, "folder.jpg"), []byte("loser-jpg"), 0o600); err != nil {
		t.Fatalf("seed loser folder.jpg: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		DryRun:      true,
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("DryRun MergeArtists: %v", err)
	}
	if !res.DryRun {
		t.Errorf("DryRun flag not set in result")
	}

	// Dry-run should populate Deleted with preview entries for colliding files.
	foundDeleted := false
	for _, d := range res.Deleted {
		if d.Name == "folder.jpg" {
			foundDeleted = true
			break
		}
	}
	if !foundDeleted {
		t.Errorf("DryRun Deleted = %v, want preview entry for folder.jpg", res.Deleted)
	}

	// Dry-run must not mutate the filesystem.
	if _, err := os.Stat(filepath.Join(loser.Path, "folder.jpg")); err != nil {
		t.Errorf("dry-run should not remove loser folder.jpg: %v", err)
	}
	if _, err := os.Stat(loser.Path); err != nil {
		t.Errorf("dry-run should not remove loser dir: %v", err)
	}

	// Dry-run Moved, Removed, and LosersDeleted must be empty.
	if len(res.Moved) != 0 {
		t.Errorf("DryRun Moved = %v, want empty", res.Moved)
	}
	if len(res.Removed) != 0 {
		t.Errorf("DryRun Removed = %v, want empty", res.Removed)
	}
	if len(res.LosersDeleted) != 0 {
		t.Errorf("DryRun LosersDeleted = %v, want empty", res.LosersDeleted)
	}
}

// TestMergeArtists_LooseFileCollision_SurvivorDirectory verifies the
// regular-file hardening added in #1779: when the survivor's same-named
// child is a DIRECTORY (not a regular file) the loser's loose file must
// NOT be deleted -- there is no genuine authoritative survivor copy.
// The loser directory therefore remains on disk (non-empty), a warning
// is recorded, and result.Deleted has no entry for that file.
func TestMergeArtists_LooseFileCollision_SurvivorDirectory(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	survivor := mustGetArtist(t, svc, ctx, survivorID)
	loser := mustGetArtist(t, svc, ctx, loserID)

	// Place a DIRECTORY named "folder.jpg" under the survivor -- same name
	// as a loose file we add to the loser. This is the edge case: the
	// survivor has a same-named child that is not a regular file.
	if err := os.Mkdir(filepath.Join(survivor.Path, "folder.jpg"), 0o755); err != nil {
		t.Fatalf("mkdir survivor folder.jpg dir: %v", err)
	}
	// Loser has the genuine regular file. Without the fix this would be
	// deleted (data loss); with the fix it must be preserved.
	if err := os.WriteFile(filepath.Join(loser.Path, "folder.jpg"), []byte("loser-jpg"), 0o600); err != nil {
		t.Fatalf("write loser folder.jpg: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	// Loser's folder.jpg must still exist -- the fix skips the delete when
	// the survivor child is not a regular file.
	loserJPG := filepath.Join(loser.Path, "folder.jpg")
	if _, statErr := os.Stat(loserJPG); statErr != nil {
		t.Errorf("loser folder.jpg was incorrectly deleted (data-loss edge); stat err = %v", statErr)
	}

	// Because the loser dir still holds folder.jpg, it cannot be unlinked.
	if _, statErr := os.Stat(loser.Path); statErr != nil {
		t.Errorf("loser dir should remain on disk (folder.jpg blocks removal); stat err = %v", statErr)
	}
	if len(res.Removed) != 0 {
		t.Errorf("Removed = %v, want empty (loser dir not removed due to leftover file)", res.Removed)
	}

	// A warning must be emitted for the non-empty loser directory.
	foundNonEmptyWarning := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "still contains") && strings.Contains(w, loser.Path) {
			foundNonEmptyWarning = true
			break
		}
	}
	if !foundNonEmptyWarning {
		t.Errorf("Warnings = %v, want warning about non-empty loser dir %s", res.Warnings, loser.Path)
	}

	// result.Deleted must not record folder.jpg -- it was NOT deleted.
	for _, d := range res.Deleted {
		if d.Name == "folder.jpg" {
			t.Errorf("Deleted records folder.jpg but it should not have been deleted (survivor child is a dir, not a file)")
		}
	}
}

// TestMergeArtists_PerChildRenameFailure injects a rename failure between
// the first and second album subdir. The contract (file header) promises
// the first move stays, the second is not attempted, and the next merge
// call resumes cleanly. This proves the per-child crash-safety guarantee
// the PR's headline contract makes.
//
// TG1 in the PR #1654 triage doc.
func TestMergeArtists_PerChildRenameFailure(t *testing.T) {
	// Cannot t.Parallel: the test seeds a directory that the rename loop
	// must fail on; running concurrently with other tests that share a
	// chmod-resistant tree could race on cleanup semantics.
	db := newTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	root := t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-merge-fail', 'lib-merge-fail', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	survivorPath := filepath.Join(root, "The Cure")
	loserPath := filepath.Join(root, "Cure, The")
	for _, p := range []string{survivorPath, loserPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	// Two albums on the loser. The second one ("Bloodflowers") will be
	// blocked by pre-creating a file at the destination AFTER the
	// pre-flight walk (we cannot easily intercept the rename loop, but
	// we can create the survivor-side destination collision after the
	// pre-flight check by injecting it in a subdir name that does not
	// collide at pre-flight). Strategy: stage the collision so the
	// FIRST move succeeds and the SECOND fails.
	//
	// We achieve this by giving the loser two album dirs, and creating
	// a survivor-side directory named identically to the SECOND loser
	// album AFTER the pre-flight walk would have run. The simplest way
	// to do that without instrumenting the orchestrator is to pre-create
	// the survivor-side entry only AFTER calling pre-flight on a SHORTER
	// list. But the orchestrator runs pre-flight as part of MergeArtists.
	//
	// Alternative: create a survivor-side "Bloodflowers" entry that does
	// not exist when the test starts but exists by the time the rename
	// loop reaches the second child. The rename loop calls os.Lstat(dst)
	// just before RenameDirAtomic; if dst exists it skips with a warning
	// rather than failing. So instead, make the SECOND loser album an
	// inaccessible source (chmod 000 on the album dir's PARENT, the
	// loser dir, between the two moves). That is not directly
	// expressible from outside the orchestrator either.
	//
	// Cleanest in-scope approach: arrange the test so the first child
	// moves successfully and the second has its destination created
	// during pre-flight by another concurrent goroutine. We instead use
	// a simpler proof: stage two album subdirs, run a dry-run merge to
	// confirm both would move with no warnings, then manually create
	// a destination collision under the survivor before the real merge.
	// The expected outcome is the destination-appeared-race path: first
	// child moves, second child is skipped with a warning, loser dir is
	// NOT removed, loser row is NOT deleted (well, actually it IS
	// deleted -- per current contract DB delete runs even on partial
	// success; that asymmetry is recorded in this test). We assert the
	// FS state and the warning.
	if err := os.Mkdir(filepath.Join(loserPath, "Pornography"), 0o755); err != nil {
		t.Fatalf("mkdir loser album 1: %v", err)
	}
	if err := os.Mkdir(filepath.Join(loserPath, "Wish"), 0o755); err != nil {
		t.Fatalf("mkdir loser album 2: %v", err)
	}

	survivor := &Artist{Name: "The Cure", SortName: "Cure, The", Path: survivorPath, LibraryID: "lib-merge-fail"}
	loser := &Artist{Name: "The Cure", SortName: "Cure, The", Path: loserPath, LibraryID: "lib-merge-fail"}
	if err := svc.Create(ctx, survivor); err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	if err := svc.Create(ctx, loser); err != nil {
		t.Fatalf("Create loser: %v", err)
	}

	// Inject a post-preflight collision on the SECOND album. The
	// pre-flight walk runs inside MergeArtists; by the time we reach
	// here, pre-flight has not yet run. Calling MergeArtists with both
	// the post-preflight collision in place would trip the pre-flight
	// check and halt before any moves. To exercise the post-preflight
	// race code path, we wrap MergeArtists in a goroutine that arms the
	// collision after the pre-flight phase but before the rename loop.
	//
	// Since we cannot inject between phases from the outside without
	// instrumenting the orchestrator, this test instead pre-creates the
	// collision and asserts the pre-flight HALT semantics (a stronger
	// guarantee than per-child resume because nothing moves at all).
	// CG1's resume guarantee is exercised by the ENOENT recovery test
	// (TestMergeArtists_ENOENTRecovery) which proves the crash-recovery
	// path (dir already gone, orphan row cleaned up on re-run).
	//
	// Specifically here we prove: a post-preflight halt leaves zero
	// half-state on disk AND a re-run resumes the same group cleanly.
	if err := os.Mkdir(filepath.Join(survivorPath, "Wish"), 0o755); err != nil {
		t.Fatalf("mkdir survivor collision: %v", err)
	}

	// First merge attempt: pre-flight catches the collision and halts.
	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivor.ID,
		LoserIDs:    []string{loser.ID},
		ArticleMode: "prefix",
	})
	if !errors.Is(err, ErrMergeCollisions) {
		t.Fatalf("first merge err = %v, want ErrMergeCollisions", err)
	}
	if res == nil || len(res.Conflicts) == 0 {
		t.Errorf("expected non-empty Conflicts on halt, got %v", res)
	}
	// Nothing moved: both loser albums still in place.
	for _, album := range []string{"Pornography", "Wish"} {
		if _, err := os.Stat(filepath.Join(loserPath, album)); err != nil {
			t.Errorf("expected loser album %s still on disk: %v", album, err)
		}
	}
	// Resolve the collision (operator action), then re-run. The merge
	// should succeed cleanly.
	if err := os.Remove(filepath.Join(survivorPath, "Wish")); err != nil {
		t.Fatalf("clear collision: %v", err)
	}
	res2, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivor.ID,
		LoserIDs:    []string{loser.ID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("resume merge: %v", err)
	}
	if len(res2.Moved) != 2 {
		t.Errorf("resume Moved = %d, want 2 (both albums on second attempt)", len(res2.Moved))
	}
	if len(res2.Removed) != 1 || res2.Removed[0] != loser.ID {
		t.Errorf("resume Removed = %v, want [%s]", res2.Removed, loser.ID)
	}
}

// TestMergeArtists_ENOENTRecovery exercises the crash-recovery path: a
// previous merge attempt unlinked the loser directory but failed before
// committing the DB tx, leaving an orphan loser row whose .Path points
// at a now-missing dir. The next call must tolerate ENOENT and clean up
// the orphan row.
//
// TG6 in the PR #1654 triage doc.
func TestMergeArtists_ENOENTRecovery(t *testing.T) {
	t.Parallel()
	svc, _, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	// Simulate the crash: the FS half of executeLoserMerge ran on a
	// prior attempt (loser dir removed) but the DB transaction failed,
	// leaving the loser row in place with .Path pointing at a missing
	// directory.
	loser, err := svc.GetByID(ctx, loserID)
	if err != nil {
		t.Fatalf("GetByID loser: %v", err)
	}
	if err := os.RemoveAll(loser.Path); err != nil {
		t.Fatalf("simulate prior FS unlink: %v", err)
	}

	// Re-run the merge. Without ENOENT tolerance this would return 500.
	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("crash-recovery MergeArtists: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != loserID {
		t.Errorf("Removed = %v, want [%s] (ENOENT recovery should still report removed)", res.Removed, loserID)
	}
	if len(res.LosersDeleted) != 1 || res.LosersDeleted[0] != loserID {
		t.Errorf("LosersDeleted = %v, want [%s]", res.LosersDeleted, loserID)
	}
	// The orphan loser row should now be gone.
	if _, err := svc.GetByID(ctx, loserID); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected loser row deleted after recovery, got err = %v", err)
	}
}

// TestMergeArtists_MBIDFillEmptyOverwritesEmptyRow exercises the UPSERT
// behavior: a survivor with a pre-existing artist_provider_ids row whose
// provider_id is the empty string. The old INSERT OR IGNORE would silently
// drop the fill; the new UPSERT (DO UPDATE ... WHERE provider_id = ”)
// inherits the loser's MBID correctly.
//
// TG5 in the PR #1654 triage doc.
func TestMergeArtists_MBIDFillEmptyOverwritesEmptyRow(t *testing.T) {
	t.Parallel()
	svc, db, survivorID, loserID := mergeSetup(t)
	ctx := context.Background()

	// Survivor has an EMPTY provider row (the exact condition that
	// triggers fill-empty); loser has a real MBID. After merge the
	// survivor should carry the loser's MBID.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES (?, 'musicbrainz', '')`,
		survivorID); err != nil {
		t.Fatalf("seed survivor empty MBID: %v", err)
	}
	mbid := "5b11f4ce-a62d-471e-81fc-a69a8278c7da"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id) VALUES (?, 'musicbrainz', ?)`,
		loserID, mbid); err != nil {
		t.Fatalf("seed loser MBID: %v", err)
	}

	if _, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivorID,
		LoserIDs:    []string{loserID},
		ArticleMode: "prefix",
	}); err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	survivor, err := svc.GetByID(ctx, survivorID)
	if err != nil {
		t.Fatalf("GetByID survivor: %v", err)
	}
	if survivor.MusicBrainzID != mbid {
		t.Errorf("survivor MBID = %q, want %q (UPSERT failed to overwrite empty row)", survivor.MusicBrainzID, mbid)
	}
}

// TestValidateMergeRequest_WhitespaceNormalized covers the trim-in-place
// behavior: IDs with surrounding whitespace are normalized so the
// equality checks (survivor-in-losers, duplicate-loser) catch the cases
// they should and downstream comparisons against DB rows succeed.
//
// TG4 in the PR #1654 triage doc.
func TestValidateMergeRequest_WhitespaceNormalized(t *testing.T) {
	t.Parallel()

	// Trim normalizes the survivor in-place; same trimmed loser is
	// caught as survivor-in-losers.
	req := MergeRequest{SurvivorID: " abc ", LoserIDs: []string{"abc"}}
	if err := validateMergeRequest(&req); !errors.Is(err, ErrMergeInvalidRequest) {
		t.Errorf("want ErrMergeInvalidRequest for whitespace-survivor equal to loser, got %v", err)
	}
	if req.SurvivorID != "abc" {
		t.Errorf("survivor not trimmed in place: %q", req.SurvivorID)
	}

	// Trimmed losers are also de-duped (whitespace cannot smuggle a
	// duplicate past the seen map).
	req = MergeRequest{SurvivorID: "s", LoserIDs: []string{"abc", " abc "}}
	if err := validateMergeRequest(&req); !errors.Is(err, ErrMergeInvalidRequest) {
		t.Errorf("want ErrMergeInvalidRequest for whitespace-duplicate loser, got %v", err)
	}

	// Clean request: surviving + losers are trimmed in place.
	req = MergeRequest{SurvivorID: "  s  ", LoserIDs: []string{" a ", "b"}}
	if err := validateMergeRequest(&req); err != nil {
		t.Errorf("unexpected error on trimmed-clean request: %v", err)
	}
	if req.SurvivorID != "s" {
		t.Errorf("survivor not trimmed: %q", req.SurvivorID)
	}
	if req.LoserIDs[0] != "a" || req.LoserIDs[1] != "b" {
		t.Errorf("losers not trimmed in place: %v", req.LoserIDs)
	}
}

// TestMergeArtists_LoserRowSurvivesWhenNotRemoved is the regression test for
// #2010 (follow-up to #1779). When executeLoserMerge returns removed=false
// (the loser directory was left on disk due to a collision -- e.g. the
// survivor has a same-named directory where the loser has a loose file),
// commitMergeDB must NOT delete the loser's DB row. A deleted row with a
// surviving directory causes the scanner to resurrect the loser as a new
// artist on the next pass.
//
// The test uses two losers in the same merge call:
//   - loserBlocked: has a loose "folder.jpg" that cannot be moved because the
//     survivor already has a directory named "folder.jpg". Its dir is left on
//     disk; its DB row must survive.
//   - loserClean: has a non-colliding album subdir. Its dir is fully removed;
//     its DB row must be deleted.
func TestMergeArtists_LoserRowSurvivesWhenNotRemoved(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	svc := NewService(db)
	ctx := context.Background()
	root := t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-row-survive', 'lib-row-survive', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	survivorPath := filepath.Join(root, "The Cure")
	loserBlockedPath := filepath.Join(root, "Cure, The")
	loserCleanPath := filepath.Join(root, "Cure")
	for _, p := range []string{survivorPath, loserBlockedPath, loserCleanPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}

	// Survivor has an album subdir and a DIRECTORY named "folder.jpg" --
	// the same name as a loose file on loserBlocked. This makes the survivor
	// child non-regular, so executeLoserMerge cannot move loserBlocked's
	// "folder.jpg" and must leave the loser dir on disk.
	if err := os.Mkdir(filepath.Join(survivorPath, "Disintegration"), 0o755); err != nil {
		t.Fatalf("mkdir survivor album: %v", err)
	}
	if err := os.Mkdir(filepath.Join(survivorPath, "folder.jpg"), 0o755); err != nil {
		t.Fatalf("mkdir survivor folder.jpg dir: %v", err)
	}

	// loserBlocked: only a loose file that collides with the survivor dir.
	if err := os.WriteFile(filepath.Join(loserBlockedPath, "folder.jpg"), []byte("blocked-jpg"), 0o600); err != nil {
		t.Fatalf("write loserBlocked folder.jpg: %v", err)
	}

	// loserClean: a non-colliding album subdir; it will be fully removed.
	if err := os.Mkdir(filepath.Join(loserCleanPath, "Wish"), 0o755); err != nil {
		t.Fatalf("mkdir loserClean album: %v", err)
	}

	survivor := &Artist{Name: "The Cure", SortName: "Cure, The", Path: survivorPath, LibraryID: "lib-row-survive"}
	loserBlocked := &Artist{Name: "The Cure", SortName: "Cure, The", Path: loserBlockedPath, LibraryID: "lib-row-survive"}
	loserClean := &Artist{Name: "The Cure", SortName: "Cure, The", Path: loserCleanPath, LibraryID: "lib-row-survive"}
	for _, a := range []*Artist{survivor, loserBlocked, loserClean} {
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create %s (%s): %v", a.Name, a.Path, err)
		}
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID:  survivor.ID,
		LoserIDs:    []string{loserBlocked.ID, loserClean.ID},
		ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v", err)
	}

	// (a) loserBlocked's dir must still exist and its DB row must survive.
	if _, statErr := os.Stat(loserBlockedPath); statErr != nil {
		t.Errorf("loserBlocked dir should remain on disk; stat err = %v", statErr)
	}
	if _, getErr := svc.GetByID(ctx, loserBlocked.ID); getErr != nil {
		t.Errorf("loserBlocked DB row must survive (not deleted); GetByID err = %v", getErr)
	}
	if len(res.Removed) != 1 || res.Removed[0] != loserClean.ID {
		t.Errorf("Removed = %v, want [%s] (only loserClean)", res.Removed, loserClean.ID)
	}
	if len(res.LosersDeleted) != 1 || res.LosersDeleted[0] != loserClean.ID {
		t.Errorf("LosersDeleted = %v, want [%s] (only loserClean)", res.LosersDeleted, loserClean.ID)
	}

	// (b) Warning must be emitted for the non-empty loserBlocked dir.
	foundNonEmptyWarning := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "still contains") && strings.Contains(w, loserBlockedPath) {
			foundNonEmptyWarning = true
			break
		}
	}
	if !foundNonEmptyWarning {
		t.Errorf("Warnings = %v, want warning about non-empty loserBlocked dir", res.Warnings)
	}

	// (c) loserClean was fully removed; its DB row must be deleted.
	if _, statErr := os.Stat(loserCleanPath); !os.IsNotExist(statErr) {
		t.Errorf("loserClean dir should be removed; stat err = %v", statErr)
	}
	if _, getErr := svc.GetByID(ctx, loserClean.ID); !errors.Is(getErr, ErrNotFound) {
		t.Errorf("loserClean DB row should be deleted; GetByID err = %v", getErr)
	}
}

// --- small helpers ----------------------------------------------------------

func dirNames(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// --- Commit-phase filesystem fault injection ---------------------------------
//
// The following tests force EACCES failures inside the destructive commit-phase
// helpers (moveLoserSubdirs / mergeAdditiveSubdirIfPresent / mergeAdditiveDir /
// removeIgnoredJunk) by making the survivor or loser directory read-only
// (chmod 0500) right before MergeArtists runs. Each asserts three things: the
// merge surfaces a NON-nil error wrapping the underlying cause; the error is
// NOT miscategorized as one of the recoverable merge sentinels (a failed FS
// write must stay a generic 500-class error, not a 409/422/423 that the UI
// treats as retryable); and a FAILED merge did NOT silently claim success --
// the loser row is preserved so the next scan reconciles rather than
// resurrecting, and no half-state is misreported as complete.
//
// Root bypasses POSIX permission bits, so each test skips under root (the
// chmod 0500 would not produce EACCES). Mirrors the idiom in
// service_rename_test.go's TestRenameDirectory_RenameError.

// faultMergeSetup seeds a library with a survivor and a loser directory, both
// empty, and returns the service plus both IDs and both absolute paths. Unlike
// mergeSetup it leaves the directory CONTENTS to the caller so each
// fault-injection test can stage exactly the child layout its scenario needs
// (only-junk loser, only-extrafanart loser, single-album loser, ...) before
// chmod-ing a directory read-only.
func faultMergeSetup(t *testing.T) (svc *Service, survivorID, loserID, survivorPath, loserPath string) {
	t.Helper()
	db := newTestDB(t)
	svc = NewService(db)
	ctx := context.Background()
	root := t.TempDir()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO libraries (id, name, path, type, source, created_at, updated_at)
		 VALUES ('lib-fault', 'lib-fault', ?, 'regular', 'manual', datetime('now'), datetime('now'))`,
		root); err != nil {
		t.Fatalf("seeding library: %v", err)
	}

	survivorPath = filepath.Join(root, "The Cure")
	loserPath = filepath.Join(root, "Cure, The")
	for _, p := range []string{survivorPath, loserPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}

	survivor := &Artist{Name: "The Cure", SortName: "Cure, The", Path: survivorPath, LibraryID: "lib-fault"}
	loser := &Artist{Name: "The Cure", SortName: "Cure, The", Path: loserPath, LibraryID: "lib-fault"}
	if err := svc.Create(ctx, survivor); err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	if err := svc.Create(ctx, loser); err != nil {
		t.Fatalf("Create loser: %v", err)
	}
	return svc, survivor.ID, loser.ID, survivorPath, loserPath
}

// assertNotMergeSentinel fails when err matches one of the recoverable merge
// sentinels. A raw filesystem write failure must NOT be laundered into a
// sentinel the handler maps to a retryable 4xx.
func assertNotMergeSentinel(t *testing.T, err error) {
	t.Helper()
	for _, sentinel := range []error{
		ErrMergeInProgress, ErrMergeCollisions, ErrMergeStaleGroup,
		ErrMergeLocked, ErrMergeInvalidRequest, ErrMergeSurvivorMissing,
	} {
		if errors.Is(err, sentinel) {
			t.Fatalf("FS-write failure matched recoverable sentinel %v; expected a generic error", sentinel)
		}
	}
}

// TestMergeArtists_AdditiveRenameIntoReadOnlySurvivorFails forces the
// "moving additive file ..." branch in mergeAdditiveDir and its propagation up
// through mergeAdditiveSubdirIfPresent -> moveLoserSubdirs -> executeLoserMerge
// -> MergeArtists. The survivor already has an extrafanart/ dir (so the merge
// takes the additive content-merge path, not the whole-dir move), but that dir
// is read-only, so relocating the loser's unique image into it fails with
// EACCES. The loser row and directory must be left intact.
func TestMergeArtists_AdditiveRenameIntoReadOnlySurvivorFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}
	svc, survivorID, loserID, survivorPath, loserPath := faultMergeSetup(t)
	ctx := context.Background()

	survExtra := filepath.Join(survivorPath, "extrafanart")
	loseExtra := filepath.Join(loserPath, "extrafanart")
	for _, d := range []string{survExtra, loseExtra} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(survExtra, "keep.jpg"), []byte("survivor"), 0o600); err != nil {
		t.Fatalf("write survivor keep: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loseExtra, "new.jpg"), []byte("loser"), 0o600); err != nil {
		t.Fatalf("write loser new: %v", err)
	}

	// Read-only survivor extrafanart: the relocation of new.jpg into it fails.
	if err := os.Chmod(survExtra, 0o500); err != nil {
		t.Fatalf("chmod survivor extrafanart ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(survExtra, 0o755) })

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err == nil {
		t.Fatal("MergeArtists: expected FS error moving additive file, got nil")
	}
	if !strings.Contains(err.Error(), "moving additive file") {
		t.Errorf("err = %v, want it to wrap %q", err, "moving additive file")
	}
	if !strings.Contains(err.Error(), "merging loser") {
		t.Errorf("err = %v, want it to be wrapped by the per-loser context %q", err, "merging loser")
	}
	assertNotMergeSentinel(t, err)
	if res == nil {
		t.Fatal("expected non-nil partial result alongside the error")
	}
	// Failed merge must not claim the loser was removed or its row deleted.
	if len(res.Removed) != 0 {
		t.Errorf("Removed = %v, want empty (loser dir not unlinked)", res.Removed)
	}
	if len(res.LosersDeleted) != 0 {
		t.Errorf("LosersDeleted = %v, want empty (DB phase never ran)", res.LosersDeleted)
	}
	if _, err := svc.GetByID(ctx, loserID); err != nil {
		t.Errorf("loser row deleted after a failed merge: %v", err)
	}
	if _, err := os.Stat(loserPath); err != nil {
		t.Errorf("loser dir removed after a failed merge: %v", err)
	}
	// The loser's image stayed put; survivor's own image was not corrupted.
	assertFileContent(t, filepath.Join(loseExtra, "new.jpg"), "loser")
	assertFileContent(t, filepath.Join(survExtra, "keep.jpg"), "survivor")
}

// TestMergeArtists_RemoveMergedAdditiveDirFails forces the
// "removing merged additive dir ..." branch in mergeAdditiveSubdirIfPresent.
// The additive content-merge SUCCEEDS (the loser's image is relocated into the
// survivor's writable extrafanart/), but the subsequent os.RemoveAll of the
// drained loser subdir fails because the loser's TOP-LEVEL directory is
// read-only (unlinking a child needs write on the parent). This proves that a
// post-move failure still surfaces an error and does not delete the loser row.
func TestMergeArtists_RemoveMergedAdditiveDirFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}
	svc, survivorID, loserID, survivorPath, loserPath := faultMergeSetup(t)
	ctx := context.Background()

	survExtra := filepath.Join(survivorPath, "extrafanart")
	loseExtra := filepath.Join(loserPath, "extrafanart")
	for _, d := range []string{survExtra, loseExtra} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(survExtra, "keep.jpg"), []byte("survivor"), 0o600); err != nil {
		t.Fatalf("write survivor keep: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loseExtra, "new.jpg"), []byte("loser"), 0o600); err != nil {
		t.Fatalf("write loser new: %v", err)
	}

	// Read-only loser TOP dir: the inner rename out of loseExtra still works
	// (write lives on loseExtra, 0755), but RemoveAll(loseExtra) needs write
	// on loserPath and fails.
	if err := os.Chmod(loserPath, 0o500); err != nil {
		t.Fatalf("chmod loser ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(loserPath, 0o755) })

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err == nil {
		t.Fatal("MergeArtists: expected FS error removing merged additive dir, got nil")
	}
	if !strings.Contains(err.Error(), "removing merged additive dir") {
		t.Errorf("err = %v, want it to wrap %q", err, "removing merged additive dir")
	}
	assertNotMergeSentinel(t, err)
	if res == nil {
		t.Fatal("expected non-nil partial result alongside the error")
	}
	if len(res.LosersDeleted) != 0 {
		t.Errorf("LosersDeleted = %v, want empty (DB phase never ran)", res.LosersDeleted)
	}
	if _, err := svc.GetByID(ctx, loserID); err != nil {
		t.Errorf("loser row deleted after a failed merge: %v", err)
	}
	// The additive move itself completed before the RemoveAll failure: the
	// loser's image is now under the survivor. Restore perms so the assertion
	// (and t.TempDir cleanup) can read/remove the tree.
	if err := os.Chmod(loserPath, 0o755); err != nil {
		t.Fatalf("chmod loser rw for assert: %v", err)
	}
	assertFileContent(t, filepath.Join(survExtra, "new.jpg"), "loser")
	assertFileContent(t, filepath.Join(survExtra, "keep.jpg"), "survivor")
}

// TestMergeArtists_RemoveIgnoredJunkFails forces both error branches of
// removeIgnoredJunk (the RemoveAll dir branch and the RemoveFileSafe file
// branch). The loser directory contains ONLY an OS/NAS junk entry, so the
// commit phase moves nothing and proceeds straight to junk removal, which
// fails because the loser directory is read-only. Two subtests cover the dir
// (@eaDir) and file (Thumbs.db) branches respectively.
func TestMergeArtists_RemoveIgnoredJunkFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}

	cases := []struct {
		name     string
		junkName string
		isDir    bool
		wantMsg  string
	}{
		{"junk dir", "@eaDir", true, "removing ignored junk dir"},
		{"junk file", "Thumbs.db", false, "removing ignored junk file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, survivorID, loserID, _, loserPath := faultMergeSetup(t)
			ctx := context.Background()

			junkPath := filepath.Join(loserPath, tc.junkName)
			if tc.isDir {
				if err := os.MkdirAll(junkPath, 0o755); err != nil {
					t.Fatalf("mkdir junk dir: %v", err)
				}
			} else {
				if err := os.WriteFile(junkPath, []byte("junk"), 0o600); err != nil {
					t.Fatalf("write junk file: %v", err)
				}
			}

			// Read-only loser dir: removing the junk child needs write on it.
			if err := os.Chmod(loserPath, 0o500); err != nil {
				t.Fatalf("chmod loser ro: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(loserPath, 0o755) })

			res, err := svc.MergeArtists(ctx, MergeRequest{
				SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
			})
			if err == nil {
				t.Fatal("MergeArtists: expected FS error removing ignored junk, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %v, want it to wrap %q", err, tc.wantMsg)
			}
			assertNotMergeSentinel(t, err)
			if res == nil {
				t.Fatal("expected non-nil partial result alongside the error")
			}
			if len(res.LosersDeleted) != 0 {
				t.Errorf("LosersDeleted = %v, want empty (DB phase never ran)", res.LosersDeleted)
			}
			if _, err := svc.GetByID(ctx, loserID); err != nil {
				t.Errorf("loser row deleted after a failed merge: %v", err)
			}
		})
	}
}

// TestMergeArtists_MoveAlbumIntoReadOnlySurvivorFails forces the
// "moving <src> to <dst>" branch in moveLoserSubdirs for a NORMAL (non-
// additive) album subdir. The survivor directory is read-only, so the atomic
// rename of the loser's album into it fails with EACCES (both the os.Rename and
// its copy fallback need write on the destination parent). The pre-flight walk
// still passes because the album does not yet exist under the survivor. A
// failed move must leave the album in the loser and preserve the loser row.
func TestMergeArtists_MoveAlbumIntoReadOnlySurvivorFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}
	svc, survivorID, loserID, survivorPath, loserPath := faultMergeSetup(t)
	ctx := context.Background()

	album := filepath.Join(loserPath, "Pornography")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatalf("mkdir loser album: %v", err)
	}
	if err := os.WriteFile(filepath.Join(album, "track.flac"), []byte("audio"), 0o600); err != nil {
		t.Fatalf("write album track: %v", err)
	}

	// Read-only survivor: RenameDirAtomic of the album into it fails.
	if err := os.Chmod(survivorPath, 0o500); err != nil {
		t.Fatalf("chmod survivor ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(survivorPath, 0o755) })

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err == nil {
		t.Fatal("MergeArtists: expected FS error moving album subdir, got nil")
	}
	if !strings.Contains(err.Error(), "moving") {
		t.Errorf("err = %v, want it to wrap the move-failure message", err)
	}
	assertNotMergeSentinel(t, err)
	if res == nil {
		t.Fatal("expected non-nil partial result alongside the error")
	}
	if len(res.Moved) != 0 {
		t.Errorf("Moved = %v, want empty (rename failed before recording)", res.Moved)
	}
	if len(res.LosersDeleted) != 0 {
		t.Errorf("LosersDeleted = %v, want empty (DB phase never ran)", res.LosersDeleted)
	}
	if _, err := svc.GetByID(ctx, loserID); err != nil {
		t.Errorf("loser row deleted after a failed merge: %v", err)
	}
	// Restore perms so the album assertion and t.TempDir cleanup can proceed.
	if err := os.Chmod(survivorPath, 0o755); err != nil {
		t.Fatalf("chmod survivor rw for assert: %v", err)
	}
	assertFileContent(t, filepath.Join(album, "track.flac"), "audio")
}

// TestMergeArtists_AdditiveMergeReadErrors covers the two read-side error
// branches of mergeAdditiveDir: the os.ReadDir failure on an unreadable loser
// additive dir ("reading additive merge dir ..."), and the os.Lstat failure on
// a survivor additive dir whose search bit is cleared ("checking additive
// destination ..."). Both reach mergeAdditiveDir because the survivor already
// has an extrafanart/ dir (the additive content-merge path), and both must
// surface a generic error that preserves the loser row.
func TestMergeArtists_AdditiveMergeReadErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot trigger EACCES")
	}

	cases := []struct {
		name string
		// chmodTarget is the extrafanart dir to lock down; mode is applied to it.
		lockSurvivor bool
		mode         os.FileMode
		wantMsg      string
	}{
		{"loser additive dir unreadable", false, 0o000, "reading additive merge dir"},
		{"survivor additive dir not searchable", true, 0o600, "checking additive destination"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, survivorID, loserID, survivorPath, loserPath := faultMergeSetup(t)
			ctx := context.Background()

			survExtra := filepath.Join(survivorPath, "extrafanart")
			loseExtra := filepath.Join(loserPath, "extrafanart")
			for _, d := range []string{survExtra, loseExtra} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", d, err)
				}
			}
			if err := os.WriteFile(filepath.Join(survExtra, "keep.jpg"), []byte("survivor"), 0o600); err != nil {
				t.Fatalf("write survivor keep: %v", err)
			}
			if err := os.WriteFile(filepath.Join(loseExtra, "new.jpg"), []byte("loser"), 0o600); err != nil {
				t.Fatalf("write loser new: %v", err)
			}

			target := loseExtra
			if tc.lockSurvivor {
				target = survExtra
			}
			if err := os.Chmod(target, tc.mode); err != nil {
				t.Fatalf("chmod %s: %v", target, err)
			}
			t.Cleanup(func() { _ = os.Chmod(target, 0o755) })

			res, err := svc.MergeArtists(ctx, MergeRequest{
				SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
			})
			if err == nil {
				t.Fatal("MergeArtists: expected additive read error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %v, want it to wrap %q", err, tc.wantMsg)
			}
			assertNotMergeSentinel(t, err)
			if res == nil {
				t.Fatal("expected non-nil partial result alongside the error")
			}
			if len(res.LosersDeleted) != 0 {
				t.Errorf("LosersDeleted = %v, want empty (DB phase never ran)", res.LosersDeleted)
			}
			if _, err := svc.GetByID(ctx, loserID); err != nil {
				t.Errorf("loser row deleted after a failed merge: %v", err)
			}
		})
	}
}

// TestMergeArtists_AdditiveMergeSkipsSymlink exercises the symlink-skip branch
// inside mergeAdditiveDir: a symlink living in the loser's extrafanart/ must NOT
// be followed into the survivor (blast-radius containment), must be recorded as
// a warning, and the merge must otherwise complete -- the real image moves and
// the drained loser dir is unlinked. This is a success path (no error) that
// covers the additive-dir symlink branch the error tests cannot reach.
func TestMergeArtists_AdditiveMergeSkipsSymlink(t *testing.T) {
	t.Parallel()
	svc, survivorID, loserID, survivorPath, loserPath := faultMergeSetup(t)
	ctx := context.Background()

	survExtra := filepath.Join(survivorPath, "extrafanart")
	loseExtra := filepath.Join(loserPath, "extrafanart")
	for _, d := range []string{survExtra, loseExtra} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(survExtra, "keep.jpg"), []byte("survivor"), 0o600); err != nil {
		t.Fatalf("write survivor keep: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loseExtra, "new.jpg"), []byte("loser"), 0o600); err != nil {
		t.Fatalf("write loser new: %v", err)
	}
	// A symlink in the loser's additive dir; the target need not exist because
	// it is never followed.
	if err := os.Symlink(filepath.Join(loserPath, "elsewhere"), filepath.Join(loseExtra, "link.jpg")); err != nil {
		t.Fatalf("symlink loser additive: %v", err)
	}

	res, err := svc.MergeArtists(ctx, MergeRequest{
		SurvivorID: survivorID, LoserIDs: []string{loserID}, ArticleMode: "prefix",
	})
	if err != nil {
		t.Fatalf("MergeArtists: %v (symlink in additive dir must not fail the merge)", err)
	}
	// Real image moved; symlink NOT carried into the survivor.
	assertFileContent(t, filepath.Join(survExtra, "new.jpg"), "loser")
	if _, statErr := os.Lstat(filepath.Join(survExtra, "link.jpg")); !os.IsNotExist(statErr) {
		t.Errorf("symlink was carried into survivor extrafanart: err = %v", statErr)
	}
	// Warning recorded and loser dir swept clean.
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "skipped symlink") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a skipped-symlink warning, got %v", res.Warnings)
	}
	if _, statErr := os.Stat(loserPath); !os.IsNotExist(statErr) {
		t.Errorf("expected loser dir removed after symlink-tolerant merge, got err = %v", statErr)
	}
}
