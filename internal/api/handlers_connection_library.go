package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/collision"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/connection/lidarr"
	"github.com/sydlexius/stillwater/internal/dbutil"
	img "github.com/sydlexius/stillwater/internal/image"
	"github.com/sydlexius/stillwater/internal/library"
	"github.com/sydlexius/stillwater/web/templates"
)

// LibraryOpResult tracks the state of an async library operation.
type LibraryOpResult struct {
	LibraryID   string     `json:"library_id"`
	LibraryName string     `json:"library_name"`
	Operation   string     `json:"operation"`
	Status      string     `json:"status"`
	Message     string     `json:"message,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// discoveredLibrary represents a library found on a connected service.
type discoveredLibrary struct {
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Imported   bool   `json:"imported"`
}

// importRequest is the request body for importing libraries from a connection.
type importRequest struct {
	Libraries []struct {
		ExternalID string `json:"external_id"`
		Name       string `json:"name"`
	} `json:"libraries"`
}

// populateResult summarizes the outcome of populating artists from a connection.
type populateResult struct {
	Total   int `json:"total"`
	Created int `json:"created"`
	Skipped int `json:"skipped"`
	Images  int `json:"images"`
}

// imageDownloader can retrieve raw image bytes from a media platform.
type imageDownloader interface {
	GetArtistImage(ctx context.Context, artistID, imageType string) ([]byte, string, error)
	GetArtistBackdrop(ctx context.Context, artistID string, index int) ([]byte, string, error)
}

// handleDiscoverLibraries lists music libraries available on a connection.
// GET /api/v1/connections/{id}/libraries
func (r *Router) handleDiscoverLibraries(w http.ResponseWriter, req *http.Request) {
	connID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	conn, err := r.connectionService.GetByID(req.Context(), connID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection is disabled"})
		return
	}
	if conn.Status != "ok" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "connection must be tested successfully before discovering libraries",
		})
		return
	}

	var discovered []discoveredLibrary

	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
		folders, libErr := client.GetMusicLibraries(req.Context())
		if libErr != nil {
			r.logger.Error("discovering emby libraries", "error", libErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to discover libraries from " + conn.Type})
			return
		}
		for i := range folders {
			f := &folders[i]
			d := discoveredLibrary{ExternalID: f.ItemID, Name: f.Name}
			existing, lookupErr := r.libraryService.GetByConnectionAndExternalID(req.Context(), connID, f.ItemID)
			if lookupErr != nil {
				r.logger.Error("checking existing library", "external_id", f.ItemID, "error", lookupErr)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check existing library"})
				return
			}
			d.Imported = existing != nil
			discovered = append(discovered, d)
		}

	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
		folders, libErr := client.GetMusicLibraries(req.Context())
		if libErr != nil {
			r.logger.Error("discovering jellyfin libraries", "error", libErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to discover libraries from " + conn.Type})
			return
		}
		for i := range folders {
			f := &folders[i]
			d := discoveredLibrary{ExternalID: f.ItemID, Name: f.Name}
			existing, lookupErr := r.libraryService.GetByConnectionAndExternalID(req.Context(), connID, f.ItemID)
			if lookupErr != nil {
				r.logger.Error("checking existing library", "external_id", f.ItemID, "error", lookupErr)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to check existing library"})
				return
			}
			d.Imported = existing != nil
			discovered = append(discovered, d)
		}

	case connection.TypeLidarr:
		// Lidarr is a read-only metadata source (MBID seeding); Stillwater does
		// not import libraries from Lidarr connections, so return an empty list.

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported connection type"})
		return
	}

	if discovered == nil {
		discovered = []discoveredLibrary{}
	}

	// If HTMX request, render the checklist partial
	if isHTMXRequest(req) {
		templLibs := make([]templates.DiscoveredLib, len(discovered))
		for i, d := range discovered {
			templLibs[i] = templates.DiscoveredLib{
				ExternalID: d.ExternalID,
				Name:       d.Name,
				Imported:   d.Imported,
			}
		}
		isOOBE := strings.Contains(req.Header.Get("HX-Current-URL"), "/setup/wizard")
		renderTempl(w, req, templates.DiscoverResults(connID, templLibs, isOOBE))
		return
	}
	writeJSON(w, http.StatusOK, discovered)
}

// handleImportLibraries imports selected libraries from a connection.
// POST /api/v1/connections/{id}/libraries/import
func (r *Router) handleImportLibraries(w http.ResponseWriter, req *http.Request) {
	connID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	conn, err := r.connectionService.GetByID(req.Context(), connID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection is disabled"})
		return
	}
	if conn.Status != "ok" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "connection must be tested successfully before importing libraries",
		})
		return
	}

	var body importRequest
	if !DecodeJSON(w, req, &body) {
		return
	}
	if len(body.Libraries) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no libraries selected"})
		return
	}

	var created []library.Library
	for _, entry := range body.Libraries {
		// Skip entries with missing required fields
		if entry.ExternalID == "" || entry.Name == "" {
			continue
		}

		// Skip if already imported
		existing, lookupErr := r.libraryService.GetByConnectionAndExternalID(req.Context(), connID, entry.ExternalID)
		if lookupErr != nil {
			r.logger.Error("checking existing library", "external_id", entry.ExternalID, "error", lookupErr)
			continue
		}
		if existing != nil {
			continue
		}

		name := entry.Name
		// Check for name conflict and suffix with connection name if needed
		lib := &library.Library{
			Name:         name,
			Path:         "",
			Type:         library.TypeRegular,
			Source:       conn.Type,
			ConnectionID: conn.ID,
			ExternalID:   entry.ExternalID,
		}
		if createErr := r.libraryService.Create(req.Context(), lib); createErr != nil {
			// If name conflict (unique constraint), retry with connection name suffix
			lower := strings.ToLower(createErr.Error())
			if strings.Contains(lower, "unique") || strings.Contains(lower, "duplicate") {
				lib.Name = fmt.Sprintf("%s (%s)", name, conn.Name)
				if retryErr := r.libraryService.Create(req.Context(), lib); retryErr != nil {
					r.logger.Error("importing library", "name", lib.Name, "error", retryErr)
					continue
				}
			} else {
				r.logger.Error("importing library", "name", lib.Name, "error", createErr)
				continue
			}
		}
		created = append(created, *lib)
	}

	writeJSON(w, http.StatusCreated, created)

	// Auto-populate each newly imported library in the background.
	for i := range created {
		lib := created[i]
		r.startPopulateBackground(context.WithoutCancel(req.Context()), conn, &lib)
	}
}

// startPopulateBackground registers a library populate operation and runs it
// in a background goroutine. Returns immediately if an operation is already
// running for this library. Use this for fire-and-forget populate triggers
// where no HTTP response for the operation status is needed at call time.
func (r *Router) startPopulateBackground(ctx context.Context, conn *connection.Connection, lib *library.Library) {
	r.libraryOpsMu.Lock()
	if existing, ok := r.libraryOps[lib.ID]; ok && existing.Status == "running" {
		r.libraryOpsMu.Unlock()
		return
	}
	op := &LibraryOpResult{
		LibraryID:   lib.ID,
		LibraryName: lib.Name,
		Operation:   "populate",
		Status:      "running",
		StartedAt:   time.Now().UTC(),
	}
	r.libraryOps[lib.ID] = op
	r.libraryOpsMu.Unlock()

	go r.runPopulate(ctx, conn, lib, op)
}

// handlePopulateLibrary populates artists from a connection into an imported library.
// POST /api/v1/connections/{id}/libraries/{libId}/populate
func (r *Router) handlePopulateLibrary(w http.ResponseWriter, req *http.Request) {
	connID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	libID, ok := RequirePathParam(w, req, "libId")
	if !ok {
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), connID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection is disabled"})
		return
	}
	if conn.Status != "ok" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "connection must be tested successfully before populating libraries",
		})
		return
	}

	lib, err := r.libraryService.GetByID(req.Context(), libID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "library not found"})
		return
	}
	if lib.ConnectionID != conn.ID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "library does not belong to this connection"})
		return
	}

	// Check for already-running operation on this library.
	r.libraryOpsMu.Lock()
	if existing, ok := r.libraryOps[libID]; ok && existing.Status == "running" {
		r.libraryOpsMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "operation already running for this library"})
		return
	}
	op := &LibraryOpResult{
		LibraryID:   libID,
		LibraryName: lib.Name,
		Operation:   "populate",
		Status:      "running",
		StartedAt:   time.Now().UTC(),
	}
	r.libraryOps[libID] = op
	r.libraryOpsMu.Unlock()

	writeJSON(w, http.StatusAccepted, op)

	go r.runPopulate(context.WithoutCancel(req.Context()), conn, lib, op)
}

// populateOpID returns the stable ProgressPill op_id for a library populate.
// One pill per library lets the user kick off multiple populates without
// them collapsing into a single bar; the prefix lets the JS distinguish
// populate pills from bulk-action pills without parsing the label.
func populateOpID(libID string) string {
	return "populate:" + libID
}

// runPopulate executes the populate operation in a background goroutine.
//
// Emits ProgressPill events on the shared SSE topic (op_id="populate:<libID>")
// so the user gets live feedback in the layout-level pill stack. Emby and
// Jellyfin per-page loops update the running pill via publishPopulateProgress
// once they know the total record count from the first paginated response;
// Lidarr populate does not paginate today (single GetArtists call) so it
// only emits start + terminal events. Cancel is out of scope for #1216
// per the issue, so events carry no cancel_url.
func (r *Router) runPopulate(ctx context.Context, conn *connection.Connection, lib *library.Library, op *LibraryOpResult) {
	opID := populateOpID(lib.ID)
	pillLabel := "populate: " + lib.Name
	// Emit a "running" event up-front (total=0 means indeterminate; the
	// first paginated response will publish a concrete total). The pill
	// appears immediately so the user knows the kickoff registered.
	r.publishOpProgress(opID, pillLabel, 0, 0, "running", "")

	defer func() {
		if v := recover(); v != nil {
			r.logger.Error("panic in populate goroutine",
				slog.String("library", lib.Name), slog.String("library_id", lib.ID),
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())))
			r.libraryOpsMu.Lock()
			now := time.Now().UTC()
			op.CompletedAt = &now
			op.Status = "failed"
			op.Message = "populate failed unexpectedly"
			r.libraryOpsMu.Unlock()
			// Surface the panic on the pill so the user is not left
			// staring at a stuck "running" bar.
			r.publishOpProgress(opID, pillLabel, 0, 0, "failed", "")
			go r.scheduleOpCleanup(lib.ID, op)
		}
	}()

	result := populateResult{}
	var popErr error

	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
		popErr = r.populateFromEmbyCtx(ctx, client, lib, &result)

	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
		popErr = r.populateFromJellyfinCtx(ctx, client, lib, &result)

	case connection.TypeLidarr:
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		popErr = r.populateFromLidarrCtx(ctx, client, lib, &result)

	default:
		popErr = fmt.Errorf("unsupported connection type: %s", conn.Type)
	}

	// After a successful sync, check for external file modifications (Tier 2
	// shared-FS evidence). This runs outside the per-artist loop to avoid
	// repeated database updates during sync.
	if popErr == nil && !lib.IsPathless() {
		r.checkSyncMtimeEvidence(ctx, lib)
	}

	r.libraryOpsMu.Lock()
	now := time.Now().UTC()
	op.CompletedAt = &now
	if popErr != nil {
		op.Status = "failed"
		op.Message = fmt.Sprintf("populate failed for %s", lib.Name)
		r.logger.Error("populate failed", "library", lib.Name, "error", popErr)
	} else {
		op.Status = "completed"
		op.Message = fmt.Sprintf("Populated %d artists (%d images) from %s", result.Created, result.Images, lib.Name)
	}
	r.libraryOpsMu.Unlock()

	// Terminal pill event. The pill auto-dismisses on "completed" and
	// stays sticky on "failed" until the user dismisses it. processed
	// is set equal to total on success so the bar shows full.
	pillStatus := "completed"
	if popErr != nil {
		pillStatus = "failed"
	}
	r.publishOpProgress(opID, pillLabel, result.Total, result.Total, pillStatus, "")

	go r.scheduleOpCleanup(lib.ID, op)
}

// publishPopulateProgress is the per-page progress hook called by
// populateFromEmbyCtx / populateFromJellyfinCtx between page fetches.
// Throttling lives here so the Emby/Jellyfin loops do not each carry
// their own copy. Updates are emitted every 5% of total or when the
// final page lands; for small libraries (<20 records) every page is
// emitted because the step rounds down to 1.
func (r *Router) publishPopulateProgress(lib *library.Library, processed, total int) {
	if total <= 0 {
		// Indeterminate run: emit each page so the pill shows movement.
		r.publishOpProgress(populateOpID(lib.ID), "populate: "+lib.Name, 0, processed, "running", "")
		return
	}
	step := total / 20
	if step < 1 {
		step = 1
	}
	if processed%step == 0 || processed >= total {
		r.publishOpProgress(populateOpID(lib.ID), "populate: "+lib.Name, total, processed, "running", "")
	}
}

// handleScanLibrary triggers an async API scan that checks metadata/image state.
// POST /api/v1/connections/{id}/libraries/{libId}/scan
func (r *Router) handleScanLibrary(w http.ResponseWriter, req *http.Request) {
	connID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}
	libID, ok := RequirePathParam(w, req, "libId")
	if !ok {
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), connID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection is disabled"})
		return
	}
	if conn.Status != "ok" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "connection must be tested successfully before scanning",
		})
		return
	}

	lib, err := r.libraryService.GetByID(req.Context(), libID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "library not found"})
		return
	}
	if lib.ConnectionID != conn.ID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "library does not belong to this connection"})
		return
	}

	r.libraryOpsMu.Lock()
	if existing, ok := r.libraryOps[libID]; ok && existing.Status == "running" {
		r.libraryOpsMu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "operation already running for this library"})
		return
	}
	op := &LibraryOpResult{
		LibraryID:   libID,
		LibraryName: lib.Name,
		Operation:   "scan",
		Status:      "running",
		StartedAt:   time.Now().UTC(),
	}
	r.libraryOps[libID] = op
	r.libraryOpsMu.Unlock()

	writeJSON(w, http.StatusAccepted, op)

	go r.runLibraryScan(context.WithoutCancel(req.Context()), conn, lib, op)
}

// runLibraryScan queries the platform API and resolves its artists to local
// artist rows, storing the platform ID mapping for each match. It does not
// write local image-existence state; see scanFromEmby (#2637).
func (r *Router) runLibraryScan(ctx context.Context, conn *connection.Connection, lib *library.Library, op *LibraryOpResult) {
	defer func() {
		if v := recover(); v != nil {
			r.logger.Error("panic in library scan goroutine",
				slog.String("library", lib.Name), slog.String("library_id", lib.ID),
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())))
			r.libraryOpsMu.Lock()
			now := time.Now().UTC()
			op.CompletedAt = &now
			op.Status = "failed"
			op.Message = "scan failed unexpectedly"
			r.libraryOpsMu.Unlock()
			go r.scheduleOpCleanup(lib.ID, op)
		}
	}()

	var matched int
	var scanErr error

	switch conn.Type {
	case connection.TypeEmby:
		client := emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
		matched, scanErr = r.scanFromEmby(ctx, client, lib)

	case connection.TypeJellyfin:
		client := jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger)
		matched, scanErr = r.scanFromJellyfin(ctx, client, lib)

	case connection.TypeLidarr:
		client := lidarr.New(conn.URL, conn.APIKey, r.logger)
		matched, scanErr = r.scanFromLidarr(ctx, client, lib)

	default:
		scanErr = fmt.Errorf("unsupported connection type: %s", conn.Type)
	}

	r.libraryOpsMu.Lock()
	now := time.Now().UTC()
	op.CompletedAt = &now
	if scanErr != nil {
		op.Status = "failed"
		op.Message = fmt.Sprintf("scan failed for %s", lib.Name)
		r.logger.Error("library scan failed", "library", lib.Name, "error", scanErr)
	} else {
		op.Status = "completed"
		op.Message = fmt.Sprintf("Scan complete: %d artists matched in %s", matched, lib.Name)
	}
	r.libraryOpsMu.Unlock()

	go r.scheduleOpCleanup(lib.ID, op)
}

// scheduleOpCleanup removes a completed or failed operation from the in-memory
// map after a delay, preventing unbounded growth of the libraryOps map.
func (r *Router) scheduleOpCleanup(libraryID string, op *LibraryOpResult) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	<-timer.C

	r.libraryOpsMu.Lock()
	defer r.libraryOpsMu.Unlock()
	current, ok := r.libraryOps[libraryID]
	if !ok {
		return
	}
	// Only delete if this is still the same operation and it is no longer running.
	if current == op && current.Status != "running" {
		delete(r.libraryOps, libraryID)
	}
}

// handleLibraryOpStatus returns the current operation status for a library.
// GET /api/v1/libraries/{libId}/operation/status
func (r *Router) handleLibraryOpStatus(w http.ResponseWriter, req *http.Request) {
	libID, ok := RequirePathParam(w, req, "libId")
	if !ok {
		return
	}

	r.libraryOpsMu.Lock()
	op, ok := r.libraryOps[libID]
	if !ok {
		r.libraryOpsMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "idle"})
		return
	}
	snapshot := *op
	r.libraryOpsMu.Unlock()

	writeJSON(w, http.StatusOK, snapshot)
}

// handlePopulateInFlight returns the set of populate operations currently
// in the "running" state, in the ProgressPill event-envelope shape so the
// JS reconnect-rehydrate path (#1641) can replay them through
// window.swProgressPill.push without further translation.
//
// Per-library populate is the only multi-instance op in the system
// (bulk-actions is a singleton; bulk-lock is a bulk-action subtype) so a
// single aggregate endpoint covers the reconnect-rehydrate need without
// adding {libId}-scoped status fan-out from the client.
//
// GET /api/v1/connections/populate/in-flight
func (r *Router) handlePopulateInFlight(w http.ResponseWriter, _ *http.Request) {
	type opEnvelope struct {
		OpID      string `json:"op_id"`
		Label     string `json:"label"`
		Processed int    `json:"processed"`
		Total     int    `json:"total"`
		Status    string `json:"status"`
	}
	out := struct {
		Operations []opEnvelope `json:"operations"`
	}{Operations: []opEnvelope{}}

	r.libraryOpsMu.Lock()
	for _, op := range r.libraryOps {
		if op == nil || op.Status != "running" || op.Operation != "populate" {
			continue
		}
		out.Operations = append(out.Operations, opEnvelope{
			OpID:      populateOpID(op.LibraryID),
			Label:     "populate: " + op.LibraryName,
			Processed: 0,
			Total:     0,
			Status:    "running",
		})
	}
	r.libraryOpsMu.Unlock()

	writeJSON(w, http.StatusOK, out)
}

// dedupeForImport finds an existing artist that an inbound platform item
// (Emby / Jellyfin / Lidarr) should attach to, scanning across ALL
// libraries. Returns:
//   - (existing, false): caller attaches platform mapping + library
//     membership and skips creation
//   - (nil, false): caller creates a new artist row
//   - (nil, true): a lookup error or MBID conflict was logged; caller
//     must increment Skipped and move to the next item
//
// Replaces the previous per-library-scoped GetByMBIDAndLibrary +
// GetByNameAndLibrary lookups, which produced duplicate artist rows when
// the same real-world artist was imported from multiple libraries (the
// classic Emby + Jellyfin against the same /music topology).
func (r *Router) dedupeForImport(
	ctx context.Context,
	mbid, name, source string,
	result *populateResult,
) (*artist.Artist, bool) {
	if mbid == "" && name == "" {
		return nil, false
	}
	a, err := r.artistService.FindByMBIDOrNameUnscoped(ctx, mbid, name)
	if err != nil {
		r.logger.Warn("dedup lookup",
			"mbid", mbid, "name", name, "platform", source, "error", err)
		result.Skipped++
		return nil, true
	}
	if a == nil {
		return nil, false
	}
	if mbid != "" && a.MusicBrainzID != "" && a.MusicBrainzID != mbid {
		// Name collision with conflicting MBIDs: two different artists
		// share a name. Skip to avoid wrong association.
		r.logger.Warn("mbid conflict during dedup, skipping",
			"name", name, "platform", source,
			"platform_mbid", mbid, "existing_mbid", a.MusicBrainzID)
		result.Skipped++
		return nil, true
	}
	return a, false
}

//nolint:gocognit // Paginated Emby fetch with per-artist upsert, conflict-with-manual-libraries detection, image-presence cache updates, and disambiguation when external MBID differs from local; the paginate loop's continuation conditions and per-artist outcome accounting must remain in sequence.
func (r *Router) populateFromEmbyCtx(ctx context.Context, client *emby.Client, lib *library.Library, result *populateResult) error {
	manualLibs := r.manualLibraries(ctx)
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(ctx, lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return fmt.Errorf("fetching artists from emby: %w", err)
		}
		// Per-page ProgressPill tick. resp.TotalRecordCount is reliable
		// after the first response so we always pass it as the total;
		// publishPopulateProgress handles throttling.
		r.publishPopulateProgress(lib, result.Total, resp.TotalRecordCount)

		for i := range resp.Items {
			item := &resp.Items[i]
			result.Total++
			mbid := item.ProviderIDs.MusicBrainzArtist

			existing, skip := r.dedupeForImport(ctx, mbid, item.Name, "emby", result)
			if skip {
				continue
			}

			if existing != nil {
				// Backfill MusicBrainzID if the platform provides one and the local record lacks it.
				if mbid != "" && existing.MusicBrainzID == "" {
					existing.MusicBrainzID = mbid
					if err := r.artistService.Update(ctx, existing); err != nil {
						r.logger.Warn("backfilling mbid from emby", "name", existing.Name, "error", err)
					}
				}
				// Store the platform-to-Stillwater artist ID mapping. Populate is
				// a non-authoritative writer, so use the divergence-aware stable
				// set to keep the deterministic id across re-imports rather than
				// clobber a duplicate twin (#2344).
				if outcome, setErr := r.artistService.SetPlatformIDStable(ctx, existing.ID, lib.ConnectionID, item.ID); setErr != nil {
					r.logger.Warn("storing emby platform id", "name", existing.Name, "error", setErr)
				} else {
					r.logPlatformIDDivergence(outcome, existing.Name, "emby", item.ID)
				}
				// record that this Emby library now also
				// observes the existing artist (filesystem-imported or
				// Jellyfin-imported, etc.). Idempotent.
				if memErr := r.artistService.AddLibraryMembership(ctx, existing.ID, lib.ID, "emby"); memErr != nil {
					r.logger.Warn("adding emby library membership", "name", existing.Name, "error", memErr)
				}
				r.backfillPlatformIDToManualLibs(ctx, mbid, item.Name, lib.ConnectionID, item.ID, existing.ID, manualLibs)
				// Download any missing images.
				r.downloadPlatformImages(ctx, client, item.ID, item.ImageTags, item.BackdropImageTags, existing, "emby", result)
				result.Skipped++
				continue
			}

			sortName := item.Name
			if item.SortName != "" {
				sortName = item.SortName
			}
			a := &artist.Artist{
				Name:          item.Name,
				SortName:      sortName,
				MusicBrainzID: mbid,
				LibraryID:     lib.ID,
				Biography:     item.Overview,
				Genres:        item.Genres,
				Styles:        item.Tags,
				Formed:        item.PremiereDate,
				Disbanded:     item.EndDate,
				Path:          validatedArtistPath(item.Path, lib.Path),
			}
			if err := r.artistService.Create(ctx, a); err != nil {
				r.logger.Warn("creating artist from emby", "name", item.Name, "error", err)
				result.Skipped++
				continue
			}
			result.Created++

			// Store the platform-to-Stillwater artist ID mapping.
			// Initial artist_libraries membership is recorded by
			// artist.Service.Create via AddDerivingSource. Stable set keeps the
			// deterministic id across re-imports (#2344).
			if outcome, setErr := r.artistService.SetPlatformIDStable(ctx, a.ID, lib.ConnectionID, item.ID); setErr != nil {
				r.logger.Warn("storing emby platform id", "name", a.Name, "error", setErr)
			} else {
				r.logPlatformIDDivergence(outcome, a.Name, "emby", item.ID)
			}
			r.backfillPlatformIDToManualLibs(ctx, mbid, item.Name, lib.ConnectionID, item.ID, a.ID, manualLibs)

			r.downloadPlatformImages(ctx, client, item.ID, item.ImageTags, item.BackdropImageTags, a, "emby", result)
		}

		// Per-page tick after the item loop so processed reflects the
		// items just absorbed in this page (not the lagging top-of-loop
		// count). publishPopulateProgress throttles to ~5% steps.
		r.publishPopulateProgress(lib, result.Total, resp.TotalRecordCount)

		startIndex += pageSize
		if startIndex >= resp.TotalRecordCount {
			break
		}
	}
	return nil
}

//nolint:gocognit // Paginated Jellyfin fetch with the same multi-state per-artist accounting as the Emby variant (upsert, conflict-with-manual, image cache, disambiguation); the Jellyfin response schema differs from Emby's so the two cannot share a code path without an adapter abstraction that would itself raise complexity.
func (r *Router) populateFromJellyfinCtx(ctx context.Context, client *jellyfin.Client, lib *library.Library, result *populateResult) error {
	manualLibs := r.manualLibraries(ctx)
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(ctx, lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return fmt.Errorf("fetching artists from jellyfin: %w", err)
		}
		// Per-page ProgressPill tick. See the Emby variant above.
		r.publishPopulateProgress(lib, result.Total, resp.TotalRecordCount)

		for i := range resp.Items {
			item := &resp.Items[i]
			result.Total++
			mbid := item.ProviderIDs.MusicBrainzArtist

			existing, skip := r.dedupeForImport(ctx, mbid, item.Name, "jellyfin", result)
			if skip {
				continue
			}

			if existing != nil {
				// Backfill MusicBrainzID if the platform provides one and the local record lacks it.
				if mbid != "" && existing.MusicBrainzID == "" {
					existing.MusicBrainzID = mbid
					if err := r.artistService.Update(ctx, existing); err != nil {
						r.logger.Warn("backfilling mbid from jellyfin", "name", existing.Name, "error", err)
					}
				}
				// Store the platform-to-Stillwater artist ID mapping via the
				// divergence-aware stable set (#2344).
				if outcome, setErr := r.artistService.SetPlatformIDStable(ctx, existing.ID, lib.ConnectionID, item.ID); setErr != nil {
					r.logger.Warn("storing jellyfin platform id", "name", existing.Name, "error", setErr)
				} else {
					r.logPlatformIDDivergence(outcome, existing.Name, "jellyfin", item.ID)
				}
				// record that this Jellyfin library now also
				// observes the existing artist. Idempotent.
				if memErr := r.artistService.AddLibraryMembership(ctx, existing.ID, lib.ID, "jellyfin"); memErr != nil {
					r.logger.Warn("adding jellyfin library membership", "name", existing.Name, "error", memErr)
				}
				r.backfillPlatformIDToManualLibs(ctx, mbid, item.Name, lib.ConnectionID, item.ID, existing.ID, manualLibs)
				// Download any missing images.
				r.downloadPlatformImages(ctx, client, item.ID, item.ImageTags, item.BackdropImageTags, existing, "jellyfin", result)
				result.Skipped++
				continue
			}

			sortName := item.Name
			if item.SortName != "" {
				sortName = item.SortName
			}
			a := &artist.Artist{
				Name:          item.Name,
				SortName:      sortName,
				MusicBrainzID: mbid,
				LibraryID:     lib.ID,
				Biography:     item.Overview,
				Genres:        item.Genres,
				Styles:        item.Tags,
				Formed:        item.PremiereDate,
				Disbanded:     item.EndDate,
				Path:          validatedArtistPath(item.Path, lib.Path),
			}
			if err := r.artistService.Create(ctx, a); err != nil {
				r.logger.Warn("creating artist from jellyfin", "name", item.Name, "error", err)
				result.Skipped++
				continue
			}
			result.Created++

			// Store the platform-to-Stillwater artist ID mapping.
			// Initial artist_libraries membership is recorded by
			// artist.Service.Create via AddDerivingSource. Stable set keeps the
			// deterministic id across re-imports (#2344).
			if outcome, setErr := r.artistService.SetPlatformIDStable(ctx, a.ID, lib.ConnectionID, item.ID); setErr != nil {
				r.logger.Warn("storing jellyfin platform id", "name", a.Name, "error", setErr)
			} else {
				r.logPlatformIDDivergence(outcome, a.Name, "jellyfin", item.ID)
			}
			r.backfillPlatformIDToManualLibs(ctx, mbid, item.Name, lib.ConnectionID, item.ID, a.ID, manualLibs)

			r.downloadPlatformImages(ctx, client, item.ID, item.ImageTags, item.BackdropImageTags, a, "jellyfin", result)
		}

		// Per-page tick after the item loop (see Emby variant for rationale).
		r.publishPopulateProgress(lib, result.Total, resp.TotalRecordCount)

		startIndex += pageSize
		if startIndex >= resp.TotalRecordCount {
			break
		}
	}
	return nil
}

// TODO(#1216 follow-up): Lidarr populate also needs ProgressPill ticks.
// It does not paginate today (single GetArtists call), so wiring it
// requires either splitting the inner loop into chunks or accepting a
// single mid-loop tick. Out of scope for this PR per the issue ("Lidarr
// almost certainly does too but has not been validated").
func (r *Router) populateFromLidarrCtx(ctx context.Context, client *lidarr.Client, lib *library.Library, result *populateResult) error {
	manualLibs := r.manualLibraries(ctx)
	artists, err := client.GetArtists(ctx)
	if err != nil {
		return fmt.Errorf("fetching artists from lidarr: %w", err)
	}

	for _, la := range artists {
		result.Total++
		mbid := la.ForeignArtistID

		existing, skip := r.dedupeForImport(ctx, mbid, la.ArtistName, "lidarr", result)
		if skip {
			continue
		}

		if existing != nil {
			pid := fmt.Sprintf("%d", la.ID)
			// Stable set keeps the deterministic id across re-imports (#2344).
			if outcome, setErr := r.artistService.SetPlatformIDStable(ctx, existing.ID, lib.ConnectionID, pid); setErr != nil {
				r.logger.Warn("storing lidarr platform id", "name", existing.Name, "error", setErr)
			} else {
				r.logPlatformIDDivergence(outcome, existing.Name, "lidarr", pid)
			}
			// record this Lidarr library's membership.
			if memErr := r.artistService.AddLibraryMembership(ctx, existing.ID, lib.ID, "lidarr"); memErr != nil {
				r.logger.Warn("adding lidarr library membership", "name", existing.Name, "error", memErr)
			}
			r.backfillPlatformIDToManualLibs(ctx, mbid, la.ArtistName, lib.ConnectionID, fmt.Sprintf("%d", la.ID), existing.ID, manualLibs)
			result.Skipped++
			continue
		}

		a := &artist.Artist{
			Name:          la.ArtistName,
			SortName:      la.ArtistName,
			MusicBrainzID: mbid,
			LibraryID:     lib.ID,
		}
		if err := r.artistService.Create(ctx, a); err != nil {
			r.logger.Warn("creating artist from lidarr", "name", la.ArtistName, "error", err)
			result.Skipped++
			continue
		}
		result.Created++

		// Initial artist_libraries membership is recorded by
		// artist.Service.Create via AddDerivingSource. Stable set keeps the
		// deterministic id across re-imports (#2344).
		pid := fmt.Sprintf("%d", la.ID)
		if outcome, setErr := r.artistService.SetPlatformIDStable(ctx, a.ID, lib.ConnectionID, pid); setErr != nil {
			r.logger.Warn("storing lidarr platform id", "name", a.Name, "error", setErr)
		} else {
			r.logPlatformIDDivergence(outcome, a.Name, "lidarr", pid)
		}
		r.backfillPlatformIDToManualLibs(ctx, mbid, la.ArtistName, lib.ConnectionID, pid, a.ID, manualLibs)
	}
	return nil
}

// validatedArtistPath returns the resolved item path only when it exists on
// disk as a directory and falls under libraryPath. Returns empty string if
// libraryPath is empty (pathless library), itemPath is empty, itemPath does
// not exist, or itemPath escapes the library root. Resolves symlinks to
// prevent escaping the library root via symlinked directories.
func validatedArtistPath(itemPath, libraryPath string) string {
	if libraryPath == "" || itemPath == "" {
		return ""
	}

	// Resolve symlinks for the library root when possible.
	libRoot, err := filepath.EvalSymlinks(libraryPath)
	if err != nil {
		// Path may not exist on disk (e.g. in tests or misconfig); fall back.
		libRoot, err = filepath.Abs(libraryPath)
		if err != nil {
			return ""
		}
	}

	// Resolve symlinks for the item path when possible.
	itemReal, err := filepath.EvalSymlinks(itemPath)
	if err != nil {
		// Path does not exist on disk or cannot be resolved; treat as
		// invalid so only verified, existing directories are persisted as
		// artist paths. Pathless artists can still use the image cache.
		return ""
	} else {
		// Path exists: verify it is a directory (not a file).
		info, statErr := os.Stat(itemReal)
		if statErr != nil || !info.IsDir() {
			return ""
		}
	}

	rel, err := filepath.Rel(libRoot, itemReal)
	if err != nil {
		return ""
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return itemReal
}

// platformToStillwaterType maps Emby/Jellyfin ImageTags keys to Stillwater image types.
// Backdrops are excluded: they are returned in BackdropImageTags (not ImageTags) and
// downloaded separately via the indexed GetArtistBackdrop path.
var platformToStillwaterType = map[string]string{
	"Primary": "thumb",
	"Logo":    "logo",
	"Banner":  "banner",
}

// platformImagePipeline encapsulates the download context for a single
// artist's platform-image sync: where images land on disk, how to reach the
// platform, and the provenance (connType) + result accounting to record
// against every downloaded image.
type platformImagePipeline struct {
	r                *Router
	dl               imageDownloader
	dir              string
	platformArtistID string
	artist           *artist.Artist
	connType         string
	result           *populateResult
}

// newPlatformImagePipeline validates the artist's image directory and
// returns a pipeline for downloading platform images. ok is false when the
// download should be skipped entirely (no usable path/cache dir, or the
// filesystem path isn't ready); the caller should return without downloading.
func newPlatformImagePipeline(r *Router, dl imageDownloader, platformArtistID string, a *artist.Artist, connType string, result *populateResult) (p *platformImagePipeline, ok bool) {
	dir := r.imageDir(a)
	if dir == "" {
		r.logger.Debug("skipping image download: no path or cache dir", "artist", a.Name)
		return nil, false
	}

	if a.Path == "" {
		// Cache directory: create if needed.
		if err := os.MkdirAll(dir, 0o750); err != nil {
			r.logger.Warn("creating cache directory", "artist", a.Name, "dir", dir, "error", err)
			return nil, false
		}
	} else {
		// Filesystem path: must already exist from scan and be a directory.
		info, err := os.Stat(dir)
		if err != nil {
			r.logger.Debug("artist directory not accessible, skipping images", "artist", a.Name, "dir", dir, "error", err)
			return nil, false
		}
		if !info.IsDir() {
			r.logger.Debug("artist path is not a directory, skipping images", "artist", a.Name, "dir", dir)
			return nil, false
		}
	}

	return &platformImagePipeline{
		r:                r,
		dl:               dl,
		dir:              dir,
		platformArtistID: platformArtistID,
		artist:           a,
		connType:         connType,
		result:           result,
	}, true
}

// downloadNamedImages downloads each present platform image tag that maps to
// a known Stillwater image type. Errors are non-fatal: logged and skipped.
func (p *platformImagePipeline) downloadNamedImages(ctx context.Context, imageTags map[string]string) {
	for platformKey, tagValue := range imageTags {
		if tagValue == "" {
			continue
		}
		stillwaterType, ok := platformToStillwaterType[platformKey]
		if !ok {
			continue
		}
		p.downloadNamedImage(ctx, stillwaterType)
	}
}

// downloadNamedImage downloads, saves, and records provenance for a single
// named image type (e.g. "thumb", "logo"), skipping if it already exists.
func (p *platformImagePipeline) downloadNamedImage(ctx context.Context, stillwaterType string) {
	r, a := p.r, p.artist

	patterns := r.getActiveNamingConfig(ctx, stillwaterType)
	if _, found := img.FindExistingImage(p.dir, patterns); found {
		r.logger.Debug("skipping existing image", "artist", a.Name, "type", stillwaterType)
		return
	}

	data, _, err := p.dl.GetArtistImage(ctx, p.platformArtistID, stillwaterType)
	if err != nil {
		r.logger.Warn("downloading image from platform", "artist", a.Name, "type", stillwaterType, "error", err)
		return
	}

	meta := &img.ExifMeta{Source: p.connType, Fetched: time.Now().UTC(), Mode: "user"}
	// nil collision scope: this path handles NAMED image types only (thumb, logo,
	// banner -- see platformToStillwaterType), never fanart, so #2565's
	// fanart-gated check would be a no-op here anyway. Backdrops arriving from a
	// platform go through downloadBackdrop, which #2613 already wired.
	if _, err := r.processAndSaveImage(ctx, nil, p.dir, stillwaterType, data, meta); err != nil {
		r.logger.Warn("saving downloaded image", "artist", a.Name, "type", stillwaterType, "error", err)
		return
	}

	r.updateArtistImageFlag(ctx, a, stillwaterType)
	p.result.Images++
}

// backdropOutcome reports what happened to a single backdrop slot so the
// caller can decide whether a post-download fanart compaction is needed.
type backdropOutcome int

const (
	backdropSkipped backdropOutcome = iota
	backdropExisted
	backdropDownloaded
	// backdropDuplicate: the image was downloaded but its content matches a
	// fanart image the artist already holds in another slot, so it was NOT
	// written. This is the #2540 guard against spraying the same picture into
	// many slots.
	backdropDuplicate
)

// backdropDupPhashThreshold is the maximum Hamming distance between two 64-bit
// perceptual (dHash) hashes for them to count as the same image. Sprayed
// duplicates are byte-identical (distance 0) and are caught by the content
// hash; this tiny threshold additionally catches a re-encoded or rescaled copy
// of the same picture (a few bits differ). It stays far below the ~20+ distance
// observed between genuinely distinct backdrops on real libraries, so distinct
// art is never collapsed.
const backdropDupPhashThreshold = 2

// backdropDedup tracks the content-hash and perceptual-hash of every fanart
// image the artist already holds -- the files on disk when the run starts, plus
// each one saved during the run. It is the #2540 fix: the mirror must skip a
// downloaded backdrop whose image is already present in another slot instead of
// writing it to a fresh fanart{N} slot. Emby's historical fetcher listed the
// same picture under many BackdropImageTags, and the old filename-only slot
// check wrote each one out, piling the same image into dozens of slots.
type backdropDedup struct {
	content map[string]struct{}
	phashes []uint64
}

// newBackdropDedup seeds a tracker from the artist's existing on-disk fanart so
// a duplicate of an already-present image is skipped. A discovery or hashing
// error degrades to in-run dedup only (still prevents spraying within this
// populate) rather than failing the download.
func newBackdropDedup(dir, primary string, log *slog.Logger) *backdropDedup {
	d := &backdropDedup{content: map[string]struct{}{}}
	paths, err := img.DiscoverFanart(dir, primary)
	if err != nil {
		log.Warn("discovering existing fanart for dedup; proceeding with in-run dedup only",
			slog.String("dir", dir), slog.String("error", err.Error()))
		return d
	}
	for _, p := range paths {
		h, herr := img.HashFile(p, true)
		if herr != nil {
			// A file we cannot hash cannot be deduped against; log and skip it
			// rather than fail the whole populate. Worst case a duplicate slips
			// through, which the remediation pass (#2540 PR-2) still collapses.
			log.Warn("hashing existing fanart for dedup; skipping this file",
				slog.String("path", p), slog.String("error", herr.Error()))
			continue
		}
		d.add(h.Content, h.Perceptual)
	}
	return d
}

// isDuplicate reports whether an image with the given hashes is already held.
func (d *backdropDedup) isDuplicate(content string, phash uint64) bool {
	if _, ok := d.content[content]; ok {
		return true
	}
	if phash == 0 {
		// A zero dHash is degenerate (a solid-color or blank image resamples to
		// a uniform grid): many unrelated blanks share it, so it must not be
		// used as a similarity signal. Content-hash above is the only safe
		// check for these -- which means a solid-color backdrop re-downloaded
		// in a SEPARATE run (where content-hash cannot match the EXIF-stamped
		// on-disk copy) may not dedup. This is a rare, bounded edge (music
		// backdrops are almost never solid color); the #2540 remediation pass
		// (PR-2) collapses any that slip through.
		return false
	}
	for _, existing := range d.phashes {
		if existing != 0 && img.HammingDistance(existing, phash) <= backdropDupPhashThreshold {
			return true
		}
	}
	return false
}

// add records an image's hashes so later slots in the same run dedup against it.
func (d *backdropDedup) add(content string, phash uint64) {
	d.content[content] = struct{}{}
	d.phashes = append(d.phashes, phash)
}

// downloadBackdrops downloads missing backdrop images for the given tags and,
// if anything was downloaded or already present, compacts the fanart
// numbering so the primary slot is always populated.
func (p *platformImagePipeline) downloadBackdrops(ctx context.Context, backdropTags []string) {
	if len(backdropTags) == 0 {
		return
	}
	r, a := p.r, p.artist
	primary := r.getActiveFanartPrimary(ctx)
	kodi := r.isKodiNumbering(ctx)
	dedup := newBackdropDedup(p.dir, primary, r.logger)

	// #2540 NOTIFY-ONLY cross-artist collision registry. Built ONCE per artist's
	// backdrop import (it is a whole-library scan) and reused for every slot --
	// mirroring newBackdropDedup's once-per-run seed above. A build failure
	// degrades to no checking: fail-open, never a blocked import.
	var identityIdx []img.FanartIdentityEntry
	if r.collisionNotifier != nil && r.artistService != nil {
		idx, idxErr := r.artistService.BuildFanartIdentityIndex(ctx)
		if idxErr != nil {
			r.logger.Warn("building fanart identity index; skipping cross-artist collision check for this import",
				"artist", a.Name, "error", idxErr)
		} else {
			identityIdx = idx
		}
	}

	downloaded := 0
	duplicates := 0
	anyExisted := false
	for i, tag := range backdropTags {
		if tag == "" {
			r.logger.Debug("skipping backdrop with empty tag", "artist", a.Name, "index", i)
			continue
		}
		switch p.downloadBackdrop(ctx, i, primary, kodi, dedup, identityIdx) {
		case backdropExisted:
			anyExisted = true
		case backdropDownloaded:
			downloaded++
		case backdropDuplicate:
			duplicates++
		case backdropSkipped:
		}
	}
	if duplicates > 0 {
		// Visible at Info because a nonzero count means the platform served the
		// same image under multiple backdrop tags -- the #2540 shape. Operators
		// should be able to see the mirror suppressing the spray.
		r.logger.Info("skipped duplicate backdrops during import",
			slog.String("artist", a.Name),
			slog.Int("duplicates", duplicates))
	}
	if downloaded > 0 || anyExisted || duplicates > 0 {
		// When backdrop index 0 failed (empty tag, download error, etc.)
		// but later indexes succeeded, no primary fanart file exists.
		// The UI serves the background image from /images/fanart/file which
		// only matches the primary name pattern. Compact the numbered files
		// so the lowest available becomes the primary -- same pattern used
		// by handleFanartBatchDelete.
		//
		// duplicates>0 must run this too: a backdrop deduped against a
		// pre-existing NUMBERED fanart (e.g. only fanart1.jpg on disk, no
		// primary) means the artist genuinely holds that image -- but with an
		// empty primary slot the UI would show no backdrop. Compaction promotes
		// the numbered file to the primary. Before this fix that promotion only
		// happened because index 0 was written as a spray copy; dedup removed
		// that side effect, so the compaction must be triggered explicitly.
		r.compactFanartIfNeeded(ctx, a.ID, p.dir, primary, kodi)
		r.updateArtistImageFlag(ctx, a, "fanart")
		r.updateArtistFanartCount(ctx, a)
	}
}

// downloadBackdrop handles a single backdrop slot: existence check, download,
// format conversion, and save with provenance metadata. Errors are non-fatal:
// logged as warnings and reported as backdropSkipped.
func (p *platformImagePipeline) downloadBackdrop(ctx context.Context, i int, primary string, kodi bool, dedup *backdropDedup, identityIdx []img.FanartIdentityEntry) backdropOutcome {
	r, a := p.r, p.artist

	filename := img.FanartFilename(primary, i, kodi)
	// Check all common image extensions for this slot. img.ConvertFormat converts WebP
	// to PNG, so a previously-saved file may have a different extension than the
	// current filename. FanartFilename preserves the extension from the active
	// primary name, so the saved file and the generated name may legitimately differ.
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	slotExists := false
	skipDownload := false
	for _, ext := range []string{".jpg", ".jpeg", ".png"} {
		candidate := filepath.Join(p.dir, base+ext)
		_, statErr := os.Stat(candidate)
		if statErr == nil {
			slotExists = true
			break
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			r.logger.Warn("checking backdrop existence", "artist", a.Name, "index", i, "file", base+ext, "error", statErr)
			skipDownload = true
			// Continue checking remaining extensions -- this candidate may be temporarily inaccessible.
		}
	}
	if slotExists {
		r.logger.Debug("skipping existing backdrop", "artist", a.Name, "index", i)
		return backdropExisted
	}
	if skipDownload {
		r.logger.Warn("skipping backdrop download due to filesystem error", "artist", a.Name, "index", i)
		return backdropSkipped
	}

	data, _, dlErr := p.dl.GetArtistBackdrop(ctx, p.platformArtistID, i)
	if dlErr != nil {
		r.logger.Warn("downloading backdrop from platform", "artist", a.Name, "index", i, "error", dlErr)
		return backdropSkipped
	}
	if len(data) == 0 {
		r.logger.Warn("empty backdrop response from platform", "artist", a.Name, "index", i)
		return backdropSkipped
	}
	converted, _, convertErr := img.ConvertFormat(bytes.NewReader(data))
	if convertErr != nil {
		r.logger.Warn("converting backdrop format", "artist", a.Name, "index", i, "error", convertErr)
		return backdropSkipped
	}

	// #2540: skip a backdrop whose image the artist already holds in another
	// slot. Two tiers, both on the pre-save converted bytes:
	//   - Content-hash catches the dominant vector: the SAME picture served
	//     under many BackdropImageTags in ONE populate run. Every such copy
	//     produces identical converted bytes, so their content-hashes match
	//     exactly (zero false positives). This is what the regression test
	//     proves and what fixes the 41-copies-in-one-artist prod case.
	//   - Perceptual-hash carries dedup across runs and against re-encoded
	//     copies, where content-hash cannot: files already on disk were saved
	//     with injected (timestamped) EXIF, so their bytes -- and thus their
	//     content-hash -- differ from a fresh download of the same picture.
	//     phash is computed on decoded pixels, so it is invariant to that.
	// The check runs on the converted bytes (what would be written) so a format
	// conversion cannot make two copies of one picture look distinct.
	content := img.ContentHash(converted)
	phash, phErr := img.PerceptualHash(bytes.NewReader(converted))
	if phErr != nil {
		// Fall back to content-hash-only dedup; a decode failure here is
		// unusual and non-fatal to the import.
		r.logger.Debug("perceptual hash for backdrop dedup failed; using content-hash only",
			"artist", a.Name, "index", i, "error", phErr)
		phash = 0
	}
	if dedup.isDuplicate(content, phash) {
		r.logger.Debug("skipping duplicate backdrop already held in another slot",
			"artist", a.Name, "index", i)
		return backdropDuplicate
	}

	// #2540 NOTIFY-ONLY: this backdrop passed intra-artist dedup and is about to
	// be written. Compare it against the cross-artist registry HERE (the converted
	// bytes and phash are in hand) but hold the verdict -- the notification is
	// only emitted once the save is CONFIRMED below.
	//
	// Deciding here and notifying later is deliberate. The durable half of the
	// notification is a fixable Action Queue entry whose auto-fix BACKS ARTWORK
	// OUT of the artist. Raising it for an import that then failed to write would
	// point a destructive remediation at a file that was never created.
	//
	// Fail-open: a zero phash (the decode failure handled above) or an empty
	// registry yields Indeterminate, so collisionResult stays nil.
	var collisionResult *img.IdentityResult
	if len(identityIdx) > 0 {
		if res := img.CompareIdentity(phash, a.ID, identityIdx, collision.DefaultTolerance); res.Verdict == img.IdentityMismatch {
			collisionResult = &res
		}
	}

	meta := &img.ExifMeta{Source: p.connType, Fetched: time.Now().UTC(), Mode: "user"}
	// An import that lands on an existing slot overwrites the user's image, so it
	// takes the same backup + rollback protection as every other destructive fanart
	// write (#2413).
	saved, saveErr := r.saveFanartSlotProtected(ctx, p.dir, []string{filename}, converted, meta)
	if saveErr != nil {
		r.logger.Warn("saving backdrop", "artist", a.Name, "index", i, "error", saveErr)
		return backdropSkipped
	}
	if len(saved) == 0 {
		r.logger.Warn("saving backdrop produced no files", "artist", a.Name, "index", i, "dir", p.dir, "filename", filename)
		return backdropSkipped
	}
	// The save is confirmed (no error, at least one file on disk), so the image
	// the collision was detected on genuinely exists now. Only here is it correct
	// to raise the notification: the toast tells the operator what just landed,
	// and the durable entry's back-out has real artwork to act on. The import
	// itself is never blocked -- this runs after the write, not instead of it.
	if collisionResult != nil {
		r.collisionNotifier.Notify(ctx, a.ID, a.Name, *collisionResult)
	}

	// Record the just-saved image so a later index carrying the same picture is
	// deduped against it within this same run.
	dedup.add(content, phash)
	p.result.Images++
	return backdropDownloaded
}

// downloadPlatformImages downloads available images from a media platform for a single artist.
// connType identifies the platform source (e.g. "emby", "jellyfin") for provenance metadata.
// Errors are non-fatal: logged as warnings and skipped.
func (r *Router) downloadPlatformImages(ctx context.Context, dl imageDownloader, platformArtistID string, imageTags map[string]string, backdropTags []string, a *artist.Artist, connType string, result *populateResult) {
	p, ok := newPlatformImagePipeline(r, dl, platformArtistID, a, connType, result)
	if !ok {
		return
	}

	p.downloadNamedImages(ctx, imageTags)
	p.downloadBackdrops(ctx, backdropTags)

	if result.Images > 0 {
		r.enforceCacheLimitIfNeeded(ctx, a)
	}
}

// compactFanartIfNeeded renumbers fanart files when the primary slot is missing
// but numbered files exist. This closes gaps so the primary filename always
// corresponds to the first available fanart.
func (r *Router) compactFanartIfNeeded(ctx context.Context, artistID, dir, primary string, kodi bool) {
	paths, discoverErr := img.DiscoverFanart(dir, primary)
	if discoverErr != nil {
		r.logger.Warn("discovering fanart for compact",
			slog.String("dir", dir),
			slog.String("error", discoverErr.Error()))
		return
	}
	if len(paths) == 0 {
		return
	}
	// Check whether the primary slot exists. DiscoverFanart returns paths
	// in index order, with the primary file (if present) appearing first.
	// Compare bases to confirm the primary is present.
	primaryBase := strings.TrimSuffix(primary, filepath.Ext(primary))
	firstBase := strings.TrimSuffix(filepath.Base(paths[0]), filepath.Ext(paths[0]))
	if strings.EqualFold(firstBase, primaryBase) {
		return // primary exists, nothing to compact
	}
	// Renumber all discovered files sequentially from index 0.
	if err := img.RenumberFanart(ctx, r.artistService, artistID, dir, primary, paths, kodi); err != nil {
		r.logger.Warn("compacting fanart after primary removal",
			slog.String("error", err.Error()))
	}
}

// backfillPlatformIDToManualLibs copies a platform ID mapping to any matching
// artist in the given manual-source (filesystem) libraries. It matches by
// MBID first, then case-insensitive name. This ensures that push operations
// from the primary filesystem artist can find the platform mapping.
func (r *Router) backfillPlatformIDToManualLibs(
	ctx context.Context,
	mbid, name, connectionID, platformArtistID, connArtistID string,
	manualLibs []library.Library,
) {
	// In the M:N model an artist row is shared across libraries, so the
	// per-library loop is vestigial; one unscoped lookup resolves the row
	// that any of the manual libraries would observe. The manualLibs
	// parameter is retained as a presence guard: skip entirely when the
	// caller has no manual libraries configured.
	if len(manualLibs) == 0 {
		return
	}
	fsArtist, err := r.artistService.FindByMBIDOrNameUnscoped(ctx, mbid, name)
	if err != nil {
		r.logger.Warn("backfill: finding filesystem artist", "name", name, "error", err)
		return
	}
	if fsArtist == nil || fsArtist.ID == connArtistID {
		return
	}
	// Backfill is a non-authoritative writer, so route through the stable set
	// to preserve the deterministic (lowest-id) mapping instead of clobbering
	// (#2344).
	outcome, setErr := r.artistService.SetPlatformIDStable(ctx, fsArtist.ID, connectionID, platformArtistID)
	if setErr != nil {
		// a UNIQUE index on (connection_id, platform_artist_id) means at
		// most one artist row can hold a given platform mapping. The
		// connection-library artist already claimed it before we got
		// here, so the filesystem-library copy is now redundant rather
		// than erroneous. Skip silently with a debug log instead of
		// warning.
		if errors.Is(setErr, artist.ErrPlatformIDClaimedByAnotherArtist) {
			r.logger.Debug("backfill: platform id already held by another artist row, skipping",
				"fs_artist_id", fsArtist.ID, "connection_id", connectionID)
			return
		}
		r.logger.Warn("backfill: storing platform id on filesystem artist", "name", fsArtist.Name, "error", setErr)
		return
	}
	r.logPlatformIDDivergence(outcome, fsArtist.Name, "filesystem", platformArtistID)
	r.logger.Debug("backfill: platform id propagated to filesystem artist",
		"name", fsArtist.Name, "fs_artist_id", fsArtist.ID, "connection_id", connectionID)
}

// manualLibraries returns all libraries with source "manual". Used by scan
// and populate functions to find filesystem libraries for platform ID backfill.
func (r *Router) manualLibraries(ctx context.Context) []library.Library {
	libs, err := r.libraryService.List(ctx)
	if err != nil {
		r.logger.Error("backfill: failed to list libraries, backfill will be skipped for this operation", "error", err)
		return nil
	}
	var manual []library.Library
	for i := range libs {
		if libs[i].Source == library.SourceManual {
			manual = append(manual, libs[i])
		}
	}
	return manual
}

// logPlatformIDDivergence emits a single INFO line when a non-authoritative
// platform-id write (scan, populate, or manual-library backfill) had to
// tie-break a divergent id for the same (artist, connection). The stable set
// keeps the deterministic lowest id; this makes the losing id visible without a
// ledger, so the Emby duplicate-twin flip-flop no longer happens silently
// (#2344). No-op when the write did not diverge.
func (r *Router) logPlatformIDDivergence(outcome artist.PlatformIDStableOutcome, name, platform, incoming string) {
	if !outcome.Diverged {
		return
	}
	r.logger.Info("resolved a divergent platform id; kept deterministic pick",
		"name", name, "platform", platform,
		"kept_platform_artist_id", outcome.StoredID,
		"previous_platform_artist_id", outcome.PreviousID,
		"incoming_platform_artist_id", incoming)
}

// resolveAndBackfillPlatformID finds the connection-library artist by MBID
// or exact name, stores the platform ID on it, and backfills the mapping to
// any matching filesystem-library artist. Returns the connection-library
// artist for the caller to update image flags, or nil if no match found.
//
// The lookup prefers an artist that already holds a membership in connLib
// (so a transitional state with one artist row per library still resolves
// to the connection-side row, not the filesystem-side row). It falls back
// to an unscoped MBID-then-name lookup, which is the right answer once the
// duplicate-collapse migration has merged the rows.
func (r *Router) resolveAndBackfillPlatformID(
	ctx context.Context,
	mbid, name, connectionID, platformArtistID string,
	connLib *library.Library,
	manualLibs []library.Library,
) *artist.Artist {
	// Library-scoped lookup: distinguish "not found" (a == nil, err == nil)
	// from a real DB/load failure (err != nil). Falling back to the unscoped
	// match on a real error during the duplicate-row transition could attach
	// the platform ID to a sibling-library artist instead of failing safely.
	a, err := r.findArtistInLibrary(ctx, mbid, name, connLib.ID)
	if err != nil {
		r.logger.Warn("scan artist library-scoped lookup", "name", name, "mbid", mbid, "platform", connLib.Source, "error", err)
		return nil
	}
	if a == nil {
		var lookupErr error
		a, lookupErr = r.artistService.FindByMBIDOrNameUnscoped(ctx, mbid, name)
		if lookupErr != nil {
			r.logger.Warn("scan artist lookup", "name", name, "mbid", mbid, "platform", connLib.Source, "error", lookupErr)
			return nil
		}
	}
	if a == nil {
		return nil
	}

	// Store platform ID on the resolved artist. Scans are non-authoritative
	// writers, so route through the divergence-aware stable set: when Emby
	// returns duplicate items sharing one MBID, this keeps the deterministic
	// (lowest-id) winner across scans instead of flip-flopping between the
	// twins, so metadata/image pushes always target the same item (#2344).
	if outcome, setErr := r.artistService.SetPlatformIDStable(ctx, a.ID, connectionID, platformArtistID); setErr != nil {
		r.logger.Warn("storing platform id during scan", "name", a.Name, "platform", connLib.Source, "error", setErr)
	} else {
		r.logPlatformIDDivergence(outcome, a.Name, connLib.Source, platformArtistID)
	}

	// Backfill to filesystem-library artists.
	r.backfillPlatformIDToManualLibs(ctx, mbid, name, connectionID, platformArtistID, a.ID, manualLibs)

	return a
}

// findArtistInLibrary looks up an artist by MBID then case-insensitive
// name, restricted to artists that are members of libraryID. Returns nil
// if no match exists. Used by resolveAndBackfillPlatformID to prefer the
// connection-library artist over a sibling-library artist that shares the
// same identity (transitional duplicate-row state under M:N).
// findArtistInLibrary returns (artist, nil) on a hit, (nil, nil) on a genuine
// "no match" result, and (nil, err) on a real DB/load failure. Callers must
// distinguish the latter so they do not silently fall back to an unscoped
// lookup that could attach IDs to the wrong sibling-library artist.
func (r *Router) findArtistInLibrary(ctx context.Context, mbid, name, libraryID string) (*artist.Artist, error) {
	if mbid != "" {
		a, err := r.lookupByMBIDInLibrary(ctx, mbid, libraryID)
		if err != nil {
			return nil, err
		}
		if a != nil {
			return a, nil
		}
	}
	if name == "" {
		return nil, nil
	}
	return r.lookupByNameInLibrary(ctx, name, libraryID)
}

// lookupByMBIDInLibrary returns (nil, nil) when no row matches and (nil, err)
// when the query or follow-up load fails. The caller decides whether to fall
// back; this function never silently swallows a DB error as "not found".
func (r *Router) lookupByMBIDInLibrary(ctx context.Context, mbid, libraryID string) (*artist.Artist, error) {
	var artistID string
	// ORDER BY datetime(created_at), a.id makes the LIMIT 1 deterministic when
	// multiple artists share the same library + MBID. datetime() normalizes
	// mixed timestamp formats (legacy SQLite "YYYY-MM-DD HH:MM:SS" vs RFC3339
	// "T"-separator) so chronological order survives the mixed-format reality
	// of production data; a.id is the deterministic tie-breaker.
	err := r.db.QueryRowContext(ctx, `
		SELECT a.id FROM artists a
		JOIN artist_libraries al ON al.artist_id = a.id
		JOIN artist_provider_ids p ON p.artist_id = a.id
		WHERE al.library_id = ?
		  AND p.provider = 'musicbrainz' AND p.provider_id = ?
		ORDER BY datetime(a.created_at), a.id
		LIMIT 1
	`, libraryID, mbid).Scan(&artistID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("library-scoped mbid lookup (mbid=%s library=%s): %w", mbid, libraryID, err)
	}
	a, err := r.artistService.GetByID(ctx, artistID)
	if err != nil {
		return nil, fmt.Errorf("loading library-scoped artist by id (%s): %w", artistID, err)
	}
	return a, nil
}

// lookupByNameInLibrary mirrors lookupByMBIDInLibrary's error contract: a
// real DB/load failure surfaces to the caller, only sql.ErrNoRows collapses
// to (nil, nil).
func (r *Router) lookupByNameInLibrary(ctx context.Context, name, libraryID string) (*artist.Artist, error) {
	var artistID string
	// Same determinism rationale as lookupByMBIDInLibrary: order by the
	// chronologically normalized created_at, then a.id, so duplicate-name
	// rows in the same library always resolve to the same artist.
	err := r.db.QueryRowContext(ctx, `
		SELECT a.id FROM artists a
		JOIN artist_libraries al ON al.artist_id = a.id
		WHERE al.library_id = ? AND LOWER(a.name) = LOWER(?)
		ORDER BY datetime(a.created_at), a.id
		LIMIT 1
	`, libraryID, name).Scan(&artistID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("library-scoped name lookup (name=%s library=%s): %w", name, libraryID, err)
	}
	a, err := r.artistService.GetByID(ctx, artistID)
	if err != nil {
		return nil, fmt.Errorf("loading library-scoped artist by id (%s): %w", artistID, err)
	}
	return a, nil
}

// scanFromEmby pages through Emby artists and resolves each one to a local
// artist row, storing the platform ID mapping. It returns the number of
// artists that were matched and mapped.
//
// This scan deliberately does NOT touch the artist's image-existence flags
// (#2637). Those flags, and the artist_images rows behind them, describe files
// on the LOCAL filesystem: the scanner populates them and the serve-image
// endpoint, the rule engine, and the reports export all read them that way.
// Emby's ImageTags/BackdropImageTags describe what the PLATFORM holds, which is
// a different inventory. Writing one over the other made an artist whose
// artwork exists locally but not on Emby look artwork-less, and because the
// write went through Update -> persistNormalized -> UpsertAll it cleared
// exists_flag and deleted rows outright: a false FanartExists emits no fanart
// slots at all, so the artist's entire fanart tail was deleted.
//
// Nothing needs the platform side recorded here. The artwork reconciler
// (internal/publish/reconcile.go) fetches platform image state live from the
// connection on every cycle and compares it against files on disk, so platform
// pushes are unaffected by this scan not writing anything.
func (r *Router) scanFromEmby(ctx context.Context, client *emby.Client, lib *library.Library) (int, error) {
	manualLibs := r.manualLibraries(ctx)
	matched := 0
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(ctx, lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return matched, fmt.Errorf("fetching artists from emby: %w", err)
		}

		for i := range resp.Items {
			item := &resp.Items[i]
			a := r.resolveAndBackfillPlatformID(ctx,
				item.ProviderIDs.MusicBrainzArtist, item.Name,
				lib.ConnectionID, item.ID, lib, manualLibs)
			if a == nil {
				continue
			}
			matched++
		}

		startIndex += pageSize
		if startIndex >= resp.TotalRecordCount {
			break
		}
	}
	return matched, nil
}

// scanFromJellyfin pages through Jellyfin artists and resolves each one to a
// local artist row, storing the platform ID mapping. It returns the number of
// artists that were matched and mapped. Like scanFromEmby it never writes local
// image-existence state from the platform's inventory (see scanFromEmby, #2637).
func (r *Router) scanFromJellyfin(ctx context.Context, client *jellyfin.Client, lib *library.Library) (int, error) {
	manualLibs := r.manualLibraries(ctx)
	matched := 0
	startIndex := 0
	pageSize := 100
	for {
		resp, err := client.GetArtists(ctx, lib.ExternalID, startIndex, pageSize)
		if err != nil {
			return matched, fmt.Errorf("fetching artists from jellyfin: %w", err)
		}

		for i := range resp.Items {
			item := &resp.Items[i]
			a := r.resolveAndBackfillPlatformID(ctx,
				item.ProviderIDs.MusicBrainzArtist, item.Name,
				lib.ConnectionID, item.ID, lib, manualLibs)
			if a == nil {
				continue
			}
			matched++
		}

		startIndex += pageSize
		if startIndex >= resp.TotalRecordCount {
			break
		}
	}
	return matched, nil
}

// scanFromLidarr gets all Lidarr artists and resolves each one to a local
// artist row, storing the platform ID mapping. It returns the number of artists
// that were matched and mapped. Like scanFromEmby it never writes local
// image-existence state from the platform's inventory (see scanFromEmby, #2637).
func (r *Router) scanFromLidarr(ctx context.Context, client *lidarr.Client, lib *library.Library) (int, error) {
	manualLibs := r.manualLibraries(ctx)
	artists, err := client.GetArtists(ctx)
	if err != nil {
		return 0, fmt.Errorf("fetching artists from lidarr: %w", err)
	}

	matched := 0
	for _, la := range artists {
		a := r.resolveAndBackfillPlatformID(ctx,
			la.ForeignArtistID, la.ArtistName,
			lib.ConnectionID, fmt.Sprintf("%d", la.ID), lib, manualLibs)
		if a == nil {
			continue
		}
		matched++
	}
	return matched, nil
}

// checkSyncMtimeEvidence performs Tier 2 shared-FS detection after a library
// sync. It compares the filesystem mtime of image files in each artist's
// directory against that artist's own newest last_written_at timestamp (not a
// global library-wide MAX). If any file has been modified externally (mtime
// newer than the artist's last write plus a 2-second tolerance), the library's
// shared-FS status is updated to "suspected."
//
// Using per-artist baselines avoids false negatives where a recently-written
// artist's timestamp masks an externally-modified older artist.
//
// This check is non-fatal: failures are logged at Debug/Warn level and do not
// affect the sync outcome.
func (r *Router) checkSyncMtimeEvidence(ctx context.Context, lib *library.Library) {
	// Skip if the library already has confirmed shared-FS status; do not
	// downgrade from confirmed to suspected.
	if lib.SharedFSStatus == library.SharedFSConfirmed {
		r.logger.Debug("skipping mtime check: library already confirmed as shared-FS",
			"library", lib.Name, "library_id", lib.ID)
		return
	}

	// Get per-artist newest write times for this library.
	writeTimesByArtist, err := r.artistService.NewestWriteTimesByArtistForLibrary(ctx, lib.ID)
	if err != nil {
		r.logger.Warn("mtime check: failed to query per-artist write times",
			"library", lib.Name, "library_id", lib.ID, "error", err)
		return
	}
	if len(writeTimesByArtist) == 0 {
		// No writes recorded yet -- nothing to compare against.
		r.logger.Debug("mtime check: no writes recorded for library, skipping",
			"library", lib.Name, "library_id", lib.ID)
		return
	}

	// Get all artist paths for this library (artistID -> directory path).
	artistDirs, err := r.artistService.ListPathsByLibrary(ctx, lib.ID)
	if err != nil {
		r.logger.Warn("mtime check: failed to list artist paths",
			"library", lib.Name, "library_id", lib.ID, "error", err)
		return
	}
	if len(artistDirs) == 0 {
		r.logger.Debug("mtime check: no artist paths for library, skipping",
			"library", lib.Name, "library_id", lib.ID)
		return
	}

	// Build a per-directory lastWrittenAt map using each artist's own newest
	// write time. This ensures that each artist's mtime comparison uses its
	// own baseline, rather than a single global MAX that could mask
	// modifications to artists with older write timestamps.
	lastWrittenAts := make(map[string]time.Time, len(artistDirs))
	// dirToWriteTime maps directory path to the parsed time for use in
	// evidence string formatting later.
	dirToWriteTime := make(map[string]time.Time, len(artistDirs))
	for artistID, dir := range artistDirs {
		writeStr, ok := writeTimesByArtist[artistID]
		if !ok || writeStr == "" {
			continue
		}
		parsed := dbutil.ParseTime(writeStr)
		if parsed.IsZero() {
			r.logger.Warn("mtime check: failed to parse write time for artist",
				"library", lib.Name, "artist_id", artistID, "raw", writeStr)
			continue
		}
		// When multiple artists share a directory, keep the most recent
		// write time as the baseline for mtime comparison.
		if existing, ok := lastWrittenAts[dir]; !ok || parsed.After(existing) {
			lastWrittenAts[dir] = parsed
			dirToWriteTime[dir] = parsed
		}
	}

	if len(lastWrittenAts) == 0 {
		r.logger.Debug("mtime check: no parseable write times for library, skipping",
			"library", lib.Name, "library_id", lib.ID)
		return
	}

	evidence := library.CollectMtimeEvidence(artistDirs, lastWrittenAts, r.logger)
	if len(evidence) == 0 {
		r.logger.Debug("mtime check: no external modifications detected",
			"library", lib.Name, "dirs_checked", len(artistDirs))
		return
	}

	r.logger.Debug("mtime check: external modifications detected",
		"library", lib.Name,
		"evidence_count", len(evidence))

	// Build evidence strings for the shared-FS status update, referencing
	// each artist's own baseline timestamp.
	evidenceStrings := make([]string, len(evidence))
	for i, e := range evidence {
		artistWrite := dirToWriteTime[filepath.Dir(e.Path)]
		evidenceStrings[i] = fmt.Sprintf("mtime: %s modified at %s (after last Stillwater write at %s)",
			e.Path, e.FileMtime.Format(time.RFC3339), artistWrite.Format(time.RFC3339))
	}

	evidenceJSON, marshalErr := json.Marshal(evidenceStrings)
	if marshalErr != nil {
		r.logger.Warn("mtime check: failed to marshal evidence",
			"library", lib.Name, "error", marshalErr)
		return
	}

	if setErr := r.libraryService.SetSharedFSStatus(ctx, lib.ID,
		library.SharedFSSuspected, string(evidenceJSON), lib.SharedFSPeerLibraryIDs); setErr != nil {
		r.logger.Warn("mtime check: failed to update shared-FS status",
			"library", lib.Name, "error", setErr)
	}
}
