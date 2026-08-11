// Behavioral tests for startMBIDRevalidateSweep, the #2810 wiring point
// itself. Before this file, startMBIDRevalidateSweep had 0.0% coverage --
// resolveMBIDCheckClient and resolveMBIDRevalidateSchedule (the two helpers
// it calls) were fully covered, but the 62-line method that actually decides
// whether the sweep runs, and constructs it when it does, had none.
//
// These tests exercise every early-return branch (disabled by default,
// unregistered provider, nil ledger) plus the happy path, using a real
// artist.Service backed by an in-memory-equivalent SQLite (openTestDB) so
// MBIDValidations() returns the production ledger the same way it does at
// boot.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
)

// syncBuffer is a mutex-guarded io.Writer/String() pair. Plain
// strings.Builder is not safe for concurrent use, and these tests must read
// the log while the sweep's own goroutine (launched by startMBIDRevalidateSweep,
// via mbidcheck.Sweep.Start) may still be writing to it -- an unsynchronized
// buffer here would be a genuine data race in the TEST, not the code under
// test, and -race correctly flags exactly that.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newMBIDSweepTestApp builds an Application wired the way buildServices
// would by boot time, for exactly the fields startMBIDRevalidateSweep reads:
// providerRegistry, artistService, ruleService. db is a fresh migrated
// SQLite so getDBBoolSetting/getDBIntSetting and artistService's ledger all
// behave as they do in production.
func newMBIDSweepTestApp(t *testing.T) (*Application, *syncBuffer) {
	t.Helper()
	db := openTestDB(t)
	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	app := &Application{
		db:               db,
		logger:           logger,
		artistService:    artist.NewService(db),
		ruleService:      rule.NewService(db).WithLogger(logger),
		providerRegistry: provider.NewRegistry(),
	}
	return app, logBuf
}

// TestStartMBIDRevalidateSweep_DisabledByDefault kills mutation M5 (flip the
// enabled default): with no mbid_revalidate.enabled row ever saved -- the
// state of every existing install the first time it boots this code -- the
// sweep must NOT start. Confirmed by the disabled log line and, more
// strongly, by never seeing the "sweep started" line mbidcheck.Sweep.Start
// logs on entry.
func TestStartMBIDRevalidateSweep_DisabledByDefault(t *testing.T) {
	app, logBuf := newMBIDSweepTestApp(t)
	app.providerRegistry.Register(fakeMBIDCheckProvider{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app.startMBIDRevalidateSweep(ctx, app.db, app.logger)

	// startMBIDRevalidateSweep either returns having logged "disabled" (the
	// case under test) or launches a goroutine; give any goroutine a moment
	// to log so a wrongly-enabled sweep would be caught here too.
	time.Sleep(50 * time.Millisecond)

	logs := logBuf.String()
	if !strings.Contains(logs, "mbid re-validation sweep disabled") {
		t.Fatalf("expected the disabled log line with no mbid_revalidate.enabled row saved, got logs:\n%s", logs)
	}
	if strings.Contains(logs, "mbid re-validation sweep started") {
		t.Fatalf("sweep started with no mbid_revalidate.enabled row saved -- "+
			"the default flipped from disabled to enabled, got logs:\n%s", logs)
	}
}

// TestStartMBIDRevalidateSweep_ExplicitlyEnabled_NoProvider covers the
// resolveMBIDCheckClient nil branch: mbid_revalidate.enabled=true but no
// musicbrainz provider registered. The sweep must not start, and must say
// why.
func TestStartMBIDRevalidateSweep_ExplicitlyEnabled_NoProvider(t *testing.T) {
	app, logBuf := newMBIDSweepTestApp(t)
	setDBSetting(t, app.db, "mbid_revalidate.enabled", "true")
	// providerRegistry stays empty: no musicbrainz provider registered.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app.startMBIDRevalidateSweep(ctx, app.db, app.logger)
	time.Sleep(50 * time.Millisecond)

	logs := logBuf.String()
	if !strings.Contains(logs, "musicbrainz provider not registered") {
		t.Fatalf("expected the not-registered log line with no musicbrainz provider, got logs:\n%s", logs)
	}
	if strings.Contains(logs, "mbid re-validation sweep started") {
		t.Fatalf("sweep started with no musicbrainz provider registered, got logs:\n%s", logs)
	}
}

// TestStartMBIDRevalidateSweep_ExplicitlyEnabled_NilLedger covers the
// defense-in-depth nil-ledger branch. artist.NewService(db) always wires a
// ledger in production, so this test builds an artistService with
// NewServiceWithRepos (which does NOT call SetMBIDValidationRepository) to
// reach the branch the same way TestResolveMBIDCheckClient_WrongShape
// reaches its own nil case: by constructing the exact precondition rather
// than mocking the method away.
func TestStartMBIDRevalidateSweep_ExplicitlyEnabled_NilLedger(t *testing.T) {
	app, logBuf := newMBIDSweepTestApp(t)
	setDBSetting(t, app.db, "mbid_revalidate.enabled", "true")
	app.providerRegistry.Register(fakeMBIDCheckProvider{})

	repos, providers, members, aliases, images, platformIDs, completeness := artist.NewDefaultRepos(app.db)
	app.artistService = artist.NewServiceWithRepos(repos, providers, members, aliases, images, platformIDs, completeness)
	if app.artistService.MBIDValidations() != nil {
		t.Fatal("test precondition failed: artistService built via NewServiceWithRepos already has a ledger")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app.startMBIDRevalidateSweep(ctx, app.db, app.logger)
	time.Sleep(50 * time.Millisecond)

	logs := logBuf.String()
	if !strings.Contains(logs, "no MBID validation ledger attached") {
		t.Fatalf("expected the nil-ledger log line with no ledger wired, got logs:\n%s", logs)
	}
	if strings.Contains(logs, "mbid re-validation sweep started") {
		t.Fatalf("sweep started with no ledger wired, got logs:\n%s", logs)
	}
}

// TestStartMBIDRevalidateSweep_ExplicitlyEnabled_Starts is the happy path
// and kills mutation M4 (moving SetFlagger to after `go mbidSweep.Start(ctx)`)
// indirectly by proving the sweep reaches Start() at all when every
// precondition holds -- combined with the static ordering check in
// TestMBIDRevalidateSweepSetsFlaggerBeforeStart, which pins the actual M4
// ordering property. Also confirms the sweep started log line carries the
// resolved interval, which is resolveMBIDRevalidateSchedule's output
// actually reaching mbidcheck.Config.
func TestStartMBIDRevalidateSweep_ExplicitlyEnabled_Starts(t *testing.T) {
	app, logBuf := newMBIDSweepTestApp(t)
	setDBSetting(t, app.db, "mbid_revalidate.enabled", "true")
	setDBSetting(t, app.db, "mbid_revalidate.interval_hours", "6")
	app.providerRegistry.Register(fakeMBIDCheckProvider{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app.startMBIDRevalidateSweep(ctx, app.db, app.logger)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logBuf.String(), "mbid re-validation sweep started") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "mbid re-validation sweep started") {
		t.Fatalf("sweep never logged its startup line within 2s with every precondition satisfied, got logs:\n%s", logs)
	}
	if !strings.Contains(logs, "interval=6h0m0s") {
		t.Fatalf("expected the resolved 6h interval (mbid_revalidate.interval_hours=6) in the startup log, got logs:\n%s", logs)
	}
}

// setDBSetting writes a settings row directly, the same table
// getDBBoolSetting and getDBIntSetting read from.
func setDBSetting(t *testing.T, db *sql.DB, key, value string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO settings (key, value, updated_at) VALUES (?, ?, '2024-01-01T00:00:00Z')`,
		key, value); err != nil {
		t.Fatalf("inserting setting %q=%q: %v", key, value, err)
	}
}
