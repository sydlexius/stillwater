package api

import "net/http"

// handleScannerRun triggers a filesystem scan.
// POST /api/v1/scanner/run
func (r *Router) handleScannerRun(w http.ResponseWriter, req *http.Request) {
	if r.scannerService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "scanner not configured")
		return
	}

	result, err := r.scannerService.Run(req.Context())
	if err != nil {
		writeError(w, req, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, result)
}

// handleScannerStatus returns the current or most recent scan status.
// GET /api/v1/scanner/status
func (r *Router) handleScannerStatus(w http.ResponseWriter, req *http.Request) {
	if r.scannerService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scanner not configured"})
		return
	}

	status := r.scannerService.Status()
	if status == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "idle"})
		return
	}

	writeJSON(w, http.StatusOK, status)
}
