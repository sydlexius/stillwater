package rule

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/nfo"
	"github.com/sydlexius/stillwater/internal/provider"
)

// These tests cover the CHECKER phase and the health subscriber (#2784). The
// fixer-phase tests live in cause_e2e_test.go, whose fixture is reused here
// read-only. As there, only the provider adapter at the bottom is a fake; the
// engine, orchestrator, scraper executor and HealthSubscriber are the real
// production types, and each assertion reads the "cause" attribute the shared
// fetch point actually logged.

// causeRecords returns the cause value of every not-found line the shared
// fetch point logged. A missing attribute is reported as "<attribute missing>".
func causeRecords(f *causeFixture) []string {
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var rec struct {
			Msg   string  `json:"msg"`
			Cause *string `json:"cause"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Msg != causeNotFoundMsg {
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

// langPrefCtx carries an English-only language preference. name_language_pref
// skips silently without one, so every test below needs it.
func langPrefCtx(ctx context.Context) context.Context {
	return provider.WithMetadataLanguages(ctx, []string{"en"})
}

// newLangPrefFixture builds on newCauseFixture but leaves only the given
// rules enabled and wires a real engine to the real orchestrator. The seeded
// artist makes name_language_pref FETCH: a Cyrillic name fails an English
// preference, and it has a MusicBrainz ID (without one the alias lookup
// returns before any provider call). The NFO lets discography_populated reach
// its release-group fetch for the same artist.
func newLangPrefFixture(t *testing.T, rules ...string) (*causeFixture, *Engine, *artist.Artist) {
	t.Helper()
	f := newCauseFixture(t)
	disableAllRulesExcept(t, f.db, rules...)
	for _, id := range rules {
		r, err := f.ruleSvc.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("loading rule %s: %v", id, err)
		}
		r.Enabled = true
		if err := f.ruleSvc.Update(context.Background(), r); err != nil {
			t.Fatalf("enabling rule %s: %v", id, err)
		}
	}

	dir := t.TempDir()
	writeTestNFO(t, dir, &nfo.ArtistNFO{
		Name:   "Кириллица",
		Albums: []nfo.DiscographyAlbum{{Title: "A", MusicBrainzReleaseGroupID: "rg-a"}},
	})
	a := &artist.Artist{
		Name:          "Кириллица",
		SortName:      "Кириллица",
		Path:          dir,
		MusicBrainzID: "11111111-2222-3333-4444-555555555555",
	}
	if err := f.artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}

	engine := NewEngine(f.ruleSvc, f.db, nil, nil, testLogger())
	engine.SetMetadataProvider(f.orch)
	// Scheduler and pipeline paths run the pipeline's own engine.
	f.pipeline.engine.SetMetadataProvider(f.orch)
	return f, engine, a
}

func TestCause_CheckerPhaseNamesRule(t *testing.T) {
	f, engine, a := newLangPrefFixture(t, RuleNameLanguagePref)

	if _, err := engine.Evaluate(langPrefCtx(context.Background()), a); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	got := causeRecords(f)
	if len(got) == 0 {
		t.Fatalf("no %q record: the checker never reached the provider. Log:\n%s", causeNotFoundMsg, f.logs.String())
	}
	for _, c := range got {
		if c != "rule:"+RuleNameLanguagePref {
			t.Errorf("checker-phase fetch logged cause %q, want %q", c, "rule:"+RuleNameLanguagePref)
		}
	}
}

// The scheduler knows the trigger, the checker knows the rule; the record
// must carry both.
func TestCause_ScheduledCheckerKeepsClass(t *testing.T) {
	f, _, _ := newLangPrefFixture(t, RuleNameLanguagePref)
	sched := NewScheduler(f.pipeline, f.ruleSvc, nil, testLogger())
	sched.SetLangPrefProvider(func(context.Context) []string { return []string{"en"} })

	sched.runEnabledRules(context.Background())

	got := causeRecords(f)
	if len(got) == 0 {
		t.Fatalf("no %q record: the scheduled run never reached the provider. Log:\n%s", causeNotFoundMsg, f.logs.String())
	}
	for _, c := range got {
		if c != "scheduled:rule:"+RuleNameLanguagePref {
			t.Errorf("scheduled checker fetch logged cause %q, want %q", c, "scheduled:rule:"+RuleNameLanguagePref)
		}
	}
}

// causeRecordingFetcher is a release-group fetcher that notes the cause on the
// context it was handed, i.e. what the adapter boundary would see.
type causeRecordingFetcher struct {
	mu     sync.Mutex
	causes []string
}

func (c *causeRecordingFetcher) GetReleaseGroups(ctx context.Context, _ string) ([]provider.ReleaseGroupInfo, error) {
	c.mu.Lock()
	c.causes = append(c.causes, provider.CauseFromContext(ctx).String())
	c.mu.Unlock()
	// More groups than the NFO lists, so the coverage branch runs.
	return []provider.ReleaseGroupInfo{
		{Title: "A", PrimaryType: "Album"}, {Title: "B", PrimaryType: "Album"},
		{Title: "C", PrimaryType: "Album"}, {Title: "D", PrimaryType: "Album"},
	}, nil
}

// Two provider-backed rules run in one Evaluate. Each rule's provider call must
// name ITS OWN rule id: the release-group fetch names discography_populated and
// the metadata fetch names name_language_pref.
func TestCause_EachRuleGetsItsOwnDetail(t *testing.T) {
	f, engine, a := newLangPrefFixture(t, RuleNameLanguagePref, RuleDiscographyPopulated)
	rg := &causeRecordingFetcher{}
	engine.SetReleaseGroupFetcher(rg)

	if _, err := engine.Evaluate(langPrefCtx(context.Background()), a); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// Precondition: the discography checker reached the adapter boundary.
	if len(rg.causes) == 0 {
		t.Fatal("release-group fetcher was never called: the discography checker did not reach the provider")
	}
	for _, c := range rg.causes {
		if c != "rule:"+RuleDiscographyPopulated {
			t.Errorf("release-group fetch saw cause %q, want %q", c, "rule:"+RuleDiscographyPopulated)
		}
	}
	meta := causeRecords(f)
	if len(meta) == 0 {
		t.Fatalf("no %q record from the metadata fetch. Log:\n%s", causeNotFoundMsg, f.logs.String())
	}
	for _, c := range meta {
		if c != "rule:"+RuleNameLanguagePref {
			t.Errorf("metadata fetch logged cause %q, want %q", c, "rule:"+RuleNameLanguagePref)
		}
	}
}

// Bootstrap is synchronous (no 2s debounce) and runs the same evaluateArtist
// as the ticker. A health re-evaluation replaces any inherited cause, so the
// outer "user" below must not survive.
func TestCause_HealthSubscriberAttributesEvaluation(t *testing.T) {
	f, engine, a := newLangPrefFixture(t, RuleNameLanguagePref)
	ids, err := f.artistSvc.ListUnevaluatedIDs(context.Background())
	if err != nil {
		t.Fatalf("ListUnevaluatedIDs: %v", err)
	}
	// Precondition: the artist is one Bootstrap will evaluate.
	found := false
	for _, id := range ids {
		found = found || id == a.ID
	}
	if !found {
		t.Fatalf("artist %s is not unevaluated; Bootstrap would skip it", a.ID)
	}

	h := NewHealthSubscriber(engine, f.artistSvc, testLogger())
	ctx := provider.WithCause(langPrefCtx(context.Background()), provider.Cause{Class: provider.CauseClassUser, Detail: "x"})
	h.Bootstrap(ctx)

	got := causeRecords(f)
	if len(got) == 0 {
		t.Fatalf("no %q record: Bootstrap never reached the provider. Log:\n%s", causeNotFoundMsg, f.logs.String())
	}
	for _, c := range got {
		if c != "health:rule:"+RuleNameLanguagePref {
			t.Errorf("health evaluation logged cause %q, want %q", c, "health:rule:"+RuleNameLanguagePref)
		}
	}
}

// gatedMetadataProvider parks every caller inside FetchMetadata until `want`
// callers have arrived, then delegates to the real orchestrator. Parking
// guarantees the two evaluations overlap, which is the only way a cause
// stored in shared state could leak from one into the other.
type gatedMetadataProvider struct {
	inner   MetadataProvider
	want    int
	mu      sync.Mutex
	entered int
	gate    chan struct{}
}

func (g *gatedMetadataProvider) FetchMetadata(ctx context.Context, mbid, name string, ids map[provider.ProviderName]string) (*provider.FetchResult, error) {
	g.mu.Lock()
	g.entered++
	if g.entered == g.want {
		close(g.gate)
	}
	g.mu.Unlock()
	select {
	case <-g.gate:
	case <-time.After(10 * time.Second):
		return nil, context.DeadlineExceeded
	}
	return g.inner.FetchMetadata(ctx, mbid, name, ids)
}

// Two evaluations overlap on one engine, each parked inside the provider until
// both have arrived, and each logs its own "class:rule:id". The user side calls
// Engine.Evaluate under a user cause. The health side runs the real
// HealthSubscriber.evaluateArtist under an unrelated outer cause, so its
// replace-with-health is what produces the health class. Neither call carries an
// EvaluationContext, so this does NOT cover the fetch coalescer.
func TestCause_ConcurrentEvaluationsDoNotLeak(t *testing.T) {
	f, engine, a1 := newLangPrefFixture(t, RuleNameLanguagePref)
	gate := &gatedMetadataProvider{inner: f.orch, want: 2, gate: make(chan struct{})}
	engine.SetMetadataProvider(gate)

	a2 := &artist.Artist{
		Name: "Другой", SortName: "Другой", Path: t.TempDir(),
		MusicBrainzID: "99999999-2222-3333-4444-555555555555",
	}
	if err := f.artistSvc.Create(context.Background(), a2); err != nil {
		t.Fatalf("creating second artist: %v", err)
	}
	h := NewHealthSubscriber(engine, f.artistSvc, testLogger())

	userCtx := provider.WithCause(langPrefCtx(context.Background()), provider.Cause{Class: provider.CauseClassUser})
	// A non-health outer cause, so dropping the replace shows up as a wrong class.
	healthCtx := provider.WithCause(langPrefCtx(context.Background()), provider.Cause{Class: provider.CauseClassUser, Detail: "x"})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := engine.Evaluate(userCtx, a1); err != nil {
			t.Errorf("Evaluate: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		h.evaluateArtist(healthCtx, a2.ID)
	}()
	wg.Wait()

	// Precondition: both evaluations were parked together, so they overlapped.
	if gate.entered != 2 {
		t.Fatalf("only %d evaluation(s) reached the provider; the overlap was not exercised", gate.entered)
	}
	want := map[string]int{
		"user:rule:" + RuleNameLanguagePref:   1,
		"health:rule:" + RuleNameLanguagePref: 1,
	}
	got := map[string]int{}
	for _, c := range causeRecords(f) {
		got[c]++
	}
	if len(got) != len(want) {
		t.Errorf("records by cause = %v, want one each of %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("records by cause = %v, want one each of %v", got, want)
			break
		}
	}
}
