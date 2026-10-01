package aiblock

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const blockedURL = "https://images.nightcafe.studio/jobs/a.jpg"

func readSample(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "upstream_sample.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// listServer serves whatever status and body are currently set, and counts hits.
type listServer struct {
	*httptest.Server
	status atomic.Int64
	body   atomic.Pointer[string]
	hits   atomic.Int64
}

func newListServer(t *testing.T, status int, body string) *listServer {
	t.Helper()
	ls := &listServer{}
	ls.set(status, body)
	ls.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ls.hits.Add(1)
		w.WriteHeader(int(ls.status.Load()))
		_, _ = w.Write([]byte(*ls.body.Load()))
	}))
	t.Cleanup(ls.Close)
	return ls
}

func (ls *listServer) set(status int, body string) {
	ls.status.Store(int64(status))
	ls.body.Store(&body)
}

func (ls *listServer) store(dir string, interval time.Duration) *Store {
	return NewStore(Options{CacheDir: dir, URL: ls.URL, Client: ls.Client(), Interval: interval})
}

func TestStoreEmptyBeforeLoad(t *testing.T) {
	s := NewStore(Options{})
	if s.Matcher() == nil {
		t.Fatal("Matcher must never be nil: callers use it without a check")
	}
	if s.Matcher().MatchURL(blockedURL) {
		t.Error("before any load the matcher must block nothing")
	}
	if st := s.Status(); st.Loaded || st.Rules != 0 || !st.LastFetch.IsZero() {
		t.Errorf("Status before load = %+v, want not loaded", st)
	}
}

// A cached list is in force as soon as Start returns, before (and even
// without) a successful fetch.
func TestStoreLoadsCacheAtStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CacheFileName), []byte(readSample(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	ls := newListServer(t, http.StatusInternalServerError, "")
	s := ls.store(dir, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := s.Start(ctx)
	t.Cleanup(func() { cancel(); <-done })

	if !s.Matcher().MatchURL(blockedURL) {
		t.Error("cached list must be active as soon as Start returns")
	}
	if st := s.Status(); !st.Loaded || st.Rules == 0 || st.LastFetch.IsZero() {
		t.Errorf("Status after cache load = %+v, want loaded with rules and a time", st)
	}
}

func TestStoreRefreshSwapsMatcherAndWritesCache(t *testing.T) {
	sample := readSample(t)
	// Upstream serves CRLF; the cache must hold LF.
	ls := newListServer(t, http.StatusOK, strings.ReplaceAll(sample, "\n", "\r\n"))
	dir := t.TempDir()
	s := ls.store(dir, time.Hour)

	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !s.Matcher().MatchURL(blockedURL) {
		t.Error("fetched list must become the active matcher")
	}
	st := s.Status()
	if !st.Loaded || st.Rules != Parse(sample).ruleCount() || st.LastFetch.IsZero() || st.LastError != "" {
		t.Errorf("Status after fetch = %+v", st)
	}
	got, err := os.ReadFile(filepath.Join(dir, CacheFileName))
	if err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	if string(got) != sample {
		t.Errorf("cache content differs from the LF-normalized list (len %d vs %d)", len(got), len(sample))
	}
}

// A failed refresh keeps the previous matcher and leaves the cache untouched.
// Each bad body is otherwise a valid list, so only the named guard rejects it.
func TestStoreFailedRefreshKeepsPrevious(t *testing.T) {
	sample := readSample(t)
	// A literal 1 MiB, not maxListBytes, so the test pins the documented cap.
	oversize := sample + "#" + strings.Repeat("x", 1<<20) + "\n"
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"non-200", http.StatusInternalServerError, sample},
		{"oversize", http.StatusOK, oversize},
		{"zero rules", http.StatusOK, "# comments only\nnot a rule\n"},
		// Lists that would block every clean result (canary guard).
		{"match-all regex", http.StatusOK, sample + "/./\n"},
		{"scheme regex", http.StatusOK, sample + "/^https?:/\n"},
		{"public suffix", http.StatusOK, sample + "*://*.co.uk/*\n"},
		// Regex caps: over-count and over-length lists are rejected whole.
		{"too many regexes", http.StatusOK, sample + manyRegexes(maxRegexes)},
		{"long regex", http.StatusOK, sample + "/" + strings.Repeat("q", maxRegexBytes) + "/\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			first := "*://*.first.example/*\n"
			ls := newListServer(t, http.StatusOK, first)
			dir := t.TempDir()
			s := ls.store(dir, time.Hour)
			if err := s.Refresh(context.Background()); err != nil {
				t.Fatalf("initial Refresh: %v", err)
			}
			before := s.Matcher()

			ls.set(c.status, c.body)
			if err := s.Refresh(context.Background()); err == nil {
				t.Fatal("Refresh must fail")
			}
			if s.Matcher() != before {
				t.Error("a failed refresh must keep the previous matcher")
			}
			if got, _ := os.ReadFile(filepath.Join(dir, CacheFileName)); string(got) != first {
				t.Error("a failed refresh must not overwrite the cache")
			}
			if st := s.Status(); !st.Loaded || st.Rules != 1 || st.LastError == "" {
				t.Errorf("Status after failure = %+v, want still loaded with an error", st)
			}
		})
	}
}

// manyRegexes returns n harmless regex lines; with the fixture's own regexes
// the total exceeds n.
func manyRegexes(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "/zz%d\\.invalid\\//\n", i)
	}
	return b.String()
}

// A slow earlier refresh must not overwrite a newer one: refreshes serialize,
// so the one that starts last installs and persists last.
func TestStoreRefreshesSerialize(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{})
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			close(arrived)
			<-release
			_, _ = w.Write([]byte("*://*.old.example/*\n"))
			return
		}
		_, _ = w.Write([]byte("*://*.new.example/*\n"))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	s := NewStore(Options{CacheDir: dir, URL: srv.URL, Client: srv.Client()})

	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- s.Refresh(context.Background()) }()
	<-arrived
	go func() { second <- s.Refresh(context.Background()) }()
	// Unserialized, the second refresh completes while the first is held.
	select {
	case <-second:
		second <- nil
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if !s.Matcher().MatchURL("https://new.example/x") || s.Matcher().MatchURL("https://old.example/x") {
		t.Error("the later refresh's list must be active")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, CacheFileName)); string(got) != "*://*.new.example/*\n" {
		t.Errorf("cache = %q, want the later refresh's list", got)
	}
}

// An oversized cache file is ignored without being read.
func TestStoreRejectsOversizedCache(t *testing.T) {
	dir := t.TempDir()
	big := readSample(t) + "#" + strings.Repeat("x", maxListBytes) + "\n"
	if err := os.WriteFile(filepath.Join(dir, CacheFileName), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(Options{CacheDir: dir})
	s.loadCache()
	if st := s.Status(); st.Loaded || st.LastError == "" {
		t.Errorf("Status = %+v, want not loaded with an error", st)
	}
}

// Start refreshes on the interval and its goroutine exits on cancel.
func TestStoreStartRefreshesUntilCanceled(t *testing.T) {
	ls := newListServer(t, http.StatusOK, readSample(t))
	s := ls.store("", 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := s.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for ls.hits.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if ls.hits.Load() < 3 {
		t.Fatalf("refresh loop made %d fetches, want >= 3", ls.hits.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh goroutine did not exit after cancel")
	}
	if !s.Matcher().MatchURL(blockedURL) {
		t.Error("background fetch must install the list")
	}
}

func TestDefaultFollowsInstalledStore(t *testing.T) {
	t.Cleanup(func() { SetDefault(nil) })
	if Default().MatchURL(blockedURL) {
		t.Error("with no store, Default must block nothing")
	}
	if m, st := Snapshot(); st.Loaded || m.MatchURL(blockedURL) {
		t.Error("with no store, Snapshot must be the empty matcher and not loaded")
	}
	ls := newListServer(t, http.StatusOK, readSample(t))
	s := ls.store("", time.Hour)
	SetDefault(s)
	if _, st := Snapshot(); st.Loaded {
		t.Error("installed but not refreshed: Snapshot must report not loaded")
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !Default().MatchURL(blockedURL) {
		t.Error("Default must return the installed store's active matcher")
	}
	if m, st := Snapshot(); !st.Loaded || st.Rules == 0 || !m.MatchURL(blockedURL) {
		t.Errorf("Snapshot status = %+v, want the installed store's loaded list and matcher", st)
	}
}

// Disabled is reported through Status (before and after a load) so callers
// can tell "turned off" from "not loaded yet".
func TestStoreDisabledIsReported(t *testing.T) {
	if NewStore(Options{}).Status().Disabled {
		t.Error("a default store must not report disabled")
	}
	t.Cleanup(func() { SetDefault(nil) })
	SetDefault(NewStore(Options{Disabled: true}))
	if _, st := Snapshot(); !st.Disabled || st.Loaded {
		t.Errorf("disabled store: Snapshot status = %+v, want Disabled and not loaded", st)
	}
}

// Snapshot's contract (#2310 review F8): the matcher and status it returns
// describe ONE list. Readers race a writer that installs fresh stores and
// refreshes them; a loaded status must never pair with the empty matcher (or
// an unloaded one with a real matcher), and a loaded status's rule count must
// be the matcher's. Bounded to ~1s; a correct Snapshot never fails it.
//
// Coverage of both states is deterministic: the writer itself takes a
// Snapshot after each phase (not loaded right after installing a fresh store,
// loaded right after its Refresh) and asserts it. The racing readers only add
// the concurrent consistency check, so scheduling can neither let the test
// pass without exercising both states nor fail it when Snapshot is correct.
func TestSnapshotMatcherAndStatusAgree(t *testing.T) {
	ls := newListServer(t, http.StatusOK, readSample(t))
	t.Cleanup(func() { SetDefault(nil) })
	consistent := func(m *Matcher, st Status) bool {
		return st.Loaded != (m == emptyMatcher) && (!st.Loaded || m.ruleCount() == st.Rules)
	}
	var stop atomic.Bool
	var bad atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if m, st := Snapshot(); !consistent(m, st) {
					bad.Add(1)
				}
			}
		}()
	}
	halt := func() { stop.Store(true); wg.Wait() }
	phases := 0
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); phases++ {
		s := ls.store("", time.Hour)
		SetDefault(s)
		if m, st := Snapshot(); st.Loaded || m != emptyMatcher {
			halt()
			t.Fatalf("fresh store: Snapshot status %+v (empty matcher: %v), want not loaded with the empty matcher", st, m == emptyMatcher)
		}
		if err := s.Refresh(context.Background()); err != nil {
			halt()
			t.Fatal(err)
		}
		if m, st := Snapshot(); !st.Loaded || !consistent(m, st) {
			halt()
			t.Fatalf("after Refresh: Snapshot status %+v with a %d-rule matcher, want loaded and matching", st, m.ruleCount())
		}
	}
	halt()
	if phases == 0 {
		t.Fatal("the writer ran no phases")
	}
	if n := bad.Load(); n != 0 {
		t.Fatalf("%d snapshots paired a matcher with a status describing a different list", n)
	}
}

// A failed fetch is retried on the short backoff, not after the full interval.
func TestStoreRetriesFailedFetchOnBackoff(t *testing.T) {
	var hits atomic.Int64
	var mu sync.Mutex
	var times []time.Time
	sample := readSample(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		if hits.Add(1) <= 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(sample))
	}))
	t.Cleanup(srv.Close)
	s := NewStore(Options{URL: srv.URL, Client: srv.Client(), Interval: time.Hour,
		RetryInitial: 5 * time.Millisecond, RetryMax: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := s.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for !s.Matcher().MatchURL(blockedURL) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !s.Matcher().MatchURL(blockedURL) {
		t.Fatalf("list not loaded after a failed first fetch (hits=%d): retry waited for the full interval", hits.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh goroutine did not exit after cancel")
	}
	// Timers never fire early, so these lower bounds are not timing-sensitive:
	// waits after failures 1, 2, 3 are 5ms, 10ms, 20ms (doubling).
	mu.Lock()
	defer mu.Unlock()
	for i, min := range []time.Duration{5, 10, 20} {
		if gap := times[i+1].Sub(times[i]); gap < min*time.Millisecond {
			t.Errorf("gap before request %d = %v, want >= %dms (backoff must double)", i+2, gap, min)
		}
	}
}

// A replacement far smaller than the active list is rejected; the old matcher
// and the cache file survive. Exactly the floor is still accepted.
func TestStoreRejectsGuttedList(t *testing.T) {
	full := manyRegexes(40)
	ls := newListServer(t, http.StatusOK, full)
	dir := t.TempDir()
	s := ls.store(dir, time.Hour)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("initial Refresh: %v", err)
	}
	before := s.Matcher()
	cachePath := filepath.Join(dir, CacheFileName)
	cacheBefore, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}

	ls.set(http.StatusOK, manyRegexes(19)) // 19 of 40 is under 50%
	if err := s.Refresh(context.Background()); err == nil {
		t.Fatal("a gutted list must be rejected")
	}
	if s.Matcher() != before {
		t.Error("a rejected list must leave the active matcher")
	}
	if got, _ := os.ReadFile(cachePath); string(got) != string(cacheBefore) {
		t.Error("a rejected list must not touch the cache")
	}
	if st := s.Status(); st.Rules != 40 || st.LastError == "" {
		t.Errorf("Status = %+v, want 40 rules and an error", st)
	}

	ls.set(http.StatusOK, manyRegexes(20)) // exactly 50%
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("a list at the floor must be accepted: %v", err)
	}
}

// A list blocking any host Stillwater serves images from is rejected.
func TestStoreRejectsListBlockingCanaryHost(t *testing.T) {
	// Pinned here, not derived from canaryURLs, so dropping a canary fails.
	for _, host := range []string{
		"assets.fanart.tv", "r2.theaudiodb.com", "cdn-images.dzcdn.net",
		"lastfm.freetls.fastly.net", "i.scdn.co", "commons.wikimedia.org",
		"e-cdns-images.dzcdn.net", "ia800000.us.archive.org", "images.genius.com",
	} {
		t.Run(host, func(t *testing.T) {
			ls := newListServer(t, http.StatusOK, readSample(t)+"*://"+host+"/*\n")
			s := ls.store("", time.Hour)
			if err := s.Refresh(context.Background()); err == nil {
				t.Fatalf("list blocking %s must be rejected", host)
			}
			if s.Status().Loaded {
				t.Error("nothing must be loaded")
			}
		})
	}
}

// If the cache cannot be written the valid list stays active (a read-only data
// dir must not disable the feature) and the error says so.
func TestStoreCacheWriteFailureKeepsListAndSaysSo(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ls := newListServer(t, http.StatusOK, readSample(t))
	s := ls.store(filepath.Join(blocker, "cache"), time.Hour)

	err := s.Refresh(context.Background())
	if err == nil || !errors.Is(err, errCacheWrite) || !strings.Contains(err.Error(), "not cached") {
		t.Fatalf("Refresh error = %v, want the active-but-not-cached error", err)
	}
	if strings.Contains(err.Error(), "keeping the current list") {
		t.Errorf("error must not claim the list was kept: %v", err)
	}
	if !s.Matcher().MatchURL(blockedURL) {
		t.Error("the valid list must be active despite the cache failure")
	}
	if st := s.Status(); !st.Loaded || st.LastError == "" {
		t.Errorf("Status = %+v, want loaded with the error recorded", st)
	}
}

// The floor baselines on a list fetched by this process, never on the cache:
// a stale 100-rule cache must not block a valid 16-rule upstream, but a later
// gutted fetch in the same process is still refused.
func TestStoreShrinkFloorIgnoresCacheBaseline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CacheFileName), []byte(manyRegexes(100)), 0o600); err != nil {
		t.Fatal(err)
	}
	ls := newListServer(t, http.StatusOK, manyRegexes(16))
	s := ls.store(dir, time.Hour)
	s.loadCache()
	if st := s.Status(); st.Rules != 100 {
		t.Fatalf("cache rules = %d, want 100", st.Rules)
	}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatalf("first fetch after a cache load must be accepted: %v", err)
	}
	ls.set(http.StatusOK, manyRegexes(7)) // under 50% of the 16 fetched
	if err := s.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("gutted second fetch: err = %v, want a rejection with a restart hint", err)
	}
}

func TestNextWait(t *testing.T) {
	s := NewStore(Options{Interval: time.Hour, RetryInitial: time.Minute, RetryMax: 8 * time.Minute})
	fail := fetchErr{errors.New("boom")}
	retry := s.opts.RetryInitial
	var wait time.Duration
	for i, want := range []time.Duration{1, 2, 4, 8, 8} {
		wait, retry = s.nextWait(fail, retry)
		if wait != want*time.Minute {
			t.Fatalf("failure %d waits %v, want %v", i+1, wait, want*time.Minute)
		}
	}
	// Success and non-fetch errors wait the Interval and reset the backoff.
	for _, err := range []error{nil, errCacheWrite, errors.New("list blocks known-clean URL")} {
		if wait, next := s.nextWait(err, 4*time.Minute); wait != time.Hour || next != time.Minute {
			t.Errorf("nextWait(%v) = %v, %v; want 1h, 1m", err, wait, next)
		}
	}
	if wait, _ = s.nextWait(fail, s.opts.RetryInitial); wait != time.Minute {
		t.Errorf("failure after success waits %v, want 1m", wait)
	}
	if wait, next := s.nextWait(fail, 8*time.Minute); wait != 8*time.Minute || next != 8*time.Minute {
		t.Errorf("at the cap: wait %v, next %v; want both 8m", wait, next)
	}
	if wait, next := s.nextWait(fail, 16*time.Minute); wait != 8*time.Minute || next != 8*time.Minute {
		t.Errorf("over the cap: wait %v, next %v; want both 8m", wait, next)
	}
	big := newListServer(t, http.StatusOK, "#"+strings.Repeat("x", 1<<20)+"\n")
	if err := big.store("", time.Hour).Refresh(context.Background()); err == nil || errors.As(err, new(fetchErr)) {
		t.Errorf("oversize Refresh error = %v, want a non-fetchErr rejection", err)
	}
	huge := NewStore(Options{Interval: time.Hour, RetryInitial: math.MaxInt64 / 2, RetryMax: math.MaxInt64 - 1})
	if _, next := huge.nextWait(fail, math.MaxInt64/2+1); next != math.MaxInt64-1 {
		t.Errorf("doubling near MaxInt64 gave %v, want saturation at RetryMax", next)
	}
	short := NewStore(Options{Interval: 3 * time.Minute, RetryInitial: time.Minute, RetryMax: time.Hour})
	if wait, _ = short.nextWait(fail, 16*time.Minute); wait != 3*time.Minute {
		t.Errorf("wait %v not capped at Interval", wait)
	}
}

func TestStoreSourceHostDefaultStoreAndLastChecked(t *testing.T) {
	for url, want := range map[string]string{
		"https://lists.example/path/x.txt?token=secret": "lists.example",
		"://bad":                                 "",
		"https://user:pass@lists.example:8443/x": "lists.example:8443",
	} {
		if got := NewStore(Options{URL: url}).SourceHost(); got != want {
			t.Errorf("SourceHost(%q) = %q, want %q", url, got, want)
		}
	}
	if got := NewStore(Options{URL: "https://lists.example/x", Disabled: true}).SourceHost(); got != "" {
		t.Errorf("disabled SourceHost = %q, want empty", got)
	}
	SetDefault(NewStore(Options{}))
	t.Cleanup(func() { SetDefault(nil) })
	if DefaultStore() != defaultStore.Load() || DefaultStore() == nil {
		t.Error("DefaultStore must return the installed store")
	}
	ls := newListServer(t, http.StatusInternalServerError, "")
	s := ls.store("", time.Hour)
	if !s.Status().LastChecked.IsZero() {
		t.Fatal("LastChecked must be zero before any attempt")
	}
	_ = s.Refresh(context.Background())
	if s.Status().LastChecked.IsZero() {
		t.Error("a failed attempt must still stamp LastChecked")
	}
}

func TestStoreRefreshIfDue(t *testing.T) {
	ls := newListServer(t, http.StatusOK, readSample(t))
	s := ls.store("", time.Hour)
	ctx := context.Background()
	if ran, _, err := s.RefreshIfDue(ctx, time.Minute); !ran || err != nil {
		t.Fatalf("first RefreshIfDue = ran %v, err %v; want a clean run", ran, err)
	}
	if ran, wait, err := s.RefreshIfDue(ctx, time.Minute); ran || err != nil || wait <= 0 || wait > time.Minute {
		t.Errorf("second RefreshIfDue = ran %v, wait %v, err %v; want a refusal with a wait up to 1m", ran, wait, err)
	}
	if n := ls.hits.Load(); n != 1 {
		t.Errorf("list server got %d requests, want 1", n)
	}
	if ran, _, _ := s.RefreshIfDue(ctx, 0); !ran {
		t.Error("a zero gap must always run")
	}
}

// A disabled store must never fetch: NewStore swaps an empty URL for the
// upstream SourceURL, so only an explicit check keeps Refresh off the network.
func TestStoreDisabledNeverFetches(t *testing.T) {
	ls := newListServer(t, http.StatusOK, readSample(t))
	s := NewStore(Options{URL: ls.URL, Client: ls.Client(), Disabled: true})
	if err := s.Refresh(context.Background()); err == nil {
		t.Error("Refresh on a disabled store must return an error")
	}
	if ran, _, err := s.RefreshIfDue(context.Background(), 0); err == nil {
		t.Errorf("RefreshIfDue on a disabled store = ran %v, err nil; want an error", ran)
	}
	if n := ls.hits.Load(); n != 0 {
		t.Errorf("list server got %d requests, want 0", n)
	}
}

// RefreshIfDue must not queue behind a refresh already running (up to the
// client timeout); it reports ran=false at once.
func TestStoreRefreshIfDueDoesNotWaitForRunningRefresh(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(readSample(t)))
	}))
	t.Cleanup(srv.Close)
	s := NewStore(Options{URL: srv.URL, Client: srv.Client()})
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Refresh(context.Background()) }()
	<-started
	res := make(chan bool, 1)
	go func() { ran, _, _ := s.RefreshIfDue(context.Background(), time.Minute); res <- ran }()
	select {
	case ran := <-res:
		if ran {
			t.Error("RefreshIfDue ran while another refresh was in flight")
		}
	case <-time.After(2 * time.Second):
		t.Error("RefreshIfDue blocked behind a running refresh")
	}
	unblock()
	<-done
}

func TestDescribeSource(t *testing.T) {
	cases := []struct{ name, in, display, repo string }{
		{"default list", SourceURL, "raw.githubusercontent.com/laylavish/uBlockOrigin-HUGE-AI-Blocklist/main/list_uBlacklist.txt", "laylavish/uBlockOrigin-HUGE-AI-Blocklist"},
		{"github.com", "https://github.com/own/rep/raw/main/l.txt", "github.com/own/rep/raw/main/l.txt", "own/rep"},
		{"github one segment", "https://github.com/own", "github.com/own", ""},
		{"non-github", "https://example.com:8443/lists/ai.txt", "example.com:8443/lists/ai.txt", ""},
		{"userinfo query fragment", "https://u:SECRETPW@example.com/a.txt?token=SECRETQ#SECRETF", "example.com/a.txt", ""},
		{"github with secrets", "https://tok:SECRETPW@raw.githubusercontent.com/o/r/main/l.txt?k=SECRETQ#SECRETF", "raw.githubusercontent.com/o/r/main/l.txt", "o/r"},
		{"uppercase host", "https://GITHUB.COM/o/r", "GITHUB.COM/o/r", "o/r"},
		{"github with port", "https://github.com:8443/o/r", "github.com:8443/o/r", "o/r"},
		{"encoded slash kept escaped", "https://github.com/o%2Fx/r", "github.com/o%2Fx/r", ""},
		{"leading empty segment", "https://github.com//r", "github.com//r", ""},
		{"trailing empty segments", "https://github.com/o//", "github.com/o//", ""},
		{"middle empty segment", "https://github.com/o//r", "github.com/o//r", ""},
		{"unparsable", "http://[::1", "", ""},
		{"no host", "/just/a/path", "", ""},
	}
	for _, c := range cases {
		display, repo := DescribeSource(c.in)
		if display != c.display || repo != c.repo {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, display, repo, c.display, c.repo)
		}
		for _, secret := range []string{"SECRET", "u:", "token="} {
			if strings.Contains(display+repo, secret) {
				t.Errorf("%s: output %q leaks %q", c.name, display+repo, secret)
			}
		}
	}
}

func TestSourceDisplay_DisabledShowsNothing(t *testing.T) {
	s := NewStore(Options{URL: SourceURL, Disabled: true})
	if d, r := s.SourceDisplay(); d != "" || r != "" {
		t.Errorf("disabled store: got (%q, %q), want empty", d, r)
	}
}
