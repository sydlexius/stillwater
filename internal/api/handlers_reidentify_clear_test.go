package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// reidentifyProviderRow reads a single artist_provider_ids row directly from the
// database so assertions target actual persisted state, not a counter or a
// handler return value.
func reidentifyProviderRow(t *testing.T, db *sql.DB, artistID, prov string) (exists bool, providerID string) {
	t.Helper()
	var pid string
	err := db.QueryRowContext(context.Background(),
		`SELECT provider_id FROM artist_provider_ids WHERE artist_id = ? AND provider = ?`,
		artistID, prov).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ""
	}
	if err != nil {
		t.Fatalf("querying %s row for artist %s: %v", prov, artistID, err)
	}
	return true, pid
}

// TestHandleReidentify_ClearIDsRemovesModeledPreservesOrphan exercises the REAL
// handleReidentify clear_ids=true path end to end. That handler blanks the seven
// struct-modeled provider fields and relies on Update removing their rows. The
// scoped-delete fix (#2725) must keep that destructive behavior for modeled
// providers while no longer collaterally destroying orphan-provider fetched_at
// rows (allmusic, fanarttv, ...) that share the same artist.
func TestHandleReidentify_ClearIDsRemovesModeledPreservesOrphan(t *testing.T) {
	db := newTestDB(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := artist.NewService(db)
	r := &Router{
		logger:        logger,
		artistService: svc,
		db:            db,
	}
	ctx := context.Background()

	// Seed an artist carrying a modeled provider ID (discogs) plus an orphan
	// fetched_at row (allmusic).
	a := &artist.Artist{
		Name:      "Reidentify Target",
		SortName:  "Reidentify Target",
		Type:      "group",
		Path:      "/music/reidentify-target",
		DiscogsID: "99",
	}
	fetched := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	a.DiscogsIDFetchedAt = &fetched
	a.MusicBrainzID = "mbid-seed"
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.UpdateProviderFetchedAt(ctx, a.ID, string(provider.NameAllMusic)); err != nil {
		t.Fatalf("stamp allmusic orphan: %v", err)
	}

	// PRECONDITION (real, non-vacuous): both the modeled discogs row and the
	// orphan allmusic row exist before the clear.
	if exists, pid := reidentifyProviderRow(t, db, a.ID, string(provider.NameDiscogs)); !exists || pid != "99" {
		t.Fatalf("precondition: discogs row exists=%v provider_id=%q, want exists=true provider_id=%q", exists, pid, "99")
	}
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameMusicBrainz)); !exists {
		t.Fatalf("precondition: musicbrainz row missing before clear")
	}
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
		t.Fatalf("precondition: allmusic orphan row missing before clear")
	}

	// Drive the REAL handler with clear_ids=true. Non-HTMX request -> JSON path
	// (no templ rendering needed).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/reidentify",
		strings.NewReader("clear_ids=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	rec := httptest.NewRecorder()

	r.handleReidentify(rec, req)

	if rec.Code != 200 {
		t.Fatalf("handleReidentify status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Modeled rows must be REMOVED by the clear.
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameDiscogs)); exists {
		t.Errorf("discogs row still present after clear_ids; re-identify clear regressed")
	}
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameMusicBrainz)); exists {
		t.Errorf("musicbrainz row still present after clear_ids; re-identify clear regressed")
	}
	// The orphan row must SURVIVE the clear.
	if exists, _ := reidentifyProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
		t.Errorf("allmusic orphan row destroyed by re-identify clear; scoped delete boundary is wrong (#2725)")
	}
}
