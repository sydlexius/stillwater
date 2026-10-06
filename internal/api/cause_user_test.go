package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/logging/logtest"
	"github.com/sydlexius/stillwater/internal/provider"
)

// noopScraperExecutor is a provider.ScraperExecutor that scrapes nothing: it
// returns a zero result and no error. Tests that need a constructed
// orchestrator but never exercise FetchMetadata pass it so the executor
// argument is always present.
type noopScraperExecutor struct{}

// ScrapeAll returns an empty result and no error. It is for tests where the
// orchestrator needs an executor but the handler never fetches metadata; a test
// that starts reaching FetchMetadata sees an empty result, not ErrNoScraperExecutor.
func (noopScraperExecutor) ScrapeAll(_ context.Context, _, _, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	return &provider.FetchResult{}, nil
}

// A user-initiated request that reaches a provider is attributed to the user
// and to the route that was hit (#2784). Driven through a real ServeMux and
// each production auth wrapper into a real handler and a real orchestrator, so
// the assertion reads what the shared fetch point actually logged.
//
// The route has a base path and a wildcard so the registered PATTERN and the
// requested URL differ: the cause must name the route, never the concrete
// value a caller put in the path.
func TestUserRequest_AttributesProviderFetchToRoute(t *testing.T) {
	const (
		pattern  = "POST /base/api/v1/providers/{op}"
		concrete = "search-7f3a"
	)
	wrappers := map[string]func(http.HandlerFunc, func(http.Handler) http.Handler) http.HandlerFunc{
		"wrapAuth": wrapAuth, "wrapOptionalAuth": wrapOptionalAuth,
	}
	for name, wrap := range wrappers {
		t.Run(name, func(t *testing.T) {
			registry := provider.NewRegistry()
			registry.Register(&identifyStubProvider{
				name: provider.NameAudioDB, // needs no API key, so it is always available
				searchFn: func(context.Context, string) ([]provider.ArtistSearchResult, error) {
					return nil, errors.New("upstream exploded")
				},
			})
			logger, logs := logtest.NewJSONLogger()
			r := &Router{
				logger:       logger,
				orchestrator: provider.NewOrchestrator(registry, provider.NewSettingsService(newTestDB(t), nil), logger, nil, noopScraperExecutor{}),
			}
			passthrough := func(next http.Handler) http.Handler { return next }
			mux := http.NewServeMux()
			mux.HandleFunc(pattern, wrap(r.handleProviderSearch, passthrough))

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/base/api/v1/providers/"+concrete, strings.NewReader(`{"name":"Some Artist"}`)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}

			found := false
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var got struct {
					Msg   string `json:"msg"`
					Cause string `json:"cause"`
				}
				if err := json.Unmarshal([]byte(line), &got); err != nil || got.Msg != "provider search failed" {
					continue
				}
				found = true
				if want := "user:" + pattern; got.Cause != want {
					t.Errorf("provider fetch logged cause %q, want %q", got.Cause, want)
				}
				if strings.Contains(line, concrete) {
					t.Errorf("the record carries the requested path value %q; the cause must name the route pattern. Record: %s", concrete, line)
				}
			}
			// Precondition: the request reached the provider.
			if !found {
				t.Fatalf("no provider search record; the request never reached the fetch point. Log was:\n%s", logs.String())
			}
		})
	}
}
