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

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	templates "github.com/sydlexius/stillwater/web/templates"
)

// reIdentifyWizardSessionTTL bounds how long an idle wizard session is kept.
// The review flow is intentionally interactive; a session that has been idle
// longer than this is assumed abandoned and its in-memory entry is released
// on the next access. The session store is a process-local map so no backing
// table or migration is required for this RC blocker.
const reIdentifyWizardSessionTTL = 30 * time.Minute

// reIdentifyWizardStep captures the per-artist state of a wizard session.
// Candidates is populated lazily when the UI advances to this step (or when
// the previous step pre-fetches its successor) so that listing 200 artists
// does not burn 200 provider calls up front.
type reIdentifyWizardStep struct {
	ArtistID   string
	ArtistName string
	ArtistPath string
	// Decision is one of "", "accepted", "skipped", "declined". Written when
	// the user advances off a step so Save-and-exit can classify remaining
	// artists as "left in review".
	Decision string
	// Candidates is the top-N provider matches for the artist. Nil means
	// "not fetched yet"; an empty slice means "fetched, no results".
	Candidates []ScoredCandidate
	// inFlight prevents concurrent pre-fetch and on-demand fetch from
	// double-calling the provider for the same step. ready is flipped once
	// the fetch either succeeds or fails so the template can distinguish
	// "still loading" from "fetched, no results". errored carries a terminal
	// error message so the UI can surface retry affordances instead of
	// rendering an ambiguous "no matches" state.
	inFlight bool
	ready    bool
	errored  bool
	errMsg   string
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
}

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
	if !step.ready {
		return nil
	}
	out := make([]templates.WizardCandidateView, 0, len(step.Candidates))
	for _, c := range step.Candidates {
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
			Country:        c.Country,
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
	seen := make(map[string]struct{}, len(body.IDs))
	steps := make([]*reIdentifyWizardStep, 0, len(body.IDs))
	for _, id := range body.IDs {
		if !idPattern.MatchString(id) {
			writeError(w, req, http.StatusBadRequest, "invalid id format")
			return
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		a, err := r.artistService.GetByID(req.Context(), id)
		if err != nil {
			if errors.Is(err, artist.ErrNotFound) {
				continue
			}
			r.logger.Warn("reidentify wizard: load artist failed", "id", id, "error", err)
			continue
		}
		steps = append(steps, &reIdentifyWizardStep{
			ArtistID:   a.ID,
			ArtistName: a.Name,
			ArtistPath: a.Path,
		})
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

	// Pre-fetch the first step's candidates synchronously-ish so the
	// initial render does not show a spinner. A background goroutine
	// primes the second step concurrently.
	bgCtx := context.WithoutCancel(req.Context())
	r.ensureWizardCandidates(bgCtx, sess, 0)
	go r.ensureWizardCandidates(bgCtx, sess, 1)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"session_id": sess.ID,
		"total":      len(steps),
		"index":      0,
	})
}

// handleReIdentifyWizardStep returns the HTML fragment for a single step in
// the wizard. The step's candidates are fetched on demand if not already
// cached, and the next step's candidates are kicked off in the background
// to hide provider latency behind the user's decision time.
//
// GET /artists/re-identify/wizard/{sid}/step/{idx}
func (r *Router) handleReIdentifyWizardStep(w http.ResponseWriter, req *http.Request) {
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
	data := templates.ReIdentifyWizardStepData{
		SessionID:  sess.ID,
		Index:      idx,
		Total:      total,
		ArtistID:   step.ArtistID,
		ArtistName: step.ArtistName,
		Candidates: projectWizardCandidates(step),
	}
	sess.mu.Unlock()
	if isHTMXRequest(req) {
		renderTempl(w, req, templates.ReIdentifyWizardStep(data))
		return
	}
	renderTempl(w, req, templates.ReIdentifyWizardPage(r.assets(), data))
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
	if err := r.autoLinkAndRefresh(req.Context(), a); err != nil {
		r.logger.Error("reidentify wizard: accept failed", "artist_id", a.ID, "error", err)
		writeError(w, req, http.StatusInternalServerError, "failed to link artist")
		return
	}
	sess.mu.Lock()
	applyDecision(sess, step, "accepted")
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
	applyDecision(sess, step, "skipped")
	sess.touch()
	sess.mu.Unlock()
	r.advanceWizard(w, req, sess, idx)
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
	applyDecision(sess, step, "declined")
	sess.touch()
	sess.mu.Unlock()
	r.advanceWizard(w, req, sess, idx)
}

// applyDecision makes wizard decisions idempotent across Back/retry. The
// session mutex must be held. Revisiting a step with the same decision is a
// no-op; changing decisions normalizes counters so the totals always match
// the set of step.Decision values.
func applyDecision(sess *reIdentifyWizardSession, step *reIdentifyWizardStep, next string) {
	if step.Decision == next {
		return
	}
	switch step.Decision {
	case "accepted":
		if sess.Accepted > 0 {
			sess.Accepted--
		}
	case "skipped":
		if sess.Skipped > 0 {
			sess.Skipped--
		}
	case "declined":
		if sess.Declined > 0 {
			sess.Declined--
		}
	}
	step.Decision = next
	switch next {
	case "accepted":
		sess.Accepted++
	case "skipped":
		sess.Skipped++
	case "declined":
		sess.Declined++
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
		if step.Decision != "" {
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
		data := templates.ReIdentifyWizardStepData{
			SessionID:  sess.ID,
			Index:      next,
			Total:      total,
			ArtistID:   step.ArtistID,
			ArtistName: step.ArtistName,
			Candidates: projectWizardCandidates(step),
		}
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
	if step.ready || step.inFlight {
		sess.mu.Unlock()
		return
	}
	step.inFlight = true // claim first so concurrent callers bail out
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
		results, serr := r.orchestrator.SearchForLinking(ctx, searchName, []provider.ProviderName{provider.NameMusicBrainz})
		if serr != nil {
			r.logger.Warn("reidentify wizard: provider search failed", "artist", a.Name, "error", serr)
			fetchErr = serr
			break
		}
		if len(localAlbums) > 0 {
			candidates = r.enrichAndScoreTier2(ctx, results, localAlbums)
		} else {
			candidates = make([]ScoredCandidate, 0, len(results))
			for _, res := range results {
				candidates = append(candidates, ScoredCandidate{
					ArtistSearchResult: res,
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

	// Commit the result atomically with the ready flip. Store only a
	// sanitized, client-safe message on the step; full errors are logged
	// server-side via slog.Warn above. A surface for the errored flag in
	// the wizard template is tracked as an M46.5 follow-up.
	sess.mu.Lock()
	step.inFlight = false
	if fetchErr != nil {
		step.errored = true
		step.errMsg = "candidate lookup failed; retry or skip this artist"
	} else {
		step.Candidates = candidates
	}
	step.ready = true
	sess.touch()
	sess.mu.Unlock()
}
