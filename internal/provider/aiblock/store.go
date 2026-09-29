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
	// maxListBytes caps a fetched body; the real list is about 100 KiB.
	maxListBytes = 4 << 20
	fetchTimeout = 30 * time.Second
)

// emptyMatcher is the matcher before any list has loaded: it blocks nothing.
var emptyMatcher = &Matcher{}

// defaultStore backs Default(); see SetDefault.
var defaultStore atomic.Pointer[Store]

// SetDefault installs s as the store Default() reads from.
func SetDefault(s *Store) { defaultStore.Store(s) }

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
}

// Store owns the active blocklist. It loads a cached copy at start, then
// fetches and periodically refreshes the list in the background. Readers call
// Matcher, which is a lock-free atomic load.
type Store struct {
	opts   Options
	active atomic.Pointer[Matcher]

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
	s.active.Store(emptyMatcher)
	return s
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
	path := filepath.Join(s.opts.CacheDir, CacheFileName)
	body, err := os.ReadFile(path) //nolint:gosec // G304: path is operator config plus a constant name
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var info os.FileInfo
	if err == nil {
		info, err = os.Stat(path)
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
// full with a 200 and parses to at least one rule; on any failure the current
// matcher and the cache file are left untouched.
func (s *Store) Refresh(ctx context.Context) error {
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

// adopt parses body and, if it has rules, makes it the active matcher.
func (s *Store) adopt(body string, fetched time.Time) error {
	m := Parse(body)
	n := m.ruleCount()
	if n == 0 {
		return errors.New("list has no usable rules")
	}
	if m.Skipped > 0 {
		s.opts.Logger.Warn("AI blocklist: skipped unparsable lines", slog.Int("skipped", m.Skipped))
	}
	s.active.Store(m)
	s.mu.Lock()
	s.status = Status{Loaded: true, Rules: n, LastFetch: fetched}
	s.mu.Unlock()
	return nil
}

func (s *Store) setError(err error) {
	s.mu.Lock()
	s.status.LastError = err.Error()
	s.mu.Unlock()
}
