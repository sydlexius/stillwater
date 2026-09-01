package provider_test

// The live dead-slot guard for #2897, scoped to the nine provider/field pairs
// migration 030 strips.
//
// Why this test runs against GetPriorities and not DefaultPriorities: the
// latter is a FALLBACK, not the routing table. Migration 001 seeds
// provider.priority.* rows on first run, and GetPriorities reads the stored
// row and then APPENDS every default the row is missing. Two consequences,
// and the fix needs both halves or it does nothing:
//
//   - Correcting DefaultPriorities() alone does not reach an install that has
//     already run 001. The stored row keeps the dead entry.
//   - Scrubbing the stored row alone does not work either: on the next call,
//     GetPriorities re-appends the entry from the uncorrected defaults. The
//     migration would be undone in memory on every read.
//
// So a guard asserting over DefaultPriorities() is blind to the first, and a
// migration without the DefaultPriorities correction is silently reversed by
// the second. This test is the one that sees both, because it asserts over
// what GetPriorities actually returns after the real migrations have run.
//
// Scope note: this checks the nine pairs migration 030 owns. years_active/
// audiodb is deliberately excluded: audiodb.mapArtist never assigns
// YearsActive literally, so it is dead on both full-refresh paths, but the
// per-field comparison path (extractFieldForComparison in orchestrator.go)
// synthesizes a candidate from AudioDB's Born/Died, so it is a live,
// user-facing answer there. See migration 030's header for the mechanism.
//
// The general invariant -- that NO priority entry anywhere names a provider
// whose adapter cannot supply the field on ANY path -- needs the
// capability-table reconciliation to assert against, and lands with it.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
)

// strippedPairs is the set migration 030 removes: each provider was listed for
// a field its adapter never populates on either full-refresh path, so the
// entry could only ever appear as a structurally-empty routing option in the
// settings UI and the per-field comparison panel. It does not save a
// request -- see migration 030's header for why.
var strippedPairs = []struct {
	field    string
	provider provider.ProviderName
	why      string
}{
	{"genres", provider.NameDiscogs, "the Discogs adapter aggregates Styles from master releases and never assigns Genres"},
	{"members", provider.NameWikidata, "wikidata.mapArtist assigns only Formed, Disbanded, Origin and Genres"},
	{"born", provider.NameWikidata, "wikidata.mapArtist assigns only Formed, Disbanded, Origin and Genres"},
	{"died", provider.NameWikidata, "wikidata.mapArtist assigns only Formed, Disbanded, Origin and Genres"},
	{"gender", provider.NameWikidata, "wikidata.mapArtist assigns only Formed, Disbanded, Origin and Genres"},
	{"type", provider.NameWikidata, "wikidata.mapArtist assigns only Formed, Disbanded, Origin and Genres"},
	{"type", provider.NameDiscogs, "the Discogs adapter never assigns Type"},
	{"disbanded", provider.NameWikipedia, "the Wikipedia infobox parser assigns Born and Died, never Disbanded"},
	{"biography", provider.NameMusicBrainz, "MusicBrainz returns no biography text; migration 001 seeded it first and migration 007 removed only wikidata"},
}

// TestGetPrioritiesOnMigratedDBHasNoStrippedPairs asserts that after the full
// migration set runs, none of the stripped pairs is still routed -- neither
// left in the stored row nor re-appended from the defaults.
func TestGetPrioritiesOnMigratedDBHasNoStrippedPairs(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "migrated.db")

	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	defer db.Close()
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	// Precondition: the rows must have come from the seeded settings table. On
	// an empty table GetPriorities returns DefaultPriorities() wholesale, and
	// this test would silently become blind to the stored rows, which is the
	// exact failure it exists to catch.
	var seeded int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM settings WHERE key LIKE 'provider.priority.%'").Scan(&seeded); err != nil {
		t.Fatalf("counting seeded priority rows: %v", err)
	}
	if seeded == 0 {
		t.Fatal("no provider.priority.* rows in the migrated database -- GetPriorities would be reading defaults, so this test could not see a stored dead slot")
	}

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	svc := provider.NewSettingsService(db, enc)

	priorities, err := svc.GetPriorities(ctx)
	if err != nil {
		t.Fatalf("GetPriorities: %v", err)
	}
	if len(priorities) == 0 {
		t.Fatal("GetPriorities returned no rows -- every assertion below would pass vacuously")
	}

	byField := make(map[string][]provider.ProviderName, len(priorities))
	for _, fp := range priorities {
		byField[fp.Field] = fp.Providers
	}

	var checked int
	for _, pair := range strippedPairs {
		chain, ok := byField[pair.field]
		if !ok {
			t.Errorf("GetPriorities returned no row for %q, so migration 030's strip of %q cannot be verified", pair.field, pair.provider)
			continue
		}
		if len(chain) == 0 {
			t.Errorf("the %q chain is empty; stripping a dead slot must never leave a field with no provider", pair.field)
			continue
		}
		checked++
		for _, name := range chain {
			if name == pair.provider {
				t.Errorf("LIVE DEAD SLOT: %q still routes to %q (chain: %v). %s. It offers a structurally-empty option in the settings UI and per-field comparison panel. Both halves are needed: migration 030 scrubs the stored row, and the DefaultPriorities() correction stops GetPriorities re-appending it.",
					pair.field, name, chain, pair.why)
			}
		}
	}
	if checked != len(strippedPairs) {
		t.Fatalf("checked %d of %d stripped pairs -- the rest passed vacuously", checked, len(strippedPairs))
	}
}
