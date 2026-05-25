package api

// handlers_artist_duplicates.go -- handler for the "Possible duplicate artists"
// detection report.
//
// Route: GET {basePath}/reports/duplicates (canonical; was
// /settings/artist-duplicates pre-#1615, which now 301s here).
// Admin-only (reuses requireForeignAdmin).
//
// The page lists detected near-duplicate groups and exposes a per-group
// merge action that calls POST /api/v1/artists/merge (#1615). Detection
// runs fully in-memory (no stored column, no migration) via
// artist.DetectDuplicates.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/web/templates"
)

// duplicatesCountTTL bounds the load that sidebar polling places on the
// detector. With a 60s sidebar poll per active tab, this TTL means at most
// one DetectDuplicates run every 5 minutes regardless of tab count.
const duplicatesCountTTL = 5 * time.Minute

// duplicatesCountCache memoizes the most recent duplicate-group count so the
// sidebar badge endpoint does not re-run the full detector on every poll.
// Module-level (rather than Router-scoped) so the cache survives across
// hypothetical multi-router test setups; in production there is one Router.
type duplicatesCountCache struct {
	mu        sync.Mutex
	count     int
	expiresAt time.Time
}

var duplicatesCount duplicatesCountCache

// get returns the cached count when fresh; otherwise refreshes via fn and
// caches the result for duplicatesCountTTL. Concurrent callers serialize
// on mu so the refresh fires at most once per TTL window even under burst
// load.
func (c *duplicatesCountCache) get(ctx context.Context, fn func(context.Context) (int, error)) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.expiresAt) {
		return c.count, nil
	}
	n, err := fn(ctx)
	if err != nil {
		return 0, err
	}
	c.count = n
	c.expiresAt = time.Now().Add(duplicatesCountTTL)
	return n, nil
}

// invalidate drops the cached value, forcing the next get call to refresh.
// Exposed for tests; production code relies on TTL expiry.
func (c *duplicatesCountCache) invalidate() {
	c.mu.Lock()
	c.count = 0
	c.expiresAt = time.Time{}
	c.mu.Unlock()
}

// handleArtistDuplicatesPage renders /reports/duplicates. Admin-only.
func (r *Router) handleArtistDuplicatesPage(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}

	// r.db is the raw *sql.DB wired in during Router construction.  Using it
	// directly avoids any intermediate layer and keeps detection off the
	// Service.List / buildWhereClause path.
	if r.db == nil {
		renderTempl(w, req, templates.ArtistDuplicatesPage(r.assetsFor(req), templates.ArtistDuplicatesPageView{}))
		return
	}

	groups, err := artist.DetectDuplicates(req.Context(), r.db)
	if err != nil {
		r.logger.Error("detecting near-duplicate artists", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	view := buildArtistDuplicatesView(groups, r.lookupArticleMode(req))
	renderTempl(w, req, templates.ArtistDuplicatesPage(r.assetsFor(req), view))
}

// mergeRequestBody is the wire shape for POST /api/v1/artists/merge. The
// JSON tags are snake_case to match the rest of the public API surface.
type mergeRequestBody struct {
	SurvivorID string   `json:"survivor_id"`
	LoserIDs   []string `json:"loser_ids"`
	DryRun     bool     `json:"dry_run"`
}

// mergeConflictPayload mirrors artist.ConflictItem in snake_case for the
// JSON response. Defined locally so the public API can evolve independently
// of the internal struct layout.
type mergeConflictPayload struct {
	Name         string `json:"name"`
	SurvivorPath string `json:"survivor_path"`
	LoserPath    string `json:"loser_path"`
}

// mergeMovedPayload mirrors artist.MovedItem in snake_case.
type mergeMovedPayload struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

// mergeResultPayload mirrors artist.MergeResult in snake_case. Conflicts is
// omitted when empty so success responses do not carry the field.
type mergeResultPayload struct {
	DryRun           bool                   `json:"dry_run"`
	SurvivorID       string                 `json:"survivor_id"`
	SurvivorPath     string                 `json:"survivor_path"`
	SurvivorOverride bool                   `json:"survivor_override"`
	Moved            []mergeMovedPayload    `json:"moved,omitempty"`
	Conflicts        []mergeConflictPayload `json:"conflicts,omitempty"`
	Removed          []string               `json:"removed,omitempty"`
	Warnings         []string               `json:"warnings,omitempty"`
	LosersDeleted    []string               `json:"losers_deleted,omitempty"`
}

// handleArtistsMerge processes POST /api/v1/artists/merge. Admin-only via
// requireForeignAdmin (same gate as the duplicates view). Maps the
// orchestrator's sentinel errors to the documented HTTP status codes:
//
//	400 ErrMergeInvalidRequest  (malformed body, missing IDs, etc.)
//	409 ErrMergeInProgress       (concurrent merge running)
//	409 ErrMergeCollisions       (pre-flight collision halt; conflicts in body)
//	422 ErrMergeStaleGroup       (IDs no longer co-resolve to one group)
//	422 ErrMergeSurvivorMissing  (survivor id absent from the group)
//	423 ErrMergeLocked           (a group member is locked)
//	500 anything else            (server-side failure; details in logs)
func (r *Router) handleArtistsMerge(w http.ResponseWriter, req *http.Request) {
	if !r.requireForeignAdmin(w, req) {
		return
	}
	if r.artistService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "artist service not configured"})
		return
	}

	var body mergeRequestBody
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "message": "invalid JSON body"})
		return
	}

	mergeReq := artist.MergeRequest{
		SurvivorID:  body.SurvivorID,
		LoserIDs:    body.LoserIDs,
		DryRun:      body.DryRun,
		ArticleMode: r.lookupArticleMode(req),
	}

	result, err := r.artistService.MergeArtists(req.Context(), mergeReq)
	if err != nil {
		r.respondMergeError(w, err, result)
		return
	}

	// A successful real merge changes the duplicate-group set; drop the
	// sidebar's cached count so the next poll re-runs detection. Dry runs
	// don't mutate state, so the cache stays valid.
	if !body.DryRun {
		duplicatesCount.invalidate()
	}

	writeJSON(w, http.StatusOK, toMergeResultPayload(result))
}

// lookupArticleMode pulls the directory-rename rule's configured
// ArticleMode so survivor selection picks the same canonical basename the
// rule engine would. Best-effort: a missing rule, a missing rule service,
// or any lookup failure falls back to the empty string, which
// CanonicalDirName treats as "prefix" (the rule's own default).
//
// A real DB error is logged at warn (distinguishing it from "rule not
// configured" which is the silent rl==nil branch); the operator gets an
// observable signal when article-mode drives survivor selection in an
// unexpected direction because the lookup transiently failed.
func (r *Router) lookupArticleMode(req *http.Request) string {
	if r.ruleService == nil {
		return ""
	}
	rl, err := r.ruleService.GetByID(req.Context(), rule.RuleDirectoryNameMismatch)
	if err != nil {
		r.logger.Warn("merge: directory-rename rule lookup failed; defaulting article mode to prefix",
			"error", err)
		return ""
	}
	if rl == nil {
		return ""
	}
	return rl.Config.ArticleMode
}

// respondMergeError maps an orchestrator sentinel to the documented HTTP
// status. The MergeResult is included on 409 (collisions) so the caller
// gets the conflict list.
//
// Client-facing messages are fixed human strings; the raw err.Error() is
// logged via r.logger so operators can debug without leaking internal
// detail (wrapped error chains, file paths, etc.) to API callers. The
// 422 case splits ErrMergeStaleGroup and ErrMergeSurvivorMissing into
// separate error codes so the UI can show a specific message instead of
// conflating both as "stale group".
func (r *Router) respondMergeError(w http.ResponseWriter, err error, result *artist.MergeResult) {
	switch {
	case errors.Is(err, artist.ErrMergeInvalidRequest):
		r.logger.Info("artist merge: invalid request", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_request",
			"message": "invalid merge request",
		})
	case errors.Is(err, artist.ErrMergeInProgress):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "merge_in_progress"})
	case errors.Is(err, artist.ErrMergeCollisions):
		// API contract: when error=collisions, the conflicts array MUST
		// be present (even if empty). Initialize the payload with an
		// empty slice so the shape stays stable when result is nil.
		payload := map[string]any{
			"error":     "collisions",
			"conflicts": []mergeConflictPayload{},
		}
		if result != nil {
			payload["conflicts"] = toMergeConflictPayloads(result.Conflicts)
			payload["survivor_id"] = result.SurvivorID
			payload["survivor_path"] = result.SurvivorPath
		}
		writeJSON(w, http.StatusConflict, payload)
	case errors.Is(err, artist.ErrMergeSurvivorMissing):
		r.logger.Info("artist merge: survivor missing from group", "error", err)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error":   "survivor_missing",
			"message": "survivor id is not a member of the duplicate group; refresh duplicates and retry",
		})
	case errors.Is(err, artist.ErrMergeStaleGroup):
		r.logger.Info("artist merge: stale group", "error", err)
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error":   "stale_group",
			"message": "merge target is stale; refresh duplicates and retry",
		})
	case errors.Is(err, artist.ErrMergeLocked):
		r.logger.Info("artist merge: locked member", "error", err)
		writeJSON(w, http.StatusLocked, map[string]string{
			"error":   "locked",
			"message": "one or more artists are locked; unlock and retry",
		})
	default:
		r.logger.Error("merging near-duplicate artists", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "internal",
			"message": "see server logs",
		})
	}
}

func toMergeConflictPayloads(in []artist.ConflictItem) []mergeConflictPayload {
	out := make([]mergeConflictPayload, 0, len(in))
	for _, c := range in {
		out = append(out, mergeConflictPayload{Name: c.Name, SurvivorPath: c.SurvivorPath, LoserPath: c.LoserPath})
	}
	return out
}

func toMergeMovedPayloads(in []artist.MovedItem) []mergeMovedPayload {
	out := make([]mergeMovedPayload, 0, len(in))
	for _, m := range in {
		out = append(out, mergeMovedPayload{Name: m.Name, From: m.From, To: m.To})
	}
	return out
}

func toMergeResultPayload(r *artist.MergeResult) mergeResultPayload {
	if r == nil {
		return mergeResultPayload{}
	}
	return mergeResultPayload{
		DryRun:           r.DryRun,
		SurvivorID:       r.SurvivorID,
		SurvivorPath:     r.SurvivorPath,
		SurvivorOverride: r.SurvivorOverride,
		Moved:            toMergeMovedPayloads(r.Moved),
		Conflicts:        toMergeConflictPayloads(r.Conflicts),
		Removed:          r.Removed,
		Warnings:         r.Warnings,
		LosersDeleted:    r.LosersDeleted,
	}
}

// handleArtistDuplicatesCount returns an HTML fragment for the sidebar's
// Duplicates child link. Admin-only (sidebar callers from non-admin pages
// receive a 403; the sidebar omits the placeholder element for non-admins
// so a healthy session never makes the call).
//
// GET /api/v1/reports/duplicates/count
//
// Returns:
//   - empty body when there are no duplicate groups (HTMX innerHTML swap
//     leaves the parent <li> empty so the child disappears from the nav);
//   - an <a> link populated with the group count when count > 0.
//
// The detection result is cached at module scope for duplicatesCountTTL so
// that polling sidebars across many tabs collapse to at most one
// DetectDuplicates run per TTL window. The cache is invalidated on
// successful merges (handled in handleArtistsMerge).
func (r *Router) handleArtistDuplicatesCount(w http.ResponseWriter, req *http.Request) {
	// Admin gate: middleware.RoleFromContext is populated by wrapAuth.
	// Mirrors the gate enforced by requireForeignAdmin on the page handler.
	if middleware.RoleFromContext(req.Context()) != "administrator" {
		http.Error(w, "Forbidden: administrator role required", http.StatusForbidden)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	db := r.db
	count, err := duplicatesCount.get(req.Context(), func(ctx context.Context) (int, error) {
		return countDuplicateGroups(ctx, db)
	})
	if err != nil {
		// Fail-safe: log and emit an empty body so the sidebar simply
		// doesn't show the Duplicates child. Surfacing the error inline
		// would clutter every sidebar; the duplicates page itself surfaces
		// detector failures.
		r.logger.Warn("duplicates count refresh failed", "error", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
	if count <= 0 {
		return
	}

	label := html.EscapeString(i18n.TFromCtx(req.Context()).T("nav.reports.duplicates"))
	href := html.EscapeString(r.basePath + "/reports/duplicates")
	fmt.Fprintf(w, //nolint:errcheck // Best-effort HTTP write; client disconnect is not actionable
		`<a href="%s" class="sw-sidebar-link sw-sidebar-subnav-link" data-path="/reports/duplicates" aria-label="%s">`+
			`<span class="sw-sidebar-label">%s</span>`+
			`<span class="sw-sidebar-badge-pill">%d</span>`+
			`</a>`,
		href, label, label, count)
}

// countDuplicateGroups runs the duplicate detector and returns the group
// count. Split from the handler so the cache callback stays a pure function
// of (ctx, db) and so tests can drive the count without going through HTTP.
//
// A nil db (test seam) returns 0 with no error -- matches the page handler's
// behavior of rendering an empty view when the DB isn't wired.
func countDuplicateGroups(ctx context.Context, db *sql.DB) (int, error) {
	if db == nil {
		return 0, nil
	}
	groups, err := artist.DetectDuplicates(ctx, db)
	if err != nil {
		return 0, err
	}
	return len(groups), nil
}

// buildArtistDuplicatesView converts the detection result into the view model
// used by the template.  Extracted as a named function so tests can exercise
// the conversion logic independently.
//
// articleMode is the directory-rename rule's configured article handling
// ("prefix" / "suffix" / ""); it must match what the merge endpoint computes
// at submit time so the recommendation badge agrees with the server's
// survivor-override flag.
func buildArtistDuplicatesView(groups []artist.NearDuplicateGroup, articleMode string) templates.ArtistDuplicatesPageView {
	rows := make([]templates.ArtistDuplicateGroupRow, 0, len(groups))
	for _, g := range groups {
		recommendedID, recommendedReason := artist.ChooseSurvivor(g.Members, articleMode)
		members := make([]templates.ArtistDuplicateMember, 0, len(g.Members))
		for _, m := range g.Members {
			mem := templates.ArtistDuplicateMember{
				ID:   m.ID,
				Name: m.Name,
				Path: m.Path,
				MBID: m.MBID,
			}
			if m.ID == recommendedID {
				mem.Recommended = true
				mem.RecommendedReason = recommendedReason
			}
			members = append(members, mem)
		}
		rows = append(rows, templates.ArtistDuplicateGroupRow{
			Key:     g.Key,
			Reason:  g.Reason,
			Members: members,
		})
	}
	return templates.ArtistDuplicatesPageView{
		Groups: rows,
	}
}
