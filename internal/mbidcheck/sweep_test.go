package mbidcheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
)

// ---------------------------------------------------------------------------
// Test doubles for the sweep
// ---------------------------------------------------------------------------

// fakePopulation returns a scripted population.
type fakePopulation struct {
	// mu guards calls so a test can read the count safely. Run itself is
	// single-threaded, but the double is shared state read from the test
	// goroutine after the pass, and the lock keeps that honest under -race
	// regardless of how a future caller drives it.
	mu    sync.Mutex
	rows  []artist.MBIDPath
	err   error
	calls int
}

func (p *fakePopulation) ListMBIDPopulation(context.Context) ([]artist.MBIDPath, error) {
	p.mu.Lock()
	p.calls++
	rows, err := p.rows, p.err
	p.mu.Unlock()
	return rows, err
}

// fakeArtists resolves artist ids to records.
type fakeArtists struct {
	byID map[string]*artist.Artist
	err  error
	// opts records the hydration options the sweep asked for, so a test can
	// pin that the sweep requests provider ids (without them the artist's
	// MusicBrainzID is empty and every artist would leave the population).
	opts []artist.HydrateOpts
}

func (f *fakeArtists) GetByID(_ context.Context, id string, opts ...artist.HydrateOpts) (*artist.Artist, error) {
	f.opts = append(f.opts, opts...)
	if f.err != nil {
		return nil, f.err
	}
	a, ok := f.byID[id]
	if !ok {
		return nil, fmt.Errorf("no such artist %q", id)
	}
	// A copy, so a sweep that mutated the record could not silently share the
	// mutation with the fixture the test asserts against.
	cp := *a
	return &cp, nil
}

// fakeLedger records upserts, keyed by artist id, latest wins -- matching the
// real repository's one-row-per-artist contract.
type fakeLedger struct {
	mu      sync.Mutex
	rows    map[string]artist.MBIDValidation
	writes  []artist.MBIDValidation
	err     error
	failFor map[string]bool
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{rows: map[string]artist.MBIDValidation{}, failFor: map[string]bool{}}
}

func (l *fakeLedger) Upsert(_ context.Context, v *artist.MBIDValidation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	if l.failFor[v.ArtistID] {
		return errors.New("ledger write refused for this artist")
	}
	l.writes = append(l.writes, *v)
	l.rows[v.ArtistID] = *v
	return nil
}

func (l *fakeLedger) get(id string) (artist.MBIDValidation, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.rows[id]
	return v, ok
}

// sweepArtist builds an artist carrying a stored id, since an artist without
// one is not a member of this feature's population at all.
func sweepArtist(id, path string) *artist.Artist {
	return &artist.Artist{
		ID:            id,
		Name:          "Example Band",
		MusicBrainzID: fmt.Sprintf("%s-2222-3333-4444-555555555555", id),
		Path:          path,
	}
}

// newTestSweep wires a sweep over the supplied doubles with logging silenced
// and a small per-pass cap, so a test's slice boundaries are its own.
func newTestSweep(t *testing.T, pop *fakePopulation, arts *fakeArtists, led *fakeLedger, r *Resolver, cfg Config) *Sweep {
	t.Helper()
	return NewSweep(pop, arts, led, r, cfg, slog.New(slog.DiscardHandler))
}

// ---------------------------------------------------------------------------
// THE skip-don't-clear test
// ---------------------------------------------------------------------------

// TestRunSkipsTransientAndKeepsPriorVerdict is the most important test in this
// feature.
//
// It wires the WHOLE machinery that would misbehave -- a real Resolver over a
// MusicBrainz client that fails the way an outage fails, a real prior verdict
// sitting in the ledger -- and asserts the destructive effect is ABSENT: the
// prior "failed" row is still there, unmodified, and no write was issued at
// all.
//
// The failure it guards against is catastrophic rather than cosmetic. Without
// the skip, one sweep during a MusicBrainz outage rewrites every real finding
// in the library to "not checkable / provider unavailable", and the operator's
// review queue empties itself overnight with no trace of what it held.
func TestRunSkipsTransientAndKeepsPriorVerdict(t *testing.T) {
	t.Parallel()

	const id = "artist-1"
	prior := artist.MBIDValidation{
		ArtistID:     id,
		MBID:         "11111111-2222-3333-4444-555555555555",
		Outcome:      artist.MBIDOutcomeFailed,
		Reason:       artist.MBIDReasonCatalogueMismatch,
		Detail:       "the real finding an outage must not erase",
		ResolvedName: "Someone Else",
		CheckedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	led := newFakeLedger()
	if err := led.Upsert(t.Context(), &prior); err != nil {
		t.Fatalf("seeding the prior verdict: %v", err)
	}
	// PRECONDITION: the prior verdict really is a FAILED row. Without this the
	// assertion below would pass just as happily over an empty ledger, which is
	// exactly the state a broken skip produces on a fresh library.
	seeded, ok := led.get(id)
	if !ok || seeded.Outcome != artist.MBIDOutcomeFailed {
		t.Fatalf("precondition: ledger must hold a failed verdict for %s, has %+v (present=%v)", id, seeded, ok)
	}
	writesBefore := len(led.writes)

	// An outage: GetArtist errors with something that is NOT ErrNotFound, which
	// is the resolver's provider_unavailable path and carries Transient.
	mb := &fakeMB{metaErr: errors.New("dial tcp: connection refused")}
	res := newTestResolver(mb, found("An Album"))

	pop := &fakePopulation{rows: []artist.MBIDPath{{ArtistID: id, MBID: prior.MBID, Path: "/library/Example"}}}
	arts := &fakeArtists{byID: map[string]*artist.Artist{id: sweepArtist(id, "/library/Example")}}
	sw := newTestSweep(t, pop, arts, led, res, Config{MaxPerPass: 10})

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// PRECONDITION on the verdict itself: the resolver really did produce a
	// transient not-checkable verdict. If the fixture stopped producing one,
	// every assertion below would be vacuous.
	if c.Checked != 1 {
		t.Fatalf("precondition: expected 1 checked artist, got %d (%+v)", c.Checked, c)
	}
	if c.SkippedTransient != 1 {
		t.Fatalf("expected the transient verdict to be skipped, counters = %+v", c)
	}

	if len(led.writes) != writesBefore {
		t.Fatalf("a transient verdict was persisted: %d write(s) after the pass, want %d", len(led.writes), writesBefore)
	}
	after, ok := led.get(id)
	if !ok {
		t.Fatal("the prior verdict was deleted")
	}
	if after.Outcome != artist.MBIDOutcomeFailed || after.Reason != artist.MBIDReasonCatalogueMismatch {
		t.Errorf("prior verdict was overwritten: %+v", after)
	}
	if after.Detail != prior.Detail {
		t.Errorf("prior detail changed: %q, want %q", after.Detail, prior.Detail)
	}
	if c.NotCheckable != 0 {
		t.Errorf("a skipped verdict must not count as persisted not_checkable, got %d", c.NotCheckable)
	}
}

// TestRunSkipsTransientOnAFreshArtist covers the second half of the policy: a
// transient verdict is not written even when there is nothing to protect. That
// is what makes every PERSISTED no_local_albums row mean "genuinely no albums"
// rather than "could not look" -- a distinction the ledger's frozen reason
// vocabulary cannot otherwise carry.
func TestRunSkipsTransientOnAFreshArtist(t *testing.T) {
	t.Parallel()

	const id = "artist-1"
	led := newFakeLedger()
	if _, present := led.get(id); present {
		t.Fatal("precondition: the ledger must start empty for this artist")
	}

	// EvidenceUnknown: the album source could not look. The resolver classifies
	// this not_checkable/no_local_albums AND marks it transient.
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}}
	albums := stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceUnknown}, err: errors.New("mount is gone")}
	res := newTestResolver(mb, albums)

	pop := &fakePopulation{rows: []artist.MBIDPath{{ArtistID: id, MBID: "m", Path: ""}}}
	arts := &fakeArtists{byID: map[string]*artist.Artist{id: sweepArtist(id, "")}}
	sw := newTestSweep(t, pop, arts, led, res, Config{MaxPerPass: 10})

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.Checked != 1 || c.SkippedTransient != 1 {
		t.Fatalf("expected 1 checked and 1 skipped, got %+v", c)
	}
	if _, present := led.get(id); present {
		t.Error("an unreadable-catalogue verdict reached the ledger, making it indistinguishable from a genuinely empty artist")
	}
}

// TestRunPersistsEvidenceNoneAsNotCheckable is the counterpart: EvidenceNone is
// a real determination, is NOT transient, and must land. Without it the test
// above could be satisfied by a sweep that persisted nothing at all.
func TestRunPersistsEvidenceNoneAsNotCheckable(t *testing.T) {
	t.Parallel()

	const id = "artist-1"
	led := newFakeLedger()
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}}
	res := newTestResolver(mb, stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceNone}})

	pop := &fakePopulation{rows: []artist.MBIDPath{{ArtistID: id, MBID: "m"}}}
	arts := &fakeArtists{byID: map[string]*artist.Artist{id: sweepArtist(id, "/library/Example")}}
	sw := newTestSweep(t, pop, arts, led, res, Config{MaxPerPass: 10})

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.NotCheckable != 1 || c.SkippedTransient != 0 {
		t.Fatalf("expected 1 persisted not_checkable and 0 skipped, got %+v", c)
	}
	row, ok := led.get(id)
	if !ok {
		t.Fatal("a genuinely-no-albums verdict must be persisted")
	}
	if row.Reason != artist.MBIDReasonNoLocalAlbums {
		t.Errorf("reason = %q, want %q", row.Reason, artist.MBIDReasonNoLocalAlbums)
	}
}

// ---------------------------------------------------------------------------
// Outcome accounting
// ---------------------------------------------------------------------------

// TestRunCounters walks one artist per outcome and asserts both the ledger
// contents and the counters, including the headline ZeroRemoteCatalogue tally.
func TestRunCounters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mb      *fakeMB
		albums  artist.AlbumSource
		want    Counters
		wantRow bool
		wantOut artist.MBIDValidationOutcome
	}{
		{
			name:    "validated",
			mb:      &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("One", "Two")},
			albums:  found("One", "Two"),
			want:    Counters{Checked: 1, Validated: 1, CatalogueBuckets: [5]int{0, 0, 0, 0, 1}},
			wantRow: true,
			wantOut: artist.MBIDOutcomeValidated,
		},
		{
			name:    "failed: zero remote catalogue is counted as the headline finding",
			mb:      &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: nil},
			albums:  found("One", "Two"),
			want:    Counters{Checked: 1, Failed: 1, ZeroRemoteCatalogue: 1, CatalogueBuckets: [5]int{1}},
			wantRow: true,
			wantOut: artist.MBIDOutcomeFailed,
		},
		{
			name:    "failed: catalogue disagrees but the remote list is not empty",
			mb:      &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("Something Else")},
			albums:  found("One", "Two", "Three", "Four"),
			want:    Counters{Checked: 1, Failed: 1, CatalogueBuckets: [5]int{1}},
			wantRow: true,
			wantOut: artist.MBIDOutcomeFailed,
		},
		{
			name:    "not checkable: the id resolves to nobody",
			mb:      &fakeMB{metaErr: &provider.ErrNotFound{Provider: provider.NameMusicBrainz, ID: "m"}},
			albums:  found("One"),
			want:    Counters{Checked: 1, NotCheckable: 1},
			wantRow: true,
			wantOut: artist.MBIDOutcomeNotCheckable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const id = "artist-1"
			led := newFakeLedger()
			pop := &fakePopulation{rows: []artist.MBIDPath{{ArtistID: id, MBID: "m", Path: "/library/Example"}}}
			arts := &fakeArtists{byID: map[string]*artist.Artist{id: sweepArtist(id, "/library/Example")}}
			sw := newTestSweep(t, pop, arts, led, newTestResolver(tt.mb, tt.albums), Config{MaxPerPass: 10})

			got, err := sw.Run(t.Context())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got != tt.want {
				t.Errorf("counters = %+v, want %+v", got, tt.want)
			}
			row, ok := led.get(id)
			if ok != tt.wantRow {
				t.Fatalf("ledger row present = %v, want %v", ok, tt.wantRow)
			}
			if tt.wantRow && row.Outcome != tt.wantOut {
				t.Errorf("persisted outcome = %q, want %q", row.Outcome, tt.wantOut)
			}
		})
	}
}

// TestCatalogueBucket pins the distribution boundaries around the default
// threshold of 25, which is what makes a pass summary usable for judging
// whether that threshold is set right for a given library.
func TestCatalogueBucket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pct  float64
		want int
	}{
		{0, 0}, {0.4, 1}, {1, 1}, {24.9, 1},
		{25, 2}, {49.9, 2},
		{50, 3}, {74.9, 3},
		{75, 4}, {100, 4},
	}
	for _, tt := range tests {
		if got := catalogueBucket(tt.pct); got != tt.want {
			t.Errorf("catalogueBucket(%v) = %d, want %d", tt.pct, got, tt.want)
		}
	}
	if len(catalogueBucketNames) != len(Counters{}.CatalogueBuckets) {
		t.Fatal("bucket names and bucket slots have drifted apart")
	}
}

// ---------------------------------------------------------------------------
// Population handling
// ---------------------------------------------------------------------------

// TestRunChecksPathlessArtists asserts the population's pathless members are
// actually processed rather than filtered out somewhere in the sweep. The
// query-level guarantee is tested in internal/artist; this is the other half,
// since a sweep that skipped them would restore the exclusion one layer up.
func TestRunChecksPathlessArtists(t *testing.T) {
	t.Parallel()

	rows := []artist.MBIDPath{
		{ArtistID: "a-1", MBID: "m1", Path: "/library/One"},
		{ArtistID: "a-2", MBID: "m2", Path: ""},
	}
	// PRECONDITION: the fixture really does carry a pathless row.
	var pathless int
	for _, r := range rows {
		if r.Path == "" {
			pathless++
		}
	}
	if pathless != 1 {
		t.Fatalf("precondition: fixture must hold exactly 1 pathless row, has %d", pathless)
	}

	led := newFakeLedger()
	// EvidenceNone for both, so both produce a PERSISTED verdict and a missing
	// row means the artist was skipped rather than merely classified transient.
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}}
	res := newTestResolver(mb, stubAlbums{set: artist.AlbumSet{Evidence: artist.EvidenceNone}})
	arts := &fakeArtists{byID: map[string]*artist.Artist{
		"a-1": sweepArtist("a-1", "/library/One"),
		"a-2": sweepArtist("a-2", ""),
	}}
	sw := newTestSweep(t, &fakePopulation{rows: rows}, arts, led, res, Config{MaxPerPass: 10})

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.Checked != 2 {
		t.Fatalf("checked = %d, want 2 (the pathless artist must be checked): %+v", c.Checked, c)
	}
	if _, ok := led.get("a-2"); !ok {
		t.Error("the pathless artist produced no ledger row: it was dropped from the sweep")
	}
}

// TestRunAdvancesCursorAcrossPasses asserts the per-pass cap walks the whole
// population instead of re-checking the same head slice forever, and that the
// cursor wraps once the population is exhausted.
func TestRunAdvancesCursorAcrossPasses(t *testing.T) {
	t.Parallel()

	rows := []artist.MBIDPath{
		{ArtistID: "a-1", MBID: "m1"},
		{ArtistID: "a-2", MBID: "m2"},
		{ArtistID: "a-3", MBID: "m3"},
	}
	byID := map[string]*artist.Artist{}
	for _, r := range rows {
		byID[r.ArtistID] = sweepArtist(r.ArtistID, "/library/"+r.ArtistID)
	}

	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("One")}
	res := newTestResolver(mb, found("One"))
	led := newFakeLedger()
	arts := &fakeArtists{byID: byID}
	sw := newTestSweep(t, &fakePopulation{rows: rows}, arts, led, res, Config{MaxPerPass: 2})

	if _, err := sw.Run(t.Context()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	firstPass := checkedIDs(led)
	if len(firstPass) != 2 || firstPass[0] != "a-1" || firstPass[1] != "a-2" {
		t.Fatalf("first pass checked %v, want [a-1 a-2]", firstPass)
	}

	if _, err := sw.Run(t.Context()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	second := checkedIDs(led)[2:]
	if len(second) != 1 || second[0] != "a-3" {
		t.Fatalf("second pass checked %v, want [a-3]", second)
	}

	// Exhausting the population wraps the cursor, so the third pass starts over
	// rather than going permanently idle.
	if _, err := sw.Run(t.Context()); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	third := checkedIDs(led)[3:]
	if len(third) != 2 || third[0] != "a-1" {
		t.Fatalf("third pass checked %v, want it to wrap to [a-1 a-2]", third)
	}
}

// checkedIDs is the artist ids written to the ledger, in write order.
func checkedIDs(l *fakeLedger) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.writes))
	for _, w := range l.writes {
		out = append(out, w.ArtistID)
	}
	return out
}

// TestRunPopulationError asserts a failed population query aborts the pass with
// an error rather than logging a tidy zero-finding summary.
func TestRunPopulationError(t *testing.T) {
	t.Parallel()

	pop := &fakePopulation{err: errors.New("database is locked")}
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}}
	sw := newTestSweep(t, pop, &fakeArtists{byID: map[string]*artist.Artist{}}, newFakeLedger(),
		newTestResolver(mb, found("One")), Config{})

	if _, err := sw.Run(t.Context()); err == nil {
		t.Fatal("expected an error when the population query fails")
	}
}

// ---------------------------------------------------------------------------
// Per-artist failure isolation
// ---------------------------------------------------------------------------

// TestRunIsolatesPerArtistFailures asserts one unloadable artist, one that has
// left the population, and one failing ledger write are each counted and
// stepped over, leaving the remaining artists checked.
func TestRunIsolatesPerArtistFailures(t *testing.T) {
	t.Parallel()

	rows := []artist.MBIDPath{
		{ArtistID: "a-1", MBID: "m1"}, // loads fine
		{ArtistID: "a-2", MBID: "m2"}, // stored id cleared since the query
		{ArtistID: "a-3", MBID: "m3"}, // ledger write fails
	}
	noMBID := sweepArtist("a-2", "/library/a-2")
	noMBID.MusicBrainzID = ""

	byID := map[string]*artist.Artist{
		"a-1": sweepArtist("a-1", "/library/a-1"),
		"a-2": noMBID,
		"a-3": sweepArtist("a-3", "/library/a-3"),
	}
	led := newFakeLedger()
	led.failFor["a-3"] = true

	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("One")}
	sw := newTestSweep(t, &fakePopulation{rows: rows}, &fakeArtists{byID: byID}, led,
		newTestResolver(mb, found("One")), Config{MaxPerPass: 10})

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.SkippedNoMBID != 1 {
		t.Errorf("SkippedNoMBID = %d, want 1", c.SkippedNoMBID)
	}
	if c.Errored != 1 {
		t.Errorf("Errored = %d, want 1 (the refused ledger write)", c.Errored)
	}
	if c.Validated != 1 {
		t.Errorf("Validated = %d, want 1 (a-1 must still be checked)", c.Validated)
	}
	if _, ok := led.get("a-1"); !ok {
		t.Error("a-1 should have been persisted despite its neighbors failing")
	}
}

// TestRunRequestsProviderIDHydration pins the hydration option. The stored MBID
// lives in a side table, so a sweep that asked for HydrateOpts{} would load
// every artist with an empty MusicBrainzID and file the entire library as "left
// the population" -- a pass that reports success and checks nothing.
func TestRunRequestsProviderIDHydration(t *testing.T) {
	t.Parallel()

	arts := &fakeArtists{byID: map[string]*artist.Artist{
		"a-1": sweepArtist("a-1", "/library/a-1"),
	}}
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("One")}
	sw := newTestSweep(t, &fakePopulation{rows: []artist.MBIDPath{{ArtistID: "a-1", MBID: "m1"}}},
		arts, newFakeLedger(), newTestResolver(mb, found("One")), Config{})

	if _, err := sw.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(arts.opts) != 1 {
		t.Fatalf("expected exactly 1 GetByID call, got %d", len(arts.opts))
	}
	if !arts.opts[0].ProviderIDs {
		t.Error("the sweep must request ProviderIDs hydration; without it no artist carries a stored MBID")
	}
}

// ---------------------------------------------------------------------------
// Scheduling and cancellation
// ---------------------------------------------------------------------------

// TestRunStopsOnCanceledContext asserts the per-item cancellation check fires:
// a context canceled after the first artist must stop the pass rather than
// letting it grind through the whole slice issuing requests nobody is waiting
// for.
//
// It then asserts the far more important half: the pass KEEPS ITS PLACE. A
// canceled pass leaves an unprocessed tail, so the cursor must not wrap, and
// the NEXT pass must resume at the first artist nobody looked at. Stopping
// early is cheap to get right and was; the earlier version of this test
// asserted only that, and so passed happily while the cursor was cleared on
// every cancellation -- sending each pass back to the head of the population
// and leaving the tail permanently unchecked behind a summary reporting a
// clean, exhausted sweep.
func TestRunStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	rows := []artist.MBIDPath{
		{ArtistID: "a-1", MBID: "m1"},
		{ArtistID: "a-2", MBID: "m2"},
		{ArtistID: "a-3", MBID: "m3"},
	}
	byID := map[string]*artist.Artist{}
	for _, r := range rows {
		byID[r.ArtistID] = sweepArtist(r.ArtistID, "/library/"+r.ArtistID)
	}

	// PRECONDITION: the fixture must actually have had more artists left to do.
	if len(rows) <= 1 {
		t.Fatal("precondition: need more than one artist to observe an early stop")
	}
	// PRECONDITION: with this cap the first pass's slice genuinely REACHES the
	// end of the population, so wrapped is true and the cursor is a candidate
	// for being cleared. Without that, the resume assertion below would hold
	// for the wrong reason -- it would be testing a slice that never wrapped.
	if _, wrapped := (&Sweep{}).selectSlice(rows, 10); !wrapped {
		t.Fatal("precondition: the first pass's slice must reach the end of the population")
	}

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel from inside the first artist's provider call, so the pass is
	// genuinely mid-slice when the context goes down. This WIRES the condition
	// rather than pre-canceling, which any implementation would notice.
	mb := &cancelingMB{cancel: cancel, meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("One")}
	led := newFakeLedger()
	sw := newTestSweep(t, &fakePopulation{rows: rows}, &fakeArtists{byID: byID}, led,
		newTestResolver(mb, found("One")), Config{MaxPerPass: 10})

	c, err := sw.Run(ctx)
	// An abandoned pass reports its cancellation, so a caller can tell a
	// partial cycle from a complete one; the counters alone cannot say it.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want a context.Canceled: an abandoned pass must not report success", err)
	}
	if mb.artistCalls != 1 {
		t.Errorf("provider was called %d time(s) after cancellation; want exactly 1", mb.artistCalls)
	}
	// PRECONDITION for the resume assertion: the first pass really did process
	// exactly one artist, leaving a-2 and a-3 untouched.
	if c.Checked != 1 {
		t.Fatalf("precondition: expected exactly 1 checked artist in the canceled pass, got %d (%+v)", c.Checked, c)
	}
	if got := checkedIDs(led); len(got) != 1 || got[0] != "a-1" {
		t.Fatalf("precondition: canceled pass wrote %v, want [a-1]", got)
	}

	// THE RESUME. A fresh context, a second pass: it must pick up the tail the
	// canceled pass abandoned rather than restarting at the head.
	if _, err := sw.Run(t.Context()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	resumed := checkedIDs(led)[1:]
	if len(resumed) == 0 {
		t.Fatal("the second pass checked nothing")
	}
	if resumed[0] != "a-2" {
		t.Fatalf("second pass resumed at %q (wrote %v), want a-2: the canceled pass wrapped the cursor and skipped the population tail",
			resumed[0], resumed)
	}
	if len(resumed) != 2 || resumed[1] != "a-3" {
		t.Errorf("second pass checked %v, want the whole abandoned tail [a-2 a-3]", resumed)
	}
}

// TestRunDoesNotReportAnAbandonedPassAsExhausted pins the summary half of the
// same correction. population_exhausted is the field an operator reads to know
// the sweep has been all the way round the library, and it was previously fed
// the slice's SHAPE rather than what the pass actually did -- so an abandoned
// tail was announced as a completed cycle. A counters-only assertion cannot see
// this: the log line is the only place the claim is made.
func TestRunDoesNotReportAnAbandonedPassAsExhausted(t *testing.T) {
	t.Parallel()

	rows := []artist.MBIDPath{
		{ArtistID: "a-1", MBID: "m1"},
		{ArtistID: "a-2", MBID: "m2"},
	}
	byID := map[string]*artist.Artist{}
	for _, r := range rows {
		byID[r.ArtistID] = sweepArtist(r.ArtistID, "/library/"+r.ArtistID)
	}
	// PRECONDITION: the slice reaches the end of the population, so wrapped is
	// true and the field would read true if it were still fed from the shape.
	if _, wrapped := (&Sweep{}).selectSlice(rows, 10); !wrapped {
		t.Fatal("precondition: the pass's slice must reach the end of the population")
	}

	rec := &recordingHandler{}
	ctx, cancel := context.WithCancel(t.Context())
	mb := &cancelingMB{cancel: cancel, meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("One")}
	sw := NewSweep(&fakePopulation{rows: rows}, &fakeArtists{byID: byID}, newFakeLedger(),
		newTestResolver(mb, found("One")), Config{MaxPerPass: 10}, slog.New(rec))

	if _, err := sw.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}

	got, ok := rec.attr("mbid re-validation pass complete", "population_exhausted")
	if !ok {
		t.Fatal("no pass summary was logged: an operator has nothing to read")
	}
	if got != false {
		t.Errorf("population_exhausted = %v on a pass abandoned mid-slice, want false: an unprocessed tail is not a completed cycle", got)
	}
}

// TestRunReportsCancellationDuringTheFinalRow covers the path the between-items
// check structurally cannot reach.
//
// That check runs at the TOP of an iteration, so it only fires when there is a
// next artist. Cancel during the LAST row's work -- inside the provider call or
// inside the ledger write -- and the loop ends normally with nothing having
// observed it, so the pass reported success, cleared the cursor and announced
// population_exhausted. The population tail would then be re-walked from the
// head on the next pass while the summary claimed a completed cycle: the exact
// defect the cursor logic was corrected for, on the one row it could not see.
//
// A test that cancels on an EARLIER row passes with or without the post-loop
// check, which is why the cancellation is pinned to the final row and the
// preconditions below assert it landed there.
func TestRunReportsCancellationDuringTheFinalRow(t *testing.T) {
	t.Parallel()

	rows := []artist.MBIDPath{
		{ArtistID: "a-1", MBID: "m1"},
		{ArtistID: "a-2", MBID: "m2"},
	}
	byID := map[string]*artist.Artist{}
	for _, r := range rows {
		byID[r.ArtistID] = sweepArtist(r.ArtistID, "/library/"+r.ArtistID)
	}

	// PRECONDITION: the slice reaches the end of the population, so wrapped is
	// true and the cursor really is a candidate for being cleared. Without this
	// the assertions below would hold for the wrong reason.
	if _, wrapped := (&Sweep{}).selectSlice(rows, 10); !wrapped {
		t.Fatal("precondition: the pass's slice must reach the end of the population")
	}

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel from inside the LAST row's provider call: at that moment there is
	// no next iteration left to notice it.
	mb := &cancelOnCallMB{cancelAt: len(rows), cancel: cancel,
		meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("One")}
	rec := &recordingHandler{}
	led := newFakeLedger()
	sw := NewSweep(&fakePopulation{rows: rows}, &fakeArtists{byID: byID}, led,
		newTestResolver(mb, found("One")), Config{MaxPerPass: 10}, slog.New(rec))

	c, err := sw.Run(ctx)

	// PRECONDITION: the cancellation genuinely landed on the FINAL row. Every
	// row was reached (so nothing broke out early) and the pass completed all of
	// them, meaning the in-loop check never fired and only a post-loop read can
	// have seen the cancellation.
	if mb.artistCalls != len(rows) {
		t.Fatalf("precondition: provider was called %d time(s), want %d -- the cancellation must land on the final row, not an earlier one",
			mb.artistCalls, len(rows))
	}
	if c.Checked != len(rows) {
		t.Fatalf("precondition: checked = %d, want %d -- every row including the canceled one must have been processed (%+v)",
			c.Checked, len(rows), c)
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run err = %v, want context.Canceled: a pass canceled during its last row must not report a completed cycle", err)
	}
	got, ok := rec.attr("mbid re-validation pass complete", "population_exhausted")
	if !ok {
		t.Fatal("no pass summary was logged: an operator has nothing to read")
	}
	if got != false {
		t.Errorf("population_exhausted = %v, want false: the pass was canceled, so it did not complete a cycle", got)
	}
	// The cursor must NOT have wrapped. It stays on the last row processed, so
	// the next pass continues from there rather than restarting at the head.
	if sw.cursor != "a-2" {
		t.Errorf("cursor = %q, want %q: a canceled pass cleared its place", sw.cursor, "a-2")
	}
}

// TestRunDoesNotBlameArtistsForACanceledLedgerWrite pins the second half of the
// same principle, on the write side.
//
// A ledger write refused because the process is shutting down says nothing
// about the artist, and the resolver already applies exactly this rule to a
// provider outage. Counting it as Errored and logging at ERROR turns a routine
// stop into one error line per remaining artist, each naming an artist that is
// perfectly fine -- an operator restarting the service would see a burst of
// failures pointing at their library.
//
// The wrapped sentinel is deliberate: a repository wraps its driver errors, so
// an == comparison would compile, pass a bare-error test, and silently stop
// matching in production.
func TestRunDoesNotBlameArtistsForACanceledLedgerWrite(t *testing.T) {
	t.Parallel()

	const id = "artist-1"
	led := newFakeLedger()
	led.err = fmt.Errorf("persisting the verdict: %w", context.Canceled)
	// PRECONDITION: the fixture's error really is a WRAPPED cancellation, not
	// the bare sentinel. errors.Is must match it while it is still a wrapper (a
	// non-nil Unwrap), which is what makes an == comparison in the sweep unable
	// to satisfy this test.
	if !errors.Is(led.err, context.Canceled) || errors.Unwrap(led.err) == nil {
		t.Fatalf("precondition: the ledger error must be a WRAPPED context.Canceled, got %v", led.err)
	}

	rec := &recordingHandler{}
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("One")}
	sw := NewSweep(&fakePopulation{rows: []artist.MBIDPath{{ArtistID: id, MBID: "m1"}}},
		&fakeArtists{byID: map[string]*artist.Artist{id: sweepArtist(id, "/library/"+id)}},
		led, newTestResolver(mb, found("One")), Config{MaxPerPass: 10}, slog.New(rec))

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// PRECONDITION: the artist really did reach the write. Without this the
	// zero-Errored assertion would pass over a pass that never got that far.
	if c.Checked != 1 {
		t.Fatalf("precondition: checked = %d, want 1 -- the write path must actually have been reached (%+v)", c.Checked, c)
	}

	if c.Errored != 0 {
		t.Errorf("Errored = %d, want 0: shutdown is our condition, never the artist's (%+v)", c.Errored, c)
	}
	if c.SkippedTransient != 1 {
		t.Errorf("SkippedTransient = %d, want 1: an abandoned write must still be reported as a verdict that did not land (%+v)",
			c.SkippedTransient, c)
	}
	if msgs := rec.messagesAtLevel(slog.LevelError); len(msgs) != 0 {
		t.Errorf("a canceled write logged at ERROR: %v", msgs)
	}
}

// TestRunStillReportsAGenuineLedgerFailure is the inverse, and without it the
// test above could be satisfied by a sweep that swallowed every write failure.
// A real refusal must keep counting and keep logging at ERROR.
func TestRunStillReportsAGenuineLedgerFailure(t *testing.T) {
	t.Parallel()

	const id = "artist-1"
	led := newFakeLedger()
	led.err = errors.New("disk is full")
	// PRECONDITION: the fixture's error is NOT a cancellation, so this really is
	// testing the other branch.
	if errors.Is(led.err, context.Canceled) || errors.Is(led.err, context.DeadlineExceeded) {
		t.Fatalf("precondition: the ledger error must not be a cancellation, got %v", led.err)
	}

	rec := &recordingHandler{}
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("One")}
	sw := NewSweep(&fakePopulation{rows: []artist.MBIDPath{{ArtistID: id, MBID: "m1"}}},
		&fakeArtists{byID: map[string]*artist.Artist{id: sweepArtist(id, "/library/"+id)}},
		led, newTestResolver(mb, found("One")), Config{MaxPerPass: 10}, slog.New(rec))

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.Checked != 1 {
		t.Fatalf("precondition: checked = %d, want 1 (%+v)", c.Checked, c)
	}
	if c.Errored != 1 {
		t.Errorf("Errored = %d, want 1: a real write failure must still be counted (%+v)", c.Errored, c)
	}
	if c.SkippedTransient != 0 {
		t.Errorf("SkippedTransient = %d, want 0: a disk failure is not a transient skip (%+v)", c.SkippedTransient, c)
	}
	if msgs := rec.messagesAtLevel(slog.LevelError); len(msgs) != 1 {
		t.Errorf("ERROR records = %v, want exactly one naming the failed write", msgs)
	}
}

// recordingHandler keeps every record's message and attributes so a test can
// assert what the pass summary actually claimed.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

// WithAttrs and WithGroup return the same handler: the sweep only ever calls
// With for its component tag, which no assertion here reads.
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// attr returns the named attribute's value from the first record carrying msg.
func (h *recordingHandler) attr(msg, key string) (any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message != msg {
			continue
		}
		var found any
		var ok bool
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				found, ok = a.Value.Any(), true
				return false
			}
			return true
		})
		return found, ok
	}
	return nil, false
}

// messagesAtLevel is every record's message at exactly the given level, so a
// test can assert what a path did and did not log rather than only its
// counters.
func (h *recordingHandler) messagesAtLevel(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		if r.Level == level {
			out = append(out, r.Message)
		}
	}
	return out
}

// cancelOnCallMB cancels the sweep's context during the cancelAt'th GetArtist
// call, so a test can put the cancellation on a chosen row -- specifically the
// LAST one, where no next iteration exists to observe it.
type cancelOnCallMB struct {
	cancelAt    int
	cancel      context.CancelFunc
	meta        *provider.ArtistMetadata
	groups      []provider.ReleaseGroupInfo
	artistCalls int
}

func (c *cancelOnCallMB) GetArtist(context.Context, string) (*provider.ArtistMetadata, error) {
	c.artistCalls++
	if c.artistCalls == c.cancelAt && c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	return c.meta, nil
}

func (c *cancelOnCallMB) GetReleaseGroups(context.Context, string) ([]provider.ReleaseGroupInfo, error) {
	return c.groups, nil
}

// cancelingMB cancels the sweep's context during its first GetArtist call, and
// only that one: a later pass driven by a fresh context must be allowed to run
// to completion, which is what the resume assertion measures.
type cancelingMB struct {
	cancel      context.CancelFunc
	meta        *provider.ArtistMetadata
	groups      []provider.ReleaseGroupInfo
	artistCalls int
}

func (c *cancelingMB) GetArtist(context.Context, string) (*provider.ArtistMetadata, error) {
	c.artistCalls++
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	return c.meta, nil
}

func (c *cancelingMB) GetReleaseGroups(context.Context, string) ([]provider.ReleaseGroupInfo, error) {
	return c.groups, nil
}

// ---------------------------------------------------------------------------
// Config defaults
// ---------------------------------------------------------------------------

// TestConfigDefaults asserts a zero or nonsense value falls back to the
// documented default rather than producing a zero interval (a ticker panic) or
// a zero cap (a pass that checks nothing).
func TestConfigDefaults(t *testing.T) {
	t.Parallel()

	for _, c := range []Config{{}, {Interval: -1, StartupDelay: -1, MaxPerPass: -1}} {
		if got := c.interval(); got != DefaultInterval {
			t.Errorf("interval() = %v, want %v", got, DefaultInterval)
		}
		if got := c.startupDelay(); got != DefaultStartupDelay {
			t.Errorf("startupDelay() = %v, want %v", got, DefaultStartupDelay)
		}
		if got := c.maxPerPass(); got != DefaultMaxPerPass {
			t.Errorf("maxPerPass() = %d, want %d", got, DefaultMaxPerPass)
		}
	}

	explicit := Config{Interval: time.Minute, StartupDelay: time.Second, MaxPerPass: 7}
	if explicit.interval() != time.Minute || explicit.startupDelay() != time.Second || explicit.maxPerPass() != 7 {
		t.Errorf("an explicit config must be honored, got %+v", explicit)
	}
}

// TestNewSweepRejectsNilDependencies asserts a missing collaborator panics
// rather than yielding a sweep that runs, logs a summary, and persists nothing.
func TestNewSweepRejectsNilDependencies(t *testing.T) {
	t.Parallel()

	pop := &fakePopulation{}
	arts := &fakeArtists{}
	led := newFakeLedger()
	res := newTestResolver(&fakeMB{}, found("One"))

	tests := []struct {
		name string
		call func()
	}{
		{"nil population", func() { NewSweep(nil, arts, led, res, Config{}, nil) }},
		{"nil artists", func() { NewSweep(pop, nil, led, res, Config{}, nil) }},
		{"nil ledger", func() { NewSweep(pop, arts, nil, res, Config{}, nil) }},
		{"nil resolver", func() { NewSweep(pop, arts, led, nil, Config{}, nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("expected a panic")
				}
			}()
			tt.call()
		})
	}
}

// TestSweepNeverWritesTheArtistIdentity is a structural guard on the issue's
// hardest acceptance criterion: no code path in this feature changes an
// identity. The sweep's only writer seam is the Ledger interface, whose sole
// method takes an *artist.MBIDValidation -- a record that has no field capable
// of expressing an artist's MusicBrainzID. This compile-time assertion fails if
// a writer that could is ever added.
func TestSweepNeverWritesTheArtistIdentity(t *testing.T) {
	t.Parallel()

	var l Ledger = newFakeLedger()
	// If Ledger ever grew a method taking an *artist.Artist, this assertion is
	// where a reader is told to look at the acceptance criterion again.
	if _, ok := l.(interface {
		Update(context.Context, *artist.Artist) error
	}); ok {
		t.Fatal("the sweep's ledger seam can write an artist record; #2810 forbids automatic identity change")
	}
}

// ---------------------------------------------------------------------------
// Operator-review flagging
// ---------------------------------------------------------------------------

// fakeFlagger records the operator-review entries the sweep raises.
type fakeFlagger struct {
	mu     sync.Mutex
	raised []struct{ artistID, artistName, message string }
	err    error
}

func (f *fakeFlagger) RaiseMBIDValidationFailure(_ context.Context, artistID, artistName, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.raised = append(f.raised, struct{ artistID, artistName, message string }{artistID, artistName, message})
	return nil
}

func (f *fakeFlagger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.raised)
}

// TestFlagsOnlyFailedOutcomes is the guard on which verdicts reach the
// operator's queue.
//
// It WIRES a real flagger and a real resolver for each outcome and asserts the
// effect is present for exactly one of them and ABSENT for the other two. A
// validated verdict has nothing to review; a not-checkable one is an honest
// "unknown" (an id resolving to nobody may be correct and merely stale), and
// queueing either would bury the real findings under states meaning nothing is
// wrong.
func TestFlagsOnlyFailedOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mb          *fakeMB
		albums      artist.AlbumSource
		wantOutcome artist.MBIDValidationOutcome
		wantRaised  int
	}{
		{
			name:        "validated raises nothing",
			mb:          &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("One")},
			albums:      found("One"),
			wantOutcome: artist.MBIDOutcomeValidated,
			wantRaised:  0,
		},
		{
			name:        "not checkable raises nothing",
			mb:          &fakeMB{metaErr: &provider.ErrNotFound{Provider: provider.NameMusicBrainz, ID: "m"}},
			albums:      found("One"),
			wantOutcome: artist.MBIDOutcomeNotCheckable,
			wantRaised:  0,
		},
		{
			name:        "failed raises exactly one entry",
			mb:          &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("Something Else")},
			albums:      found("One", "Two", "Three", "Four"),
			wantOutcome: artist.MBIDOutcomeFailed,
			wantRaised:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const id = "artist-1"
			led := newFakeLedger()
			flag := &fakeFlagger{}
			sw := newTestSweep(t, &fakePopulation{rows: []artist.MBIDPath{{ArtistID: id, MBID: "m"}}},
				&fakeArtists{byID: map[string]*artist.Artist{id: sweepArtist(id, "/library/Example")}},
				led, newTestResolver(tt.mb, tt.albums), Config{MaxPerPass: 10})
			sw.SetFlagger(flag)

			if _, err := sw.Run(t.Context()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			// PRECONDITION: the fixture really produced the outcome this case
			// is about. Without it, a fixture that silently stopped producing a
			// "failed" verdict would make "raised nothing" look correct.
			row, ok := led.get(id)
			if !ok {
				t.Fatalf("precondition: expected a persisted verdict for %s", id)
			}
			if row.Outcome != tt.wantOutcome {
				t.Fatalf("precondition: outcome = %q, want %q", row.Outcome, tt.wantOutcome)
			}

			if got := flag.count(); got != tt.wantRaised {
				t.Errorf("raised %d operator-review entries, want %d", got, tt.wantRaised)
			}
		})
	}
}

// TestFlagMessageNamesTheZeroCatalogueCase asserts the headline finding gets
// its own wording, derived from Result.ZeroRemoteCatalogue rather than by
// matching detail prose, so an operator can recognize it in a queue without
// opening the row.
func TestFlagMessageNamesTheZeroCatalogueCase(t *testing.T) {
	t.Parallel()

	const id = "artist-1"
	flag := &fakeFlagger{}
	// An empty remote catalogue with albums on disk: the motivating shape.
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Someone Else"}, groups: nil}
	sw := newTestSweep(t, &fakePopulation{rows: []artist.MBIDPath{{ArtistID: id, MBID: "m"}}},
		&fakeArtists{byID: map[string]*artist.Artist{id: sweepArtist(id, "/library/Example")}},
		newFakeLedger(), newTestResolver(mb, found("One", "Two")), Config{MaxPerPass: 10})
	sw.SetFlagger(flag)

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// PRECONDITION: this really is the zero-remote-catalogue case.
	if c.ZeroRemoteCatalogue != 1 {
		t.Fatalf("precondition: expected 1 zero-remote-catalogue finding, got %+v", c)
	}
	if flag.count() != 1 {
		t.Fatalf("expected exactly 1 raised entry, got %d", flag.count())
	}
	msg := flag.raised[0].message
	for _, want := range []string{"no releases at all", "Someone Else", "2 album"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
	if !strings.Contains(msg, "Nothing has been changed") {
		t.Errorf("message %q must say nothing was changed automatically", msg)
	}
}

// TestFlaggerFailureDoesNotAbortThePass asserts a refused queue write is
// counted and logged rather than aborting: the verdict is already durably in
// the ledger, so the cost is visibility, not evidence.
func TestFlaggerFailureDoesNotAbortThePass(t *testing.T) {
	t.Parallel()

	rows := []artist.MBIDPath{{ArtistID: "a-1", MBID: "m1"}, {ArtistID: "a-2", MBID: "m2"}}
	byID := map[string]*artist.Artist{
		"a-1": sweepArtist("a-1", "/library/a-1"),
		"a-2": sweepArtist("a-2", "/library/a-2"),
	}
	led := newFakeLedger()
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: nil}
	sw := newTestSweep(t, &fakePopulation{rows: rows}, &fakeArtists{byID: byID}, led,
		newTestResolver(mb, found("One")), Config{MaxPerPass: 10})
	sw.SetFlagger(&fakeFlagger{err: errors.New("action queue is unavailable")})

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.Failed != 2 {
		t.Errorf("Failed = %d, want 2: both artists must still be checked and persisted", c.Failed)
	}
	if c.Errored != 2 {
		t.Errorf("Errored = %d, want 2 (one per refused queue write)", c.Errored)
	}
	if _, ok := led.get("a-2"); !ok {
		t.Error("the second artist's verdict is missing: a failed flag aborted the pass")
	}
}

// TestCanceledFlagIsNotBlamedOnTheArtist is the flag-side half of the rule the
// ledger write and the artist load already follow: a condition on OUR side is
// never attributed to the artist being processed.
//
// RaiseMBIDValidationFailure runs a transaction on the sweep's own context, so
// a shutdown part-way through a pass fails it for EVERY remaining failed
// verdict at once. Without the cancellation branch that is one ERROR line per
// artist, each naming an artist that is perfectly fine, plus an Errored tally
// that contradicts what Counters.Errored documents about itself.
//
// The wrapped sentinel is deliberate, and matches the ledger test: a service
// wraps its errors, so an == comparison would compile, satisfy a bare-error
// test, and stop matching in production.
func TestCanceledFlagIsNotBlamedOnTheArtist(t *testing.T) {
	t.Parallel()

	rows := []artist.MBIDPath{{ArtistID: "a-1", MBID: "m1"}, {ArtistID: "a-2", MBID: "m2"}}
	byID := map[string]*artist.Artist{
		"a-1": sweepArtist("a-1", "/library/a-1"),
		"a-2": sweepArtist("a-2", "/library/a-2"),
	}

	flagErr := fmt.Errorf("raising the operator-review entry: %w", context.Canceled)
	// PRECONDITION: the fixture's error really is a WRAPPED cancellation, not
	// the bare sentinel, so an == comparison in the sweep could not satisfy it.
	if !errors.Is(flagErr, context.Canceled) || errors.Unwrap(flagErr) == nil {
		t.Fatalf("precondition: the flagger error must be a WRAPPED context.Canceled, got %v", flagErr)
	}

	rec := &recordingHandler{}
	led := newFakeLedger()
	// groups: nil against albums on disk is the FAILED shape, which is the only
	// outcome that reaches flag at all.
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: nil}
	sw := NewSweep(&fakePopulation{rows: rows}, &fakeArtists{byID: byID}, led,
		newTestResolver(mb, found("One")), Config{MaxPerPass: 10}, slog.New(rec))
	sw.SetFlagger(&fakeFlagger{err: flagErr})

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// PRECONDITION: both artists genuinely reached flag. Without a FAILED
	// verdict the assertions below would hold over a pass that never called the
	// flagger at all.
	if c.Failed != len(rows) {
		t.Fatalf("precondition: Failed = %d, want %d -- the flag path must actually have been reached (%+v)",
			c.Failed, len(rows), c)
	}

	if c.Errored != 0 {
		t.Errorf("Errored = %d, want 0: an abandoned queue write is our shutdown, never the artist's fault (%+v)", c.Errored, c)
	}
	if c.SkippedFlag != len(rows) {
		t.Errorf("SkippedFlag = %d, want %d: an abandoned entry must still be reported as something that did not land (%+v)",
			c.SkippedFlag, len(rows), c)
	}
	// And NOT in SkippedTransient, whose documented signature is "the sweep
	// could not reach MusicBrainz". These verdicts DID reach the ledger, so
	// counting them there would point an operator at an outage that never
	// happened.
	if c.SkippedTransient != 0 {
		t.Errorf("SkippedTransient = %d, want 0: the verdicts reached the ledger, so only the review entry was lost (%+v)",
			c.SkippedTransient, c)
	}
	if msgs := rec.messagesAtLevel(slog.LevelError); len(msgs) != 0 {
		t.Errorf("a canceled operator-review write logged at ERROR: %v", msgs)
	}
	// The ledger row is untouched either way: the verdict is durable before the
	// flag is attempted, which is why losing the queue entry is survivable.
	if _, ok := led.get("a-2"); !ok {
		t.Error("the verdict must still be persisted when its queue entry is abandoned")
	}
	// The counter has to be SURFACED, not merely incremented: a tally no
	// operator can read is dead weight, and the pass summary is the one line
	// they see.
	v, ok := rec.attr("mbid re-validation pass complete", "skipped_flag")
	if !ok {
		t.Fatal("the pass summary does not report skipped_flag; a counter nothing surfaces cannot be read")
	}
	if got, want := v, int64(len(rows)); got != any(want) {
		t.Errorf("summary skipped_flag = %v, want %d", got, want)
	}
}

// TestFlagMessageReadsTheMethodNotTheDetailProse pins the zero-catalogue
// wording to Result.ZeroRemoteCatalogue rather than to a phrase in
// Validation.Detail.
//
// TestFlagMessageNamesTheZeroCatalogueCase drives the whole sweep, and the
// resolver's zero-catalogue branch happens to write "no releases" into Detail
// today -- so a flagMessage that matched that prose instead of calling the
// method would pass it. This case hands flagMessage a Result whose Detail
// contains no such phrase, so the only way to produce the zero-catalogue
// wording is to have read the method. Reword the resolver's Detail in a future
// change and this test still holds; swap the method for a prose match and it
// fails.
func TestFlagMessageReadsTheMethodNotTheDetailProse(t *testing.T) {
	t.Parallel()

	res := Result{
		Validation: artist.MBIDValidation{
			ArtistID:     "a-1",
			MBID:         "11111111-2222-3333-4444-555555555555",
			Outcome:      artist.MBIDOutcomeFailed,
			Reason:       artist.MBIDReasonCatalogueMismatch,
			Detail:       "nothing on the remote side lined up with what is on disk",
			ResolvedName: "Someone Else",
		},
		LocalEvidence:           artist.EvidenceFound,
		RemoteReleaseGroupCount: 0,
		LocalAlbumCount:         3,
	}

	// PRECONDITION: this really is the zero-catalogue case by the METHOD's
	// judgment, and its Detail carries none of the prose the generic wording
	// would otherwise be distinguished by. Both halves are load-bearing: the
	// first makes the assertion meaningful, the second is what makes a prose
	// match unable to satisfy it.
	if !res.ZeroRemoteCatalogue() {
		t.Fatalf("precondition: the fixture must be the zero-remote-catalogue case, got %+v", res)
	}
	for _, phrase := range []string{"no releases", "release", "catalog"} {
		if strings.Contains(strings.ToLower(res.Validation.Detail), phrase) {
			t.Fatalf("precondition: Detail %q contains %q, so a prose match could still pass this test",
				res.Validation.Detail, phrase)
		}
	}

	sw := &Sweep{}
	msg := sw.flagMessage(res)
	if !strings.Contains(msg, "no releases at all") {
		t.Errorf("flagMessage = %q, want the zero-catalogue wording: it must read ZeroRemoteCatalogue(), not Detail prose", msg)
	}
	if !strings.Contains(msg, "Someone Else") || !strings.Contains(msg, "3 album") {
		t.Errorf("flagMessage = %q, want it to name the resolved artist and the local album count", msg)
	}
	if strings.Contains(msg, res.Validation.Detail) {
		t.Errorf("flagMessage = %q must not fall through to the generic detail wording", msg)
	}
}

// TestSetFlaggerIsSafeWhileAPassIsRunning pins the concurrency contract on the
// one field written after construction.
//
// SetFlagger exists BECAUSE the rule service is built late in main.go's wiring
// order, so the shape it invites is a wiring author calling it after the
// scheduler goroutine has already started a pass. That is a write racing a read
// in flag, it is reachable through the documented wiring, and it only manifests
// once a FAILED verdict reaches flag -- so a probe over a healthy library would
// never surface it. Run under -race, this test is the guard.
func TestSetFlaggerIsSafeWhileAPassIsRunning(t *testing.T) {
	t.Parallel()

	rows := make([]artist.MBIDPath, 0, 40)
	byID := map[string]*artist.Artist{}
	for i := range 40 {
		id := fmt.Sprintf("a-%02d", i)
		rows = append(rows, artist.MBIDPath{ArtistID: id, MBID: "m"})
		byID[id] = sweepArtist(id, "/library/"+id)
	}

	// FAILED verdicts throughout, so every row reaches flag and the racing read
	// actually happens. A validated fixture would exercise nothing.
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: nil}
	sw := newTestSweep(t, &fakePopulation{rows: rows}, &fakeArtists{byID: byID}, newFakeLedger(),
		newTestResolver(mb, found("One")), Config{MaxPerPass: len(rows)})

	flag := &fakeFlagger{}
	type runResult struct {
		c   Counters
		err error
	}
	// The error travels back over the channel rather than becoming a panic in
	// this goroutine: a panic off the test goroutine takes down the whole test
	// binary, so one failure here would hide every other test's result. t.Fatalf
	// is equally unavailable off the test goroutine, so the assertion happens
	// below, where it belongs.
	done := make(chan runResult, 1)
	go func() {
		c, err := sw.Run(context.Background())
		done <- runResult{c: c, err: err}
	}()
	for range 40 {
		sw.SetFlagger(flag)
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("Run: %v", got.err)
	}
	c := got.c
	// PRECONDITION: the pass really did reach flag. Without a failed verdict the
	// racing read never happens and -race has nothing to observe.
	if c.Failed != len(rows) {
		t.Fatalf("precondition: Failed = %d, want %d -- the flag path must be exercised (%+v)", c.Failed, len(rows), c)
	}
}

// TestSetFlaggerRejectsATypedNilPointer covers the trap in the wiring this
// setter is built for.
//
// main.go builds the rule service conditionally, so a `var svc *rule.Service`
// that stayed nil is passed as a NON-nil interface wrapping a nil pointer. The
// obvious `f != nil` check is TRUE for that value, so it would be stored, and
// flag would call a method on a nil receiver at the first failed verdict --
// panicking in a background goroutine hours after the wiring ran, rather than
// taking the supported no-flagger path.
func TestSetFlaggerRejectsATypedNilPointer(t *testing.T) {
	t.Parallel()

	var typedNil *fakeFlagger
	var asInterface Flagger = typedNil
	// PRECONDITION: this value really is the trap -- nil as a POINTER while the
	// interface box around it is not nil. Asserted through reflect rather than
	// `asInterface == nil`, which is the very comparison that returns false
	// here: writing it out would be a comparison staticcheck can prove is never
	// true, and it would read as if it were checking something.
	box := reflect.ValueOf(asInterface)
	if !box.IsValid() {
		t.Fatal("precondition: the fixture is a nil INTERFACE, not the typed-nil trap this test is about")
	}
	if box.Kind() != reflect.Pointer || !box.IsNil() {
		t.Fatalf("precondition: the fixture must wrap a nil pointer, got kind %v", box.Kind())
	}

	const id = "artist-1"
	led := newFakeLedger()
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: nil}
	sw := newTestSweep(t, &fakePopulation{rows: []artist.MBIDPath{{ArtistID: id, MBID: "m"}}},
		&fakeArtists{byID: map[string]*artist.Artist{id: sweepArtist(id, "/library/Example")}},
		led, newTestResolver(mb, found("One")), Config{MaxPerPass: 10})
	sw.SetFlagger(asInterface)

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.Failed != 1 {
		t.Fatalf("precondition: Failed = %d, want 1 -- the flag path must be reached (%+v)", c.Failed, c)
	}
	if _, ok := led.get(id); !ok {
		t.Error("the verdict must still be persisted")
	}
}

// TestSweepWithoutAFlaggerStillWritesTheLedger asserts the flagger is genuinely
// optional, so a build with no rule service wired still records what it checked
// rather than panicking on a nil interface.
func TestSweepWithoutAFlaggerStillWritesTheLedger(t *testing.T) {
	t.Parallel()

	const id = "artist-1"
	led := newFakeLedger()
	mb := &fakeMB{meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: nil}
	sw := newTestSweep(t, &fakePopulation{rows: []artist.MBIDPath{{ArtistID: id, MBID: "m"}}},
		&fakeArtists{byID: map[string]*artist.Artist{id: sweepArtist(id, "/library/Example")}},
		led, newTestResolver(mb, found("One")), Config{MaxPerPass: 10})
	// No SetFlagger call at all.

	c, err := sw.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.Failed != 1 || c.Errored != 0 {
		t.Fatalf("counters = %+v, want 1 failed and 0 errored", c)
	}
	if _, ok := led.get(id); !ok {
		t.Error("the verdict must still be persisted without a flagger")
	}

	// A nil SetFlagger must not disconnect a working one either.
	flag := &fakeFlagger{}
	sw.SetFlagger(flag)
	sw.SetFlagger(nil)
	if _, err := sw.Run(t.Context()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if flag.count() != 1 {
		t.Errorf("a nil SetFlagger disconnected the working flagger: raised %d", flag.count())
	}
}
