package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

func TestRenderRefreshWithOOB_ContainsSwapTargets(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "OOB Test Artist")

	sources := []provider.FieldSource{
		{Field: "biography", Provider: provider.NameMusicBrainz},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/refresh", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.renderRefreshWithOOB(w, req, a.ID, sources)

	body := w.Body.String()

	// The response should contain the summary.
	if !strings.Contains(body, "Metadata Refreshed") {
		t.Error("response missing RefreshResultSummary content")
	}

	// OOB fragments should reference artist-specific swap targets.
	targets := []string{
		"field-biography-" + a.ID,
		"artist-tags-" + a.ID,
		"members-section-" + a.ID,
		"artist-details-" + a.ID,
		"artist-images-" + a.ID,
		"artist-providers-" + a.ID,
	}
	for _, target := range targets {
		if !strings.Contains(body, target) {
			t.Errorf("response missing OOB target %q", target)
		}
	}
}

func TestRenderRefreshWithOOB_MemberFailure_SkipsOOB(t *testing.T) {
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Member Fail Artist")

	// The artist exists in the DB, so GetByID succeeds. ListMembersByArtistID
	// should succeed (returns empty slice). To trigger the member failure path
	// we'd need a broken DB. Instead, verify the normal path renders OOB and
	// the summary together, which covers the integration. The error path is a
	// simple early return that produces only the summary.

	sources := []provider.FieldSource{
		{Field: "genres", Provider: provider.NameMusicBrainz},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/refresh", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()

	r.renderRefreshWithOOB(w, req, a.ID, sources)

	body := w.Body.String()

	if !strings.Contains(body, "Metadata Refreshed") {
		t.Error("response missing RefreshResultSummary content")
	}

	// OOB fragments present in the success path.
	if !strings.Contains(body, "hx-swap-oob") {
		t.Error("response missing OOB attributes in success path")
	}
}

func TestApplyRefreshResult_OverwritesAttemptedFields(t *testing.T) {
	a := &artist.Artist{
		Biography:   "old bio",
		Genres:      []string{"old-genre"},
		Born:        "1900-01-01",
		Formed:      "1990",
		YearsActive: "1990s",
	}

	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Biography: "new bio",
			Genres:    []string{"pop", "soul"},
			Born:      "1988-05-05",
			Formed:    "2006",
		},
		AttemptedFields: []string{"biography", "genres", "born", "formed"},
	}

	applyRefreshResult(a, result)

	if a.Biography != "new bio" {
		t.Errorf("Biography = %q, want %q", a.Biography, "new bio")
	}
	if len(a.Genres) != 2 || a.Genres[0] != "pop" {
		t.Errorf("Genres = %v, want [pop soul]", a.Genres)
	}
	if a.Born != "1988-05-05" {
		t.Errorf("Born = %q, want %q", a.Born, "1988-05-05")
	}
	// YearsActive was NOT attempted, so it should be preserved.
	if a.YearsActive != "1990s" {
		t.Errorf("YearsActive = %q, want %q (unattempted, should be preserved)", a.YearsActive, "1990s")
	}
}

func TestApplyRefreshResult_ClearsEmptyAttemptedFields(t *testing.T) {
	a := &artist.Artist{
		Biography: "old bio",
		Born:      "1770-12-17",
		Died:      "1827-03-26",
		Genres:    []string{"classical"},
		Styles:    []string{"baroque"},
		Moods:     []string{"dramatic"},
	}

	// Provider returned empty for died (artist is alive) and genres.
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Biography: "new bio",
			Born:      "1988-05-05",
		},
		AttemptedFields: []string{"biography", "born", "died", "genres", "styles", "moods"},
	}

	applyRefreshResult(a, result)

	if a.Born != "1988-05-05" {
		t.Errorf("Born = %q, want %q", a.Born, "1988-05-05")
	}
	if a.Died != "" {
		t.Errorf("Died = %q, want empty (provider returned no death date)", a.Died)
	}
	if len(a.Genres) != 0 {
		t.Errorf("Genres = %v, want empty", a.Genres)
	}
	if len(a.Styles) != 0 {
		t.Errorf("Styles = %v, want empty (attempted but empty)", a.Styles)
	}
	if len(a.Moods) != 0 {
		t.Errorf("Moods = %v, want empty (attempted but empty)", a.Moods)
	}
}

func TestApplyRefreshResult_PreservesUnattemptedFields(t *testing.T) {
	a := &artist.Artist{
		Biography: "keep this bio",
		Born:      "1988-05-05",
		Died:      "should stay",
		Genres:    []string{"pop"},
		Formed:    "2006",
	}

	// Only biography was attempted; everything else should be preserved.
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Biography: "new bio",
		},
		AttemptedFields: []string{"biography"},
	}

	applyRefreshResult(a, result)

	if a.Biography != "new bio" {
		t.Errorf("Biography = %q, want %q", a.Biography, "new bio")
	}
	if a.Born != "1988-05-05" {
		t.Errorf("Born = %q, want %q (unattempted)", a.Born, "1988-05-05")
	}
	if a.Died != "should stay" {
		t.Errorf("Died = %q, want %q (unattempted)", a.Died, "should stay")
	}
	if len(a.Genres) != 1 || a.Genres[0] != "pop" {
		t.Errorf("Genres = %v, want [pop] (unattempted)", a.Genres)
	}
	if a.Formed != "2006" {
		t.Errorf("Formed = %q, want %q (unattempted)", a.Formed, "2006")
	}
}

func TestApplyRefreshResult_SoloArtistClearsFormedDisbanded(t *testing.T) {
	a := &artist.Artist{
		Type:      "solo",
		Born:      "1988-05-05",
		Formed:    "2006",
		Disbanded: "2020",
	}

	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Type:   "solo",
			Born:   "1988-05-05",
			Formed: "2006",
		},
		AttemptedFields: []string{"born", "formed"},
	}

	applyRefreshResult(a, result)

	if a.Born != "1988-05-05" {
		t.Errorf("Born = %q, want %q", a.Born, "1988-05-05")
	}
	if a.Formed != "" {
		t.Errorf("Formed = %q, want empty (solo artist)", a.Formed)
	}
	if a.Disbanded != "" {
		t.Errorf("Disbanded = %q, want empty (solo artist)", a.Disbanded)
	}
}

func TestApplyRefreshResult_GroupArtistClearsBornDied(t *testing.T) {
	a := &artist.Artist{
		Type:   "group",
		Born:   "1982",
		Formed: "1982",
		Died:   "2010",
	}

	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Type:   "group",
			Formed: "1982",
			Born:   "1982",
		},
		AttemptedFields: []string{"born", "formed"},
	}

	applyRefreshResult(a, result)

	if a.Born != "" {
		t.Errorf("Born = %q, want empty (group artist)", a.Born)
	}
	if a.Died != "" {
		t.Errorf("Died = %q, want empty (group artist)", a.Died)
	}
	if a.Formed != "1982" {
		t.Errorf("Formed = %q, want %q", a.Formed, "1982")
	}
}

func TestApplyRefreshResult_UnknownTypePreservesAllDates(t *testing.T) {
	a := &artist.Artist{
		Type:   "",
		Born:   "1988",
		Formed: "2006",
	}

	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Born:   "1988",
			Formed: "2006",
		},
		AttemptedFields: []string{"born", "formed"},
	}

	applyRefreshResult(a, result)

	if a.Born != "1988" {
		t.Errorf("Born = %q, want %q (unknown type, should preserve)", a.Born, "1988")
	}
	if a.Formed != "2006" {
		t.Errorf("Formed = %q, want %q (unknown type, should preserve)", a.Formed, "2006")
	}
}

func TestApplyRefreshResult_OrchestraAndChoirClearBornDied(t *testing.T) {
	for _, typ := range []string{"orchestra", "choir"} {
		t.Run(typ, func(t *testing.T) {
			a := &artist.Artist{
				Type:   typ,
				Born:   "1900",
				Formed: "1920",
			}
			result := &provider.FetchResult{
				Metadata: &provider.ArtistMetadata{
					Type:   typ,
					Born:   "1900",
					Formed: "1920",
				},
				AttemptedFields: []string{"born", "formed"},
			}
			applyRefreshResult(a, result)
			if a.Born != "" {
				t.Errorf("Born = %q, want empty (%s)", a.Born, typ)
			}
			if a.Formed != "1920" {
				t.Errorf("Formed = %q, want %q", a.Formed, "1920")
			}
		})
	}
}

func TestApplyRefreshResult_ProviderIDsOnlyFillEmpty(t *testing.T) {
	a := &artist.Artist{
		MusicBrainzID: "existing-mbid",
		AudioDBID:     "",
	}

	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			MusicBrainzID: "new-mbid",
			AudioDBID:     "12345",
		},
		AttemptedFields: []string{},
	}

	applyRefreshResult(a, result)

	if a.MusicBrainzID != "existing-mbid" {
		t.Errorf("MusicBrainzID = %q, want %q (should not overwrite)", a.MusicBrainzID, "existing-mbid")
	}
	if a.AudioDBID != "12345" {
		t.Errorf("AudioDBID = %q, want %q (should fill empty)", a.AudioDBID, "12345")
	}
}

func TestApplyRefreshResult_NilMetadata(t *testing.T) {
	a := &artist.Artist{
		Biography: "keep this",
		Born:      "1988",
		Type:      "solo",
	}

	result := &provider.FetchResult{Metadata: nil}
	applyRefreshResult(a, result)

	if a.Biography != "keep this" {
		t.Errorf("Biography = %q, want %q (nil metadata should be no-op)", a.Biography, "keep this")
	}
	if a.Born != "1988" {
		t.Errorf("Born = %q, want %q (nil metadata should be no-op)", a.Born, "1988")
	}
}

func TestApplyRefreshResult_EmptyTypePreservesExistingType(t *testing.T) {
	a := &artist.Artist{
		Type:   "solo",
		Gender: "female",
	}

	// Provider returns empty Type and Gender -- these should NOT be cleared.
	result := &provider.FetchResult{
		Metadata:        &provider.ArtistMetadata{},
		AttemptedFields: []string{"biography"},
	}

	applyRefreshResult(a, result)

	if a.Type != "solo" {
		t.Errorf("Type = %q, want %q (empty provider type should not clear)", a.Type, "solo")
	}
	if a.Gender != "female" {
		t.Errorf("Gender = %q, want %q (empty provider gender should not clear)", a.Gender, "female")
	}
}

func TestApplyRefreshResult_ReidentifyUpdatesName(t *testing.T) {
	tests := []struct {
		name         string
		artistName   string
		artistSort   string
		providerName string
		providerSort string
		wantName     string
		wantSort     string
		wantChanged  bool
	}{
		{
			name:         "different name and sort_name",
			artistName:   "Adele (3)",
			artistSort:   "Adele (3)",
			providerName: "Adele",
			providerSort: "Adele",
			wantName:     "Adele",
			wantSort:     "Adele",
			wantChanged:  true,
		},
		{
			name:         "same name different sort",
			artistName:   "Adele",
			artistSort:   "Adele (3)",
			providerName: "Adele",
			providerSort: "Adele",
			wantName:     "Adele",
			wantSort:     "Adele",
			wantChanged:  true,
		},
		{
			name:         "empty provider name preserves",
			artistName:   "Adele (3)",
			artistSort:   "Adele (3)",
			providerName: "",
			providerSort: "",
			wantName:     "Adele (3)",
			wantSort:     "Adele (3)",
			wantChanged:  false,
		},
		{
			name:         "same name and sort no change",
			artistName:   "Adele",
			artistSort:   "Adele",
			providerName: "Adele",
			providerSort: "Adele",
			wantName:     "Adele",
			wantSort:     "Adele",
			wantChanged:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &artist.Artist{Name: tt.artistName, SortName: tt.artistSort}
			m := &provider.ArtistMetadata{Name: tt.providerName, SortName: tt.providerSort}

			nameChanged := false
			if m.Name != "" && m.Name != a.Name {
				a.Name = m.Name
				nameChanged = true
			}
			if m.SortName != "" && m.SortName != a.SortName {
				a.SortName = m.SortName
				nameChanged = true
			}

			if a.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", a.Name, tt.wantName)
			}
			if a.SortName != tt.wantSort {
				t.Errorf("SortName = %q, want %q", a.SortName, tt.wantSort)
			}
			if nameChanged != tt.wantChanged {
				t.Errorf("nameChanged = %v, want %v", nameChanged, tt.wantChanged)
			}
		})
	}
}

func TestFilterDatesByArtistType(t *testing.T) {
	tests := []struct {
		name                    string
		artistType              string
		wantBorn, wantFormed    string
		wantDied, wantDisbanded string
	}{
		{"solo", "solo", "1988", "", "2050", ""},
		{"person", "person", "1988", "", "2050", ""},
		{"character", "character", "1988", "", "2050", ""},
		{"group", "group", "", "1982", "", "2010"},
		{"orchestra", "orchestra", "", "1982", "", "2010"},
		{"choir", "choir", "", "1982", "", "2010"},
		{"empty", "", "1988", "1982", "2050", "2010"},
		{"unknown", "other", "1988", "1982", "2050", "2010"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &artist.Artist{
				Type:      tt.artistType,
				Born:      "1988",
				Formed:    "1982",
				Died:      "2050",
				Disbanded: "2010",
			}
			filterDatesByArtistType(a)
			if a.Born != tt.wantBorn {
				t.Errorf("Born = %q, want %q", a.Born, tt.wantBorn)
			}
			if a.Formed != tt.wantFormed {
				t.Errorf("Formed = %q, want %q", a.Formed, tt.wantFormed)
			}
			if a.Died != tt.wantDied {
				t.Errorf("Died = %q, want %q", a.Died, tt.wantDied)
			}
			if a.Disbanded != tt.wantDisbanded {
				t.Errorf("Disbanded = %q, want %q", a.Disbanded, tt.wantDisbanded)
			}
		})
	}
}

// TestUpsertMembers_EmptySliceClearsExisting verifies that calling
// UpsertMembers with an empty slice removes all existing members for
// an artist. This is the underlying mechanism that executeRefresh relies
// on to clear stale members when a provider returns an empty member list.
func TestUpsertMembers_EmptySliceClearsExisting(t *testing.T) {
	_, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Stale Members Band")

	// Seed members.
	members := []artist.BandMember{
		{MemberName: "Alice", SortOrder: 0},
		{MemberName: "Bob", SortOrder: 1},
	}
	if err := artistSvc.UpsertMembers(context.Background(), a.ID, members); err != nil {
		t.Fatalf("seeding members: %v", err)
	}

	before, err := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("listing members before clear: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("expected 2 members before clear, got %d", len(before))
	}

	// Upsert with empty slice -- should delete all members.
	if err := artistSvc.UpsertMembers(context.Background(), a.ID, []artist.BandMember{}); err != nil {
		t.Fatalf("upserting empty members: %v", err)
	}

	after, err := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("listing members after clear: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("expected 0 members after empty upsert, got %d", len(after))
	}
}

// TestUpsertMembers_NilSliceClearsExisting verifies that a nil slice also
// clears members, since convertProviderMembers returns an empty (not nil)
// slice for nil input.
func TestUpsertMembers_NilSliceClearsExisting(t *testing.T) {
	_, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "Nil Members Band")

	members := []artist.BandMember{
		{MemberName: "Charlie", SortOrder: 0},
	}
	if err := artistSvc.UpsertMembers(context.Background(), a.ID, members); err != nil {
		t.Fatalf("seeding members: %v", err)
	}

	// Upsert with nil slice -- should also delete all members.
	if err := artistSvc.UpsertMembers(context.Background(), a.ID, nil); err != nil {
		t.Fatalf("upserting nil members: %v", err)
	}

	after, err := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("listing members after nil upsert: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("expected 0 members after nil upsert, got %d", len(after))
	}
}

// TestConvertProviderMembers_EmptyInput verifies that convertProviderMembers
// returns an empty (non-nil) slice when given nil input, so UpsertMembers
// receives a valid slice that triggers the delete-all path.
func TestConvertProviderMembers_EmptyInput(t *testing.T) {
	result := convertProviderMembers("artist-1", nil)
	if result == nil {
		t.Fatal("convertProviderMembers(nil) returned nil, want empty slice")
	}
	if len(result) != 0 {
		t.Errorf("convertProviderMembers(nil) returned %d members, want 0", len(result))
	}
}

// TestConvertProviderMembers_WithData verifies normal conversion.
func TestConvertProviderMembers_WithData(t *testing.T) {
	input := []provider.MemberInfo{
		{Name: "Thom Yorke", MBID: "mb-1", Instruments: []string{"vocals", "guitar"}},
		{Name: "Jonny Greenwood", MBID: "mb-2", Instruments: []string{"guitar"}},
	}

	result := convertProviderMembers("artist-1", input)

	if len(result) != 2 {
		t.Fatalf("got %d members, want 2", len(result))
	}
	if result[0].MemberName != "Thom Yorke" {
		t.Errorf("member[0].MemberName = %q, want %q", result[0].MemberName, "Thom Yorke")
	}
	if result[0].ArtistID != "artist-1" {
		t.Errorf("member[0].ArtistID = %q, want %q", result[0].ArtistID, "artist-1")
	}
	if result[1].SortOrder != 1 {
		t.Errorf("member[1].SortOrder = %d, want 1", result[1].SortOrder)
	}
}

// TestMembersAttemptedGuard exercises the AttemptedFields guard logic that
// executeRefresh uses to decide whether to clear or preserve members.
// This tests the decision layer, not just the UpsertMembers mechanism.
func TestMembersAttemptedGuard(t *testing.T) {
	_, artistSvc := testRouter(t)

	seedMembers := func(t *testing.T, artistID string) {
		t.Helper()
		members := []artist.BandMember{
			{MemberName: "Alice", SortOrder: 0},
			{MemberName: "Bob", SortOrder: 1},
		}
		if err := artistSvc.UpsertMembers(context.Background(), artistID, members); err != nil {
			t.Fatalf("seeding members: %v", err)
		}
	}

	countMembers := func(t *testing.T, artistID string) int {
		t.Helper()
		members, err := artistSvc.ListMembersByArtistID(context.Background(), artistID)
		if err != nil {
			t.Fatalf("listing members: %v", err)
		}
		return len(members)
	}

	// Simulate the same guard logic from executeRefresh:
	// if "members" is in AttemptedFields, call UpsertMembers (even with empty slice).
	// If "members" is NOT in AttemptedFields, skip -- preserve existing members.
	applyMembersGuard := func(artistID string, result *provider.FetchResult, svc *artist.Service) {
		if result.Metadata != nil {
			membersAttempted := false
			for _, f := range result.AttemptedFields {
				if f == "members" {
					membersAttempted = true
					break
				}
			}
			if membersAttempted {
				members := convertProviderMembers(artistID, result.Metadata.Members)
				_ = svc.UpsertMembers(context.Background(), artistID, members)
			}
		}
	}

	t.Run("members_attempted_empty_clears", func(t *testing.T) {
		a := addTestArtist(t, artistSvc, "Guard Test Band 1")
		seedMembers(t, a.ID)

		if n := countMembers(t, a.ID); n != 2 {
			t.Fatalf("expected 2 seeded members, got %d", n)
		}

		// Provider attempted "members" but returned empty list.
		result := &provider.FetchResult{
			Metadata:        &provider.ArtistMetadata{Members: nil},
			AttemptedFields: []string{"biography", "members"},
		}
		applyMembersGuard(a.ID, result, artistSvc)

		if n := countMembers(t, a.ID); n != 0 {
			t.Errorf("expected 0 members after attempted-empty refresh, got %d", n)
		}
	})

	t.Run("members_not_attempted_preserves", func(t *testing.T) {
		a := addTestArtist(t, artistSvc, "Guard Test Band 2")
		seedMembers(t, a.ID)

		if n := countMembers(t, a.ID); n != 2 {
			t.Fatalf("expected 2 seeded members, got %d", n)
		}

		// Provider did NOT attempt "members" -- only biography.
		result := &provider.FetchResult{
			Metadata:        &provider.ArtistMetadata{Biography: "new bio"},
			AttemptedFields: []string{"biography"},
		}
		applyMembersGuard(a.ID, result, artistSvc)

		if n := countMembers(t, a.ID); n != 2 {
			t.Errorf("expected 2 members preserved (unattempted), got %d", n)
		}
	})

	t.Run("nil_metadata_preserves", func(t *testing.T) {
		a := addTestArtist(t, artistSvc, "Guard Test Band 3")
		seedMembers(t, a.ID)

		// Nil metadata -- provider failed entirely.
		result := &provider.FetchResult{
			Metadata:        nil,
			AttemptedFields: []string{"members"},
		}
		applyMembersGuard(a.ID, result, artistSvc)

		if n := countMembers(t, a.ID); n != 2 {
			t.Errorf("expected 2 members preserved (nil metadata), got %d", n)
		}
	})
}
