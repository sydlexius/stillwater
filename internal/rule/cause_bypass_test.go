package rule

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/logging/logtest"
	"github.com/sydlexius/stillwater/internal/provider"
)

// These tests cover the rule-package provider calls that bypass the shared
// fetch points (#2784, D1): the release-group fetcher is called directly, so
// the cause is only visible if the caller's own failure line prints it. Each
// test drives the real engine, scheduler or pipeline and reads the "cause"
// attribute off the log record actually emitted; only the release-group
// fetcher (the adapter boundary) is a fake, and it always fails.

const (
	discographyFetchFailedMsg = "discography_populated: release-group fetch failed"
	albumGateFetchFailedMsg   = "album-evidence gate: fetching the candidate's release groups failed, so its catalogue cannot contradict anything"
	languageFetchFailedMsg    = "name_language_pref: metadata fetch failed"
)

// failingReleaseGroupFetcher is the adapter-boundary fake: every call fails.
type failingReleaseGroupFetcher struct{}

func (failingReleaseGroupFetcher) GetReleaseGroups(context.Context, string) ([]provider.ReleaseGroupInfo, error) {
	return nil, errors.New("musicbrainz unavailable")
}

// failingMetadataProvider is the same idea for the metadata lookup.
type failingMetadataProvider struct{}

func (failingMetadataProvider) FetchMetadata(context.Context, string, string, map[provider.ProviderName]string) (*provider.FetchResult, error) {
	return nil, errors.New("provider unavailable")
}

// causeOnLine returns the "cause" value of every record whose message is msg.
// A missing attribute is reported as "<attribute missing>". It also fails the
// test on a duplicated top-level key, since a wrapping logger that already
// adds a cause would make the appended attribute ambiguous.
func causeOnLine(t *testing.T, logs *logtest.Buffer, msg string) []string {
	t.Helper()
	if dups := logtest.DuplicateKeys(logs.String()); len(dups) > 0 {
		t.Errorf("duplicate log keys: %v", dups)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec struct {
			Msg   string  `json:"msg"`
			Cause *string `json:"cause"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Msg != msg {
			continue
		}
		if rec.Cause == nil {
			got = append(got, "<attribute missing>")
			continue
		}
		got = append(got, *rec.Cause)
	}
	return got
}

// requireCause asserts at least one record with msg exists (the precondition:
// the path really reached the failing call) and that every one carries want.
func requireCause(t *testing.T, logs *logtest.Buffer, msg, want string) {
	t.Helper()
	got := causeOnLine(t, logs, msg)
	if len(got) == 0 {
		t.Fatalf("no %q record: the path never reached the failing provider call. Log:\n%s", msg, logs.String())
	}
	for _, c := range got {
		if c != want {
			t.Errorf("%q logged cause %q, want %q", msg, c, want)
		}
	}
}

// bypassEngineFixture builds a real engine (and a real pipeline around it, so
// the scheduler runs it) whose logs are captured. Only discography_populated
// is enabled; the seeded artist has an MBID, so its checker reaches the
// release-group fetch.
func bypassEngineFixture(t *testing.T, rules ...string) (*Engine, *Pipeline, *artist.Artist, *logtest.Buffer) {
	t.Helper()
	f, _, a := newLangPrefFixture(t, rules...)
	logger, logs := logtest.NewJSONLogger()
	engine := NewEngine(f.ruleSvc, f.db, nil, nil, logger)
	engine.SetReleaseGroupFetcher(failingReleaseGroupFetcher{})
	engine.SetMetadataProvider(failingMetadataProvider{})
	pipeline := NewPipeline(engine, f.artistSvc, f.ruleSvc, nil, nil, logger)
	return engine, pipeline, a, logs
}

// A bare Engine.Evaluate: the cause is the rule alone.
func TestCause_DiscographyFailureLineNamesRule_Evaluate(t *testing.T) {
	engine, _, a, logs := bypassEngineFixture(t, RuleDiscographyPopulated)

	if _, err := engine.Evaluate(context.Background(), a); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	requireCause(t, logs, discographyFetchFailedMsg, "rule:"+RuleDiscographyPopulated)
}

// Through the scheduler: the outer class must survive in front of the rule.
func TestCause_DiscographyFailureLineKeepsScheduledClass(t *testing.T) {
	_, pipeline, _, logs := bypassEngineFixture(t, RuleDiscographyPopulated)
	sched := NewScheduler(pipeline, pipeline.ruleService, nil, testLogger())

	sched.runEnabledRules(context.Background())

	requireCause(t, logs, discographyFetchFailedMsg, "scheduled:rule:"+RuleDiscographyPopulated)
}

// The name_language_pref checker has its own caller-side failure line for a
// failed metadata lookup; it must name the rule too.
func TestCause_LanguageFailureLineNamesRule(t *testing.T) {
	engine, _, a, logs := bypassEngineFixture(t, RuleNameLanguagePref)

	if _, err := engine.Evaluate(langPrefCtx(context.Background()), a); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	requireCause(t, logs, languageFetchFailedMsg, "rule:"+RuleNameLanguagePref)
}

// B2: a real nfo_has_mbid fix whose candidate reaches the album-evidence gate,
// with a failing release-group fetch. The gate's failure line must name the
// rule being fixed, and keep an outer user class in front of it.
func TestCause_AlbumGateFailureLineNamesFixedRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"bare", context.Background(), "rule:" + RuleNFOHasMBID},
		{"user outer", provider.WithCause(context.Background(), provider.Cause{Class: provider.CauseClassUser, Detail: "POST /x"}), "user:rule:" + RuleNFOHasMBID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCauseFixture(t)
			logger, logs := logtest.NewJSONLogger()
			results := gateSearchResults()
			assertNameGatesPass(t, results)

			// A full local album directory gives the gate real local evidence,
			// so it proceeds to fetch the candidate's catalogue (which fails).
			dir := gateArtistDir(t, gateAlbums)
			assertLocalEvidence(t, dir, artist.EvidenceFound, len(gateAlbums))

			fixer := &MetadataFixer{orchestrator: &stubMBIDSearchOrchestrator{results: results}, logger: logger}
			fixer.SetAlbumGate(artist.NewFilesystemAlbumSource(), failingReleaseGroupFetcher{})
			engine := NewEngine(f.ruleSvc, f.db, nil, nil, logger)
			pipeline := NewPipeline(engine, f.artistSvc, f.ruleSvc, []Fixer{fixer}, nil, logger)

			a := &artist.Artist{Name: gateArtistName, SortName: gateArtistName, Path: dir}
			if err := f.artistSvc.Create(context.Background(), a); err != nil {
				t.Fatalf("creating artist: %v", err)
			}
			rv := &RuleViolation{
				RuleID: RuleNFOHasMBID, ArtistID: a.ID, ArtistName: a.Name,
				Severity: "error", Message: "missing MBID", Fixable: true, Status: ViolationStatusOpen,
			}
			if err := f.ruleSvc.UpsertViolation(context.Background(), rv); err != nil {
				t.Fatalf("upserting violation: %v", err)
			}

			if _, err := pipeline.FixViolation(tc.ctx, rv.ID); err != nil {
				t.Fatalf("FixViolation: %v", err)
			}
			requireCause(t, logs, albumGateFetchFailedMsg, tc.want)
		})
	}
}

// countingFailingFetcher fails every call and counts how many reached it.
type countingFailingFetcher struct{ calls atomic.Int32 }

func (c *countingFailingFetcher) GetReleaseGroups(context.Context, string) ([]provider.ReleaseGroupInfo, error) {
	c.calls.Add(1)
	return nil, errors.New("musicbrainz unavailable")
}

// The production path coalesces: the discography checker and the album gate
// ask for the same artist's release groups and share ONE upstream fetch, whose
// failure is cached and served to the second asker. The shared fetch runs on
// the FIRST asker's detached context and the coalescer logs nothing, so a line
// that read its cause from there would name the wrong operation for the second
// caller. Each of the two real callers must log under its OWN cause. The
// discography checker runs first (it starts the fetch); the album gate's
// catalogue fetch runs second and is the cache-served one.
func TestCause_CoalescedSharedFailureEachCallerNamesItself(t *testing.T) {
	engine, _, a, logs := bypassEngineFixture(t, RuleDiscographyPopulated)
	fetcher := &countingFailingFetcher{}
	engine.SetReleaseGroupFetcher(fetcher)
	gateLogger, gateLogs := logtest.NewJSONLogger()
	gate := ruleAlbumGate{fetcher: fetcher, logger: gateLogger}

	ec := NewEvaluationContext(a, probeEvalProvider{}, nil)
	base := WithEvaluationContext(context.Background(), ec)

	engine.countMBReleaseGroups(provider.EnrichCause(base, provider.CauseClassRule, "rule_a"), a, RuleConfig{})
	gateCtx := provider.EnrichCause(base, provider.CauseClassBulk, "job_b")
	if _, known := gate.candidateTitles(gateCtx, "rule_b", a, a.MusicBrainzID); known {
		t.Fatalf("candidateTitles reported a determination for a failing fetch")
	}

	// Precondition: the fetch really was shared, not run twice.
	if got := fetcher.calls.Load(); got != 1 {
		t.Fatalf("fetcher called %d times, want 1: the two callers did not share one coalesced fetch", got)
	}
	requireCause(t, logs, discographyFetchFailedMsg, "rule:rule_a")
	requireCause(t, gateLogs, albumGateFetchFailedMsg, "bulk:job_b")
}

// probeEvalProvider satisfies the EvaluationContext's orchestrator slot; the
// release-group path never calls it.
type probeEvalProvider struct{ EvalProvider }
