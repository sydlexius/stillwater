package mbidcheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// mu guards calls: Start drives passes on its own goroutine, so a test
	// reading the count from the test goroutine would otherwise race.
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

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel from inside the first artist's provider call, so the pass is
	// genuinely mid-slice when the context goes down. This WIRES the condition
	// rather than pre-canceling, which any implementation would notice.
	mb := &cancelingMB{cancel: cancel, meta: &provider.ArtistMetadata{Name: "Example Band"}, groups: rgs("One")}
	led := newFakeLedger()
	sw := newTestSweep(t, &fakePopulation{rows: rows}, &fakeArtists{byID: byID}, led,
		newTestResolver(mb, found("One")), Config{MaxPerPass: 10})

	c, err := sw.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// PRECONDITION: the fixture must actually have had more artists left to do.
	if len(rows) <= 1 {
		t.Fatal("precondition: need more than one artist to observe an early stop")
	}
	if mb.artistCalls != 1 {
		t.Errorf("provider was called %d time(s) after cancellation; want exactly 1", mb.artistCalls)
	}
	if c.Checked > 1 {
		t.Errorf("checked = %d, want at most 1: the pass ignored ctx.Done()", c.Checked)
	}
}

// cancelingMB cancels the sweep's context during its first GetArtist call.
type cancelingMB struct {
	cancel      context.CancelFunc
	meta        *provider.ArtistMetadata
	groups      []provider.ReleaseGroupInfo
	artistCalls int
}

func (c *cancelingMB) GetArtist(context.Context, string) (*provider.ArtistMetadata, error) {
	c.artistCalls++
	c.cancel()
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
