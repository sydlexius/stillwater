package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/conflict"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/web/templates"
)

// hasQualifyingConflictConnection reports whether the connection list
// contains at least one enabled emby/jellyfin/lidarr connection. The OOBE
// conflict pre-flight step is gated on this in addition to the library
// count: with no qualifying peers there is nothing to probe and the step
// would only display a meaningless "all clear" message.
func hasQualifyingConflictConnection(conns []connection.Connection) bool {
	for _, c := range conns {
		if !c.Enabled {
			continue
		}
		switch c.Type {
		case connection.TypeEmby, connection.TypeJellyfin, connection.TypeLidarr:
			return true
		}
	}
	return false
}

// handlePostOnboardingConflictStep renders the body of the OOBE conflict
// pre-flight step (#1184). It is HTMX-loaded by onboarding.templ when the
// user transitions into step 5 so the synchronous peer probe runs lazily
// rather than on every wizard page render.
//
// POST is used (instead of the more typical GET for a render-only fragment)
// because the handler mutates state on every call: it persists the
// completion marker and, with ?refresh=1, invalidates the detector cache.
// Routing it as POST keeps it inside CSRF protection -- a malicious page
// embedding `<img src=…/conflict-step?refresh=1>` would no longer be able
// to trigger a settings write or detector invalidation.
//
// Behavior:
//   - ?refresh=1 invalidates the detector cache before rendering, used by
//     the in-step "Re-check" button and after a per-connection
//     "Let Stillwater manage" toggle. Without it the cached ledger from
//     before remediation would still be returned and the user would see
//     stale state.
//   - Sets onboarding.conflict_check_completed_at on each successful render
//     so future iterations can short-circuit when the user navigates back
//     and forth without the peer state having changed. The value is
//     advisory; the body still re-renders on every transition because peer
//     reachability can change between steps.
//   - On detector misconfiguration renders an empty clean-state body so
//     the OOBE page sees a real swap (HTMX skips swap and afterSwap on
//     204 by default, which would leave the spinner up and the gate
//     stuck closed). The clean body sets ob-conflict-block-state to "0"
//     so Continue unlocks via the existing afterSwap sync.
//
// POST /api/v1/onboarding/conflict-step
func (r *Router) handlePostOnboardingConflictStep(w http.ResponseWriter, req *http.Request) {
	if r.conflictDetector == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.OnboardingConflictBody(templates.OnboardingConflictView{}).Render(req.Context(), w); err != nil {
			r.logger.Warn("rendering onboarding conflict body (no detector) failed", "error", err)
		}
		return
	}
	if req.URL.Query().Get("refresh") == "1" {
		r.conflictDetector.Invalidate()
	}
	// Force a fresh probe on the first render of the step too: Current()
	// returns cached state if the prior call was within the TTL window,
	// but for OOBE we want the most recent peer answer because the user
	// may have just enabled the connection in step 4.
	ledger := r.conflictDetector.Refresh(req.Context())

	probeErr := aggregateProbeError(ledger)
	view := templates.OnboardingConflictView{
		Banner: conflictBannerView(ledger),
		// When a probe error is shown the body renders the non-blocking
		// warning panel; clear Blocking so #ob-conflict-block does not
		// keep Continue disabled in an advisory state.
		Blocking:   probeErr == "" && ledger.BannerState() == "round_trip",
		ProbeError: probeErr,
	}

	// Persist a completion marker so future logic can detect that the
	// step has been visited at least once. Best-effort: render proceeds
	// even if the write fails so a transient settings-store error does
	// not leave the user stuck on a blank step.
	if r.db != nil {
		now := time.Now().UTC().Format(time.RFC3339)
		_, err := r.db.ExecContext(req.Context(),
			`INSERT INTO settings (key, value) VALUES ('onboarding.conflict_check_completed_at', ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, now)
		if err != nil {
			r.logger.Warn("persisting onboarding.conflict_check_completed_at failed", "error", err)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.OnboardingConflictBody(view).Render(req.Context(), w); err != nil {
		r.logger.Warn("rendering onboarding conflict body failed", "error", err)
	}
}

// aggregateProbeError returns a single-line summary of any per-connection
// probe failures present in the ledger, or "" when every probe succeeded.
// The OOBE body uses this to render a non-blocking "could not reach"
// notice in place of the per-state banner so the user is not gated on
// transient peer outages during first-time setup.
func aggregateProbeError(l conflict.Ledger) string {
	var failed []string
	for _, c := range l.Connections {
		if !c.Enabled {
			continue
		}
		if c.CheckErr != "" {
			failed = append(failed, c.ConnectionName)
		}
	}
	if len(failed) == 0 {
		return ""
	}
	if len(failed) == 1 {
		return "Could not reach " + failed[0] + " to probe its saver settings."
	}
	return "Could not reach: " + strings.Join(failed, ", ") + "."
}
