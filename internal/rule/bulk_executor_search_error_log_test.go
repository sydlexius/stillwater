package rule

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/provider"
	_ "modernc.org/sqlite"
)

// A provider search that FAILED and a search that genuinely found nothing lead
// to the same operator-visible outcome in the bulk image job -- the artist is
// skipped -- and that is deliberate. What they must not share is the TRACE: with
// the error discarded, a provider outage across a library-wide sweep is
// indistinguishable from "none of these artists exist upstream", and there is
// nothing in the log to tell an operator which one happened.
//
// These two tests pin that a failed search leaves a warn record naming the
// error, and that the returned status and message are byte-for-byte what they
// were -- this is diagnostics only and must not move any decision.
//
// Env-independent: the failure is induced by closing the settings database the
// orchestrator reads its available-provider list from. No network.

// searchFailureExecutor builds a BulkExecutor whose orchestrator's Search is
// guaranteed to fail, plus the buffer its logger writes to.
func searchFailureExecutor(t *testing.T) (*BulkExecutor, *bytes.Buffer) {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening settings db: %v", err)
	}
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	settings := provider.NewSettingsService(db, enc)
	registry := provider.NewRegistry()
	registry.Register(&mockSearchProvider{name: provider.NameMusicBrainz})

	// Closing the database makes availableProviders (and therefore Search)
	// return a real error rather than an empty result set -- the discriminator
	// this whole test rests on.
	if err := db.Close(); err != nil {
		t.Fatalf("closing settings db: %v", err)
	}

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return &BulkExecutor{
		orchestrator: provider.NewOrchestrator(registry, settings, logger, nil),
		logger:       logger,
	}, buf
}

// TestSelfHealMBID_LogsProviderSearchFailure is the finding itself: a failed
// search must leave a record naming the error.
func TestSelfHealMBID_LogsProviderSearchFailure(t *testing.T) {
	e, buf := searchFailureExecutor(t)
	a := &artist.Artist{Name: gateArtistName}

	// PRECONDITION: the orchestrator really does FAIL here rather than return
	// an empty result set. Without this the test could be exercising the
	// len(results) == 0 branch, which legitimately logs nothing, and the
	// assertions below would be measuring the wrong branch.
	probeResults, probeErr := e.orchestrator.Search(context.Background(), a.Name)
	if probeErr == nil {
		t.Fatalf("fixture precondition: orchestrator.Search must return an error, got nil (results=%d)", len(probeResults))
	}
	buf.Reset() // the probe itself may have logged; measure only the call under test.

	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	// The outcome is UNCHANGED. This is the half of the finding that must not
	// regress: the fix is diagnostic, so both the status and the exact operator
	// message stay as they were.
	if status != BulkItemSkipped {
		t.Errorf("status = %q, want %q", status, BulkItemSkipped)
	}
	if msg != "no MBID and provider search found nothing" {
		t.Errorf("message = %q, want it unchanged at %q", msg, "no MBID and provider search found nothing")
	}

	logged := buf.String()
	if !strings.Contains(logged, "provider search for a missing MusicBrainz ID failed") {
		t.Fatalf("no record of the failed provider search in the log; a broken provider must not be "+
			"silently indistinguishable from an artist nobody has heard of. Log was:\n%s", logged)
	}
	// The ERROR ITSELF, not merely the fact that something failed -- an operator
	// cannot act on "a search failed" without knowing how.
	if !strings.Contains(logged, probeErr.Error()) {
		t.Errorf("the log record does not name the underlying error %q. Log was:\n%s", probeErr.Error(), logged)
	}
	if !strings.Contains(logged, a.Name) {
		t.Errorf("the log record does not name the artist %q. Log was:\n%s", a.Name, logged)
	}
	if !strings.Contains(logged, `"level":"WARN"`) {
		t.Errorf("the failed-search record is not at WARN level (an upstream fault, unlike the "+
			"Info-level deliberate decline). Log was:\n%s", logged)
	}
}

// TestSelfHealMBID_EmptySearchResultsLogNoFailure is the negative control: an
// artist that genuinely matched nothing is not a fault and must not manufacture
// a warning. Without this, a fix that logged unconditionally would pass the test
// above while burying real provider outages in noise.
func TestSelfHealMBID_EmptySearchResultsLogNoFailure(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening settings db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatalf("creating settings table: %v", err)
	}
	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	registry := provider.NewRegistry()
	// A working provider that simply returns no hits.
	registry.Register(&mockSearchProvider{name: provider.NameMusicBrainz, results: nil})

	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := &BulkExecutor{
		orchestrator: provider.NewOrchestrator(registry, provider.NewSettingsService(db, enc), logger, nil),
		logger:       logger,
	}
	a := &artist.Artist{Name: gateArtistName}

	// PRECONDITION: the search SUCCEEDS and returns nothing -- the other branch.
	results, searchErr := e.orchestrator.Search(context.Background(), a.Name)
	if searchErr != nil {
		t.Fatalf("fixture precondition: orchestrator.Search must succeed, got %v", searchErr)
	}
	if len(results) != 0 {
		t.Fatalf("fixture precondition: orchestrator.Search must return no results, got %d", len(results))
	}
	buf.Reset()

	status, msg := e.selfHealMBID(context.Background(), a, BulkModeYOLO)

	if status != BulkItemSkipped {
		t.Errorf("status = %q, want %q", status, BulkItemSkipped)
	}
	if msg != "no MBID and provider search found nothing" {
		t.Errorf("message = %q, want %q", msg, "no MBID and provider search found nothing")
	}
	if logged := buf.String(); strings.Contains(logged, "provider search for a missing MusicBrainz ID failed") {
		t.Errorf("a successful search that found nothing logged a FAILURE record; that noise is what "+
			"would bury a real outage. Log was:\n%s", logged)
	}
}
