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

// --- chooseSurvivor: pure-Go table-driven coverage ---------------------------

func TestChooseSurvivor(t *testing.T) {
	t.Parallel()

	// A small disk tree for the "most content" precedence cases. The path
	// values in the chooseSurvivor inputs point under this root.
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
			gotID, gotReason := chooseSurvivor(tc.members, tc.articleMode)
			if gotID != tc.wantID {
				t.Errorf("chooseSurvivor id = %q, want %q", gotID, tc.wantID)
			}
			if gotReason != tc.wantReason {
				t.Errorf("chooseSurvivor reason = %q, want %q", gotReason, tc.wantReason)
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
	target := filepath.Join(root, "Album A")
	link := filepath.Join(root, "link-to-A")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	subdirs, files, symlinks, err := enumerateChildren(root)
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
			err := validateMergeRequest(tc.req)
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
		survivor, _ := svc.GetByID(ctx, survivorID)
		p := filepath.Join(survivor.Path, album)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s on disk: %v", p, err)
		}
	}
	// Loose file moved.
	survivor, _ := svc.GetByID(ctx, survivorID)
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
	survivor, _ := svc.GetByID(ctx, survivorID)
	loser, _ := svc.GetByID(ctx, loserID)
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
	loser, _ := svc.GetByID(ctx, loserID)
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
	// survivor's path is "The Cure" (canonical). chooseSurvivor will
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
