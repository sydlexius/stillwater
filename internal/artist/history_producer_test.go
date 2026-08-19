package artist

import (
	"context"
	"testing"
	"time"
)

// history_producer_test.go -- PR 1 tests for issue #3078 (T3, T5, plus the
// round-trip and vocabulary table tests). T4 (migration-does-not-backfill)
// lives in internal/database/migrate_029_no_backfill_test.go, where the
// migrateUpTo harness already exists. T1/T1b/T2 (the actual write-path
// stamping) belong to PR 2 and are not present here -- this PR ships a
// column that is always "".

// --- T3: a pre-029 row still reads as unrecorded (guard test) ---------

// TestPre029Row_ReadsAsUnrecordedProducer is the guard test for #3078's
// central constraint: an existing metadata_changes row must NOT be
// reinterpreted by this change. It inserts a row with RAW SQL naming ONLY
// the columns a v1.6.2 build (pre-migration-029) could have written --
// literally what production data looks like -- and asserts every read path
// (GetByID, List, ListBlastRadius) reports Producer == ProducerUnrecorded,
// and that the blast-radius classifier calls it UNATTRIBUTED rather than
// operator-authored.
//
// The raw insert (naming only id, artist_id, field, old_value, new_value,
// source, created_at) is load-bearing: writing the row through
// HistoryService.Record would only prove the WRITER defaults producer
// correctly, never that a genuinely pre-existing row -- one that never had a
// producer value written at all -- survives unreinterpreted. This test wires
// the actual misbehavior a bad migration would cause: fails immediately if
// migration 029 is given a non-empty DEFAULT (see the mutation proof in the
// #3078 PR 1 report, reproduced by temporarily editing the migration and
// re-running this test).
func TestPre029Row_ReadsAsUnrecordedProducer(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	ctx := context.Background()

	artistID := "artist-pre029"
	seedTestArtist(t, db, artistID)

	// A "manual" write that replaced a real value with a shorter one -- the
	// exact shape of the production damage #3078 exists to make attributable
	// going forward. This is precisely the row type that must NOT be
	// reinterpreted as operator-authored just because producer now exists as
	// a column.
	changeID := "mc-pre029-001"
	const insertPre029 = `
		INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	createdAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if _, err := db.ExecContext(ctx, insertPre029,
		changeID, artistID, "biography", "a long curated biography", "short", "manual", createdAt,
	); err != nil {
		t.Fatalf("raw pre-029 insert: %v", err)
	}

	svc := NewHistoryService(db)

	// GetByID
	c, err := svc.GetByID(ctx, changeID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if c.Producer != ProducerUnrecorded {
		t.Errorf("GetByID: Producer = %q, want %q (ProducerUnrecorded)", c.Producer, ProducerUnrecorded)
	}

	// List
	changes, total, err := svc.List(ctx, artistID, 10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 1 || len(changes) != 1 {
		t.Fatalf("List: total=%d len=%d, want 1/1", total, len(changes))
	}
	if changes[0].Producer != ProducerUnrecorded {
		t.Errorf("List: Producer = %q, want %q", changes[0].Producer, ProducerUnrecorded)
	}

	// ListBlastRadius: the row is old_value != '' and old_value != new_value
	// and source != 'revert', so it reads as damage. Assert its attribution
	// is UNKNOWN -- never AUTOMATED, and critically, producer's mere
	// existence must not make it look like ProducerOperator either. There is
	// no "attribution by producer" surface in this PR (attribution is still
	// derived purely from source, per T5 below), so this asserts the
	// existing, unmoved behavior: source="manual" classifies unknown.
	repo := svc.Repo()
	rows, err := repo.ListBlastRadius(ctx, BlastRadiusFilter{})
	if err != nil {
		t.Fatalf("ListBlastRadius: %v", err)
	}
	var found *BlastRadiusRow
	for i := range rows {
		if rows[i].ID == changeID {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatalf("ListBlastRadius did not return the pre-029 damage row")
	}
	if found.Producer != ProducerUnrecorded {
		t.Errorf("ListBlastRadius: Producer = %q, want %q", found.Producer, ProducerUnrecorded)
	}
	if found.Attribution != BlastAttributionUnknown {
		t.Errorf("ListBlastRadius: Attribution = %q, want %q (unattributed, NOT operator-authored)",
			found.Attribution, BlastAttributionUnknown)
	}
}

// --- T5: the damage predicate is byte-identical -------------------------

// TestBlastRadiusDamageWhere_ProducerNeverAppears is a golden-string tripwire
// (#3078 T5): blastRadiusDamageWhere's output must be IDENTICAL to what it
// was before producer existed, for every (class, attribution) combination.
// This is not a behavior test -- it fails loudly if a future edit puts
// "producer" into the predicate, which is the exact silent alteration #3078
// forbids (the whole value of a second column is that source's predicates
// stay untouched).
func TestBlastRadiusDamageWhere_ProducerNeverAppears(t *testing.T) {
	t.Parallel()
	classes := []string{BlastScopeAll, BlastClassBlanked, BlastClassReplaced}
	attributions := []string{BlastScopeAll, BlastAttributionAutomated, BlastAttributionUnknown}

	golden := map[string]string{
		"all|all":            `WHERE rn = 1 AND old_value != '' AND old_value != new_value AND source != 'revert'`,
		"all|automated":      `WHERE rn = 1 AND old_value != '' AND old_value != new_value AND source != 'revert' AND (source IN ('scan', 'import') OR source LIKE 'provider:%' OR source LIKE 'rule:%')`,
		"all|unknown":        `WHERE rn = 1 AND old_value != '' AND old_value != new_value AND source != 'revert' AND NOT (source IN ('scan', 'import') OR source LIKE 'provider:%' OR source LIKE 'rule:%')`,
		"blanked|all":        `WHERE rn = 1 AND old_value != '' AND old_value != new_value AND source != 'revert' AND new_value = ''`,
		"blanked|automated":  `WHERE rn = 1 AND old_value != '' AND old_value != new_value AND source != 'revert' AND new_value = '' AND (source IN ('scan', 'import') OR source LIKE 'provider:%' OR source LIKE 'rule:%')`,
		"blanked|unknown":    `WHERE rn = 1 AND old_value != '' AND old_value != new_value AND source != 'revert' AND new_value = '' AND NOT (source IN ('scan', 'import') OR source LIKE 'provider:%' OR source LIKE 'rule:%')`,
		"replaced|all":       `WHERE rn = 1 AND old_value != '' AND old_value != new_value AND source != 'revert' AND new_value != ''`,
		"replaced|automated": `WHERE rn = 1 AND old_value != '' AND old_value != new_value AND source != 'revert' AND new_value != '' AND (source IN ('scan', 'import') OR source LIKE 'provider:%' OR source LIKE 'rule:%')`,
		"replaced|unknown":   `WHERE rn = 1 AND old_value != '' AND old_value != new_value AND source != 'revert' AND new_value != '' AND NOT (source IN ('scan', 'import') OR source LIKE 'provider:%' OR source LIKE 'rule:%')`,
	}

	for _, class := range classes {
		for _, attribution := range attributions {
			key := class + "|" + attribution
			want, ok := golden[key]
			if !ok {
				t.Fatalf("no golden entry for %q -- test fixture is incomplete", key)
			}
			got := blastRadiusDamageWhere(class, attribution)
			if got != want {
				t.Errorf("blastRadiusDamageWhere(%q, %q) =\n  %s\nwant\n  %s", class, attribution, got, want)
			}
			if containsProducer(got) {
				t.Errorf("blastRadiusDamageWhere(%q, %q) mentions 'producer': %s", class, attribution, got)
			}
		}
	}
}

// TestBlastRadiusAutomatedSQL_ProducerNeverAppears is T5's companion for the
// other predicate builder that feeds both the SQL and (indirectly, via the
// same shared slices) the Go classifier.
func TestBlastRadiusAutomatedSQL_ProducerNeverAppears(t *testing.T) {
	t.Parallel()
	for _, col := range []string{"source", "mc.source"} {
		got := blastRadiusAutomatedSQL(col)
		want := "(" + col + " IN ('scan', 'import') OR " + col + " LIKE 'provider:%' OR " + col + " LIKE 'rule:%')"
		if got != want {
			t.Errorf("blastRadiusAutomatedSQL(%q) =\n  %s\nwant\n  %s", col, got, want)
		}
		if containsProducer(got) {
			t.Errorf("blastRadiusAutomatedSQL(%q) mentions 'producer': %s", col, got)
		}
	}
}

func containsProducer(s string) bool {
	for i := 0; i+len("producer") <= len(s); i++ {
		if s[i:i+len("producer")] == "producer" {
			return true
		}
	}
	return false
}

// --- Round-trip: a stamped producer survives Record -> GetByID ----------

// TestRecord_RoundTripsProducerFromContext exercises the context plumbing
// this PR adds (ContextWithProducer, ContextWithFieldProducers,
// producerForField) end to end, even though no production call site uses it
// yet. Proves the mechanism PR 2 will build on actually works.
func TestRecord_RoundTripsProducerFromContext(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	artistID := "artist-roundtrip"
	seedTestArtist(t, db, artistID)

	t.Run("scalar producer", func(t *testing.T) {
		ctx := ContextWithProducer(context.Background(), ProducerOperator)
		if err := svc.Record(ctx, artistID, "biography", "old", "new", "manual"); err != nil {
			t.Fatalf("Record: %v", err)
		}
		changes, _, err := svc.List(context.Background(), artistID, 10, 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(changes) == 0 || changes[0].Producer != ProducerOperator {
			t.Fatalf("Producer = %q, want %q", changes[0].Producer, ProducerOperator)
		}
	})

	t.Run("field-producer overlay takes precedence over scalar", func(t *testing.T) {
		artistID := "artist-roundtrip-2"
		seedTestArtist(t, db, artistID)
		ctx := ContextWithProducer(context.Background(), ProducerOperator)
		ctx = ContextWithFieldProducers(ctx, map[string]string{"genres": "provider:lastfm"})

		if err := svc.Record(ctx, artistID, "genres", "old", "new", "manual"); err != nil {
			t.Fatalf("Record(genres): %v", err)
		}
		if err := svc.Record(ctx, artistID, "biography", "old", "new", "manual"); err != nil {
			t.Fatalf("Record(biography): %v", err)
		}

		changes, _, err := svc.List(context.Background(), artistID, 10, 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var genresProducer, bioProducer string
		for _, c := range changes {
			switch c.Field {
			case "genres":
				genresProducer = c.Producer
			case "biography":
				bioProducer = c.Producer
			}
		}
		if genresProducer != "provider:lastfm" {
			t.Errorf("genres Producer = %q, want %q (overlay)", genresProducer, "provider:lastfm")
		}
		if bioProducer != ProducerOperator {
			t.Errorf("biography Producer = %q, want %q (scalar fallback)", bioProducer, ProducerOperator)
		}
	})

	t.Run("no context value defaults to unrecorded", func(t *testing.T) {
		artistID := "artist-roundtrip-3"
		seedTestArtist(t, db, artistID)
		if err := svc.Record(context.Background(), artistID, "biography", "old", "new", "manual"); err != nil {
			t.Fatalf("Record: %v", err)
		}
		changes, _, err := svc.List(context.Background(), artistID, 10, 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if changes[0].Producer != ProducerUnrecorded {
			t.Errorf("Producer = %q, want %q", changes[0].Producer, ProducerUnrecorded)
		}
	})
}

// --- validHistoryProducer table test -------------------------------------

func TestValidHistoryProducer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		producer string
		want     bool
	}{
		{"", true},
		{ProducerOperator, true},
		{ProducerRestore, true},
		{ProducerNFO, true},
		{ProducerFilesystem, true},
		{"provider:musicbrainz", true},
		{"provider:", true},
		{"platform:emby", true},
		{"platform:jellyfin", true},
		{"platform:", true},
		{"rule:bio_exists", true},
		{"rule:", true},
		{"garbage", false},
		{"Operator", false}, // case-sensitive
		{"provider", false}, // missing colon
		{" operator", false},
		{"manual", false}, // a source token, not a producer token
	}
	for _, tc := range cases {
		got := validHistoryProducer(tc.producer)
		if got != tc.want {
			t.Errorf("validHistoryProducer(%q) = %v, want %v", tc.producer, got, tc.want)
		}
	}
}

// --- recordHistoryTx writes producer too ---------------------------------

// TestRecordHistoryTx_WritesProducer exercises lock_restore.go's transactional
// insert directly (the path RestoreLockedFieldGuarded uses), confirming it
// carries producer through the same producerForField resolution as Record,
// without touching the no-op-skip behavior documented on that function.
func TestRecordHistoryTx_WritesProducer(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)
	artistID := "artist-tx"
	seedTestArtist(t, db, artistID)

	ctx := ContextWithProducer(context.Background(), ProducerRestore)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := recordHistoryTx(ctx, tx, artistID, "biography", "damaged", "restored", "revert"); err != nil {
		t.Fatalf("recordHistoryTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var producer string
	if err := db.QueryRowContext(context.Background(),
		`SELECT producer FROM metadata_changes WHERE artist_id = ? AND field = 'biography'`, artistID,
	).Scan(&producer); err != nil {
		t.Fatalf("querying producer: %v", err)
	}
	if producer != ProducerRestore {
		t.Errorf("producer = %q, want %q", producer, ProducerRestore)
	}
}

// --- F1 fix round: producer round-trips through EVERY read path ---------

// TestProducer_RoundTripsEveryReadPath is the F1 fix-round test (#3078 PR 1
// hostile review). TestRecord_RoundTripsProducerFromContext above only
// exercises List; GetByID, both branches of ListGlobal, and ListBlastRadius
// were unpinned -- dropping producer from any of their SELECT/scan pairs
// left the suite green, because the only other producer-aware test
// (TestPre029Row_ReadsAsUnrecordedProducer) asserts Producer == "", which is
// indistinguishable from a column that was never scanned at all.
//
// Each row gets a DISTINCT, non-empty producer that also differs from every
// other column on that row, so a dropped column or a scan-order swap shows
// up as a wrong string rather than a silent "".
func TestProducer_RoundTripsEveryReadPath(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	ctx := context.Background()
	artistID := "artist-producer-roundtrip"
	seedTestArtist(t, db, artistID)

	writes := []struct{ field, oldV, newV, source, producer string }{
		{"biography", "OLDBIO", "NEWBIO", "manual", ProducerOperator},
		{"genres", "OLDGEN", "NEWGEN", "scan", "provider:musicbrainz"},
		{"origin", "OLDORI", "NEWORI", "rule:origin_missing", "rule:mbid_validation"},
	}
	ids := make(map[string]string, len(writes))
	want := make(map[string]string, len(writes))
	for _, w := range writes {
		c := &MetadataChange{
			ID:       "mc-producer-rt-" + w.field,
			ArtistID: artistID,
			Field:    w.field,
			OldValue: w.oldV,
			NewValue: w.newV,
			Source:   w.source,
			Producer: w.producer,
		}
		if err := svc.Repo().Record(ctx, c); err != nil {
			t.Fatalf("Record(%s): %v", w.field, err)
		}
		ids[w.field] = c.ID
		want[w.field] = w.producer
	}

	// check asserts the producer matches, AND that it does not collide with
	// old_value/new_value/source on the same row -- guarding against a
	// scan-order swap that would otherwise still pass an equality check.
	check := func(t *testing.T, path, field, gotProducer, gotOld, gotNew, gotSource string) {
		t.Helper()
		if gotProducer != want[field] {
			t.Errorf("%s[%s]: Producer = %q, want %q", path, field, gotProducer, want[field])
		}
		if gotProducer == gotOld || gotProducer == gotNew || gotProducer == gotSource {
			t.Errorf("%s[%s]: producer %q collides with old=%q new=%q source=%q (possible scan-order swap)",
				path, field, gotProducer, gotOld, gotNew, gotSource)
		}
	}

	// 1. GetByID
	for field, id := range ids {
		c, err := svc.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("GetByID(%s): %v", field, err)
		}
		check(t, "GetByID", field, c.Producer, c.OldValue, c.NewValue, c.Source)
	}

	repo := svc.Repo()

	// 2. ListGlobal -- plain branch (PerFieldLimit == 0)
	g, _, err := repo.ListGlobal(ctx, GlobalHistoryFilter{ArtistID: artistID, Limit: 50})
	if err != nil {
		t.Fatalf("ListGlobal: %v", err)
	}
	if len(g) != len(writes) {
		t.Fatalf("ListGlobal len = %d, want %d", len(g), len(writes))
	}
	for _, c := range g {
		check(t, "ListGlobal", c.Field, c.Producer, c.OldValue, c.NewValue, c.Source)
	}

	// 3. ListGlobal -- per-field-capped branch (PerFieldLimit > 0); a
	// separate SELECT and scan from the plain branch above.
	gp, _, err := repo.ListGlobal(ctx, GlobalHistoryFilter{ArtistID: artistID, PerFieldLimit: 5})
	if err != nil {
		t.Fatalf("ListGlobal(PerFieldLimit): %v", err)
	}
	if len(gp) != len(writes) {
		t.Fatalf("ListGlobal(PerFieldLimit) len = %d, want %d", len(gp), len(writes))
	}
	for _, c := range gp {
		check(t, "ListGlobal(PerFieldLimit)", c.Field, c.Producer, c.OldValue, c.NewValue, c.Source)
	}

	// 4. ListBlastRadius -- every row here is old_value != '' and
	// old_value != new_value and source != 'revert', so all three qualify
	// as damage and are returned.
	br, err := repo.ListBlastRadius(ctx, BlastRadiusFilter{ArtistID: artistID})
	if err != nil {
		t.Fatalf("ListBlastRadius: %v", err)
	}
	if len(br) != len(writes) {
		t.Fatalf("ListBlastRadius len = %d, want %d", len(br), len(writes))
	}
	for _, row := range br {
		check(t, "ListBlastRadius", row.Field, row.Producer, row.OldValue, row.NewValue, row.Source)
	}
}
