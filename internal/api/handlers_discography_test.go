package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	if got := discographyFromNFO(nil); got != nil {
		t.Errorf("discographyFromNFO(nil) = %v, want nil", got)
	}
}

func TestDiscographyFromNFO_Empty(t *testing.T) {
	t.Parallel()
	if got := discographyFromNFO(&nfo.ArtistNFO{}); got != nil {
		t.Errorf("discographyFromNFO(empty) = %v, want nil", got)
	}
}

func TestDiscographyFromNFO_MapsFields(t *testing.T) {
	t.Parallel()
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

// --- handleFetchDiscography tests ---

// discographyFetchRouter builds a Router with a stub MB provider that returns
// the provided release groups. The registry is set on the router so
// resolveMBAdapter can find it.
func discographyFetchRouter(
	t *testing.T,
	rgFn func(ctx context.Context, mbid string) ([]provider.ReleaseGroupInfo, error),
) (*Router, *artist.Service) {
	t.Helper()
	r, artistSvc := testRouter(t)

	stub := &identifyStubProvider{
		name:             provider.NameMusicBrainz,
		getReleaseGrpsFn: rgFn,
	}
	reg := provider.NewRegistry()
	reg.Register(stub)
	r.providerRegistry = reg
	return r, artistSvc
}

func TestHandleFetchDiscography_NoMBID(t *testing.T) {
	t.Parallel()
	r, artistSvc := discographyFetchRouter(t, nil)

	dir := t.TempDir()
	a := &artist.Artist{Name: "No MBID", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var payload map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if !strings.Contains(payload["error"], "MusicBrainz") {
		t.Errorf("expected MBID-related error, got: %q", payload["error"])
	}
}

func TestHandleFetchDiscography_NoPath(t *testing.T) {
	t.Parallel()
	r, artistSvc := discographyFetchRouter(t, nil)

	a := &artist.Artist{Name: "No Path", MusicBrainzID: "some-mbid"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleFetchDiscography_NotFound(t *testing.T) {
	t.Parallel()
	r, _ := discographyFetchRouter(t, nil)

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/nonexistent/discography/fetch", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleFetchDiscography_MBError(t *testing.T) {
	t.Parallel()
	r, artistSvc := discographyFetchRouter(t, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return nil, fmt.Errorf("MB unavailable")
	})

	dir := t.TempDir()
	a := &artist.Artist{Name: "MB Error Artist", Path: dir, MusicBrainzID: "mbid-x"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

func TestHandleFetchDiscography_EmptyNFO(t *testing.T) {
	t.Parallel()
	groups := []provider.ReleaseGroupInfo{
		{ID: "mbid-1", Title: "Bleach", PrimaryType: "Album", FirstReleaseDate: "1989"},
		{ID: "mbid-2", Title: "Nevermind", PrimaryType: "Album", FirstReleaseDate: "1991-09-24"},
		{ID: "mbid-3", Title: "Heart-Shaped Box Single", PrimaryType: "Single", FirstReleaseDate: "1993"},
	}
	r, artistSvc := discographyFetchRouter(t, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return groups, nil
	})

	dir := t.TempDir()
	a := &artist.Artist{Name: "Nirvana", Path: dir, MusicBrainzID: "5b11f4ce-a62d-471e-81fc-a69a8278c7da"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var result DiscographyFetchResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	// Default filter is Album,EP; Single should be skipped.
	if result.Added != 2 {
		t.Errorf("Added = %d, want 2", result.Added)
	}
	if result.Total != 3 {
		t.Errorf("Total = %d, want 3", result.Total)
	}
	// Verify NFO was written with the two album entries.
	nfoPath := filepath.Join(dir, "artist.nfo")
	parsed, err := nfo.Parse(mustOpen(t, nfoPath))
	if err != nil {
		t.Fatalf("parsing written NFO: %v", err)
	}
	if len(parsed.Albums) != 2 {
		t.Errorf("NFO album count = %d, want 2", len(parsed.Albums))
	}
}

func TestHandleFetchDiscography_PartialNFO_UserEntriesPreserved(t *testing.T) {
	t.Parallel()
	groups := []provider.ReleaseGroupInfo{
		{ID: "mbid-1", Title: "Bleach", PrimaryType: "Album", FirstReleaseDate: "1989"},
	}
	r, artistSvc := discographyFetchRouter(t, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return groups, nil
	})

	dir := t.TempDir()
	// Write an existing NFO with a user-added album (no MBID) and an MBID-tagged one.
	existingNFO := `<?xml version="1.0"?>
<artist>
  <name>Nirvana</name>
  <album>
    <title>User Added Album</title>
    <year>1985</year>
  </album>
  <album>
    <title>Bleach (my edit)</title>
    <year>1989</year>
    <musicbrainzreleasegroupid>mbid-1</musicbrainzreleasegroupid>
  </album>
</artist>`
	if err := os.WriteFile(filepath.Join(dir, "artist.nfo"), []byte(existingNFO), 0o600); err != nil {
		t.Fatalf("writing existing NFO: %v", err)
	}

	a := &artist.Artist{Name: "Nirvana", Path: dir, MusicBrainzID: "5b11f4ce-a62d-471e-81fc-a69a8278c7da"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var result DiscographyFetchResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	// Bleach is kept (MBID match), nothing new added.
	if result.Added != 0 {
		t.Errorf("Added = %d, want 0 (Bleach already present by MBID)", result.Added)
	}
	if result.Kept != 1 {
		t.Errorf("Kept = %d, want 1", result.Kept)
	}
	// Existing user album must still be present.
	nfoPath := filepath.Join(dir, "artist.nfo")
	// File should remain unchanged on disk (no write when Added==0).
	// User album must still be there.
	parsed, err := nfo.Parse(mustOpen(t, nfoPath))
	if err != nil {
		t.Fatalf("parsing NFO: %v", err)
	}
	var hasUserAlbum bool
	for _, alb := range parsed.Albums {
		if alb.Title == "User Added Album" {
			hasUserAlbum = true
		}
	}
	if !hasUserAlbum {
		t.Errorf("user-added album was removed from NFO; albums: %+v", parsed.Albums)
	}
	// User's custom title for Bleach must be preserved.
	for _, alb := range parsed.Albums {
		if alb.MusicBrainzReleaseGroupID == "mbid-1" && alb.Title != "Bleach (my edit)" {
			t.Errorf("user edit overwritten; got title %q", alb.Title)
		}
	}
}

func TestHandleFetchDiscography_NoProviderRegistry(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	// No registry set; resolveMBAdapter returns nil.
	r.providerRegistry = nil

	dir := t.TempDir()
	a := &artist.Artist{Name: "No Registry Artist", Path: dir, MusicBrainzID: "mbid-reg"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestHandleFetchDiscography_IncludeFilter(t *testing.T) {
	t.Parallel()
	groups := []provider.ReleaseGroupInfo{
		{ID: "mbid-1", Title: "Album One", PrimaryType: "Album", FirstReleaseDate: "2001"},
		{ID: "mbid-2", Title: "Single One", PrimaryType: "Single", FirstReleaseDate: "2001"},
	}
	r, artistSvc := discographyFetchRouter(t, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return groups, nil
	})

	dir := t.TempDir()
	a := &artist.Artist{Name: "Filter Test", Path: dir, MusicBrainzID: "mbid-filter"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	// Request with include=Album,Single
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch?include=Album,Single", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var result DiscographyFetchResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if result.Added != 2 {
		t.Errorf("Added = %d, want 2 (Album+Single both included)", result.Added)
	}
}

// TestHandleFetchDiscography_IncludeFromJSONBody verifies the JSON-body
// fallback: when no include query parameter is present, the handler reads
// the release-type filter from a JSON request body.
func TestHandleFetchDiscography_IncludeFromJSONBody(t *testing.T) {
	t.Parallel()
	groups := []provider.ReleaseGroupInfo{
		{ID: "mbid-1", Title: "Album One", PrimaryType: "Album", FirstReleaseDate: "2001"},
		{ID: "mbid-2", Title: "Single One", PrimaryType: "Single", FirstReleaseDate: "2001"},
	}
	r, artistSvc := discographyFetchRouter(t, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return groups, nil
	})

	dir := t.TempDir()
	a := &artist.Artist{Name: "Body Filter", Path: dir, MusicBrainzID: "mbid-body"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch",
		strings.NewReader(`{"include":"Album,Single"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var result DiscographyFetchResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if result.Added != 2 {
		t.Errorf("Added = %d, want 2 (include from JSON body should admit Album+Single)", result.Added)
	}
}

// TestHandleFetchDiscography_IncludeQueryBeatsBody verifies the documented
// precedence: when both a query parameter and a JSON body supply include,
// the query parameter wins.
func TestHandleFetchDiscography_IncludeQueryBeatsBody(t *testing.T) {
	t.Parallel()
	groups := []provider.ReleaseGroupInfo{
		{ID: "mbid-1", Title: "Album One", PrimaryType: "Album", FirstReleaseDate: "2001"},
		{ID: "mbid-2", Title: "Single One", PrimaryType: "Single", FirstReleaseDate: "2001"},
	}
	r, artistSvc := discographyFetchRouter(t, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return groups, nil
	})

	dir := t.TempDir()
	a := &artist.Artist{Name: "Precedence", Path: dir, MusicBrainzID: "mbid-prec"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	// Query says Album only; body says Album,Single. The query must win.
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch?include=Album",
		strings.NewReader(`{"include":"Album,Single"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var result DiscographyFetchResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if result.Added != 1 {
		t.Errorf("Added = %d, want 1 (query include=Album must override body include=Album,Single)", result.Added)
	}
}

// TestHandleFetchDiscography_HXRequest_ReturnsHTML verifies that a POST with
// the HX-Request header returns an HTML partial (the re-rendered tab), not
// JSON. The HTMX button uses hx-swap="outerHTML" on #discography-tab-content
// so the tab refreshes in place without showing raw JSON to the user.
func TestHandleFetchDiscography_HXRequest_ReturnsHTML(t *testing.T) {
	t.Parallel()
	groups := []provider.ReleaseGroupInfo{
		{ID: "mbid-a", Title: "Bleach", PrimaryType: "Album", FirstReleaseDate: "1989"},
	}
	r, artistSvc := discographyFetchRouter(t, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return groups, nil
	})

	dir := t.TempDir()
	a := &artist.Artist{Name: "Nirvana", Path: dir, MusicBrainzID: "5b11f4ce-a62d-471e-81fc-a69a8278c7da"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch", nil)
	req.SetPathValue("id", a.ID)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html; got non-HTML response for HX-Request", ct)
	}
	body := w.Body.String()
	// Must contain the tab container id so outerHTML swap works.
	if !strings.Contains(body, "discography-tab-content") {
		t.Errorf("body missing discography-tab-content id:\n%s", body)
	}
	// Must NOT look like a JSON object response.
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Errorf("body looks like JSON; expected HTML partial:\n%s", body)
	}
	// Album entry written by the fetch must appear in the refreshed partial.
	if !strings.Contains(body, "Bleach") {
		t.Errorf("body missing album title Bleach:\n%s", body)
	}
	// Fetch summary should be rendered into #discography-fetch-msg.
	if !strings.Contains(body, "Fetched") {
		t.Errorf("body missing fetch summary message:\n%s", body)
	}
}

// TestHandleFetchDiscography_CorruptNFO_Returns422 verifies that when the
// existing artist.nfo is malformed XML, the handler returns 422 and does NOT
// overwrite the file, preserving any recoverable content.
func TestHandleFetchDiscography_CorruptNFO_Returns422(t *testing.T) {
	t.Parallel()
	groups := []provider.ReleaseGroupInfo{
		{ID: "mbid-x", Title: "Some Album", PrimaryType: "Album", FirstReleaseDate: "2001"},
	}
	r, artistSvc := discographyFetchRouter(t, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return groups, nil
	})

	// Write a corrupt (non-XML) artist.nfo.
	dir := t.TempDir()
	nfoPath := filepath.Join(dir, "artist.nfo")
	corruptContent := []byte("<<<this is not xml at all>>>")
	if err := os.WriteFile(nfoPath, corruptContent, 0o600); err != nil {
		t.Fatalf("writing corrupt nfo: %v", err)
	}

	a := &artist.Artist{Name: "Corrupt Artist", Path: dir, MusicBrainzID: "mbid-corrupt"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}
	if !strings.Contains(payload["error"], "could not be parsed") {
		t.Errorf("expected parse-error message, got: %q", payload["error"])
	}
	// The file must be unchanged -- corrupt content was NOT overwritten.
	onDisk, err := os.ReadFile(nfoPath)
	if err != nil {
		t.Fatalf("reading nfo after handler: %v", err)
	}
	if string(onDisk) != string(corruptContent) {
		t.Errorf("nfo file was overwritten despite parse error; want original corrupt content, got %q", string(onDisk))
	}
}

// mustOpen opens a file and registers it for cleanup. Fails the test if the file
// cannot be opened.
func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestHandleFetchDiscography_MissingID exercises the RequirePathParam 400 path.
func TestHandleFetchDiscography_MissingID(t *testing.T) {
	t.Parallel()
	r, _ := discographyFetchRouter(t, nil)

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists//discography/fetch", nil)
	// No SetPathValue: RequirePathParam should return 400.
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleFetchDiscography_ConcurrentFetchConflict verifies that a fetch is
// rejected with 409 when another fetch for the same artist is already in
// flight, so two read-modify-write cycles cannot race the NFO write.
func TestHandleFetchDiscography_ConcurrentFetchConflict(t *testing.T) {
	t.Parallel()
	r, artistSvc := discographyFetchRouter(t, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return nil, nil
	})

	dir := t.TempDir()
	a := &artist.Artist{Name: "Nirvana", Path: dir, MusicBrainzID: "5b11f4ce-a62d-471e-81fc-a69a8278c7da"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Simulate an in-flight fetch by claiming the slot directly.
	r.discographyFetchMu.Lock()
	r.discographyFetchInFlight[a.ID] = true
	r.discographyFetchMu.Unlock()

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
}

// TestHandleFetchDiscography_SlotReleasedAfterFetch verifies the in-flight slot
// is released once a fetch completes, so a subsequent fetch for the same artist
// is not spuriously rejected with 409.
func TestHandleFetchDiscography_SlotReleasedAfterFetch(t *testing.T) {
	t.Parallel()
	r, artistSvc := discographyFetchRouter(t, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		return []provider.ReleaseGroupInfo{
			{ID: "mbid-1", Title: "Bleach", PrimaryType: "Album", FirstReleaseDate: "1989"},
		}, nil
	})

	dir := t.TempDir()
	a := &artist.Artist{Name: "Nirvana", Path: dir, MusicBrainzID: "5b11f4ce-a62d-471e-81fc-a69a8278c7da"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	ctx := testI18nCtx(t, context.Background())
	for i := range 2 {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost,
			"/api/v1/artists/"+a.ID+"/discography/fetch", nil)
		req.SetPathValue("id", a.ID)
		w := httptest.NewRecorder()
		r.handleFetchDiscography(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("fetch %d: status = %d, want 200; body = %s", i+1, w.Code, w.Body.String())
		}
	}

	// The slot must be empty after both fetches return.
	r.discographyFetchMu.Lock()
	inFlight := r.discographyFetchInFlight[a.ID]
	r.discographyFetchMu.Unlock()
	if inFlight {
		t.Errorf("in-flight slot still claimed after fetch completed")
	}
}

// Silence unused import for errors package (used in tests that may be added).
var _ = errors.New
