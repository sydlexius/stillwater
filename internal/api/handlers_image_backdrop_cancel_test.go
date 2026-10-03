package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
	"modernc.org/sqlite"
)

// backdropLogRecorder is a slog.Handler that records every record and runs an
// optional hook on each one. The cancel test uses the hook to cancel the
// request context at a precise point inside the handler's loop.
type backdropLogRecorder struct {
	mu   sync.Mutex
	recs []slog.Record
	hook func(slog.Record)
}

func (h *backdropLogRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *backdropLogRecorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *backdropLogRecorder) WithGroup(string) slog.Handler            { return h }
func (h *backdropLogRecorder) Handle(_ context.Context, rec slog.Record) error {
	h.mu.Lock()
	h.recs = append(h.recs, rec)
	h.mu.Unlock()
	if h.hook != nil {
		h.hook(rec)
	}
	return nil
}

// count returns how many records match the level and message.
func (h *backdropLogRecorder) count(level slog.Level, msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, rec := range h.recs {
		if rec.Level == level && rec.Message == msg {
			n++
		}
	}
	return n
}

// countLevel returns how many records were emitted at the given level.
func (h *backdropLogRecorder) countLevel(level slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, rec := range h.recs {
		if rec.Level == level {
			n++
		}
	}
	return n
}

// seedStaleBackdropPool creates n artists whose fanart exists_flag is set but
// whose file is missing, so each one the handler visits clears its flag and
// logs "cleared stale backdrop flag". The pool size is asserted.
func seedStaleBackdropPool(t *testing.T, r *Router, svc *artist.Service, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("Cancel Pool %d", i)
		a := &artist.Artist{Name: name, SortName: name, Path: t.TempDir()}
		if err := svc.Create(ctx, a); err != nil {
			t.Fatalf("creating artist: %v", err)
		}
		if _, err := r.db.ExecContext(ctx,
			`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
			 VALUES (lower(hex(randomblob(16))), ?, 'fanart', 0, 1)`, a.ID); err != nil {
			t.Fatalf("seeding artist_images: %v", err)
		}
	}
	var got int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_images WHERE image_type = 'fanart' AND slot_index = 0 AND exists_flag = 1`,
	).Scan(&got); err != nil {
		t.Fatalf("counting pool: %v", err)
	}
	if got != n {
		t.Fatalf("precondition: pool has %d artists, want %d", got, n)
	}
}

// TestHandleRandomBackdrop_CancelMidLoopIsQuiet cancels the request after the
// first artist is processed. The handler must not warn for the artists it never
// reached and must not report a server error. It also proves the walk STOPS: a
// counting wrapper around the artist repository sees exactly two GetByID calls
// (the first succeeds, the second fails on the canceled context and ends the
// loop). With a continue instead of a break it would see all five.
func TestHandleRandomBackdrop_CancelMidLoopIsQuiet(t *testing.T) {
	t.Parallel()
	r, svc := testRouterWithPlatform(t)
	const poolSize = 5
	seedStaleBackdropPool(t, r, svc, poolSize)
	counter := &countingArtistRepo{Repository: nil}
	{
		ar, pr, mr, al, im, pl, co := artist.NewDefaultRepos(r.db)
		counter.Repository = ar
		r.artistService = artist.NewServiceWithRepos(counter, pr, mr, al, im, pl, co)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &backdropLogRecorder{hook: func(l slog.Record) {
		// The first processed artist logs this Info line; cancel right then,
		// so the cancellation lands inside the loop, after the pool is read.
		if l.Message == "cleared stale backdrop flag" {
			cancel()
		}
	}}
	r.logger = slog.New(rec)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	r.handleRandomBackdrop(w, req)

	// Precondition, not a behavior check: the hook fired and the cancel landed
	// inside the loop. This holds with or without the fix.
	if got := rec.count(slog.LevelInfo, "cleared stale backdrop flag"); got != 1 {
		t.Fatalf("precondition: %d artists cleared before cancel, want exactly 1", got)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if got := rec.countLevel(slog.LevelWarn); got != 0 {
		t.Errorf("got %d Warn records for a canceled request, want 0", got)
	}
	if got := counter.calls.Load(); got != 2 {
		t.Errorf("GetByID called %d times, want 2 (the walk must stop at the first canceled lookup)", got)
	}
}

// countingArtistRepo counts GetByID calls and delegates everything else.
type countingArtistRepo struct {
	artist.Repository
	calls atomic.Int32
}

func (c *countingArtistRepo) GetByID(ctx context.Context, id string) (*artist.Artist, error) {
	c.calls.Add(1)
	return c.Repository.GetByID(ctx, id)
}

// faultDriver wraps the real SQLite driver. For the random-backdrop pool query
// only, it replaces the returned rows with ones that misbehave on the first
// Next call, which is how a failure after QueryContext returns is produced
// deterministically.
type faultDriver struct {
	inner  driver.Driver
	onNext func(dest []driver.Value) error // runs on the first Next
}

func (d *faultDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: c, d: d}, nil
}

type faultConn struct {
	driver.Conn
	d *faultDriver
}

func (c *faultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := qc.QueryContext(ctx, q, args)
	if err != nil || !strings.Contains(q, "ORDER BY RANDOM()") {
		return rows, err
	}
	return &faultRows{Rows: rows, d: c.d}, nil
}

type faultRows struct {
	driver.Rows
	d *faultDriver
}

func (f *faultRows) Next(dest []driver.Value) error { return f.d.onNext(dest) }

// withFaultyPoolRows points r.db at the same database file through faultDriver
// and returns nothing; onNext decides how the pool read fails.
func withFaultyPoolRows(t *testing.T, r *Router, onNext func(dest []driver.Value) error) {
	t.Helper()
	var path string
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path); err != nil {
		t.Fatalf("db path: %v", err)
	}
	probe, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("probe open: %v", err)
	}
	inner := probe.Driver()
	_ = probe.Close()
	db := sql.OpenDB(dsnConnector{d: &faultDriver{inner: inner, onNext: onNext}, dsn: path})
	t.Cleanup(func() { _ = db.Close() })
	r.db = db
}

type dsnConnector struct {
	d   driver.Driver
	dsn string
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.d.Open(c.dsn) }
func (c dsnConnector) Driver() driver.Driver                        { return c.d }

// TestHandleRandomBackdrop_FailureAfterQueryReturns drives the two pool-drain
// failure paths (rows.Err after a failing Next, and rows.Scan on a NULL id)
// with the request canceled at that exact point, and with a live request as the
// control. Canceled: no 500, no Error, no Warn. Live: Error log and 500.
func TestHandleRandomBackdrop_FailureAfterQueryReturns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cancel  bool
		nullID  bool // true: Next yields a NULL id (Scan fails); false: Next fails (rows.Err)
		wantMsg string
	}{
		{"iteration error, canceled", true, false, ""},
		{"iteration error, live", false, false, "random backdrop iteration failed"},
		{"scan error, canceled", true, true, ""},
		{"scan error, live", false, true, "random backdrop scan failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, svc := testRouterWithPlatform(t)
			seedStaleBackdropPool(t, r, svc, 2)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var fired atomic.Int32
			withFaultyPoolRows(t, r, func(dest []driver.Value) error {
				fired.Add(1)
				if tc.cancel {
					cancel()
				}
				if tc.nullID {
					dest[0] = nil
					return nil
				}
				return errors.New("injected iteration failure")
			})
			rec := &backdropLogRecorder{}
			r.logger = slog.New(rec)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil).WithContext(ctx)
			w := httptest.NewRecorder()
			r.handleRandomBackdrop(w, req)

			if fired.Load() == 0 {
				t.Fatal("precondition: the injected Next never ran")
			}
			if tc.cancel {
				if w.Code == http.StatusInternalServerError {
					t.Error("status = 500 for a request canceled mid-drain")
				}
				if got := rec.countLevel(slog.LevelError) + rec.countLevel(slog.LevelWarn); got != 0 {
					t.Errorf("got %d Error/Warn records, want 0", got)
				}
				return
			}
			if w.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", w.Code)
			}
			if got := rec.count(slog.LevelError, tc.wantMsg); got != 1 {
				t.Errorf("got %d %q Error records, want 1", got, tc.wantMsg)
			}
		})
	}
}

// TestHandleRandomBackdrop_PreCanceledIsQuiet runs the handler with a request
// context that is already canceled: the pool query fails with a cancellation.
// That must not log an Error or answer 500.
func TestHandleRandomBackdrop_PreCanceledIsQuiet(t *testing.T) {
	t.Parallel()
	r, svc := testRouterWithPlatform(t)
	seedStaleBackdropPool(t, r, svc, 3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ctx.Err() == nil {
		t.Fatal("precondition: context must be canceled")
	}
	rec := &backdropLogRecorder{}
	r.logger = slog.New(rec)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	r.handleRandomBackdrop(w, req)

	if w.Code == http.StatusInternalServerError {
		t.Errorf("status = 500 for a canceled request")
	}
	if got := rec.countLevel(slog.LevelError); got != 0 {
		t.Errorf("got %d Error records, want 0", got)
	}
	if got := rec.countLevel(slog.LevelWarn); got != 0 {
		t.Errorf("got %d Warn records, want 0", got)
	}
}

// TestHandleRandomBackdrop_GenuineQueryFailureStillErrors is the control for
// the pre-canceled test: a real database failure with a live context keeps its
// Error log and 500.
func TestHandleRandomBackdrop_GenuineQueryFailureStillErrors(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithPlatform(t)
	if _, err := r.db.ExecContext(context.Background(), `DROP TABLE artist_images`); err != nil {
		t.Fatalf("dropping table: %v", err)
	}
	rec := &backdropLogRecorder{}
	r.logger = slog.New(rec)

	w := httptest.NewRecorder()
	r.handleRandomBackdrop(w, httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if got := rec.count(slog.LevelError, "random backdrop query failed"); got != 1 {
		t.Errorf("got %d query-failed Error records, want 1", got)
	}
}

var (
	flagHookOnce sync.Once
	flagHooks    sync.Map // artist id -> func(); set by tests
)

// registerFlagClearHook installs a SQL function the flag-clear UPDATE can call
// through a trigger. It runs the test's callback (which cancels the request)
// and then fails the statement, so ClearImageFlag returns an error at a point
// the test controls. It only reaches connections opened after registration,
// so call it before building the router.
func registerFlagClearHook(t *testing.T) {
	t.Helper()
	flagHookOnce.Do(func() {
		err := sqlite.RegisterScalarFunction("sw_test_flag_hook", 1,
			func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
				if fn, ok := flagHooks.Load(args[0]); ok {
					fn.(func())()
				}
				return nil, errors.New("injected flag-clear failure")
			})
		if err != nil {
			t.Fatalf("registering sql function: %v", err)
		}
	})
}

// failFlagClear makes every exists_flag UPDATE for the artist fail, calling fn
// first.
func failFlagClear(t *testing.T, r *Router, artistID string, fn func()) {
	t.Helper()
	flagHooks.Store(artistID, fn)
	t.Cleanup(func() { flagHooks.Delete(artistID) })
	if _, err := r.db.ExecContext(context.Background(), `CREATE TRIGGER sw_test_flag_fail
		BEFORE UPDATE ON artist_images
		BEGIN SELECT sw_test_flag_hook(OLD.artist_id); END`); err != nil {
		t.Fatalf("creating trigger: %v", err)
	}
}

// onlyBackdropArtist returns the id of the single seeded pool artist.
func onlyBackdropArtist(t *testing.T, r *Router) string {
	t.Helper()
	var id string
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT artist_id FROM artist_images WHERE image_type = 'fanart' LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("reading pool artist: %v", err)
	}
	return id
}

// TestHandleRandomBackdrop_CancelDuringFlagClearIsQuiet cancels the request
// while the stale flag is being cleared, so ClearImageFlag fails with the
// client gone. No Warn may be logged for that.
func TestHandleRandomBackdrop_CancelDuringFlagClearIsQuiet(t *testing.T) {
	t.Parallel()
	registerFlagClearHook(t)
	r, svc := testRouterWithPlatform(t)
	seedStaleBackdropPool(t, r, svc, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var fired int
	failFlagClear(t, r, onlyBackdropArtist(t, r), func() { fired++; cancel() })
	rec := &backdropLogRecorder{}
	r.logger = slog.New(rec)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil).WithContext(ctx)
	r.handleRandomBackdrop(httptest.NewRecorder(), req)

	if fired != 1 {
		t.Fatalf("precondition: flag-clear hook fired %d times, want 1", fired)
	}
	if got := rec.countLevel(slog.LevelWarn); got != 0 {
		t.Errorf("got %d Warn records, want 0", got)
	}
}

// TestHandleRandomBackdrop_GenuineFlagClearFailureStillWarns is the control: the
// same injected failure with a live request context keeps its Warn.
func TestHandleRandomBackdrop_GenuineFlagClearFailureStillWarns(t *testing.T) {
	t.Parallel()
	registerFlagClearHook(t)
	r, svc := testRouterWithPlatform(t)
	seedStaleBackdropPool(t, r, svc, 1)

	var fired int
	failFlagClear(t, r, onlyBackdropArtist(t, r), func() { fired++ })
	rec := &backdropLogRecorder{}
	r.logger = slog.New(rec)

	r.handleRandomBackdrop(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil))

	if fired != 1 {
		t.Fatalf("precondition: flag-clear hook fired %d times, want 1", fired)
	}
	if got := rec.count(slog.LevelWarn, "failed to clear stale backdrop flag"); got != 1 {
		t.Errorf("got %d flag-clear Warn records, want 1", got)
	}
}

// TestHandleRandomBackdrop_GenuineLookupFailureStillWarns is the other
// direction: a lookup failure that is not a cancellation keeps its Warn.
func TestHandleRandomBackdrop_GenuineLookupFailureStillWarns(t *testing.T) {
	t.Parallel()
	r, _ := testRouterWithPlatform(t)
	ctx := context.Background()

	// An artist_images row whose artist does not exist makes GetByID fail with
	// a plain not-found error. Foreign keys are switched off on a pinned
	// connection only to plant the orphan row.
	conn, err := r.db.Conn(ctx)
	if err != nil {
		t.Fatalf("db conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disabling fk: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO artist_images (id, artist_id, image_type, slot_index, exists_flag)
		 VALUES (lower(hex(randomblob(16))), 'no-such-artist', 'fanart', 0, 1)`); err != nil {
		t.Fatalf("seeding orphan row: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("restoring fk: %v", err)
	}
	_ = conn.Close()

	rec := &backdropLogRecorder{}
	r.logger = slog.New(rec)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/images/random-backdrop", nil)
	w := httptest.NewRecorder()
	r.handleRandomBackdrop(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
	if got := rec.count(slog.LevelWarn, "random backdrop artist lookup failed"); got != 1 {
		t.Errorf("got %d lookup-failed Warn records, want 1", got)
	}
}
