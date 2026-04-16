package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"log/slog"

	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/auth"
	"github.com/sydlexius/stillwater/internal/connection"
	"github.com/sydlexius/stillwater/internal/database"
	"github.com/sydlexius/stillwater/internal/encryption"
	"github.com/sydlexius/stillwater/internal/rule"
)

// stubPipeline is a test double for rule.PipelineRunner that returns
// preconfigured results without requiring a real Engine or Service.
type stubPipeline struct {
	runForArtistFn  func(ctx context.Context, a *artist.Artist) (*rule.RunResult, error)
	runImageRulesFn func(ctx context.Context, a *artist.Artist) (*rule.RunResult, error)
	runRuleFn       func(ctx context.Context, ruleID string) (*rule.RunResult, error)
	fixViolationFn  func(ctx context.Context, violationID string) (*rule.FixResult, error)
}

func (s *stubPipeline) RunForArtist(ctx context.Context, a *artist.Artist) (*rule.RunResult, error) {
	if s.runForArtistFn != nil {
		return s.runForArtistFn(ctx, a)
	}
	return &rule.RunResult{}, nil
}

func (s *stubPipeline) RunImageRulesForArtist(ctx context.Context, a *artist.Artist) (*rule.RunResult, error) {
	if s.runImageRulesFn != nil {
		return s.runImageRulesFn(ctx, a)
	}
	return &rule.RunResult{}, nil
}

func (s *stubPipeline) RunRule(ctx context.Context, ruleID string) (*rule.RunResult, error) {
	if s.runRuleFn != nil {
		return s.runRuleFn(ctx, ruleID)
	}
	return &rule.RunResult{}, nil
}

func (s *stubPipeline) RunAll(_ context.Context) (*rule.RunResult, error) {
	return &rule.RunResult{}, nil
}

func (s *stubPipeline) FixViolation(ctx context.Context, violationID string) (*rule.FixResult, error) {
	if s.fixViolationFn != nil {
		return s.fixViolationFn(ctx, violationID)
	}
	return &rule.FixResult{Fixed: true, Message: "stub fixed"}, nil
}

// testRouterWithStubPipeline creates a Router backed by a stubPipeline,
// avoiding the need for a real Engine, rule seeding, and rule service.
func testRouterWithStubPipeline(t *testing.T, stub *stubPipeline) (*Router, *artist.Service) {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	enc, _, err := encryption.NewEncryptor("")
	if err != nil {
		t.Fatalf("creating encryptor: %v", err)
	}

	authSvc := auth.NewService(db)
	artistSvc := artist.NewService(db)
	connSvc := connection.NewService(db, enc)
	ruleSvc := rule.NewService(db)
	if err := ruleSvc.SeedDefaults(context.Background()); err != nil {
		t.Fatalf("seeding rules: %v", err)
	}

	r := NewRouter(RouterDeps{
		AuthService:       authSvc,
		ArtistService:     artistSvc,
		ConnectionService: connSvc,
		RuleService:       ruleSvc,
		Pipeline:          stub,
		DB:                db,
		Logger:            logger,
		StaticFS:          os.DirFS("../../web/static"),
	})

	return r, artistSvc
}

func TestStub_RunArtistRules_ViolationsFound_JSON(t *testing.T) {
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			return &rule.RunResult{
				ArtistsProcessed: 1,
				ViolationsFound:  3,
				FixesAttempted:   1,
				FixesSucceeded:   1,
			}, nil
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Stub Violations Artist")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got, ok := resp["violations_found"]; !ok {
		t.Error("response missing violations_found field")
	} else if got != float64(3) {
		t.Errorf("violations_found = %v, want 3", got)
	}
}

func TestStub_RunArtistRules_ViolationsFound_HTMX(t *testing.T) {
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			return &rule.RunResult{
				ArtistsProcessed: 1,
				ViolationsFound:  5,
			}, nil
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Stub HTMX Violations Artist")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "5 violation(s).") {
		t.Errorf("HTMX body = %q, want '5 violation(s).'", body)
	}
	if !strings.Contains(body, "/?search=") {
		t.Errorf("HTMX body = %q, want /?search= dashboard link", body)
	}
}

func TestStub_RunArtistRules_PipelineError_JSON(t *testing.T) {
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			return nil, fmt.Errorf("engine exploded")
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Stub Error Artist")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

func TestStub_RunArtistRules_PipelineError_HTMX(t *testing.T) {
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			return nil, fmt.Errorf("engine exploded")
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Stub HTMX Error Artist")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed") {
		t.Errorf("HTMX body = %q, want failure message containing 'Failed'", body)
	}
	if strings.Contains(body, "engine exploded") {
		t.Error("HTMX body leaked internal error message to client")
	}
}

func TestStub_RunArtistRules_NoViolations_HTMX(t *testing.T) {
	stub := &stubPipeline{
		runForArtistFn: func(_ context.Context, _ *artist.Artist) (*rule.RunResult, error) {
			return &rule.RunResult{ArtistsProcessed: 1}, nil
		},
	}
	r, artistSvc := testRouterWithStubPipeline(t, stub)
	a := addTestArtist(t, artistSvc, "Stub Clean Artist")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/artists/"+a.ID+"/run-rules", nil)
	req.Header.Set("HX-Request", "true")
	req.SetPathValue("id", a.ID)
	w := httptest.NewRecorder()

	r.handleRunArtistRules(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "No violations found.") {
		t.Errorf("HTMX body = %q, want 'No violations found.'", body)
	}
}
