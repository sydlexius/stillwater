package artist

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
)

func setupAliasTest(t *testing.T) *Service {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewService(db)
}

func createTestArtist(t *testing.T, svc *Service, name string) *Artist {
	t.Helper()
	a := &Artist{
		Name:     name,
		SortName: name,
		Type:     "person",
		Path:     "/music/" + name,
		Genres:   []string{"Rock"},
	}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAddAlias(t *testing.T) {
	svc := setupAliasTest(t)
	ctx := context.Background()
	artist := createTestArtist(t, svc, "Radiohead")

	alias, err := svc.AddAlias(ctx, artist.ID, "On a Friday", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if alias.ID == "" {
		t.Error("expected alias ID to be set")
	}
	if alias.Alias != "On a Friday" {
		t.Errorf("alias = %q, want 'On a Friday'", alias.Alias)
	}
}

func TestAddAlias_EmptyAlias(t *testing.T) {
	svc := setupAliasTest(t)
	ctx := context.Background()
	artist := createTestArtist(t, svc, "Radiohead")

	_, err := svc.AddAlias(ctx, artist.ID, "", "manual")
	if err == nil {
		t.Error("expected error for empty alias")
	}
}

func TestAddAlias_ArtistNotFound(t *testing.T) {
	svc := setupAliasTest(t)
	_, err := svc.AddAlias(context.Background(), "nonexistent", "alias", "manual")
	if err == nil {
		t.Error("expected error for nonexistent artist")
	}
}

func TestListAliases(t *testing.T) {
	svc := setupAliasTest(t)
	ctx := context.Background()
	artist := createTestArtist(t, svc, "Radiohead")

	svc.AddAlias(ctx, artist.ID, "On a Friday", "manual")
	svc.AddAlias(ctx, artist.ID, "RH", "manual")

	aliases, err := svc.ListAliases(ctx, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 2 {
		t.Fatalf("got %d aliases, want 2", len(aliases))
	}
}

func TestRemoveAlias(t *testing.T) {
	svc := setupAliasTest(t)
	ctx := context.Background()
	artist := createTestArtist(t, svc, "Radiohead")

	alias, _ := svc.AddAlias(ctx, artist.ID, "On a Friday", "manual")
	if err := svc.RemoveAlias(ctx, alias.ID); err != nil {
		t.Fatal(err)
	}

	aliases, _ := svc.ListAliases(ctx, artist.ID)
	if len(aliases) != 0 {
		t.Errorf("got %d aliases after removal, want 0", len(aliases))
	}
}

func TestRemoveAlias_NotFound(t *testing.T) {
	svc := setupAliasTest(t)
	if err := svc.RemoveAlias(context.Background(), "nonexistent"); err == nil {
		t.Error("expected error for nonexistent alias")
	}
}

func TestSearchWithAliases(t *testing.T) {
	svc := setupAliasTest(t)
	ctx := context.Background()

	a1 := createTestArtist(t, svc, "Radiohead")
	a2 := createTestArtist(t, svc, "Bjork")

	svc.AddAlias(ctx, a1.ID, "On a Friday", "manual")
	svc.AddAlias(ctx, a2.ID, "The Sugarcubes", "manual")

	// Search by name
	results, err := svc.SearchWithAliases(ctx, "radiohead")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("name search: got %d, want 1", len(results))
	}

	// Search by alias
	results, err = svc.SearchWithAliases(ctx, "sugarcubes")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("alias search: got %d, want 1", len(results))
	}
	if results[0].Name != "Bjork" {
		t.Errorf("got %q, want Bjork", results[0].Name)
	}
}

func TestFindDuplicates_SharedMBID(t *testing.T) {
	svc := setupAliasTest(t)
	ctx := context.Background()

	a1 := &Artist{Name: "Radiohead", SortName: "Radiohead", Type: "group", Path: "/music/Radiohead", MusicBrainzID: "mbid-001", Genres: []string{}}
	a2 := &Artist{Name: "Radiohead (UK)", SortName: "Radiohead", Type: "group", Path: "/music/Radiohead_UK", MusicBrainzID: "mbid-001", Genres: []string{}}
	svc.Create(ctx, a1)
	svc.Create(ctx, a2)

	groups, err := svc.FindDuplicates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 {
		t.Fatal("expected at least one duplicate group")
	}
	if len(groups[0].Artists) != 2 {
		t.Errorf("got %d artists in group, want 2", len(groups[0].Artists))
	}
}

func TestFindDuplicates_SharedAlias(t *testing.T) {
	svc := setupAliasTest(t)
	ctx := context.Background()

	a1 := createTestArtist(t, svc, "Radiohead")
	a2 := createTestArtist(t, svc, "On a Friday")

	svc.AddAlias(ctx, a1.ID, "On a Friday", "manual")
	svc.AddAlias(ctx, a2.ID, "On a Friday", "manual")

	groups, err := svc.FindDuplicates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range groups {
		if len(g.Artists) == 2 {
			found = true
		}
	}
	if !found {
		t.Error("expected to find a duplicate group with shared alias")
	}
}
