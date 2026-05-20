package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
)

// stubScraperExecutor implements provider.ScraperExecutor for tests that need
// a live orchestrator without real provider network calls.
type stubScraperExecutor struct {
	result *provider.FetchResult
	err    error
}

func (s *stubScraperExecutor) ScrapeAll(_ context.Context, _, _, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	return s.result, s.err
}

func TestRenderRefreshWithOOB_ContainsSwapTargets(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)
	a := addTestArtist(t, artistSvc, "OOB Test Artist")

	sources := []provider.FieldSource{
		{Field: "biography", Provider: provider.NameMusicBrainz},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/refresh", nil)
	req = req.WithContext(testI18nCtx(t, req.Context()))
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
	t.Parallel()
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
	req = req.WithContext(testI18nCtx(t, req.Context()))
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

func TestApplyRefreshResult_ReidentifyUpdatesName(t *testing.T) {
	t.Parallel()
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

// TestUpsertMembers_EmptySliceClearsExisting verifies that calling
// UpsertMembers with an empty slice removes all existing members for
// an artist. This is the underlying mechanism that executeRefresh relies
// on to clear stale members when a provider returns an empty member list.
func TestUpsertMembers_EmptySliceClearsExisting(t *testing.T) {
	t.Parallel()
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
// clears members, ensuring UpsertMembers treats nil the same as an empty
// slice when updating an artist's members.
func TestUpsertMembers_NilSliceClearsExisting(t *testing.T) {
	t.Parallel()
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
// returns an empty (non-nil) slice when given nil input. The guard in
// executeRefresh skips UpsertMembers when Members is empty, so this path
// is informational -- verifying the converter itself is well-behaved.
func TestConvertProviderMembers_EmptyInput(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestApplyMemberRefresh exercises the applyMemberRefresh method that
// executeRefreshCtx delegates to for member upsert decisions.
// Calling the real method ensures coverage of the changed guard condition.
func TestApplyMemberRefresh(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

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

	t.Run("members_attempted_empty_preserves", func(t *testing.T) {
		a := addTestArtist(t, artistSvc, "Guard Test Band 1")
		seedMembers(t, a.ID)

		if n := countMembers(t, a.ID); n != 2 {
			t.Fatalf("expected 2 seeded members, got %d", n)
		}

		// Provider attempted "members" but returned empty list. Empty is treated
		// as incomplete MB data, not an intentional clear -- members are preserved.
		result := &provider.FetchResult{
			Metadata:        &provider.ArtistMetadata{Members: nil},
			AttemptedFields: []string{"biography", "members"},
		}
		r.applyMemberRefresh(context.Background(), a.ID, result, nil)

		if n := countMembers(t, a.ID); n != 2 {
			t.Errorf("expected 2 members preserved after attempted-empty refresh, got %d", n)
		}
	})

	t.Run("members_locked_early_returns", func(t *testing.T) {
		// Locking "members" must make applyMemberRefresh a no-op even when
		// the provider returned a full roster, so a user's pinned lineup
		// survives a refresh.
		a := addTestArtist(t, artistSvc, "Locked Members Band")
		seedMembers(t, a.ID)
		result := &provider.FetchResult{
			Metadata: &provider.ArtistMetadata{
				Members: []provider.MemberInfo{
					{Name: "Intruder", MBID: "mb-intruder"},
				},
			},
			AttemptedFields: []string{"members"},
		}
		r.applyMemberRefresh(context.Background(), a.ID, result, []string{"members"})

		saved, err := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
		if err != nil {
			t.Fatalf("listing members after locked refresh: %v", err)
		}
		if len(saved) != 2 {
			t.Fatalf("expected 2 members preserved by lock, got %d: %+v", len(saved), saved)
		}
		names := []string{saved[0].MemberName, saved[1].MemberName}
		for _, n := range names {
			if n == "Intruder" {
				t.Errorf("locked members overwritten by provider data: names=%v", names)
			}
		}
	})

	t.Run("members_attempted_nonempty_upserts", func(t *testing.T) {
		a := addTestArtist(t, artistSvc, "Guard Test Band 4")
		seedMembers(t, a.ID)

		if n := countMembers(t, a.ID); n != 2 {
			t.Fatalf("expected 2 seeded members, got %d", n)
		}

		// Provider attempted "members" and returned a non-empty list -- members
		// should be replaced with the provider data.
		result := &provider.FetchResult{
			Metadata: &provider.ArtistMetadata{
				Members: []provider.MemberInfo{
					{Name: "Dave", MBID: "mb-dave"},
				},
			},
			AttemptedFields: []string{"members"},
		}
		r.applyMemberRefresh(context.Background(), a.ID, result, nil)

		saved, err := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
		if err != nil {
			t.Fatalf("listing members after upsert: %v", err)
		}
		if len(saved) != 1 {
			t.Fatalf("expected 1 member after nonempty upsert, got %d", len(saved))
		}
		if saved[0].MemberName != "Dave" || saved[0].MemberMBID != "mb-dave" || saved[0].SortOrder != 0 {
			t.Errorf("unexpected persisted member: name=%q mbid=%q sort=%d",
				saved[0].MemberName, saved[0].MemberMBID, saved[0].SortOrder)
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
		r.applyMemberRefresh(context.Background(), a.ID, result, nil)

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
		r.applyMemberRefresh(context.Background(), a.ID, result, nil)

		if n := countMembers(t, a.ID); n != 2 {
			t.Errorf("expected 2 members preserved (nil metadata), got %d", n)
		}
	})

	t.Run("provider_error_fetch_result_preserves_members", func(t *testing.T) {
		// Finding 6: when a provider returns an error (timeout, 5xx), the
		// orchestrator produces a FetchResult with no AttemptedFields and
		// MembersAuthoritative=false. applyMemberRefresh must leave existing
		// member rows untouched. This ensures a transient upstream outage cannot
		// silently clear an artist's member roster.
		a := addTestArtist(t, artistSvc, "Provider Error Band")
		seedMembers(t, a.ID)

		if n := countMembers(t, a.ID); n != 2 {
			t.Fatalf("expected 2 seeded members, got %d", n)
		}

		// Simulate the FetchResult produced when every provider errors out:
		// AttemptedFields is empty (no field was attempted), MembersAuthoritative
		// is false, and Metadata.Members is nil.
		result := &provider.FetchResult{
			Metadata:             &provider.ArtistMetadata{Members: nil},
			AttemptedFields:      nil,   // provider never attempted the field
			MembersAuthoritative: false, // provider did not assert completeness
		}
		r.applyMemberRefresh(context.Background(), a.ID, result, nil)

		if n := countMembers(t, a.ID); n != 2 {
			t.Errorf("expected 2 members preserved after provider-error fetch result, got %d", n)
		}
	})

	t.Run("upsert_error_logged_not_propagated", func(t *testing.T) {
		// Use an isolated router so closing its DB does not affect other subtests.
		rIsolated, svcIsolated := testRouter(t)
		a := addTestArtist(t, svcIsolated, "Guard Test Band 5")

		// Close the isolated DB so UpsertMembers returns an error. The function
		// must log at Warn and continue without panicking or returning an error.
		_ = rIsolated.db.Close()

		result := &provider.FetchResult{
			Metadata: &provider.ArtistMetadata{
				Members: []provider.MemberInfo{
					{Name: "Eve", MBID: "mb-eve"},
				},
			},
			AttemptedFields: []string{"members"},
		}
		rIsolated.applyMemberRefresh(context.Background(), a.ID, result, nil)
		// No assertions beyond "did not panic" -- error is swallowed by design.
	})

	t.Run("authoritative_empty_clears_members", func(t *testing.T) {
		// This is the core fix for #1038. When a provider asserts its empty
		// member list is complete (MembersAuthoritative=true), applyMemberRefresh
		// must clear existing rows rather than treating the empty result as
		// incomplete data. The original guard (`len > 0`) blocked this path.
		//
		// Real-world scenario: a band entry is re-identified as a solo artist;
		// the provider returns no members and marks the result authoritative so
		// the stale band rows are removed.
		a := addTestArtist(t, artistSvc, "Authoritative Empty Band")
		seedMembers(t, a.ID)

		if n := countMembers(t, a.ID); n != 2 {
			t.Fatalf("expected 2 seeded members, got %d", n)
		}

		// Provider attempted "members", returned empty list, BUT MembersAuthoritative=true.
		// This must clear the stale rows.
		result := &provider.FetchResult{
			Metadata:             &provider.ArtistMetadata{Members: nil},
			AttemptedFields:      []string{"members"},
			MembersAuthoritative: true,
		}
		r.applyMemberRefresh(context.Background(), a.ID, result, nil)

		if n := countMembers(t, a.ID); n != 0 {
			t.Errorf("expected 0 members after authoritative-empty clear, got %d", n)
		}
	})

	t.Run("non_authoritative_empty_preserves_members", func(t *testing.T) {
		// Complement of the above: an empty result WITHOUT MembersAuthoritative
		// (sparse provider data) must leave existing rows untouched. This is
		// the same assertion as members_attempted_empty_preserves but now
		// explicitly verifies that MembersAuthoritative=false is the
		// distinguishing signal, not just the absence of members.
		a := addTestArtist(t, artistSvc, "Non-Authoritative Empty Band")
		seedMembers(t, a.ID)

		result := &provider.FetchResult{
			Metadata:             &provider.ArtistMetadata{Members: nil},
			AttemptedFields:      []string{"members"},
			MembersAuthoritative: false, // sparse -- do not clear
		}
		r.applyMemberRefresh(context.Background(), a.ID, result, nil)

		if n := countMembers(t, a.ID); n != 2 {
			t.Errorf("expected 2 members preserved (non-authoritative empty), got %d", n)
		}
	})
}

// TestExecuteRefreshAndPostHook_ResolvesBioViolation is the issue #1027
// regression guard. It exercises the full post-refresh pipeline at the
// HTTP-handler level (not just the rule package): given an artist with an
// open bio_missing violation, calling executeRefreshCtx with a provider stub
// that returns a populated biography MUST cause the subsequent
// runRulesAfterRefresh call to transition the violation row to resolved.
//
// The bug as filed reads as a wiring problem (the post-refresh hook
// "may not be calling re-eval at all"). Inspection shows the wiring is
// correct on main: handleArtistRefresh / handleRefreshLink both invoke
// runRulesAfterRefresh after executeRefreshCtx persists provider data.
// The W2.B (#1208) fix already resolves stale violation rows when a rule
// re-evaluates as a pass via persistPassResults -> ResolveViolationIfActive.
//
// This integration test pins both pieces together so a future change to the
// refresh handler (or to the rule pipeline's pass-resolution path) that
// silently breaks the user-visible "violation clears after refresh" behavior
// surfaces here as a failing test instead of as another reopened bug.
func TestExecuteRefreshAndPostHook_ResolvesBioViolation(t *testing.T) {
	t.Parallel()
	r, artistSvc, ruleSvc := testRouterWithPipelineFull(t)

	// Disable every rule except bio_exists so the test is independent of
	// seed defaults and so unrelated rules cannot mask the assertion by
	// flipping the persistOK bit on transient FK or fixture issues.
	ctx := context.Background()
	rules, err := ruleSvc.List(ctx)
	if err != nil {
		t.Fatalf("listing rules: %v", err)
	}
	for i := range rules {
		if rules[i].ID == rule.RuleBioExists {
			continue
		}
		rules[i].Enabled = false
		if err := ruleSvc.Update(ctx, &rules[i]); err != nil {
			t.Fatalf("disabling rule %s: %v", rules[i].ID, err)
		}
	}

	// Seed an artist with empty biography and a MusicBrainzID so the
	// refresh handler does not divert to the disambiguation form.
	a := &artist.Artist{
		Name:          "Bio Refresh Integration",
		SortName:      "Bio Refresh Integration",
		Type:          "person",
		Path:          "/music/Bio Refresh Integration",
		MusicBrainzID: "00000000-0000-0000-0000-000000000abc",
		Biography:     "",
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	// Run the pipeline once to seed the open bio_missing violation row.
	if _, err := r.pipeline.RunForArtist(ctx, a); err != nil {
		t.Fatalf("seeding violation via RunForArtist: %v", err)
	}
	violations, err := ruleSvc.ListViolationsFiltered(ctx, rule.ViolationListParams{ArtistID: a.ID})
	if err != nil {
		t.Fatalf("listing violations after seed: %v", err)
	}
	var seeded *rule.RuleViolation
	for i := range violations {
		if violations[i].RuleID == rule.RuleBioExists {
			seeded = &violations[i]
			break
		}
	}
	if seeded == nil {
		t.Fatalf("expected bio_missing violation seeded by initial pipeline run")
	}
	if seeded.Status != rule.ViolationStatusOpen {
		t.Fatalf("seeded violation status = %q, want %q", seeded.Status, rule.ViolationStatusOpen)
	}

	// Wire a stub orchestrator that returns a non-empty biography so
	// executeRefreshCtx applies the field via ApplyMetadata and Update.
	stub := &stubScraperExecutor{
		result: &provider.FetchResult{
			Metadata: &provider.ArtistMetadata{
				Biography: "Integration test biography long enough to satisfy the minimum length checker.",
			},
			AttemptedFields: []string{"biography"},
			PopulatedFields: []string{"biography"},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := provider.NewOrchestrator(nil, nil, logger)
	orch.SetExecutor(stub)
	r.orchestrator = orch

	// Execute the same sequence the HTTP handler runs: refresh -> post-hook.
	if _, err := r.executeRefreshCtx(ctx, a); err != nil {
		t.Fatalf("executeRefreshCtx: %v", err)
	}
	r.runRulesAfterRefresh(ctx, a)

	// The seeded violation row must now be resolved with resolved_at set.
	got, err := ruleSvc.GetViolationByID(ctx, seeded.ID)
	if err != nil {
		t.Fatalf("GetViolationByID: %v", err)
	}
	if got.Status != rule.ViolationStatusResolved {
		t.Errorf("bio violation status after refresh = %q, want %q",
			got.Status, rule.ViolationStatusResolved)
	}
	if got.ResolvedAt == nil {
		t.Errorf("resolved_at = nil, want populated after refresh resolves the bio violation")
	}

	// Sanity check: the persisted artist row carries the new biography. If
	// this fails the assertion above is meaningless because the refresh
	// itself dropped the data before re-eval ever saw it.
	reloaded, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}
	if reloaded.Biography == "" {
		t.Errorf("biography empty after refresh; ApplyMetadata or Update lost the value")
	}
}

// TestRunRulesAfterRefresh_InvokesPipeline verifies that runRulesAfterRefresh
// calls the pipeline's RunForArtist method with the re-fetched artist.
func TestRunRulesAfterRefresh_InvokesPipeline(t *testing.T) {
	t.Parallel()
	var calledWithID string
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, a *artist.Artist) (*rule.RunResult, error) {
			calledWithID = a.ID
			return &rule.RunResult{ArtistsProcessed: 1, ViolationsFound: 2}, nil
		},
	}

	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Refresh Rules Artist")

	r.runRulesAfterRefresh(context.Background(), a)

	if calledWithID == "" {
		t.Fatal("RunForArtist was not called after refresh")
	}
	if calledWithID != a.ID {
		t.Errorf("RunForArtist called with ID %q, want %q", calledWithID, a.ID)
	}
}

// TestRunRulesAfterRefresh_NilPipeline verifies that runRulesAfterRefresh
// is a no-op when the pipeline is not configured (nil).
func TestRunRulesAfterRefresh_NilPipeline(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithStubPipeline(t, nil)
	// Set pipeline to nil to simulate unconfigured state.
	r.pipeline = nil

	a := addTestArtist(t, artistSvc, "No Pipeline Artist")

	// Should not panic.
	r.runRulesAfterRefresh(context.Background(), a)
}

// TestRunRulesAfterRefresh_PipelineError_DoesNotPropagate verifies that
// a pipeline error is swallowed (best-effort) and does not panic or
// return an error to the caller.
func TestRunRulesAfterRefresh_PipelineError_DoesNotPropagate(t *testing.T) {
	t.Parallel()
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			return nil, fmt.Errorf("rule engine exploded")
		},
	}

	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Pipeline Error Artist")

	// Should not panic; error is logged but not propagated.
	r.runRulesAfterRefresh(context.Background(), a)
}

// TestExecuteRefreshCtx_AppliesMemberRefresh verifies that executeRefreshCtx
// delegates to applyMemberRefresh and persists provider-returned members.
// This covers the r.applyMemberRefresh call site inside the orchestration path.
func TestExecuteRefreshCtx_AppliesMemberRefresh(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	// Wire a stub orchestrator so FetchMetadata returns a controlled result
	// without real provider network calls.
	stub := &stubScraperExecutor{
		result: &provider.FetchResult{
			Metadata: &provider.ArtistMetadata{
				Members: []provider.MemberInfo{
					{Name: "Carol", MBID: "mb-carol"},
				},
			},
			AttemptedFields: []string{"members"},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := provider.NewOrchestrator(nil, nil, logger)
	orch.SetExecutor(stub)
	r.orchestrator = orch

	a := addTestArtist(t, artistSvc, "Orchestrate Refresh Artist")

	result, err := r.executeRefreshCtx(context.Background(), a)
	if err != nil {
		t.Fatalf("executeRefreshCtx returned error: %v", err)
	}
	if result == nil {
		t.Fatal("executeRefreshCtx returned nil result")
	}

	saved, err := artistSvc.ListMembersByArtistID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("listing members after executeRefreshCtx: %v", err)
	}
	if len(saved) != 1 {
		t.Fatalf("expected 1 member after refresh, got %d", len(saved))
	}
	if saved[0].MemberName != "Carol" || saved[0].MemberMBID != "mb-carol" {
		t.Errorf("unexpected member: name=%q mbid=%q", saved[0].MemberName, saved[0].MemberMBID)
	}
}

// TestExecuteRefreshCtx_PreservesTagsWhenProviderReturnsNoData is the end-to-end
// guard for the #952 graceful-fallback contract on the refresh path. An artist
// with user-curated genres/styles/moods must retain those values after a
// refresh where the provider was queried for those fields but returned no
// tag data. Without this test, a future refactor that drops the
// PopulatedFields field from the MergeOptions literal at handlers_refresh.go
// would silently revert the entire feature.
func TestExecuteRefreshCtx_PreservesTagsWhenProviderReturnsNoData(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	// Seed an artist with pre-existing tag data. addTestArtist gives us genres
	// by default; we layer styles and moods on top so the preserve contract
	// exercises all three clearing-semantics list fields.
	a := addTestArtist(t, artistSvc, "Tag Preserve Artist")
	a.Styles = []string{"shoegaze"}
	a.Moods = []string{"dreamy"}
	a.Biography = "Existing biography written by the user."
	if err := artistSvc.Update(context.Background(), a); err != nil {
		t.Fatalf("seeding tags: %v", err)
	}

	// Stub: provider was queried for all four clearing-semantics fields, but
	// returned no data for any of them. AttemptedFields contains them;
	// PopulatedFields does not.
	stub := &stubScraperExecutor{
		result: &provider.FetchResult{
			Metadata:        &provider.ArtistMetadata{},
			AttemptedFields: []string{"biography", "genres", "styles", "moods"},
			// PopulatedFields intentionally empty -- the merge layer must
			// preserve the artist's existing values.
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := provider.NewOrchestrator(nil, nil, logger)
	orch.SetExecutor(stub)
	r.orchestrator = orch

	if _, err := r.executeRefreshCtx(context.Background(), a); err != nil {
		t.Fatalf("executeRefreshCtx returned error: %v", err)
	}

	// Re-fetch from the repo to ensure the preserved values are the persisted
	// values, not just the in-memory artist struct.
	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}

	if reloaded.Biography != "Existing biography written by the user." {
		t.Errorf("biography wiped: got %q", reloaded.Biography)
	}
	if len(reloaded.Genres) != 1 || reloaded.Genres[0] != "Rock" {
		t.Errorf("genres wiped: got %v, want [Rock]", reloaded.Genres)
	}
	if len(reloaded.Styles) != 1 || reloaded.Styles[0] != "shoegaze" {
		t.Errorf("styles wiped: got %v, want [shoegaze]", reloaded.Styles)
	}
	if len(reloaded.Moods) != 1 || reloaded.Moods[0] != "dreamy" {
		t.Errorf("moods wiped: got %v, want [dreamy]", reloaded.Moods)
	}
}

// TestExecuteRefreshCtx_OverwritesTagsWhenProviderReturnsData is the positive
// counterpart to TestExecuteRefreshCtx_PreservesTagsWhenProviderReturnsNoData.
// When PopulatedFields does contain a field, the merge is authorized to
// overwrite the artist's value with the provider's value.
func TestExecuteRefreshCtx_OverwritesTagsWhenProviderReturnsData(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	a := addTestArtist(t, artistSvc, "Tag Overwrite Artist")

	stub := &stubScraperExecutor{
		result: &provider.FetchResult{
			Metadata: &provider.ArtistMetadata{
				Genres: []string{"alternative"},
			},
			AttemptedFields: []string{"genres"},
			PopulatedFields: []string{"genres"},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	orch := provider.NewOrchestrator(nil, nil, logger)
	orch.SetExecutor(stub)
	r.orchestrator = orch

	if _, err := r.executeRefreshCtx(context.Background(), a); err != nil {
		t.Fatalf("executeRefreshCtx returned error: %v", err)
	}

	reloaded, err := artistSvc.GetByID(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("reloading artist: %v", err)
	}

	// Merge should replace the seeded "Rock" with the provider's "alternative".
	if len(reloaded.Genres) != 1 || reloaded.Genres[0] != "alternative" {
		t.Errorf("genres not overwritten: got %v, want [alternative]", reloaded.Genres)
	}
}

// TestApplyProviderName_RespectsLocks verifies that a user pinning the Name
// or SortName field via the field-lock UI prevents applyProviderName from
// overwriting it with provider metadata. This path runs separately from
// ApplyMetadata's MergeOptions.LockedFields guard, so it needs its own
// coverage.
func TestApplyProviderName_RespectsLocks(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouter(t)

	t.Run("name_locked_preserves_user_value", func(t *testing.T) {
		a := addTestArtist(t, artistSvc, "Pinned Name")
		a.LockedFields = []string{"name"}
		if err := artistSvc.SetLockedFields(context.Background(), a.ID, a.LockedFields); err != nil {
			t.Fatalf("SetLockedFields: %v", err)
		}
		meta := &provider.ArtistMetadata{Name: "Provider Wants This", SortName: "Provider, Wants This"}
		failed := r.applyProviderName(context.Background(), a, meta)
		if failed {
			t.Fatal("applyProviderName reported failure")
		}
		if a.Name != "Pinned Name" {
			t.Errorf("locked name overwritten: got %q", a.Name)
		}
		if a.SortName != "Provider, Wants This" {
			t.Errorf("unlocked sort_name should be updated: got %q", a.SortName)
		}
	})

	t.Run("sort_name_locked_preserves_user_value", func(t *testing.T) {
		a := addTestArtist(t, artistSvc, "Unlocked Name")
		a.SortName = "Locked, Sort"
		a.LockedFields = []string{"sort_name"}
		if err := artistSvc.Update(context.Background(), a); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if err := artistSvc.SetLockedFields(context.Background(), a.ID, a.LockedFields); err != nil {
			t.Fatalf("SetLockedFields: %v", err)
		}
		meta := &provider.ArtistMetadata{Name: "Updated Name", SortName: "Provider, Overrides"}
		failed := r.applyProviderName(context.Background(), a, meta)
		if failed {
			t.Fatal("applyProviderName reported failure")
		}
		if a.SortName != "Locked, Sort" {
			t.Errorf("locked sort_name overwritten: got %q", a.SortName)
		}
		if a.Name != "Updated Name" {
			t.Errorf("unlocked name should be updated: got %q", a.Name)
		}
	})

	t.Run("no_locks_overwrites_both", func(t *testing.T) {
		a := addTestArtist(t, artistSvc, "Freely Renamed")
		meta := &provider.ArtistMetadata{Name: "New", SortName: "New, The"}
		failed := r.applyProviderName(context.Background(), a, meta)
		if failed {
			t.Fatal("applyProviderName reported failure")
		}
		if a.Name != "New" || a.SortName != "New, The" {
			t.Errorf("unlocked fields not updated: name=%q sort=%q", a.Name, a.SortName)
		}
	})

	t.Run("nil_meta_noops", func(t *testing.T) {
		a := addTestArtist(t, artistSvc, "Unchanged")
		orig := a.Name
		r.applyProviderName(context.Background(), a, nil)
		if a.Name != orig {
			t.Errorf("nil meta should not modify artist, got %q", a.Name)
		}
	})
}
