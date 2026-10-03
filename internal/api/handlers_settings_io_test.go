package api

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"golang.org/x/crypto/pbkdf2"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/platform"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scraper"
	"github.com/sydlexius/stillwater/internal/settingsio"
	"github.com/sydlexius/stillwater/internal/webhook"
)

// settingsIOTestDeps builds a fresh DB, migrates it, and wires the settingsio
// service and a minimal Router for handler-level tests. The DB is returned so
// tests can seed rows (e.g. user_preferences) that the service has no public
// API for.
func settingsIOTestDeps(t *testing.T) (*Router, *settingsio.Service, *sql.DB) {
	t.Helper()

	db := newTestDB(t)

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	provSvc := provider.NewSettingsService(db, enc)
	connSvc := connection.NewService(db, enc)
	platSvc := platform.NewService(db)
	whSvc := webhook.NewService(db)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	authSvc := auth.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	scraperSvc := scraper.NewService(db, logger)
	if err := scraperSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding scraper: %v", err)
	}

	// Wire rule + scraper services into settingsio so export/import covers
	// the new sections that this PR adds (rules, scraper_configs).
	sioSvc := settingsio.NewService(db, provSvc, connSvc, platSvc, whSvc).
		WithRuleService(ruleSvc).
		WithScraperService(scraperSvc)

	r := NewRouter(RouterDeps{
		SessionSecret:      testSessionSecret,
		AuthService:        authSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfo.NewSnapshotService(db),
		SettingsIOService:  sioSvc,
		DB:                 db,
		Logger:             logger,
		StaticFS:           os.DirFS("../../web/static"),
	})

	return r, sioSvc, db
}

// buildExportedEnvelope exports settings from a fresh DB and returns the JSON
// envelope bytes, ready to be used in an import request.
func buildExportedEnvelope(t *testing.T, svc *settingsio.Service, passphrase string) []byte {
	t.Helper()
	env, err := svc.Export(context.Background(), passphrase)
	if err != nil {
		t.Fatalf("exporting: %v", err)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshaling envelope: %v", err)
	}
	return b
}

// seedUserPreferences inserts a user and their preference rows so handler tests
// can verify that the user_preferences section round-trips through the
// settingsio export/import pipeline.
func seedUserPreferences(t *testing.T, db *sql.DB, username string, prefs map[string]string) {
	t.Helper()
	ctx := context.Background()
	userID := "u-" + username
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, role) VALUES (?, ?, 'operator')`,
		userID, username,
	); err != nil {
		t.Fatalf("seeding user %q: %v", username, err)
	}
	for k, v := range prefs {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO user_preferences (user_id, key, value) VALUES (?, ?, ?)`,
			userID, k, v,
		); err != nil {
			t.Fatalf("seeding pref %q for %q: %v", k, username, err)
		}
	}
}

// setupTestDBForIO mirrors the pattern in settingsio/export_test.go but returns
// a sql.DB so helper functions can seed data without going through the service.
func setupTestDBForIO(t *testing.T) *sql.DB {
	t.Helper()
	return newTestDB(t)
}

// --- Export handler tests ---

func TestHandleSettingsExport_NilService(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	db := setupTestDBForIO(t)
	authSvc := auth.NewService(db)
	r := NewRouter(RouterDeps{
		SessionSecret: testSessionSecret,
		AuthService:   authSvc,
		DB:            db,
		Logger:        logger,
		StaticFS:      os.DirFS("../../web/static"),
	})

	body := `{"passphrase":"secret"}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/export", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSettingsExport(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleSettingsExport_MissingPassphrase(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	body := `{"passphrase":""}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/export", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.handleSettingsExport(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSettingsExport_JSON(t *testing.T) {
	t.Parallel()
	router, _, db := settingsIOTestDeps(t)

	// Seed two preference rows for one user so summary.user_preferences is
	// pinned to a specific value rather than just non-zero.
	seedUserPreferences(t, db, "alice", map[string]string{
		"theme": "dark",
		"lang":  "en",
	})

	body := `{"passphrase":"hunter2"}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/export", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.handleSettingsExport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if !strings.Contains(cd, "stillwater-settings-") {
		t.Errorf("Content-Disposition = %q, want filename with stillwater-settings-", cd)
	}

	var env settingsio.Envelope
	if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if env.Data == "" {
		t.Error("expected non-empty envelope data")
	}
	if env.Salt == "" {
		t.Error("expected non-empty envelope salt")
	}
	if env.Summary == nil {
		t.Fatal("expected non-nil envelope summary")
	}
	// Assert specific summary counts so a regression that drops a section from
	// the response surfaces here, not on a downstream consumer.
	if env.Summary.Rules == 0 {
		t.Errorf("Summary.Rules = 0, want >0 (seeded defaults)")
	}
	if env.Summary.ScraperConfigs != 1 {
		t.Errorf("Summary.ScraperConfigs = %d, want 1 (seeded global)", env.Summary.ScraperConfigs)
	}
	if env.Summary.UserPreferences != 2 {
		t.Errorf("Summary.UserPreferences = %d, want 2 (seeded pairs)", env.Summary.UserPreferences)
	}
}

func TestHandleSettingsExport_FormEncoded(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	body := "passphrase=hunter2"
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/export", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()

	router.handleSettingsExport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

// --- Import handler tests ---

func TestHandleSettingsImport_NilService(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	db := setupTestDBForIO(t)
	authSvc := auth.NewService(db)
	r := NewRouter(RouterDeps{
		SessionSecret: testSessionSecret,
		AuthService:   authSvc,
		DB:            db,
		Logger:        logger,
		StaticFS:      os.DirFS("../../web/static"),
	})

	body := `{"passphrase":"secret","envelope":{}}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSettingsImport(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleSettingsImport_MissingPassphrase_JSON(t *testing.T) {
	t.Parallel()
	router, svc, _ := settingsIOTestDeps(t)
	envBytes := buildExportedEnvelope(t, svc, "secret")

	// Send envelope without passphrase
	body, _ := json.Marshal(map[string]interface{}{
		"passphrase": "",
		"envelope":   json.RawMessage(envBytes),
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.handleSettingsImport(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleSettingsImport_WrongPassphrase_JSON(t *testing.T) {
	t.Parallel()
	router, svc, _ := settingsIOTestDeps(t)
	envBytes := buildExportedEnvelope(t, svc, "correct-passphrase")

	body, _ := json.Marshal(map[string]interface{}{
		"passphrase": "wrong-passphrase",
		"envelope":   json.RawMessage(envBytes),
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.handleSettingsImport(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !strings.Contains(resp["error"], "passphrase") {
		t.Errorf("error message %q should mention passphrase", resp["error"])
	}
}

func TestHandleSettingsImport_WrongPassphrase_HTMX(t *testing.T) {
	t.Parallel()
	router, svc, _ := settingsIOTestDeps(t)
	envBytes := buildExportedEnvelope(t, svc, "correct-passphrase")

	body, _ := json.Marshal(map[string]interface{}{
		"passphrase": "wrong-passphrase",
		"envelope":   json.RawMessage(envBytes),
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	router.handleSettingsImport(w, req)

	// HTMX errors return 200 + red HTML fragment
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (HTMX swap requires 200)", w.Code, http.StatusOK)
	}
	body2 := w.Body.String()
	if !strings.Contains(body2, "text-red") {
		t.Errorf("expected red HTML fragment, got: %s", body2)
	}
	if !strings.Contains(body2, "passphrase") {
		t.Errorf("expected passphrase hint in error fragment, got: %s", body2)
	}
}

func TestHandleSettingsImport_RoundTrip_JSON(t *testing.T) {
	t.Parallel()
	router, svc, db := settingsIOTestDeps(t)
	// Seed two preference pairs and one user before exporting so the import
	// counters can be checked against known values.
	seedUserPreferences(t, db, "alice", map[string]string{
		"theme": "dark",
		"lang":  "en",
	})
	const passphrase = "my-secret"
	envBytes := buildExportedEnvelope(t, svc, passphrase)

	body, _ := json.Marshal(map[string]interface{}{
		"passphrase": passphrase,
		"envelope":   json.RawMessage(envBytes),
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.handleSettingsImport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var result settingsio.ImportResult
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	// Pin the new sections so a regression that drops a counter from the
	// response surfaces here instead of silently shrinking the import shape.
	if result.Rules == 0 {
		t.Errorf("ImportResult.Rules = 0, want >0 (seeded defaults round-trip)")
	}
	if result.ScraperConfigs != 1 {
		t.Errorf("ImportResult.ScraperConfigs = %d, want 1 (seeded global)", result.ScraperConfigs)
	}
	if result.UserPreferences != 2 {
		t.Errorf("ImportResult.UserPreferences = %d, want 2 (seeded pairs)", result.UserPreferences)
	}
}

func TestHandleSettingsImport_RoundTrip_HTMX(t *testing.T) {
	t.Parallel()
	router, svc, _ := settingsIOTestDeps(t)
	const passphrase = "my-secret"
	envBytes := buildExportedEnvelope(t, svc, passphrase)

	body, _ := json.Marshal(map[string]interface{}{
		"passphrase": passphrase,
		"envelope":   json.RawMessage(envBytes),
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	router.handleSettingsImport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body2 := w.Body.String()
	if !strings.Contains(body2, "Import complete:") {
		t.Errorf("expected 'Import complete:' in HTMX response, got: %s", body2)
	}
	if !strings.Contains(body2, "text-green") {
		t.Errorf("expected green HTML fragment for success, got: %s", body2)
	}
	// A clean import must not render the dropped-rows warning (#3012).
	if strings.Contains(body2, "dropped rows") {
		t.Errorf("clean import rendered a dropped-rows warning: %s", body2)
	}
}

func TestHandleSettingsImport_Multipart(t *testing.T) {
	t.Parallel()
	router, svc, _ := settingsIOTestDeps(t)
	const passphrase = "multipart-pass"
	envBytes := buildExportedEnvelope(t, svc, passphrase)

	// Build multipart form
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// passphrase field
	if err := mw.WriteField("passphrase", passphrase); err != nil {
		t.Fatalf("writing passphrase field: %v", err)
	}
	// file field
	fw, err := mw.CreateFormFile("file", "export.json")
	if err != nil {
		t.Fatalf("creating form file: %v", err)
	}
	if _, err := fw.Write(envBytes); err != nil {
		t.Fatalf("writing file field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()

	router.handleSettingsImport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("multipart import status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleSettingsImport_Multipart_MissingFile(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("passphrase", "secret"); err != nil {
		t.Fatalf("writing passphrase field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()

	router.handleSettingsImport(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleSettingsImport_InvalidJSON(t *testing.T) {
	t.Parallel()
	router, _, _ := settingsIOTestDeps(t)

	body := `{not valid json`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.handleSettingsImport(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleSettingsImport_ClearsPipelineRuleCache (#3138): an import rewrites
// rule rows (config, mode, enabled), and the pipeline's rule cache never
// expires, so a successful import must clear it.
func TestHandleSettingsImport_ClearsPipelineRuleCache(t *testing.T) {
	t.Parallel()
	router, svc, _ := settingsIOTestDeps(t)
	clears := &atomic.Int32{}
	router.pipeline = &stubPipeline{ruleCacheClears: clears}
	const passphrase = "cache-pass"
	envBytes := buildExportedEnvelope(t, svc, passphrase)
	body, _ := json.Marshal(map[string]interface{}{"passphrase": passphrase, "envelope": json.RawMessage(envBytes)})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.handleSettingsImport(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	if n := clears.Load(); n != 1 {
		t.Fatalf("pipeline ClearRuleCache calls after a successful import = %d, want 1", n)
	}
}

// sealImportPayload encrypts a hand-built settingsio.Payload into the JSON
// envelope the import handler expects. Export cannot produce an envelope with
// dropped rows (it only writes what the source database holds), so the payload
// is built directly, using the same PBKDF2-SHA256 + AES-256-GCM scheme as
// settingsio.encryptWithPassphrase (600k iterations, 16-byte salt, nonce
// prepended to the ciphertext).
func sealImportPayload(t *testing.T, p settingsio.Payload, passphrase string) []byte {
	t.Helper()
	plain, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshaling payload: %v", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatalf("salt: %v", err)
	}
	block, err := aes.NewCipher(pbkdf2.Key([]byte(passphrase), salt, 600_000, 32, sha256.New))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	env, err := json.Marshal(settingsio.Envelope{
		Version:    settingsio.CurrentEnvelopeVersion,
		AppVersion: "test",
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Data:       base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, nil)),
	})
	if err != nil {
		t.Fatalf("marshaling envelope: %v", err)
	}
	return env
}

// postImport posts the envelope to the real handler (HTMX fragment or JSON
// result, per htmx) and returns the recorder.
func postImport(t *testing.T, router *Router, env []byte, passphrase string, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"passphrase": passphrase, "envelope": json.RawMessage(env)})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	router.handleSettingsImport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	return w
}

// TestHandleSettingsImport_HTMX_ShowsDroppedRows is the #3012 regression test:
// every drop counter an import can report must reach the HTMX summary.
func TestHandleSettingsImport_HTMX_ShowsDroppedRows(t *testing.T) {
	t.Parallel()
	const passphrase = "my-secret"
	payload := settingsio.Payload{
		Settings: map[string]string{
			// Real rejection: a 0-100 threshold cannot hold "0.5" (#3008).
			"mbid_revalidate.name_similarity_threshold": "0.5",
			// Old and current name both present: the old one is dropped.
			"mbid_revalidate.name_similarity": "80",
		},
		Connections: []settingsio.ConnectionExport{{
			Name: "Lidarr A", Type: "lidarr", URL: "http://lidarr.local:8686",
			APIKey: "key1", Enabled: true, FeatureImageWrite: true,
		}},
		Libraries: []settingsio.LibraryExport{{Name: "", Path: "/music", Type: "regular", Source: "manual"}},
		APITokens: []settingsio.APITokenExport{{Name: "t", TokenHash: ""}},
	}
	env := sealImportPayload(t, payload, passphrase)

	// Precondition: a JSON-mode import into its own target proves each
	// counter is really non-zero for this payload.
	jr, _, _ := settingsIOTestDeps(t)
	var res settingsio.ImportResult
	if err := json.NewDecoder(postImport(t, jr, env, passphrase, false).Body).Decode(&res); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if res.SettingsRejected != 1 || res.SettingsRenamedDropped != 1 || res.LibrariesSkipped != 1 ||
		res.APITokensSkipped != 1 || res.ConnectionFeaturesIgnored != 1 {
		t.Fatalf("precondition: want one of each drop counter, got %+v", res)
	}

	router, _, _ := settingsIOTestDeps(t)
	out := postImport(t, router, env, passphrase, true).Body.String()
	for _, want := range []string{
		"Import completed with dropped rows:",
		"Settings rejected as invalid: 1 (mbid_revalidate.name_similarity_threshold)",
		"Settings discarded because the file also carried the current name: 1",
		"Libraries skipped: 1",
		"API tokens skipped: 1",
		"Connection feature settings ignored (not supported by that connection type): 1",
		"text-amber-800",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q; got: %s", want, out)
		}
	}
}

// TestImportDropWarning_CleanCappedEscaped covers the branches the handler test
// cannot reach: a clean result renders nothing, a long rejected-key list is
// truncated with the remainder counted, and key names are HTML-escaped. The
// importer only rejects keys from its fixed validator table, so a hostile key
// name cannot be produced through the real handler; the escape is checked on
// the renderer directly.
func TestImportDropWarning_CleanCappedEscaped(t *testing.T) {
	t.Parallel()
	if got := importDropWarning(&settingsio.ImportResult{Settings: 5}); got != "" {
		t.Errorf("clean import rendered a warning: %q", got)
	}
	// Distinct value per counter, so a swapped number or label fails.
	distinct := importDropWarning(&settingsio.ImportResult{
		SettingsRejected: 2, SettingsRenamedDropped: 3, LibrariesSkipped: 4,
		APITokensSkipped: 5, ConnectionFeaturesIgnored: 6,
	})
	for _, want := range []string{
		"Settings rejected as invalid: 2",
		"Settings discarded because the file also carried the current name: 3",
		"Libraries skipped: 4",
		"API tokens skipped: 5",
		"Connection feature settings ignored (not supported by that connection type): 6",
	} {
		if !strings.Contains(distinct, want) {
			t.Errorf("missing %q in: %s", want, distinct)
		}
	}
	// Rejected with no key names: count only, no parentheses.
	if got := importDropWarning(&settingsio.ImportResult{SettingsRejected: 3}); !strings.Contains(got, "Settings rejected as invalid: 3</li>") || strings.Contains(got, "(") {
		t.Errorf("rejected count without keys rendered wrongly: %s", got)
	}
	keys := make([]string, 25)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%02d", i)
	}
	got := importDropWarning(&settingsio.ImportResult{SettingsRejected: 30, SettingsRejectedKeys: keys})
	if !strings.Contains(got, "k09") || strings.Contains(got, "k10") || !strings.Contains(got, "and 20 more)") {
		t.Errorf("cap not applied (want k00..k09 and 20 more): %s", got)
	}

	const evil = "<script>x</script>"
	got = importDropWarning(&settingsio.ImportResult{SettingsRejected: 1, SettingsRejectedKeys: []string{evil}})
	if strings.Contains(got, evil) || !strings.Contains(got, "&lt;script&gt;x&lt;/script&gt;") {
		t.Errorf("rejected key must appear escaped only; got: %s", got)
	}
}

// TestHandleSettingsImport_JSONResponseMatchesSpec (#3012): the import 200
// response must conform to openapi.yaml AND every field it carries must be
// declared there. kin-openapi allows undeclared extra properties, so the
// conformance check alone cannot notice a counter missing from the schema;
// the declared-property check does. The payload makes the five drop counters
// and ConnectionFeaturesIgnored non-zero so those omitempty fields really
// appear; settings_renamed is covered by its own test below.
func TestHandleSettingsImport_JSONResponseMatchesSpec(t *testing.T) {
	t.Parallel()
	const passphrase = "my-secret"
	env := sealImportPayload(t, settingsio.Payload{
		Settings: map[string]string{
			"mbid_revalidate.name_similarity_threshold": "0.5",
			"mbid_revalidate.name_similarity":           "80",
		},
		Connections: []settingsio.ConnectionExport{{
			Name: "Lidarr A", Type: "lidarr", URL: "http://lidarr.local:8686",
			APIKey: "key1", Enabled: true, FeatureImageWrite: true,
		}},
		Libraries: []settingsio.LibraryExport{{Name: "", Path: "/music", Type: "regular", Source: "manual"}},
		APITokens: []settingsio.APITokenExport{{Name: "t", TokenHash: ""}},
	}, passphrase)
	router, _, _ := settingsIOTestDeps(t)

	body, _ := json.Marshal(map[string]interface{}{"passphrase": passphrase, "envelope": json.RawMessage(env)})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := serveValidated(t, http.HandlerFunc(router.handleSettingsImport), req)

	var got map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	// Precondition: the drop counters are really present in the response.
	for _, k := range []string{"settings_rejected", "settings_rejected_keys", "settings_renamed_dropped",
		"libraries_skipped", "api_tokens_skipped", "connection_features_ignored"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("precondition: response lacks %q: %s", k, w.Body.String())
		}
	}

	route, _, err := loadSpec(t).FindRoute(req)
	if err != nil {
		t.Fatalf("finding spec route: %v", err)
	}
	schema := route.Operation.Responses.Status(200).Value.Content.Get("application/json").Schema.Value
	for k := range got {
		if _, declared := schema.Properties[k]; !declared {
			t.Errorf("response field %q is not declared in the openapi 200 schema", k)
		}
	}
}

// TestHandleSettingsImport_JSONRequiredCountersPresentAtZero (#3012): the four
// counters the spec marks required must be emitted even when zero, because
// making them optional would be a breaking change for API clients. A clean
// payload zeroes all four, so an omit-when-zero tag fails this test.
func TestHandleSettingsImport_JSONRequiredCountersPresentAtZero(t *testing.T) {
	t.Parallel()
	const passphrase = "my-secret"
	env := sealImportPayload(t, settingsio.Payload{Settings: map[string]string{}}, passphrase)
	router, _, _ := settingsIOTestDeps(t)
	body, _ := json.Marshal(map[string]interface{}{"passphrase": passphrase, "envelope": json.RawMessage(env)})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// serveValidated enforces the spec's required list against the response.
	w := serveValidated(t, http.HandlerFunc(router.handleSettingsImport), req)
	var got map[string]int
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	for _, k := range []string{"libraries_skipped", "api_tokens_skipped", "users_imported", "ownership_reassigned"} {
		v, ok := got[k]
		if !ok || v != 0 {
			t.Errorf("%s = %d (present=%v), want present and 0", k, v, ok)
		}
	}
}

// TestHandleSettingsImport_JSONSettingsRenamedMatchesSpec: a payload carrying
// only the OLD key name migrates it, so settings_renamed is non-zero and must
// be declared in the spec (deleting it from the schema must fail here).
func TestHandleSettingsImport_JSONSettingsRenamedMatchesSpec(t *testing.T) {
	t.Parallel()
	const passphrase = "my-secret"
	env := sealImportPayload(t, settingsio.Payload{
		Settings: map[string]string{"mbid_revalidate.name_similarity": "80"},
	}, passphrase)
	router, _, _ := settingsIOTestDeps(t)
	body, _ := json.Marshal(map[string]interface{}{"passphrase": passphrase, "envelope": json.RawMessage(env)})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/settings/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := serveValidated(t, http.HandlerFunc(router.handleSettingsImport), req)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if string(got["settings_renamed"]) != "1" {
		t.Fatalf("precondition: settings_renamed = %s, want 1", got["settings_renamed"])
	}
	route, _, err := loadSpec(t).FindRoute(req)
	if err != nil {
		t.Fatalf("finding spec route: %v", err)
	}
	props := route.Operation.Responses.Status(200).Value.Content.Get("application/json").Schema.Value.Properties
	if _, ok := props["settings_renamed"]; !ok {
		t.Error("settings_renamed is not declared in the import 200 schema")
	}
}
