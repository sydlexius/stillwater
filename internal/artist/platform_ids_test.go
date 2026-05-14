package artist

import (
	"context"
	"errors"
	"testing"
)

func setupPlatformIDTest(t *testing.T) *Service {
	t.Helper()
	db := newTestDB(t)

	// Insert test connections for foreign key constraints.
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		VALUES ('conn-1', 'Test Emby', 'emby', 'http://emby:8096', 'enc-key', 1, 'ok', datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
		VALUES ('conn-2', 'Test Jellyfin', 'jellyfin', 'http://jf:8096', 'enc-key', 1, 'ok', datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatal(err)
	}

	return NewService(db)
}

func TestSetPlatformID(t *testing.T) {
	t.Parallel()
	svc := setupPlatformIDTest(t)
	ctx := context.Background()
	artist := createTestArtist(t, svc, "Radiohead")

	err := svc.SetPlatformID(ctx, artist.ID, "conn-1", "emby-item-123")
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetPlatformID(ctx, artist.ID, "conn-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "emby-item-123" {
		t.Errorf("got %q, want %q", got, "emby-item-123")
	}
}

func TestSetPlatformID_Upsert(t *testing.T) {
	t.Parallel()
	svc := setupPlatformIDTest(t)
	ctx := context.Background()
	artist := createTestArtist(t, svc, "Radiohead")

	svc.SetPlatformID(ctx, artist.ID, "conn-1", "old-id")
	err := svc.SetPlatformID(ctx, artist.ID, "conn-1", "new-id")
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetPlatformID(ctx, artist.ID, "conn-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "new-id" {
		t.Errorf("got %q, want %q", got, "new-id")
	}
}

func TestSetPlatformID_Validation(t *testing.T) {
	t.Parallel()
	svc := setupPlatformIDTest(t)
	ctx := context.Background()

	tests := []struct {
		name         string
		artistID     string
		connectionID string
		platformID   string
	}{
		{"empty artist_id", "", "conn-1", "plat-1"},
		{"empty connection_id", "artist-1", "", "plat-1"},
		{"empty platform_id", "artist-1", "conn-1", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := svc.SetPlatformID(ctx, tt.artistID, tt.connectionID, tt.platformID)
			if err == nil {
				t.Error("expected error for empty field")
			}
		})
	}
}

func TestGetPlatformID_NotFound(t *testing.T) {
	t.Parallel()
	svc := setupPlatformIDTest(t)
	ctx := context.Background()
	artist := createTestArtist(t, svc, "Radiohead")

	got, err := svc.GetPlatformID(ctx, artist.ID, "conn-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty string for missing platform id, got %q", got)
	}
}

func TestGetPlatformIDs(t *testing.T) {
	t.Parallel()
	svc := setupPlatformIDTest(t)
	ctx := context.Background()
	artist := createTestArtist(t, svc, "Radiohead")

	svc.SetPlatformID(ctx, artist.ID, "conn-1", "emby-123")
	svc.SetPlatformID(ctx, artist.ID, "conn-2", "jf-456")

	ids, err := svc.GetPlatformIDs(ctx, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d platform ids, want 2", len(ids))
	}

	found := map[string]string{}
	for _, p := range ids {
		found[p.ConnectionID] = p.PlatformArtistID
	}
	if found["conn-1"] != "emby-123" {
		t.Errorf("conn-1: got %q, want %q", found["conn-1"], "emby-123")
	}
	if found["conn-2"] != "jf-456" {
		t.Errorf("conn-2: got %q, want %q", found["conn-2"], "jf-456")
	}
}

func TestGetPlatformIDs_Empty(t *testing.T) {
	t.Parallel()
	svc := setupPlatformIDTest(t)
	ctx := context.Background()
	artist := createTestArtist(t, svc, "Radiohead")

	ids, err := svc.GetPlatformIDs(ctx, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ids != nil {
		t.Errorf("expected nil for no platform ids, got %v", ids)
	}
}

func TestDeletePlatformID(t *testing.T) {
	t.Parallel()
	svc := setupPlatformIDTest(t)
	ctx := context.Background()
	artist := createTestArtist(t, svc, "Radiohead")

	svc.SetPlatformID(ctx, artist.ID, "conn-1", "emby-123")

	err := svc.DeletePlatformID(ctx, artist.ID, "conn-1")
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetPlatformID(ctx, artist.ID, "conn-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("expected empty string after delete, got %q", got)
	}
}

func TestDeletePlatformID_NotFound(t *testing.T) {
	t.Parallel()
	svc := setupPlatformIDTest(t)
	ctx := context.Background()

	err := svc.DeletePlatformID(ctx, "nonexistent", "conn-1")
	if err == nil {
		t.Fatal("expected error for nonexistent platform id")
	}
	if !errors.Is(err, ErrPlatformIDNotFound) {
		t.Errorf("expected ErrPlatformIDNotFound, got %v", err)
	}
}

func TestDeletePlatformIDsByArtist(t *testing.T) {
	t.Parallel()
	svc := setupPlatformIDTest(t)
	ctx := context.Background()
	artist := createTestArtist(t, svc, "Radiohead")

	svc.SetPlatformID(ctx, artist.ID, "conn-1", "emby-123")
	svc.SetPlatformID(ctx, artist.ID, "conn-2", "jf-456")

	err := svc.DeletePlatformIDsByArtist(ctx, artist.ID)
	if err != nil {
		t.Fatal(err)
	}

	ids, err := svc.GetPlatformIDs(ctx, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ids != nil {
		t.Errorf("expected nil after bulk delete, got %v", ids)
	}
}

func TestSetPlatformID_MultipleArtists(t *testing.T) {
	t.Parallel()
	svc := setupPlatformIDTest(t)
	ctx := context.Background()
	a1 := createTestArtist(t, svc, "Radiohead")
	a2 := createTestArtist(t, svc, "Bjork")

	svc.SetPlatformID(ctx, a1.ID, "conn-1", "emby-111")
	svc.SetPlatformID(ctx, a2.ID, "conn-1", "emby-222")

	got1, _ := svc.GetPlatformID(ctx, a1.ID, "conn-1")
	got2, _ := svc.GetPlatformID(ctx, a2.ID, "conn-1")
	if got1 != "emby-111" {
		t.Errorf("artist 1: got %q, want %q", got1, "emby-111")
	}
	if got2 != "emby-222" {
		t.Errorf("artist 2: got %q, want %q", got2, "emby-222")
	}
}

// setupPlatformPresenceTest creates a test service with emby, jellyfin, and lidarr connections.
// Returns the service and db for direct SQL access in tests.
func setupPlatformPresenceTest(t *testing.T) *Service {
	t.Helper()
	// newTestDB already enables foreign keys (see testmain_test.go).
	db := newTestDB(t)

	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
			VALUES ('conn-1', 'Test Emby', 'emby', 'http://emby:8096', 'enc-key', 1, 'ok', datetime('now'), datetime('now'))`,
		`INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
			VALUES ('conn-2', 'Test Jellyfin', 'jellyfin', 'http://jf:8096', 'enc-key', 1, 'ok', datetime('now'), datetime('now'))`,
		`INSERT INTO connections (id, name, type, url, encrypted_api_key, enabled, status, created_at, updated_at)
			VALUES ('conn-3', 'Test Lidarr', 'lidarr', 'http://lidarr:8686', 'enc-key', 1, 'ok', datetime('now'), datetime('now'))`,
		// presence is derived from artist_libraries, so the
		// fixture creates one library per connection. Tests using this
		// setup add membership rows pointing at these library IDs to
		// signal presence (in addition to the artist_platform_ids row,
		// which carries the actual platform identifier).
		`INSERT INTO libraries (id, name, path, type, source, connection_id, created_at, updated_at)
			VALUES ('lib-emby', 'lib-emby', '/music', 'regular', 'import', 'conn-1', datetime('now'), datetime('now'))`,
		`INSERT INTO libraries (id, name, path, type, source, connection_id, created_at, updated_at)
			VALUES ('lib-jelly', 'lib-jelly', '/music', 'regular', 'import', 'conn-2', datetime('now'), datetime('now'))`,
		`INSERT INTO libraries (id, name, path, type, source, connection_id, created_at, updated_at)
			VALUES ('lib-lidarr', 'lib-lidarr', '/music', 'regular', 'import', 'conn-3', datetime('now'), datetime('now'))`,
		// connection_id NULL marks a manual filesystem library; presence
		// derived from artist_libraries treats this as HasFilesystem.
		`INSERT INTO libraries (id, name, path, type, source, connection_id, created_at, updated_at)
			VALUES ('lib-fs', 'lib-fs', '/music', 'regular', 'manual', NULL, datetime('now'), datetime('now'))`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	return NewService(db)
}

// addMembership inserts an artist_libraries row for a presence test. Issue
// #1004: presence is now derived from these rows, not from the platform
// mapping, so tests asserting Has* must seed both.
func addMembership(t *testing.T, svc *Service, artistID, libraryID, source string) {
	t.Helper()
	if err := svc.AddLibraryMembership(context.Background(), artistID, libraryID, source); err != nil {
		t.Fatalf("AddLibraryMembership(%s, %s, %s): %v", artistID, libraryID, source, err)
	}
}

func TestGetPlatformPresenceForArtists(t *testing.T) {
	t.Parallel()
	svc := setupPlatformPresenceTest(t)
	ctx := context.Background()

	a1 := createTestArtist(t, svc, "Radiohead")
	a2 := createTestArtist(t, svc, "Bjork")
	a3 := createTestArtist(t, svc, "Portishead")
	a4 := createTestArtist(t, svc, "Aphex Twin")

	// a1: Emby + Jellyfin (presence requires both the
	// platform mapping AND the library membership; helper seeds both).
	if err := svc.SetPlatformID(ctx, a1.ID, "conn-1", "emby-111"); err != nil {
		t.Fatal(err)
	}
	addMembership(t, svc, a1.ID, "lib-emby", "emby")
	if err := svc.SetPlatformID(ctx, a1.ID, "conn-2", "jf-111"); err != nil {
		t.Fatal(err)
	}
	addMembership(t, svc, a1.ID, "lib-jelly", "jellyfin")

	// a2: Lidarr only
	if err := svc.SetPlatformID(ctx, a2.ID, "conn-3", "lidarr-222"); err != nil {
		t.Fatal(err)
	}
	addMembership(t, svc, a2.ID, "lib-lidarr", "lidarr")

	// a3: no platform IDs and no memberships (should be absent from result)

	// a4: filesystem-only (NULL connection_id library). No platform mapping;
	// presence comes purely from the membership row.
	addMembership(t, svc, a4.ID, "lib-fs", "filesystem")

	result, err := svc.GetPlatformPresenceForArtists(ctx, []string{a1.ID, a2.ID, a3.ID, a4.ID})
	if err != nil {
		t.Fatal(err)
	}

	// a1 should have Emby and Jellyfin (no filesystem membership).
	p1 := result[a1.ID]
	if !p1.HasEmby {
		t.Error("a1: expected HasEmby=true")
	}
	if !p1.HasJellyfin {
		t.Error("a1: expected HasJellyfin=true")
	}
	if p1.HasLidarr {
		t.Error("a1: expected HasLidarr=false")
	}
	if p1.HasFilesystem {
		t.Error("a1: expected HasFilesystem=false")
	}

	// a2 should have Lidarr only (no filesystem membership).
	p2 := result[a2.ID]
	if p2.HasEmby {
		t.Error("a2: expected HasEmby=false")
	}
	if p2.HasJellyfin {
		t.Error("a2: expected HasJellyfin=false")
	}
	if !p2.HasLidarr {
		t.Error("a2: expected HasLidarr=true")
	}
	if p2.HasFilesystem {
		t.Error("a2: expected HasFilesystem=false")
	}

	// a3 should not be in the map
	if _, ok := result[a3.ID]; ok {
		t.Error("a3: expected to be absent from result map")
	}

	// a4 has only a filesystem (NULL connection_id) membership; assert
	// HasFilesystem=true and every platform flag false.
	p4 := result[a4.ID]
	if !p4.HasFilesystem {
		t.Error("a4: expected HasFilesystem=true")
	}
	if p4.HasEmby || p4.HasJellyfin || p4.HasLidarr {
		t.Errorf("a4: expected platform flags all false, got emby=%v jellyfin=%v lidarr=%v",
			p4.HasEmby, p4.HasJellyfin, p4.HasLidarr)
	}
}

func TestGetPlatformPresenceForArtists_Nil(t *testing.T) {
	t.Parallel()
	svc := setupPlatformPresenceTest(t)
	ctx := context.Background()

	result, err := svc.GetPlatformPresenceForArtists(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Errorf("expected nil for nil input, got %v", result)
	}
}

func TestGetPlatformPresenceForArtists_EmptySlice(t *testing.T) {
	t.Parallel()
	svc := setupPlatformPresenceTest(t)
	ctx := context.Background()

	result, err := svc.GetPlatformPresenceForArtists(ctx, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Errorf("expected nil for empty slice input, got %v", result)
	}
}

func TestGetPlatformPresenceForArtists_AllPlatforms(t *testing.T) {
	t.Parallel()
	svc := setupPlatformPresenceTest(t)
	ctx := context.Background()

	a := createTestArtist(t, svc, "Radiohead")

	// Artist with all three platform types. presence flows
	// from artist_libraries memberships, not the platform mappings; seed
	// both to assert presence.
	svc.SetPlatformID(ctx, a.ID, "conn-1", "emby-111")
	addMembership(t, svc, a.ID, "lib-emby", "emby")
	svc.SetPlatformID(ctx, a.ID, "conn-2", "jf-111")
	addMembership(t, svc, a.ID, "lib-jelly", "jellyfin")
	svc.SetPlatformID(ctx, a.ID, "conn-3", "lidarr-111")
	addMembership(t, svc, a.ID, "lib-lidarr", "lidarr")

	result, err := svc.GetPlatformPresenceForArtists(ctx, []string{a.ID})
	if err != nil {
		t.Fatal(err)
	}

	p := result[a.ID]
	if !p.HasEmby {
		t.Error("expected HasEmby=true")
	}
	if !p.HasJellyfin {
		t.Error("expected HasJellyfin=true")
	}
	if !p.HasLidarr {
		t.Error("expected HasLidarr=true")
	}
}

// Note: the previous TestGetPlatformPresenceForArtists_LegacyLibraryIDFallback
// and *_MembershipAndLegacyDeDuplicate tests covered the hybrid OR-fallback
// for the legacy artists.library_id column. Migration 004 dropped the column
// and the corresponding UNION branch in GetPresenceForArtists, so the
// fallback shape is no longer reachable. Membership-only coverage remains in
// TestGetPlatformPresenceForArtists* above.
