package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/backup"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/rule"
)

// testRouterWithBackup creates a Router backed by a file-based SQLite DB
// (required for VACUUM INTO) and a configured backup.Service.
func testRouterWithBackup(t *testing.T) (*Router, *backup.Service) {
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

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	backupDir := filepath.Join(dbDir, "backups")
	backupSvc := backup.NewService(db, backupDir, 7, logger)

	authSvc := auth.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	nfoSnapSvc := nfo.NewSnapshotService(db)

	r := NewRouter(RouterDeps{
		AuthService:        authSvc,
		RuleService:        ruleSvc,
		NFOSnapshotService: nfoSnapSvc,
		BackupService:      backupSvc,
		DB:                 db,
		Logger:             logger,
		StaticDir:          "../../web/static",
	})

	return r, backupSvc
}

func TestHandleBackupCreate_JSON(t *testing.T) {
	r, _ := testRouterWithBackup(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/backup", nil)
	w := httptest.NewRecorder()

	r.handleBackupCreate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var info backup.BackupInfo
	if err := json.NewDecoder(w.Body).Decode(&info); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if info.Filename == "" {
		t.Error("expected non-empty filename")
	}
	if info.Size == 0 {
		t.Error("expected non-zero size")
	}
}

func TestHandleBackupCreate_HTMX(t *testing.T) {
	r, _ := testRouterWithBackup(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/backup", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleBackupCreate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if ct != "text/html" {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := w.Body.String()
	if !strings.Contains(body, "stillwater-") {
		t.Errorf("expected backup filename in HTML, got: %s", body)
	}
}

func TestHandleBackupHistory_JSONEmpty(t *testing.T) {
	r, _ := testRouterWithBackup(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/backup/history", nil)
	w := httptest.NewRecorder()

	r.handleBackupHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var backups []backup.BackupInfo
	if err := json.NewDecoder(w.Body).Decode(&backups); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("expected empty backup list, got %d items", len(backups))
	}
}

func TestHandleBackupHistory_JSONWithBackups(t *testing.T) {
	r, backupSvc := testRouterWithBackup(t)

	if _, err := backupSvc.Backup(context.Background()); err != nil {
		t.Fatalf("creating test backup: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/backup/history", nil)
	w := httptest.NewRecorder()

	r.handleBackupHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var backups []backup.BackupInfo
	if err := json.NewDecoder(w.Body).Decode(&backups); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(backups) != 1 {
		t.Errorf("expected 1 backup, got %d", len(backups))
	}
}

func TestHandleBackupHistory_HTMXEmpty(t *testing.T) {
	r, _ := testRouterWithBackup(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/backup/history", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleBackupHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if !strings.Contains(body, "No backups yet") {
		t.Errorf("expected 'No backups yet' in response, got: %s", body)
	}
}

func TestHandleBackupHistory_HTMXWithBackups(t *testing.T) {
	r, backupSvc := testRouterWithBackup(t)

	if _, err := backupSvc.Backup(context.Background()); err != nil {
		t.Fatalf("creating test backup: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/backup/history", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleBackupHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "text/html" {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := w.Body.String()
	if !strings.Contains(body, "<table") {
		t.Error("expected HTML table in response")
	}
	if !strings.Contains(body, "stillwater-") {
		t.Error("expected backup filename in HTML table")
	}
}

func TestHandleBackupDownload_InvalidFilename(t *testing.T) {
	r, _ := testRouterWithBackup(t)

	cases := []struct {
		name     string
		filename string
	}{
		{"path traversal", "../etc/passwd"},
		{"backslash traversal", `..\\secret`},
		{"wrong pattern", "not-a-backup.txt"},
		{"empty filename", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/backup/"+tc.filename, nil)
			req.SetPathValue("filename", tc.filename)
			w := httptest.NewRecorder()

			r.handleBackupDownload(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleBackupDownload_ValidFile(t *testing.T) {
	r, backupSvc := testRouterWithBackup(t)

	info, err := backupSvc.Backup(context.Background())
	if err != nil {
		t.Fatalf("creating test backup: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/backup/"+info.Filename, nil)
	req.SetPathValue("filename", info.Filename)
	w := httptest.NewRecorder()

	r.handleBackupDownload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, info.Filename) {
		t.Errorf("Content-Disposition = %q, want filename %q", cd, info.Filename)
	}
	if !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}

	if w.Body.Len() == 0 {
		t.Error("expected non-empty response body")
	}
}

func TestHandleBackupDownload_FileNotFound(t *testing.T) {
	r, _ := testRouterWithBackup(t)

	filename := "stillwater-20260101-120000.db"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/backup/"+filename, nil)
	req.SetPathValue("filename", filename)
	w := httptest.NewRecorder()

	r.handleBackupDownload(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleBackupDelete_JSON(t *testing.T) {
	r, backupSvc := testRouterWithBackup(t)

	info, err := backupSvc.Backup(context.Background())
	if err != nil {
		t.Fatalf("creating test backup: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/settings/backup/"+info.Filename, nil)
	req.SetPathValue("filename", info.Filename)
	w := httptest.NewRecorder()

	r.handleBackupDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "deleted" {
		t.Errorf("status = %q, want %q", resp["status"], "deleted")
	}
}

func TestHandleBackupDelete_HTMX(t *testing.T) {
	r, backupSvc := testRouterWithBackup(t)

	info, err := backupSvc.Backup(context.Background())
	if err != nil {
		t.Fatalf("creating test backup: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/settings/backup/"+info.Filename, nil)
	req.SetPathValue("filename", info.Filename)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleBackupDelete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if ct != "text/html" {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := w.Body.String()
	if !strings.Contains(body, "No backups yet") {
		t.Errorf("expected empty backup list after delete, got: %s", body)
	}
}

func TestHandleBackupDelete_InvalidFilename(t *testing.T) {
	r, _ := testRouterWithBackup(t)

	cases := []struct {
		name     string
		filename string
	}{
		{"path traversal", "../etc/passwd"},
		{"wrong pattern", "not-a-backup.txt"},
		{"empty filename", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, "/api/v1/settings/backup/"+tc.filename, nil)
			req.SetPathValue("filename", tc.filename)
			w := httptest.NewRecorder()

			r.handleBackupDelete(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
			}
		})
	}
}

func TestHandleBackupDelete_NotFound(t *testing.T) {
	r, _ := testRouterWithBackup(t)

	filename := "stillwater-20260101-120000.db"
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/settings/backup/"+filename, nil)
	req.SetPathValue("filename", filename)
	w := httptest.NewRecorder()

	r.handleBackupDelete(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1572864, "1.5 MB"},
	}
	for _, tc := range cases {
		got := formatBytes(tc.input)
		if got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
