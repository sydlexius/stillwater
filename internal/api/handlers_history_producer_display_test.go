package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/auth"
)

// Display tests for #3078 slice 4b: every surface that renders a history row
// shows what supplied the value, a row written before producer tracking
// reads "Value source not recorded" (never "set by a user"), and /next serves
// the same words as the stable route. Real SQLite, real mux.

const (
	chipUnrecorded = "Value source not recorded"
	chipOperator   = "set by a user"
	chipLastfm     = "Value: from Last.fm"
)

// producerDisplayFixture seeds one artist with three history rows:
//   - legacy: inserted with raw SQL naming ONLY the pre-029 columns, byte-for-
//     byte what a v1.6.2 install holds (producer takes its column default);
//   - stamped: a provider:lastfm row;
//   - identify: source and producer both provider:identify_album.
type producerDisplayFixture struct {
	get      func(path string, hdr map[string]string) *httptest.ResponseRecorder
	post     func(path string, hdr map[string]string) *httptest.ResponseRecorder
	artistID string
}

func newProducerDisplayFixture(t *testing.T) producerDisplayFixture {
	t.Helper()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	// Without this the revert's own history row is never written.
	artistSvc.SetHistoryService(historySvc)
	// The /next lane answers 404 unless SW_UX enables it.
	r.ux = "dual"
	a := addTestArtist(t, artistSvc, "Producer Display")

	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		  VALUES ('chg-legacy', ?, 'biography', '', 'legacy bio', 'manual', '2026-01-01T00:00:00Z')`, []any{a.ID}},
		{`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, producer, created_at)
		  VALUES ('chg-stamped', ?, 'genres', '', 'Ambient', 'manual', 'provider:lastfm', '2026-01-02T00:00:00Z')`, []any{a.ID}},
		{`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, producer, created_at)
		  VALUES ('chg-identify', ?, 'musicbrainz_id', '', 'mbid-1', 'provider:identify_album', 'provider:identify_album', '2026-01-03T00:00:00Z')`, []any{a.ID}},
		{`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, producer, created_at)
		  VALUES ('chg-revert', ?, 'biography', 'legacy bio', '', 'revert', 'restore', '2026-01-04T00:00:00Z')`, []any{a.ID}},
		// The revert below needs the field to currently hold the legacy value.
		{`UPDATE artists SET biography = 'legacy bio' WHERE id = ?`, []any{a.ID}},
	}
	for _, s := range stmts {
		if _, err := r.db.ExecContext(context.Background(), s.q, s.args...); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	// Precondition: the legacy row really has no producer, the stamped row does.
	for id, want := range map[string]string{"chg-legacy": "", "chg-stamped": "provider:lastfm"} {
		var got string
		if err := r.db.QueryRowContext(context.Background(),
			`SELECT producer FROM metadata_changes WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("reading producer of %s: %v", id, err)
		}
		if got != want {
			t.Fatalf("fixture: producer of %s = %q, want %q", id, got, want)
		}
	}

	if _, err := r.authService.Setup(context.Background(), "admin", "password"); err != nil {
		t.Fatalf("auth setup: %v", err)
	}
	var userID string
	if err := r.db.QueryRowContext(context.Background(), `SELECT id FROM users WHERE username = 'admin'`).Scan(&userID); err != nil {
		t.Fatalf("looking up admin: %v", err)
	}
	token, _, err := r.authService.CreateAPIToken(context.Background(), userID, "producer-display", string(auth.ScopeAdmin))
	if err != nil {
		t.Fatalf("creating token: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := r.Handler(ctx)

	do := func(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200; body: %.300s", method, path, w.Code, w.Body.String())
		}
		return w
	}
	return producerDisplayFixture{
		artistID: a.ID,
		get:      func(p string, h map[string]string) *httptest.ResponseRecorder { return do(http.MethodGet, p, h) },
		post:     func(p string, h map[string]string) *httptest.ResponseRecorder { return do(http.MethodPost, p, h) },
	}
}

func TestHistoryProducerDisplay_EverySurface(t *testing.T) {
	t.Parallel()
	f := newProducerDisplayFixture(t)
	id := f.artistID

	surfaces := []struct {
		name string
		path string
		hdr  map[string]string
		// rowPrefix, when set, scopes the legacy assertions to that row: the full
		// pages carry a help tooltip that spells out the same words.
		rowPrefix string
		// Which of the seeded rows this surface lists.
		wantStamped, wantLegacy bool
	}{
		{"activity", "/activity?artist_id=" + id, nil, "activity-change-", true, true},
		{"activity fragment", "/activity/content?artist_id=" + id, nil, "activity-change-", true, true},
		{"next activity", "/next/activity?artist_id=" + id, nil, "activity-change-", true, true},
		{"history tab", "/artists/" + id + "/history/tab", nil, "history-change-", true, true},
		{"next history tab", "/next/artists/" + id + "/history/tab", nil, "history-change-", true, true},
		// The popover is per field: biography holds the legacy row only.
		{"biography popover", "/api/v1/artists/" + id + "/fields/biography/history/fragment", nil, "", false, true},
		{"genres popover", "/api/v1/artists/" + id + "/fields/genres/history/fragment", nil, "", true, false},
	}
	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			w := f.get(s.path, s.hdr)
			body := w.Body.String()
			// Precondition: a /next path really went through the /next lane.
			if got, want := w.Header().Get("X-Stillwater-UX") == "next", strings.HasPrefix(s.path, "/next/"); got != want {
				t.Fatalf("X-Stillwater-UX=next is %v, want %v", got, want)
			}
			if s.wantLegacy {
				row := body
				if s.rowPrefix != "" {
					row = rowByID(t, body, s.rowPrefix, "chg-legacy")
				}
				if !strings.Contains(row, chipUnrecorded) {
					t.Errorf("legacy row lacks %q", chipUnrecorded)
				}
				if strings.Contains(row, chipOperator) {
					t.Errorf("a surface rendered %q for a row that recorded no producer", chipOperator)
				}
			}
			if s.wantStamped && !strings.Contains(body, chipLastfm) {
				t.Errorf("stamped row lacks %q", chipLastfm)
			}
			if !s.wantStamped && strings.Contains(body, "Last.fm") {
				t.Errorf("surface unexpectedly lists the genres row")
			}
		})
	}
}

// rowByID returns the markup of one history row: from its id attribute to the
// next row id (or the end of the body).
func rowByID(t *testing.T, body, prefix, id string) string {
	t.Helper()
	start := strings.Index(body, `id="`+prefix+id+`"`)
	if start < 0 {
		t.Fatalf("no row %s%s in the body", prefix, id)
	}
	rest := body[start+1:]
	if end := strings.Index(rest, `id="`+prefix); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// The Undo row's value is a restore: the popover item carries no value label,
// and its second line is width-capped like the first so a long token wraps.
func TestHistoryProducerDisplay_PopoverUndoItemHasNoValueLabel(t *testing.T) {
	t.Parallel()
	f := newProducerDisplayFixture(t)
	body := f.get("/api/v1/artists/"+f.artistID+"/fields/biography/history/fragment", nil).Body.String()
	var item string
	for _, chunk := range strings.Split(body, "</button>") {
		if strings.Contains(chunk, ">Revert<") || strings.Contains(chunk, "Revert\n") || strings.Contains(chunk, "Revert ") {
			item = chunk
		}
	}
	if item == "" {
		t.Fatalf("precondition: no Revert item in the popover fragment: %.600s", body)
	}
	if strings.Contains(item, "Value:") || strings.Contains(item, chipUnrecorded) {
		t.Errorf("the Undo item carries a value label: %s", item)
	}
	if !strings.Contains(body, "max-w-[12rem] break-words") {
		t.Errorf("the second line is not width-capped and wrapping")
	}
}

func TestHistoryProducerDisplay_IdentifyTierNamedAsTier(t *testing.T) {
	t.Parallel()
	f := newProducerDisplayFixture(t)
	for _, path := range []string{
		"/activity?artist_id=" + f.artistID,
		"/next/activity?artist_id=" + f.artistID,
		"/artists/" + f.artistID + "/history/tab",
	} {
		body := f.get(path, nil).Body.String()
		if !strings.Contains(body, "Identify: album match") {
			t.Errorf("%s: identify row lacks %q", path, "Identify: album match")
		}
		if strings.Contains(body, "identify_album") {
			t.Errorf("%s: leaked the raw token identify_album", path)
		}
		// producer == source, so the value chip would only repeat the badge.
		if strings.Contains(body, "Value: identify") {
			t.Errorf("%s: redundant value chip on an identify row", path)
		}
	}
}

func TestHistoryProducerDisplay_UndoRowCarriesNoNotRecordedChip(t *testing.T) {
	t.Parallel()
	for name, current := range map[string]string{
		"activity":    "/activity",
		"history tab": "/artists/ID/history/tab",
	} {
		t.Run(name, func(t *testing.T) {
			// A fresh fixture per case: an undo consumes the legacy value.
			g := newProducerDisplayFixture(t)
			w := g.post("/api/v1/history/chg-legacy/revert", map[string]string{
				"HX-Request": "true", "HX-Current-URL": "http://localhost" + strings.ReplaceAll(current, "ID", g.artistID),
			})
			body := w.Body.String()
			// Precondition: the fragment is the new Revert row, not a fallback.
			if !strings.Contains(body, ">Revert<") && !strings.Contains(body, "Revert\n") {
				t.Fatalf("response is not a Revert row: %.400s", body)
			}
			if strings.Contains(body, chipUnrecorded) {
				t.Errorf("undo row says %q: its value is a restore, not unknown", chipUnrecorded)
			}
		})
	}
}
