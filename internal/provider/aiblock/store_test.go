package aiblock

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	ls := newListServer(t, http.StatusOK, readSample(t))
	s := ls.store("", time.Hour)
	SetDefault(s)
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !Default().MatchURL(blockedURL) {
		t.Error("Default must return the installed store's active matcher")
	}
}
