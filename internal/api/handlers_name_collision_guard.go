package api

// handlers_name_collision_guard.go -- the API-boundary half of the #2730
// rename guard.
//
// The handlers_ filename prefix is load-bearing: .golangci.yml suppresses
// contextcheck's known templ-interface false positive ("should pass the
// context parameter") only under path 'internal/api/handlers.*\.go', and this
// file renders a templ component.
//
// internal/artist owns the GUARANTEE (#3037): Service.UpdateField routes a
// name write through UpdateNameGuarded, so no caller can reach the column
// unguarded by picking a different SINGLE-FIELD service method. (Two methods
// still reach it without the guard -- Service.Update and Service.Create --
// enumerated in internal/artist/name_collision.go's scope block; neither is
// reachable from this handler.) This file owns the PRESENTATION -- what status
// code the operator gets and what the response body says -- plus a pre-write
// fast path that fails before a transaction opens. If this file were deleted
// the rename would still be refused; the operator would just get a worse
// message.

import (
	"log/slog"
	"net/http"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/i18n"
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
//     duplicates report.
//
//     The response deliberately does NOT promise that the report will DISPLAY
//     the pair. There are two distinct ways it will not, with an identical
//     operator experience. First, conflicting MBIDs: DetectDuplicates refuses
//     to group artists bound to different non-empty MusicBrainz IDs
//     (makeGuardedUnion, the #2527 data-loss guard), while this check
//     considers only the name key, so such a pair is refused here and never
//     forms a group. Second, an ignored group (#2798): the page filters
//     server-side ignores (FilterIgnoredGroups), so the pair exists but is
//     hidden from the very operator who ignored it.
//
//     The guard still refuses the conflicting-MBID rename on purpose: nothing
//     in this repo verifies a stored MBID is correct (validation checks UUID
//     shape only, and #2715 records that some are adopted from an unscored
//     name-search hit), so exempting on "different MBIDs" would defer to a
//     signal that may itself be the defect.
//
//     The copy is therefore written to be true in ALL THREE outcomes rather
//     than detecting which applies -- distinguishing case 1 would mean
//     consulting MBIDs here, which is exactly what the guard must not do. What
//     it always promises is the OTHER ARTIST'S IDENTITY, which is always known
//     and always useful; what it never promises unconditionally is that a
//     merge affordance is waiting on the report.
//
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

	r.writeNameCollisionRefusal(w, req, artistID, newName, collision)
	return false
}

// writeNameCollisionRefusal writes the complete 409 response (status + body)
// for a rename the guard refused. It is shared by BOTH refusal paths so they
// are the same response by construction rather than by inspection:
//
//   - guardNameCollision, the pre-write fast path above; and
//   - handleFieldUpdate's transactional path, where Service.UpdateNameGuarded
//     re-runs the check inside the writing transaction and refuses a rename
//     that raced past the fast path (#2807).
//
// The operator must not be able to tell which path refused: the two differ
// only in WHERE the collision was detected, never in what is reported. Giving
// the second path its own copy of this body would let the two drift, which is
// how the #2798 rewording came to need applying twice.
func (r *Router) writeNameCollisionRefusal(w http.ResponseWriter, req *http.Request, artistID, newName string, collision *artist.NameCollision) {
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
		return
	}

	// detail reuses the SAME translation key the HTMX fragment renders, rather
	// than a second hardcoded copy of it. A duplicated literal drifts: the
	// #2798 rewording had to be applied twice, and nothing would have caught a
	// miss. Sharing the key also means an API client gets the operator's
	// locale, and it keeps the "does not promise a merge" copy guard covering
	// both surfaces at once (see the doc comment above).
	//
	// The key carries no leading space -- the template supplies its own
	// spacing via explicit { " " } nodes -- so it is safe at sentence start.
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":              artist.ErrNameCollision.Error(),
		"existing_artist_id": collision.ArtistID,
		"existing_name":      collision.Name,
		"detail":             i18n.TFromCtx(req.Context()).T("name_collision.resolve_hint"),
	})
}
