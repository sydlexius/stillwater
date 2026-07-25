package api

// handlers_name_collision_guard.go -- the API-boundary half of the #2730
// rename guard.
//
// The handlers_ filename prefix is load-bearing: .golangci.yml suppresses
// contextcheck's known templ-interface false positive ("should pass the
// context parameter") only under path 'internal/api/handlers.*\.go', and this
// file renders a templ component.
//
// internal/artist supplies the detection (Service.FindNameCollision); this
// file owns the POLICY: which request paths are gated, what status code the
// operator gets, and what the response body says. Keeping the policy here is
// deliberate -- Service.UpdateField is also driven by history-revert and
// platform-state sync, which must stay able to write a prior value, so the
// service method itself must not become a hard gate.

import (
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/templates"
)

// guardNameCollision reports whether the caller may proceed with writing
// newName to artistID's name field.
//
// It returns true when the write is safe. It returns false when the write must
// be abandoned, and in that case it has ALREADY written the complete response
// (status + body), so the caller must return immediately without touching w.
//
// Failure modes and their status codes:
//
//   - collision detected -> 409 Conflict, with a warning fragment (HTMX) or a
//     JSON error envelope (API) naming the existing artist and pointing at the
//     duplicates report where the two can be merged.
//   - the check itself failed -> 500. A guard that could not run is NOT
//     evidence that the rename is safe, so we refuse rather than fall through
//     to the unguarded write. This is the whole defect #2730 describes.
func (r *Router) guardNameCollision(w http.ResponseWriter, req *http.Request, artistID, newName string) bool {
	collision, err := r.artistService.FindNameCollision(req.Context(), artistID, newName)
	if err != nil {
		// Fail closed. See the doc comment above: an unavailable guard must
		// not silently degrade into the pre-#2730 behavior.
		r.logger.Error("checking artist name collision",
			slog.String("artist_id", artistID),
			slog.String("new_name", newName),
			slog.String("error", err.Error()))
		writeError(w, req, http.StatusInternalServerError, "failed to check for a name collision")
		return false
	}
	if collision == nil {
		return true
	}

	r.logger.Warn("artist name change rejected: identity collision",
		slog.String("artist_id", artistID),
		slog.String("new_name", newName),
		slog.String("existing_artist_id", collision.ArtistID),
		slog.Int("status", http.StatusConflict))

	if isHTMXRequest(req) {
		// 409 + an HTML body. htmx does not swap 4xx responses by default, so
		// the fragment reaches the operator through the global
		// htmx:responseError handler in layout.templ, which extracts the body
		// text and shows it as a toast. Rendering a real fragment rather than
		// a bare string keeps the message translated and keeps the styling on
		// the existing warning-banner pattern for any surface that does opt
		// into swapping it.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		view := templates.NameCollisionWarningView{
			ExistingName:         collision.Name,
			ExistingPlatformOnly: collision.PlatformOnly(),
			DuplicatesURL:        r.basePath + "/reports/duplicates",
		}
		component := templates.NameCollisionWarning(view)
		if renderErr := component.Render(req.Context(), w); renderErr != nil {
			// The status and headers are already committed, so this cannot be
			// upgraded to a 500. Log it loudly: a silent render failure here
			// would leave the operator with an empty 409 and no explanation.
			r.logger.Error("rendering name collision warning",
				slog.String("artist_id", artistID),
				slog.String("error", renderErr.Error()))
		}
		return false
	}

	writeJSON(w, http.StatusConflict, map[string]string{
		"error":              artist.ErrNameCollision.Error(),
		"existing_artist_id": collision.ArtistID,
		"existing_name":      collision.Name,
	})
	return false
}
