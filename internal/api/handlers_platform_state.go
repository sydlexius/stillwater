package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/connection/emby"
	"github.com/sydlexius/stillwater/internal/connection/jellyfin"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/provider/tagdict"
	"github.com/sydlexius/stillwater/web/templates"
)

// handleGetPlatformState fetches the current state of an artist on a platform connection
// and returns an HTML partial for HTMX lazy-loading.
// GET /api/v1/artists/{id}/platform-state?connection_id=X
func (r *Router) handleGetPlatformState(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	connectionID := req.URL.Query().Get("connection_id")
	if connectionID == "" {
		writeError(w, req, http.StatusBadRequest, "connection_id is required")
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), connectionID)
	if err != nil {
		writeError(w, req, http.StatusNotFound, "connection not found")
		return
	}
	if !conn.Enabled {
		writeError(w, req, http.StatusBadRequest, "connection is disabled")
		return
	}

	platformArtistID, err := r.artistService.GetPlatformID(req.Context(), artistID, connectionID)
	if err != nil {
		r.logger.Error("looking up platform id", "artist_id", artistID, "connection_id", connectionID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "internal error")
		return
	}
	if platformArtistID == "" {
		writeError(w, req, http.StatusNotFound, "no platform ID stored for this artist on this connection")
		return
	}

	getter, err := r.newStateGetter(conn)
	if err != nil {
		r.logger.Error("creating state getter", "connection", conn.Name, "type", conn.Type, "error", err)
		msg := "connection type does not support platform state"
		if isHTMXRequest(req) {
			renderTempl(w, req, templates.PlatformStateError(conn, msg))
		} else {
			writeError(w, req, http.StatusBadRequest, msg)
		}
		return
	}

	state, err := getter.GetArtistDetail(req.Context(), platformArtistID)
	if err != nil {
		r.logger.Error("fetching platform state", "artist", a.Name, "connection", conn.Name, "error", err)
		msg := "check the connection and try again"
		if isHTMXRequest(req) {
			renderTempl(w, req, templates.PlatformStateError(conn, msg))
		} else {
			writeError(w, req, http.StatusInternalServerError, msg)
		}
		return
	}

	// Normalize ISO 8601 timestamps to date-only so the template comparison
	// and display use the same form as Stillwater's stored date fields.
	state.PremiereDate = dateOnly(state.PremiereDate)
	state.EndDate = dateOnly(state.EndDate)

	if req.URL.Query().Get("readonly") == "true" {
		renderTempl(w, req, templates.PlatformStateCardReadOnly(a, conn, state, r.getActiveProfileName(req.Context())))
	} else {
		renderTempl(w, req, templates.PlatformStateCard(a, conn, state, r.getActiveProfileName(req.Context())))
	}
}

// handlePullMetadata pulls metadata from a platform connection into Stillwater.
// Biography and dates overwrite the stored values. Genres accumulate: the
// platform's genres are unioned with the artist's existing ones (canonical,
// locale-aware dedup, then the Tag Sources exclude patterns and caps), so a
// pull does not replace the existing tags. An existing tag that matches an
// exclude pattern, or falls past the genre cap, is still removed. The union is computed here,
// not in UpdateField, so manual edits and history reverts still replace.
// POST /api/v1/artists/{id}/pull?connection_id=X
func (r *Router) handlePullMetadata(w http.ResponseWriter, req *http.Request) {
	artistID := req.PathValue("id")
	connectionID := req.URL.Query().Get("connection_id")
	if connectionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection_id is required"})
		return
	}

	a, err := r.artistService.GetByID(req.Context(), artistID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
		return
	}

	conn, err := r.connectionService.GetByID(req.Context(), connectionID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "connection not found"})
		return
	}
	if !conn.Enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "connection is disabled"})
		return
	}

	platformArtistID, err := r.artistService.GetPlatformID(req.Context(), artistID, connectionID)
	if err != nil {
		r.logger.Error("looking up platform id", "artist_id", artistID, "connection_id", connectionID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if platformArtistID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no platform ID stored for this artist on this connection"})
		return
	}

	getter, err := r.newStateGetter(conn)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	state, err := getter.GetArtistDetail(req.Context(), platformArtistID)
	if err != nil {
		r.logger.Error("pulling platform state", "artist_id", artistID, "connection", conn.Name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to fetch platform state: " + err.Error()})
		return
	}

	// #3078: the operator clicked Pull (source stays "manual"), but the VALUES
	// come from the platform, so the history rows record platform:<type> as
	// their producer rather than leaving the write looking operator-authored.
	pullCtx := artist.ContextWithProducer(req.Context(), "platform:"+conn.Type)

	var updated []string

	if state.Biography != "" {
		changed, err := r.artistService.UpdateField(pullCtx, artistID, "biography", state.Biography)
		if err != nil {
			r.logger.Warn("updating biography from platform", "error", err)
		} else if changed {
			updated = append(updated, "biography")
		}
	}

	if len(state.Genres) > 0 {
		if r.pullGenres(pullCtx, artistID, state.Genres) {
			updated = append(updated, "genres")
		}
	}

	// Mirror push logic: write to born/died for persons, formed/disbanded for groups.
	premiereField, endField := "formed", "disbanded"
	if artist.NormalizeType(a.Type) == "person" {
		premiereField, endField = "born", "died"
	}

	if state.PremiereDate != "" {
		changed, err := r.artistService.UpdateField(pullCtx, artistID, premiereField, dateOnly(state.PremiereDate))
		if err != nil {
			r.logger.Warn("updating date from platform", "field", premiereField, "error", err)
		} else if changed {
			updated = append(updated, premiereField)
		}
	}

	if state.EndDate != "" {
		changed, err := r.artistService.UpdateField(pullCtx, artistID, endField, dateOnly(state.EndDate))
		if err != nil {
			r.logger.Warn("updating date from platform", "field", endField, "error", err)
		} else if changed {
			updated = append(updated, endField)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "pulled",
		"updated": updated,
	})
}

// pullGenres adds the platform's genres to the artist's existing ones and
// reports whether the stored list changed. The read-merge-write is serialized
// per artist (the platform call and the rest of the pull stay outside the
// lock). This orders pull-vs-pull only; pull-vs-refresh/edit is the general
// lost-update gap tracked by #2804.
func (r *Router) pullGenres(reqCtx context.Context, artistID string, platform []string) bool {
	lk, _ := r.pullGenresLocks.LoadOrStore(artistID, &sync.Mutex{})
	mu := lk.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	// Re-read inside the lock: the caller's artist predates the network call.
	// If the re-read fails, skip the genre update rather than merge into a
	// stale snapshot.
	cur, err := r.artistService.GetByID(reqCtx, artistID)
	if err != nil {
		r.logger.Warn("reloading artist for genre pull, skipping genres", "artist_id", artistID, "error", err)
		return false
	}
	ctx := r.injectMetadataLanguages(reqCtx)
	merged := tagdict.ApplyVocabFilter(tagdict.MetadataVocab(ctx), tagdict.VocabFieldGenres,
		tagdict.MergeAndDeduplicateLocale(cur.Genres, platform, provider.FirstMetadataLang(ctx)))
	if r.pullGenresReadHook != nil {
		r.pullGenresReadHook()
	}
	changed, err := r.artistService.UpdateField(ctx, artistID, "genres", strings.Join(merged, ", "))
	if err != nil {
		r.logger.Warn("updating genres from platform", "error", err)
		return false
	}
	return changed
}

// newStateGetter instantiates an ArtistStateGetter for the given connection type.
func (r *Router) newStateGetter(conn *connection.Connection) (connection.ArtistStateGetter, error) {
	switch conn.Type {
	case connection.TypeEmby:
		return emby.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger), nil
	case connection.TypeJellyfin:
		return jellyfin.New(conn.URL, conn.APIKey, conn.GetPlatformUserID(), r.logger), nil
	default:
		return nil, errUnsupportedConnectionType
	}
}

// errUnsupportedConnectionType is returned when a connection type does not support platform state.
var errUnsupportedConnectionType = errors.New("connection type does not support platform state")

// dateOnly strips the time component from an ISO 8601 datetime string
// (e.g. "1985-01-01T00:00:00.0000000Z" -> "1985-01-01"). If the string
// contains no 'T' separator it is returned unchanged.
func dateOnly(s string) string {
	if date, _, ok := strings.Cut(s, "T"); ok {
		return date
	}
	return s
}
