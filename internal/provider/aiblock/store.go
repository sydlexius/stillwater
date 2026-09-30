package aiblock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sydlexius/stillwater/internal/filesystem"
	"github.com/sydlexius/stillwater/internal/httpsafe"
	"github.com/sydlexius/stillwater/internal/version"
)

// SourceURL is the upstream list: laylavish/uBlockOrigin-HUGE-AI-Blocklist,
// list_uBlacklist.txt, licensed CC0-1.0.
const SourceURL = "https://raw.githubusercontent.com/laylavish/uBlockOrigin-HUGE-AI-Blocklist/main/list_uBlacklist.txt"

const (
	// DefaultRefreshInterval is how often a running Store re-fetches the list.
	DefaultRefreshInterval = 24 * time.Hour
	// CacheFileName is the file the accepted list is persisted to in CacheDir.
	CacheFileName = "ai_blocklist.txt"
	// Caps on an accepted list. The real list is about 100 KiB with about 90
	// regexes; a list over any cap is rejected whole, never truncated, so a
	// hostile upstream cannot make Parse retain gigabytes of compiled regexes.
	maxListBytes  = 1 << 20
	maxRegexes    = 1000
	maxRegexBytes = 1024
	fetchTimeout  = 30 * time.Second
)

// emptyMatcher is the matcher before any list has loaded: it blocks nothing.
var emptyMatcher = &Matcher{}

// canaryURLs are ordinary image URLs Stillwater relies on. A list that blocks
// any of them is broken or hostile (e.g. "/./", or a public-suffix rule like
// "*://*.co.uk/*") and is rejected rather than hiding every search result.
var canaryURLs = [...]string{
	"https://upload.wikimedia.org/wikipedia/commons/a/a9/Example.jpg",
	"https://coverartarchive.org/release/76df3287-6cda-33eb-8e9a-044b5e15ffdd/829521842.jpg",
	"https://i.discogs.com/R-1234-1600000000.jpeg",
	"https://www.example.co.uk/images/artist.jpg",
}

// defaultStore backs Default(); see SetDefault.
var defaultStore atomic.Pointer[Store]

// SetDefault installs s as the store Default() reads from.
func SetDefault(s *Store) { defaultStore.Store(s) }

// Snapshot returns the installed store's active matcher together with the
// Status describing it, read under one lock, so a caller filtering with the
// matcher reports the state of that same list (a load finishing mid-request
// cannot pair an empty matcher with Loaded=true). With no store installed it
// is the empty matcher and the zero Status (not loaded).
func Snapshot() (*Matcher, Status) {
	if s := defaultStore.Load(); s != nil {
		return s.snapshot()
	}
	return emptyMatcher, Status{}
}

// Options configures a Store. The zero value is usable: no disk cache, the
// upstream SourceURL, a 24h refresh, and the SSRF-guarded shared client.
type Options struct {
	// CacheDir holds the last accepted list across restarts. Empty disables
	// the cache.
	CacheDir string
	// Interval between refreshes; zero means DefaultRefreshInterval.
	Interval time.Duration
	// Client performs the fetch; nil means httpsafe.SafeClient.
	Client *http.Client
	// URL overrides SourceURL (tests).
	URL    string
	Logger *slog.Logger
	// Disabled marks a store whose download the operator turned off. It is
	// only reported through Status, so callers can say "off" rather than
	// "not loaded yet"; the caller is responsible for never starting it.
	Disabled bool
}

// Status describes the list a Store is filtering with, for display.
type Status struct {
	// Loaded is false until a list (cached or fetched) has been accepted.
	// While false the active matcher blocks nothing.
	Loaded bool
	// Rules is the number of compiled rules in the active list.
	Rules int
	// LastFetch is when the active list was fetched: the time of the last
	// successful download, or the cache file's modification time when the
	// list came from the cache. Zero when nothing is loaded.
	LastFetch time.Time
	// LastError is the most recent failure, cleared by a clean refresh.
	LastError string
	// Disabled is true when the operator turned the download off, so no list
	// will ever load (Options.Disabled).
	Disabled bool
}

// Store owns the active blocklist. It loads a cached copy at start, then
// fetches and periodically refreshes the list in the background. Readers call
// Matcher, which is a lock-free atomic load.
type Store struct {
	opts   Options
	active atomic.Pointer[Matcher]

	// refreshMu serializes loads (fetch through persist), so a slow earlier
	// fetch can never overwrite a newer list in memory or in the cache.
	refreshMu sync.Mutex

	// mu guards status and is held across the matcher swap, so a Status
	// snapshot always describes the matcher that was active when it was taken.
	mu     sync.Mutex
	status Status
}

// NewStore returns a Store with an empty (block-nothing) matcher.
func NewStore(opts Options) *Store {
	if opts.Interval <= 0 {
		opts.Interval = DefaultRefreshInterval
	}
	if opts.Client == nil {
		opts.Client = httpsafe.SafeClient(fetchTimeout)
	}
	if opts.URL == "" {
		opts.URL = SourceURL
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Store{opts: opts}
	s.status.Disabled = opts.Disabled
	s.active.Store(emptyMatcher)
	return s
}

// snapshot returns the active matcher and its Status under mu, the lock adopt
// holds across the swap, so the pair always describes one list.
func (s *Store) snapshot() (*Matcher, Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active.Load(), s.status
}

// Matcher returns the active matcher. It is never nil.
func (s *Store) Matcher() *Matcher { return s.active.Load() }

// Status returns a snapshot of the store's state.
func (s *Store) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Start loads the cached list (a local read, so filtering works right after a
// restart), then fetches and refreshes in a background goroutine until ctx is
// canceled. The returned channel closes when that goroutine has exited.
func (s *Store) Start(ctx context.Context) <-chan struct{} {
	s.loadCache()
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(s.opts.Interval)
		defer t.Stop()
		for {
			if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
				s.opts.Logger.Warn("AI blocklist: refresh failed; keeping the current list",
					slog.String("url", s.opts.URL), slog.String("error", err.Error()))
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return done
}

// loadCache adopts the cached list if there is a usable one.
func (s *Store) loadCache() {
	if s.opts.CacheDir == "" {
		return
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	path := filepath.Join(s.opts.CacheDir, CacheFileName)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var body []byte
	switch {
	case err != nil:
	case info.Size() > maxListBytes:
		err = fmt.Errorf("exceeds %d bytes", maxListBytes)
	default:
		body, err = os.ReadFile(path) //nolint:gosec // G304: path is operator config plus a constant name
	}
	if err == nil {
		err = s.adopt(string(body), info.ModTime())
	}
	if err != nil {
		s.setError(fmt.Errorf("cached list: %w", err))
		s.opts.Logger.Warn("AI blocklist: ignoring cached list",
			slog.String("path", path), slog.String("error", err.Error()))
	}
}

// Refresh fetches the list once. A list is adopted only if it downloads in
// full with a 200 and passes adopt's checks; on any failure the current
// matcher and the cache file are left untouched. Concurrent calls serialize.
func (s *Store) Refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	body, err := s.fetch(ctx)
	if err == nil {
		err = s.adopt(body, time.Now())
	}
	if err != nil {
		s.setError(err)
		return err
	}
	if s.opts.CacheDir != "" {
		path := filepath.Join(s.opts.CacheDir, CacheFileName)
		if werr := filesystem.WriteFileAtomic(path, []byte(body), 0o644); werr != nil {
			werr = fmt.Errorf("writing cache: %w", werr)
			s.setError(werr)
			return werr
		}
	}
	return nil
}

func (s *Store) fetch(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.opts.URL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("User-Agent", version.UserAgent("Stillwater", "https://github.com/sydlexius/stillwater"))
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching list: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching list: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxListBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading list: %w", err)
	}
	if len(data) > maxListBytes {
		return "", fmt.Errorf("list exceeds %d bytes", maxListBytes)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n"), nil
}

// adopt parses body and, if it passes validation, makes it the active matcher.
// It rejects a list with no rules, over the regex caps, or blocking a canary.
func (s *Store) adopt(body string, fetched time.Time) error {
	if err := checkRegexCaps(body); err != nil {
		return err
	}
	m := Parse(body)
	n := m.ruleCount()
	if n == 0 {
		return errors.New("list has no usable rules")
	}
	for _, u := range canaryURLs {
		if m.MatchURL(u) {
			return fmt.Errorf("list blocks known-clean URL %s", u)
		}
	}
	if m.Skipped > 0 {
		s.opts.Logger.Warn("AI blocklist: skipped unparsable lines", slog.Int("skipped", m.Skipped))
	}
	s.mu.Lock()
	s.active.Store(m)
	s.status = Status{Loaded: true, Rules: n, LastFetch: fetched, Disabled: s.opts.Disabled}
	s.mu.Unlock()
	return nil
}

// checkRegexCaps rejects a list with too many or too long regex lines. It
// scans the source (the lines Parse treats as regexes) before anything is
// compiled, so an oversized list never reaches regexp.Compile.
func checkRegexCaps(body string) error {
	count := 0
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "/") {
			continue
		}
		if len(line) > maxRegexBytes {
			return fmt.Errorf("list has a regex over %d bytes", maxRegexBytes)
		}
		count++
		if count > maxRegexes {
			return fmt.Errorf("list has over %d regexes", maxRegexes)
		}
	}
	return nil
}

func (s *Store) setError(err error) {
	s.mu.Lock()
	s.status.LastError = err.Error()
	s.mu.Unlock()
}
