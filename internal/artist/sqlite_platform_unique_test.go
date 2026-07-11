package artist

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestIsUniqueConstraintErr covers the sentinel-mapping helper added for the
// SetStable TOCTOU fix (#2354): a nil error, a genuine SQLite UNIQUE-constraint
// message, and an unrelated error must each be classified correctly.
func TestIsUniqueConstraintErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{
			"sqlite unique constraint message",
			errors.New("UNIQUE constraint failed: artist_platform_ids.connection_id, artist_platform_ids.platform_artist_id"),
			true,
		},
		{
			"lowercase variant",
			errors.New("unique constraint failed: some_table.some_col"),
			true,
		},
		{"unrelated error", fmt.Errorf("some other database error"), false},
		{"mentions unique but not the constraint phrase", errors.New("column name must be unique"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isUniqueConstraintErr(tt.err); got != tt.want {
				t.Errorf("isUniqueConstraintErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestSetPlatformIDStable_ConcurrentCrossArtistClaim is the race-window
// regression for #2354: SetStable's cross-artist collision check is a SELECT
// followed by a separate INSERT, so a concurrent writer can claim the same
// (connection_id, platform_artist_id) pair in the gap between them. The
// INSERT then fails the UNIQUE INDEX on (connection_id, platform_artist_id)
// (distinct from the ON CONFLICT target of (artist_id, connection_id)), and
// that failure must be mapped to the typed sentinel
// ErrPlatformIDClaimedByAnotherArtist rather than surfacing as a generic
// wrapped "setting platform id (stable)" error, so best-effort callers (e.g.
// the Lidarr merge/rename self-heal) can treat it as skip-and-continue.
//
// Run under `go test -race` to also confirm no data race in the repository
// path itself.
func TestSetPlatformIDStable_ConcurrentCrossArtistClaim(t *testing.T) {
	t.Parallel()
	svc := setupPlatformIDTest(t)
	ctx := context.Background()

	const numArtists = 16
	artists := make([]*Artist, numArtists)
	for i := range artists {
		artists[i] = createTestArtist(t, svc, fmt.Sprintf("Contender %d", i))
	}

	const platformArtistID = "emby-contended-item"

	var wg sync.WaitGroup
	errs := make([]error, numArtists)
	for i, a := range artists {
		wg.Add(1)
		go func(i int, artistID string) {
			defer wg.Done()
			_, err := svc.SetPlatformIDStable(ctx, artistID, "conn-1", platformArtistID)
			errs[i] = err
		}(i, a.ID)
	}
	wg.Wait()

	var winners, sentinels, unexpected int
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrPlatformIDClaimedByAnotherArtist):
			sentinels++
		default:
			unexpected++
			t.Errorf("artist %d: got unmapped error %v, want nil or ErrPlatformIDClaimedByAnotherArtist", i, err)
		}
	}

	if winners != 1 {
		t.Errorf("got %d winners for the contended platform id, want exactly 1 (winners=%d sentinels=%d unexpected=%d)",
			winners, winners, sentinels, unexpected)
	}
	if winners+sentinels != numArtists {
		t.Errorf("winners(%d)+sentinels(%d) != numArtists(%d); unexpected=%d", winners, sentinels, numArtists, unexpected)
	}

	// Exactly one artist should hold the row afterward.
	rows, err := svc.platformIDs.(*sqlitePlatformIDRepo).db.QueryContext(ctx, `
		SELECT COUNT(*) FROM artist_platform_ids WHERE connection_id = ? AND platform_artist_id = ?
	`, "conn-1", platformArtistID)
	if err != nil {
		t.Fatalf("querying stored rows: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected one row from COUNT(*) query")
	}
	var count int
	if err := rows.Scan(&count); err != nil {
		t.Fatalf("scanning count: %v", err)
	}
	if count != 1 {
		t.Errorf("stored row count = %d, want 1", count)
	}
}
