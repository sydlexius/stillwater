package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/stillwater/internal/api/middleware"
	"github.com/sydlexius/stillwater/internal/artist"
	"github.com/sydlexius/stillwater/internal/rule"
)

// pHashRunsPipeline adds the run-listing capability to the shared fake.
type pHashRunsPipeline struct {
	*pHashCapablePipeline
	listFn func(ctx context.Context, artistID string) ([]rule.PHashRepairRun, error)
}

func (f *pHashRunsPipeline) ListPHashRepairRuns(ctx context.Context, artistID string) ([]rule.PHashRepairRun, error) {
	return f.listFn(ctx, artistID)
}

func newPHashRunsRouter(t *testing.T, listFn func(context.Context, string) ([]rule.PHashRepairRun, error)) *Router {
	t.Helper()
	p := &pHashRunsPipeline{
		pHashCapablePipeline: &pHashCapablePipeline{stubPipeline: &stubPipeline{}},
		listFn:               listFn,
	}
	return testRouterWithFanartPipeline(t, p)
}

func getPHashRuns(r *Router, ctx context.Context, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/artists/"+id+"/backdrop-repairs", nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	r.handlePHashRepairRuns(w, req)
	return w
}

func TestPHashRepairRuns_NonAdminForbidden(t *testing.T) {
	t.Parallel()
	r := newPHashRunsRouter(t, func(context.Context, string) ([]rule.PHashRepairRun, error) {
		t.Error("a non-admin must never reach the pipeline")
		return nil, nil
	})
	ctx := middleware.WithTestRole(middleware.WithTestUserID(context.Background(), "u1"), "operator")
	if w := getPHashRuns(r, ctx, "art-a"); w.Code != http.StatusForbidden {
		t.Errorf("non-admin should get 403; got %d", w.Code)
	}
}

func TestPHashRepairRuns_EmptyListIsAnArray(t *testing.T) {
	t.Parallel()
	// Return a nil slice on purpose: the handler must still emit [] not null.
	r := newPHashRunsRouter(t, func(context.Context, string) ([]rule.PHashRepairRun, error) {
		return nil, nil
	})
	w := getPHashRuns(r, adminContext(), "art-a")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"runs":[]}` {
		t.Errorf("body = %s, want {\"runs\":[]}", got)
	}
}

func TestPHashRepairRuns_Success(t *testing.T) {
	t.Parallel()
	var gotID string
	r := newPHashRunsRouter(t, func(_ context.Context, id string) ([]rule.PHashRepairRun, error) {
		gotID = id
		return []rule.PHashRepairRun{{
			OpID:      "op-1",
			CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
			Entries:   []rule.PHashRepairRunEntry{{FileName: "fanart2.jpg", StoredName: "1-fanart2.jpg", SlotIndex: 1, MatchedArtistID: "art-b", Similarity: 0.97}},
		}}, nil
	})
	w := getPHashRuns(r, adminContext(), "art-a")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", w.Code, w.Body.String())
	}
	if gotID != "art-a" {
		t.Errorf("artist id not threaded through: %q", gotID)
	}
	var out struct {
		Runs []struct {
			OpID    string `json:"op_id"`
			Entries []struct {
				FileName string `json:"file_name"`
			} `json:"entries"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Runs) != 1 || out.Runs[0].OpID != "op-1" || out.Runs[0].Entries[0].FileName != "fanart2.jpg" {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestPHashRepairRuns_UnknownArtistIs404(t *testing.T) {
	t.Parallel()
	r := newPHashRunsRouter(t, func(_ context.Context, id string) ([]rule.PHashRepairRun, error) {
		return nil, fmt.Errorf("loading artist %s: %w", id, artist.ErrNotFound)
	})
	if w := getPHashRuns(r, adminContext(), "nope"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestPHashRepairRuns_PipelineErrorIs500(t *testing.T) {
	t.Parallel()
	r := newPHashRunsRouter(t, func(context.Context, string) ([]rule.PHashRepairRun, error) {
		return nil, fmt.Errorf("reading repair dir: boom")
	})
	if w := getPHashRuns(r, adminContext(), "art-a"); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestPHashRepairRuns_UnimplementedPipelineFailsLoud(t *testing.T) {
	t.Parallel()
	// The plain fake has no ListPHashRepairRuns: a wiring bug, surfaced as 500.
	r := newPHashRepairRouter(t, nil)
	if w := getPHashRuns(r, adminContext(), "art-a"); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestPHashRepairRuns_ThroughTheRealRouter drives the route through the real
// mux and auth middleware, which the direct-handler tests above bypass: no
// cookie is 401, a non-admin session is 403, an admin session is 200.
func TestPHashRepairRuns_ThroughTheRealRouter(t *testing.T) {
	t.Parallel()
	r := newPHashRunsRouter(t, func(context.Context, string) ([]rule.PHashRepairRun, error) {
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mux := r.Handler(ctx)

	session := func(name, role string) *http.Cookie {
		u, err := r.authService.CreateLocalUser(context.Background(), name, "password123", name, role, "")
		if err != nil {
			t.Fatalf("CreateLocalUser(%s): %v", role, err)
		}
		tok, err := r.authService.CreateSession(context.Background(), u.ID)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return &http.Cookie{Name: "session", Value: tok}
	}
	do := func(c *http.Cookie) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/artists/art-a/backdrop-repairs", nil)
		if c != nil {
			req.AddCookie(c)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}

	if got := do(nil); got != http.StatusUnauthorized {
		t.Errorf("no cookie: status = %d, want 401", got)
	}
	// The first local user is bootstrapped as admin by some paths; create the
	// admin first so the operator is unambiguously non-admin.
	admin := session("adminuser", "administrator")
	op := session("opuser", "operator")
	if got := do(op); got != http.StatusForbidden {
		t.Errorf("operator: status = %d, want 403", got)
	}
	if got := do(admin); got != http.StatusOK {
		t.Errorf("admin: status = %d, want 200", got)
	}
}
