package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/logging/logtest"
	"github.com/sydlexius/stillwater/internal/provider"
)

// Twelve provider call sites bypass the shared fetch points that log the
// operation cause, so the failure line each of them already emitted said
// nothing about WHY the call was made (#2784). The API-package ones now append
// the cause to that existing line. Each row here drives a real handler through
// the real mux and the production wrapAuth with a provider that fails, then
// reads the record the handler actually logged.
//
// Rows that use a route with an {id} wildcard prove the cause names the
// registered PATTERN and never the concrete artist ID from the URL.
func TestCause_APIBypassFailureLines(t *testing.T) {
	const base = "/base"

	// albumDir returns an artist folder holding two album subdirectories, so
	// local album evidence is "found" and the album-comparison branch runs.
	albumDir := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		for _, alb := range []string{"Album One", "Album Two"} {
			if err := os.Mkdir(filepath.Join(dir, alb), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}
		return dir
	}
	newArtist := func(t *testing.T, svc *artist.Service, a *artist.Artist) *artist.Artist {
		t.Helper()
		if err := svc.Create(context.Background(), a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}
		return a
	}
	failRG := func(context.Context, string) ([]provider.ReleaseGroupInfo, error) {
		return nil, errors.New("upstream exploded")
	}
	oneMBResult := func(context.Context, string) ([]provider.ArtistSearchResult, error) {
		return []provider.ArtistSearchResult{{Name: "Some Artist", ProviderID: "555", MusicBrainzID: "mbid-555", Score: 60}}, nil
	}
	jsonBody := func(s string) (string, string) { return s, "application/json" }

	// built is what each row's setup hands back: the router, the handler under
	// test, the URL path (with the artist ID where the route has one), the
	// request body and an optional wait for work the handler detaches.
	type built struct {
		r       *Router
		handler http.HandlerFunc
		path    string
		body    string
		ctype   string
		wait    func(t *testing.T)
	}

	rows := []struct {
		id      string
		msg     string // the EXISTING Warn message the site already logged
		method  string
		pattern string // registered route pattern; the {id} wildcard is the artist ID
		hasID   bool   // the route has a wildcard, so the concrete-value check applies
		setup   func(t *testing.T) built
	}{
		{
			id: "B6 discography merge", msg: "fetching release groups from MusicBrainz",
			method: http.MethodPost, pattern: "/api/v1/artists/{id}/discography/fetch", hasID: true,
			setup: func(t *testing.T) built {
				r, svc := discographyFetchRouter(t, failRG)
				a := newArtist(t, svc, &artist.Artist{Name: "Merge Artist", Path: t.TempDir(), MusicBrainzID: "mbid-x"})
				return built{r: r, handler: r.handleFetchDiscography, path: "/api/v1/artists/" + a.ID + "/discography/fetch"}
			},
		},
		{
			id: "B7 deezer identify", msg: "deezer identify: fetching release groups",
			method: http.MethodPost, pattern: "/api/v1/artists/{id}/deezer/search", hasID: true,
			setup: func(t *testing.T) built {
				r, svc := testRouter(t)
				installDeezerOrchestrator(t, r, oneMBResult, failRG)
				a := newArtist(t, svc, &artist.Artist{Name: "Deezer Artist", SortName: "Deezer Artist", Type: "group", Path: albumDir(t)})
				body, ct := jsonBody(`{"query":"Some Artist"}`)
				return built{r: r, handler: r.handleDeezerSearch, path: "/api/v1/artists/" + a.ID + "/deezer/search", body: body, ctype: ct}
			},
		},
		{
			id: "B8 discogs identify", msg: "discogs identify: fetching main release titles",
			method: http.MethodPost, pattern: "/api/v1/artists/{id}/discogs/search", hasID: true,
			setup: func(t *testing.T) built {
				r, svc := testRouter(t)
				installDiscogsOrchestrator(t, r, oneMBResult, failRG)
				a := newArtist(t, svc, &artist.Artist{Name: "Discogs Artist", SortName: "Discogs Artist", Type: "group", Path: albumDir(t)})
				body, ct := jsonBody(`{"query":"Some Artist"}`)
				return built{r: r, handler: r.handleDiscogsSearch, path: "/api/v1/artists/" + a.ID + "/discogs/search", body: body, ctype: ct}
			},
		},
		{
			id: "B9 disambiguation album evidence", msg: "fetching release groups for disambiguation",
			method: http.MethodPost, pattern: "/api/v1/artists/{id}/refresh/search", hasID: true,
			setup: func(t *testing.T) built {
				r, svc := newIdentifyTestServer(t, oneMBResult, failRG)
				a := newArtist(t, svc, &artist.Artist{Name: "Refresh Artist", SortName: "Refresh Artist", Type: "group", Path: albumDir(t)})
				body, ct := jsonBody(`{"query":"Some Artist"}`)
				return built{r: r, handler: r.handleRefreshSearch, path: "/api/v1/artists/" + a.ID + "/refresh/search", body: body, ctype: ct}
			},
		},
		{
			// Bulk identify detaches its work with context.WithoutCancel, so this
			// row also proves the cause survives that detach. The route has no
			// wildcard, so only the pattern itself is asserted.
			id: "B10 identify album gate (bulk identify)", msg: "identify: fetching a candidate's release groups failed, so its catalogue cannot corroborate anything",
			method: http.MethodPost, pattern: "/api/v1/artists/bulk-identify", hasID: false,
			setup: func(t *testing.T) built {
				r, svc := newIdentifyTestServer(t, oneMBResult, failRG)
				newArtist(t, svc, &artist.Artist{Name: "Bulk Artist", SortName: "Bulk Artist", Type: "group", Path: albumDir(t)})
				return built{r: r, handler: r.handleBulkIdentify, path: "/api/v1/artists/bulk-identify",
					wait: func(t *testing.T) {
						t.Helper()
						// The job runs in a goroutine after the handler answers 202.
						deadline := time.Now().Add(10 * time.Second)
						for time.Now().Before(deadline) {
							r.identifyMu.RLock()
							p := r.identifyProgress
							r.identifyMu.RUnlock()
							if p != nil {
								p.mu.RLock()
								done := p.Status != "running"
								p.mu.RUnlock()
								if done {
									return
								}
							}
							time.Sleep(10 * time.Millisecond)
						}
						t.Fatal("bulk identify did not finish within 10s")
					}}
			},
		},
		{
			id: "B11 web image search", msg: "web image search failed",
			method: http.MethodGet, pattern: "/api/v1/artists/{id}/images/websearch", hasID: true,
			setup: func(t *testing.T) built {
				r, svc := newImageHandlerTestServer(t)
				stub := &stubWebImageProvider{name: provider.NameDuckDuckGo, err: errors.New("HTTP 403 Forbidden")}
				a := setUpWebSearchTest(t, r, svc, stub)
				return built{r: r, handler: r.handleWebImageSearch, path: "/api/v1/artists/" + a.ID + "/images/websearch?type=thumb"}
			},
		},
	}

	for _, row := range rows {
		t.Run(row.id, func(t *testing.T) {
			b := row.setup(t)
			logger, logs := logtest.NewJSONLogger()
			b.r.logger = logger

			passthrough := func(next http.Handler) http.Handler { return next }
			mux := http.NewServeMux()
			pattern := row.method + " " + base + row.pattern
			mux.HandleFunc(pattern, wrapAuth(b.handler, passthrough))

			req := httptest.NewRequest(row.method, base+b.path, strings.NewReader(b.body))
			if b.ctype != "" {
				req.Header.Set("Content-Type", b.ctype)
			}
			req = req.WithContext(testI18nCtx(t, req.Context()))
			mux.ServeHTTP(httptest.NewRecorder(), req)
			if b.wait != nil {
				b.wait(t)
			}

			// The concrete value is the artist ID the URL carried.
			concrete := ""
			if row.hasID {
				concrete = strings.Split(strings.TrimPrefix(b.path, "/api/v1/artists/"), "/")[0]
				if concrete == "" || strings.Contains(pattern, concrete) {
					t.Fatalf("precondition: concrete value %q must differ from the registered pattern %q", concrete, pattern)
				}
			}

			found := false
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var got struct {
					Msg   string `json:"msg"`
					Cause string `json:"cause"`
				}
				if err := json.Unmarshal([]byte(line), &got); err != nil || got.Msg != row.msg {
					continue
				}
				found = true
				if want := "user:" + pattern; got.Cause != want {
					t.Errorf("failure line carries cause %q, want %q", got.Cause, want)
				}
				if concrete != "" && strings.Contains(got.Cause, concrete) {
					t.Errorf("cause %q carries the requested path value %q; it must name the route pattern", got.Cause, concrete)
				}
			}
			// Precondition: the request really reached the failing provider call.
			if !found {
				t.Fatalf("no %q record; the request never reached the provider call. Log was:\n%s", row.msg, logs.String())
			}
		})
	}
}
