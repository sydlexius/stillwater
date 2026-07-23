package artist

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/provider"
)

// queryProviderRow reads a single artist_provider_ids row straight from the
// database (never a counter or a code-under-test return value) so assertions
// are made against the actual persisted state. It returns whether the row
// exists plus its provider_id and fetched_at contents so callers can assert on
// substance, not just presence.
func queryProviderRow(t *testing.T, db *sql.DB, artistID, prov string) (exists bool, providerID, fetchedAt string) {
	t.Helper()
	var pid string
	var fa sql.NullString
	err := db.QueryRowContext(context.Background(),
		`SELECT provider_id, fetched_at FROM artist_provider_ids WHERE artist_id = ? AND provider = ?`,
		artistID, prov).Scan(&pid, &fa)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", ""
	}
	if err != nil {
		t.Fatalf("querying %s row for artist %s: %v", prov, artistID, err)
	}
	return true, pid, fa.String
}

// TestUpsertAll_OrphanProvidersSurviveUpdate is the #2725 repro inverted to
// green. Providers with a writable fetched_at row but no Artist struct field
// (allmusic, duckduckgo, fanarttv, genius, wikipedia) must NOT be destroyed by
// an ordinary Update. Before the scoped-delete fix, UpsertAll opened with an
// unconditional "DELETE ... WHERE artist_id = ?", so every Update wiped these
// rows because extractProviderIDs never re-emits them.
//
// For each orphan: stamp fetched_at, SELECT-assert the row exists as a real
// precondition (fails loudly if the stamp itself regressed -- not vacuous),
// run a plain Update, then SELECT-assert the row still exists with its
// fetched_at intact.
func TestUpsertAll_OrphanProvidersSurviveUpdate(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	orphans := []string{
		string(provider.NameAllMusic),
		string(provider.NameDuckDuckGo),
		string(provider.NameFanartTV),
		string(provider.NameGenius),
		string(provider.NameWikipedia),
	}

	for _, orphan := range orphans {
		t.Run(orphan, func(t *testing.T) {
			// Each subtest gets its own artist so parallel-friendly and
			// isolated. (Not t.Parallel(): shared svc/db, and cheap.)
			a := testArtist("Orphan "+orphan, "/music/orphan-"+orphan)
			if err := svc.Create(ctx, a); err != nil {
				t.Fatalf("Create: %v", err)
			}

			// Stamp the orphan's fetched_at bookkeeping row.
			if err := svc.UpdateProviderFetchedAt(ctx, a.ID, orphan); err != nil {
				t.Fatalf("UpdateProviderFetchedAt(%s): %v", orphan, err)
			}

			// PRECONDITION (real assertion): the orphan row must exist now,
			// with a non-empty fetched_at and an empty provider_id. If this
			// fails the test is not vacuous -- it means the stamp path broke.
			exists, pid, fetchedAt := queryProviderRow(t, db, a.ID, orphan)
			if !exists {
				t.Fatalf("precondition: %s row missing after stamp; nothing to protect", orphan)
			}
			if fetchedAt == "" {
				t.Fatalf("precondition: %s row has empty fetched_at %q; stamp did not record a timestamp", orphan, fetchedAt)
			}
			if pid != "" {
				t.Fatalf("precondition: %s row has unexpected provider_id %q; orphan rows carry only fetched_at", orphan, pid)
			}

			// Ordinary Update: the exact operation that used to nuke the row.
			// Mutate an unrelated field so the write is a real state change.
			a.Biography = "updated bio for " + orphan
			if err := svc.Update(ctx, a); err != nil {
				t.Fatalf("Update: %v", err)
			}

			// The orphan row must SURVIVE the Update, with fetched_at intact.
			existsAfter, pidAfter, fetchedAfter := queryProviderRow(t, db, a.ID, orphan)
			if !existsAfter {
				t.Fatalf("%s row was DESTROYED by Update (the #2725 bug)", orphan)
			}
			if fetchedAfter != fetchedAt {
				t.Errorf("%s fetched_at changed across Update: before %q after %q", orphan, fetchedAt, fetchedAfter)
			}
			if pidAfter != "" {
				t.Errorf("%s provider_id mutated to %q across Update; expected still empty", orphan, pidAfter)
			}
		})
	}
}

// TestUpsertAll_ModeledClearRemovesRow verifies the destructive clear semantics
// for the struct-modeled providers are PRESERVED by the scoped delete: emptying
// a modeled provider's flat field (mirroring re-identify clear_ids) and calling
// Update must REMOVE its row. It also seeds an orphan row alongside and asserts
// the clear leaves the orphan untouched -- proving the scope cuts exactly at the
// modeled/orphan boundary and nowhere else.
func TestUpsertAll_ModeledClearRemovesRow(t *testing.T) {
	t.Parallel()
	db := setupTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	a := testArtist("Modeled Clear", "/music/modeled-clear")
	if err := svc.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Populate a modeled provider with a value (discogs: id + fetched_at).
	fetched := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	a.DiscogsID = "42"
	a.DiscogsIDFetchedAt = &fetched
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("Update (populate discogs): %v", err)
	}

	// Stamp an orphan (allmusic) directly so it coexists with the modeled row.
	if err := svc.UpdateProviderFetchedAt(ctx, a.ID, string(provider.NameAllMusic)); err != nil {
		t.Fatalf("UpdateProviderFetchedAt(allmusic): %v", err)
	}

	// PRECONDITION: both rows present. Non-vacuous -- if discogs is missing the
	// clear below would pass trivially.
	if exists, pid, _ := queryProviderRow(t, db, a.ID, string(provider.NameDiscogs)); !exists || pid != "42" {
		t.Fatalf("precondition: discogs row exists=%v provider_id=%q, want exists=true provider_id=%q", exists, pid, "42")
	}
	if exists, _, _ := queryProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
		t.Fatalf("precondition: allmusic orphan row missing before clear")
	}

	// Clear the modeled discogs fields exactly as handleReidentify's clear_ids
	// path does (blank the flat field + nil the fetched_at), then Update.
	a.DiscogsID = ""
	a.DiscogsIDFetchedAt = nil
	if err := svc.Update(ctx, a); err != nil {
		t.Fatalf("Update (clear discogs): %v", err)
	}

	// The modeled row must be REMOVED (clear semantics preserved).
	if exists, _, _ := queryProviderRow(t, db, a.ID, string(provider.NameDiscogs)); exists {
		t.Errorf("discogs row still present after clear; scoped delete failed to remove a modeled provider")
	}
	// The orphan row must SURVIVE the clear (scope boundary).
	if exists, _, _ := queryProviderRow(t, db, a.ID, string(provider.NameAllMusic)); !exists {
		t.Errorf("allmusic orphan row destroyed by a modeled-provider clear; delete scope is too wide")
	}
}

// TestModeledProvidersMatchExtractEmitSet is the drift guard. The scoped delete
// in UpsertAll is built from modeledProviders; extractProviderIDs re-inserts the
// providers it can emit. If those two sets ever diverge -- someone adds an Artist
// struct field + extractProviderIDs case without adding the provider to
// modeledProviders, or vice versa -- an Update would either fail to clear the new
// provider's row or (worse) destroy rows it should keep. This test populates
// EVERY modeled field, collects the providers extractProviderIDs actually emits,
// and asserts that set is identical to modeledProviders.
func TestModeledProvidersMatchExtractEmitSet(t *testing.T) {
	t.Parallel()

	// Populate every struct-modeled provider field so extractProviderIDs emits
	// its full set. A field left empty here would silently shrink the emit set
	// and mask a real divergence, so all seven are set to non-empty values.
	fetched := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	a := &Artist{
		MusicBrainzID:       "mbid",
		AudioDBID:           "audiodb",
		AudioDBIDFetchedAt:  &fetched,
		DiscogsID:           "discogs",
		DiscogsIDFetchedAt:  &fetched,
		WikidataID:          "wikidata",
		WikidataIDFetchedAt: &fetched,
		DeezerID:            "deezer",
		SpotifyID:           "spotify",
		LastFMFetchedAt:     &fetched,
	}

	emitted := make([]string, 0, len(extractProviderIDs(a)))
	for _, p := range extractProviderIDs(a) {
		emitted = append(emitted, p.Provider)
	}

	canonical := make([]string, 0, len(modeledProviders))
	for _, p := range modeledProviders {
		canonical = append(canonical, string(p))
	}

	sort.Strings(emitted)
	sort.Strings(canonical)

	if len(emitted) != len(canonical) {
		t.Fatalf("DRIFT: extractProviderIDs emits %d providers %v but modeledProviders has %d %v.\n"+
			"The UpsertAll delete scope (modeledProviders) and the re-insert set (extractProviderIDs) MUST be identical.\n"+
			"Adding a struct field + extractProviderIDs case requires adding the provider to modeledProviders (and vice versa),\n"+
			"or an ordinary Update will either leak or destroy that provider's rows (#2725).",
			len(emitted), emitted, len(canonical), canonical)
	}
	for i := range emitted {
		if emitted[i] != canonical[i] {
			t.Fatalf("DRIFT: emit set %v != modeledProviders %v (first mismatch %q vs %q).\n"+
				"Keep the UpsertAll delete scope and extractProviderIDs in lockstep or an Update will corrupt provider rows (#2725).",
				emitted, canonical, emitted[i], canonical[i])
		}
	}
}
