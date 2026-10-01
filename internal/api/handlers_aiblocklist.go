package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider/aiblock"
	"github.com/sydlexius/stillwater/web/templates"
)

// aiBlocklistRefreshGap is the least time between two refresh attempts, so the
// manual button cannot be used to hammer the upstream host.
const aiBlocklistRefreshGap = 60 * time.Second

// aiBlocklistStatusResponse is the JSON shape of both AI-blocklist endpoints.
// It never carries the full URL: source_host is the host, source is host plus
// path and source_repo is owner/repo for a GitHub list (none holds userinfo, a
// query or a fragment). LastError is a generic sentence: the raw error can hold
// a URL with a query or a data path.
type aiBlocklistStatusResponse struct {
	Enabled    bool   `json:"enabled"`
	SourceHost string `json:"source_host,omitempty"`
	// Source is host plus path (no userinfo, query or fragment); SourceRepo is
	// owner/repo for a GitHub-hosted list.
	Source      string     `json:"source,omitempty"`
	SourceRepo  string     `json:"source_repo,omitempty"`
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
	src, repo := s.SourceDisplay()
	return aiBlocklistStatusResponse{
		Source:      src,
		SourceRepo:  repo,
		Enabled:     !st.Disabled,
		SourceHost:  s.SourceHost(),
		Loaded:      st.Loaded,
		Rules:       st.Rules,
		LastFetch:   utcPtr(st.LastFetch),
		LastChecked: utcPtr(st.LastChecked),
		LastError:   sanitizeAIBlocklistError(st.LastError),
	}
}

// aiBlocklistView maps the store state to the Settings card's view. A nil
// store (never installed) reads as "download off".
func aiBlocklistView(s *aiblock.Store) templates.AIBlocklistView {
	if s == nil {
		return templates.AIBlocklistView{}
	}
	st := aiBlocklistStatusOf(s)
	v := templates.AIBlocklistView{
		Enabled:    st.Enabled,
		Loaded:     st.Loaded,
		Rules:      st.Rules,
		Source:     st.Source,
		SourceRepo: st.SourceRepo,
		LastError:  st.LastError,
	}
	if st.LastFetch != nil {
		v.LastFetch = *st.LastFetch
	}
	if st.LastChecked != nil {
		v.LastChecked = *st.LastChecked
	}
	return v
}

// refreshRefused answers a refused manual refresh. The JSON API gets the real
// 4xx; an HTMX click gets 200 with the card re-rendered plus the message,
// because HTMX does not swap a 4xx body and the click must never fail silently.
func (r *Router) refreshRefused(w http.ResponseWriter, req *http.Request, s *aiblock.Store, status int, msg, kind string, secs int) {
	if !isHTMXRequest(req) {
		writeError(w, req, status, msg)
		return
	}
	v := aiBlocklistView(s)
	v.NoticeKind, v.NoticeSecs, v.Refreshed = kind, secs, true
	renderTempl(w, req, templates.AIBlocklistBody(v))
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
		r.refreshRefused(w, req, s, http.StatusConflict, "the AI image list download is turned off (SW_AI_BLOCKLIST_URL is empty)", "disabled", 0)
		return
	}
	// A client disconnect must not cancel the download: that would record a
	// false last_error and start the 429 lockout. The store client's own
	// timeout still bounds the fetch.
	ran, wait, err := s.RefreshIfDue(context.WithoutCancel(req.Context()), aiBlocklistRefreshGap)
	if !ran {
		secs := int(wait/time.Second) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		if errors.Is(err, aiblock.ErrRefreshInFlight) {
			r.refreshRefused(w, req, s, http.StatusTooManyRequests, "a refresh is already in progress; try again in a moment", "in_progress", secs)
			return
		}
		r.refreshRefused(w, req, s, http.StatusTooManyRequests, "the list was refreshed moments ago; try again in "+strconv.Itoa(secs)+" seconds", "rate_limited", secs)
		return
	}
	if err != nil {
		r.logger.Warn("AI blocklist: manual refresh failed", slog.String("error", err.Error()))
	}
	if isHTMXRequest(req) {
		v := aiBlocklistView(s)
		v.Refreshed = true
		renderTempl(w, req, templates.AIBlocklistBody(v))
		return
	}
	writeJSON(w, http.StatusOK, aiBlocklistStatusOf(s))
}
