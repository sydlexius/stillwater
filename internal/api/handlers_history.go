package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/web/templates"
)

// parseFilterValues extracts filter values from a multi-valued query parameter,
// stripping the "+" include prefix that the filter flyout component emits.
// Bare values (no prefix) are treated as includes. Values with a "-" exclude
// prefix are ignored for now (exclude filtering is not yet implemented).
func parseFilterValues(values []string) []string {
	var result []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if strings.HasPrefix(v, "-") {
			continue // exclude not yet supported
		}
		result = append(result, strings.TrimPrefix(v, "+"))
	}
	return result
}

// splitSourceFilters separates source filter values into exact matches and
// prefix patterns. Values like "provider:*" and "rule:*" are treated as prefix
// patterns (matching any source that starts with "provider:" or "rule:").
// All other values are treated as exact matches.
func splitSourceFilters(sources []string) (exact []string, prefixes []string) {
	for _, s := range sources {
		if strings.HasSuffix(s, ":*") {
			// Convert "provider:*" to prefix "provider:"
			prefixes = append(prefixes, strings.TrimSuffix(s, "*"))
		} else {
			exact = append(exact, s)
		}
	}
	return exact, prefixes
}

// sliceContains reports whether s is present in the slice.
func sliceContains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// isPlainDate reports whether s is a plain YYYY-MM-DD date string.
func isPlainDate(s string) bool {
	if len(s) != 10 {
		return false
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			if c != '-' {
				return false
			}
		} else if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// parseTimeValue parses a date/time string, applying end-of-day semantics for
// the "to" bound. Accepts RFC 3339 timestamps and plain YYYY-MM-DD dates.
// Plain "from" dates resolve to UTC midnight; plain "to" dates resolve to
// 23:59:59.999999999 UTC so the full day is included in range queries.
// Returns the zero value if raw is empty or unparsable.
func parseTimeValue(raw, name string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.DateOnly, raw); err == nil {
		t = t.UTC()
		if name == "to" && isPlainDate(raw) {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return t
	}
	return time.Time{}
}

// parseTimeParam reads a query parameter by name and delegates to parseTimeValue.
func parseTimeParam(req *http.Request, name string) time.Time {
	return parseTimeValue(req.URL.Query().Get(name), name)
}

// buildGlobalFilter constructs a GlobalHistoryFilter from query parameters.
// Shared between the API handler and the page/content handlers.
func buildGlobalFilter(req *http.Request, limit int) artist.GlobalHistoryFilter {
	q := req.URL.Query()
	sources := parseFilterValues(q["source"])
	exactSources, sourcePrefixes := splitSourceFilters(sources)

	return artist.GlobalHistoryFilter{
		ArtistID:       q.Get("artist_id"),
		Fields:         parseFilterValues(q["field"]),
		Sources:        exactSources,
		SourcePrefixes: sourcePrefixes,
		From:           parseTimeParam(req, "from"),
		To:             parseTimeParam(req, "to"),
		Limit:          limit,
		Offset:         intQuery(req, "offset", 0),
	}
}

// resolveShowingCount returns the count to render in the "Showing X of Y"
// counter on a revert response. The hint argument is the client-reported
// visible-row count from the undo button's hx-vals (DOM nodes matching the
// activity/history row prefix). When the hint is a positive integer not
// exceeding total, it is preferred over fallback because the browser URL
// does not change after Load-more (so server-side offset+limit underreports
// the actual visible count). Non-numeric, missing, or out-of-range hints
// fall back to the caller-supplied value, which is computed from offset+limit
// (activity feed) or len(changes)-1 (artist history tab) under the assumption
// that only the first page is loaded. The logger is used to record malformed
// hints at Debug level for diagnostic visibility without log spam. Both the
// activity and artist-tab render branches share this helper so the validation
// rules stay in one place.
func resolveShowingCount(req *http.Request, fallback, total int, logger *slog.Logger) int {
	// Normalize fallback bounds so callers can't accidentally produce a
	// "Showing N of M" string with N > M (or N < 0). The hint validation
	// below clamps the request value, but we also need to clamp the
	// fallback so a forgetful caller can't bypass that guarantee.
	if fallback < 0 {
		fallback = 0
	}
	if total >= 0 && fallback > total {
		fallback = total
	}
	v := req.FormValue("showing")
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		// A non-numeric hint indicates a client-side bug (stale JS or a
		// tampered request body). Log at Debug so the symptom is visible
		// in verbose logs without polluting normal output, and keep using
		// the fallback so the user still sees a counter.
		if logger != nil {
			logger.Debug("invalid showing hint in revert request",
				"value", v, "error", err)
		}
		return fallback
	}
	if n <= 0 || n > total {
		// A non-positive count is meaningless (the user clicked undo, so at
		// least one row was visible). A count exceeding total would render
		// "Showing N of M" with N > M, which is nonsense; reject and use
		// the fallback. Both cases also short-circuit a tampered hint.
		return fallback
	}
	return n
}

// buildGlobalFilterFromURL constructs a GlobalHistoryFilter from a full URL
// string (typically from HX-Current-URL). This preserves the active filters
// when rendering revert fragments so the "showing X of Y" counter stays
// consistent with the current feed view.
func buildGlobalFilterFromURL(rawURL string) artist.GlobalHistoryFilter {
	u, err := url.Parse(rawURL)
	if err != nil {
		return artist.GlobalHistoryFilter{}
	}
	q := u.Query()
	sources := parseFilterValues(q["source"])
	exactSources, sourcePrefixes := splitSourceFilters(sources)

	from := parseTimeValue(q.Get("from"), "from")
	to := parseTimeValue(q.Get("to"), "to")

	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}

	return artist.GlobalHistoryFilter{
		ArtistID:       q.Get("artist_id"),
		Fields:         parseFilterValues(q["field"]),
		Sources:        exactSources,
		SourcePrefixes: sourcePrefixes,
		From:           from,
		To:             to,
		Offset:         offset,
	}
}

// handleListArtistHistory returns paginated metadata change records for an artist.
// GET /api/v1/artists/{id}/history
func (r *Router) handleListArtistHistory(w http.ResponseWriter, req *http.Request) {
	if r.historyService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history service is not available"})
		return
	}

	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	// Verify the artist exists before returning history.
	if _, err := r.artistService.GetByID(req.Context(), artistID); err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artist not found"})
			return
		}
		r.logger.Error("failed to verify artist for history", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	userID := middleware.UserIDFromContext(req.Context())
	limit := r.getUserPageSize(req.Context(), userID, intQuery(req, "limit", 0))
	offset := intQuery(req, "offset", 0)
	if offset < 0 {
		offset = 0
	}

	changes, total, err := r.historyService.List(req.Context(), artistID, limit, offset)
	if err != nil {
		r.logger.Error("listing artist history", "artist_id", artistID, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Return an empty array instead of null when there are no changes.
	if changes == nil {
		changes = []artist.MetadataChange{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"changes": changes,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}

// handleArtistHistoryTab renders the history tab HTML fragment for HTMX.
// GET /artists/{id}/history/tab
func (r *Router) handleArtistHistoryTab(w http.ResponseWriter, req *http.Request) {
	artistID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	if r.historyService == nil {
		r.logger.Warn("history tab requested but history service is not configured", "artist_id", artistID)
		// History service not wired; render empty state.
		renderTempl(w, req, templates.ArtistHistoryTab(templates.HistoryTabData{
			ArtistID: artistID,
		}))
		return
	}

	// Verify the artist exists before loading history.
	if _, err := r.artistService.GetByID(req.Context(), artistID); err != nil {
		if errors.Is(err, artist.ErrNotFound) {
			http.Error(w, "artist not found", http.StatusNotFound)
			return
		}
		r.logger.Error("failed to verify artist for history tab", "artist_id", artistID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	userID := middleware.UserIDFromContext(req.Context())
	limit := r.getUserPageSize(req.Context(), userID, intQuery(req, "limit", 0))
	offset := intQuery(req, "offset", 0)

	changes, total, err := r.historyService.List(req.Context(), artistID, limit, offset)
	if err != nil {
		r.logger.Error("loading history tab", "artist_id", artistID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := templates.HistoryTabData{
		ArtistID: artistID,
		Changes:  changes,
		Total:    total,
		Limit:    limit,
		Offset:   offset,
	}

	// Load-more requests use a different template to append rows.
	if offset > 0 {
		renderTempl(w, req, templates.ArtistHistoryMoreRows(data))
		return
	}

	renderTempl(w, req, templates.ArtistHistoryTab(data))
}

// errRevertNotTrackable is returned by validateRevertable when the field is
// not tracked by the history system.
var errRevertNotTrackable = errors.New("field is not revertible")

// errRevertOfRevert is returned by validateRevertable when the caller attempts
// to revert an entry that was itself produced by a revert.
var errRevertOfRevert = errors.New("revert entries cannot be reverted")

// errRevertInvalidOldValue is returned by validateRevertable when the value
// the revert would write back is one the field itself does not accept (#3037).
// It is wrapped with the validator's own curated sentence, so the operator is
// told WHICH rule refused rather than only that something did.
var errRevertInvalidOldValue = errors.New("the previous value cannot be restored")

// validateRevertable returns an error if the change is ineligible for revert.
// It checks three conditions independently so tests can cover each in
// isolation: (a) the field must be tracked by the history system, (b) the
// source must not already be "revert" (reverting a revert would create
// infinite chains), and (c) the OLD VALUE must be one the field accepts.
//
// (c) is #3037. A revert writes change.OldValue back, so a row whose old value
// the field would refuse is not revertible in any useful sense: the only two
// outcomes are a refusal at write time or a write that damages the row.
// Service.UpdateField and Service.ClearField refuse such a value regardless --
// that is the guarantee those methods now carry -- but refusing HERE is what
// makes the operator's experience honest rather than merely safe. The single
// revert endpoint's eligibility answer and the blast-radius PREVIEW both
// consult this function, so the row is reported as unrestorable while the
// operator is still deciding, instead of only at commit time.
//
// The check delegates to artist.ValidateFieldUpdate rather than special-casing
// a field name, so a field that gains a rule tomorrow is covered without
// editing this function. It was DORMANT when it landed -- no field was both
// history-tracked and carrying a validation rule -- and #3037 made it LIVE by
// adding "name" and "sort_name" to trackableFields. "name" is the case the
// check was written for: it carries the empty-name and empty-identity-key
// rules, so a recorded change whose OldValue is empty or punctuation-only is
// now answered 400 here instead of reaching a write. Putting such a value back
// would blank artists.name, which is NOT NULL but carries no non-empty CHECK,
// so the blank would persist and every identity mechanism keyed on the name
// would stop matching the artist. Checked, not assumed -- the intersection of
// `sed -n '/^var trackableFields/,/^}/p' internal/artist/service.go` with the
// switch arms of ValidateFieldUpdate (name, musicbrainz_id) is exactly {name}:
// "sort_name" joined trackableFields in the same change but carries no rule,
// and "musicbrainz_id" carries a rule but is not tracked.
func validateRevertable(change *artist.MetadataChange) error {
	if !artist.IsTrackableField(change.Field) {
		return errRevertNotTrackable
	}
	if change.Source == "revert" {
		return errRevertOfRevert
	}
	if err := artist.ValidateFieldUpdate(change.Field, change.OldValue); err != nil {
		return fmt.Errorf("%w: %s", errRevertInvalidOldValue, fieldRefusalReason(err))
	}
	return nil
}

// fieldRefusalReason extracts the operator-facing sentence from a field
// validation refusal, for a message that will reach a client.
//
// It reads the typed error's Reason FIELD rather than the rendered error
// string, and that distinction is the point rather than a lint dodge:
// artist.FieldValidationError.Reason is a hand-written sentence chosen for an
// operator to read ("name cannot be empty"), while the rendered chain is
// whatever the wrapping happens to produce -- today the same string, tomorrow
// one carrying an artist id, a column name, or a driver message. Only the
// first is safe to put in a response body, so only the first is read here.
//
// An error that is not a *artist.FieldValidationError falls back to the
// generic sentinel text: unknown provenance means it is not shown.
func fieldRefusalReason(err error) string {
	var ve *artist.FieldValidationError
	if errors.As(err, &ve) && ve.Reason != "" {
		return ve.Reason
	}
	return artist.ErrInvalidFieldValue.Error()
}

// performRevert applies the revert mutation for a single metadata change.
// It injects "revert" as the history source and pre-assigns a deterministic
// change ID (returned as revertChangeID) via ContextWithHistoryID so the
// caller can fetch the resulting history row by ID without racing against
// concurrent writers to the same field. The returned revertChangeID is the
// ID pre-assigned to the new history row; callers should fetch it with
// GetByID after this call returns.
//
// changed is true when a real write (and history record) occurred, false when
// the revert was a no-op because the field already equalled OldValue. Callers
// can use changed to select an appropriate user-facing message.
//
// ClearField/UpdateField currently succeed silently when the artist ID does
// not exist (UPDATE affects zero rows). The ErrNotFound guards are defensive:
// they activate if the repo layer is updated to check RowsAffected.
//
// A revert whose OLD VALUE the field refuses is refused twice over (#3037):
// validateRevertable rejects the row before this function is reached, and the
// service method refuses it again if anything ever calls here without
// validating first. That second refusal arrives as a
// *artist.FieldValidationError, which writeRevertFailure classifies rather
// than reporting as a server fault. Two layers on purpose: the first is what
// the operator sees, the second is what makes the guarantee independent of the
// caller.
func (r *Router) performRevert(ctx context.Context, change *artist.MetadataChange) (revertChangeID string, changed bool, err error) {
	revertChangeID = uuid.New().String()
	ctx = artist.ContextWithSource(ctx, "revert")
	ctx = artist.ContextWithHistoryID(ctx, revertChangeID)

	if change.OldValue == "" {
		changed, err = r.artistService.ClearField(ctx, change.ArtistID, change.Field)
	} else {
		changed, err = r.artistService.UpdateField(ctx, change.ArtistID, change.Field, change.OldValue)
	}
	return revertChangeID, changed, err
}

// renderActivityRevertFragment renders the HTMX fragment for a revert that was
// triggered from the global activity feed. It fetches the artist name, applies
// the active source and date filters, and falls through to the generic fallback
// if the new revert row does not belong in the current view.
// Returns true when a rich fragment was written to w; false signals the caller
// should write the generic fallback instead.
func (r *Router) renderActivityRevertFragment(
	w http.ResponseWriter, req *http.Request,
	origChangeID string,
	revertChange *artist.MetadataChange,
) bool {
	artistRow, artistErr := r.artistService.GetByID(req.Context(), revertChange.ArtistID)
	if artistErr != nil {
		r.logger.Error("fetching artist for revert fragment",
			"artist_id", revertChange.ArtistID, "error", artistErr)
		return false
	}

	newChange := artist.MetadataChangeWithArtist{
		MetadataChange: *revertChange,
		ArtistName:     artistRow.Name,
	}

	// Rebuild the active filter from query params carried in HX-Current-URL
	// so the "showing X of Y" counter stays accurate relative to the feed.
	activeFilter := buildGlobalFilterFromURL(req.Header.Get("HX-Current-URL"))

	// If the active feed filter restricts sources and does not include
	// "revert", the new row is outside the current view. Skip injection to
	// avoid inserting a row that would not normally appear in the feed.
	// Also suppress when SourcePrefixes is non-empty: "revert" does not
	// match any provider:/rule: prefix pattern.
	allowsRevert := (len(activeFilter.Sources) == 0 && len(activeFilter.SourcePrefixes) == 0) ||
		sliceContains(activeFilter.Sources, "revert")
	if !allowsRevert {
		return false
	}

	// Guard against active date-range bounds: skip if the revert row falls
	// outside the from/to window.
	createdAt := newChange.CreatedAt
	if (!activeFilter.From.IsZero() && createdAt.Before(activeFilter.From)) ||
		(!activeFilter.To.IsZero() && createdAt.After(activeFilter.To)) {
		return false
	}

	userID := middleware.UserIDFromContext(req.Context())
	limit := r.getUserPageSize(req.Context(), userID, 0)
	activeFilter.Limit = limit
	_, total, listErr := r.historyService.ListGlobal(req.Context(), activeFilter)
	if listErr != nil {
		r.logger.Error("fetching global history for revert counter",
			"artist_id", revertChange.ArtistID, "error", listErr)
		return false
	}
	// Compute fallback from offset+limit. After Load-more the browser URL
	// does not push offset, so this only matches reality when no Load-more
	// clicks have occurred. The client-supplied hx-vals showing hint is
	// preferred when present (see resolveShowingCount).
	fallback := activeFilter.Offset + limit
	if fallback > total {
		fallback = total
	}
	showing := resolveShowingCount(req, fallback, total, r.logger)
	renderTempl(w, req, templates.ActivityRevertFragment(origChangeID, newChange, r.basePath, showing, total))
	return true
}

// renderArtistTabRevertFragment renders the HTMX fragment for a revert that
// was triggered from the artist detail page's history tab. It calls List once
// to get the page total for the counter and delegates to resolveShowingCount
// to honor the hx-vals showing hint when present.
// Returns true when a rich fragment was written to w.
func (r *Router) renderArtistTabRevertFragment(
	w http.ResponseWriter, req *http.Request,
	origChangeID string,
	origChange *artist.MetadataChange,
	revertChange *artist.MetadataChange,
) bool {
	userID := middleware.UserIDFromContext(req.Context())
	limit := r.getUserPageSize(req.Context(), userID, 0)
	changes, total, listErr := r.historyService.List(req.Context(), origChange.ArtistID, limit, 0)
	if listErr != nil {
		r.logger.Error("fetching revert confirmation", "change_id", origChangeID, "error", listErr)
		return false
	}
	// The fragment hides the reverted row and prepends the new revert row so
	// the visible count is unchanged (+1 prepended, -1 hidden). DB total grew
	// by exactly 1 and len(changes) from the first page overstates visible rows
	// by 1 when the list fits on one page. Compensate by subtracting 1
	// (clamped at 0). When Load-more has been used the hx-vals showing hint
	// overrides this fallback via resolveShowingCount.
	fallback := len(changes) - 1
	if fallback < 0 {
		fallback = 0
	}
	showing := resolveShowingCount(req, fallback, total, r.logger)
	renderTempl(w, req, templates.HistoryRevertFragment(origChangeID, *revertChange, showing, total))
	return true
}

// writeRevertFailure renders the complete response for a revert the service
// refused or could not perform. Split out of handleRevertHistory so the
// classification lives in one readable place; it always writes a response, so
// the caller returns immediately after calling it.
//
// The ORDER of the branches is by specificity, not by likelihood: each one
// exists because falling through to the generic 500 would tell the operator
// "revert failed" -- a server fault -- for a request that was refused on
// purpose and will be refused identically on every retry.
func (r *Router) writeRevertFailure(w http.ResponseWriter, req *http.Request,
	changeID string, change *artist.MetadataChange, err error,
) {
	if errors.Is(err, artist.ErrNotFound) {
		writeError(w, req, http.StatusNotFound, "artist not found")
		return
	}

	// The service refused the VALUE (#3037). DEFENSIVE, in the same sense as
	// the ErrNotFound guard above: it is unreachable while validateRevertable
	// runs ahead of it, because a history row's OldValue is immutable, so a
	// value that passed the eligibility check cannot have become invalid by
	// the time the write is attempted.
	//
	// It is kept because performRevert is a shared helper whose service calls
	// now CAN refuse, and the alternative fall-through reports a refused value
	// as a 500. A caller added later that reaches performRevert without the
	// eligibility gate gets the honest answer instead of a misleading one.
	//
	// The client-visible message is built from the refusal's curated Reason
	// (fieldRefusalReason), never the rendered error chain; the full error goes
	// to the server log on the line above it.
	if errors.Is(err, artist.ErrInvalidFieldValue) {
		r.logger.Warn("revert refused: the previous value is not one the field accepts",
			"change_id", changeID, "field", change.Field,
			"artist_id", change.ArtistID, "error", err)
		writeError(w, req, http.StatusBadRequest,
			errRevertInvalidOldValue.Error()+": "+fieldRefusalReason(err))
		return
	}

	// A name revert whose OLD value is an identity another artist now holds
	// (#3037). Undoing the very rename that de-duplicated the pair would
	// recreate the duplicate, so Service.UpdateField routes every name write
	// through the transactional collision guard and refuses. This branch is
	// LIVE, not defensive: the same change that added "name" to
	// trackableFields turned on the Undo affordance that reaches it.
	//
	// Unlike the two branches above, this one cannot be moved earlier into
	// validateRevertable. Whether a name collides is a property of the CURRENT
	// database, not of the immutable history row, so it can only be answered
	// by the guard inside the writing transaction.
	//
	// The response reuses writeNameCollisionRefusal, which is what a refused
	// manual rename already returns, so the operator sees ONE refusal for one
	// kind of refusal (409 Conflict, plus the HTMX fragment on an HTMX
	// request) rather than a second wording that can drift from the first.
	// It is handed change.OldValue because that is the name the revert tried
	// to write.
	//
	// The nil-Collision check is not ceremony: writeNameCollisionRefusal
	// dereferences that pointer, so a nil would panic the handler, which is
	// strictly worse than the 500 below. artist constructs this error only
	// with a non-nil Collision, and its own Error() method still guards the
	// nil case, so this branch declines to be the one place that assumes
	// otherwise. A nil falls through and is reported by the generic arm,
	// which logs the full error rather than swallowing it.
	var collisionErr *artist.NameCollisionError
	if errors.As(err, &collisionErr) && collisionErr.Collision != nil {
		r.writeNameCollisionRefusal(w, req, change.ArtistID, change.OldValue, collisionErr.Collision)
		return
	}

	r.logger.Error("performing revert", "change_id", changeID, "error", err)
	writeError(w, req, http.StatusInternalServerError, "revert failed")
}

// handleRevertHistory reverts a single metadata change by restoring the old value.
// POST /api/v1/history/{id}/revert
func (r *Router) handleRevertHistory(w http.ResponseWriter, req *http.Request) {
	if r.historyService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "history service is not available")
		return
	}

	changeID, ok := RequirePathParam(w, req, "id")
	if !ok {
		return
	}

	change, err := r.historyService.GetByID(req.Context(), changeID)
	if err != nil {
		if errors.Is(err, artist.ErrChangeNotFound) {
			writeError(w, req, http.StatusNotFound, "change not found")
			return
		}
		r.logger.Error("fetching change for revert", "change_id", changeID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "internal error")
		return
	}

	if err := validateRevertable(change); err != nil {
		writeError(w, req, http.StatusBadRequest, err.Error())
		return
	}

	revertChangeID, revertChanged, err := r.performRevert(req.Context(), change)
	if err != nil {
		r.writeRevertFailure(w, req, changeID, change, err)
		return
	}

	// Emit activity.recent so the next/ dashboard live activity rail
	// (M55 #1334) shows the revert without polling. Gate on revertChanged
	// so a no-op revert (field already at OldValue) does not inject a
	// spurious "reverted" entry into the activity rail.
	if revertChanged {
		r.publishActivityRecent("reverted", change.Field+" reverted", change.ArtistID)
	}

	// For HTMX requests (undo button click), return an HTML fragment showing
	// the new history entry. For plain API callers, return JSON.
	if !isHTMXRequest(req) {
		writeJSON(w, http.StatusOK, map[string]any{
			"reverted":  revertChanged,
			"noop":      !revertChanged,
			"change_id": changeID,
		})
		return
	}

	// Fetch the new revert history row by its pre-assigned ID. This avoids
	// the prior "find most recent revert for this field" pattern, which races
	// against any concurrent writer to the same field at the same instant
	// (e.g. a metadata refresh, an automated rule fix, or a parallel revert).
	revertChange, err := r.historyService.GetByID(req.Context(), revertChangeID)
	if err != nil {
		// A missing revert history row is an expected edge case: the revert
		// can become a no-op (e.g. the field already matched OldValue so
		// UpdateField/ClearField recorded nothing) or history recording was
		// best-effort. Log at Info rather than Error to avoid noise.
		if errors.Is(err, artist.ErrChangeNotFound) {
			r.logger.Info("revert change history row not found; rendering fallback fragment",
				"change_id", changeID, "revert_change_id", revertChangeID)
		} else {
			r.logger.Error("fetching revert change by id",
				"change_id", changeID, "revert_change_id", revertChangeID, "error", err)
		}
	}

	// Determine whether the undo was triggered from the activity page or the
	// artist history tab, then delegate to the appropriate render helper.
	//
	// This Contains check intentionally matches BOTH the stable "/activity" page
	// and the next/ "/next/activity" page (M55 #1772): both render the identical
	// shared activity surface (templates.ActivityBody / ActivityRevertFragment),
	// so the activity revert fragment is correct for either channel and no
	// channel-specific branch is needed. The next/ artist-detail page
	// (/next/artists/{id}) does NOT contain "/activity", so it correctly falls to
	// the artist-tab branch below. Locked by TestHandleRevertHistory_NextActivity.
	fromActivity := strings.Contains(req.Header.Get("HX-Current-URL"), "/activity")
	if revertChange != nil {
		if fromActivity {
			if r.renderActivityRevertFragment(w, req, changeID, revertChange) {
				return
			}
		} else {
			if r.renderArtistTabRevertFragment(w, req, changeID, change, revertChange) {
				return
			}
		}
	}

	// Fallback: the revert succeeded but we could not render the rich
	// confirmation fragment. Causes: (a) new row is outside the active feed
	// filter, (b) a lookup failed (already logged above), (c) the row is
	// genuinely missing, or (d) the revert was a no-op (field already matched
	// OldValue so no write occurred). Log level distinguishes the cases.
	var fallbackMsg string
	if !revertChanged {
		// No-op revert: field already equalled OldValue; nothing was written.
		r.logger.Info("revert was a no-op; field already at old value",
			"change_id", changeID, "revert_change_id", revertChangeID,
			"field", change.Field, "artist_id", change.ArtistID)
		fallbackMsg = "This field was already at the reverted value, nothing to change."
	} else if revertChange != nil {
		r.logger.Info("revert succeeded; fragment suppressed by active filter",
			"change_id", changeID, "revert_change_id", revertChangeID,
			"field", change.Field, "artist_id", change.ArtistID)
		fallbackMsg = "Change reverted. Refresh the page to see the updated entry."
	} else {
		r.logger.Warn("revert record not located after mutation, using fallback confirmation",
			"change_id", changeID, "revert_change_id", revertChangeID,
			"field", change.Field, "artist_id", change.ArtistID)
		fallbackMsg = "Change reverted. Refresh the page to see the updated entry."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<div class="border-l-2 border-amber-400 dark:border-amber-500 pl-4 py-2"><p class="text-sm text-amber-600 dark:text-amber-400">` + fallbackMsg + `</p></div>`))
}

// handleListGlobalHistory returns paginated metadata changes across all artists.
// GET /api/v1/history
func (r *Router) handleListGlobalHistory(w http.ResponseWriter, req *http.Request) {
	if r.historyService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history service is not available"})
		return
	}

	userID := middleware.UserIDFromContext(req.Context())
	filter := buildGlobalFilter(req, r.getUserPageSize(req.Context(), userID, intQuery(req, "limit", 0)))
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	changes, total, err := r.historyService.ListGlobal(req.Context(), filter)
	if err != nil {
		r.logger.Error("listing global history", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if changes == nil {
		changes = []artist.MetadataChangeWithArtist{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"changes": changes,
		"total":   total,
		"limit":   filter.Limit,
		"offset":  filter.Offset,
	})
}

// rebuildSourceFilters reconstructs the combined source list from a
// GlobalHistoryFilter by merging exact sources with wildcard-suffixed prefixes.
// This is the inverse of splitSourceFilters and is used when passing filter
// state back to templates.
func rebuildSourceFilters(filter artist.GlobalHistoryFilter) []string {
	all := make([]string, 0, len(filter.Sources)+len(filter.SourcePrefixes))
	all = append(all, filter.Sources...)
	for _, p := range filter.SourcePrefixes {
		all = append(all, p+"*")
	}
	return all
}

// buildActivityPageData loads the global activity feed at offset 0 and assembles
// the templates.ActivityPageData view model used by handleActivityPage. It
// returns ok==false only after it has already written an error response, so the
// caller must return immediately without rendering when ok is false.
func (r *Router) buildActivityPageData(w http.ResponseWriter, req *http.Request, userID string) (templates.ActivityPageData, bool) {
	filter := buildGlobalFilter(req, r.getUserPageSize(req.Context(), userID, 0))
	filter.Offset = 0 // activity page always starts at offset 0

	var changes []artist.MetadataChangeWithArtist
	var total int
	if r.historyService != nil {
		var err error
		changes, total, err = r.historyService.ListGlobal(req.Context(), filter)
		if err != nil {
			r.logger.Error("loading activity page", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return templates.ActivityPageData{}, false
		}
	}
	if changes == nil {
		changes = []artist.MetadataChangeWithArtist{}
	}

	return templates.ActivityPageData{
		Changes:        changes,
		Total:          total,
		Limit:          filter.Limit,
		Offset:         filter.Offset,
		BasePath:       r.basePath,
		FilterArtistID: filter.ArtistID,
		FilterFields:   filter.Fields,
		FilterSources:  rebuildSourceFilters(filter),
		FilterFrom:     filter.From,
		FilterTo:       filter.To,
	}, true
}

// handleActivityPage renders the global activity page.
// GET /activity
func (r *Router) handleActivityPage(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		r.renderLoginPage(w, req)
		return
	}

	data, ok := r.buildActivityPageData(w, req, userID)
	if !ok {
		return
	}
	renderTempl(w, req, templates.ActivityPage(r.assetsFor(req), data))
}

// handleActivityContent renders the activity list fragment for HTMX.
// GET /activity/content
func (r *Router) handleActivityContent(w http.ResponseWriter, req *http.Request) {
	userID := middleware.UserIDFromContext(req.Context())
	if userID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if r.historyService == nil {
		r.logger.Warn("activity content requested but history service is not configured")
		renderTempl(w, req, templates.ActivityContent(templates.ActivityPageData{Limit: r.getUserPageSize(req.Context(), userID, 0), BasePath: r.basePath}))
		return
	}

	filter := buildGlobalFilter(req, r.getUserPageSize(req.Context(), userID, intQuery(req, "limit", 0)))
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	changes, total, err := r.historyService.ListGlobal(req.Context(), filter)
	if err != nil {
		r.logger.Error("loading activity content", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if changes == nil {
		changes = []artist.MetadataChangeWithArtist{}
	}

	data := templates.ActivityPageData{
		Changes:        changes,
		Total:          total,
		Limit:          filter.Limit,
		Offset:         filter.Offset,
		BasePath:       r.basePath,
		FilterArtistID: filter.ArtistID,
		FilterFields:   filter.Fields,
		FilterSources:  rebuildSourceFilters(filter),
		FilterFrom:     filter.From,
		FilterTo:       filter.To,
	}

	// Load-more requests (offset > 0) return just the new rows + updated
	// load-more button, appending to the existing list.
	if filter.Offset > 0 {
		renderTempl(w, req, templates.ActivityMoreRows(data))
		return
	}
	renderTempl(w, req, templates.ActivityContent(data))
}
