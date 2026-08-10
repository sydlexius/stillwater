package artist

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// Tests for the MusicBrainz ID re-validation ledger (#2810).
//
// Every test asserts its PRECONDITION before asserting the behavior it is
// about, so a fixture that failed to seed cannot make an assertion pass
// vacuously.

// seedMBIDValidationArtists inserts the artists the ledger tests attach
// verdicts to, and asserts they landed. artist_id is a FK, so a missing artist
// makes every Upsert fail rather than silently no-op -- but the precondition
// check is what turns that into a clear failure message.
func seedMBIDValidationArtists(t *testing.T, db *sql.DB, ids ...string) {
	t.Helper()
	ctx := context.Background()
	for _, id := range ids {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO artists (id, name, sort_name, path, created_at, updated_at)
			 VALUES (?, ?, ?, ?, datetime('now'), datetime('now'))`,
			id, "Artist "+id, "Artist "+id, "/music/"+id,
		); err != nil {
			t.Fatalf("seeding artist %s: %v", id, err)
		}
	}

	var got int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artists`).Scan(&got); err != nil {
		t.Fatalf("counting seeded artists: %v", err)
	}
	if got != len(ids) {
		t.Fatalf("precondition: expected %d seeded artists, found %d", len(ids), got)
	}
}

// newMBIDValidationRepo returns a ledger repo over a fresh migrated DB, plus
// the DB handle for direct precondition/cascade assertions.
func newMBIDValidationRepo(t *testing.T) (MBIDValidationRepository, *sql.DB) {
	t.Helper()
	db := openTestDB(t)
	return newSQLiteMBIDValidationRepo(db), db
}

func floatPtr(f float64) *float64 { return &f }

// countLedgerRows is the direct-SQL row count, used for preconditions and for
// the "upsert replaced rather than appended" assertion. It deliberately does
// not go through the repository, so a bug in Count cannot mask a bug in
// Upsert.
func countLedgerRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM mbid_validation`).Scan(&n); err != nil {
		t.Fatalf("counting mbid_validation rows: %v", err)
	}
	return n
}

// TestMBIDValidation_UpsertRoundTrip pins that every field survives a write
// and read back, including the two that carry the evidence an operator judges
// a verdict by (ResolvedName and CatalogueMatchPercent).
func TestMBIDValidation_UpsertRoundTrip(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedMBIDValidationArtists(t, db, "a-1")
	ctx := context.Background()

	if n := countLedgerRows(t, db); n != 0 {
		t.Fatalf("precondition: ledger must start empty, found %d rows", n)
	}

	checked := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	want := &MBIDValidation{
		ArtistID:              "a-1",
		MBID:                  "11111111-1111-1111-1111-111111111111",
		Outcome:               MBIDOutcomeFailed,
		Reason:                MBIDReasonCatalogueMismatch,
		Detail:                "remote artist has no releases",
		ResolvedName:          "Same Name Different Act",
		CatalogueMatchPercent: floatPtr(0),
		CheckedAt:             checked,
	}
	if err := repo.Upsert(ctx, want); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.GetByArtistID(ctx, "a-1")
	if err != nil {
		t.Fatalf("GetByArtistID: %v", err)
	}
	if got.MBID != want.MBID {
		t.Errorf("MBID = %q, want %q", got.MBID, want.MBID)
	}
	if got.Outcome != MBIDOutcomeFailed {
		t.Errorf("Outcome = %q, want %q", got.Outcome, MBIDOutcomeFailed)
	}
	if got.Reason != MBIDReasonCatalogueMismatch {
		t.Errorf("Reason = %q, want %q", got.Reason, MBIDReasonCatalogueMismatch)
	}
	if got.Detail != want.Detail {
		t.Errorf("Detail = %q, want %q", got.Detail, want.Detail)
	}
	if got.ResolvedName != want.ResolvedName {
		t.Errorf("ResolvedName = %q, want %q", got.ResolvedName, want.ResolvedName)
	}
	// A MEASURED zero percent is the motivating case's evidence and must come
	// back as a present 0, never as nil.
	if got.CatalogueMatchPercent == nil {
		t.Fatal("CatalogueMatchPercent = nil, want a present 0 -- a measured 0% must not read as unmeasured")
	}
	if *got.CatalogueMatchPercent != 0 {
		t.Errorf("CatalogueMatchPercent = %v, want 0", *got.CatalogueMatchPercent)
	}
	if !got.CheckedAt.Equal(checked) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, checked)
	}
}

// TestMBIDValidation_UnmeasuredPercentStaysNil is the other half of the
// nil-vs-zero distinction: a verdict reached without ever running the
// catalogue comparison must read back as unmeasured, not as 0%.
func TestMBIDValidation_UnmeasuredPercentStaysNil(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedMBIDValidationArtists(t, db, "a-1")
	ctx := context.Background()

	if err := repo.Upsert(ctx, &MBIDValidation{
		ArtistID: "a-1",
		MBID:     "mbid-1",
		Outcome:  MBIDOutcomeNotCheckable,
		Reason:   MBIDReasonProviderUnavailable,
		// CatalogueMatchPercent deliberately left nil.
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.GetByArtistID(ctx, "a-1")
	if err != nil {
		t.Fatalf("GetByArtistID: %v", err)
	}
	if got.CatalogueMatchPercent != nil {
		t.Errorf("CatalogueMatchPercent = %v, want nil -- an unmeasured percent must not read as 0%%",
			*got.CatalogueMatchPercent)
	}
}

// TestMBIDValidation_UpsertReplacesPriorOutcome pins the idempotence contract:
// one row per artist, the later verdict wins, and no row accumulates. Asserted
// with a direct-SQL count so a bug in Count cannot hide an appending Upsert.
func TestMBIDValidation_UpsertReplacesPriorOutcome(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedMBIDValidationArtists(t, db, "a-1")
	ctx := context.Background()

	first := &MBIDValidation{
		ArtistID:              "a-1",
		MBID:                  "mbid-old",
		Outcome:               MBIDOutcomeFailed,
		Reason:                MBIDReasonResolvesToDifferentArtist,
		Detail:                "wrong act",
		ResolvedName:          "Wrong Act",
		CatalogueMatchPercent: floatPtr(0),
		CheckedAt:             time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := repo.Upsert(ctx, first); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	// Precondition: the first verdict really is stored, so the assertions
	// below are about REPLACEMENT and not about an insert that never happened.
	if n := countLedgerRows(t, db); n != 1 {
		t.Fatalf("precondition: expected 1 row after first Upsert, got %d", n)
	}
	pre, err := repo.GetByArtistID(ctx, "a-1")
	if err != nil {
		t.Fatalf("precondition GetByArtistID: %v", err)
	}
	if pre.Outcome != MBIDOutcomeFailed {
		t.Fatalf("precondition: stored outcome = %q, want %q", pre.Outcome, MBIDOutcomeFailed)
	}

	second := &MBIDValidation{
		ArtistID:              "a-1",
		MBID:                  "mbid-new",
		Outcome:               MBIDOutcomeValidated,
		Reason:                MBIDReasonNone,
		ResolvedName:          "Right Act",
		CatalogueMatchPercent: floatPtr(96.5),
		CheckedAt:             time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC),
	}
	if err := repo.Upsert(ctx, second); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	if n := countLedgerRows(t, db); n != 1 {
		t.Fatalf("expected the ledger to still hold 1 row after re-check, got %d", n)
	}

	got, err := repo.GetByArtistID(ctx, "a-1")
	if err != nil {
		t.Fatalf("GetByArtistID: %v", err)
	}
	if got.Outcome != MBIDOutcomeValidated || got.Reason != MBIDReasonNone {
		t.Errorf("outcome/reason = %q/%q, want %q/%q",
			got.Outcome, got.Reason, MBIDOutcomeValidated, MBIDReasonNone)
	}
	if got.MBID != "mbid-new" {
		t.Errorf("MBID = %q, want mbid-new", got.MBID)
	}
	// Stale evidence from the superseded verdict must not survive beside the
	// new outcome.
	if got.ResolvedName != "Right Act" {
		t.Errorf("ResolvedName = %q, want Right Act -- superseded evidence leaked through", got.ResolvedName)
	}
	if got.CatalogueMatchPercent == nil || *got.CatalogueMatchPercent != 96.5 {
		t.Errorf("CatalogueMatchPercent = %v, want 96.5", got.CatalogueMatchPercent)
	}
	if got.Detail != "" {
		t.Errorf("Detail = %q, want empty -- the prior verdict's detail must not survive", got.Detail)
	}
}

// TestMBIDValidation_CascadeDeleteWithArtist pins that deleting an artist
// clears its ledger row through the schema's ON DELETE CASCADE, which is the
// evidence behind not adding a hand-written orphan-sweep entry for this table.
func TestMBIDValidation_CascadeDeleteWithArtist(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedMBIDValidationArtists(t, db, "a-1", "a-2")
	ctx := context.Background()

	for _, id := range []string{"a-1", "a-2"} {
		if err := repo.Upsert(ctx, &MBIDValidation{
			ArtistID: id,
			MBID:     "mbid-" + id,
			Outcome:  MBIDOutcomeValidated,
		}); err != nil {
			t.Fatalf("Upsert %s: %v", id, err)
		}
	}
	// Precondition: both rows exist, so a passing assertion below cannot be
	// "the row was never there".
	if n := countLedgerRows(t, db); n != 2 {
		t.Fatalf("precondition: expected 2 ledger rows, got %d", n)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM artists WHERE id = 'a-1'`); err != nil {
		t.Fatalf("deleting artist a-1: %v", err)
	}

	if _, err := repo.GetByArtistID(ctx, "a-1"); !errors.Is(err, ErrMBIDValidationNotFound) {
		t.Errorf("after deleting the artist, GetByArtistID error = %v, want ErrMBIDValidationNotFound", err)
	}
	// The OTHER artist's verdict must be untouched -- a cascade that took the
	// whole table with it would also pass the assertion above.
	if _, err := repo.GetByArtistID(ctx, "a-2"); err != nil {
		t.Errorf("a-2's verdict should survive a-1's deletion, got %v", err)
	}
	if n := countLedgerRows(t, db); n != 1 {
		t.Errorf("expected exactly 1 surviving ledger row, got %d", n)
	}
}

// seedThreeOutcomes writes one verdict of each outcome plus a second failed
// one, so outcome filtering has a subset to select and a remainder to exclude.
// Timestamps descend with the artist number so ordering is deterministic.
func seedThreeOutcomes(t *testing.T, repo MBIDValidationRepository, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	seedMBIDValidationArtists(t, db, "a-1", "a-2", "a-3", "a-4")

	rows := []MBIDValidation{
		{ArtistID: "a-1", MBID: "m1", Outcome: MBIDOutcomeFailed, Reason: MBIDReasonCatalogueMismatch,
			CheckedAt: time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)},
		{ArtistID: "a-2", MBID: "m2", Outcome: MBIDOutcomeFailed, Reason: MBIDReasonNameMismatch,
			CheckedAt: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)},
		{ArtistID: "a-3", MBID: "m3", Outcome: MBIDOutcomeValidated,
			CheckedAt: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)},
		{ArtistID: "a-4", MBID: "m4", Outcome: MBIDOutcomeNotCheckable, Reason: MBIDReasonMBIDNotFound,
			CheckedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
	}
	for i := range rows {
		if err := repo.Upsert(ctx, &rows[i]); err != nil {
			t.Fatalf("seeding verdict for %s: %v", rows[i].ArtistID, err)
		}
	}
	if n := countLedgerRows(t, db); n != len(rows) {
		t.Fatalf("precondition: expected %d ledger rows, got %d", len(rows), n)
	}
}

// TestMBIDValidation_ListFiltersByOutcome pins that an outcome filter returns
// exactly the matching subset -- and, in the unfiltered case, that it returns
// EVERY row. A filter that quietly dropped rows would report a damaged
// population as clean, which is the failure this ledger exists to prevent.
func TestMBIDValidation_ListFiltersByOutcome(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedThreeOutcomes(t, repo, db)
	ctx := context.Background()

	cases := []struct {
		name    string
		outcome MBIDValidationOutcome
		want    []string
	}{
		{"failed only", MBIDOutcomeFailed, []string{"a-1", "a-2"}},
		{"validated only", MBIDOutcomeValidated, []string{"a-3"}},
		{"not checkable only", MBIDOutcomeNotCheckable, []string{"a-4"}},
		{"unfiltered returns every row", "", []string{"a-1", "a-2", "a-3", "a-4"}},
		// An unrecognized outcome must WIDEN to every row, never narrow to
		// none. See MBIDValidationFilter's contract.
		{"unrecognized outcome widens", "bogus", []string{"a-1", "a-2", "a-3", "a-4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.List(ctx, MBIDValidationFilter{Outcome: tc.outcome})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d rows, want %d (%+v)", len(got), len(tc.want), got)
			}
			// Rows come back newest check first, which the fixture's
			// descending timestamps make match the expected order exactly.
			for i, id := range tc.want {
				if got[i].ArtistID != id {
					t.Errorf("row %d artist = %q, want %q", i, got[i].ArtistID, id)
				}
			}

			n, err := repo.Count(ctx, MBIDValidationFilter{Outcome: tc.outcome})
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if n != len(tc.want) {
				t.Errorf("Count = %d, want %d", n, len(tc.want))
			}
		})
	}
}

// TestMBIDValidation_ListPaginationBoundaries walks the page boundaries: each
// page holds the right rows, the last page is short rather than wrapping, an
// offset past the end is empty rather than an error, and Count reports the
// whole set regardless of the page.
func TestMBIDValidation_ListPaginationBoundaries(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedThreeOutcomes(t, repo, db)
	ctx := context.Background()

	all := []string{"a-1", "a-2", "a-3", "a-4"}

	cases := []struct {
		name           string
		limit, offset  int
		want           []string
		wantCountTotal int
	}{
		{"first page", 3, 0, []string{"a-1", "a-2", "a-3"}, 4},
		{"last page is short", 3, 3, []string{"a-4"}, 4},
		{"offset past the end is empty", 3, 99, nil, 4},
		{"a limit covering everything returns everything", 10, 0, all, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.List(ctx, MBIDValidationFilter{Limit: tc.limit, Offset: tc.offset})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d rows, want %d (%+v)", len(got), len(tc.want), got)
			}
			for i, id := range tc.want {
				if got[i].ArtistID != id {
					t.Errorf("row %d artist = %q, want %q", i, got[i].ArtistID, id)
				}
			}

			// Count describes the whole result set, so it must NOT move with
			// the page.
			n, err := repo.Count(ctx, MBIDValidationFilter{Limit: tc.limit, Offset: tc.offset})
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if n != tc.wantCountTotal {
				t.Errorf("Count = %d, want %d -- the count must ignore Limit/Offset", n, tc.wantCountTotal)
			}
		})
	}
}

// TestMBIDValidation_ListPaginationTiebreak pins the artist_id ASC tiebreaker
// in the ORDER BY. seedThreeOutcomes gives every row a distinct checked_at, so
// it cannot detect the tiebreaker's removal -- with a shared checked_at, SQLite
// has no other deterministic order, and a row could appear on two consecutive
// pages or on none without it.
//
// Asserts the exact artist_id order across both pages, not merely that every
// id appears once: two back-to-back List calls against an UNCHANGED table are
// self-consistent regardless of whether the tiebreaker is present (SQLite's
// scan order for a static table does not vary call to call), so a
// no-duplication check alone cannot detect the tiebreaker's removal. Only
// checking the order against the known artist_id ASC sequence can.
func TestMBIDValidation_ListPaginationTiebreak(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	ctx := context.Background()
	seedMBIDValidationArtists(t, db, "a-1", "a-2", "a-3", "a-4")

	// Upserted out of artist_id order so the row's natural (rowid/insertion)
	// scan order does not coincidentally match artist_id ASC.
	same := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"a-3", "a-1", "a-4", "a-2"} {
		rec := MBIDValidation{
			ArtistID: id, MBID: "m-" + id, Outcome: MBIDOutcomeValidated, CheckedAt: same,
		}
		if err := repo.Upsert(ctx, &rec); err != nil {
			t.Fatalf("seeding verdict for %s: %v", id, err)
		}
	}
	if n := countLedgerRows(t, db); n != 4 {
		t.Fatalf("precondition: expected 4 ledger rows, got %d", n)
	}

	want := []string{"a-1", "a-2", "a-3", "a-4"}
	var got []string
	for _, page := range []struct{ limit, offset int }{{2, 0}, {2, 2}} {
		rows, err := repo.List(ctx, MBIDValidationFilter{Limit: page.limit, Offset: page.offset})
		if err != nil {
			t.Fatalf("List(limit=%d, offset=%d): %v", page.limit, page.offset, err)
		}
		for _, row := range rows {
			got = append(got, row.ArtistID)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %d artist ids across both pages, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got artist %q, want %q (full order: %v)", i, got[i], want[i], got)
		}
	}
}

// TestMBIDValidation_ListEmptyLedger pins that an unchecked library yields an
// empty slice and a zero count, not an error and not a phantom row.
func TestMBIDValidation_ListEmptyLedger(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	ctx := context.Background()

	if n := countLedgerRows(t, db); n != 0 {
		t.Fatalf("precondition: ledger must start empty, found %d rows", n)
	}

	got, err := repo.List(ctx, MBIDValidationFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List on an empty ledger returned %+v, want no rows", got)
	}
	n, err := repo.Count(ctx, MBIDValidationFilter{})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Errorf("Count = %d, want 0", n)
	}
}

// TestMBIDValidation_GetByArtistIDNotFound pins that "never checked" surfaces
// as ErrMBIDValidationNotFound rather than a zero-valued verdict, so a caller
// can never mistake an unchecked artist for a checked one.
func TestMBIDValidation_GetByArtistIDNotFound(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedMBIDValidationArtists(t, db, "a-1")

	got, err := repo.GetByArtistID(context.Background(), "a-1")
	if !errors.Is(err, ErrMBIDValidationNotFound) {
		t.Fatalf("error = %v, want ErrMBIDValidationNotFound", err)
	}
	if got != nil {
		t.Errorf("record = %+v, want nil alongside the not-found error", got)
	}
}

// TestMBIDValidation_UpsertRejectsMalformedVerdicts pins the reason/outcome
// pairing at the Go layer. The cases that matter operationally are the two
// that would let an unactionable row into the ledger: a non-validated verdict
// with no reason ("something is wrong, we won't say what") and a validated one
// carrying a reason.
func TestMBIDValidation_UpsertRejectsMalformedVerdicts(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedMBIDValidationArtists(t, db, "a-1")
	ctx := context.Background()

	cases := []struct {
		name    string
		rec     MBIDValidation
		wantErr string
	}{
		{"failed without a reason", MBIDValidation{
			ArtistID: "a-1", MBID: "m-1", Outcome: MBIDOutcomeFailed}, "requires a reason"},
		{"not checkable without a reason", MBIDValidation{
			ArtistID: "a-1", MBID: "m-1", Outcome: MBIDOutcomeNotCheckable}, "requires a reason"},
		{"validated with a reason", MBIDValidation{
			ArtistID: "a-1", MBID: "m-1", Outcome: MBIDOutcomeValidated, Reason: MBIDReasonNameMismatch},
			"carries no reason"},
		{"unknown outcome", MBIDValidation{
			ArtistID: "a-1", MBID: "m-1", Outcome: "probably_fine", Reason: MBIDReasonNameMismatch},
			"unknown outcome"},
		{"unknown reason", MBIDValidation{
			ArtistID: "a-1", MBID: "m-1", Outcome: MBIDOutcomeFailed, Reason: "vibes"}, "unknown reason"},
		{"missing artist id", MBIDValidation{
			MBID: "m-1", Outcome: MBIDOutcomeValidated}, "artist id is required"},
		{"missing mbid", MBIDValidation{
			ArtistID: "a-1", Outcome: MBIDOutcomeValidated}, "mbid is required"},
		{"catalogue match percent out of range", MBIDValidation{
			ArtistID: "a-1", MBID: "m-1", Outcome: MBIDOutcomeValidated,
			CatalogueMatchPercent: floatPtr(150)}, "out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.rec
			err := repo.Upsert(ctx, &rec)
			if err == nil {
				t.Fatal("expected Upsert to reject the record")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error to contain %q, got %q", tc.wantErr, err.Error())
			}
		})
	}

	// Nothing malformed reached the table.
	if n := countLedgerRows(t, db); n != 0 {
		t.Errorf("expected no rows written by the rejected upserts, got %d", n)
	}

	if err := repo.Upsert(ctx, nil); err == nil {
		t.Error("expected Upsert(nil) to return an error")
	}
}

// TestMBIDValidation_SchemaRejectsMalformedVerdicts is the same contract one
// layer down, bypassing Validate entirely with raw SQL. It is what makes the
// Go-level check an ergonomic convenience rather than the only guard: a future
// writer that skips Validate still cannot store an unactionable verdict.
func TestMBIDValidation_SchemaRejectsMalformedVerdicts(t *testing.T) {
	t.Parallel()
	_, db := newMBIDValidationRepo(t)
	seedMBIDValidationArtists(t, db, "a-1")
	ctx := context.Background()

	cases := []struct {
		name            string
		outcome, reason string
	}{
		{"failed with a blank reason", "failed", ""},
		{"not_checkable with a blank reason", "not_checkable", ""},
		{"validated carrying a reason", "validated", "name_mismatch"},
		{"outcome outside the vocabulary", "probably_fine", "name_mismatch"},
		{"reason outside the vocabulary", "failed", "vibes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx,
				`INSERT INTO mbid_validation (artist_id, mbid, outcome, reason)
				 VALUES ('a-1', 'm', ?, ?)`, tc.outcome, tc.reason)
			if err == nil {
				t.Fatal("expected the schema CHECK constraint to reject this row")
			}
		})
	}
}

// TestMBIDValidation_QueryErrorsPropagate covers the error paths: a closed DB
// makes each query fail, and the wrapped error must reach the caller instead
// of being reported as an empty result.
func TestMBIDValidation_QueryErrorsPropagate(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedMBIDValidationArtists(t, db, "a-1")
	ctx := context.Background()

	// Precondition: the repo works before the DB is closed, so the failures
	// below are attributable to the close and not to a broken fixture.
	if _, err := repo.List(ctx, MBIDValidationFilter{}); err != nil {
		t.Fatalf("precondition: List on an open DB should succeed, got %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("closing db: %v", err)
	}

	if err := repo.Upsert(ctx, &MBIDValidation{
		ArtistID: "a-1", MBID: "m", Outcome: MBIDOutcomeValidated,
	}); err == nil {
		t.Error("expected Upsert to fail on a closed DB")
	}
	if _, err := repo.List(ctx, MBIDValidationFilter{}); err == nil {
		t.Error("expected List to fail on a closed DB")
	}
	if _, err := repo.Count(ctx, MBIDValidationFilter{}); err == nil {
		t.Error("expected Count to fail on a closed DB")
	}
	// A closed DB must NOT be reported as "this artist was never checked" --
	// that would turn an infrastructure failure into a clean-looking absence.
	_, err := repo.GetByArtistID(ctx, "a-1")
	if err == nil {
		t.Error("expected GetByArtistID to fail on a closed DB")
	} else if errors.Is(err, ErrMBIDValidationNotFound) {
		t.Error("a DB failure was reported as ErrMBIDValidationNotFound")
	}
}

// TestMBIDValidationFilter_Validate pins the normalization directly, including
// the direction that matters: an unrecognized outcome widens to every row.
func TestMBIDValidationFilter_Validate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   MBIDValidationFilter
		want MBIDValidationFilter
	}{
		{"zero value gets the default page", MBIDValidationFilter{},
			MBIDValidationFilter{Limit: mbidValidationDefaultLimit}},
		{"unrecognized outcome is blanked, not rejected",
			MBIDValidationFilter{Outcome: "made up", Limit: 10},
			MBIDValidationFilter{Outcome: "", Limit: 10}},
		{"a known outcome survives",
			MBIDValidationFilter{Outcome: MBIDOutcomeFailed, Limit: 10},
			MBIDValidationFilter{Outcome: MBIDOutcomeFailed, Limit: 10}},
		{"an over-large limit is capped",
			MBIDValidationFilter{Limit: 100000},
			MBIDValidationFilter{Limit: mbidValidationMaxLimit}},
		{"a negative offset is floored",
			MBIDValidationFilter{Limit: 5, Offset: -7},
			MBIDValidationFilter{Limit: 5, Offset: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in
			got.Validate()
			if got != tc.want {
				t.Errorf("Validate() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestMBIDValidation_UnparsableCheckedAtIsZero pins that a checked_at nobody
// can parse reads back as the zero time rather than as "now". Synthesizing a
// timestamp would present a fabricated check time as fact.
func TestMBIDValidation_UnparsableCheckedAtIsZero(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedMBIDValidationArtists(t, db, "a-1")
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO mbid_validation (artist_id, mbid, outcome, reason, checked_at)
		 VALUES ('a-1', 'm', 'validated', '', 'not a timestamp')`); err != nil {
		t.Fatalf("seeding an unparsable checked_at: %v", err)
	}

	got, err := repo.GetByArtistID(ctx, "a-1")
	if err != nil {
		t.Fatalf("GetByArtistID: %v", err)
	}
	if !got.CheckedAt.IsZero() {
		t.Errorf("CheckedAt = %v, want the zero time for an unparsable value", got.CheckedAt)
	}
}

// TestMBIDValidation_SQLiteDatetimeFormatParses covers the schema DEFAULT and
// legacy SQLite "YYYY-MM-DD HH:MM:SS" shape, which Upsert never writes but a
// hand-inserted or default-stamped row can carry.
func TestMBIDValidation_SQLiteDatetimeFormatParses(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedMBIDValidationArtists(t, db, "a-1")
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO mbid_validation (artist_id, mbid, outcome, reason, checked_at)
		 VALUES ('a-1', 'm', 'validated', '', '2026-08-10 12:00:00')`); err != nil {
		t.Fatalf("seeding a SQLite-datetime checked_at: %v", err)
	}

	got, err := repo.GetByArtistID(ctx, "a-1")
	if err != nil {
		t.Fatalf("GetByArtistID: %v", err)
	}
	want := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	if !got.CheckedAt.Equal(want) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, want)
	}
}

// TestMBIDValidation_ZeroCheckedAtIsStamped pins that a caller who leaves
// CheckedAt unset gets the current time rather than year zero -- the ledger's
// whole point is knowing WHEN a verdict was reached.
func TestMBIDValidation_ZeroCheckedAtIsStamped(t *testing.T) {
	t.Parallel()
	repo, db := newMBIDValidationRepo(t)
	seedMBIDValidationArtists(t, db, "a-1")
	ctx := context.Background()

	before := time.Now().Add(-time.Minute)
	if err := repo.Upsert(ctx, &MBIDValidation{
		ArtistID: "a-1", MBID: "m", Outcome: MBIDOutcomeValidated,
		// CheckedAt deliberately left zero.
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.GetByArtistID(ctx, "a-1")
	if err != nil {
		t.Fatalf("GetByArtistID: %v", err)
	}
	if got.CheckedAt.IsZero() {
		t.Fatal("CheckedAt is zero; a zero CheckedAt must be stamped at write time")
	}
	if got.CheckedAt.Before(before) {
		t.Errorf("CheckedAt = %v, want a time at or after %v", got.CheckedAt, before)
	}
}

// TestServiceMBIDValidations pins that a Service built by NewService exposes a
// working ledger, and that one built without the repository reports nil rather
// than panicking.
func TestServiceMBIDValidations(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	seedMBIDValidationArtists(t, db, "a-1")
	svc := NewService(db)
	ctx := context.Background()

	repo := svc.MBIDValidations()
	if repo == nil {
		t.Fatal("NewService must wire the mbid validation ledger")
	}
	if err := repo.Upsert(ctx, &MBIDValidation{
		ArtistID: "a-1", MBID: "m", Outcome: MBIDOutcomeValidated,
	}); err != nil {
		t.Fatalf("Upsert through the service repo: %v", err)
	}
	if _, err := repo.GetByArtistID(ctx, "a-1"); err != nil {
		t.Fatalf("GetByArtistID through the service repo: %v", err)
	}

	bare := &Service{}
	if bare.MBIDValidations() != nil {
		t.Error("a Service with no ledger configured must report nil")
	}
	bare.SetMBIDValidationRepository(repo)
	if bare.MBIDValidations() == nil {
		t.Error("SetMBIDValidationRepository did not attach the repository")
	}
}
