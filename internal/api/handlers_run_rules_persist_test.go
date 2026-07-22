package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
)

// persistFailPipeline is a PipelineRunner stub whose runs report that they
// could not be fully written. It exists to exercise the #2724 failure surface:
// before that fix, a run whose writes were lost still returned HTTP 200 with a
// violation count, so the operator was told it succeeded.
//
// Only the entry points the handlers under test call are meaningful; the rest
// satisfy the interface.
type persistFailPipeline struct {
	persistFailures int
	violationsFound int
}

func (p *persistFailPipeline) result() *rule.RunResult {
	return &rule.RunResult{
		ArtistsProcessed: 1,
		ViolationsFound:  p.violationsFound,
		PersistFailures:  p.persistFailures,
	}
}

func (p *persistFailPipeline) RunForArtist(context.Context, *artist.Artist) (*rule.RunResult, error) {
	return p.result(), nil
}

func (p *persistFailPipeline) RunImageRulesForArtist(context.Context, *artist.Artist) (*rule.RunResult, error) {
	return p.result(), nil
}
func (p *persistFailPipeline) RunRule(context.Context, string) (*rule.RunResult, error) {
	return p.result(), nil
}
func (p *persistFailPipeline) RunAll(context.Context) (*rule.RunResult, error) {
	return p.result(), nil
}
func (p *persistFailPipeline) RunAllScoped(context.Context, rule.RunScope) (*rule.RunResult, error) {
	return p.result(), nil
}
func (p *persistFailPipeline) RunRuleScoped(context.Context, string, rule.RunScope) (*rule.RunResult, error) {
	return p.result(), nil
}
func (p *persistFailPipeline) FixViolation(context.Context, string) (*rule.FixResult, error) {
	return nil, nil
}
func (p *persistFailPipeline) SetArtistWorkers(int) {}
func (p *persistFailPipeline) ArtistWorkers() int   { return 1 }

// seedArtistForRunRules creates an artist the run-rules handler can resolve.
func seedArtistForRunRules(t *testing.T, artistSvc *artist.Service, name string) *artist.Artist {
	t.Helper()
	a := &artist.Artist{Name: name, SortName: name, Path: t.TempDir()}
	if err := artistSvc.Create(context.Background(), a); err != nil {
		t.Fatalf("creating artist: %v", err)
	}
	return a
}

// TestHandleRunArtistRules_PersistFailure_JSON is the regression guard for the
// JSON half of #2724: a run that could not save its results must not report
// success. Returning 200 here is the exact symptom the issue describes -- a
// scripted caller polling this endpoint was told the run succeeded while the
// database had nothing.
func TestHandleRunArtistRules_PersistFailure_JSON(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPipeline(t)
	r.pipeline = &persistFailPipeline{persistFailures: 1, violationsFound: 3}

	a := seedArtistForRunRules(t, artistSvc, "Persist Fail JSON")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d -- a run that lost its writes must not report success; body: %s",
			w.Code, http.StatusInternalServerError, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v (body: %s)", err, w.Body.String())
	}
	if _, ok := body["error"]; !ok {
		t.Error("response has no error key; a caller cannot distinguish this from a successful run")
	}
	if got, ok := body["persist_failures"]; !ok || got.(float64) != 1 {
		t.Errorf("persist_failures = %v (present=%v), want 1", got, ok)
	}
}

// TestHandleRunArtistRules_PersistFailure_HTMX covers the browser half: the
// operator sees that the results were not saved rather than a violation count.
func TestHandleRunArtistRules_PersistFailure_HTMX(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPipeline(t)
	r.pipeline = &persistFailPipeline{persistFailures: 1, violationsFound: 3}

	a := seedArtistForRunRules(t, artistSvc, "Persist Fail HTMX")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "could not be saved") {
		t.Errorf("HTMX body = %q, want a not-saved message", body)
	}
	// The violation count must NOT be the headline when the run did not stick.
	if strings.Contains(body, "Found 3 violation") {
		t.Errorf("HTMX body reports a violation count for a run that was not persisted: %q", body)
	}
}

// TestHandleRunArtistRules_CleanRun_ReportsSuccess is the counterpart, and the
// guard against over-correction: a run with no persist failures must still
// return 200 with the violation count. Without this, a fix that always
// reported failure would pass the tests above.
func TestHandleRunArtistRules_CleanRun_ReportsSuccess(t *testing.T) {
	t.Parallel()
	r, artistSvc := testRouterWithPipeline(t)
	r.pipeline = &persistFailPipeline{persistFailures: 0, violationsFound: 2}

	a := seedArtistForRunRules(t, artistSvc, "Clean Run")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d for a clean run; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if _, ok := body["error"]; ok {
		t.Error("clean run returned an error key")
	}
	if got := body["violations_found"]; got.(float64) != 2 {
		t.Errorf("violations_found = %v, want 2", got)
	}
}
