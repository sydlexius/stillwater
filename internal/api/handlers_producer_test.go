package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/provider"
)

// producerRowFor returns the newest metadata_changes row for field whose
// NewValue equals newValue, failing the test if there is none. Driving the
// real writers and reading the stored row (not a spy) is the point of these
// tests (#3078): a producer that never reaches the table is the defect.
func producerRowFor(t *testing.T, h *artist.HistoryService, artistID, field, newValue string) artist.MetadataChange {
	t.Helper()
	rows, _, err := h.List(context.Background(), artistID, 50, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	for _, row := range rows {
		if row.Field == field && row.NewValue == newValue {
			return row
		}
	}
	t.Fatalf("no %q history row with new value %q among %d rows", field, newValue, len(rows))
	return artist.MetadataChange{}
}

// patchField drives handleFieldUpdate with a form body.
func patchField(t *testing.T, r *Router, artistID, field string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+artistID+"/fields/"+field, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", artistID)
	req.SetPathValue("field", field)
	w := httptest.NewRecorder()
	r.handleFieldUpdate(w, req)
	return w
}

// TestProducerStamps_RefreshAndOperatorEdit is T1: the same biography column
// moved by a provider refresh and by an operator edit must record DIFFERENT
// producers while sharing source "manual".
func TestProducerStamps_RefreshAndOperatorEdit(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)
	ctx := context.Background()

	a := &artist.Artist{
		Name: "Producer Artist", SortName: "Producer Artist", Type: "group",
		Path: "/music/Producer Artist", MusicBrainzID: "00000000-0000-0000-0000-000000003078",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Path A: provider refresh, biography attributed to lastfm.
	stub := &stubScraperExecutor{result: &provider.FetchResult{
		Metadata:        &provider.ArtistMetadata{Biography: "bio from the refresh", Origin: "origin from the refresh"},
		AttemptedFields: []string{"biography", "origin"},
		PopulatedFields: []string{"biography", "origin"},
		Sources:         []provider.FieldSource{{Field: "biography", Provider: provider.NameLastFM}},
	}}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := provider.NewOrchestrator(nil, nil, logger, nil)
	orch.SetExecutor(stub)
	r.orchestrator = orch
	if _, err := r.executeRefreshCtx(artist.ContextWithSource(ctx, "manual"), a); err != nil {
		t.Fatalf("executeRefreshCtx: %v", err)
	}
	refresh := producerRowFor(t, historySvc, a.ID, "biography", "bio from the refresh")
	if refresh.Source != "manual" || refresh.Producer != "provider:lastfm" {
		t.Errorf("refresh row source/producer = %q/%q, want manual/provider:lastfm", refresh.Source, refresh.Producer)
	}

	// A field the refresh moved with NO FieldSource falls back to the bare
	// "provider:" (it must never read as operator-authored or unrecorded).
	noSource := producerRowFor(t, historySvc, a.ID, "origin", "origin from the refresh")
	if noSource.Producer != "provider:" {
		t.Errorf("source-less refresh row producer = %q, want the bare provider: fallback", noSource.Producer)
	}

	// Path B: operator types a biography through the field handler.
	w := patchField(t, r, a.ID, "biography", url.Values{"value": {"bio typed by operator"}, "producer": {"operator"}})
	if w.Code != http.StatusOK {
		t.Fatalf("handleFieldUpdate status = %d, body: %s", w.Code, w.Body.String())
	}
	typed := producerRowFor(t, historySvc, a.ID, "biography", "bio typed by operator")
	if typed.Source != "manual" || typed.Producer != "operator" {
		t.Errorf("operator row source/producer = %q/%q, want manual/operator", typed.Source, typed.Producer)
	}
}

// TestProducerStamps_PlatformPull is T1b: a Pull from Emby is operator
// triggered (source manual) but platform authored.
func TestProducerStamps_PlatformPull(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)
	ctx := context.Background()
	a := addTestArtist(t, artistSvc, "Pull Producer Artist")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Name":"Pull Producer Artist","SortName":"Pull Producer Artist","Overview":"bio from the platform","Genres":[],"Tags":[],"ProviderIds":{},"ImageTags":{},"BackdropImageTags":[],"LockData":false,"LockedFields":[]}`))
	}))
	defer srv.Close()

	conn := &connection.Connection{
		Name: "Producer Emby", Type: connection.TypeEmby, URL: srv.URL, APIKey: "k",
		Emby: &connection.EmbyConfig{PlatformUserID: "user-001"}, Enabled: true,
	}
	if err := r.connectionService.Create(ctx, conn); err != nil {
		t.Fatalf("creating connection: %v", err)
	}
	if err := artistSvc.SetPlatformID(ctx, a.ID, conn.ID, "emby-artist-3078"); err != nil {
		t.Fatalf("setting platform id: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/pull?connection_id="+conn.ID, nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()
	r.handlePullMetadata(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	row := producerRowFor(t, historySvc, a.ID, "biography", "bio from the platform")
	if row.Source != "manual" || row.Producer != "platform:emby" {
		t.Errorf("pull row source/producer = %q/%q, want manual/platform:emby", row.Source, row.Producer)
	}
}

// TestHandleFieldUpdate_ProducerAllowList is T2: the client-supplied producer
// is an allow-list. Anything but operator or provider:<known> records "".
func TestHandleFieldUpdate_ProducerAllowList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		claim    string
		sendJSON bool
		want     string
	}{
		{"operator accepted", "operator", false, "operator"},
		{"known provider accepted", "provider:lastfm", false, "provider:lastfm"},
		{"known provider accepted over JSON", "provider:lastfm", true, "provider:lastfm"},
		{"forged rule rejected", "rule:bio_exists", false, ""},
		{"forged platform rejected", "platform:emby", false, ""},
		{"forged restore rejected", "restore", false, ""},
		{"unknown provider rejected", "provider:notarealprovider", false, ""},
		{"bare provider prefix rejected", "provider:", false, ""},
		{"absent claim is unrecorded, never operator", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, artistSvc, historySvc := testRouterWithHistory(t)
			artistSvc.SetHistoryService(historySvc)
			a := addTestArtist(t, artistSvc, "Allow List Artist")

			var w *httptest.ResponseRecorder
			if tc.sendJSON {
				body := `{"value":"allow-list bio","producer":"` + tc.claim + `"}`
				req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/biography", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.SetPathValue("id", a.ID)
				req.SetPathValue("field", "biography")
				w = httptest.NewRecorder()
				r.handleFieldUpdate(w, req)
			} else {
				form := url.Values{"value": {"allow-list bio"}}
				if tc.claim != "" {
					form.Set("producer", tc.claim)
				}
				w = patchField(t, r, a.ID, "biography", form)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
			}
			row := producerRowFor(t, historySvc, a.ID, "biography", "allow-list bio")
			if row.Producer != tc.want {
				t.Errorf("producer = %q, want %q", row.Producer, tc.want)
			}
		})
	}
}

// TestHandleFieldClear_StampsOperator: clearing a field is an operator act.
func TestHandleFieldClear_StampsOperator(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)
	ctx := context.Background()
	a := addTestArtist(t, artistSvc, "Clear Producer Artist")
	if _, err := artistSvc.UpdateField(ctx, a.ID, "biography", "bio to clear"); err != nil {
		t.Fatalf("seeding biography: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/fields/biography", nil)
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()
	r.handleFieldClear(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	rows, _, err := historySvc.List(ctx, a.ID, 50, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	for _, row := range rows {
		if row.Field == "biography" && row.OldValue == "bio to clear" && row.NewValue == "" {
			if row.Producer != "operator" {
				t.Errorf("clear row producer = %q, want operator", row.Producer)
			}
			return
		}
	}
	t.Fatalf("no clear row found among %d rows", len(rows))
}

// TestApplyProviderName_StampsProvider: the provider name promotion is a
// refresh write and records the bare "provider:" (which provider supplied a
// promoted name is not recoverable here).
func TestApplyProviderName_StampsProvider(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)
	a := addTestArtist(t, artistSvc, "Old Name")

	failed := r.applyProviderName(context.Background(), a, &provider.ArtistMetadata{Name: "Promoted Name"})
	if failed {
		t.Fatal("applyProviderName reported a failed write")
	}
	row := producerRowFor(t, historySvc, a.ID, "name", "Promoted Name")
	if row.Producer != "provider:" {
		t.Errorf("name row producer = %q, want provider:", row.Producer)
	}
}

// TestHandleFieldUpdate_NameProducer covers the guarded-rename branch, which
// writes through UpdateNameGuarded rather than UpdateField and so needs the
// stamped context passed separately.
func TestHandleFieldUpdate_NameProducer(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)
	a := addTestArtist(t, artistSvc, "Rename Me")

	w := patchField(t, r, a.ID, "name", url.Values{"value": {"Renamed Artist"}, "producer": {"operator"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if row := producerRowFor(t, historySvc, a.ID, "name", "Renamed Artist"); row.Producer != "operator" {
		t.Errorf("rename row producer = %q, want operator", row.Producer)
	}

	w = patchField(t, r, a.ID, "name", url.Values{"value": {"Forged Rename"}, "producer": {"rule:x"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if row := producerRowFor(t, historySvc, a.ID, "name", "Forged Rename"); row.Producer != "" {
		t.Errorf("forged rename row producer = %q, want empty", row.Producer)
	}
}

// TestHandleFieldUpdate_ProducerOnlyFromBody: a producer in the URL query is
// not a claim; only the request body is read (PostForm, not Form).
func TestHandleFieldUpdate_ProducerOnlyFromBody(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)
	a := addTestArtist(t, artistSvc, "Query Producer Artist")

	form := url.Values{"value": {"query claim bio"}}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/biography?producer=operator", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", a.ID)
	req.SetPathValue("field", "biography")
	w := httptest.NewRecorder()
	r.handleFieldUpdate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if row := producerRowFor(t, historySvc, a.ID, "biography", "query claim bio"); row.Producer != "" {
		t.Errorf("query-string producer recorded %q, want empty", row.Producer)
	}
}

// TestHandleFieldUpdate_NonStringJSONProducer: a producer of the wrong JSON
// type degrades to unrecorded and never fails the write.
func TestHandleFieldUpdate_NonStringJSONProducer(t *testing.T) {
	t.Parallel()
	for name, producer := range map[string]string{"number": `5`, "object": `{}`, "array": `["operator"]`, "null": `null`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, artistSvc, historySvc := testRouterWithHistory(t)
			artistSvc.SetHistoryService(historySvc)
			a := addTestArtist(t, artistSvc, "Bad Producer Artist")

			body := `{"value":"bad producer bio","producer":` + producer + `}`
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/artists/"+a.ID+"/fields/biography", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("id", a.ID)
			req.SetPathValue("field", "biography")
			w := httptest.NewRecorder()
			r.handleFieldUpdate(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (a bad producer must not fail the write); body: %s", w.Code, w.Body.String())
			}
			if row := producerRowFor(t, historySvc, a.ID, "biography", "bad producer bio"); row.Producer != "" {
				t.Errorf("producer = %q, want empty", row.Producer)
			}
		})
	}
}

// TestArtistDetailPage_DoneStampsOperatorOnlyForTypedValues pins the edit-mode
// Done commit script (the only visible commit path on the detail page): a
// typed value is sent with producer=operator, a value staged from the undo
// popover (possibly old provider text) is not, and staging sets the flag the
// commit reads. The script is inline in the templ, so this asserts the
// rendered source.
func TestArtistDetailPage_DoneStampsOperatorOnlyForTypedValues(t *testing.T) {
	t.Parallel()
	r, artistSvc := detailTestRouter(t)
	id := seedDetailArtist(t, artistSvc, "Done Producer Artist")

	w := detailRequest(t, r, id)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`+ (job.staged ? '' : '&amp;producer=operator')`,
		`staged: input.dataset.swStaged === '1'`,
		`input.dataset.swStaged = '1'`,
		// Typing over a staged value clears the flag, on TRUSTED input only so
		// staging's own synthetic input event does not clear it.
		`if (e.isTrusted && t && t.dataset && t.dataset.swStaged) { delete t.dataset.swStaged; }`,
		`document.addEventListener('input', swClearStagedOnUserEdit, true)`,
		// A direct per-field submit of a staged value drops the operator claim.
		`document.addEventListener('htmx:configRequest'`,
		`delete e.detail.parameters.producer;`,
	} {
		if !strings.Contains(body, want) && !strings.Contains(body, strings.ReplaceAll(want, "&amp;", "&")) {
			t.Errorf("rendered detail page missing %q", want)
		}
	}
}

// TestFieldUpdate_LogsBoundedCleanProducerClaim sends an oversized,
// control-character-laden claim through the real handler and asserts the
// rejection log carries a bounded, clean token (#3078).
func TestFieldUpdate_LogsBoundedCleanProducerClaim(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)
	a := &artist.Artist{Name: "Claim Artist", SortName: "Claim Artist", Type: "group", Path: "/music/Claim Artist"}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	var buf bytes.Buffer
	r.logger = slog.New(slog.NewJSONHandler(&buf, nil))

	claim := "forged\nINFO fake line\x1b[31m\x00\u202e\u2028\u200b" + strings.Repeat("x", 5000)
	w := patchField(t, r, a.ID, "biography", url.Values{"value": {"a new bio"}, "producer": {claim}})
	if w.Code >= 400 {
		t.Fatalf("precondition: the write itself should succeed, got %d", w.Code)
	}
	var logged string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "rejected producer claim, recording unrecorded" {
			logged, _ = rec["producer_claim"].(string)
		}
	}
	if logged == "" {
		t.Fatalf("precondition: no rejection log line found in %q", buf.String())
	}
	if n := len([]rune(logged)); n != 67 {
		t.Errorf("logged claim is %d runes, want exactly 67 (64 plus the ellipsis)", n)
	}
	for _, c := range logged {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) || c == 0x2028 {
			t.Errorf("logged claim carries control character %q", c)
		}
	}
	if !strings.HasPrefix(logged, "forgedINFO fake line") {
		t.Errorf("logged claim = %q, want the printable prefix preserved", logged)
	}
	if row := producerRowFor(t, historySvc, a.ID, "biography", "a new bio"); row.Producer != artist.ProducerUnrecorded {
		t.Errorf("stored producer = %q, want unrecorded", row.Producer)
	}
}
