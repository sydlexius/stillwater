package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/logging/logtest"
	"github.com/sydlexius/stillwater/internal/provider"
	"github.com/sydlexius/stillwater/internal/rule"
	"github.com/sydlexius/stillwater/internal/scanner"
	"github.com/sydlexius/stillwater/internal/scraper"
)

// These tests drive the REAL path from an inbound webhook down to the shared
// provider fetch point and read the "cause" attribute it logged (#2784). The
// real mux, the production wrapAuth (which puts a "user:<route>" cause on the
// request), the real handler, a real rule pipeline with the real fixer, the
// orchestrator and the scraper executor are all production types. Only the
// provider adapter at the very bottom is a fake, and it answers "not found" so
// the fetch point emits its existing not-found line.

const webhookNotFoundMsg = "provider has no data for artist"

// webhookFakeProvider is registered as AudioDB (no API key needed, and in the
// default biography chain). onGet, when set, runs before every GetArtist so a
// test can park the call.
type webhookFakeProvider struct {
	onGet func(ctx context.Context, id string)
}

func (webhookFakeProvider) Name() provider.ProviderName { return provider.NameAudioDB }
func (webhookFakeProvider) RequiresAuth() bool          { return false }
func (webhookFakeProvider) SearchArtist(context.Context, string) ([]provider.ArtistSearchResult, error) {
	return nil, nil
}

func (p webhookFakeProvider) GetArtist(ctx context.Context, id string) (*provider.ArtistMetadata, error) {
	if p.onGet != nil {
		p.onGet(ctx, id)
	}
	return nil, &provider.ErrNotFound{Provider: provider.NameAudioDB, ID: id}
}

func (webhookFakeProvider) GetImages(context.Context, string) ([]provider.ImageResult, error) {
	return nil, nil
}

type webhookCauseFixture struct {
	r      *Router
	mux    *http.ServeMux
	logs   *logtest.Buffer
	artist map[string]*artist.Artist // by MBID
}

// newWebhookCauseFixture wires a Router whose pipeline has bio_exists as its
// only enabled rule (auto mode, real MetadataFixer) and one artist per mbid,
// each violating it. scanSvc may be nil.
func newWebhookCauseFixture(t *testing.T, onGet func(context.Context, string), scanSvc *scanner.Service, mbids ...string) *webhookCauseFixture {
	t.Helper()
	ctx := context.Background()
	db := newTestDB(t)
	artistSvc := artist.NewService(db)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE rules SET enabled = 0 WHERE id != ?`, rule.RuleBioExists); err != nil {
		t.Fatalf("disabling other rules: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE rules SET automation_mode = ? WHERE id = ?`,
		rule.AutomationModeAuto, rule.RuleBioExists); err != nil {
		t.Fatalf("setting automation_mode=auto: %v", err)
	}

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}
	registry := provider.NewRegistry()
	registry.Register(webhookFakeProvider{onGet: onGet})
	logger, logs := logtest.NewJSONLogger()
	settings := provider.NewSettingsService(db, enc)
	orch := provider.NewOrchestrator(registry, settings, logger, nil)
	// Production wiring: FetchMetadata delegates to the scraper executor, which
	// is what reaches the shared fetch point.
	scraperSvc := scraper.NewService(db, logger)
	if err := scraperSvc.SeedDefaults(ctx); err != nil {
		t.Fatalf("seeding scraper config: %v", err)
	}
	orch.SetExecutor(scraper.NewExecutor(scraperSvc, registry, settings, logger, nil))

	f := &webhookCauseFixture{logs: logs, artist: map[string]*artist.Artist{}}
	for i, mbid := range mbids {
		name := "Cause Subject " + string(rune('A'+i))
		a := &artist.Artist{Name: name, SortName: name, Path: t.TempDir(), Biography: "short", MusicBrainzID: mbid}
		if err := artistSvc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}
		f.artist[mbid] = a
	}

	engine := rule.NewEngine(ruleSvc, db, nil, nil, logger)
	pipeline := rule.NewPipeline(engine, artistSvc, ruleSvc,
		[]rule.Fixer{rule.NewMetadataFixer(orch, logger)}, nil, logger)
	pipeline.SetOrchestrator(orch)

	// webhookShutdownCtx stays a plain Background context, as in production.
	wctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.r = &Router{
		logger:                logger,
		artistService:         artistSvc,
		pipeline:              pipeline,
		scannerService:        scanSvc,
		webhookShutdownCtx:    wctx,
		webhookShutdownCancel: cancel,
	}
	passthrough := func(next http.Handler) http.Handler { return next }
	f.mux = http.NewServeMux()
	f.mux.HandleFunc("POST /api/v1/webhooks/inbound/lidarr", wrapAuth(f.r.handleLidarrWebhook, passthrough))
	f.mux.HandleFunc("POST /api/v1/webhooks/inbound/emby", wrapAuth(f.r.handleEmbyWebhook, passthrough))
	f.mux.HandleFunc("POST /api/v1/webhooks/inbound/jellyfin", wrapAuth(f.r.handleJellyfinWebhook, passthrough))
	return f
}

// post sends body to the named source's route and requires a 200.
func (f *webhookCauseFixture) post(t *testing.T, source, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/webhooks/inbound/"+source, strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s webhook status = %d, want 200; body: %s", source, rec.Code, rec.Body.String())
	}
}

// drain waits for the webhook goroutines to finish. It waits on the WaitGroup
// directly rather than calling DrainWebhooks, because DrainWebhooks cancels
// the shutdown context first and would cut the work short.
func (f *webhookCauseFixture) drain(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { f.r.webhookWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("webhook goroutines did not finish")
	}
}

type webhookFetchRecord struct {
	ID    string
	Cause string
	Raw   string
}

// fetchRecords returns every not-found record from the shared fetch point.
func (f *webhookCauseFixture) fetchRecords() []webhookFetchRecord {
	var out []webhookFetchRecord
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var rec struct {
			Msg   string  `json:"msg"`
			ID    string  `json:"id"`
			Cause *string `json:"cause"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Msg != webhookNotFoundMsg {
			continue
		}
		c := "<attribute missing>"
		if rec.Cause != nil {
			c = *rec.Cause
		}
		out = append(out, webhookFetchRecord{ID: rec.ID, Cause: c, Raw: line})
	}
	return out
}

// artistEventBody builds an event for source that makes the handler evaluate
// the artist with the given MBID. marker, when set, fills every peer-controlled
// name field so payload safety can be checked.
func artistEventBody(source, mbid, marker string) string {
	switch source {
	case "lidarr":
		return `{"eventType":"Download","artist":{"name":"` + marker + `","mbId":"` + mbid + `"}}`
	case "emby":
		return `{"Event":"library.new","Item":{"Type":"MusicAlbum","Name":"` + marker +
			`","ProviderIds":{"MusicBrainzAlbumArtist":"` + mbid + `"},"ArtistItems":[{"Name":"` + marker + `"}]}}`
	default:
		return `{"NotificationType":"ItemAdded","ItemType":"MusicAlbum","Name":"` + marker +
			`","Artist":"` + marker + `","Provider_musicbrainzalbumartist":"` + mbid + `"}`
	}
}

// Work a webhook starts is attributed to the webhook source, and the user:
// cause that wrapAuth put on the request does not survive into it.
func TestCause_WebhookReplacesUserCause(t *testing.T) {
	for _, source := range []string{"lidarr", "emby", "jellyfin"} {
		t.Run(source, func(t *testing.T) {
			const mbid = "11111111-1111-4111-8111-111111111111"
			f := newWebhookCauseFixture(t, nil, nil, mbid)
			f.post(t, source, artistEventBody(source, mbid, "Peer Name"))
			f.drain(t)

			recs := f.fetchRecords()
			// Precondition: the event reached the provider fetch point.
			if len(recs) == 0 {
				t.Fatalf("no %q record: the webhook never reached the provider. Log was:\n%s", webhookNotFoundMsg, f.logs.String())
			}
			want := "webhook." + source + ":rule:" + rule.RuleBioExists
			for _, rec := range recs {
				if rec.Cause != want {
					t.Errorf("provider fetch logged cause %q, want %q", rec.Cause, want)
				}
				if strings.Contains(rec.Cause, "user:") {
					t.Errorf("webhook work inherited the request's user cause: %q", rec.Cause)
				}
			}
		})
	}
}

// The artist name and every other payload field is controlled by whatever sent
// the webhook, so none of it may reach a cause.
func TestCause_WebhookPayloadNeverEntersCause(t *testing.T) {
	const marker = "MARKER-7f3a9c-peer-controlled"
	for _, source := range []string{"lidarr", "emby", "jellyfin"} {
		t.Run(source, func(t *testing.T) {
			const mbid = "22222222-2222-4222-8222-222222222222"
			f := newWebhookCauseFixture(t, nil, nil, mbid)
			f.post(t, source, artistEventBody(source, mbid, marker))
			f.drain(t)

			recs := f.fetchRecords()
			if len(recs) == 0 {
				t.Fatalf("no %q record: the webhook never reached the provider. Log was:\n%s", webhookNotFoundMsg, f.logs.String())
			}
			for _, rec := range recs {
				if strings.Contains(rec.Cause, marker) {
					t.Errorf("payload marker leaked into cause %q", rec.Cause)
				}
			}
			// Precondition: the marker really was in the payload the handler
			// logged, so absence from the cause is meaningful.
			if source == "lidarr" && !strings.Contains(f.logs.String(), marker) {
				t.Errorf("marker never reached the handler's own log; the payload was not parsed as intended")
			}
		})
	}
}

// An Emby LibraryChanged event starts a scan, which must carry the webhook's
// cause (the scanner adds "scan").
func TestCause_WebhookScanLeg(t *testing.T) {
	logger, _ := logtest.NewJSONLogger()
	db := newTestDB(t)
	scanSvc := scanner.NewService(artist.NewService(db), nil, nil, logger, t.TempDir(), nil)
	got := make(chan provider.Cause, 1)
	scanSvc.SetPostScanHook(func(ctx context.Context) { got <- provider.CauseFromContext(ctx) })

	f := newWebhookCauseFixture(t, nil, scanSvc)
	f.post(t, "emby", `{"Event":"library.changed"}`)
	f.drain(t)

	select {
	case c := <-got:
		if c.String() != "webhook.emby:scan" {
			t.Errorf("scan cause = %q, want %q", c.String(), "webhook.emby:scan")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("post-scan hook never ran: the webhook did not start a scan")
	}
}

// Two webhooks in flight at once, both parked inside the provider until both
// have arrived, must each keep their own source.
func TestCause_ConcurrentWebhooksDoNotLeak(t *testing.T) {
	const mbidA, mbidB = "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"

	var entered sync.WaitGroup
	entered.Add(2)
	gate := make(chan struct{})
	go func() { entered.Wait(); close(gate) }()
	var once sync.Map // park each artist id only on its first call
	onGet := func(ctx context.Context, id string) {
		if _, loaded := once.LoadOrStore(id, true); loaded {
			return
		}
		entered.Done()
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	f := newWebhookCauseFixture(t, onGet, nil, mbidA, mbidB)
	f.post(t, "lidarr", artistEventBody("lidarr", mbidA, "x"))
	f.post(t, "jellyfin", artistEventBody("jellyfin", mbidB, "x"))
	f.drain(t)

	// Precondition: both calls really were parked together (the gate opened
	// because both arrived), and each artist produced a record.
	select {
	case <-gate:
	default:
		t.Fatal("the two webhooks never overlapped inside the provider")
	}
	// The "id" attribute of a fetch record is the artist's MBID, which tells the
	// two artists apart: each must carry the source of ITS webhook.
	byID := map[string]string{}
	for _, rec := range f.fetchRecords() {
		byID[rec.ID] = rec.Cause
	}
	want := map[string]string{
		mbidA: "webhook.lidarr:rule:" + rule.RuleBioExists,
		mbidB: "webhook.jellyfin:rule:" + rule.RuleBioExists,
	}
	for id, w := range want {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("no fetch record for artist %s. Log was:\n%s", id, f.logs.String())
		}
		if got != w {
			t.Errorf("artist %s logged cause %q, want %q", id, got, w)
		}
	}
}
