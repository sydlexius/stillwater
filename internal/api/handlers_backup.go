package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
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

	if backups == nil {
		backups = []backup.BackupInfo{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(backups) //nolint:errcheck
}

func (r *Router) handleBackupDelete(w http.ResponseWriter, req *http.Request) {
	filename := req.PathValue("filename")
	if !backup.IsValidBackupFilename(filename) {
		writeError(w, req, http.StatusBadRequest, "invalid filename")
		return
	}

	if err := r.backupService.Delete(filename); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, req, http.StatusNotFound, "backup not found")
			return
		}
		r.logger.Error("deleting backup", "filename", filename, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to delete backup")
		return
	}

	if req.Header.Get("HX-Request") == "true" {
		backups, listErr := r.backupService.ListBackups()
		if listErr != nil {
			r.logger.Error("listing backups after delete", "error", listErr)
			writeError(w, req, http.StatusInternalServerError, "listing backups failed")
			return
		}
		r.renderBackupList(w, backups)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (r *Router) handleBackupDownload(w http.ResponseWriter, req *http.Request) {
	filename := req.PathValue("filename")
	if !backup.IsValidBackupFilename(filename) {
		http.Error(w, `{"error":"invalid filename"}`, http.StatusBadRequest)
		return
	}

	path := filepath.Join(r.backupService.BackupDir(), filename)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, req, path)
}

func (r *Router) renderBackupList(w http.ResponseWriter, backups []backup.BackupInfo) {
	w.Header().Set("Content-Type", "text/html")
	if len(backups) == 0 {
		w.Write([]byte(`<p class="text-sm text-gray-500 dark:text-gray-400 italic">No backups yet.</p>`)) //nolint:errcheck
		return
	}
	out := `<table class="w-full text-sm"><thead><tr class="text-left text-xs text-gray-500 dark:text-gray-400"><th class="py-2">Filename</th><th class="py-2">Size</th><th class="py-2">Date</th><th class="py-2"></th></tr></thead><tbody>`
	for _, b := range backups {
		out += fmt.Sprintf(
			`<tr class="border-t border-gray-200 dark:border-gray-700">`+
				`<td class="py-2">%s</td>`+
				`<td class="py-2">%s</td>`+
				`<td class="py-2">%s</td>`+
				`<td class="py-2 text-right">`+
				`<a href="%s/api/v1/settings/backup/%s" class="text-blue-600 dark:text-blue-400 hover:underline mr-3">Download</a>`+
				`<button type="button" class="text-red-600 dark:text-red-400 hover:underline" hx-delete="%s/api/v1/settings/backup/%s" hx-target="#backup-list" hx-swap="innerHTML" hx-confirm="Delete backup %s?">Delete</button>`+
				`</td></tr>`,
			b.Filename, formatBytes(b.Size), b.CreatedAt.Format("2006-01-02 15:04:05"),
			html.EscapeString(r.basePath), b.Filename,
			html.EscapeString(r.basePath), b.Filename, b.Filename,
		)
	}
	out += `</tbody></table>`
	w.Write([]byte(out)) //nolint:errcheck,gosec // all values are from validated backup filenames, formatBytes, and time.Format
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
