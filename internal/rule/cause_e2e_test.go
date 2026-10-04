package rule

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/logging/logtest"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/scraper"
)

// These tests drive the REAL path from each cause-setting site in this package
// down to provider.FetchProviderResult and read the attribute it emits (#2784).
// Nothing between the entry point and the provider is stubbed: the pipeline,
// the fixer, the coalescing EvaluationContext (which detaches the context with
// WithoutCancel -- the hop most likely to drop a context value), the
// orchestrator and the scraper executor are all the production types. Only the provider adapter at the
// very bottom is a fake, and it answers "not found" so the shared fetch point
// emits its existing not-found line.

const causeNotFoundMsg = "provider has no data for artist"

// causeNotFoundProvider is registered as AudioDB: no API key needed, and it is
// in the default biography priority chain.
type causeNotFoundProvider struct{}

func (causeNotFoundProvider) Name() provider.ProviderName { return provider.NameAudioDB }
func (causeNotFoundProvider) RequiresAuth() bool          { return false }
func (causeNotFoundProvider) SearchArtist(context.Context, string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}

func (causeNotFoundProvider) GetArtist(_ context.Context, id string) (*provider.ArtistMetadata, error) {
	return nil, &provider.ErrNotFound{Provider: provider.NameAudioDB, ID: id}
}

func (causeNotFoundProvider) GetImages(context.Context, string) ([]provider.ImageResult, error) {
	return nil, nil
}

type causeFixture struct {
	db        *sql.DB
	artistSvc *artist.Service
	ruleSvc   *Service
	orch      *provider.Orchestrator
	pipeline  *Pipeline
	artist    *artist.Artist
	logs      *logtest.Buffer
}

// newCauseFixture wires a pipeline whose only enabled rule is bio_exists (in
// auto mode), whose only fixer is the real MetadataFixer, and one artist that
// violates it.
func newCauseFixture(t *testing.T) *causeFixture {
	t.Helper()
	ctx := context.Background()
	db := setupTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	disableAllRulesExcept(t, db, RuleBioExists)
	if _, err := db.ExecContext(ctx,
		`UPDATE rules SET automation_mode = ? WHERE id = ?`, AutomationModeAuto, RuleBioExists); err != nil {
		t.Fatalf("setting automation_mode=auto: %v", err)
	}

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	registry := provider.NewRegistry()
	registry.Register(causeNotFoundProvider{})
	logger, logs := logtest.NewJSONLogger()
	settings := provider.NewSettingsService(db, enc)
	orch := provider.NewOrchestrator(registry, settings, logger, nil)
	// Production wiring (cmd/stillwater/main.go): FetchMetadata delegates to
	// the scraper executor, which is what reaches FetchProviderResult. Without
	// it these tests would run the orchestrator's legacy path instead.
	scraperSvc := scraper.NewService(db, logger)
	if err := scraperSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding scraper config: %v", err)
	}
	orch.SetExecutor(scraper.NewExecutor(scraperSvc, registry, settings, logger, nil))

	a := &artist.Artist{Name: "Cause Subject", SortName: "Cause Subject", Path: t.TempDir(), Biography: "short"}
	if err := artistSvc.Create(ctx, a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	if err := artistSvc.MarkDirty(ctx, a.ID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}

	engine := NewEngine(ruleSvc, db, nil, nil, testLogger())
	pipeline := NewPipeline(engine, artistSvc, ruleSvc,
		[]Fixer{NewMetadataFixer(orch, testLogger())}, nil, testLogger())
	// Production wiring: with an orchestrator installed, fixer fetches go
	// through the coalescer and its detached context.
	pipeline.SetOrchestrator(orch)

	return &causeFixture{db: db, artistSvc: artistSvc, ruleSvc: ruleSvc, orch: orch, pipeline: pipeline, artist: a, logs: logs}
}

// assertFetchCause requires at least one not-found record from the shared
// fetch point and that EVERY one carries want.
func (f *causeFixture) assertFetchCause(t *testing.T, want string) {
	t.Helper()
	found := 0
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var rec struct {
			Msg   string  `json:"msg"`
			Cause *string `json:"cause"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Msg != causeNotFoundMsg {
			continue
		}
		found++
		got := "<attribute missing>"
		if rec.Cause != nil {
			got = *rec.Cause
		}
		if got != want {
			t.Errorf("provider fetch logged cause %q, want %q", got, want)
		}
	}
	// Precondition: the run reached the provider. Without it a path that never
	// fetched would pass with nothing checked.
	if found == 0 {
		t.Fatalf("no %q record: the path never reached the provider fetch point. Log was:\n%s",
			causeNotFoundMsg, f.logs.String())
	}
}

func TestCause_RuleFixReachesProviderFetch(t *testing.T) {
	f := newCauseFixture(t)
	ctx := context.Background()
	rv := &RuleViolation{
		RuleID: RuleBioExists, ArtistID: f.artist.ID, ArtistName: f.artist.Name,
		Severity: "error", Message: "biography needs work", Fixable: true, Status: ViolationStatusOpen,
	}
	if err := f.ruleSvc.UpsertViolation(ctx, rv); err != nil {
		t.Fatalf("upserting violation: %v", err)
	}

	if _, err := f.pipeline.FixViolation(ctx, rv.ID); err != nil {
		t.Fatalf("FixViolation: %v", err)
	}
	f.assertFetchCause(t, "rule:"+RuleBioExists)
}

// The scheduler knows the trigger, the fixer knows the rule; the record must
// carry both.
func TestCause_ScheduledRunKeepsClassAndAddsRule(t *testing.T) {
	f := newCauseFixture(t)
	sched := NewScheduler(f.pipeline, f.ruleSvc, nil, testLogger())

	sched.runEnabledRules(context.Background())

	f.assertFetchCause(t, "scheduled:rule:"+RuleBioExists)
}

func TestCause_BulkJobReachesProviderFetch(t *testing.T) {
	f := newCauseFixture(t)
	bulkSvc := NewBulkService(f.db)
	job, err := bulkSvc.CreateJob(context.Background(), BulkTypeFetchMetadata, BulkModeYOLO, 0)
	if err != nil {
		t.Fatalf("creating bulk job: %v", err)
	}
	job.ArtistIDs = []string{f.artist.ID}
	e := &BulkExecutor{bulkService: bulkSvc, artistService: f.artistSvc, orchestrator: f.orch, logger: testLogger()}

	e.run(context.Background(), job)

	f.assertFetchCause(t, "bulk:"+BulkTypeFetchMetadata)
}
