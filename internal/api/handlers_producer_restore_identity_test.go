package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// rowsBySource returns the artist's history rows recorded under source.
func rowsBySource(t *testing.T, h *artist.HistoryService, artistID, source string) []artist.MetadataChange {
	t.Helper()
	rows, _, err := h.List(context.Background(), artistID, 50, 0)
	if err != nil {
		t.Fatalf("listing history: %v", err)
	}
	var out []artist.MetadataChange
	for _, row := range rows {
		if row.Source == source {
			out = append(out, row)
		}
	}
	return out
}

// TestProducerStamps_RevertRecordsRestore drives an Undo through the real
// revert handler (#3078). The row being undone is provider-supplied text, which
// is exactly why the undo must record "restore" and not an author: putting a
// value back says nothing about who wrote it.
func TestProducerStamps_RevertRecordsRestore(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)
	ctx := context.Background()
	a := addTestArtist(t, artistSvc, "Revert Producer Artist")

	// Seed through the real writers: an operator-typed biography, then a
	// provider-supplied one replacing it.
	for _, form := range []url.Values{
		{"value": {"bio the operator typed"}, "producer": {"operator"}},
		{"value": {"bio a provider supplied"}, "producer": {"provider:lastfm"}},
	} {
		if w := patchField(t, r, a.ID, "biography", form); w.Code != http.StatusOK {
			t.Fatalf("seeding biography: status = %d, body: %s", w.Code, w.Body.String())
		}
	}
	original := producerRowFor(t, historySvc, a.ID, "biography", "bio a provider supplied")

	// PRECONDITIONS: the row to undo really is provider-stamped, and nothing
	// has been reverted yet -- otherwise the assertions below could be reading
	// a row some other write produced.
	if original.Source != "manual" || original.Producer != "provider:lastfm" {
		t.Fatalf("fixture: seeded row source/producer = %q/%q, want manual/provider:lastfm",
			original.Source, original.Producer)
	}
	if n := len(rowsBySource(t, historySvc, a.ID, "revert")); n != 0 {
		t.Fatalf("fixture: %d revert rows before the undo, want 0", n)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/history/"+original.ID+"/revert", nil)
	req.SetPathValue("id", original.ID)
	w := httptest.NewRecorder()
	r.handleRevertHistory(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("revert status = %d, body: %s", w.Code, w.Body.String())
	}

	reverts := rowsBySource(t, historySvc, a.ID, "revert")
	if len(reverts) != 1 {
		t.Fatalf("revert rows = %d, want exactly 1", len(reverts))
	}
	if got := reverts[0]; got.Producer != artist.ProducerRestore || got.NewValue != "bio the operator typed" {
		t.Errorf("revert row producer/new value = %q/%q, want restore and the value put back",
			got.Producer, got.NewValue)
	}

	// The undo adds a row; it must not rewrite the one it undid.
	after, err := historySvc.GetByID(ctx, original.ID)
	if err != nil {
		t.Fatalf("re-reading the undone row: %v", err)
	}
	if *after != original {
		t.Errorf("the undone row changed: got %+v, want %+v", *after, original)
	}
}

// blankMBIDArtist creates an artist and asserts the shared PRECONDITION: no
// stored MusicBrainz ID and no history for it, so a later row is the test's own.
func blankMBIDArtist(t *testing.T, svc *artist.Service, h *artist.HistoryService, name string) *artist.Artist {
	t.Helper()
	a := &artist.Artist{Name: name, SortName: name, Type: "group"}
	if err := svc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if got, n := reloadMBID(t, svc, a.ID), len(mbidHistoryRows(t, h, a.ID)); got != "" || n != 0 {
		t.Fatalf("fixture: stored MBID = %q with %d history rows, want blank and 0", got, n)
	}
	return a
}

// TestProducerStamps_IdentityWrites covers every writer that reaches
// applyIdentity (#3078). Each case fills a BLANK MusicBrainz ID, so the only
// thing that varies between them is the source the writer declares.
func TestProducerStamps_IdentityWrites(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		source       string
		provenance   string
		wantProducer string
	}{
		{"operator link", artist.IdentifySourceOperator, artist.SourceOperatorConfirmed, artist.ProducerOperator},
		{"tier 1 connection index", artist.IdentifySourceConnection, artist.SourceMachinePicked, "provider:identify_connection"},
		{"tier 2 album comparison", artist.IdentifySourceAlbum, artist.SourceMachinePicked, "provider:identify_album"},
		{"tier 3 name search", artist.IdentifySourceName, artist.SourceMachinePicked, "provider:identify_name"},
		// A source the mapping does not know must stay unrecorded. It must
		// never fall through to "operator".
		{"unrecognized source stays unrecorded", "provider:some_future_tier", artist.SourceMachinePicked, artist.ProducerUnrecorded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r, artistSvc, histSvc := identifyHistoryRouter(t, nil)

			a := blankMBIDArtist(t, artistSvc, histSvc, "Identity "+tc.name)

			if _, err := r.applyIdentity(ctx, a, identityWrite{
				MBID: mbidProposed, Source: tc.source, Provenance: tc.provenance,
			}); err != nil {
				t.Fatalf("applyIdentity: %v", err)
			}

			rows := mbidHistoryRows(t, histSvc, a.ID)
			if len(rows) != 1 || rows[0].Source != tc.source || rows[0].Producer != tc.wantProducer {
				t.Errorf("MBID history rows = %+v, want exactly one, %q/%q", rows, tc.source, tc.wantProducer)
			}
		})
	}
}

// TestProducerStamps_OperatorLinkEndpointRecordsOperator drives the real
// operator endpoint, so the source (and so the producer) is the handler's own.
func TestProducerStamps_OperatorLinkEndpointRecordsOperator(t *testing.T) {
	t.Parallel()
	r, artistSvc, histSvc := identifyHistoryRouter(t, nil)
	a := blankMBIDArtist(t, artistSvc, histSvc, "Operator Linked")

	body := strings.NewReader(`{"artist_id":"` + a.ID + `","mbid":"` + mbidProposed + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/bulk-identify/link", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handleBulkIdentifyLink(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}

	rows := mbidHistoryRows(t, histSvc, a.ID)
	if len(rows) != 1 || rows[0].Source != "manual" || rows[0].Producer != artist.ProducerOperator {
		t.Errorf("MBID history rows = %+v, want exactly one, manual/operator", rows)
	}
}

// TestIdentify_LockedBlankMBIDRecordsNoHistory covers the write that does not
// happen: the MusicBrainz ID is locked while empty, an automated identify finds
// a match, and the lock puts the blank back. Nothing changed, so no row.
func TestIdentify_LockedBlankMBIDRecordsNoHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, artistSvc, histSvc := identifyHistoryRouter(t, nil)
	a := blankMBIDArtist(t, artistSvc, histSvc, "Pinned Blank")
	if err := artistSvc.SetLockedFields(ctx, a.ID, []string{string(artist.FieldMusicBrainzID)}); err != nil {
		t.Fatalf("locking musicbrainz_id: %v", err)
	}
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if len(stored.LockedFields) != 1 {
		t.Fatalf("fixture: locked fields = %v, want the one lock", stored.LockedFields)
	}

	// Tier 1: a connected platform's index offers an ID under the same name.
	// PRECONDITION: it auto-links, or zero rows would prove nothing.
	idx := &connectionIndex{byName: map[string][]connEntry{
		"pinned blank": {{Name: "Pinned Blank", MusicBrainzID: mbidProposed}},
	}}
	if got := r.identifyArtist(ctx, stored, idx); got.Outcome != outcomeAutoLinked {
		t.Fatalf("fixture: Outcome = %v, want autoLinked, or the write was never attempted", got.Outcome)
	}

	got, rows := reloadMBID(t, artistSvc, a.ID), mbidHistoryRows(t, histSvc, a.ID)
	if got != "" || len(rows) != 0 {
		t.Errorf("stored MBID = %q, history rows = %+v; want still blank and no rows", got, rows)
	}
}

// TestProducerStamps_ProviderAndOperatorRowsDifferOnlyInProducer is the diff
// issue #3078 asks for: the same field written once with a provider-supplied
// value and once with an operator-typed one, both through the operator write
// path. With the per-row identity (id, values, timestamp) blanked, the two
// stored rows must be identical in everything except Producer -- which is the
// whole defect: before the column existed they were identical, full stop.
func TestProducerStamps_ProviderAndOperatorRowsDifferOnlyInProducer(t *testing.T) {
	t.Parallel()
	r, artistSvc, historySvc := testRouterWithHistory(t)
	artistSvc.SetHistoryService(historySvc)
	a := addTestArtist(t, artistSvc, "Diff Producer Artist")

	for _, form := range []url.Values{
		{"value": {"bio a provider supplied"}, "producer": {"provider:lastfm"}},
		{"value": {"bio the operator typed"}, "producer": {"operator"}},
	} {
		if w := patchField(t, r, a.ID, "biography", form); w.Code != http.StatusOK {
			t.Fatalf("writing biography: status = %d, body: %s", w.Code, w.Body.String())
		}
	}
	supplied := producerRowFor(t, historySvc, a.ID, "biography", "bio a provider supplied")
	typed := producerRowFor(t, historySvc, a.ID, "biography", "bio the operator typed")

	// blank removes what is unique to any single row, leaving what the row
	// says about the write.
	blank := func(c artist.MetadataChange) artist.MetadataChange {
		c.ID, c.OldValue, c.NewValue = "", "", ""
		c.CreatedAt = supplied.CreatedAt
		return c
	}
	suppliedBlank, typedBlank := blank(supplied), blank(typed)
	if suppliedBlank == typedBlank {
		t.Fatalf("the two rows are indistinguishable: %+v", suppliedBlank)
	}
	if supplied.Producer != "provider:lastfm" || typed.Producer != artist.ProducerOperator {
		t.Errorf("producers = %q / %q, want provider:lastfm / operator", supplied.Producer, typed.Producer)
	}
	// With Producer blanked as well, nothing else may differ.
	suppliedBlank.Producer, typedBlank.Producer = "", ""
	if suppliedBlank != typedBlank {
		t.Errorf("rows differ in more than Producer:\n provider: %+v\n operator: %+v", suppliedBlank, typedBlank)
	}
}
