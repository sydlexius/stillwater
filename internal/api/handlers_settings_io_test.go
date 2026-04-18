package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"log/slog"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
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

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test.db")

	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

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
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// --- Export handler tests ---

func TestHandleSettingsExport_NilService(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	db := setupTestDBForIO(t)
	authSvc := auth.NewService(db)
	r := NewRouter(RouterDeps{
		AuthService: authSvc,
		DB:          db,
		Logger:      logger,
		StaticFS:    os.DirFS("../../web/static"),
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	db := setupTestDBForIO(t)
	authSvc := auth.NewService(db)
	r := NewRouter(RouterDeps{
		AuthService: authSvc,
		DB:          db,
		Logger:      logger,
		StaticFS:    os.DirFS("../../web/static"),
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
}

func TestHandleSettingsImport_Multipart(t *testing.T) {
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
