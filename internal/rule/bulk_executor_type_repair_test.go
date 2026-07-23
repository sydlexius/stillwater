package rule

import (
	"context"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// stubScrapeAll is a provider.ScraperExecutor that returns a fixed FetchResult.
// It is the narrowest seam into BulkExecutor.fetchMetadata's provider call:
// Orchestrator.FetchMetadata delegates to the executor when one is set, so no
// registry, settings service or network is involved.
type stubScrapeAll struct {
	result *provider.FetchResult
	calls  int
}

func (s *stubScrapeAll) ScrapeAll(_ context.Context, _, _, _ string, _ map[provider.ProviderName]string) (*provider.FetchResult, error) {
	s.calls++
	return s.result, nil
}

// newTypeRepairExecutor wires the minimum BulkExecutor needed to drive
// fetchMetadata against a stubbed provider result. publisher and pipeline are
// left nil, and NEITHER nil is a tripwire -- both fail quietly, so no test
// below may rest on them:
//
//   - publisher is reached on the changed==true branch, but
//     publish.Publisher.PublishMetadata opens with `if p == nil { return }`,
//     so a nil publisher silently no-ops instead of panicking. It genuinely
//     executes in TestBulkFetchMetadata_RealChangeStillPersists and does
//     nothing.
//   - pipeline is never dereferenced at all: fetchMetadata does not touch it,
//     and BulkExecutor.pipeline has no reader anywhere in the package.
//
// What the tests actually rest on is the DATABASE RE-READ each performs after
// fetchMetadata returns. The stored row is the side effect that cannot be
// faked by control flow, and it is what carries the proof.
func newTypeRepairExecutor(t *testing.T, artistSvc *artist.Service, result *provider.FetchResult) (*BulkExecutor, *stubScrapeAll) {
	t.Helper()

	stub := &stubScrapeAll{result: result}
	orch := provider.NewOrchestrator(nil, nil, testLogger(), nil)
	orch.SetExecutor(stub)

	return &BulkExecutor{
		artistService: artistSvc,
		orchestrator:  orch,
		logger:        testLogger(),
	}, stub
}

// TestBulkFetchMetadata_TypeRepairAloneReportsSkipped is the consumer-level
// proof for the #2748 changed-signal fix, taken at the seam that actually
// causes the harm.
//
// THE HARM: internal/rule/bulk_executor.go's fetchMetadata treats
// artist.ApplyMetadata's bool as "there is something to persist". On true it
// runs artistService.Update (a DB write plus a metadata_changes audit row) and
// publisher.PublishMetadata (an NFO rewrite on disk plus a push to
// Emby/Jellyfin). Before the fix, the post-merge type-consistency repair fed
// that bool, so a bulk sweep that fetched NOTHING NEW still persisted and
// PUBLISHED every pre-existing inconsistent row it walked past -- for artists
// the operator never touched.
//
// WHY BulkItemSkipped IS THE RIGHT ASSERTION: Update and PublishMetadata both
// sit AFTER the `if !changed { return BulkItemSkipped }` early return, so a
// Skipped status is a complete proof that neither ran. The DB re-read below
// makes that independent of reading the control flow: the stored row still
// carries the bad gender, which it could not if Update had fired.
//
// The test also asserts the two things that would make it vacuous:
//   - the provider stub was actually called (otherwise "skipped" would merely
//     mean the early already-has-MBID-and-biography bailout fired);
//   - the IN-MEMORY artist gender was still repaired (otherwise this would pass
//     on an implementation that deleted the repair outright, regressing #2748).
func TestBulkFetchMetadata_TypeRepairAloneReportsSkipped(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)

	// An artist that is ALREADY inconsistent in the database: a group type
	// carrying a gender. Nothing about this row is the merge's doing.
	a := &artist.Artist{
		Name:          "Repair Only Collective",
		SortName:      "Repair Only Collective",
		Type:          "group",
		Gender:        "female",
		MusicBrainzID: "mbid-repair-only",
		Path:          t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if a.Gender != "female" || a.Type != "group" {
		t.Fatalf("fixture precondition: want an inconsistent group/female row, got type=%q gender=%q", a.Type, a.Gender)
	}

	// The provider returns exactly what the artist already has, so the merge
	// itself moves nothing. This is the "bulk sweep found no new metadata"
	// case, which must not persist.
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Name:          a.Name,
			Type:          "group",
			MusicBrainzID: a.MusicBrainzID,
		},
		AttemptedProviders: []provider.ProviderName{provider.NameMusicBrainz},
	}

	e, stub := newTypeRepairExecutor(t, artistSvc, result)

	status, reason := e.fetchMetadata(ctx, a, BulkModeYOLO)

	if stub.calls != 1 {
		t.Fatalf("provider stub called %d times, want 1; fetchMetadata bailed out before "+
			"the merge, so a %q status proves nothing about the repair", stub.calls, status)
	}
	if status != BulkItemSkipped {
		t.Errorf("status = %q (%s), want %q; a repair of a pre-existing inconsistency "+
			"must not trigger the Update + PublishMetadata cycle", status, reason, BulkItemSkipped)
	}

	// The repair still had to happen in memory.
	if a.Gender != "" {
		t.Errorf("in-memory gender = %q, want cleared; the repair must still run, it "+
			"merely must not be the reason to persist", a.Gender)
	}

	// Independent of the control flow: the stored row is untouched. If Update
	// had run, the persisted gender would have been cleared too.
	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("re-reading artist: %v", err)
	}
	if stored.Gender != "female" {
		t.Errorf("stored gender = %q, want %q still on disk; a write reached the database "+
			"for an artist the operator never touched", stored.Gender, "female")
	}
}

// TestBulkFetchMetadata_RealChangeStillPersists is the positive control for the
// test above. Without it, that test would pass on a fetchMetadata that returned
// Skipped unconditionally -- or on an ApplyMetadata that always returned false.
//
// It asserts the DB write rather than the publish: the publisher is nil here
// and no-ops, so it can prove nothing. The DB write is the first of the two
// side effects and shares the same `changed` gate, so it is the one that
// carries the signal.
func TestBulkFetchMetadata_RealChangeStillPersists(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	artistSvc := artist.NewService(db)

	a := &artist.Artist{
		Name:          "Real Change Collective",
		SortName:      "Real Change Collective",
		Type:          "group",
		Gender:        "female",
		MusicBrainzID: "mbid-real-change",
		Path:          t.TempDir(),
	}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if a.Biography != "" {
		t.Fatalf("fixture precondition: biography must start empty, got %q", a.Biography)
	}

	// Same inconsistent row, but this time the provider genuinely brings
	// something new.
	result := &provider.FetchResult{
		Metadata: &provider.ArtistMetadata{
			Name:          a.Name,
			Type:          "group",
			MusicBrainzID: a.MusicBrainzID,
			Biography:     "a biography this sweep actually learned",
		},
		AttemptedProviders: []provider.ProviderName{provider.NameMusicBrainz},
	}

	e, _ := newTypeRepairExecutor(t, artistSvc, result)

	status, reason := e.fetchMetadata(ctx, a, BulkModeYOLO)

	if status != BulkItemFixed {
		t.Fatalf("status = %q (%s), want %q; a genuine metadata arrival must still persist",
			status, reason, BulkItemFixed)
	}

	stored, err := artistSvc.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("re-reading artist: %v", err)
	}
	if stored.Biography == "" {
		t.Error("stored biography is empty; the real change did not reach the database")
	}
	// The repair rides along on the write it did not itself trigger.
	if stored.Gender != "" {
		t.Errorf("stored gender = %q, want cleared; when a merge persists for a real "+
			"reason it must carry the type-consistency repair with it", stored.Gender)
	}
}
