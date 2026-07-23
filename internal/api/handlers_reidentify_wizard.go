package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/i18n"
	"github.com/sydlexius/stillwater/internal/provider"
	templates "github.com/sydlexius/stillwater/web/templates"
)

// reIdentifyWizardSessionTTL bounds how long an idle wizard session is kept.
// The review flow is intentionally interactive; a session that has been idle
// longer than this is assumed abandoned and its in-memory entry is released
// on the next access. The session store is a process-local map so no backing
// table or migration is required for this RC blocker.
const reIdentifyWizardSessionTTL = 30 * time.Minute

// wizardStepState tracks the lifecycle of a single re-identify wizard step's
// provider lookup. The single-field enum replaces the prior tri-state booleans
// (inFlight / ready / errored), which permitted representable-but-invalid
// combinations like ready=true with inFlight=true.
type wizardStepState int

const (
	wizardStepPending wizardStepState = iota // no fetch attempted yet
	wizardStepLoading                        // provider lookup in flight
	wizardStepReady                          // lookup completed; Candidates is authoritative
	wizardStepFailed                         // provider lookup returned an error; errMsg carries the sanitized text
)

// wizardDecision captures the user's choice on a single step. Empty string
// means "no decision yet"; the named values match the three terminal actions
// the wizard UI exposes.
type wizardDecision string

const (
	wizardDecisionNone     wizardDecision = ""
	wizardDecisionAccepted wizardDecision = "accepted"
	wizardDecisionSkipped  wizardDecision = "skipped"
	wizardDecisionDeclined wizardDecision = "declined"
)

// reIdentifyWizardStep captures the per-artist state of a wizard session.
// Candidates is populated lazily when the UI advances to this step (or when
// the previous step pre-fetches its successor) so that listing 200 artists
// does not burn 200 provider calls up front.
type reIdentifyWizardStep struct {
	ArtistID   string
	ArtistName string
	ArtistPath string
	// Decision records the user's action on this step. Written when the user
	// advances off a step so Save-and-exit can classify remaining artists as
	// "left in review".
	Decision wizardDecision
	// Candidates is the top-N provider matches for the artist. Nil means
	// "not fetched yet"; an empty slice means "fetched, no results".
	Candidates []ScoredCandidate
	// state tracks the lifecycle of this step's provider lookup; errMsg is
	// only meaningful when state == wizardStepFailed.
	state  wizardStepState
	errMsg string
}

// reIdentifyWizardSession holds the server-side state for one interactive
// review run. It is mutex-protected because the pre-fetch goroutine writes
// to future steps while the request handler reads the current step.
type reIdentifyWizardSession struct {
	mu      sync.Mutex
	ID      string
	Steps   []*reIdentifyWizardStep
	Created time.Time
	Updated time.Time
	// Totals for the completion summary; updated as decisions are recorded.
	Accepted int
	Skipped  int
	Declined int
	// SkippedAtStart records IDs the start handler had to drop because the
	// underlying artist record was missing or could not be loaded. The
	// wizard page renders a banner from these so a user who selected 50
	// artists and gets a wizard with 47 steps can see which 3 dropped and
	// why, instead of silently losing them.
	SkippedAtStart []SkippedWizardArtist
}

// SkippedWizardArtist names one artist the wizard start endpoint had to
// drop, together with a coarse reason classification suitable for client
// rendering. Reason is one of skippedReasonNotFound or skippedReasonLoadError
// so the UI can pick localized copy without parsing free-form text.
type SkippedWizardArtist struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

const (
	// skippedReasonNotFound is the SkippedWizardArtist.Reason value used
	// when the artist ID resolved cleanly to "no such record" (the request
	// referenced a stale ID or one the user no longer has permission to
	// see). The UI may treat this as benign.
	skippedReasonNotFound = "not_found"
	// skippedReasonLoadError is used for any other GetByID failure (DB
	// outage, decryption error, etc.). The UI should surface this more
	// loudly because it implies an environmental problem rather than a
	// stale selection.
	skippedReasonLoadError = "load_error"
)

// touch marks the session as recently used. Callers must hold s.mu.
func (s *reIdentifyWizardSession) touch() { s.Updated = time.Now() }

// reIdentifyWizardStore is the in-memory session registry. Sessions are keyed
// by an opaque 128-bit token delivered to the client. TTL is enforced lazily.
type reIdentifyWizardStore struct {
	mu       sync.Mutex
	sessions map[string]*reIdentifyWizardSession
}

func newReIdentifyWizardStore() *reIdentifyWizardStore {
	return &reIdentifyWizardStore{sessions: make(map[string]*reIdentifyWizardSession)}
}

func (s *reIdentifyWizardStore) create(steps []*reIdentifyWizardStep) (*reIdentifyWizardSession, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(buf)
	sess := &reIdentifyWizardSession{
		ID:      id,
		Steps:   steps,
		Created: time.Now(),
		Updated: time.Now(),
	}
	s.mu.Lock()
	s.sessions[id] = sess
	// Opportunistic GC: prune expired siblings while we hold the lock.
	cutoff := time.Now().Add(-reIdentifyWizardSessionTTL)
	for k, v := range s.sessions {
		v.mu.Lock()
		expired := v.Updated.Before(cutoff)
		v.mu.Unlock()
		if expired && k != id {
			delete(s.sessions, k)
		}
	}
	s.mu.Unlock()
	return sess, nil
}

func (s *reIdentifyWizardStore) get(id string) *reIdentifyWizardSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil {
		return nil
	}
	sess.mu.Lock()
	expired := sess.Updated.Before(time.Now().Add(-reIdentifyWizardSessionTTL))
	sess.mu.Unlock()
	if expired {
		delete(s.sessions, id)
		return nil
	}
	return sess
}

func (s *reIdentifyWizardStore) delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// projectWizardCandidates converts a step's ScoredCandidate slice into the
// flat view type consumed by the wizard template. Returns a typed nil when
// the background fetch has not yet populated the step, so the template's
// "loading" branch renders. Caller must hold sess.mu.
func projectWizardCandidates(step *reIdentifyWizardStep) []templates.WizardCandidateView {
	// A step in either the Ready (succeeded) or Failed (errored) terminal
	// states must render the "no more loading" branch of the template -- in
	// the pre-refactor code this was a single `ready=true` flag set on both
	// success and error paths, and Candidates being nil/empty drove the
	// "no candidates" render. Treat both terminal states the same here; the
	// Failed-state branch is then rendered by the dedicated error banner
	// path in the template (driven by buildWizardStepData populating
	// Errored / ErrMsg from the step state).
	if step.state != wizardStepReady && step.state != wizardStepFailed {
		return nil
	}
	out := make([]templates.WizardCandidateView, 0, len(step.Candidates))
	for i := range step.Candidates {
		c := &step.Candidates[i]
		pct := 0
		switch {
		case c.AlbumComparison != nil:
			pct = c.AlbumComparison.MatchPercent
		case c.Confidence > 0:
			pct = int(c.Confidence * 100)
		}
		out = append(out, templates.WizardCandidateView{
			Name:           c.Name,
			MBID:           c.MusicBrainzID,
			Origin:         c.Origin,
			Disambiguation: c.Disambiguation,
			ConfidencePct:  pct,
		})
	}
	return out
}

// reIdentifyWizardStartRequest is the JSON body accepted by the start endpoint.
type reIdentifyWizardStartRequest struct {
	IDs []string `json:"ids"`
}

// handleReIdentifyWizardStart creates a new wizard session over the supplied
// artist IDs and returns the session ID plus the index of the first step.
// Uses the same ID validation and cap as handleBulkAction so a hostile caller
// cannot smuggle thousands of IDs past the public cap on /bulk-actions.
//
// POST /api/v1/artists/re-identify/wizard
func (r *Router) handleReIdentifyWizardStart(w http.ResponseWriter, req *http.Request) {
	if r.artistService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "artist service not configured")
		return
	}
	var body reIdentifyWizardStartRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, req, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, req, http.StatusBadRequest, "ids must be a non-empty list")
		return
	}
	if len(body.IDs) > MaxBulkActionIDs {
		writeError(w, req, http.StatusBadRequest, "too many ids")
		return
	}
	steps, skipped, badID := r.buildWizardStartSteps(req.Context(), body.IDs)
	if badID {
		writeError(w, req, http.StatusBadRequest, "invalid id format")
		return
	}
	if len(steps) == 0 {
		writeError(w, req, http.StatusBadRequest, "no valid artists")
		return
	}

	sess, err := r.reIdentifyWizardStore.create(steps)
	if err != nil {
		r.logger.Error("reidentify wizard: session create", "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to start wizard")
		return
	}
	// Persist the skipped set on the session so the wizard page can render
	// a banner when the user lands on step 0. The JSON response below is
	// for API consumers; the browser flow currently fetch()es then
	// redirects so it never displays the response body itself.
	if len(skipped) > 0 {
		sess.mu.Lock()
		sess.SkippedAtStart = skipped
		sess.mu.Unlock()
	}

	// Pre-fetch the first step's candidates synchronously-ish so the
	// initial render does not show a spinner. A background goroutine
	// primes the second step concurrently.
	bgCtx := context.WithoutCancel(req.Context())
	r.ensureWizardCandidates(bgCtx, sess, 0)
	go r.ensureWizardCandidates(bgCtx, sess, 1)

	// skipped_ids carries the bare ID list for callers that only need to
	// know "what disappeared"; skipped_errors pairs each ID with a coarse
	// reason class so a richer client can render distinct messaging for
	// "not found" vs "load failed".
	skippedIDs := make([]string, 0, len(skipped))
	for _, s := range skipped {
		skippedIDs = append(skippedIDs, s.ID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"session_id":     sess.ID,
		"total":          len(steps),
		"index":          0,
		"skipped_ids":    skippedIDs,
		"skipped_errors": skipped,
	})
}

// buildWizardStartSteps validates and resolves the requested artist IDs into
// wizard steps. Invalid IDs (failing idPattern) abort with badID=true so the
// caller can return 400; otherwise IDs that resolve to ErrNotFound or any
// other GetByID error are accumulated into skipped with a reason class. The
// loop is extracted so the per-ID classification can be unit-tested without
// standing up the full HTTP handler.
func (r *Router) buildWizardStartSteps(ctx context.Context, ids []string) (steps []*reIdentifyWizardStep, skipped []SkippedWizardArtist, badID bool) {
	seen := make(map[string]struct{}, len(ids))
	steps = make([]*reIdentifyWizardStep, 0, len(ids))
	// Pre-allocate skipped as an empty slice (not nil) so the JSON response
	// always serializes an array, satisfying the OpenAPI `required` contract
	// for skipped_ids and skipped_errors on the start response.
	skipped = make([]SkippedWizardArtist, 0, len(ids))
	for _, id := range ids {
		if !idPattern.MatchString(id) {
			return nil, nil, true
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		a, err := r.artistService.GetByID(ctx, id)
		if err != nil {
			if errors.Is(err, artist.ErrNotFound) {
				skipped = append(skipped, SkippedWizardArtist{ID: id, Reason: skippedReasonNotFound})
				continue
			}
			r.logger.Warn("reidentify wizard: load artist failed", "id", id, "error", err)
			skipped = append(skipped, SkippedWizardArtist{ID: id, Reason: skippedReasonLoadError})
			continue
		}
		steps = append(steps, &reIdentifyWizardStep{
			ArtistID:   a.ID,
			ArtistName: a.Name,
			ArtistPath: a.Path,
		})
	}
	return steps, skipped, false
}

// handleReIdentifyWizardStep returns the HTML fragment for a single step in
// the wizard. The step's candidates are fetched on demand if not already
// cached, and the next step's candidates are kicked off in the background
// to hide provider latency behind the user's decision time.
//
// GET /artists/re-identify/wizard/{sid}/step/{idx}
func (r *Router) handleReIdentifyWizardStep(w http.ResponseWriter, req *http.Request) {
	// Render the login page for unauthenticated visitors. wrapOptionalAuth
	// passes the request through; the handler is responsible for the check.
	if middleware.UserIDFromContext(req.Context()) == "" {
		r.renderLoginPage(w, req)
		return
	}

	sid := req.PathValue("sid")
	sess := r.reIdentifyWizardStore.get(sid)
	if sess == nil {
		writeError(w, req, http.StatusNotFound, "wizard session not found or expired")
		return
	}
	idx, err := strconv.Atoi(req.PathValue("idx"))
	if err != nil || idx < 0 {
		writeError(w, req, http.StatusBadRequest, "invalid step index")
		return
	}
	sess.mu.Lock()
	total := len(sess.Steps)
	sess.touch()
	sess.mu.Unlock()
	if idx >= total {
		// Finished: render the completion summary and release the session.
		r.renderWizardDone(w, req, sess)
		return
	}
	bgCtx := context.WithoutCancel(req.Context())
	r.ensureWizardCandidates(bgCtx, sess, idx)
	// Pre-fetch next step so user's think time overlaps provider latency.
	go r.ensureWizardCandidates(bgCtx, sess, idx+1)

	sess.mu.Lock()
	step := sess.Steps[idx]
	data := buildWizardStepData(req.Context(), sess, step, idx, total)
	// Only surface the start-time skipped list on the initial full-page
	// landing. Subsequent HTMX swaps replace just #wizard-body so showing
	// the banner there would be inert; suppress it on intra-wizard
	// navigation too so the user does not see the same warning on every
	// step.
	if !isHTMXRequest(req) && idx == 0 {
		data.SkippedAtStart = projectSkippedAtStart(sess.SkippedAtStart)
	}
	sess.mu.Unlock()
	if isHTMXRequest(req) {
		renderTempl(w, req, templates.ReIdentifyWizardStep(data))
		return
	}
	renderTempl(w, req, templates.ReIdentifyWizardPage(r.assets(), data))
}

// buildWizardStepData assembles the per-step view model from a step's
// terminal state. Failed steps populate Errored + ErrMsg so the template
// renders the error banner instead of the ambiguous "no candidates" line;
// Ready steps populate Candidates via the existing projection. Caller must
// hold sess.mu.
func buildWizardStepData(ctx context.Context, sess *reIdentifyWizardSession, step *reIdentifyWizardStep, idx, total int) templates.ReIdentifyWizardStepData {
	data := templates.ReIdentifyWizardStepData{
		SessionID:  sess.ID,
		Index:      idx,
		Total:      total,
		ArtistID:   step.ArtistID,
		ArtistName: step.ArtistName,
		Candidates: projectWizardCandidates(step),
	}
	if step.state == wizardStepFailed {
		data.Errored = true
		data.ErrMsg = step.errMsg
		if data.ErrMsg == "" {
			// Defensive fallback: a future code path that flips state to
			// Failed without populating errMsg would otherwise render the
			// banner heading with no body text. The translated copy
			// matches what ensureWizardCandidates writes on fetch failure
			// today; localizing it keeps non-English locales consistent.
			data.ErrMsg = i18n.TFromCtx(ctx).T("artists.bulk.reidentify.wizard.error.fallback")
		}
	}
	return data
}

// projectSkippedAtStart converts the handler-owned SkippedWizardArtist slice
// into the flat view type the template consumes. Returns nil for the empty
// case so the template's len() guard short-circuits cleanly. Caller must
// hold sess.mu.
func projectSkippedAtStart(skipped []SkippedWizardArtist) []templates.SkippedArtistView {
	if len(skipped) == 0 {
		return nil
	}
	out := make([]templates.SkippedArtistView, 0, len(skipped))
	for _, s := range skipped {
		out = append(out, templates.SkippedArtistView{ID: s.ID, Reason: s.Reason})
	}
	return out
}

// wizardDecisionRequest is the JSON body for accept/decline actions.
type wizardDecisionRequest struct {
	MBID      string `json:"mbid,omitempty"`
	DiscogsID string `json:"discogs_id,omitempty"`
}

// handleReIdentifyWizardAccept links the selected candidate and advances the
// wizard. POST /api/v1/artists/re-identify/wizard/{sid}/step/{idx}/accept
func (r *Router) handleReIdentifyWizardAccept(w http.ResponseWriter, req *http.Request) {
	sess, step, idx, ok := r.wizardStepFromRequest(w, req)
	if !ok {
		return
	}
	var body wizardDecisionRequest
	// Accept either JSON or form-encoded bodies. HTMX hx-vals default to
	// form encoding, and the bulk-action JS path uses JSON; supporting both
	// keeps the template free of content-type wrangling. Use
	// mime.ParseMediaType so charsets, case, and whitespace don't break the
	// branch selection (raw prefix match was fragile).
	mediaType, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if mediaType == "application/json" {
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, req, http.StatusBadRequest, "invalid JSON body")
			return
		}
	} else {
		body.MBID = req.FormValue("mbid")
		body.DiscogsID = req.FormValue("discogs_id")
	}
	if body.MBID == "" {
		writeError(w, req, http.StatusBadRequest, "mbid is required")
		return
	}
	a, err := r.artistService.GetByID(req.Context(), step.ArtistID)
	if err != nil {
		// Distinguish a missing artist from a transient DB/backend error.
		// Returning 404 for every failure hides real outages and makes
		// "artist not found" confusing during debugging.
		if errors.Is(err, artist.ErrNotFound) {
			writeError(w, req, http.StatusNotFound, "artist not found")
			return
		}
		r.logger.Error("reidentify wizard: accept GetByID", "artist_id", step.ArtistID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to load artist")
		return
	}
	a.MusicBrainzID = body.MBID
	if body.DiscogsID != "" {
		a.DiscogsID = body.DiscogsID
	}
	// The wizard advances to the next step either way; the skip flag is
	// reported by the link endpoints that answer with their own JSON, not by
	// this navigation response.
	if _, err := r.autoLinkAndRefresh(req.Context(), a); err != nil {
		r.logger.Error("reidentify wizard: accept failed", "artist_id", a.ID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to link artist")
		return
	}
	sess.mu.Lock()
	applyDecision(sess, step, wizardDecisionAccepted)
	sess.touch()
	sess.mu.Unlock()
	r.advanceWizard(w, req, sess, idx)
}

// handleReIdentifyWizardSkip leaves the artist unchanged and advances.
// POST /api/v1/artists/re-identify/wizard/{sid}/step/{idx}/skip
func (r *Router) handleReIdentifyWizardSkip(w http.ResponseWriter, req *http.Request) {
	sess, step, idx, ok := r.wizardStepFromRequest(w, req)
	if !ok {
		return
	}
	sess.mu.Lock()
	applyDecision(sess, step, wizardDecisionSkipped)
	sess.touch()
	sess.mu.Unlock()
	r.advanceWizard(w, req, sess, idx)
}

// handleReIdentifyWizardRetry re-issues the provider lookup for a step the
// user retried from the error banner, then renders the step fragment back
// into #wizard-body. ensureWizardCandidates already treats a Failed step as
// re-fetchable (it only short-circuits on Ready or Loading) and clears the
// prior errMsg as part of claiming the work, so this handler is a thin
// orchestration wrapper: trigger the fetch, render the new state.
// POST /api/v1/artists/re-identify/wizard/{sid}/step/{idx}/retry
func (r *Router) handleReIdentifyWizardRetry(w http.ResponseWriter, req *http.Request) {
	sess, step, idx, ok := r.wizardStepFromRequest(w, req)
	if !ok {
		return
	}
	sess.mu.Lock()
	sess.touch()
	sess.mu.Unlock()

	bgCtx := context.WithoutCancel(req.Context())
	r.ensureWizardCandidates(bgCtx, sess, idx)

	sess.mu.Lock()
	total := len(sess.Steps)
	data := buildWizardStepData(req.Context(), sess, step, idx, total)
	sess.mu.Unlock()
	renderTempl(w, req, templates.ReIdentifyWizardStep(data))
}

// handleReIdentifyWizardDecline marks the artist as explicitly unmatched
// (the user confirmed none of the candidates apply). Currently this is
// recorded on the session only; hooking it up to a persistent "no match"
// flag on the artist is tracked separately.
// POST /api/v1/artists/re-identify/wizard/{sid}/step/{idx}/decline
func (r *Router) handleReIdentifyWizardDecline(w http.ResponseWriter, req *http.Request) {
	sess, step, idx, ok := r.wizardStepFromRequest(w, req)
	if !ok {
		return
	}
	sess.mu.Lock()
	applyDecision(sess, step, wizardDecisionDeclined)
	sess.touch()
	sess.mu.Unlock()
	r.advanceWizard(w, req, sess, idx)
}

// applyDecision makes wizard decisions idempotent across Back/retry. The
// session mutex must be held. Revisiting a step with the same decision is a
// no-op; changing decisions normalizes counters so the totals always match
// the set of step.Decision values.
func applyDecision(sess *reIdentifyWizardSession, step *reIdentifyWizardStep, next wizardDecision) {
	if step.Decision == next {
		return
	}
	switch step.Decision {
	case wizardDecisionAccepted:
		if sess.Accepted > 0 {
			sess.Accepted--
		}
	case wizardDecisionSkipped:
		if sess.Skipped > 0 {
			sess.Skipped--
		}
	case wizardDecisionDeclined:
		if sess.Declined > 0 {
			sess.Declined--
		}
	case wizardDecisionNone:
		// No prior decision to decrement; counters stay as-is.
	}
	step.Decision = next
	switch next {
	case wizardDecisionAccepted:
		sess.Accepted++
	case wizardDecisionSkipped:
		sess.Skipped++
	case wizardDecisionDeclined:
		sess.Declined++
	case wizardDecisionNone:
		// Callers do not pass None as the next decision; the type permits
		// it so the exhaustive linter is satisfied without a default arm.
	}
}

// handleReIdentifyWizardSaveExit ends the wizard early and leaves any
// not-yet-reviewed artists on the existing bulk-identify review queue so the
// user can come back to them from the main identify flow.
// POST /api/v1/artists/re-identify/wizard/{sid}/save-exit
func (r *Router) handleReIdentifyWizardSaveExit(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("sid")
	sess := r.reIdentifyWizardStore.get(sid)
	if sess == nil {
		writeError(w, req, http.StatusNotFound, "wizard session not found")
		return
	}
	// Push remaining undecided steps onto the existing bulk-identify review
	// queue (reuse of the queue means the main Identify page picks them up
	// without new storage). If no bulk-identify progress exists, spin one up
	// so the queue has a home.
	r.identifyMu.Lock()
	if r.identifyProgress == nil {
		r.identifyProgress = &IdentifyProgress{Status: "completed"}
	}
	progress := r.identifyProgress
	r.identifyMu.Unlock()

	sess.mu.Lock()
	leftover := 0
	for _, step := range sess.Steps {
		if step.Decision != wizardDecisionNone {
			continue
		}
		// Push every undecided artist onto the leftover queue, regardless
		// of whether candidate pre-fetch completed, hit an error, or
		// returned zero results. Previously this filtered on len>0, which
		// silently dropped artists whose fetch was still in flight when
		// the user clicked Save-and-exit. The main identify page will
		// re-query for any step with nil/empty Candidates.
		queued := step.Candidates
		if queued == nil {
			queued = []ScoredCandidate{}
		}
		progress.mu.Lock()
		progress.ReviewQueue = append(progress.ReviewQueue, IdentifyCandidate{
			ArtistID:   step.ArtistID,
			ArtistName: step.ArtistName,
			ArtistPath: step.ArtistPath,
			Tier:       "wizard",
			Candidates: queued,
		})
		progress.mu.Unlock()
		leftover++
	}
	acc, skip, dec := sess.Accepted, sess.Skipped, sess.Declined
	sid2 := sess.ID
	sess.mu.Unlock()
	r.reIdentifyWizardStore.delete(sid2)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "saved",
		"accepted": acc,
		"skipped":  skip,
		"declined": dec,
		"leftover": leftover,
	})
}

// wizardStepFromRequest resolves the {sid}/{idx} path pair to a live session
// and step, writing an appropriate error response on failure. Returns ok=false
// when the caller must not continue.
func (r *Router) wizardStepFromRequest(w http.ResponseWriter, req *http.Request) (*reIdentifyWizardSession, *reIdentifyWizardStep, int, bool) {
	if r.artistService == nil {
		writeError(w, req, http.StatusServiceUnavailable, "artist service not configured")
		return nil, nil, 0, false
	}
	sid := req.PathValue("sid")
	idx, err := strconv.Atoi(req.PathValue("idx"))
	if err != nil || idx < 0 {
		writeError(w, req, http.StatusBadRequest, "invalid step index")
		return nil, nil, 0, false
	}
	sess := r.reIdentifyWizardStore.get(sid)
	if sess == nil {
		writeError(w, req, http.StatusNotFound, "wizard session not found or expired")
		return nil, nil, 0, false
	}
	sess.mu.Lock()
	if idx >= len(sess.Steps) {
		sess.mu.Unlock()
		writeError(w, req, http.StatusBadRequest, "step index out of range")
		return nil, nil, 0, false
	}
	step := sess.Steps[idx]
	sess.mu.Unlock()
	return sess, step, idx, true
}

// advanceWizard renders the next step (or the completion summary if the
// current step was the last one). HTMX callers get a fragment; non-HTMX
// callers get a JSON pointer they can follow.
func (r *Router) advanceWizard(w http.ResponseWriter, req *http.Request, sess *reIdentifyWizardSession, idx int) {
	next := idx + 1
	sess.mu.Lock()
	total := len(sess.Steps)
	sess.mu.Unlock()
	if next >= total {
		if isHTMXRequest(req) {
			r.renderWizardDone(w, req, sess)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "done", "session_id": sess.ID})
		return
	}
	if isHTMXRequest(req) {
		bgCtx := context.WithoutCancel(req.Context())
		r.ensureWizardCandidates(bgCtx, sess, next)
		go r.ensureWizardCandidates(bgCtx, sess, next+1)
		sess.mu.Lock()
		step := sess.Steps[next]
		data := buildWizardStepData(req.Context(), sess, step, next, total)
		sess.mu.Unlock()
		renderTempl(w, req, templates.ReIdentifyWizardStep(data))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "advanced", "index": next})
}

// renderWizardDone renders the completion summary and tears down the session.
func (r *Router) renderWizardDone(w http.ResponseWriter, req *http.Request, sess *reIdentifyWizardSession) {
	sess.mu.Lock()
	data := templates.ReIdentifyWizardDoneData{
		SessionID: sess.ID,
		Total:     len(sess.Steps),
		Accepted:  sess.Accepted,
		Skipped:   sess.Skipped,
		Declined:  sess.Declined,
	}
	sess.mu.Unlock()
	r.reIdentifyWizardStore.delete(sess.ID)
	if isHTMXRequest(req) {
		renderTempl(w, req, templates.ReIdentifyWizardDone(data))
		return
	}
	renderTempl(w, req, templates.ReIdentifyWizardPageDone(r.assets(), data))
}

// ensureWizardCandidates fetches provider candidates for step idx if they
// have not been fetched yet. Safe to call multiple times and from multiple
// goroutines; the per-step fetched flag is guarded by the session mutex.
func (r *Router) ensureWizardCandidates(ctx context.Context, sess *reIdentifyWizardSession, idx int) {
	sess.mu.Lock()
	if idx < 0 || idx >= len(sess.Steps) {
		sess.mu.Unlock()
		return
	}
	step := sess.Steps[idx]
	if step.state == wizardStepReady || step.state == wizardStepLoading {
		sess.mu.Unlock()
		return
	}
	step.state = wizardStepLoading // claim first so concurrent callers bail out
	step.errMsg = ""               // clear any prior error so retry shows a clean state
	artistID := step.ArtistID
	artistName := step.ArtistName
	artistPath := step.ArtistPath
	sess.mu.Unlock()

	// Perform provider I/O without holding the session mutex. Collect the
	// outcome locally, then commit (candidates OR error) in a single
	// critical section with the ready flip. Readers must never see
	// ready=true with stale nil Candidates.
	var (
		candidates []ScoredCandidate
		fetchErr   error
	)
	switch r.orchestrator {
	case nil:
		fetchErr = errors.New("orchestrator not configured")
	default:
		a, err := r.artistService.GetByID(ctx, artistID)
		if err != nil {
			r.logger.Warn("reidentify wizard: load artist for candidates", "artist_id", artistID, "error", err)
			fetchErr = err
			break
		}
		// Prefer album-enriched Tier-2 scoring when the artist has local
		// albums so the wizard displays the same match-percent badges the
		// auto path uses. Falls back to name-only search when no album
		// data exists.
		localAlbums := artist.ListLocalAlbums(artistPath)
		searchName := artistName
		if len(localAlbums) > 0 && artistPath != "" {
			searchName = filepath.Base(artistPath)
		}
		results, statuses, serr := r.orchestrator.SearchForLinking(ctx, searchName, []provider.ProviderName{provider.NameMusicBrainz})
		if serr != nil {
			r.logger.Warn("reidentify wizard: provider search failed", "artist", a.Name, "error", serr)
			fetchErr = serr
			break
		}
		// Per-provider failures now arrive on `statuses` instead of `serr`
		// (see SearchForLinking in internal/provider/orchestrator.go). The
		// wizard's per-step search only queries one provider (MusicBrainz);
		// if it errored, every result is unreliable, so we keep the existing
		// "step failed" path to preserve the Retry-banner UX. A future
		// multi-provider wizard query would surface a partial-failure banner
		// instead -- see issue #1663.
		failedProviders := collectFailedProviderDisplayNames(statuses)
		if len(failedProviders) > 0 {
			fetchErr = errors.New("all queried providers errored during candidate lookup")
			break
		}
		if len(localAlbums) > 0 {
			candidates = r.enrichAndScoreTier2(ctx, results, localAlbums)
		} else {
			candidates = make([]ScoredCandidate, 0, len(results))
			for i := range results {
				res := &results[i]
				candidates = append(candidates, ScoredCandidate{
					ArtistSearchResult: *res,
					Confidence:         float64(res.Score) / 200.0,
					Reason:             "name match",
				})
			}
		}
		// Cap to top 10 for display. Provider search already scores these
		// so the tail is rarely useful and keeps the DOM under control.
		if len(candidates) > 10 {
			candidates = candidates[:10]
		}
	}

	// Commit the result atomically with the state transition. Store only a
	// sanitized, client-safe message on the step; full errors are logged
	// server-side via slog.Warn above. The Failed-state UI surface is
	// rendered by the dedicated error banner branch in the wizard template.
	sess.mu.Lock()
	if fetchErr != nil {
		step.state = wizardStepFailed
		step.errMsg = "Candidate lookup failed; retry or skip this artist"
	} else {
		step.Candidates = candidates
		step.state = wizardStepReady
	}
	sess.touch()
	sess.mu.Unlock()
}
