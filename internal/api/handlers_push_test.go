package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/platform"
)

// addTestConnectionWithURL creates a connection with a custom URL for handler tests
// that need to call a mock HTTP server.
func addTestConnectionWithURL(t *testing.T, r *Router, id, name, connType, url string) {
	t.Helper()
	c := &connection.Connection{
		ID:      id,
		Name:    name,
		Type:    connType,
		URL:     url,
		APIKey:  "test-key",
		Enabled: true,
		Status:  "ok",
	}
	if err := r.connectionService.Create(context.Background(), c); err != nil {
		t.Fatalf("creating test connection %s: %v", id, err)
	}
}

func TestHandleDeletePushImage_Success(t *testing.T) {
	type capture struct {
		method string
		path   string
	}
	captureCh := make(chan capture, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case captureCh <- capture{method: r.Method, path: r.URL.Path}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	addTestConnectionWithURL(t, router, "conn-1", "Emby", "emby", srv.URL)
	a := addTestArtist(t, artistSvc, "Radiohead")

	body := `{"connection_id":"conn-1","platform_artist_id":"emby-artist-1"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	select {
	case got := <-captureCh:
		if got.method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", got.method)
		}
		if !strings.Contains(got.path, "/Images/Primary") {
			t.Errorf("unexpected path: %s", got.path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mock server received no request within timeout")
	}
	// Drain check: verify no unexpected extra platform delete requests.
	select {
	case <-captureCh:
		t.Error("unexpected extra platform delete request")
	default:
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "deleted" {
		t.Errorf("status = %q, want deleted", resp["status"])
	}
}

func TestHandleDeletePushImage_AutoLookupPlatformID(t *testing.T) {
	pathCh := make(chan string, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case pathCh <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	addTestConnectionWithURL(t, router, "conn-1", "Emby", "emby", srv.URL)
	a := addTestArtist(t, artistSvc, "Radiohead")

	// Store a platform ID mapping.
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-1", "emby-stored-id"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Omit platform_artist_id -- handler should auto-lookup from stored mapping.
	body := `{"connection_id":"conn-1"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	select {
	case got := <-pathCh:
		if !strings.Contains(got, "/Items/emby-stored-id/Images/Primary") {
			t.Errorf("unexpected path: %s (want stored platform id in path)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mock server received no request within timeout")
	}
	// Drain check: verify no unexpected extra platform delete requests.
	select {
	case <-pathCh:
		t.Error("unexpected extra platform delete request")
	default:
	}
}

func TestHandleDeletePushImage_InvalidType(t *testing.T) {
	router, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	body := `{"connection_id":"conn-1","platform_artist_id":"emby-001"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/clearart",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "clearart")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeletePushImage_MissingConnectionID(t *testing.T) {
	router, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	body := `{"platform_artist_id":"emby-001"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeletePushImage_ConnectionNotFound(t *testing.T) {
	router, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	body := `{"connection_id":"nonexistent","platform_artist_id":"emby-001"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleDeletePushImage_ConnectionDisabled(t *testing.T) {
	router, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	// Create a disabled connection.
	c := &connection.Connection{
		ID:      "conn-disabled",
		Name:    "Disabled Emby",
		Type:    "emby",
		URL:     "http://localhost:8096",
		APIKey:  "key",
		Enabled: false,
		Status:  "ok",
	}
	if err := router.connectionService.Create(context.Background(), c); err != nil {
		t.Fatalf("creating disabled connection: %v", err)
	}

	body := `{"connection_id":"conn-disabled","platform_artist_id":"emby-001"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeletePushImage_ArtistNotFound(t *testing.T) {
	router, _ := testRouter(t)
	addTestConnection(t, router, "conn-1", "Emby", "emby")

	body := `{"connection_id":"conn-1","platform_artist_id":"emby-001"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/nonexistent/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", "nonexistent")
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleDeletePushImage_JellyfinSuccess(t *testing.T) {
	type capture struct {
		method string
		path   string
	}
	captureCh := make(chan capture, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case captureCh <- capture{method: r.Method, path: r.URL.Path}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	router, artistSvc := testRouter(t)
	addTestConnectionWithURL(t, router, "conn-jf", "Jellyfin", "jellyfin", srv.URL)
	a := addTestArtist(t, artistSvc, "Portishead")

	body := `{"connection_id":"conn-jf","platform_artist_id":"jf-artist-1"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/logo",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "logo")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	select {
	case got := <-captureCh:
		if got.method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", got.method)
		}
		if !strings.Contains(got.path, "/Images/Logo") {
			t.Errorf("unexpected path: %s", got.path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mock server received no request within timeout")
	}
	// Drain check: verify no unexpected extra platform delete requests.
	select {
	case <-captureCh:
		t.Error("unexpected extra platform delete request")
	default:
	}
}

func TestHandleDeletePushImage_InvalidJSON(t *testing.T) {
	router, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(`{invalid`))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleDeletePushImage_PlatformIDNotStoredAndNotProvided(t *testing.T) {
	router, artistSvc := testRouter(t)
	addTestConnection(t, router, "conn-1", "Emby", "emby")
	a := addTestArtist(t, artistSvc, "Radiohead")

	// No platform ID stored, none provided.
	body := `{"connection_id":"conn-1"}`
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/push/images/thumb",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.SetPathValue("type", "thumb")
	w := httptest.NewRecorder()

	router.handleDeletePushImage(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// --- handlePushImages tests ---

func TestHandlePushImages_FanartUploadedAtCorrectIndices(t *testing.T) {
	var mu sync.Mutex
	var captured []int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Emby/Jellyfin upload path: POST /Items/{id}/Images/Backdrop/{index}
		if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/Images/Backdrop/") {
			parts := strings.Split(req.URL.Path, "/")
			idx, err := strconv.Atoi(parts[len(parts)-1])
			if err == nil {
				mu.Lock()
				captured = append(captured, idx)
				mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, artistSvc := testRouter(t)
	r.platformService = platform.NewService(r.db)
	dir := t.TempDir()

	a := &artist.Artist{Name: "Fanart Push", SortName: "Fanart Push", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-push-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Create 3 fanart files using the default primary name (fanart.jpg).
	for _, name := range []string{"fanart.jpg", "fanart2.jpg", "fanart3.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	body := `{"connection_id":"conn-emby","platform_artist_id":"emby-push-1","image_types":["fanart"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/push/images",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handlePushImages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Uploaded []string `json:"uploaded"`
		Errors   []string `json:"errors"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(resp.Uploaded) != 3 {
		t.Fatalf("expected 3 uploaded, got %d: %v", len(resp.Uploaded), resp.Uploaded)
	}
	for i, want := range []string{"fanart[0]", "fanart[1]", "fanart[2]"} {
		if resp.Uploaded[i] != want {
			t.Errorf("uploaded[%d] = %q, want %q", i, resp.Uploaded[i], want)
		}
	}
	if len(resp.Errors) != 0 {
		t.Errorf("expected no errors, got %v", resp.Errors)
	}

	// Verify the mock server received uploads at indices 0, 1, 2.
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 3 {
		t.Fatalf("mock server received %d upload calls, want 3", len(captured))
	}
	for i, idx := range captured {
		if idx != i {
			t.Errorf("upload[%d] index = %d, want %d", i, idx, i)
		}
	}
}

func TestHandlePushImages_ReadFailureProducesSanitizedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r, artistSvc := testRouter(t)
	r.platformService = platform.NewService(r.db)
	dir := t.TempDir()

	a := &artist.Artist{Name: "Read Fail", SortName: "Read Fail", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-rf-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Create a symlink to a non-existent target. DiscoverFanart will find it
	// (it passes the IsDir check), but os.ReadFile will fail.
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "fanart.jpg")); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	body := `{"connection_id":"conn-emby","platform_artist_id":"emby-rf-1","image_types":["fanart"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/push/images",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handlePushImages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Uploaded []string `json:"uploaded"`
		Errors   []string `json:"errors"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	// The error message should say "read failed", not leak the raw OS error.
	if len(resp.Errors) == 0 {
		t.Fatal("expected at least one error for unreadable fanart")
	}
	if !strings.Contains(resp.Errors[0], "read failed") {
		t.Errorf("error should say 'read failed', got %q", resp.Errors[0])
	}
	// Must not contain raw error details (OS error text or on-disk paths/filenames).
	for _, leak := range []string{"is a directory", "no such file", "permission denied", "does-not-exist"} {
		if strings.Contains(resp.Errors[0], leak) {
			t.Errorf("error leaks raw OS detail %q: %q", leak, resp.Errors[0])
		}
	}
}

func TestHandlePushImages_UploadFailureProducesSanitizedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r, artistSvc := testRouter(t)
	r.platformService = platform.NewService(r.db)
	dir := t.TempDir()

	a := &artist.Artist{Name: "Upload Fail", SortName: "Upload Fail", Path: dir}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	addTestConnectionWithURL(t, r, "conn-emby", "Emby", "emby", srv.URL)
	if err := artistSvc.SetPlatformID(context.Background(), a.ID, "conn-emby", "emby-uf-1"); err != nil {
		t.Fatalf("SetPlatformID: %v", err)
	}

	// Create a fanart file that can be read successfully.
	if err := os.WriteFile(filepath.Join(dir, "fanart.jpg"), []byte("fake-image"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := `{"connection_id":"conn-emby","platform_artist_id":"emby-uf-1","image_types":["fanart"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/push/images",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handlePushImages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Uploaded []string `json:"uploaded"`
		Errors   []string `json:"errors"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if len(resp.Errors) == 0 {
		t.Fatal("expected at least one error for failed upload")
	}
	if !strings.Contains(resp.Errors[0], "upload failed") {
		t.Errorf("error should say 'upload failed', got %q", resp.Errors[0])
	}
	// Must not contain raw HTTP status or internal error details.
	if strings.Contains(resp.Errors[0], "500") {
		t.Errorf("error leaks HTTP status code: %q", resp.Errors[0])
	}
}

func TestBuildArtistPushData_TypeAwareDates(t *testing.T) {
	tests := []struct {
		name          string
		artistType    string
		wantBorn      string
		wantFormed    string
		wantDied      string
		wantDisbanded string
	}{
		{
			name:          "group excludes born and died",
			artistType:    "group",
			wantBorn:      "",
			wantFormed:    "1985",
			wantDied:      "",
			wantDisbanded: "2010",
		},
		{
			name:          "orchestra excludes born and died",
			artistType:    "orchestra",
			wantBorn:      "",
			wantFormed:    "1985",
			wantDied:      "",
			wantDisbanded: "2010",
		},
		{
			name:          "solo excludes formed and disbanded",
			artistType:    "solo",
			wantBorn:      "1982",
			wantFormed:    "",
			wantDied:      "2016",
			wantDisbanded: "",
		},
		{
			name:          "choir excludes born and died",
			artistType:    "choir",
			wantBorn:      "",
			wantFormed:    "1985",
			wantDied:      "",
			wantDisbanded: "2010",
		},
		{
			name:          "unknown type includes all fields",
			artistType:    "",
			wantBorn:      "1982",
			wantFormed:    "1985",
			wantDied:      "2016",
			wantDisbanded: "2010",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &artist.Artist{
				Name:      "Test",
				Type:      tt.artistType,
				Born:      "1982",
				Formed:    "1985",
				Died:      "2016",
				Disbanded: "2010",
			}
			data := buildArtistPushData(a)
			if data.Born != tt.wantBorn {
				t.Errorf("Born = %q, want %q", data.Born, tt.wantBorn)
			}
			if data.Formed != tt.wantFormed {
				t.Errorf("Formed = %q, want %q", data.Formed, tt.wantFormed)
			}
			if data.Died != tt.wantDied {
				t.Errorf("Died = %q, want %q", data.Died, tt.wantDied)
			}
			if data.Disbanded != tt.wantDisbanded {
				t.Errorf("Disbanded = %q, want %q", data.Disbanded, tt.wantDisbanded)
			}
		})
	}
}
