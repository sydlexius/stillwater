package artist

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/database"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testArtist(name, path string) *Artist {
	return &Artist{
		Name:           name,
		SortName:       name,
		Type:           "group",
		Gender:         "",
		Disambiguation: "",
		MusicBrainzID:  "",
		Path:           path,
		Genres:         []string{"Rock", "Alternative"},
		Styles:         []string{"Grunge"},
		Moods:          []string{"Energetic"},
		Biography:      "A test artist.",
	}
}

func TestCreateAndGetByID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Nirvana", "/music/Nirvana")
	a.MusicBrainzID = "5b11f4ce-a62d-471e-81fc-a69a8278c7da"
	a.AudioDBID = "111239"
	a.YearsActive = "1987-1994"
	a.Formed = "1987"
	a.Disbanded = "1994"

	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if a.ID == "" {
		t.Fatal("expected ID to be set after Create")
	}

	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if got.Name != "Nirvana" {
		t.Errorf("Name = %q, want %q", got.Name, "Nirvana")
	}
	if got.MusicBrainzID != a.MusicBrainzID {
		t.Errorf("MusicBrainzID = %q, want %q", got.MusicBrainzID, a.MusicBrainzID)
	}
	if got.AudioDBID != "111239" {
		t.Errorf("AudioDBID = %q, want %q", got.AudioDBID, "111239")
	}
	if len(got.Genres) != 2 || got.Genres[0] != "Rock" {
		t.Errorf("Genres = %v, want [Rock Alternative]", got.Genres)
	}
	if got.YearsActive != "1987-1994" {
		t.Errorf("YearsActive = %q, want %q", got.YearsActive, "1987-1994")
	}
	if got.Biography != "A test artist." {
		t.Errorf("Biography = %q, want %q", got.Biography, "A test artist.")
	}
	if got.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
}

func TestGetByID_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	_, err := svc.GetByID(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent ID")
	}
}

func TestGetByMBID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Radiohead", "/music/Radiohead")
	a.MusicBrainzID = "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.GetByMBID(ctx, a.MusicBrainzID)
	if err != nil {
		t.Fatalf("GetByMBID: %v", err)
	}
	if got == nil {
		t.Fatal("expected artist, got nil")
	}
	if got.Name != "Radiohead" {
		t.Errorf("Name = %q, want %q", got.Name, "Radiohead")
	}
}

func TestGetByMBID_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	got, err := svc.GetByMBID(context.Background(), "nonexistent-mbid")
	if err != nil {
		t.Fatalf("GetByMBID: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestGetByProviderID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Tool", "/music/Tool")
	a.DiscogsID = "54321"
	a.WikidataID = "Q184843"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Test discogs lookup
	got, err := svc.GetByProviderID(ctx, "discogs", "54321")
	if err != nil {
		t.Fatalf("GetByProviderID(discogs): %v", err)
	}
	if got == nil || got.Name != "Tool" {
		t.Errorf("discogs lookup: got %v, want Tool", got)
	}

	// Test wikidata lookup
	got, err = svc.GetByProviderID(ctx, "wikidata", "Q184843")
	if err != nil {
		t.Fatalf("GetByProviderID(wikidata): %v", err)
	}
	if got == nil || got.Name != "Tool" {
		t.Errorf("wikidata lookup: got %v, want Tool", got)
	}

	// Test unknown provider
	_, err = svc.GetByProviderID(ctx, "spotify", "123")
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestGetByPath(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Pink Floyd", "/music/Pink Floyd")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.GetByPath(ctx, "/music/Pink Floyd")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got == nil || got.Name != "Pink Floyd" {
		t.Errorf("path lookup: got %v, want Pink Floyd", got)
	}

	// Not found
	got, err = svc.GetByPath(ctx, "/music/Nonexistent")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent path, got %+v", got)
	}
}

func TestList_Pagination(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	names := []string{"Alice", "Bob", "Charlie", "Dave", "Eve", "Frank", "Grace", "Heidi", "Ivan", "Judy"}
	for _, name := range names {
		if err := svc.Create(ctx, testArtist(name, "/music/"+name)); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	// Page 1
	artists, total, err := svc.List(ctx, ListParams{Page: 1, PageSize: 5, Sort: "name", Order: "asc"})
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	if len(artists) != 5 {
		t.Errorf("page 1 len = %d, want 5", len(artists))
	}
	if artists[0].Name != "Alice" {
		t.Errorf("first artist = %q, want Alice", artists[0].Name)
	}

	// Page 2
	artists, _, err = svc.List(ctx, ListParams{Page: 2, PageSize: 5, Sort: "name", Order: "asc"})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	if len(artists) != 5 {
		t.Errorf("page 2 len = %d, want 5", len(artists))
	}
	if artists[0].Name != "Frank" {
		t.Errorf("first artist page 2 = %q, want Frank", artists[0].Name)
	}
}

func TestList_SearchAndFilter(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a1 := testArtist("The Beatles", "/music/The Beatles")
	a1.NFOExists = true
	a1.MusicBrainzID = "b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d"

	a2 := testArtist("The Rolling Stones", "/music/The Rolling Stones")
	// a2 has no NFO and no MBID

	if err := svc.Create(ctx, a1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Create(ctx, a2); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Search by name
	artists, total, err := svc.List(ctx, ListParams{Search: "Beatles"})
	if err != nil {
		t.Fatalf("List search: %v", err)
	}
	if total != 1 {
		t.Errorf("search total = %d, want 1", total)
	}
	if len(artists) != 1 || artists[0].Name != "The Beatles" {
		t.Errorf("search result = %v, want The Beatles", artists)
	}

	// Filter missing NFO
	artists, total, err = svc.List(ctx, ListParams{Filter: "missing_nfo"})
	if err != nil {
		t.Fatalf("List filter missing_nfo: %v", err)
	}
	if total != 1 || artists[0].Name != "The Rolling Stones" {
		t.Errorf("missing_nfo filter: total=%d, artists=%v", total, artists)
	}

	// Filter missing MBID
	artists, total, err = svc.List(ctx, ListParams{Filter: "missing_mbid"})
	if err != nil {
		t.Fatalf("List filter missing_mbid: %v", err)
	}
	if total != 1 || artists[0].Name != "The Rolling Stones" {
		t.Errorf("missing_mbid filter: total=%d, artists=%v", total, artists)
	}
}

func TestUpdate(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Led Zeppelin", "/music/Led Zeppelin")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	a.MusicBrainzID = "678d88b2-87b0-403b-b63d-5da7465aecc3"
	a.NFOExists = true
	a.ThumbExists = true
	a.Biography = "English rock band."
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.MusicBrainzID != a.MusicBrainzID {
		t.Errorf("MusicBrainzID = %q, want %q", got.MusicBrainzID, a.MusicBrainzID)
	}
	if !got.NFOExists {
		t.Error("NFOExists should be true")
	}
	if !got.ThumbExists {
		t.Error("ThumbExists should be true")
	}
	if got.Biography != "English rock band." {
		t.Errorf("Biography = %q, want %q", got.Biography, "English rock band.")
	}
}

func TestDelete(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Temporary", "/music/Temporary")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := svc.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := svc.GetByID(ctx, a.ID)
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestDelete_NotFound(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)

	err := svc.Delete(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent delete")
	}
}

func TestSearch(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	for _, name := range []string{"Metallica", "Megadeth", "Meshuggah"} {
		if err := svc.Create(ctx, testArtist(name, "/music/"+name)); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	if err := svc.Create(ctx, testArtist("Radiohead", "/music/Radiohead")); err != nil {
		t.Fatalf("Create Radiohead: %v", err)
	}

	results, err := svc.Search(ctx, "Me")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("search results = %d, want 3", len(results))
	}
}

func TestBandMembers_CRUD(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Nirvana", "/music/Nirvana")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create artist: %v", err)
	}

	m := &BandMember{
		ArtistID:         a.ID,
		MemberName:       "Kurt Cobain",
		MemberMBID:       "some-mbid",
		Instruments:      []string{"guitar", "vocals"},
		VocalType:        "lead vocals",
		DateJoined:       "1987",
		IsOriginalMember: true,
		SortOrder:        1,
	}
	if err := svc.CreateMember(ctx, m); err != nil {
		t.Fatalf("CreateMember: %v", err)
	}

	members, err := svc.ListMembersByArtistID(ctx, a.ID)
	if err != nil {
		t.Fatalf("ListMembersByArtistID: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("members count = %d, want 1", len(members))
	}
	if members[0].MemberName != "Kurt Cobain" {
		t.Errorf("MemberName = %q, want %q", members[0].MemberName, "Kurt Cobain")
	}
	if len(members[0].Instruments) != 2 {
		t.Errorf("Instruments = %v, want [guitar vocals]", members[0].Instruments)
	}
	if !members[0].IsOriginalMember {
		t.Error("expected IsOriginalMember to be true")
	}

	// Delete member
	if err := svc.DeleteMember(ctx, m.ID); err != nil {
		t.Fatalf("DeleteMember: %v", err)
	}
	members, _ = svc.ListMembersByArtistID(ctx, a.ID)
	if len(members) != 0 {
		t.Errorf("members after delete = %d, want 0", len(members))
	}
}

func TestUpsertMembers(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Nirvana", "/music/Nirvana")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create artist: %v", err)
	}

	// First upsert
	members := []BandMember{
		{MemberName: "Kurt Cobain", Instruments: []string{"guitar"}, SortOrder: 1},
		{MemberName: "Krist Novoselic", Instruments: []string{"bass"}, SortOrder: 2},
	}
	if err := svc.UpsertMembers(ctx, a.ID, members); err != nil {
		t.Fatalf("UpsertMembers: %v", err)
	}

	got, _ := svc.ListMembersByArtistID(ctx, a.ID)
	if len(got) != 2 {
		t.Fatalf("members after first upsert = %d, want 2", len(got))
	}

	// Second upsert replaces all
	members2 := []BandMember{
		{MemberName: "Kurt Cobain", Instruments: []string{"guitar", "vocals"}, SortOrder: 1},
		{MemberName: "Krist Novoselic", Instruments: []string{"bass"}, SortOrder: 2},
		{MemberName: "Dave Grohl", Instruments: []string{"drums"}, SortOrder: 3},
	}
	if err := svc.UpsertMembers(ctx, a.ID, members2); err != nil {
		t.Fatalf("UpsertMembers 2: %v", err)
	}

	got, _ = svc.ListMembersByArtistID(ctx, a.ID)
	if len(got) != 3 {
		t.Fatalf("members after second upsert = %d, want 3", len(got))
	}
}

func TestDeleteMembersByArtistID(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Test Band", "/music/Test Band")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	members := []BandMember{
		{MemberName: "Member 1", SortOrder: 1},
		{MemberName: "Member 2", SortOrder: 2},
	}
	if err := svc.UpsertMembers(ctx, a.ID, members); err != nil {
		t.Fatalf("UpsertMembers: %v", err)
	}

	if err := svc.DeleteMembersByArtistID(ctx, a.ID); err != nil {
		t.Fatalf("DeleteMembersByArtistID: %v", err)
	}

	got, _ := svc.ListMembersByArtistID(ctx, a.ID)
	if len(got) != 0 {
		t.Errorf("members after delete all = %d, want 0", len(got))
	}
}

func TestArtist_LastScannedAt(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Scanned", "/music/Scanned")
	now := time.Now().UTC().Truncate(time.Second)
	a.LastScannedAt = &now
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.LastScannedAt == nil {
		t.Fatal("expected LastScannedAt to be set")
	}
	if got.LastScannedAt.Unix() != now.Unix() {
		t.Errorf("LastScannedAt = %v, want %v", got.LastScannedAt, now)
	}
}

func TestList_ExcludedFilter(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a1 := testArtist("Normal Band", "/music/Normal Band")
	a2 := testArtist("Various Artists", "/music/Various Artists")
	a2.IsExcluded = true
	a2.ExclusionReason = "default exclusion list"

	if err := svc.Create(ctx, a1); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Create(ctx, a2); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Filter excluded
	artists, total, err := svc.List(ctx, ListParams{Filter: "excluded"})
	if err != nil {
		t.Fatalf("List excluded: %v", err)
	}
	if total != 1 || artists[0].Name != "Various Artists" {
		t.Errorf("excluded filter: total=%d, artists=%v", total, artists)
	}

	// Filter not excluded
	artists, total, err = svc.List(ctx, ListParams{Filter: "not_excluded"})
	if err != nil {
		t.Fatalf("List not_excluded: %v", err)
	}
	if total != 1 || artists[0].Name != "Normal Band" {
		t.Errorf("not_excluded filter: total=%d, artists=%v", total, artists)
	}
}

func TestList_SortOrder(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.Create(ctx, testArtist("Zephyr", "/music/Zephyr")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Create(ctx, testArtist("Alpha", "/music/Alpha")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// ASC
	artists, _, _ := svc.List(ctx, ListParams{Sort: "name", Order: "asc"})
	if len(artists) != 2 || artists[0].Name != "Alpha" {
		t.Errorf("asc sort: first = %q, want Alpha", artists[0].Name)
	}

	// DESC
	artists, _, _ = svc.List(ctx, ListParams{Sort: "name", Order: "desc"})
	if len(artists) != 2 || artists[0].Name != "Zephyr" {
		t.Errorf("desc sort: first = %q, want Zephyr", artists[0].Name)
	}
}

func TestLibraryID_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create a library to reference
	_, err := db.ExecContext(ctx, `
		INSERT INTO libraries (id, name, path, type, created_at, updated_at)
		VALUES ('lib-1', 'Test Library', '/music/test', 'regular', datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("creating library: %v", err)
	}

	// Create artist with library_id
	a := testArtist("Library Artist", "/music/Library Artist")
	a.LibraryID = "lib-1"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.LibraryID != "lib-1" {
		t.Errorf("LibraryID = %q, want %q", got.LibraryID, "lib-1")
	}

	// Update library_id
	got.LibraryID = "lib-1" // same value, just verify update path works
	if err := svc.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got2, _ := svc.GetByID(ctx, a.ID)
	if got2.LibraryID != "lib-1" {
		t.Errorf("LibraryID after update = %q, want %q", got2.LibraryID, "lib-1")
	}
}

func TestLibraryID_NullOnEmpty(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create artist without library_id (should store NULL, read back as "")
	a := testArtist("No Library", "/music/No Library")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.LibraryID != "" {
		t.Errorf("LibraryID = %q, want empty string", got.LibraryID)
	}
}

func TestGetByNameAndLibrary(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create two libraries
	for _, q := range []string{
		`INSERT INTO libraries (id, name, path, type, created_at, updated_at) VALUES ('lib-x', 'Library X', '/music/x', 'regular', datetime('now'), datetime('now'))`,
		`INSERT INTO libraries (id, name, path, type, created_at, updated_at) VALUES ('lib-y', 'Library Y', '/music/y', 'regular', datetime('now'), datetime('now'))`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("creating library: %v", err)
		}
	}

	a := testArtist("Deftones", "/music/Deftones")
	a.LibraryID = "lib-x"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Found in correct library
	got, err := svc.GetByNameAndLibrary(ctx, "Deftones", "lib-x")
	if err != nil {
		t.Fatalf("GetByNameAndLibrary: %v", err)
	}
	if got == nil || got.Name != "Deftones" {
		t.Errorf("expected Deftones, got %v", got)
	}

	// Not found in different library
	got, err = svc.GetByNameAndLibrary(ctx, "Deftones", "lib-y")
	if err != nil {
		t.Fatalf("GetByNameAndLibrary different lib: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for different library, got %+v", got)
	}

	// Not found (wrong name)
	got, err = svc.GetByNameAndLibrary(ctx, "Nonexistent", "lib-x")
	if err != nil {
		t.Fatalf("GetByNameAndLibrary not found: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent name, got %+v", got)
	}
}

func TestGetByMBIDAndLibrary(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create two libraries
	for _, q := range []string{
		`INSERT INTO libraries (id, name, path, type, created_at, updated_at) VALUES ('lib-m', 'Library M', '/music/m', 'regular', datetime('now'), datetime('now'))`,
		`INSERT INTO libraries (id, name, path, type, created_at, updated_at) VALUES ('lib-n', 'Library N', '/music/n', 'regular', datetime('now'), datetime('now'))`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("creating library: %v", err)
		}
	}

	mbid := "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	a := testArtist("Radiohead", "/music/Radiohead-lib")
	a.MusicBrainzID = mbid
	a.LibraryID = "lib-m"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Found in correct library
	got, err := svc.GetByMBIDAndLibrary(ctx, mbid, "lib-m")
	if err != nil {
		t.Fatalf("GetByMBIDAndLibrary: %v", err)
	}
	if got == nil || got.Name != "Radiohead" {
		t.Errorf("expected Radiohead, got %v", got)
	}

	// Not found in different library
	got, err = svc.GetByMBIDAndLibrary(ctx, mbid, "lib-n")
	if err != nil {
		t.Fatalf("GetByMBIDAndLibrary different lib: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for different library, got %+v", got)
	}

	// Not found (wrong MBID)
	got, err = svc.GetByMBIDAndLibrary(ctx, "nonexistent-mbid", "lib-m")
	if err != nil {
		t.Fatalf("GetByMBIDAndLibrary not found: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent MBID, got %+v", got)
	}
}

func TestList_LibraryIDFilter(t *testing.T) {
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	// Create two libraries
	for _, q := range []string{
		`INSERT INTO libraries (id, name, path, type, created_at, updated_at) VALUES ('lib-a', 'Library A', '/music/a', 'regular', datetime('now'), datetime('now'))`,
		`INSERT INTO libraries (id, name, path, type, created_at, updated_at) VALUES ('lib-b', 'Library B', '/music/b', 'regular', datetime('now'), datetime('now'))`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("creating library: %v", err)
		}
	}

	a1 := testArtist("Artist A", "/music/Artist A")
	a1.LibraryID = "lib-a"
	a2 := testArtist("Artist B", "/music/Artist B")
	a2.LibraryID = "lib-b"
	a3 := testArtist("Artist C", "/music/Artist C")
	a3.LibraryID = "lib-a"

	for _, a := range []*Artist{a1, a2, a3} {
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("Create %s: %v", a.Name, err)
		}
	}

	// Filter by lib-a
	artists, total, err := svc.List(ctx, ListParams{LibraryID: "lib-a"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	for _, a := range artists {
		if a.LibraryID != "lib-a" {
			t.Errorf("artist %q has LibraryID %q, want lib-a", a.Name, a.LibraryID)
		}
	}

	// Filter by lib-b
	artists, total, err = svc.List(ctx, ListParams{LibraryID: "lib-b"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if artists[0].Name != "Artist B" {
		t.Errorf("artist = %q, want Artist B", artists[0].Name)
	}
}
