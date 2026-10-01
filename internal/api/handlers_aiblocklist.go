package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider/aiblock"
)

// aiBlocklistRefreshGap is the least time between two refresh attempts, so the
// manual button cannot be used to hammer the upstream host.
const aiBlocklistRefreshGap = 60 * time.Second

// aiBlocklistStatusResponse is the JSON shape of both AI-blocklist endpoints.
// It carries the source HOST only, never the full URL, and LastError is a
// generic sentence: the raw error can hold a URL with a query or a data path.
type aiBlocklistStatusResponse struct {
	Enabled     bool       `json:"enabled"`
	SourceHost  string     `json:"source_host,omitempty"`
	Loaded      bool       `json:"loaded"`
	Rules       int        `json:"rules"`
	LastFetch   *time.Time `json:"last_fetch,omitempty"`
	LastChecked *time.Time `json:"last_checked,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

func utcPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// sanitizeAIBlocklistError maps the store's raw error text to a sentence safe
// to show a client; the raw text goes to the server log.
func sanitizeAIBlocklistError(raw string) string {
	switch {
	case raw == "":
		return ""
	case strings.Contains(raw, "restart Stillwater to accept it"):
		return "The downloaded list was much smaller than the active one and was not used. Restart Stillwater to accept it."
	case strings.Contains(raw, "not cached"):
		return "The list is active but could not be saved to the cache."
	case strings.HasPrefix(raw, "cached list:"):
		return "The saved copy of the list was ignored."
	case strings.Contains(raw, "HTTP "):
		_, code, _ := strings.Cut(raw, "HTTP ")
		if n, err := strconv.Atoi(strings.TrimSpace(code)); err == nil {
			return "The list server answered with HTTP " + strconv.Itoa(n) + "."
		}
		return "The list could not be downloaded."
	case strings.HasPrefix(raw, "fetching list") || strings.HasPrefix(raw, "reading list") || strings.HasPrefix(raw, "building request"):
		return "The list could not be downloaded."
	}
	return "The downloaded list was rejected."
}

func aiBlocklistStatusOf(s *aiblock.Store) aiBlocklistStatusResponse {
	st := s.Status()
	return aiBlocklistStatusResponse{
		Enabled:     !st.Disabled,
		SourceHost:  s.SourceHost(),
		Loaded:      st.Loaded,
		Rules:       st.Rules,
		LastFetch:   utcPtr(st.LastFetch),
		LastChecked: utcPtr(st.LastChecked),
		LastError:   sanitizeAIBlocklistError(st.LastError),
	}
}

// handleAIBlocklistStatus serves GET /api/v1/images/ai-blocklist/status.
func (r *Router) handleAIBlocklistStatus(w http.ResponseWriter, req *http.Request) {
	s := aiblock.DefaultStore()
	if s == nil {
		writeJSON(w, http.StatusOK, aiBlocklistStatusResponse{})
		return
	}
	writeJSON(w, http.StatusOK, aiBlocklistStatusOf(s))
}

// handleAIBlocklistRefresh serves POST /api/v1/images/ai-blocklist/refresh: one
// refresh now, then the new status. A failed download is reported in the
// status (last_error), not as an HTTP error, since the request itself worked.
func (r *Router) handleAIBlocklistRefresh(w http.ResponseWriter, req *http.Request) {
	s := aiblock.DefaultStore()
	if s == nil || s.Status().Disabled {
		writeError(w, req, http.StatusConflict, "the AI image list download is turned off (SW_AI_BLOCKLIST_URL is empty)")
		return
	}
	// A client disconnect must not cancel the download: that would record a
	// false last_error and start the 429 lockout. The store client's own
	// timeout still bounds the fetch.
	ran, wait, err := s.RefreshIfDue(context.WithoutCancel(req.Context()), aiBlocklistRefreshGap)
	if !ran {
		secs := int(wait/time.Second) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeError(w, req, http.StatusTooManyRequests, "the list was refreshed moments ago; try again in "+strconv.Itoa(secs)+" seconds")
		return
	}
	if err != nil {
		r.logger.Warn("AI blocklist: manual refresh failed", slog.String("error", err.Error()))
	}
	writeJSON(w, http.StatusOK, aiBlocklistStatusOf(s))
}
