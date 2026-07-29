package artist

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// The two audit-message wordings that exist in metadata_changes for this rule.
// Both are seeded in the fixture: the short form is the one the entire
// already-affected population carries, and a report that only handled the newer
// form would miss exactly the artists it exists to find.
const (
	nfoMBIDOldFormMessage = "set MBID to 5b11f4ce-a62d-471e-81fc-a69a8278c7da for Old Form Artist"
	nfoMBIDNewFormMessage = `set MusicBrainz ID 83d91898-7763-47d7-b03b-b92132375c47 for New Form Artist ` +
		`(matched "New Form Artist" via name-search, confidence 91, runner-up scored 62)`
)

// seedNFOMBIDChange inserts one artists row plus one metadata_changes row
// directly, bypassing HistoryService.Record so a test controls the exact source,
// message, and timestamp. ts is formatted RFC3339 to match what Record writes
// and what migration 004 normalized legacy rows to, so the text ordering the
// query relies on holds.
func seedNFOMBIDChange(t *testing.T, db *sql.DB, id, artistID, artistName, message, source string, ts time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO artists (id, name, sort_name, path) VALUES (?, ?, ?, '')`,
		artistID, artistName, artistName,
	); err != nil {
		t.Fatalf("seeding artist %q: %v", artistID, err)
	}
	// old_value is '' on every row on purpose: the rule-fix history path always
	// records an empty previous value, and this rule only ever ran on an artist
	// with no MusicBrainz ID. A fixture with a non-empty old_value would not
	// represent anything that can exist in a real database.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO metadata_changes (id, artist_id, field, old_value, new_value, source, created_at)
		 VALUES (?, ?, 'rule_fix', '', ?, ?, ?)`,
		id, artistID, message, source, ts.UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seeding change %q: %v", id, err)
	}
}

// seedNFOMBIDProviderID records a current MusicBrainz ID for an artist, which is
// what the report's LEFT JOIN reads.
func seedNFOMBIDProviderID(t *testing.T, db *sql.DB, artistID, mbid string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO artist_provider_ids (artist_id, provider, provider_id)
		 VALUES (?, 'musicbrainz', ?)`,
		artistID, mbid,
	); err != nil {
		t.Fatalf("seeding provider id for %q: %v", artistID, err)
	}
}

var nfoMBIDFixtureBase = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// seedNFOMBIDFixture builds the canonical fixture.
//
// Every axis varies INDEPENDENTLY of every other, which is what makes the
// ordering and selection assertions mean something. In particular the artist
// names are deliberately in the OPPOSITE alphabetical order from the timestamp
// order, so a query that sorted by the wrong column would produce a visibly
// different sequence rather than the same one by coincidence.
//
//	change id     artist name        created_at   current MBID
//	n-old         "Old Form Artist"  +1           present, DIFFERENT from the written one
//	n-new         "New Form Artist"  +2           present, matches the written one
//	n-cleared     "Cleared Artist"   +3           ABSENT (no provider row at all)
//	n-second      "Cleared Artist"   +4           ABSENT (same artist, second write)
//	x-unrelated   "Bystander"        +5           present  (source is NOT this rule)
func seedNFOMBIDFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	at := func(min int) time.Time { return nfoMBIDFixtureBase.Add(time.Duration(min) * time.Minute) }

	// Reported. Old message wording. Its live ID differs from the one the message
	// records, which is the case that proves the report reads the live value
	// rather than trusting the audit text.
	seedNFOMBIDChange(t, db, "n-old", "art-old", "Old Form Artist",
		nfoMBIDOldFormMessage, NFOMBIDReportSource, at(1))
	seedNFOMBIDProviderID(t, db, "art-old", "ffffffff-0000-0000-0000-000000000000")

	// Reported. New message wording, live ID matches what it wrote.
	seedNFOMBIDChange(t, db, "n-new", "art-new", "New Form Artist",
		nfoMBIDNewFormMessage, NFOMBIDReportSource, at(2))
	seedNFOMBIDProviderID(t, db, "art-new", "83d91898-7763-47d7-b03b-b92132375c47")

	// Reported TWICE for one artist, with NO provider row. This artist's ID has
	// since been cleared, which is precisely the artist somebody already noticed
	// was wrong: an inner join would drop it. Two writes also make the
	// writes-vs-artists count distinction observable.
	seedNFOMBIDChange(t, db, "n-cleared", "art-cleared", "Cleared Artist",
		nfoMBIDOldFormMessage, NFOMBIDReportSource, at(3))
	seedNFOMBIDChange(t, db, "n-second", "art-cleared", "Cleared Artist",
		nfoMBIDNewFormMessage, NFOMBIDReportSource, at(4))

	// NOT reported: a different rule wrote this one. Present in the fixture so
	// the exclusion assertion is not vacuous.
	seedNFOMBIDChange(t, db, "x-unrelated", "art-other", "Bystander",
		"set biography for Bystander", "rule:bio_exists", at(5))
	seedNFOMBIDProviderID(t, db, "art-other", "11111111-2222-3333-4444-555555555555")
}

func nfoMBIDIDs(rows []NFOMBIDWriteRow) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].ID
	}
	return out
}

func nfoMBIDByID(rows []NFOMBIDWriteRow) map[string]NFOMBIDWriteRow {
	out := make(map[string]NFOMBIDWriteRow, len(rows))
	for i := range rows {
		out[rows[i].ID] = rows[i]
	}
	return out
}

// countRowsWithSource is the PRECONDITION helper. Every exclusion assertion
// below first proves the row it expects to be absent from the report is actually
// present in the database, so "it is not in the results" cannot pass because the
// fixture never inserted it.
func countRowsWithSource(t *testing.T, db *sql.DB, source string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM metadata_changes WHERE source = ?`, source).Scan(&n); err != nil {
		t.Fatalf("counting rows with source %q: %v", source, err)
	}
	return n
}

// TestListNFOMBIDWrites_ReportsEveryRuleWriteAndNothingElse is the core
// selection assertion: all four rule-written rows, in every message wording, and
// not the row written by a different rule.
func TestListNFOMBIDWrites_ReportsEveryRuleWriteAndNothingElse(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedNFOMBIDFixture(t, db)

	// PRECONDITION: the unrelated row exists, so its absence below is a real
	// exclusion rather than an empty fixture.
	if n := countRowsWithSource(t, db, "rule:bio_exists"); n != 1 {
		t.Fatalf("precondition: rows with source rule:bio_exists = %d, want 1; "+
			"the exclusion assertion would be vacuous", n)
	}
	if n := countRowsWithSource(t, db, NFOMBIDReportSource); n != 4 {
		t.Fatalf("precondition: rows with source %q = %d, want 4", NFOMBIDReportSource, n)
	}

	rows, err := svc.ListNFOMBIDWrites(context.Background(), NFOMBIDFilter{})
	if err != nil {
		t.Fatalf("ListNFOMBIDWrites: %v", err)
	}

	got := nfoMBIDByID(rows)
	if len(got) != 4 {
		t.Fatalf("row ids = %v, want exactly the 4 rule-written rows", nfoMBIDIDs(rows))
	}
	if _, ok := got["x-unrelated"]; ok {
		t.Errorf("row written by a different rule leaked into the report; got %v", nfoMBIDIDs(rows))
	}

	// Both message wordings survive verbatim. The report shows recorded audit
	// text, so neither form may be reshaped, truncated, or normalized.
	wantMessages := map[string]string{
		"n-old":     nfoMBIDOldFormMessage,
		"n-new":     nfoMBIDNewFormMessage,
		"n-cleared": nfoMBIDOldFormMessage,
		"n-second":  nfoMBIDNewFormMessage,
	}
	for id, want := range wantMessages {
		row, ok := got[id]
		if !ok {
			t.Errorf("missing expected row %q; got %v", id, nfoMBIDIDs(rows))
			continue
		}
		if row.Message != want {
			t.Errorf("row %q Message = %q, want the recorded text verbatim %q", id, row.Message, want)
		}
		if row.ArtistName == "" {
			t.Errorf("row %q has empty ArtistName; the report cannot name the affected artist", id)
		}
		if row.Source != NFOMBIDReportSource {
			t.Errorf("row %q Source = %q, want %q", id, row.Source, NFOMBIDReportSource)
		}
	}
}

// TestListNFOMBIDWrites_MissingCurrentIDIsDistinctFromEmpty is the
// anti-"unknown rendered as clean" guard for the nullable join, and the most
// important test in this file.
//
// An artist whose MusicBrainz ID has since been cleared has NO row in
// artist_provider_ids. It must still be listed, and it must be distinguishable
// from an artist that has one: an inner join would drop it entirely, and a plain
// string field would report it as "" -- indistinguishable from an artist whose
// recorded ID happens to be blank.
func TestListNFOMBIDWrites_MissingCurrentIDIsDistinctFromEmpty(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedNFOMBIDFixture(t, db)

	// PRECONDITION: the cleared artist genuinely has no provider row.
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM artist_provider_ids WHERE artist_id = 'art-cleared'`).Scan(&n); err != nil {
		t.Fatalf("counting provider ids: %v", err)
	}
	if n != 0 {
		t.Fatalf("precondition: art-cleared has %d provider rows, want 0", n)
	}

	rows, err := svc.ListNFOMBIDWrites(context.Background(), NFOMBIDFilter{})
	if err != nil {
		t.Fatalf("ListNFOMBIDWrites: %v", err)
	}
	got := nfoMBIDByID(rows)

	cleared, ok := got["n-cleared"]
	if !ok {
		t.Fatalf("the artist with no current MusicBrainz ID was DROPPED from the report; "+
			"an artist whose wrong ID was already cleared is exactly who this report must list. got %v",
			nfoMBIDIDs(rows))
	}
	if cleared.HasCurrentMusicBrainzID {
		t.Errorf("n-cleared HasCurrentMusicBrainzID = true, want false; " +
			"a missing ID must not be reported as a recorded one")
	}
	if cleared.CurrentMusicBrainzID != "" {
		t.Errorf("n-cleared CurrentMusicBrainzID = %q, want empty", cleared.CurrentMusicBrainzID)
	}

	// The present case, for contrast, and to prove the join reads the LIVE value
	// rather than parsing the audit message: n-old's message records a different
	// ID from the one the artist carries now.
	old, ok := got["n-old"]
	if !ok {
		t.Fatalf("missing row n-old; got %v", nfoMBIDIDs(rows))
	}
	if !old.HasCurrentMusicBrainzID {
		t.Errorf("n-old HasCurrentMusicBrainzID = false, want true")
	}
	if old.CurrentMusicBrainzID != "ffffffff-0000-0000-0000-000000000000" {
		t.Errorf("n-old CurrentMusicBrainzID = %q, want the LIVE value; the report must not "+
			"read the ID out of the recorded message", old.CurrentMusicBrainzID)
	}
}

// TestListNFOMBIDWrites_EveryWriteIsReportedNotJustTheLatest pins the
// deliberate difference from the blast-radius report. Two writes for one artist
// produce two rows: each was a separate guess, and collapsing them would hide a
// second guess made after the first.
func TestListNFOMBIDWrites_EveryWriteIsReportedNotJustTheLatest(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedNFOMBIDFixture(t, db)

	rows, err := svc.ListNFOMBIDWrites(context.Background(), NFOMBIDFilter{ArtistID: "art-cleared"})
	if err != nil {
		t.Fatalf("ListNFOMBIDWrites: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows for art-cleared = %v, want both writes (n-cleared, n-second)", nfoMBIDIDs(rows))
	}
	got := nfoMBIDByID(rows)
	for _, id := range []string{"n-cleared", "n-second"} {
		if _, ok := got[id]; !ok {
			t.Errorf("write %q is missing; only the newest write per artist was reported", id)
		}
	}
}

// TestListNFOMBIDWrites_Sorting covers both sort keys in both directions. The
// fixture's artist names run opposite to its timestamps, so a query sorting on
// the wrong column cannot produce the expected sequence by coincidence.
func TestListNFOMBIDWrites_Sorting(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedNFOMBIDFixture(t, db)

	tests := []struct {
		name   string
		filter NFOMBIDFilter
		want   []string
	}{
		{
			name:   "created_at desc is the default",
			filter: NFOMBIDFilter{},
			want:   []string{"n-second", "n-cleared", "n-new", "n-old"},
		},
		{
			name:   "created_at asc",
			filter: NFOMBIDFilter{Sort: NFOMBIDSortCreatedAt, Order: "asc"},
			want:   []string{"n-old", "n-new", "n-cleared", "n-second"},
		},
		{
			// "Cleared Artist" < "New Form Artist" < "Old Form Artist", and the two
			// Cleared rows tie on name, so the tiebreaker puts the newer one first.
			name:   "artist_name asc",
			filter: NFOMBIDFilter{Sort: NFOMBIDSortArtistName, Order: "asc"},
			want:   []string{"n-second", "n-cleared", "n-new", "n-old"},
		},
		{
			name:   "artist_name desc",
			filter: NFOMBIDFilter{Sort: NFOMBIDSortArtistName, Order: "desc"},
			want:   []string{"n-old", "n-new", "n-second", "n-cleared"},
		},
		{
			// An unrecognized sort key falls back to created_at rather than
			// erroring, so a malformed parameter cannot hide affected artists.
			name:   "unknown sort key falls back to created_at desc",
			filter: NFOMBIDFilter{Sort: "'; DROP TABLE artists; --", Order: "sideways"},
			want:   []string{"n-second", "n-cleared", "n-new", "n-old"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := svc.ListNFOMBIDWrites(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("ListNFOMBIDWrites: %v", err)
			}
			got := nfoMBIDIDs(rows)
			if len(got) != len(tc.want) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("ids = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestListNFOMBIDWrites_Pagination walks the result set one row at a time and
// asserts the pages tile the whole set with no repeats and no gaps. A missing
// tiebreaker in the ORDER BY would show up here as a duplicate or a dropped row
// across the two Cleared Artist rows, which share an artist name.
func TestListNFOMBIDWrites_Pagination(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedNFOMBIDFixture(t, db)
	ctx := context.Background()

	want := []string{"n-second", "n-cleared", "n-new", "n-old"}
	var seen []string
	for offset := 0; offset < len(want)+1; offset++ {
		rows, err := svc.ListNFOMBIDWrites(ctx, NFOMBIDFilter{Limit: 1, Offset: offset})
		if err != nil {
			t.Fatalf("ListNFOMBIDWrites(offset=%d): %v", offset, err)
		}
		if offset == len(want) {
			if len(rows) != 0 {
				t.Errorf("offset past the end returned %v, want no rows", nfoMBIDIDs(rows))
			}
			continue
		}
		if len(rows) != 1 {
			t.Fatalf("offset=%d returned %d rows, want 1", offset, len(rows))
		}
		seen = append(seen, rows[0].ID)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("paged ids = %v, want %v", seen, want)
		}
	}
}

// TestCountNFOMBIDWrites counts writes and distinct artists, and proves the
// counts exclude other rules' rows and ignore the page window.
func TestCountNFOMBIDWrites(t *testing.T) {
	t.Parallel()
	svc, db := setupHistoryTestDB(t)
	seedNFOMBIDFixture(t, db)
	ctx := context.Background()

	// PRECONDITION: a row from another rule exists, so "5 total rows but 4
	// counted" is a real exclusion.
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metadata_changes`).Scan(&total); err != nil {
		t.Fatalf("counting all changes: %v", err)
	}
	if total != 5 {
		t.Fatalf("precondition: metadata_changes holds %d rows, want 5", total)
	}

	// Limit 1 on purpose: the counts describe the whole result set, not the page.
	counts, err := svc.CountNFOMBIDWrites(ctx, NFOMBIDFilter{Limit: 1})
	if err != nil {
		t.Fatalf("CountNFOMBIDWrites: %v", err)
	}
	if counts.Writes != 4 {
		t.Errorf("Writes = %d, want 4 (the page window must not narrow the count)", counts.Writes)
	}
	// Three artists, four writes: the distinction is the whole reason both
	// numbers exist, and a fixture where they coincided would prove nothing.
	if counts.Artists != 3 {
		t.Errorf("Artists = %d, want 3 distinct artists", counts.Artists)
	}
	if counts.Total != counts.Writes {
		t.Errorf("Total = %d, want it to mirror Writes (%d)", counts.Total, counts.Writes)
	}

	// Narrowed to one artist: two writes, one artist.
	narrowed, err := svc.CountNFOMBIDWrites(ctx, NFOMBIDFilter{ArtistID: "art-cleared"})
	if err != nil {
		t.Fatalf("CountNFOMBIDWrites(artist): %v", err)
	}
	if narrowed.Writes != 2 || narrowed.Artists != 1 {
		t.Errorf("narrowed counts = %+v, want Writes=2 Artists=1", narrowed)
	}
}

// TestNFOMBIDReport_EmptyLibrary proves an empty database yields an empty slice
// and honest zeros, never a nil slice (which marshals to JSON null) and never an
// error.
func TestNFOMBIDReport_EmptyLibrary(t *testing.T) {
	t.Parallel()
	svc, _ := setupHistoryTestDB(t)
	ctx := context.Background()

	rows, err := svc.ListNFOMBIDWrites(ctx, NFOMBIDFilter{})
	if err != nil {
		t.Fatalf("ListNFOMBIDWrites: %v", err)
	}
	if rows == nil {
		t.Error("rows is nil; it must be an empty slice so it marshals as [] rather than null")
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want none", nfoMBIDIDs(rows))
	}

	counts, err := svc.CountNFOMBIDWrites(ctx, NFOMBIDFilter{})
	if err != nil {
		t.Fatalf("CountNFOMBIDWrites: %v", err)
	}
	if counts.Writes != 0 || counts.Artists != 0 || counts.Total != 0 {
		t.Errorf("counts = %+v, want all zero", counts)
	}
}

// TestNFOMBIDFilter_Validate proves the coercion contract: nothing is ever
// rejected, and every value that reaches SQL is a known-good one.
func TestNFOMBIDFilter_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   NFOMBIDFilter
		want NFOMBIDFilter
	}{
		{
			name: "zero value gets the defaults",
			in:   NFOMBIDFilter{},
			want: NFOMBIDFilter{Sort: NFOMBIDSortCreatedAt, Order: "desc", Limit: 50},
		},
		{
			name: "unknown sort coerces to created_at",
			in:   NFOMBIDFilter{Sort: "artist_name; DROP TABLE artists"},
			want: NFOMBIDFilter{Sort: NFOMBIDSortCreatedAt, Order: "desc", Limit: 50},
		},
		{
			name: "artist_name survives",
			in:   NFOMBIDFilter{Sort: NFOMBIDSortArtistName, Order: "asc"},
			want: NFOMBIDFilter{Sort: NFOMBIDSortArtistName, Order: "asc", Limit: 50},
		},
		{
			name: "anything other than asc becomes desc",
			in:   NFOMBIDFilter{Order: "ASC"},
			want: NFOMBIDFilter{Sort: NFOMBIDSortCreatedAt, Order: "desc", Limit: 50},
		},
		{
			name: "non-positive limit becomes the default",
			in:   NFOMBIDFilter{Limit: -7},
			want: NFOMBIDFilter{Sort: NFOMBIDSortCreatedAt, Order: "desc", Limit: 50},
		},
		{
			name: "oversized limit is capped",
			in:   NFOMBIDFilter{Limit: 100000},
			want: NFOMBIDFilter{Sort: NFOMBIDSortCreatedAt, Order: "desc", Limit: 500},
		},
		{
			name: "negative offset becomes zero",
			in:   NFOMBIDFilter{Offset: -3},
			want: NFOMBIDFilter{Sort: NFOMBIDSortCreatedAt, Order: "desc", Limit: 50},
		},
		{
			name: "artist id is left alone; it is bound, never interpolated",
			in:   NFOMBIDFilter{ArtistID: "a' OR 1=1 --"},
			want: NFOMBIDFilter{ArtistID: "a' OR 1=1 --", Sort: NFOMBIDSortCreatedAt, Order: "desc", Limit: 50},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.in
			got.Validate()
			if got != tc.want {
				t.Errorf("Validate() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
