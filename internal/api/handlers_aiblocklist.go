package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/stillwater/internal/provider/aiblock"
)

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
