package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

func TestHandleClearMembers_JSON(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "The Beatles")

	// Seed some members first.
	members := []artist.BandMember{
		{MemberName: "John Lennon", SortOrder: 0},
		{MemberName: "Paul McCartney", SortOrder: 1},
	}
	if err := artistSvc.UpsertMembers(context.Background(), a.ID, members); err != nil {
		t.Fatalf("seeding members: %v", err)
	}

	// Verify members exist.
	before, _ := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if len(before) != 2 {
		t.Fatalf("expected 2 members before clear, got %d", len(before))
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/members", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleClearMembers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "cleared" {
		t.Errorf("status = %q, want %q", resp["status"], "cleared")
	}

	// Verify members are gone.
	after, _ := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if len(after) != 0 {
		t.Errorf("expected 0 members after clear, got %d", len(after))
	}
}

func TestHandleClearMembers_HTMX(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Radiohead")

	members := []artist.BandMember{
		{MemberName: "Thom Yorke", SortOrder: 0},
	}
	if err := artistSvc.UpsertMembers(context.Background(), a.ID, members); err != nil {
		t.Fatalf("seeding members: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/artists/"+a.ID+"/members", nil)
	req = req.WithContext(testI18nCtx(t, req.Context()))
	req.SetPathValue("id", a.ID)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleClearMembers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := w.Body.String()
	if !strings.Contains(body, "No members.") {
		t.Errorf("expected 'No members.' in HTML response, got: %s", body)
	}

	// Verify members are gone.
	after, _ := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if len(after) != 0 {
		t.Errorf("expected 0 members after clear, got %d", len(after))
	}
}

func TestHandleSaveMembers_JSON(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Nirvana")

	body := `[
		{"name": "Kurt Cobain", "instruments": ["guitar", "vocals"]},
		{"name": "Krist Novoselic", "instruments": ["bass"]},
		{"name": "Dave Grohl", "instruments": ["drums"]}
	]`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/members/from-provider",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSaveMembers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp["status"] != "saved" {
		t.Errorf("status = %q, want %q", resp["status"], "saved")
	}

	// Verify members were persisted.
	saved, err := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("listing members: %v", err)
	}
	if len(saved) != 3 {
		t.Fatalf("expected 3 members, got %d", len(saved))
	}
	if saved[0].MemberName != "Kurt Cobain" {
		t.Errorf("first member = %q, want %q", saved[0].MemberName, "Kurt Cobain")
	}
}

func TestHandleSaveMembers_HTMX(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Led Zeppelin")

	body := `[
		{"name": "Jimmy Page", "instruments": ["guitar"]},
		{"name": "Robert Plant", "instruments": ["vocals"]}
	]`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/members/from-provider",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleSaveMembers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	// Check that the HX-Trigger header is set.
	if trigger := w.Header().Get("HX-Trigger"); trigger != "hideFieldProviderModal" {
		t.Errorf("HX-Trigger = %q, want %q", trigger, "hideFieldProviderModal")
	}

	// HTML should contain the member names.
	html := w.Body.String()
	if !strings.Contains(html, "Jimmy Page") {
		t.Errorf("expected 'Jimmy Page' in HTML response")
	}
	if !strings.Contains(html, "Robert Plant") {
		t.Errorf("expected 'Robert Plant' in HTML response")
	}
}

func TestHandleSaveMembers_InvalidBody(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Pink Floyd")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/members/from-provider",
		strings.NewReader("not json"))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSaveMembers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandleSaveMembers_InvalidBody_ToastEnvelope_JSON pins the body shape
// returned by handleSaveMembers when a non-HTMX (raw JSON) caller posts an
// invalid payload. The saveMembers raw-fetch UI parses the response body
// for a JSON {error: ...} envelope so the user-facing toast carries the
// server's reason rather than a bare status code -- without this contract
// the apply-path failure surfaces silently, which is the regression that
// issue #1034 reported.
func TestHandleSaveMembers_InvalidBody_ToastEnvelope_JSON(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Black Sabbath")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/members/from-provider",
		strings.NewReader("not json"))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSaveMembers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	// Non-HTMX path -- writeError takes the JSON branch and emits the
	// {error: "..."} envelope that the saveMembers script parses via
	// JSON.parse(body).error in its 4xx fallback.
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var envelope map[string]string
	if err := json.NewDecoder(w.Body).Decode(&envelope); err != nil {
		t.Fatalf("decoding error envelope: %v", err)
	}
	msg, ok := envelope["error"]
	if !ok {
		t.Fatalf("error envelope missing 'error' key; got: %v", envelope)
	}
	if !strings.Contains(strings.ToLower(msg), "invalid") {
		t.Errorf("error message %q should describe an invalid request body", msg)
	}
}

// TestHandleSaveMembers_InvalidBody_ToastEnvelope_HTMX pins the HTML
// fragment shape returned when the raw-fetch UI sends HX-Request: true
// (which it does so the success branch returns the re-rendered
// MembersSection HTML). On the error path writeError routes by HX-Request
// and renders an ErrorToast component; the saveMembers script's parser
// strips tags from this fragment to extract the message. Pinning the
// fragment shape prevents a future writeError refactor from silently
// regressing this surface to a status-only failure.
func TestHandleSaveMembers_InvalidBody_ToastEnvelope_HTMX(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Soundgarden")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/members/from-provider",
		strings.NewReader("not json"))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.handleSaveMembers(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	// HTMX path -- writeError takes the HTML branch and emits the
	// ErrorToast component. The body must contain the human-readable
	// reason text so the saveMembers script's tag-stripping fallback
	// can surface it via window.showToast.
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", ct)
	}
	body := w.Body.String()
	if !strings.Contains(strings.ToLower(body), "invalid") {
		t.Errorf("HTML error fragment missing 'invalid' reason text; got: %s", body)
	}
}

// TestHandleSaveMembers_PersistsApplyPayload is the integration assertion
// for the apply-path contract: a JSON payload posted to the from-provider
// endpoint must be decoded, converted via convertProviderMembers, and
// upserted into the artist's band_members rows. This test goes beyond
// TestHandleSaveMembers_JSON's status-code check and asserts that every
// member field carried by MemberInfo (Name, MBID, Instruments, VocalType,
// DateJoined/DateLeft) round-trips through to the persisted BandMember
// records, so a future refactor of convertProviderMembers cannot silently
// drop a column.
func TestHandleSaveMembers_PersistsApplyPayload(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Apply Test Band")

	body := `[
		{
			"name": "Lead Singer",
			"mbid": "11111111-1111-1111-1111-111111111111",
			"instruments": ["vocals", "guitar"],
			"vocal_type": "tenor",
			"date_joined": "2010",
			"date_left": "2020"
		},
		{
			"name": "Bassist",
			"instruments": ["bass"]
		}
	]`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/members/from-provider",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSaveMembers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	saved, err := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("listing persisted members: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("expected 2 persisted members, got %d", len(saved))
	}

	// Order-by-sort-order: ListMembersByArtistID returns members in
	// SortOrder ascending, and convertProviderMembers stamps SortOrder
	// from the slice index, so saved[0] is the first MemberInfo.
	first := saved[0]
	if first.MemberName != "Lead Singer" {
		t.Errorf("saved[0].MemberName = %q, want %q", first.MemberName, "Lead Singer")
	}
	if first.MemberMBID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("saved[0].MemberMBID = %q, want the MBID from the apply payload", first.MemberMBID)
	}
	if len(first.Instruments) != 2 || first.Instruments[0] != "vocals" || first.Instruments[1] != "guitar" {
		t.Errorf("saved[0].Instruments = %v, want [vocals, guitar]", first.Instruments)
	}
	if first.VocalType != "tenor" {
		t.Errorf("saved[0].VocalType = %q, want %q", first.VocalType, "tenor")
	}
	if first.DateJoined != "2010" {
		t.Errorf("saved[0].DateJoined = %q, want %q", first.DateJoined, "2010")
	}
	if first.DateLeft != "2020" {
		t.Errorf("saved[0].DateLeft = %q, want %q", first.DateLeft, "2020")
	}

	second := saved[1]
	if second.MemberName != "Bassist" {
		t.Errorf("saved[1].MemberName = %q, want %q", second.MemberName, "Bassist")
	}
}

func TestHandleSaveMembers_ReplacesExisting(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Queen")

	// Seed initial members.
	initial := []artist.BandMember{
		{MemberName: "Freddie Mercury", SortOrder: 0},
		{MemberName: "Brian May", SortOrder: 1},
	}
	if err := artistSvc.UpsertMembers(context.Background(), a.ID, initial); err != nil {
		t.Fatalf("seeding members: %v", err)
	}

	// Save new members from provider (should replace, not append).
	body := `[{"name": "Roger Taylor", "instruments": ["drums"]}]`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/members/from-provider",
		strings.NewReader(body))
	req.SetPathValue("id", a.ID)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleSaveMembers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	saved, _ := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if len(saved) != 1 {
		t.Fatalf("expected 1 member after replace, got %d", len(saved))
	}
	if saved[0].MemberName != "Roger Taylor" {
		t.Errorf("member = %q, want %q", saved[0].MemberName, "Roger Taylor")
	}
}
