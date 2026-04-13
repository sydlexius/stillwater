package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
)

// writeArtistNFO writes an artist.nfo file under a temp dir and returns the dir.
func writeArtistNFO(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing artist.nfo: %v", err)
	}
	return dir
}

const discographyTestNFO = `<?xml version="1.0" encoding="UTF-8"?>
<artist>
  <name>Nirvana</name>
  <type>group</type>
  <album>
    <title>Bleach</title>
    <year>1989</year>
  </album>
  <album>
    <title>Nevermind</title>
    <year>1991</year>
    <musicbrainzreleasegroupid>1b022e01-4da6-387b-8658-8678046e4cef</musicbrainzreleasegroupid>
  </album>
</artist>
`

func TestHandleArtistDiscographyTab_HappyPath(t *testing.T) {
	r, artistSvc := testRouter(t)

	dir := writeArtistNFO(t, discographyTestNFO)

	a := &artist.Artist{Name: "Nirvana", Path: dir, NFOExists: true}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/artists/"+a.ID+"/discography/tab", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Bleach") {
		t.Errorf("body missing Bleach:\n%s", body)
	}
	if !strings.Contains(body, "Nevermind") {
		t.Errorf("body missing Nevermind:\n%s", body)
	}
	if !strings.Contains(body, "1991") {
		t.Errorf("body missing year 1991:\n%s", body)
	}
	if !strings.Contains(body, "1b022e01-4da6-387b-8658-8678046e4cef") {
		t.Errorf("body missing mbid link:\n%s", body)
	}
}

func TestHandleArtistDiscographyTab_NotFound(t *testing.T) {
	r, _ := testRouter(t)

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/artists/nonexistent-id/discography/tab", nil)
	req.SetPathValue("id", "nonexistent-id")
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var payload map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding JSON: %v (body=%q)", err, w.Body.String())
	}
	if payload["error"] != "artist not found" {
		t.Errorf("payload = %+v, want message=\"artist not found\"", payload)
	}
}

func TestHandleArtistDiscographyTab_InternalError(t *testing.T) {
	// Force a repository error by closing the underlying DB before dispatch.
	// GetByID will then return a non-NotFound error, exercising the 500 path.
	r, artistSvc := testRouter(t)

	a := &artist.Artist{Name: "Closed", Path: t.TempDir(), NFOExists: false}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Close the DB to trigger a query error on the next read.
	if err := r.db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/artists/"+a.ID+"/discography/tab", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%q)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var payload map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding JSON: %v (body=%q)", err, w.Body.String())
	}
	if payload["error"] != "internal error" {
		t.Errorf("payload = %+v, want message=\"internal error\"", payload)
	}
}

func TestHandleArtistDiscographyTab_MissingID(t *testing.T) {
	r, _ := testRouter(t)

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/artists//discography/tab", nil)
	// No SetPathValue: RequirePathParam should 400.
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleArtistDiscographyTab_NFOAbsent(t *testing.T) {
	// Artist exists but NFOExists is false -- handler should render empty state.
	r, artistSvc := testRouter(t)

	a := &artist.Artist{Name: "No NFO", Path: t.TempDir(), NFOExists: false}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/artists/"+a.ID+"/discography/tab", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// Empty-state marker from the i18n bundle.
	if !strings.Contains(body, "No discography") {
		t.Errorf("body missing empty-state text:\n%s", body)
	}
}

func TestHandleArtistDiscographyTab_NFOMalformed(t *testing.T) {
	// Malformed NFO: parseNFOFile returns nil; handler should still 200 with
	// empty state AND emit a structured warn log so operators can diagnose.
	r, artistSvc := testRouter(t)

	// Swap in a slog handler we can inspect, at Warn level.
	var logBuf bytes.Buffer
	r.logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	dir := writeArtistNFO(t, "this is not xml <<<")

	a := &artist.Artist{Name: "Malformed", Path: dir, NFOExists: true}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/artists/"+a.ID+"/discography/tab", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No discography") {
		t.Errorf("expected empty-state for malformed NFO:\n%s", w.Body.String())
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "failed to parse artist.nfo") {
		t.Errorf("expected warn log for malformed NFO, got:\n%s", logged)
	}
	if !strings.Contains(logged, "artist_id="+a.ID) {
		t.Errorf("expected artist_id in warn log, got:\n%s", logged)
	}
}

func TestHandleArtistDiscographyTab_NFOWithoutAlbums(t *testing.T) {
	// Valid NFO but no <album> entries -- empty state.
	r, artistSvc := testRouter(t)

	dir := writeArtistNFO(t, `<?xml version="1.0"?><artist><name>Solo</name></artist>`)

	a := &artist.Artist{Name: "Solo", Path: dir, NFOExists: true}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet,
		"/artists/"+a.ID+"/discography/tab", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No discography") {
		t.Errorf("expected empty-state when NFO has no albums:\n%s", w.Body.String())
	}
}

func TestDiscographyFromNFO_Nil(t *testing.T) {
	if got := discographyFromNFO(nil); got != nil {
		t.Errorf("discographyFromNFO(nil) = %v, want nil", got)
	}
}

func TestDiscographyFromNFO_Empty(t *testing.T) {
	if got := discographyFromNFO(&nfo.ArtistNFO{}); got != nil {
		t.Errorf("discographyFromNFO(empty) = %v, want nil", got)
	}
}

func TestDiscographyFromNFO_MapsFields(t *testing.T) {
	in := &nfo.ArtistNFO{
		Albums: []nfo.DiscographyAlbum{
			{Title: "A", Year: "2001"},
			{Title: "B", Year: "2002", MusicBrainzReleaseGroupID: "mbid-1"},
			{Title: "C"}, // missing year and mbid
		},
	}
	got := discographyFromNFO(in)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0] != (artist.DiscographyAlbum{Title: "A", Year: "2001"}) {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1] != (artist.DiscographyAlbum{Title: "B", Year: "2002", MusicBrainzReleaseGroupID: "mbid-1"}) {
		t.Errorf("got[1] = %+v", got[1])
	}
	if got[2] != (artist.DiscographyAlbum{Title: "C"}) {
		t.Errorf("got[2] = %+v", got[2])
	}
}
