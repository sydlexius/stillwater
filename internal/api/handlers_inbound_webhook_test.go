package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// TestHandleEmbyWebhook_OK verifies the handler returns 200 immediately.
func TestHandleEmbyWebhook_OK(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := `{"Event":"system.notificationtest"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/emby",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleEmbyWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// TestHandleEmbyWebhook_InvalidJSON verifies 400 on bad JSON.
func TestHandleEmbyWebhook_InvalidJSON(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/emby",
		strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleEmbyWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleEmbyWebhook_MissingEvent verifies 400 when Event field is absent.
func TestHandleEmbyWebhook_MissingEvent(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/emby",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleEmbyWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleEmbyWebhook_UnknownEventType verifies 200 with unknown event type (handled gracefully).
func TestHandleEmbyWebhook_UnknownEventType(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := `{"Event":"some.unknown.event"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/emby",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleEmbyWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// TestHandleJellyfinWebhook_OK verifies the handler returns 200 immediately.
func TestHandleJellyfinWebhook_OK(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := `{"NotificationType":"Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/jellyfin",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleJellyfinWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// TestHandleJellyfinWebhook_InvalidJSON verifies 400 on bad JSON.
func TestHandleJellyfinWebhook_InvalidJSON(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/jellyfin",
		strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleJellyfinWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleJellyfinWebhook_MissingNotificationType verifies 400 when field is absent.
func TestHandleJellyfinWebhook_MissingNotificationType(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/jellyfin",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleJellyfinWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleJellyfinWebhook_UnknownEventType verifies 200 with unknown event type.
func TestHandleJellyfinWebhook_UnknownEventType(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := `{"NotificationType":"SomeUnknownEvent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/jellyfin",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleJellyfinWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

// TestLidarrArtistAdd_NilPipeline verifies that handleLidarrArtistAdd does not
// panic when pipeline is nil and an existing artist is found.
func TestLidarrArtistAdd_NilPipeline(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	// testRouter does not set pipeline, so r.pipeline == nil.

	mbid := "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	a := &artist.Artist{
		Name:          "Radiohead",
		SortName:      "Radiohead",
		Type:          "group",
		Path:          "/music/Radiohead",
		MusicBrainzID: mbid,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	payload := webhook.LidarrPayload{
		EventType: webhook.LidarrEventArtistAdd,
		Artist:    &webhook.LidarrArtist{Name: "Radiohead", MBId: mbid},
	}

	// Should not panic; should log warning and return gracefully.
	r.handleLidarrArtistAdd(context.Background(), payload)
}

// TestLidarrDownload_NilPipeline verifies that handleLidarrDownload does not
// panic when pipeline is nil and an existing artist is found.
func TestLidarrDownload_NilPipeline(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	mbid := "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	a := &artist.Artist{
		Name:          "Radiohead",
		SortName:      "Radiohead",
		Type:          "group",
		Path:          "/music/Radiohead",
		MusicBrainzID: mbid,
	}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	payload := webhook.LidarrPayload{
		EventType: webhook.LidarrEventDownload,
		Artist:    &webhook.LidarrArtist{Name: "Radiohead", MBId: mbid},
	}

	// Should not panic; should log warning and return gracefully.
	r.handleLidarrDownload(context.Background(), payload)
}

// ---------------------------------------------------------------------------
// Shutdown drain tests (#1463)
// ---------------------------------------------------------------------------

// TestDrainWebhooks_AllGoroutinesFinish verifies that DrainWebhooks blocks
// until all in-flight webhook goroutines have completed and returns nil.
func TestDrainWebhooks_AllGoroutinesFinish(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	const n = 5
	var completed atomic.Int32
	started := make(chan struct{}, n)
	release := make(chan struct{})

	// Manually spawn goroutines that simulate in-flight webhook processing.
	for i := 0; i < n; i++ {
		r.webhookWg.Add(1)
		go func() {
			defer r.webhookWg.Done()
			// Signal that this goroutine has started, then block until released.
			started <- struct{}{}
			<-release
			completed.Add(1)
		}()
	}

	// Wait for all goroutines to start so we know the WaitGroup is live.
	for i := 0; i < n; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("goroutine did not start within timeout")
		}
	}

	// Release all goroutines in the background and then drain.
	go func() { close(release) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := r.DrainWebhooks(ctx); err != nil {
		t.Fatalf("DrainWebhooks returned error: %v", err)
	}

	if got := completed.Load(); got != n {
		t.Errorf("completed goroutines = %d, want %d", got, n)
	}
}

// TestDrainWebhooks_CancelPropagates verifies that DrainWebhooks returns
// ctx.Err() when the caller's context is canceled before goroutines finish.
func TestDrainWebhooks_CancelPropagates(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	// Spawn a goroutine that will never finish on its own.
	block := make(chan struct{})
	r.webhookWg.Add(1)
	go func() {
		defer r.webhookWg.Done()
		<-block // blocked forever until test exits
	}()
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := r.DrainWebhooks(ctx)
	if err == nil {
		t.Fatal("expected error from DrainWebhooks when context canceled, got nil")
	}
}

// ---------------------------------------------------------------------------
// HMAC verification tests (#1404)
// ---------------------------------------------------------------------------

// testHMACSignature computes the sha256= header value for a given body and secret.
func testHMACSignature(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestVerifyHMACSignature_Valid verifies that a correct signature returns nil.
func TestVerifyHMACSignature_Valid(t *testing.T) {
	t.Parallel()
	body := []byte(`{"eventType":"Test"}`)
	secret := "supersecret"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", testHMACSignature(body, secret))

	if err := verifyHMACSignature(req, body, secret); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// TestVerifyHMACSignature_Invalid verifies that a wrong signature returns an error.
func TestVerifyHMACSignature_Invalid(t *testing.T) {
	t.Parallel()
	body := []byte(`{"eventType":"Test"}`)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")

	if err := verifyHMACSignature(req, body, "secret"); err == nil {
		t.Error("expected error for invalid signature, got nil")
	}
}

// TestVerifyHMACSignature_MissingHeader verifies that an absent header returns an error.
func TestVerifyHMACSignature_MissingHeader(t *testing.T) {
	t.Parallel()
	body := []byte(`{"eventType":"Test"}`)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))

	if err := verifyHMACSignature(req, body, "secret"); err == nil {
		t.Error("expected error for missing header, got nil")
	}
}

// TestHandleLidarrWebhook_HMAC_NoSecret verifies that requests pass when no
// secret is configured in the settings table (backward compatible).
func TestHandleLidarrWebhook_HMAC_NoSecret(t *testing.T) {
	t.Parallel()
	r, _ := testRouter(t)

	body := `{"eventType":"Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/lidarr",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleLidarrWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// TestHandleLidarrWebhook_HMAC_Valid verifies 200 when a valid HMAC is provided.
func TestHandleLidarrWebhook_HMAC_Valid(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithHMACSecret(t, "webhook.inbound.lidarr.secret", "mysecret")

	bodyBytes := []byte(`{"eventType":"Test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/lidarr",
		strings.NewReader(string(bodyBytes)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", testHMACSignature(bodyBytes, "mysecret"))
	w := httptest.NewRecorder()

	r.handleLidarrWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// TestHandleLidarrWebhook_HMAC_Invalid verifies 401 when the HMAC is wrong.
func TestHandleLidarrWebhook_HMAC_Invalid(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithHMACSecret(t, "webhook.inbound.lidarr.secret", "mysecret")

	bodyBytes := []byte(`{"eventType":"Test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/lidarr",
		strings.NewReader(string(bodyBytes)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256=badhex")
	w := httptest.NewRecorder()

	r.handleLidarrWebhook(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

// TestHandleLidarrWebhook_HMAC_MissingHeader verifies 401 when a secret is
// configured but the signature header is absent.
func TestHandleLidarrWebhook_HMAC_MissingHeader(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithHMACSecret(t, "webhook.inbound.lidarr.secret", "mysecret")

	body := `{"eventType":"Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/lidarr",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No X-Hub-Signature-256 header.
	w := httptest.NewRecorder()

	r.handleLidarrWebhook(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

// TestHandleEmbyWebhook_HMAC_Valid verifies 200 when Emby HMAC is correct.
func TestHandleEmbyWebhook_HMAC_Valid(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithHMACSecret(t, "webhook.inbound.emby.secret", "embysecret")

	bodyBytes := []byte(`{"Event":"system.notificationtest"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/emby",
		strings.NewReader(string(bodyBytes)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", testHMACSignature(bodyBytes, "embysecret"))
	w := httptest.NewRecorder()

	r.handleEmbyWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// TestHandleEmbyWebhook_HMAC_Invalid verifies 401 when Emby HMAC is wrong.
func TestHandleEmbyWebhook_HMAC_Invalid(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithHMACSecret(t, "webhook.inbound.emby.secret", "embysecret")

	bodyBytes := []byte(`{"Event":"system.notificationtest"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/emby",
		strings.NewReader(string(bodyBytes)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256=wrongmac")
	w := httptest.NewRecorder()

	r.handleEmbyWebhook(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

// TestHandleJellyfinWebhook_HMAC_Valid verifies 200 when Jellyfin HMAC is correct.
func TestHandleJellyfinWebhook_HMAC_Valid(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithHMACSecret(t, "webhook.inbound.jellyfin.secret", "jfsecret")

	bodyBytes := []byte(`{"NotificationType":"Test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/jellyfin",
		strings.NewReader(string(bodyBytes)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", testHMACSignature(bodyBytes, "jfsecret"))
	w := httptest.NewRecorder()

	r.handleJellyfinWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// TestHandleJellyfinWebhook_HMAC_Invalid verifies 401 when Jellyfin HMAC is wrong.
func TestHandleJellyfinWebhook_HMAC_Invalid(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithHMACSecret(t, "webhook.inbound.jellyfin.secret", "jfsecret")

	bodyBytes := []byte(`{"NotificationType":"Test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/jellyfin",
		strings.NewReader(string(bodyBytes)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256=badhex")
	w := httptest.NewRecorder()

	r.handleJellyfinWebhook(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

// testRouterWithHMACSecret returns a Router with a real encryptor and the given
// HMAC secret stored encrypted in the settings table under settingsKey.
func testRouterWithHMACSecret(t *testing.T, settingsKey, secret string) (*Router, *artist.Service) {
	t.Helper()

	r, artistSvc := testRouter(t)

	// Wire a real encryptor into the router.
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	r.encryptor = enc

	// Encrypt the secret and persist it to the settings table.
	encrypted, err := enc.Encrypt(secret)
	if err != nil {
		t.Fatalf("encrypting HMAC secret: %v", err)
	}
	_, err = r.db.ExecContext(context.Background(),
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingsKey, encrypted, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("inserting HMAC secret into settings: %v", err)
	}

	return r, artistSvc
}
