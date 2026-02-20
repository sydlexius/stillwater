package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/sydlexius/stillwater/internal/backup"
)

func (r *Router) handleBackupCreate(w http.ResponseWriter, req *http.Request) {
	info, err := r.backupService.Backup(req.Context())
	if err != nil {
		r.logger.Error("backup failed", "error", err)
		http.Error(w, `{"error":"backup failed"}`, http.StatusInternalServerError)
		return
	}

	if req.Header.Get("HX-Request") == "true" {
		// Return the updated backup list for HTMX swap
		backups, listErr := r.backupService.ListBackups()
		if listErr != nil {
			r.logger.Error("listing backups after create", "error", listErr)
			http.Error(w, `{"error":"listing backups failed"}`, http.StatusInternalServerError)
			return
		}
		r.renderBackupList(w, backups)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info) //nolint:errcheck
}

func (r *Router) handleBackupHistory(w http.ResponseWriter, req *http.Request) {
	backups, err := r.backupService.ListBackups()
	if err != nil {
		r.logger.Error("listing backups failed", "error", err)
		http.Error(w, `{"error":"listing backups failed"}`, http.StatusInternalServerError)
		return
	}

	if req.Header.Get("HX-Request") == "true" {
		r.renderBackupList(w, backups)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(backups) //nolint:errcheck
}

func (r *Router) handleBackupDownload(w http.ResponseWriter, req *http.Request) {
	filename := req.PathValue("filename")
	if !backup.IsValidBackupFilename(filename) {
		http.Error(w, `{"error":"invalid filename"}`, http.StatusBadRequest)
		return
	}

	path := filepath.Join(r.backupService.BackupDir(), filename)
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	http.ServeFile(w, req, path)
}

func (r *Router) renderBackupList(w http.ResponseWriter, backups []backup.BackupInfo) {
	w.Header().Set("Content-Type", "text/html")
	if len(backups) == 0 {
		w.Write([]byte(`<p class="text-sm text-gray-500 dark:text-gray-400 italic">No backups yet.</p>`)) //nolint:errcheck
		return
	}
	html := `<table class="w-full text-sm"><thead><tr class="text-left text-xs text-gray-500 dark:text-gray-400"><th class="py-2">Filename</th><th class="py-2">Size</th><th class="py-2">Date</th><th class="py-2"></th></tr></thead><tbody>`
	for _, b := range backups {
		html += fmt.Sprintf(
			`<tr class="border-t border-gray-200 dark:border-gray-700"><td class="py-2">%s</td><td class="py-2">%s</td><td class="py-2">%s</td><td class="py-2"><a href="%s/api/v1/settings/backup/%s" class="text-blue-600 dark:text-blue-400 hover:underline">Download</a></td></tr>`,
			b.Filename, formatBytes(b.Size), b.CreatedAt.Format("2006-01-02 15:04:05"), r.basePath, b.Filename,
		)
	}
	html += `</tbody></table>`
	w.Write([]byte(html)) //nolint:errcheck,gosec // G705: all values are from validated backup filenames, formatBytes, and time.Format
}

func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
	)
	switch {
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
