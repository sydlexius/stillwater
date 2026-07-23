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
	"sync/atomic"
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

// --- handleArtistDiscographyTab search/sort/order tests ---

// multiAlbumNFO has three albums in NFO order: Nevermind (1991), In Utero (1993), Bleach (1989).
// Chosen so title order (Bleach < In Utero < Nevermind) and year order (Bleach < Nevermind < In Utero)
// both differ from NFO order, making sort-direction assertions unambiguous.
const multiAlbumNFO = `<?xml version="1.0" encoding="UTF-8"?>
<artist>
  <name>Nirvana</name>
  <album>
    <title>Nevermind</title>
    <year>1991</year>
  </album>
  <album>
    <title>In Utero</title>
    <year>1993</year>
  </album>
  <album>
    <title>Bleach</title>
    <year>1989</year>
  </album>
</artist>
`

// discographyTabReq builds a GET request for the discography tab with optional query params.
func discographyTabReq(t *testing.T, artistID, query string) *http.Request {
	t.Helper()
	url := "/artists/" + artistID + "/discography/tab"
	if query != "" {
		url += "?" + query
	}
	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.SetPathValue("id", artistID)
	return req
}

// setupMultiAlbumArtist creates an artist with multiAlbumNFO and returns it.
func setupMultiAlbumArtist(t *testing.T, artistSvc *artist.Service) *artist.Artist {
	t.Helper()
	dir := writeArtistNFO(t, multiAlbumNFO)
	a := &artist.Artist{Name: "Nirvana", Path: dir, NFOExists: true}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	return a
}

func TestHandleArtistDiscographyTab_Search_CaseInsensitiveMatch(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := setupMultiAlbumArtist(t, artistSvc)

	// Lowercase search should match the mixed-case title "Nevermind".
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, discographyTabReq(t, a.ID, "search=neverm"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Nevermind") {
		t.Errorf("body missing matched album Nevermind:\n%s", body)
	}
	// Non-matching albums must be absent.
	if strings.Contains(body, "Bleach") {
		t.Errorf("body unexpectedly contains Bleach (should be filtered out):\n%s", body)
	}
	if strings.Contains(body, "In Utero") {
		t.Errorf("body unexpectedly contains In Utero (should be filtered out):\n%s", body)
	}
}

func TestHandleArtistDiscographyTab_Search_UppercaseMatch(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := setupMultiAlbumArtist(t, artistSvc)

	// Uppercase search should also match (case-insensitive).
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, discographyTabReq(t, a.ID, "search=BLEACH"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Bleach") {
		t.Errorf("body missing matched album Bleach:\n%s", body)
	}
	if strings.Contains(body, "Nevermind") {
		t.Errorf("body unexpectedly contains Nevermind:\n%s", body)
	}
}

func TestHandleArtistDiscographyTab_Search_NoMatch(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := setupMultiAlbumArtist(t, artistSvc)

	// A search term that matches nothing should yield the empty state.
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, discographyTabReq(t, a.ID, "search=xyzzy"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "Bleach") || strings.Contains(body, "Nevermind") || strings.Contains(body, "In Utero") {
		t.Errorf("body should have no album results for non-matching search:\n%s", body)
	}
	if !strings.Contains(body, "No discography") {
		t.Errorf("body missing empty-state text for no-match search:\n%s", body)
	}
}

func TestHandleArtistDiscographyTab_SortTitle_Asc(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := setupMultiAlbumArtist(t, artistSvc)

	// sort=title&order=asc -> Bleach, In Utero, Nevermind
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, discographyTabReq(t, a.ID, "sort=title&order=asc"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	idxBleach := strings.Index(body, "Bleach")
	idxInUtero := strings.Index(body, "In Utero")
	idxNevermind := strings.Index(body, "Nevermind")
	if idxBleach < 0 || idxInUtero < 0 || idxNevermind < 0 {
		t.Fatalf("one or more album titles missing from body:\n%s", body)
	}
	if idxBleach >= idxInUtero || idxInUtero >= idxNevermind {
		t.Errorf("title asc order wrong: Bleach@%d InUtero@%d Nevermind@%d", idxBleach, idxInUtero, idxNevermind)
	}
}

func TestHandleArtistDiscographyTab_SortTitle_Desc(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := setupMultiAlbumArtist(t, artistSvc)

	// sort=title&order=desc -> Nevermind, In Utero, Bleach
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, discographyTabReq(t, a.ID, "sort=title&order=desc"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	idxBleach := strings.Index(body, "Bleach")
	idxInUtero := strings.Index(body, "In Utero")
	idxNevermind := strings.Index(body, "Nevermind")
	if idxBleach < 0 || idxInUtero < 0 || idxNevermind < 0 {
		t.Fatalf("one or more album titles missing from body:\n%s", body)
	}
	if idxNevermind >= idxInUtero || idxInUtero >= idxBleach {
		t.Errorf("title desc order wrong: Nevermind@%d InUtero@%d Bleach@%d", idxNevermind, idxInUtero, idxBleach)
	}
}

func TestHandleArtistDiscographyTab_SortYear_Asc(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := setupMultiAlbumArtist(t, artistSvc)

	// sort=year&order=asc -> Bleach(1989), Nevermind(1991), In Utero(1993)
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, discographyTabReq(t, a.ID, "sort=year&order=asc"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	idxBleach := strings.Index(body, "Bleach")
	idxNevermind := strings.Index(body, "Nevermind")
	idxInUtero := strings.Index(body, "In Utero")
	if idxBleach < 0 || idxNevermind < 0 || idxInUtero < 0 {
		t.Fatalf("one or more album titles missing from body:\n%s", body)
	}
	if idxBleach >= idxNevermind || idxNevermind >= idxInUtero {
		t.Errorf("year asc order wrong: Bleach@%d Nevermind@%d InUtero@%d", idxBleach, idxNevermind, idxInUtero)
	}
}

func TestHandleArtistDiscographyTab_SortYear_Desc(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := setupMultiAlbumArtist(t, artistSvc)

	// sort=year&order=desc -> In Utero(1993), Nevermind(1991), Bleach(1989)
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, discographyTabReq(t, a.ID, "sort=year&order=desc"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	idxBleach := strings.Index(body, "Bleach")
	idxNevermind := strings.Index(body, "Nevermind")
	idxInUtero := strings.Index(body, "In Utero")
	if idxBleach < 0 || idxNevermind < 0 || idxInUtero < 0 {
		t.Fatalf("one or more album titles missing from body:\n%s", body)
	}
	if idxInUtero >= idxNevermind || idxNevermind >= idxBleach {
		t.Errorf("year desc order wrong: InUtero@%d Nevermind@%d Bleach@%d", idxInUtero, idxNevermind, idxBleach)
	}
}

func TestHandleArtistDiscographyTab_DefaultParams(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := setupMultiAlbumArtist(t, artistSvc)

	// No query params: NFO order should be preserved (Nevermind, In Utero, Bleach).
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, discographyTabReq(t, a.ID, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// All three albums must be present.
	for _, title := range []string{"Nevermind", "In Utero", "Bleach"} {
		if !strings.Contains(body, title) {
			t.Errorf("body missing album %q:\n%s", title, body)
		}
	}
	// NFO order: Nevermind first, then In Utero, then Bleach.
	idxNevermind := strings.Index(body, "Nevermind")
	idxInUtero := strings.Index(body, "In Utero")
	idxBleach := strings.Index(body, "Bleach")
	if idxNevermind >= idxInUtero || idxInUtero >= idxBleach {
		t.Errorf("default (NFO) order wrong: Nevermind@%d InUtero@%d Bleach@%d", idxNevermind, idxInUtero, idxBleach)
	}
}

func TestHandleArtistDiscographyTab_UnknownOrderDefaultsAsc(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := setupMultiAlbumArtist(t, artistSvc)

	// order=bogus should default to "asc" behavior for sort=title.
	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, discographyTabReq(t, a.ID, "sort=title&order=bogus"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	idxBleach := strings.Index(body, "Bleach")
	idxNevermind := strings.Index(body, "Nevermind")
	if idxBleach < 0 || idxNevermind < 0 {
		t.Fatalf("album titles missing from body:\n%s", body)
	}
	// With asc (the default fallback), Bleach < Nevermind alphabetically.
	if idxBleach > idxNevermind {
		t.Errorf("unknown order should default to asc; Bleach@%d Nevermind@%d", idxBleach, idxNevermind)
	}
}

func TestHandleArtistDiscographyTab_SortYear_EmptyYearSortsLast(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	// NFO with one album missing its year; sort=year&order=asc -> dated entries first.
	nfo := `<?xml version="1.0" encoding="UTF-8"?>
<artist>
  <name>Test</name>
  <album><title>Undated</title></album>
  <album><title>Dated</title><year>2000</year></album>
</artist>
`
	dir := writeArtistNFO(t, nfo)
	a := &artist.Artist{Name: "Test", Path: dir, NFOExists: true}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, discographyTabReq(t, a.ID, "sort=year&order=asc"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	idxDated := strings.Index(body, "Dated")
	idxUndated := strings.Index(body, "Undated")
	if idxDated < 0 || idxUndated < 0 {
		t.Fatalf("album titles missing from body:\n%s", body)
	}
	// Dated (year=2000, key "2000") should appear before Undated (key "9999").
	if idxDated > idxUndated {
		t.Errorf("empty year should sort after dated entries: Dated@%d Undated@%d", idxDated, idxUndated)
	}
}

func TestHandleArtistDiscographyTab_SortYear_EmptyYearSortsLast_Desc(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	// Same NFO as the ASC variant; sort=year&order=desc -> dated entries
	// first (descending by year), undated entries still last -- not floated
	// to the top by the old 9999 sentinel inversion.
	nfo := `<?xml version="1.0" encoding="UTF-8"?>
<artist>
  <name>Test</name>
  <album><title>Undated</title></album>
  <album><title>Dated</title><year>2000</year></album>
</artist>
`
	dir := writeArtistNFO(t, nfo)
	a := &artist.Artist{Name: "Test", Path: dir, NFOExists: true}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	w := httptest.NewRecorder()
	r.handleArtistDiscographyTab(w, discographyTabReq(t, a.ID, "sort=year&order=desc"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	idxDated := strings.Index(body, "Dated")
	idxUndated := strings.Index(body, "Undated")
	if idxDated < 0 || idxUndated < 0 {
		t.Fatalf("album titles missing from body:\n%s", body)
	}
	// Undated should remain LAST even in descending order; a 9999 sentinel
	// would have inverted and placed it first.
	if idxUndated < idxDated {
		t.Errorf("empty year should sort after dated entries in desc order: Dated@%d Undated@%d", idxDated, idxUndated)
	}
}

// Silence unused import for errors package (used in tests that may be added).
var _ = errors.New

// --- Artist-level lock gate on the discography fetch path (#2754) ------------
//
// This is the sixth provider-writing surface the artist lock governs; the other
// five (single refresh, refresh/link, and the deezer / discogs / audiodb /
// bulk-identify / wizard link handlers via autoLinkAndRefresh) are covered in
// handlers_refresh_test.go and the per-provider link test files. Read all of
// them together: one contract, six entry points.
//
// handleFetchDiscography is the strongest place to prove the contract because
// its side effect is a FILE. The tests below assert non-invocation two ways --
// the MusicBrainz adapter was never called, and the bytes of artist.nfo on disk
// are byte-identical afterwards -- so a "returns the right status but still
// does the work" implementation cannot pass them.

// lockedDiscographyNFO is the on-disk artist.nfo used by the locked tests. It
// deliberately contains ONE album that the stub provider does NOT return, so a
// fetch that actually ran would provably rewrite the file.
const lockedDiscographyNFO = `<?xml version="1.0" encoding="UTF-8"?>
<artist>
  <name>Locked Discography</name>
  <album>
    <title>Only Existing Album</title>
    <year>1980</year>
  </album>
</artist>
`

// countingDiscographyRouter builds a discography-fetch router whose MusicBrainz
// stub records how many times it was queried. The counter is the direct
// non-invocation probe: a gate that runs the fetch anyway increments it.
func countingDiscographyRouter(t *testing.T, groups []provider.ReleaseGroupInfo) (*Router, *artist.Service, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	r, artistSvc := discographyFetchRouter(t, func(_ context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
		calls.Add(1)
		return groups, nil
	})
	return r, artistSvc, &calls
}

// lockedDiscographyArtist seeds an artist that satisfies every precondition
// handleFetchDiscography needs BEFORE the lock gate could possibly matter -- an
// MBID, a filesystem path, an existing artist.nfo -- and then locks it,
// asserting each precondition. Without the MBID and path assertions the tests
// below would pass vacuously through the 400 branches, which also perform no
// fetch and no write.
func lockedDiscographyArtist(t *testing.T, svc *artist.Service, name string) (*artist.Artist, string) {
	t.Helper()
	dir := writeArtistNFO(t, lockedDiscographyNFO)
	a := &artist.Artist{Name: name, Path: dir, MusicBrainzID: "mbid-" + name, NFOExists: true}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := svc.Lock(context.Background(), a.ID, "user"); err != nil {
		t.Fatalf("locking artist: %v", err)
	}
	locked, err := svc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if !locked.Locked {
		t.Fatal("precondition failed: artist is not locked after Lock")
	}
	if locked.MusicBrainzID == "" {
		t.Fatal("precondition failed: artist has no MusicBrainz ID, so a skip would prove nothing")
	}
	if locked.Path == "" {
		t.Fatal("precondition failed: artist has no path, so a skip would prove nothing")
	}
	return locked, filepath.Join(dir, "artist.nfo")
}

// readFileBytes returns the raw bytes of a file, failing the test on error.
func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// TestHandleFetchDiscography_LockedArtistSkipped is the guard for the sixth
// ungated surface found in review of #2754: the Discography tab's Fetch button
// ran a MusicBrainz query and rewrote artist.nfo on an artist the operator had
// explicitly locked.
//
// The load-bearing assertions are the provider call count and the on-disk
// bytes, not the status code. An implementation that answered "skipped_locked"
// and then fetched and wrote anyway fails both.
func TestHandleFetchDiscography_LockedArtistSkipped(t *testing.T) {
	t.Parallel()
	r, artistSvc, calls := countingDiscographyRouter(t, []provider.ReleaseGroupInfo{
		{ID: "mbid-new", Title: "Album That Must Not Land", PrimaryType: "Album", FirstReleaseDate: "1991"},
	})
	a, nfoPath := lockedDiscographyArtist(t, artistSvc, "Locked Discography Fetch")
	before := readFileBytes(t, nfoPath)

	ctx := testI18nCtx(t, context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/artists/"+a.ID+"/discography/fetch", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handleFetchDiscography(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "skipped_locked" {
		t.Errorf("status = %v, want skipped_locked; body = %v", resp["status"], resp)
	}

	// Non-invocation proof 1: MusicBrainz was never asked.
	if n := calls.Load(); n != 0 {
		t.Errorf("MusicBrainz release-group calls = %d, want 0; a locked artist must not trigger a provider fetch", n)
	}
	// Non-invocation proof 2: the file on disk is byte-identical.
	if after := readFileBytes(t, nfoPath); !bytes.Equal(before, after) {
		t.Errorf("artist.nfo was rewritten on a locked artist:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestHandleFetchDiscography_UnlockedArtistStillFetches is the positive control
// for the test above. Without it, the locked test would pass just as happily if
// the fetch endpoint were broken for every artist -- the provider count would
// be 0 and the file unchanged for the wrong reason.
func TestHandleFetchDiscography_UnlockedArtistStillFetches(t *testing.T) {
	t.Parallel()
	r, artistSvc, calls := countingDiscographyRouter(t, []provider.ReleaseGroupInfo{
		{ID: "mbid-new", Title: "Album That Must Land", PrimaryType: "Album", FirstReleaseDate: "1991"},
	})

	dir := writeArtistNFO(t, lockedDiscographyNFO)
	nfoPath := filepath.Join(dir, "artist.nfo")
	a := &artist.Artist{Name: "Unlocked Discography Fetch", Path: dir,
		MusicBrainzID: "mbid-unlocked", NFOExists: true}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	// Precondition: the artist is genuinely NOT locked, so success here really
	// does exercise the ungated path.
	if a.Locked {
		t.Fatal("precondition failed: artist is locked; this test covers the unlocked path")
	}
	before := readFileBytes(t, nfoPath)

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
	if result.Added != 1 {
		t.Errorf("Added = %d, want 1; the unlocked fetch did not merge the new release group", result.Added)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("MusicBrainz release-group calls = %d, want 1; the unlocked path must query the provider", n)
	}
	after := readFileBytes(t, nfoPath)
	if bytes.Equal(before, after) {
		t.Fatal("artist.nfo unchanged on the unlocked path; the file-identity assertion in the locked test would be vacuous")
	}
	if !bytes.Contains(after, []byte("Album That Must Land")) {
		t.Errorf("artist.nfo missing the fetched album:\n%s", after)
	}
}

// TestHandleFetchDiscography_LockedArtistHTMX covers the branch the operator
// actually sees. The Fetch button swaps this response over
// #discography-tab-content, so the status must be 200 (a 4xx skips the HTMX
// swap and fires the generic error toast) and the body must be the tab
// partial, carrying the skip explanation and the unchanged album list.
func TestHandleFetchDiscography_LockedArtistHTMX(t *testing.T) {
	t.Parallel()
	r, artistSvc, calls := countingDiscographyRouter(t, []provider.ReleaseGroupInfo{
		{ID: "mbid-new", Title: "Album That Must Not Land", PrimaryType: "Album", FirstReleaseDate: "1991"},
	})
	a, nfoPath := lockedDiscographyArtist(t, artistSvc, "Locked Discography HTMX")
	before := readFileBytes(t, nfoPath)

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
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html so HTMX swaps the partial", ct)
	}
	body := w.Body.String()
	// The swap target must be present or HTMX replaces the tab with nothing.
	if !strings.Contains(body, "discography-tab-content") {
		t.Errorf("body missing discography-tab-content id:\n%s", body)
	}
	if !strings.Contains(body, "Discography fetch skipped: this artist is locked.") {
		t.Errorf("body missing the locked-skip explanation:\n%s", body)
	}
	// The success summary must NOT appear: rendering both would mean the gate
	// returned the locked partial but still ran the merge.
	if strings.Contains(body, "Fetched") {
		t.Errorf("body contains the fetch summary on a locked artist:\n%s", body)
	}
	// The existing on-disk album list still renders, so the operator does not
	// lose the tab contents to the skip.
	if !strings.Contains(body, "Only Existing Album") {
		t.Errorf("body missing the existing on-disk album:\n%s", body)
	}
	if strings.Contains(body, "Album That Must Not Land") {
		t.Errorf("body contains a provider album on a locked artist:\n%s", body)
	}

	if n := calls.Load(); n != 0 {
		t.Errorf("MusicBrainz release-group calls = %d, want 0", n)
	}
	if after := readFileBytes(t, nfoPath); !bytes.Equal(before, after) {
		t.Errorf("artist.nfo was rewritten on a locked artist:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
