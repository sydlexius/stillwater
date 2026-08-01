package rule

import (
	"context"
	"sync"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// These two tests pin the SYNCHRONIZATION half of the #2858 album gate rather
// than its decision table: SetAlbumGate is a late-wiring setter called after
// construction (cmd/stillwater/main.go wires both of these once the provider
// registry has resolved a MusicBrainz release-group fetcher), while the field
// is READ on a hot path that runs concurrently -- the bulk fetch-images job's
// MBID self-heal, and the auto-fix pass's nfo_has_mbid fixer, which the
// pipeline runs across parallel artist workers.
//
// Both drive that exact overlap: one goroutine hammers the setter while another
// drives the read path. Under `go test -race` they report a DATA RACE if the
// field is read without the lock, and they pass silently once it is guarded.
//
// The fixtures deliberately make the gate BLOCK (a full local album directory
// against a 10%-overlap candidate), for two reasons. It keeps every iteration
// free of database writes and artist mutation, so the only shared state under
// test is the gate field itself and a failure cannot be some unrelated race.
// And the blocked COUNT is asserted afterwards: if the read site were ever
// short-circuited, the loop would spin without touching the field and the test
// would prove nothing.
//
// The setter alternates between two EQUIVALENT blocking gates, so whichever one
// a given iteration observes, the decision is the same and the assertion stays
// deterministic.

// albumGateRaceIterations is high enough that the setter and the reader
// genuinely interleave on a loaded machine, and low enough to stay well inside
// the package's normal runtime (each iteration is a filesystem album read plus
// an in-memory stub fetch; no network, no database).
const albumGateRaceIterations = 300

// TestBulkExecutor_SetAlbumGate_ConcurrentWithSelfHeal races SetAlbumGate
// against BulkExecutor.selfHealMBID, the bulk fetch-images hot path.
func TestBulkExecutor_SetAlbumGate_ConcurrentWithSelfHeal(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	// 10% overlap: clears every name gate, refused only by the album gate, so
	// each iteration ends before any write and leaves the artist untouched.
	e, svc, a := bulkGateArtist(t, gateAlbums, &stubReleaseGroupFetcher{groups: gateReleaseGroups(1)}, results)
	assertLocalEvidence(t, a.Path, artist.EvidenceFound, len(gateAlbums))

	// PRECONDITION: a single call really does block. Without this the loop below
	// could be measuring an allow path that never consults the gate.
	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)
	assertBulkNotAdopted(t, svc, a, status, msg)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < albumGateRaceIterations; i++ {
			// Equivalent gates, so the reader's verdict is stable whichever it sees.
			e.SetAlbumGate(artist.NewFilesystemAlbumSource(), &stubReleaseGroupFetcher{groups: gateReleaseGroups(1)})
		}
	}()

	blocked := 0
	go func() {
		defer wg.Done()
		for i := 0; i < albumGateRaceIterations; i++ {
			if s, _ := e.selfHealMBID(context.Background(), a, BulkModeYOLO); s == BulkItemSkipped {
				blocked++
			}
		}
	}()

	wg.Wait()

	if blocked != albumGateRaceIterations {
		t.Fatalf("gate-blocked self-heals = %d, want %d: every iteration must reach the album-gate read, or this test races nothing", blocked, albumGateRaceIterations)
	}
	if a.MusicBrainzID != "" {
		t.Errorf("a.MusicBrainzID = %q, want empty: no iteration should have adopted a blocked candidate", a.MusicBrainzID)
	}
}

// TestMetadataFixer_SetAlbumGate_ConcurrentWithFixMBID races SetAlbumGate
// against MetadataFixer.fixMBID, which the auto-fix pipeline runs concurrently
// across artist workers.
func TestMetadataFixer_SetAlbumGate_ConcurrentWithFixMBID(t *testing.T) {
	results := gateSearchResults()
	assertNameGatesPass(t, results)

	dir := gateArtistDir(t, gateAlbums)
	assertLocalEvidence(t, dir, artist.EvidenceFound, len(gateAlbums))

	f := gatedMBIDFixer(t, &stubReleaseGroupFetcher{groups: gateReleaseGroups(1)}, results)

	// PRECONDITION: one call blocks, so the loop below is exercising the gate
	// read rather than an early return upstream of it. Each goroutine iteration
	// uses its OWN artist value so a shared a.MusicBrainzID cannot itself race.
	if fr := fixMBIDFor(t, f, &artist.Artist{Name: gateArtistName, Path: dir}); fr.Fixed {
		t.Fatalf("fixture precondition: the candidate must be BLOCKED by the album gate, got Fixed=true (message: %q)", fr.Message)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < albumGateRaceIterations; i++ {
			f.SetAlbumGate(artist.NewFilesystemAlbumSource(), &stubReleaseGroupFetcher{groups: gateReleaseGroups(1)})
		}
	}()

	blocked := 0
	go func() {
		defer wg.Done()
		for i := 0; i < albumGateRaceIterations; i++ {
			fr, err := f.Fix(context.Background(), &artist.Artist{Name: gateArtistName, Path: dir}, &Violation{RuleID: RuleNFOHasMBID})
			if err == nil && fr != nil && !fr.Fixed {
				blocked++
			}
		}
	}()

	wg.Wait()

	if blocked != albumGateRaceIterations {
		t.Fatalf("gate-blocked fixes = %d, want %d: every iteration must reach the album-gate read, or this test races nothing", blocked, albumGateRaceIterations)
	}
}
