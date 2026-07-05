package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNextFallback_ReDispatchesToStablePath verifies that a /next/* request
// falls back to the stable route for the same path (M55 #1340, decision 12):
// the handler strips the /next prefix and re-dispatches through the mux, so the
// v1 page is served and navigation never breaks.
func TestNextFallback_ReDispatchesToStablePath(t *testing.T) {
	t.Parallel()

	r := &Router{basePath: ""}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("V1 DASHBOARD"))
	})
	mux.HandleFunc("GET /artists/{id}", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("V1 ARTIST " + req.PathValue("id")))
	})
	mux.HandleFunc("GET /artists/{id}/artwork-modal", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("MODAL " + req.PathValue("id") + " q=" + req.URL.RawQuery))
	})
	// Bare /artists echoes the forwarded query string so we can assert the
	// fallback preserves it for bookmarked /next/artists?view=... URLs.
	mux.HandleFunc("GET /artists", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("V1 ARTISTS q=" + req.URL.RawQuery))
	})
	mux.HandleFunc("GET /reports", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("WORKSPACE q=" + req.URL.RawQuery))
	})
	mux.HandleFunc("GET /reports/{name}", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("WORKSPACE " + req.PathValue("name")))
	})
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("SETTINGS"))
	})
	mux.HandleFunc("GET /preferences", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("PREFERENCES"))
	})
	mux.HandleFunc("GET /preferences-drawer", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("PREFS DRAWER"))
	})
	mux.HandleFunc("GET /activity", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("ACTIVITY q=" + req.URL.RawQuery))
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte("LOGS q=" + req.URL.RawQuery))
	})
	mux.HandleFunc("GET /next/{path...}", r.nextFallback(mux))

	tests := []struct {
		path string
		want string
	}{
		{"/next/dashboard", "V1 DASHBOARD"},
		{"/next/artists/42", "V1 ARTIST 42"},
		// #1757 PR-3a: bare /next/artists re-dispatches to the promoted list...
		{"/next/artists", "V1 ARTISTS q="},
		// ...and the query string (view/page/sort/etc.) is forwarded intact.
		{"/next/artists?view=grid&page=2", "V1 ARTISTS q=view=grid&page=2"},
		// #1757 PR-3b: the dedicated /next/artists/{id} detail + artwork-modal
		// routes are gone; both re-dispatch to the promoted canonical pages,
		// query string (the modal's kind param) intact.
		{"/next/artists/42/artwork-modal?kind=logo", "MODAL 42 q=kind=logo"},
		// #1757 PR-4: the dedicated /next/reports + /next/reports/{name} routes
		// are gone; both re-dispatch to the promoted canonical workspace, query
		// string intact for bookmarked filtered URLs.
		{"/next/reports", "WORKSPACE q="},
		{"/next/reports?search=abba", "WORKSPACE q=search=abba"},
		{"/next/reports/health", "WORKSPACE health"},
		// #1757 PR-5: settings (#1339), preferences (#1774), the preferences
		// drawer fragment, activity (#1772), and logs (#1338) promoted to their
		// canonical paths; their dedicated /next/* routes are gone and re-dispatch
		// to the promoted pages via the fallback, query string intact for
		// bookmarked deep-links.
		{"/next/settings", "SETTINGS"},
		{"/next/preferences", "PREFERENCES"},
		{"/next/preferences-drawer", "PREFS DRAWER"},
		{"/next/activity", "ACTIVITY q="},
		{"/next/logs?level=error", "LOGS q=level=error"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", tt.path, rec.Code)
			}
			if got := rec.Body.String(); got != tt.want {
				t.Errorf("%s: body = %q, want %q (should fall back to stable page)", tt.path, got, tt.want)
			}
		})
	}
}
